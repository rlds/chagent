#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""独立产物验证器：从真实场景 E2E 的原始 artifacts 重新推导 PASS/FAIL。

设计原则
--------
1. **不信任 Lead harness 的 results.json**：本脚本从 session JSONL、meta.json、
   spill 目录、gateway 日志、config 哈希、workspace 文件与项目测试重新推导断言。
   results.json / summary.md 只用于“声明 vs 证据”的矛盾检测。
2. **独立实现**：不 import、不复制 Lead harness（scripts/agent-e2e-scenario.py）的
   任何逻辑。所有结构常量均从被验证产物中推导，或来自本脚本内的 schema 知识。
3. **每条断言带 evidence**：文件路径 + 行号 + 关键内容片段；失败退出码非 0。
4. **无副作用**：只读 artifacts；自测 fixture 与报告写入 --selftest-dir / --json-out。

用法
----
    python3 scripts/verify-agent-e2e-artifacts.py \
        --artifacts build/functest-20261030/live \
        --json-out build/functest-20261030/verify/verify-artifacts.json

    python3 scripts/verify-agent-e2e-artifacts.py --self-test
        # 内置合成 fixture（含 PASS 基线与 12 组“注入缺陷”变异自测），
        # 不需要真实 gateway / 模型 / 网络。

退出码
------
    0  全部 required 断言通过（advisory 警告不改变退出码）
    1  存在 required 断言失败
    2  CLI / 环境错误（artifacts 目录不存在等）

可选 expectations JSON（自动探测 <artifacts>/expectations.json、
<artifacts>/verify-expectations.json、<artifacts>/verify/expectations.json，
或用 --expect 显式指定）：见 DEFAULT_EXPECTATIONS 的注释。所有 marker 与
次数门槛都有安全默认值，不给 expectations 也能跑。
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import shutil
import stat
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

SCHEMA_VERSION = 1

# ---------------------------------------------------------------------------
# 与 chagent 持久化契约对齐的常量（来自代码阅读，非 harness 逻辑）
# ---------------------------------------------------------------------------

CTX_KINDS = [
    "workspace-instructions",
    "runtime-context",
    "task-state",
    "task-round",
    "skill-catalog",
    "loop-guard",
    "context-pressure",
]
# pre-step context source 顺序（pkg/agent/prestep.go builtinPreStepContextSources）：
# workspace -> runtime -> task-state -> context-pressure -> skill-catalog -> loop-guard。
# loop-guard / context-pressure 是事件型通知，可出现在回合任意位置；顺序断言只覆盖
# 状态型上下文（core four），pressure/loop-guard 仅做出现性检查。
CORE_ORDER = ["workspace-instructions", "runtime-context", "task-state", "skill-catalog"]
STATE_KINDS = {"runtime-context", "task-state", "skill-catalog"}
ALLOWED_CONTEXT_FORMS = {"", "reminder", "message", "state", "prompt", "catalog", "removed"}
ALLOWED_CHANGE_ACTIONS = {"set", "replace", "remove"}

TASKSTATE_PHASES = {"active", "paused", "blocked", "complete"}
TASKSTATE_TODO_STATUS = {"pending", "in_progress", "completed"}
TASKSTATE_BLOCKED_CODES = {"round-limit", "no-progress", "model-reported"}
TASKSTATE_REQUIRED_KEYS = ["version", "objective", "phase", "round", "max_rounds", "updated_at"]
TASKSTATE_OPTIONAL_KEYS = [
    "todos",
    "last_progress_at",
    "no_progress_rounds",
    "blocker_fingerprint",
    "blocker_rounds",
    "blocker_last_round",
    "blocked_reason",
    "evidence",
]

SPILL_PRUNE_PREFIX = "…[tool output middle pruned; full output: "
SPILL_PRUNE_CLOSE = "]…"
PLAIN_PRUNE_MARKER = "…[tool output middle pruned]…"
DEFAULT_SPILL_THRESHOLD = 8192  # DefaultToolResultPruneConfig().ThresholdChars
DEFAULT_PRUNE_HEAD = 4096
DEFAULT_PRUNE_TAIL = 1024

SECRET_KEY_RE = re.compile(r"(api[_-]?key|token|secret|password|passwd|credential)", re.I)
HEX64_RE = re.compile(r"\b[0-9a-fA-F]{64}\b")
HEX32_RE = re.compile(r"\b[0-9a-fA-F]{32}\b")
MARKER_PATTERNS = [
    re.compile(r"\b(?:e2e|live)[-_][A-Za-z0-9][A-Za-z0-9_-]{2,}\b", re.I),
    re.compile(r"\b[A-Z][A-Z0-9]*(?:[-_][A-Z0-9]{2,}){1,}\b"),
]
MARKER_STOPWORDS = {
    "AGENTS",
    "README",
    "SKILL",
    "SKILLS",
    "SYSTEM",
    "TODO",
    "FIXME",
    "NOTE",
    "HTTP",
    "HTTPS",
    "JSON",
    "UTF-8",
    "UTF8",
    "AS-IS",
    "E2E",
}

DEFAULT_EXPECTATIONS = {
    # 场景 marker；None 表示从产物自动探测（AGENTS.md / SKILL.md / report.md）
    "markers": {"agents_md": None, "skill": None, "report": None},
    # 日志关键字最低次数
    "min_counts": {
        "tool_call_logs": 1,
        "tool_call_batch_logs": 0,
        "autonomy_round_scheduled": 1,
        "forced_compression_pruned": 0,
        "spill_events": 1,
    },
    "require": {
        "summary": True,           # meta.summary 必须非空
        "task_state": "auto",      # auto: 有 task-round/todo_write/task_update 证据则必须存在
        "todos": "auto",
        "evidence": "auto",
        "config_hash": "auto",     # auto: before/after 都存在才启用
        "restart": "auto",         # auto: gateway-restart.log 存在才启用
        "project_tests": "auto",
        "spill": "auto",
        "report_marker": "auto",
        "agents_delta_history": True,
        "skill_marker_in_tool_result": True,
        "loop_guard": "auto",      # auto: results.json 提到 loop-guard / 会话已出现则要求存在
    },
    "project_test_cmd": None,      # list[str] 覆盖自动探测
    "project_buggy_patterns": [],  # 正则列表；命中即说明 bug 未修
    "project_extra_checks": [],    # [{cmd:[...], expect_contains:"", cwd:""}]
    # 已由 Lead/其他 teammate 独立确认的生产缺陷对应的断言 id：
    # 这些断言在原始证据中确实应失败，标记为 expected-known-failure，
    # 不计入“矛盾清单”，也不影响 overall（但会单列）。
    "known_failures": [],
    "allow_incomplete_tail": False,
    "timeout_sec": 180,
    "hash_binds_redacted_config": False,  # True: before/after 摘要必须等于 config.redacted.json 的 sha256
}


# ---------------------------------------------------------------------------
# 基础工具
# ---------------------------------------------------------------------------


def now_iso() -> str:
    return datetime.now(timezone.utc).astimezone().isoformat(timespec="seconds")


def clip(text, limit: int = 200) -> str:
    if text is None:
        return ""
    text = str(text).replace("\n", "\\n").replace("\r", "")
    if len(text) <= limit:
        return text
    return text[: limit - 1] + "…"


def norm_ws(text) -> str:
    return " ".join(str(text or "").split())


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def read_bytes(path: Path):
    try:
        return path.read_bytes()
    except OSError:
        return None


def read_text(path: Path):
    data = read_bytes(path)
    if data is None:
        return None
    return data.decode("utf-8", errors="replace")


def deep_merge(base, override):
    out = dict(base)
    for key, val in (override or {}).items():
        if isinstance(val, dict) and isinstance(out.get(key), dict):
            out[key] = deep_merge(out[key], val)
        else:
            out[key] = val
    return out


def load_expectations(artifacts: Path, explicit: str | None) -> tuple[dict, str | None]:
    candidates = []
    if explicit:
        candidates.append(Path(explicit))
    else:
        candidates.extend(
            [
                artifacts / "expectations.json",
                artifacts / "verify-expectations.json",
                artifacts / "verify" / "expectations.json",
            ]
        )
    for cand in candidates:
        if cand.is_file():
            try:
                raw = json.loads(cand.read_text(encoding="utf-8"))
            except (OSError, ValueError) as exc:
                raise SystemExit(f"[fatal] 无法解析 expectations: {cand}: {exc}")
            if not isinstance(raw, dict):
                raise SystemExit(f"[fatal] expectations 必须是 JSON object: {cand}")
            return deep_merge(DEFAULT_EXPECTATIONS, raw), str(cand)
    return dict(DEFAULT_EXPECTATIONS), None


def mode_perm(path: Path) -> int | None:
    try:
        return stat.S_IMODE(path.stat().st_mode)
    except OSError:
        return None


def process_alive(pid: int) -> tuple[bool, str]:
    """返回 (是否存活, 命令行)。仅用于“无残留进程”证据。"""
    if pid <= 0 or pid == os.getpid():
        return False, ""
    try:
        os.kill(pid, 0)
        alive = True
    except ProcessLookupError:
        return False, ""
    except PermissionError:
        alive = True
    except OSError:
        return False, ""
    cmd = ""
    try:
        proc = subprocess.run(
            ["ps", "-o", "command=", "-p", str(pid)],
            capture_output=True,
            text=True,
            timeout=10,
        )
        cmd = (proc.stdout or "").strip()
    except (OSError, subprocess.SubprocessError):
        pass
    if cmd and pid == 1:
        alive = False
    return alive, cmd


# ---------------------------------------------------------------------------
# 断言收集器
# ---------------------------------------------------------------------------

PASS, FAIL, WARN, SKIP = "PASS", "FAIL", "WARN", "SKIP"


class Checker:
    def __init__(self, artifacts: Path, known_failures=None):
        self.artifacts = artifacts
        self.checks: list[dict] = []
        self.known_failures = set(known_failures or [])

    def rel(self, path) -> str:
        try:
            return os.path.relpath(str(path), str(self.artifacts))
        except ValueError:
            return str(path)

    def ev(self, path, line=None, snippet=None) -> str:
        base = self.rel(path)
        if line:
            base = f"{base}:{line}"
        if snippet is not None:
            base = f"{base}: {clip(snippet)}"
        return base

    def _add(self, cid, title, group, severity, status, detail="", evidence=None):
        self.checks.append(
            {
                "id": cid,
                "title": title,
                "group": group,
                "severity": severity,
                "status": status,
                "detail": detail,
                "evidence": list(evidence or []),
                "expected_known_failure": cid in self.known_failures,
            }
        )

    def ok(self, cid, title, group="", detail="", evidence=None, severity="required"):
        self._add(cid, title, group, severity, PASS, detail, evidence)

    def fail(self, cid, title, group="", detail="", evidence=None, severity="required"):
        self._add(cid, title, group, severity, FAIL, detail, evidence)

    def warn(self, cid, title, group="", detail="", evidence=None):
        self._add(cid, title, group, "advisory", WARN, detail, evidence)

    def skip(self, cid, title, group="", detail="", evidence=None):
        self._add(cid, title, group, "advisory", SKIP, detail, evidence)

    def counts(self):
        out = {PASS: 0, FAIL: 0, WARN: 0, SKIP: 0}
        for check in self.checks:
            out[check["status"]] = out.get(check["status"], 0) + 1
        return out

    def required_failures(self):
        return [
            c
            for c in self.checks
            if c["severity"] == "required" and c["status"] == FAIL and not c.get("expected_known_failure")
        ]

    def known_failure_checks(self):
        return [c for c in self.checks if c.get("expected_known_failure")]

    def advisory_warnings(self):
        return [c for c in self.checks if c["severity"] == "advisory" and c["status"] == WARN]


# ---------------------------------------------------------------------------
# Session JSONL 分析
# ---------------------------------------------------------------------------


def parse_jsonl(path: Path) -> tuple[list[tuple[int, object | None, str]], list[dict]]:
    """返回 [(1-based 行号, 解析后的对象或 None, 原始行)] 与解析问题列表。"""
    text = read_text(path)
    if text is None:
        return [], [{"line": 0, "error": "无法读取文件"}]
    records: list[tuple[int, object | None, str]] = []
    issues: list[dict] = []
    for lineno, raw in enumerate(text.splitlines(), 1):
        if not raw.strip():
            continue
        try:
            obj = json.loads(raw)
        except ValueError as exc:
            obj = None
            issues.append({"line": lineno, "error": str(exc), "snippet": clip(raw, 160)})
        if obj is not None and not isinstance(obj, dict):
            issues.append({"line": lineno, "error": "JSON 不是 object", "snippet": clip(raw, 160)})
            obj = None
        records.append((lineno, obj, raw))
    return records, issues


def msg_context(obj) -> dict:
    if isinstance(obj, dict):
        ctx = obj.get("context")
        if isinstance(ctx, dict):
            return ctx
    return {}


def msg_kind(obj) -> str:
    kind = msg_context(obj).get("kind")
    return kind if isinstance(kind, str) else ""


def tool_calls_of(obj) -> list[dict]:
    out = []
    if not isinstance(obj, dict):
        return out
    calls = obj.get("tool_calls")
    if not isinstance(calls, list):
        return out
    for call in calls:
        if not isinstance(call, dict):
            continue
        fn = call.get("function") if isinstance(call.get("function"), dict) else {}
        raw_args = fn.get("arguments")
        parsed = None
        if isinstance(raw_args, str):
            try:
                parsed = json.loads(raw_args)
            except ValueError:
                parsed = None
        out.append(
            {
                "id": call.get("id") if isinstance(call.get("id"), str) else "",
                "type": call.get("type") or "",
                "name": fn.get("name") if isinstance(fn.get("name"), str) else "",
                "args_raw": raw_args if isinstance(raw_args, str) else "",
                "args": parsed if isinstance(parsed, dict) else {},
            }
        )
    return out


class SessionAnalysis:
    def __init__(self, path: Path, artifacts: Path):
        self.path = path
        self.rel = os.path.relpath(str(path), str(artifacts))
        self.records, self.parse_issues = parse_jsonl(path)
        self.active: list[tuple[int, dict]] = []
        self.meta = None
        self.meta_path = None
        self.meta_error = None
        self.raw_nonempty = len(self.records)
        self.kinds: dict[str, dict] = {}
        self.unknown_kinds: list[dict] = []
        self.tool_calls: list[dict] = []
        self.tool_results: list[dict] = []
        self.messages: list[dict] = []
        self.segments: list[dict] = []
        self.supersede: dict[str, dict] = {}
        self.prune_refs: list[dict] = []
        self.plain_prune_count = 0
        self.texts: list[tuple[int, str, str, str]] = []  # (line, role, kind, content)

    # -- 装载 ---------------------------------------------------------------
    def load_meta(self):
        stem = self.path.name
        for suffix in (".jsonl",):
            if stem.endswith(suffix):
                stem = stem[: -len(suffix)]
                break
        meta_path = self.path.with_name(stem + ".meta.json")
        self.meta_path = meta_path
        if not meta_path.is_file():
            self.meta_error = "meta.json 不存在"
            return
        try:
            self.meta = json.loads(meta_path.read_text(encoding="utf-8"))
        except (OSError, ValueError) as exc:
            self.meta_error = f"meta.json 解析失败: {exc}"
            self.meta = None

    def build(self):
        self.load_meta()
        skip = 0
        if isinstance(self.meta, dict):
            try:
                skip = int(self.meta.get("skip") or 0)
            except (TypeError, ValueError):
                skip = 0
        if skip < 0:
            skip = 0
        for lineno, obj, _raw in self.records:
            if not isinstance(obj, dict):
                continue
            self.active.append((lineno, obj))
            role = obj.get("role") if isinstance(obj.get("role"), str) else ""
            content = obj.get("content") if isinstance(obj.get("content"), str) else ""
            kind = msg_kind(obj)
            self.messages.append(
                {"line": lineno, "role": role, "kind": kind, "content": content, "obj": obj}
            )
            if kind:
                entry = self.kinds.setdefault(
                    kind, {"count": 0, "first": lineno, "last": lineno, "forms": {}, "contents": set()}
                )
                entry["count"] += 1
                entry["last"] = lineno
                form = msg_context(obj).get("form") or ""
                entry["forms"][form] = entry["forms"].get(form, 0) + 1
                entry["contents"].add(content)
            if role == "assistant":
                for call in tool_calls_of(obj):
                    call["line"] = lineno
                    self.tool_calls.append(call)
            elif role == "tool":
                self.tool_results.append(
                    {
                        "line": lineno,
                        "id": obj.get("tool_call_id") if isinstance(obj.get("tool_call_id"), str) else "",
                        "content": content,
                        "obj": obj,
                    }
                )
            self.texts.append((lineno, role, kind, content))
        # active 区域应为跳过 skip 行之后的部分
        self.active = self.active[skip:] if skip < len(self.active) else []
        self._collect_context_extras()
        self._collect_turns()

    def _collect_context_extras(self):
        known = set(CTX_KINDS)
        for kind, info in self.kinds.items():
            if kind not in known:
                self.unknown_kinds.append(
                    {"kind": kind, "count": info["count"], "first": info["first"]}
                )
        # prune marker 引用（spill 与普通 prune）
        for idx, (lineno, obj) in enumerate(self.active):
            if not isinstance(obj, dict) or obj.get("role") != "tool":
                continue
            content = obj.get("content") if isinstance(obj.get("content"), str) else ""
            if not content:
                continue
            if PLAIN_PRUNE_MARKER in content:
                self.plain_prune_count += 1
            pos = content.find(SPILL_PRUNE_PREFIX)
            if pos < 0:
                continue
            close = content.find(SPILL_PRUNE_CLOSE, pos + len(SPILL_PRUNE_PREFIX))
            target = content[pos + len(SPILL_PRUNE_PREFIX): close] if close >= 0 else ""
            head = content[:pos]
            tail = content[close + len(SPILL_PRUNE_CLOSE):] if close >= 0 else ""
            self.prune_refs.append(
                {
                    "line": lineno,
                    "path": target,
                    "head": head,
                    "tail": tail,
                    "tool_call_id": obj.get("tool_call_id") or "",
                }
            )

    def _collect_turns(self):
        """按“真实用户消息”切分回合段，检查 pre-step context 首次出现顺序。"""
        segment = None
        for lineno, obj in self.active:
            if not isinstance(obj, dict):
                continue
            role = obj.get("role")
            kind = msg_kind(obj)
            if role == "user" and not kind:
                if segment and segment["contexts"]:
                    self.segments.append(segment)
                segment = {"start_line": lineno, "contexts": []}
                continue
            if kind:
                if segment is None:
                    segment = {"start_line": lineno, "contexts": []}
                segment["contexts"].append({"line": lineno, "kind": kind})
        if segment and segment["contexts"]:
            self.segments.append(segment)

    # -- 派生统计 -----------------------------------------------------------
    def kind_histogram(self):
        out = {}
        for kind in CTX_KINDS:
            info = self.kinds.get(kind)
            out[kind] = {
                "count": info["count"] if info else 0,
                "first_line": info["first"] if info else None,
                "last_line": info["last"] if info else None,
                "forms": dict(info["forms"]) if info else {},
                "distinct_contents": len(info["contents"]) if info else 0,
            }
        for kind, info in sorted(self.kinds.items()):
            if kind not in out:
                out[kind] = {
                    "count": info["count"],
                    "first_line": info["first"],
                    "last_line": info["last"],
                    "forms": dict(info["forms"]),
                    "distinct_contents": len(info["contents"]),
                }
        return out

    def pairing(self):
        call_ids: dict[str, dict] = {}
        duplicate_calls = []
        for call in self.tool_calls:
            cid = call["id"]
            if not cid:
                continue
            if cid in call_ids:
                duplicate_calls.append(call)
            else:
                call_ids[cid] = call
        result_ids: dict[str, dict] = {}
        duplicate_results = []
        for res in self.tool_results:
            tid = res["id"]
            if not tid:
                continue
            if tid in result_ids:
                duplicate_results.append(res)
            else:
                result_ids[tid] = res
        unmatched_calls = [c for cid, c in call_ids.items() if cid not in result_ids]
        orphan_results = [r for tid, r in result_ids.items() if tid not in call_ids]
        return {
            "calls": call_ids,
            "results": result_ids,
            "unmatched_calls": unmatched_calls,
            "orphan_results": orphan_results,
            "duplicate_call_ids": duplicate_calls,
            "duplicate_results": duplicate_results,
        }

    def supersede_analysis(self):
        """状态型上下文的 supersede/dedup 语义分析。

        代码契约（prestep.go / workspace_context_runtime.go / skill_catalog_context.go）：
        - runtime-context 按 context.digest 去重（仅比较最近一条）；
        - task-state 按渲染文本去重（仅比较最近一条），round/timestamp 变化不重注；
        - skill-catalog 按目录 digest 去重；
        - 模型可见历史只保留每种状态型上下文的最新副本，旧副本保留在 JSONL 供审计。
        因此 JSONL 中“相邻同类状态上下文”的去重字段必须不同，否则说明注入器没有
        执行替换/去重语义（若发生逻辑截断 skip>0，降级为 advisory）。
        """
        out = {}
        for kind in ("runtime-context", "task-state", "skill-catalog"):
            seq = [
                (lineno, obj)
                for lineno, obj in self.active
                if isinstance(obj, dict) and msg_kind(obj) == kind
            ]
            duplicates = []
            keys = []
            for idx, (lineno, obj) in enumerate(seq):
                ctx = msg_context(obj)
                content = obj.get("content") if isinstance(obj.get("content"), str) else ""
                if kind == "task-state":
                    key = task_state_render_signature(content)
                else:
                    key = ctx.get("digest") or ""
                    if not key:
                        key = "content:" + sha256_bytes(content.encode("utf-8"))
                keys.append(key)
                if idx > 0 and keys[idx - 1] == key:
                    duplicates.append(
                        {
                            "prev_line": seq[idx - 1][0],
                            "line": lineno,
                            "kind": kind,
                            "digest": clip(key, 80),
                        }
                    )
            out[kind] = {
                "copies": len(seq),
                "distinct_keys": len(set(keys)),
                "duplicate_adjacent": duplicates,
                "last_line": seq[-1][0] if seq else None,
            }
        return out


# ---------------------------------------------------------------------------
# task-state 渲染块解析（P-09 格式）
# ---------------------------------------------------------------------------


def parse_quoted(text: str) -> str:
    text = text.strip()
    if len(text) >= 2 and text[0] == '"' and text[-1] == '"':
        try:
            return json.loads(text)
        except ValueError:
            return text[1:-1]
    return text


def parse_task_state_render(content: str) -> dict | None:
    if not content or "<task_state>" not in content:
        return None
    out = {"todos": [], "evidence": [], "blocked": None}
    mode = None
    for raw_line in content.splitlines():
        line = raw_line.rstrip()
        if line.startswith("<task_state>"):
            continue
        if line.startswith("目标："):
            out["objective"] = parse_quoted(line[len("目标："):])
            mode = None
        elif line.startswith("阶段："):
            out["phase"] = line[len("阶段："):].strip()
            mode = None
        elif line.startswith("轮次："):
            m = re.match(r"(\d+)\s*/\s*(\d+)", line[len("轮次："):].strip())
            if m:
                out["round"], out["max_rounds"] = int(m.group(1)), int(m.group(2))
            mode = None
        elif line.startswith("待办："):
            body = line[len("待办："):].strip()
            mode = "todos" if body == "" else None
        elif line.startswith("最近检查点："):
            mode = None
        elif line.startswith("证据："):
            body = line[len("证据："):].strip()
            mode = "evidence" if body == "" else None
        elif line.startswith("阻塞原因："):
            body = line[len("阻塞原因："):].strip()
            out["blocked"] = None if body in ("无", "") else body
            mode = None
        elif line.startswith("</task_state>"):
            mode = None
        elif mode == "todos" and line.startswith("- ["):
            m = re.match(r"- \[([^\]]+)\]\s*(.*)$", line)
            if m:
                out["todos"].append({"status": m.group(1), "content": m.group(2)})
        elif mode == "evidence" and line.startswith("- "):
            out["evidence"].append(line[2:])
    return out


def task_state_render_signature(content: str) -> str:
    """按 TaskStateDigest 的字段集合生成去重签名（round/timestamps 不参与）。"""
    parsed = parse_task_state_render(content)
    if parsed is None:
        return "raw:" + sha256_bytes(content.encode("utf-8"))
    payload = {
        "objective": norm_ws(parsed.get("objective", "")),
        "phase": parsed.get("phase", ""),
        "max_rounds": parsed.get("max_rounds"),
        "todos": [[t["status"], norm_ws(t["content"])] for t in parsed["todos"]],
        "evidence": [norm_ws(e) for e in parsed["evidence"]],
        "blocked": parsed.get("blocked"),
    }
    return "taskstate:" + sha256_bytes(json.dumps(payload, ensure_ascii=False, sort_keys=True).encode("utf-8"))


# ---------------------------------------------------------------------------
# 日志解析
# ---------------------------------------------------------------------------

LEVEL_MAP = {
    "TRC": "trace",
    "DBG": "debug",
    "DEB": "debug",
    "INF": "info",
    "WRN": "warn",
    "ERR": "error",
    "FTL": "fatal",
    "PNC": "panic",
}
LEVEL_RE = re.compile(r"(?<![A-Za-z])(TRC|DBG|DEB|INF|WRN|ERR|FTL|PNC)(?![A-Za-z])")
CONSOLE_FIELD_RE = re.compile(r"([A-Za-z_][A-Za-z0-9_]*)=(?:\"([^\"]*)\"|(\S+))")


def parse_log_file(path: Path) -> list[dict]:
    text = read_text(path)
    if text is None:
        return []
    events = []
    for lineno, raw in enumerate(text.splitlines(), 1):
        if not raw.strip():
            continue
        event = {
            "file": os.path.basename(str(path)),
            "line": lineno,
            "raw": raw,
            "level": "",
            "component": "",
            "message": "",
            "fields": {},
        }
        stripped = raw.strip()
        obj = None
        if stripped.startswith("{"):
            try:
                obj = json.loads(stripped)
            except ValueError:
                obj = None
        if isinstance(obj, dict):
            event["message"] = str(obj.get("message") or "")
            event["component"] = str(obj.get("component") or "")
            level = str(obj.get("level") or "").lower()
            event["level"] = level
            fields = {}
            for key, val in obj.items():
                if key in ("level", "message", "time", "component", "caller"):
                    continue
                if isinstance(val, (str, int, float, bool)) or val is None:
                    fields[key] = str(val)
            event["fields"] = fields
            # JSON 行文本用于关键字/字段搜索
            event["text"] = event["message"] + " " + " ".join(
                f"{k} {v}" for k, v in fields.items()
            )
        else:
            m = LEVEL_RE.search(stripped)
            if m:
                event["level"] = LEVEL_MAP.get(m.group(1), "")
            event["text"] = stripped
            fields = {}
            for fm in CONSOLE_FIELD_RE.finditer(stripped):
                fields[fm.group(1)] = fm.group(2) if fm.group(2) is not None else fm.group(3)
            event["fields"] = fields
            event["message"] = stripped
        events.append(event)
    return events


