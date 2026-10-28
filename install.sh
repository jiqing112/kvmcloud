#!/usr/bin/env bash
# ============================================================================
# kvmcloud 安装脚本（三形态）
#
#   ./install.sh all-in-one                     单机全栈：控制面 + Agent 同机
#   ./install.sh control                        独立主控
#   ./install.sh agent --control-addr IP:9443 --token <JOIN_TOKEN>
#                                               计算节点接入已有主控
#   ./install.sh deps                           仅安装系统依赖
#   ./install.sh uninstall [control|agent|all]  卸载
#
# 关键约束：
#   - all-in-one：依赖服务（PostgreSQL/Redis/NATS）只装一份，由 control 侧负责；
#     Agent 复用本机依赖，CONTROL_ADDR=127.0.0.1:9443，不经外部网络。
#   - agent 模式：不安装 PG/Redis/NATS，只装 KVM/libvirt/TC 计算侧依赖。
#   - 端口规划：HTTP 8080 / gRPC 9443 / NATS 4222 / PG 5432 / Redis 6379；
#     Agent 不监听端口（主动连主控）。
# ============================================================================
set -euo pipefail

RED=$'\e[31m'; GREEN=$'\e[32m'; YELLOW=$'\e[33m'; NC=$'\e[0m'
info()  { echo "${GREEN}[INFO]${NC} $*"; }
warn()  { echo "${YELLOW}[WARN]${NC} $*"; }
die()   { echo "${RED}[ERROR]${NC} $*" >&2; exit 1; }

# ---------------- 默认值 ----------------
ROLE="menu"
CONTROL_ADDR=""; JOIN_TOKEN=""; NODE_NAME="$(hostname 2>/dev/null || echo node)"; NO_DEPS=false
CPU_RATIO="4"; MEM_RATIO="1.5"; DISK_RATIO="2"; BW_RATIO="1"

# 凭据/敏感配置只从环境变量读取，不写入源码（安全约束）：
#   DATABASE_URL            必填（control/all-in-one）
#   CONTROL_JOIN_TOKEN      节点注册令牌（control/all-in-one 生成侧）
#   JOIN_TOKEN              agent 模式必填（或 --token 传入）
#   AGENT_NIC_BANDWIDTH_KBPS 节点物理网卡速率，默认自动探测
VERSION="v0.1.1"
REPO_RAW="https://github.com/ojoti/kvmcloud/releases/download/${VERSION}"

usage() {
    cat <<EOF
用法:
  ./install.sh all-in-one                                       单机全栈
  ./install.sh control                                          独立主控
  ./install.sh agent --control-addr IP:9443 --token TOKEN       计算节点接入
  ./install.sh deps                                             仅装依赖
  ./install.sh uninstall [control|agent|all]                    卸载

选项:
  --control-addr ADDR   主控 gRPC 地址（agent 必填）
  --token TOKEN         节点注册令牌（agent 必填）
  --node-name NAME      节点名（默认 \$(hostname)）
  --no-deps             跳过系统依赖安装
  --cpu-ratio R         CPU 超开比（默认 4）
  --mem-ratio R         内存超开比（默认 1.5）
  --disk-ratio R        磁盘超开比（默认 2）
  --bw-ratio R          带宽超开比（默认 1）
  -h, --help            帮助

环境变量（凭据不落盘，仅环境注入）:
  DATABASE_URL                    postgres 连接串（control/all-in-one 必填）
  CONTROL_JOIN_TOKEN              主控生成注册令牌（all-in-one 自动生成到 /etc/cloud/join-token）
  JOIN_TOKEN / --token            agent 注册用令牌
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
        --no-deps)      NO_DEPS=true; shift ;;
        --cpu-ratio)    CPU_RATIO="$2"; shift 2 ;;
        --mem-ratio)    MEM_RATIO="$2"; shift 2 ;;
        --disk-ratio)   DISK_RATIO="$2"; shift 2 ;;
        --bw-ratio)     BW_RATIO="$2"; shift 2 ;;
        -h|--help)      usage; exit 0 ;;
        *)              die "未知参数: $1（--help 查看用法）" ;;
    esac
done

[[ "$(id -u)" -eq 0 ]] || die "请以 root 运行（sudo ./install.sh ...）"

detect_pkg() {
    if command -v apt-get >/dev/null; then echo "apt";
    elif command -v dnf >/dev/null; then echo "dnf";
    elif command -v yum >/dev/null; then echo "yum";
    else echo "unknown"; fi
}

