#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'USAGE'
用法：find-sessions.sh [-L socket-name|-S socket-path|-A] [-q pattern]

列出某个 socket 上的 tmux 会话（未指定时使用默认 tmux socket）。

选项：
  -L, --socket       tmux socket 名称（传给 tmux -L）
  -S, --socket-path  tmux socket 路径（传给 tmux -S）
  -A, --all          扫描 NANOBOT_TMUX_SOCKET_DIR 下的所有 socket
  -q, --query        按不区分大小写的子串过滤会话名
  -h, --help         显示本帮助
USAGE
}

socket_name=""
socket_path=""
query=""
scan_all=false
socket_dir="${NANOBOT_TMUX_SOCKET_DIR:-${TMPDIR:-/tmp}/nanobot-tmux-sockets}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    -L|--socket)      socket_name="${2-}"; shift 2 ;;
    -S|--socket-path) socket_path="${2-}"; shift 2 ;;
    -A|--all)         scan_all=true; shift ;;
    -q|--query)       query="${2-}"; shift 2 ;;
    -h|--help)        usage; exit 0 ;;
    *) echo "未知选项：$1" >&2; usage; exit 1 ;;
  esac
done

if [[ "$scan_all" == true && ( -n "$socket_name" || -n "$socket_path" ) ]]; then
  echo "不能将 --all 与 -L 或 -S 组合使用" >&2
  exit 1
fi

if [[ -n "$socket_name" && -n "$socket_path" ]]; then
  echo "请只使用 -L 或 -S 其中之一，不要同时使用" >&2
  exit 1
fi

if ! command -v tmux >/dev/null 2>&1; then
  echo "PATH 中未找到 tmux" >&2
  exit 1
fi

list_sessions() {
  local label="$1"; shift
  local tmux_cmd=(tmux "$@")

  if ! sessions="$("${tmux_cmd[@]}" list-sessions -F '#{session_name}\t#{session_attached}\t#{session_created_string}' 2>/dev/null)"; then
    echo "在 $label 上未找到 tmux 服务器" >&2
    return 1
  fi

  if [[ -n "$query" ]]; then
    sessions="$(printf '%s\n' "$sessions" | grep -i -- "$query" || true)"
  fi

  if [[ -z "$sessions" ]]; then
    echo "在 $label 上未找到会话"
    return 0
  fi

  echo "$label 上的会话："
  printf '%s\n' "$sessions" | while IFS=$'\t' read -r name attached created; do
    attached_label=$([[ "$attached" == "1" ]] && echo "已附加" || echo "已分离")
    printf '  - %s（%s，启动于 %s）\n' "$name" "$attached_label" "$created"
  done
}

if [[ "$scan_all" == true ]]; then
  if [[ ! -d "$socket_dir" ]]; then
    echo "未找到 socket 目录：$socket_dir" >&2
    exit 1
  fi

  shopt -s nullglob
  sockets=("$socket_dir"/*)
  shopt -u nullglob

  if [[ "${#sockets[@]}" -eq 0 ]]; then
    echo "在 $socket_dir 下未找到 socket" >&2
    exit 1
  fi

  exit_code=0
  for sock in "${sockets[@]}"; do
    if [[ ! -S "$sock" ]]; then
      continue
    fi
    list_sessions "socket 路径 '$sock'" -S "$sock" || exit_code=$?
  done
  exit "$exit_code"
fi

tmux_cmd=(tmux)
socket_label="默认 socket"

if [[ -n "$socket_name" ]]; then
  tmux_cmd+=(-L "$socket_name")
  socket_label="socket 名称 '$socket_name'"
elif [[ -n "$socket_path" ]]; then
  tmux_cmd+=(-S "$socket_path")
  socket_label="socket 路径 '$socket_path'"
fi

list_sessions "$socket_label" "${tmux_cmd[@]:1}"