def log_events_with(events: list[dict], needle: str) -> list[dict]:
    needle = needle.lower()
    return [e for e in events if needle in (e.get("text") or "").lower()]


def log_count(events: list[dict], needle: str) -> int:
    return len(log_events_with(events, needle))


def log_int_field(events: list[dict], field: str) -> list[int]:
    out = []
    for event in events:
        values = event.get("fields") or {}
        if field in values:
            try:
                out.append(int(values[field]))
            except (TypeError, ValueError):
                pass
    return out


def log_first(events: list[dict], needle: str) -> dict | None:
    found = log_events_with(events, needle)
    return found[0] if found else None


# ---------------------------------------------------------------------------
# 文件 / marker 探测
# ---------------------------------------------------------------------------


def detect_markers(text: str, exclude: set[str] | None = None) -> list[str]:
    if not text:
        return []
    exclude = set(exclude or set())
    found = []
    for pattern in MARKER_PATTERNS:
        for match in pattern.finditer(text):
            token = match.group(0)
            if token in exclude or token.upper() in MARKER_STOPWORDS:
                continue
            if len(token) < 6:
                continue
            if token not in found:
                found.append(token)
    # e2e/live 前缀优先
    found.sort(key=lambda t: (0 if re.match(r"(?i)(e2e|live)[-_]", t) else 1, -len(t)))
    return found


def extract_hex_digest(text: str) -> list[str]:
    digests = [m.group(0).lower() for m in HEX64_RE.finditer(text or "")]
    if not digests:
        digests = [m.group(0).lower() for m in HEX32_RE.finditer(text or "")]
    return digests


def iter_project_python_files(project_dir: Path):
    for path in sorted(project_dir.rglob("*.py")):
        if any(part in (".git", "__pycache__", ".venv", "venv", "node_modules") for part in path.parts):
            continue
        yield path


# ---------------------------------------------------------------------------
# 验证主体
# ---------------------------------------------------------------------------


class VerifyContext:
    def __init__(self, artifacts: Path, expectations: dict, timeout_sec: int):
        self.artifacts = artifacts
        self.exp = expectations
        self.timeout = timeout_sec
        self.checker = Checker(artifacts, expectations.get("known_failures") or [])
        self.original_workspace = None
        self.results_text = ""
        self.harness_text = ""
        self.sessions: list[SessionAnalysis] = []
        self.primary: SessionAnalysis | None = None
        self.logs: dict[str, list[dict]] = {}
        self.corpus: list[dict] = []
        self.markers: dict = {}
        self.project: dict = {}
        self.config: dict = {}
        self.lead_claims: dict = {}
        self.contradictions: list[dict] = []
        self.summary: dict = {}

    # -- 小工具 -------------------------------------------------------------
    def c(self) -> Checker:
        return self.checker

    def add_session(self, path: Path):
        analysis = SessionAnalysis(path, self.artifacts)
        analysis.build()
        self.sessions.append(analysis)
        return analysis

    def rel(self, path) -> str:
        return self.checker.rel(path)

    def workspace(self) -> Path:
        return self.artifacts / "workspace"


# ---------------------------------------------------------------------------
# 1) 布局
# ---------------------------------------------------------------------------


def check_layout(ctx: VerifyContext):
    c = ctx.c()
    group = "layout"
    if not ctx.artifacts.is_dir():
        c.fail("layout.artifacts_dir", "artifacts 目录存在", group, f"缺失: {ctx.artifacts}")
        return
    c.ok("layout.artifacts_dir", "artifacts 目录存在", group, str(ctx.artifacts))

    required = {
        "gateway.log": ctx.artifacts / "gateway.log",
        "harness.log": ctx.artifacts / "harness.log",
        "results.json": ctx.artifacts / "results.json",
        "summary.md": ctx.artifacts / "summary.md",
        "workspace/": ctx.artifacts / "workspace",
        "workspace/sessions/": ctx.artifacts / "workspace" / "sessions",
    }
    for name, path in required.items():
        if not path.exists():
            c.fail(f"layout.required.{name}", f"必需产物 {name} 存在", group, f"缺失: {c.rel(path)}")
        elif path.is_file() and path.stat().st_size == 0:
            c.fail(f"layout.required.{name}", f"必需产物 {name} 非空", group, f"空文件: {c.rel(path)}")
        elif path.is_dir() and not any(path.iterdir()):
            c.fail(f"layout.required.{name}", f"必需产物 {name} 非空", group, f"空目录: {c.rel(path)}")
        else:
            c.ok(f"layout.required.{name}", f"必需产物 {name} 存在且非空", group, c.rel(path))

    optional = {
        "gateway-restart.log": ctx.artifacts / "gateway-restart.log",
        "config.redacted.json": ctx.artifacts / "config.redacted.json",
        "config.hash.before": ctx.artifacts / "config.hash.before",
        "config.hash.after": ctx.artifacts / "config.hash.after",
        "workspace/AGENTS.md": ctx.artifacts / "workspace" / "AGENTS.md",
        "workspace/.chagent/spill": ctx.artifacts / "workspace" / ".chagent" / "spill",
        "evidence/context-p6.json": ctx.artifacts / "evidence" / "context-p6.json",
        "evidence/context-kinds-observed.json": ctx.artifacts / "evidence" / "context-kinds-observed.json",
    }
    for name, path in optional.items():
        if path.exists():
            c.ok(f"layout.optional.{name}", f"可选产物 {name} 存在", group, c.rel(path), severity="advisory")
        elif name.startswith("evidence/"):
            c.skip(f"layout.optional.{name}", f"可选产物 {name} 存在", group, f"未提供: {c.rel(path)}")
        else:
            c.warn(f"layout.optional.{name}", f"可选产物 {name} 存在", group, f"缺失: {c.rel(path)}")


# ---------------------------------------------------------------------------
# 2) Session JSONL
# ---------------------------------------------------------------------------


def _find_first(node, keys):
    """递归查找第一个匹配 key 的值（兼容 harness 快照 schema 变化）。"""
    if isinstance(node, dict):
        for key in keys:
            if key in node:
                return node[key]
        for value in node.values():
            found = _find_first(value, keys)
            if found is not None:
                return found
    elif isinstance(node, list):
        for value in node:
            found = _find_first(value, keys)
            if found is not None:
                return found
    return None


def observed_context_kinds(ctx: VerifyContext) -> dict:
    """读取 harness 记录的运行期 context kind 并集。

    async summarize 可能在运行结束时合法重写 session JSONL，导致最终文件缺少早期
    阶段注入的 kind；harness 的 evidence/context-kinds-observed.json 记录运行期
    观察到的 kind 并集，可作为“运行时确实出现”的证据。
    """
    c = ctx.c()
    out = {"path": None, "kinds": {}, "evidence": [], "data": None}
    path = ctx.artifacts / "evidence" / "context-kinds-observed.json"
    if not path.is_file():
        return out
    out["path"] = c.rel(path)
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except ValueError as exc:
        out["evidence"].append(f"{c.rel(path)}: 解析失败: {exc}")
        return out
    out["data"] = data
    kinds_value = _find_first(data, ("observed_kinds", "observedKinds", "kinds", "context_kinds"))
    if isinstance(kinds_value, dict):
        for kind, value in kinds_value.items():
            try:
                count = int(value)
            except (TypeError, ValueError):
                count = 1 if value else 0
            if count > 0:
                out["kinds"][str(kind)] = count
    elif isinstance(kinds_value, list):
        for kind in kinds_value:
            if isinstance(kind, str):
                out["kinds"][kind] = out["kinds"].get(kind, 0) + 1
    if out["kinds"]:
        out["evidence"].append(
            f"{c.rel(path)}: 运行期观察 kinds="
            + ", ".join(f"{k}×{v}" for k, v in sorted(out["kinds"].items()))
        )
    return out


def loop_guard_evidence(ctx: VerifyContext) -> dict:
    """loop-guard 上下文证据：最终 JSONL 或 harness 在 P6 时刻保留的快照。

    背景：P6 注入的 loop-guard context 之后会被 P7/P9 的 summarize/压缩合法重写，
    最终 JSONL 不再保留它。因此“运行时确实注入过”的原始证据也可以是 P6 快照
    （evidence/context-p6.json 与 evidence/sessions-at-p6-*.jsonl）。
    """
    c = ctx.c()
    out = {
        "final_jsonl": [],
        "p6_snapshot": None,
        "p6_session_jsonl": [],
        "supported": False,
        "evidence": [],
    }
    for analysis in ctx.sessions:
        for lineno, role, kind, content in analysis.texts:
            if kind == "loop-guard":
                out["final_jsonl"].append(f"{analysis.rel}:{lineno}")
    evidence_dir = ctx.artifacts / "evidence"
    snapshot_path = evidence_dir / "context-p6.json"
    if snapshot_path.is_file():
        try:
            data = json.loads(snapshot_path.read_text(encoding="utf-8"))
        except ValueError as exc:
            out["p6_snapshot"] = {"path": c.rel(snapshot_path), "error": f"解析失败: {exc}"}
        else:
            messages = _find_first(data, ("loop_guard_messages", "loopGuardMessages", "loop_guard_msgs"))
            kinds = _find_first(data, ("kinds", "context_kinds", "contextKinds"))
            count = len(messages) if isinstance(messages, list) else (int(messages) if isinstance(messages, int) else 0)
            kinds_lg = 0
            if isinstance(kinds, dict):
                try:
                    kinds_lg = int(kinds.get("loop-guard") or 0)
                except (TypeError, ValueError):
                    kinds_lg = 0
            out["p6_snapshot"] = {
                "path": c.rel(snapshot_path),
                "loop_guard_messages": count,
                "kinds": kinds if isinstance(kinds, dict) else None,
            }
            if count > 0 or kinds_lg > 0:
                out["evidence"].append(
                    f"{c.rel(snapshot_path)}: P6 运行时快照 loop_guard_messages={count}"
                    + (f", kinds[loop-guard]={kinds_lg}" if kinds_lg else "")
                )
    for path in sorted(evidence_dir.glob("sessions-at-p6-*.jsonl")) if evidence_dir.is_dir() else []:
        hits = 0
        first_line = None
        try:
            text = path.read_text(encoding="utf-8", errors="replace")
        except OSError:
            continue
        for lineno, raw in enumerate(text.splitlines(), 1):
            if not raw.strip():
                continue
            try:
                obj = json.loads(raw)
            except ValueError:
                continue
            ctx_meta = obj.get("context") if isinstance(obj, dict) else None
            if isinstance(ctx_meta, dict) and ctx_meta.get("kind") == "loop-guard":
                hits += 1
                if first_line is None:
                    first_line = lineno
        out["p6_session_jsonl"].append({"path": c.rel(path), "loop_guard_count": hits})
        if hits:
            out["evidence"].append(f"{c.rel(path)}:{first_line}: P6 时刻会话快照含 loop-guard context ×{hits}")
    out["supported"] = bool(out["final_jsonl"] or out["evidence"])
    return out


def check_sessions_context(ctx: VerifyContext):
    c = ctx.c()
    group = "session"
    ws = ctx.workspace()
    if not ctx.sessions:
        c.fail("session.discovered", "发现 session JSONL", group, f"workspace/sessions/ 下无 *.jsonl")
        return
    c.ok(
        "session.discovered",
        "发现 session JSONL",
        group,
        f"{len(ctx.sessions)} 个文件",
        [c.rel(a.path) for a in ctx.sessions],
    )

    primary = ctx.primary
    agents_md = ctx.artifacts / "workspace" / "AGENTS.md"
    skills_dir = ws / "skills"
    has_taskstate = bool(primary and isinstance(primary.meta, dict) and primary.meta.get("task_state"))
    task_round_count = sum(1 for a in ctx.sessions for k in a.kinds if k == "task-round")
    todo_tool_calls = any(
        call.get("name") in ("todo_write", "task_update") for a in ctx.sessions for call in a.tool_calls
    )

    for analysis in ctx.sessions:
        rel = analysis.rel
        is_primary = analysis is primary
        severity = "required" if is_primary else "advisory"

        # 2.1 JSON 合法性
        if analysis.parse_issues:
            middle = [i for i in analysis.parse_issues if i["line"] < analysis.raw_nonempty]
            evidence = [
                f"{rel}:{item['line']}: {item.get('error')} | {item.get('snippet', '')}"
                for item in analysis.parse_issues[:6]
            ]
            if len(analysis.parse_issues) == 1 and analysis.parse_issues[0]["line"] >= analysis.raw_nonempty:
                c.warn(
                    "session.parse_jsonl",
                    "每一行都是合法 JSON",
                    group,
                    f"{rel}: 仅最后一行损坏（崩溃容忍路径）",
                    evidence,
                )
            elif middle:
                c.fail(
                    "session.parse_jsonl",
                    "每一行都是合法 JSON",
                    group,
                    f"{rel}: {len(analysis.parse_issues)} 行无法解析",
                    evidence,
                    severity=severity,
                )
            else:
                c.warn("session.parse_jsonl", "每一行都是合法 JSON", group, f"{rel}: 存在损坏行", evidence)
        else:
            c.ok(
                "session.parse_jsonl",
                "每一行都是合法 JSON",
                group,
                f"{rel}: {analysis.raw_nonempty} 行全部可解析",
                severity=severity,
            )

        # 2.2 消息 schema
        schema_problems = []
        for msg in analysis.messages:
            lineno, obj = msg["line"], msg["obj"]
            role = obj.get("role")
            if role not in ("system", "user", "assistant", "tool"):
                schema_problems.append(f"{rel}:{lineno}: 非法 role={clip(role, 40)}")
                continue
            if not isinstance(obj.get("content"), str):
                schema_problems.append(f"{rel}:{lineno}: content 非字符串")
            if role == "tool" and not obj.get("tool_call_id"):
                schema_problems.append(f"{rel}:{lineno}: tool 消息缺 tool_call_id")
            ctx_meta = msg_context(obj)
            if ctx_meta:
                kind = ctx_meta.get("kind")
                if not kind:
                    schema_problems.append(f"{rel}:{lineno}: context 缺 kind")
                form = ctx_meta.get("form") or ""
                if form not in ALLOWED_CONTEXT_FORMS:
                    schema_problems.append(f"{rel}:{lineno}: context.form 非预期: {clip(form, 30)}")
                for change in ctx_meta.get("changes") or []:
                    if isinstance(change, dict):
                        action = change.get("action")
                        if action not in ALLOWED_CHANGE_ACTIONS:
                            schema_problems.append(
                                f"{rel}:{lineno}: context.change.action 非预期: {clip(action, 30)}"
                            )
            if role == "assistant":
                for call in tool_calls_of(obj):
                    if not call["id"]:
                        schema_problems.append(f"{rel}:{lineno}: tool_call 缺 id")
                    if not call["name"]:
                        schema_problems.append(f"{rel}:{lineno}: tool_call 缺 function.name")
                    if call["args_raw"] and call["args"] is None:
                        schema_problems.append(
                            f"{rel}:{lineno}: tool_call {clip(call['id'], 24)} arguments 不是合法 JSON"
                        )
        if schema_problems:
            c.fail(
                "session.message_schema",
                "消息 / 上下文 schema 合法",
                group,
                f"{rel}: {len(schema_problems)} 处问题",
                schema_problems[:8],
                severity=severity,
            )
        else:
            c.ok(
                "session.message_schema",
                "消息 / 上下文 schema 合法",
                group,
                f"{rel}: {len(analysis.messages)} 条消息",
                severity=severity,
            )

        # 2.3 tool_call / tool_result 一一配对
        pairing = analysis.pairing()
        pairing_problems = []
        for call in pairing["duplicate_call_ids"]:
            pairing_problems.append(f"{rel}:{call['line']}: 重复 tool_call id {clip(call['id'], 40)}")
        for res in pairing["duplicate_results"]:
            pairing_problems.append(f"{rel}:{res['line']}: 重复 tool result {clip(res['id'], 40)}")
        unmatched = pairing["unmatched_calls"]
        for call in unmatched:
            tol = ""
            if not ctx.exp["allow_incomplete_tail"] and call["line"] >= (analysis.active[-1][0] if analysis.active else 0) - 1:
                tol = "（文件末尾，可能是中断；可用 allow_incomplete_tail 放宽）"
            pairing_problems.append(
                f"{rel}:{call['line']}: tool_call {clip(call['id'], 40)} ({clip(call['name'], 24)}) 无 tool_result{tol}"
            )
        orphan = pairing["orphan_results"]
        skip_lines = int(analysis.meta.get("skip") or 0) if isinstance(analysis.meta, dict) else 0
        summary_chars = len((analysis.meta or {}).get("summary") or "") if isinstance(analysis.meta, dict) else 0
        summarize_events = len(log_events_with(getattr(ctx, "corpus", []), "agent.session.summarize"))
        rewrite_evidence = skip_lines > 0 or summary_chars > 0 or summarize_events > 0
        # 判定每个孤儿结果是否位于“保留历史开头、其 assistant tool_call 已被摘要/压缩丢弃”的边界：
        # 该结果之前没有任何 assistant 消息（因此不可能存在对应 call）。
        active_objs = [(lineno, obj) for lineno, obj in analysis.active if isinstance(obj, dict)]
        lead_tool_boundary = []
        mid_orphans = []
        for res in orphan:
            index = next((i for i, (ln, _obj) in enumerate(active_objs) if ln == res["line"]), -1)
            if index < 0:
                mid_orphans.append(res)
                continue
            preceded_by_assistant = any(
                obj.get("role") == "assistant" for _ln, obj in active_objs[:index]
            )
            if not preceded_by_assistant and rewrite_evidence:
                lead_tool_boundary.append(res)
            else:
                mid_orphans.append(res)
        for res in mid_orphans:
            pairing_problems.append(f"{rel}:{res['line']}: tool_result {clip(res['id'], 40)} 无对应 tool_call")
        if lead_tool_boundary:
            c.warn(
                "session.tool_pairing_boundary",
                "tool 配对边界：历史重写后保留的历史开头可能有孤立 tool_result",
                group,
                f"{rel}: {len(lead_tool_boundary)} 个 tool_result 位于保留历史开头，其 assistant tool_call 已被"
                f"摘要/压缩丢弃（summary={summary_chars} 字符, skip={skip_lines}, summarize 事件 {summarize_events}），"
                "属合法重写边界（harness 的配对断言为单向：所有 call 均有 result）",
                [f"{rel}:{r['line']}: tool_result {clip(r['id'], 40)}（call 已不在保留历史）" for r in lead_tool_boundary[:4]],
            )
        if pairing_problems:
            c.fail(
                "session.tool_pairing",
                "tool_call 与 tool_result 一一配对",
                group,
                f"{rel}: {len(pairing_problems)} 处不配对",
                pairing_problems[:8],
                severity=severity,
            )
        else:
            c.ok(
                "session.tool_pairing",
                "tool_call 与 tool_result 一一配对",
                group,
                f"{rel}: {len(pairing['calls'])} 组调用全部配对",
                severity=severity,
            )

        # 2.4 context kind 直方图
        hist = analysis.kind_histogram()
        hist_desc = ", ".join(f"{kind}×{info['count']}" for kind, info in hist.items())
        if analysis.unknown_kinds:
            c.fail(
                "session.context_histogram",
                "context kind 直方图符合契约",
                group,
                f"{rel}: 未知 kind",
                [f"{rel}:{item['first']}: 未知 context kind={item['kind']}" for item in analysis.unknown_kinds],
                severity=severity,
            )
        else:
            c.ok("session.context_histogram", "context kind 直方图符合契约", group, f"{rel}: {hist_desc or '(无 context)'}", severity=severity)

        expected_kinds = {}
        if agents_md.exists():
            expected_kinds["workspace-instructions"] = "workspace/AGENTS.md 存在"
        expected_kinds["runtime-context"] = "runtime snapshot 注入预期"
        if has_taskstate or task_round_count or todo_tool_calls:
            expected_kinds["task-state"] = "存在持久 task_state 证据"
        if skills_dir.is_dir() and any(skills_dir.iterdir()):
            expected_kinds["skill-catalog"] = "workspace/skills 已配置"
        # 注意：P-10 普通自主轮提示以普通 user 消息持久化（kind=None）；
        # ContextKindTaskRound 仅在“模型空回复且任务仍 active”时由
        # appendTaskContinuation 注入，因此真实场景不要求出现 kind=task-round。
        observed = observed_context_kinds(ctx)
        if is_primary:
            ctx.context_kinds_observed = observed
        effective = set(analysis.kinds) | set(observed["kinds"])
        missing = [kind for kind in expected_kinds if kind not in effective]
        covered_by_snapshot = [
            kind for kind in expected_kinds if kind not in analysis.kinds and kind in observed["kinds"]
        ]
        source_desc = f"最终 JSONL: {', '.join(sorted(analysis.kinds)) or '(无)'}"
        if observed["kinds"]:
            source_desc += f"; 运行期快照: {', '.join(sorted(observed['kinds']))}"
        if missing:
            c.fail(
                "session.context_kinds_present",
                "预期的 context kind 均出现（最终 JSONL 或运行期快照）",
                group,
                f"{rel}: 缺失 {', '.join(missing)}（{source_desc}）",
                [f"{rel}: {expected_kinds[kind]}" for kind in missing],
                severity=severity,
            )
        else:
            c.ok(
                "session.context_kinds_present",
                "预期的 context kind 均出现（最终 JSONL 或运行期快照）",
                group,
                f"{rel}: {', '.join(sorted(expected_kinds))}；{source_desc}",
                observed["evidence"],
                severity=severity,
            )
        if covered_by_snapshot:
            c.warn(
                "session.context_kinds_evidence",
                "必需 context kind 未保留在最终 JSONL，但由运行期 kind 并集快照佐证（summarize 合法重写）",
                group,
                f"{rel}: {', '.join(covered_by_snapshot)} 仅存在于运行期快照；final JSONL 被 summarize 重写",
                observed["evidence"],
            )

        # 2.4a task-round 语义说明（已确认实现细节，不作为 required 存在性要求）
        continuation_msgs = [
            f"{rel}:{lineno}"
            for lineno, role, kind, content in analysis.texts
            if role == "user" and not kind and "<task_round>" in content
        ]
        task_round_kind = analysis.kinds.get("task-round")
        c.ok(
            "session.task_round_semantics",
            "task-round 语义：普通自主轮提示以普通 user 消息持久化，kind=task-round 仅空回复续跑时出现",
            group,
            f"{rel}: user-visible <task_round> 提示 {len(continuation_msgs)} 条；kind=task-round context "
            f"{task_round_kind['count'] if task_round_kind else 0} 条（不作为 required 存在性要求）",
            continuation_msgs[:3],
            severity="advisory",
        )

        # 2.4b loop-guard / context-pressure 出现性
        loop_kind = analysis.kinds.get("loop-guard")
        require_loop = ctx.exp["require"].get("loop_guard")
        loop_evidence = loop_guard_evidence(ctx)
        loop_required = require_loop is True or (
            require_loop == "auto"
            and (
                "loop-guard" in (ctx.results_text or "").lower()
                or bool(loop_kind)
                or loop_evidence["supported"]
            )
        )
        if is_primary:
            ctx.loop_guard_evidence = loop_evidence
            ctx.loop_guard_required = loop_required
            c.ok(
                "session.loop_guard_evidence",
                "loop-guard 运行时注入证据（最终 JSONL 或 P6 快照）",
                group,
                f"最终 JSONL={len(loop_evidence['final_jsonl'])} 条, P6 快照={len(loop_evidence['evidence'])} 处",
                loop_evidence["evidence"] or [f"{rel}: 最终 JSONL loop-guard={loop_kind['count'] if loop_kind else 0} 条"],
                severity="advisory",
            )
        if loop_evidence["supported"]:
            sources = []
            if loop_evidence["final_jsonl"]:
                sources.append(f"最终 JSONL {len(loop_evidence['final_jsonl'])} 条")
            if loop_evidence["p6_snapshot"] and not loop_evidence["p6_snapshot"].get("error"):
                snap = loop_evidence["p6_snapshot"]
                if snap.get("loop_guard_messages"):
                    sources.append(f"P6 快照 loop_guard_messages={snap['loop_guard_messages']}")
                elif (snap.get("kinds") or {}).get("loop-guard"):
                    sources.append(f"P6 快照 kinds[loop-guard]={snap['kinds'].get('loop-guard')}")
            for item in loop_evidence["p6_session_jsonl"]:
                if item["loop_guard_count"]:
                    sources.append(f"P6 会话快照 {item['loop_guard_count']} 条")
            c.ok(
                "session.loop_guard_present",
                "loop-guard 提示上下文存在（最终 JSONL 或 P6 运行时快照）",
                group,
                f"{rel}: " + ("; ".join(sources) if sources else "有证据")
                + ("（后续 summarize/压缩合法改写历史，最终 JSONL 可不保留）" if not loop_kind else ""),
                loop_evidence["evidence"]
                or [f"{rel}:{loop_evidence['final_jsonl'][0].split(':')[-1]}: loop-guard context" if loop_evidence["final_jsonl"] else f"{rel}: {loop_kind['count']} 次"],
            )
        elif loop_required:
            c.fail(
                "session.loop_guard_present",
                "loop-guard 提示上下文存在（最终 JSONL 或 P6 运行时快照）",
                group,
                f"{rel}: 最终 JSONL 与 evidence/ P6 快照均无 loop-guard context（场景/声明预期存在）",
                [
                    f"{rel}: 最终 JSONL loop-guard=0",
                    f"{c.rel(ctx.artifacts / 'evidence' / 'context-p6.json')}: 不存在或无 loop_guard_messages",
                ],
                severity=severity,
            )
        else:
            c.skip("session.loop_guard_present", "loop-guard 提示上下文存在（最终 JSONL 或 P6 运行时快照）", group, f"{rel}: 无（无预期）")
        pressure_kind = analysis.kinds.get("context-pressure")
        if pressure_kind:
            c.ok(
                "session.context_pressure_present",
                "context-pressure 提示上下文存在",
                group,
                f"{rel}: {pressure_kind['count']} 次",
                severity="advisory",
            )
        else:
            c.skip(
                "session.context_pressure_present",
                "context-pressure 提示上下文存在",
                group,
                f"{rel}: 未触发临近上限提醒（压缩可能先于提醒生效，属可选路径）",
            )

        # 2.5 回合段内 pre-step context 首次出现顺序
        order_problems = []
        for seg in analysis.segments:
            first = {}
            for item in seg["contexts"]:
                first.setdefault(item["kind"], item["line"])
            # 关键：按“首次出现的行号”排序，而不是按 CORE_ORDER（否则永远有序）
            present = sorted((kind for kind in CORE_ORDER if kind in first), key=lambda k: first[k])
            indexes = [CORE_ORDER.index(kind) for kind in present]
            if indexes != sorted(indexes):
                order_problems.append(
                    f"{rel}:{seg['start_line']}: 回合段 context 顺序异常 "
                    + " -> ".join(f"{kind}@{first[kind]}" for kind in present)
                )
        if order_problems:
            skip_lines = int(analysis.meta.get("skip") or 0) if isinstance(analysis.meta, dict) else 0
            summary_chars = len((analysis.meta or {}).get("summary") or "") if isinstance(analysis.meta, dict) else 0
            if skip_lines > 0 or summary_chars > 0:
                c.warn(
                    "session.context_order",
                    "context 注入顺序符合 pre-step 顺序",
                    group,
                    f"{rel}: {len(order_problems)} 个回合段顺序异常；但存在逻辑截断(skip={skip_lines})或摘要重建"
                    f"(summary={summary_chars})，重建后的注入顺序可能变化，降级为 advisory",
                    order_problems[:6],
                )
            else:
                c.fail(
                    "session.context_order",
                    "context 注入顺序符合 pre-step 顺序",
                    group,
                    f"{rel}: {len(order_problems)} 个回合段顺序异常",
                    order_problems[:6],
                    severity=severity,
                )
        else:
            c.ok(
                "session.context_order",
                "context 注入顺序符合 pre-step 顺序",
                group,
                f"{rel}: {len(analysis.segments)} 个回合段顺序正常",
                severity=severity,
            )

        # 2.6 supersede / 去重语义
        for kind, info in analysis.supersede_analysis().items():
            analysis.supersede[kind] = info
        dedup_problems = []
        for kind, info in analysis.supersede.items():
            for dup in info["duplicate_adjacent"]:
                dedup_problems.append(
                    f"{rel}:{dup['line']}: {kind} 与上一条去重键相同（{clip(dup['digest'], 60)}）"
                )
        if dedup_problems:
            skip = int((analysis.meta or {}).get("skip") or 0) if isinstance(analysis.meta, dict) else 0
            summary_chars = len((analysis.meta or {}).get("summary") or "") if isinstance(analysis.meta, dict) else 0
            if skip > 0 or summary_chars > 0:
                c.warn(
                    "session.context_supersede",
                    "状态型上下文具备 supersede/去重语义",
                    group,
                    f"{rel}: 检测到相邻重复去重键；但存在逻辑截断(skip={skip})或摘要重建(summary={summary_chars} 字符)，"
                    "历史被合法重写后同一内容可被重新注入，降级为 advisory",
                    dedup_problems[:6],
                )
            else:
                # 已确认实现语义（mechanism-audit D4）：supersede/去重只在回合起点生效，
                # 同一回合内的重复注入属已知实现限制，不作为 required 契约；
                # harness P9.8 仅断言 workspace-instructions 不重复注入，不应映射到全局 supersede。
                c.warn(
                    "session.context_supersede",
                    "状态型上下文具备 supersede/去重语义（已确认实现限制 D4：仅回合起点生效）",
                    group,
                    f"{rel}: {len(dedup_problems)} 处相邻重复去重键（D4 已知限制，advisory；"
                    "harness P9.8 仅覆盖 workspace-instructions 不重复注入）",
                    dedup_problems[:6],
                )
        else:
            summary = "; ".join(
                f"{kind} {info['copies']} 份/{info['distinct_keys']} 去重键" for kind, info in analysis.supersede.items()
            )
            c.ok(
                "session.context_supersede",
                "状态型上下文具备 supersede/去重语义（已确认实现限制 D4：仅回合起点生效）",
                group,
                f"{rel}: {summary}",
                severity=severity,
            )
        # 模型可见视图：每种状态型上下文只保留最新一份（报告数值）
        visible = {kind: (1 if info["copies"] else 0) for kind, info in analysis.supersede.items()}
        c.ok(
            "session.superseded_visible_view",
            "supersede 后模型可见视图只剩最新副本",
            group,
            f"{rel}: 旧副本 {sum(max(0, i['copies'] - 1) for i in analysis.supersede.values())} 份保留在审计日志；可见视图 "
            + ", ".join(f"{k}×{v}" for k, v in visible.items()),
            severity="advisory",
        )

    # 2.7 最终 JSONL 保留范围说明（压缩会物理重写 JSONL，早期阶段上下文可能已不在文件内）
    if primary:
        hist = primary.kind_histogram()
        missing_kinds = [kind for kind, info in hist.items() if info["count"] == 0]
        summary_chars = len((primary.meta or {}).get("summary") or "") if isinstance(primary.meta, dict) else 0
        covered = []
        if "loop-guard" in missing_kinds and (getattr(ctx, "loop_guard_evidence", {}) or {}).get("supported"):
            covered.append("loop-guard 已由 P6 快照佐证")
        kinds_snapshot = getattr(ctx, "context_kinds_observed", {}) or {}
        snapshot_covered = [
            kind for kind in missing_kinds if kind in (kinds_snapshot.get("kinds") or {})
        ]
        if snapshot_covered:
            covered.append(
                "final JSONL 被 summarize 重写，缺失 kind " + ", ".join(snapshot_covered) + " 由运行期 kind 并集快照佐证"
            )
        c.ok(
            "session.final_jsonl_retained_scope",
            "最终 JSONL 保留范围（压缩后可能不含早期阶段上下文）",
            group,
            f"{primary.rel}: {len(primary.messages)} 条消息；缺失 kind={missing_kinds or '无'}；"
            f"summary={summary_chars} 字符。压缩/摘要会物理重写 JSONL，早期阶段的 loop-guard/task-round/delta "
            "可能已不在最终文件中，相关 PASS 声明需以运行期快照或 gateway 日志佐证"
            + (f"；{'；'.join(covered)}" if covered else "")
            + "。",
            severity="advisory",
        )

    # 2.8 跨 session：主 session 选择说明
    if primary:
        c.ok(
            "session.primary",
            "选定主 session",
            group,
            f"{primary.rel}（{primary.raw_nonempty} 行，最大）",
            severity="advisory",
        )


