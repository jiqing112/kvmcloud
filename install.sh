#!/usr/bin/env bash
# ============================================================================
# kvmcloud 极客模式一键安装（Ubuntu/Debian/CentOS/Rocky/Alma）
#
# 一行命令（两步式：先下载校验，再执行）：
#   curl -fsSL https://gitee.com/yuntianojo/kvmcloud/raw/main/install.sh -o /tmp/kvmcloud-install.sh \
#     && sudo bash /tmp/kvmcloud-install.sh all-in-one
#
# 角色:
#   all-in-one    单机全栈：主控 + 本机计算节点
#   control       仅主控
#   agent         仅计算节点（接入已有主控）
#   deps          仅系统依赖
#   uninstall     卸载
#
# 特性:
#   - 双通道自动选择（国内 Gitee / 海外 GitHub，失败自动回退）
#   - 自动识别发行版并替换国内镜像源（--no-mirror-swap 关闭，原文件 .bak 备份）
#   - 全静默安装；all-in-one/control 自动开通 PostgreSQL 库号（免手配 DATABASE_URL）
#   - env / systemd 单元由脚本内联生成，不依赖仓库检出
#   - 二进制 sha256 校验（release checksums.txt）；端口占用 / KVM 可用性预检
#   - 安装完成打印摘要与下一步命令
#
# 安全约束：二进制仅从固定 https 主机（gitee.com / github.com）下载并校验摘要；
#   版本号强校验；注册令牌与数据库口令 0600 落盘，不进源码、不进日志。
# ============================================================================
set -euo pipefail

RED=$'\e[31m'; GREEN=$'\e[32m'; YELLOW=$'\e[33m'; CYAN=$'\e[36m'; NC=$'\e[0m'
info()  { echo "${GREEN}[INFO]${NC} $*"; }
warn()  { echo "${YELLOW}[WARN]${NC} $*"; }
die()   { echo "${RED}[ERROR]${NC} $*" >&2; exit 1; }

# ---------------- 默认值 ----------------
ROLE="menu"
VERSION="${KC_VERSION:-v0.2.1}"
KC_MIRROR="${KC_MIRROR:-auto}"           # auto | gitee | github
SWAP_MIRRORS=true
CONTROL_ADDR=""; JOIN_TOKEN=""; NODE_NAME="$(hostname 2>/dev/null || echo node)"; NO_DEPS=false
CPU_RATIO="4"; MEM_RATIO="1.5"; DISK_RATIO="2"; BW_RATIO="1"
DB_PASSWORD="${KC_DB_PASSWORD:-}"

usage() {
    cat <<'EOF'
用法（推荐两步式：下载后校验执行）:
  curl -fsSL https://gitee.com/yuntianojo/kvmcloud/raw/main/install.sh -o /tmp/kvmcloud-install.sh \
    && sudo bash /tmp/kvmcloud-install.sh all-in-one
  curl -fsSL https://raw.githubusercontent.com/yuntianojo/kvmcloud/main/install.sh -o /tmp/kvmcloud-install.sh \
    && sudo bash /tmp/kvmcloud-install.sh control

角色:
  all-in-one          单机全栈：主控 + 本机计算节点（数据库自动开通）
  control             仅主控（PostgreSQL/Redis 自动装并自动建库）
  agent               仅计算节点（只装 KVM/libvirt/TC，零控制面依赖）
  deps                仅安装系统依赖
  uninstall [target]  卸载（control|agent|all，默认 all）

选项:
  --control-addr ADDR   主控 gRPC 地址（agent 必填）
  --token TOKEN         节点注册令牌（agent 必填；主控上 cat /etc/cloud/join-token）
  --node-name NAME      节点名（默认 $(hostname)）
  --mirror SRC          下载通道：gitee | github（默认 auto 探测）
  --version VER         发行版本（默认 v0.2.1）
  --no-deps             路过系统依赖安装
  --no-mirror-swap      不替换国内镜像源
  --db-password PASS    自动建库口令（默认随机生成）
  --cpu-ratio R 等      四维超开比（默认 4 / 1.5 / 2 / 1）
环境变量: DATABASE_URL（外部数据库）、CONTROL_JOIN_TOKEN、KC_VERSION、KC_MIRROR
EOF
}

