#!/usr/bin/env bash
# T7.6：测量二进制体积、到 /health 的冷启动耗时以及常驻内存。
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WS="$(mktemp -d)"
BIN="$(mktemp -u "${TMPDIR:-/tmp}/chagent-measure.XXXXXX")"
PID=""
cleanup() {
  [[ -n "$PID" ]] && kill -TERM "$PID" 2>/dev/null || true
  rm -rf "$WS" "$BIN"
}
trap cleanup EXIT

(cd "$ROOT" && go build -trimpath -o "$BIN" ./cmd/chagent)
BIN_BYTES=$(stat -f%z "$BIN" 2>/dev/null || stat -c%s "$BIN")
cat > "$WS/config.json" <<'JSON'
{"version":2,"model_list":[{"model_name":"fake","model":"fake","provider":"openai","api_base":"http://127.0.0.1:1","api_key":"test"}],"channels":{"pico":{"enabled":true,"token":"measure-token"}},"gateway":{"host":"127.0.0.1","port":0}}
JSON
START=$(python3 -c 'import time;print(time.time())')
CHAGENT_HOME="$WS/home" CHAGENT_WORKSPACE="$WS" CHAGENT_CONFIG="$WS/config.json" \
  "$BIN" gateway --name measure >"$WS/gateway.log" 2>&1 &
PID=$!
for _ in $(seq 1 300); do
  [[ -f "$WS/.chagent.pid" ]] && break
  sleep 0.02
done
PORT=$(python3 -c "import json,sys;print(json.load(open(sys.argv[1]))['port'])" "$WS/.chagent.pid")
for _ in $(seq 1 300); do
  code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/health" || true)
  [[ "$code" == "200" ]] && break
  sleep 0.02
done
END=$(python3 -c 'import time;print(time.time())')
RSS_KB=$(ps -o rss= -p "$PID" | tr -d ' ')
START_MS=$(python3 -c "print(round(($END-$START)*1000))")
echo "binary_bytes=$BIN_BYTES"
echo "startup_to_health_ms=$START_MS"
echo "rss_kb=$RSS_KB"