# ---------------------------------------------------------------------------
# 3) meta.json
# ---------------------------------------------------------------------------


def validate_task_state(state: dict, rel_meta: str, c: Checker, group: str, severity: str, exp: dict):
    problems = []
    required = list(TASKSTATE_REQUIRED_KEYS)
    if exp["require"].get("todos") is True:
        required.append("todos")
    if exp["require"].get("evidence") is True:
        required.append("evidence")
    for key in required:
        if key not in state:
            problems.append(f"{rel_meta}: task_state 缺字段 {key}")
    allowed = set(TASKSTATE_REQUIRED_KEYS) | set(TASKSTATE_OPTIONAL_KEYS)
    for key in state:
        if key not in allowed:
            problems.append(f"{rel_meta}: task_state 未知字段 {key}（schema 漂移）")
    if problems:
        c.fail("meta.task_state_schema", "task_state schema 合法", group, f"{rel_meta}: {len(problems)} 处问题", problems[:8], severity=severity)
        return
    for key in ("version", "round", "max_rounds"):
        if not isinstance(state.get(key), int) or isinstance(state.get(key), bool):
            problems.append(f"{rel_meta}: task_state.{key} 必须是整数")
    for key in (
        "no_progress_rounds",
        "blocker_rounds",
        "blocker_last_round",
    ):
        if key in state and not isinstance(state[key], int):
            problems.append(f"{rel_meta}: task_state.{key} 必须是整数")
    version = state.get("version")
    if isinstance(version, int) and not (0 <= version <= 1):
        problems.append(f"{rel_meta}: task_state.version 越界: {version}")
    objective = state.get("objective")
    if not isinstance(objective, str) or not objective.strip():
        problems.append(f"{rel_meta}: task_state.objective 为空")
    phase = state.get("phase")
    if phase not in TASKSTATE_PHASES:
        problems.append(f"{rel_meta}: task_state.phase 非法: {clip(phase, 30)}")
    maximum = state.get("max_rounds")
    if not isinstance(maximum, int) or not (1 <= maximum <= 200):
        problems.append(f"{rel_meta}: task_state.max_rounds 越界: {maximum}")
    round_no = state.get("round")
    if isinstance(round_no, int):
        if round_no < 0:
            problems.append(f"{rel_meta}: task_state.round 为负: {round_no}")
        elif isinstance(maximum, int) and round_no > maximum:
            problems.append(f"{rel_meta}: task_state.round({round_no}) > max_rounds({maximum})")
    for key in ("last_progress_at", "updated_at"):
        if key in state:
            val = state[key]
            if not isinstance(val, str) or not val.strip():
                problems.append(f"{rel_meta}: task_state.{key} 非法时间")
            elif val.strip():
                try:
                    datetime.fromisoformat(val.replace("Z", "+00:00"))
                except ValueError:
                    problems.append(f"{rel_meta}: task_state.{key} 不是 RFC3339: {clip(val, 40)}")
    todos = state.get("todos") or []
    if todos:
        if not isinstance(todos, list):
            problems.append(f"{rel_meta}: task_state.todos 必须是数组")
        else:
            seen = set()
            for idx, todo in enumerate(todos):
                if not isinstance(todo, dict):
                    problems.append(f"{rel_meta}: task_state.todos[{idx}] 不是 object")
                    continue
                extra = set(todo) - {"content", "status"}
                if extra:
                    problems.append(f"{rel_meta}: task_state.todos[{idx}] 未知字段 {sorted(extra)}")
                content = todo.get("content")
                if not isinstance(content, str) or not content.strip():
                    problems.append(f"{rel_meta}: task_state.todos[{idx}].content 为空")
                elif norm_ws(content) in seen:
                    problems.append(f"{rel_meta}: task_state.todos 内容重复: {clip(content, 60)}")
                else:
                    seen.add(norm_ws(content))
                if todo.get("status") not in TASKSTATE_TODO_STATUS:
                    problems.append(f"{rel_meta}: task_state.todos[{idx}].status 非法: {clip(todo.get('status'), 30)}")
    evidence = state.get("evidence") or []
    if evidence:
        if not isinstance(evidence, list):
            problems.append(f"{rel_meta}: task_state.evidence 必须是数组")
        else:
            for idx, item in enumerate(evidence):
                if not isinstance(item, str) or not item.strip():
                    problems.append(f"{rel_meta}: task_state.evidence[{idx}] 为空或非字符串")
    blocked = state.get("blocked_reason")
    if phase == "blocked":
        if not isinstance(blocked, dict):
            problems.append(f"{rel_meta}: blocked 阶段缺 blocked_reason")
        else:
            if blocked.get("code") not in TASKSTATE_BLOCKED_CODES:
                problems.append(f"{rel_meta}: blocked_reason.code 非法: {clip(blocked.get('code'), 30)}")
            if not isinstance(blocked.get("message"), str) or not blocked["message"].strip():
                problems.append(f"{rel_meta}: blocked_reason.message 为空")
    elif blocked is not None:
        problems.append(f"{rel_meta}: 非 blocked 阶段不应有 blocked_reason")

    if problems:
        c.fail("meta.task_state_schema", "task_state schema 合法", group, f"{rel_meta}: {len(problems)} 处问题", problems[:10], severity=severity)
    else:
        c.ok(
            "meta.task_state_schema",
            "task_state schema 合法",
            group,
            f"{rel_meta}: phase={phase} round={round_no}/{maximum} todos={len(todos)} evidence={len(evidence)}",
            severity=severity,
        )


def check_meta(ctx: VerifyContext):
    c = ctx.c()
    group = "meta"
    primary = ctx.primary
    if not ctx.sessions:
        c.skip("meta.present", "读取 session meta.json", group, "无 session 文件")
        return
    for analysis in ctx.sessions:
        is_primary = analysis is primary
        severity = "required" if is_primary else "advisory"
        meta_rel = c.rel(analysis.meta_path) if analysis.meta_path else analysis.rel
        if analysis.meta_error or not isinstance(analysis.meta, dict):
            c.fail(
                "meta.parse",
                "meta.json 存在且可解析",
                group,
                f"{meta_rel}: {analysis.meta_error or '无 meta'}",
                severity=severity,
            )
            continue
        meta = analysis.meta
        c.ok("meta.parse", "meta.json 存在且可解析", group, meta_rel, severity=severity)

        # count / skip 一致性
        problems = []
        try:
            count = int(meta.get("count"))
        except (TypeError, ValueError):
            count = None
            problems.append(f"{meta_rel}: count 缺失或非整数")
        try:
            skip = int(meta.get("skip") or 0)
        except (TypeError, ValueError):
            skip = 0
            problems.append(f"{meta_rel}: skip 非整数")
        if count is not None and count != analysis.raw_nonempty:
            problems.append(f"{meta_rel}: count={count} 与 JSONL 非空行数 {analysis.raw_nonempty} 不一致")
        if skip < 0:
            problems.append(f"{meta_rel}: skip 为负: {skip}")
        if count is not None and skip > count:
            problems.append(f"{meta_rel}: skip({skip}) > count({count})")
        stem = analysis.path.name
        for suffix in (".jsonl",):
            if stem.endswith(suffix):
                stem = stem[: -len(suffix)]
        if isinstance(meta.get("key"), str) and meta["key"]:
            sanitized = re.sub(r"[:/\\]", "_", meta["key"])
            if sanitized != stem:
                problems.append(f"{meta_rel}: key={clip(meta['key'], 40)} 与文件名 {stem} 不一致")
        for key in ("created_at", "updated_at"):
            val = meta.get(key)
            if not isinstance(val, str) or not val.strip():
                problems.append(f"{meta_rel}: {key} 缺失")
                continue
            try:
                datetime.fromisoformat(val.replace("Z", "+00:00"))
            except ValueError:
                problems.append(f"{meta_rel}: {key} 不是 RFC3339: {clip(val, 40)}")
        if problems:
            c.fail("meta.count_consistency", "meta count/skip/key 一致", group, f"{meta_rel}: {len(problems)} 处问题", problems[:8], severity=severity)
        else:
            c.ok("meta.count_consistency", "meta count/skip/key 一致", group, f"{meta_rel}: count={count} skip={skip}", severity=severity)

        # summary
        summary = meta.get("summary") if isinstance(meta.get("summary"), str) else ""
        require_summary = is_primary and ctx.exp["require"].get("summary") is not False
        if summary.strip():
            c.ok("meta.summary", "会话摘要存在", group, f"{meta_rel}: {len(summary)} 字符", severity=severity)
        elif require_summary:
            c.fail("meta.summary", "会话摘要存在", group, f"{meta_rel}: summary 为空", severity="required")
        else:
            c.warn("meta.summary", "会话摘要存在", group, f"{meta_rel}: summary 为空（advisory）")

        # task_state 存在性 + schema
        state = meta.get("task_state")
        expect_state = ctx.exp["require"].get("task_state")
        derived_expected = bool(
            analysis.kinds.get("task-state")
            or analysis.kinds.get("task-round")
            or any(call.get("name") in ("todo_write", "task_update") for call in analysis.tool_calls)
        )
        if isinstance(state, dict):
            validate_task_state(state, meta_rel, c, group, severity, ctx.exp)
            if not isinstance(state.get("todos"), list) or not state.get("todos"):
                if ctx.exp["require"].get("todos") is True or (ctx.exp["require"].get("todos") == "auto" and (
                    any(call.get("name") == "todo_write" for call in analysis.tool_calls)
                )):
                    c.fail("meta.task_state_todos", "task_state.todos 非空", group, f"{meta_rel}: todos 缺失或为空", severity=severity)
                elif ctx.exp["require"].get("todos") is None or ctx.exp["require"].get("todos") is False:
                    pass
            if not isinstance(state.get("evidence"), list) or not state.get("evidence"):
                if ctx.exp["require"].get("evidence") is True:
                    c.fail("meta.task_state_evidence", "task_state.evidence 非空", group, f"{meta_rel}: evidence 缺失或为空", severity=severity)
        elif expect_state is True or (expect_state == "auto" and derived_expected):
            c.fail(
                "meta.task_state_present",
                "持久 task_state 存在",
                group,
                f"{meta_rel}: 无 task_state 字段，但存在任务/自主续跑证据",
                [f"{analysis.rel}: task-round={analysis.kinds.get('task-round', {}).get('count', 0)}, task-state context={analysis.kinds.get('task-state', {}).get('count', 0)}"],
                severity=severity,
            )
        else:
            c.skip("meta.task_state_present", "持久 task_state 存在", group, f"{meta_rel}: 无 task_state（无任务证据）", severity=severity)

    # 主 session 的 task-state 渲染块与 meta 交叉核对
    if primary and isinstance(primary.meta, dict) and isinstance(primary.meta.get("task_state"), dict):
        state = primary.meta["task_state"]
        task_state_msgs = [
            (lineno, obj) for lineno, obj in primary.active if isinstance(obj, dict) and msg_kind(obj) == "task-state"
        ]
        if not task_state_msgs:
            c.fail(
                "meta.taskstate_matches_context",
                "最后一份 task-state 上下文与持久 task_state 一致",
                group,
                f"{primary.rel}: 有 meta.task_state 但 JSONL 无 task-state 上下文",
            )
            return
        lineno, last = task_state_msgs[-1]
        parsed = parse_task_state_render(last.get("content") or "")
        if parsed is None:
            c.fail(
                "meta.taskstate_matches_context",
                "最后一份 task-state 上下文与持久 task_state 一致",
                group,
                f"{primary.rel}:{lineno}: 最后一条 task-state 无法按 P-09 格式解析",
                [c.ev(primary.path, lineno, (last.get("content") or "")[:120])],
            )
            return
        problems = []
        if norm_ws(parsed.get("objective", "")) != norm_ws(state.get("objective", "")):
            problems.append(
                f"{primary.rel}:{lineno}: objective 不一致: context={clip(parsed.get('objective'), 60)} meta={clip(state.get('objective'), 60)}"
            )
        if parsed.get("phase") != state.get("phase"):
            problems.append(
                f"{primary.rel}:{lineno}: phase 不一致: context={parsed.get('phase')} meta={state.get('phase')}"
            )
        if parsed.get("max_rounds") != state.get("max_rounds"):
            problems.append(
                f"{primary.rel}:{lineno}: max_rounds 不一致: context={parsed.get('max_rounds')} meta={state.get('max_rounds')}"
            )
        meta_round = state.get("round")
        ctx_round = parsed.get("round")
        if isinstance(meta_round, int) and isinstance(ctx_round, int) and ctx_round > meta_round:
            problems.append(f"{primary.rel}:{lineno}: context round({ctx_round}) > meta round({meta_round})")
        excepted = state.get("todos") or []
        rendered = parsed.get("todos") or []
        if len(excepted) != len(rendered):
            problems.append(f"{primary.rel}:{lineno}: todos 数量不一致: context={len(rendered)} meta={len(excepted)}")
        else:
            for idx, (meta_todo, ctx_todo) in enumerate(zip(excepted, rendered)):
                if meta_todo.get("status") != ctx_todo.get("status"):
                    problems.append(f"{primary.rel}:{lineno}: todos[{idx}].status 不一致")
                meta_content = norm_ws(meta_todo.get("content", ""))
                ctx_content = norm_ws(ctx_todo.get("content", ""))
                if not (meta_content == ctx_content or meta_content.startswith(ctx_content) or ctx_content.startswith(meta_content[:80])):
                    problems.append(f"{primary.rel}:{lineno}: todos[{idx}].content 不一致")
        meta_ev = [norm_ws(e) for e in (state.get("evidence") or [])][-5:]
        rendered_ev = [norm_ws(e) for e in (parsed.get("evidence") or [])]
        if len(rendered_ev) != len(meta_ev):
            problems.append(f"{primary.rel}:{lineno}: evidence 数量不一致: context={len(rendered_ev)} meta={len(meta_ev)}")
        else:
            for idx, (m_ev, r_ev) in enumerate(zip(meta_ev, rendered_ev)):
                if not (m_ev == r_ev or m_ev.startswith(r_ev)):
                    problems.append(f"{primary.rel}:{lineno}: evidence[{idx}] 不一致")
        if problems:
            c.fail(
                "meta.taskstate_matches_context",
                "最后一份 task-state 上下文与持久 task_state 一致",
                group,
                f"{primary.rel}:{lineno}: {len(problems)} 处不一致",
                problems[:8],
            )
        else:
            c.ok(
                "meta.taskstate_matches_context",
                "最后一份 task-state 上下文与持久 task_state 一致",
                group,
                f"{primary.rel}:{lineno}: phase/rounds/todos/evidence 一致",
            )


# ---------------------------------------------------------------------------
# 4) spill
# ---------------------------------------------------------------------------


def _map_spill_path(ctx: VerifyContext, raw: str) -> tuple[Path | None, str]:
    """把运行期绝对 spill 路径重映射到 artifacts 中的工作区副本。

    真实 harness 把 /tmp 工作区原样复制到 artifacts/workspace，JSONL 里
    spill marker 记录的是运行期绝对路径；workspace.path 提供原始根目录。
    """
    if not raw:
        return None, "空路径"
    parsed = Path(raw)
    original = ctx.original_workspace
    if original:
        try:
            rel = parsed.resolve().relative_to(Path(original).resolve())
        except (ValueError, OSError):
            rel = None
        if rel is not None:
            return ctx.workspace() / rel, f"workspace.path 重映射: {Path(original)} -> {ctx.workspace()}"
    return parsed, "直接路径"


