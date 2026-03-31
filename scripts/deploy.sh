#!/usr/bin/env bash
# 安全部署脚本：拉取最新代码并重建容器，保护 config/ 运行时数据不被覆盖。
#
# 用法:
#   cd /DATA/AppData/azure_proxy && bash scripts/deploy.sh
#
set -euo pipefail

APP_DIR="$(cd "$(dirname "$0")/.." && pwd)"
CONFIG_DIR="$APP_DIR/config"
BACKUP_DIR="/tmp/llms-proxy-config-$(date +%Y%m%d%H%M%S)"

echo "==> 工作目录: $APP_DIR"
cd "$APP_DIR"

# 1. 备份 config/
if [ -d "$CONFIG_DIR" ] && [ "$(ls -A "$CONFIG_DIR" 2>/dev/null)" ]; then
  cp -a "$CONFIG_DIR" "$BACKUP_DIR"
  echo "==> 已备份 config/ -> $BACKUP_DIR"
else
  echo "==> config/ 为空，跳过备份"
fi

# 2. 拉取最新代码
echo "==> 拉取最新代码..."
git fetch origin
git reset --hard origin/main

# 3. 恢复 config/（git reset 可能清除了运行时文件）
if [ -d "$BACKUP_DIR" ]; then
  cp -a "$BACKUP_DIR"/. "$CONFIG_DIR"/
  echo "==> 已恢复 config/ 数据"
fi

# 4. 重建并启动容器
echo "==> 重建容器..."
docker compose up --build -d

echo "==> 部署完成！"
echo "    备份保留在: $BACKUP_DIR"
docker compose ps
