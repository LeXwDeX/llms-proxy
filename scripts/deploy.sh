#!/usr/bin/env bash
# 安全部署脚本：拉取最新代码并重建容器，强制保护 config/ 和 data/ 运行时数据。
#
# 设计原则（来自 2026-04-17 事故复盘）：
#   1. 预检：DB 文件存在 + 服务端口通 + 无并发部署 → 才允许继续
#   2. 备份：持久路径（/DATA/Backups/llms-proxy/）而不是 /tmp
#   3. 绝不执行 git clean（会删除 untracked 的 data/ logs/）
#   4. 不兼容的错误立即退出（set -euo pipefail）
#   5. 部署后健康检查失败 → 打印回滚命令（不自动回滚，由人工决策）
#   6. 发布锁：/var/run/llms-proxy-deploy.lock 防止并发
#
# 用法:
#   cd /DATA/AppData/llms_proxy && bash scripts/deploy.sh           # 生产（8000）
#   cd /DATA/AppData/llms_proxy && bash scripts/deploy.sh --test    # 测试（8001）
#
# 部署文档: docs/部署要求.md
#
set -euo pipefail

# ============================================================
# 解析参数
# ============================================================
DEPLOY_MODE="prod"
for arg in "$@"; do
  case "$arg" in
    --test) DEPLOY_MODE="test" ;;
    --prod) DEPLOY_MODE="prod" ;;
    *) die "未知参数: $arg（支持 --test / --prod）" ;;
  esac
done

# ============================================================
# 常量（按部署模式自动切换）
# ============================================================
APP_DIR="$(cd "$(dirname "$0")/.." && pwd)"
CONFIG_DIR="$APP_DIR/config"
DATA_DIR="$APP_DIR/data"
LOGS_DIR="$APP_DIR/logs"

if [ "$DEPLOY_MODE" = "test" ]; then
  COMPOSE_FILE="docker-compose.test.yml"
  HEALTH_URL="http://localhost:8001/healthz"
  CONTAINER_NAME="llms-proxy-test"
  PORT_LABEL="8001"
else
  COMPOSE_FILE="docker-compose.yml"
  HEALTH_URL="http://localhost:8000/healthz"
  CONTAINER_NAME="llms-proxy"
  PORT_LABEL="8000"
fi

# 持久备份路径（避开 /tmp，/tmp 会被系统清理）
BACKUP_ROOT="/DATA/Backups/llms-proxy"
TIMESTAMP="$(date +%Y%m%d-%H%M%S)"
BACKUP_DIR="$BACKUP_ROOT/$TIMESTAMP"

LOCK_FILE="/var/run/llms-proxy-deploy-$DEPLOY_MODE.lock"

# 颜色
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
NC='\033[0m'

die() { echo -e "${RED}[FATAL] $*${NC}" >&2; exit 1; }
info() { echo -e "${GREEN}==>${NC} $*"; }
warn() { echo -e "${YELLOW}[WARN]${NC} $*"; }

# ============================================================
# 0. 发布锁（防止并发部署）
# ============================================================
if [ -f "$LOCK_FILE" ]; then
  PID=$(cat "$LOCK_FILE" 2>/dev/null || echo "?")
  if [ -n "$PID" ] && kill -0 "$PID" 2>/dev/null; then
    die "另一个部署正在运行（PID=$PID），请等待其完成。如确认是僵尸锁请删除: rm $LOCK_FILE"
  fi
  warn "发现僵尸锁（PID=$PID 已不存在），继续"
fi
echo $$ > "$LOCK_FILE"
trap 'rm -f "$LOCK_FILE"' EXIT

cd "$APP_DIR"
info "工作目录: $APP_DIR"

# ============================================================
# 1. 预检 —— 确保环境正常，不正常时立即退出
# ============================================================
info "预检（检查 DB/目录/git 状态）..."

# 1.1 必须在正确的项目目录
[ -f "$APP_DIR/$COMPOSE_FILE" ] || die "当前目录不是 llms-proxy 项目根（缺 $COMPOSE_FILE）"
[ -f "$APP_DIR/scripts/deploy.sh" ] || die "缺 scripts/deploy.sh"

# 1.2 data/ 必须存在，且 DB 文件必须存在（首次部署除外）
if [ ! -d "$DATA_DIR" ]; then
  warn "data/ 目录不存在，将创建（确认这是首次部署？按 Ctrl+C 取消，10 秒后继续）"
  sleep 10
  mkdir -p "$DATA_DIR"
elif [ ! -f "$DATA_DIR/llms-proxy.db" ]; then
  warn "data/llms-proxy.db 不存在，将创建新 DB（确认这是首次部署或已手动清空？按 Ctrl+C 取消，10 秒后继续）"
  sleep 10
fi

# 1.3 config/config.json 必须存在（首次除外）
if [ ! -f "$CONFIG_DIR/config.json" ]; then
  warn "config/config.json 不存在（首次部署？按 Ctrl+C 取消，10 秒后继续）"
  sleep 10
fi

# 1.4 git 仓库应无未提交改动（有改动说明有人在服务器上手改了代码，需人工确认）
if ! git diff --quiet || ! git diff --cached --quiet; then
  warn "git 工作区有未提交改动，将被 git reset --hard 丢弃："
  git status --short
  echo -n "按回车继续，Ctrl+C 取消 > "
  read -r _
fi

# ============================================================
# 2. 备份（持久路径）
# ============================================================
info "备份到 $BACKUP_DIR"
mkdir -p "$BACKUP_DIR"

