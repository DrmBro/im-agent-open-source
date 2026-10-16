#!/bin/bash
set -euo pipefail

case "${1:-}" in
  client) application='/Applications/IM Agent.app' ;;
  background) application='/Applications/IM Agent 后台.app' ;;
  *) printf '不支持的 IM Agent 启动动作。\n' >&2; exit 2 ;;
esac

if [[ ! -d "$application" ]]; then
  printf '未找到已安装的应用：%s\n' "$application" >&2
  exit 1
fi
exec /usr/bin/open "$application"
