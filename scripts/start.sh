#!/usr/bin/env bash
# Chagent 启动脚本：从 ./bin/chagent 运行网关。
# 用法：
#   scripts/start.sh                       # 使用默认配置启动网关
#   scripts/start.sh --name my-agent       # 传递网关参数
#   scripts/start.sh --config /path.json   # 透传给网关
# 环境变量：CHAGENT_HOME、CHAGENT_WORKSPACE、CHAGENT_CONFIG。
# 重命名后不再有意支持旧版 PICOCLAW_*/DDMCCLAW_* 名称。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="${CHAGENT_BIN:-$ROOT/bin/chagent}"

if [[ ! -x "$BIN" ]]; then
  echo "chagent binary not found: $BIN" >&2
  echo "building it first (scripts/build.sh)..." >&2
  "$ROOT/scripts/build.sh"
fi

exec "$BIN" gateway "$@"
