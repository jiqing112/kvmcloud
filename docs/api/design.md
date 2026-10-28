# KVMCloud 云主机生命周期 Open API 设计说明

配套文件：

- OpenAPI 3.0 规范：[`kvmcloud-openapi.yaml`](./kvmcloud-openapi.yaml)
- 任务模型与投递抽象：`internal/task/`
- 业务编排（CreateVM / ResizeVM）：`internal/service/vm.go`
- 租户面 Handler 与鉴权：`internal/api/vm_lifecycle.go`

> 以下三段代码均已随本次设计入库并通过 `go build ./...` / `go vet`；
> 本文档给出需要落库的 SQL、NATS 生产接线与调用示例。

## 1. 请求时序

```
租户                控制面(Gin)                 Store/Postgres            NATS JetStream          Agent(gRPC)
 │ POST /vms             │                            │                        │                     │
 │ Bearer <API_Key>      │                            │                        │                     │
 ├──────────────────────>│ ①鉴权(SHA-256摘要查api_keys) │                        │                     │
 │                       │ ②ValidatePolicy(min≤max)   │                        │                     │
 │                       │ ③QuotaLedger.Reserve ──────>│ tenant_quotas 守卫UPDATE│                     │
 │                       │ ④PlaceVM 四维超开预留 ──────>│ nodes 守卫UPDATE(原子)  │                     │
 │                       │ ⑤VM(creating)/VNIC/策略落库─>│                        │                     │
 │                       │ ⑥task.New + CreateTask ────>│ tasks(pending)         │                     │
 │                       │ ⑦Publisher.Publish ════════╪═══════════════════════>│ kvmcloud.tasks.vm.create
 │<── 202 {task_id} ─────┤                            │                        │ worker 消费 ───> 建域/QoS(tc htb)/注IP
 │                       │                            │  progress 回写 <───────┤<──────────────────── gRPC mTLS
 │ GET /tasks/{id}       │                            │                        │                     │
 ├──────────────────────>│ 读 tasks ──> 200 {state,progress,result}            │                     │
```

同步段（①~⑦）失败时按逆序补偿（service 层 rollback 栈），节点四维账本与租户配额不留悬空；
补偿逻辑在生产实现中应整段并入单事务，rollback 栈保留为兜底。

## 2. 关键设计决策

| 决策 | 说明 |
| --- | --- |
| `min_kbps ≤ max_kbps` 双层校验 | 请求绑定层 + Service 层各校验一次（Service 为权威层，供内部调用复用）。min 是**保障带宽**，参与四维超开 BW 维度预留（写节点 NIC 账本或共享池账本）；max 只是借用天花板。颠倒会让调度器拿天花板占保障额度，且 Agent 侧 tc htb 无法生成 `ceil < rate` 规则。 |
| 预留同步、执行异步 | 四维超开校验与配额扣除在 202 之前同步完成（`store.PlaceVM` 守卫式 UPDATE 天然防并发竞态），Agent 建机/QoS/注 IP 走 NATS 任务。校验不过同步返回 4xx/409，用户无需轮询即知结果。 |
| 幂等 | JetStream `Nats-Msg-Id = IdempotencyKey` 去重（2min 窗口）+ `tasks(tenant_id, idempotency_key)` 唯一约束兜底；重试创建不会重复建机。 |
| 任务互斥 | 变配/IP/重装前检查 `HasActiveTask(vm_id)`（部分索引 `WHERE state IN ('pending','running')`），冲突返回 409 `task_in_progress`。 |
| 变配换容 | 释放旧规格 → 原子预留新规格 → 失败自动回补旧规格；磁盘缩容 422；运行态负载带 `allow_hot_plug` 提示，Agent 评估热插拔、不支持则冷迁移。 |
| 敏感信息 | API Key 库内只存 SHA-256；任务 payload 不出控制面（`GET /tasks` 响应剥离 payload，含初始密码的负载只经 gRPC mTLS 到 Agent）。 |

## 3. 新增存储（DDL 草案）

