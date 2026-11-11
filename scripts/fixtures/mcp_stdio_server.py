#!/usr/bin/env python3
"""用于功能/E2E 测试的最小 MCP stdio 服务器夹具。

在 stdin/stdout 上以换行分隔的 JSON-RPC 2.0 实现 Model Context Protocol
（Go MCP 客户端在 `type: "stdio"` 时使用的传输方式）。

支持：
  - initialize / notifications/initialized
  - ping
  - tools/list  -> echo(text)，返回 "$CFGCAP_MCP_ECHO_PREFIX:<text>"
  - tools/call  -> echo / echo_dynamic
  - 运行期动态增删工具（可选，用于验证 notifications/tools/list_changed）：
      * 设置 $CFGCAP_MCP_CONTROL_FILE 后，后台线程每 200ms 轮询该文件；
      * 文件内容为 "add"    -> 增加 echo_dynamic 工具并发送 tools/list_changed；
      * 文件内容为 "remove" -> 移除 echo_dynamic 工具并发送 tools/list_changed。
    这样 E2E 可以在“真实模型调用 echo 成功后”精确触发工具列表变更，
    断言 agent 是否重注册工具（无需依赖 sleep 竞态）。

只依赖 Python 标准库。诊断信息输出到 stderr，以及由 $CFGCAP_MCP_LOG_FILE
指定的可选 JSONL 日志文件，便于测试 harness 断言服务器确实收到了这些调用。
"""

from __future__ import annotations

import json
import os
import sys
import threading
import time
from datetime import datetime, timezone

LOG_FILE = os.environ.get("CFGCAP_MCP_LOG_FILE", "").strip()
SERVER_NAME = os.environ.get("CFGCAP_MCP_SERVER_NAME", "cfgcap")
ECHO_PREFIX = os.environ.get("CFGCAP_MCP_ECHO_PREFIX", "MCP_ECHO")
CONTROL_FILE = os.environ.get("CFGCAP_MCP_CONTROL_FILE", "").strip()

DYNAMIC_TOOL = "echo_dynamic"

SUPPORTED_PROTOCOL_VERSIONS = {
    "2026-06-30",
    "2025-11-25",
    "2025-06-18",
    "2025-03-26",
    "2024-11-05",
}
FALLBACK_PROTOCOL_VERSION = "2025-06-18"

_STATE_LOCK = threading.Lock()
_OUT_LOCK = threading.Lock()
_dynamic_enabled = False


def log(entry: dict) -> None:
    payload = {"ts": datetime.now(timezone.utc).isoformat(), **entry}
    line = json.dumps(payload, ensure_ascii=True)
    if LOG_FILE:
        try:
            log_dir = os.path.dirname(LOG_FILE)
            if log_dir:
                os.makedirs(log_dir, exist_ok=True)
            with open(LOG_FILE, "a", encoding="utf-8") as handle:
                handle.write(line + "\n")
        except OSError as exc:  # pragma: no cover - best effort
            print(f"mcp fixture log write failed: {exc}", file=sys.stderr)
    print(line, file=sys.stderr, flush=True)


def write_message(message: dict) -> None:
    with _OUT_LOCK:
        sys.stdout.write(json.dumps(message, ensure_ascii=True) + "\n")
        sys.stdout.flush()


def respond(message_id, result=None, error=None) -> None:
    payload = {"jsonrpc": "2.0", "id": message_id}
    if error is not None:
        payload["error"] = error
    else:
        payload["result"] = result if result is not None else {}
    write_message(payload)


def _echo_tool(name: str, description: str) -> dict:
    return {
        "name": name,
        "description": description,
        "inputSchema": {
            "type": "object",
            "properties": {
                "text": {
                    "type": "string",
                    "description": "Text to echo back.",
                }
            },
            "required": ["text"],
        },
    }


def tool_definitions() -> list[dict]:
    tools = [_echo_tool("echo", "Echo back the provided text with a configurable marker prefix.")]
    with _STATE_LOCK:
        dynamic = _dynamic_enabled
    if dynamic:
        tools.append(
            _echo_tool(
                DYNAMIC_TOOL,
                "Dynamically added echo tool (runtime tools/list_changed fixture).",
            )
        )
    return tools


