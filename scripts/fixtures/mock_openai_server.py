#!/usr/bin/env python3
"""用于健壮性测试的确定性 OpenAI 兼容 mock 供应商。

该服务器从不调用真实模型。它回放一份 JSON "plan"（或单个具名场景），
使网关在供应商故障和模型输出畸形时的行为可复现，且不受网络抖动影响。

用法：
    mock_openai_server.py --port-file PORT.txt --log REQ.jsonl \
        [--plan PLAN.json] [--scenario ok] [--delay 0]

运行时控制（无需重启）：
    POST http://127.0.0.1:<port>/__control
    {"scenario": "ok" | "plan" | "http_500" | "http_429" | "context_400" |
                 "malformed_json" | "timeout" | "stream_cut" | "stream_malformed",
     "plan": [...], "idx": 0, "delay": 0, "content": "MOCK_OK"}
    GET  http://127.0.0.1:<port>/__state   -> {"requests": N, "idx": M, "scenario": ...}

计划条目（每个 chat 请求消费一条；--loop 时最后一条会重复）：
    {"kind": "text", "content": "hello", "finish_reason": "stop"}
    {"kind": "empty"}                                  # content 为空串（模型没有返回任何内容）
    {"kind": "tool", "name": "exec", "arguments": {"command": "echo hi"}}
    {"kind": "tool", "name": "read_file", "arguments_raw": "{not json"}
    {"kind": "tools", "calls": [{"name": ..., "arguments": {...}}, ...]}
    {"kind": "mixed", "content": "partial", "tool": {"name": ..., "arguments": {...}}}
    {"kind": "unknown_tool"}                           # 未注册的 tool_calls 名称
    {"kind": "huge_args"}                              # 5 万字符的 read_file path
    {"kind": "http", "status": 500, "body": {...}}     # 任意 HTTP 错误
    {"kind": "malformed", "body": "{not json"}         # 返回 200 但 JSON 无效
    {"kind": "sleep", "seconds": 8, "content": "late"} # 供应商侧延迟
    {"kind": "stream_cut"}                             # SSE 在 JSON 中途截断
    {"kind": "stream_malformed"}                       # 含无效 JSON 事件的 SSE
    {"kind": "weird_finish", "finish_reason": "length", "content": "truncated"}

每个 chat 请求都会作为一行 JSON 追加写入 --log（seq、scenario、stream、
消息数、最后一条用户消息预览、工具数），以便测试证明网关确实与 mock 通信，
并统计重试/回退次数。
"""

from __future__ import annotations

import argparse
import json
import os
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

LOCK = threading.Lock()
STATE = {
    "scenario": "ok",
    "plan": [],
    "idx": 0,
    "delay": 0.0,
    "content": "MOCK_OK",
    "loop": True,
    "requests": 0,
    "log": "",
}


def append_log(entry: dict) -> None:
    if not STATE["log"]:
        return
    with LOCK:
        try:
            with open(STATE["log"], "a", encoding="utf-8") as handle:
                handle.write(json.dumps(entry, ensure_ascii=False) + "\n")
        except OSError:
            pass


def next_step() -> tuple[dict, int]:
    with LOCK:
        idx = STATE["idx"]
        STATE["idx"] += 1
        plan = STATE["plan"]
    if plan and idx < len(plan):
        return plan[idx], idx
    if plan and STATE["loop"]:
        return plan[-1], idx
    return {"kind": "text", "content": "PLAN_EXHAUSTED"}, idx


def tool_call(name: str, arguments=None, arguments_raw: str | None = None,
              call_id: str = "call_1") -> dict:
    raw = arguments_raw if arguments_raw is not None else json.dumps(arguments or {}, ensure_ascii=False)
    return {"id": call_id, "type": "function",
            "function": {"name": name, "arguments": raw}}


def completion_body(model: str, content: str = "", tool_calls=None,
                    finish_reason: str = "stop") -> dict:
    message = {"role": "assistant", "content": content}
    if tool_calls:
        message["tool_calls"] = tool_calls
    return {
        "id": "chatcmpl-mock",
        "object": "chat.completion",
        "created": 0,
        "model": model,
        "choices": [{"index": 0, "message": message, "finish_reason": finish_reason}],
        "usage": {"prompt_tokens": 12, "completion_tokens": 8, "total_tokens": 20},
    }


