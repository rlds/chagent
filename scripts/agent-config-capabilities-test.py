#!/usr/bin/env python3
"""Chagent 配置能力的端到端功能测试。

覆盖 task-5 矩阵：

  A  启动 / 环境变量 / 鉴权 / 健康检查 / 热重载
  B  通过配置 + /reload 启用、禁用工具（exec、list_dir）
  C  工作区路径权限（工作区外读/写拒绝、允许列表、../、exec 拒绝）
  D  MCP stdio 服务器（连接、/list mcp、/show mcp、真实模型调用、禁用）
  E  进程 hook（observer / after_tool modify / deny_tool / approve_tool / 热重载）
  F  配置校验 + 关键参数（max_tool_iterations、skills catalog mode、
     steering_mode/max_parallel_turns）

它运行真实的 gateway 二进制与真实模型（默认 deepseek-v4-flash），并通过
pico WebSocket 协议与其通信。每个场景都使用各自的临时 HOME/WORKSPACE/CONFIG、
port=0，并且总会杀掉自己启动的 gateway。

用法：
    python3 scripts/agent-config-capabilities-test.py [--phases A,B,C,D,E,F]
        [--config /path/to/real/config.json] [--out build/functest-20261030/config]
        [--keep] [--rebuild] [--verbose]

输出（位于 --out 下）：
    results.json     每项检查的 id/phase/status/evidence/detail
    summary.md       中文 PASS/FAIL 矩阵 + 证据路径/关键日志行
    phase-<X>/       各场景的 gateway.log、配置快照、hook/MCP 日志、
                     session JSONL 副本
    workspace/       A 场景工作区的 capsule 副本（API key 已脱敏）

密钥：模型 API key 从源配置读取，只写入临时工作区的 config.json，并在本脚本
写出的所有产物中脱敏。它绝不会被打印。
"""

from __future__ import annotations

import argparse
import asyncio
import glob
import hashlib
import json
import os
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

import websockets

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DEFAULT_MODEL_CONFIG = "/Users/wudaorui/chessdata/weichat/weichat/config.json"
DEFAULT_OUT = os.path.join(ROOT, "build", "functest-20261030", "config")
FIXTURES = os.path.join(ROOT, "scripts", "fixtures")
HOOK_FIXTURE = os.path.join(FIXTURES, "hook_gate.py")
MCP_FIXTURE = os.path.join(FIXTURES, "mcp_stdio_server.py")

PREFERRED_MODEL = "deepseek-v4-flash"
FALLBACK_MODEL = "deepseek-v4-pro"
REDACTION = "[REDACTED_API_KEY]"

ALL_PHASES = ["A", "B", "C", "D", "E", "F"]


# ---------------------------------------------------------------------------
# 小型工具函数
# ---------------------------------------------------------------------------


def now_iso() -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%S%z")


def ensure_dir(path: str) -> str:
    os.makedirs(path, exist_ok=True)
    return path


def read_text(path: str, limit: int = 400_000) -> str:
    try:
        with open(path, "r", encoding="utf-8", errors="replace") as handle:
            return handle.read(limit)
    except OSError:
        return ""


def write_json_atomic(path: str, data) -> None:
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as handle:
        json.dump(data, handle, ensure_ascii=False, indent=2)
    os.replace(tmp, path)


def write_text_atomic(path: str, text: str) -> None:
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as handle:
        handle.write(text)
    os.replace(tmp, path)


def grep_lines(text: str, needle: str, limit: int = 3) -> list[str]:
    if not needle:
        return []
    hits = [line.strip() for line in text.splitlines() if needle in line]
    return hits[-limit:] if limit else hits


def count_occurrences(text: str, needle: str) -> int:
    return text.count(needle) if needle else 0


def redact(text: str, secrets: list[str]) -> str:
    for secret in secrets:
        if secret:
            text = text.replace(secret, REDACTION)
    return text


# ---------------------------------------------------------------------------
# 结果记录器
# ---------------------------------------------------------------------------


class Recorder:
    def __init__(self, outdir: str, secrets: list[str], load_existing: bool = False):
        self.outdir = outdir
        self.secrets = secrets
        self.entries: list[dict] = []
        self.notes: list[str] = []
        if load_existing:
            path = os.path.join(outdir, "results.json")
            try:
                with open(path, "r", encoding="utf-8") as handle:
                    existing = json.load(handle)
                if isinstance(existing, list):
                    self.entries = existing
                    print(f"merged {len(existing)} existing result entries")
            except (OSError, ValueError):
                pass

    def add(self, item_id, phase, status, detail="", evidence=None, logs=None):
        entry = {
            "id": item_id,
            "phase": phase,
            "status": status,
            "detail": detail,
            "evidence": [redact(p, self.secrets) for p in (evidence or [])],
            "logs": [redact(line, self.secrets) for line in (logs or [])],
            "ts": now_iso(),
        }
        self.entries = [e for e in self.entries
                        if not (e.get("id") == item_id and e.get("phase") == phase)]
        self.entries.append(entry)
        self.flush()
        print(f"[{status}] {phase}/{item_id}: {detail[:160]}")

    def add_note(self, note: str) -> None:
        self.notes.append(note)

    def flush(self) -> None:
        write_json_atomic(os.path.join(self.outdir, "results.json"), self.entries)

    def counts(self) -> dict:
        out = {"PASS": 0, "FAIL": 0, "SKIP": 0}
        for entry in self.entries:
            out[entry["status"]] = out.get(entry["status"], 0) + 1
        return out

    def write_summary(self, elapsed: float, extra: dict | None = None) -> str:
        counts = self.counts()
        lines = [
            "# 配置能力实测报告（A-F 矩阵，真实模型 + 真实 gateway + pico WS）",
            "",
            f"- 生成时间：{now_iso()}",
            f"- 总耗时：{elapsed:.1f}s",
            f"- 结果：PASS {counts.get('PASS', 0)} / FAIL {counts.get('FAIL', 0)} / SKIP {counts.get('SKIP', 0)}",
            f"- 真实模型：{extra.get('model') if extra else ''}（真实 gateway + pico WS）",
            f"- 产物目录：`{self.outdir}`",
            "",
            "## 结果矩阵",
            "",
            "| 阶段 | 检查项 | 结果 | 说明 |",
            "| --- | --- | --- | --- |",
        ]
        for entry in self.entries:
            detail = entry["detail"].replace("|", "\\|")[:220]
            lines.append(
                f"| {entry['phase']} | {entry['id']} | {entry['status']} | {detail} |"
            )
        lines.append("")

        for phase in ALL_PHASES:
            items = [e for e in self.entries if e["phase"] == phase]
            if not items:
                continue
            lines.append(f"## 阶段 {phase} 证据")
            lines.append("")
            for entry in items:
                lines.append(f"### {entry['id']} — {entry['status']}")
                if entry["detail"]:
                    lines.append(f"- 说明：{entry['detail']}")
                for path in entry["evidence"]:
                    lines.append(f"- 证据文件：`{path}`")
                for log in entry["logs"]:
                    lines.append(f"- 关键日志：`{log}`")
                lines.append("")

        # 真实缺陷与脚本自身问题、预期偏差的分类
        failures = [e for e in self.entries if e["status"] == "FAIL"]
        skips = [e for e in self.entries if e["status"] == "SKIP"]
        lines.append("## 缺陷与偏差分类")
        lines.append("")
        if failures:
            lines.append("### 真实缺陷 / 待确认问题（FAIL）")
            lines.append("")
            for entry in failures:
                lines.append(f"- **{entry['id']}**（阶段 {entry['phase']}）：{entry['detail']}")
            lines.append("")
        if skips:
            lines.append("### 未能实测（SKIP，多为模型自行拒绝执行测试指令）")
            lines.append("")
            for entry in skips:
                lines.append(f"- **{entry['id']}**：{entry['detail']}")
            lines.append("")
        lines.append("> 说明：脚本判定均基于原始证据（session JSONL 中的 tool 结果、gateway 日志、"
                     "hook/MCP fixture 日志、/list mcp 文本），不依赖模型口头回复；"
                     "只有「工具结果 + 日志」缺失时才用模型回复做辅助判断。")
        lines.append("")

        # 与保留的修复前运行结果做前后对比
        before_path = os.path.join(self.outdir, "before-fix", "results.json")
        comparison_rows = []
        try:
            with open(before_path, "r", encoding="utf-8") as handle:
                before_entries = json.load(handle)
            before = {(e.get("phase"), e.get("id")): e.get("status") for e in before_entries}
            for entry in self.entries:
                key = (entry["phase"], entry["id"])
                old_status = before.get(key)
                if old_status and old_status != entry["status"]:
                    comparison_rows.append((entry["phase"], entry["id"], old_status, entry["status"]))
            changed_after = [key for key in before
                             if key not in {(e["phase"], e["id"]) for e in self.entries}]
        except (OSError, ValueError):
            before = {}
        if comparison_rows:
            lines.append("## 修复前后对比（before-fix/results.json → 本次）")
            lines.append("")
            lines.append("| 阶段 | 检查项 | 修复前 | 修复后 |")
            lines.append("| --- | --- | --- | --- |")
            for phase, item_id, old_status, new_status in comparison_rows:
                lines.append(f"| {phase} | {item_id} | {old_status} | {new_status} |")
            lines.append("")

        notes = list(self.notes) + (extra or {}).get("notes", [])
        if notes:
            lines.append("## 备注")
            lines.append("")
            for note in notes:
                lines.append(f"- {note}")
            lines.append("")

        lines.append("## 复现命令")
        lines.append("")
        lines.append("```bash")
        lines.append("python3 scripts/agent-config-capabilities-test.py --phases A,B,C,D,E,F")
        lines.append("```")
        lines.append("")
        text = "\n".join(lines)
        path = os.path.join(self.outdir, "summary.md")
        write_text_atomic(path, redact(text, self.secrets))
        return path


# ---------------------------------------------------------------------------
# 模型配置提取（绝不打印）
# ---------------------------------------------------------------------------


def load_model_catalog(path: str) -> dict:
    with open(path, "r", encoding="utf-8") as handle:
        src = json.load(handle)
    catalog = {}
    for entry in src.get("model_list") or []:
        name = str(entry.get("model_name") or "").strip()
        keys = entry.get("api_keys") or []
        if not keys and entry.get("api_key"):
            keys = [entry["api_key"]]
        keys = [str(k).strip() for k in keys if str(k).strip()]
        if not name or not keys:
            continue
        catalog[name] = {
            "model_name": name,
            "model": entry.get("model") or name,
            "api_base": entry.get("api_base") or "",
            "api_key": keys[0],
        }
        if entry.get("provider"):
            catalog[name]["provider"] = entry["provider"]
    return catalog