def handle_tools_call(params: dict) -> dict:
    name = str(params.get("name") or "")
    arguments = params.get("arguments") or {}
    if not isinstance(arguments, dict):
        arguments = {}
    log({"direction": "tool_call", "tool": name, "arguments": arguments})
    if name not in ("echo", DYNAMIC_TOOL):
        return {
            "content": [{"type": "text", "text": f"unknown tool: {name}"}],
            "isError": True,
        }
    text = str(arguments.get("text", ""))
    return {
        "content": [{"type": "text", "text": f"{ECHO_PREFIX}:{text}"}],
        "isError": False,
    }


def notify_tool_list_changed() -> None:
    write_message(
        {
            "jsonrpc": "2.0",
            "method": "notifications/tools/list_changed",
            "params": {},
        }
    )


def set_dynamic_tool(enabled: bool) -> None:
    global _dynamic_enabled
    with _STATE_LOCK:
        if _dynamic_enabled == enabled:
            return
        _dynamic_enabled = enabled
    count = len(tool_definitions())
    log(
        {
            "direction": "dynamic_tools",
            "action": "add" if enabled else "remove",
            "tool_count": count,
        }
    )
    notify_tool_list_changed()


def control_file_poller() -> None:
    """监视 $CFGCAP_MCP_CONTROL_FILE 中的 "add"/"remove" 命令。"""
    last_command = ""
    while True:
        command = ""
        try:
            with open(CONTROL_FILE, encoding="utf-8") as handle:
                command = handle.read().strip().lower()
        except OSError:
            command = ""
        if command in ("add", "remove") and command != last_command:
            last_command = command
            set_dynamic_tool(command == "add")
        time.sleep(0.2)


def handle_request(method: str, params: dict) -> tuple[dict | None, dict | None]:
    """返回 (result, error)。"""
    if method == "initialize":
        requested = str(params.get("protocolVersion") or "")
        version = requested if requested in SUPPORTED_PROTOCOL_VERSIONS else FALLBACK_PROTOCOL_VERSION
        log({"direction": "initialize", "requested": requested, "negotiated": version})
        return (
            {
                "protocolVersion": version,
                # listChanged=true：服务器可在运行期动态增删工具
                #（动态夹具模式）；Go 客户端会为其注册处理器。
                "capabilities": {"tools": {"listChanged": True}},
                "serverInfo": {"name": f"{SERVER_NAME}-fixture", "version": "1.0.0"},
            },
            None,
        )
    if method == "ping":
        return {}, None
    if method == "tools/list":
        tools = tool_definitions()
        log({"direction": "tools_list", "count": len(tools)})
        return {"tools": tools}, None
    if method == "tools/call":
        return handle_tools_call(params), None
    if method in ("shutdown", "exit"):
        return {}, None
    return None, {"code": -32601, "message": f"method not found: {method}"}


def main() -> int:
    log(
        {
            "direction": "start",
            "pid": os.getpid(),
            "server": SERVER_NAME,
            "dynamic_control": bool(CONTROL_FILE),
        }
    )
    if CONTROL_FILE:
        threading.Thread(target=control_file_poller, daemon=True).start()

    for raw_line in sys.stdin:
        line = raw_line.strip()
        if not line:
            continue
        try:
            message = json.loads(line)
        except json.JSONDecodeError as exc:
            log({"direction": "decode_error", "error": str(exc), "raw": line[:500]})
            continue

        method = str(message.get("method") or "")
        message_id = message.get("id")
        params = message.get("params") or {}
        if not isinstance(params, dict):
            params = {}

        if message_id is None:
            # 通知消息（例如 notifications/initialized）：不返回响应。
            log({"direction": "notification", "method": method})
            continue

        log({"direction": "request", "id": message_id, "method": method})
        result, error = handle_request(method, params)
        respond(message_id, result=result, error=error)
        if method == "shutdown":
            break

    log({"direction": "stop"})
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (BrokenPipeError, KeyboardInterrupt):
        raise SystemExit(0)