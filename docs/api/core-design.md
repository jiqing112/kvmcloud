# KVMCloud 核心 API 与安全基线

## 交付边界

- API 契约：`docs/api/core-openapi.yaml`，覆盖本文 12 个模块。
- 完整基线 DDL：`deploy/sql/core/001_schema.sql`；最小 HTTP/接入角色：`002_roles.sql`。
- 可编译核心骨架：`internal/coreapi/`，实现 `CreateVM`、`GenerateInstallToken`、`GetVMBandwidthMetrics`。
- 这是独立的 `kvmcloud_core` schema，不是现有 `public.*` 的增量迁移，不自动执行、不替换旧路由。
- 其余模块交付 API、表结构、事务/状态机约束，不假称已有完整 Agent 实现。
- 旧 `internal/api`、`internal/service` 及旧 OpenAPI 中的密码、SSH、公网免鉴权等行为不属于此安全基线；正式切换前必须关停旧入口。不要同时挂载两个 `POST /vms`。

## 1. REST API 契约

统一前缀 `/api/v1`；HTTPS；UUID 标识符。OIDC 验签或高熵 API Key 鉴权后注入 `Principal`，禁止信任请求体/请求头传入的租户身份。
OIDC 必须校验签名算法白名单、issuer、audience、exp、nbf；API Key 只保存 SHA-256，校验到期、撤销、成员关系和作用域。
`nodes:enroll` 必须来自平台管理员身份，不允许租户自行声明。平台接口与租户接口使用独立数据库角色。

写请求必须拒绝未知 JSON 字段，并在网关限制正文、速率和连接数。禁止 `password`、`root_password`、SSH 凭据、命令、脚本、任意 XML、文件路径、镜像下载 URL。
这里不提供任何“重置 Root 密码”接口。初始化访问凭据由客户在来宾系统或独立的密钥交付通道处理，不进入本平台控制 API/任务/事件。

除安装凭证签发外，写接口要求 `Idempotency-Key: <16..128 ASCII>`。作用域为租户、主体、HTTP 方法、规范化资源路径；保存规范化请求摘要及原响应至少 24 小时。
同键同体重放原响应，同键异体返回 `409 idempotency_conflict`。权限检查在重放之前；原请求最终状态仍通过任务查询。
凭证签发每次生成新令牌，不缓存可重放的明文凭证，不接受幂等头。

所有底层变更返回 `202 {vm_id?, task_id, state:"pending"}`，附 `Location: /api/v1/tasks/{task_id}`。任务创建与 Outbox 同事务；`202` 不代表 Agent 已经执行。
购买流量包在数据库内完成，返回 `201`，不伪造无意义的 Agent 任务。安装凭证返回 `201`。
错误格式：`{"error":{"code":"quota_exceeded","request_id":"UUID"}}`；不向客户返回 SQL、驱动错误、栈、宿主路径。

