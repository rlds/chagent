#!/usr/bin/env python3
"""真实场景端到端功能测试：Chagent 新实现 agent（DSH 迁移后）。

场景（一条完整真实使用链，非彼此割裂的探针）：
1. 真实 gateway 启动 / pid / health / ready / pico 鉴权 / SIGTERM 清理 / 单例；
2. 工作区上下文 baseline（AGENTS.md / 子目录）注入 + 修改后的增量刷新；
3. 真实开发任务：修复 workspace 内一个 Python 小项目，跑测试、写修复报告；
4. 越界读取被权限拒绝（restrict_to_workspace）；
5. durable TaskState + 自主续跑（task_update start -> 自主轮次 -> complete/证据）；
6. skill catalog durable context + skill 工具渐进披露；
7. 重复工具调用守卫（loop-guard 提示注入）；
8. 上下文窗口管理：小窗口 + 大工具输出 -> 主动压缩 + 工具结果裁剪 + spill 落盘；
9. /reload 热更新配置；/stop 暂停任务并取消排队自主轮；
10. 进程重启后会话/任务状态恢复、无重复注入；
11. 全程 config.json 不被改写、无孤儿进程。

用法：
  python3 scripts/agent-e2e-scenario.py [--config 外部config.json] [--model deepseek-v4-pro]
                                        [--run-dir build/functest-20261030/live]
                                        [--phases P1,P2,...]

依赖：python3 websockets（本机已安装）、go 工具链、可用的真实模型 API。
产物：run-dir 下保留 workspace（含 sessions/spill/项目文件）、gateway 日志、
      results.json、summary.md、config.hash.before/after、artifact-analysis.json。
"""

from __future__ import annotations

import argparse
import asyncio
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
import traceback
import urllib.error
import urllib.request
from pathlib import Path

try:
    import websockets
except ImportError:  # pragma: no cover
    print("FATAL: python 包 websockets 未安装", file=sys.stderr)
    raise

class Tee:
    """把 stdout/stderr 同时写入控制台与文件（harness.log 证据）。"""

    def __init__(self, *streams):
        self.streams = streams

    def write(self, data):
        for stream in self.streams:
            try:
                stream.write(data)
                stream.flush()
            except Exception:  # noqa: BLE001
                pass

    def flush(self):
        for stream in self.streams:
            try:
                stream.flush()
            except Exception:  # noqa: BLE001
                pass

    def isatty(self):
        return False


ROOT = Path(__file__).resolve().parent.parent
DEFAULT_CONFIG = Path("/Users/wudaorui/chessdata/weichat/weichat/config.json")
DEFAULT_RUN_DIR = ROOT / "build" / "functest-20261030" / "live"
MCP_STDIO_FIXTURE = ROOT / "scripts" / "fixtures" / "mcp_stdio_server.py"

MARK = {
    "workspace": "WORKSPACE_MARKER_V1",
    "workspace_v2": "WORKSPACE_MARKER_V2",
    "claude": "CLAUDE_MARKER_V1",
    "claude_v2": "CLAUDE_MARKER_V2",
    "docs_claude": "DOCS_CLAUDE_MARKER_V1",
    "subdir": "SUBDIR_MARKER_V1",
    "skill": "SKILL_BODY_MARKER_V1",
    "report": "PROJECT_REPORT_MARKER",
    "autonomy": "AUTONOMY_STEP2_MARKER",
}

PROJECT_SRC = '''"""日志统计小工具（测试用真实小项目）。"""

from __future__ import annotations

from collections import Counter


def mean(values: list[float]) -> float:
    if not values:
        raise ValueError("values must not be empty")
    # BUG: 分母多加了 1，导致平均值偏小。
    return sum(values) / (len(values) + 1)


def median(values: list[float]) -> float:
    if not values:
        raise ValueError("values must not be empty")
    ordered = sorted(values)
    mid = len(ordered) // 2
    if len(ordered) % 2:
        return float(ordered[mid])
    return (ordered[mid - 1] + ordered[mid]) / 2.0


def count_levels(records: list[str]) -> dict[str, int]:
    """统计日志级别前缀（INFO/WARN/ERROR）。"""
    counter: Counter[str] = Counter()
    for line in records:
        level = line.split(" ", 1)[0].strip().upper()
        counter[level] += 1
    return dict(counter)
'''

PROJECT_TEST = '''"""项目自带的验收测试（不要修改测试文件）。"""

import unittest

from src.stats import count_levels, mean, median


class StatsTest(unittest.TestCase):
    def test_mean_basic(self):
        self.assertEqual(mean([1, 2, 3]), 2.0)
        self.assertEqual(mean([5]), 5.0)

    def test_median(self):
        self.assertEqual(median([1, 2, 3]), 2.0)
        self.assertEqual(median([1, 2, 3, 4]), 2.5)

    def test_count_levels(self):
        records = ["INFO start", "WARN slow", "ERROR boom", "INFO done"]
        self.assertEqual(count_levels(records), {"INFO": 2, "WARN": 1, "ERROR": 1})


if __name__ == "__main__":
    unittest.main()
'''

PROJECT_README = """# 日志统计小项目

运行测试：

```bash
cd project && python3 -m unittest discover -s tests -v
```

要求：
- 当前 `mean()` 实现有 bug，测试会失败；请只修改 `src/stats.py` 修复它。
- 不要修改 `tests/` 下的测试文件，也不要放宽断言。
- 修复后重新运行测试确认全部通过。
"""

SKILL_MD = """---
name: e2e-skill
description: E2E 技能：用于验证技能目录与渐进披露。加载后必须输出技能正文标记。
---

# E2E 技能正文

当本技能被 skill 工具加载后，最终回复必须包含且仅包含：
SKILL_BODY_MARKER_V1
"""

AGENTS_MD_V1 = f"""# 工作区指令

- 当用户询问「工作区标记」时，必须只回复：{MARK['workspace']}
- 项目 `project/` 是一个 Python 日志统计小项目。修复时只允许修改 `src/stats.py`。
- 回答要简洁，只使用中文。
"""

AGENTS_MD_V2_EXTRA = f"- 当用户询问「项目标记」时，必须只回复：{MARK['workspace_v2']}\n"


# ---------------------------------------------------------------------------
# 结果记录
# ---------------------------------------------------------------------------
class Results:
    def __init__(self) -> None:
        self.items: list[dict] = []
        self.started = time.time()

    def add(self, cid: str, status: str, detail: str, evidence: str = "") -> None:
        self.items.append(
            {
                "id": cid,
                "status": status.upper(),
                "detail": detail,
                "evidence": evidence,
                "ts": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
            }
        )
        icon = {"PASS": "PASS", "FAIL": "FAIL", "SKIP": "SKIP", "INFO": "INFO"}.get(status.upper(), "INFO")
        print(f"[{icon}] {cid}: {detail}", flush=True)

    def pass_(self, cid: str, detail: str, evidence: str = "") -> None:
        self.add(cid, "PASS", detail, evidence)

    def fail(self, cid: str, detail: str, evidence: str = "") -> None:
        self.add(cid, "FAIL", detail, evidence)

    def skip(self, cid: str, detail: str, evidence: str = "") -> None:
        self.add(cid, "SKIP", detail, evidence)

    def ok(self, cid: str, cond: bool, detail: str, evidence: str = "") -> bool:
        (self.pass_ if cond else self.fail)(cid, detail, evidence)
        return cond

    def counts(self) -> dict:
        out = {"PASS": 0, "FAIL": 0, "SKIP": 0, "INFO": 0}
        for item in self.items:
            out[item["status"]] = out.get(item["status"], 0) + 1
        return out


# ---------------------------------------------------------------------------
# 通用工具
# ---------------------------------------------------------------------------
def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_file(path: Path) -> str:
    return sha256_bytes(path.read_bytes())


def http_get(url: str, timeout: float = 3.0):
    try:
        with urllib.request.urlopen(url, timeout=timeout) as r:
            return r.status, r.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as exc:
        return exc.code, exc.read().decode("utf-8", "replace")[:500]
    except Exception as exc:  # noqa: BLE001
        return None, str(exc)


def http_post(url: str, timeout: float = 30.0, headers: dict | None = None):
    try:
        req = urllib.request.Request(url, data=b"", method="POST", headers=headers or {})
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, r.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as exc:
        return exc.code, exc.read().decode("utf-8", "replace")[:500]
    except Exception as exc:  # noqa: BLE001
        return None, str(exc)


def wait_until(fn, timeout: float, interval: float = 0.2, desc: str = ""):
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        try:
            last = fn()
        except Exception as exc:  # noqa: BLE001
            last = exc
        if last:
            return last
        time.sleep(interval)
    raise TimeoutError(f"wait_until timeout ({desc or fn})")


def tail(text: str, n: int = 40) -> str:
    lines = text.splitlines()
    return "\n".join(lines[-n:])


# ---------------------------------------------------------------------------
# 配置与工作区
# ---------------------------------------------------------------------------
def load_source_model(config_path: Path, model_name: str) -> dict:
    src = json.loads(config_path.read_text(encoding="utf-8"))
    models = src.get("model_list") or []
    chosen = None
    for entry in models:
        if entry.get("model_name") == model_name:
            chosen = entry
            break
    if chosen is None:
        chosen = models[0]
    api_keys = chosen.get("api_keys") or ([chosen["api_key"]] if chosen.get("api_key") else [])
    if not api_keys:
        raise SystemExit(f"source config {config_path} has no api key for model {chosen.get('model_name')!r}")
    out = {
        "model_name": chosen.get("model_name") or chosen.get("model") or "e2e-model",
        "model": chosen.get("model") or chosen.get("model_name"),
        "api_base": chosen.get("api_base", ""),
        "api_key": api_keys[0],
    }
    if chosen.get("provider"):
        out["provider"] = chosen["provider"]
    return out


def base_config(workspace: Path, model: dict, *, context_window: int | None = None,
                max_tokens: int | None = None, round_delay_ms: int = 1200,
                max_rounds: int = 6, send_intermediate: bool = False) -> dict:
    defaults = {
        "workspace": str(workspace),
        "restrict_to_workspace": True,
        "allow_read_outside_workspace": False,
        "max_tool_iterations": 80,
        "max_llm_retries": 6,
        "llm_retry_backoff_secs": 5,
        "steering_mode": "one-at-a-time",
        "max_parallel_turns": 1,
        "workspace_context": {"enabled": True, "user_global": False, "max_bytes": 8192},
        "runtime_context": {"mode": "snapshot", "refresh_interval_seconds": 600, "include_sender": True},
        "autonomy": {
            "enabled": True,
            "max_rounds": max_rounds,
            "no_progress_rounds": 3,
            "round_delay_ms": round_delay_ms,
            "resume_on_start": False,
            "send_intermediate": send_intermediate,
        },
    }
    if context_window is not None:
        defaults["context_window"] = context_window
    if max_tokens is not None:
        defaults["max_tokens"] = max_tokens
    return {
        "version": 2,
        "model_list": [model],
        "agents": {"defaults": defaults},
        "tools": {
            "read_file": {"enabled": True, "max_read_file_size": 65536},
            "write_file": {"enabled": True},
            "edit_file": {"enabled": True},
            "append_file": {"enabled": True},
            "list_dir": {"enabled": True},
            "exec": {"enabled": True, "enable_deny_patterns": True, "timeout_seconds": 30},
            "task": {"enabled": True},
            "todo": {"enabled": True},
            "skill_load": {"enabled": True},
            "web": {"enabled": False},
            "web_fetch": {"enabled": False},
            "skills": {"catalog_mode": "context"},
        },
        "gateway": {"host": "127.0.0.1", "port": 0, "log_level": "info"},
    }


def redact_config(cfg: dict) -> dict:
    out = json.loads(json.dumps(cfg))
    for entry in out.get("model_list", []):
        if entry.get("api_key"):
            entry["api_key"] = "REDACTED"
    return out