# ---------------- 参数解析 ----------------
while [[ $# -gt 0 ]]; do
    case "$1" in
        all-in-one|control|agent|deps) ROLE="$1"; shift ;;
        uninstall) ROLE="uninstall"; [[ -n "${2:-}" && "$2" != -* ]] && { UNINSTALL_TARGET="$2"; shift; }; shift ;;
        --control-addr) CONTROL_ADDR="$2"; shift 2 ;;
        --token)        JOIN_TOKEN="$2"; shift 2 ;;
        --node-name)    NODE_NAME="$2"; shift 2 ;;
        --mirror)       KC_MIRROR="$2"; shift 2 ;;
        --version)      VERSION="$2"; shift 2 ;;
        --no-deps)      NO_DEPS=true; shift ;;
        --no-mirror-swap) SWAP_MIRRORS=false; shift ;;
        --db-password)  DB_PASSWORD="$2"; shift 2 ;;
        --cpu-ratio)    CPU_RATIO="$2"; shift 2 ;;
        --mem-ratio)    MEM_RATIO="$2"; shift 2 ;;
        --disk-ratio)   DISK_RATIO="$2"; shift 2 ;;
        --bw-ratio)     BW_RATIO="$2"; shift 2 ;;
        -h|--help)      usage; exit 0 ;;
        *)              die "未知参数: $1（--help 查看用法）" ;;
    esac
done

[[ "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "版本号非法: $VERSION"
case "$KC_MIRROR" in auto|gitee|github) ;; *) die "mirror 只支持 auto|gitee|github" ;; esac
[[ "$(id -u)" -eq 0 ]] || die "请以 root 运行：curl -fsSL <url> -o /tmp/kvmcloud-install.sh && sudo bash /tmp/kvmcloud-install.sh ${ROLE}"

# ---------------- 基础探测 ----------------
command -v curl >/dev/null || command -v wget >/dev/null || die "需要 curl 或 wget"

http_probe() { # $1=url → 0=可达
    if command -v curl >/dev/null; then curl -m 3 -fsSI "$1" >/dev/null 2>&1
    else wget -q -T 3 --spider "$1" >/dev/null 2>&1; fi
}

fetch() { # $1=url $2=dest（仅 https，由调用方保证主机白名单）
    if command -v curl >/dev/null; then
        curl -fsSL --proto '=https' --retry 3 --connect-timeout 10 -o "$2" "$1"
    else
        wget -q -T 20 -t 3 -O "$2" "$1"
    fi
}

# 下载通道：gitee（国内快）优先探测，不通回退 github；固定 https 主机白名单。
detect_channel() {
    [[ "$KC_MIRROR" != "auto" ]] && { echo "$KC_MIRROR"; return; }
    if http_probe "https://gitee.com"; then echo "gitee"; else echo "github"; fi
}
CHANNEL=$(detect_channel)
case "$CHANNEL" in gitee|github) ;; *) CHANNEL="gitee" ;; esac

detect_pkg() {
    if command -v apt-get >/dev/null; then echo "apt";
    elif command -v dnf >/dev/null; then echo "dnf";
    elif command -v yum >/dev/null; then echo "yum";
    else echo "unknown"; fi
}
PKG=$(detect_pkg)

detect_arch() {
    case "$(uname -m)" in
        x86_64)  echo "amd64" ;;
        aarch64) echo "arm64" ;;
        *)       echo "unsupported" ;;
    esac
}
ARCH=$(detect_arch)

