# KVMCloud 工程设计交付总览

> 控制面（Control Plane）与数据面（Agent）分离；API First 且安全隔离。
> 所有 API 仅作为**声明式**控制接口：只存元数据、下事件，绝不直接操作底层，
> 绝不传递 SSH 凭证 / Root 密码，绝不在 Handler 中拼接 Shell。

## 0. 端到端控制回路（所有写路径统一）

```
客户端 → Gin Handler（参数校验 → scope/RLS 鉴权 → 幂等键）
       → 单事务：元数据入库 + 配额/容量账本守卫扣减 + task(pending)
                  + outbox 事件（声明式 payload，无凭证/路径/shell）
       → 202 Accepted { task_id } + Location: /api/v1/tasks/{id}
       → outbox publisher → NATS (kvmcloud.commands.{node_id})
       → Agent 白名单执行（TC / nftables / qemu-img / virsh / curl）
       → gRPC 回报 → task 状态机 pending→running→succeeded|failed
前端轮询 GET /api/v1/tasks/{id}
```

- 幂等：`Idempotency-Key` + `idempotency_keys(request_sha256)` 重放已提交响应；
  任务级幂等由 `tasks_vm_mutation / tasks_volume_mutation` 活跃唯一索引兜底。
- 回退：事务内失败整体回滚（账本自动恢复）；Agent 执行失败走任务 failed 分支
  回退账本与清理残留，绝不补偿已提交的歧义状态（COMMIT 丢失 → 503 + 原键重试）。

## 1. 交付物索引

| 交付物 | 位置 |
| --- | --- |
| 全量 DDL（评审快照） | [01_full_ddl.sql](./01_full_ddl.sql)；权威分片：`deploy/sql/core/001_schema.sql`、`002_roles.sql`、`003_images.sql` |
| 核心 Handler 骨架 | `internal/coreapi/`（见 §2 映射） |
| Agent 执行器骨架 | `internal/agentlib/`（qos / nftables / imagepipeline / inject / provision） |
| 安全设计说明 | [03_security.md](./03_security.md) |
| 前端（Vue 3）交互建议 | [04_frontend.md](./04_frontend.md) |

## 2. 模块 → 表 → Handler → 任务/事件 → Agent 执行 映射