detect_arch() {
    case "$(uname -m)" in
        x86_64)  echo "amd64" ;;
        aarch64) echo "arm64" ;;
        *)       echo "unsupported" ;;
    esac
}

install_deps_control() {
    local pkg; pkg=$(detect_pkg)
    info "安装控制面依赖（PostgreSQL/Redis/NATS）[${pkg}]"
    case "$pkg" in
        apt)
            export DEBIAN_FRONTEND=noninteractive
            apt-get update -qq
            apt-get install -y -qq postgresql redis-server gnupg curl >/dev/null
            ;;
        dnf|yum)
            $pkg install -y -q postgresql-server postgresql-server-devel redis curl >/dev/null
            postgresql-setup --initdb >/dev/null 2>&1 || true
            ;;
        *) die "不支持的包管理器，请手动安装 PostgreSQL/Redis" ;;
    esac
    systemctl enable --now postgresql redis-server 2>/dev/null || systemctl enable --now postgresql redis || true
    info "依赖服务已启动（仅此一份，agent 复用）"
}

install_deps_agent() {
    local pkg; pkg=$(detect_pkg)
    info "安装计算节点依赖（KVM/libvirt/TC）[${pkg}] —— 不安装 PG/Redis/NATS"
    case "$pkg" in
        apt)
            export DEBIAN_FRONTEND=noninteractive
            apt-get update -qq
            apt-get install -y -qq qemu-kvm libvirt-daemon-system libvirt-clients bridge-utils iproute2 >/dev/null
            ;;
        dnf|yum)
            $pkg install -y -q qemu-kvm libvirt bridge-utils iproute-tc >/dev/null
            ;;
        *) die "不支持的包管理器，请手动安装 KVM/libvirt/iproute-tc" ;;
    esac
    systemctl enable --now libvirtd
    # KVM 可用性检查（软告警，允许嵌套虚拟化场景）
    if ! grep -qE 'vmx|svm' /proc/cpuinfo; then
        warn "未检测到硬件虚拟化（vmx/svm），KVM 将不可用"
    fi
}

install_deps_only() { install_deps_control; install_deps_agent; }

fetch_binary() {
    local name="$1" dest="$2"
    local arch; arch=$(detect_arch)
    [[ "$arch" != "unsupported" ]] || die "不支持的 CPU 架构：$(uname -m)（发布含 amd64/arm64）"
    local url="${REPO_RAW}/kvmcloud-${name}_linux_${arch}.tar.gz"
    info "下载 ${url}"
    curl -fsSL --retry 3 -o "/tmp/${name}.tar.gz" "$url" || die "下载失败：$url（也可 make build 后手动拷贝 bin/）"
    tar -xzf "/tmp/${name}.tar.gz" -C /usr/local/bin/
    chmod 0755 "/usr/local/bin/kvmcloud-${name}"
}

# ---------------- control 侧 ----------------
install_control() {
    [[ "${NO_DEPS}" == "false" ]] && install_deps_control
    fetch_binary "control" /usr/local/bin

    mkdir -p /etc/cloud /var/log/cloud
    [[ -f /etc/cloud/control.env ]] || cp config/control.env.example /etc/cloud/control.env
    chmod 0600 /etc/cloud/control.env

    if [[ -z "${DATABASE_URL:-}" ]]; then
        die "请导出 DATABASE_URL（如 postgres://kvmcloud:***@127.0.0.1:5432/kvmcloud）后重试"
    fi
    # DATABASE_URL 仅写入 0600 权限的 env 文件（凭据不进源码/日志）
    if grep -q "^DATABASE_URL=" /etc/cloud/control.env; then
        sed -i "s|^DATABASE_URL=.*|DATABASE_URL=${DATABASE_URL}|" /etc/cloud/control.env
    else
        echo "DATABASE_URL=${DATABASE_URL}" >> /etc/cloud/control.env
    fi

    # JOIN_TOKEN：优先用环境变量，否则生成一次性令牌
    if [[ -z "${CONTROL_JOIN_TOKEN:-}" ]]; then
        CONTROL_JOIN_TOKEN="$(head -c 32 /dev/urandom | base64 | tr -d '/+=' | head -c 40)"
        info "已生成注册令牌 /etc/cloud/join-token（节点安装时 --token 引用）"
        echo -n "${CONTROL_JOIN_TOKEN}" > /etc/cloud/join-token
        chmod 0600 /etc/cloud/join-token
    fi
    if grep -q "^CONTROL_JOIN_TOKEN=" /etc/cloud/control.env; then
        sed -i "s|^CONTROL_JOIN_TOKEN=.*|CONTROL_JOIN_TOKEN=${CONTROL_JOIN_TOKEN}|" /etc/cloud/control.env
    else
        echo "CONTROL_JOIN_TOKEN=${CONTROL_JOIN_TOKEN}" >> /etc/cloud/control.env
    fi

    install -m 0644 deploy/systemd/cloud-control.service /etc/systemd/system/
    systemctl daemon-reload
    systemctl enable --now cloud-control
    sleep 2
    systemctl is-active --quiet cloud-control || { journalctl -u cloud-control --no-pager | tail -20; die "控制面启动失败"; }
    info "控制面就绪：HTTP :8080  gRPC :9443"
}

