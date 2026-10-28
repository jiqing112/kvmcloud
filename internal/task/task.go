// Package task 定义控制面异步任务的任务模型与 NATS 投递抽象。
//
// 生命周期：API 同步完成校验/预留后落库 Task(pending) 并向 NATS 投递 Envelope，
// worker（kvmcloud-worker）消费 kvmcloud.tasks.* 主题，经 gRPC mTLS 下发 Agent，
// 阶段化回写 progress；JetStream 依据 Nats-Msg-Id 去重，consumer 侧再按
// (tenant_id, idempotency_key) 唯一约束兜底幂等。
//
// 本包对 NATS 只依赖最小接口 Bus（见 publisher.go），不直接引入 nats.go，
// 便于单测替身；生产接线（JetStream + Nats-Msg-Id）见 docs/api/design.md。
package task

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Kind 任务类型，同时构成 NATS 主题后缀。
type Kind string

const (
	KindVMCreate        Kind = "vm.create"
	KindVMResize        Kind = "vm.resize"
	KindVMPower         Kind = "vm.power" // payload.action = start | stop | reboot
	KindVMResetPassword Kind = "vm.reset_password"
	KindVMReinstall     Kind = "vm.reinstall"
	KindIPAttach        Kind = "ip.attach"
	KindIPDetach        Kind = "ip.detach"
	KindTrafficAttach   Kind = "traffic.attach"
	KindQoSUpdate       Kind = "qos.update"
)

// State 任务状态机：pending → running → succeeded | failed。
type State string

const (
	StatePending   State = "pending"
	StateRunning   State = "running"
	StateSucceeded State = "succeeded"
	StateFailed    State = "failed"
)

// Active 判断任务是否仍占用 VM（变配/IP 等互斥操作检查此条件）。
func (s State) Active() bool { return s == StatePending || s == StateRunning }

// SubjectPrefix NATS 主题前缀，对应 JetStream Stream "KVMCLOUD_TASKS"。
const SubjectPrefix = "kvmcloud.tasks"

// Subject 返回某任务类型的投递主题，如 kvmcloud.tasks.vm.create。
func Subject(k Kind) string { return SubjectPrefix + "." + string(k) }

// Task 一条异步任务的持久化视图（tasks 表行）。
type Task struct {
	ID             string          `json:"task_id"`
	TenantID       string          `json:"tenant_id"`
	VMID           string          `json:"vm_id,omitempty"`
	Kind           Kind            `json:"kind"`
	State          State           `json:"state"`
	Progress       int             `json:"progress"`
	IdempotencyKey string          `json:"idempotency_key"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	Result         json.RawMessage `json:"result,omitempty"` // succeeded 后的任务产物（如 addresses）
	Error          string          `json:"error,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

// New 构造一条 pending 任务；idempotencyKey 为空时自动生成。
func New(kind Kind, tenantID, vmID string, payload any, idempotencyKey string) (*Task, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("task: tenant_id required")
	}
	if idempotencyKey == "" {
		idempotencyKey = uuid.NewString()
	}
	var raw json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("task: marshal payload: %w", err)
		}
		raw = b
	}
	now := time.Now().UTC()
	return &Task{
		ID:             uuid.NewString(),
		TenantID:       tenantID,
		VMID:           vmID,
		Kind:           kind,
		State:          StatePending,
		IdempotencyKey: idempotencyKey,
		Payload:        raw,
		CreatedAt:      now,
		UpdatedAt:      now,
	}, nil
}

// Envelope NATS 消息信封；worker 收到后凭 TaskID 回写进度。
type Envelope struct {
	TaskID         string          `json:"task_id"`
	Kind           Kind            `json:"kind"`
	TenantID       string          `json:"tenant_id"`
	VMID           string          `json:"vm_id,omitempty"`
	IdempotencyKey string          `json:"idempotency_key"`
	Payload        json.RawMessage `json:"payload"`
	IssuedAt       time.Time       `json:"issued_at"`
}

// Envelope 生成投递信封。JetStream 侧应设置 Nats-Msg-Id = IdempotencyKey 去重。
func (t *Task) Envelope() *Envelope {
	return &Envelope{
		TaskID:         t.ID,
		Kind:           t.Kind,
		TenantID:       t.TenantID,
		VMID:           t.VMID,
		IdempotencyKey: t.IdempotencyKey,
		Payload:        t.Payload,
		IssuedAt:       time.Now().UTC(),
	}
}