| 模块 | 路由 | 输入与结果 |
| --- | --- | --- |
| 1. 端口带宽 | `PUT /vms/{vm_id}/bandwidth` | 仅 `min_kbps,max_kbps,shared_pool_id,priority`，替换主网卡 egress 期望策略，202 |
| 2. 四维调度 | `POST /vms` | `name,image_id,network_id,vcpus,memory_mib,root_disk_gib,bandwidth`，202 |
| 3. 只读监控 | `GET /nodes/{id}/metrics` | 平台 `nodes:read`；缓存的四维物理用量、分配量、采样时间、stale |
| 3. 租户配额 | `GET /tenants/{id}/quotas` | 必须与当前租户一致；读取已聚合配额快照，非修改配额接口 |
| 3. 带宽监控 | `GET /vms/{id}/bandwidth-metrics` | 鉴权后 Redis GET，入/出 kbps、egress 字节、时间序列、stale |
| 3. 总览 | `GET /dashboard/summary` | 当前租户聚合；管理员聚合使用另一个受控读模型，不接受任意 tenant_id |
| 4. 安全接入 | `POST /nodes/install-token` | `cluster_id,expected_spki_sha256?`；201；节点 ID、10 分钟到期时间、安装命令 |
| 5. 流量包 | `POST /tenants/{id}/traffic-packages` | `product_id,quantity`；服务端定价、锁钱包、扣余额、订单/包/账本同事务，201 |
| 5. 余额查询 | `GET /tenants/{id}/traffic-packages` | 有效包、余量、购买/到期时间；字节和金额用十进制字符串 |
| 6. NAT | `POST /vms/{vm_id}/port-forwardings` | `public_ip_id,protocol,external_port,internal_port`；目标 IP 从主网卡取，202 |
| 6. NAT 查询/删除 | `GET /vms/{vm_id}/port-forwardings`、`DELETE /vms/{vm_id}/port-forwardings/{id}` | 删除也经任务确认后释放公网端口唯一占用 |
| 7. 变配 | `POST /vms/{id}/resize` | 新 CPU/内存/磁盘期望值、`allow_shutdown`；未授权关机绝不自动关机，202 |
| 8. 扫描 | `POST /nodes/{id}/adoption-scans` | 平台权限；空对象；Agent 只读枚举，202；扫描结果从任务安全结果读取 |
| 8. 纳管 | `POST /vms/adopt` | 平台权限且有目标租户上下文；`node_id,scan_task_id,domain_uuid,network_id,name,bandwidth`，202 |
| 9. 安全组 | `PUT /vms/{vm_id}/security-groups` | `groups:[{id?,name,rules:[...]}]`；规则仅类型化 allow 规则；原子替换绑定和策略，202 |
| 10. 快照 | `POST /volumes/{id}/snapshots` | `name,consistency:crash|filesystem`；卷状态锁与快照任务，202 |
| 11. 热迁移 | `POST /vms/{id}/migrate` | `target_node_id,mode:"live"`；目标二次预留和两端 fencing，202 |
| 11. 回收站 | `DELETE /vms/{id}` | 无正文；标记 recycling/deleted_at/purge_after，不立刻释放容量，202 |
| 11. 恢复 | `POST /vms/{id}/restore` | 保留期内恢复；状态检查，202 |
| 12. 审计 | `GET /audit-logs` | 当前租户；时间范围、动作、资源、游标；响应不含请求正文或凭证 |
| 13. 镜像源列表 | `GET /image-sources` | 当前租户可见（平台 domestic/standard + 本租户 custom）；不含 URL、allowlist |
| 13. 镜像源管理 | `POST /tenants/{id}/image-sources` | `name,base_url`；仅本租户；custom 源强制 HTTPS + 非空域名 allowlist（数据库 CHECK），201 |
| 13. 镜像源状态 | `DELETE /image-sources/{id}` | 仅本租户 custom 源；软停用 `state=disabled`，202 |
| 13. 发起镜像拉取 | `POST /image-sources/{id}/images` | `remote_name,arch,format,version`；源可达性校验；同租户幂等，202 |
| 13. 本地镜像列表 | `GET /images` | 当前租户本地镜像（含平台）；name/os_type/size/pull_state；不含 storage_ref |
| 通用任务 | `GET /tasks/{id}` | 当前租户；state/progress/error_code/safe_result；绝不返回原始命令负载 |

请求示例：

```json
{
  "name": "web-01",
  "image_id": "11111111-1111-4111-8111-111111111111",
  "network_id": "22222222-2222-4222-8222-222222222222",
  "vcpus": 4,
  "memory_mib": 8192,
  "root_disk_gib": 80,
  "bandwidth": {
    "min_kbps": 10000,
    "max_kbps": 100000,
    "shared_pool_id": null,
    "priority": 50
  }
}
```

