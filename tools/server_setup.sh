#!/usr/bin/env bash
# kvmcloud 服务器端一键部署脚本（Debian 12，root 运行）。
# 由本机上传后以 nohup 执行，日志 /root/setup.log。
# 凭据策略：数据库密码在服务器现场生成（openssl rand），仅存 /etc/cloud/db-password（0600）。
set -uo pipefail
trap 'echo SETUP_FAIL' ERR

log() { echo "[setup $(date +%H:%M:%S)] $*"; }

export DEBIAN_FRONTEND=noninteractive

log "step 1/5: 系统包"
apt-get update -qq
apt-get install -y -qq git curl openssl >/dev/null

log "step 2/5: 克隆仓库"
if [ ! -d /root/kvmcloud/.git ]; then
    git clone --depth 1 https://github.com/ojoti/kvmcloud.git /root/kvmcloud
else
    git -C /root/kvmcloud pull --ff-only || true
fi
cd /root/kvmcloud
git fetch --depth 1 origin main && git reset --hard origin/main
log "repo at $(git rev-parse --short HEAD)"

log "step 3/5: PostgreSQL 初始化"
if ! dpkg -s postgresql >/dev/null 2>&1; then
    apt-get install -y -qq postgresql redis-server >/dev/null
fi
systemctl enable --now postgresql redis-server 2>/dev/null || systemctl enable --now postgresql redis

mkdir -p /etc/cloud
DBPASS_FILE=/etc/cloud/db-password
if [ ! -s "$DBPASS_FILE" ]; then
    openssl rand -hex 16 > "$DBPASS_FILE"
    chmod 600 "$DBPASS_FILE"
fi
DBPASS=$(cat "$DBPASS_FILE")
sudo -u postgres psql -tAc "SELECT 1 FROM pg_roles WHERE rolname='kvmcloud'" | grep -q 1 || \
    sudo -u postgres psql -c "CREATE USER kvmcloud WITH PASSWORD '$DBPASS';"
sudo -u postgres psql -tAc "SELECT 1 FROM pg_database WHERE datname='kvmcloud'" | grep -q 1 || \
    sudo -u postgres createdb -O kvmcloud kvmcloud
export DATABASE_URL="postgres://kvmcloud:${DBPASS}@127.0.0.1:5432/kvmcloud?sslmode=disable"
log "数据库就绪（密码见 /etc/cloud/db-password，0600）"

log "step 4/5: install.sh all-in-one"
bash install.sh all-in-one

log "step 5/5: 验收"
sleep 3
systemctl is-active --quiet cloud-control || { journalctl -u cloud-control --no-pager | tail -20; exit 1; }
systemctl is-active --quiet cloud-agent   || { journalctl -u cloud-agent --no-pager | tail -20; exit 1; }
curl -sf http://127.0.0.1:8080/healthz
echo
echo "JOIN_TOKEN=$(cat /etc/cloud/join-token)"
echo SETUP_OK
