# 安全设计说明（Agent 执行面 + API 面）

## 1. Agent 接收 QoS / NAT / 安全组 / 镜像指令时的内部防线

### 1.1 白名单校验

| 维度 | 实现 | 位置 |
| --- | --- | --- |
| 命令白名单 | `ExecRunner` 仅 10 个二进制可达（`tc / virsh / qemu-img / curl / genisoimage / nft / timeout / nproc / df / free`），每个调用点的可执行文件名是**字符串字面量**；`default` 分支拒绝一切其他命令 | `internal/agentlib/qos.go` |
| 域名白名单 | 下载 URL 主机必须**精确命中**源的 `domain_allowlist`（大小写不敏感）；allowlist 条目本身禁止 IP；`custom` 源强制非空（DDL CHECK） | `ImagePipeline.checkURL` + `image_sources.domain_allowlist` |
| 路径白名单 | 落盘路径 = `DataDir` 前缀 + 过正则的规范名（`^[A-Za-z0-9][A-Za-z0-9_.-]+$`）；ISO 挂载要求 `filepath.Clean` 后仍在 `DataDir` 内且后缀 `.iso`；`storage_ref` 形如 `images/{uuid}.qcow2`（DDL CHECK） | `ImagePipeline` / `AttachISO` / `local_images.storage_ref` |
| 接口名白名单 | `iface` 匹配 `^[a-z0-9][a-z0-9._-]{0,14}$` | `ValidateIface` |
| 域名/表名 | nftables 表/链名固定字面量（`kvmcloud_nat` / `kvmcloud_sec`），租户输入无通路 | `NftApplier` |

### 1.2 参数化执行（禁止字符串拼接进 shell）

- **不经 shell**：全部 `exec.Command(字面量, args...)` 参数列表执行；事件负载只含
  声明式标识与规格（UUID / 枚举 / int64），无任何命令字符串字段。
- **数值参数**：仅接受 `int64` 格式化结果（`strconv.FormatInt`），端口 1..65535、
  kbps 上限 10^12，由 Handler 正则/范围校验 + DDL CHECK 双层约束。
- **nftables**：规则文件由结构化字段生成——协议枚举（tcp/udp/icmp/icmpv6/any）、
  `netip.ParseAddr/ParsePrefix` 严格解析（族与 ethertype 一致性双保险）、
  ICMP type/code 0..255；经 `nft -f <file>` 原子应用，绝无拼接进命令行的路径。
- **qemu-img**：`-f` 来自 `formatFlag` 枚举映射（qcow2/raw/iso→raw/vhd→vpc），
  `-O` 固定 `qcow2`；`-f`/`-O` 之外的参数全部为已校验路径。

### 1.3 资源限制

| 资源 | 上限 |
| --- | --- |
| 下载大小 | `curl --max-filesize`（≤16GiB，`egress_bytes_limit` 收敛）+ `HashFile` 读取上限 |
| 下载时长 | `curl --max-time 7200`；转换/生成 ISO 经 `timeout --kill-after` 进程级硬超时（1h/5m） |
| 重定向 | `curl --max-redirs 3`；协议钉死 `--proto '=https'` |
| 请求体 | Handler 侧 64KiB `MaxBytesReader` + `DisallowUnknownFields`（凭证形态字段直接 400） |
| 规则/结果集 | 安全组 ≤50 组/VM、≤200 规则下发；NAT ≤256 映射；列表 ≤200/500 行 |
| SQL | 事务级 `lock_timeout=2s`、`statement_timeout=5s`；任务租约 `lease_until` 防僵死 |
| SSRF | 仅 https；主机为 IP 字面量（环回/私有/保留地址）一律拒绝；URL 只能来自控制面 `remote_images.download_url`（源同步 worker 写入），API/Agent 均不接收任意 URL |

### 1.4 幂等与回退

- **幂等**：
  - API 层：`Idempotency-Key` + 请求摘要，重放已提交响应（含崩溃后）；
  - 任务层：每 VM/卷仅允许一个活跃任务（部分唯一索引），重复下发 409；
  - 执行层：镜像产物存在即幂等成功；nftables 声明式全量收敛（flush 后重建，
    重复应用结果一致）；libvirt 域已定义则确保运行。
- **回退**：
  - 事务内任一步失败 → 整体回滚（配额/节点/池账本自动恢复）；
  - 镜像管道失败 → `cleanup` 删除 `.partial` 与半成品，不留残留；
  - Cloud-Init 注入失败 → 删除 seed 目录；
  - nft 删除不存在对象 → 幂等视为成功；
  - Agent 任务失败 → 控制面 failed 分支回退账本（如迁移：释放目标预留、回填源计数）；
  - COMMIT 回执丢失属歧义状态 → 不做外部补偿，返回 503 要求原幂等键重试。

## 2. API 面不变量（全部 Handler 强制）

1. **身份只来自网关**：`Authenticate` 钩子（OIDC / API Key SHA-256）注入
   `Principal{ID, TenantID, Scopes}`；绝不信任请求体。scope fail-closed。
2. **租户隔离纵深**：RLS（`app.tenant_id`）+ 显式 `tenant_id` 绑定 + 归属
   SELECT 前置；不存在与越权一律 404（不泄露存在性）。
3. **无凭证通道**：接入走一次性 Join Token（Redis SETNX + Lua 原子消费 + SPKI
   绑定）；install.sh 以 HTTPS + SHA256 校验钉死；API 不接收 SSH/密码/命令。
4. **错误脱敏**：响应仅含 `error.code + request_id`；23505 → `resource_conflict`，
   其余 DB 错误 → `transaction_retry`（503）。
5. **审计不可篡改**：写路径事务内追加 `audit_logs`，触发器拒绝更新/删除/清空。
6. **声明式 outbox**：事件负载 = UUID + 枚举 + 数值规格，结构化校验后经
   JetStream（`Nats-Msg-Id` 去重）投递；无凭证、无路径、无 shell 片段。
7. **列级授权**：`storage_ref / observed_sha256` 仅镜像 worker 角色可写，
   HTTP 服务角色禁写——伪造本地路径在数据库层即被拒绝。