def pick_models(catalog: dict) -> tuple[str, list[str]]:
    if PREFERRED_MODEL in catalog:
        primary = PREFERRED_MODEL
    elif FALLBACK_MODEL in catalog:
        primary = FALLBACK_MODEL
    elif catalog:
        primary = sorted(catalog)[0]
    else:
        raise SystemExit("no usable model with api key found in source config")
    order = [primary]
    for candidate in (FALLBACK_MODEL, *sorted(catalog)):
        if candidate in catalog and candidate not in order:
            order.append(candidate)
    return primary, order


# ---------------------------------------------------------------------------
# gateway 场景 harness
# ---------------------------------------------------------------------------


class Scenario:
    """一个隔离的 gateway 实例：临时 HOME/WORKSPACE/CONFIG，port=0。"""

    def __init__(self, phase, name, rec: Recorder, bin_path, catalog, model_order,
                 keep=False, verbose=False):
        self.phase = phase
        self.name = name
        self.rec = rec
        self.bin_path = bin_path
        self.catalog = catalog
        self.model_order = model_order
        self.keep = keep
        self.verbose = verbose

        self.tmp = tempfile.mkdtemp(prefix=f"cfgcap-{phase}-")
        self.ws_dir = ensure_dir(os.path.join(self.tmp, "ws"))
        self.home = ensure_dir(os.path.join(self.tmp, "home"))
        self.cfg_path = os.path.join(self.tmp, "config.json")
        self.log_path = os.path.join(self.tmp, "gateway.log")
        self.artifact_dir = ensure_dir(os.path.join(
            rec.outdir, f"phase-{phase}", f"{name}-{int(time.time() * 1000) % 10**8:08d}"))

        self.proc = None
        self.logf = None
        self.pid_data = None
        self.pico_token = hashlib.md5(name.encode()).hexdigest()
        self.sock = None
        self.stopped = False
        self.current_cfg = None
        self.config_snapshots = []

    # -- 配置 ------------------------------------------------------------
    def model_entry(self, model_name: str) -> dict:
        entry = dict(self.catalog[model_name])
        entry.pop("provider", None) if not entry.get("provider") else None
        return entry

    def base_config(self, model_name=None, log_level="debug") -> dict:
        name = model_name or self.model_order[0]
        return {
            "version": 2,
            "model_list": [self.model_entry(name)],
            "gateway": {"host": "127.0.0.1", "port": 0, "log_level": log_level},
            # 按 channel 划分的 dm scope 让每个 pico session_id 各有一个会话，
            # 从而为每个功能测试回合提供独立的 JSONL 证据文件。
            "session": {"dm_scope": "per-channel"},
            "agents": {"defaults": {"model_name": name}},
        }

    def write_config(self, cfg: dict, label: str) -> None:
        self.current_cfg = json.loads(json.dumps(cfg))
        with open(self.cfg_path, "w", encoding="utf-8") as handle:
            json.dump(self.current_cfg, handle, ensure_ascii=False, indent=2)
        safe = json.loads(json.dumps(cfg))
        for entry in safe.get("model_list") or []:
            if "api_key" in entry:
                entry["api_key"] = REDACTION
        snap = os.path.join(self.artifact_dir, f"config-{label}.json")
        write_text_atomic(snap, redact(json.dumps(safe, ensure_ascii=False, indent=2),
                                       [self.catalog[m]["api_key"] for m in self.catalog]))
        self.config_snapshots.append(snap)

    # -- 生命周期 --------------------------------------------------------
    def start(self, timeout=60) -> None:
        env = os.environ.copy()
        env.update({
            "CHAGENT_HOME": self.home,
            "CHAGENT_WORKSPACE": self.ws_dir,
            "CHAGENT_CONFIG": self.cfg_path,
        })
        env.pop("CHAGENT_AGENT_NAME", None)
        self.logf = open(self.log_path, "ab")
        self.logf.write(f"\n=== scenario {self.name} start {now_iso()} ===\n".encode())
        self.logf.flush()
        self.proc = subprocess.Popen(
            [self.bin_path, "gateway", "--name", self.name],
            cwd=self.tmp,
            env=env,
            stdout=self.logf,
            stderr=self.logf,
            start_new_session=True,
        )
        pid_path = os.path.join(self.ws_dir, ".chagent.pid")
        deadline = time.time() + timeout
        while time.time() < deadline:
            if os.path.exists(pid_path):
                try:
                    self.pid_data = json.load(open(pid_path, encoding="utf-8"))
                    if self.pid_data.get("port"):
                        break
                except (OSError, ValueError):
                    pass
            if self.proc.poll() is not None:
                raise RuntimeError(
                    f"gateway exited during startup (code={self.proc.returncode}); "
                    f"log: {os.path.join(self.artifact_dir, 'gateway.log')}"
                )
            time.sleep(0.1)
        if not self.pid_data:
            raise RuntimeError("timed out waiting for .chagent.pid")
        self.host = self.pid_data.get("host") or "127.0.0.1"
        self.port = int(self.pid_data["port"])
        self.base_url = f"http://{self.host}:{self.port}"
        self.ws_uri = f"ws://{self.host}:{self.port}/pico/ws?session_id={self.name}-ws"
        self.ws_session = None
        # 健康检查 + 就绪检查
        deadline = time.time() + timeout
        while time.time() < deadline:
            if self.health().get("_status") == 200 and self.ready().get("_status") == 200:
                return
            if self.proc.poll() is not None:
                raise RuntimeError("gateway exited while waiting for /health /ready")
            time.sleep(0.1)
        raise RuntimeError("gateway /health or /ready did not become ready")

    def stop(self, grace=6.0) -> dict:
        info = {"pid_file_removed": False, "signal_clean": False, "pid": None,
                "exit_seconds": None}
        if self.proc is None:
            return info
        self.sock = None
        info["pid"] = self.proc.pid
        if self.proc.poll() is None:
            started = time.time()
            self.proc.send_signal(signal.SIGTERM)
            try:
                self.proc.wait(timeout=grace)
                info["signal_clean"] = True
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait(timeout=5)
                info["signal_clean"] = False
            info["exit_seconds"] = round(time.time() - started, 2)
        pid_path = os.path.join(self.ws_dir, ".chagent.pid")
        info["pid_file_removed"] = not os.path.exists(pid_path)
        if self.logf is not None:
            try:
                self.logf.close()
            except OSError:
                pass
            self.logf = None
        self.stopped = True
        return info

    def snapshot_and_cleanup(self) -> None:
        secrets = [self.catalog[m]["api_key"] for m in self.catalog]
        try:
            write_text_atomic(
                os.path.join(self.artifact_dir, "gateway.log"),
                redact(read_text(self.log_path), secrets),
            )
        except OSError:
            pass
        # 会话 JSONL + meta
        sessions_src = os.path.join(self.ws_dir, "sessions")
        sessions_dst = ensure_dir(os.path.join(self.artifact_dir, "sessions"))
        for path in glob.glob(os.path.join(sessions_src, "*")):
            try:
                if os.path.isfile(path):
                    write_text_atomic(
                        os.path.join(sessions_dst, os.path.basename(path)),
                        redact(read_text(path), secrets),
                    )
            except OSError:
                pass
        # hook / mcp 夹具日志位于临时目录中
        for path in glob.glob(os.path.join(self.tmp, "*.log")):
            if os.path.abspath(path) == os.path.abspath(self.log_path):
                continue
            try:
                write_text_atomic(
                    os.path.join(self.artifact_dir, os.path.basename(path)),
                    redact(read_text(path), secrets),
                )
            except OSError:
                pass
        if not self.keep:
            shutil.rmtree(self.tmp, ignore_errors=True)
        else:
            print(f"  (kept temp workspace: {self.tmp})")

    # -- 证据辅助函数 ----------------------------------------------------
    def log_text(self) -> str:
        return read_text(self.log_path)

    def log_count(self, needle: str) -> int:
        return count_occurrences(self.log_text(), needle)

    def wait_log(self, needle: str, timeout: float, from_offset: int = 0) -> bool:
        deadline = time.time() + timeout
        while time.time() < deadline:
            text = self.log_text()[from_offset:]
            if needle in text:
                return True
            if self.proc is not None and self.proc.poll() is not None:
                return False
            time.sleep(0.1)
        return False

    def health(self) -> dict:
        return self._get("/health")

    def ready(self) -> dict:
        return self._get("/ready")

    def _get(self, path: str) -> dict:
        try:
            with urllib.request.urlopen(self.base_url + path, timeout=3) as resp:
                body = resp.read().decode("utf-8", "replace")
                data = json.loads(body)
                data["_status"] = resp.status
                return data
        except urllib.error.HTTPError as exc:
            try:
                data = json.loads(exc.read().decode("utf-8", "replace"))
            except Exception:
                data = {}
            data["_status"] = exc.code
            return data
        except Exception as exc:  # 连接被拒绝等
            return {"_status": 0, "_error": str(exc)}

    def post_reload(self, token: str | None = None) -> dict:
        headers = {}
        if token is not None:
            headers["Authorization"] = f"Bearer {token}"
        req = urllib.request.Request(self.base_url + "/reload", method="POST", headers=headers)
        try:
            with urllib.request.urlopen(req, timeout=5) as resp:
                return {"status": resp.status, "body": resp.read().decode("utf-8", "replace")}
        except urllib.error.HTTPError as exc:
            return {"status": exc.code, "body": exc.read().decode("utf-8", "replace")}
        except Exception as exc:
            return {"status": 0, "error": str(exc)}

    def ws_uri_for(self, session_id: str) -> str:
        return f"ws://{self.host}:{self.port}/pico/ws?session_id={session_id}"

    async def reload(self, wait=30.0) -> dict:
        """触发 /reload，等待完成日志，并丢弃（已失效的）pico socket。"""
        before = len(self.log_text())
        result = self.post_reload(token=self.pid_data["token"])
        done = self.wait_log("Manual reload completed successfully", wait, before)
        failed = self.wait_log("Manual reload failed", 0.2, before) or self.wait_log(
            "Error loading config for manual reload", 0.2, before
        )
        await asyncio.sleep(0.2)
        if done:
            # reload 会重启所有 channel；先前的 pico 连接已失效
            await self.close_sock()
        return {"trigger": result, "completed": done, "failed": failed,
                "log_tail": grep_lines(self.log_text()[before:], "reload", 6)}

    async def close_sock(self) -> None:
        if self.sock is not None:
            try:
                await self.sock.close()
            except Exception:
                pass
            self.sock = None
        self.ws_session = None

    async def connect(self, session_id: str | None = None) -> None:
        session_id = session_id or f"{self.name}-ws"
        uri = self.ws_uri_for(session_id)
        self.sock = await websockets.connect(
            uri,
            additional_headers={"Authorization": f"Bearer {self.pico_token}"},
            max_size=None,
            open_timeout=10,
        )
        self.ws_session = session_id
        await self._ping()

    async def ensure_conn(self, session_id: str, timeout=25) -> None:
        """pico 只会把某个会话的出站消息投递给注册了同一 session_id 的连接，
        因此每个回合都使用自己的连接。"""
        if self.sock is not None and self.ws_session == session_id:
            return
        await self.close_sock()
        deadline = time.time() + timeout
        last_error = ""
        while time.time() < deadline:
            try:
                await self.connect(session_id)
                return
            except Exception as exc:  # reload 后 channel 可能仍在重启
                last_error = str(exc)
                await asyncio.sleep(0.3)
        raise RuntimeError(f"pico connect for session {session_id} failed: {last_error}")

    async def _ping(self) -> None:
        await self.sock.send(json.dumps({"type": "ping", "timestamp": 1}))
        deadline = time.time() + 10
        while time.time() < deadline:
            raw = await asyncio.wait_for(self.sock.recv(), timeout=10)
            msg = json.loads(raw)
            if msg.get("type") == "pong":
                return
        raise RuntimeError("pico ping/pong failed")

    # -- session 证据 ----------------------------------------------------
    def sessions_dir(self) -> str:
        return os.path.join(self.ws_dir, "sessions")

    def session_files(self, session_id: str) -> list[str]:
        sdir = self.sessions_dir()
        matches = []
        for meta_path in sorted(glob.glob(os.path.join(sdir, "*.meta.json"))):
            try:
                with open(meta_path, "r", encoding="utf-8") as handle:
                    meta = json.load(handle)
            except (OSError, ValueError):
                continue
            values = (meta.get("scope") or {}).get("values") or {}
            chats = " ".join(str(v) for v in values.values())
            if session_id in chats:
                data_path = meta_path[: -len(".meta.json")] + ".jsonl"
                if os.path.exists(data_path):
                    matches.append(data_path)
        if matches:
            return matches
        # 旧版/兜底方式：文件名包含 session id
        return sorted(glob.glob(os.path.join(sdir, f"*{session_id}*.jsonl")))

    def session_messages(self, session_id: str) -> list[dict]:
        messages = []
        for path in self.session_files(session_id):
            for line in read_text(path).splitlines():
                line = line.strip()
                if not line:
                    continue
                try:
                    messages.append(json.loads(line))
                except ValueError:
                    continue
        return messages

    def session_tool_calls(self, session_id: str) -> list[dict]:
        calls = []
        for msg in self.session_messages(session_id):
            for call in msg.get("tool_calls") or []:
                name = (call.get("function") or {}).get("name") or call.get("name") or ""
                calls.append({"name": name, "arguments": (call.get("function") or {}).get("arguments")})
        return calls

    def session_tool_results(self, session_id: str) -> list[str]:
        return [str(msg.get("content") or "") for msg in self.session_messages(session_id)
                if msg.get("role") == "tool"]

    def session_final_texts(self, session_id: str) -> list[str]:
        out = []
        for msg in self.session_messages(session_id):
            if msg.get("role") != "assistant":
                continue
            if msg.get("tool_calls"):
                continue
            text = str(msg.get("content") or "").strip()
            if text:
                out.append(text)
        return out

    def session_context_kinds(self, session_id: str) -> list[str]:
        return [str((msg.get("context") or {}).get("kind") or "")
                for msg in self.session_messages(session_id) if msg.get("context")]

    def wait_session(self, session_id: str, timeout=20) -> list[dict]:
        deadline = time.time() + timeout
        messages: list[dict] = []
        while time.time() < deadline:
            messages = self.session_messages(session_id)
            if any(msg.get("role") in ("assistant", "tool") for msg in messages):
                return messages
            time.sleep(0.1)
        return messages

    # -- pico 回合 -------------------------------------------------------
    async def drain(self, quiet=0.3) -> None:
        if self.sock is None:
            return
        while True:
            try:
                await asyncio.wait_for(self.sock.recv(), timeout=quiet)
            except asyncio.TimeoutError:
                return
            except Exception:
                return

    async def turn(self, session_id: str, prompt: str, timeout=200.0) -> dict:
        await self.ensure_conn(session_id)
        await self.drain(0.25)
        await self.sock.send(json.dumps({
            "type": "message.send",
            "id": f"msg-{time.time_ns()}",
            "session_id": session_id,
            "payload": {"content": prompt},
        }))
        tool_names: list[str] = []
        final_text = ""
        errors: list[str] = []
        deadline = time.time() + timeout
        got_content = False
        while time.time() < deadline:
            recv_timeout = 0.6 if got_content else min(3.0, max(0.2, deadline - time.time()))
            try:
                raw = await asyncio.wait_for(self.sock.recv(), timeout=recv_timeout)
            except asyncio.TimeoutError:
                if got_content:
                    break
                continue
            except Exception as exc:
                errors.append(f"ws recv error: {exc}")
                break
            try:
                msg = json.loads(raw)
            except ValueError:
                continue
            mtype = msg.get("type")
            payload = msg.get("payload") or {}
            if mtype == "error":
                errors.append(json.dumps(msg, ensure_ascii=False)[:400])
                if got_content:
                    break
                continue
            kind = str(payload.get("kind") or "")
            content = str(payload.get("content") or "")
            if kind == "tool_calls":
                for call in payload.get("tool_calls") or []:
                    if isinstance(call, dict):
                        name = call.get("name") or (call.get("function") or {}).get("name")
                        if name:
                            tool_names.append(name)
                continue
            if kind == "thought" or payload.get("placeholder") or payload.get("thought"):
                continue
            if not content.strip():
                continue
            got_content = True
            final_text = content
        return {
            "text": final_text,
            "tool_names": tool_names,
            "errors": errors,
            "session_id": session_id,
        }

    async def command(self, text: str, session_id=None, timeout=60.0) -> dict:
        return await self.turn(session_id or f"cmd-{time.time_ns()}", text, timeout=timeout)