# ---------------- 国内镜像源自动替换（备份 + 一次性） ----------------
swap_mirrors() {
    [[ "$SWAP_MIRRORS" == true && "$CHANNEL" == "gitee" ]] || return 0
    [[ "$PKG" == "unknown" ]] && return 0
    mkdir -p /var/lib/kvmcloud
    local marker="/var/lib/kvmcloud/.mirrors-swapped"
    [[ -e "$marker" ]] && return 0
    case "$PKG" in
        apt)
            local f
            for f in /etc/apt/sources.list /etc/apt/sources.list.d/ubuntu.sources; do
                [[ -f "$f" ]] || continue
                cp -n "$f" "${f}.kvmcloud.bak" 2>/dev/null || true
                sed -i -E \
                  -e 's#(https?)://(archive\.ubuntu\.com|security\.ubuntu\.com|cn\.archive\.ubuntu\.com)#https://mirrors.aliyun.com#g' \
                  -e 's#(https?)://(deb\.debian\.org|security\.debian\.org)#https://mirrors.aliyun.com#g' \
                  "$f"
            done
            info "已替换 apt 镜像源为阿里云（原文件 *.kvmcloud.bak）"
            ;;
        dnf|yum)
            local f swapped=false
            for f in /etc/yum.repos.d/*.repo; do
                [[ -f "$f" ]] || continue
                if grep -q "dl.rockylinux.org" "$f"; then
                    cp -n "$f" "${f}.kvmcloud.bak" 2>/dev/null || true
                    sed -i -e 's|^mirrorlist=|#mirrorlist=|' \
                        -e 's|^#baseurl=http://dl\.rockylinux\.org/\$contentdir|baseurl=https://mirrors.aliyun.com/rockylinux|' "$f"
                    swapped=true
                elif grep -q "dl.almalinux.org" "$f"; then
                    cp -n "$f" "${f}.kvmcloud.bak" 2>/dev/null || true
                    sed -i -e 's|^mirrorlist=|#mirrorlist=|' \
                        -e 's|^#baseurl=http://dl\.almalinux\.org/\$contentdir|baseurl=https://mirrors.aliyun.com/almalinux|' "$f"
                    swapped=true
                elif grep -q "mirror.centos.org" "$f"; then
                    cp -n "$f" "${f}.kvmcloud.bak" 2>/dev/null || true
                    sed -i -e 's|^mirrorlist=|#mirrorlist=|' \
                        -e 's|# baseurl=http://mirror.centos.org|baseurl=https://mirrors.aliyun.com/centos|' \
                        -e 's|^baseurl=http://mirror.centos.org|baseurl=https://mirrors.aliyun.com/centos|' "$f"
                    swapped=true
                fi
            done
            $swapped && info "已替换 yum/dnf 镜像源为阿里云（原文件 *.kvmcloud.bak）"
            ;;
    esac
    touch "$marker"
}

# ---------------- 端口预检 ----------------
preflight_ports() {
    command -v ss >/dev/null || return 0
    local port
    for port in 8080 9443; do
        if ss -ltn 2>/dev/null | awk '{print $4}' | grep -qE "[:.]${port}\$"; then
            warn "端口 ${port} 已被监听（升级/重装属正常现象，新服务将复用）"
        fi
    done
}

# ---------------- 依赖安装 ----------------
install_deps_control() {
    info "安装控制面依赖（PostgreSQL/Redis）[${PKG}]"
    case "$PKG" in
        apt)
            export DEBIAN_FRONTEND=noninteractive
            apt-get update -qq
            apt-get install -y -qq postgresql redis-server curl ca-certificates >/dev/null
            ;;
        dnf|yum)
            $PKG install -y -q postgresql-server postgresql-contrib redis curl ca-certificates >/dev/null
            postgresql-setup --initdb >/dev/null 2>&1 || true
            ;;
        *) die "不支持的包管理器，请手动安装 PostgreSQL/Redis" ;;
    esac
    systemctl enable --now postgresql redis-server 2>/dev/null \
        || systemctl enable --now postgresql redis 2>/dev/null || true
    info "依赖服务已启动（仅此一份，agent 复用）"
}

install_deps_agent() {
    info "安装计算节点依赖（KVM/libvirt/TC）[${PKG}] —— 不安装 PG/Redis"
    case "$PKG" in
        apt)
            export DEBIAN_FRONTEND=noninteractive
            apt-get update -qq
            apt-get install -y -qq qemu-kvm libvirt-daemon-system libvirt-clients \
                bridge-utils iproute2 curl genisoimage >/dev/null
            ;;
        dnf|yum)
            $PKG install -y -q qemu-kvm libvirt bridge-utils iproute-tc curl genisoimage >/dev/null
            ;;
        *) die "不支持的包管理器，请手动安装 KVM/libvirt/iproute-tc" ;;
    esac
    systemctl enable --now libvirtd
    if ! grep -qE 'vmx|svm' /proc/cpuinfo; then
        warn "未检测到硬件虚拟化（vmx/svm），KVM 将不可用（允许嵌套虚拟化场景）"
    fi
}

# ---------------- 数据库自动开通（all-in-one / control 免手配） ----------------
provision_database() {
    [[ -n "${DATABASE_URL:-}" ]] && return 0
    command -v pg_isready >/dev/null || command -v psql >/dev/null || \
        die "未检测到本地 PostgreSQL 且未提供 DATABASE_URL；请导出 DATABASE_URL 或去掉 --no-deps 重试"
    local i
    for i in 1 2 3 4 5 6 7 8 9 10; do pg_isready -h 127.0.0.1 -q && break; sleep 1; done
    [[ -z "$DB_PASSWORD" ]] && DB_PASSWORD="$(head -c 24 /dev/urandom | base64 | tr -dc 'a-zA-Z0-9' | head -c 24)"
    if ! sudo -u postgres psql -tAc "SELECT 1 FROM pg_roles WHERE rolname='kvmcloud'" 2>/dev/null | grep -q 1; then
        sudo -u postgres psql -qc "CREATE ROLE kvmcloud LOGIN PASSWORD '${DB_PASSWORD}' CREATEDB CREATEROLE" >/dev/null
    fi
    if ! sudo -u postgres psql -tAc "SELECT 1 FROM pg_database WHERE datname='kvmcloud'" 2>/dev/null | grep -q 1; then
        sudo -u postgres createdb -O kvmcloud kvmcloud
    fi
    # 本机 TCP 口令认证：默认 pg_hba 通常已含 scram；显式置顶一条确保万无一失
    local pghba
    pghba="$(sudo -u postgres psql -tAc 'SHOW hba_file;' 2>/dev/null | head -1 || true)"
    if [[ -n "$pghba" && -f "$pghba" ]] && ! grep -q "^host kvmcloud kvmcloud" "$pghba"; then
        cp -n "$pghba" "${pghba}.kvmcloud.bak" 2>/dev/null || true
        sed -i "1i host kvmcloud kvmcloud 127.0.0.1/32 scram-sha-256" "$pghba"
        sudo -u postgres psql -qc "SELECT pg_reload_conf()" >/dev/null
    fi
    DATABASE_URL="postgres://kvmcloud:${DB_PASSWORD}@127.0.0.1:5432/kvmcloud?sslmode=disable"
    info "已自动开通数据库 kvmcloud（口令存于 /etc/cloud/control.env，0600）"
}

# ---------------- 二进制下载（双通道 + sha256 校验 + 回退） ----------------
release_asset_url() { # $1=name $2=channel → 固定 https 主机 + 已校验版本号
    case "$2" in
        github) echo "https://github.com/yuntianojo/kvmcloud/releases/download/${VERSION}/kvmcloud-${1}_linux_${ARCH}.tar.gz" ;;
        *)      echo "https://gitee.com/yuntianojo/kvmcloud/releases/download/${VERSION}/kvmcloud-${1}_linux_${ARCH}.tar.gz" ;;
    esac
}

verify_checksum() { # $1=tarball路径 $2=通道 → 0=校验通过或无清单
    local sums="/tmp/kvmcloud-checksums-${VERSION}.txt"
    local url
    case "$2" in
        github) url="https://github.com/yuntianojo/kvmcloud/releases/download/${VERSION}/checksums.txt" ;;
        *)      url="https://gitee.com/yuntianojo/kvmcloud/releases/download/${VERSION}/checksums.txt" ;;
    esac
    fetch "$url" "$sums" || { warn "checksums.txt 不可用，跳过校验（建议使用带清单的发行版）"; return 0; }
    local expected
    expected="$(grep "kvmcloud-${1}_linux_${ARCH}.tar.gz" "$sums" | awk '{print $1}' | head -1)"
    [[ -n "$expected" ]] || { warn "checksums.txt 无该文件条目，跳过校验"; return 0; }
    local actual
    actual="$(sha256sum "$1" | awk '{print $1}')"
    [[ "$actual" == "$expected" ]] || die "二进制 sha256 校验失败：${1}"
    info "sha256 校验通过"
}

fetch_binary() {
    local name="$1"
    [[ "$ARCH" != "unsupported" ]] || die "不支持的 CPU 架构：$(uname -m)（发布含 amd64/arm64）"
    local other="github"; [[ "$CHANNEL" == "github" ]] && other="gitee"
    local tmp="/tmp/kvmcloud-${name}-${VERSION}.tar.gz" ch
    for ch in "$CHANNEL" "$other"; do
        info "下载 kvmcloud-${name}（${ch}）"
        if fetch "$(release_asset_url "$name" "$ch")" "$tmp" \
            && verify_checksum "$name" "$ch" \
            && tar -xzf "$tmp" -C /usr/local/bin/; then
            chmod 0755 "/usr/local/bin/kvmcloud-${name}"
            return 0
        fi
        warn "通道 ${ch} 不可用，尝试备用源"
    done
    die "二进制下载失败（gitee/github 均不可达）。备选：make build 后手动安装，或 --mirror 指定通道"
}

# ---------------- 内联配置与 systemd 单元（独立执行无仓库依赖） ----------------
write_control_env() {
    mkdir -p /etc/cloud /var/log/cloud
    cat > /etc/cloud/control.env <<EOF
# kvmcloud-control 配置（0600；由安装器生成，含凭据勿提交）
DATABASE_URL=${DATABASE_URL}
CONTROL_JOIN_TOKEN=${CONTROL_JOIN_TOKEN}
CONTROL_JOIN_TOKEN_TTL_HOURS=
CONTROL_JOIN_TOKEN_SINGLE_USE=false
CONTROL_HTTP_ADDR=:8080
CONTROL_GRPC_ADDR=:9443
OVERCOMMIT_ALERT_THRESHOLD=0.90
DISK_FULL_PROTECT_THRESHOLD=0.95
MONITOR_INTERVAL=10s
NODE_STALE_AFTER=90s
NODE_WATCHDOG_INTERVAL=30s
EOF
    chmod 0600 /etc/cloud/control.env
}

write_agent_env() { # $1=control_addr $2=join_token
    mkdir -p /etc/cloud /var/log/cloud /var/lib/kvmcloud
    cat > /etc/cloud/agent.env <<EOF
# kvmcloud-agent 配置（0600；由安装器生成）
CONTROL_ADDR=$1
JOIN_TOKEN=$2
NODE_NAME=${NODE_NAME}
CPU_RATIO=${CPU_RATIO}
MEM_RATIO=${MEM_RATIO}
DISK_RATIO=${DISK_RATIO}
BW_RATIO=${BW_RATIO}
AGENT_NIC=
AGENT_NIC_BANDWIDTH_KBPS=0
HEARTBEAT_INTERVAL=15s
PROVISION_INTERVAL=10s
AGENT_DATA_DIR=/var/lib/kvmcloud
#AGENT_BASE_IMAGE=/var/lib/kvmcloud/images/base.qcow2
AGENT_BRIDGE=virbr0
MEM_BALLOON_ENABLED=false
KSM_ENABLED=false
EOF
    chmod 0600 /etc/cloud/agent.env
}

write_systemd_units() {
    cat > /etc/systemd/system/cloud-control.service <<'EOF'
[Unit]
Description=KVM Cloud Control Plane (HTTP API + gRPC registry + monitor)
After=network-online.target postgresql.service
Wants=network-online.target
Before=cloud-agent.service

[Service]
Type=simple
User=root
EnvironmentFile=/etc/cloud/control.env
ExecStart=/usr/local/bin/kvmcloud-control
Restart=on-failure
RestartSec=5
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF
    cat > /etc/systemd/system/cloud-agent.service <<'EOF'
[Unit]
Description=KVM Cloud Compute Agent (register/heartbeat/QoS/provision)
After=network-online.target libvirtd.service cloud-control.service
Wants=network-online.target
Requires=libvirtd.service

[Service]
Type=simple
User=root
EnvironmentFile=/etc/cloud/agent.env
ExecStart=/usr/local/bin/kvmcloud-agent
Restart=always
RestartSec=5
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
}

primary_ip() {
    hostname -I 2>/dev/null | awk '{print $1}' || echo "127.0.0.1"
}

mask_url() { # 摘要中隐藏数据库口令
    sed -E 's#//([^:]+):[^@]+@#//\1:***@#' <<<"$1"
}

# ---------------- control 侧 ----------------
install_control() {
    [[ "$NO_DEPS" == false ]] && install_deps_control
    [[ -n "${DATABASE_URL:-}" ]] || provision_database
    [[ -n "${DATABASE_URL:-}" ]] || die "缺少 DATABASE_URL（--no-deps 模式必须外部提供）"
    fetch_binary "control"

    if [[ -z "${CONTROL_JOIN_TOKEN:-}" ]]; then
        CONTROL_JOIN_TOKEN="$(head -c 32 /dev/urandom | base64 | tr -dc 'a-zA-Z0-9' | head -c 40)"
        info "已生成注册令牌 /etc/cloud/join-token"
        echo -n "${CONTROL_JOIN_TOKEN}" > /etc/cloud/join-token
        chmod 0600 /etc/cloud/join-token
    fi
    write_control_env
    write_systemd_units
    systemctl enable --now cloud-control
    sleep 2
    systemctl is-active --quiet cloud-control || { journalctl -u cloud-control --no-pager | tail -20; die "控制面启动失败"; }
    info "控制面就绪：HTTP :8080  gRPC :9443"
}

# ---------------- agent 侧（同机复用 / 分机独立） ----------------
install_agent_local() {
    [[ "$NO_DEPS" == false ]] && install_deps_agent
    fetch_binary "agent"
    write_agent_env "127.0.0.1:9443" "$(cat /etc/cloud/join-token)"
    write_systemd_units
    systemctl enable --now cloud-agent
    sleep 3
    systemctl is-active --quiet cloud-agent || { journalctl -u cloud-agent --no-pager | tail -20; die "Agent 启动失败"; }
    info "本机 Agent 已接入 127.0.0.1:9443"
}

install_agent_remote() {
    [[ "$NO_DEPS" == false ]] && install_deps_agent
    fetch_binary "agent"
    write_agent_env "${CONTROL_ADDR}" "${JOIN_TOKEN}"
    write_systemd_units
    systemctl enable --now cloud-agent
    sleep 3
    systemctl is-active --quiet cloud-agent || die "Agent 启动失败，检查 /var/log/cloud/agent.log"
    info "节点 ${NODE_NAME} 已接入主控 ${CONTROL_ADDR}"
}

# ---------------- 安装摘要 ----------------
print_summary() {
    local ip; ip=$(primary_ip)
    echo
    echo "${CYAN}──────────────────────────────────────────────────────────${NC}"
    echo "${GREEN}  kvmcloud ${VERSION} · ${ROLE} 安装完成${NC}"
    echo "${CYAN}──────────────────────────────────────────────────────────${NC}"
    case "$ROLE" in
        control)
            echo "  控制面 API : http://${ip}:8080   (curl http://127.0.0.1:8080/healthz 验活)"
            echo "  gRPC 接入  : ${ip}:9443"
            echo "  数据库     : $(mask_url "${DATABASE_URL:-<沿用 control.env>}")"
            echo "  注册令牌   : /etc/cloud/join-token（节点接入用，0600）"
            echo "  添加节点   : curl -fsSL https://gitee.com/yuntianojo/kvmcloud/raw/main/install.sh \\"
            echo "                 -o /tmp/kc-agent.sh && sudo bash /tmp/kc-agent.sh agent \\"
            echo "                 --control-addr ${ip}:9443 --token \$(cat /etc/cloud/join-token)"
            echo "  服务管理   : systemctl status cloud-control"
            ;;
        all-in-one)
            echo "  控制台/API : http://${ip}:8080   (curl http://127.0.0.1:8080/healthz 验活)"
            echo "  gRPC       : 127.0.0.1:9443（本机 Agent 已接入）"
            echo "  数据库     : $(mask_url "${DATABASE_URL:-<沿用 control.env>}")"
            echo "  注册令牌   : /etc/cloud/join-token（扩容新节点用）"
            echo "  下一步     : 1) /etc/cloud/agent.env 配置 AGENT_BASE_IMAGE 后重启 cloud-agent"
            echo "             2) 节点就绪检查：curl -s http://127.0.0.1:8080/api/v1/nodes"
            echo "  服务管理   : systemctl status cloud-control cloud-agent"
            ;;
        agent)
            echo "  节点名     : ${NODE_NAME}"
            echo "  主控地址   : ${CONTROL_ADDR}"
            echo "  下一步     : 主控上 curl -s http://127.0.0.1:8080/api/v1/nodes 应可见本节点 online"
            echo "  建镜像     : /etc/cloud/agent.env 配置 AGENT_BASE_IMAGE（qcow2）后重启 cloud-agent"
            echo "  服务管理   : systemctl status cloud-agent"
            ;;
    esac
    echo "  下载通道   : ${CHANNEL}（${VERSION}，$(uname -m)）"
    echo "${CYAN}──────────────────────────────────────────────────────────${NC}"
    echo
}

# ---------------- 卸载 ----------------
do_uninstall() {
    local target="${UNINSTALL_TARGET:-all}"
    if [[ "$target" == "agent" || "$target" == "all" ]]; then
        systemctl disable --now cloud-agent 2>/dev/null || true
        rm -f /etc/systemd/system/cloud-agent.service /etc/cloud/agent.env /usr/local/bin/kvmcloud-agent
    fi
    if [[ "$target" == "control" || "$target" == "all" ]]; then
        systemctl disable --now cloud-control 2>/dev/null || true
        rm -f /etc/systemd/system/cloud-control.service /etc/cloud/control.env /usr/local/bin/kvmcloud-control
    fi
    systemctl daemon-reload
    info "卸载完成（target=${target}）。数据库未删除：如需清库请手动 drop database。"
}

# ---------------- 分发 ----------------
case "${ROLE}" in
    all-in-one)
        preflight_ports
        swap_mirrors
        install_control
        install_agent_local
        print_summary
        ;;
    control)
        preflight_ports
        swap_mirrors
        install_control
        print_summary
        ;;
    agent)
        [[ -n "${CONTROL_ADDR}" && -n "${JOIN_TOKEN}" ]] || die "agent 模式需 --control-addr 和 --token（主控上 cat /etc/cloud/join-token 获取）"
        swap_mirrors
        install_agent_remote
        print_summary
        ;;
    deps)
        swap_mirrors
        install_deps_control
        install_deps_agent
        info "系统依赖安装完成"
        ;;
    uninstall) do_uninstall ;;
    *)         usage; exit 1 ;;
esac
