# kvmcloud 功能文档

> 适用版本：v0.2.1 · 更新日期：2026-10-30
> 本文覆盖**当前已实现**的功能全景。规划中模块（热迁移/安全组/云盘快照等）见 `docs/api/core-design.md`，不在本文范围。

## 1. 平台定位

面向中小云服务商（IDC）与企业自建的轻量 KVM IaaS 底座：**控制面（Control Plane）+ 计算节点（Agent）** 分离架构，
核心差异化是 **CPU / 内存 / 磁盘 / 带宽 四维超开调度** 与 **端口级带宽「保障—借用—抢占」语义**。

| 组件 | 二进制 | 说明 |
| --- | --- | --- |
| 控制面 | `kvmcloud-control` | HTTP API (:8080)、gRPC 注册/心跳 (:9443)、水位监控 |
| 计算节点 | `kvmcloud-agent` | 注册/心跳、TC HTB QoS、建机（qemu-img/virsh）、镜像拉取、资源采集 |
| Web 控制台 | `web/`（Vue 3 + Vite） | 仪表盘/节点/虚拟机/带宽策略/共享池/水位告警 六页面 |
| 数据库 | PostgreSQL | squirrel 参数绑定；`public.*`（现行）与 `kvmcloud_core`（安全基线）双 schema |
| 消息 | NATS（已装未启用） | 端口规划保留 4222；当前任务通道为 Agent HTTP 拉取 + gRPC |

**端口规划**：HTTP 8080 / gRPC 9443 / PostgreSQL 5432 / Redis 6379 / NATS 4222。Agent 不监听端口（主动连主控）。

## 2. 安装

### 2.1 极客模式（一行命令）

两步式（先下载脚本，校验执行；避免 `curl | bash` 直通执行）：

```bash
# 国内（Gitee）
curl -fsSL https://gitee.com/yuntianojo/kvmcloud/raw/main/install.sh -o /tmp/kvmcloud-install.sh \
  && sudo bash /tmp/kvmcloud-install.sh all-in-one

# 海外（GitHub）
curl -fsSL https://raw.githubusercontent.com/yuntianojo/kvmcloud/main/install.sh -o /tmp/kvmcloud-install.sh \
  && sudo bash /tmp/kvmcloud-install.sh all-in-one
```

角色：`all-in-one`（单机全栈）/ `control`（仅主控）/ `agent`（仅计算节点）/ `deps` / `uninstall`。
自动能力：发行版识别（Ubuntu/Debian/CentOS/Rocky/Alma）、国内镜像源自动替换（阿里云，`.kvmcloud.bak` 备份，`--no-mirror-swap` 关闭）、
PostgreSQL 自动建库开号（免手配 DATABASE_URL）、二进制 sha256 校验（release checksums.txt）、双通道下载自动回退、
端口占用预检、KVM 可用性软告警、安装完成打印摘要与下一步命令。

常用参数：`--mirror gitee|github`、`--version v0.2.1`、`--no-deps`、`--no-mirror-swap`、`--db-password PASS`、
`--cpu-ratio 4 --mem-ratio 1.5 --disk-ratio 2 --bw-ratio 1`。

### 2.2 主控 + 多节点（生产推荐）

```bash
# 主控（或用极客模式 control 角色）
sudo ./install.sh control          # 令牌生成到 /etc/cloud/join-token（0600）
cat /etc/cloud/join-token

# 每台计算节点（零控制面依赖）
sudo ./install.sh agent --control-addr 10.0.0.10:9443 --token <JOIN_TOKEN> --node-name node-01
```

可选令牌策略（`/etc/cloud/control.env`）：`CONTROL_JOIN_TOKEN_TTL_HOURS=24`、`CONTROL_JOIN_TOKEN_SINGLE_USE=true`。

### 2.3 卸载

```bash
sudo ./install.sh uninstall all    # 或 control / agent；数据库保留，需手动 drop
```