def check_spill(ctx: VerifyContext):
    c = ctx.c()
    group = "spill"
    spill_root = ctx.workspace() / ".chagent" / "spill"
    refs = []
    for analysis in ctx.sessions:
        for ref in analysis.prune_refs:
            ref["session_rel"] = analysis.rel
            refs.append(ref)

    compression_events = log_events_with(getattr(ctx, "corpus", []), "pruned tool results") + log_events_with(
        getattr(ctx, "corpus", []), "proactive compression"
    )
    spilled_field_total = sum(log_int_field(compression_events, "spilled_msgs"))

    auto = ctx.exp["require"].get("spill")
    if not spill_root.exists():
        if refs or auto is True:
            c.fail(
                "spill.dir_exists",
                ".chagent/spill 目录存在",
                group,
                f"缺失: {c.rel(spill_root)}，但 JSONL 中有 {len(refs)} 处 spill prune marker",
                [c.ev(c.workspace().parent / "__missing__", r["line"], r["path"]) for r in refs][:6],
            )
        else:
            c.skip("spill.dir_exists", ".chagent/spill 目录存在", group, "无 spill 预期与引用")
        ctx.spill = {"root": str(spill_root), "exists": False}
        return
    perm = mode_perm(spill_root)
    if perm == 0o700:
        c.ok("spill.dir_mode", "spill 目录权限 0700", group, f"{c.rel(spill_root)}: 0700")
    else:
        c.fail(
            "spill.dir_mode",
            "spill 目录权限 0700",
            group,
            f"{c.rel(spill_root)}: 实际 {oct(perm) if perm is not None else 'unknown'}",
        )

    spill_dirs = [spill_root] + [p for p in spill_root.rglob("*") if p.is_dir()]
    bad_dirs = [p for p in spill_dirs if mode_perm(p) != 0o700]
    if bad_dirs:
        c.fail(
            "spill.subdir_mode",
            "spill 会话子目录权限 0700",
            group,
            f"{len(bad_dirs)} 个目录权限非 0700",
            [f"{c.rel(p)}: {oct(mode_perm(p))}" for p in bad_dirs[:6]],
        )
    else:
        c.ok("spill.subdir_mode", "spill 会话子目录权限 0700", group, f"{len(spill_dirs) - 1} 个子目录")

    files = [p for p in spill_root.rglob("*") if p.is_file()]
    bad_files = [p for p in files if mode_perm(p) != 0o600]
    if bad_files:
        c.fail(
            "spill.file_mode",
            "spill 文件权限 0600",
            group,
            f"{len(bad_files)}/{len(files)} 个文件权限非 0600",
            [f"{c.rel(p)}: {oct(mode_perm(p))}" for p in bad_files[:6]],
        )
    else:
        c.ok("spill.file_mode", "spill 文件权限 0600", group, f"{len(files)} 个文件")

    # ---- 内容寻址完整性（不依赖 JSONL marker；压缩可能已丢弃 marker 消息） ----
    integrity_problems = []
    integrity_ok = []
    for path in files:
        data = read_bytes(path)
        if data is None:
            integrity_problems.append(f"{c.rel(path)}: 无法读取")
            continue
        try:
            text = data.decode("utf-8")
        except UnicodeDecodeError:
            integrity_problems.append(f"{c.rel(path)}: 非 UTF-8 文本")
            continue
        m = re.match(r"^(?P<label>.+)-(?P<hash>[0-9a-f]{16})\.txt$", path.name)
        if not m:
            integrity_problems.append(f"{c.rel(path)}: 文件名不符合内容寻址格式 <label>-<sha256[:16]>.txt")
            continue
        expected = sha256_bytes(data)[:16]
        if m.group("hash") != expected:
            integrity_problems.append(
                f"{c.rel(path)}: 内容寻址摘要不匹配（文件名 {m.group('hash')} / 内容 {expected}）"
            )
        if len(text) <= DEFAULT_SPILL_THRESHOLD:
            integrity_problems.append(
                f"{c.rel(path)}: 全文 {len(text)} 字符未超过裁剪阈值 {DEFAULT_SPILL_THRESHOLD}，不该被 spill"
            )
        if not integrity_problems:
            integrity_ok.append(f"{c.rel(path)}: {len(text)} 字符, sha256[:16]={expected}")
    if files and not integrity_problems:
        c.ok(
            "spill.file_integrity",
            "spill 文件内容寻址完整（文件名 sha256 与内容一致、体积超过裁剪阈值）",
            group,
            f"{len(files)} 个文件全部一致",
            integrity_ok,
        )
    elif files:
        c.fail(
            "spill.file_integrity",
            "spill 文件内容寻址完整（文件名 sha256 与内容一致、体积超过裁剪阈值）",
            group,
            f"{len(integrity_problems)} 处问题",
            integrity_problems[:8],
        )
    else:
        c.skip("spill.file_integrity", "spill 文件内容寻址完整（文件名 sha256 与内容一致、体积超过裁剪阈值）", group, "无 spill 文件")

    # ---- JSONL prune marker 引用（若已被压缩丢弃，则用日志字段 + 文件完整性兜底） ----
    referenced = set()
    problems = []
    for ref in refs:
        mapped, how = _map_spill_path(ctx, ref["path"])
        if mapped is None:
            problems.append(f"{ref['session_rel']}:{ref['line']}: 无法解析 spill 路径: {ref['path']}")
            continue
        referenced.add(mapped)
        if not mapped.exists():
            problems.append(f"{ref['session_rel']}:{ref['line']}: spill 文件不存在（{how}）: {mapped}")
            continue
        try:
            mapped.resolve().relative_to(spill_root.resolve())
        except ValueError:
            problems.append(f"{ref['session_rel']}:{ref['line']}: spill 路径逃出 spill 根目录: {mapped}")
            continue
        text = (read_bytes(mapped) or b"").decode("utf-8", errors="replace")
        head, tail = ref["head"], ref["tail"]
        head_candidates = [head]
        if head.endswith("\n"):
            head_candidates.append(head[:-1])
        tail_candidates = [tail]
        if tail.startswith("\n"):
            tail_candidates.append(tail[1:])
        if head and not any(text.startswith(cand) for cand in head_candidates if cand):
            problems.append(f"{ref['session_rel']}:{ref['line']}: spill 文件开头与裁剪前 head 不一致")
        if tail and not any(text.endswith(cand) for cand in tail_candidates if cand):
            problems.append(f"{ref['session_rel']}:{ref['line']}: spill 文件结尾与裁剪后 tail 不一致")
        name = mapped.name
        m = re.match(r"^(?P<label>.+)-(?P<hash>[0-9a-f]{16})\.txt$", name)
        if m and ref.get("tool_call_id") and m.group("label") != "tool-" + ref["tool_call_id"]:
            problems.append(
                f"{ref['session_rel']}:{ref['line']}: spill label {m.group('label')} 与 tool_call_id {ref['tool_call_id']} 不匹配"
            )
    if refs and not problems:
        c.ok(
            "spill.marker_refs",
            "裁剪标记中的 spill 路径有效且内容一致",
            group,
            f"{len(refs)} 处引用，全部存在且 head/tail/内容寻址一致",
            [c.rel(p) for p in sorted(referenced)][:8],
        )
    elif refs:
        c.fail(
            "spill.marker_refs",
            "裁剪标记中的 spill 路径有效且内容一致",
            group,
            f"{len(problems)} 处不一致",
            problems[:10],
        )
    elif files and (spilled_field_total >= len(files) or not compression_events):
        c.ok(
            "spill.marker_refs",
            "裁剪标记中的 spill 路径有效且内容一致",
            group,
            f"JSONL 中无 spill prune marker（消息已被压缩从活跃历史移除）；改用日志 spilled_msgs={spilled_field_total} 与 "
            f"{len(files)} 个 spill 文件的内容寻址完整性交叉验证",
            [f"spill.file_integrity 已验证 {len(files)} 个文件", c.rel(files[0])],
        )
    elif files:
        c.warn(
            "spill.marker_refs",
            "裁剪标记中的 spill 路径有效且内容一致",
            group,
            f"JSONL 无 marker，且日志 spilled_msgs={spilled_field_total} < spill 文件数 {len(files)}",
            [c.rel(p) for p in files[:4]],
        )
    else:
        c.skip("spill.marker_refs", "裁剪标记中的 spill 路径有效且内容一致", group, "无 spill 文件与 marker")

    all_referenced = {p.resolve() for p in referenced if p.exists()}
    unreferenced = [p for p in files if p.resolve() not in all_referenced]
    if not files:
        c.skip("spill.unreferenced", "无未引用的 spill 文件", group, "无 spill 文件")
    elif not refs:
        c.skip(
            "spill.unreferenced",
            "无未引用的 spill 文件",
            group,
            f"{len(unreferenced)} 个文件无 JSONL marker 引用；因压缩已丢弃 marker 消息，属预期（完整性另验）",
        )
    elif unreferenced:
        c.warn(
            "spill.unreferenced",
            "无未引用的 spill 文件",
            group,
            f"{len(unreferenced)} 个文件未被任何 marker 引用（可能是历史运行残留）",
            [c.rel(p) for p in unreferenced[:6]],
        )
    else:
        c.ok("spill.unreferenced", "无未引用的 spill 文件", group, f"{len(files)} 个文件全部被引用", severity="advisory")

    ctx.spill = {
        "root": c.rel(spill_root),
        "exists": True,
        "dir_mode": oct(perm) if perm is not None else None,
        "files": [c.rel(p) for p in files],
        "refs": [{"session": r["session_rel"], "line": r["line"], "path": r["path"]} for r in refs],
        "unreferenced": [c.rel(p) for p in unreferenced],
        "compression_spilled_msgs": spilled_field_total,
    }


# ---------------------------------------------------------------------------
# 5) marker 证据（AGENTS.md 编辑 / skill / report.md）
# ---------------------------------------------------------------------------


def _marker_evidence(ctx: VerifyContext, marker: str) -> dict:
    """在 session JSONL 中查找 marker 的各类证据。"""
    evidence = {"file": [], "delta_context": [], "tool_result": [], "assistant": [], "all_context": []}
    for analysis in ctx.sessions:
        for lineno, role, kind, content in analysis.texts:
            if marker not in content:
                continue
            item = f"{analysis.rel}:{lineno}"
            if role in ("user", "assistant", "tool"):
                evidence["all_context"].append(item)
            if role == "assistant":
                evidence["assistant"].append(item)
            elif role == "tool":
                evidence["tool_result"].append(item)
            elif role == "user" and kind == "workspace-instructions":
                obj = next((o for ln, o in analysis.active if ln == lineno), None)
                ctx_meta = msg_context(obj) if obj else {}
                if ctx_meta.get("baseline"):
                    evidence["file"].append(item)
                else:
                    evidence["delta_context"].append(item)
    return evidence


def _gateway_response_texts(events: list[dict]) -> list[tuple[str, int, str]]:
    """从 gateway 日志提取模型最终回复文本（`Response: ...` 行）。"""
    out = []
    for event in events:
        raw = event.get("raw") or ""
        text = event.get("text") or ""
        marker = "Response: "
        idx = text.find(marker)
        if idx < 0:
            continue
        tail = text[idx + len(marker):]
        # console 行：`... > Response: <text> agent_id=... field=...`
        cut = re.search(r"\s+(?:agent_id|final_length|session_key|iterations)=", tail)
        if cut:
            tail = tail[: cut.start()]
        out.append((event.get("file") or "gateway.log", event.get("line") or 0, tail.strip()))
    return out


def _gateway_marker_evidence(ctx: VerifyContext, marker: str) -> dict:
    responses, tool_args, other = [], [], []
    for event in getattr(ctx, "corpus", []):
        text = event.get("text") or ""
        if marker not in text:
            continue
        item = f"{event.get('file')}:{event.get('line')}"
        snippet = clip(text, 180)
        if "Response: " in text:
            responses.append(f"{item}: {snippet}")
        elif "tool call" in text.lower():
            tool_args.append(f"{item}: {snippet}")
        else:
            other.append(f"{item}: {snippet}")
    return {"response": responses, "tool_args": tool_args, "other": other}


def check_markers(ctx: VerifyContext):
    c = ctx.c()
    group = "markers"
    ws = ctx.workspace()
    exp_markers = ctx.exp["markers"] or {}

    # ---- AGENTS.md 编辑后的新标记进入 user-visible 历史/模型回复 ----
    agents_md = ws / "AGENTS.md"
    agents_text = read_text(agents_md) if agents_md.is_file() else None
    explicit = exp_markers.get("agents_md")
    if explicit:
        candidates = [explicit]
    elif agents_text:
        candidates = detect_markers(agents_text, exclude={"AGENTS.md", "AGENTS"})[:8]
        ctx.markers["agents_md_candidates"] = candidates
    else:
        candidates = []

    baseline_texts = []
    for analysis in ctx.sessions:
        for lineno, role, kind, content in analysis.texts:
            if role == "user" and kind == "workspace-instructions":
                obj = next((o for ln, o in analysis.active if ln == lineno), None)
                if obj is not None and msg_context(obj).get("baseline"):
                    baseline_texts.append(content)
    baseline_joined = "\n".join(baseline_texts)

    if agents_md.is_file():
        if agents_text is None:
            c.fail("markers.agents_file", "AGENTS.md 可读取", group, f"{c.rel(agents_md)} 无法读取")
        else:
            c.ok("markers.agents_file", "AGENTS.md 存在且可读", group, f"{c.rel(agents_md)}: {len(agents_text)} 字符", severity="required")
            if not candidates:
                c.warn(
                    "markers.agents_delta",
                    "AGENTS.md 编辑后的新标记进入后续 user-visible 历史/模型回复",
                    group,
                    "无法自动探测 marker 候选（可通过 expectations.markers.agents_md 指定）",
                )
            else:
                best = None
                for cand in candidates:
                    ev = _marker_evidence(ctx, cand)
                    gw = _gateway_marker_evidence(ctx, cand)
                    introduced = cand not in baseline_joined
                    score = (
                        4 * len(ev["delta_context"])
                        + 2 * len(ev["assistant"])
                        + 3 * len(gw["response"])
                        + len(ev["tool_result"])
                        + len(gw["tool_args"])
                    )
                    item = {
                        "marker": cand,
                        "score": score,
                        "ev": ev,
                        "gw": gw,
                        "in_file": cand in agents_text,
                        "introduced": introduced,
                    }
                    if best is None or (item["introduced"], item["score"], item["in_file"]) > (
                        best["introduced"],
                        best["score"],
                        best["in_file"],
                    ):
                        best = item
                marker = best["marker"]
                ev, gw = best["ev"], best["gw"]
                introduced = best["introduced"]
                ctx.markers["agents_md"] = marker
                ctx.markers["agents_md_introduced"] = introduced
                has_history = bool(ev["delta_context"])
                has_reply = bool(ev["assistant"]) or bool(gw["response"])
                has_tool = bool(ev["tool_result"]) or bool(gw["tool_args"])
                detail = (
                    f"marker={marker}（候选 {len(candidates)}）; delta_context={len(ev['delta_context'])}, "
                    f"模型回复证据={len(ev['assistant']) + len(gw['response'])}, 工具证据={len(ev['tool_result']) + len(gw['tool_args'])}"
                )
                evidence = [f"{c.rel(agents_md)}: 含 marker {marker}"] + [
                    f"{item}: 注入后的 workspace-instructions delta" for item in ev["delta_context"][:3]
                ] + [f"{item}: 模型回复引用 marker" for item in ev["assistant"][:2]] + [
                    f"{item}: gateway 日志模型回复含 marker" for item in gw["response"][:3]
                ] + [f"{item}: 工具调用参数含 marker" for item in (ev["tool_result"] + gw["tool_args"])[:2]]
                if explicit and marker not in agents_text:
                    c.fail(
                        "markers.agents_delta",
                        "AGENTS.md 编辑后的新标记进入后续 user-visible 历史/模型回复",
                        group,
                        f"expectations 指定 marker={marker}，但 AGENTS.md 不含它",
                        [c.rel(agents_md)],
                    )
                elif has_history and has_reply:
                    c.ok("markers.agents_delta", "AGENTS.md 编辑后的新标记进入后续 user-visible 历史/模型回复", group, detail, evidence)
                elif has_history or (has_reply and introduced):
                    c.ok(
                        "markers.agents_delta",
                        "AGENTS.md 编辑后的新标记进入后续 user-visible 历史/模型回复",
                        group,
                        detail
                        + ("（有注入历史；模型回复未复述）" if has_history and not has_reply else "（原始 JSONL 已压缩，模型回复/gateway 日志含新标记）"),
                        evidence,
                    )
                elif has_reply and not introduced:
                    c.warn(
                        "markers.agents_delta",
                        "AGENTS.md 编辑后的新标记进入后续 user-visible 历史/模型回复",
                        group,
                        detail + "：marker 出现在 baseline 与模型回复，但无法确认是运行期新引入（可能为初始文件内容）",
                        evidence,
                    )
                elif has_tool:
                    c.warn(
                        "markers.agents_delta",
                        "AGENTS.md 编辑后的新标记进入后续 user-visible 历史/模型回复",
                        group,
                        detail + "：仅工具结果/调用参数含 marker（本地写入证据，未见注入历史或回复）",
                        evidence,
                    )
                else:
                    c.fail(
                        "markers.agents_delta",
                        "AGENTS.md 编辑后的新标记进入后续 user-visible 历史/模型回复",
                        group,
                        detail + "：marker 未出现在任何模型可见历史/回复/工具证据",
                        evidence,
                    )
    else:
        c.skip("markers.agents_file", "AGENTS.md 存在且可读", group, f"缺失 {c.rel(agents_md)}（advisory）")
        c.skip("markers.agents_delta", "AGENTS.md 编辑后的新标记进入后续 user-visible 历史/模型回复", group, "无 AGENTS.md")

    # ---- CLAUDE.md / docs/CLAUDE.md 标记（advisory：文件 + 模型回复证据） ----
    for key, rel_path, check_id in (
        ("claude_md", "CLAUDE.md", "markers.claude_md"),
        ("docs_claude", "docs/CLAUDE.md", "markers.docs_claude"),
    ):
        marker_value = exp_markers.get(key)
        target = ws / rel_path
        text = read_text(target) if target.is_file() else None
        if marker_value is None and text:
            auto = detect_markers(text)
            marker_value = auto[0] if auto else None
            if auto:
                ctx.markers.setdefault("auto_detected", {})[key] = auto[:5]
        if marker_value is None:
            c.skip(check_id, f"{rel_path} 含预期标记", group, "无法确定 marker（expectations 未提供且自动探测失败）")
            continue
        if text is None:
            c.warn(check_id, f"{rel_path} 含预期标记", group, f"文件缺失: {c.rel(target)}")
            continue
        if marker_value not in text:
            c.warn(check_id, f"{rel_path} 含预期标记", group, f"{c.rel(target)} 不含 {marker_value}")
            continue
        gw = _gateway_marker_evidence(ctx, marker_value)
        session_hits = [
            f"{a.rel}:{ln}" for a in ctx.sessions for ln, role, kind, content in a.texts if marker_value in content
        ]
        if gw["response"] or session_hits:
            c.ok(
                check_id,
                f"{rel_path} 标记进入模型可见回复/历史",
                group,
                f"marker={marker_value}; gateway 回复证据={len(gw['response'])}, jsonl={len(session_hits)}",
                [f"{c.rel(target)}: 含 marker"] + gw["response"][:2] + [f"{item}: 会话上下文/回复含 marker" for item in session_hits[:2]],
                severity="advisory",
            )
        else:
            c.warn(check_id, f"{rel_path} 标记进入模型可见回复/历史", group, f"{c.rel(target)} 含 marker，但无回复/历史证据（可能已被压缩）")

    # ---- 技能标记 ----
    skill_md = None
    skills_dir = ws / "skills"
    if skills_dir.is_dir():
        for cand in sorted(skills_dir.glob("*/SKILL.md")):
            skill_md = cand
            break
    skill_marker = exp_markers.get("skill")
    skill_text = read_text(skill_md) if skill_md else None
    if skill_marker is None and skill_text:
        candidates = detect_markers(skill_text, exclude={skill_md.parent.name if skill_md else "SKILL"})
        skill_marker = candidates[0] if candidates else None
        ctx.markers["skill_candidates"] = candidates[:5]
    if skill_text is not None:
        c.ok("markers.skill_file", "技能 SKILL.md 存在且可读", group, f"{c.rel(skill_md)}: {len(skill_text)} 字符")
        if skill_marker:
            call_names = {}
            for analysis in ctx.sessions:
                for call in analysis.tool_calls:
                    call_names[call["id"]] = call["name"]
            tool_hits, assistant_hits = [], []
            for analysis in ctx.sessions:
                for res in analysis.tool_results:
                    if skill_marker in res["content"] and call_names.get(res["id"]) == "skill":
                        tool_hits.append(f"{analysis.rel}:{res['line']}")
                for msg in analysis.messages:
                    if msg["role"] == "assistant" and skill_marker in msg["content"]:
                        assistant_hits.append(f"{analysis.rel}:{msg['line']}")
            gw = _gateway_marker_evidence(ctx, skill_marker)
            evidence = [f"{c.rel(skill_md)}: 含 marker {skill_marker}"] + [
                f"{item}: skill 工具结果" for item in tool_hits[:3]
            ] + [f"{item}: 模型回复（JSONL）" for item in assistant_hits[:2]] + [
                f"{item}: gateway 日志模型回复" for item in gw["response"][:2]
            ]
            if tool_hits or assistant_hits or gw["response"]:
                c.ok(
                    "markers.skill_visible",
                    "skill 标记出现在 skill 工具结果或最终回复",
                    group,
                    f"marker={skill_marker}; tool_result={len(tool_hits)}, assistant={len(assistant_hits)}, gateway_response={len(gw['response'])}",
                    evidence,
                )
            else:
                c.fail(
                    "markers.skill_visible",
                    "skill 标记出现在 skill 工具结果或最终回复",
                    group,
                    f"marker={skill_marker} 未出现在任何 skill 工具结果或模型回复",
                    evidence,
                )
        else:
            c.warn("markers.skill_visible", "skill 标记出现在 skill 工具结果或最终回复", group, "无法自动探测 SKILL.md marker")
    else:
        c.skip("markers.skill_file", "技能 SKILL.md 存在且可读", group, "workspace/skills/*/SKILL.md 不存在")

    # ---- autonomy 第 2 步：AUTONOMY_NOTES.md 标记 + 外部信号内容 ----
    autonomy_marker = exp_markers.get("autonomy")
    notes_candidates = [ws / "project" / "AUTONOMY_NOTES.md", ws / "AUTONOMY_NOTES.md"]
    notes = next((p for p in notes_candidates if p.is_file()), notes_candidates[0])
    notes_text = read_text(notes) if notes.is_file() else None
    if notes_text is not None:
        if autonomy_marker is None:
            found = detect_markers(notes_text)
            autonomy_marker = found[0] if found else None
        if autonomy_marker and autonomy_marker in notes_text:
            wait_flag = read_text(ws / "WAIT_FLAG.txt")
            flag_content = ""
            if wait_flag is not None:
                flag_content = wait_flag.strip()
                flag_content = flag_content.splitlines()[0].strip() if flag_content else ""
            evidence = [f"{c.rel(notes)}: 含 {autonomy_marker}"]
            detail = f"marker={autonomy_marker}"
            if flag_content and flag_content in notes_text:
                evidence.append(f"{c.rel(ws / 'WAIT_FLAG.txt')}: 外部信号内容 {flag_content[:40]!r} 被写入 NOTES")
                detail += f"；外部信号内容 {flag_content[:40]!r} 已写入（P4.5）"
            elif flag_content:
                c.warn(
                    "markers.autonomy_notes",
                    "自主续跑第 2 步真实执行（NOTES 含标记与外部信号内容）",
                    group,
                    detail + f"；NOTES 未包含 WAIT_FLAG 内容 {flag_content[:40]!r}",
                    evidence,
                )
            autonomy_events = log_events_with(getattr(ctx, "corpus", []), "autonomy round scheduled")
            if autonomy_events:
                evidence.append(f"gateway 日志 autonomy round scheduled ×{len(autonomy_events)}")
            c.ok(
                "markers.autonomy_notes",
                "自主续跑第 2 步真实执行（NOTES 含标记与外部信号内容）",
                group,
                detail,
                evidence,
            )
        else:
            c.fail(
                "markers.autonomy_notes",
                "自主续跑第 2 步真实执行（NOTES 含标记与外部信号内容）",
                group,
                f"{c.rel(notes)} 不含 autonomy marker={autonomy_marker}",
                [c.rel(notes)],
            )
    else:
        c.skip("markers.autonomy_notes", "自主续跑第 2 步真实执行（NOTES 含标记与外部信号内容）", group, "无 AUTONOMY_NOTES.md")

    # ---- 报告（FIX_REPORT.md / report.md） ----
    report_candidates = [
        ws / "report.md",
        ws / "project" / "report.md",
        ws / "project" / "FIX_REPORT.md",
        ws / "FIX_REPORT.md",
    ]
    if ws.is_dir():
        for extra in sorted(ws.rglob("*REPORT*.md")) + sorted(ws.rglob("report*.md")):
            if extra not in report_candidates:
                report_candidates.append(extra)
    report = next((cand for cand in report_candidates if cand.is_file()), report_candidates[0])
    report_text = read_text(report) if report.is_file() else None
    ctx.markers["report_file"] = c.rel(report) if report.is_file() else None
    report_marker = exp_markers.get("report")
    if report_text is not None:
        if report_marker is None:
            candidates = detect_markers(report_text)
            report_marker = candidates[0] if candidates else ""
            ctx.markers["report_candidates"] = candidates[:5]
        if report_marker:
            if report_marker in report_text:
                c.ok(
                    "markers.report",
                    "修复报告存在且含指定标记",
                    group,
                    f"{c.rel(report)}: marker={report_marker}, {len(report_text)} 字符",
                )
            else:
                c.fail(
                    "markers.report",
                    "修复报告存在且含指定标记",
                    group,
                    f"{c.rel(report)} 缺少 marker={report_marker}",
                )
        elif report_text.strip():
            c.warn("markers.report", "修复报告存在且含指定标记", group, f"{c.rel(report)} 存在但无法自动探测 marker")
        else:
            c.fail("markers.report", "修复报告存在且含指定标记", group, f"{c.rel(report)} 为空")
        test_words = re.search(r"(测试|test|unittest|pytest|通过|pass)", report_text, re.I)
        c.ok(
            "markers.report_test_evidence",
            "修复报告记录了修复验证信息",
            group,
            f"{c.rel(report)}: {'含测试/通过字样' if test_words else '未提及测试'}",
            severity="advisory",
        )
    else:
        c.fail("markers.report", "修复报告存在且含指定标记", group, "未找到 report.md / project/FIX_REPORT.md")

    ctx.markers["skill"] = skill_marker
    ctx.markers["report"] = report_marker
    ctx.markers["autonomy"] = autonomy_marker


# ---------------------------------------------------------------------------
# 6) 配置哈希
# ---------------------------------------------------------------------------


def _iter_config_diffs(redacted, live, path="", out=None, depth=0):
    """递归比较 redacted 与 live config；返回差异列表（含路径与原因）。"""
    if out is None:
        out = []
    if depth > 12:
        return out
    if isinstance(live, dict) and isinstance(redacted, dict):
        for key in live:
            if key not in redacted:
                out.append(f"{path}/{key}: live 有但 redacted 缺（脱敏遗漏或版本不一致）")
                continue
            _iter_config_diffs(redacted[key], live[key], f"{path}/{key}", out, depth + 1)
        for key in redacted:
            if key not in live:
                out.append(f"{path}/{key}: redacted 有但 live 缺（redacted 可能是旧版本）")
    elif isinstance(live, list) and isinstance(redacted, list):
        if len(live) != len(redacted):
            out.append(f"{path}: 列表长度不一致 live={len(live)} redacted={len(redacted)}")
        else:
            for idx, (r_item, l_item) in enumerate(zip(redacted, live)):
                _iter_config_diffs(r_item, l_item, f"{path}[{idx}]", out, depth + 1)
    else:
        if redacted == live:
            return out
        is_secret_path = bool(SECRET_KEY_RE.search(path))
        redacted_str = str(redacted)
        looks_redacted = bool(re.search(r"(REDACTED|\*{3,}|YOUR_|redacted|placeholder|CHANGE_ME)", redacted_str, re.I))
        if is_secret_path or looks_redacted:
            return out
        out.append(f"{path}: live/redacted 值不一致且不像脱敏值（内容不打印）")
    return out