def plan_step_to_response(step: dict, model: str, seq: int) -> tuple[int, bytes, str]:
    """返回计划步骤对应的 (status, body_bytes, content_type)。"""
    kind = step.get("kind") or "text"
    if kind == "http":
        status = int(step.get("status") or 500)
        body = step.get("body")
        if body is None:
            body = {"error": {"message": f"mock http error {status}", "type": "mock_error"}}
        if isinstance(body, str):
            return status, body.encode(), "application/json"
        return status, json.dumps(body).encode(), "application/json"
    if kind == "malformed":
        return 200, str(step.get("body") or "{not json").encode(), "application/json"
    if kind == "empty":
        return 200, json.dumps(completion_body(model, "")).encode(), "application/json"
    if kind == "unknown_tool":
        return 200, json.dumps(completion_body(
            model,
            tool_calls=[tool_call("totally_unknown_tool", {"x": 1}, call_id=f"call_{seq}")],
            finish_reason="tool_calls",
        )).encode(), "application/json"
    if kind == "huge_args":
        return 200, json.dumps(completion_body(
            model,
            tool_calls=[tool_call("read_file", {"path": "a" * 50000}, call_id=f"call_{seq}")],
            finish_reason="tool_calls",
        )).encode(), "application/json"
    if kind in ("tool", "tools", "mixed", "weird_finish", "text"):
        content = str(step.get("content") or "")
        finish = str(step.get("finish_reason") or "stop")
        calls = []
        if kind == "tool":
            calls = [tool_call(step.get("name") or "exec",
                               step.get("arguments"),
                               step.get("arguments_raw"),
                               call_id=f"call_{seq}")]
            finish = "tool_calls"
        elif kind == "tools":
            calls = [tool_call(call.get("name") or "exec", call.get("arguments"),
                               call.get("arguments_raw"), call_id=f"call_{seq}_{i+1}")
                     for i, call in enumerate(step.get("calls") or [])]
            finish = "tool_calls"
        elif kind == "mixed":
            tool = step.get("tool") or {}
            calls = [tool_call(tool.get("name") or "exec", tool.get("arguments"),
                               tool.get("arguments_raw"), call_id=f"call_{seq}")]
            finish = "tool_calls"
        elif kind == "weird_finish":
            finish = str(step.get("finish_reason") or "length")
        return 200, json.dumps(completion_body(model, content, calls, finish)).encode(), "application/json"
    if kind == "sleep":
        time.sleep(float(step.get("seconds") or 5))
        return 200, json.dumps(completion_body(model, str(step.get("content") or "LATE_OK"))).encode(), "application/json"
    return 200, json.dumps(completion_body(model, "MOCK_OK")).encode(), "application/json"


def scenario_step(scenario: str, seq: int) -> dict:
    if scenario == "ok":
        return {"kind": "text", "content": STATE["content"] or "MOCK_OK"}
    if scenario == "http_500":
        return {"kind": "http", "status": 500}
    if scenario == "http_429":
        return {"kind": "http", "status": 429,
                "body": {"error": {"message": "rate limit exceeded", "type": "rate_limit_error"}}}
    if scenario == "context_400":
        return {"kind": "http", "status": 400,
                "body": {"error": {"message": "This model's maximum context length is 8192 tokens",
                                   "code": "context_length_exceeded", "type": "invalid_request_error"}}}
    if scenario == "malformed_json":
        return {"kind": "malformed"}
    if scenario == "timeout":
        return {"kind": "sleep", "seconds": 8, "content": "LATE_OK"}
    if scenario == "stream_cut":
        return {"kind": "stream_cut"}
    if scenario == "stream_malformed":
        return {"kind": "stream_malformed"}
    return {"kind": "text", "content": STATE["content"] or "MOCK_OK"}


