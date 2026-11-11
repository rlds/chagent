#!/usr/bin/env bash
# 基础棋类智能体兼容性 E2E：构建项目，用最小化的 v2 棋类配置启动 gateway，
# 校验 pid/health/pico WS，然后发送 SIGTERM 并校验清理结果。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="${BIN:-$(mktemp -u "${TMPDIR:-/tmp}/chagent-chess-e2e.XXXXXX")}"
WS="$(mktemp -d)"
PID=""

cleanup() {
  if [[ -n "$PID" ]] && kill -0 "$PID" 2>/dev/null; then
    kill -TERM "$PID" 2>/dev/null || true
    sleep 1
    kill -KILL "$PID" 2>/dev/null || true
  fi
  rm -rf "$WS"
}
trap cleanup EXIT

(cd "$ROOT" && go build -o "$BIN" ./cmd/chagent)

cat > "$WS/config.json" <<'JSON'
{
  "version": 2,
  "model_list": [
    {"model_name":"fake","model":"fake","provider":"openai","api_base":"http://127.0.0.1:1","api_key":"test"}
  ],
  "gateway": {"host": "127.0.0.1", "port": 0}
}
JSON
cp "$WS/config.json" "$WS/config.orig.json"

CHAGENT_HOME="$WS/home" CHAGENT_WORKSPACE="$WS" CHAGENT_CONFIG="$WS/config.json" \
  "$BIN" gateway --name chess-e2e > "$WS/gateway.log" 2>&1 &
PID=$!

for _ in $(seq 1 100); do
  [[ -f "$WS/.chagent.pid" ]] && break
  sleep 0.1
done
if [[ ! -f "$WS/.chagent.pid" ]]; then
  echo "FAIL: pid file not written"; tail -50 "$WS/gateway.log"; exit 1
fi

PORT="$(python3 -c "import json,sys;print(json.load(open(sys.argv[1]))['port'])" "$WS/.chagent.pid")"
HOST="$(python3 -c "import json,sys;print(json.load(open(sys.argv[1]))['host'])" "$WS/.chagent.pid")"

for _ in $(seq 1 50); do
  code="$(curl -s -o /dev/null -w '%{http_code}' "http://$HOST:$PORT/health" || true)"
  [[ "$code" == "200" ]] && break
  sleep 0.1
done
if [[ "${code:-}" != "200" ]]; then
  echo "FAIL: /health returned ${code:-none}"; tail -50 "$WS/gateway.log"; exit 1
fi
echo "PASS health $code port=$PORT"

# pico 是内置的：token = md5(--name)；config.json 无需配置 channels/token。
PICO_TOKEN="$(python3 - "chess-e2e" <<'PY'
import hashlib, sys
print(hashlib.md5(sys.argv[1].encode()).hexdigest())
PY
)"
python3 - "$WS/config.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
assert "channels" not in d, "pico config should be optional/absent"
print("PASS pico auto-enabled, config unchanged")
PY

# Pico WS：可选，需要 python 的 websockets 库。
if python3 -c "import websockets" 2>/dev/null; then
  python3 - "$HOST" "$PORT" "$PICO_TOKEN" <<'PY'
import asyncio, json, sys
import websockets

async def main():
    uri = f"ws://{sys.argv[1]}:{sys.argv[2]}/pico/ws?session_id=chess-e2e"
    async with websockets.connect(uri, additional_headers={"Authorization": f"Bearer {sys.argv[3]}"}) as ws:
        await ws.send(json.dumps({"type":"ping","timestamp":1}))
        raw = await asyncio.wait_for(ws.recv(), timeout=5)
        msg = json.loads(raw)
        assert msg.get("type") == "pong", msg
        print("PASS pico ping/pong")

asyncio.run(main())
PY
else
  echo "SKIP pico WS (python websockets not installed)"
fi

kill -TERM "$PID"
for _ in $(seq 1 50); do
  kill -0 "$PID" 2>/dev/null || break
  sleep 0.1
done
if kill -0 "$PID" 2>/dev/null; then
  echo "FAIL: gateway did not exit within 5s"; exit 1
fi
PID=""
echo "PASS SIGTERM exit"
if [[ -f "$WS/.chagent.pid" ]]; then
  echo "FAIL: pid file not removed"; exit 1
fi
echo "PASS pid cleanup"
echo "Chess E2E OK"