# ---------------- agent 侧（同机，复用本机依赖） ----------------
install_agent_local() {
    [[ "${NO_DEPS}" == "false" ]] && install_deps_agent
    fetch_binary "agent" /usr/local/bin

    mkdir -p /etc/cloud /var/log/cloud
    [[ -f /etc/cloud/agent.env ]] || cp config/agent.env.example /etc/cloud/agent.env

    # 同机：连本机控制面，令牌取本机生成值
    sed -i "s|__CONTROL_ADDR__|127.0.0.1:9443|g"        /etc/cloud/agent.env
    sed -i "s|__JOIN_TOKEN__|$(cat /etc/cloud/join-token)|g" /etc/cloud/agent.env
    sed -i "s|__NODE_NAME__|${NODE_NAME}|g"             /etc/cloud/agent.env
    sed -i "s|__CPU_RATIO__|${CPU_RATIO}|g"             /etc/cloud/agent.env
    sed -i "s|__MEM_RATIO__|${MEM_RATIO}|g"             /etc/cloud/agent.env
    sed -i "s|__DISK_RATIO__|${DISK_RATIO}|g"           /etc/cloud/agent.env
    sed -i "s|__BW_RATIO__|${BW_RATIO}|g"               /etc/cloud/agent.env
    chmod 0600 /etc/cloud/agent.env

    install -m 0644 deploy/systemd/cloud-agent.service /etc/systemd/system/
    systemctl daemon-reload
    systemctl enable --now cloud-agent
    sleep 3
    systemctl is-active --quiet cloud-agent || { journalctl -u cloud-agent --no-pager | tail -20; die "Agent 启动失败，检查 /var/log/cloud/agent.log"; }
    info "本机 Agent 已接入 127.0.0.1:9443"
}

# ---------------- agent 侧（分机，只装计算依赖） ----------------
install_agent_remote() {
    [[ "${NO_DEPS}" == "false" ]] && install_deps_agent   # 只装 KVM/libvirt/TC，不装 PG/Redis/NATS
    fetch_binary "agent" /usr/local/bin

    mkdir -p /etc/cloud /var/log/cloud
    [[ -f /etc/cloud/agent.env ]] || cp config/agent.env.example /etc/cloud/agent.env
    sed -i "s|__CONTROL_ADDR__|${CONTROL_ADDR}|g" /etc/cloud/agent.env
    sed -i "s|__JOIN_TOKEN__|${JOIN_TOKEN}|g"     /etc/cloud/agent.env
    sed -i "s|__NODE_NAME__|${NODE_NAME}|g"       /etc/cloud/agent.env
    sed -i "s|__CPU_RATIO__|${CPU_RATIO}|g"       /etc/cloud/agent.env
    sed -i "s|__MEM_RATIO__|${MEM_RATIO}|g"       /etc/cloud/agent.env
    sed -i "s|__DISK_RATIO__|${DISK_RATIO}|g"     /etc/cloud/agent.env
    sed -i "s|__BW_RATIO__|${BW_RATIO}|g"         /etc/cloud/agent.env
    chmod 0600 /etc/cloud/agent.env

    install -m 0644 deploy/systemd/cloud-agent.service /etc/systemd/system/
    systemctl daemon-reload
    systemctl enable --now cloud-agent
    sleep 3
    systemctl is-active --quiet cloud-agent || die "Agent 启动失败，检查 /var/log/cloud/agent.log"
    info "节点 ${NODE_NAME} 已接入主控 ${CONTROL_ADDR}"
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
        install_control
        install_agent_local
        info "all-in-one 部署完成：依赖服务一份，控制面与 Agent 同机运行"
        ;;
    control)    install_control ;;
    agent)
        [[ -n "${CONTROL_ADDR}" && -n "${JOIN_TOKEN}" ]] || die "agent 模式需 --control-addr 和 --token"
        install_agent_remote
        ;;
    deps)       install_deps_only ;;
    uninstall)  do_uninstall ;;
    *)          usage; exit 1 ;;
esac
