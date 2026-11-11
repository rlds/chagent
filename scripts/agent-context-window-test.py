#!/usr/bin/env python3
"""上下文窗口管理的真实模型功能测试（task-6）。

在较小的 `context_window` 配置下走通 pico WS -> 真实 gateway -> 真实模型
（默认 deepseek-v4-pro），使真实的工具输出必然把 agent 推过预算，从而触发
生产环境的压缩流水线：

  A  强制压缩 / 工具结果裁剪 + spill 文件（目录 0700 / 文件 0600，
     与原内容逐字节一致，marker <-> 文件一致性，工具配对）
  B  异步摘要（meta.summary，JSONL 条数/跳过数一致性，摘要
     不得包含注入上下文的正文）
  C  回合边界 + 持久上下文替换语义（workspace / runtime / skill-catalog
     经压缩后依然存活且不累积；注入的上下文消息永不作为回合边界；
     自主 task-round 提示以 user 回合形式持久化）
  D  SIGTERM 重启恢复（历史恢复、无重复上下文注入、任务状态存活、
     spill 文件存活、/context 数字自洽）
  E  接近上限时的 context-pressure 提示（中文、kind=context-pressure、
     同 band 去重、不是每一步都发）
  F  /context + provider usage 锚点（window / compressAt / summarizeAt
     公式，usage 与估算处于同一量级，实际数字会被记录）
  G  pruner 幂等性（marker 不叠加，spill 按内容寻址且稳定，
     已裁剪的结果不会被重复计数）
  H  catalog_mode=context 技能目录作为持久上下文（kind=skill-catalog，
     压缩后重新发布，模型仍可见）

只写 scripts/agent-context-window-test.py 和
build/functest-20261030/context/，绝不触碰生产代码。API key 从外部模型配置
读取，既不打印也不落盘（运行结束时会脱敏归档的工作区配置）。

用法：
  scripts/agent-context-window-test.py                       # 全部阶段
  scripts/agent-context-window-test.py --phases A            # 单个分组
  scripts/agent-context-window-test.py --phases B            # 摘要分组
  scripts/agent-context-window-test.py --phases C,D,E,F,G,H  # 复用主工作区
  scripts/agent-context-window-test.py --model deepseek-v4-flash
"""
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
import time
import traceback
import urllib.request

import websockets

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DEFAULT_MODEL_CONFIG = "/Users/wudaorui/chessdata/weichat/weichat/config.json"
DEFAULT_OUT = os.path.join(ROOT, "build", "functest-20261030", "context")
DEFAULT_MODEL = "deepseek-v4-pro"

MAIN_GW = "ctx-main"
SUM_GW = "ctx-summary"
MAIN_SID = "ctx-main-session"
SUM_SID = "ctx-summary-session"

WORKSPACE_SENTINEL = "WORKSPACE_SENTINEL_9F3A"
SKIP_CONTEXT_SENTINEL = "SKIP_CONTEXT_SENTINEL_7C1D"
SKILL_MARKER = "CTXSKILL_9F3A"
SKILL_NAME = "ctx-probe-skill"
SECOND_SKILL_NAME = "ctx-second-skill"
TASK_ROUND_SEED = "CTX_TURN_BOUNDARY_TEST"
RESTART_SEED = "DSEED_CTX8842"

# 主分组配置：窗口较小，使真实工具输出会触发 isOverContextBudget，
# 但要大到仅裁剪（prune-only）压缩后重建的请求仍能装下。
MAIN_WINDOW = 12000
MAIN_MAX_TOKENS = 1024
# 摘要分组配置：窗口给得宽松（不会主动压缩）；软摘要阈值设得很小，
# 让真实摘要器能够确定性地运行。
SUM_WINDOW = 60000
SUM_MAX_TOKENS = 1024
SUM_MSG_THRESHOLD = 2
SUM_TOKEN_PERCENT = 1

ALL_PHASES = "ABCDEFGH"


# ---------------------------------------------------------------------------
# 记录 / 断言
# ---------------------------------------------------------------------------
class Phase:
    def __init__(self, key, title):
        self.key = key
        self.title = title
        self.assertions = []
        self.evidence = {}
        self.started = now_iso()

    def check(self, aid, ok, detail):
        self.assertions.append({"id": aid, "ok": bool(ok), "detail": str(detail)})
        print(f"  [{'PASS' if ok else 'FAIL'}] {self.key}.{aid}: {detail}")
        return bool(ok)

    def skip(self, aid, reason):
        self.assertions.append({"id": aid, "ok": None, "detail": "SKIP: " + str(reason)})
        print(f"  [SKIP] {self.key}.{aid}: {reason}")

    def fail_all(self, reason):
        self.assertions.append({"id": self.key + ".exception", "ok": False, "detail": reason})

    @property
    def status(self):
        oks = [a["ok"] for a in self.assertions]
        if any(ok is False for ok in oks):
            return "FAIL"
        if any(ok is True for ok in oks):
            return "PASS"
        return "SKIP"

    def to_dict(self):
        return {
            "phase": self.key,
            "title": self.title,
            "status": self.status,
            "started": self.started,
            "assertions": self.assertions,
            "evidence": self.evidence,
        }

    @classmethod
    def from_dict(cls, data):
        p = cls(data.get("phase", "?"), data.get("title", "?"))
        p.assertions = data.get("assertions") or []
        p.evidence = data.get("evidence") or {}
        p.started = data.get("started") or now_iso()
        return p


class Recorder:
    def __init__(self, out_root, phases, model, model_cfg):
        self.out_root = out_root
        self.phases_requested = phases
        self.model = model
        self.model_cfg_masked = model_cfg
        self.started = now_iso()
        self.phases = {}
        self.defects = []
        self.steps = []
        self.cleanup = {}
        self.failure_analysis = []
        self.before_after = []

    def phase(self, key, title):
        p = Phase(key, title)
        self.phases[key] = p
        self.save()
        return p

    def note_step(self, name, reply, extra=None):
        entry = {
            "step": name,
            "at": now_iso(),
            "text": (reply or {}).get("text", "")[:4000],
            "tools": (reply or {}).get("tool_names", []),
            "usage": (reply or {}).get("usage"),
            "context_usage": (reply or {}).get("context_usage"),
            "elapsed_s": round((reply or {}).get("elapsed", 0), 2),
            "error": (reply or {}).get("error"),
        }
        if extra:
            entry.update(extra)
        self.steps.append(entry)

    def drop_phase(self, key):
        """删除某个已写入的阶段，使重跑时替换它而不是追加。"""
        self.phases.pop(key, None)

    def seed_from(self, doc):
        """合并上一份 results.json，使 `--phases <subset>` 重跑时
        只替换所请求的阶段，而保留矩阵的其余部分。"""
        if not doc:
            return
        self.steps = list(doc.get("steps") or [])
        self.defect_candidates = list(doc.get("defect_candidates") or [])
        self.failure_analysis = list(doc.get("failure_analysis") or [])
        self.before_after = list(doc.get("before_after") or [])
        self.started = doc.get("started") or self.started
        old_phases = ""
        for key, data in (doc.get("phases") or {}).items():
            self.phases[key] = Phase.from_dict(data)
            old_phases += key
        self.phases_requested = "".join(sorted(set(self.phases_requested) | set(old_phases)))

    def save(self):
        os.makedirs(self.out_root, exist_ok=True)
        doc = {
            "task": "task-6 context-window management live test",
            "head": git_head(),
            "branch": git_branch(),
            "started": self.started,
            "finished": now_iso(),
            "phases_requested": self.phases_requested,
            "model": self.model,
            "model_config": self.model_cfg_masked,
            "config": {
                "main": {
                    "context_window": MAIN_WINDOW,
                    "max_tokens": MAIN_MAX_TOKENS,
                },
                "summary": {
                    "context_window": SUM_WINDOW,
                    "max_tokens": SUM_MAX_TOKENS,
                    "summarize_message_threshold": SUM_MSG_THRESHOLD,
                    "summarize_token_percent": SUM_TOKEN_PERCENT,
                },
            },
            "phases": {k: v.to_dict() for k, v in self.phases.items()},
            "cleanup": self.cleanup,
            "failure_analysis": self.failure_analysis,
            "before_after": self.before_after,
            "steps": self.steps,
            "defect_candidates": self.defects,
        }
        with open(os.path.join(self.out_root, "results.json"), "w", encoding="utf-8") as f:
            json.dump(doc, f, ensure_ascii=False, indent=2)
        with open(os.path.join(self.out_root, "summary.md"), "w", encoding="utf-8") as f:
            f.write(render_summary(doc))
        return doc


def render_summary(doc):
    lines = []
    lines.append("# " + (doc.get("title") or "task-6 上下文窗口管理实测结果"))
    lines.append("")
    lines.append(f"- 仓库 HEAD：`{doc.get('head')}`（分支 `{doc.get('branch')}`）")
    lines.append(f"- 模型：`{doc.get('model')}`（真实 provider，经真实 gateway + pico WS）")
    lines.append(f"- 开始/结束：{doc.get('started')} → {doc.get('finished')}")
    lines.append(f"- 请求阶段：`{''.join(doc.get('phases_requested', '')) or '-'}`")
    lines.append("")
    lines.append("## 阶段结论")
    lines.append("")
    lines.append("| 阶段 | 结果 | 断言数 | 失败断言 |")
    lines.append("|---|---|---|---|")
    for key, ph in doc.get("phases", {}).items():
        failed = [a["id"] for a in ph["assertions"] if a["ok"] is False]
        lines.append(
            f"| {key} {ph['title']} | **{ph['status']}** | {len(ph['assertions'])} | "
            f"{', '.join(failed) if failed else '-'} |"
        )
    lines.append("")
    for key, ph in doc.get("phases", {}).items():
        lines.append(f"## {key} {ph['title']}")
        lines.append("")
        for a in ph["assertions"]:
            mark = "PASS" if a["ok"] is True else ("FAIL" if a["ok"] is False else "SKIP")
            lines.append(f"- **{mark}** `{a['id']}`：{a['detail']}")
        if ph.get("evidence"):
            lines.append("")
            lines.append("证据：")
            lines.append("")
            lines.append("```json")
            lines.append(json.dumps(ph["evidence"], ensure_ascii=False, indent=2)[:12000])
            lines.append("```")
        lines.append("")
    lines.append("")
    if doc.get("failure_analysis"):
        lines.append("## 失败定性（跑后分析，不改断言原始结果）")
        lines.append("")
        for item in doc["failure_analysis"]:
            lines.append(f"### {item.get('group')} — {item.get('verdict')}")
            lines.append("")
            lines.append(item.get("explanation", ""))
            lines.append("")
            if item.get("defect_ids"):
                lines.append("涉及断言：`" + "`, `".join(item["defect_ids"]) + "`")
                lines.append("")
            if item.get("raw_evidence"):
                lines.append("原始证据：")
                lines.append("")
                lines.append("```json")
                lines.append(json.dumps(item["raw_evidence"], ensure_ascii=False, indent=2)[:8000])
                lines.append("```")
                lines.append("")
    if doc.get("before_after"):
        lines.append("## 修复前后对比")
        lines.append("")
        for item in doc["before_after"]:
            lines.append(f"### {item.get('title')}")
            lines.append("")
            lines.append(item.get("body", ""))
            lines.append("")
            if item.get("evidence"):
                lines.append("```json")
                lines.append(json.dumps(item["evidence"], ensure_ascii=False, indent=2)[:8000])
                lines.append("```")
                lines.append("")
    if doc.get("cleanup"):
        lines.append("## 进程清理")
        lines.append("")
        lines.append("```json")
        lines.append(json.dumps(doc["cleanup"], ensure_ascii=False, indent=2))
        lines.append("```")
        lines.append("")
    if doc.get("defect_candidates"):
        lines.append("## 缺陷候选")
        lines.append("")
        for d in doc["defect_candidates"]:
            lines.append(f"- {d}")
        lines.append("")
    return "\n".join(lines)


# ---------------------------------------------------------------------------
# 小型辅助函数
# ---------------------------------------------------------------------------
def now_iso():
    return time.strftime("%Y-%m-%dT%H:%M:%S%z")


def git_head():
    try:
        return subprocess.check_output(["git", "rev-parse", "--short", "HEAD"], cwd=ROOT, text=True).strip()
    except Exception:
        return "unknown"


def git_branch():
    try:
        return subprocess.check_output(["git", "branch", "--show-current"], cwd=ROOT, text=True).strip()
    except Exception:
        return "unknown"