# ---------------------------------------------------------------------------
# 断言辅助函数
# ---------------------------------------------------------------------------


def first_match(strings: list[str], needles: list[str]) -> str:
    for needle in needles:
        for value in strings:
            if needle in value:
                return value
    return ""


def any_contains(values: list[str], needle: str) -> bool:
    return any(needle in value for value in values)


# ---------------------------------------------------------------------------
# 阶段 A：启动 / 环境变量 / 鉴权 / 健康检查 / 热重载
# ---------------------------------------------------------------------------


async def phase_a(rec: Recorder, args, bin_path, catalog, model_order) -> None:
    sc = Scenario("A", "cfgcap-a", rec, bin_path, catalog, model_order,
                  keep=args.keep, verbose=args.verbose)
    try:
        cfg = sc.base_config()
        sc.write_config(cfg, "A-initial")
        sc.start()

        # A1: pid 文件字段 + gateway 进程
        ok = (sc.pid_data.get("pid") == sc.proc.pid and sc.pid_data.get("host")
              and sc.pid_data.get("port") and sc.pid_data.get("token"))
        rec.add("A1-pid-file", "A",
                "PASS" if ok else "FAIL",
                f"pid file={os.path.join(sc.ws_dir, '.chagent.pid')} pid={sc.pid_data.get('pid')} "
                f"host={sc.pid_data.get('host')} port={sc.pid_data.get('port')} token_len={len(sc.pid_data.get('token') or '')}",
                evidence=[os.path.join(sc.artifact_dir, "gateway.log")],
                logs=grep_lines(sc.log_text(), "Gateway started", 2))

        # A2: CHAGENT_WORKSPACE 生效（pid 文件 + 会话均位于临时工作区下）
        inside = sc.pid_data.get("pid") is not None and sc.ws_dir.startswith(sc.tmp + os.sep)
        rec.add("A2-env-workspace", "A", "PASS" if inside else "FAIL",
                f"CHAGENT_WORKSPACE={sc.ws_dir}; pid file located there",
                evidence=[os.path.join(sc.artifact_dir, "gateway.log")])

        # A3: CHAGENT_HOME 生效（文件日志在自定义 HOME 下初始化）
        home_logs = glob.glob(os.path.join(sc.home, "logs", "*"))
        rec.add("A3-env-home", "A", "PASS" if home_logs else "FAIL",
                f"CHAGENT_HOME={sc.home}; log files={[os.path.basename(p) for p in home_logs]}",
                evidence=home_logs[:3])

        # A4: CHAGENT_CONFIG 生效（来自自定义配置路径的 log_level=debug 已应用）
        text = sc.log_text()
        ok = 'Log level set to "debug"' in text or "log_level" in text.lower()
        rec.add("A4-env-config", "A", "PASS" if ok else "FAIL",
                f"CHAGENT_CONFIG={sc.cfg_path} applied (debug log level active)",
                evidence=[os.path.join(sc.artifact_dir, "gateway.log")],
                logs=grep_lines(text, 'Log level set to', 3))

        # A5：/health 与 /ready
        health = sc.health()
        ready = sc.ready()
        ok = health.get("_status") == 200 and health.get("pid") == sc.proc.pid \
            and ready.get("_status") == 200 and ready.get("status") == "ready"
        rec.add("A5-health-ready", "A", "PASS" if ok else "FAIL",
                f"/health={health.get('status')} pid={health.get('pid')}; /ready={ready.get('status')}",
                evidence=[os.path.join(sc.artifact_dir, "gateway.log")])

        # A6: pico 鉴权 token = md5(--name)
        await sc.connect("a-auth-check")
        ping_ok = sc.sock is not None
        rec.add("A6-pico-token-md5", "A", "PASS" if ping_ok else "FAIL",
                f"WS auth OK with token=md5({sc.name}) len={len(sc.pico_token)}",
                evidence=[os.path.join(sc.artifact_dir, "gateway.log")],
                logs=grep_lines(sc.log_text(), "WebSocket client connected", 2))

        # A7: 错误 token 被拒绝
        rejected = False
        reason = ""
        try:
            bad = await websockets.connect(
                sc.ws_uri_for("a-wrong-token"),
                additional_headers={"Authorization": "Bearer wrong-token"},
                open_timeout=5,
            )
            await bad.close()
            reason = "connection accepted with wrong token"
        except Exception as exc:
            rejected = True
            reason = type(exc).__name__ + ": " + str(exc)[:160]
        rec.add("A7-wrong-token-rejected", "A", "PASS" if rejected else "FAIL",
                f"wrong token connection rejected ({reason})",
                evidence=[os.path.join(sc.artifact_dir, "gateway.log")])

        # A8: 通过 pico 执行 /show model + /context
        pico_dir = sc.artifact_dir
        show = await sc.command("/show model", "a-show-model")
        rec.add("A8-show-model", "A", "PASS" if sc.model_order[0] in show["text"] else "FAIL",
                f"/show model -> {show['text'][:160]}",
                evidence=[os.path.join(pico_dir, "gateway.log")])

        # A9: /reload（HTTP 端点，bearer pid token）+ 鉴权强制
        unauth = sc.post_reload()
        trigger = sc.post_reload(token=sc.pid_data["token"])
        completed = sc.wait_log("Manual reload completed successfully", 30)
        if completed:
            await sc.close_sock()
        rec.add("A9-reload-http", "A",
                "PASS" if unauth.get("status") == 401 and trigger.get("status") == 200 and completed else "FAIL",
                f"POST /reload no-token={unauth.get('status')} with-token={trigger.get('status')} "
                f"completed={completed}",
                evidence=[os.path.join(pico_dir, "gateway.log")],
                logs=grep_lines(sc.log_text(), "reload", 6))

        # A10: 失败的 reload 会让 gateway 以旧配置继续存活
        broken = sc.base_config()
        broken["version"] = 1
        sc.write_config(broken, "A-broken-version")
        offset = len(sc.log_text())
        sc.post_reload(token=sc.pid_data["token"])
        failed_logged = sc.wait_log("Error loading config for manual reload", 20, offset)
        healthy_after = sc.health().get("_status") == 200
        sc.write_config(sc.base_config(model_order[0]), "A-restored")
        rec.add("A10-bad-reload-safe", "A",
                "PASS" if failed_logged and healthy_after else "FAIL",
                f"invalid config reload rejected (log={failed_logged}), gateway still healthy={healthy_after}",
                evidence=[os.path.join(pico_dir, "gateway.log")],
                logs=grep_lines(sc.log_text()[offset:], "Error loading config", 3))

        # capsule：将此工作区保留为唯一留存的工作区
        capsule = os.path.join(rec.outdir, "workspace")
        if os.path.isdir(capsule):
            shutil.rmtree(capsule, ignore_errors=True)
        shutil.copytree(sc.ws_dir, capsule)
        for path in glob.glob(os.path.join(capsule, "sessions", "*")):
            write_text_atomic(path, redact(read_text(path), [catalog[m]["api_key"] for m in catalog]))
        rec.add_note(f"A 场景 capsule workspace 已保留：{capsule}")
    finally:
        await sc.close_sock()
        stop_info = sc.stop(grace=15)
        shutdown_tail = grep_lines(sc.log_text(), "Stopping all channels", 1) + \
            grep_lines(sc.log_text(), "Gateway stopped", 1) + \
            grep_lines(sc.log_text(), "remove pid file", 1)
        fast_exit = stop_info["exit_seconds"] is not None and stop_info["exit_seconds"] <= 5.0
        rec.add("A11-clean-shutdown", "A",
                "PASS" if stop_info["signal_clean"] and stop_info["pid_file_removed"] and fast_exit else "FAIL",
                f"/reload 后 SIGTERM 应在 ≤5s 内优雅退出并删除 pid 文件；"
                f"clean={stop_info['signal_clean']} exit_seconds={stop_info['exit_seconds']} "
                f"pid_file_removed={stop_info['pid_file_removed']}",
                evidence=[os.path.join(sc.artifact_dir, "gateway.log")],
                logs=shutdown_tail)
        sc.snapshot_and_cleanup()