if [ -d "$CONFIG_DIR" ] && [ -n "$(ls -A "$CONFIG_DIR" 2>/dev/null)" ]; then
  cp -a "$CONFIG_DIR" "$BACKUP_DIR/config"
  info "  config/ 已备份"
fi

if [ -d "$DATA_DIR" ] && [ -n "$(ls -A "$DATA_DIR" 2>/dev/null)" ]; then
  cp -a "$DATA_DIR" "$BACKUP_DIR/data"
  info "  data/ 已备份"
fi

# 记录当前 commit，用于回滚
CURRENT_COMMIT=$(git rev-parse HEAD 2>/dev/null || echo "unknown")
echo "$CURRENT_COMMIT" > "$BACKUP_DIR/previous-commit.txt"
info "  previous-commit: $CURRENT_COMMIT"

# 清理旧备份（保留最近 20 个）
if [ -d "$BACKUP_ROOT" ]; then
  # 兼容 ls 输出按时间排序
  OLD_BACKUPS=$(ls -1t "$BACKUP_ROOT" 2>/dev/null | tail -n +21)
  if [ -n "$OLD_BACKUPS" ]; then
    echo "$OLD_BACKUPS" | while read -r old; do
      [ -n "$old" ] && rm -rf "$BACKUP_ROOT/$old"
    done
    info "  已清理 $(echo "$OLD_BACKUPS" | wc -l) 个旧备份，保留最近 20 个"
  fi
fi

# ============================================================
# 3. 拉取最新代码（绝不执行 git clean）
# ============================================================
info "拉取最新代码..."
git fetch origin

# 探测默认分支：优先 main，其次 master
if git rev-parse --verify --quiet origin/main >/dev/null; then
  REMOTE_BRANCH="origin/main"
elif git rev-parse --verify --quiet origin/master >/dev/null; then
  REMOTE_BRANCH="origin/master"
else
  die "无法定位 origin/main 或 origin/master 分支"
fi
info "  目标分支: $REMOTE_BRANCH"

git reset --hard "$REMOTE_BRANCH"

NEW_COMMIT=$(git rev-parse HEAD)
info "  新 commit: $NEW_COMMIT"

# ⚠️ 刻意不执行 git clean —— untracked 文件可能是 data/ 或 logs/
# 如需清理 build artifact 请手工处理

# ============================================================
# 4. 恢复数据（git reset 不删 untracked，但保险起见）
# ============================================================
if [ -d "$BACKUP_DIR/config" ]; then
  mkdir -p "$CONFIG_DIR"
  cp -a "$BACKUP_DIR/config"/. "$CONFIG_DIR"/
  info "已从备份恢复 config/"
fi

if [ -d "$BACKUP_DIR/data" ]; then
  mkdir -p "$DATA_DIR"
  cp -a "$BACKUP_DIR/data"/. "$DATA_DIR"/
  info "已从备份恢复 data/"
fi

# ============================================================
# 5. 修复挂载目录权限
# ============================================================
mkdir -p "$CONFIG_DIR" "$DATA_DIR" "$LOGS_DIR"
chmod 777 "$CONFIG_DIR" "$DATA_DIR" "$LOGS_DIR"
[ -f "$CONFIG_DIR/config.json" ] && chmod 666 "$CONFIG_DIR/config.json"

# ============================================================
# 6. 重建并启动容器
# ============================================================
info "重建容器（模式: $DEPLOY_MODE，端口: $PORT_LABEL）..."
docker compose -f "$COMPOSE_FILE" up --build -d

# ============================================================
# 7. 发布后健康检查
# ============================================================
info "等待容器就绪（最长 30 秒）..."
HEALTHY=0
for i in $(seq 1 30); do
  if curl -s -f -m 2 "$HEALTH_URL" >/dev/null 2>&1; then
    HEALTHY=1
    break
  fi
  sleep 1
done

echo ""
echo "============================================================"
if [ "$HEALTHY" = "1" ]; then
  echo -e "${GREEN}[OK] 部署成功（${DEPLOY_MODE}）${NC}"
  echo "  新 commit:  $NEW_COMMIT"
  echo "  备份路径:   $BACKUP_DIR"
  echo "  健康检查:   $HEALTH_URL → ok"
  echo ""
  echo "验证清单（请人工确认）："
  echo "  1. curl http://192.168.33.110:$PORT_LABEL/healthz    → {\"status\":\"ok\"}"
  echo "  2. curl http://192.168.33.110:$PORT_LABEL/admin/     → 登录页可访问"
  echo "  3. 实际调用任一模型对话，确认无异常"
  echo "============================================================"
  docker compose -f "$COMPOSE_FILE" ps
else
  echo -e "${RED}[FAIL] 健康检查未通过，容器可能未正常启动！${NC}"
  echo ""
  echo "诊断："
  docker compose -f "$COMPOSE_FILE" ps || true
  echo ""
  docker compose -f "$COMPOSE_FILE" logs --tail 50 || true
  echo ""
  echo -e "${YELLOW}回滚命令：${NC}"
  echo "  cd $APP_DIR"
  echo "  docker compose -f $COMPOSE_FILE down"
  echo "  cp -a $BACKUP_DIR/config/. $CONFIG_DIR/"
  echo "  cp -a $BACKUP_DIR/data/. $DATA_DIR/"
  echo "  git reset --hard $CURRENT_COMMIT"
  echo "  docker compose -f $COMPOSE_FILE up --build -d"
  echo "============================================================"
  exit 1
fi