单位固定：CPU 为 vCPU，内存 MiB，存储 GiB，带宽十进制 kbps，计费流量为整数 bytes；`kbps = bytes/second * 8 / 1000`，不要误用 1024。
`priority` 越大越优先，范围 0..100；实际可使用的优先级还必须受套餐/租户权限限制，不能靠自行填 100 获得跨租户抢占权。
所有列表使用稳定游标 `(created_at,id)`，默认 50、上限 200。跨租户资源一律 404；权限不足 403；未登录 401；参数非法 422；状态/容量/唯一端口冲突 409；依赖不可用 503。

## 2. 数据库与四维原子事务

DDL 涵盖身份、成员、API Key、接入记录、集群/节点、租户配额、镜像/网络、VM/网卡、带宽池/策略/租约、
资源预留、卷/快照、公网 IP/NAT、安全组、流量商品/钱包/订单/流量包/计量/扣减/账本、任务/Outbox、幂等与审计。
所有租户子资源用 `(tenant_id,id)` 外键阻止跨租户引用，并启用 FORCE RLS。
`nodes` 等平台资源不使用租户 RLS；仅向 HTTP 角色开放调度所需容量列的 UPDATE。

执行顺序：迁移拥有者执行 `001_schema.sql`，角色管理员执行 `002_roles.sql`（随后立即 `003_images.sql`——它创建镜像模块表、`kvmcloud_core_image_worker` 角色并完成列级授权）。
另外创建独立的 LOGIN 身份并授予相应 NOLOGIN 组角色；凭据来自 Secret Manager，不写入迁移。
不要让 HTTP 身份拥有 schema/table、SUPERUSER、BYPASSRLS、CREATEROLE 或任一特权 worker 角色。
DDL 不是幂等脚本：版本管理器执行一次，并记录 checksum；已有 public schema 需另写数据回填迁移。

每个租户事务使用参数化 `set_config('app.tenant_id', tenantUUID, true)`；只能从已认证主体取值；连接池严禁使用非 LOCAL 的 SET。
RLS 只提供纵深防护，不把“允许任意 SQL 的不可信客户”变成安全客户，因为相同 DB 角色本身可设置自定义 GUC。

`CreateVM` 实际代码路径：

```text
BEGIN READ COMMITTED
  SET LOCAL tenant / lock_timeout / statement_timeout
  校验 active 租户、镜像权限/大小、网络归属及集群
  插入 (tenant,principal,operation,key)；冲突则锁定并比较摘要/重放
  UPDATE quota ... WHERE 五项 used+requested <= max
  UPDATE shared_pool ... WHERE guaranteed+leased+new_min <= capacity
  SELECT 满足四维和心跳约束的节点 FOR UPDATE SKIP LOCKED
  UPDATE node ... WHERE 四维 used+requested <= floor(physical*ratio)
  INSERT VM、NIC、QoS、根盘元数据、resource_reservation(held)
  INSERT task(pending)、outbox、accepted audit
  保存幂等响应
COMMIT
返回 202，不在 Handler 发布 NATS 或访问 Agent
```

`image_source_name`（可选）：`POST /vms` 传入镜像源规范名时，CreateVM 校验该源对当前租户可见（平台源或本租户源）且 `state='active'`，并把规范名落库；Agent 端镜像流水线据此选择本地镜像或发起 `image.pull` 任务。

镜像拉取路径（Agent 侧，`image.pull` 任务）：

```text
校验下载 URL 域名 ∈ 源 allowlist（非白名单域一律拒绝）
HTTPS 下载到临时目录 → 限制并发/大小 → qemu-img info 校验格式
qemu-img convert → 目标盘存储路径（storage_ref 只能由镜像 worker 角色回报）
cloud-init 镜像注入 meta-data/user-data（不落命令行）
回报 size_bytes/observed_sha256/observed_version → local_images → done
```