def check_config(ctx: VerifyContext):
    c = ctx.c()
    group = "config"
    root = ctx.artifacts
    redacted = root / "config.redacted.json"
    live_config = ctx.workspace() / "config.json"
    if redacted.is_file():
        text = read_text(redacted)
        try:
            json.loads(text or "")
            c.ok("config.redacted_json", "config.redacted.json 是合法 JSON", group, c.rel(redacted))
        except ValueError as exc:
            c.fail("config.redacted_json", "config.redacted.json 是合法 JSON", group, f"解析失败: {exc}", [c.rel(redacted)])
        leaks = []
        try:
            data = json.loads(text or "")
        except ValueError:
            data = None
        if isinstance(data, dict):
            def walk(node, path):
                if isinstance(node, dict):
                    for key, val in node.items():
                        if isinstance(val, str) and SECRET_KEY_RE.search(str(key)):
                            if len(val) >= 20 and not re.search(r"(YOUR_|xxx|\*{3,}|example|placeholder|redacted|CHANGE_ME|REDACTED)", val, re.I):
                                leaks.append(f"{path}/{key}: 可能是未脱敏的密钥（长度 {len(val)}）")
                        walk(val, f"{path}/{key}")
                elif isinstance(node, list):
                    for idx, val in enumerate(node):
                        walk(val, f"{path}[{idx}]")

            walk(data, "")
        if leaks:
            c.warn("config.no_secrets", "config.redacted.json 未泄漏密钥", group, ", ".join(leaks[:5]))
        else:
            c.ok("config.no_secrets", "config.redacted.json 未泄漏密钥", group, "未发现明显明文密钥", severity="advisory")
    else:
        c.fail("config.redacted_json", "config.redacted.json 存在", group, "缺失 config.redacted.json")

    # ---- 哈希记录 ----
    hash_files = {}
    for name in ("before", "after", "after-reload", "after_reload"):
        key = name.replace("after_reload", "after-reload")
        for cand in (
            root / f"config.hash.{name}",
            root / f"config.hash-{name}",
            root / f"config.{name}.sha256",
        ):
            if cand.is_file():
                hash_files[key] = cand
                break
    require_hash = ctx.exp["require"].get("config_hash")
    before = hash_files.get("before")
    after = hash_files.get("after")
    after_reload = hash_files.get("after-reload")
    before_digest = extract_hex_digest(read_text(before) or "") if before else []
    after_digest = extract_hex_digest(read_text(after) or "") if after else []
    reload_digest = extract_hex_digest(read_text(after_reload) or "") if after_reload else []

    if after_reload:
        c.ok(
            "config.reload_hash_present",
            "存在 /reload 计划变更的哈希记录",
            group,
            f"{c.rel(after_reload)}: sha256={reload_digest[0][:16] if reload_digest else '?'}…",
            severity="advisory",
        )
    if before and after:
        if not before_digest or not after_digest:
            c.fail(
                "config.hash_before_after",
                "配置哈希与运行记录一致（无计划外改写）",
                group,
                "hash 文件未包含可识别的十六进制摘要",
                [c.rel(before), c.rel(after)],
            )
        elif after_reload and reload_digest:
            # 场景内主动 /reload 改写了 config.json：after 必须等于计划变更后的哈希，
            # before 与 after 的差异由计划变更解释，不算“运行期被篡改”。
            if after_digest[0] == reload_digest[0]:
                c.ok(
                    "config.hash_before_after",
                    "配置哈希与运行记录一致（无计划外改写）",
                    group,
                    f"after == planned /reload hash（{after_digest[0][:16]}…）；before={before_digest[0][:16]}… 差异由计划内 /reload 解释",
                    [c.rel(before), c.rel(after), c.rel(after_reload)],
                )
            else:
                c.fail(
                    "config.hash_before_after",
                    "配置哈希与运行记录一致（无计划外改写）",
                    group,
                    f"after={after_digest[0][:16]}… ≠ planned /reload hash={reload_digest[0][:16]}…（运行期存在计划外配置改写）",
                    [c.rel(before), c.rel(after), c.rel(after_reload)],
                )
            if before_digest[0] == after_digest[0]:
                c.warn(
                    "config.reload_effective",
                    "/reload 计划变更确实生效",
                    group,
                    "before == after，但存在 /reload 记录：计划内配置变更可能未落盘",
                )
            else:
                c.ok(
                    "config.reload_effective",
                    "/reload 计划变更确实生效",
                    group,
                    "before != after 且 after == planned reload hash",
                    severity="advisory",
                )
        elif before_digest == after_digest:
            c.ok(
                "config.hash_before_after",
                "配置哈希与运行记录一致（无计划外改写）",
                group,
                f"sha256={before_digest[0][:16]}…（before == after，运行期未改写）",
                [c.rel(before), c.rel(after)],
            )
        else:
            c.fail(
                "config.hash_before_after",
                "配置哈希与运行记录一致（无计划外改写）",
                group,
                f"before={before_digest[0][:16]}… after={after_digest[0][:16]}… 不一致且无 /reload 计划变更记录",
                [c.rel(before), c.rel(after)],
            )
    elif before or after:
        c.skip("config.hash_before_after", "配置哈希与运行记录一致（无计划外改写）", group, f"仅提供 {sorted(hash_files)}，无法比较")
    elif require_hash is True:
        c.fail("config.hash_before_after", "配置哈希与运行记录一致（无计划外改写）", group, "缺少 config.hash.before/after")
    else:
        c.skip("config.hash_before_after", "配置哈希与运行记录一致（无计划外改写）", group, "未提供 before/after 哈希文件（advisory）")

    # ---- 记录哈希 ↔ artifacts 内 config 文件绑定 ----
    recorded = set()
    for digests in (before_digest, after_digest, reload_digest):
        recorded.update(digests)
    if recorded:
        bindings = []
        for candidate in (live_config, redacted):
            if candidate.is_file():
                digest = sha256_bytes(candidate.read_bytes())
                bindings.append((candidate, digest, digest in recorded))
        bound = [item for item in bindings if item[2]]
        if bound:
            c.ok(
                "config.hash_binds_artifacts",
                "记录的哈希与 artifacts 中的 config 文件对应",
                group,
                "; ".join(f"{c.rel(path)} sha256={digest[:16]}…" for path, digest, _ in bound),
                [c.rel(path) for path, _, _ in bound],
            )
        elif bindings:
            detail = "; ".join(
                f"{c.rel(path)} sha256={digest[:16]}… 与记录摘要不同" for path, digest, _ in bindings
            )
            if bool(ctx.exp.get("hash_binds_redacted_config")):
                c.fail(
                    "config.hash_binds_artifacts",
                    "记录的哈希与 artifacts 中的 config 文件对应",
                    group,
                    detail,
                    [c.rel(path) for path, _, _ in bindings],
                )
            else:
                c.warn(
                    "config.hash_binds_artifacts",
                    "记录的哈希与 artifacts 中的 config 文件对应",
                    group,
                    detail + "（若记录的是 artifacts 之外的原始 config，属正常）",
                    [c.rel(path) for path, _, _ in bindings],
                )
        else:
            c.skip("config.hash_binds_artifacts", "记录的哈希与 artifacts 中的 config 文件对应", group, "artifacts 内无 config.json/config.redacted.json")

    # ---- config.redacted.* 与 workspace/config.json 内容一致性（脱敏校验） ----
    # config.redacted.json 对应运行前初始配置；config.reload.redacted.json 对应 P7 计划内
    # /reload 后的终值。最终 config.json 应与后者的内容一致（仅密钥脱敏）。
    reload_redacted = root / "config.reload.redacted.json"
    reload_data = None
    if reload_redacted.is_file():
        try:
            reload_data = json.loads(reload_redacted.read_text(encoding="utf-8"))
        except ValueError:
            reload_data = None
    if reload_data is not None and live_config.is_file():
        try:
            live_data = json.loads(live_config.read_text(encoding="utf-8"))
        except ValueError:
            live_data = None
        c.ok(
            "config.reload_redacted_present",
            "存在 /reload 计划变更的脱敏配置",
            group,
            c.rel(reload_redacted),
            severity="advisory",
        )
        if isinstance(live_data, dict):
            diffs = _iter_config_diffs(reload_data, live_data)
            if diffs:
                c.warn(
                    "config.redacted_consistency",
                    "最终 workspace/config.json 与 config.reload.redacted.json 一致（仅密钥被脱敏）",
                    group,
                    f"{len(diffs)} 处非脱敏差异",
                    diffs[:6],
                )
            else:
                c.ok(
                    "config.redacted_consistency",
                    "最终 workspace/config.json 与 config.reload.redacted.json 一致（仅密钥被脱敏）",
                    group,
                    "除密钥字段外结构/值完全一致",
                )
        # 初始 redacted 与 reload redacted 的差异应仅限计划内改写的键
        if redacted.is_file():
            try:
                red_data = json.loads(redacted.read_text(encoding="utf-8"))
            except ValueError:
                red_data = None
            if isinstance(red_data, dict):
                planned_diffs = _iter_config_diffs(red_data, reload_data)
                planned_prefixes = (
                    "/agents/defaults/context_window",
                    "/agents/defaults/max_tokens",
                )
                unexpected = [d for d in planned_diffs if not d.startswith(planned_prefixes)]
                if unexpected:
                    c.warn(
                        "config.initial_redacted_planned_diff",
                        "初始/终态脱敏配置差异仅限 P7 计划内键",
                        group,
                        f"{len(unexpected)} 处计划外差异",
                        unexpected[:6],
                    )
                else:
                    c.ok(
                        "config.initial_redacted_planned_diff",
                        "初始/终态脱敏配置差异仅限 P7 计划内键",
                        group,
                        "差异: " + (", ".join(planned_diffs[:6]) if planned_diffs else "无"),
                        severity="advisory",
                    )
    elif redacted.is_file() and live_config.is_file():
        try:
            red_data = json.loads(redacted.read_text(encoding="utf-8"))
            live_data = json.loads(live_config.read_text(encoding="utf-8"))
        except ValueError:
            red_data = live_data = None
        if isinstance(red_data, dict) and isinstance(live_data, dict):
            diffs = _iter_config_diffs(red_data, live_data)
            if diffs:
                c.warn(
                    "config.redacted_consistency",
                    "config.redacted.json 与 workspace/config.json 内容一致（仅密钥被脱敏）",
                    group,
                    f"{len(diffs)} 处非脱敏差异",
                    diffs[:6],
                )
            else:
                c.ok(
                    "config.redacted_consistency",
                    "config.redacted.json 与 workspace/config.json 内容一致（仅密钥被脱敏）",
                    group,
                    "除密钥字段外结构/值完全一致",
                )
    else:
        c.warn(
            "config.redacted_consistency",
            "config.redacted.* 与 workspace/config.json 内容一致（仅密钥被脱敏）",
            group,
            "缺少对比文件",
        )

    # ---- artifacts 是否把真实明文配置带出（安全审计） ----
    if live_config.is_file():
        live_text = read_text(live_config) or ""
        leaked = []
        try:
            live_data = json.loads(live_text)
        except ValueError:
            live_data = None
        if isinstance(live_data, dict):
            def scan_secrets(node, path):
                if isinstance(node, dict):
                    for key, val in node.items():
                        if isinstance(val, str) and SECRET_KEY_RE.search(str(key)):
                            if len(val) >= 16 and not re.search(
                                r"(YOUR_|xxx|\*{3,}|REDACTED|redacted|example|placeholder|CHANGE_ME)", val, re.I
                            ):
                                # 安全：只报路径与长度，绝不打印密钥内容
                                leaked.append(f"{path}/{key}: 疑似真实密钥（长度 {len(val)}，内容不打印）")
                        scan_secrets(val, f"{path}/{key}")
                elif isinstance(node, list):
                    for idx, val in enumerate(node):
                        scan_secrets(val, f"{path}[{idx}]")

            scan_secrets(live_data, "")
        for m in re.finditer(r"[A-Za-z0-9_-]{40,}", live_text):
            token = m.group(0)
            if re.search(r"(YOUR_|xxx|REDACTED|redacted|placeholder|CHANGE_ME)", token, re.I):
                continue
            leaked.append(f"疑似长密钥/令牌（长度 {len(token)}，内容不打印）")
        if leaked:
            c.warn(
                "config.artifacts_no_secrets",
                "artifacts 未复制真实明文配置/密钥",
                group,
                f"{len(leaked)} 处疑似明文密钥（artifacts 对外分享前需脱敏）",
                leaked[:5],
            )
        else:
            c.ok("config.artifacts_no_secrets", "artifacts 未复制真实明文配置/密钥", group, "未发现明显明文密钥", severity="advisory")

    ctx.config = {
        "redacted": c.rel(redacted) if redacted.is_file() else None,
        "live_config": c.rel(live_config) if live_config.is_file() else None,
        "hash_files": {k: c.rel(v) for k, v in hash_files.items()},
        "before": before_digest[0] if before_digest else None,
        "after": after_digest[0] if after_digest else None,
        "after_reload": reload_digest[0] if reload_digest else None,
    }


# ---------------------------------------------------------------------------
# 7) gateway 日志
# ---------------------------------------------------------------------------


def check_logs(ctx: VerifyContext):
    c = ctx.c()
    group = "gateway"
    log_order = ["gateway.log", "gateway-restart.log"]
    log_paths = {}
    for name in log_order:
        path = ctx.artifacts / name
        if path.is_file():
            log_paths[name] = path
        else:
            ctx.logs[name] = []
    for name, path in log_paths.items():
        ctx.logs[name] = parse_log_file(path)
    ctx.corpus = [event for events in ctx.logs.values() for event in events]

    if not ctx.logs.get("gateway.log"):
        c.fail("log.gateway_log", "gateway.log 可解析", group, "缺失或为空")
        return
    c.ok("log.gateway_log", "gateway.log 可解析", group, f"{len(ctx.logs['gateway.log'])} 条日志事件")

    # 7.1 启动证据（每个 run 日志都应含启动行）
    for name in ctx.logs:
        events = ctx.logs[name]
        startups = [e for e in events if any(k in (e.get("text") or "").lower() for k in ("agent initialized", "log level set to", "gateway started", "config hot reload enabled"))]
        if startups:
            c.ok(
                f"log.startup.{name}",
                f"{name} 含启动日志",
                group,
                f"{len(startups)} 条启动事件",
                severity="required" if name == "gateway.log" else "advisory",
            )
        else:
            c.fail(
                f"log.startup.{name}",
                f"{name} 含启动日志",
                group,
                "未发现启动事件",
                severity="required" if name == "gateway.log" else "advisory",
            )

    # 7.2 Tool call 执行日志（与 JSONL 已执行调用数交叉核对，并验证重复调用证据）
    executed = sum(len(a.tool_calls) for a in ctx.sessions)
    singles = log_count(ctx.corpus, "tool call: ")
    batches = log_count(ctx.corpus, "tool call (batch)")
    hooks = log_count(ctx.corpus, "tool call (hook respond)")
    total_tool_lines = singles + batches + hooks
    min_tool = int(ctx.exp["min_counts"].get("tool_call_logs") or 1)
    configured_levels = set()
    for event in ctx.corpus:
        m = re.search(r"log level set to \"?([a-z]+)\"?", (event.get("text") or "").lower())
        if m:
            configured_levels.add(m.group(1))
    low_level_logging = bool(configured_levels & {"warn", "error", "fatal"})
    if total_tool_lines < min_tool and low_level_logging:
        c.warn(
            "log.tool_calls",
            "gateway 日志包含 Tool call / Tool call (batch)",
            group,
            f"日志级别被配置为 {sorted(configured_levels)}，INFO 级工具日志可能被过滤（发现 {total_tool_lines} 条）",
            [c.ev(log_paths.get("gateway.log", ctx.artifacts / "gateway.log"))],
        )
    elif total_tool_lines < min_tool:
        c.fail(
            "log.tool_calls",
            "gateway 日志包含 Tool call / Tool call (batch)",
            group,
            f"发现 {total_tool_lines} 条（要求 >= {min_tool}）",
            [c.ev(log_paths.get("gateway.log", ctx.artifacts / "gateway.log"))],
        )
    elif executed and total_tool_lines < executed:
        c.warn(
            "log.tool_calls",
            "gateway 日志包含 Tool call / Tool call (batch)",
            group,
            f"日志执行记录 {total_tool_lines} 条 < JSONL 保留的 tool_call 数 {executed}（可能日志级别或截断）",
        )
    else:
        c.ok(
            "log.tool_calls",
            "gateway 日志包含 Tool call / Tool call (batch)",
            group,
            f"单条 {singles} + batch {batches} + hook {hooks} = {total_tool_lines}",
            [c.ev(log_paths.get("gateway.log", ctx.artifacts / "gateway.log"))],
        )
    min_batch = int(ctx.exp["min_counts"].get("tool_call_batch_logs") or 0)
    multi_call_msgs = [
        (a.rel, msg["line"], len(tool_calls_of(msg["obj"])))
        for a in ctx.sessions
        for msg in a.messages
        if msg["role"] == "assistant" and len(tool_calls_of(msg["obj"])) >= 2
    ]
    if batches or multi_call_msgs or min_batch:
        if batches >= max(1, min_batch):
            c.ok(
                "log.tool_call_batch",
                "批量工具调用有 Tool call (batch) 日志",
                group,
                f"batch 日志 {batches} 条"
                + ("（JSONL 已压缩，批量调用证据来自日志）" if not multi_call_msgs else f"；JSONL 多调用消息 {len(multi_call_msgs)} 条"),
                [c.ev(log_paths.get("gateway.log", ctx.artifacts / "gateway.log"), e["line"]) for e in log_events_with(ctx.corpus, "tool call (batch)")[:2]],
            )
        elif multi_call_msgs:
            c.fail(
                "log.tool_call_batch",
                "批量工具调用有 Tool call (batch) 日志",
                group,
                f"JSONL 有 {len(multi_call_msgs)} 条多 tool_call 助手消息，但 batch 日志为 0",
                [f"{rel}:{line}: {count} tool_calls" for rel, line, count in multi_call_msgs[:4]],
            )
        else:
            c.warn(
                "log.tool_call_batch",
                "批量工具调用有 Tool call (batch) 日志",
                group,
                f"期望 batch 日志 >= {min_batch}，实际 {batches}",
            )
    else:
        c.skip("log.tool_call_batch", "批量工具调用有 Tool call (batch) 日志", group, "无批量调用预期")

    repeat_tool = "list_dir" if "list_dir" in (ctx.results_text or "").lower() else None
    if repeat_tool:
        hits = [e for e in log_events_with(ctx.corpus, "tool call") if repeat_tool in (e.get("text") or "")]
        if len(hits) >= 3:
            c.ok(
                "log.repeat_tool_calls",
                f"重复工具调用证据（{repeat_tool} ≥3 次真实执行）",
                group,
                f"{len(hits)} 次（loop-guard 触发前提条件成立）",
                [c.ev(log_paths.get("gateway.log", ctx.artifacts / "gateway.log"), e["line"]) for e in hits[:4]],
            )
        else:
            c.fail(
                "log.repeat_tool_calls",
                f"重复工具调用证据（{repeat_tool} ≥3 次真实执行）",
                group,
                f"仅 {len(hits)} 次，不支持 loop-guard 触发声明",
            )

    # 7.3 自主轮次调度（autonomy round scheduled）日志
    task_round_count = sum(a.kinds.get("task-round", {}).get("count", 0) for a in ctx.sessions)
    autonomy_events = log_events_with(ctx.corpus, "autonomy round scheduled")
    min_autonomy = int(ctx.exp["min_counts"].get("autonomy_round_scheduled") or 0)
    has_task_evidence = bool(
        ctx.primary
        and isinstance(ctx.primary.meta, dict)
        and isinstance(ctx.primary.meta.get("task_state"), dict)
    )
    expected_autonomy = max(min_autonomy, 1 if (task_round_count or has_task_evidence) else 0)
    if expected_autonomy:
        if autonomy_events:
            rounds = log_int_field(autonomy_events, "round")
            c.ok(
                "log.autonomy_rounds",
                "gateway 日志包含 autonomy round scheduled",
                group,
                f"{len(autonomy_events)} 条；round 字段 {rounds[:8]}；JSONL task-round={task_round_count}",
                [c.ev(log_paths.get("gateway.log", ctx.artifacts / "gateway.log"), e["line"]) for e in autonomy_events[:3]],
            )
            c.ok(
                "log.autonomy_rounds_match",
                "autonomy 日志次数与任务状态一致",
                group,
                f"日志 {len(autonomy_events)} 条；最终 task_state round={(ctx.primary.meta.get('task_state') or {}).get('round') if has_task_evidence else 'n/a'}",
            )
        else:
            c.fail(
                "log.autonomy_rounds",
                "gateway 日志包含 autonomy round scheduled",
                group,
                "未发现 autonomy round scheduled",
                [c.ev(log_paths.get("gateway.log", ctx.artifacts / "gateway.log"))],
            )
    else:
        c.skip("log.autonomy_rounds", "gateway 日志包含 autonomy round scheduled", group, "无自主续跑预期")

    # 7.4 压缩 / spill（字段级核对）
    forced_events = log_events_with(ctx.corpus, "forced compression")
    proactive_events = log_events_with(ctx.corpus, "proactive compression")
    compression_events = forced_events + proactive_events
    refs = [ref for a in ctx.sessions for ref in a.prune_refs]
    spill_files = [p for p in (ctx.workspace() / ".chagent" / "spill").rglob("*") if p.is_file()] if (ctx.workspace() / ".chagent" / "spill").is_dir() else []
    min_compression = int(ctx.exp["min_counts"].get("forced_compression_pruned") or 0)
    expect_compression = bool(compression_events) or bool(spill_files) or bool(refs) or min_compression > 0
    if expect_compression:
        if compression_events:
            pruned_values = log_int_field(forced_events, "pruned_msgs")
            spilled_values = log_int_field(forced_events, "spilled_msgs")
            saved_values = log_int_field(forced_events, "saved_chars")
            c.ok(
                "log.forced_compression",
                "gateway 日志包含压缩/裁剪记录",
                group,
                f"forced={len(forced_events)}, proactive={len(proactive_events)}；pruned_msgs={pruned_values[:4]} "
                f"spilled_msgs={spilled_values[:4]} saved_chars={saved_values[:4]}",
                [c.ev(log_paths.get("gateway.log", ctx.artifacts / "gateway.log"), e["line"]) for e in compression_events[:3]],
            )
            field_problems = []
            if forced_events:
                if not pruned_values:
                    field_problems.append("Forced compression 日志缺少 pruned_msgs 字段")
                if spill_files and not spilled_values:
                    field_problems.append("存在 spill 文件，但 Forced compression 日志缺少 spilled_msgs 字段")
                if not saved_values:
                    field_problems.append("Forced compression 日志缺少 saved_chars 字段")
                if spilled_values and sum(spilled_values) < len(spill_files) and not refs:
                    field_problems.append(
                        f"日志 spilled_msgs={sum(spilled_values)} < spill 文件数 {len(spill_files)}"
                    )
            if field_problems:
                c.fail(
                    "log.compression_fields",
                    "压缩日志字段 pruned_msgs/spilled_msgs/saved_chars 可解析且自洽",
                    group,
                    f"{len(field_problems)} 处问题",
                    field_problems,
                )
            else:
                c.ok(
                    "log.compression_fields",
                    "压缩日志字段 pruned_msgs/spilled_msgs/saved_chars 可解析且自洽",
                    group,
                    f"pruned_msgs={pruned_values}, spilled_msgs={spilled_values}, saved_chars={saved_values}, "
                    f"spill 文件={len(spill_files)}, JSONL marker={len(refs)}",
                )
        else:
            c.fail(
                "log.forced_compression",
                "gateway 日志包含压缩/裁剪记录",
                group,
                f"未发现压缩日志，但存在 spill 文件 {len(spill_files)} 个 / JSONL marker {len(refs)} 处",
            )
    else:
        c.skip("log.forced_compression", "gateway 日志包含压缩/裁剪记录", group, "无压缩/spill 预期")

    spill_events = log_events_with(ctx.corpus, "spill")
    min_spill = int(ctx.exp["min_counts"].get("spill_events") or 1)
    if spill_files or refs:
        if spill_events:
            c.ok(
                "log.spill_keyword",
                "gateway 日志包含 spill 关键字",
                group,
                f"{len(spill_events)} 条 spill 相关日志",
                [c.ev(log_paths.get("gateway.log", ctx.artifacts / "gateway.log"), e["line"]) for e in spill_events[:3]],
            )
        else:
            c.fail(
                "log.spill_keyword",
                "gateway 日志包含 spill 关键字",
                group,
                f"存在 spill 文件 {len(spill_files)} 个，但日志无 spill 关键字（要求 >= {min_spill}）",
            )
        failed_spill = log_events_with(ctx.corpus, "spill failed")
        if failed_spill:
            c.warn(
                "log.spill_failure",
                "无 tool result spill 失败日志",
                group,
                f"{len(failed_spill)} 条 spill 失败日志",
                [c.ev(log_paths.get("gateway.log", ctx.artifacts / "gateway.log"), e["line"]) for e in failed_spill[:3]],
            )
        else:
            c.ok("log.spill_failure", "无 tool result spill 失败日志", group, "无失败日志", severity="advisory")
    else:
        c.skip("log.spill_keyword", "gateway 日志包含 spill 关键字", group, "无 spill 文件与引用")

    # 7.5 关闭语义：final run 必须优雅完成；first run 单独检查（P9.1/P9.2 已知缺陷证据）
    present_logs = [name for name in log_order if ctx.logs.get(name)]
    final_name = present_logs[-1] if present_logs else "gateway.log"

    def shutdown_evidence(name):
        events = ctx.logs.get(name) or []
        sh = [e for e in events if "shutting down" in (e.get("text") or "").lower()]
        stopped = [e for e in events if "gateway stopped" in (e.get("text") or "").lower()]
        removed = [e for e in events if "remove pid file" in (e.get("text") or "").lower()]
        return sh, stopped, removed

    final_sh, final_stopped, final_removed = shutdown_evidence(final_name)
    if final_sh:
        c.ok(
            "log.shutdown_signal",
            "SIGTERM 后进入关闭流程（Shutting down...）",
            group,
            f"{final_name}: {len(final_sh)} 条关闭信号",
            [c.ev(log_paths[final_name], final_sh[0]["line"])],
        )
    else:
        c.fail("log.shutdown_signal", "SIGTERM 后进入关闭流程（Shutting down...）", group, f"{final_name} 无 Shutting down 日志")
    if final_stopped:
        c.ok(
            "log.shutdown_complete",
            "SIGTERM 后优雅退出（✓ Gateway stopped）",
            group,
            f"{final_name}: {len(final_stopped)} 条完成日志",
            [c.ev(log_paths[final_name], final_stopped[0]["line"])],
        )
    else:
        c.fail(
            "log.shutdown_complete",
            "SIGTERM 后优雅退出（✓ Gateway stopped）",
            group,
            f"{final_name} 无 ✓ Gateway stopped（关闭可能挂起/被 kill）",
            [c.ev(log_paths[final_name])],
        )
    if final_removed:
        c.ok(
            "log.pid_cleanup_final",
            "最终关闭删除 pid 文件（remove pid file）",
            group,
            f"{final_name}: {len(final_removed)} 条删除记录",
            [c.ev(log_paths[final_name], final_removed[-1]["line"])],
        )
    else:
        c.fail(
            "log.pid_cleanup_final",
            "最终关闭删除 pid 文件（remove pid file）",
            group,
            f"{final_name} 无 remove pid file 日志",
        )

    # first run（P9 第一次 SIGTERM）：P9.1/P9.2 已知缺陷的原始证据
    first_name = "gateway.log"
    first_sh, first_stopped, first_removed = shutdown_evidence(first_name)
    if first_sh:
        missing = []
        if not first_stopped:
            missing.append("无 ✓ Gateway stopped（关闭未完成，进程被 kill）")
        if not first_removed:
            missing.append("无 remove pid file（pid 文件残留）")
        if missing:
            c.fail(
                "log.shutdown_first_run",
                "第一次 SIGTERM 优雅完成关闭（✓ Gateway stopped）",
                group,
                f"{first_name}: " + "；".join(missing),
                [c.ev(log_paths[first_name], first_sh[0]["line"]), c.ev(log_paths[first_name])],
            )
        else:
            c.ok(
                "log.shutdown_first_run",
                "第一次 SIGTERM 优雅完成关闭（✓ Gateway stopped）",
                group,
                f"{first_name}: 关闭完成",
                [c.ev(log_paths[first_name], first_stopped[0]["line"])],
            )
        if not first_removed:
            c.fail(
                "log.pid_cleanup_first_run",
                "第一次 SIGTERM 后 pid 文件被删除",
                group,
                f"{first_name}: Shutting down 之后无 remove pid file（pid 残留，P9.2 证据）",
                [c.ev(log_paths[first_name])],
            )
        else:
            c.ok(
                "log.pid_cleanup_first_run",
                "第一次 SIGTERM 后 pid 文件被删除",
                group,
                f"{first_name}: pid 已删除",
            )
    else:
        c.skip("log.shutdown_first_run", "第一次 SIGTERM 优雅完成关闭（✓ Gateway stopped）", group, f"{first_name} 无关闭记录")
        c.skip("log.pid_cleanup_first_run", "第一次 SIGTERM 后 pid 文件被删除", group, f"{first_name} 无关闭记录")

    # 重启证据
    if "gateway-restart.log" in log_paths:
        if ctx.logs.get("gateway-restart.log"):
            c.ok(
                "log.restart",
                "重启日志存在且是新的启动",
                group,
                f"{len(ctx.logs['gateway-restart.log'])} 条事件",
                [c.ev(log_paths["gateway-restart.log"])],
                severity="required" if ctx.exp["require"].get("restart") is True else "advisory",
            )
        else:
            c.warn("log.restart", "重启日志存在且是新的启动", group, "gateway-restart.log 为空")

    # 7.6 无残留进程 / pid 文件已删除（最终状态）
    pid_candidates = set()
    for event in ctx.corpus:
        text = (event.get("text") or "") + " " + (event.get("raw") or "")
        for m in re.finditer(r"\(PID:\s*(\d+)\)", text):
            pid_candidates.add(int(m.group(1)))
        for m in re.finditer(r"(?:gateway\s+pid|pid)\s*[=:]\s*(\d{2,7})", text, re.I):
            pid_candidates.add(int(m.group(1)))
    for m in re.finditer(r"(?:gateway\s+pid|pid)\s*[=:]\s*(\d{2,7})", ctx.harness_text, re.I):
        pid_candidates.add(int(m.group(1)))
    residual, checked = [], []
    for pid in sorted(pid_candidates):
        if pid <= 1 or pid == os.getpid():
            continue
        alive, cmd = process_alive(pid)
        checked.append({"pid": pid, "alive": alive, "cmd": clip(cmd, 120)})
        if alive and re.search(r"chagent|chagent", cmd, re.I):
            residual.append(f"PID {pid} 仍在运行: {cmd}")
    pid_file = ctx.workspace() / ".chagent.pid"
    pid_tmp = ctx.workspace() / ".chagent.pid.tmp"
    pid_problems = []
    if pid_file.exists():
        pid_problems.append(f"残留 pid 文件: {c.rel(pid_file)}")
    if pid_tmp.exists():
        pid_problems.append(f"残留临时 pid 文件: {c.rel(pid_tmp)}")
    pid_problems.extend(residual)
    if pid_problems:
        c.fail("log.no_residual", "SIGTERM 后无残留进程 / pid 文件已删除", group, f"{len(pid_problems)} 处残留", pid_problems[:6])
    else:
        c.ok(
            "log.no_residual",
            "SIGTERM 后无残留进程 / pid 文件已删除",
            group,
            f"pid 文件不存在；检查 {len(checked)} 个日志 PID，均非存活的 chagent 进程",
        )

    # 7.7 error / fatal / panic（区分 E2E 场景里“预期被拒绝/预期失败复现”的 error）
    default_expected_errors = [
        r"Tool execution failed error=\"STDERR",              # P3 复现失败测试
        r"path escapes workspace",                             # P3.7 越界读被拒
        r"file not found: openat WAIT_FLAG\.txt",             # P4 自主轮轮询等待外部信号
        r"file not found: openat STOP_FLAG\.txt",             # P8/P10 STOP_E2E 等待外部信号（计划内）
        r"任务已完成，不能更新任务清单",                       # todo_write 守卫
        r"gateway is already running",                         # P0.8 单例保护
        r"未知技能|unknown skill",                              # 负例
    ]
    expected_patterns = list(ctx.exp.get("expected_error_patterns") or default_expected_errors)
    error_events = [e for e in ctx.corpus if e.get("level") in ("error", "fatal", "panic")]
    panic_events = [e for e in error_events if e.get("level") in ("fatal", "panic") or "panic" in (e.get("text") or "").lower()]
    unexpected_errors, expected_errors = [], []
    for event in error_events:
        text = event.get("text") or ""
        if any(re.search(pattern, text, re.I) for pattern in expected_patterns):
            expected_errors.append(event)
        else:
            unexpected_errors.append(event)
    if panic_events:
        c.fail(
            "log.errors",
            "gateway 日志无 fatal/panic",
            group,
            f"{len(panic_events)} 条 fatal/panic 日志",
            [c.ev(log_paths[e["file"]], e["line"], e["message"]) for e in panic_events[:5]],
        )
    elif unexpected_errors:
        c.warn(
            "log.errors",
            "gateway 日志无计划外 error/fatal",
            group,
            f"{len(unexpected_errors)} 条计划外 error 日志（需人工确认）",
            [c.ev(log_paths[e["file"]], e["line"], e["message"]) for e in unexpected_errors[:5]],
        )
    else:
        c.ok("log.errors", "gateway 日志无计划外 error/fatal", group, "无计划外 error/fatal 事件", severity="advisory")
    if expected_errors:
        c.ok(
            "log.expected_errors",
            "功能测试中预期被拒绝/失败复现的 error 均已识别",
            group,
            f"{len(expected_errors)} 条预期 error（测试复现/权限拒绝/信号轮询/守卫拒绝），{len(unexpected_errors)} 条计划外",
            [c.ev(log_paths[e["file"]], e["line"], clip(e["message"], 120)) for e in expected_errors[:4]],
            severity="advisory",
        )
    else:
        c.skip("log.expected_errors", "功能测试中预期被拒绝/失败复现的 error 均已识别", group, "无 error 事件")

    ctx.logs_summary = {
        "files": {name: len(events) for name, events in ctx.logs.items()},
        "tool_call_lines": total_tool_lines,
        "batch_lines": batches,
        "autonomy_events": len(autonomy_events),
        "forced_compression_events": len(forced_events),
        "proactive_compression_events": len(proactive_events),
        "spill_events": len(spill_events),
        "checked_pids": checked[:10],
    }