```sql
-- 租户 API Key：明文仅创建时展示一次，库存 SHA-256 hex
CREATE TABLE IF NOT EXISTS api_keys (
    key_hash     CHAR(64) PRIMARY KEY,
    tenant_id    UUID NOT NULL,
    name         TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ
);

-- 异步任务
CREATE TABLE IF NOT EXISTS tasks (
    id              UUID PRIMARY KEY,
    tenant_id       UUID NOT NULL,
    vm_id           UUID,
    kind            TEXT NOT NULL,          -- vm.create / vm.resize / vm.power / ...
    state           TEXT NOT NULL DEFAULT 'pending',
    progress        INT  NOT NULL DEFAULT 0,
    idempotency_key TEXT NOT NULL,
    payload         JSONB NOT NULL DEFAULT '{}',  -- 不对外暴露
    result          JSONB,                        -- succeeded 后的产物（addresses 等）
    error           TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, idempotency_key)
);
CREATE INDEX IF NOT EXISTS tasks_vm_active ON tasks (vm_id) WHERE state IN ('pending','running');
CREATE INDEX IF NOT EXISTS tasks_tenant_list ON tasks (tenant_id, created_at DESC);

-- 租户配额账本：Reserve/Release 使用与 store.PlaceVM 同构的守卫式 UPDATE
CREATE TABLE IF NOT EXISTS tenant_quotas (
    tenant_id     UUID PRIMARY KEY,
    max_vcpus     INT    NOT NULL,
    max_mem_mb    BIGINT NOT NULL,
    max_disk_gb   BIGINT NOT NULL,
    max_min_kbps  BIGINT NOT NULL,
    used_vcpus    INT    NOT NULL DEFAULT 0,
    used_mem_mb   BIGINT NOT NULL DEFAULT 0,
    used_disk_gb  BIGINT NOT NULL DEFAULT 0,
    used_min_kbps BIGINT NOT NULL DEFAULT 0
);

-- vms 冗余带宽账本列：GetVMMinKbps 数据源、变配 BW 差量来源
ALTER TABLE vms ADD COLUMN IF NOT EXISTS min_kbps BIGINT NOT NULL DEFAULT 0;
```

需要补齐的 store 方法（均为薄实现）：

```go
// GetVMMinKbps：service.VMSchedulerStore 端口的唯一新增方法
func (s *Store) GetVMMinKbps(ctx context.Context, vmID string) (int64, error) {
	var v int64
	err := s.db.QueryRowContext(ctx, `SELECT min_kbps FROM vms WHERE id = $1`, vmID).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return v, err
}
// 同时在 store.CreateVM 的 INSERT 中写入 min_kbps = req.MinKbps。
// 生产建议：ResizeVM 把 ReleaseVM + PlaceVM + UpdateVMSpec 并入单事务 ResizeVMReservation。
```

## 4. NATS JetStream 接线（cmd/control/main.go 追加）

```go
import (
	"context"
	"encoding/json"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/kvm-cloud/platform/internal/task"
)

nc, err := nats.Connect(envOr("NATS_URL", "nats://127.0.0.1:4222"), nats.Name("kvmcloud-control"))
js, err := nc.JetStream()
_, _ = js.AddStream(&nats.StreamConfig{
	Name:            "KVMCLOUD_TASKS",
	Subjects:        []string{"kvmcloud.tasks.>"},
	MaxAge:          24 * time.Hour,    // 任务消息保留 24h
	DuplicateWindow: 2 * time.Minute,   // Nats-Msg-Id 去重窗口
})

bus := &natsJetBus{js: js}
vmSvc := service.NewVMService(service.VMServiceDeps{
	St: st, Writer: vmWriterImpl, Tasks: taskStoreImpl, Quota: quotaImpl,
	Flavors: flavorImpl, Publisher: task.NewPublisher(bus), Log: log,
})
lifecycle := api.NewLifecycleAPI(vmSvc, apiKeyStore, log)
lifecycle.Register(r.Group("/api/v1"))
// 注意：删除 api.go 中 api.POST("/vms", s.placeVM)，同步放置逻辑由 service 层
// 调用 store.PlaceVM 接管；placeVM 端点如需保留，迁移到 /api/v1/admin/vms 管理面。

// natsJetBus：task.Bus 的生产实现（测试用 BusFunc 打桩）
type natsJetBus struct{ js nats.JetStreamContext }

func (b *natsJetBus) Publish(ctx context.Context, env *task.Envelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	msg := nats.NewMsg(task.Subject(env.Kind)) // kvmcloud.tasks.<kind>
	msg.Data = data
	msg.Header.Set("Nats-Msg-Id", env.IdempotencyKey) // JetStream 级去重
	fut, err := b.js.PublishMsgAsync(msg)
	if err != nil {
		return err
	}
	select {
	case <-fut.Ok():
		return nil
	case err := <-fut.Err():
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
```

Worker（`cmd/worker`，另一进程）以 Durable Queue 订阅：

```go
js.QueueSubscribe("kvmcloud.tasks.>", "kvmcloud-workers", func(m *nats.Msg) {
	env := &task.Envelope{}
	_ = json.Unmarshal(m.Data, env)
	// 1) 幂等兜底：tasks.state != pending 直接 Ack 丢弃
	// 2) 按 env.Kind 路由：gRPC mTLS 调 Agent（vm.create → 建域/QoS/注IP）
	// 3) 阶段化回写 progress（10 投递/30 域/60 QoS/90 地址/100 完成）
	// 4) 失败写 tasks.error 并 NAK 延迟重投，超过 5 次转 failed + 告警
	m.Ack()
}, nats.ManualAck(), nats.Durable("KVMCLOUD_WORKERS"))
```

## 5. 调用示例

以下示例以生产域名为例；本地联调把 `https://api.kvmcloud.example.com` 换成 `http://localhost:8080`。
凭据只从环境变量或密钥服务读取，不写入代码与示例。