def load_model_entry(config_path, model_name):
    with open(config_path, encoding="utf-8") as f:
        data = json.load(f)
    for m in data.get("model_list", []):
        if m.get("model_name") == model_name:
            keys = m.get("api_keys") or ([m.get("api_key")] if m.get("api_key") else [])
            if not keys:
                raise SystemExit(f"model {model_name} has no api key")
            entry = {
                "model_name": m["model_name"],
                "model": m.get("model", ""),
                "api_base": m.get("api_base", ""),
                "api_key": keys[0],
            }
            if m.get("provider"):
                entry["provider"] = m["provider"]
            return entry
    raise SystemExit(f"model {model_name!r} not found in {config_path}")


def mask_model_config(cfg):
    return {
        "model_name": cfg.get("model_name"),
        "model": cfg.get("model"),
        "api_base": cfg.get("api_base"),
        "provider": cfg.get("provider", ""),
        "api_key": "<redacted>",
    }


def write_json(path, obj):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w", encoding="utf-8") as f:
        json.dump(obj, f, ensure_ascii=False, indent=2)


def read_text(path, default=""):
    try:
        with open(path, encoding="utf-8", errors="replace") as f:
            return f.read()
    except OSError:
        return default


def health_ok(host, port):
    try:
        with urllib.request.urlopen(f"http://{host}:{port}/health", timeout=2) as r:
            return r.status == 200
    except Exception:
        return False


# ---------------------------------------------------------------------------
# 工作区准备
# ---------------------------------------------------------------------------
def _tools_config(max_read_file_size=200000):
    return {
        "read_file": {"enabled": True, "mode": "bytes", "max_read_file_size": max_read_file_size},
        "write_file": {"enabled": True},
        "edit_file": {"enabled": True},
        "exec": {"enabled": True, "enable_deny_patterns": True, "allow_remote": True, "timeout_seconds": 60},
        "task": {"enabled": True},
        "todo": {"enabled": True},
        "skill_load": {"enabled": True},
        "skills": {"catalog_mode": "context"},
        "web": {"enabled": False},
        "web_fetch": {"enabled": False},
        "mcp": {"servers": {}},
    }


def write_main_config(ws, model_entry, autonomy_enabled=True):
    cfg = {
        "version": 2,
        "model_list": [model_entry],
        "gateway": {"host": "127.0.0.1", "port": 0, "log_level": "debug"},
        "events": {
            "logging": {"enabled": True, "include": ["agent.*"], "min_severity": "info", "include_payload": False}
        },
        "agents": {
            "defaults": {
                "workspace": ws,
                "restrict_to_workspace": True,
                "allow_read_outside_workspace": False,
                "max_tool_iterations": 30,
                "context_window": MAIN_WINDOW,
                "max_tokens": MAIN_MAX_TOKENS,
                # 关闭软摘要触发，避免主分组的压缩证据
                # 被异步摘要抹掉。
                "summarize_message_threshold": 100000,
                "summarize_token_percent": 10000,
                "steering_mode": "all",
                "max_parallel_turns": 20,
                "max_llm_retries": 2,
                "workspace_context": {"enabled": True, "user_global": False, "max_bytes": 32768},
                "runtime_context": {"mode": "snapshot", "refresh_interval_seconds": 86400},
                "autonomy": {
                    "enabled": autonomy_enabled,
                    "max_rounds": 1,
                    "no_progress_rounds": 3,
                    "round_delay_ms": 100,
                    "resume_on_start": False,
                    "send_intermediate": False,
                },
            }
        },
        "tools": _tools_config(),
    }
    write_json(os.path.join(ws, "config.json"), cfg)


def write_summary_config(ws, model_entry):
    cfg = {
        "version": 2,
        "model_list": [model_entry],
        "gateway": {"host": "127.0.0.1", "port": 0, "log_level": "debug"},
        "events": {
            "logging": {"enabled": True, "include": ["agent.*"], "min_severity": "info", "include_payload": False}
        },
        "agents": {
            "defaults": {
                "workspace": ws,
                "restrict_to_workspace": True,
                "max_tool_iterations": 30,
                "context_window": SUM_WINDOW,
                "max_tokens": SUM_MAX_TOKENS,
                "summarize_message_threshold": SUM_MSG_THRESHOLD,
                "summarize_token_percent": SUM_TOKEN_PERCENT,
                "steering_mode": "all",
                "max_parallel_turns": 20,
                "workspace_context": {"enabled": True, "user_global": False, "max_bytes": 32768},
                "runtime_context": {"mode": "snapshot", "refresh_interval_seconds": 86400},
                "autonomy": {"enabled": False},
            }
        },
        "tools": _tools_config(),
    }
    write_json(os.path.join(ws, "config.json"), cfg)


def write_claude_md(ws):
    """工作区指令文件。

    注意：这里刻意不使用 AGENTS.md：agent 定义加载器会把 AGENTS.md 当作
    旧版 agent prompt 消费，并把它排除在 workspace-instructions 上下文投影
    之外（开发期间已验证）。CLAUDE.md 是纯粹的工作区指令来源，用于覆盖
    workspace-instructions 持久上下文路径。
    """
    os.makedirs(ws, exist_ok=True)
    with open(os.path.join(ws, "CLAUDE.md"), "w", encoding="utf-8") as f:
        f.write(
            "# 上下文窗口实测工作区指令\n\n"
            f"唯一标记：{WORKSPACE_SENTINEL}；注入上下文排除标记：{SKIP_CONTEXT_SENTINEL}。\n\n"
            "当被问到工作区指令中的唯一标记时，只回复该标记本身。\n"
        )


def write_big_file(ws, total_bytes=150000):
    marker = "BIGFILE_TAIL_UNIQUE_MARK_9F3A"
    line = "BIGLINE-%05d "
    fixed = len(line % 0) + len("abcdefghijklmnopqrstuvwxyz0123456789") + 1
    per_line_max = 80
    lines = []
    written = 0
    i = 0
    while written < total_bytes:
        payload = f"BIGLINE-{i:05d} " + ("abcdefghijklmnopqrstuvwxyz0123456789" * 3)[: per_line_max - 13]
        lines.append(payload)
        written += len(payload) + 1
        i += 1
    lines.append(marker)
    content = "\n".join(lines) + "\n"
    with open(os.path.join(ws, "big-input.txt"), "w", encoding="utf-8") as f:
        f.write(content)
    return len(content)


def write_skills(ws):
    skills = [
        (SKILL_NAME, f"Probe skill for context window testing {SKILL_MARKER}"),
        (SECOND_SKILL_NAME, "Second probe skill CTXSKILL_SECOND_4B2E"),
    ]
    for name, desc in skills:
        d = os.path.join(ws, "skills", name)
        os.makedirs(d, exist_ok=True)
        with open(os.path.join(d, "SKILL.md"), "w", encoding="utf-8") as f:
            f.write(f"---\nname: {name}\ndescription: {desc}\n---\n\n# {name}\n\n只用于上下文窗口测试。\n")


def prepare_main_workspace(ws, model_entry):
    if os.path.isdir(ws):
        shutil.rmtree(ws)
    os.makedirs(ws)
    write_main_config(ws, model_entry, autonomy_enabled=True)
    write_claude_md(ws)
    big_len = write_big_file(ws)
    write_skills(ws)
    return {"workspace": ws, "big_file_chars": big_len}


def prepare_summary_workspace(ws, model_entry):
    if os.path.isdir(ws):
        shutil.rmtree(ws)
    os.makedirs(ws)
    write_summary_config(ws, model_entry)
    write_claude_md(ws)
    write_skills(ws)
    return {"workspace": ws}


# ---------------------------------------------------------------------------
# gateway 进程
# ---------------------------------------------------------------------------
class Gateway:
    def __init__(self, ws, name):
        self.ws = ws
        self.name = name
        self.proc = None
        self.log = None
        self.log_path = os.path.join(ws, "gateway.log")
        self.host = None
        self.port = None
        self.token = hashlib.md5(name.encode()).hexdigest()

    def _env(self):
        env = dict(os.environ)
        env.update(
            {
                "CHAGENT_HOME": os.path.join(self.ws, "home"),
                "CHAGENT_WORKSPACE": self.ws,
                "CHAGENT_CONFIG": os.path.join(self.ws, "config.json"),
                "CHAGENT_LOG_LEVEL": "debug",
            }
        )
        return env

    def start(self, binary, timeout=60):
        os.makedirs(os.path.join(self.ws, "home"), exist_ok=True)
        self.log = open(self.log_path, "ab")
        self.proc = subprocess.Popen(
            [binary, "gateway", "--name", self.name],
            cwd=self.ws,
            env=self._env(),
            stdout=self.log,
            stderr=self.log,
            start_new_session=True,
        )
        pid_path = os.path.join(self.ws, ".chagent.pid")
        deadline = time.time() + timeout
        while time.time() < deadline and not os.path.exists(pid_path):
            if self.proc.poll() is not None:
                raise RuntimeError(f"gateway exited early rc={self.proc.returncode}\n{read_text(self.log_path)[-4000:]}")
            time.sleep(0.05)
        if not os.path.exists(pid_path):
            raise RuntimeError("gateway pid file did not appear")
        info = json.loads(read_text(pid_path))
        self.host, self.port = info["host"], int(info["port"])
        deadline = time.time() + timeout
        while time.time() < deadline:
            if health_ok(self.host, self.port):
                return self
            time.sleep(0.1)
        raise RuntimeError("gateway /health did not become ready")

    def stop(self, timeout=15):
        rc = None
        if self.proc is not None and self.proc.poll() is None:
            self.proc.send_signal(signal.SIGTERM)
            try:
                rc = self.proc.wait(timeout=timeout)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                rc = self.proc.wait(timeout=5)
        if self.log is not None:
            try:
                self.log.close()
            except Exception:
                pass
            self.log = None
        pid_path = os.path.join(self.ws, ".chagent.pid")
        pid_gone = not os.path.exists(pid_path)
        alive = self.proc is not None and self.proc.poll() is None
        return {"rc": rc, "pid_file_removed": pid_gone, "process_alive": alive}

    def log_text(self):
        return read_text(self.log_path)

    def new_log_since(self, offset):
        data = read_text(self.log_path)
        return data[offset:]


# ---------------------------------------------------------------------------
# pico WS 会话
# ---------------------------------------------------------------------------
class PicoSession:
    def __init__(self):
        self.sock = None

    async def connect(self, gateway, session_id, timeout=30):
        uri = f"ws://{gateway.host}:{gateway.port}/pico/ws?session_id={session_id}"
        self.sock = await websockets.connect(
            uri,
            additional_headers={"Authorization": f"Bearer {gateway.token}"},
            max_size=64 * 1024 * 1024,
        )
        await self.sock.send(json.dumps({"type": "ping", "timestamp": 1}))
        deadline = time.time() + timeout
        while time.time() < deadline:
            msg = json.loads(await asyncio.wait_for(self.sock.recv(), timeout=10))
            if msg.get("type") == "pong":
                return self
        raise RuntimeError("pico pong timeout")

    async def close(self):
        if self.sock is not None:
            try:
                await self.sock.close()
            except Exception:
                pass
            self.sock = None

    async def drain(self, idle=0.2):
        """丢弃服务器端已缓冲的消息（发生在一次超时之后）。"""
        if self.sock is None:
            return
        try:
            while True:
                await asyncio.wait_for(self.sock.recv(), timeout=idle)
        except (asyncio.TimeoutError, Exception):
            return

    async def send(self, session_id, content, timeout=300):
        started = time.time()
        reply = {"text": "", "tool_names": [], "usage": None, "context_usage": None, "error": None}
        try:
            await self.sock.send(
                json.dumps(
                    {
                        "type": "message.send",
                        "id": f"msg-{time.time_ns()}",
                        "session_id": session_id,
                        "payload": {"content": content},
                    }
                )
            )
            while True:
                raw = await asyncio.wait_for(self.sock.recv(), timeout=timeout)
                msg = json.loads(raw)
                payload = msg.get("payload") or {}
                for tc in payload.get("tool_calls") or []:
                    if isinstance(tc, dict):
                        name = tc.get("name") or (tc.get("function") or {}).get("name")
                        if name:
                            reply["tool_names"].append(name)
                if payload.get("usage"):
                    reply["usage"] = payload["usage"]
                if payload.get("context_usage"):
                    reply["context_usage"] = payload["context_usage"]
                if msg.get("type") == "error":
                    reply["error"] = json.dumps(msg, ensure_ascii=False)[:1000]
                    break
                if (
                    msg.get("type") in ("message.create", "message.update")
                    and payload.get("content")
                    and payload.get("kind") not in ("thought", "tool_calls")
                    and not payload.get("placeholder")
                ):
                    reply["text"] = payload["content"]
                    break
        except asyncio.TimeoutError:
            reply["error"] = f"timeout after {timeout}s"
            await self.drain()
        except Exception as exc:  # 连接被重置等
            reply["error"] = f"{type(exc).__name__}: {exc}"
        reply["elapsed"] = time.time() - started
        return reply

    async def command(self, session_id, content, timeout=90, expect=None):
        """发送一条斜杠命令并返回回复文本。

        等待期间，自主轮次可能向同一个 pico 会话发布它们的最终摘要。
        若设置了 `expect`，无关消息会被跳过，而不会被误当作命令的回复。
        """
        reply = await self.send(session_id, content, timeout=timeout)
        if not expect or expect in (reply.get("text") or ""):
            return reply
        deadline = time.time() + timeout
        while time.time() < deadline:
            try:
                raw = await asyncio.wait_for(self.sock.recv(), timeout=max(5, deadline - time.time()))
            except asyncio.TimeoutError:
                break
            except Exception:
                break
            msg = json.loads(raw)
            payload = msg.get("payload") or {}
            text = payload.get("content") or ""
            if payload.get("usage"):
                reply["usage"] = payload["usage"]
            if payload.get("context_usage"):
                reply["context_usage"] = payload["context_usage"]
            if expect in text and payload.get("kind") not in ("thought", "tool_calls") and not payload.get("placeholder"):
                reply["text"] = text
                reply["error"] = None
                return reply
        return reply


