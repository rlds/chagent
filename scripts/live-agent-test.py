#!/usr/bin/env python3
"""棋类基础智能体的真实模型功能测试。

从外部配置读取 model_list（默认：chess/weichat 的配置），用棋类默认值构建临时
工作区、启动 gateway，并完整验证 pico 聊天、工作区指令、/context、任务工具与
生命周期。

用法：scripts/live-agent-test.py [/path/to/config.json]
依赖：python websockets；go 工具链。
"""
import asyncio
import json
import os
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import urllib.request

import websockets

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DEFAULT_CONFIG = "/Users/wudaorui/chessdata/weichat/weichat/config.json"



def build_binary(dst):
    subprocess.run(["go", "build", "-o", dst, "./cmd/chagent"], cwd=ROOT, check=True)


def prepare_workspace(src_cfg_path, ws):
    src = json.load(open(src_cfg_path, encoding="utf-8"))
    model_list = []
    for model in src["model_list"]:
        entry = {
            "model_name": model["model_name"],
            "model": model.get("model", ""),
            "api_base": model.get("api_base", ""),
        }
        if model.get("provider"):
            entry["provider"] = model["provider"]
        keys = model.get("api_keys") or ([model["api_key"]] if model.get("api_key") else [])
        if keys:
            entry["api_key"] = keys[0]
        model_list.append(entry)
    cfg = {
        "version": 2,
        "model_list": model_list,
        "gateway": {"host": "127.0.0.1", "port": 0},
    }
    json.dump(cfg, open(os.path.join(ws, "config.json"), "w", encoding="utf-8"), indent=2)
    open(os.path.join(ws, "AGENTS.md"), "w", encoding="utf-8").write(
        "# 测试工作区指令\n\n当用户询问“工作区标记”时，必须只回复：WORKSPACE_CTX_OK\n"
    )


def wait_file(path, timeout=15):
    deadline = time.time() + timeout
    while time.time() < deadline:
        if os.path.exists(path):
            return
        time.sleep(0.05)
    raise RuntimeError(f"timed out waiting for {path}")


def health_ok(host, port):
    try:
        with urllib.request.urlopen(f"http://{host}:{port}/health", timeout=2) as r:
            return r.status == 200
    except Exception:
        return False


async def recv_final(ws, timeout=180):
    """读取直到出现非思考、非工具调用的内容消息；返回 (text, tool_names)。"""
    tool_names = []
    while True:
        raw = await asyncio.wait_for(ws.recv(), timeout=timeout)
        msg = json.loads(raw)
        payload = msg.get("payload") or {}
        for tc in payload.get("tool_calls") or []:
            if isinstance(tc, dict):
                name = tc.get("name") or (tc.get("function") or {}).get("name")
                if name:
                    tool_names.append(name)
        if msg.get("type") == "error":
            raise AssertionError(f"pico error: {msg}")
        if (
            msg.get("type") in ("message.create", "message.update")
            and payload.get("content")
            and payload.get("kind") not in ("thought", "tool_calls")
            and not payload.get("placeholder")
        ):
            return payload["content"], tool_names


async def send(ws, session_id, content):
    await ws.send(json.dumps({
        "type": "message.send",
        "id": f"msg-{time.time_ns()}",
        "session_id": session_id,
        "payload": {"content": content},
    }))
    return await recv_final(ws)