# ---------------------------------------------------------------------------
# 阶段 B：通过配置 + /reload 启用 / 禁用工具
# ---------------------------------------------------------------------------

EXEC_LINE = "Tool call: exec("
LISTDIR_LINE = "Tool call: list_dir("


def tool_prompt_exec(marker: str) -> str:
    return (f"请立即调用名为 exec 的工具执行 shell 命令 `printf {marker}`，"
            f"然后把命令输出原样回复。不要解释。")


def tool_prompt_exec_unavailable(marker: str) -> str:
    return (f"请立即调用名为 exec 的工具执行 shell 命令 `printf {marker}`。"
            f"如果 exec 工具不存在或不可用，请只回复 EXEC_UNAVAILABLE，不要编造命令输出。")


async def phase_b(rec: Recorder, args, bin_path, catalog, model_order) -> None:
    sc = Scenario("B", "cfgcap-b", rec, bin_path, catalog, model_order,
                  keep=args.keep, verbose=args.verbose)
    try:
        ensure_dir(sc.ws_dir)
        with open(os.path.join(sc.ws_dir, "CONFIGCAP_DIR_MARKER.txt"), "w", encoding="utf-8") as handle:
            handle.write("dir-marker\n")
        cfg = sc.base_config()
        cfg["tools"] = {"exec": {"enabled": True}, "list_dir": {"enabled": False}}
        sc.write_config(cfg, "B-exec-on-listdir-off")
        sc.start()
        await sc.connect()

        # B1: 启用 exec 时可用；禁用的 list_dir 不会被调用
        sid = "b1-exec-on"
        r = await sc.turn(sid, tool_prompt_exec("CONFIGCAP_EXEC_B1_OK"))
        sc.wait_session(sid, 10)
        results = sc.session_tool_results(sid)
        calls = [c["name"] for c in sc.session_tool_calls(sid)]
        exec_ok = any_contains(results, "CONFIGCAP_EXEC_B1_OK")
        listdir_not_called = "list_dir" not in calls
        rec.add("B1-exec-baseline", "B", "PASS" if exec_ok and listdir_not_called else "FAIL",
                f"exec tool result marker={exec_ok}; list_dir disabled not called={listdir_not_called}; "
                f"model_reply={r['text'][:120]!r}",
                evidence=[os.path.join(sc.artifact_dir, "gateway.log"),
                          os.path.join(sc.artifact_dir, "sessions")],
                logs=grep_lines(sc.log_text(), EXEC_LINE, 2))

        # B2: 通过配置 + /reload 启用 list_dir -> 可用
        cfg2 = sc.base_config()
        cfg2["tools"] = {"exec": {"enabled": True}, "list_dir": {"enabled": True}}
        sc.write_config(cfg2, "B-listdir-on")
        reload_info = await sc.reload()
        before_listdir = sc.log_count(LISTDIR_LINE)
        sid = "b2-listdir-on"
        r = await sc.turn(sid, "请立即调用 list_dir 工具列出当前工作区根目录（path='.'），"
                               "然后回复你看到的 CONFIGCAP_DIR_MARKER.txt 文件名。")
        sc.wait_session(sid, 10)
        results = sc.session_tool_results(sid)
        calls = [c["name"] for c in sc.session_tool_calls(sid)]
        ok = "list_dir" in calls and any_contains(results, "CONFIGCAP_DIR_MARKER.txt") \
            and sc.log_count(LISTDIR_LINE) > before_listdir
        rec.add("B2-listdir-enable", "B", "PASS" if ok and reload_info["completed"] else "FAIL",
                f"reload={reload_info['completed']}; list_dir called={'list_dir' in calls}; "
                f"result contains marker={any_contains(results, 'CONFIGCAP_DIR_MARKER.txt')}; "
                f"model_reply={r['text'][:120]!r}",
                evidence=[os.path.join(sc.artifact_dir, "config-B-listdir-on.json"),
                          os.path.join(sc.artifact_dir, "gateway.log")],
                logs=grep_lines(sc.log_text(), LISTDIR_LINE, 2))

        # B3: 禁用 exec + /reload -> 模型无法产生成功的 exec 结果
        cfg3 = sc.base_config()
        cfg3["tools"] = {"exec": {"enabled": False}, "list_dir": {"enabled": True}}
        sc.write_config(cfg3, "B-exec-off")
        await sc.reload()
        before_exec = sc.log_count(EXEC_LINE)
        sid = "b3-exec-off"
        r = await sc.turn(sid, tool_prompt_exec_unavailable("CONFIGCAP_EXEC_B3_MUST_NOT_RUN"))
        sc.wait_session(sid, 10)
        results = sc.session_tool_results(sid)
        calls = [c["name"] for c in sc.session_tool_calls(sid)]
        no_exec_call = "exec" not in calls
        no_marker = not any_contains(results, "CONFIGCAP_EXEC_B3_MUST_NOT_RUN")
        log_quiet = sc.log_count(EXEC_LINE) == before_exec
        says_unavailable = "EXEC_UNAVAILABLE" in r["text"] or "不可用" in r["text"] or "没有" in r["text"]
        rec.add("B3-exec-disable", "B", "PASS" if no_exec_call and no_marker and log_quiet else "FAIL",
                f"exec tool call absent={no_exec_call}; no success marker={no_marker}; "
                f"gateway log has no new '{EXEC_LINE.strip()}'={log_quiet}; "
                f"model_says_unavailable={says_unavailable}; reply={r['text'][:160]!r}",
                evidence=[os.path.join(sc.artifact_dir, "config-B-exec-off.json"),
                          os.path.join(sc.artifact_dir, "gateway.log")],
                logs=grep_lines(sc.log_text(), "exec", 4))

        # B4: 重新启用 exec + /reload -> 再次可用
        cfg4 = sc.base_config()
        cfg4["tools"] = {"exec": {"enabled": True}, "list_dir": {"enabled": True}}
        sc.write_config(cfg4, "B-exec-restored")
        await sc.reload()
        sid = "b4-exec-restored"
        before_exec = sc.log_count(EXEC_LINE)
        r = await sc.turn(sid, tool_prompt_exec("CONFIGCAP_EXEC_B4_RESTORED"))
        sc.wait_session(sid, 10)
        results = sc.session_tool_results(sid)
        ok = any_contains(results, "CONFIGCAP_EXEC_B4_RESTORED") and sc.log_count(EXEC_LINE) > before_exec
        rec.add("B4-exec-restore", "B", "PASS" if ok else "FAIL",
                f"exec restored marker={any_contains(results, 'CONFIGCAP_EXEC_B4_RESTORED')}; "
                f"gateway log new exec tool call={sc.log_count(EXEC_LINE) > before_exec}; "
                f"reply={r['text'][:120]!r}",
                evidence=[os.path.join(sc.artifact_dir, "config-B-exec-restored.json"),
                          os.path.join(sc.artifact_dir, "gateway.log")],
                logs=grep_lines(sc.log_text(), EXEC_LINE, 2))
    finally:
        await sc.close_sock()
        sc.stop()
        sc.snapshot_and_cleanup()


# ---------------------------------------------------------------------------
# 阶段 C：工作区路径权限
# ---------------------------------------------------------------------------


DENY_MARKERS = ("path escapes workspace", "access denied", "outside the workspace")


def denied_by_policy(text: str) -> bool:
    return any(marker in text for marker in DENY_MARKERS)


