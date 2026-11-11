#!/usr/bin/env python3
"""用于配置能力测试的进程 hook 夹具（通过 stdio 传输 JSON-RPC）。

实现 docs/architecture/hooks/README.zh.md 中描述的 Chagent 进程 hook 协议：

  - hook.hello            （请求/响应握手）
  - hook.runtime_event    （通知，observe 模式）
  - hook.before_llm       （请求/响应）
  - hook.after_llm        （请求/响应）
  - hook.before_tool      （请求/响应）
  - hook.after_tool       （请求/响应）
  - hook.approve_tool     （请求/响应）

通过环境变量选择行为，使同一个夹具即可覆盖整个 E 场景矩阵：

  CFGCAP_HOOK_LOG_FILE    用于追加记录每个已观测请求/响应的 JSONL 文件
  CFGCAP_HOOK_MODE         observe | observe_modify | modify_after_tool | deny_tool | approve_false
  CFGCAP_HOOK_TARGET_TOOL  拦截器所应用的工具名（默认：exec）
  CFGCAP_HOOK_MODIFIED_TAG 插入到 after_tool 结果中的标记
                           （默认：HOOK_MODIFIED）
  CFGCAP_HOOK_DENY_REASON  deny_tool / 审批拒绝的原因字符串
"""

from __future__ import annotations

import json
import os
import signal
import sys
from datetime import datetime, timezone
from typing import Any

LOG_FILE = os.environ.get("CFGCAP_HOOK_LOG_FILE", "").strip()
MODE = os.environ.get("CFGCAP_HOOK_MODE", "observe").strip() or "observe"
TARGET_TOOL = os.environ.get("CFGCAP_HOOK_TARGET_TOOL", "exec").strip() or "exec"
MODIFIED_TAG = os.environ.get("CFGCAP_HOOK_MODIFIED_TAG", "HOOK_MODIFIED").strip() or "HOOK_MODIFIED"
DENY_REASON = os.environ.get("CFGCAP_HOOK_DENY_REASON", "CFGCAP_DENIED_BY_HOOK").strip() or "CFGCAP_DENIED_BY_HOOK"


def append_log(entry: dict[str, Any]) -> None:
    if not LOG_FILE:
        return
    payload = {"ts": datetime.now(timezone.utc).isoformat(), **entry}
    try:
        log_dir = os.path.dirname(LOG_FILE)
        if log_dir:
            os.makedirs(log_dir, exist_ok=True)
        with open(LOG_FILE, "a", encoding="utf-8") as handle:
            handle.write(json.dumps(payload, ensure_ascii=True) + "\n")
    except OSError as exc:
        log_stderr(f"failed to write hook log file {LOG_FILE}: {exc}")


def log_stderr(message: str) -> None:
    try:
        sys.stderr.write(message + "\n")
        sys.stderr.flush()
    except (BrokenPipeError, ValueError):
        raise SystemExit(0) from None


def send_response(message_id: int, result: Any | None = None, error: str | None = None) -> None:
    payload: dict[str, Any] = {"jsonrpc": "2.0", "id": message_id}
    if error is not None:
        payload["error"] = {"code": -32000, "message": error}
    else:
        payload["result"] = result if result is not None else {}
    append_log({"direction": "out", "id": message_id, "response": payload.get("result"), "error": payload.get("error")})
    try:
        sys.stdout.write(json.dumps(payload, ensure_ascii=True) + "\n")
        sys.stdout.flush()
    except (BrokenPipeError, ValueError):
        raise SystemExit(0) from None


def summarize_params(method: str, params: dict[str, Any]) -> dict[str, Any]:
    """为高频 LLM hook 保持 hook 日志精简。"""
    if method in ("hook.before_llm", "hook.after_llm"):
        messages = params.get("messages")
        return {
            "model": params.get("model"),
            "message_count": len(messages) if isinstance(messages, list) else None,
        }
    return params


def tool_name_of(params: dict[str, Any]) -> str:
    return str(params.get("tool") or "")


def is_target_tool(params: dict[str, Any]) -> bool:
    return tool_name_of(params) == TARGET_TOOL


def handle_before_tool(params: dict[str, Any]) -> dict[str, Any]:
    if MODE == "deny_tool" and is_target_tool(params):
        return {"action": "deny_tool", "reason": DENY_REASON}
    return {"action": "continue"}


def handle_after_tool(params: dict[str, Any]) -> dict[str, Any]:
    if MODE not in ("modify_after_tool", "observe_modify") or not is_target_tool(params):
        return {"action": "continue"}
    updated = dict(params)
    result = updated.get("result")
    if not isinstance(result, dict):
        return {"action": "continue"}
    result = dict(result)
    original = str(result.get("for_llm") or "")
    result["for_llm"] = f"{MODIFIED_TAG} {original}".strip()
    result.setdefault("silent", False)
    result.setdefault("is_error", False)
    updated["result"] = result
    return {"action": "modify", "result": updated}


def handle_approve_tool(params: dict[str, Any]) -> dict[str, Any]:
    if MODE == "approve_false" and is_target_tool(params):
        return {"approved": False, "reason": DENY_REASON}
    return {"approved": True}


def handle_request(method: str, params: dict[str, Any]) -> dict[str, Any]:
    if method == "hook.hello":
        return {"ok": True, "name": "cfgcap-hook-gate"}
    if method == "hook.before_tool":
        return handle_before_tool(params)
    if method == "hook.after_tool":
        return handle_after_tool(params)
    if method == "hook.approve_tool":
        return handle_approve_tool(params)
    if method == "hook.before_llm":
        return {"action": "continue"}
    if method == "hook.after_llm":
        return {"action": "continue"}
    raise KeyError(f"method not found: {method}")


def handle_shutdown_signal(signum: int, _frame: Any) -> None:
    raise KeyboardInterrupt(f"received signal {signum}")


def main() -> int:
    append_log({"direction": "start", "pid": os.getpid(), "mode": MODE, "target_tool": TARGET_TOOL})
    try:
        for raw_line in sys.stdin:
            line = raw_line.strip()
            if not line:
                continue
            try:
                message = json.loads(line)
            except json.JSONDecodeError as exc:
                append_log({"direction": "in", "decode_error": str(exc), "raw": line[:500]})
                continue

            method = str(message.get("method") or "")
            message_id = message.get("id")
            params = message.get("params") or {}
            if not isinstance(params, dict):
                params = {}

            append_log(
                {
                    "direction": "in",
                    "id": message_id,
                    "method": method,
                    "params": summarize_params(method, params),
                    "notification": message_id is None,
                }
            )

            if message_id is None:
                if method == "hook.runtime_event":
                    log_stderr(f"observed event: {params.get('kind')}")
                continue

            try:
                result = handle_request(method, params)
            except KeyError as exc:
                send_response(int(message_id), error=str(exc))
                continue
            except Exception as exc:  # pragma: no cover - defensive
                send_response(int(message_id), error=f"unexpected error: {exc}")
                continue
            send_response(int(message_id), result=result)
    except KeyboardInterrupt:
        return 0

    return 0


if __name__ == "__main__":
    signal.signal(signal.SIGINT, handle_shutdown_signal)
    signal.signal(signal.SIGTERM, handle_shutdown_signal)
    raise SystemExit(main())