async def run_checks(host, port, ws, token):
    uri = f"ws://{host}:{port}/pico/ws?session_id=live-test"
    async with websockets.connect(uri, additional_headers={"Authorization": f"Bearer {token}"}) as sock:
        await sock.send(json.dumps({"type": "ping", "timestamp": 1}))
        while True:
            msg = json.loads(await asyncio.wait_for(sock.recv(), timeout=10))
            if msg.get("type") == "pong":
                break
        print("PASS pico ping/pong")

        text, _ = await send(sock, LIVE_SESSION, "请只回复：AGENT_OK")
        if "AGENT_OK" not in text:
            raise AssertionError(f"basic chat response: {text!r}")
        print("PASS real model chat")

        text, _ = await send(sock, LIVE_SESSION, "请告诉我工作区标记是什么？只按工作区指令回复。")
        if "WORKSPACE_CTX_OK" not in text:
            raise AssertionError(f"workspace context response: {text!r}")
        print("PASS workspace instruction injection")

        text, _ = await send(sock, LIVE_SESSION, "/context")
        for needle in ("上下文用量", "工作区指令", "运行时上下文"):
            if needle not in text:
                raise AssertionError(f"/context missing {needle!r}: {text}")
        print("PASS /context stats")

        prompt = (
            "请立即使用工具完成以下操作，不要只解释：\n"
            '1. 调用 task_update，action=start，objective="LIVE_TASK：验证任务工具"\n'
            '2. 调用 todo_write，todos=[{"content":"检查任务工具","status":"in_progress"}]\n'
            "3. 调用 task_get 确认状态。\n"
            "完成后只回复 TOOLS_OK。"
        )
        text, tool_names = await send(sock, LIVE_SESSION, prompt)
        required = {"task_update", "todo_write", "task_get"}
        if not required.issubset(set(tool_names)):
            raise AssertionError(f"task tools not all called: {tool_names}")
        if "TOOLS_OK" not in text:
            raise AssertionError(f"task-tools final response: {text!r}")
        print("PASS task_update/todo_write/task_get")

        text, _ = await send(sock, LIVE_SESSION, "/context")
        if "任务状态：active" not in text and "任务状态：complete" not in text:
            raise AssertionError(f"task state missing from /context: {text}")
        print("PASS TaskState context/persistence")


LIVE_SESSION = "live-test"


def main():
    cfg_path = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_CONFIG
    if not os.path.exists(cfg_path):
        print(f"SKIP: config not found: {cfg_path}")
        return 0
    ws = tempfile.mkdtemp(prefix="pcl-live-test-")
    bin_path = os.path.join(ws, "chagent")
    proc = None
    try:
        prepare_workspace(cfg_path, ws)
        build_binary(bin_path)
        env = os.environ.copy()
        env.update({
            "CHAGENT_HOME": os.path.join(ws, "home"),
            "CHAGENT_WORKSPACE": ws,
            "CHAGENT_CONFIG": os.path.join(ws, "config.json"),
        })
        log_path = os.path.join(ws, "gateway.log")
        log = open(log_path, "wb")
        proc = subprocess.Popen([bin_path, "gateway", "--name", "live-test"], cwd=ws, env=env, stdout=log, stderr=log)
        wait_file(os.path.join(ws, ".chagent.pid"))
        pid_info = json.load(open(os.path.join(ws, ".chagent.pid"), encoding="utf-8"))
        host, port = pid_info["host"], pid_info["port"]
        # pico 是内置的：调用方与 gateway 使用同一个 md5(--name) 计算令牌。
        import hashlib
        actual_token = hashlib.md5(b"live-test").hexdigest()
        deadline = time.time() + 20
        while time.time() < deadline and not health_ok(host, port):
            time.sleep(0.1)
        if not health_ok(host, port):
            raise AssertionError("gateway /health did not become ready")
        print(f"PASS gateway health {host}:{port}")

        asyncio.run(run_checks(host, port, ws, actual_token))

        proc.send_signal(signal.SIGTERM)
        try:
            proc.wait(timeout=8)
        except subprocess.TimeoutExpired:
            raise AssertionError("gateway did not exit after SIGTERM")
        if os.path.exists(os.path.join(ws, ".chagent.pid")):
            raise AssertionError("pid file was not removed after SIGTERM")
        print("PASS SIGTERM + pid cleanup")
        print("LIVE AGENT TEST OK")
        return 0
    finally:
        if proc is not None and proc.poll() is None:
            proc.kill()
        if os.environ.get("KEEP_LIVE_TEST_DIR") == "1":
            print(f"KEEP ws={ws}")
        else:
            shutil.rmtree(ws, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