# ---------------------------------------------------------------------------
# 8) 项目修复复跑（独立 python）
# ---------------------------------------------------------------------------


def detect_project_test_commands(project: Path) -> list[list[str]]:
    commands = []
    test_files = [
        p
        for p in project.rglob("*.py")
        if re.match(r"^test_.*\.py$|.*_test\.py$", p.name)
    ]
    # 解释器候选：系统 python3 → 项目自带 venv（Lead harness 可能用 venv 安装 pytest）
    pythons = [sys.executable]
    for venv_python in (project / ".venv" / "bin" / "python", project / "venv" / "bin" / "python"):
        if venv_python.is_file():
            pythons.append(str(venv_python))
    for python in pythons:
        pytest_available = False
        try:
            pytest_available = subprocess.run(
                [python, "-c", "import pytest"],
                capture_output=True,
                timeout=20,
            ).returncode == 0
        except (OSError, subprocess.SubprocessError):
            pytest_available = False
        if test_files and pytest_available:
            commands.append([python, "-m", "pytest", "-q"])
        if (project / "tests").is_dir():
            commands.append([python, "-m", "unittest", "discover", "-s", "tests", "-t", ".", "-v"])
        commands.append([python, "-m", "unittest", "discover", "-s", ".", "-p", "test*.py", "-v"])
        for test_file in test_files[:6]:
            commands.append([python, str(test_file)])
    if (project / "go.mod").is_file():
        commands.append(["go", "test", "./..."])
    # 去重
    unique = []
    for cmd in commands:
        if cmd not in unique:
            unique.append(cmd)
    return unique


def parse_test_output(cmd: list[str], rc: int, output: str) -> dict:
    ran = 0
    ok = False
    joined = output or ""
    m = re.search(r"Ran\s+(\d+)\s+tests?", joined)
    if m:
        ran = int(m.group(1))
        ok = rc == 0 and re.search(r"^OK", joined, re.M) is not None
    m = re.search(r"(\d+)\s+passed", joined)
    if m:
        ran = max(ran, int(m.group(1)))
        ok = rc == 0 and "failed" not in joined.lower()
    if "go test" in " ".join(cmd):
        ok_go = len(re.findall(r"^ok\s+", joined, re.M))
        fail_go = len(re.findall(r"^FAIL", joined, re.M))
        ran = max(ran, ok_go)
        ok = rc == 0 and fail_go == 0 and ok_go > 0
    return {"ran": ran, "ok": ok}


def run_project_tests(ctx: VerifyContext):
    c = ctx.c()
    group = "project"
    ws = ctx.workspace()
    project = ws / "project"
    if not project.is_dir():
        c.fail("project.dir", "workspace/project 存在", group, f"缺失 {c.rel(project)}")
        return
    c.ok("project.dir", "workspace/project 存在", group, c.rel(project))

    # report.md 在 markers 组已检查；这里核对修复证据（advisory）
    buggy_patterns = ctx.exp.get("project_buggy_patterns") or []
    if buggy_patterns:
        hits = []
        for path in iter_project_python_files(project):
            text = read_text(path) or ""
            for pattern in buggy_patterns:
                for m in re.finditer(pattern, text):
                    hits.append(f"{c.rel(path)}:{text[:m.start()].count(chr(10)) + 1}: 命中 {clip(pattern, 40)}")
        if hits:
            c.fail("project.buggy_patterns", "buggy 代码特征已消失", group, f"{len(hits)} 处命中", hits[:8])
        else:
            c.ok("project.buggy_patterns", "buggy 代码特征已消失", group, "无命中")

    cmd = ctx.exp.get("project_test_cmd")
    attempts = []
    passed_attempt = None
    if cmd:
        attempts.append({"cmd": list(cmd), "cwd": str(project)})
    else:
        for candidate in detect_project_test_commands(project):
            attempts.append({"cmd": candidate, "cwd": str(project)})

    for attempt in attempts:
        try:
            proc = subprocess.run(
                attempt["cmd"],
                cwd=attempt["cwd"],
                capture_output=True,
                text=True,
                timeout=ctx.timeout,
                env={**os.environ, "PYTHONDONTWRITEBYTECODE": "1"},
            )
            output = (proc.stdout or "") + (proc.stderr or "")
            parsed = parse_test_output(attempt["cmd"], proc.returncode, output)
            attempt.update({"rc": proc.returncode, "output": output, "ran": parsed["ran"], "ok": parsed["ok"]})
        except subprocess.TimeoutExpired:
            attempt.update({"rc": -1, "output": f"timeout after {ctx.timeout}s", "ran": 0, "ok": False})
        except OSError as exc:
            attempt.update({"rc": -1, "output": f"无法执行: {exc}", "ran": 0, "ok": False})
        if attempt.get("ok") and attempt.get("ran", 0) > 0:
            passed_attempt = attempt
            break

    ctx.project = {"dir": c.rel(project), "attempts": attempts}

    if passed_attempt:
        evidence = [f"cmd: {' '.join(passed_attempt['cmd'])}（cwd={c.rel(project)}）"]
        evidence.extend((passed_attempt.get("output") or "").strip().splitlines()[-12:])
        c.ok(
            "project.tests_pass",
            "独立复跑项目测试通过（buggy 代码已修）",
            group,
            f"命令 {' '.join(passed_attempt['cmd'])}：{passed_attempt['ran']} 个测试通过",
            evidence,
        )
    else:
        ran_some = [a for a in attempts if a.get("ran", 0) > 0]
        if ran_some:
            best = ran_some[0]
            evidence = [f"cmd: {' '.join(best['cmd'])}（cwd={c.rel(project)}）rc={best.get('rc')}"]
            evidence.extend((best.get("output") or "").strip().splitlines()[-20:])
            c.fail("project.tests_pass", "独立复跑项目测试通过（buggy 代码已修）", group, f"{len(ran_some)} 次尝试运行了测试但未通过", evidence)
        else:
            evidence = []
            for attempt in attempts[:4]:
                evidence.append(f"cmd: {' '.join(attempt['cmd'])} rc={attempt.get('rc')}")
                evidence.extend((attempt.get("output") or "").strip().splitlines()[-4:])
            c.fail(
                "project.tests_pass",
                "独立复跑项目测试通过（buggy 代码已修）",
                group,
                "未能发现/运行任何项目测试（无法验证修复）",
                evidence[:12],
            )

    # 独立行为断言：直接调用项目 API 验证 bug 已修（不依赖项目自带测试的通过状态）
    behavior_snippet = ctx.exp.get("project_behavior_check") or (
        "from src.stats import mean, median, count_levels\n"
        "assert mean([1,2,3]) == 2.0, mean([1,2,3])\n"
        "assert mean([5]) == 5.0, mean([5])\n"
        "assert median([1,2,3,4]) == 2.5, median([1,2,3,4])\n"
        "assert count_levels(['INFO a','WARN b','ERROR c','INFO d']) == {'INFO':2,'WARN':1,'ERROR':1}\n"
        "print('BEHAVIOR_OK')\n"
    )
    if (project / "src" / "stats.py").is_file() or (project / "calc.py").is_file():
        try:
            proc = subprocess.run(
                [sys.executable, "-c", behavior_snippet],
                cwd=str(project),
                capture_output=True,
                text=True,
                timeout=60,
                env={**os.environ, "PYTHONDONTWRITEBYTECODE": "1"},
            )
            output = ((proc.stdout or "") + (proc.stderr or "")).strip()
            if proc.returncode == 0 and "BEHAVIOR_OK" in output:
                c.ok(
                    "project.behavior_fix",
                    "独立行为断言确认 bug 已修",
                    group,
                    "mean/median/count_levels 行为符合预期",
                    [f"cmd: {sys.executable} -c <behavior snippet> (cwd={c.rel(project)})"] + output.splitlines()[-3:],
                )
            else:
                c.fail(
                    "project.behavior_fix",
                    "独立行为断言确认 bug 已修",
                    group,
                    f"独立行为断言失败 rc={proc.returncode}",
                    output.splitlines()[-8:],
                )
        except (OSError, subprocess.SubprocessError) as exc:
            c.fail("project.behavior_fix", "独立行为断言确认 bug 已修", group, f"无法执行: {exc}")

    for extra in ctx.exp.get("project_extra_checks") or []:
        cmd_list = extra.get("cmd")
        if not cmd_list:
            continue
        try:
            proc = subprocess.run(
                cmd_list,
                cwd=str(project / extra["cwd"]) if extra.get("cwd") else str(project),
                capture_output=True,
                text=True,
                timeout=ctx.timeout,
            )
            output = (proc.stdout or "") + (proc.stderr or "")
            expect = extra.get("expect_contains", "")
            ok = proc.returncode == 0 and (not expect or expect in output)
            if ok:
                c.ok(f"project.extra.{len(ctx.checker.checks)}", f"独立检查通过: {' '.join(cmd_list)}", group, "ok", output.strip().splitlines()[-6:])
            else:
                c.fail(f"project.extra.{len(ctx.checker.checks)}", f"独立检查失败: {' '.join(cmd_list)}", group, f"rc={proc.returncode}", output.strip().splitlines()[-10:])
        except (OSError, subprocess.SubprocessError) as exc:
            c.fail(f"project.extra.{len(ctx.checker.checks)}", f"独立检查无法执行: {' '.join(cmd_list)}", group, str(exc))


# ---------------------------------------------------------------------------
# 9) Lead 声明 vs 独立证据（矛盾检测）
# ---------------------------------------------------------------------------

VERDICT_PASS = {"PASS", "PASSED", "OK", "TRUE", "SUCCESS", "SUCCEEDED"}
VERDICT_FAIL = {"FAIL", "FAILED", "ERROR", "FALSE", "FAILURE", "BROKEN", "MISSING"}
VERDICT_KEYS = ("status", "result", "verdict", "pass", "ok", "passed", "success")
NAME_KEYS = ("id", "name", "check", "check_id", "title", "assertion", "case", "test", "step", "label")

# 声明文本关键词 -> 独立断言 id（按顺序匹配，先命中先用）。
# 用于把 harness 自定义断言 id（如 P3.2/A5）映射到本验证器的语义断言，
# 以检测“Lead 宣称 PASS 但原始证据不成立”的矛盾。
CLAIM_KEYWORD_MAP = [
    ("tool 配对", ["session.tool_pairing"]),
    ("tool pairing", ["session.tool_pairing"]),
    ("配对完整", ["session.tool_pairing"]),
    ("loop-guard", ["session.loop_guard_present"]),
    ("workspace-instructions", ["session.context_kinds_present"]),
    ("runtime-context", ["session.context_kinds_present"]),
    ("skill-catalog", ["session.context_kinds_present"]),
    ("上下文未无限重复", ["session.context_supersede"]),
    ("spill 文件存在", ["spill.dir_exists"]),
    ("spill", ["spill.marker_refs"]),
    ("自主轮调度", ["log.autonomy_rounds"]),
    ("autonomy", ["log.autonomy_rounds"]),
    ("压缩日志", ["log.forced_compression"]),
    ("compression", ["log.forced_compression"]),
    ("pid 文件已删除", ["log.no_residual"]),
    ("无孤儿", ["log.no_residual"]),
    ("配置未改写", ["config.hash_before_after"]),
    ("config.json", ["config.hash_before_after"]),
    ("fix_report", ["markers.report"]),
    ("report.md", ["markers.report"]),
    ("修复说明", ["markers.report"]),
    ("技能正文标记", ["markers.skill_visible"]),
    ("技能标记", ["markers.skill_visible"]),
    ("skill_body", ["markers.skill_visible"]),
    ("agents.md 修改后", ["markers.agents_delta"]),
    ("claude.md 修改后", ["markers.claude_md"]),
    ("autonomy_step2", ["markers.autonomy_notes"]),
    ("自主续跑真实执行", ["markers.autonomy_notes"]),
    ("独立复跑", ["project.tests_pass"]),
    ("主动压缩", ["log.forced_compression"]),
    ("裁剪证据", ["log.compression_fields"]),
    ("list_dir", ["log.repeat_tool_calls"]),
    ("未改写配置", ["config.hash_before_after"]),
    ("任务状态", ["meta.task_state_schema"]),
    ("task_state", ["meta.task_state_schema"]),
    ("摘要", ["meta.summary"]),
    ("unittest", ["project.tests_pass"]),
    ("测试通过", ["project.tests_pass"]),
    ("测试全部通过", ["project.tests_pass"]),
]


def normalize_verdict(value) -> str | None:
    if isinstance(value, bool):
        return PASS if value else FAIL
    if isinstance(value, str):
        upper = value.strip().upper()
        if upper in VERDICT_PASS:
            return PASS
        if upper in VERDICT_FAIL:
            return FAIL
    return None


def parse_lead_claims(results_path: Path) -> tuple[list[dict], str | None, dict]:
    if not results_path.is_file():
        return [], None, {}
    try:
        data = json.loads(results_path.read_text(encoding="utf-8"))
    except (OSError, ValueError) as exc:
        return [], f"results.json 解析失败: {exc}", {}
    claims = []
    overall = None

    def walk(node, path, depth=0):
        nonlocal overall
        if depth > 8 or len(claims) > 4000:
            return
        if isinstance(node, dict):
            if path in ("", "/") and overall is None:
                for key in ("overall", "overall_status", "status", "result", "verdict"):
                    candidate = normalize_verdict(node.get(key))
                    if candidate:
                        overall = candidate
                        break
            verdict_value = None
            verdict_key = None
            for key in VERDICT_KEYS:
                if key in node:
                    verdict_value = node[key]
                    verdict_key = key
                    break
            verdict = normalize_verdict(verdict_value)
            name = None
            for key in NAME_KEYS:
                if isinstance(node.get(key), str) and node.get(key).strip():
                    name = node[key].strip()
                    break
            if verdict:
                detail_text = ""
                for key in ("detail", "message", "desc", "description", "note"):
                    if isinstance(node.get(key), (str, int, float)):
                        detail_text = str(node[key])
                        break
                claims.append(
                    {
                        "path": path or "/",
                        "name": name,
                        "verdict": verdict,
                        "key": verdict_key,
                        "depth": depth,
                        "detail": detail_text,
                    }
                )
                if path in ("", "/") and str(node.get(verdict_key)).strip().upper() in (VERDICT_PASS | VERDICT_FAIL):
                    if overall is None:
                        overall = verdict
            for key, val in node.items():
                walk(val, f"{path}/{key}" if path else key, depth + 1)
        elif isinstance(node, list):
            for idx, val in enumerate(node):
                walk(val, f"{path}[{idx}]", depth + 1)

    walk(data, "")
    # 有些 harness 不写顶层 overall，只写 counts；尝试从 counts 反推。
    if overall is None and isinstance(data, dict) and isinstance(data.get("counts"), dict):
        counts = data["counts"]
        try:
            fail_count = int(counts.get("FAIL", 0))
            pass_count = int(counts.get("PASS", 0))
            if fail_count == 0 and pass_count > 0:
                overall = PASS
            elif fail_count > 0:
                overall = FAIL
        except (TypeError, ValueError):
            pass
    return claims, None, {"overall": overall, "claims": claims, "counts": data.get("counts") if isinstance(data, dict) else None}