### 5.1 创建云主机（自定义规格 + 独享保障带宽）

```bash
export KVM_API_KEY="$(pass show kvmcloud/tenant-api-key)"   # 或任意密钥服务

curl -sS -D /tmp/h.txt -X POST "https://api.kvmcloud.example.com/api/v1/vms" \
  -H "Authorization: Bearer ${KVM_API_KEY}" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: create-web-01-20261028" \
  -d '{
    "name": "web-01",
    "image_id": "img-debian12-x64",
    "vcpus": 4,
    "memory_mb": 8192,
    "disk_gb": 80,
    "network_id": "net-7f3a",
    "bandwidth_policy": {
      "min_kbps": 51200,
      "max_kbps": 1024000,
      "priority": 100
    },
    "enable_ipv6": true,
    "disk_iops_limit": 20000,
    "ssh_keys": ["ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI... me@laptop"]
  }' | jq
```

响应（同步完成校验与配额/四维预留后）：

```
HTTP/1.1 202 Accepted
Location: /api/v1/tasks/9b2f4d5e-8a1c-4c3e-b7a9-2e6f1d0c8b45
```

```json
{
  "task_id": "9b2f4d5e-8a1c-4c3e-b7a9-2e6f1d0c8b45",
  "kind": "vm.create",
  "state": "pending",
  "vm_id": "5f0e1d2c-3b4a-4968-8776-665544332211",
  "created_at": "2026-10-28T08:30:00Z"
}
```

轮询任务（建议 2s 起步、指数退避到 10s）：

```bash
curl -sS -H "Authorization: Bearer ${KVM_API_KEY}" \
  "https://api.kvmcloud.example.com/api/v1/tasks/9b2f4d5e-8a1c-4c3e-b7a9-2e6f1d0c8b45" | jq
```

```json
{
  "task_id": "9b2f4d5e-8a1c-4c3e-b7a9-2e6f1d0c8b45",
  "kind": "vm.create",
  "state": "succeeded",
  "progress": 100,
  "vm_id": "5f0e1d2c-3b4a-4968-8776-665544332211",
  "result": {
    "addresses": [
      { "ip_address": "203.0.113.10", "ip_version": "ipv4", "type": "primary" },
      { "ip_address": "2001:db8::10",  "ip_version": "ipv6", "type": "primary" }
    ]
  },
  "created_at": "2026-10-28T08:30:00Z",
  "updated_at": "2026-10-28T08:30:51Z"
}
```

### 5.2 变更配置 / 扩容（含实时调整带宽策略）

```bash
curl -sS -D /tmp/h.txt -X POST \
  "https://api.kvmcloud.example.com/api/v1/vms/5f0e1d2c-3b4a-4968-8776-665544332211/resize" \
  -H "Authorization: Bearer ${KVM_API_KEY}" \
  -H "Content-Type: application/json" \
  -d '{
    "new_vcpus": 8,
    "new_memory_mb": 16384,
    "new_disk_gb": 160,
    "bandwidth_policy": {
      "min_kbps": 102400,
      "max_kbps": 2048000,
      "priority": 100
    }
  }' | jq
```

响应：

```
HTTP/1.1 202 Accepted
Location: /api/v1/tasks/1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d
```

```json
{
  "task_id": "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d",
  "kind": "vm.resize",
  "state": "pending",
  "vm_id": "5f0e1d2c-3b4a-4968-8776-665544332211",
  "created_at": "2026-10-28T09:12:40Z"
}
```

轮询成功后 `result` 带 `hot_plug` 与最终规格（示例见 OpenAPI `/tasks/{task_id}` 段）。

### 5.3 典型错误

```bash
# min_kbps > max_kbps → 400
curl -sS -X POST ".../api/v1/vms" -H "Authorization: Bearer ${KVM_API_KEY}" \
  -H "Content-Type: application/json" \
  -d '{"name":"bad","image_id":"img-debian12-x64","vcpus":1,"memory_mb":512,"disk_gb":10,
       "network_id":"net-7f3a",
       "bandwidth_policy":{"min_kbps":102400,"max_kbps":51200}}' | jq
```

```json
{ "code": "invalid_bandwidth_policy", "message": "min_kbps(102400) 不得超过 max_kbps(51200)" }
```

```json
// 磁盘缩容 → 422
{ "code": "disk_shrink_not_allowed", "message": "new_disk_gb(40) 小于当前 disk_gb(80)，仅允许扩容" }
// 四维超开不足 → 409
{ "code": "insufficient_capacity", "message": "变配四维超开校验：overcommit exceeded: dimension mem alloc=61440 need=16384 limit=73728" }
```

## 6. 交付验证

- `go build ./...`、`go vet ./internal/...`、`gofmt -l` 全部通过（internal/task、internal/service、internal/api/vm_lifecycle.go）。
- OpenAPI 规范可用 `swagger validate docs/api/kvmcloud-openapi.yaml` 或导入 Swagger UI / Postman 校验。