所有资源写路径锁顺序统一为：租户 quota -> pool UUID 升序 -> node UUID 升序 -> VM/卷元数据；
预读版本不可代替锁定后的复核。同 VM 的部分唯一任务索引包括 `unknown`，避免未决任务与新指令重叠。
SQL 参数绑定，容量乘法转 numeric 后 floor，避免 Go float64 舍入；不是“先 SELECT 校验，再用另一个连接扣减”。

HTTP/连接在 COMMIT 回执前断开时结果可能已提交，返回 503 后客户端必须使用原幂等键重试；不能无条件补偿预留。
节点忙导致 SKIP LOCKED 暂无可用行返回 409，可短暂退避；不可把它当成容量永久耗尽。
资源比率调低也必须受 DDL CHECK 约束，不能降到当前已分配量以下。

带宽 ratio > 1 是统计超售准入，不是真实 SLA 保障。销售硬保障时必须强制 `bandwidth_ratio=1`，并按物理上联/共享池瓶颈准入。
内存超售不能替代宿主内存压力保护；实际 RAM、磁盘水位来自监控，临界时将 schedulable 关闭，不以虚拟已分配量代替物理余量。

## 3. Outbox、gRPC 与幂等 Agent

HTTP 服务只写数据库。独立 Outbox worker：

1. 短事务用 `FOR UPDATE SKIP LOCKED` 领取未发布且租约到期的事件，写 lease_owner/lease_until 后提交。
2. 通过 JetStream 发布到 `kvmcloud.commands.{node_uuid}`，`Nats-Msg-Id = event_id`，等待持久化 PubAck。
3. 用事件 ID 与 lease_owner 条件更新 published_at；失败指数退避，保留原事件，不释放资源。
4. PubAck 后崩溃可能重复投递。JetStream 去重窗口不是 exactly-once；任务 ID/期望版本与 Agent 持久化日志才是长期幂等基础。

worker 通过 gRPC mTLS 下发类型化 protobuf 指令；也可用 Agent 主动建立的双向流，让数据面无公网监听端口。
Bootstrap RPC 仅服务端 TLS + Join Token；注册成功后所有心跳、计量、状态回报都使用节点证书。
校验证书 SAN 中 node_id 与消息 node_id 一致；控制面复核 VM 当前节点/迁移 fencing token，Agent 不可声明其他节点或租户的资源。
NATS 使用 TLS 和逐主体 ACL；节点只能消费自己的 subject，不得使用全局 `>`；浏览器没有 NATS 权限。

protobuf/内部命令必须使用 `oneof {QoS,NAT,SecurityRules,Create,Resize,Snapshot,Migrate,...}`，
字段只有 ID、枚举、整数、CIDR、协议化资源配置，不得出现 `shell`、`command`、`root_password`、SSH 凭据或未验证 XML。
Agent 检查 node_id、任务 ID、deadline、generation、资源归属；过期或旧 generation 拒绝；幂等结果写本地持久化日志后回复。
租约过期/失联时将借用 ceil 收回 min；执行成功但回报丢失由相同期望状态重调谐，不重复创建实体。

只有 Agent 确认实体不存在或 fenced 后，控制面才能把预留从 held/committed 原子转 released 并返还账本。
任务超时改 unknown 并主动调谐，不能因网络超时直接将“可能已建成功”的资源额度释放。
不存在通用“失败全部回滚 Shell”；每种资源有补偿状态机，失败补偿也是有版本的任务。

## 4. Agent 内部安全执行

### QoS

