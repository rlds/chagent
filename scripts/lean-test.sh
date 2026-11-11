#!/usr/bin/env bash
# 精简的棋类智能体测试套件：只做构建 + 与 agent 相关的包。
# 全量套件的等价命令：`go test ./... -count=1`（更慢，包含 media/memory/cron
# 这类依赖 vendor 较重的包）。
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

echo "== go build ./..."
go build ./...

echo "== lean go test"
go test -count=1 \
  ./pkg/agent/... \
  ./pkg/config/... \
  ./pkg/tools/... \
  ./pkg/channels/pico/... \
  ./pkg/gateway/... \
  ./pkg/providers/... \
  ./pkg/taskstate/... \
  ./pkg/session/... \
  ./pkg/memory/... \
  ./pkg/skills/... \
  ./pkg/events/... \
  ./pkg/bus/... \
  ./pkg/commands/... \
  ./cmd/...

echo "== lean test OK"