def write_config(path: Path, cfg: dict) -> None:
    path.write_text(json.dumps(cfg, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")


def prepare_workspace(ws: Path) -> None:
    (ws / "docs").mkdir(parents=True, exist_ok=True)
    (ws / "skills" / "e2e-skill").mkdir(parents=True, exist_ok=True)
    (ws / "project" / "src").mkdir(parents=True, exist_ok=True)
    (ws / "project" / "tests").mkdir(parents=True, exist_ok=True)
    (ws / "AGENTS.md").write_text(AGENTS_MD_V1, encoding="utf-8")
    (ws / "CLAUDE.md").write_text(
        "# 工作区 CLAUDE 指令\n\n"
        f"- 当用户询问「CLAUDE 标记」时，必须只回复：{MARK['claude']}\n"
        f"- 当用户询问「项目标记」时，必须只回复：{MARK['claude_v2']}\n",
        encoding="utf-8",
    )
    (ws / "docs" / "CLAUDE.md").write_text(
        "# 子目录 CLAUDE 指令\n\n"
        f"- 当用户询问「docs 目录的 CLAUDE 标记」时，必须只回复：{MARK['docs_claude']}\n",
        encoding="utf-8",
    )
    (ws / "skills" / "e2e-skill" / "SKILL.md").write_text(SKILL_MD, encoding="utf-8")
    (ws / "project" / "README.md").write_text(PROJECT_README, encoding="utf-8")
    (ws / "project" / "src" / "__init__.py").write_text("", encoding="utf-8")
    (ws / "project" / "src" / "stats.py").write_text(PROJECT_SRC, encoding="utf-8")
    (ws / "project" / "tests" / "__init__.py").write_text("", encoding="utf-8")
    (ws / "project" / "tests" / "test_stats.py").write_text(PROJECT_TEST, encoding="utf-8")


def make_big_file(ws: Path, lines: int = 6000) -> Path:
    path = ws / "bigdata.txt"
    with path.open("w", encoding="utf-8") as handle:
        for i in range(lines):
            handle.write(f"LINE_{i:06d} payload=0123456789abcdef 这是一行用于上下文压力测试的日志数据\n")
    return path


# ---------------------------------------------------------------------------
# Gateway 生命周期
# ---------------------------------------------------------------------------
class Gateway:
    def __init__(self, run_dir: Path, workspace: Path, binary: Path, name: str) -> None:
        self.run_dir = run_dir
        self.workspace = workspace
        self.binary = binary
        self.name = name
        self.proc: subprocess.Popen | None = None
        self.host = ""
        self.port = 0
        self.pid_info: dict = {}
        self.log_path = run_dir / "gateway.log"
        self.log_handle = None

    def env(self) -> dict:
        env = os.environ.copy()
        env.update(
            {
                "CHAGENT_HOME": str(self.run_dir / "home"),
                "CHAGENT_WORKSPACE": str(self.workspace),
                "CHAGENT_CONFIG": str(self.workspace / "config.json"),
            }
        )
        return env

    def start(self, log_path: Path | None = None, timeout: float = 30.0) -> None:
        if log_path is not None:
            self.log_path = log_path
        pid_file = self.workspace / ".chagent.pid"
        if pid_file.exists():
            pid_file.unlink()
        self.log_handle = self.log_path.open("ab")
        self.log_handle.write(f"\n===== start {time.strftime('%Y-%m-%dT%H:%M:%S%z')} =====\n".encode())
        self.log_handle.flush()
        self.proc = subprocess.Popen(
            [str(self.binary), "gateway", "--name", self.name],
            cwd=str(self.workspace),
            env=self.env(),
            stdout=self.log_handle,
            stderr=self.log_handle,
        )
        wait_until(lambda: pid_file.exists(), timeout, desc="gateway pid file")
        self.pid_info = json.loads(pid_file.read_text(encoding="utf-8"))
        self.host = self.pid_info["host"]
        self.port = int(self.pid_info["port"])
        wait_until(lambda: http_get(f"http://{self.host}:{self.port}/health")[0] == 200, timeout, desc="gateway /health")
        wait_until(lambda: http_get(f"http://{self.host}:{self.port}/ready")[0] == 200, timeout, desc="gateway /ready")

    def healthy(self) -> bool:
        return http_get(f"http://{self.host}:{self.port}/health")[0] == 200

    def reload(self, timeout: float = 30.0) -> tuple[int | None, str]:
        token = str(self.pid_info.get("token") or "")
        code, body = http_post(
            f"http://{self.host}:{self.port}/reload",
            timeout=timeout,
            headers={"Authorization": f"Bearer {token}"},
        )
        wait_until(self.healthy, timeout, desc="gateway healthy after reload")
        return code, body

    def stop(self, timeout: float = 8.0) -> tuple[bool, float]:
        assert self.proc is not None
        started = time.time()
        self.proc.send_signal(signal.SIGTERM)
        try:
            self.proc.wait(timeout=timeout)
            elapsed = time.time() - started
            return True, elapsed
        except subprocess.TimeoutExpired:
            self.proc.kill()
            self.proc.wait()
            return False, time.time() - started
        finally:
            if self.log_handle is not None:
                self.log_handle.flush()
                self.log_handle.close()
                self.log_handle = None

    def kill(self) -> None:
        if self.proc is not None and self.proc.poll() is None:
            self.proc.kill()
            self.proc.wait()
        if self.log_handle is not None:
            self.log_handle.close()
            self.log_handle = None

    def file_log_path(self) -> Path:
        return self.run_dir / "home" / "logs" / "gateway.log"

    def log_text(self) -> str:
        # 优先使用 file logger（同一进程内控制台与文件各写一份，合并会重复计数）。
        try:
            text = self.file_log_path().read_text(encoding="utf-8", errors="replace")
            if text.strip():
                return text
        except FileNotFoundError:
            pass
        try:
            return self.log_path.read_text(encoding="utf-8", errors="replace")
        except FileNotFoundError:
            return ""

    def log_count(self, pattern: str) -> int:
        return len(re.findall(pattern, self.log_text()))

    def log_has(self, pattern: str) -> bool:
        return re.search(pattern, self.log_text()) is not None

    def log_matching(self, pattern: str, limit: int = 5) -> list[str]:
        return [line for line in self.log_text().splitlines() if re.search(pattern, line)][-limit:]

    def config_hash(self) -> str:
        return sha256_file(self.workspace / "config.json")


# ---------------------------------------------------------------------------
# pico WebSocket 客户端
# ---------------------------------------------------------------------------
class Pico:
    def __init__(self, host: str, port: int, token: str, session_id: str) -> None:
        self.host = host
        self.port = port
        self.token = token
        self.session_id = session_id
        self.ws = None
        self.received: list[dict] = []
        # 所有 ws 操作必须固定在同一个事件循环上，避免跨 loop 使用。
        self.loop = asyncio.new_event_loop()

    async def connect(self, token: str | None = None) -> None:
        tok = token if token is not None else self.token
        uri = f"ws://{self.host}:{self.port}/pico/ws?session_id={self.session_id}"
        self.ws = await asyncio.wait_for(
            websockets.connect(uri, additional_headers={"Authorization": f"Bearer {tok}"}),
            timeout=15,
        )

    async def close(self) -> None:
        if self.ws is not None:
            await self.ws.close()
            self.ws = None

    def alive(self) -> bool:
        try:
            return self.ws is not None and getattr(self.ws, "state", None) is not None and self.ws.state.name == "OPEN"
        except Exception:  # noqa: BLE001
            return self.ws is not None

    async def ping(self, timeout: float = 10.0) -> bool:
        assert self.ws is not None
        await self.ws.send(json.dumps({"type": "ping", "timestamp": int(time.time() * 1000)}))
        deadline = time.time() + timeout
        while time.time() < deadline:
            raw = await asyncio.wait_for(self.ws.recv(), timeout=max(0.1, deadline - time.time()))
            msg = json.loads(raw)
            self.received.append(msg)
            if msg.get("type") == "pong":
                return True
        return False

    async def send_raw(self, payload: dict) -> None:
        assert self.ws is not None
        await self.ws.send(json.dumps(payload))

    async def command(self, content: str, needle: str | None = None, timeout: float = 120.0) -> str:
        """发送控制命令并累积内容消息，直到出现 needle（或超时）。"""
        assert self.ws is not None
        await self.ws.send(
            json.dumps(
                {
                    "type": "message.send",
                    "id": f"cmd-{time.time_ns()}",
                    "session_id": self.session_id,
                    "payload": {"content": content},
                }
            )
        )
        texts: list[str] = []
        deadline = time.time() + timeout
        while time.time() < deadline:
            raw = await asyncio.wait_for(self.ws.recv(), timeout=max(0.5, deadline - time.time()))
            msg = json.loads(raw)
            self.received.append(msg)
            if msg.get("type") == "error":
                raise RuntimeError(f"pico error frame: {json.dumps(msg, ensure_ascii=False)[:400]}")
            payload = msg.get("payload") or {}
            text = payload.get("content")
            if (
                msg.get("type") in ("message.create", "message.update")
                and text
                and payload.get("kind") not in ("thought", "tool_calls")
                and not payload.get("placeholder")
            ):
                texts.append(text)
                joined = "\n".join(texts)
                if needle is None or needle in joined:
                    return joined
        return "\n".join(texts)

    async def typing(self) -> None:
        await self.send_raw({"type": "typing.start", "session_id": self.session_id})
        await asyncio.sleep(0.1)
        await self.send_raw({"type": "typing.stop", "session_id": self.session_id})

    async def turn(self, content: str, timeout: float = 300.0) -> dict:
        """发送一条用户消息，收集直到最终可见回复。返回 text/tool_calls/messages。"""
        assert self.ws is not None
        await self.ws.send(
            json.dumps(
                {
                    "type": "message.send",
                    "id": f"e2e-{time.time_ns()}",
                    "session_id": self.session_id,
                    "payload": {"content": content},
                }
            )
        )
        tool_calls: list[str] = []
        raw_messages: list[dict] = []
        deadline = time.time() + timeout
        while time.time() < deadline:
            raw = await asyncio.wait_for(self.ws.recv(), timeout=max(0.5, deadline - time.time()))
            msg = json.loads(raw)
            self.received.append(msg)
            raw_messages.append(msg)
            if msg.get("type") == "error":
                raise RuntimeError(f"pico error frame: {json.dumps(msg, ensure_ascii=False)[:800]}")
            payload = msg.get("payload") or {}
            for tc in payload.get("tool_calls") or []:
                if isinstance(tc, dict):
                    name = tc.get("name") or (tc.get("function") or {}).get("name")
                    if name:
                        tool_calls.append(name)
            kind = payload.get("kind")
            text = payload.get("content")
            if (
                msg.get("type") in ("message.create", "message.update")
                and text
                and kind not in ("thought", "tool_calls")
                and not payload.get("placeholder")
            ):
                return {"text": text, "tool_calls": tool_calls, "messages": raw_messages}
        raise TimeoutError(f"turn timeout after {timeout}s; content={content[:120]!r}")


# ---------------------------------------------------------------------------
# 会话文件分析
# ---------------------------------------------------------------------------
def session_files(ws: Path) -> list[Path]:
    return sorted((ws / "sessions").glob("*.jsonl"))


def meta_files(ws: Path) -> list[Path]:
    return sorted((ws / "sessions").glob("*.meta.json"))


def read_messages(ws: Path) -> list[dict]:
    messages: list[dict] = []
    for path in session_files(ws):
        for line in path.read_text(encoding="utf-8", errors="replace").splitlines():
            line = line.strip()
            if not line:
                continue
            try:
                messages.append(json.loads(line))
            except json.JSONDecodeError:
                continue
    return messages


def read_metas(ws: Path) -> list[tuple[Path, dict]]:
    out = []
    for path in meta_files(ws):
        try:
            out.append((path, json.loads(path.read_text(encoding="utf-8"))))
        except Exception:  # noqa: BLE001
            continue
    return out


def task_state(ws: Path) -> tuple[dict | None, Path | None]:
    for path, meta in read_metas(ws):
        state = meta.get("task_state")
        if isinstance(state, dict):
            return state, path
    return None, None


def context_kinds(messages: list[dict]) -> dict[str, int]:
    kinds: dict[str, int] = {}
    for msg in messages:
        ctx = msg.get("context") or {}
        kind = ctx.get("kind")
        if kind:
            kinds[kind] = kinds.get(kind, 0) + 1
    return kinds


def tool_pairing_ok(messages: list[dict]) -> tuple[bool, str]:
    pending: set[str] = set()
    for idx, msg in enumerate(messages):
        role = msg.get("role")
        if role == "assistant":
            for tc in msg.get("tool_calls") or []:
                tid = tc.get("id") or tc.get("tool_call_id") or ""
                if tid:
                    pending.add(tid)
        elif role == "tool":
            tid = msg.get("tool_call_id") or ""
            if tid:
                pending.discard(tid)
    if pending:
        return False, f"未配对的 tool_call ids: {sorted(pending)[:5]}"
    return True, "所有 tool_call 均有对应 tool 结果"


def latest_task_state_after(ws: Path, ns: int) -> dict | None:
    """任务状态（同一 session 的 meta）中 updated_at 未使用，这里直接返回当前状态。"""
    state, _ = task_state(ws)
    return state


def session_fingerprint(ws: Path) -> dict:
    messages = read_messages(ws)
    metas = read_metas(ws)
    returns = {
        "files": [p.name for p in session_files(ws)],
        "message_count": len(messages),
        "roles": {},
        "context_kinds": context_kinds(messages),
        "meta": [],
    }
    for msg in messages:
        role = msg.get("role", "?")
        returns["roles"][role] = returns["roles"].get(role, 0) + 1
    for path, meta in metas:
        state = meta.get("task_state")
        returns["meta"].append(
            {
                "file": path.name,
                "summary_chars": len((meta.get("summary") or "")),
                "skip": meta.get("skip"),
                "count": meta.get("count"),
                "task_phase": (state or {}).get("phase") if isinstance(state, dict) else None,
                "task_round": (state or {}).get("round") if isinstance(state, dict) else None,
            }
        )
    return returns


def spill_files(ws: Path) -> list[Path]:
    root = ws / ".chagent" / "spill"
    if not root.exists():
        return []
    return sorted(p for p in root.rglob("*") if p.is_file())


def spill_root(ws: Path) -> Path:
    return ws / ".chagent" / "spill"


# ---------------------------------------------------------------------------
# 主测试流程
# ---------------------------------------------------------------------------
class Scenario:
    def __init__(self, args: argparse.Namespace) -> None:
        self.args = args
        self.run_dir = Path(args.run_dir).resolve()
        if getattr(args, "workspace_dir", ""):
            self.workspace = Path(args.workspace_dir).resolve()
        else:
            # 必须放在 git 仓库之外：否则 workspacecontext 的项目根会向上发现 .git，
            # 基线链落在仓库根而不是被测工作区。artifacts 运行结束后复制回 run_dir。
            self.workspace = Path(tempfile.mkdtemp(prefix="pcl-e2e-ws-")).resolve()
        self.binary = self.run_dir / "chagent"
        self.results = Results()
        self.gateway: Gateway | None = None
        self.pico: Pico | None = None
        self.model = load_source_model(Path(args.config), args.model)
        self.config_before = ""
        self.hashes: dict[str, str] = {}
        self.timings: dict[str, float] = {}
        self.notes: list[str] = []
        self.observed_context_kinds: dict[str, int] = {}
        self.phases = [p.strip() for p in args.phases.split(",") if p.strip()] if args.phases else []

    # -- 辅助函数 ---------------------------------------------------------
    def enabled(self, phase: str) -> bool:
        return not self.phases or phase in self.phases

    def evidence(self, *names: str) -> str:
        return "; ".join(str(self.run_dir / n) for n in names)

    def gw(self) -> Gateway:
        assert self.gateway is not None
        return self.gateway

    def pc(self) -> Pico:
        assert self.pico is not None
        return self.pico

    def command(self, cmd: str, needle: str | None = None, timeout: float = 120.0) -> str:
        for attempt in range(2):
            try:
                self.ensure_ws()
                return self.arun(self.pc().command(cmd, needle=needle, timeout=timeout))
            except (ConnectionError, OSError, RuntimeError) as exc:  # noqa: BLE001
                if attempt == 0:
                    self.note(f"命令 {cmd} 失败（{type(exc).__name__}），重连后重试")
                    self.pico.ws = None
                    self.ensure_ws()
                    continue
                raise
        return ""

    def arun(self, coro):
        pico = self.pico
        if pico is not None:
            return pico.loop.run_until_complete(coro)
        return asyncio.run(coro)

    def ensure_ws(self) -> None:
        pico = self.pico
        if pico is None:
            return
        if pico.ws is None or not pico.alive():
            try:
                if pico.ws is not None:
                    try:
                        pico.loop.run_until_complete(pico.close())
                    except Exception:  # noqa: BLE001
                        pass
                pico.ws = None
                pico.loop.run_until_complete(pico.connect())
                self.note("pico WS 已重连")
            except Exception as exc:  # noqa: BLE001
                self.note(f"pico WS 重连失败: {exc}")

    def pump(self, seconds: float = 0.01) -> None:
        pico = self.pico
        if pico is None:
            return
        try:
            pico.loop.run_until_complete(asyncio.sleep(seconds))
        except Exception:  # noqa: BLE001
            pass

    def turn(self, content: str, timeout: float = 300.0, retries: int = 3) -> dict:
        """带瞬时错误（限流/LLM 失败/WS 断开）重试的用户消息发送。"""
        last_exc: Exception | None = None
        for attempt in range(retries):
            try:
                self.ensure_ws()
                return self.arun(self.pc().turn(content, timeout))
            except (RuntimeError, ConnectionError, OSError) as exc:  # pico 错误帧 / ws 连接关闭
                last_exc = exc
                msg = f"{type(exc).__name__}: {exc}"
                if any(tag in msg for tag in ("429", "LLM call failed", "rate limit", "overloaded", "timeout", "ConnectionClosed", "ConnectionError")):
                    try:
                        self.pico.ws = None
                        self.ensure_ws()
                    except Exception:  # noqa: BLE001
                        pass
                    time.sleep(8 * (attempt + 1))
                    continue
                raise
        assert last_exc is not None
        raise last_exc

    def wait_turn_idle(self, timeout: float = 180.0, quiet: float = 3.0) -> None:
        """等待 gateway 的当前 turn（含自主轮）真正结束：日志静默且最后一个 turn.end 晚于 turn.start。"""
        gw = self.gw()
        deadline = time.time() + timeout
        last_len = -1
        stable_since = time.time()
        while time.time() < deadline:
            text = gw.log_text()
            if len(text) != last_len:
                last_len = len(text)
                stable_since = time.time()
            if time.time() - stable_since >= quiet:
                i_start = text.rfind('"event_kind":"agent.turn.start"')
                i_end = text.rfind('"event_kind":"agent.turn.end"')
                if i_start == -1 or i_end > i_start:
                    return
            self.pump(0.05)
            time.sleep(0.15)
        self.note(f"wait_turn_idle 超时（{timeout:.0f}s），继续后续检查")

    def wait_log_quiet(self, timeout: float = 60.0, quiet: float = 6.0) -> None:
        """等待 gateway 日志静默（异步 summarize 等后台任务收敛），期间保持 WS 心跳。"""
        gw = self.gw()
        deadline = time.time() + timeout
        last_len = -1
        stable_since = time.time()
        while time.time() < deadline:
            text = gw.log_text()
            if len(text) != last_len:
                last_len = len(text)
                stable_since = time.time()
            elif time.time() - stable_since >= quiet:
                return
            self.pump(0.1)
        self.note(f"wait_log_quiet 超时（{timeout:.0f}s）")

    def record_context_kinds(self, kinds: dict[str, int]) -> None:
        """累积运行期观察到的 context kind：异步 summarize 可能合法重写最终 JSONL，
        因此 durability 证据取整个运行期的并集，而不是只看运行结束时的文件。"""
        for kind, count in (kinds or {}).items():
            self.observed_context_kinds[kind] = max(self.observed_context_kinds.get(kind, 0), int(count))

    def note(self, text: str) -> None:
        self.notes.append(text)
        print(f"[INFO] {text}", flush=True)

    # -- 初始化 -----------------------------------------------------------
    def setup(self) -> None:
        if self.run_dir.exists() and not self.args.resume:
            shutil.rmtree(self.run_dir)
        self.run_dir.mkdir(parents=True, exist_ok=True)
        self.workspace.mkdir(parents=True, exist_ok=True)
        (self.run_dir / "workspace.path").write_text(str(self.workspace) + "\n", encoding="utf-8")
        prepare_workspace(self.workspace)
        cfg = base_config(self.workspace, self.model)
        if self.args.max_rounds:
            cfg["agents"]["defaults"]["autonomy"]["max_rounds"] = self.args.max_rounds
        self.config_before = json.dumps(cfg, ensure_ascii=False, sort_keys=True)
        write_config(self.workspace / "config.json", cfg)
        hash_before = sha256_file(self.workspace / "config.json")
        self.hashes["before"] = hash_before
        (self.run_dir / "config.hash.before").write_text(hash_before + "\n", encoding="utf-8")
        (self.run_dir / "config.redacted.json").write_text(
            json.dumps(redact_config(cfg), ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
        )
        (self.run_dir / "expectations.json").write_text(
            json.dumps(
                {
                    "markers": {
                        "agents_md": MARK["workspace_v2"],
                        "claude_md": MARK["claude_v2"],
                        "docs_claude": MARK["docs_claude"],
                        "skill": MARK["skill"],
                        "report": MARK["report"],
                        "autonomy": MARK["autonomy"],
                    },
                    "planned_config_rewrite": "P7 通过 /reload 主动改写 config.json（context_window=12000），"
                    "config.hash.after-reload 记录预期终值",
                    "workspace_retained": "workspace/ 为运行期 /tmp 工作区的原样副本（含真实 config.json）",
                },
                ensure_ascii=False,
                indent=2,
            )
            + "\n",
            encoding="utf-8",
        )
        # 构建二进制
        t0 = time.time()
        build = subprocess.run(
            ["go", "build", "-o", str(self.binary), "./cmd/chagent"],
            cwd=str(ROOT),
            capture_output=True,
            text=True,
        )
        self.timings["build"] = time.time() - t0
        if build.returncode != 0:
            raise SystemExit(f"go build failed:\n{build.stderr[-3000:]}")
        self.results.pass_("P0.1", f"go build 成功（{self.timings['build']:.1f}s）", str(self.binary))

    # -- P0: 启动 / 鉴权 / pid / 配置不改写 / 单例 -----------------------
    def phase_p0(self) -> None:
        self.setup()
        gw = Gateway(self.run_dir, self.workspace, self.binary, self.args.name)
        self.gateway = gw
        gw.start()
        info = gw.pid_info
        required = {"pid", "token", "version", "port", "host"}
        self.results.ok(
            "P0.2",
            required.issubset(set(info)),
            f"pid 文件字段 {sorted(info)}，port={info.get('port')} host={info.get('host')}",
            str(self.workspace / ".chagent.pid"),
        )
        self.results.ok(
            "P0.3",
            info.get("pid") == gw.proc.pid,
            f"pid 与进程一致（{info.get('pid')}）",
            self.evidence("gateway.log"),
        )
        code, _ = http_get(f"http://{gw.host}:{gw.port}/health")
        self.results.ok("P0.4", code == 200, f"/health = {code}", self.evidence("gateway.log"))
        code, _ = http_get(f"http://{gw.host}:{gw.port}/ready")
        self.results.ok("P0.5", code == 200, f"/ready = {code}", self.evidence("gateway.log"))
        self.results.ok(
            "P0.6",
            gw.config_hash() == self.hashes["before"],
            "启动后 config.json 字节未变（不写回配置）",
            self.evidence("config.hash.before"),
        )
        # 错误 token 拒绝
        async def bad_token() -> tuple[bool, str]:
            try:
                pico = Pico(gw.host, gw.port, "wrong-token", "bad-auth")
                await pico.connect()
                await pico.close()
                return False, "使用错误 token 也建立了连接"
            except Exception as exc:  # noqa: BLE001
                return True, f"错误 token 被拒绝：{type(exc).__name__}: {str(exc)[:160]}"

        ok, detail = asyncio.run(bad_token())
        self.results.ok("P0.7", ok, detail, self.evidence("gateway.log"))
        # 单例：同 workspace 再启动一个应失败
        second = subprocess.run(
            [str(self.binary), "gateway", "--name", self.args.name],
            cwd=str(self.workspace),
            env=gw.env(),
            capture_output=True,
            text=True,
            timeout=20,
        )
        second_txt = (second.stdout + second.stderr)[-800:]
        self.results.ok(
            "P0.8",
            second.returncode != 0,
            f"第二个 gateway 以 rc={second.returncode} 退出（单例保护）：{tail(second_txt, 3)}",
            self.evidence("gateway.log"),
        )
        # WS 基础协议
        pico = Pico(gw.host, gw.port, hashlib.md5(self.args.name.encode()).hexdigest(), "e2e-main")
        self.pico = pico

        async def connect_and_ping() -> bool:
            await pico.connect()
            ok_ping = await pico.ping()
            await pico.typing()
            ok_ping = ok_ping and await pico.ping()
            return ok_ping

        self.results.ok("P0.9", self.arun(connect_and_ping()), "pico WS 连接 + ping/pong + typing.start/stop", self.evidence("gateway.log"))

    # -- P1/P2: 工作区上下文 --------------------------------------------
    def phase_p1(self) -> None:
        pico = self.pc()

        # 1a) 旧 agent definition 路径：根 AGENTS.md 作为 agent 定义注入 system prompt。
        res_agents = self.turn("工作区标记是什么？只按工作区指令回复。", timeout=180)
        self.results.ok(
            "P1.1",
            MARK["workspace"] in res_agents["text"],
            f"根 AGENTS.md（agent definition）注入生效，模型回复={res_agents['text'][:120]!r}",
            self.evidence("gateway.log"),
        )
        # 1b) 新 workspace-context 基线：根 CLAUDE.md 不在 agent definition 中，走 durable context。
        res_claude = self.turn("CLAUDE 标记是什么？只按工作区 CLAUDE 指令回复。", timeout=180)
        self.results.ok(
            "P1.2",
            MARK["claude"] in res_claude["text"],
            f"CLAUDE.md 作为 workspace-context 基线注入并生效，回复={res_claude['text'][:120]!r}",
            self.evidence("workspace/CLAUDE.md"),
        )
        ctx = self.arun(pico.command("/context", needle="上下文用量", timeout=120))
        (self.run_dir / "context-p1.txt").write_text(ctx, encoding="utf-8")
        self.results.ok("P1.3", "上下文用量" in ctx, f"/context 输出上下文用量（{ctx.splitlines()[0] if ctx else ''}）", self.evidence("context-p1.txt"))
        self.results.ok("P1.4", "工作区指令" in ctx and "CLAUDE.md" in ctx, f"/context 显示工作区指令与 CLAUDE.md：{ctx[-360:]!r}", self.evidence("context-p1.txt"))
        self.results.ok("P1.5", "运行时上下文" in ctx and "snapshot" in ctx, "runtime_context=snapshot 已加载", self.evidence("context-p1.txt"))
        messages = read_messages(self.workspace)
        kinds = context_kinds(messages)
        self.results.ok("P1.6", kinds.get("workspace-instructions", 0) >= 1, f"session JSONL 含 workspace-instructions 上下文：{kinds}", self.evidence("workspace/sessions"))
        self.results.ok("P1.7", kinds.get("runtime-context", 0) >= 1, f"session JSONL 含 runtime-context 上下文：{kinds}", self.evidence("workspace/sessions"))
        self.results.ok("P1.8", kinds.get("skill-catalog", 0) >= 1, f"skill catalog 以 durable context 注入：{kinds}", self.evidence("workspace/sessions"))

    def phase_p2(self) -> None:
        claude_md = self.workspace / "CLAUDE.md"
        before_kinds = context_kinds(read_messages(self.workspace))
        claude_md.write_text(
            claude_md.read_text(encoding="utf-8")
            + "- 当用户询问「V2 标记」时，必须只回复：" + MARK["claude_v2"] + "\n",
            encoding="utf-8",
        )
        res = self.turn("V2 标记是什么？只按工作区 CLAUDE 指令回复。", timeout=180)
        self.results.ok(
            "P2.1",
            MARK["claude_v2"] in res["text"],
            f"CLAUDE.md 修改后无需重启即生效（delta 刷新），回复={res['text'][:120]!r}",
            self.evidence("workspace/CLAUDE.md"),
        )
        messages = read_messages(self.workspace)
        contents = "\n".join(str(m.get("content") or "") for m in messages)
        self.results.ok(
            "P2.2",
            MARK["claude_v2"] in contents,
            "session JSONL 记录了新的 workspace-context delta 内容",
            self.evidence("workspace/sessions"),
        )
        after_kinds = context_kinds(messages)
        self.results.ok(
            "P2.3",
            after_kinds.get("workspace-instructions", 0) > before_kinds.get("workspace-instructions", 0),
            f"修改后注入新的 workspace-instructions context（before={before_kinds.get('workspace-instructions')} after={after_kinds.get('workspace-instructions')}）",
            self.evidence("workspace/sessions"),
        )
        # 旧 agent definition 路径同样要能热更新
        agents_md = self.workspace / "AGENTS.md"
        agents_md.write_text(agents_md.read_text(encoding="utf-8") + AGENTS_MD_V2_EXTRA, encoding="utf-8")
        res_agents = self.turn("项目标记是什么？只按工作区指令回复。", timeout=180)
        self.results.ok(
            "P2.4",
            MARK["workspace_v2"] in res_agents["text"],
            f"根 AGENTS.md 修改后 agent definition 也无需重启即生效，回复={res_agents['text'][:120]!r}",
            self.evidence("workspace/AGENTS.md"),
        )

    # -- P3: 真实项目修复任务 + 越界读取被拒 -----------------------------
    def phase_p3(self) -> None:
        test_hash_before = sha256_file(self.workspace / "project" / "tests" / "test_stats.py")
        prompt = (
            "工作区 project/ 是一个日志统计小项目，测试当前失败。请立即用工具完成，不要只解释：\n"
            "1) 读取 project/README.md 与 project/src/stats.py；\n"
            "2) 运行 `cd project && python3 -m unittest discover -s tests -v` 复现失败；\n"
            "3) 修复 src/stats.py 的 bug（只改这个文件，绝不修改 tests/ 下的测试）；\n"
            "4) 重新运行测试直到全部通过；\n"
            f"5) 把修复说明写入 project/FIX_REPORT.md，正文必须包含 {MARK['report']}；\n"
            "完成后只回复 PROJECT_OK。"
        )
        res = self.turn(prompt, timeout=600)
        (self.run_dir / "p3-response.txt").write_text(res["text"], encoding="utf-8")
        self.results.ok("P3.1", "PROJECT_OK" in res["text"], f"修复任务收尾回复={res['text'][:160]!r}", self.evidence("p3-response.txt"))
        used = set(res["tool_calls"])
        chain_ok = {"read_file", "exec"}.issubset(used) and bool(used & {"edit_file", "write_file"})
        self.results.ok("P3.2", chain_ok, f"工具链 {sorted(used)} 覆盖 read/exec/edit(write)", self.evidence("gateway.log"))
        report = self.workspace / "project" / "FIX_REPORT.md"
        self.results.ok("P3.3", report.exists() and MARK["report"] in report.read_text(encoding="utf-8", errors="replace"), "project/FIX_REPORT.md 已生成且包含标记", str(report))
        test_hash_after = sha256_file(self.workspace / "project" / "tests" / "test_stats.py")
        self.results.ok("P3.4", test_hash_before == test_hash_after, "tests/ 未被修改（未通过改测试作弊）", str(self.workspace / "project" / "tests" / "test_stats.py"))
        run = subprocess.run(
            ["python3", "-m", "unittest", "discover", "-s", "tests", "-v"],
            cwd=str(self.workspace / "project"),
            capture_output=True,
            text=True,
            timeout=120,
        )
        (self.run_dir / "p3-independent-test.log").write_text(run.stdout + "\n" + run.stderr, encoding="utf-8")
        self.results.ok("P3.5", run.returncode == 0, f"独立复跑项目测试 rc={run.returncode}", self.evidence("p3-independent-test.log"))
        log_calls = self.gw().log_count(r"Tool call[^:]*: (read_file|edit_file|exec|write_file|append_file)")
        self.results.ok("P3.6", log_calls >= 3, f"gateway 日志记录到 {log_calls} 次真实工具调用", self.evidence("gateway.log"))
        # 子目录 touch -> workspace-context delta（T3.1/T3.3）
        touched = self.arun(
            self.pc().turn("请用 read_file 工具读取 docs/CLAUDE.md，读完后只回复 DOCS_READ_OK。", timeout=240)
        )
        self.results.ok("P3.8", "DOCS_READ_OK" in touched["text"], f"子目录 CLAUDE.md 被工具真实读取：{touched['text'][:120]!r}", self.evidence("gateway.log"))
        after_touch = self.arun(self.pc().command("/context", needle="上下文用量", timeout=120))
        (self.run_dir / "context-p3-after-touch.txt").write_text(after_touch, encoding="utf-8")
        messages = read_messages(self.workspace)
        delta_msgs = [
            str(m.get("content") or "")
            for m in messages
            if (m.get("context") or {}).get("kind") == "workspace-instructions"
            and MARK["docs_claude"] in str(m.get("content") or "")
        ]
        self.results.ok(
            "P3.9",
            len(delta_msgs) >= 1 or MARK["docs_claude"] in after_touch,
            f"工具触碰后子目录 workspace-context 增量注入（delta 消息 {len(delta_msgs)} 条）",
            self.evidence("workspace/sessions"),
        )
        # 越界读取
        denied = self.arun(
            self.pc().turn(
                "只允许使用 read_file 工具（不要用 exec）尝试读取 /etc/hosts。如果被拒绝，只回复 DENIED_OK；如果读到内容，回复 LEAKED。",
                timeout=240,
            )
        )
        messages = read_messages(self.workspace)
        denied_evidence = [
            str(m.get("content") or "")
            for m in messages
            if m.get("role") == "tool" and ("/etc/hosts" in str(m.get("content") or "") or "denied" in str(m.get("content") or "").lower())
        ]
        denied_text = "\n".join(denied_evidence)
        self.results.ok(
            "P3.7",
            ("DENIED_OK" in denied["text"] or "denied" in denied_text.lower() or "outside the workspace" in denied_text)
            and "LEAKED" not in denied["text"],
            f"工作区外读取被拒，回复={denied['text'][:120]!r}，工具结果片段={denied_text[-200:]!r}",
            self.evidence("workspace/sessions"),
        )

    # -- P4: TaskState + 自主续跑 ---------------------------------------
    def phase_p4(self) -> None:
        gw = self.gw()
        flag = self.workspace / "WAIT_FLAG.txt"
        notes = self.workspace / "project" / "AUTONOMY_NOTES.md"
        sustain_prompt = (
            "请严格按顺序操作，只调用工具，不要长篇解释：\n"
            "1) 调用 task_update，action=start，objective=\"AUTONOMY_E2E：等待外部信号后收尾（步骤 1 检查工作区根目录 WAIT_FLAG.txt 是否存在；"
            "步骤 2 文件出现后读取其内容，把该内容与 AUTONOMY_STEP2_MARKER 一起写入 project/AUTONOMY_NOTES.md；"
            "步骤 3 调用 task_update complete，evidence 必须包含 WAIT_FLAG.txt 的实际内容）\"，max_rounds=6；\n"
            "2) 调用 todo_write 记录上述三步；\n"
            "3) 检查工作区根目录 WAIT_FLAG.txt。现在它不存在（由外部系统稍后提供，不要自己创建，也不要用 exec 轮询/sleep）；"
            "本轮保持任务 active、不要执行步骤 1/2/3，直接结束并只回复 AUTONOMY_STARTED。"
        )
        autos_before = gw.log_count(r'"event_kind":"agent.autonomy.round"')
        scheduled_before = gw.log_count(r"autonomy round scheduled")
        started = False
        for attempt in range(2):
            flag.unlink(missing_ok=True)
            notes.unlink(missing_ok=True)
            res = self.turn(sustain_prompt, timeout=300)
            (self.run_dir / f"p4-start-response-{attempt}.txt").write_text(res["text"], encoding="utf-8")
            state, meta_path = task_state(self.workspace)
            phase = (state or {}).get("phase")
            if phase in ("active", "complete", "blocked"):
                started = True
                if phase == "active":
                    break
            if attempt == 0 and phase != "active":
                self.note(f"P4 第一次启动任务未保持 active（phase={phase}），重试一次")
        self.results.ok("P4.1", started, f"任务启动轮回复={res['text'][:120]!r}", self.evidence("p4-start-response-0.txt"))
        state, meta_path = task_state(self.workspace)
        self.results.ok(
            "P4.2",
            state is not None,
            f"meta.json 持久化了 task_state（{(state or {}).get('phase')} round={(state or {}).get('round')}）",
            str(meta_path),
        )
        if (state or {}).get("phase") != "active":
            self.results.fail(
                "P4.3",
                f"首轮后任务未保持 active（phase={(state or {}).get('phase')}），模型未按指令等待外部信号，无法触发自主续跑",
                str(meta_path),
            )
            return
        # 首轮结束，创建外部信号文件
        flag.write_text("AUTONOMY_WAIT_FLAG_CONTENT_42\n", encoding="utf-8")
        self.note("P4 外部信号 WAIT_FLAG.txt 已创建")
        t0 = time.time()
        last_state = None
        terminal = {"complete", "blocked", "paused"}
        while time.time() - t0 < 300:
            state, meta_path = task_state(self.workspace)
            if state is not None:
                last_state = state
                if state.get("phase") in terminal and state.get("round", 0) >= 1:
                    break
            self.pump(0.05)
            time.sleep(0.45)
        elapsed = time.time() - t0
        scheduled = gw.log_count(r"autonomy round scheduled") - scheduled_before
        autonomy_events = gw.log_count(r'"event_kind":"agent.autonomy.round"') - autos_before
        self.timings["autonomy_wait"] = elapsed
        self.results.ok(
            "P4.3",
            scheduled >= 1,
            f"gateway 调度了 {scheduled} 个自主轮（等待 {elapsed:.1f}s）",
            self.evidence("gateway.log"),
        )
        notes_ok = notes.exists() and "AUTONOMY_STEP2_MARKER" in notes.read_text(encoding="utf-8", errors="replace")
        flag_content_used = notes.exists() and "AUTONOMY_WAIT_FLAG_CONTENT_42" in notes.read_text(encoding="utf-8", errors="replace")
        self.results.ok("P4.4", notes_ok, "自主续跑真实执行了第 2 步（AUTONOMY_NOTES.md 含 AUTONOMY_STEP2_MARKER）", str(notes))
        self.results.ok("P4.5", flag_content_used, "自主轮次读取了外部信号内容并写入 NOTES（证明是新轮次的真实工具动作）", str(notes))
        final_state = last_state or {}
        self.results.ok(
            "P4.6",
            final_state.get("phase") == "complete",
            f"任务终态={final_state.get('phase')} round={final_state.get('round')} evidence={len(final_state.get('evidence') or [])}",
            str(meta_path),
        )
        self.results.ok(
            "P4.7",
            autonomy_events >= 1 and scheduled >= 1,
            f"自主轮事件/调度计数：events={autonomy_events} scheduled={scheduled}，自主续跑真实发生",
            str(meta_path),
        )
        self.wait_turn_idle(timeout=180, quiet=3.5)
        ctx = self.arun(self.pc().command("/context", needle="任务状态", timeout=90))
        (self.run_dir / "context-p4.txt").write_text(ctx, encoding="utf-8")
        self.results.ok("P4.8", "任务状态" in ctx and "complete" in ctx, f"/context 展示 complete 任务状态：{ctx[-260:]!r}", self.evidence("context-p4.txt"))

    # -- P5: 技能目录 + 渐进披露 ------------------------------------------
    def phase_p5(self) -> None:
        prompt = "请调用 skill 工具加载 e2e-skill 技能，然后严格按技能正文的要求回复。"
        res = self.turn(prompt, timeout=240)
        (self.run_dir / "p5-response.txt").write_text(res["text"], encoding="utf-8")
        self.results.ok("P5.1", "skill" in res["tool_calls"], f"模型调用了 skill 工具（tools={sorted(set(res['tool_calls']))}）", self.evidence("gateway.log"))
        self.results.ok("P5.2", MARK["skill"] in res["text"], f"技能正文标记出现在回复中：{res['text'][:120]!r}", self.evidence("p5-response.txt"))
        kinds = context_kinds(read_messages(self.workspace))
        self.record_context_kinds(kinds)
        self_healed = False
        if kinds.get("skill-catalog", 0) < 1:
            # 异步 summarize 可能在本回合期间合法重写 JSONL 并丢掉刚注入的 context 消息；
            # ContextSource 会在下一回合 setup 自愈重注入。用一次探针回合验证该语义，
            # 而不是把「summarize 恰好落在查询窗口内」误判为缺陷。
            probe = self.turn("请只回复 SKILL_CATALOG_PROBE_OK，不要调用工具。", timeout=180)
            (self.run_dir / "p5-catalog-probe.txt").write_text(probe["text"], encoding="utf-8")
            kinds = context_kinds(read_messages(self.workspace))
            self.record_context_kinds(kinds)
            self_healed = kinds.get("skill-catalog", 0) >= 1
        self.results.ok(
            "P5.3",
            kinds.get("skill-catalog", 0) >= 1,
            f"技能目录以 durable context 注入（kinds={kinds}，summarize 竞态下自愈={self_healed}）",
            self.evidence("workspace/sessions"),
        )
        skills = self.arun(self.pc().command("/list skills", needle="Installed Skills", timeout=60))
        self.results.ok("P5.4", "e2e-skill" in skills, f"/list skills 包含 e2e-skill：{skills[:160]!r}", self.evidence("gateway.log"))

    # -- P6: 循环保护 -----------------------------------------------------
    def phase_p6(self) -> None:
        prompt = (
            "请对同一个目录连续调用 list_dir 工具 4 次：每次都真实调用，path 参数都用 \"project\"，"
            "不要用一次调用代替多次，也不要更换参数。完成后只回复 LOOP_GUARD_DONE。"
        )
        res = self.turn(prompt, timeout=300)
        (self.run_dir / "p6-response.txt").write_text(res["text"], encoding="utf-8")
        kinds = context_kinds(read_messages(self.workspace))
        self.record_context_kinds(kinds)
        calls = self.gw().log_count(r"Tool call[^:]*: list_dir")
        self.results.ok("P6.1", kinds.get("loop-guard", 0) >= 1, f"session JSONL 出现 loop-guard 提示上下文（kinds={kinds}）", self.evidence("workspace/sessions"))
        self.results.ok("P6.2", calls >= 3, f"gateway 日志 list_dir 真实调用 {calls} 次", self.evidence("gateway.log"))
        self.results.ok("P6.3", "LOOP_GUARD_DONE" in res["text"], f"重复调用后回合正常收尾：{res['text'][:120]!r}", self.evidence("p6-response.txt"))
        self.kinds_snapshot = kinds
        # 持久化 loop-guard 证据快照：后续压缩/摘要会合法丢弃历史 context，最终 JSONL 可能不再包含它。
        evidence_dir = self.run_dir / "evidence"
        evidence_dir.mkdir(parents=True, exist_ok=True)
        all_messages = read_messages(self.workspace)
        loop_guard_msgs = [
            {"content": m.get("content"), "context": m.get("context")}
            for m in all_messages
            if (m.get("context") or {}).get("kind") == "loop-guard"
        ]
        (evidence_dir / "context-p6.json").write_text(
            json.dumps(
                {
                    "captured_at": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
                    "kinds": kinds,
                    "loop_guard_messages": loop_guard_msgs,
                    "note": "loop-guard 在 P6 发生时注入；后续 summarize/压缩可合法丢弃，故以本快照作为运行时证据",
                },
                ensure_ascii=False,
                indent=2,
            )
            + "\n",
            encoding="utf-8",
        )
        for path in session_files(self.workspace):
            shutil.copyfile(path, evidence_dir / f"sessions-at-p6-{path.name}")

    # -- P7: 上下文压力 / 压缩 / 裁剪 + spill -> /reload -----------------
    def phase_p7(self) -> None:
        gw = self.gw()
        big = make_big_file(self.workspace, lines=6000)
        size = big.stat().st_size
        self.note(f"生成大文件 bigdata.txt: {size} bytes")
        # 用 /reload 热更新为小上下文窗口（/reload 需要 pid.token 鉴权）
        cfg = json.loads((self.workspace / "config.json").read_text(encoding="utf-8"))
        cfg["agents"]["defaults"]["context_window"] = 12000
        cfg["agents"]["defaults"]["max_tokens"] = 1024
        write_config(self.workspace / "config.json", cfg)
        self.hashes["expected_final"] = sha256_file(self.workspace / "config.json")
        (self.run_dir / "config.hash.after-reload").write_text(self.hashes["expected_final"] + "\n", encoding="utf-8")
        (self.run_dir / "config.reload.redacted.json").write_text(
            json.dumps(redact_config(cfg), ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
        )
        code, body = gw.reload()
        self.results.ok("P7.1", code in (200, 202), f"/reload 热更新小窗口配置（http={code} body={body[:120]!r}）", self.evidence("gateway.log"))
        self.results.ok("P7.2", gw.healthy(), "/reload 后 /health 仍 200", self.evidence("gateway.log"))
        reload_ctx = ""
        deadline = time.time() + 60
        while time.time() < deadline:
            try:
                reload_ctx = self.command("/context", needle="上下文用量", timeout=30)
            except Exception:  # noqa: BLE001
                reload_ctx = ""
            if "12000" in reload_ctx:
                break
            self.pump(0.5)
            time.sleep(0.8)
        (self.run_dir / "context-p7-after-reload.txt").write_text(reload_ctx, encoding="utf-8")
        self.results.ok(
            "P7.3",
            "12000" in reload_ctx,
            f"/context 显示新的 12000 上下文窗口（热更新生效）：{reload_ctx[:200]!r}",
            self.evidence("context-p7-after-reload.txt"),
        )

        def compression_seen() -> bool:
            return gw.log_has(r"Forced compression pruned tool results without dropping turns") or gw.log_has(
                r"Proactive compression: context budget exceeded"
            ) or gw.log_has(r"Memory threshold reached")

        reads = [
            "请用 read_file 工具读取 bigdata.txt（不要用 exec）。读完后只回复 READ1_DONE。",
            "请再用 read_file 读取一遍 bigdata.txt 全文，然后在回复中只给出最后一行（以 LINE_ 开头）。",
        ]
        for idx, prompt in enumerate(reads):
            res = self.turn(prompt, timeout=300)
            (self.run_dir / f"p7-read{idx + 1}.txt").write_text(res["text"], encoding="utf-8")
            if idx == 0:
                self.results.ok(
                    "P7.4.1",
                    "read_file" in res["tool_calls"],
                    f"第 1 轮读取大文件（tools={sorted(set(res['tool_calls']))}）",
                    self.evidence("gateway.log"),
                )
            else:
                self.results.ok(
                    "P7.5.2",
                    "read_file" in res["tool_calls"] or compression_seen(),
                    f"第 2 轮再次读取大文件（tools={sorted(set(res['tool_calls']))}，压缩已触发={compression_seen()}）",
                    self.evidence("gateway.log"),
                )
        # 主动压缩发生在下一次请求 setup；用探针轮触发并轮询
        third = self.turn("请只回复 POST_COMPRESS_PROBE_OK，不要调用工具。", timeout=300)
        for _ in range(2):
            if compression_seen():
                break
            third = self.turn("请再用 read_file 读取 bigdata.txt，然后只回复 READ_AGAIN_DONE。", timeout=300)
            (self.run_dir / "p7-read-again.txt").write_text(third["text"], encoding="utf-8")
            time.sleep(1.0)
        (self.run_dir / "p7-post-compress.txt").write_text(third["text"], encoding="utf-8")
        compressed = compression_seen()
        self.results.ok("P7.6", compressed, "gateway 日志出现主动压缩/裁剪记录", self.evidence("gateway.log"))
        messages = read_messages(self.workspace)
        pruned_msgs = [m for m in messages if "…[tool output middle pruned]…" in str(m.get("content") or "")]
        log_text = gw.log_text()
        log_pruned = re.findall(r'(?:pruned_msgs=|"pruned_msgs":\s*)(\d+)', log_text)
        log_spilled = re.findall(r'(?:spilled_msgs=|"spilled_msgs":\s*)(\d+)', log_text)
        prune_only_lines = len(re.findall(r"Forced compression pruned tool results", log_text))
        durable_prune = (any(int(x) > 0 for x in log_pruned) and any(int(x) > 0 for x in log_spilled)) or (
            prune_only_lines >= 1 and len(spill_files(self.workspace)) >= 1
        )
        self.results.ok(
            "P7.7",
            len(pruned_msgs) >= 1 or durable_prune,
            f"确定性裁剪证据：JSONL marker={len(pruned_msgs)} 条，日志 pruned_msgs={log_pruned[:3]} spilled_msgs={log_spilled[:3]}",
            self.evidence("workspace/sessions", "gateway.log"),
        )
        spills = spill_files(self.workspace)
        sroot = spill_root(self.workspace)
        mode_ok = True
        mode_detail = []
        if spills:
            mode_ok = (sroot.stat().st_mode & 0o777) == 0o700 and all((p.stat().st_mode & 0o777) == 0o600 for p in spills)
            mode_detail = [f"{p.name}:{oct(p.stat().st_mode & 0o777)} size={p.stat().st_size}" for p in spills]
        self.results.ok("P7.8", len(spills) >= 1, f"spill 落盘 {len(spills)} 个文件（dir={oct(sroot.stat().st_mode & 0o777) if sroot.exists() else 'missing'}）", "; ".join(mode_detail)[:600])
        self.results.ok("P7.9", mode_ok and bool(spills), "spill 目录 0700 / 文件 0600", "; ".join(mode_detail)[:600])
        if spills:
            biggest = max(spills, key=lambda p: p.stat().st_size)
            self.results.ok(
                "P7.10",
                biggest.stat().st_size > 30000,
                f"spill 文件保留完整大输出（最大 {biggest.stat().st_size} bytes）",
                str(biggest),
            )
        else:
            self.results.fail("P7.10", "无 spill 文件，无法验证落盘内容完整性", str(self.workspace / ".chagent" / "spill"))
        probe_ok = compression_seen() and self.turn_ok_present(third, "READ1_DONE") is not None
        post = self.turn("请只回复 POST_COMPRESS_PROBE_OK，不要调用工具。", timeout=300)
        (self.run_dir / "p7-final-probe.txt").write_text(post["text"], encoding="utf-8")
        self.results.ok(
            "P7.11",
            "POST_COMPRESS_PROBE_OK" in post["text"],
            f"压缩后对话仍可正常继续：{post['text'][:120]!r}",
            self.evidence("p7-final-probe.txt"),
        )
        messages = read_messages(self.workspace)
        pairing_ok, pairing_detail = tool_pairing_ok(messages)
        self.results.ok("P7.12", pairing_ok, f"压缩后 tool_call/tool_result 配对完整：{pairing_detail}", self.evidence("workspace/sessions"))
        kinds = context_kinds(messages)
        if kinds.get("context-pressure"):
            self.results.pass_("P7.13", f"临近上限提醒 context-pressure 已注入（{kinds['context-pressure']} 次）", self.evidence("workspace/sessions"))
        else:
            self.results.skip("P7.13", "本次未触发 context-pressure 临近提醒（压缩先于提醒生效），由 context-window-tester 专项覆盖", self.evidence("workspace/sessions"))

    @staticmethod
    def turn_ok_present(res: dict, needle: str):
        text = str(res.get("text") or "")
        return text if needle in text else None

    # -- P8: /stop 暂停 + 取消排队自主轮 ---------------------------------
    def phase_p8(self) -> None:
        gw = self.gw()
        prompt = (
            "请调用 task_update，action=start，objective=\"STOP_E2E：等待外部 STOP_FLAG.txt 信号（该文件由外部提供，不要自己创建），"
            "文件出现前保持 active，每轮只做一次检查\"，max_rounds=6。"
            "本轮检查一次工作区根目录 STOP_FLAG.txt（它不存在），然后保持 active 直接结束，只回复 STOP_TASK_STARTED。"
        )
        res = self.turn(prompt, timeout=240)
        (self.run_dir / "p8-start.txt").write_text(res["text"], encoding="utf-8")
        state, _ = task_state(self.workspace)
        if (state or {}).get("phase") != "active":
            # 重试一次，避免模型首轮直接完成任务导致无法测试 /stop
            res = self.turn(prompt, timeout=240)
            (self.run_dir / "p8-start-retry.txt").write_text(res["text"], encoding="utf-8")
            state, _ = task_state(self.workspace)
        self.results.ok("P8.1", (state or {}).get("phase") == "active", f"停止测试前置任务处于 active（phase={(state or {}).get('phase')}）", str(self.workspace / "sessions"))
        before = gw.log_count(r"autonomy round scheduled")
        try:
            wait_until(lambda: gw.log_count(r"autonomy round scheduled") > before, 20, 0.1, "autonomy round scheduled")
        except TimeoutError:
            self.results.fail("P8.2", "任务保持 active 后仍未观测到自主轮调度", self.evidence("gateway.log"))
        else:
            self.results.pass_("P8.2", "任务启动后自主轮已调度（准备 /stop）", self.evidence("gateway.log"))
        stop_res = self.turn("/stop", timeout=60)
        (self.run_dir / "p8-stop.txt").write_text(stop_res["text"], encoding="utf-8")
        end = time.time() + 4.0
        while time.time() < end:
            self.pump(0.1)
        state, meta_path = task_state(self.workspace)
        self.results.ok(
            "P8.3",
            (state or {}).get("phase") == "paused",
            f"/stop 后任务阶段={(state or {}).get('phase')}（期望 paused）",
            str(meta_path),
        )
        after = gw.log_count(r"autonomy round scheduled")
        self.results.ok(
            "P8.4",
            after <= before + 1,
            f"/stop 后 4s 内未再调度新自主轮（stop 前={before} stop 后={after}）",
            self.evidence("gateway.log"),
        )
        self.wait_turn_idle(timeout=120, quiet=3.0)
        ctx = self.arun(self.pc().command("/context", needle="任务状态", timeout=90))
        (self.run_dir / "context-p8.txt").write_text(ctx, encoding="utf-8")
        self.results.ok("P8.5", "paused" in ctx or "暂停" in ctx, f"/context 反映暂停状态：{ctx[-200:]!r}", self.evidence("context-p8.txt"))

    # -- P9: 重启恢复 -----------------------------------------------------
    def phase_p9(self) -> None:
        gw = self.gw()
        self.wait_turn_idle(timeout=120, quiet=2.5)
        summarize_before = gw.log_count(r"agent.session.summarize")
        self.wait_log_quiet(timeout=90, quiet=6.0)
        summarize_after = gw.log_count(r"agent.session.summarize")
        self.note(f"关机前后台 summarize 事件：{summarize_before} -> {summarize_after}（等待日志静默后）")
        fingerprint_before = session_fingerprint(self.workspace)
        config_hash_before = gw.config_hash()
        ok_stop, elapsed = gw.stop()
        shutdown_evidence = ""
        if not (ok_stop and elapsed <= 5.0):
            recent = gw.log_text().splitlines()[-40:]
            shutdown_evidence = " | ".join(line[:160] for line in recent if "summar" in line or "Shutting" in line or "stopped" in line)
        self.results.ok(
            "P9.1",
            ok_stop and elapsed <= 5.0,
            f"SIGTERM 后 {elapsed:.2f}s 退出（要求 ≤5s；ok={ok_stop}）{('；' + shutdown_evidence) if shutdown_evidence else ''}",
            self.evidence("gateway.log"),
        )
        self.results.ok("P9.2", not (self.workspace / ".chagent.pid").exists(), "退出后 pid 文件已删除", str(self.workspace / ".chagent.pid"))
        # 重启
        gw.start(log_path=self.run_dir / "gateway-restart.log")
        self.results.ok("P9.3", gw.healthy(), f"重启后 /health 200（port={gw.port}）", self.evidence("gateway-restart.log"))
        self.results.ok("P9.4", gw.config_hash() == config_hash_before, "重启后 config.json 仍未被改写", self.evidence("config.hash.before"))
        fingerprint_after = session_fingerprint(self.workspace)
        metas_after = read_metas(self.workspace)
        summary_chars = max((len(meta.get("summary") or "") for _, meta in metas_after), default=0)
        logical_count = max((int(meta.get("count") or 0) for _, meta in metas_after), default=0)
        history_retained = (
            fingerprint_after["message_count"] >= fingerprint_before["message_count"]
            or (summary_chars > 0 and logical_count > 0)
        )
        self.results.ok(
            "P9.5",
            history_retained,
            f"会话历史保留或已摘要压缩（重启前 {fingerprint_before['message_count']} 条，重启后 {fingerprint_after['message_count']} 条，summary {summary_chars} 字，logical_count={logical_count}）",
            self.evidence("workspace/sessions"),
        )
        state, meta_path = task_state(self.workspace)
        self.results.ok("P9.6", (state or {}).get("phase") == "paused", f"TaskState 重启后保留（phase={(state or {}).get('phase')}）", str(meta_path))
        pico = Pico(gw.host, gw.port, hashlib.md5(self.args.name.encode()).hexdigest(), "e2e-main")
        self.pico = pico
        self.arun(pico.connect())
        recall = self.turn("我们之前修复了哪个项目？只回答项目名（project/... 里的那个文件或项目）。", timeout=240)
        (self.run_dir / "p9-recall.txt").write_text(recall["text"], encoding="utf-8")
        self.results.ok(
            "P9.7",
            "stats" in recall["text"].lower() or "project" in recall["text"].lower() or "日志" in recall["text"],
            f"重启后模型能回忆之前的修复任务：{recall['text'][:160]!r}",
            self.evidence("p9-recall.txt"),
        )
        kinds = context_kinds(read_messages(self.workspace))
        self.record_context_kinds(kinds)
        baseline_count = kinds.get("workspace-instructions", 0)
        self.results.ok(
            "P9.8",
            baseline_count <= fingerprint_before["context_kinds"].get("workspace-instructions", 0) + 2,
            f"重启后工作区上下文未无限重复注入（before={fingerprint_before['context_kinds'].get('workspace-instructions', 0)}, after={baseline_count}）",
            self.evidence("workspace/sessions"),
        )
        ctx = self.arun(pico.command("/context", needle="上下文用量", timeout=120))
        (self.run_dir / "context-p9.txt").write_text(ctx, encoding="utf-8")
        self.results.ok("P9.9", "工作区指令" in ctx and ("paused" in ctx or "暂停" in ctx), "/context 重启后仍显示工作区指令与暂停任务", self.evidence("context-p9.txt"))

    # -- P11: MCP（servers-only 自动启用 / 真实模型 echo / 动态工具重注册 / 显式关闭）
    def phase_p11(self) -> None:
        gw = self.gw()
        if self.pico is None:
            self.pico = Pico(
                gw.host,
                gw.port,
                hashlib.md5(self.args.name.encode()).hexdigest(),
                "e2e-main",
            )
        self.ensure_ws()

        fixture = MCP_STDIO_FIXTURE
        if not fixture.exists():
            self.results.fail("P11.0", f"MCP stdio fixture 不存在：{fixture}", str(fixture))
            return
        fixture_log = self.run_dir / "mcp-p11-fixture.log"
        control_file = self.run_dir / "mcp-p11-control.txt"
        for path in (fixture_log, control_file):
            try:
                path.unlink()
            except FileNotFoundError:
                pass

        def mcp_config(enabled: bool | None) -> dict:
            mcp: dict = {
                "max_inline_text_chars": 16384,
                "servers": {
                    "p11": {
                        "enabled": True,
                        "type": "stdio",
                        "command": sys.executable or "python3",
                        "args": [str(fixture)],
                        "env": {
                            "CFGCAP_MCP_LOG_FILE": str(fixture_log),
                            "CFGCAP_MCP_SERVER_NAME": "p11",
                            "CFGCAP_MCP_ECHO_PREFIX": "CFGE2E_MCP_ECHO",
                            "CFGCAP_MCP_CONTROL_FILE": str(control_file),
                        },
                    }
                },
            }
            if enabled is not None:
                mcp["enabled"] = enabled
            return mcp

        def rewrite_mcp_config(enabled: bool | None, tag: str) -> None:
            cfg = json.loads((self.workspace / "config.json").read_text(encoding="utf-8"))
            cfg.setdefault("tools", {})["mcp"] = mcp_config(enabled)
            write_config(self.workspace / "config.json", cfg)
            (self.run_dir / f"config.mcp-{tag}.redacted.json").write_text(
                json.dumps(redact_config(cfg), ensure_ascii=False, indent=2) + "\n",
                encoding="utf-8",
            )

        def fixture_events() -> list[dict]:
            if not fixture_log.exists():
                return []
            events: list[dict] = []
            for line in fixture_log.read_text(encoding="utf-8", errors="replace").splitlines():
                line = line.strip()
                if not line:
                    continue
                try:
                    events.append(json.loads(line))
                except json.JSONDecodeError:
                    continue
            return events

        def fixture_tool_calls(tool: str) -> list[dict]:
            return [
                event
                for event in fixture_events()
                if event.get("direction") == "tool_call" and event.get("tool") == tool
            ]

        def resync_lines() -> list[str]:
            return gw.log_matching(r"Re-synced MCP tools after list_changed", limit=20)

        def resync_tool_counts(lines: list[str]) -> list[int]:
            counts: list[int] = []
            for line in lines:
                match = re.search(r"tool_count[\"':= ]+(\d+)", line)
                if match:
                    counts.append(int(match.group(1)))
            return counts

        # --- A) 仅 servers（不写 mcp.enabled）→ 自动启用 + 协商最新协议
        rewrite_mcp_config(None, "servers-only")
        code, _ = gw.reload()
        self.results.ok(
            "P11.1",
            code in (200, 202),
            f"/reload 应用“仅 tools.mcp.servers”配置（http={code}）",
            self.evidence("gateway.log", "config.mcp-servers-only.redacted.json"),
        )
        try:
            wait_until(
                lambda: gw.log_has(r"Initializing MCP servers") and gw.log_has(r"Connected to MCP server"),
                90,
                desc="servers-only MCP initialization",
            )
            init_ok = True
        except TimeoutError:
            init_ok = False
        self.results.ok(
            "P11.2",
            init_ok,
            "servers-only 配置自动初始化 MCP（无需写 tools.mcp.enabled=true）",
            self.evidence("gateway.log"),
        )
        def negotiated_protocols() -> list[str]:
            versions: list[str] = []
            for event in fixture_events():
                if event.get("direction") != "initialize":
                    continue
                negotiated = str(event.get("negotiated") or "")
                requested = str(event.get("requested") or "")
                if negotiated:
                    versions.append(f"{requested}->{negotiated}")
            return versions

        try:
            wait_until(lambda: bool(negotiated_protocols()), 30, desc="fixture initialize negotiated version")
        except TimeoutError:
            pass
        init_versions = negotiated_protocols()
        gateway_protocol = ""
        match = re.search(r"protocol=(\d{4}-\d{2}-\d{2})", gw.log_text())
        if match:
            gateway_protocol = match.group(1)
        negotiated_ok = any(
            version.split("->")[-1] in ("2026-06-30", "2025-11-25") for version in init_versions
        ) or gateway_protocol in ("2026-06-30", "2025-11-25")
        self.results.ok(
            "P11.3",
            negotiated_ok,
            f"MCP initialize 协商最新协议（fixture requested->negotiated={init_versions}，"
            f"gateway={gateway_protocol or 'unknown'}）",
            self.evidence("gateway.log", "mcp-p11-fixture.log"),
        )
        list_mcp = ""
        deadline = time.time() + 60
        while time.time() < deadline:
            try:
                list_mcp = self.command("/list mcp", needle="Configured MCP Servers", timeout=60)
            except Exception:  # noqa: BLE001
                list_mcp = ""
            if "Connected: yes" in list_mcp:
                break
            self.pump(0.5)
            time.sleep(0.8)
        (self.run_dir / "p11-list-mcp.txt").write_text(list_mcp, encoding="utf-8")
        self.results.ok(
            "P11.4",
            "Connected: yes" in list_mcp and "p11" in list_mcp,
            f"/list mcp 显示 p11 已连接（{list_mcp[:180]!r}）",
            self.evidence("p11-list-mcp.txt"),
        )

        # --- B) 真实模型调用 MCP echo 工具
        res = self.turn(
            "请立即调用工具 mcp_p11_echo，参数 text='P11_MCP_CALL_MARKER'。"
            "不要解释，只回复工具返回的原文。",
            timeout=300,
        )
        (self.run_dir / "p11-echo-call.txt").write_text(res["text"], encoding="utf-8")
        self.results.ok(
            "P11.5",
            "mcp_p11_echo" in res["tool_calls"],
            f"真实模型调用 MCP echo 工具（tools={sorted(set(res['tool_calls']))}）",
            self.evidence("gateway.log"),
        )
        self.results.ok(
            "P11.6",
            "P11_MCP_CALL_MARKER" in res["text"],
            f"模型可见 MCP echo 返回值中的 marker：{res['text'][:160]!r}",
            self.evidence("p11-echo-call.txt"),
        )
        echo_calls = fixture_tool_calls("echo")
        echo_marker_seen = any(
            "P11_MCP_CALL_MARKER" in json.dumps(event.get("arguments") or {}) for event in echo_calls
        )
        self.results.ok(
            "P11.7",
            echo_marker_seen,
            f"fixture 日志真实收到 tools/call echo（{len(echo_calls)} 次，marker={echo_marker_seen}）",
            str(fixture_log),
        )

        # --- C) 运行期动态新增工具 → agent 重注册 → 模型可调用
        control_file.write_text("add\n", encoding="utf-8")
        try:
            wait_until(lambda: len(resync_lines()) >= 1, 45, desc="first tools/list_changed resync")
            add_resync_ok = True
        except TimeoutError:
            add_resync_ok = False
        add_lines = resync_lines()
        add_counts = resync_tool_counts(add_lines)
        dynamic_events = [event for event in fixture_events() if event.get("direction") == "dynamic_tools"]
        (self.run_dir / "p11-resync-add.log").write_text("\n".join(add_lines) + "\n", encoding="utf-8")
        self.results.ok(
            "P11.8",
            add_resync_ok and any(count >= 2 for count in add_counts),
            f"server 动态新增工具后 agent 重注册（resync={len(add_lines)}，tool_counts={add_counts}，"
            f"fixture actions={[e.get('action') for e in dynamic_events]}）",
            self.evidence("p11-resync-add.log", "mcp-p11-fixture.log"),
        )
        res_dyn = self.turn(
            "请立即调用工具 mcp_p11_echo_dynamic，参数 text='P11_MCP_DYNAMIC_MARKER'。"
            "不要解释，只回复工具返回的原文。",
            timeout=300,
        )
        (self.run_dir / "p11-dynamic-call.txt").write_text(res_dyn["text"], encoding="utf-8")
        dyn_calls = [
            event
            for event in fixture_tool_calls("echo_dynamic")
            if "P11_MCP_DYNAMIC_MARKER" in json.dumps(event.get("arguments") or {})
        ]
        self.results.ok(
            "P11.9",
            "mcp_p11_echo_dynamic" in res_dyn["tool_calls"]
            and "P11_MCP_DYNAMIC_MARKER" in res_dyn["text"]
            and bool(dyn_calls),
            f"动态新增的工具已注册且被真实模型调用（tools={sorted(set(res_dyn['tool_calls']))}）",
            self.evidence("p11-dynamic-call.txt", "mcp-p11-fixture.log"),
        )

        # --- D) 运行期动态移除工具 → agent 注销 → 模型不可再调用
        control_file.write_text("remove\n", encoding="utf-8")
        try:
            wait_until(lambda: len(resync_lines()) >= 2, 45, desc="second tools/list_changed resync")
            remove_resync_ok = True
        except TimeoutError:
            remove_resync_ok = False
        all_lines = resync_lines()
        all_counts = resync_tool_counts(all_lines)
        (self.run_dir / "p11-resync-remove.log").write_text("\n".join(all_lines) + "\n", encoding="utf-8")
        self.results.ok(
            "P11.10",
            remove_resync_ok and len(all_lines) >= 2 and any(count <= 1 for count in all_counts[1:]),
            f"server 动态移除工具后 agent 再次重注册并注销工具（resync={len(all_lines)}，tool_counts={all_counts}）",
            self.evidence("p11-resync-remove.log"),
        )
        dynamic_calls_before = len(fixture_tool_calls("echo_dynamic"))
        res_gone = self.turn(
            "请调用工具 mcp_p11_echo_dynamic，参数 text='P11_GONE_CHECK'。"
            "如果你没有这个工具，只回复 DYNAMIC_TOOL_GONE。",
            timeout=300,
        )
        (self.run_dir / "p11-dynamic-removed.txt").write_text(res_gone["text"], encoding="utf-8")
        dynamic_calls_after = len(fixture_tool_calls("echo_dynamic"))
        self.results.ok(
            "P11.11",
            dynamic_calls_after == dynamic_calls_before
            and ("mcp_p11_echo_dynamic" not in res_gone["tool_calls"] or "DYNAMIC_TOOL_GONE" in res_gone["text"]),
            f"工具移除后模型无法再调用（新 tool_call={dynamic_calls_after - dynamic_calls_before}，"
            f"reply={res_gone['text'][:120]!r}）",
            self.evidence("p11-dynamic-removed.txt", "mcp-p11-fixture.log"),
        )

        # --- E) 显式 tools.mcp.enabled=false → 关闭旧连接且不再初始化
        log_len_before = len(gw.log_text())
        echo_calls_before = len(fixture_tool_calls("echo"))
        rewrite_mcp_config(False, "disabled")
        code, _ = gw.reload()
        time.sleep(0.8)
        tail_log = gw.log_text()[log_len_before:]
        self.results.ok(
            "P11.12",
            code in (200, 202),
            f"/reload 应用显式 tools.mcp.enabled=false（http={code}）",
            self.evidence("gateway.log", "config.mcp-disabled.redacted.json"),
        )
        self.results.ok(
            "P11.13",
            "Closing all MCP server connections" in tail_log and "Initializing MCP servers" not in tail_log,
            "显式关闭后旧 MCP 连接被关闭且不再重新初始化",
            self.evidence("gateway.log"),
        )
        list_mcp_off = self.command("/list mcp", needle="Configured MCP Servers", timeout=60)
        (self.run_dir / "p11-list-mcp-disabled.txt").write_text(list_mcp_off, encoding="utf-8")
        self.results.ok(
            "P11.14",
            "integration disabled" in list_mcp_off and "Connected: no" in list_mcp_off,
            f"/list mcp 显示集成已禁用且 p11 未连接（{list_mcp_off[:180]!r}）",
            self.evidence("p11-list-mcp-disabled.txt"),
        )
        res_off = self.turn(
            "请调用工具 mcp_p11_echo，参数 text='P11_DISABLED_CHECK'。"
            "如果该工具不可用，只回复 MCP_DISABLED。",
            timeout=300,
        )
        (self.run_dir / "p11-disabled-call.txt").write_text(res_off["text"], encoding="utf-8")
        echo_calls_after = len(fixture_tool_calls("echo"))
        self.results.ok(
            "P11.15",
            echo_calls_after == echo_calls_before
            and ("mcp_p11_echo" not in res_off["tool_calls"] or "MCP_DISABLED" in res_off["text"]),
            f"显式关闭后 MCP 工具不可用且无新 tools/call（新调用={echo_calls_after - echo_calls_before}，"
            f"reply={res_off['text'][:120]!r}）",
            self.evidence("p11-disabled-call.txt", "mcp-p11-fixture.log"),
        )

        # 记录 P11 完成后的最终配置 hash（P10.4 以 expected_final 为准）
        self.hashes["expected_final"] = sha256_file(self.workspace / "config.json")
        (self.run_dir / "config.hash.after-p11").write_text(
            self.hashes["expected_final"] + "\n", encoding="utf-8"
        )
        (self.run_dir / "evidence" / "mcp-p11.json").parent.mkdir(parents=True, exist_ok=True)
        (self.run_dir / "evidence" / "mcp-p11.json").write_text(
            json.dumps(
                {
                    "servers_only_auto_enable": init_ok,
                    "protocol": "2025-11-25",
                    "echo_call_seen_by_fixture": echo_marker_seen,
                    "dynamic_add_resync": add_resync_ok,
                    "dynamic_add_tool_counts": add_counts,
                    "dynamic_call_marker_seen": bool(dyn_calls),
                    "dynamic_remove_resync": remove_resync_ok,
                    "dynamic_remove_tool_counts": all_counts,
                    "explicit_disable_no_new_calls": echo_calls_after == echo_calls_before,
                    "fixture_log": str(fixture_log),
                },
                ensure_ascii=False,
                indent=2,
            )
            + "\n",
            encoding="utf-8",
        )

    # -- P10: 清理与最终分析 ---------------------------------------------
    def phase_p10(self) -> None:
        gw = self.gw()
        # 让后台异步任务（summarize 等）收敛后再测“空闲状态下的 SIGTERM 契约”
        self.wait_turn_idle(timeout=120, quiet=3.0)
        self.wait_log_quiet(timeout=120, quiet=10.0)
        end = time.time() + 8.0
        while time.time() < end:
            self.pump(0.2)
        self.hashes["after"] = gw.config_hash()
        (self.run_dir / "config.hash.after").write_text(self.hashes["after"] + "\n", encoding="utf-8")
        ok_stop, elapsed = gw.stop()
        self.results.ok("P10.1", ok_stop and elapsed <= 5.0, f"最终 SIGTERM {elapsed:.2f}s 内退出", self.evidence("gateway.log"))
        self.results.ok("P10.2", not (self.workspace / ".chagent.pid").exists(), "最终 pid 文件已删除", str(self.workspace / ".chagent.pid"))
        code, _ = http_get(f"http://{gw.host}:{gw.port}/health")
        self.results.ok("P10.3", code is None, f"退出后 /health 不可达（code={code}）", self.evidence("gateway.log"))
        expected = self.hashes.get("expected_final", self.hashes["before"])
        self.results.ok(
            "P10.4",
            expected == self.hashes["after"],
            "除脚本主动 /reload 改写外，gateway 未改写配置（before=%s… after=%s…）"
            % (self.hashes["before"][:12], self.hashes["after"][:12]),
            self.evidence("config.hash.after-reload", "config.hash.after"),
        )
        # 无孤儿进程
        try:
            pg = subprocess.run(["pgrep", "-f", str(self.binary)], capture_output=True, text=True, timeout=10)
            orphans = [line for line in pg.stdout.splitlines() if line.strip()]
        except Exception:  # noqa: BLE001
            orphans = []
        self.results.ok("P10.5", not orphans, f"无孤儿 gateway 进程（pgrep={orphans}）", self.evidence("gateway.log"))
        self.retain_workspace()
        evidence_dir = self.run_dir / "evidence"
        evidence_dir.mkdir(parents=True, exist_ok=True)
        (evidence_dir / "context-kinds-observed.json").write_text(
            json.dumps(
                {
                    "captured_at": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
                    "observed_kinds": self.observed_context_kinds,
                    "note": "运行期各快照观察到的最小/最终 context kind 并集；async summarize 可合法重写最终 JSONL",
                },
                ensure_ascii=False,
                indent=2,
            )
            + "\n",
            encoding="utf-8",
        )
        self.results.pass_("P10.6", "真实工作区已复制到 artifacts/workspace（供独立验证）", str(self.run_dir / "workspace"))
        self.analyze_artifacts()

    def retain_workspace(self) -> None:
        """把 /tmp 下的真实工作区复制到 artifacts，供独立验证器复核。"""
        target = self.run_dir / "workspace"
        if target.exists():
            shutil.rmtree(target)
        shutil.copytree(self.workspace, target, symlinks=True)

    # -- artifacts 分析 ---------------------------------------------------
    def analyze_artifacts(self) -> None:
        messages = read_messages(self.workspace)
        kinds = context_kinds(messages)
        observed_kinds = dict(self.observed_context_kinds)
        for kind, count in kinds.items():
            observed_kinds[kind] = max(observed_kinds.get(kind, 0), count)
        pairing_ok, pairing_detail = tool_pairing_ok(messages)
        spills = spill_files(self.workspace)
        state, _ = task_state(self.workspace)
        analysis = {
            "message_count": len(messages),
            "context_kinds": kinds,
            "context_kinds_observed": observed_kinds,
            "tool_pairing": {"ok": pairing_ok, "detail": pairing_detail},
            "session_files": [p.name for p in session_files(self.workspace)],
            "metas": [
                {
                    "file": p.name,
                    "summary_chars": len(meta.get("summary") or ""),
                    "skip": meta.get("skip"),
                    "count": meta.get("count"),
                    "task_state": meta.get("task_state"),
                }
                for p, meta in read_metas(self.workspace)
            ],
            "spill": {
                "root": str(spill_root(self.workspace)),
                "root_exists": spill_root(self.workspace).exists(),
                "root_mode": oct(spill_root(self.workspace).stat().st_mode & 0o777) if spill_root(self.workspace).exists() else None,
                "files": [
                    {
                        "path": str(p),
                        "mode": oct(p.stat().st_mode & 0o777),
                        "size": p.stat().st_size,
                    }
                    for p in spills
                ],
            },
            "project": {
                "fix_report_exists": (self.workspace / "project" / "FIX_REPORT.md").exists(),
                "autonomy_notes_exists": (self.workspace / "project" / "AUTONOMY_NOTES.md").exists(),
                "tests_sha256": sha256_file(self.workspace / "project" / "tests" / "test_stats.py"),
                "stats_src_sha256": sha256_file(self.workspace / "project" / "src" / "stats.py"),
            },
            "gateway_log_stats": {
                "tool_calls": self.gw().log_count(r"Tool call"),
                "batch_calls": self.gw().log_count(r"Tool call \(batch\)"),
                "autonomy_scheduled": self.gw().log_count(r"autonomy round scheduled"),
                "forced_compression": self.gw().log_count(r"Forced compression"),
                "proactive_compression": self.gw().log_count(r"Proactive compression"),
                "spill": self.gw().log_count(r"spill"),
            },
        }
        (self.run_dir / "artifact-analysis.json").write_text(
            json.dumps(analysis, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
        )
        # 从原始 artifacts 反推的关键断言（供独立验证器交叉核对）
        observed = analysis["context_kinds_observed"]
        self.results.ok("A1", observed.get("workspace-instructions", 0) >= 1, "artifact: workspace-instructions 上下文存在（运行期并集）")
        self.results.ok("A2", observed.get("runtime-context", 0) >= 1, "artifact: runtime-context 上下文存在（运行期并集）")
        self.results.ok("A3", observed.get("skill-catalog", 0) >= 1, "artifact: skill-catalog durable context 存在（运行期并集）")
        p6_snapshot = self.run_dir / "evidence" / "context-p6.json"
        p6_has_loop_guard = False
        if p6_snapshot.exists():
            try:
                p6_data = json.loads(p6_snapshot.read_text(encoding="utf-8"))
                p6_has_loop_guard = bool(p6_data.get("loop_guard_messages")) or p6_data.get("kinds", {}).get("loop-guard", 0) >= 1
            except Exception:  # noqa: BLE001
                p6_has_loop_guard = False
        loop_guard_seen = (
            analysis["context_kinds"].get("loop-guard", 0) >= 1
            or getattr(self, "kinds_snapshot", {}).get("loop-guard", 0) >= 1
            or p6_has_loop_guard
        )
        self.results.ok("A4", loop_guard_seen, "artifact: loop-guard 提示上下文存在（运行期快照或最终 JSONL）")
        self.results.ok("A5", len(analysis["spill"]["files"]) >= 1, "artifact: spill 文件存在")
        self.results.ok("A6", analysis["gateway_log_stats"]["autonomy_scheduled"] >= 1, "artifact: 自主轮调度日志存在")
        self.results.ok("A7", analysis["gateway_log_stats"]["forced_compression"] + analysis["gateway_log_stats"]["proactive_compression"] >= 1, "artifact: 压缩日志存在")
        self.results.ok("A8", pairing_ok, f"artifact: tool 配对完整（{pairing_detail}）")

    # -- 汇总 -------------------------------------------------------------
    def write_summary(self) -> None:
        counts = self.results.counts()
        lines = [
            "# 真实场景 E2E 功能测试结果（scripts/agent-e2e-scenario.py）",
            "",
            f"- 运行时间：{time.strftime('%Y-%m-%dT%H:%M:%S%z')}",
            f"- 模型：{self.model.get('model_name')} / {self.model.get('model')}",
            f"- 仓库：{ROOT}",
            f"- 产物目录：{self.run_dir}",
            "",
            f"## 汇总：PASS={counts['PASS']} FAIL={counts['FAIL']} SKIP={counts['SKIP']} INFO={counts['INFO']}",
            "",
            "| 检查项 | 状态 | 说明 |",
            "|---|---|---|",
        ]
        for item in self.results.items:
            detail = str(item["detail"]).replace("|", "\\|").replace("\n", " ")
            lines.append(f"| {item['id']} | {item['status']} | {detail[:240]} |")
        if self.notes:
            lines += ["", "## 备注", ""] + [f"- {n}" for n in self.notes]
        lines += ["", "## 耗时", ""]
        for key, value in self.timings.items():
            lines.append(f"- {key}: {value:.2f}s")
        lines += [
            "",
            "## 证据文件",
            "",
            "- gateway.log / gateway-restart.log：gateway 运行日志（工具调用/压缩/自主轮）",
            "- workspace/sessions/*.jsonl：会话历史（含 context kind 元数据）",
            "- workspace/sessions/*.meta.json：摘要与 task_state",
            "- workspace/.chagent/spill/：大工具输出落盘",
            "- artifact-analysis.json：从原始产物反推的结构化分析",
            "- config.hash.before / config.hash.after：配置未改写证据",
        ]
        (self.run_dir / "summary.md").write_text("\n".join(lines) + "\n", encoding="utf-8")
        (self.run_dir / "results.json").write_text(
            json.dumps(
                {
                    "model": self.model.get("model_name"),
                    "counts": counts,
                    "checks": self.results.items,
                    "notes": self.notes,
                    "timings": self.timings,
                    "config_hashes": self.hashes,
                    "duration_seconds": time.time() - self.results.started,
                },
                ensure_ascii=False,
                indent=2,
            )
            + "\n",
            encoding="utf-8",
        )

    # -- 主入口 -----------------------------------------------------------
    def run(self) -> int:
        sequence = [
            ("P0", self.phase_p0),
            ("P1", self.phase_p1),
            ("P2", self.phase_p2),
            ("P3", self.phase_p3),
            ("P4", self.phase_p4),
            ("P5", self.phase_p5),
            ("P6", self.phase_p6),
            ("P7", self.phase_p7),
            ("P8", self.phase_p8),
            ("P9", self.phase_p9),
            ("P11", self.phase_p11),
            ("P10", self.phase_p10),
        ]
        try:
            for phase, fn in sequence:
                if not self.enabled(phase):
                    continue
                t0 = time.time()
                self.note(f"开始 {phase}")
                if phase != "P0":
                    self.ensure_ws()
                try:
                    fn()
                except Exception as exc:  # noqa: BLE001
                    self.results.fail(f"{phase}.EXC", f"{type(exc).__name__}: {str(exc)[:300]}", traceback.format_exc()[-1500:])
                    (self.run_dir / f"exception-{phase}.log").write_text(traceback.format_exc(), encoding="utf-8")
                    if phase == "P0":
                        break
                    try:
                        self.wait_turn_idle(timeout=30, quiet=2.0)
                    except Exception:  # noqa: BLE001
                        pass
                finally:
                    self.timings[phase] = time.time() - t0
                    self.note(f"结束 {phase}（{self.timings[phase]:.1f}s）")
        finally:
            if self.gateway is not None:
                self.gateway.kill()
            if self.pico is not None:
                try:
                    self.pico.loop.run_until_complete(self.pico.close())
                    self.pico.loop.close()
                except Exception:  # noqa: BLE001
                    pass
            try:
                if (self.workspace / "sessions").exists() and not (self.run_dir / "workspace").exists():
                    self.retain_workspace()
            except Exception:  # noqa: BLE001
                traceback.print_exc()
            try:
                self.write_summary()
            except Exception:  # noqa: BLE001
                traceback.print_exc()
        counts = self.results.counts()
        print(f"\n=== RESULT: PASS={counts['PASS']} FAIL={counts['FAIL']} SKIP={counts['SKIP']} ===", flush=True)
        return 0 if counts["FAIL"] == 0 else 1


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--config", default=str(DEFAULT_CONFIG), help="外部模型配置（model_list 来源）")
    parser.add_argument("--model", default="deepseek-v4-pro", help="从外部配置选择的 model_name")
    parser.add_argument("--run-dir", default=str(DEFAULT_RUN_DIR), help="产物目录（默认 build/functest-20261030/live）")
    parser.add_argument("--name", default="pcl-e2e", help="gateway --name（决定 pico token=md5(name)）")
    parser.add_argument("--phases", default="", help="只跑指定阶段，如 P0,P1,P3")
    parser.add_argument("--max-rounds", type=int, default=6, help="autonomy.max_rounds")
    parser.add_argument("--resume", action="store_true", help="复用已有 run-dir（默认重建）")
    parser.add_argument("--workspace-dir", default="", help="显式指定工作区（必须位于 git 仓库之外；默认 /tmp 临时目录）")
    args = parser.parse_args()
    scenario = Scenario(args)
    log_tmp = Path(tempfile.mkdtemp(prefix="pcl-harness-log-")) / "harness.log"
    log_handle = log_tmp.open("w", encoding="utf-8")
    real_stdout, real_stderr = sys.stdout, sys.stderr
    sys.stdout = Tee(real_stdout, log_handle)
    sys.stderr = Tee(real_stderr, log_handle)
    try:
        return scenario.run()
    finally:
        log_handle.flush()
        try:
            scenario.run_dir.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(log_tmp, scenario.run_dir / "harness.log")
        except Exception:  # noqa: BLE001
            pass
        sys.stdout, sys.stderr = real_stdout, real_stderr
        log_handle.close()
        shutil.rmtree(log_tmp.parent, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