- 优先通过 rtnetlink 编程；调用 TC 时使用固定绝对二进制与 `exec.CommandContext(path,args...)`，绝不调用 `sh -c`、`bash -c`。
- NIC UUID -> ifindex/tap 名由 Agent 根据本地 libvirt 元数据解析，不接收客户指定的 Linux 接口或 bridge。
- 校验 rate/ceil 整数范围、`0 <= min <= max`、priority 范围，classid/handle 由内部整数分配；禁止携带原始 TC token。
- 来宾出站是宿主 tap ingress，使用受控 IFB/redirect 或经过验证的 tc ingress 方案；来宾入站对应 tap egress，不能把方向写反。
- HTB rate 不能直接使用 0。`min=0` 作为 best-effort 端口单独处理，内部最小 quantum/rate 不宣称为客户保障，ceil 仍受有效授权约束。
- 同池借用/抢占锁定 pool；低优先级借用先降速并收到 ACK，或等待其租约过期，再授予高优先级；永不减少任何端口的 min。
- 跨节点共享池必须使用短时、有 generation 的额度租约；仅广播“池有余量”不能保证全局不超卖。

### NAT 与安全组

- 优先 netlink/nftables 类型化库和单批原子 transaction；规则 AST 内的 IP 用 `net/netip` 解析，协议 enum，端口 1..65535。
- 仅修改平台拥有的 `inet kvmcloud_*`/`bridge kvmcloud_*` 表和链；不 `flush ruleset`，不改宿主 INPUT/SSH/管理网规则。
- DNAT 仅使用数据库授权公网 IP 与客户 VM 主网卡地址，不接受任意目标地址，从而避免访问宿主管理网。
- 外部 `(public_ip,protocol,port)` 唯一索引防冲突；旧映射确认删除前保留唯一占用。
- NAT 只负责地址转换，不修改 tap 上的 TC qdisc/class；过滤在转发 hook/DNAT 后按 VM 身份与内部目标匹配，不把 NAT ACCEPT 当成安全组放行。
- 安全组默认拒绝、规则为 allow 并集；受控放行 ESTABLISHED/RELATED、必要 DHCP/ND，进行 MAC/IP 反欺骗，覆盖同宿主 east-west 流量。
- 明确管理 conntrack zone 与策略收紧后的既有连接失效策略；防止共享公网 IP 的不同 VM 因连接跟踪串流绕过隔离。
- 自定义规则名称、注释、CIDR 等从不进入 shell；即使使用 `nft -j -f -`，也从类型化 AST 生成 JSON，经 stdin 输入固定程序，不拼接规则文本。

libvirt 使用本地受限 socket/API；纳管 XML 从 Agent 只读取得，限制大小和深度、拒绝 DTD/外部实体，校验设备/存储/网络类型。
XML 构造使用 encoding/xml，不能把请求字段直接插入 XML；不允许客户注入 hostdev、qemu commandline 或任意宿主磁盘路径。
qemu-img 仅 Agent 固定二进制+参数列表，卷 UUID 在专用存储目录解析；使用符号链接/路径穿越防护与格式白名单。
迁移通过 libvirt 受控 TLS URI，不接收客户指定的 URI；数据面权限通过独立特权 helper 最小化，不给整个控制服务 root。

## 5. 一次性 Pull 接入

签发只允许 nodes:enroll；256 位随机 token，Redis 只保存 token SHA-256 派生键及非秘密 claims，`SET NX PX`，10 分钟失效。
安装命令固定部署 HTTPS URL，先校验由控制配置固定的 SHA-256 再执行；API 只返回字符串，不执行命令。
客户端自行执行安装；Agent 在本机产生私钥/CSR；私钥永不上传。安装包与二进制还需签名验证和固定发布版本。
可预先绑定 expected_spki_sha256；未绑定时 token 是 first-claim bearer，持有者即有接入权，必须在安全通道交付。

`ConsumeJoinToken` 用 Lua 原子校验 node/cluster/SPKI 绑定后删除；不能用 GET + DEL。
消费前验证 TLS 服务端身份、CSR 签名与私钥持有证明；验证失败不浪费令牌。消费后失败采用 fail-closed，管理员重新签发。

Redis 的 DEL 在故障切换时可能回退，因此只靠 Redis 不足以保证注册永久单次：

