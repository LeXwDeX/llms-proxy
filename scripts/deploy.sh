#!/usr/bin/env bash
# 安全部署脚本：拉取最新代码并重建容器，保护 config/ 和 data/ 运行时数据不被覆盖。
#
# 用法:
#   cd /DATA/AppData/llms_proxy && bash scripts/deploy.sh
#
# 部署文档: docs/部署要求.md
#
set -euo pipefail

APP_DIR="$(cd "$(dirname "$0")/.." && pwd)"
CONFIG_DIR="$APP_DIR/config"
DATA_DIR="$APP_DIR/data"
BACKUP_DIR="/tmp/llms-proxy-backup-$(date +%Y%m%d%H%M%S)"

echo "==> 工作目录: $APP_DIR"
cd "$APP_DIR"

# 1. 备份 config/ 和 data/（绝对不能跳过！）
mkdir -p "$BACKUP_DIR"

if [ -d "$CONFIG_DIR" ] && [ "$(ls -A "$CONFIG_DIR" 2>/dev/null)" ]; then
  cp -a "$CONFIG_DIR" "$BACKUP_DIR/config"
  echo "==> 已备份 config/ -> $BACKUP_DIR/config"
else
  echo "==> config/ 为空，跳过备份"
fi

if [ -d "$DATA_DIR" ] && [ "$(ls -A "$DATA_DIR" 2>/dev/null)" ]; then
  cp -a "$DATA_DIR" "$BACKUP_DIR/data"
  echo "==> 已备份 data/ -> $BACKUP_DIR/data"
else
  echo "==> data/ 为空，跳过备份"
fi

# 2. 拉取最新代码
echo "==> 拉取最新代码..."
git fetch origin
git reset --hard origin/main

# 3. 恢复 config/（git reset 可能清除了运行时文件）
if [ -d "$BACKUP_DIR/config" ]; then
  mkdir -p "$CONFIG_DIR"
  cp -a "$BACKUP_DIR/config"/. "$CONFIG_DIR"/
  echo "==> 已恢复 config/ 数据"
fi

# 4. 恢复 data/（git reset 可能清除了运行时文件）
if [ -d "$BACKUP_DIR/data" ]; then
  mkdir -p "$DATA_DIR"
  cp -a "$BACKUP_DIR/data"/. "$DATA_DIR"/
  echo "==> 已恢复 data/ 数据"
fi

# 5. 确保挂载目录存在且容器用户可写
echo "==> 修复挂载目录权限..."
mkdir -p "$APP_DIR/config" "$APP_DIR/data" "$APP_DIR/logs"
chmod 777 "$APP_DIR/config" "$APP_DIR/data" "$APP_DIR/logs"
[ -f "$APP_DIR/config/config.json" ] && chmod 666 "$APP_DIR/config/config.json"

# 6. 重建并启动容器
echo "==> 重建容器..."
docker compose up --build -d

echo ""
echo "==> 部署完成！"
echo "    备份保留在: $BACKUP_DIR"
echo ""
echo "==> 验证清单："
echo "    1. docker compose ps              (状态应为 Up)"
echo "    2. curl http://localhost:8000/healthz  (应返回 ok)"
echo "    3. ls -la config/config.json      (应存在且有内容)"
echo "    4. ls -la data/llms-proxy.db      (应存在且大小>0)"
echo ""
docker compose ps
