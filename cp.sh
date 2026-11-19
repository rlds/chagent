#!/usr/bin/env bash
# 原子替换 chagent 发布二进制。
#
# 不能直接 cp 覆盖正在运行的二进制：macOS 上同一 inode 被运行中的进程映射后，
# 新 exec 可能卡在 UE（不可中断）状态，表现为 Agent 启动后立即“僵尸”。
# 这里先复制到目标目录临时文件，再 rename 替换，新进程使用新 inode。
set -euo pipefail

destinations=(
  "/Users/wudaorui/code/chess/client/bin"
#  "/Users/wudaorui/code/chess/claw"
  "/Users/wudaorui/code/chess/build/bin"
)

for dest in "${destinations[@]}"; do
  mkdir -p "$dest"
  for src in bin/chagent*; do
    [ -f "$src" ] || continue
    name="$(basename "$src")"
    tmp="$dest/.${name}.tmp.$$"
    cp "$src" "$tmp"
    chmod +x "$tmp"
    mv -f "$tmp" "$dest/$name"
  done
done