```sql
-- gRPC 注册 worker 的事务；独立角色，不是 HTTP/enrollment 签发角色。
BEGIN;
UPDATE kvmcloud_core.node_join_issuances
SET consumed_at = now()
WHERE id = $1 AND node_id = $2 AND cluster_id = $3
  AND token_sha256 = $4 AND consumed_at IS NULL AND expires_at > now();
-- 必须 RowsAffected=1，且 expected_spki_sha256 与已验证 CSR 一致。
-- 同事务 INSERT nodes(node_id,agent_spki_sha256,...)；唯一约束防重复身份。
-- 同事务写审计和证书签发任务；提交后由 CA signer 签发固定 SAN 的短期证书。
COMMIT;
```

CA signer 重试只为已持久化的相同节点/SPKI 签名，不因注册响应丢失给另一个密钥签证书；更新 consumed_at 后的未知结果先查注册状态。
令牌原文仅出现在首次 HTTPS 响应中；禁用此路由的 body capture、APM 正文和缓存，禁止在 URL、日志、审计、浏览器 localStorage 中保存。
命令里的 `--token` 可能进入终端历史/进程参数；生产安装器应提供 `--token-stdin` 或受限文件描述符输入，前端标注敏感并在到期时销毁展示。
不使用 HTTP 安装 URL，不使用 curl `-k`，不返回主机 SSH/Root 凭据。

## 6. 流量包与 FIFO 计费

购买事务：校验 URL 租户与主体 -> 幂等 -> 锁 tenant quota 行（租户写入串行点） -> 读取服务端商品和币种 ->
守卫 `wallet.balance_minor >= 服务端价格*数量` 扣款 -> 插入 paid 订单、流量包、负向账本、审计 -> 提交。
金额始终最小货币单位整数；乘法先检查溢出；不接受客户端价格、总流量、余额或已付款标记。

Agent 按网卡 attachment_epoch 和单调 sequence 上报 egress 增量，先持久化本地未确认区间再发送，重传使用同一去重键。
节点证书身份、当前 VM 归属、epoch fencing 和序号连续性均需复核；乱序可暂存但不越过缺口计费；重启不能把累计计数器当成新增用量。
迁移/重启创建新的受控 epoch，旧 epoch 完结后关闭，禁止两个节点同时对一个有效 epoch 记费。
`usage_reports(vm_id,attachment_epoch,sequence)` 唯一约束防重复；原始 report 不更新字节，仅设置 billed_at。

FIFO worker 在一个事务内：

```sql
BEGIN;
SELECT tenant_id FROM kvmcloud_core.tenant_quotas WHERE tenant_id=$1 FOR UPDATE;
SELECT * FROM kvmcloud_core.usage_reports WHERE id=$2 AND tenant_id=$1 FOR UPDATE;
-- billed_at 非空直接成功，不重复扣减。
SELECT id, remaining_bytes FROM kvmcloud_core.traffic_packages
WHERE tenant_id=$1 AND purchased_at <= $3 AND expires_at > $3 AND remaining_bytes>0
ORDER BY purchased_at, id FOR UPDATE;
-- $3 为 report.received_at（入库时数据库生成的结算时刻）。
-- 按序 take=min(remaining_bytes, uncharged_bytes)：
-- UPDATE ... SET remaining_bytes=remaining_bytes-$take WHERE remaining_bytes >= $take;
-- INSERT traffic_debits(kind='package', bytes=$take, report_id,package_id)。
-- 剩余流量写 kind='overage'，按锁定的计费版本扣钱包或形成欠费处理事件。
UPDATE kvmcloud_core.usage_reports SET billed_at=now() WHERE id=$2 AND billed_at IS NULL;
COMMIT;
```