# ---------------------------------------------------------------------------
# 会话 JSONL / meta / spill / 日志解析
# ---------------------------------------------------------------------------
def session_paths(ws):
    sdir = os.path.join(ws, "sessions")
    if not os.path.isdir(sdir):
        return None, None
    jsonls = [f for f in os.listdir(sdir) if f.endswith(".jsonl")]
    if not jsonls:
        return None, None
    jsonls.sort(key=lambda f: os.path.getmtime(os.path.join(sdir, f)))
    jsonl = os.path.join(sdir, jsonls[-1])
    meta = jsonl[: -len(".jsonl")] + ".meta.json"
    return jsonl, meta


def read_meta(ws):
    _, meta_path = session_paths(ws)
    if not meta_path or not os.path.exists(meta_path):
        return {}
    try:
        return json.loads(read_text(meta_path))
    except json.JSONDecodeError:
        return {}


def read_history(ws):
    jsonl, _ = session_paths(ws)
    if not jsonl or not os.path.exists(jsonl):
        return []
    out = []
    for raw in read_text(jsonl).splitlines():
        raw = raw.strip()
        if not raw:
            continue
        try:
            out.append(json.loads(raw))
        except json.JSONDecodeError:
            out.append({"role": "?", "content": "", "corrupt_line": raw[:200]})
    return out


PRUNE_RE = re.compile(r"full output: (.+?)\]")
PRUNE_MARKER = "…[tool output middle pruned; full output: "
PLAIN_MARKER = "…[tool output middle pruned]…"


def kind_of(msg):
    ctx = msg.get("context") or {}
    return ctx.get("kind")


def tool_calls_of(msg):
    calls = []
    for tc in msg.get("tool_calls") or []:
        if isinstance(tc, dict):
            calls.append(tc.get("id") or "")
    return calls


def snapshot(ws):
    hist = read_history(ws)
    meta = read_meta(ws)
    kinds = {}
    tool_msgs = []
    tool_call_ids = set()
    tool_result_ids = []
    msgs = []
    pressure = []
    for i, m in enumerate(hist):
        content = m.get("content") or ""
        kind = kind_of(m)
        kinds[kind or "none"] = kinds.get(kind or "none", 0) + 1
        msgs.append({"i": i, "role": m.get("role"), "kind": kind, "len": len(content)})
        if m.get("role") == "assistant":
            for cid in tool_calls_of(m):
                if cid:
                    tool_call_ids.add(cid)
        if m.get("role") == "tool":
            tool_result_ids.append(m.get("tool_call_id") or "")
            paths = PRUNE_RE.findall(content)
            tool_msgs.append(
                {
                    "i": i,
                    "tool_call_id": m.get("tool_call_id") or "",
                    "len": len(content),
                    "plain_markers": content.count(PLAIN_MARKER),
                    "spill_markers": content.count(PRUNE_MARKER),
                    "marker_paths": paths,
                    "has_marker": PRUNE_MARKER in content or PLAIN_MARKER in content,
                    "content": content,
                }
            )
        if kind == "context-pressure":
            pressure.append({"i": i, "len": len(content), "content": content[:400], "digest": (m.get("context") or {}).get("digest")})
    orphans = [cid for cid in tool_result_ids if cid and cid not in tool_call_ids]
    boundary = None
    for i, m in enumerate(hist):
        k = kind_of(m)
        if m.get("role") == "user" and (not k or k == "task-round"):
            boundary = {"i": i, "role": m.get("role"), "kind": k, "len": len(m.get("content") or "")}
            break
    return {
        "line_count": len(hist),
        "meta": meta,
        "kinds": kinds,
        "msgs": msgs,
        "tool_msgs": tool_msgs,
        "pressure": pressure,
        "orphan_tool_results": orphans,
        "first_msg": msgs[0] if msgs else None,
        "first_turn_boundary": boundary,
        "workspace_count": kinds.get("workspace-instructions", 0),
        "runtime_count": kinds.get("runtime-context", 0),
        "skill_count": kinds.get("skill-catalog", 0),
        "taskstate_count": kinds.get("task-state", 0),
        "taskround_count": kinds.get("task-round", 0),
        "pressure_count": kinds.get("context-pressure", 0),
        "max_tool_len": max([t["len"] for t in tool_msgs], default=0),
    }


def spill_inventory(ws):
    root = os.path.join(ws, ".chagent", "spill")
    inv = {"root": root, "root_exists": os.path.isdir(root), "root_mode": None, "dirs": {}, "files": {}}
    if not os.path.isdir(root):
        return inv
    inv["root_mode"] = oct(os.stat(root).st_mode & 0o777)
    for dirpath, dirnames, filenames in os.walk(root):
        for d in dirnames:
            p = os.path.join(dirpath, d)
            inv["dirs"][os.path.relpath(p, root)] = oct(os.stat(p).st_mode & 0o777)
        for f in filenames:
            p = os.path.join(dirpath, f)
            st = os.stat(p)
            with open(p, "rb") as fh:
                data = fh.read()
            inv["files"][p] = {
                "size": st.st_size,
                "mode": oct(st.st_mode & 0o777),
                "mtime": round(st.st_mtime, 3),
                "sha256": hashlib.sha256(data).hexdigest(),
            }
    return inv


COMPRESS_PRUNE_RE = re.compile(r"Forced compression pruned tool results without dropping turns")
COMPRESS_DROP_RE = re.compile(r"Forced compression executed")
NUM_RE = re.compile(r"(\w+)=([^\s]+)")


def parse_compress_events(log_text):
    events = []
    for line in log_text.splitlines():
        if "Forced compression pruned tool results without dropping turns" in line:
            fields = dict(NUM_RE.findall(line))
            events.append(
                {
                    "kind": "prune_only",
                    "raw": line.strip(),
                    "pruned_msgs": int(fields.get("pruned_msgs", 0)),
                    "saved_chars": int(fields.get("saved_chars", 0)),
                    "spilled_msgs": int(fields.get("spilled_msgs", 0)),
                    "new_count": int(fields.get("new_count", 0)),
                }
            )
        elif "Forced compression executed" in line:
            fields = dict(NUM_RE.findall(line))
            events.append(
                {
                    "kind": "drop_turns",
                    "raw": line.strip(),
                    "dropped_msgs": int(fields.get("dropped_msgs", 0)),
                    "pruned_msgs": int(fields.get("pruned_msgs", 0)),
                    "saved_chars": int(fields.get("saved_chars", 0)),
                    "spilled_msgs": int(fields.get("spilled_msgs", 0)),
                    "new_count": int(fields.get("new_count", 0)),
                }
            )
    return events


def parse_summarize_events(log_text):
    out = []
    for line in log_text.splitlines():
        if "Runtime event: agent.session.summarize" in line:
            fields = dict(NUM_RE.findall(line))
            try:
                out.append(
                    {
                        "raw": line.strip(),
                        "summarized_messages": int(fields.get("summarized_messages", 0)),
                        "kept_messages": int(fields.get("kept_messages", 0)),
                        "summary_len": int(fields.get("summary_len", 0)),
                    }
                )
            except ValueError:
                continue
    return out


def context_stats_from_text(text):
    stats = {}
    m = re.search(r"消息数：(\d+)", text)
    if m:
        stats["message_count"] = int(m.group(1))
    m = re.search(r"已用：约 (\d+) / (\d+) tokens（(\d+)%）", text)
    if m:
        stats["used_tokens"] = int(m.group(1))
        stats["total_tokens"] = int(m.group(2))
        stats["used_window_percent"] = int(m.group(3))
    m = re.search(r"历史：约 (\d+) tokens", text)
    if m:
        stats["history_tokens"] = int(m.group(1))
    m = re.search(r"压缩阈值：(\d+) tokens", text)
    if m:
        stats["compress_at_tokens"] = int(m.group(1))
    m = re.search(r"摘要阈值：(\d+) tokens", text)
    if m:
        stats["summarize_at_tokens"] = int(m.group(1))
    m = re.search(r"压缩进度：(\d+)%", text)
    if m:
        stats["compress_percent"] = int(m.group(1))
    m = re.search(r"剩余：约 (\d+) tokens", text)
    if m:
        stats["remaining_tokens"] = int(m.group(1))
    m = re.search(r"工作区指令：(\d+) 个文件，约 (\d+) 字节", text)
    if m:
        stats["workspace_files"] = int(m.group(1))
        stats["workspace_bytes"] = int(m.group(2))
    m = re.search(r"运行时上下文：模式 (\S+?)[，。]", text)
    if m:
        stats["runtime_mode"] = m.group(1)
    m = re.search(r"任务状态：(\S+?)，轮次 (\d+)/(\d+)", text)
    if m:
        stats["task_phase"] = m.group(1)
        stats["task_round"] = int(m.group(2))
        stats["task_max_rounds"] = int(m.group(3))
    return stats


# ---------------------------------------------------------------------------
# 主分组流程步骤
# ---------------------------------------------------------------------------
READ_PROMPT = (
    "请立即调用 read_file 工具读取文件 big-input.txt（不要分页、不要设置 offset/length，"
    "一次性读完整个文件，只调用一次）。读完以后只回复 READ_DONE。"
)
EXEC_PROMPT = (
    "请立即调用 exec 工具执行命令（command 参数必须完全等于：yes E | head -c 9500），"
    "然后只回复 EXEC_DONE。"
)
PING_PROMPT = "只回复 PING"
VERIFY_PROMPT = (
    "请只回复两行，不要调用任何工具：\n"
    "第一行：技能目录中第一个技能的名字。\n"
    "第二行：工作区指令中的唯一标记。"
)
TASK_PROMPT = (
    "请立即调用 task_update 工具，action=start，objective=\"%s\"，然后只回复 TASK_STARTED。" % TASK_ROUND_SEED
)
SEED_PROMPT = "请记住唯一标记 %s，只回复 SEED_OK" % RESTART_SEED


async def run_turn(rec, sess, sid, prompt, name, phase, extra=None):
    reply = await sess.send(sid, prompt)
    rec.note_step(name, reply, extra)
    ok = reply["error"] is None
    phase.check(
        f"{name}.turn_ok",
        ok,
        f"reply={reply['text'][:120]!r} tools={reply['tool_names']} usage={reply['usage']} "
        f"ctx={reply['context_usage']} elapsed={round(reply.get('elapsed', 0), 1)}s err={reply['error']}",
    )
    return reply


