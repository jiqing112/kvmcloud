# kvmcloud

四维超开的轻量 KVM 云平台：**CPU / 内存 / 磁盘 / 网络带宽**统一超分调度，
带宽支持 **保障（min）— 借用（borrow）— 抢占（preempt）** 语义与租户共享带宽池。

```
┌─────────────────────────────── 控制面 kvmcloud-control ───────────────────────────────┐
│  HTTP API :8080（Gin）          gRPC :9443（节点注册/心跳）         水位监控（≥90% 告警）  │
│  ├─ 四维超开放置 PlaceVM        ├─ Register（JOIN_TOKEN，幂等）      └─ overcommit_alerts │
│  ├─ 带宽仲裁（保障/借用/抢占）  └─ Heartbeat（快照 + 看门狗 offline）                     │
│  └─ 共享池管理                                                PostgreSQL（ squirrel）  │
└──────────────────────────────────────────┬───────────────────────────────────────────┘
                                     gRPC（9443）
┌──────────────────────────────────────────┴──────────── 计算节点 kvmcloud-agent ────────┐
│  注册/心跳 · TC HTB QoS（rate=min, ceil 动态）· 资源采集 · 内存气球 · KSM               │
└────────────────────────────────────────────────────────────────────────────────────────┘
```

## 核心语义

**四维超开**（每节点可独立调优）：

| 维度 | 校验公式 | 默认比 |
| --- | --- | --- |
| CPU | `ΣvCPU ≤ physical × ratio` | 4.0 |
| 内存 | `Σmem ≤ physical × ratio` | 1.5 |
| 磁盘（精简置备） | `Σdisk ≤ physical × ratio` | 2.0 |
| 带宽 | `Σmin_kbps ≤ NIC × ratio` | 1.0 |

放置请求任一维超限即拒绝（`409 overcommit_exceeded`，错误含维度名），四维同事务原子更新。

**带宽三场景**（`internal/bandwidth`，纯函数 + advisory lock 串行化同池）：

1. **保障**：创建策略时两级校验——节点侧 `Σmin ≤ NIC×ratio`、池侧 `Σmin ≤ capacity`；
2. **借用**：池余量充足直接授予（`granted`），ceil 从 min 提升到 min+granted；
3. **抢占**：余量不足按优先级压回低优先级借用方（永不低于其 min），凑足授予（`preempted`），
   凑不足整体拒绝（`denied`）；全部事件落 `borrow_events` 审计。

**水位告警**：任一维 `allocated/(physical×ratio) ≥ 0.90` 写入告警（阈值可配，10 分钟冷却），
心跳回执携带四维水位。

## 快速开始

```bash
# 单机全栈（开发/体验/小规模生产）
export DATABASE_URL="postgres://kvmcloud:<密码>@127.0.0.1:5432/kvmcloud"
sudo -E ./install.sh all-in-one

# 生产：主控
sudo -E ./install.sh control
# 生产：每台计算节点（不装 PG/Redis/NATS，只装 KVM/libvirt/TC）
sudo ./install.sh agent --control-addr 10.0.0.10:9443 --token <JOIN_TOKEN> \
    --cpu-ratio 4 --mem-ratio 1.5 --disk-ratio 2 --bw-ratio 1
```

三形态（all-in-one / control+多agent / agent-only）行为完全一致：
依赖服务只装一份；Agent 不监听端口；端口规划 8080/9443/5432/6379/4222。
完整步骤与验收见 [docs/deploy.md](docs/deploy.md)。

## 开发

```bash
make build     # bin/kvmcloud-control + bin/kvmcloud-agent
make test      # 单元测试（调度/仲裁/水位/QoS 命令生成）
make coverage  # 覆盖率
cd web && npm ci && npm run build   # 控制台（Vue3 + Vite）
```

前端六个页面：仪表盘（四维水位总览）/ 节点（超开比调优）/ 虚拟机（放置）/
带宽策略（借用/回收/事件）/ 共享池 / 水位告警。

## 安全设计

- **源码零 SQL 文本**：语句经 squirrel 构建器生成，参数一律绑定占位符；DDL 为内嵌静态文件。
- **凭据不落源码**：`DATABASE_URL`、`CONTROL_JOIN_TOKEN` 等只从环境变量读取；
  env 文件 0600；`config/*.env.example` 不含可用凭据。
- **命令白名单**：Agent 侧仅 tc/virsh/nproc/df/free 可达，参数列表执行不经 shell，
  接口名正则白名单校验。
- **令牌校验**：注册令牌常量时间比较，支持有效期与一次性使用。

## 目录

```
api/proto/           gRPC 协议（Register/Heartbeat/QoS 下发）
cmd/control|agent    两个入口
internal/            store / bandwidth / monitor / grpcserver / api / agentlib / auth / model / pb
deploy/              systemd 单元
web/                 Vue3 控制台
install.sh           三形态安装/卸载
docs/deploy.md       部署指南（含 API 示例与故障排查）
```
## 赞助者

九霄云-买服务器就找九霄云

## License

Apache-2.0