| # | 模块 | 核心表（kvmcloud_core schema） | Handler | 任务种类 / 事件 | Agent 执行器 |
| --- | --- | --- | --- | --- | --- |
| 1 | 端口级带宽调度 | `bandwidth_policies`、`shared_bandwidth_pools`、`bandwidth_leases`、`vm_nics` | `PUT /vms/:id/bandwidth` → `SetVMBandwidth` | `vm.bandwidth` | `QoSApplier`（TC HTB：rate=min / ceil=仲裁值）+ `bandwidth.Decide`（借用/抢占纯函数） |
| 2 | 四维超开调度 | `nodes`、`tenant_quotas`、`resource_reservations` | `POST /vms` → `CreateVM`（五维配额+四维节点原子扣减，SKIP LOCKED） | `vm.create` | `Provisioner`（qcow2 + virsh define/start） |
| 3 | 资源/状态只读查询 | `nodes`、`tenant_quotas`、Redis 聚合 | `GetNodeMetrics`、`GetTenantQuotas`、`GetVMBandwidthMetrics`、`GetDashboardSummary` | —（只读，不触发底层） | — |
| 4 | 节点安全接入（Pull） | `clusters`、`node_join_issuances` | `POST /nodes/install-token` → `GenerateInstallToken`（Redis 一次性 Token + 校验和钉死的 install.sh） | gRPC 注册/CSR | gRPC mTLS 注册（`grpcserver`，SPKI 指纹绑定） |
| 5 | 流量包与计费 | `traffic_products/orders/packages`、`tenant_wallets`、`usage_reports`、`traffic_debits`、`billing_ledger` | `POST /tenants/:id/traffic-packages` → `BuyTrafficPackage`（钱包守卫扣减） | 上报侧：Agent Egress 周期上报 | FIFO 抵扣在计费引擎（`traffic_packages_fifo` 索引序 + 唯一索引幂等） |
| 6 | 单 IP NAT 转发 | `public_ips`、`port_forwardings` | `POST /vms/:id/port-forwardings` → `CreatePortForwarding` | `vm.nat` | `NftApplier.ApplyNAT`（与 TC 分层共存） |
| 7 | 在线变配 | `vms(desired/observed_version)`、`nodes` | `POST /vms/:id/resize` → `ResizeVM`（增量守卫扣减） | `vm.resize` | virsh 热插拔；失败回退账本 |
| 8 | 无缝纳管 | `vms(state=adopting)` | `POST /vms/adopt` → `AdoptVMs`（扫描任务） | `vm.adopt` | Agent 扫描域 XML 上报 → 入库后补发 QoS（`vm.bandwidth`） |
| 9 | 分布式安全组 | `security_groups(+rules)`、`vm_security_groups` | `PUT /vms/:id/security-groups` → `SetVMSecurityGroups`（规则集全量下发） | `vm.security` | `NftApplier.ApplySecurityGroups`（独立 inet 表，与 NAT 共存） |
| 10 | 云盘与快照 | `volumes`、`volume_snapshots` | `POST /volumes/:id/snapshots` → `CreateSnapshot` | `volume.snapshot` | qemu-img snapshot / RBD snap（参数化） |
| 11 | 热迁移与回收站 | `vms(state=migrating/recycling + purge_after)` | `POST /vms/:id/migrate`、`DELETE /vms/:id` | `vm.migrate`、`vm.recycle`→`vm.purge` | libvirt 迁移；延迟物理清理（默认 7 天） |
| 12 | 审计与监控 | `audit_logs`（append-only 触发器） | `GET /audit-logs`、`GET /tasks/:id` | 中间件/事务内写入 | — |
| 13 | 镜像源与自动装机 | `image_sources`、`remote_images`、`local_images`、`vms.image_source_name` | `CreateImageSource` / `ListImageSources` / `UpdateImageSource` / `DeleteImageSource` / `SyncImageSources` / `ListRemoteImages` / `PullLocalImage` / `ListLocalImages`；`CreateVM` 内置自动拉取分支 | `image.source.sync`、`image.pull`（控制面 worker）；`vm.create` 链内拉取（Agent） | `ImagePipeline`：DownloadImage → VerifyImage → ConvertImage → InjectCloudInit/InjectVirtioDrivers |

### 镜像模块状态机（PullPipeline）

```
local_images.pull_state:
pending → downloading → verifying → converting → injecting → available
                                   ↘ failed（error_code）↘ retired
```
每步由 NATS 事件/任务回报驱动；`observed_sha256` 由 Agent 下载后回传
（列级授权：HTTP 角色禁写 `storage_ref / observed_sha256`，仅镜像 worker 角色可写）。

### CreateVM 自动拉镜像分支（返回 202 + task_id）

1. 四维超开事务内按 `image_source_name` 查本地可用镜像；
2. 未命中 → 解析对该租户可见的 `remote_images`（active 源按 priority 排序）→
   插入 `local_images(pending)`（OS 类型来自 `image_os_type`，缺省 linux）；
3. `vm.create` 任务的 outbox 负载携带 `local_image_id`：Agent 先执行
   DownloadImage→VerifyImage→ConvertImage（→ Windows 注入 virtio），成功后建机并回报；
4. 响应体 `image_pull_required: true`，前端据此提示"镜像将异步拉取"并轮询任务。

## 3. 关键工程约定

- **SQL**：全部经 squirrel 参数绑定（占位符 `$n`）；事务首语句绑定
  `app.tenant_id`（RLS）+ `lock_timeout=2s` + `statement_timeout=5s`；
  错误响应只含机器可读 code，永不回传驱动/SQL 文本。
- **调度锁序**：tenant quota → shared pool → node（`FOR UPDATE SKIP LOCKED`）→ VM 元数据。
- **任务互斥**：`tasks_vm_mutation`（每 VM 一个活跃任务）/
  `tasks_volume_mutation`（每卷一个）唯一部分索引。
- **审计**：`audit_logs` append-only（触发器拒绝 UPDATE/DELETE/TRUNCATE），
  写路径事务内落 `safe_metadata`（脱敏）。
- **角色最小授权**：`kvmcloud_core_api`（HTTP，NOBYPASSRLS，列级写权限受限）、
  `kvmcloud_core_image_worker`（唯一可写 `storage_ref/observed_sha256`）、
  `kvmcloud_core_enrollment`（仅接入签发）。见 `002_roles.sql`。