async def main_flow(rec, args, model_entry, phases):
    ws = os.path.join(rec.out_root, "workspaces", "main")
    pre_hist = read_history(ws)
    run_a = "A" in phases or (bool(set(phases) & set("CDEFGH")) and len(pre_hist) < 3)
    fresh = run_a and ("A" in phases or len(pre_hist) < 3)
    if fresh:
        prep = prepare_main_workspace(ws, model_entry)
        print(f"MAIN workspace prepared: {ws} big_file_chars={prep['big_file_chars']}")
    else:
        # 恢复配置（上一次运行会脱敏 api_key），并保留
        # 复用该工作区的阶段所需的已持久化历史/状态。
        write_main_config(ws, model_entry, autonomy_enabled=True)
        print(f"MAIN workspace reused: {ws} lines={len(pre_hist)}")

    gw = Gateway(ws, MAIN_GW)
    sess = PicoSession()
    state = {
        "ws": ws,
        "snapshots": {},
        "log_offsets": {},
        "compress_events": [],
        "binary": args.binary,
        "window": MAIN_WINDOW,
        "max_tokens": MAIN_MAX_TOKENS,
        "baseline_dir": getattr(args, "baseline", "") or "",
    }

    async def snap(name):
        s = snapshot(ws)
        state["snapshots"][name] = s
        state["log_offsets"][name] = len(gw.log_text())
        rec.note_step(
            "snapshot:" + name,
            {"text": "", "tool_names": [], "usage": None, "context_usage": None},
            {"snapshot": s["line_count"], "kinds": s["kinds"], "tool_max": s["max_tool_len"],
             "pressure": s["pressure_count"]},
        )
        return s

    def log_events_since(name):
        off = state["log_offsets"].get(name, 0)
        return parse_compress_events(gw.log_text()[off:])

    try:
        gw.start(args.binary)
        print(f"MAIN gateway ready on {gw.host}:{gw.port} token=md5({MAIN_GW})")
        await sess.connect(gw, MAIN_SID)
        await snap("start")

        if run_a:
            rec.drop_phase("A")
            phase = rec.phase("A", "forced compression / tool-result pruning + spill")
            await phase_a(rec, phase, sess, gw, ws, state, snap, log_events_since, MAIN_SID)
            rec.save()
        elif "A" in phases:
            phase = rec.phase("A", "forced compression / tool-result pruning + spill")
            phase.skip("A.not_run", "workspace has no history and A was not requested")

        # F 只读取 /context 以及已持久化的历史/步骤，不得新增回合，
        # 否则已归档的 notice/usage 配对会发生偏移。
        need_verify = bool({"C", "E", "G", "H"} & set(phases))
        if need_verify:
            rec.drop_phase("V")
            await verify_flow(rec, sess, ws, state, snap)
            rec.save()
        # 先跑只读阶段：自主任务轮会向同一个 pico 会话发布异步最终摘要，
        # 不能与 /context 发生竞争。
        if "E" in phases:
            rec.drop_phase("E")
            phase = rec.phase("E", "near-limit context-pressure notices")
            phase_e(rec, phase, state, gw)
        if "G" in phases:
            rec.drop_phase("G")
            phase = rec.phase("G", "pruner idempotency")
            phase_g(rec, phase, state, gw)
        if "H" in phases:
            rec.drop_phase("H")
            phase = rec.phase("H", "skill catalog as durable context")
            phase_h(rec, phase, state, gw)
        if "F" in phases:
            rec.drop_phase("F")
            phase = rec.phase("F", "/context + usage anchors")
            await phase_f(rec, phase, sess, ws, state, MAIN_SID)
        if "C" in phases:
            rec.drop_phase("C")
            await task_round_step(rec, sess, gw, ws, state, snap)
            rec.save()
            phase = rec.phase("C", "turn boundaries + context replace")
            phase_c(rec, phase, state, gw, ws)
        if "D" in phases:
            rec.drop_phase("D")
            phase = rec.phase("D", "SIGTERM restart recovery")
            await phase_d(rec, phase, sess, gw, ws, state, MAIN_SID)
        rec.save()
    except Exception:
        tb = traceback.format_exc()
        print(tb)
        for key in ("A", "C", "D", "E", "F", "G", "H"):
            if key in phases:
                ph = rec.phases.get(key) or rec.phase(key, key)
                ph.fail_all("main flow exception: " + tb.splitlines()[-1])
        rec.save()
    finally:
        try:
            await sess.close()
        except Exception:
            pass
        try:
            stop_info = gw.stop()
        except Exception:
            stop_info = {"error": traceback.format_exc()}
        rec.cleanup["main_gateway"] = stop_info
        rec.cleanup["no_residual_gateway"] = (
            not stop_info.get("process_alive") and stop_info.get("pid_file_removed") is True
        )
        try:
            archive_evidence_snapshots(rec, state)
        except Exception:
            print("warn: archive snapshots failed: " + traceback.format_exc())
        copy_evidence_logs(rec, ws, "gateway-main.log")
        redact_workspace_config(ws)
        rec.save()


async def phase_a(rec, phase, sess, gw, ws, state, snap, log_events_since, sid):
    await snap("A.pre")
    phase.evidence["config"] = {
        "context_window": MAIN_WINDOW,
        "max_tokens": MAIN_MAX_TOKENS,
        "summarize_message_threshold": 100000,
        "summarize_token_percent": 10000,
    }

    # --- A1：先制造一条超大的真实工具结果，再由一个普通回合触发主动压缩
    reply = await run_turn(rec, sess, sid, READ_PROMPT, "A1.read_big_file", phase)
    s1 = await snap("A1.after_read")
    biggest = max(s1["tool_msgs"], key=lambda t: t["len"], default=None)
    phase.evidence["A1_read"] = {
        "tool_messages": [(t["tool_call_id"], t["len"], t["has_marker"]) for t in s1["tool_msgs"]],
        "max_tool_len": s1["max_tool_len"],
        "read_tools": reply["tool_names"],
        "read_usage": reply["usage"],
        "read_context_usage": reply["context_usage"],
    }
    if not biggest or biggest["len"] <= 8192:
        # 确定性兜底：两次 exec 回合（每次上限 10000 字符）
        phase.evidence["A1_fallback"] = "oversized read not produced; using exec fallback"
        for i in range(2):
            await run_turn(rec, sess, sid, EXEC_PROMPT, f"A1.fallback_exec_{i}", phase)
            await snap(f"A1.fallback_exec_{i}")
        s1 = await snap("A1.pre_compress")
        biggest = max(s1["tool_msgs"], key=lambda t: t["len"], default=None)
    phase.check(
        "A1.oversized_tool_result",
        bool(biggest) and biggest["len"] > 8192,
        f"max tool result len={s1['max_tool_len']} (>8192 required) tool_msgs={len(s1['tool_msgs'])} "
        f"history_lines={s1['line_count']}",
    )
    rough_tokens = int(sum(len((m.get("content") or "")) for m in read_history(ws)) * 0.4)
    phase.evidence["A1_precompress"] = {
        "history_lines": s1["line_count"],
        "max_tool_len": s1["max_tool_len"],
        "rough_history_tokens_ascii": rough_tokens,
        "kinds": s1["kinds"],
        "orig_by_tool_id": {t["tool_call_id"]: t["len"] for t in s1["tool_msgs"]},
    }

    reply2 = await run_turn(rec, sess, sid, PING_PROMPT, "A1.ping_after_big", phase)
    s2 = await snap("A1.post_compress")
    events = parse_compress_events(gw.log_text())
    state["compress_events"] = events
    phase.evidence["A1_compress_events"] = events
    prune_only = [e for e in events if e["kind"] == "prune_only"]
    phase.check(
        "A1.prune_only_log",
        len(prune_only) >= 1,
        f"'pruned tool results without dropping turns' lines={len(prune_only)} events={events}",
    )
    if prune_only:
        ev = prune_only[0]
        phase.check(
            "A1.prune_log_fields",
            ev["pruned_msgs"] >= 1 and ev["spilled_msgs"] >= 1 and ev["saved_chars"] > 0,
            f"pruned_msgs={ev['pruned_msgs']} spilled_msgs={ev['spilled_msgs']} "
            f"saved_chars={ev['saved_chars']} new_count={ev['new_count']}",
        )
        phase.evidence["A1_saved_chars"] = ev["saved_chars"]

    inv = spill_inventory(ws)
    state["spill_after_A1"] = inv
    phase.evidence["A1_spill"] = {
        "root": inv["root"],
        "root_exists": inv["root_exists"],
        "root_mode": inv["root_mode"],
        "dir_modes": inv["dirs"],
        "files": {k: {kk: v[kk] for kk in ("size", "mode", "sha256")} for k, v in inv["files"].items()},
    }
    phase.check("A1.spill_root_exists", inv["root_exists"], f"{inv['root']} exists={inv['root_exists']}")
    phase.check(
        "A1.spill_permissions",
        inv["root_mode"] in (None, "0o700")
        and all(m == "0o700" for m in inv["dirs"].values())
        and all(f["mode"] == "0o600" for f in inv["files"].values()),
        f"root_mode={inv['root_mode']} dir_modes={inv['dirs']} "
        f"file_modes={[f['mode'] for f in inv['files'].values()]}",
    )
    phase.check("A1.spill_files_created", len(inv["files"]) >= 1, f"files={list(inv['files'])}")

    markers = [t for t in s2["tool_msgs"] if t["spill_markers"]]
    phase.check(
        "A1.marker_in_history",
        len(markers) >= 1,
        f"pruned tool messages carrying spill marker={len(markers)} tool_msgs={len(s2['tool_msgs'])}",
    )
    orig_lens = phase.evidence["A1_precompress"]["orig_by_tool_id"]
    if markers:
        t = markers[0]
        path = t["marker_paths"][0] if t["marker_paths"] else ""
        info = inv["files"].get(path)
        content = t["content"]
        phase.check("A1.marker_path_resolves", bool(info), f"marker path={path} in spill inventory={bool(info)}")
        phase.check(
            "A1.marker_contains_spill_path",
            bool(path) and path in content,
            f"marker text embeds the spilled absolute path={bool(path) and path in content}",
        )
        if info and path:
            spill_text = read_text(path)
            head = content[:4096]
            tail = content[-1024:]
            phase.check(
                "A1.spill_content_matches_head",
                spill_text.startswith(head),
                f"spill[{len(spill_text)} chars] startsWith pruned head(4096)={spill_text.startswith(head)}",
            )
            phase.check(
                "A1.spill_content_matches_tail",
                spill_text.endswith(tail),
                f"spill endsWith pruned tail(1024)={spill_text.endswith(tail)}",
            )
            orig_len = orig_lens.get(t["tool_call_id"])
            if orig_len is None:
                orig_len = biggest["len"] if biggest else None
            if orig_len is not None:
                phase.check(
                    "A1.spill_size_equals_original",
                    abs(info["size"] - orig_len) <= 2,
                    f"spill file size={info['size']} bytes, original tool result chars={orig_len} "
                    f"(saved_chars={phase.evidence.get('A1_saved_chars')})",
                )
        state["phaseA1"] = {"compress_events": events, "snapshot": s2, "spill": inv, "marker_tool_id": t["tool_call_id"]}
    else:
        state["phaseA1"] = {"compress_events": events, "snapshot": s2, "spill": inv, "marker_tool_id": None}

    phase.check(
        "A1.tool_pairing_intact",
        not s2["orphan_tool_results"],
        f"orphan tool results={s2['orphan_tool_results']} "
        f"tool result ids={[t['tool_call_id'] for t in s2['tool_msgs']]}",
    )
    phase.check(
        "A1.boundary_not_context",
        bool(s2["first_turn_boundary"]),
        f"first real user/task-round turn boundary={s2['first_turn_boundary']} first_msg={s2['first_msg']}",
    )
    phase.check(
        "A1.conversation_continues",
        reply2["error"] is None and "PING" in (reply2["text"] or ""),
        f"post-compression reply={(reply2['text'] or '')[:120]!r} err={reply2['error']}",
    )
    if reply2["context_usage"]:
        phase.evidence["A1_after_compress_context_usage"] = reply2["context_usage"]

    # --- A2：不断追加真实的 10000 字符 exec 输出，直到触发丢弃回合（drop-turns）分支
    exec_rounds = 0
    all_events = list(parse_compress_events(gw.log_text()))
    for i in range(8):
        if any(e["kind"] == "drop_turns" for e in all_events):
            break
        before_ids = {t["tool_call_id"] for t in snapshot(ws)["tool_msgs"]}
        await run_turn(rec, sess, sid, EXEC_PROMPT, f"A2.exec_{i}", phase)
        s_exec = await snap(f"A2.exec_{i}")
        new_big = [t for t in s_exec["tool_msgs"] if t["tool_call_id"] not in before_ids and t["len"] > 8192]
        if not new_big:
            phase.evidence.setdefault("A2_exec_retries", []).append(f"round {i}: no oversized exec result, retrying")
            await run_turn(rec, sess, sid, EXEC_PROMPT, f"A2.exec_{i}_retry", phase)
            s_exec = await snap(f"A2.exec_{i}_retry")
        all_events = parse_compress_events(gw.log_text())
        if any(e["kind"] == "drop_turns" for e in all_events):
            exec_rounds = i + 1
            break
        await run_turn(rec, sess, sid, PING_PROMPT, f"A2.ping_{i}", phase)
        await snap(f"A2.ping_{i}")
        all_events = parse_compress_events(gw.log_text())
        exec_rounds = i + 1
    drop_events = [e for e in all_events if e["kind"] == "drop_turns"]
    if not drop_events:
        # 最后兜底：再读一次超大文件（约 150KB），然后发一次 ping
        phase.evidence["A2_fallback"] = "drop-turns branch not reached; extra oversized read"
        await run_turn(rec, sess, sid, READ_PROMPT, "A2.fallback_read", phase)
        await snap("A2.fallback_read")
        await run_turn(rec, sess, sid, PING_PROMPT, "A2.fallback_ping", phase)
        await snap("A2.fallback_ping")
        all_events = parse_compress_events(gw.log_text())
        drop_events = [e for e in all_events if e["kind"] == "drop_turns"]
    state["compress_events"] = all_events
    s_final = await snap("A2.final")
    phase.evidence["A2_compress_events"] = all_events
    phase.evidence["A2_exec_rounds"] = exec_rounds
    phase.check(
        "A2.drop_turns_log",
        len(drop_events) >= 1,
        f"'Forced compression executed' events={len(drop_events)} all_events={all_events}",
    )
    if drop_events:
        ev = drop_events[0]
        phase.check(
            "A2.drop_log_fields",
            ev["dropped_msgs"] > 0,
            f"dropped_msgs={ev['dropped_msgs']} pruned_msgs={ev['pruned_msgs']} "
            f"saved_chars={ev['saved_chars']} new_count={ev['new_count']}",
        )
        phase.evidence["A2_drop_event"] = ev
    phase.check(
        "A2.boundary_first_msg",
        bool(s_final["first_msg"])
        and s_final["first_msg"]["role"] == "user"
        and (not s_final["first_msg"]["kind"] or s_final["first_msg"]["kind"] == "task-round"),
        f"first active message after drop={s_final['first_msg']}",
    )
    phase.check(
        "A2.tool_pairing_intact",
        len(s_final["orphan_tool_results"]) == 0,
        f"orphan tool results={s_final['orphan_tool_results']} "
        f"active tool result ids={[t['tool_call_id'] for t in s_final['tool_msgs']]}",
    )
    marker_msgs = [t for t in s_final["tool_msgs"] if t["has_marker"]]
    phase.check(
        "A2.markers_survive",
        len(marker_msgs) >= 1,
        f"marker-bearing tool messages retained={len(marker_msgs)} of {len(s_final['tool_msgs'])}",
    )
    state["phaseA2"] = {"snapshot": s_final, "events": all_events}