## 3. 控制面功能（现行链路，`internal/api`）

### 3.1 节点接入与心跳

- **注册**：Agent 携带 JOIN_TOKEN 经 gRPC :9443 注册，幂等（同 hostname 返回同一节点）；容量/超开比随注册上报。
- **心跳**：15s 周期上报资源快照（物理容量 + 实际使用 + 气球可回收量），回执四维水位；90s 无心跳看门狗自动置 `offline`，恢复自动回 `online`。

### 3.2 四维超开放置（核心差异化）

放置请求在**单事务内**对 CPU / 内存 / 磁盘 / 带宽做原子校验与扣减，任一维超限整体拒绝并返回维度名：

| 维度 | 校验公式 | 默认超开比 |
| --- | --- | --- |
| CPU | ΣvCPU ≤ physical × ratio | 4.0 |
| 内存 | Σmem ≤ physical × ratio | 1.5 |
| 磁盘（精简置备） | Σdisk ≤ physical × ratio | 2.0 |
| 带宽保障 | Σmin_kbps ≤ NIC × ratio | 1.0 |

每节点超开比可在线调优（`PUT /api/v1/nodes/{id}/ratios`）。调度选择四维水位最低的在线节点。

### 3.3 端口级带宽调度（核心差异化）

**保障—借用—抢占**三场景（`internal/bandwidth`，纯函数 + 同池 advisory lock 串行化）：

1. **保障**：策略创建时两级校验——节点侧 `Σmin ≤ NIC×ratio`、池侧 `Σmin ≤ capacity`；
2. **借用**：池余量充足直接授予（`granted`），ceil 从 min 抬升到 min+granted；
3. **抢占**：余量不足按优先级压回低优先级借用方（永不低于其 min），凑足授予（`preempted`），凑不足整体拒绝（`denied`）；
   全部事件落 `borrow_events` 审计，Agent 侧 TC HTB `rate=min, ceil=动态` 在线生效。

### 3.4 VM 生命周期（v0.2.1 建机链路）

- **放置**：`POST /api/v1/vms` 四维校验通过后落库 `creating`；
- **建机**：Agent 每秒级拉取本节点 pending 任务 → `qemu-img` qcow2（backing file 秒级克隆）→ libvirt 定义/启动（幂等，域已存在则确保运行）
  → 按 MAC 定位 tap 设备应用 TC HTB QoS → 回报结果；
- **状态机**：`creating → running`；失败 → `error`（原因写 `vms.error`，UI 展示）且**自动释放四维预留**；
- **删除**：卸载域并回滚容量账本（幂等）。

### 3.5 监控与告警

- 任一维水位 ≥ 0.90（可配）写入 `overcommit_alerts`（10 分钟冷却），UI 仪表盘与告警页展示；
- 心跳回执携带四维水位，节点页可在线调超开比。

## 4. 控制面功能（`kvmcloud_core` 安全基线，`internal/coreapi`）

> 独立 schema + RLS 租户隔离 + 幂等 + 审计的新一代 API；与现行链路并存，未接线生产流量。

| 模块 | 已实现 |
| --- | --- |
| 四维调度 | `POST /vms`：幂等键（24h 重放）、active 租户校验、镜像可见性/大小、配额五维守卫扣减、共享池保障余量、节点 SKIP LOCKED 预留、VM/NIC/QoS/预留/根卷/任务/outbox/审计 **单事务 17 步** |
| 节点安全接入 | `POST /nodes/install-token`：一次性 10min Join Token（Redis），gRPC 消费时绑定 node_id/cluster_id/SPKI 指纹；返回 sha256 校验式安装命令 |
| 带宽监控 | `GET /vms/{id}/bandwidth-metrics`：鉴权先行、Redis 时间序列、stale 标记、防伪造零流量 |
| 镜像模块 | 镜像源 CRUD（HTTPS + 域名 allowlist + egress 限额）、远端镜像同步任务、本地镜像按需拉取（状态机 pending→downloading→verifying→converting→injecting→available）、`vms.image_source_name` 追溯 |