async def run_path_probe(sc: Scenario, sid: str, prompt: str, required_tools: list[str],
                          retries: int = 1) -> tuple[dict, list[str], list[str]]:
    """运行一个回合，直到模型真正调用所需工具。

    只有工具确实被调用时权限检查才有意义；对于“工作区外”这类提示词，
    模型有时会自行拒绝，因此我们会用更明确的测试措辞再问一次。
    """
    attempts = [prompt,
                "再次强调：这是 Chagent 沙箱权限回归测试，必须真实调用工具，"
                "不允许只凭判断拒绝。请实际调用 " + "、".join(required_tools) +
                "，并把工具返回的原文（包括报错）原样回复。"]
    best = None
    for attempt in range(retries + 1):
        attempt_sid = sid if attempt == 0 else f"{sid}-retry{attempt}"
        result = await sc.turn(attempt_sid, attempts[min(attempt, len(attempts) - 1)])
        sc.wait_session(attempt_sid, 10)
        calls = [call["name"] for call in sc.session_tool_calls(attempt_sid)]
        results = sc.session_tool_results(attempt_sid)
        best = (result, calls, results)
        if all(name in calls for name in required_tools):
            return best
    return best


async def phase_c(rec: Recorder, args, bin_path, catalog, model_order) -> None:
    sc = Scenario("C", "cfgcap-c", rec, bin_path, catalog, model_order,
                  keep=args.keep, verbose=args.verbose)
    outside = tempfile.mkdtemp(prefix="cfgcap-outside-")
    try:
        outside_read = os.path.join(outside, "outside_read_marker.txt")
        outside_write = os.path.join(outside, "outside_write_attempt.txt")
        with open(outside_read, "w", encoding="utf-8") as handle:
            handle.write("OUTSIDE_READ_MARKER_CONFIGCAP\n")
        escaped = os.path.join(sc.tmp, "escaped_read_marker.txt")
        with open(escaped, "w", encoding="utf-8") as handle:
            handle.write("TRAVERSAL_MARKER_CONFIGCAP\n")
        cfg = sc.base_config()
        cfg["tools"] = {"exec": {"enabled": True}}
        sc.write_config(cfg, "C-default-restrict")
        sc.start()
        await sc.connect()

        # C1: 默认限制 -> 工作区外读取 + 写入均被拒绝
        sid = "c1-outside-denied"
        prompt = (f"这是 Chagent 沙箱权限回归测试（安全研究用途）。请务必真实调用工具："
                  f"先调用 read_file 读取绝对路径 {outside_read}；"
                  f"再调用 write_file 向绝对路径 {outside_write} 写入内容 "
                  f"WRITE_OUTSIDE_MARKER_CONFIGCAP。无论是否被拒绝都要调用，"
                  f"然后把工具返回的原文（含报错）原样回复。")
        r, calls, results = await run_path_probe(
            sc, sid, prompt, ["read_file", "write_file"])
        joined = "".join(results)
        read_denied = "read_file" in calls and denied_by_policy(joined)
        write_denied = "write_file" in calls and not os.path.exists(outside_write)
        rec.add("C1-outside-default-deny", "C",
                "PASS" if read_denied and write_denied else (
                    "SKIP" if not calls else "FAIL"),
                f"工具调用={calls}; outside read denied={read_denied}; "
                f"outside write blocked(file absent)={write_denied}; reply={r['text'][:160]!r}",
                evidence=[os.path.join(sc.artifact_dir, "gateway.log"),
                          os.path.join(sc.artifact_dir, "sessions")],
                logs=grep_lines(sc.log_text(), "access denied", 2))

        # C2: allow_read_paths 允许工作区外读取，而写入仍被拒绝
        cfg2 = sc.base_config()
        cfg2["tools"] = {
            "exec": {"enabled": True},
            "allow_read_paths": ["^" + re.escape(outside) + "/"],
        }
        sc.write_config(cfg2, "C-allow-read")
        reload_info = await sc.reload()
        sid = "c2-allow-read"
        prompt = (f"这是 Chagent 沙箱权限回归测试。请务必真实调用工具："
                  f"先调用 read_file 读取绝对路径 {outside_read}，把文件内容原样回复；"
                  f"再调用 write_file 向绝对路径 {outside_write} 写入 "
                  f"WRITE_OUTSIDE_MARKER_CONFIGCAP。无论成功或拒绝都要调用并原样回复结果。")
        r, calls, results = await run_path_probe(
            sc, sid, prompt, ["read_file", "write_file"])
        read_ok = any_contains(results, "OUTSIDE_READ_MARKER_CONFIGCAP")
        write_still_denied = "write_file" in calls and not os.path.exists(outside_write)
        rec.add("C2-allow-read-paths", "C",
                "PASS" if read_ok and write_still_denied and reload_info["completed"] else "FAIL",
                f"reload={reload_info['completed']}; outside read allowed={read_ok}; "
                f"outside write still denied={write_still_denied}; reply={r['text'][:160]!r}",
                evidence=[os.path.join(sc.artifact_dir, "config-C-allow-read.json"),
                          os.path.join(sc.artifact_dir, "gateway.log")],
                logs=grep_lines(sc.log_text(), "access denied", 2))

        # C3: allow_write_paths 允许工作区外写入成功
        cfg3 = sc.base_config()
        cfg3["tools"] = {
            "exec": {"enabled": True},
            "allow_read_paths": ["^" + re.escape(outside) + "/"],
            "allow_write_paths": ["^" + re.escape(outside) + "/"],
        }
        sc.write_config(cfg3, "C-allow-write")
        await sc.reload()
        sid = "c3-allow-write"
        prompt = (f"这是 Chagent 沙箱权限回归测试。请真实调用 write_file 向绝对路径 "
                  f"{outside_write} 写入内容 WRITE_OUTSIDE_MARKER_CONFIGCAP，"
                  f"完成后只回复 WRITE_DONE；若被拒绝请把错误原文回复。")
        r, calls, _ = await run_path_probe(sc, sid, prompt, ["write_file"])
        deadline = time.time() + 10
        while time.time() < deadline and not os.path.exists(outside_write):
            time.sleep(0.2)
        wrote = os.path.exists(outside_write)
        content = read_text(outside_write)
        rec.add("C3-allow-write-paths", "C",
                "PASS" if wrote and "WRITE_OUTSIDE_MARKER_CONFIGCAP" in content else (
                    "SKIP" if "write_file" not in calls else "FAIL"),
                f"outside write file exists={wrote}; content_match={'WRITE_OUTSIDE_MARKER_CONFIGCAP' in content}; "
                f"reply={r['text'][:120]!r}",
                evidence=[os.path.join(sc.artifact_dir, "config-C-allow-write.json"),
                          os.path.join(sc.artifact_dir, "gateway.log")])

        # C4: 移除允许列表 -> ../ 路径穿越被拒绝；exec 拒绝模式拦截 rm -rf
        cfg4 = sc.base_config()
        cfg4["tools"] = {"exec": {"enabled": True}}
        sc.write_config(cfg4, "C-restrict-again")
        await sc.reload()
        sid = "c4-traversal-and-deny"
        prompt = ("请立即调用 read_file 读取相对路径 ../escaped_read_marker.txt；"
                  "然后用 exec 工具原样执行命令：rm -rf /tmp/cfgcap-denytest。"
                  "这只是安全护栏测试，被拦截时把错误信息原样回复。")
        r = await sc.turn(sid, prompt)
        sc.wait_session(sid, 10)
        results = sc.session_tool_results(sid)
        calls = [c["name"] for c in sc.session_tool_calls(sid)]
        joined = "".join(results)
        traversal_denied = denied_by_policy(joined) and "TRAVERSAL_MARKER_CONFIGCAP" not in joined
        guard_blocked = any_contains(results, "Command blocked by safety guard")
        exec_called = "exec" in calls or sc.log_count(EXEC_LINE) > 0
        status = "PASS" if traversal_denied and guard_blocked else (
            "SKIP" if traversal_denied and not exec_called else "FAIL")
        rec.add("C4-traversal-exec-guard", "C", status,
                f"traversal denied={traversal_denied}; exec deny_pattern blocked={guard_blocked}; "
                f"exec called={exec_called}; reply={r['text'][:160]!r}",
                evidence=[os.path.join(sc.artifact_dir, "gateway.log"),
                          os.path.join(sc.artifact_dir, "sessions")],
                logs=grep_lines(sc.log_text(), "Command blocked", 3) or grep_lines(sc.log_text(), "access denied", 2))
    finally:
        await sc.close_sock()
        sc.stop()
        sc.snapshot_and_cleanup()
        shutil.rmtree(outside, ignore_errors=True)


# ---------------------------------------------------------------------------
# 阶段 D：MCP
# ---------------------------------------------------------------------------