async def verify_flow(rec, sess, ws, state, snap):
    """一次低成本的真实回合，在压缩后重新注入全部持久上下文。"""
    phase = rec.phase("V", "durable context re-injection verification")
    reply = await sess.send(MAIN_SID, VERIFY_PROMPT)
    rec.note_step("V.context_answer", reply)
    s = await snap("V.after_verify")
    text = reply["text"] or ""
    phase.check(
        "V.turn_ok",
        reply["error"] is None,
        f"reply={text[:200]!r} tools={reply['tool_names']} err={reply['error']}",
    )
    phase.check(
        "V.workspace_instructions_visible",
        WORKSPACE_SENTINEL in text,
        f"workspace sentinel in model answer={WORKSPACE_SENTINEL in text!r} answer={text[:240]!r}",
    )
    catalogs = [
        (m.get("content") or "") for m in read_history(ws) if kind_of(m) == "skill-catalog"
    ]
    phase.check(
        "V.skill_catalog_visible",
        SKILL_NAME in text or any(SKILL_NAME in c for c in catalogs),
        f"skill name in model answer={SKILL_NAME in text!r} in catalog context={any(SKILL_NAME in c for c in catalogs)} "
        f"answer={text[:240]!r}",
    )
    phase.check(
        "V.runtime_context_present",
        s["runtime_count"] == 1,
        f"runtime-context messages={s['runtime_count']} kinds={s['kinds']}",
    )
    phase.evidence["verify_answer"] = text[:1000]
    phase.evidence["verify_context_usage"] = reply["context_usage"]
    phase.evidence["snapshot_kinds"] = s["kinds"]
    state["verify"] = {"snapshot": s, "reply": reply}


async def task_round_step(rec, sess, gw, ws, state, snap):
    """启动一个持久任务，让自主调度器跑一个任务轮。"""
    phase = rec.phase("C", "turn boundaries + context replace")
    task_reply = await sess.send(MAIN_SID, TASK_PROMPT)
    rec.note_step("C.task_start", task_reply)
    if "task_update" not in (task_reply["tool_names"] or []) and read_meta(ws).get("task_state") is None:
        retry_prompt = (
            "必须调用 task_update 工具：action=\"start\"，objective=\"%s\"，"
            "不要只解释，调用完成后只回复 TASK_STARTED。" % TASK_ROUND_SEED
        )
        task_reply = await sess.send(MAIN_SID, retry_prompt)
        rec.note_step("C.task_start_retry", task_reply)
    deadline = time.time() + 180
    task_round_msg = None
    autonomy_logged = False
    phase.check(
        "C.task_update_called",
        "task_update" in (task_reply["tool_names"] or []) or (read_meta(ws).get("task_state") is not None),
        f"tools={task_reply['tool_names']} task_state={read_meta(ws).get('task_state')} err={task_reply['error']}",
    )
    while time.time() < deadline:
        for m in read_history(ws):
            if kind_of(m) == "task-round" or (m.get("role") == "user" and "<task_round>" in (m.get("content") or "")):
                task_round_msg = m
                break
        autonomy_logged = "autonomy round scheduled" in gw.log_text()
        if task_round_msg and autonomy_logged:
            break
        await asyncio.sleep(0.5)
    # 等自主轮结束，避免后续 /context 命令
    # 与它的出站摘要相互交错
    settle_deadline = time.time() + 120
    while time.time() < settle_deadline:
        st = (read_meta(ws).get("task_state") or {})
        if st.get("phase") not in (None, "active"):
            break
        await asyncio.sleep(0.5)
    await asyncio.sleep(0.5)
    await sess.drain(idle=0.3)
    s2 = await snap("V.task_round")
    meta = read_meta(ws)
    state["task_round"] = {
        "message": task_round_msg,
        "snapshot": s2,
        "meta_task_state": meta.get("task_state"),
        "autonomy_logged": autonomy_logged,
        "task_reply": task_reply,
    }
    phase.check(
        "C.autonomy_round_scheduled",
        autonomy_logged,
        f"log has 'autonomy round scheduled'={autonomy_logged}",
    )
    phase.check(
        "C.task_round_is_turn_start",
        bool(task_round_msg) and task_round_msg.get("role") == "user",
        f"task-round user message present={bool(task_round_msg)} role={(task_round_msg or {}).get('role')} "
        f"kind={kind_of(task_round_msg or {})} content={(task_round_msg or {}).get('content', '')[:160]!r}",
    )
    phase.evidence["task_round_message"] = {
        "role": (task_round_msg or {}).get("role"),
        "kind": kind_of(task_round_msg or {}),
        "content": (task_round_msg or {}).get("content", "")[:800],
    }
    phase.evidence["task_state_meta"] = meta.get("task_state")


def phase_c(rec, phase, state, gw, ws):
    snap_final = snapshot(ws)
    events = state.get("compress_events") or []
    phase.evidence["final_snapshot_kinds"] = snap_final["kinds"]
    phase.evidence["compress_events"] = events
    # 1. 注入的上下文消息只是搭车消息，永远不是回合边界
    first = snap_final["first_msg"]
    phase.check(
        "C.compression_keeps_turn_boundary",
        bool(first) and first["role"] == "user" and (not first["kind"] or first["kind"] == "task-round"),
        f"first active message after compressions={first}",
    )
    # 2. 持久上下文的替换语义：模型可见历史中恰好只有一份副本
    for kind, label, aid in (
        ("workspace-instructions", "workspace", "C.workspace_single_copy"),
        ("runtime-context", "runtime", "C.runtime_single_copy"),
        ("skill-catalog", "skill catalog", "C.skill_catalog_single_copy"),
    ):
        count = snap_final["kinds"].get(kind, 0)
        phase.check(aid, count == 1, f"{label} context messages in active history={count} (want exactly 1)")
    # 3. 不存在正文完全相同的重复上下文
    bodies = {}
    for m in read_history(ws):
        k = kind_of(m)
        if k in ("runtime-context", "skill-catalog", "workspace-instructions"):
            c = m.get("content") or ""
            bodies.setdefault((k, c), 0)
            bodies[(k, c)] += 1
    dups = {f"{k}:{i}": n for (k, i), n in bodies.items() if n > 1}
    phase.check("C.no_duplicate_context_bodies", not dups, f"duplicates={str(dups)[:500]}")
    # 4. 反复的普通回合不会累积上下文副本
    counts_across = {}
    for name, s in state.get("snapshots", {}).items():
        counts_across[name] = {
            "workspace": s["workspace_count"],
            "runtime": s["runtime_count"],
            "skill": s["skill_count"],
        }
    phase.evidence["context_counts_across_snapshots"] = counts_across
    bad = {n: c for n, c in counts_across.items() if any(v > 1 for v in c.values())}
    phase.check("C.no_context_accumulation", not bad, f"snapshots with >1 copy={bad}")
    # 5. task-round 证据
    tr = state.get("task_round") or {}
    phase.check(
        "C.task_round_persisted_as_user_turn",
        bool(tr.get("message")) and tr["message"].get("role") == "user",
        f"autonomy round persisted user message={bool(tr.get('message'))} "
        f"kind={kind_of(tr.get('message') or {})}",
    )
    if tr.get("message") and kind_of(tr["message"]) != "task-round":
        rec.defects.append(
            "ContextKindTaskRound 仅在 model 返回空内容且任务仍 active 时由 appendTaskContinuation 注入；"
            "普通自主轮次的 task_round 提示以普通 user 消息持久化，因此直播内难以观察到 kind=task-round 的持久上下文"
            "（本轮观察到的是普通 user 轮次起点，isTurnBoundary 行为正确，但 kind 断言只能部分覆盖）。"
        )


def analyze_turns(hist):
    """把历史切分为回合，并标注 context-pressure 提示。

    一个回合以真实用户消息或 task-round 消息开头；注入的上下文消息只是搭车
    消息。`before_first_assistant` 标记的是在回合的第一次 LLM 调用
    （SetupTurn 路径，D5 修复）时注入的提示，而不是工具结果之后注入的提示。
    """
    turns = []
    cur = None
    for i, m in enumerate(hist):
        k = kind_of(m)
        is_boundary = m.get("role") == "user" and (not k or k == "task-round")
        if is_boundary or cur is None:
            cur = {"boundary_i": i if is_boundary else -1, "items": [], "pressure": []}
            turns.append(cur)
        cur["items"].append((i, m))
        if k == "context-pressure":
            content = m.get("content") or ""
            used = None
            mm = re.search(r"上下文使用约 (\d+) tokens", content)
            if mm:
                used = int(mm.group(1))
            cur["pressure"].append(
                {"i": i, "digest": (m.get("context") or {}).get("digest"), "content": content, "used_tokens": used}
            )
    for t in turns:
        first_asst = next((i for i, m in t["items"] if m.get("role") == "assistant"), None)
        for p in t["pressure"]:
            p["before_first_assistant"] = first_asst is None or p["i"] < first_asst
    return [t for t in turns if t["boundary_i"] >= 0]


def read_history_file(jsonl_path):
    if not jsonl_path or not os.path.exists(jsonl_path):
        return []
    out = []
    for raw in read_text(jsonl_path).splitlines():
        raw = raw.strip()
        if not raw:
            continue
        try:
            out.append(json.loads(raw))
        except json.JSONDecodeError:
            continue
    return out


ANCHOR_TURN_MARKERS = [
    ("big-input.txt", "A1.read_big_file"),
    ("yes E | head -c 9500", "A2.exec"),
    ("只回复 PING", "A2.ping"),
    ("技能目录中第一个技能的名字", "V.context_answer"),
    ("CTX_TURN_BOUNDARY_TEST", "C.task_start"),
    ("DSEED_CTX8842", "D.seed"),
    ("AFTER_RESTART", "D.after_restart_ping"),
    ("BSUM_SEED_123", "B.turn1"),
    ("只回复 OK2", "B.turn2"),
    ("只回复 OK3", "B.turn3"),
    ("只回复 OK4", "B.turn4"),
]


def pair_notices_with_steps(hist, steps):
    """把 context-pressure 提示与对应回合的 provider usage 配对。

    回合通过其（唯一的）用户提示标记与已记录的步骤匹配，因此对于被压缩
    丢弃的回合、以及不产生记录步骤的额外自主轮，都具有鲁棒性。
    """
    turns = analyze_turns(hist)
    used = set()
    pairs = []
    for turn in turns:
        if not turn["items"]:
            continue
        user_content = turn["items"][0][1].get("content") or ""
        step = None
        for needle, prefix in ANCHOR_TURN_MARKERS:
            if needle in user_content:
                cands = [s for s in steps if s["step"].startswith(prefix) and s["step"] not in used]
                if cands:
                    step = cands[0]
                    used.add(step["step"])
                    break
        if step is None or not step.get("usage"):
            continue
        provider = (step.get("usage") or {}).get("prompt_tokens") or 0
        tools = step.get("tools") or []
        for notice in turn.get("pressure") or []:
            notice_used = notice.get("used_tokens")
            if not notice_used or provider <= 0:
                continue
            before_first = bool(notice.get("before_first_assistant"))
            pairs.append(
                {
                    "step": step["step"],
                    "step_tools": tools,
                    "provider_prompt_tokens": provider,
                    "notice_used_tokens": notice_used,
                    "ratio_provider_over_notice": round(provider / notice_used, 3),
                    "before_first_assistant": before_first,
                    # 只有当该回合之后没有发生工具调用时，首调用提示才可比
                    # （否则最终的 provider 请求会包含该提示
                    # 看不到的工具输出）。
                    "valid_pair": (not before_first) or (not tools),
                    "digest": notice.get("digest"),
                }
            )
    return pairs