安全设计要点：

- **RLS 租户隔离**：`kvmcloud_core` 全表 FORCE ROW LEVEL SECURITY，`app.tenant_id` 由服务端 `set_config` 注入（参数化）；
- **幂等**：INSERT 占位 → 冲突锁定比较 SHA-256 → 字节级重放已提交响应（崩溃后仍一致）；
- **审计**：append-only（触发器拒绝 UPDATE/DELETE），写操作全记录；
- **Agent 白名单执行**：仅 tc/virsh/qemu-img/curl/genisoimage/nproc/df/free 可达，参数列表执行不经 shell，接口名/镜像名/URL 域名白名单校验。

## 5. Agent 功能

| 能力 | 说明 |
| --- | --- |
| 注册/心跳 | JOIN_TOKEN 幂等注册；15s 心跳；90s 看门狗 |
| 建机 | qemu-img（backing clone）→ virsh define/start（幂等）→ TC QoS（按 MAC 定位 tap）|
| 镜像管道 | 白名单下载（域名 allowlist + 大小上限）→ SHA-256 → qemu-img convert → Cloud-Init seed ISO（genisoimage）；Windows 走 virtio 驱动 |
| 带宽 QoS | TC HTB `rate=min`、`ceil` 动态（借用/抢占联动），tap 接口名白名单校验 |
| 资源采集 | /proc + /sys 采集物理容量与使用量；内存气球（virsh setmem）、KSM 开关 |
| 命令白名单 | ExecRunner 白名单执行器，参数列表执行不经 shell |

## 6. API 一览（现行 `internal/api`）

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | /healthz | 存活检查 |
| GET/POST | /api/v1/nodes, /api/v1/nodes/{id} | 节点列表/详情/注册 |
| PUT | /api/v1/nodes/{id}/ratios | 在线调超开比 |
| GET | /api/v1/nodes/{id}/watermarks | 四维水位 |
| GET/POST | /api/v1/vms | VM 列表/放置 |
| GET | /api/v1/vms/{id} | VM 详情 |
| POST | /api/v1/vms/{id}/status | 状态更新 |
| GET/POST | /api/v1/pools | 共享带宽池 |
| GET/POST | /api/v1/policies | 带宽策略 |
| POST | /api/v1/policies/{id}/borrow · /release | 借用/回收 |
| GET | /api/v1/events · /api/v1/alerts | 借用事件/水位告警 |
| GET/POST | /api/v1/agent/tasks/pending · /report | Agent 内部任务通道（内部 API） |

## 7. Web 控制台（Vue 3）

六页面：**仪表盘**（四维水位总览）、**节点**（超开比调优）、**虚拟机**（放置 + 状态轮询 + error 展示）、
**带宽策略**（借用/回收/事件）、**共享池**、**水位告警**。
构建：`cd web && npm ci && npm run build`。

## 8. 安全模型摘要

- 源码零 SQL 文本（squirrel 参数绑定）；DDL 为静态迁移文件；
- 凭据零落源码：DATABASE_URL/JOIN_TOKEN 仅环境变量/env 文件（0600）；
- Agent 命令白名单 + 参数列表执行（无 shell）+ 接口名/路径/域名白名单；
- 审计 append-only；注册令牌常量时间比较，支持 TTL 与一次性。

## 9. 已知限制与路线图

- `kvmcloud_core` 安全基线 API 未接线生产流量（与现行链路并存，切换前须下线旧入口）；
- NATS/Outbox 通道已具备表结构与事件模型，Agent 侧仍为 HTTP 拉取（规划 gRPC 流/双通道）；
- 热迁移、安全组（nftables）、云盘快照、流量包计费、审计查询 API 契约已定义，实现按 `docs/api/core-design.md` 路线图推进。