def check_lead_claims(ctx: VerifyContext):
    c = ctx.c()
    group = "harness"
    results_path = ctx.artifacts / "results.json"
    claims, error, meta = parse_lead_claims(results_path)
    required_fail = ctx.checker.required_failures()  # 已排除 expected-known-failure
    known_fail = [chk for chk in ctx.checker.known_failure_checks() if chk["status"] == FAIL]
    my_overall = FAIL if required_fail else PASS
    contradictions = []
    advisory_mismatches = []
    lead_fail_claims = []

    if error:
        c.fail("harness.results_parse", "results.json 可解析", group, error, [c.rel(results_path)])

    if claims:
        check_by_id = {chk["id"].lower(): chk for chk in ctx.checker.checks}
        c.ok(
            "harness.claims_parse",
            "解析 Lead 声明",
            group,
            f"{len(claims)} 条带判定值的声明；Lead overall={meta.get('overall')}；独立 overall={my_overall}",
        )
        for claim in claims:
            if claim["verdict"] == FAIL:
                lead_fail_claims.append(
                    {"path": claim["path"], "name": claim.get("name"), "detail": clip(claim.get("detail") or "", 200)}
                )
        for claim in claims:
            if claim["verdict"] != PASS:
                continue
            name = (claim.get("name") or "").strip()
            if not name:
                continue
            needle = re.sub(r"[^a-z0-9]+", "", name.lower())
            if not needle:
                continue
            matches = []
            for cid, chk in check_by_id.items():
                cid_norm = re.sub(r"[^a-z0-9]+", "", cid)
                if cid_norm and (needle == cid_norm or needle in cid_norm or cid_norm in needle):
                    matches.append(chk)
            claim_text = (name + " " + str(claim.get("detail") or "")).lower()
            for keyword, check_ids in CLAIM_KEYWORD_MAP:
                if keyword in claim_text:
                    for cid in check_ids:
                        mapped = check_by_id.get(cid.lower())
                        if mapped and mapped not in matches:
                            matches.append(mapped)
                    break
            for chk in matches:
                if chk.get("expected_known_failure"):
                    continue
                if chk["status"] == FAIL and chk["severity"] == "required":
                    contradictions.append(
                        {
                            "kind": "UNSUPPORTED_BY_FINAL_ARTIFACTS",
                            "claim": {"path": claim["path"], "name": name, "verdict": "PASS", "detail": clip(claim.get("detail") or "", 240)},
                            "independent_check": chk["id"],
                            "independent_status": chk["status"],
                            "detail": chk["detail"],
                            "evidence": chk["evidence"][:4],
                        }
                    )
                elif chk["status"] == WARN:
                    # 只有 required 契约断言失败才算「矛盾」；advisory 降级项不计入矛盾清单，
                    # 仅记录为 advisory 映射（供人工参考）。
                    advisory_mismatches.append(
                        {
                            "claim": {"path": claim["path"], "name": name, "verdict": "PASS", "detail": clip(claim.get("detail") or "", 240)},
                            "independent_check": chk["id"],
                            "independent_status": chk["status"],
                            "detail": chk["detail"],
                        }
                    )
        # 顶层 overall 矛盾
        lead_overall = meta.get("overall")
        if lead_overall == PASS and my_overall == FAIL:
            contradictions.append(
                {
                    "kind": "UNSUPPORTED_BY_FINAL_ARTIFACTS",
                    "claim": {"path": "/overall", "name": "overall", "verdict": "PASS"},
                    "independent_check": "overall",
                    "independent_status": FAIL,
                    "detail": f"独立验证判定 FAIL：{len(required_fail)} 条必需断言失败",
                    "evidence": [f"{chk['id']}: {chk['detail']}" for chk in required_fail[:6]],
                }
            )
        required_contradictions = [item for item in contradictions if not item.get("advisory")]
        if required_contradictions:
            c.fail(
                "harness.contradictions",
                "Lead PASS 声明均有原始证据支撑",
                group,
                f"{len(required_contradictions)} 条 Lead PASS 声明无法由最终原始 artifacts 支撑",
                [
                    f"{item['claim']['name']} (results.json {item['claim']['path']}) 宣称 PASS，"
                    f"但独立断言 {item['independent_check']} = {item['independent_status']}: {clip(item['detail'], 200)}"
                    for item in required_contradictions[:10]
                ],
            )
        else:
            c.ok("harness.contradictions", "Lead PASS 声明均有原始证据支撑", group, "未发现 PASS→FAIL 矛盾")
        if advisory_mismatches:
            c.warn(
                "harness.advisory_mismatches",
                "Lead PASS 声明对应的独立断言为 advisory 告警（不计入矛盾）",
                group,
                f"{len(advisory_mismatches)} 条 PASS 声明对应 advisory 告警",
                [f"{item['claim']['name']} -> {item['independent_check']}" for item in advisory_mismatches[:8]],
            )
    else:
        c.warn("harness.claims_parse", "解析 Lead 声明", group, "results.json 无带判定值的声明")

    # ---- 校验 harness 自己的 artifact-analysis.json 是否支撑同类 PASS 声明 ----
    analysis_path = ctx.artifacts / "artifact-analysis.json"
    aa = None
    if analysis_path.is_file():
        try:
            aa = json.loads(analysis_path.read_text(encoding="utf-8"))
        except ValueError:
            aa = None
    if isinstance(aa, dict) and claims:
        kinds = aa.get("context_kinds") or {}
        log_stats = aa.get("gateway_log_stats") or {}
        spill_info = aa.get("spill") or {}
        aa_rules = [
            ("workspace-instructions", lambda: (kinds.get("workspace-instructions") or 0) >= 1),
            ("runtime-context", lambda: (kinds.get("runtime-context") or 0) >= 1),
            ("skill-catalog", lambda: (kinds.get("skill-catalog") or 0) >= 1),
            ("loop-guard", lambda: (kinds.get("loop-guard") or 0) >= 1 or bool(getattr(ctx, "loop_guard_evidence", {}).get("supported"))),
            ("spill 文件存在", lambda: len(spill_info.get("files") or []) >= 1),
            ("自主轮调度", lambda: (log_stats.get("autonomy_scheduled") or 0) >= 1),
            ("压缩日志", lambda: ((log_stats.get("forced_compression") or 0) + (log_stats.get("proactive_compression") or 0)) >= 1),
            ("tool 配对", lambda: bool((aa.get("tool_pairing") or {}).get("ok"))),
        ]
        inconsistencies = []
        for claim in claims:
            if claim["verdict"] != PASS:
                continue
            text = ((claim.get("name") or "") + " " + str(claim.get("detail") or "")).lower()
            for keyword, evaluate in aa_rules:
                if keyword.lower() in text:
                    try:
                        supported = bool(evaluate())
                    except (TypeError, ValueError):
                        supported = True
                    if not supported:
                        inconsistencies.append(
                            {
                                "claim": claim.get("name") or claim["path"],
                                "keyword": keyword,
                                "analysis_field": "artifact-analysis.json",
                                "detail": clip(claim.get("detail") or "", 200),
                            }
                        )
                    break
        if inconsistencies:
            c.fail(
                "harness.self_analysis_consistency",
                "harness 自产 artifact-analysis.json 与其 PASS 声明自洽",
                group,
                f"{len(inconsistencies)} 条 PASS 声明与 artifact-analysis.json 不一致",
                [
                    f"{item['claim']}: 声明包含 {item['keyword']}，但 artifact-analysis.json 不支持（{item['detail']}）"
                    for item in inconsistencies[:8]
                ],
            )
        else:
            c.ok(
                "harness.self_analysis_consistency",
                "harness 自产 artifact-analysis.json 与其 PASS 声明自洽",
                group,
                f"artifact-analysis.json: kinds={kinds}",
                [c.rel(analysis_path)],
            )

    # ---- harness.log 的 FAIL 声明是否与 known failures 对齐 ----
    harness_fails = re.findall(r"\[FAIL\]\s+([A-Za-z0-9_.]+):", ctx.harness_text or "")
    if harness_fails:
        c.ok(
            "harness.known_failures_declared",
            "harness 记录的 FAIL 与已知缺陷声明一致",
            group,
            f"harness.log [FAIL]: {harness_fails}；known_failures 断言: {[chk['id'] for chk in known_fail]}",
            [f"harness.log [FAIL] {item}" for item in harness_fails[:6]],
        )
        mapped = {item.split(".")[0] if item.startswith("P") else item for item in harness_fails}
        if known_fail and not mapped:
            c.warn("harness.known_failures_declared", "harness 记录的 FAIL 与已知缺陷声明一致", group, "harness FAIL 与已知缺陷无法对应")
    elif known_fail:
        c.warn(
            "harness.known_failures_declared",
            "harness 记录的 FAIL 与已知缺陷声明一致",
            group,
            "配置了 known_failures，但 harness.log 未记录 [FAIL] 行",
        )
    else:
        c.skip("harness.known_failures_declared", "harness 记录的 FAIL 与已知缺陷声明一致", group, "harness.log 无 [FAIL] 行")

    # ---- summary.md 声明的整体结论 ----
    summary_text = read_text(ctx.artifacts / "summary.md") or ""
    summary_verdict = None
    summary_fail_count = None
    if summary_text:
        agg = re.search(r"PASS\s*=\s*(\d+)[^\n]*FAIL\s*=\s*(\d+)", summary_text)
        if agg:
            summary_fail_count = int(agg.group(2))
            summary_verdict = PASS if summary_fail_count == 0 else FAIL
        else:
            m = re.search(r"\b(PASS|FAIL)\b", summary_text)
            if m:
                summary_verdict = m.group(1)
    if summary_verdict:
        known_ids = {chk["id"] for chk in known_fail}
        if summary_fail_count is not None:
            # summary 的 FAIL 数若全部对应已知缺陷，则与独立结论一致（已知缺陷不计入矛盾）
            consistent = summary_fail_count == len(known_ids) and my_overall == PASS
            if consistent:
                summary_verdict = PASS
        if summary_verdict == PASS and my_overall == FAIL:
            c.fail(
                "harness.summary_consistency",
                "summary.md 结论与独立证据一致",
                group,
                "summary.md 宣称 PASS，但独立验证 FAIL",
                [f"summary.md 结论: {summary_verdict}"] + [f"{chk['id']}: {clip(chk['detail'], 140)}" for chk in required_fail[:5]],
            )
        else:
            c.ok(
                "harness.summary_consistency",
                "summary.md 结论与独立证据一致",
                group,
                f"summary={summary_verdict}（FAIL 声明={summary_fail_count}）独立={my_overall} 已知缺陷={sorted(known_ids)}",
                severity="advisory",
            )
    else:
        c.warn("harness.summary_consistency", "summary.md 结论与独立证据一致", group, "summary.md 无 PASS/FAIL 结论词")

    # ---- harness.log 异常堆栈 / health ----
    tracebacks = [line for line in (ctx.harness_text or "").splitlines() if "Traceback (most recent call last)" in line]
    if tracebacks:
        c.warn("harness.log_tracebacks", "harness.log 无异常堆栈", group, f"{len(tracebacks)} 处 Traceback", tracebacks[:3])
    else:
        c.ok("harness.log_tracebacks", "harness.log 无异常堆栈", group, "无 Traceback", severity="advisory")
    if re.search(r"health", ctx.harness_text or "", re.I):
        health_ok = bool(re.search(r"health[^\n]*(200|ok|healthy)", ctx.harness_text or "", re.I))
        if health_ok:
            c.ok("harness.health", "harness 记录了成功的 /health 检查", group, "health 200/ok", severity="advisory")
        else:
            c.warn("harness.health", "harness 记录了成功的 /health 检查", group, "harness.log 提到 health 但未发现 200/ok")
    else:
        c.skip("harness.health", "harness 记录了成功的 /health 检查", group, "harness.log 未记录 health")

    ctx.advisory_mismatches = advisory_mismatches
    ctx.lead_claims = {
        "claims": claims[:400],
        "overall": meta.get("overall"),
        "counts": meta.get("counts"),
        "fail_claims": lead_fail_claims,
        "summary_verdict": summary_verdict,
        "summary_fail_count": summary_fail_count,
    }
    ctx.contradictions = contradictions


# ---------------------------------------------------------------------------
# 主验证流程
# ---------------------------------------------------------------------------


def discover_sessions(artifacts: Path) -> list[Path]:
    ws = artifacts / "workspace"
    sessions_dir = ws / "sessions"
    files = []
    if sessions_dir.is_dir():
        files.extend(sorted(sessions_dir.glob("*.jsonl")))
        if not files:
            files.extend(sorted(sessions_dir.rglob("*.jsonl")))
    if not files and ws.is_dir():
        files = sorted(ws.rglob("*.jsonl"))
    return files


def verify(artifacts: Path, expectations: dict, timeout_sec: int | None = None) -> dict:
    artifacts = Path(artifacts).resolve()
    exp = deep_merge(DEFAULT_EXPECTATIONS, expectations or {})
    timeout = int(timeout_sec or exp.get("timeout_sec") or 180)
    ctx = VerifyContext(artifacts, exp, timeout)
    ctx.results_text = read_text(artifacts / "results.json") or ""
    ctx.harness_text = read_text(artifacts / "harness.log") or ""
    # 真实 harness 把运行期 /tmp 工作区原样复制到 artifacts/workspace，
    # workspace.path 记录原始路径：spill marker 等绝对路径需要重映射。
    for name in ("workspace.path", "workspace_path", "workspace.path.txt"):
        candidate = artifacts / name
        if candidate.is_file():
            ctx.original_workspace = (read_text(candidate) or "").strip()
            break
    # 日志提前解析：spill marker / marker 证据会在日志检查之前使用 corpus。
    for name in ("gateway.log", "gateway-restart.log"):
        path = artifacts / name
        ctx.logs[name] = parse_log_file(path) if path.is_file() else []
    ctx.corpus = [event for events in ctx.logs.values() for event in events]
    check_layout(ctx)
    for path in discover_sessions(artifacts):
        ctx.add_session(path)
    if ctx.sessions:
        ctx.primary = max(ctx.sessions, key=lambda a: (a.path.stat().st_size, a.raw_nonempty))
    check_sessions_context(ctx)
    check_meta(ctx)
    check_spill(ctx)
    check_markers(ctx)
    check_config(ctx)
    check_logs(ctx)
    run_project_tests(ctx)
    check_lead_claims(ctx)

    counts = ctx.checker.counts()
    required_failures = ctx.checker.required_failures()
    advisory_warnings = ctx.checker.advisory_warnings()
    known_checks = ctx.checker.known_failure_checks()
    known_reproduced = [c for c in known_checks if c["status"] == FAIL]
    known_not_reproduced = [c for c in known_checks if c["status"] != FAIL]
    overall = FAIL if required_failures else PASS
    analysis = {
        "sessions": [
            {
                "file": a.rel,
                "raw_nonempty_lines": a.raw_nonempty,
                "parse_issues": a.parse_issues,
                "messages": len(a.messages),
                "context_histogram": a.kind_histogram(),
                "unknown_context_kinds": a.unknown_kinds,
                "tool_calls": len(a.tool_calls),
                "tool_results": len(a.tool_results),
                "pairing": {
                    "unmatched_calls": [
                        {"line": c["line"], "id": c["id"], "name": c["name"]} for c in a.pairing()["unmatched_calls"]
                    ],
                    "orphan_results": [
                        {"line": r["line"], "id": r["id"]} for r in a.pairing()["orphan_results"]
                    ],
                },
                "segments": a.segments,
                "supersede": a.supersede,
                "prune_refs": [
                    {"line": r["line"], "path": r["path"], "tool_call_id": r["tool_call_id"]} for r in a.prune_refs
                ],
                "plain_prune_markers": a.plain_prune_count,
                "meta": a.meta,
            }
            for a in ctx.sessions
        ],
        "logs": getattr(ctx, "logs_summary", {}),
        "spill": getattr(ctx, "spill", {}),
        "loop_guard_evidence": getattr(ctx, "loop_guard_evidence", {}),
        "context_kinds_observed": getattr(ctx, "context_kinds_observed", {}),
        "markers": ctx.markers,
        "config": ctx.config,
        "project": ctx.project,
        "lead_claims": ctx.lead_claims,
        "contradictions": ctx.contradictions,
        "advisory_claim_mismatches": getattr(ctx, "advisory_mismatches", []),
    }
    report = {
        "schema_version": SCHEMA_VERSION,
        "generated_at": now_iso(),
        "artifacts": str(artifacts),
        "expectations_source": ctx.exp.get("_source") if isinstance(ctx.exp, dict) else None,
        "overall_status": overall,
        "exit_code": 0 if overall == PASS else 1,
        "counts": counts,
        "required_failure_ids": [chk["id"] for chk in required_failures],
        "known_failures": known_checks,
        "known_failures_reproduced": [chk["id"] for chk in known_reproduced],
        "known_failures_not_reproduced": [chk["id"] for chk in known_not_reproduced],
        "checks": ctx.checker.checks,
        "analysis": analysis,
    }
    return report


# ---------------------------------------------------------------------------
# 人读摘要
# ---------------------------------------------------------------------------

STATUS_ICON = {PASS: "✓", FAIL: "✗", WARN: "⚠", SKIP: "–"}


def render_summary(report: dict) -> str:
    lines = []
    counts = report["counts"]
    lines.append("=" * 78)
    lines.append("独立产物验证报告（不信任 harness results.json，直接复核原始 artifacts）")
    lines.append("=" * 78)
    lines.append(f"artifacts : {report['artifacts']}")
    lines.append(f"生成时间  : {report['generated_at']}")
    lines.append(
        f"独立结论  : {report['overall_status']}   "
        f"(PASS {counts.get(PASS, 0)} / FAIL {counts.get(FAIL, 0)} / "
        f"WARN {counts.get(WARN, 0)} / SKIP {counts.get(SKIP, 0)})"
    )
    required_failures = [
        c
        for c in report["checks"]
        if c["severity"] == "required" and c["status"] == FAIL and not c.get("expected_known_failure")
    ]
    known_checks = [c for c in report["checks"] if c.get("expected_known_failure")]
    advisory = [c for c in report["checks"] if c["severity"] == "advisory" and c["status"] == WARN]
    contradictions = report["analysis"].get("contradictions") or []

    lines.append("")
    if required_failures:
        lines.append(f"-- 必需断言失败 ({len(required_failures)}) --")
        for check in required_failures:
            lines.append(f"✗ [{check['id']}] {check['title']}")
            lines.append(f"    {check['detail']}")
            for evidence in check["evidence"][:5]:
                lines.append(f"    - {evidence}")
        lines.append("")
    if known_checks:
        lines.append(f"-- 已知预期失败 / expected-known-failure ({len(known_checks)}) --")
        for check in known_checks:
            state = "已由原始证据复现" if check["status"] == FAIL else "未复现（advisory）"
            lines.append(f"! [{check['id']}] {check['title']}：{state}")
            lines.append(f"    {check['detail']}")
            for evidence in check["evidence"][:4]:
                lines.append(f"    - {evidence}")
        lines.append("")
    if advisory:
        lines.append(f"-- advisory 告警 ({len(advisory)}) --")
        for check in advisory[:20]:
            lines.append(f"⚠ [{check['id']}] {check['title']}: {clip(check['detail'], 220)}")
            for evidence in check["evidence"][:2]:
                lines.append(f"    - {evidence}")
        if len(advisory) > 20:
            lines.append(f"    … 其余 {len(advisory) - 20} 条见 JSON")
        lines.append("")
    if contradictions:
        lines.append(f"-- Lead 声明 vs 原始证据矛盾 ({len(contradictions)}) --")
        for item in contradictions[:12]:
            tag = "advisory" if item.get("advisory") else "REQUIRED"
            kind = item.get("kind") or "UNSUPPORTED_BY_FINAL_ARTIFACTS"
            lines.append(
                f"! [{tag}/{kind}] results.json {item['claim']['path']} 宣称 {item['claim']['verdict']} "
                f"({item['claim'].get('name')}), 独立断言 {item['independent_check']} = {item['independent_status']}"
            )
            lines.append(f"    {clip(item['detail'], 200)}")
            for evidence in item.get("evidence", [])[:3]:
                lines.append(f"    - {evidence}")
        lines.append("")

    passed = [c for c in report["checks"] if c["status"] == PASS]
    if passed:
        lines.append(f"-- 通过的断言 ({len(passed)}) --")
        by_group = {}
        for check in passed:
            by_group.setdefault(check["group"], []).append(check)
        for group in sorted(by_group):
            ids = ", ".join(check["id"] for check in by_group[group])
            lines.append(f"✓ {group}: {ids}")
        lines.append("")
    skipped = [c for c in report["checks"] if c["status"] == SKIP]
    if skipped:
        lines.append(f"-- 跳过 ({len(skipped)}) --")
        for check in skipped[:15]:
            lines.append(f"– [{check['id']}] {check['title']}: {clip(check['detail'], 160)}")
        lines.append("")

    lead_overall = report["analysis"].get("lead_claims", {}).get("overall")
    lines.append(
        f"Lead 声明 overall: {lead_overall if lead_overall else 'n/a'} | 独立结论: {report['overall_status']}"
    )
    known_reproduced = report.get("known_failures_reproduced") or []
    known_note = (
        f"；另有 {len(known_reproduced)} 项已确认的 expected-known-failure（{', '.join(known_reproduced)}）"
        if known_reproduced
        else ""
    )
    if report["overall_status"] == PASS:
        lines.append(f"结论: PASS —— 全部 required 断言有原始证据支撑{known_note}。")
    else:
        lines.append(
            f"结论: FAIL —— 存在 required 断言未被原始证据支撑（详见上方证据）{known_note}。"
        )
    return "\n".join(lines)


# ---------------------------------------------------------------------------
# 自测 fixture
# ---------------------------------------------------------------------------


def _write(path: Path, content: str, mode: int | None = None):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(content, encoding="utf-8")
    if mode is not None:
        os.chmod(path, mode)


def _jsonl(records: list[dict]) -> str:
    return "\n".join(json.dumps(record, ensure_ascii=False) for record in records) + "\n"


def _log_json(**fields) -> str:
    return json.dumps(fields, ensure_ascii=False)


def build_fixture(live: Path) -> dict:
    """构造一份与真实 harness 产物布局一致的合成 artifacts。

    布局对齐真实 E2E（run_dir/workspace 是真实工作区副本）：
    - workspace/AGENTS.md（baseline 标记 + 运行时追加标记）
    - workspace/skills/e2e-skill/SKILL.md
    - workspace/project/src/stats.py + project/tests/test_stats.py + project/FIX_REPORT.md
    - workspace/sessions/main.jsonl + main.meta.json
    - workspace/.chagent/spill/<session>/tool-<id>-<sha16>.txt
    - gateway.log / gateway-restart.log / harness.log / results.json / summary.md
    - config.json（运行期真实配置，hash 对象）/ config.redacted.json / config.hash.before/after
    """
    ws = live / "workspace"
    sessions = ws / "sessions"
    for path in (ws, sessions):
        path.mkdir(parents=True, exist_ok=True)

    agents_marker_v1 = "WORKSPACE_MARKER_V1"
    agents_marker = "WORKSPACE_MARKER_V2"
    skill_marker = "SKILL_BODY_MARKER_V1"
    report_marker = "PROJECT_REPORT_MARKER"

    # AGENTS.md：运行后内容（baseline V1 + 运行时追加 V2）
    agents_full = (
        "# AGENTS.md\n\n"
        f"- 当用户询问「工作区标记」时，必须只回复：{agents_marker_v1}\n"
        f"- 当用户询问「项目标记」时，必须只回复：{agents_marker}\n"
    )
    baseline_agents = (
        "# AGENTS.md\n\n"
        f"- 当用户询问「工作区标记」时，必须只回复：{agents_marker_v1}\n"
    )
    _write(ws / "AGENTS.md", agents_full)
    _write(
        ws / "skills" / "e2e-skill" / "SKILL.md",
        "# e2e-skill\n\nname: e2e-skill\n\ndescription: 端到端技能。\n\n"
        f"加载后必须输出技能正文标记：{skill_marker}\n",
    )

    # 项目：与真实场景相同的 src/tests 布局（已修复）
    _write(
        ws / "project" / "src" / "stats.py",
        "def mean(values):\n    if not values:\n        return 0.0\n"
        "    return sum(values) / len(values)\n\n\n"
        "def median(values):\n    if not values:\n        return 0.0\n"
        "    ordered = sorted(values)\n    mid = len(ordered) // 2\n"
        "    if len(ordered) % 2:\n        return float(ordered[mid])\n"
        "    return (ordered[mid - 1] + ordered[mid]) / 2.0\n\n\n"
        "def count_levels(records):\n    counter = {}\n"
        "    for line in records:\n        level = line.split(\" \", 1)[0].strip().upper()\n"
        "        counter[level] = counter.get(level, 0) + 1\n    return counter\n",
    )
    _write(ws / "project" / "tests" / "__init__.py", "")
    _write(
        ws / "project" / "tests" / "test_stats.py",
        "import unittest\n\nfrom src.stats import count_levels, mean, median\n\n\n"
        "class StatsTest(unittest.TestCase):\n"
        "    def test_mean_basic(self):\n"
        "        self.assertEqual(mean([1, 2, 3]), 2.0)\n"
        "        self.assertEqual(mean([5]), 5.0)\n\n"
        "    def test_median(self):\n"
        "        self.assertEqual(median([1, 2, 3]), 2.0)\n"
        "        self.assertEqual(median([1, 2, 3, 4]), 2.5)\n\n"
        "    def test_count_levels(self):\n"
        "        records = [\"INFO start\", \"WARN slow\", \"ERROR boom\", \"INFO done\"]\n"
        "        self.assertEqual(count_levels(records), {\"INFO\": 2, \"WARN\": 1, \"ERROR\": 1})\n\n\n"
        "if __name__ == '__main__':\n    unittest.main()\n",
    )
    _write(
        ws / "project" / "FIX_REPORT.md",
        "# 修复报告\n\n"
        f"{report_marker}\n\n"
        "- 修复 src/stats.py 的 mean/median 计算\n"
        "- `python3 -m unittest discover -s tests -v` 全部测试通过\n",
    )
    _write(ws / "project" / "README.md", "# 日志统计小项目\n\ncd project && python3 -m unittest discover -s tests -v\n")

    # spill：内容寻址文件（0600），目录 0700
    spill_root = ws / ".chagent" / "spill"
    spill_dir = spill_root / "main"
    spill_dir.mkdir(parents=True, exist_ok=True)
    os.chmod(spill_root, 0o700)
    os.chmod(spill_dir, 0o700)
    original = "H" * DEFAULT_PRUNE_HEAD + "中间内容" * 3000 + "T" * DEFAULT_PRUNE_TAIL
    spill_hash = sha256_bytes(original.encode("utf-8"))[:16]
    spill_name = f"tool-call-3-{spill_hash}.txt"
    spill_path = spill_dir / spill_name
    _write(spill_path, original, 0o600)
    pruned_content = (
        original[:DEFAULT_PRUNE_HEAD]
        + "\n"
        + SPILL_PRUNE_PREFIX
        + str(spill_path.resolve())
        + SPILL_PRUNE_CLOSE
        + "\n"
        + original[-DEFAULT_PRUNE_TAIL:]
    )

    records = [
        {"role": "user", "content": "开始真实场景 E2E：修复 project 的 bug 并生成修复报告。", "created_at": "2026-10-30T15:00:00+08:00"},
        {
            "role": "user",
            "content": baseline_agents,
            "context": {"kind": "workspace-instructions", "form": "reminder", "baseline": True,
                        "baseline_identity": "ws1", "changes": [{"action": "set", "scope": "workspace", "path": "AGENTS.md", "digest": "d0"}]},
        },
        {
            "role": "user",
            "content": "<runtime>workspace=/tmp/e2e</runtime>",
            "context": {"kind": "runtime-context", "form": "state", "digest": "runtime@2026-10-30T15:00",
                        "sections": [{"name": "runtime", "text": "<runtime>workspace=/tmp/e2e</runtime>"}]},
        },
        {
            "role": "user",
            "content": "<task_state>\n目标：\"修复 E2E 项目 bug 并生成报告\"\n阶段：paused\n轮次：1/3\n待办：\n- [completed] 修复 src/stats.py\n- [completed] 生成 FIX_REPORT.md\n最近检查点：2026-10-30T15:00:00+08:00\n证据：\n- unittest 运行通过\n阻塞原因：无\n</task_state>",
            "context": {"kind": "task-state", "form": "state"},
        },
        {
            "role": "user",
            "content": "<system-reminder>技能目录：e2e-skill</system-reminder>",
            "context": {"kind": "skill-catalog", "form": "catalog", "digest": "skills-d1",
                        "changes": [{"action": "replace", "scope": "skill-catalog", "digest": "skills-d1"}]},
        },
        {
            "role": "assistant",
            "content": "",
            "tool_calls": [
                {
                    "id": "call-1",
                    "type": "function",
                    "function": {
                        "name": "edit_file",
                        "arguments": json.dumps({"path": "AGENTS.md", "new_string": f"当用户询问「项目标记」时，必须只回复：{agents_marker}"}, ensure_ascii=False),
                    },
                }
            ],
        },
        {"role": "tool", "tool_call_id": "call-1", "content": f"已编辑 AGENTS.md：追加 {agents_marker}"},
        {
            "role": "user",
            "content": agents_full,
            "context": {"kind": "workspace-instructions", "form": "reminder",
                        "changes": [{"action": "replace", "scope": "workspace", "path": "AGENTS.md", "digest": "d1"}]},
        },
        {
            "role": "assistant",
            "content": "",
            "tool_calls": [
                {
                    "id": "call-2",
                    "type": "function",
                    "function": {"name": "skill", "arguments": json.dumps({"name": "e2e-skill"})},
                }
            ],
        },
        {
            "role": "tool",
            "tool_call_id": "call-2",
            "content": f"<skill_content>\n# e2e-skill\n\n{skill_marker}\n</skill_content>",
        },
        {
            "role": "assistant",
            "content": "",
            "tool_calls": [
                {
                    "id": "call-3",
                    "type": "function",
                    "function": {"name": "exec", "arguments": json.dumps({"command": "cd project && python3 -m unittest discover -s tests -v"})},
                }
            ],
        },
        {"role": "tool", "tool_call_id": "call-3", "content": pruned_content},
        {
            "role": "user",
            "content": "第 2 轮自主续跑：继续收尾。",
            "context": {"kind": "task-round", "form": "prompt"},
        },
        {
            "role": "user",
            "content": "检测到重复的工具调用，请调整策略。",
            "context": {"kind": "loop-guard", "form": "reminder"},
        },
        {
            "role": "assistant",
            "content": f"项目标记是 {agents_marker}。修复完成，测试通过，{report_marker} 已写入 project/FIX_REPORT.md。PROJECT_OK",
        },
    ]
    session_text = _jsonl(records)
    session_path = sessions / "main.jsonl"
    _write(session_path, session_text)
    line_count = len([line for line in session_text.splitlines() if line.strip()])

    meta = {
        "key": "main",
        "summary": "完成真实场景 E2E：修复 project/src/stats.py、生成 FIX_REPORT.md、验证技能/autonomy/spill。",
        "skip": 0,
        "count": line_count,
        "created_at": "2026-10-30T15:00:00+08:00",
        "updated_at": "2026-10-30T15:02:00+08:00",
        "task_state": {
            "version": 1,
            "objective": "修复 E2E 项目 bug 并生成报告",
            "phase": "paused",
            "round": 1,
            "max_rounds": 3,
            "todos": [
                {"content": "修复 src/stats.py", "status": "completed"},
                {"content": "生成 FIX_REPORT.md", "status": "completed"},
            ],
            "evidence": ["unittest 运行通过"],
            "last_progress_at": "2026-10-30T15:01:00+08:00",
            "updated_at": "2026-10-30T15:02:00+08:00",
        },
    }
    _write(sessions / "main.meta.json", json.dumps(meta, ensure_ascii=False, indent=2))

    # gateway 日志：与真实运行一致，是 stdout+stderr 的 console 抓取。
    gateway_lines = [
        "===== start 2026-10-30T15:00:00+0800 =====",
        '15:00:01 INF component=gateway Log level set to "info"',
        "15:00:02 INF component=agent Agent initialized tools_count=12 skills_total=1",
        "15:00:02 INF component=gateway Config hot reload enabled",
        "15:01:00 INF component=agent Tool call: edit_file({\"path\": \"AGENTS.md\"})",
        "15:01:05 INF component=agent Tool call (batch): skill({\"name\": \"e2e-skill\"})",
        "15:01:06 INF component=agent Tool call: exec({\"command\": \"cd project && python3 -m unittest\"})",
        "15:02:00 INF component=agent autonomy round scheduled session_key=main round=1 max_rounds=3 source=continuation delay_ms=0",
        "15:02:01 WRN component=agent Forced compression pruned tool results without dropping turns pruned_msgs=1 spilled_msgs=1 saved_chars=12000 new_count=15",
        "15:03:00 INF component=gateway Shutting down...",
        "15:03:00 INF component=gateway ✓ Gateway stopped",
        "15:03:00 INF component=pid remove pid file: /tmp/e2e/.chagent.pid",
    ]
    _write(live / "gateway.log", "\n".join(gateway_lines) + "\n")

    restart_lines = [
        "===== start 2026-10-30T15:05:00+0800 =====",
        '15:05:01 INF component=gateway Log level set to "info"',
        "15:05:02 INF component=agent Agent initialized tools_count=12 skills_total=1",
        "15:06:00 INF component=gateway Shutting down...",
        "15:06:00 INF component=gateway ✓ Gateway stopped",
        "15:06:00 INF component=pid remove pid file: /tmp/e2e/.chagent.pid",
    ]
    _write(live / "gateway-restart.log", "\n".join(restart_lines) + "\n")

    _write(
        live / "harness.log",
        "harness: start gateway pid=424242\n"
        "harness: health check GET /health -> 200 {\"status\":\"ok\"}\n"
        f"harness: markers agents={agents_marker} skill={skill_marker} report={report_marker}\n"
        "harness: stop gateway via SIGTERM\n",
    )
    _write(
        live / "summary.md",
        "# 真实场景 E2E 功能测试结果\n\n"
        "## 汇总：PASS=20 FAIL=0 SKIP=1 INFO=0\n\n"
        "| 检查项 | 状态 | 说明 |\n|---|---|---|\n"
        f"| A8 | PASS | artifact: tool 配对完整（ok） |\n"
        f"| A5 | PASS | artifact: spill 文件存在 |\n"
        f"| A4 | PASS | artifact: loop-guard 提示上下文存在 |\n",
    )

    # results.json：真实 harness schema（checks 带 id/status/detail/evidence）
    checks = [
        ("P1.6", "session JSONL 含 workspace-instructions 上下文"),
        ("P1.7", "session JSONL 含 runtime-context 上下文"),
        ("P1.8", "skill catalog 以 durable context 注入"),
        ("P2.2", "session JSONL 记录了新的 workspace-context delta 内容"),
        ("P2.4", "根 AGENTS.md 修改后 agent definition 也无需重启即生效"),
        ("P3.3", "project/FIX_REPORT.md 已生成且包含标记"),
        ("P6.1", "session JSONL 出现 loop-guard 提示上下文"),
        ("P10.1", "最终 SIGTERM 5s 内退出"),
        ("P10.2", "最终 pid 文件已删除"),
        ("P10.4", "gateway 未改写配置"),
        ("A4", "artifact: loop-guard 提示上下文存在"),
        ("A5", "artifact: spill 文件存在"),
        ("A8", "artifact: tool 配对完整（ok）"),
        ("A6", "artifact: 自主轮调度日志存在"),
        ("A7", "artifact: 压缩日志存在"),
    ]
    _write(
        live / "results.json",
        json.dumps(
            {
                "model": "e2e-model",
                "counts": {"PASS": len(checks), "FAIL": 0, "SKIP": 1, "INFO": 0},
                "checks": [
                    {"id": cid, "status": "PASS", "detail": detail, "evidence": f"artifact:{cid}"}
                    for cid, detail in checks
                ],
                "notes": [],
                "timings": {"setup": 0.1},
                "config_hashes": {},
            },
            ensure_ascii=False,
            indent=2,
        ),
    )

    # 配置与哈希：真实 harness 哈希的是 workspace/config.json
    live_config = {
        "version": 2,
        "model_list": [
            {
                "model_name": "e2e-model",
                "model": "e2e-model",
                "provider": "openai",
                "api_key": "YOUR_API_KEY",
            }
        ],
        "agents": {
            "defaults": {
                "workspace": "/tmp/e2e-workspace",
                "context_window": 64000,
                "max_tokens": 4096,
            }
        },
        "gateway": {"host": "127.0.0.1", "port": 18790},
    }
    live_config_text = json.dumps(live_config, ensure_ascii=False, indent=2)
    _write(ws / "config.json", live_config_text)
    redacted_config = json.loads(json.dumps(live_config))
    redacted_config["model_list"][0]["api_key"] = "REDACTED"
    _write(live / "config.redacted.json", json.dumps(redacted_config, ensure_ascii=False, indent=2))
    config_hash = sha256_bytes(live_config_text.encode("utf-8"))
    _write(live / "config.hash.before", f"{config_hash}\n")
    _write(live / "config.hash.after", f"{config_hash}\n")
    # .chagent.pid 不存在（已被优雅关闭删除）

    # expectations：不含 markers，自测同时覆盖“marker 自动探测”路径
    _write(
        live / "expectations.json",
        json.dumps(
            {
                "min_counts": {
                    "tool_call_logs": 2,
                    "tool_call_batch_logs": 1,
                    "autonomy_round_scheduled": 1,
                    "forced_compression_pruned": 1,
                    "spill_events": 1,
                },
                "require": {"summary": True},
            },
            ensure_ascii=False,
            indent=2,
        ),
    )
    return {
        "live": live,
        "agents_marker": agents_marker,
        "skill_marker": skill_marker,
        "report_marker": report_marker,
        "spill_path": spill_path,
        "session_path": session_path,
        "meta_path": sessions / "main.meta.json",
    }