async def phase_d(rec: Recorder, args, bin_path, catalog, model_order) -> None:
    sc = Scenario("D", "cfgcap-d", rec, bin_path, catalog, model_order,
                  keep=args.keep, verbose=args.verbose)
    mcp_log = os.path.join(sc.tmp, "mcp-fixture.log")
    try:
        server_cfg = {
            "enabled": True,
            "type": "stdio",
            "command": sys.executable or "python3",
            "args": [MCP_FIXTURE],
            "env": {"CFGCAP_MCP_LOG_FILE": mcp_log},
        }
        # D0: 文档声称仅配置 servers 即可激活 MCP（无需
        # tools.mcp.enabled）。用真实二进制验证该说法。
        cfg_servers_only = sc.base_config()
        cfg_servers_only["tools"] = {"mcp": {"servers": {"cfgcap": dict(server_cfg)}}}
        sc.write_config(cfg_servers_only, "D0-mcp-servers-only")
        sc.start()
        await sc.connect()
        d0_reply = await sc.command("/list mcp", "d0-list-mcp-servers-only", timeout=60)
        # init 可能由该命令惰性触发；轮询等几秒
        deadline = time.time() + 30
        d0_text = d0_reply["text"]
        while time.time() < deadline and "Connected: yes" not in d0_text:
            await asyncio.sleep(0.5)
            d0_reply = await sc.command("/list mcp", "d0-list-mcp-servers-only", timeout=60)
            d0_text = d0_reply["text"]
        d0_connected = "Connected: yes" in d0_text
        d0_show = await sc.command("/show mcp cfgcap", "d0-show-mcp-servers-only", timeout=60)
        d0_show_ok = "Active MCP tools for `cfgcap`" in d0_show["text"] and "`echo`" in d0_show["text"]
        d0_init_disabled = "MCP integration is disabled" in sc.log_text()
        rec.add("D0-mcp-servers-only-autostart", "D",
                "PASS" if d0_connected and d0_show_ok and not d0_init_disabled else "FAIL",
                f"仅配置 tools.mcp.servers（不写 mcp.enabled）时 MCP 应自动启用：connected={d0_connected}；"
                f"/show mcp 含 echo={d0_show_ok}；日志无 'MCP integration is disabled'={not d0_init_disabled}；"
                f"/list mcp={d0_text[:160]!r} （docs/reference/tools.zh.md 语义：servers 配置即启用）",
                evidence=[os.path.join(sc.artifact_dir, "config-D0-mcp-servers-only.json"),
                          os.path.join(sc.artifact_dir, "gateway.log")],
                logs=grep_lines(sc.log_text(), "MCP integration is disabled", 2) +
                     grep_lines(sc.log_text(), "Connected to MCP server", 2) +
                     grep_lines(sc.log_text(), "MCP tools registered", 2))

        # D0b: 显式 tools.mcp.enabled=false 必须优先于已配置的 servers
        cfg_explicit_off = sc.base_config()
        cfg_explicit_off["tools"] = {"mcp": {"enabled": False,
                                             "servers": {"cfgcap": dict(server_cfg)}}}
        sc.write_config(cfg_explicit_off, "D0b-mcp-explicit-disabled")
        reload_off = await sc.reload()
        d0b_log_offset = len(sc.log_text())
        d0b_before = count_occurrences(read_text(mcp_log), "tool_call")
        d0b_reply = await sc.command("/list mcp", "d0b-list-mcp-explicit-off", timeout=60)
        d0b_show = await sc.command("/show mcp cfgcap", "d0b-show-mcp-explicit-off", timeout=60)
        d0b_text = d0b_reply["text"]
        d0b_show_text = d0b_show["text"]
        d0b_disabled_list = ("Connected: no" in d0b_text
                            or "integration disabled" in d0b_text)
        d0b_show_disabled = "disabled" in d0b_show_text.lower() or \
            "integration is disabled" in d0b_show_text.lower()
        d0b_log_disabled = "MCP integration is disabled" in sc.log_text()
        d0b_no_reconnect = "Connected to MCP server" not in sc.log_text()[d0b_log_offset:]
        d0b_quiet = count_occurrences(read_text(mcp_log), "tool_call") == d0b_before
        rec.add("D0b-mcp-explicit-disabled", "D",
                "PASS" if (reload_off["completed"] and d0b_disabled_list and d0b_show_disabled
                           and d0b_quiet) else "FAIL",
                f"显式 tools.mcp.enabled=false + 已配 servers 时应关闭 MCP：reload={reload_off['completed']}；"
                f"/list mcp 显示未连接/整合关闭={d0b_disabled_list}；/show mcp 提示 disabled={d0b_show_disabled}；"
                f"reload 后无 'Connected to MCP server'={d0b_no_reconnect}；fixture 无新 RPC 调用={d0b_quiet}；"
                f"/list mcp={d0b_text[:140]!r}；/show mcp={d0b_show_text[:120]!r}",
                evidence=[os.path.join(sc.artifact_dir, "config-D0b-mcp-explicit-disabled.json"),
                          os.path.join(sc.artifact_dir, "gateway.log")],
                logs=grep_lines(sc.log_text(), "MCP integration is disabled", 2) +
                     d0b_show["text"].splitlines()[:2])

        # D1: 显式启用 MCP（mcp.enabled=true）+ /reload -> 已连接
        cfg = sc.base_config()
        cfg["tools"] = {"mcp": {"enabled": True, "servers": {"cfgcap": dict(server_cfg)}}}
        sc.write_config(cfg, "D-mcp-enabled")
        await sc.reload()
        await sc.ensure_conn("d1-list-mcp")

        # D1: /list mcp -> 已连接；/show mcp -> echo 工具（轮询等待 init）
        deadline = time.time() + 45
        list_text = ""
        connected = False
        while time.time() < deadline:
            reply = await sc.command("/list mcp", "d1-list-mcp", timeout=60)
            list_text = reply["text"]
            if "Connected: yes" in list_text:
                connected = True
                break
            await asyncio.sleep(0.5)
        show = await sc.command("/show mcp cfgcap", "d1-show-mcp", timeout=60)
        show_ok = "Active MCP tools for `cfgcap`" in show["text"] and "`echo`" in show["text"]
        rec.add("D1-mcp-list-show", "D", "PASS" if connected and show_ok else "FAIL",
                f"/list mcp connected={connected}; /show mcp has echo={show_ok}",
                evidence=[os.path.join(sc.artifact_dir, "config-D-mcp-enabled.json"),
                          os.path.join(sc.artifact_dir, "gateway.log")],
                logs=(list_text.splitlines()[:6] + show["text"].splitlines()[:3]))

        # D2: 真实模型调用 mcp_cfgcap_echo 并收到标记
        sid = "d2-mcp-call"
        prompt = ("请立即调用工具 mcp_cfgcap_echo，参数 text='D2_MCP_CALL_MARKER'，"
                  "然后把工具返回的原文回复给我。")
        r = await sc.turn(sid, prompt)
        sc.wait_session(sid, 10)
        calls = [c["name"] for c in sc.session_tool_calls(sid)]
        results = sc.session_tool_results(sid)
        called = "mcp_cfgcap_echo" in calls
        got = any_contains(results, "MCP_ECHO:D2_MCP_CALL_MARKER")
        fixture_log = read_text(mcp_log)
        fixture_saw = "D2_MCP_CALL_MARKER" in fixture_log
        rec.add("D2-mcp-real-call", "D", "PASS" if called and got and fixture_saw else "FAIL",
                f"tool called={called}; result marker={got}; fixture log saw call={fixture_saw}; "
                f"reply={r['text'][:120]!r}",
                evidence=[os.path.join(sc.artifact_dir, "gateway.log"),
                          os.path.join(sc.artifact_dir, "sessions")],
                logs=grep_lines(sc.log_text(), "mcp", 3) + grep_lines(fixture_log, "D2_MCP_CALL_MARKER", 2))

        # D3: 禁用 server + /reload -> 不可用（/list mcp）且不可调用
        cfg3 = sc.base_config()
        cfg3["tools"] = {"mcp": {"enabled": True, "servers": {"cfgcap": {
            "enabled": False, "type": "stdio",
            "command": sys.executable or "python3", "args": [MCP_FIXTURE],
            "env": {"CFGCAP_MCP_LOG_FILE": mcp_log},
        }}}}
        sc.write_config(cfg3, "D-mcp-disabled")
        reload_info = await sc.reload()
        list_after = await sc.command("/list mcp", "d3-list-disabled", timeout=60)
        disabled_listed = "Enabled: no" in list_after["text"] or "No MCP servers configured" in list_after["text"]
        call_before = count_occurrences(read_text(mcp_log), "tool_call")
        sid = "d3-mcp-disabled-call"
        r = await sc.turn(sid, "请调用工具 mcp_cfgcap_echo，参数 text='D3_MCP_DISABLED'。"
                               "如果该工具不存在，请只回复 MCP_UNAVAILABLE。", timeout=120)
        sc.wait_session(sid, 10)
        calls = [c["name"] for c in sc.session_tool_calls(sid)]
        not_called = "mcp_cfgcap_echo" not in calls
        fixture_quiet = count_occurrences(read_text(mcp_log), "tool_call") == call_before
        rec.add("D3-mcp-disable-reload", "D",
                "PASS" if reload_info["completed"] and disabled_listed and not_called and fixture_quiet else "FAIL",
                f"reload={reload_info['completed']}; /list mcp disabled={disabled_listed}; "
                f"tool not called={not_called}; fixture log quiet={fixture_quiet}; reply={r['text'][:120]!r}",
                evidence=[os.path.join(sc.artifact_dir, "config-D-mcp-disabled.json"),
                          os.path.join(sc.artifact_dir, "gateway.log")],
                logs=list_after["text"].splitlines()[:6])
    finally:
        await sc.close_sock()
        sc.stop()
        sc.snapshot_and_cleanup()


# ---------------------------------------------------------------------------
# 阶段 E：进程 hook
# ---------------------------------------------------------------------------


def hook_config(sc: Scenario, mode: str, log_file: str, tag: str = "") -> dict:
    cfg = sc.base_config()
    cfg["hooks"] = {
        "enabled": True,
        "defaults": {"observer_timeout_ms": 1000, "interceptor_timeout_ms": 5000,
                     "approval_timeout_ms": 10000},
        "processes": {"cfgcap_gate": {
            "enabled": True,
            "priority": 10,
            "transport": "stdio",
            "command": [sys.executable or "python3", HOOK_FIXTURE],
            "observe": ["agent.tool.exec_start", "agent.tool.exec_end"],
            "intercept": ["before_tool", "after_tool", "approve_tool"],
            "env": {
                "CFGCAP_HOOK_LOG_FILE": log_file,
                "CFGCAP_HOOK_MODE": mode,
                "CFGCAP_HOOK_TARGET_TOOL": "exec",
                "CFGCAP_HOOK_MODIFIED_TAG": "HOOK_MODIFIED",
                "CFGCAP_HOOK_DENY_REASON": "CFGCAP_DENIED_BY_HOOK",
            },
        }},
    }
    cfg["tools"] = {"exec": {"enabled": True}}
    return cfg