def sse_chunk(delta: dict, finish=None) -> bytes:
    payload = {
        "id": "chatcmpl-mock",
        "object": "chat.completion.chunk",
        "created": 0,
        "model": "mock-model",
        "choices": [{"index": 0, "delta": delta, "finish_reason": finish}],
    }
    return ("data: " + json.dumps(payload, ensure_ascii=False) + "\n\n").encode()


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        pass

    # -- 辅助函数 ---------------------------------------------------
    def _send(self, status: int, body: bytes, content_type: str = "application/json") -> None:
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _send_json(self, obj, status=200) -> None:
        self._send(status, json.dumps(obj, ensure_ascii=False).encode())

    # -- 路由 -------------------------------------------------------
    def do_GET(self):  # noqa: N802
        if self.path.rstrip("/").endswith("/models"):
            self._send_json({"object": "list", "data": [{"id": "mock-model", "object": "model"}]})
        elif self.path.startswith("/__state"):
            with LOCK:
                state = {k: STATE[k] for k in ("scenario", "idx", "requests", "delay")}
            self._send_json(state)
        else:
            self._send_json({"error": "not found"}, 404)

    def do_POST(self):  # noqa: N802
        if self.path.startswith("/__control"):
            length = int(self.headers.get("Content-Length") or 0)
            try:
                payload = json.loads(self.rfile.read(length) or b"{}")
            except json.JSONDecodeError:
                self._send_json({"ok": False, "error": "bad control json"}, 400)
                return
            with LOCK:
                if "scenario" in payload:
                    STATE["scenario"] = str(payload["scenario"])
                if "plan" in payload:
                    STATE["plan"] = list(payload.get("plan") or [])
                if "idx" in payload:
                    STATE["idx"] = int(payload.get("idx") or 0)
                if "delay" in payload:
                    STATE["delay"] = float(payload.get("delay") or 0)
                if "content" in payload:
                    STATE["content"] = str(payload.get("content") or "")
            self._send_json({"ok": True})
            return

        if not self.path.rstrip("/").endswith("/chat/completions"):
            self._send_json({"error": "not found"}, 404)
            return

        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length)
        try:
            req = json.loads(raw or b"{}")
        except json.JSONDecodeError:
            self._send_json({"error": {"message": "bad request json"}}, 400)
            return
        model = req.get("model") or "mock-model"
        messages = req.get("messages") or []
        user_tail = ""
        for message in reversed(messages):
            if message.get("role") == "user":
                user_tail = str(message.get("content") or "")[:200]
                break
        with LOCK:
            scenario = STATE["scenario"]
            delay = STATE["delay"]
            STATE["requests"] += 1
            seq = STATE["requests"]
        step, plan_idx = (next_step() if scenario == "plan" else (scenario_step(scenario, seq), seq - 1))
        append_log({
            "seq": seq,
            "scenario": scenario,
            "plan_idx": plan_idx,
            "stream": bool(req.get("stream")),
            "messages": len(messages),
            "last_role": messages[-1].get("role") if messages else "",
            "user_tail": user_tail,
            "tools": len(req.get("tools") or []),
            "step_kind": step.get("kind"),
        })
        if delay > 0:
            time.sleep(delay)
        if req.get("stream"):
            self._stream(step, model, seq)
            return
        status, body, content_type = plan_step_to_response(step, model, seq)
        self._send(status, body, content_type)

    # -- 流式传输 ---------------------------------------------------
    def _stream(self, step: dict, model: str, seq: int) -> None:
        kind = step.get("kind") or "text"
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "close")
        self.end_headers()
        try:
            if kind == "stream_cut":
                self.wfile.write(sse_chunk({"content": "partial "}))
                self.wfile.flush()
                # 在 JSON 中途截断：不发送末尾空行，然后关闭 socket
                self.wfile.write(b'data: {"choices": [{"delta": {"content": "cut')
                self.wfile.flush()
                self.close_connection = True
                return
            if kind == "stream_malformed":
                self.wfile.write(sse_chunk({"content": "partial "}))
                self.wfile.write(b"data: {not-a-json-event}\n\n")
                self.wfile.flush()
                self.close_connection = True
                return
            if kind == "malformed":
                self.wfile.write(b"data: {not-json}\n\n")
                self.wfile.flush()
                return
            if kind == "http":
                self.wfile.write(b"")
                return
            # 常规文本/工具流式输出
            status, body, _ = plan_step_to_response(step, model, seq)
            if status != 200:
                self.wfile.write(b"")
                return
            parsed = json.loads(body)
            message = parsed["choices"][0]["message"]
            if message.get("content"):
                for piece in [message["content"][i:i + 4] for i in range(0, len(message["content"]), 4)]:
                    self.wfile.write(sse_chunk({"content": piece}))
            for index, call in enumerate(message.get("tool_calls") or []):
                self.wfile.write(sse_chunk({"tool_calls": [{
                    "index": index,
                    "id": call.get("id"),
                    "function": {"name": call["function"]["name"],
                                 "arguments": call["function"]["arguments"]},
                }]}))
            self.wfile.write(sse_chunk({}, finish=str(parsed["choices"][0].get("finish_reason") or "stop")))
            self.wfile.write(b"data: [DONE]\n\n")
            self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            pass


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--port-file", required=True)
    parser.add_argument("--log", required=True)
    parser.add_argument("--plan")
    parser.add_argument("--scenario", default="ok")
    parser.add_argument("--delay", type=float, default=0.0)
    parser.add_argument("--no-loop", action="store_true")
    args = parser.parse_args()

    if args.plan:
        with open(args.plan, "r", encoding="utf-8") as handle:
            plan = json.load(handle)
    else:
        plan = []
    STATE.update(scenario=args.scenario, plan=plan, delay=args.delay,
                 loop=not args.no_loop, log=args.log)

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    port = server.server_address[1]
    tmp = args.port_file + ".tmp"
    with open(tmp, "w", encoding="utf-8") as handle:
        handle.write(str(port))
    os.replace(tmp, args.port_file)
    print(f"mock_openai_server listening on 127.0.0.1:{port} "
          f"(scenario={args.scenario}, plan_steps={len(plan)})", flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