不要对包使用 SKIP LOCKED，否则会跳过更早购买的包、破坏 FIFO。筛选到期包后才 FIFO，不是按 expires_at 排序的 FEFO。
本基线以接收入库时刻结算有效期，购买前收到的报表不能用后来购买的包抵扣；实际流量时刻结算需要将区间按购买/到期边界拆分，并另定义迟报政策。
不要从 Redis rate 图表进行计费。无流量包时仍必须记 overage，不能把未抵扣部分丢掉。精确 bytes 用 NUMERIC(20,0) 与 Go 整数/decimal，不能用 float64。

## 7. 生命周期与故障边界

| 操作 | 同步事务 | Agent 与完成条件 |
| --- | --- | --- |
| 带宽更新 | quota 差量、按 UUID 排序锁新旧池、节点 BW 差量、策略 version、任务/Outbox | 先安全降借用，再应用 QoS；未 ACK 保留旧/新占用中较大者，成功后才释放差额 |
| Resize | 锁 quota/pool/node 后复核 VM 版本；只预留正差量，保存旧规格与目标任务 | 不先释放旧资源；磁盘只扩容。热插拔不支持且 allow_shutdown=false 则失败，不偷偷关机；部分扩容成功必须重新观测，不声称可将磁盘缩回 |
| Adoption | 只用指定节点最近扫描任务结果，唯一 domain_uuid，验证目标租户授权、网络/卷所有权 | 不执行用户 XML；预留四维并补 QoS。扫描节点保持 draining，先重建已有域占用再开放调度，避免账本重复或漏记 |
| Snapshot | 锁定卷/VM 活跃任务，记录一致性与配额预留 | 有受信 guest-agent 才能 filesystem freeze，所有失败分支必须 thaw；磁盘快照不假称内存一致；成功后记实际保留字节 |
| Live migrate | 锁两节点升序，目标预留完整规格，源占用保留；租户规格不重复计费 | TLS 迁移、存储/CPU 兼容性、目标 QoS/NAT/安全组预装；fencing 确认唯一活动实例后更新归属、释放源预留 |
| Recycle | 标记 recycling/deleted_at/purge_after、独占任务，不释放预留 | 停机确认转 recycled；保留卷和资源防无配额恢复；保留期后 purge 任务确认域/卷清理再返还资源 |
| Restore | 锁 VM 确认在保留期内且无 purge 活动任务，恢复 desired state | 未完全销毁才可恢复；不假定已删除磁盘能够找回 |

快照占用、数据盘和根盘都计入 tenant.max_disk_gib；根盘已由 VM 预留覆盖，不再重复计一次；额外挂盘/快照分别用正差量预留。
生产快照和薄置备还需物理水位保护，不得仅依赖虚拟 size_gib。
迁移必须暂停该 VM 的冲突策略任务；共享公网 IP 的 NAT 网关可独立于计算节点，迁移时更新其路由/目标身份并保持 TC 仍在新 tap 上。

## 8. 只读指标与审计

采集器使用 gRPC mTLS 接收节点指标，写 Redis/Prometheus；浏览器读接口不向 Agent 发起“实时采集”。
Redis key 包含 `tenant_id:vm_id`；先数据库验证资源归属再读取 key，不能通过猜 key 绕过隔离。
节点指标仅平台可读；租户配额与 dashboard 使用当前租户聚合快照，定期与 PostgreSQL 权威账本核对，附 collected_at/stale。
缓存 miss/故障返回 `503 metrics_unavailable`；过期但尚存在的样本显式 `stale:true`，前端画断点，不画虚假的零值。

HTTP 中间件生成 request_id、验证主体/作用域、记录不含正文的结果；真正的成功写审计必须随业务事务提交，不能只在 c.Next 后“尽力写一条”。
拒绝/失败请求由独立审计通道 append，异步任务完成也 append；日志脱敏采用允许字段列表，而不是事后正则删除所有可能秘密。
DDL 对 audit_logs、billing_ledger、traffic_debits 禁止 UPDATE/DELETE/TRUNCATE，HTTP 角色不具备这些权限或 DDL 权限。
这保证应用身份不可改写，不保证数据库超级用户不可篡改。审计 worker 还需将事件 ID/摘要签名并送往独立账户的对象存储 WORM/Object Lock；签名密钥不在数据库。
审计查询按租户 RLS 与稳定游标分页；管理员全局审计通过独立只读服务和独立授权，不给普通 HTTP 角色 BYPASSRLS。