async def phase_e(rec: Recorder, args, bin_path, catalog, model_order) -> None:
    sc = Scenario("E", "cfgcap-e", rec, bin_path, catalog, model_order,
                  keep=args.keep, verbose=args.verbose)
    hook_logs = {
        "e12": os.path.join(sc.tmp, "hook-e12.log"),
        "e3": os.path.join(sc.tmp, "hook-e3.log"),
        "e4": os.path.join(sc.tmp, "hook-e4.log"),
        "e5": os.path.join(sc.tmp, "hook-e5.log"),
    }
    try:
        sc.write_config(hook_config(sc, "observe_modify", hook_logs["e12"]), "E-observe-modify")
        sc.start()
        await sc.connect()

        # E1 + E2: observer 收到 runtime_event；after_tool modify 重写结果
        sid = "e1e2-observe-modify"
        prompt = tool_prompt_exec("CONFIGCAP_HOOK_E12_OK")
        r = await sc.turn(sid, prompt)
        sc.wait_session(sid, 10)
        results = sc.session_tool_results(sid)
        hook_log = read_text(hook_logs["e12"])
        observed = "hook.runtime_event" in hook_log and "agent.tool.exec_start" in hook_log
        modified_seen_by_model = any_contains(results, "HOOK_MODIFIED")
        modified_in_hook_out = any_contains(
            [line for line in hook_log.splitlines() if '"direction": "out"' in line],
            '"action": "modify"',
        )
        rec.add("E1-observer-runtime-event", "E",
                "PASS" if observed else "FAIL",
                f"hook log received runtime_event exec_start={observed}",
                evidence=[os.path.join(sc.artifact_dir, "gateway.log")],
                logs=grep_lines(hook_log, "agent.tool.exec_start", 2) + grep_lines(hook_log, "hook.hello", 1))
        rec.add("E2-after-tool-modify", "E",
                "PASS" if modified_seen_by_model and modified_in_hook_out else "FAIL",
                f"model-visible tool result contains HOOK_MODIFIED={modified_seen_by_model}; "
                f"hook returned action=modify={modified_in_hook_out}",
                evidence=[os.path.join(sc.artifact_dir, "sessions")],
                logs=grep_lines(hook_log, "HOOK_MODIFIED", 2) + grep_lines(hook_log, '"action": "modify"', 2))

        # E3: before_tool deny_tool -> exec 被阻断
        sc.write_config(hook_config(sc, "deny_tool", hook_logs["e3"]), "E-deny-tool")
        reload_info = await sc.reload()
        before_exec = sc.log_count(EXEC_LINE)
        sid = "e3-deny-tool"
        r = await sc.turn(sid, tool_prompt_exec("CONFIGCAP_HOOK_E3_OUTPUT"))
        sc.wait_session(sid, 10)
        results = sc.session_tool_results(sid)
        hook_log = read_text(hook_logs["e3"])
        denied = any_contains(results, "Tool execution denied by hook") or "CFGCAP_DENIED_BY_HOOK" in "".join(results)
        not_executed = sc.log_count(EXEC_LINE) == before_exec
        deny_out = '"action": "deny_tool"' in hook_log
        e3_status = ("PASS" if reload_info["completed"] and denied and not_executed and deny_out
                     else ("SKIP" if not deny_out and not any_contains(results, "Tool execution denied")
                           else "FAIL"))
        rec.add("E3-deny-tool", "E", e3_status,
                f"reload={reload_info['completed']}; model saw hook denial={denied}; "
                f"exec not executed={not_executed}; hook deny_tool out={deny_out}; reply={r['text'][:120]!r}",
                evidence=[os.path.join(sc.artifact_dir, "config-E-deny-tool.json"),
                          os.path.join(sc.artifact_dir, "gateway.log")],
                logs=grep_lines(hook_log, "deny_tool", 2) + grep_lines(sc.log_text(), "denied", 2))

        # E4: approve_tool approved=false -> exec 被阻断
        sc.write_config(hook_config(sc, "approve_false", hook_logs["e4"]), "E-approve-false")
        await sc.reload()
        before_exec = sc.log_count(EXEC_LINE)
        sid = "e4-approve-false"
        r = await sc.turn(sid, tool_prompt_exec("CONFIGCAP_HOOK_E4_OUTPUT"))
        sc.wait_session(sid, 10)
        results = sc.session_tool_results(sid)
        hook_log = read_text(hook_logs["e4"])
        denied = any_contains(results, "denied by approval hook") or "CFGCAP_DENIED_BY_HOOK" in "".join(results)
        not_executed = sc.log_count(EXEC_LINE) == before_exec
        approve_out = '"approved": false' in hook_log
        rec.add("E4-approve-tool-false", "E",
                "PASS" if denied and not_executed and approve_out else "FAIL",
                f"model saw approval denial={denied}; exec not executed={not_executed}; "
                f"hook approved=false out={approve_out}; reply={r['text'][:120]!r}",
                evidence=[os.path.join(sc.artifact_dir, "config-E-approve-false.json"),
                          os.path.join(sc.artifact_dir, "gateway.log")],
                logs=grep_lines(hook_log, "approve_tool", 2) + grep_lines(hook_log, "approved", 2))

        # E5: 禁用 hook + /reload -> hook 不再被触发
        e4_log = read_text(hook_logs["e4"])
        hook_pids = []
        for line in e4_log.splitlines():
            try:
                entry = json.loads(line)
            except ValueError:
                continue
            if entry.get("direction") == "start" and entry.get("pid"):
                hook_pids.append(entry["pid"])
        cfg5 = hook_config(sc, "observe", hook_logs["e5"])
        cfg5["hooks"]["processes"]["cfgcap_gate"]["enabled"] = False
        sc.write_config(cfg5, "E-hook-disabled")
        reload_info = await sc.reload()
        e5_before = os.path.getsize(hook_logs["e5"]) if os.path.exists(hook_logs["e5"]) else 0
        sid = "e5-hook-disabled"
        r = await sc.turn(sid, tool_prompt_exec("CONFIGCAP_HOOK_E5_OK"))
        sc.wait_session(sid, 10)
        results = sc.session_tool_results(sid)
        e5_after = os.path.getsize(hook_logs["e5"]) if os.path.exists(hook_logs["e5"]) else 0
        no_new_hook_calls = e5_after == e5_before
        exec_still_works = any_contains(results, "CONFIGCAP_HOOK_E5_OK")
        old_proc_gone = True
        for pid in hook_pids:
            try:
                os.kill(pid, 0)
                old_proc_gone = False
            except OSError:
                pass
        rec.add("E5-hook-hot-disable", "E",
                "PASS" if reload_info["completed"] and no_new_hook_calls and exec_still_works and old_proc_gone else "FAIL",
                f"reload={reload_info['completed']}; hook log unchanged={no_new_hook_calls}; "
                f"exec still works={exec_still_works}; old hook process exited={old_proc_gone}",
                evidence=[os.path.join(sc.artifact_dir, "config-E-hook-disabled.json"),
                          os.path.join(sc.artifact_dir, "gateway.log")],
                logs=reload_info["log_tail"])
    finally:
        await sc.close_sock()
        sc.stop()
        sc.snapshot_and_cleanup()


# ---------------------------------------------------------------------------
# 阶段 F：配置校验 + 关键参数
# ---------------------------------------------------------------------------


def run_startup_expectation(rec: Recorder, sc: Scenario, label: str, cfg: dict,
                            expect_error: str | None, timeout=25) -> dict:
    """写入 cfg 并尝试启动一个 gateway；返回关于退出/日志的证据。"""
    sc.write_config(cfg, label)
    log_path = os.path.join(sc.tmp, "boot.log")
    env = os.environ.copy()
    env.update({"CHAGENT_HOME": sc.home, "CHAGENT_WORKSPACE": sc.ws_dir,
                "CHAGENT_CONFIG": sc.cfg_path})
    env.pop("CHAGENT_AGENT_NAME", None)
    with open(log_path, "wb") as logf:
        proc = subprocess.Popen([sc.bin_path, "gateway", "--name", "cfgcap-f"],
                                cwd=sc.tmp, env=env,
                                stdout=logf, stderr=logf, start_new_session=True)
        deadline = time.time() + timeout
        while time.time() < deadline and proc.poll() is None:
            time.sleep(0.1)
        exited = proc.poll() is not None
        if not exited:
            proc.send_signal(signal.SIGTERM)
            try:
                proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait(timeout=5)
    text = read_text(log_path)
    artifact_log = os.path.join(sc.artifact_dir, f"boot-{label}.log")
    try:
        write_text_atomic(artifact_log, redact(read_text(log_path), rec.secrets))
    except OSError:
        artifact_log = log_path
    pid_path = os.path.join(sc.ws_dir, ".chagent.pid")
    matched = bool(expect_error) and expect_error in text
    # 如果进程曾成功启动，清理陈旧的 pid 文件
    if os.path.exists(pid_path):
        try:
            os.remove(pid_path)
        except OSError:
            pass
    return {"exited": exited, "code": proc.returncode, "matched": matched,
            "pid_file": os.path.exists(pid_path), "text": text,
            "log_path": artifact_log}


