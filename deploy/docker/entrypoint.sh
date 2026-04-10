#!/bin/sh
# entrypoint.sh — 以 root 修复挂载卷权限后，降权运行主进程。
# 解决 QNAP / NAS 等环境下挂载目录 uid 映射不一致导致写入失败的问题。
set -e

# 修复挂载卷权限（仅在 root 运行时生效，非 root 跳过）
if [ "$(id -u)" = "0" ]; then
  chown -R llmsproxy:llmsproxy /etc/llms-proxy /var/log/llms-proxy /var/lib/llms-proxy 2>/dev/null || true
  chmod -R 775 /etc/llms-proxy /var/log/llms-proxy /var/lib/llms-proxy 2>/dev/null || true
  exec su-exec llmsproxy:llmsproxy "$@"
fi

# 非 root 直接运行
exec "$@"