## 9. Vue 3 组件结构

```text
views/NodesView.vue
  NodeJoinDialog.vue          平台权限；cluster 选择、可选 SPKI 绑定、请求生成
  InstallCommandPanel.vue     只读 pre/code、复制图标及 tooltip、到期倒计时
  NodeRegistrationStatus.vue  轮询安全节点读模型；成功/到期/关闭即清空敏感状态
views/VMDetail.vue
  BandwidthPolicyForm.vue     min/max 数字输入、pool 下拉、priority stepper、异步任务状态
  BandwidthMetricsChart.vue   ECharts 时序；ingress/egress/min/max，标记 kbps 和采样时间
views/BillingView.vue
  TrafficBalanceSummary.vue   有效总余额、近期到期余量、overage/欠费
  TrafficPackageTable.vue     购买时间 FIFO 排序、到期时间、剩余/总量 progress
  PurchaseTrafficDialog.vue   商品与数量，服务端订单金额确认；禁用重复支付
composables/
  useTaskPolling.ts           task_id、AbortController、退避、unknown/failed/succeeded
  useBandwidthMetrics.ts     单请求在途、可见页面轮询、取消卸载、stale 与 gap
  useIdempotentMutation.ts    每次用户操作一个键；网络重试保留原键
```

命令用 Vue 文本插值或 readonly textarea，不用 v-html；`navigator.clipboard.writeText` 仅在用户点击时触发，在 HTTPS 下处理复制失败。
令牌只保存在对话框短生命周期内存，不进 Pinia 持久化、URL、日志、sessionStorage/localStorage；到期后禁止复制并清空。
带宽图表区分“期望策略”与“已应用策略”，任务失败展示 error_code，不能点击保存就假定生效；隐藏页面暂停轮询、断流用 null。
流量包 bytes 以字符串传输，用 BigInt/decimal 计算总量和剩余，最后格式化为 GiB；不可先转 Number 后计算财务/流量余额。
购买成功后重新拉取包与钱包；显示币种和到期时间；后端强制 tenant match，前端隐藏按钮不是授权。

## 10. 接线与验证

`coreapi.New(tenantDB,enrollmentDB,redisAdapter,authenticate,installerHTTPSURL,pinnedSHA256)` 构造独立 Router，
放在独立安全端口或替换旧 HTTP 服务；认证实现与 Redis 客户端必须显式注入，缺失时构造失败。
不要复用 `cmd/control` 现有免鉴权 Router，也不要在旧路由下面无保护地转发新接口。

Redis 适配器可以使用 go-redis/v9；不在此骨架引入第二套客户端/消息库版本：

```go
type RedisAdapter struct { Client *redis.Client }
func (r RedisAdapter) Get(ctx context.Context, key string) (string, error) {
    value, err := r.Client.Get(ctx, key).Result()
    if errors.Is(err, redis.Nil) { return "", coreapi.ErrCacheMiss }
    return value, err
}
func (r RedisAdapter) SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
    return r.Client.SetNX(ctx, key, value, ttl).Result()
}
func (r RedisAdapter) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
    value, err := r.Client.Eval(ctx, script, keys, args...).Result()
    if errors.Is(err, redis.Nil) { return nil, coreapi.ErrCacheMiss }
    return value, err
}
```

Redis 配置 TLS、独立 ACL/前缀，命令日志关闭秘密正文。监控写角色不能读 join 前缀。
gRPC bootstrap、CA signer、Outbox publisher、计费 worker 的生产适配和对应细粒度角色仍需实现；
SQL/Handler 骨架不是可直接替换的完整生产控制面。