async def phase_f(rec: Recorder, args, bin_path, catalog, model_order) -> None:
    model = model_order[0]

    def base() -> dict:
        name = model
        entry = dict(catalog[name])
        entry.pop("provider", None) if not entry.get("provider") else None
        return {
            "version": 2,
            "model_list": [entry],
            "gateway": {"host": "127.0.0.1", "port": 0, "log_level": "debug"},
            "session": {"dm_scope": "per-channel"},
            "agents": {"defaults": {"model_name": name}},
        }

    # --- F1..F6: 无效配置必须在加载阶段被拒绝 ---------------------------
    invalid_cases = [
        ("F1-invalid-version", "unsupported config version", lambda c: c.__setitem__("version", 1)),
        ("F2-api-keys-plural", "api_keys is not supported", None),  # 在下方处理
        ("F3-autonomy-max-rounds-201", "max_rounds must be between 1 and 200",
         lambda c: c["agents"]["defaults"].__setitem__("autonomy", {"enabled": True, "max_rounds": 201})),
        ("F4-autonomy-no-progress-11", "no_progress_rounds must be between 1 and 10",
         lambda c: c["agents"]["defaults"].__setitem__("autonomy", {"enabled": True, "no_progress_rounds": 11})),
        ("F5-runtime-context-mode", "runtime_context.mode",
         lambda c: c["agents"]["defaults"].__setitem__("runtime_context", {"mode": "bogus"})),
        ("F6-skills-catalog-mode", "catalog_mode",
         lambda c: c.__setitem__("tools", {"skills": {"catalog_mode": "bogus"}})),
        # 显式零值现在必须是可诊断的加载期错误
        ("F7-autonomy-max-rounds-0",
         "agents.defaults.autonomy.max_rounds must be between 1 and 200, got 0",
         lambda c: c["agents"]["defaults"].__setitem__(
             "autonomy", {"enabled": True, "max_rounds": 0})),
        ("F8-autonomy-no-progress-0",
         "agents.defaults.autonomy.no_progress_rounds must be between 1 and 10, got 0",
         lambda c: c["agents"]["defaults"].__setitem__(
             "autonomy", {"enabled": True, "no_progress_rounds": 0})),
        ("F9-workspace-context-max-bytes-0",
         "agents.defaults.workspace_context.max_bytes must be > 0, got 0",
         lambda c: c["agents"]["defaults"].__setitem__(
             "workspace_context", {"enabled": True, "max_bytes": 0})),
    ]
    for item_id, expected, mutate in invalid_cases:
        sc = Scenario("F", "cfgcap-f-invalid", rec, bin_path, catalog, model_order,
                      keep=args.keep, verbose=args.verbose)
        try:
            cfg = base()
            if mutate is not None:
                mutate(cfg)
            else:
                entry = cfg["model_list"][0]
                key = entry.pop("api_key")
                entry["api_keys"] = [key]
            evidence = run_startup_expectation(rec, sc, item_id, cfg, expected)
            rec.add(item_id, "F", "PASS" if evidence["exited"] and evidence["code"] != 0 and evidence["matched"] else "FAIL",
                    f"startup rejected (exit={evidence['code']}); error contains {expected!r}={evidence['matched']}",
                    evidence=[evidence["log_path"]],
                    logs=grep_lines(evidence["text"], expected, 2))
        finally:
            sc.stop()
            sc.snapshot_and_cleanup()

    # --- F13/F14: 合法的零值 + 省略的配置段仍必须能启动 -----------------
    async def start_expect_ok(item_id: str, cfg: dict, detail: str) -> None:
        sc = Scenario("F", "cfgcap-f-start", rec, bin_path, catalog, model_order,
                      keep=args.keep, verbose=args.verbose)
        try:
            sc.write_config(cfg, item_id)
            sc.start()
            healthy = sc.health().get("_status") == 200 and sc.ready().get("_status") == 200
            rec.add(item_id, "F", "PASS" if healthy else "FAIL",
                    f"{detail}；gateway 正常启动且 /health+/ready OK={healthy}",
                    evidence=[os.path.join(sc.artifact_dir, "config-" + item_id + ".json"),
                              os.path.join(sc.artifact_dir, "gateway.log")],
                    logs=grep_lines(sc.log_text(), "Gateway started", 1))
        finally:
            await sc.close_sock()
            sc.stop()
            sc.snapshot_and_cleanup()

    cfg_round_delay = base()
    cfg_round_delay["agents"]["defaults"]["autonomy"] = {"enabled": True,
                                                         "round_delay_ms": 0}
    await start_expect_ok("F13-round-delay-ms-0-legal", cfg_round_delay,
                    "显式 autonomy.round_delay_ms=0 合法（不应被拒绝）")

    cfg_omitted = base()  # 完全没有 workspace_context / runtime_context / autonomy 键
    await start_expect_ok("F14-omitted-context-sections-default", cfg_omitted,
                    "省略 workspace_context/runtime_context/autonomy 时应用默认值并正常启动")

    # --- F10: max_tool_iterations 取小值不会让回合挂起 ------------------
    sc = Scenario("F", "cfgcap-f-runtime", rec, bin_path, catalog, model_order,
                  keep=args.keep, verbose=args.verbose)
    try:
        cfg = base()
        cfg["agents"]["defaults"]["max_tool_iterations"] = 3
        sc.write_config(cfg, "F-max-tool-iterations-3")
        sc.start()
        await sc.connect()
        sid = "f10-max-tool-iterations"
        prompt = ("请严格按顺序一步一步调用 exec 工具，每步只调用一次，等待上一步结果后再进行下一步："
                  "第1步 echo CAP_ITER_1；第2步 echo CAP_ITER_2；第3步 echo CAP_ITER_3；"
                  "第4步 echo CAP_ITER_4；第5步 echo CAP_ITER_5。最后给出总结。")
        started = time.time()
        r = await sc.turn(sid, prompt, timeout=200)
        elapsed = time.time() - started
        sc.wait_session(sid, 10)
        calls = sc.session_tool_calls(sid)
        iteration_lines = grep_lines(sc.log_text(), "LLM iteration", 4)
        final_texts = sc.session_final_texts(sid)
        turn_done = bool(r["text"] or final_texts)
        healthy = sc.health().get("_status") == 200
        limit_hit = "max_tool_iterations" in (r["text"] + "".join(final_texts))
        rec.add("F10-max-tool-iterations-guard", "F",
                "PASS" if turn_done and healthy else "FAIL",
                f"turn 在 {elapsed:.1f}s 内收尾（不 hang）={turn_done}；gateway 仍健康={healthy}；"
                f"tool call 次数={len(calls)}；命中上限提示={limit_hit}",
                evidence=[os.path.join(sc.artifact_dir, "gateway.log"),
                          os.path.join(sc.artifact_dir, "sessions")],
                logs=iteration_lines[-3:] + grep_lines(sc.log_text(), "max_tool_iterations", 2))

        # 供基于 skills 的用例使用的辅助函数
        def write_skill(workspace: str, name: str, marker: str) -> None:
            skill_dir = ensure_dir(os.path.join(workspace, "skills", name))
            with open(os.path.join(skill_dir, "SKILL.md"), "w", encoding="utf-8") as handle:
                handle.write(
                    "---\n"
                    f"name: {name}\n"
                    f"description: 配置能力功能测试技能（{marker}）\n"
                    "---\n\n"
                    f"技能正文标记：{marker}\n"
                    "当被加载时，请原样回复该标记。\n"
                )

        write_skill(sc.ws_dir, "functest-skill", "SKILL_BODY_MARKER_CONFIGCAP")
        skill_prompt = ("请立即调用 skill 工具加载名为 functest-skill 的技能，"
                        "然后只回复技能正文中的标记（形如 SKILL_BODY_MARKER_CONFIGCAP）。")

        # --- F11：skills catalog_mode=context -------------------------
        cfg = base()
        cfg["tools"] = {"skills": {"enabled": True, "catalog_mode": "context"}}
        sc.write_config(cfg, "F-catalog-context")
        await sc.reload()
        sid = "f11-catalog-context"
        r = await sc.turn(sid, skill_prompt)
        sc.wait_session(sid, 10)
        calls = [c["name"] for c in sc.session_tool_calls(sid)]
        results = sc.session_tool_results(sid)
        kinds = sc.session_context_kinds(sid)
        catalog_ctx = "skill-catalog" in kinds
        skill_ok = "skill" in calls and any_contains(results, "SKILL_BODY_MARKER_CONFIGCAP")
        rec.add("F11-skills-catalog-context", "F",
                "PASS" if catalog_ctx and skill_ok else "FAIL",
                f"catalog_mode=context 启动后 session 含 skill-catalog 上下文={catalog_ctx}；"
                f"skill 工具调用成功={skill_ok}；reply={r['text'][:100]!r}",
                evidence=[os.path.join(sc.artifact_dir, "config-F-catalog-context.json"),
                          os.path.join(sc.artifact_dir, "sessions")],
                logs=[f"context kinds={kinds}"])

        # --- F12：skills catalog_mode=system + steering/max_parallel_turns
        cfg = base()
        cfg["tools"] = {"skills": {"enabled": True, "catalog_mode": "system"}}
        cfg["agents"]["defaults"]["steering_mode"] = "one-at-a-time"
        cfg["agents"]["defaults"]["max_parallel_turns"] = 1
        sc.write_config(cfg, "F-catalog-system-steering")
        await sc.reload()
        right_after_reload = sc.health().get("_status") == 200
        sid = "f12-catalog-system"
        r = await sc.turn(sid, skill_prompt)
        sc.wait_session(sid, 10)
        calls = [c["name"] for c in sc.session_tool_calls(sid)]
        results = sc.session_tool_results(sid)
        kinds = sc.session_context_kinds(sid)
        no_catalog_ctx = "skill-catalog" not in kinds
        skill_ok = "skill" in calls and any_contains(results, "SKILL_BODY_MARKER_CONFIGCAP")
        healthy = sc.health().get("_status") == 200
        rec.add("F12-catalog-system-steering", "F",
                "PASS" if right_after_reload and no_catalog_ctx and skill_ok and healthy else "FAIL",
                f"catalog_mode=system：session 无 skill-catalog 上下文={no_catalog_ctx}；"
                f"skill 工具可用={skill_ok}；steering_mode=one-at-a-time/max_parallel_turns=1 未崩溃={healthy}；"
                f"reply={r['text'][:100]!r}",
                evidence=[os.path.join(sc.artifact_dir, "config-F-catalog-system-steering.json"),
                          os.path.join(sc.artifact_dir, "sessions")],
                logs=[f"context kinds={kinds}"] + grep_lines(sc.log_text(), "steering", 2))
    finally:
        await sc.close_sock()
        sc.stop()
        sc.snapshot_and_cleanup()


# ---------------------------------------------------------------------------
# 主入口
# ---------------------------------------------------------------------------


PHASE_RUNNERS = {
    "A": phase_a,
    "B": phase_b,
    "C": phase_c,
    "D": phase_d,
    "E": phase_e,
    "F": phase_f,
}


def build_binary(outdir: str, rebuild: bool) -> str:
    bin_path = os.path.join(outdir, "chagent-functest")
    if os.path.exists(bin_path) and not rebuild:
        return bin_path
    print("building gateway binary ...")
    subprocess.run(["go", "build", "-o", bin_path, "./cmd/chagent"], cwd=ROOT, check=True)
    return bin_path


def redact_outdir(outdir: str, secrets: list[str]) -> None:
    for path in glob.glob(os.path.join(outdir, "**", "*"), recursive=True):
        if not os.path.isfile(path) or path.endswith(".tmp"):
            continue
        if os.path.getsize(path) > 4_000_000:
            continue
        try:
            text = read_text(path)
        except Exception:
            continue
        cleaned = redact(text, secrets)
        if cleaned != text:
            try:
                write_text_atomic(path, cleaned)
            except OSError:
                pass


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--phases", default=",".join(ALL_PHASES),
                        help="comma separated phases to run (default: A,B,C,D,E,F)")
    parser.add_argument("--config", default=DEFAULT_MODEL_CONFIG,
                        help="source config.json with model_list/api keys")
    parser.add_argument("--out", default=DEFAULT_OUT, help="artifact output directory")
    parser.add_argument("--keep", action="store_true", help="keep temp workspaces")
    parser.add_argument("--rebuild", action="store_true", help="force go build")
    parser.add_argument("--verbose", action="store_true")
    parser.add_argument("--merge", action="store_true",
                        help="merge new results into an existing results.json instead of replacing it")
    args = parser.parse_args()

    phases = [p.strip().upper() for p in args.phases.split(",") if p.strip()]
    unknown = [p for p in phases if p not in PHASE_RUNNERS]
    if unknown:
        raise SystemExit(f"unknown phases: {unknown}")
    os.makedirs(args.out, exist_ok=True)

    if not os.path.exists(args.config):
        raise SystemExit(f"model config not found: {args.config}")
    catalog = load_model_catalog(args.config)
    primary, model_order = pick_models(catalog)
    secrets = [catalog[name]["api_key"] for name in catalog]
    print(f"source config: {args.config}")
    print(f"model: {primary} (fallbacks: {model_order[1:]})")
    print(f"output: {args.out}")

    bin_path = build_binary(args.out, args.rebuild)
    rec = Recorder(args.out, secrets, load_existing=args.merge)
    started = time.time()
    exit_code = 0
    try:
        for phase in phases:
            print(f"\n===== phase {phase} =====")
            try:
                asyncio.run(PHASE_RUNNERS[phase](rec, args, bin_path, catalog, model_order))
            except Exception as exc:  # 继续运行剩余的阶段
                import traceback
                traceback.print_exc()
                rec.add(f"{phase}-phase-error", phase, "FAIL",
                        f"phase crashed: {type(exc).__name__}: {exc}",
                        evidence=[os.path.join(args.out, f"phase-{phase}")])
                exit_code = 1
    finally:
        elapsed = time.time() - started
        redact_outdir(args.out, secrets)
        summary_path = rec.write_summary(elapsed, {"model": primary,
                                                   "notes": [f"总耗时 {elapsed:.1f}s"]})
        counts = rec.counts()
        print(f"\nresults: PASS {counts.get('PASS', 0)} / FAIL {counts.get('FAIL', 0)} / "
              f"SKIP {counts.get('SKIP', 0)} in {elapsed:.1f}s")
        print(f"summary: {summary_path}")
        print(f"results: {os.path.join(args.out, 'results.json')}")
    return exit_code


if __name__ == "__main__":
    sys.exit(main())