def _rebase_fixture_paths(old_live: Path, new_live: Path):
    for path in (new_live / "workspace" / "sessions").glob("*.jsonl"):
        text = path.read_text(encoding="utf-8")
        text = text.replace(str(old_live), str(new_live))
        path.write_text(text, encoding="utf-8")


def _mutate_jsonl_broken(live: Path):
    path = live / "workspace" / "sessions" / "main.jsonl"
    lines = path.read_text(encoding="utf-8").splitlines()
    lines.insert(3, "{ this is not valid json")
    path.write_text("\n".join(lines) + "\n", encoding="utf-8")


def _mutate_tool_unpaired(live: Path):
    path = live / "workspace" / "sessions" / "main.jsonl"
    lines = path.read_text(encoding="utf-8").splitlines()
    target = None
    for idx, line in enumerate(lines):
        if '"tool_call_id": "call-1"' in line:
            target = idx
            break
    if target is not None:
        del lines[target]
    path.write_text("\n".join(lines) + "\n", encoding="utf-8")


def _mutate_meta_count(live: Path):
    path = live / "workspace" / "sessions" / "main.meta.json"
    meta = json.loads(path.read_text(encoding="utf-8"))
    meta["count"] = int(meta.get("count") or 0) + 7
    path.write_text(json.dumps(meta, ensure_ascii=False, indent=2), encoding="utf-8")


def _mutate_spill_perms(live: Path):
    spill = live / "workspace" / ".chagent" / "spill"
    for path in spill.rglob("*.txt"):
        os.chmod(path, 0o644)


def _mutate_config_hash(live: Path):
    (live / "config.hash.after").write_text("sha256:" + "0" * 64 + "\n", encoding="utf-8")


def _mutate_project_bug(live: Path):
    """把修复过的 src/stats.py 改回有 bug 的版本（mean 分母错误）。"""
    _write(
        live / "workspace" / "project" / "src" / "stats.py",
        "def mean(values):\n    if not values:\n        return 0.0\n"
        "    return sum(values) / (len(values) + 1)\n\n\n"
        "def median(values):\n    if not values:\n        return 0.0\n"
        "    ordered = sorted(values)\n    mid = len(ordered) // 2\n"
        "    if len(ordered) % 2:\n        return float(ordered[mid])\n"
        "    return (ordered[mid - 1] + ordered[mid]) / 2.0\n\n\n"
        "def count_levels(records):\n    counter = {}\n"
        "    for line in records:\n        level = line.split(\" \", 1)[0].strip().upper()\n"
        "        counter[level] = counter.get(level, 0) + 1\n    return counter\n",
    )


def _mutate_marker_delta(live: Path):
    """AGENTS.md 磁盘文件保留运行时 marker，但会话历史/模型回复全部丢失注入证据。"""
    path = live / "workspace" / "sessions" / "main.jsonl"
    text = path.read_text(encoding="utf-8")
    text = text.replace("WORKSPACE_MARKER_V2", "REDACTED")
    path.write_text(text, encoding="utf-8")


def _mutate_taskstate_phase(live: Path):
    path = live / "workspace" / "sessions" / "main.meta.json"
    meta = json.loads(path.read_text(encoding="utf-8"))
    meta["task_state"]["phase"] = "running"
    path.write_text(json.dumps(meta, ensure_ascii=False, indent=2), encoding="utf-8")


def _mutate_claim_contradiction(live: Path):
    _mutate_tool_unpaired(live)  # 证据破损，但 results.json 仍宣称 PASS


def _mutate_log_missing_tool(live: Path):
    path = live / "gateway.log"
    lines = []
    for line in path.read_text(encoding="utf-8").splitlines():
        if "Tool call" in line:
            continue
        lines.append(line)
    path.write_text("\n".join(lines) + "\n", encoding="utf-8")


def _mutate_spill_content(live: Path):
    for path in (live / "workspace" / ".chagent" / "spill").rglob("*.txt"):
        path.write_text("tampered spill content", encoding="utf-8")
        os.chmod(path, 0o600)


def _mutate_shutdown_missing(live: Path):
    for name in ("gateway.log", "gateway-restart.log"):
        path = live / name
        lines = [line for line in path.read_text(encoding="utf-8").splitlines() if "Shutting down" not in line and "Gateway stopped" not in line]
        path.write_text("\n".join(lines) + "\n", encoding="utf-8")


def _insert_jsonl_line(live: Path, position: int, record: dict) -> None:
    path = live / "workspace" / "sessions" / "main.jsonl"
    lines = [line for line in path.read_text(encoding="utf-8").splitlines() if line.strip()]
    lines.insert(position, json.dumps(record, ensure_ascii=False))
    path.write_text("\n".join(lines) + "\n", encoding="utf-8")
    meta_path = live / "workspace" / "sessions" / "main.meta.json"
    meta = json.loads(meta_path.read_text(encoding="utf-8"))
    meta["count"] = len(lines)
    meta_path.write_text(json.dumps(meta, ensure_ascii=False, indent=2), encoding="utf-8")


def _remove_context_kind_lines(live: Path, kinds: list[str]) -> None:
    path = live / "workspace" / "sessions" / "main.jsonl"
    lines = [line for line in path.read_text(encoding="utf-8").splitlines() if line.strip()]
    kept = [
        line
        for line in lines
        if not any(f'"kind": "{kind}"' in line for kind in kinds)
    ]
    path.write_text("\n".join(kept) + "\n", encoding="utf-8")
    meta_path = live / "workspace" / "sessions" / "main.meta.json"
    meta = json.loads(meta_path.read_text(encoding="utf-8"))
    meta["count"] = len(kept)
    meta_path.write_text(json.dumps(meta, ensure_ascii=False, indent=2), encoding="utf-8")


def _scenario_kinds_compacted_with_evidence(live: Path):
    """最终 JSONL 被 summarize 重写而缺少 runtime/skill-catalog，
    但 evidence/context-kinds-observed.json 记录运行期并集：应 PASS。"""
    _remove_context_kind_lines(live, ["runtime-context", "skill-catalog"])
    evidence = live / "evidence"
    evidence.mkdir(parents=True, exist_ok=True)
    (evidence / "context-kinds-observed.json").write_text(
        json.dumps(
            {
                "captured_at": "2026-10-30T15:01:00+08:00",
                "observed_kinds": {
                    "workspace-instructions": 1,
                    "runtime-context": 1,
                    "task-state": 3,
                    "skill-catalog": 1,
                    "loop-guard": 1,
                    "context-pressure": 1,
                },
                "note": "运行期各快照观察到的最小/最终 context kind 并集；async summarize 可合法重写最终 JSONL",
            },
            ensure_ascii=False,
            indent=2,
        ),
        encoding="utf-8",
    )


def _mutate_kinds_lost_no_evidence(live: Path):
    """必需 kind 缺失且无运行期并集证据：应 FAIL。"""
    _remove_context_kind_lines(live, ["runtime-context", "skill-catalog"])


def _scenario_summarized_head_orphan(live: Path):
    """保留历史开头有孤立 tool_result（其 assistant call 被摘要丢弃）：合法边界，应 PASS。"""
    _insert_jsonl_line(
        live,
        0,
        {"role": "tool", "tool_call_id": "call_dropped_by_summarize", "content": "FILE: FIX_REPORT.md"},
    )


def _mutate_orphan_midhistory(live: Path):
    """孤立 tool_result 出现在 history 中部（前面有 assistant）：真正的配对破坏，应 FAIL。"""
    _insert_jsonl_line(
        live,
        6,
        {"role": "tool", "tool_call_id": "call_orphan_mid_history", "content": "orphan result"},
    )


def _remove_loop_guard_from_jsonl(live: Path) -> None:
    path = live / "workspace" / "sessions" / "main.jsonl"
    lines = path.read_text(encoding="utf-8").splitlines()
    kept = [line for line in lines if '"kind": "loop-guard"' not in line]
    path.write_text("\n".join(kept) + "\n", encoding="utf-8")
    meta_path = live / "workspace" / "sessions" / "main.meta.json"
    meta = json.loads(meta_path.read_text(encoding="utf-8"))
    meta["count"] = len([line for line in kept if line.strip()])
    meta_path.write_text(json.dumps(meta, ensure_ascii=False, indent=2), encoding="utf-8")


def _write_p6_evidence(live: Path) -> None:
    evidence = live / "evidence"
    evidence.mkdir(parents=True, exist_ok=True)
    (evidence / "context-p6.json").write_text(
        json.dumps(
            {
                "phase": "P6",
                "captured_at": "2026-10-30T15:01:00+08:00",
                "kinds": {"workspace-instructions": 1, "runtime-context": 1, "task-state": 3,
                          "skill-catalog": 1, "loop-guard": 1},
                "loop_guard_messages": [
                    {"role": "user", "context": {"kind": "loop-guard", "form": "reminder"},
                     "content": "检测到重复的工具调用，请调整策略。"}
                ],
            },
            ensure_ascii=False,
            indent=2,
        ),
        encoding="utf-8",
    )
    snapshot = evidence / "sessions-at-p6-main.jsonl"
    snapshot.write_text(
        json.dumps(
            {
                "role": "user",
                "content": "检测到重复的工具调用，请调整策略。",
                "context": {"kind": "loop-guard", "form": "reminder"},
            },
            ensure_ascii=False,
        )
        + "\n",
        encoding="utf-8",
    )


def _scenario_loop_guard_compacted(live: Path):
    """最终 JSONL 已被压缩（无 loop-guard），但 P6 时刻快照保留证据：应 PASS。"""
    _remove_loop_guard_from_jsonl(live)
    _write_p6_evidence(live)


def _mutate_loop_guard_lost(live: Path):
    """最终 JSONL 与 P6 快照都没有 loop-guard：应 FAIL。"""
    _remove_loop_guard_from_jsonl(live)


def _scenario_planned_reload(live: Path):
    """模拟 P7 计划内 /reload：config.json 被主动改写，after == after-reload。

    这是合法场景：验证器不能把“计划内配置变更”误判为运行期篡改。
    """
    ws = live / "workspace"
    cfg = json.loads((ws / "config.json").read_text(encoding="utf-8"))
    cfg["agents"]["defaults"]["context_window"] = 12000
    cfg["agents"]["defaults"]["max_tokens"] = 1024
    text = json.dumps(cfg, ensure_ascii=False, indent=2)
    (ws / "config.json").write_text(text, encoding="utf-8")
    digest = sha256_bytes(text.encode("utf-8"))
    (live / "config.hash.after").write_text(f"{digest}\n", encoding="utf-8")
    (live / "config.hash.after-reload").write_text(f"{digest}\n", encoding="utf-8")
    redacted = json.loads(json.dumps(cfg))
    redacted["model_list"][0]["api_key"] = "REDACTED"
    (live / "config.reload.redacted.json").write_text(
        json.dumps(redacted, ensure_ascii=False, indent=2), encoding="utf-8"
    )


def _mutate_reload_tamper(live: Path):
    """存在计划内 /reload 记录，但 after 哈希与计划值不一致 => 运行期被额外改写。"""
    _scenario_planned_reload(live)
    (live / "config.hash.after").write_text("0" * 64 + "\n", encoding="utf-8")


SELFTEST_GOOD_SCENARIOS = [
    ("planned_reload", _scenario_planned_reload),
    ("loop_guard_compacted", _scenario_loop_guard_compacted),
    ("summarized_head_orphan", _scenario_summarized_head_orphan),
    ("kinds_compacted_with_evidence", _scenario_kinds_compacted_with_evidence),
]

SELFTEST_MUTATIONS = [
    ("broken_jsonl", _mutate_jsonl_broken, ["session.parse_jsonl"]),
    ("tool_unpaired", _mutate_tool_unpaired, ["session.tool_pairing"]),
    ("meta_count", _mutate_meta_count, ["meta.count_consistency"]),
    ("spill_perms", _mutate_spill_perms, ["spill.file_mode"]),
    ("config_hash", _mutate_config_hash, ["config.hash_before_after"]),
    ("project_bug", _mutate_project_bug, ["project.tests_pass"]),
    ("marker_delta", _mutate_marker_delta, ["markers.agents_delta"]),
    ("taskstate_phase", _mutate_taskstate_phase, ["meta.task_state_schema"]),
    ("claim_contradiction", _mutate_claim_contradiction, ["harness.contradictions", "session.tool_pairing"]),
    ("log_missing_tool", _mutate_log_missing_tool, ["log.tool_calls"]),
    ("spill_content", _mutate_spill_content, ["spill.marker_refs"]),
    ("shutdown_missing", _mutate_shutdown_missing, ["log.shutdown"]),
    ("reload_tamper", _mutate_reload_tamper, ["config.hash_before_after"]),
    ("loop_guard_lost", _mutate_loop_guard_lost, ["session.loop_guard_present"]),
    ("orphan_midhistory", _mutate_orphan_midhistory, ["session.tool_pairing"]),
    ("kinds_lost_no_evidence", _mutate_kinds_lost_no_evidence, ["session.context_kinds_present"]),
]


def _prune_old_selftest_runs(base_out: Path, keep: int = 3):
    root = Path(base_out) / "selftest"
    if not root.is_dir():
        return
    runs = sorted((p for p in root.iterdir() if p.is_dir()), key=lambda p: p.name)
    for old in runs[:-keep]:
        shutil.rmtree(old, ignore_errors=True)


def run_self_test(base_out: Path, timeout_sec: int = 120) -> int:
    stamp = time.strftime("%Y%m%d-%H%M%S")
    work = Path(base_out) / "selftest" / stamp
    if work.exists():
        shutil.rmtree(work)
    work.mkdir(parents=True, exist_ok=True)
    _prune_old_selftest_runs(base_out)
    transcript: list[str] = []

    def emit(message: str = ""):
        transcript.append(message)
        print(message)

    emit(f"[self-test] 工作目录: {work}")

    good_live = work / "good" / "live"
    build_fixture(good_live)
    expectations, source = load_expectations(good_live, None)
    report = verify(good_live, expectations, timeout_sec=timeout_sec)
    _write_json(work / "good-report.json", report)
    good_ok = report["overall_status"] == PASS
    emit(f"[self-test] 基线 fixture: {report['overall_status']} (expectation={source})")

    results = []
    all_ok = good_ok
    if not good_ok:
        for check in report["checks"]:
            if check["status"] in (FAIL, WARN) and check["severity"] == "required":
                emit(f"[self-test]   基线失败: [{check['id']}] {check['detail']}")
                for evidence in check["evidence"][:3]:
                    emit(f"[self-test]      - {evidence}")
    good_scenarios = []
    for name, apply_scenario in SELFTEST_GOOD_SCENARIOS:
        variant = work / f"scenario-{name}" / "live"
        shutil.copytree(good_live, variant)
        _rebase_fixture_paths(good_live, variant)
        apply_scenario(variant)
        scenario_report = verify(variant, load_expectations(variant, None)[0], timeout_sec=timeout_sec)
        _write_json(work / f"scenario-{name}-report.json", scenario_report)
        scenario_ok = scenario_report["overall_status"] == PASS
        all_ok = all_ok and scenario_ok
        good_scenarios.append(
            {
                "scenario": name,
                "expect": PASS,
                "actual": scenario_report["overall_status"],
                "ok": scenario_ok,
                "counts": scenario_report["counts"],
            }
        )
        emit(f"[self-test] 正例场景 {name:<18} overall={scenario_report['overall_status']} 期望=PASS")
        if not scenario_ok:
            for check in scenario_report["checks"]:
                if check["severity"] == "required" and check["status"] == FAIL:
                    emit(f"[self-test]   场景失败: [{check['id']}] {check['detail']}")
                    for evidence in check["evidence"][:3]:
                        emit(f"[self-test]      - {evidence}")

    for name, mutate, expected_ids in SELFTEST_MUTATIONS:
        variant = work / f"mut-{name}" / "live"
        shutil.copytree(good_live, variant)
        # fixture 内的 absolute spill 路径随目录移动需要重写，否则会误报路径逃逸
        _rebase_fixture_paths(good_live, variant)
        mutate(variant)
        variant_report = verify(variant, load_expectations(variant, None)[0], timeout_sec=timeout_sec)
        _write_json(work / f"mut-{name}-report.json", variant_report)
        failed_ids = [
            check["id"]
            for check in variant_report["checks"]
            if check["severity"] == "required" and check["status"] == FAIL
        ]
        detected = any(any(expected in fid for fid in failed_ids) for expected in expected_ids)
        all_ok = all_ok and detected and variant_report["overall_status"] == FAIL
        results.append(
            {
                "mutation": name,
                "expected_failure_ids": expected_ids,
                "detected": detected,
                "overall": variant_report["overall_status"],
                "required_failures": failed_ids,
            }
        )
        emit(
            f"[self-test] 变异 {name:<22} 检出={'✓' if detected else '✗'} "
            f"overall={variant_report['overall_status']} 失败项={failed_ids[:6]}"
        )

    selftest_report = {
        "generated_at": now_iso(),
        "work_dir": str(work),
        "baseline_overall": report["overall_status"],
        "baseline_pass": good_ok,
        "baseline_counts": report["counts"],
        "good_scenarios": good_scenarios,
        "mutations": results,
        "all_detected": all(
            item["detected"] and item["overall"] == FAIL for item in results
        ),
        "result": "PASS" if all_ok else "FAIL",
    }
    _write_json(Path(base_out) / "selftest.json", selftest_report)
    emit("=" * 78)
    if all_ok:
        emit(f"[self-test] PASS：基线全绿，{len(results)} 组变异全部被检出。")
        emit(f"[self-test] 报告: {Path(base_out) / 'selftest.json'}")
        exit_code = 0
    else:
        emit("[self-test] FAIL：基线或变异检出不符合预期（详见 selftest.json）。")
        exit_code = 1
    (Path(base_out) / "selftest.log").write_text("\n".join(transcript) + "\n", encoding="utf-8")
    return exit_code


def _write_json(path: Path, data):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(data, ensure_ascii=False, indent=2, default=str) + "\n", encoding="utf-8")


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="独立验证真实场景 E2E artifacts（不信任 harness results.json）",
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    parser.add_argument("--artifacts", help="artifacts 目录（如 build/functest-20261030/live）")
    parser.add_argument("--json-out", help="JSON 报告输出路径")
    parser.add_argument("--expect", help="expectations JSON 路径（默认自动探测）")
    parser.add_argument("--timeout", type=int, default=None, help="项目测试复跑超时秒数（默认 180）")
    parser.add_argument(
        "--known-failure",
        action="append",
        default=[],
        help="已确认的生产缺陷对应的断言 id（可重复）；标记 expected-known-failure，不计入矛盾清单/overall",
    )
    parser.add_argument("--self-test", action="store_true", help="运行内置合成 fixture 自测（不依赖真实运行）")
    parser.add_argument("--selftest-dir", default="build/functest-20261030/verify", help="自测与报告写入目录")
    parser.add_argument("--verbose", action="store_true", help="打印每条断言")
    args = parser.parse_args(argv)

    if args.self_test:
        return run_self_test(Path(args.selftest_dir), timeout_sec=args.timeout or 120)

    if not args.artifacts:
        parser.error("需要 --artifacts DIR（或使用 --self-test）")
    artifacts = Path(args.artifacts)
    if not artifacts.is_dir():
        print(f"[fatal] artifacts 目录不存在: {artifacts}", file=sys.stderr)
        return 2
    try:
        expectations, source = load_expectations(artifacts, args.expect)
    except SystemExit as exc:
        print(str(exc), file=sys.stderr)
        return 2
    known = list(expectations.get("known_failures") or [])
    for item in args.known_failure or []:
        if item not in known:
            known.append(item)
    expectations["known_failures"] = known
    report = verify(artifacts, expectations, timeout_sec=args.timeout)
    report["expectations_source"] = source
    if args.verbose:
        for check in report["checks"]:
            print(f"{STATUS_ICON.get(check['status'], '?')} [{check['id']}] {check['title']}: {check['detail']}")
    print(render_summary(report))
    if args.json_out:
        _write_json(Path(args.json_out), report)
        print(f"JSON 报告: {args.json_out}")
    return report["exit_code"]


if __name__ == "__main__":
    sys.exit(main())