def median_abs_dev(pairs):
    devs = sorted(abs(1 - p["ratio_provider_over_notice"]) for p in pairs)
    if not devs:
        return None
    return round(devs[len(devs) // 2], 3)


def phase_e(rec, phase, state, gw):
    ws = state["ws"]
    hist = read_history(ws)
    turns = analyze_turns(hist)
    final_snap = snapshot(ws)
    pressure = [dict(p) for p in final_snap.get("pressure", [])]
    turn_of = {}
    for ti, t in enumerate(turns):
        for p in t["pressure"]:
            turn_of[p["i"]] = (ti, p)
    injected = []
    for ti, t in enumerate(turns):
        for p in t["pressure"]:
            injected.append({"turn": ti, **p})
    observed = {}
    for name, s in state.get("snapshots", {}).items():
        for p in s.get("pressure", []):
            observed.setdefault((p["digest"], p["content"]), {"snapshot": name, **p})
    bands = sorted({p.get("digest") for p in injected if p.get("digest")})
    per_turn = {}
    band_per_turn = {}
    viol_band = []
    for p in injected:
        per_turn[p["turn"]] = per_turn.get(p["turn"], 0) + 1
        key = (p["turn"], p.get("digest"))
        band_per_turn[key] = band_per_turn.get(key, 0) + 1
    for (ti, digest), n in band_per_turn.items():
        if n > 1:
            viol_band.append({"turn": ti, "digest": digest, "count": n})
    first_iter = [p for p in injected if p.get("before_first_assistant")]
    phase.evidence["pressure_messages"] = [
        {"turn": p["turn"], "i": p["i"], "digest": p["digest"], "len": len(p["content"]),
         "used_tokens": p.get("used_tokens"), "before_first_assistant": p.get("before_first_assistant"),
         "excerpt": p["content"][:200]} for p in injected
    ]
    phase.evidence["pressure_observed_any_snapshot"] = [
        {"snapshot": p["snapshot"], "digest": p["digest"], "excerpt": p["content"][:200]}
        for p in observed.values()
    ]
    phase.evidence["pressure_summary"] = {
        "final_active_history_lines": len(hist),
        "real_user_turns": len(turns),
        "pressure_messages_in_active_history": len(injected),
        "bands": bands,
        "per_turn_counts": per_turn,
        "band_per_turn": {f"turn{k[0]}:{k[1]}": v for k, v in band_per_turn.items()},
        "band_duplicate_violations": viol_band,
        "first_iteration_triggered_notices": len(first_iter),
        "first_iteration_examples": [
            {"turn": p["turn"], "digest": p["digest"], "used_tokens": p.get("used_tokens"),
             "excerpt": p["content"][:160]} for p in first_iter[:3]
        ],
    }
    phase.check("E.pressure_message_exists", len(injected) >= 1, f"context-pressure messages={len(injected)}")
    chinese = [p for p in injected if "<context_pressure>" in p["content"] and "上下文使用约" in p["content"]]
    phase.check(
        "E.pressure_chinese",
        len(chinese) == len(injected) and len(chinese) >= 1,
        f"messages with <context_pressure> + Chinese body={len(chinese)}/{len(injected)}",
    )
    phase.check(
        "E.pressure_bands",
        all(b in ("0.85", "0.95") for b in bands) and len(bands) >= 1,
        f"observed bands={bands} (allowed 0.85/0.95)",
    )
    phase.check(
        "E.pressure_band_once_per_turn",
        not viol_band,
        f"same-band notices duplicated within a turn={viol_band} "
        f"(band-once-per-turn dedupe; per turn counts={per_turn})",
    )
    phase.check(
        "E.pressure_per_turn_bounded",
        all(v <= 2 for v in per_turn.values()),
        f"per-turn notice counts={per_turn} (<=2: one per band 0.85/0.95)",
    )
    consecutive = 0
    prev = False
    for m in final_snap.get("msgs", []):
        cur = m.get("kind") == "context-pressure"
        if cur and prev:
            consecutive += 1
        prev = cur
    phase.check(
        "E.pressure_not_every_step",
        consecutive == 0 and len(injected) <= 2 * len(turns),
        f"consecutive pressure pairs={consecutive} notices={len(injected)} "
        f"real user turns={len(turns)} (<=2 per turn expected)",
    )
    if first_iter:
        phase.check(
            "E.pressure_first_iteration_trigger",
            True,
            f"notices injected before the turn's first assistant reply (SetupTurn/first LLM call)="
            f"{len(first_iter)} examples={json.dumps(phase.evidence['pressure_summary']['first_iteration_examples'], ensure_ascii=False)[:400]}",
        )
    else:
        phase.skip("E.pressure_first_iteration_trigger",
                   "本轮未观察到 SetupTurn（iteration 0）注入的 notice；已记录每 turn 次数/band 去重证据")


def phase_g(rec, phase, state, gw):
    ws = state["ws"]
    inv_a1 = state.get("spill_after_A1") or {}
    snap_final = snapshot(ws)
    inv_final = spill_inventory(ws)
    events = state.get("compress_events") or []
    if not events:
        events = parse_compress_events(gw.log_text())
    phase.evidence["compress_events"] = events
    phase.evidence["spill_counts"] = {"after_A1": len(inv_a1.get("files", {})), "final": len(inv_final["files"])}

    # 1. 活动历史中任何位置都没有 marker 叠加
    stacked = []
    for t in snap_final["tool_msgs"]:
        total = t["plain_markers"] + t["spill_markers"]
        if total > 1:
            stacked.append({"id": t["tool_call_id"], "markers": total})
    phase.check("G.no_marker_stacking", not stacked, f"tool messages with >1 marker={stacked}")

    # 2. spill 命名按内容寻址并做完整性校验：每个文件名
    #    都内嵌 sha256(content)[:16]，且摘要与磁盘字节一致。
    bad_names = []
    for path, info in inv_final["files"].items():
        name = os.path.basename(path)
        m = re.search(r"-([0-9a-f]{16})\.txt$", name)
        if not m or m.group(1) != info["sha256"][:16]:
            bad_names.append({"file": name, "expected_prefix": info["sha256"][:16]})
    phase.check(
        "G.spill_content_addressed_naming",
        not bad_names,
        f"spill files whose name does not embed matching sha256[:16]={bad_names}",
    )

    # 3. 更早压缩轮次写下的 spill 文件永远不会被重写
    stable = []
    for path, info in (inv_a1.get("files") or {}).items():
        cur = inv_final["files"].get(path)
        stable.append(bool(cur) and cur["size"] == info["size"] and cur["sha256"] == info["sha256"])
    if len(stable) == 0:
        phase.skip("G.earlier_spill_files_stable", "no A1 spill baseline captured")
    else:
        phase.check(
            "G.earlier_spill_files_stable",
            all(stable),
            f"earlier spill files unchanged across later compressions={stable} "
            f"files={[os.path.basename(p) for p in (inv_a1.get('files') or {})][:3]}",
        )

    # 4. 已裁剪过的工具结果会被跳过，绝不重复计数
    prune_only = [e for e in events if e["kind"] == "prune_only"]
    noop = [e for e in events if e.get("pruned_msgs") == 0]
    phase.evidence["idempotency"] = {
        "compress_events": len(events),
        "prune_only_events": len(prune_only),
        "drop_events": len([e for e in events if e["kind"] == "drop_turns"]),
        "no_op_prune_events": [
            {"kind": e["kind"], "pruned_msgs": e.get("pruned_msgs"), "saved_chars": e.get("saved_chars")}
            for e in noop
        ],
        "sum_pruned_msgs": sum(e.get("pruned_msgs", 0) for e in events),
    }
    phase.check(
        "G.pruner_ran_multiple_times",
        len(events) >= 2,
        f"compression events={len(events)} (>=2 required for idempotency evidence)",
    )
    phase.check(
        "G.noop_prune_is_noop",
        all(e.get("saved_chars") == 0 and e.get("spilled_msgs") == 0 for e in noop) if noop else True,
        f"compression passes with pruned_msgs=0 must save/spill nothing: {phase.evidence['idempotency']['no_op_prune_events']}",
    )
    # 上限是曾观察到的不同超大工具结果数量：
    # 被裁剪的消息绝不会被计两次（它带有 marker，会被跳过）。
    distinct_big = set()
    for _name, s in (state.get("snapshots") or {}).items():
        for t in s.get("tool_msgs", []):
            if t["len"] > 8192 and not t["has_marker"]:
                distinct_big.add(t["tool_call_id"])
    total_pruned = sum(e.get("pruned_msgs", 0) for e in events)
    phase.evidence["idempotency"]["distinct_oversized_tool_results_observed"] = len(distinct_big)
    phase.check(
        "G.each_oversized_result_pruned_at_most_once",
        total_pruned <= len(distinct_big) and total_pruned >= 2,
        f"sum(pruned_msgs across events)={total_pruned} <= distinct oversized results={len(distinct_big)} "
        f"(and >=2 to prove the pruner really ran), no-op passes={len(noop)}",
    )

    # 5. 活动历史中保留的 marker 都能对应到真实存在的 spill 文件
    unresolved = []
    for t in snap_final["tool_msgs"]:
        if not t["has_marker"]:
            continue
        for p in t["marker_paths"]:
            if p not in inv_final["files"]:
                unresolved.append(p)
    phase.check(
        "G.markers_resolve",
        not unresolved,
        f"unresolved marker paths={unresolved} "
        f"active markers={[t['tool_call_id'] for t in snap_final['tool_msgs'] if t['has_marker']]}",
    )


def phase_h(rec, phase, state, gw):
    ws = state["ws"]
    snap_final = snapshot(ws)
    hist = read_history(ws)
    catalogs = [m for m in hist if kind_of(m) == "skill-catalog"]
    catalog_text = "\n".join((m.get("content") or "") for m in catalogs)
    phase.evidence["skill_catalog_messages"] = [
        {"form": (m.get("context") or {}).get("form"), "digest": (m.get("context") or {}).get("digest"),
         "len": len(m.get("content") or ""), "excerpt": (m.get("content") or "")[:200]} for m in catalogs
    ]
    phase.check(
        "H.skill_catalog_context_exists",
        len(catalogs) >= 1,
        f"kind=skill-catalog messages in active history={len(catalogs)}",
    )
    phase.check(
        "H.catalog_lists_skills",
        SKILL_NAME in catalog_text and SKILL_MARKER in catalog_text,
        f"catalog contains {SKILL_NAME}={SKILL_NAME in catalog_text} contains {SKILL_MARKER}={SKILL_MARKER in catalog_text}",
    )
    verify_text = ((state.get("verify") or {}).get("reply") or {}).get("text", "")
    phase.check(
        "H.catalog_model_visible",
        SKILL_NAME in (verify_text or "") or SKILL_NAME in catalog_text,
        f"model answer mentions catalog skill={SKILL_NAME in (verify_text or '')!r} answer={verify_text[:160]!r}",
    )
    log = gw.log_text()
    compression_seen = bool(state.get("compress_events")) or "Forced compression" in log
    phase.check(
        "H.catalog_durable_after_compression",
        compression_seen and len(catalogs) >= 1,
        f"compression seen={compression_seen} (events={len(state.get('compress_events') or [])}) "
        f"catalog still present={len(catalogs)>=1}",
    )
    # 上下文替换：恰好只有一份目录副本
    phase.check(
        "H.catalog_single_copy",
        len(catalogs) == 1,
        f"skill-catalog copies in active history={len(catalogs)}",
    )


async def phase_f(rec, phase, sess, ws, state, sid):
    ctx_text = (await sess.command(sid, "/context", expect="上下文用量"))["text"]
    stats = context_stats_from_text(ctx_text)
    snap_final = snapshot(ws)
    active_count = snap_final["line_count"]
    phase.evidence["context_output"] = ctx_text
    phase.evidence["parsed"] = stats
    phase.evidence["active_history_messages"] = active_count
    phase.check(
        "F.context_command_output",
        "上下文用量" in ctx_text and "压缩阈值" in ctx_text,
        f"/context reply len={len(ctx_text)}",
    )
    phase.check(
        "F.window_matches_config",
        stats.get("total_tokens") == MAIN_WINDOW,
        f"total_tokens={stats.get('total_tokens')} configured={MAIN_WINDOW}",
    )
    phase.check(
        "F.compress_threshold_formula",
        stats.get("compress_at_tokens") == MAIN_WINDOW - MAIN_MAX_TOKENS,
        f"compress_at={stats.get('compress_at_tokens')} expected={MAIN_WINDOW - MAIN_MAX_TOKENS}",
    )
    phase.check(
        "F.summarize_threshold_formula",
        stats.get("summarize_at_tokens") == MAIN_WINDOW * 10000 // 100,
        f"summarize_at={stats.get('summarize_at_tokens')} expected={MAIN_WINDOW * 10000 // 100}",
    )
    phase.check(
        "F.message_count_matches_jsonl",
        stats.get("message_count") == active_count,
        f"/context messages={stats.get('message_count')} active jsonl messages={active_count}",
    )
    if stats.get("used_tokens") and stats.get("compress_at_tokens"):
        expected_pct = stats["used_tokens"] * 100 // max(stats["compress_at_tokens"], 1)
        phase.check(
            "F.used_percent_consistent",
            abs(stats.get("compress_percent", -1) - expected_pct) <= 1,
            f"/context 压缩进度={stats.get('compress_percent')} expected≈{expected_pct} "
            f"used={stats.get('used_tokens')}/{stats.get('compress_at_tokens')}",
        )
        phase.check(
            "F.remaining_consistent",
            stats.get("remaining_tokens") == max(stats["compress_at_tokens"] - stats["used_tokens"], 0),
            f"remaining={stats.get('remaining_tokens')} expected={max(stats['compress_at_tokens'] - stats['used_tokens'], 0)}",
        )
    # usage 锚点：挑选最近最便宜的回合载荷（verify 回合）后对比
    anchors = []
    for step in rec.steps:
        if step.get("usage") and step.get("context_usage"):
            anchors.append(step)
    phase.evidence["usage_anchor_samples"] = [
        {"step": a["step"], "prompt_tokens": a["usage"].get("prompt_tokens"),
         "completion_tokens": a["usage"].get("completion_tokens"),
         "used_tokens": a["context_usage"].get("used_tokens"),
         "history_tokens": a["context_usage"].get("history_tokens"),
         "total_tokens": a["context_usage"].get("total_tokens")} for a in anchors[-6:]
    ]
    ratio_ok = False
    ratio_detail = "no sample"
    simple = [a for a in anchors if a["step"].startswith("A1.ping")] or anchors[-1:]
    if simple:
        a = simple[0]
        pt = (a["usage"] or {}).get("prompt_tokens") or 0
        est = (a["context_usage"] or {}).get("used_tokens") or 0
        if pt > 0 and est > 0:
            ratio = pt / est
            ratio_ok = 0.25 <= ratio <= 4
            ratio_detail = f"provider prompt_tokens={pt} estimate used_tokens={est} ratio={ratio:.2f}"
    phase.check("F.usage_anchor_same_order", ratio_ok, ratio_detail)

    # usage 锚点修复（D1）：把 context-pressure 提示的估算值
    # 与同一请求/回合的真实 prompt_tokens 做对比。
    hist = read_history(ws)
    pairs = pair_notices_with_steps(hist, rec.steps)
    within = [p for p in pairs if not p["before_first_assistant"]]
    first_call = [p for p in pairs if p["before_first_assistant"]]
    valid = [p for p in pairs if p["valid_pair"]]
    evidence = {
        "pairs": pairs,
        "within_turn_pairs_ratios": [p["ratio_provider_over_notice"] for p in within],
        "within_turn_median_abs_deviation": median_abs_dev(within),
        "first_call_pairs_ratios": [p["ratio_provider_over_notice"] for p in first_call],
        "first_call_median_abs_deviation": median_abs_dev(first_call),
        "valid_pair_ratios": [p["ratio_provider_over_notice"] for p in valid],
        "note": "notice_used = 压力通知里的『上下文使用约 N tokens』（锚点校正后）；"
                "provider_prompt_tokens = 同回合真实 usage。首迭代 notice 在 turnState 首次请求前生成，"
                "没有本轮此前的 provider 样本可用于校正（锚点按回合状态保存），因此首迭代只应与实测同量级；"
                "iteration>1 的 notice 才能用本轮上一响应锚点校正，是修复的主要可观测点。",
    }
    phase.evidence["usage_anchor"] = evidence
    if valid:
        phase.check(
            "F.anchor_notice_vs_provider_same_order",
            all(0.5 <= r <= 2.0 for r in evidence["valid_pair_ratios"]),
            f"provider/notice ratios={evidence['valid_pair_ratios']} (valid pairs only)",
        )
    else:
        phase.skip("F.anchor_notice_vs_provider_same_order", "no comparable notice/provider pairs")
    if first_call:
        phase.check(
            "F.anchor_first_call_same_order",
            all(0.5 <= r <= 2.0 for r in evidence["first_call_pairs_ratios"]),
            f"first-call provider/notice ratios={evidence['first_call_pairs_ratios']} "
            f"median|1-ratio|={evidence['first_call_median_abs_deviation']} "
            f"(first call has no in-turn prior provider sample; same-order expected)",
        )
    else:
        phase.skip("F.anchor_first_call_same_order", "no first-call notices paired with provider usage")

    # 前后对比：iteration>1 的提示是可观测的修复目标。把中位相对偏差
    # 与归档在 baseline-before-fix/ 下的修复前运行做比较。
    baseline_dir = state.get("baseline_dir")
    baseline_median = None
    baseline_pairs_count = 0
    if baseline_dir:
        b_res = os.path.join(baseline_dir, "results.json")
        b_sessions = os.path.join(baseline_dir, "sessions-main")
        try:
            b_doc = json.loads(read_text(b_res)) if os.path.exists(b_res) else {}
            b_steps = b_doc.get("steps") or []
            b_jsonl = None
            if os.path.isdir(b_sessions):
                files = [f for f in os.listdir(b_sessions) if f.endswith(".jsonl")]
                if files:
                    b_jsonl = os.path.join(b_sessions, sorted(files)[0])
            b_pairs = pair_notices_with_steps(read_history_file(b_jsonl), b_steps)
            b_within = [p for p in b_pairs if not p["before_first_assistant"]]
            baseline_median = median_abs_dev(b_within)
            baseline_pairs_count = len(b_within)
            evidence["baseline_within_turn_pairs_ratios"] = [
                p["ratio_provider_over_notice"] for p in b_within
            ]
            evidence["baseline_within_turn_median_abs_deviation"] = baseline_median
        except Exception as exc:
            evidence["baseline_error"] = f"{type(exc).__name__}: {exc}"
    if within:
        post_median = evidence["within_turn_median_abs_deviation"]
        if baseline_median is not None and baseline_pairs_count > 0:
            phase.check(
                "F.anchor_within_turn_improved",
                post_median <= baseline_median,
                f"iteration>1 notice median|1-ratio|: pre-fix={baseline_median} → post-fix={post_median} "
                f"(pre ratios={evidence['baseline_within_turn_pairs_ratios']}, "
                f"post ratios={evidence['within_turn_pairs_ratios']})",
            )
        else:
            phase.check(
                "F.anchor_within_turn_calibrated",
                post_median <= 0.25,
                f"iteration>1 notice median|1-ratio|={post_median} (target <=0.25; no baseline available)",
            )
    else:
        phase.skip("F.anchor_within_turn_improved", "no iteration>1 notices paired in this run")


async def phase_d(rec, phase, sess, gw, ws, state, sid):
    pre_snap = snapshot(ws)
    pre_spill = spill_inventory(ws)
    pre_ctx = (await sess.command(sid, "/context", expect="上下文用量"))["text"]
    pre_stats = context_stats_from_text(pre_ctx)
    pre_meta = read_meta(ws)
    hist = read_history(ws)

    # 往历史里放入一个近期唯一标记，使重启恢复可被证明
    await run_turn(rec, sess, sid, SEED_PROMPT, "D.seed", phase)
    seed_snap = snapshot(ws)
    phase.evidence["pre_restart"] = {
        "context": pre_ctx,
        "parsed": pre_stats,
        "meta": pre_meta,
        "spill_files": len(pre_spill["files"]),
        "line_count": seed_snap["line_count"],
        "kinds": seed_snap["kinds"],
    }
    seed_in_history = any(RESTART_SEED in (m.get("content") or "") for m in read_history(ws))
    phase.check("D.seed_in_history_before_restart", seed_in_history, f"seed {RESTART_SEED} in history={seed_in_history}")

    stop_info = gw.stop()
    phase.evidence["stop"] = stop_info
    phase.check(
        "D.sigterm_clean",
        stop_info.get("process_alive") is False and stop_info.get("pid_file_removed") is True,
        f"stop_info={stop_info}",
    )
    try:
        gw.start(state["binary"])
        restarted = True
    except Exception as exc:
        restarted = False
        phase.check("D.restart", False, f"restart failed: {exc}")
    if not restarted:
        return
    phase.check("D.restart", True, f"gateway restarted on {gw.host}:{gw.port}")
    await sess.close()
    await sess.connect(gw, MAIN_SID)
    ctx_after = (await sess.command(sid, "/context", expect="上下文用量"))["text"]
    stats_after = context_stats_from_text(ctx_after)
    post_snap = snapshot(ws)
    post_spill = spill_inventory(ws)
    post_meta = read_meta(ws)
    phase.evidence["post_restart"] = {
        "context": ctx_after,
        "parsed": stats_after,
        "meta": post_meta,
        "spill_files": len(post_spill["files"]),
        "line_count": post_snap["line_count"],
        "kinds": post_snap["kinds"],
    }
    phase.check(
        "D.history_restored",
        stats_after.get("message_count") == post_snap["line_count"] and post_snap["line_count"] > 0,
        f"/context messages={stats_after.get('message_count')} jsonl active={post_snap['line_count']}",
    )
    phase.check(
        "D.seed_survives_restart",
        any(RESTART_SEED in (m.get("content") or "") for m in read_history(ws)),
        f"seed present after restart={any(RESTART_SEED in (m.get('content') or '') for m in read_history(ws))}",
    )
    reply = await sess.send(sid, "只回复 AFTER_RESTART")
    rec.note_step("D.after_restart_ping", reply)
    phase.check(
        "D.answers_after_restart",
        reply["error"] is None and "AFTER_RESTART" in (reply["text"] or ""),
        f"reply={ (reply['text'] or '')[:120]!r} err={reply['error']}",
    )
    s3 = snapshot(ws)
    phase.check(
        "D.no_duplicate_injection",
        s3["workspace_count"] == 1 and s3["runtime_count"] == 1 and s3["skill_count"] == 1,
        f"kinds after restart+turn={s3['kinds']} (workspace/runtime/skill must be exactly 1)",
    )
    changed = {k: v for k, v in post_spill["files"].items() if k in pre_spill["files"] and v != pre_spill["files"][k]}
    phase.check(
        "D.spill_persisted",
        len(post_spill["files"]) == len(pre_spill["files"]) and not changed,
        f"spill files before={len(pre_spill['files'])} after={len(post_spill['files'])} changed={list(changed)[:3]}",
    )
    if pre_meta.get("task_state"):
        phase.check(
            "D.task_state_survives",
            bool(post_meta.get("task_state")) and stats_after.get("task_phase") not in (None, "无"),
            f"task_state meta before={pre_meta.get('task_state')} after={post_meta.get('task_state')} "
            f"/context task_phase={stats_after.get('task_phase')}",
        )
    else:
        phase.skip("D.task_state_survives", "no task_state persisted before restart")
    phase.check(
        "D.context_numbers_sane",
        0 < (stats_after.get("used_tokens") or 0) <= MAIN_WINDOW
        and stats_after.get("compress_at_tokens") == MAIN_WINDOW - MAIN_MAX_TOKENS,
        f"used={stats_after.get('used_tokens')} window={stats_after.get('total_tokens')} "
        f"compress_at={stats_after.get('compress_at_tokens')}",
    )


# ---------------------------------------------------------------------------
# 摘要分组（B）
# ---------------------------------------------------------------------------
async def summary_flow(rec, args, model_entry, phases):
    rec.drop_phase("B")
    phase = rec.phase("B", "async summarization (meta.summary / JSONL count)")
    ws = os.path.join(rec.out_root, "workspaces", "summary")
    prep = prepare_summary_workspace(ws, model_entry)
    phase.evidence["config"] = {
        "context_window": SUM_WINDOW,
        "max_tokens": SUM_MAX_TOKENS,
        "summarize_message_threshold": SUM_MSG_THRESHOLD,
        "summarize_token_percent": SUM_TOKEN_PERCENT,
    }
    gw = Gateway(ws, SUM_GW)
    sess = PicoSession()
    try:
        gw.start(args.binary)
        print(f"SUMMARY gateway ready on {gw.host}:{gw.port}")
        await sess.connect(gw, SUM_SID)
        prompts = [
            ("B.turn1", f"请记住唯一标记 BSUM_SEED_123，然后只回复 OK1"),
            ("B.turn2", "只回复 OK2"),
            ("B.turn3", "只回复 OK3"),
        ]
        for name, prompt in prompts:
            reply = await sess.send(SUM_SID, prompt)
            rec.note_step(name, reply)
            phase.check(name + ".turn_ok", reply["error"] is None, f"reply={(reply['text'] or '')[:80]!r} err={reply['error']}")

        snap_before = snapshot(ws)
        phase.evidence["before_summary"] = {"line_count": snap_before["line_count"], "kinds": snap_before["kinds"],
                                            "meta": snap_before["meta"]}
        # 等待真实的异步摘要器
        deadline = time.time() + 150
        summary = ""
        while time.time() < deadline:
            meta = read_meta(ws)
            if meta.get("summary"):
                summary = meta["summary"]
                break
            await asyncio.sleep(0.5)
        await asyncio.sleep(0.5)
        meta = read_meta(ws)
        summary = meta.get("summary") or ""
        snap_after = snapshot(ws)
        log = gw.log_text()
        events = parse_summarize_events(log)
        phase.evidence["summary"] = summary[:4000]
        phase.evidence["summary_len"] = len(summary)
        phase.evidence["summarize_events"] = events
        phase.evidence["after_summary"] = {"line_count": snap_after["line_count"], "kinds": snap_after["kinds"],
                                           "meta": {"count": meta.get("count"), "skip": meta.get("skip"),
                                                    "summary_len": len(meta.get("summary") or "")}}
        phase.check("B.summary_nonempty", bool(summary), f"meta.summary len={len(summary)}")
        phase.check(
            "B.memory_threshold_logged",
            "Memory threshold reached" in log,
            f"'Memory threshold reached' in gateway.log={'Memory threshold reached' in log}",
        )
        phase.check(
            "B.summarize_event_logged",
            len(events) >= 1,
            f"agent.session.summarize events={len(events)}",
        )
        if events:
            ev = events[-1]
            phase.check(
                "B.summarize_event_fields",
                ev["summarized_messages"] > 0 and ev["kept_messages"] > 0 and ev["summary_len"] > 0,
                f"summarized_messages={ev['summarized_messages']} kept_messages={ev['kept_messages']} summary_len={ev['summary_len']}",
            )
            phase.evidence["summarize_event"] = ev
        hist_lines = snap_after["line_count"]
        phase.check(
            "B.meta_count_consistent",
            meta.get("count") == hist_lines and (meta.get("skip") or 0) == 0,
            f"meta.count={meta.get('count')} jsonl lines={hist_lines} meta.skip={meta.get('skip')}",
        )
        phase.check(
            "B.history_truncated",
            0 < hist_lines < snap_before["line_count"],
            f"lines before={snap_before['line_count']} after={hist_lines} (truncation expected)",
        )
        phase.check(
            "B.summary_excludes_context_bodies",
            SKIP_CONTEXT_SENTINEL not in summary
            and WORKSPACE_SENTINEL not in summary
            and "<system-reminder>" not in summary
            and "<runtime_context>" not in summary,
            f"summary contains SKIP_CONTEXT_SENTINEL={SKIP_CONTEXT_SENTINEL in summary} "
            f"WORKSPACE_SENTINEL={WORKSPACE_SENTINEL in summary} "
            f"<system-reminder>={'<system-reminder>' in summary} <runtime_context>={'<runtime_context>' in summary}",
        )
        # 摘要进入 prompt 后对话仍能继续
        reply = await sess.send(SUM_SID, "只回复 OK4")
        rec.note_step("B.turn4", reply)
        phase.check(
            "B.conversation_continues_after_summary",
            reply["error"] is None and "OK4" in (reply["text"] or ""),
            f"reply={(reply['text'] or '')[:120]!r} err={reply['error']}",
        )
        ctx_text = (await sess.command(SUM_SID, "/context", expect="上下文用量"))["text"]
        stats = context_stats_from_text(ctx_text)
        phase.evidence["context_after_summary"] = {"text": ctx_text, "parsed": stats}
        phase.check(
            "B.summarize_threshold_formula",
            stats.get("summarize_at_tokens") == SUM_WINDOW * SUM_TOKEN_PERCENT // 100,
            f"/context summarize_at={stats.get('summarize_at_tokens')} expected={SUM_WINDOW * SUM_TOKEN_PERCENT // 100}",
        )
        phase.check(
            "B.compress_threshold_not_hit",
            stats.get("compress_at_tokens") == SUM_WINDOW - SUM_MAX_TOKENS,
            f"/context compress_at={stats.get('compress_at_tokens')} expected={SUM_WINDOW - SUM_MAX_TOKENS}",
        )
    except Exception:
        tb = traceback.format_exc()
        print(tb)
        phase.fail_all("summary flow exception: " + tb.splitlines()[-1])
    finally:
        await sess.close()
        stop_info = gw.stop()
        phase.evidence["stop"] = stop_info
        phase.check(
            "B.sigterm_clean",
            stop_info.get("process_alive") is False and stop_info.get("pid_file_removed") is True,
            f"stop_info={stop_info}",
        )
        copy_evidence_logs(rec, ws, "gateway-summary.log")
        redact_workspace_config(ws)
        rec.save()


# ---------------------------------------------------------------------------
# 证据辅助函数
# ---------------------------------------------------------------------------
def copy_evidence_logs(rec, ws, dest_name):
    src = os.path.join(ws, "gateway.log")
    if os.path.exists(src):
        os.makedirs(rec.out_root, exist_ok=True)
        try:
            shutil.copyfile(src, os.path.join(rec.out_root, dest_name))
        except Exception as exc:
            print(f"warn: copy log failed: {exc}")


def redact_workspace_config(ws):
    """保留工作区作为证据，但绝不把 api_key 落盘。"""
    cfg_path = os.path.join(ws, "config.json")
    if not os.path.exists(cfg_path):
        return
    try:
        cfg = json.loads(read_text(cfg_path))
        for m in cfg.get("model_list", []):
            if "api_key" in m:
                m["api_key"] = "<redacted>"
            if "api_keys" in m:
                m["api_keys"] = ["<redacted>"]
        with open(cfg_path, "w", encoding="utf-8") as f:
            json.dump(cfg, f, ensure_ascii=False, indent=2)
    except Exception as exc:
        print(f"warn: redact config failed: {exc}")


def archive_evidence_snapshots(rec, state):
    ws = state.get("ws")
    if not ws:
        return
    snap_dir = os.path.join(rec.out_root, "evidence")
    os.makedirs(snap_dir, exist_ok=True)
    for name, snap in (state.get("snapshots") or {}).items():
        slim = {
            "line_count": snap["line_count"],
            "kinds": snap["kinds"],
            "meta": snap["meta"],
            "msgs": snap["msgs"],
            "pressure": snap["pressure"],
            "first_msg": snap["first_msg"],
            "orphan_tool_results": snap["orphan_tool_results"],
            "max_tool_len": snap["max_tool_len"],
            "tool_msgs": [
                {
                    "tool_call_id": t["tool_call_id"],
                    "len": t["len"],
                    "plain_markers": t["plain_markers"],
                    "spill_markers": t["spill_markers"],
                    "marker_paths": t["marker_paths"],
                }
                for t in snap["tool_msgs"]
            ],
        }
        write_json(os.path.join(snap_dir, f"snapshot-{name.replace('/', '_')}.json"), slim)
    # 选定的 gateway 原始日志行
    log = read_text(os.path.join(ws, "gateway.log"))
    keys = [
        "Proactive compression",
        "Forced compression",
        "Trimmed rebuilt history",
        "Memory threshold reached",
        "agent.session.summarize",
        "agent.context.compress",
        "autonomy round scheduled",
        "Context window error",
        "Tool result spill failed",
        "context-pressure",
        "<context_pressure>",
    ]
    lines = [ln for ln in log.splitlines() if any(k in ln for k in keys)]
    with open(os.path.join(rec.out_root, "evidence", "gateway-key-lines.log"), "w", encoding="utf-8") as f:
        f.write("\n".join(lines) + "\n")


# ---------------------------------------------------------------------------
# 入口
# ---------------------------------------------------------------------------
def build_binary(dst):
    print("building chagent binary ...")
    subprocess.run(["go", "build", "-o", dst, "./cmd/chagent"], cwd=ROOT, check=True)
    print(f"binary: {dst}")


def parse_phases(raw):
    value = (raw or ALL_PHASES).upper().replace(",", "").replace(" ", "")
    bad = set(value) - set(ALL_PHASES)
    if bad:
        raise SystemExit(f"unknown phases: {''.join(sorted(bad))}")
    return value


async def amain(args):
    phases = parse_phases(args.phases)
    out_root = args.out if os.path.isabs(args.out) else os.path.join(ROOT, args.out)
    os.makedirs(out_root, exist_ok=True)
    if not args.baseline:
        candidate = os.path.join(out_root, "baseline-before-fix")
        args.baseline = candidate if os.path.isdir(candidate) else ""
    prev_doc = None
    if "A" not in phases:
        prev_path = os.path.join(out_root, "results.json")
        if os.path.exists(prev_path):
            try:
                prev_doc = json.loads(read_text(prev_path))
            except Exception:
                prev_doc = None
    if not args.model:
        args.model = (prev_doc or {}).get("model") or DEFAULT_MODEL
    model_entry = load_model_entry(args.model_config, args.model)
    rec = Recorder(out_root, phases, args.model, mask_model_config(model_entry))
    if prev_doc:
        rec.seed_from(prev_doc)
        print(f"merged previous results.json (phases kept: {''.join(rec.phases)})")
    rec.save()
    if "A" in phases:
        # 全新运行：丢弃上一次矩阵运行残留的旧证据
        shutil.rmtree(os.path.join(out_root, "evidence"), ignore_errors=True)
    print(f"task-6 context-window test: phases={phases} model={args.model} out={out_root}")
    if not args.no_build:
        build_binary(args.binary)
    elif not os.path.exists(args.binary):
        raise SystemExit(f"binary not found: {args.binary}")

    main_phases = set(phases) & set("ACDEFGH")
    try:
        if "B" in phases:
            await summary_flow(rec, args, model_entry, phases)
        if main_phases:
            await main_flow(rec, args, model_entry, phases)
            st = {"ws": os.path.join(out_root, "workspaces", "main")}
            # 状态快照由 main_flow 挂载；通过最终的 rec.save() 归档
        rec.save()
    finally:
        rec.save()
    doc = rec.save()
    print("\n===== RESULT =====")
    for key, ph in doc["phases"].items():
        failed = [a["id"] for a in ph["assertions"] if a["ok"] is False]
        print(f"{key} {ph['title']}: {ph['status']} failures={failed}")
    total_fail = sum(1 for ph in doc["phases"].values() for a in ph["assertions"] if a["ok"] is False)
    print(f"total failed assertions: {total_fail}")
    return 0 if total_fail == 0 else 1


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--phases", default=ALL_PHASES, help=f"phases to run (default {ALL_PHASES})")
    parser.add_argument("--model", default=None,
                        help=f"model_name from the external config (default {DEFAULT_MODEL}; "
                             "on a merged --phases rerun the previous run's model is reused)")
    parser.add_argument("--model-config", default=DEFAULT_MODEL_CONFIG, help="external config.json with model_list")
    parser.add_argument("--out", default=DEFAULT_OUT, help="output directory (results.json/summary.md/workspaces)")
    parser.add_argument("--binary", default=os.path.join(os.environ.get("TMPDIR", "/tmp"), "chagent-ctx-test"),
                        help="chagent binary path (built unless --no-build)")
    parser.add_argument("--no-build", action="store_true", help="reuse an existing binary")
    parser.add_argument("--baseline", default="",
                        help="pre-fix results dir for before/after comparison "
                             "(default: <out>/baseline-before-fix when present)")
    args = parser.parse_args()
    os.makedirs(os.path.join(os.path.dirname(args.binary), ""), exist_ok=True)

    def _sigterm(_signo, _frame):
        raise SystemExit(143)

    signal.signal(signal.SIGTERM, _sigterm)
    try:
        return asyncio.run(amain(args))
    except KeyboardInterrupt:
        return 130


if __name__ == "__main__":
    sys.exit(main())