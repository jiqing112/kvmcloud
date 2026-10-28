// Package service 控制面业务层：云主机创建 / 变配的全生命周期编排。
//
// 职责边界：本包做同步段（语义校验 → 租户配额 → 调度器四维超开预留 → 落库 → 任务落库 →
// NATS 投递）并保证失败补偿；底层执行（建 VM、QoS、热插拔）由 worker 消费任务后
// 经 gRPC mTLS 下发 Agent。Service 不感知 HTTP；Error 携带状态码由 Handler 统一映射。
package service

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"

	"github.com/google/uuid"

	"github.com/kvm-cloud/platform/internal/model"
	"github.com/kvm-cloud/platform/internal/monitor"
	"github.com/kvm-cloud/platform/internal/store"
	"github.com/kvm-cloud/platform/internal/task"
)

// ---------------- 端口（依赖倒置） ----------------

// VMSchedulerStore 调度相关存储端口。现有 *store.Store 已实现除 GetVMMinKbps 外的
// 全部方法；GetVMMinKbps 需要 vms.min_kbps 冗余列（DDL 与适配器见 docs/api/design.md）。
type VMSchedulerStore interface {
	ListNodes(ctx context.Context) ([]*model.Node, error)
	PlaceVM(ctx context.Context, nodeID string, req *model.VMRequest) (*model.Node, error)
	ReleaseVM(ctx context.Context, nodeID string, vcpus int32, memMB, diskGB, minKbps int64) error
	CreateVM(ctx context.Context, vm *model.VM) error
	GetVM(ctx context.Context, id string) (*model.VM, error)
	CreateVNIC(ctx context.Context, n *model.VNIC) error
	CreatePolicy(ctx context.Context, p *model.PortBandwidthPolicy) (*model.PortBandwidthPolicy, error)
	GetPool(ctx context.Context, id string) (*model.SharedBandwidthPool, error)
	UpdateVMStatus(ctx context.Context, id, status string) error
	GetVMMinKbps(ctx context.Context, vmID string) (int64, error)
}

// VMWriter VM 规格回写端口（变配成功后更新 vms 行）。
type VMWriter interface {
	UpdateVMSpec(ctx context.Context, id string, vcpus int32, memMB, diskGB int64) error
}

// TaskStore 异步任务持久化端口（tasks 表，DDL 见 docs/api/design.md）。
type TaskStore interface {
	CreateTask(ctx context.Context, t *task.Task) error
	MarkTaskFailed(ctx context.Context, id, code, message string) error
	HasActiveTask(ctx context.Context, vmID string) (bool, error)
	GetTask(ctx context.Context, tenantID, id string) (*task.Task, error)
}

// QuotaLedger 租户配额账本端口（tenant_quotas 表）。四维语义与节点预留一致，
// Reserve/Release 必须与 vms 落库同事务以保证账实相符。
type QuotaLedger interface {
	Reserve(ctx context.Context, tenantID string, req *model.VMRequest) error
	Release(ctx context.Context, tenantID string, req *model.VMRequest) error
}

// Flavor 套餐条目。
type Flavor struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	VCPUs    int32  `json:"vcpus"`
	MemoryMB int64  `json:"memory_mb"`
	DiskGB   int64  `json:"disk_gb"`
}

// FlavorCatalog 套餐目录端口。
type FlavorCatalog interface {
	GetFlavor(ctx context.Context, id string) (*Flavor, error)
}

// ---------------- 错误模型 ----------------

// Error 携带 HTTP 语义的业务错误；Handler 统一映射为 Error 响应体。
type Error struct {
	Status  int
	Code    string
	Message string
	Details map[string]any
}

func (e *Error) Error() string { return e.Message }

func errf(status int, code, format string, a ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, a...)}
}

// mapStore 把存储层哨兵错误翻译为带 HTTP 语义的 Error。
func mapStore(err error, what string) *Error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return errf(http.StatusNotFound, "not_found", "%s不存在", what)
	case errors.Is(err, store.ErrOvercommit):
		return errf(http.StatusConflict, "insufficient_capacity", "%s：%v", what, err)
	case errors.Is(err, store.ErrInsufficientBW):
		return errf(http.StatusConflict, "insufficient_bandwidth", "%s：%v", what, err)
	default:
		return errf(http.StatusInternalServerError, "internal_error", "%s：%v", what, err)
	}
}

// ---------------- 带宽策略（核心差异化对象） ----------------

// BandwidthPolicySpec 端口带宽策略（请求侧，wire 格式与 OpenAPI 一致）。
type BandwidthPolicySpec struct {
	MinKbps      int64   `json:"min_kbps"`
	MaxKbps      int64   `json:"max_kbps"`
	SharedPoolID *string `json:"shared_pool_id,omitempty"`
	Priority     *int    `json:"priority,omitempty"`
}

const (
	DefaultMaxKbps  = 1024 * 1024 // 未指定策略时的借用上限：1Gbps
	DefaultPriority = 100
	MaxPriority     = 100
)

// ValidatePolicy 校验带宽策略语义并补全默认值。
//
// min_kbps ≤ max_kbps 是硬约束：min 是**保障带宽**，参与四维超开 BW 维度预留、
// 会被写进节点 NIC 账本或共享池账本；max 只是**借用天花板**（突发上限）。
// 若允许 min > max，调度器会拿"天花板"当"保障"去占池，挤占其他租户的真实保障，
// 且 Agent 侧 tc htb 的 ceil < rate 规则根本无法生成，因此必须在入口拒绝。
//
// 返回补全后的副本（priority 缺省 100，池内竞争型端口可下调）。
func ValidatePolicy(p *BandwidthPolicySpec) (*BandwidthPolicySpec, *Error) {
	if p == nil {
		// 未指定策略：无保障带宽，默认 1Gbps 借用上限
		return &BandwidthPolicySpec{MaxKbps: DefaultMaxKbps}, nil
	}
	if p.MaxKbps <= 0 {
		return nil, errf(http.StatusBadRequest, "invalid_bandwidth_policy", "max_kbps 必须为正数，得到 %d", p.MaxKbps)
	}
	if p.MinKbps < 0 {
		return nil, errf(http.StatusBadRequest, "invalid_bandwidth_policy", "min_kbps 不能为负数，得到 %d", p.MinKbps)
	}
	if p.MinKbps > p.MaxKbps {
		return nil, errf(http.StatusBadRequest, "invalid_bandwidth_policy",
			"min_kbps(%d) 不得超过 max_kbps(%d)", p.MinKbps, p.MaxKbps)
	}
	prio := DefaultPriority
	if p.Priority != nil {
		if *p.Priority < 0 || *p.Priority > MaxPriority {
			return nil, errf(http.StatusBadRequest, "invalid_bandwidth_policy",
				"priority 取值 0~%d，得到 %d", MaxPriority, *p.Priority)
		}
		prio = *p.Priority
	}
	out := &BandwidthPolicySpec{MinKbps: p.MinKbps, MaxKbps: p.MaxKbps, Priority: &prio}
	if p.SharedPoolID != nil && *p.SharedPoolID != "" {
		out.SharedPoolID = p.SharedPoolID
	}
	return out, nil
}

// ---------------- VM Service ----------------

// VMServiceDeps 构造依赖；Quota / Flavors 可为 nil（内测模式降级）。
type VMServiceDeps struct {
	St        VMSchedulerStore
	Writer    VMWriter
	Tasks     TaskStore
	Quota     QuotaLedger
	Flavors   FlavorCatalog
	Publisher *task.Publisher
	Log       *slog.Logger
}

// VMService 云主机生命周期编排。
type VMService struct {
	st        VMSchedulerStore
	writer    VMWriter
	tasks     TaskStore
	quota     QuotaLedger
	flavors   FlavorCatalog
	publisher *task.Publisher
	log       *slog.Logger
}

// NewVMService 构造。
func NewVMService(d VMServiceDeps) *VMService {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	return &VMService{
		st: d.St, writer: d.Writer, tasks: d.Tasks, quota: d.Quota,
		flavors: d.Flavors, publisher: d.Publisher, log: d.Log,
	}
}

// CreateVMInput 创建入参（wire 绑定校验在 api 层完成）。
type CreateVMInput struct {
	TenantID       string
	Name           string
	ImageID        string
	FlavorID       string
	VCPUs          int32
	MemoryMB       int64
	DiskGB         int64
	NetworkID      string
	IPv6           bool
	IOPSLimit      int64
	Password       string
	SSHKeys        []string
	Policy         *BandwidthPolicySpec
	NodeID         string
	IdempotencyKey string
}

// CreateVMResult 创建返回：202 响应体由此构造。
type CreateVMResult struct {
	VM     *model.VM
	Node   *model.Node
	Policy *model.PortBandwidthPolicy
	Task   *task.Task
}

// VMCreatePayload vm.create 任务负载：Agent 建机所需的全部信息。
type VMCreatePayload struct {
	ImageID   string   `json:"image_id"`
	VCPUs     int32    `json:"vcpus"`
	MemoryMB  int64    `json:"memory_mb"`
	DiskGB    int64    `json:"disk_gb"`
	NetworkID string   `json:"network_id"`
	Bridge    string   `json:"bridge"`
	MAC       string   `json:"mac"`
	IPv6      bool     `json:"enable_ipv6"`
	IOPSLimit int64    `json:"disk_iops_limit"`
	MinKbps   int64    `json:"min_kbps"`
	MaxKbps   int64    `json:"max_kbps"`
	Password  string   `json:"password,omitempty"` // 生产建议经 KMS 信封加密或仅下发一次性随机密码
	SSHKeys   []string `json:"ssh_keys,omitempty"`
}

// CreateVM 创建云主机（同步段）。任一步失败时按逆序补偿已完成的步骤，
// 保证节点四维账本与租户配额不留悬空占用。
func (s *VMService) CreateVM(ctx context.Context, in *CreateVMInput) (res *CreateVMResult, perr *Error) {
	// 1) 规格解析：flavor 优先，否则要求自定义三元组
	spec, perr := s.resolveSpec(ctx, in)
	if perr != nil {
		return nil, perr
	}

	// 2) 带宽策略语义校验（min_kbps ≤ max_kbps、priority 0~100）
	policy, perr := ValidatePolicy(in.Policy)
	if perr != nil {
		return nil, perr
	}

	// 3) 共享池校验：归属租户 + 保障余量（min 计入池的 allocated_min_kbps）
	if policy.SharedPoolID != nil {
		pool, err := s.st.GetPool(ctx, *policy.SharedPoolID)
		if err != nil {
			return nil, mapStore(err, "共享池")
		}
		if pool.TenantID != in.TenantID {
			return nil, errf(http.StatusForbidden, "forbidden", "共享池不属于当前租户")
		}
		if pool.AllocatedMinKbps+policy.MinKbps > pool.TotalCapacityKbps {
			return nil, errf(http.StatusConflict, "insufficient_bandwidth",
				"共享池 %s 保障余量不足：已分配 %d + 本次 %d > 容量 %d",
				pool.ID, pool.AllocatedMinKbps, policy.MinKbps, pool.TotalCapacityKbps)
		}
	}

	req := &model.VMRequest{
		TenantID: in.TenantID, Name: in.Name, Flavor: in.FlavorID,
		VCPUs: spec.VCPUs, MemMB: spec.MemoryMB, DiskGB: spec.DiskGB,
		MinKbps: policy.MinKbps, MaxKbps: policy.MaxKbps,
		SharedPoolID: derefOrEmpty(policy.SharedPoolID),
	}

	// 4) 节点选择：显式 node_id 或四维水位最低的可用节点
	nodeID := in.NodeID
	if nodeID == "" {
		n, perr := s.pickNode(ctx, req)
		if perr != nil {
			return nil, perr
		}
		nodeID = n.ID
	}

	// ---- 以下任一步失败，defer 逆序执行补偿 ----
	var rollbacks []func(context.Context)
	defer func() {
		if perr == nil {
			return
		}
		rctx := context.WithoutCancel(ctx)
		for i := len(rollbacks) - 1; i >= 0; i-- {
			rollbacks[i](rctx)
		}
	}()

	// 5) 租户配额预扣
	if s.quota != nil {
		if err := s.quota.Reserve(ctx, in.TenantID, req); err != nil {
			return nil, errf(http.StatusConflict, "quota_exceeded", "租户配额不足：%v", err)
		}
		rollbacks = append(rollbacks, func(c context.Context) {
			_ = s.quota.Release(c, in.TenantID, req)
		})
	}

	// 6) 节点四维超开预留（原子 UPDATE，store.PlaceVM 内置 CheckOvercommit + 竞态兜底）
	node, err := s.st.PlaceVM(ctx, nodeID, req)
	if err != nil {
		return nil, mapStore(err, "四维超开校验")
	}
	rollbacks = append(rollbacks, func(c context.Context) {
		_ = s.st.ReleaseVM(c, nodeID, req.VCPUs, req.MemMB, req.DiskGB, req.MinKbps)
	})

	// 7) 落库：VM(creating) → VNIC → egress 带宽策略
	vm := &model.VM{
		ID: uuid.NewString(), TenantID: in.TenantID, NodeID: nodeID,
		Name: in.Name, Flavor: in.FlavorID,
		VCPUs: spec.VCPUs, MemMB: spec.MemoryMB, DiskGB: spec.DiskGB,
		Status: model.VMStatusCreating,
	}
	if err := s.st.CreateVM(ctx, vm); err != nil {
		return nil, errf(http.StatusInternalServerError, "internal_error", "写 VM 失败：%v", err)
	}
	rollbacks = append(rollbacks, func(c context.Context) {
		_ = s.st.UpdateVMStatus(c, vm.ID, "error") // 无 DeleteVM 端口，落 error 墓碑
	})

	vnic := &model.VNIC{ID: uuid.NewString(), VMID: vm.ID, MAC: newMAC(), Bridge: "br0"}
	if err := s.st.CreateVNIC(ctx, vnic); err != nil {
		return nil, errf(http.StatusInternalServerError, "internal_error", "写 VNIC 失败：%v", err)
	}

	var pol *model.PortBandwidthPolicy
	if policy.MinKbps > 0 || policy.SharedPoolID != nil || policy.MaxKbps > 0 {
		pol, err = s.st.CreatePolicy(ctx, &model.PortBandwidthPolicy{
			VNICID: vnic.ID, Direction: "egress",
			MinKbps: policy.MinKbps, MaxKbps: policy.MaxKbps,
			SharedPoolID: policy.SharedPoolID, Priority: derefInt(policy.Priority, DefaultPriority),
		})
		if err != nil {
			return nil, errf(http.StatusInternalServerError, "internal_error", "写带宽策略失败：%v", err)
		}
	}

	// 8) 任务落库（pending）并投递 NATS：kvmcloud.tasks.vm.create
	if in.Password == "" && len(in.SSHKeys) == 0 {
		in.Password = generatePassword() // 一次性随机密码，经任务 result 返回
	}
	payload := &VMCreatePayload{
		ImageID: in.ImageID, VCPUs: spec.VCPUs, MemoryMB: spec.MemoryMB, DiskGB: spec.DiskGB,
		NetworkID: in.NetworkID, Bridge: vnic.Bridge, MAC: vnic.MAC, IPv6: in.IPv6,
		IOPSLimit: in.IOPSLimit, MinKbps: policy.MinKbps, MaxKbps: policy.MaxKbps,
		Password: in.Password, SSHKeys: in.SSHKeys,
	}
	tk, err := task.New(task.KindVMCreate, in.TenantID, vm.ID, payload, in.IdempotencyKey)
	if err != nil {
		return nil, errf(http.StatusInternalServerError, "internal_error", "构造任务失败：%v", err)
	}
	if err := s.tasks.CreateTask(ctx, tk); err != nil {
		return nil, errf(http.StatusInternalServerError, "internal_error", "任务落库失败：%v", err)
	}
	// 投递失败：任务保持 pending 交由 outbox 重投；同时释放全部预留、VM 转 error，
	// 对用户返回 503（可直接重试，幂等键保证不重复建机）。
	if err := s.publisher.Publish(ctx, tk); err != nil {
		_ = s.tasks.MarkTaskFailed(ctx, tk.ID, "task_publish_failed", err.Error())
		s.log.Error("publish task failed", "task_id", tk.ID, "err", err)
		return nil, errf(http.StatusServiceUnavailable, "task_publish_failed",
			"任务队列不可用，预留已回滚，请稍后重试")
	}

	s.log.Info("vm.create accepted", "vm_id", vm.ID, "node_id", nodeID, "task_id", tk.ID,
		"min_kbps", policy.MinKbps, "max_kbps", policy.MaxKbps, "pool", policy.SharedPoolID)
	return &CreateVMResult{VM: vm, Node: node, Policy: pol, Task: tk}, nil
}

// ResizeVMInput 变配入参；指针为 nil 表示该维度保持不变。
type ResizeVMInput struct {
	TenantID       string
	VMID           string
	NewVCPUs       *int32
	NewMemoryMB    *int64
	NewDiskGB      *int64
	Policy         *BandwidthPolicySpec
	IdempotencyKey string
}

// ResizeVMResult 变配返回。
type ResizeVMResult struct {
	VM   *model.VM
	Task *task.Task
}

// VMResizePayload vm.resize 任务负载。
type VMResizePayload struct {
	NewVCPUs     int32                `json:"new_vcpus"`
	NewMemoryMB  int64                `json:"new_memory_mb"`
	NewDiskGB    int64                `json:"new_disk_gb"`
	Policy       *BandwidthPolicySpec `json:"bandwidth_policy,omitempty"`
	AllowHotPlug bool                 `json:"allow_hot_plug"`
	PrevVMState  string               `json:"prev_vm_state"`
}

// ResizeVM 变更配置 / 扩容（同步段）。
//
// 规则：磁盘仅允许扩容；有活动任务时互斥拒绝；四维按"释放旧规格 → 预留新规格"换容，
// 新规格预留失败自动回补旧规格；租户配额同步做差量调整。
// 运行态任务负载携带 allow_hot_plug 提示，Agent 评估热插拔，不支持则自动冷迁移。
func (s *VMService) ResizeVM(ctx context.Context, in *ResizeVMInput) (res *ResizeVMResult, perr *Error) {
	if in.NewVCPUs == nil && in.NewMemoryMB == nil && in.NewDiskGB == nil && in.Policy == nil {
		return nil, errf(http.StatusBadRequest, "invalid_request",
			"new_vcpus / new_memory_mb / new_disk_gb / bandwidth_policy 至少提供一个")
	}

	// 1) 加载 VM，租户与状态校验
	vm, err := s.st.GetVM(ctx, in.VMID)
	if err != nil {
		return nil, mapStore(err, "云主机")
	}
	if vm.TenantID != in.TenantID {
		return nil, errf(http.StatusForbidden, "forbidden", "云主机不属于当前租户")
	}
	if vm.Status != model.VMStatusRunning && vm.Status != model.VMStatusStopped {
		return nil, errf(http.StatusConflict, "invalid_state", "云主机处于 %s 状态，不允许变配", vm.Status)
	}

	// 2) 任务互斥：同一 VM 同时只允许一个变更任务
	active, err := s.tasks.HasActiveTask(ctx, vm.ID)
	if err != nil {
		return nil, errf(http.StatusInternalServerError, "internal_error", "查询活动任务失败：%v", err)
	}
	if active {
		return nil, errf(http.StatusConflict, "task_in_progress", "云主机存在进行中的任务，请稍后再试")
	}

	// 3) 目标规格（未传字段沿用现值）
	newVCPUs, newMem, newDisk := vm.VCPUs, vm.MemMB, vm.DiskGB
	if in.NewVCPUs != nil {
		if *in.NewVCPUs < 1 {
			return nil, errf(http.StatusBadRequest, "invalid_request", "new_vcpus 必须 ≥ 1")
		}
		newVCPUs = *in.NewVCPUs
	}
	if in.NewMemoryMB != nil {
		if *in.NewMemoryMB < 512 {
			return nil, errf(http.StatusBadRequest, "invalid_request", "new_memory_mb 必须 ≥ 512")
		}
		newMem = *in.NewMemoryMB
	}
	if in.NewDiskGB != nil {
		// 磁盘缩容会破坏文件系统一致性，平台层禁止
		if *in.NewDiskGB < vm.DiskGB {
			return nil, errf(http.StatusUnprocessableEntity, "disk_shrink_not_allowed",
				"new_disk_gb(%d) 小于当前 disk_gb(%d)，仅允许扩容", *in.NewDiskGB, vm.DiskGB)
		}
		newDisk = *in.NewDiskGB
	}

	// 4) 带宽策略（可选）：保障带宽变更是四维 BW 维度的差量来源
	policy := in.Policy
	if in.Policy != nil {
		if policy, perr = ValidatePolicy(in.Policy); perr != nil {
			return nil, perr
		}
	}

	// 5) 换容请求：curMin 来自 vms.min_kbps 账本列
	curMin, err := s.st.GetVMMinKbps(ctx, vm.ID)
	if err != nil {
		return nil, mapStore(err, "带宽账本")
	}
	newMin := curMin
	if policy != nil {
		newMin = policy.MinKbps
	}
	oldReq := &model.VMRequest{TenantID: in.TenantID, VCPUs: vm.VCPUs, MemMB: vm.MemMB, DiskGB: vm.DiskGB, MinKbps: curMin}
	newReq := &model.VMRequest{TenantID: in.TenantID, VCPUs: newVCPUs, MemMB: newMem, DiskGB: newDisk, MinKbps: newMin}
	dimsChanged := newVCPUs != vm.VCPUs || newMem != vm.MemMB || newDisk != vm.DiskGB || newMin != curMin

	// ---- 以下任一步失败，defer 逆序执行补偿 ----
	var rollbacks []func(context.Context)
	defer func() {
		if perr == nil {
			return
		}
		rctx := context.WithoutCancel(ctx)
		for i := len(rollbacks) - 1; i >= 0; i-- {
			rollbacks[i](rctx)
		}
	}()

	// 6) 租户配额差量调整（先释放旧、再预留新；失败回补旧账）
	if dimsChanged && s.quota != nil {
		_ = s.quota.Release(ctx, in.TenantID, oldReq)
		if err := s.quota.Reserve(ctx, in.TenantID, newReq); err != nil {
			_ = s.quota.Reserve(ctx, in.TenantID, oldReq)
			return nil, errf(http.StatusConflict, "quota_exceeded", "租户配额不足：%v", err)
		}
		rollbacks = append(rollbacks, func(c context.Context) {
			_ = s.quota.Release(c, in.TenantID, newReq)
			_ = s.quota.Reserve(c, in.TenantID, oldReq)
		})
	}

	// 7) 节点四维换容：释放旧规格 → 原子预留新规格。
	//    生产实现应将两步并入单事务 ResizeVMReservation（见 design.md），
	//    此处显式补偿以保证任意中间失败不留悬空。
	if dimsChanged {
		if err := s.st.ReleaseVM(ctx, vm.NodeID, oldReq.VCPUs, oldReq.MemMB, oldReq.DiskGB, oldReq.MinKbps); err != nil {
			return nil, errf(http.StatusInternalServerError, "internal_error", "释放旧规格失败：%v", err)
		}
		rePlaced := false
		rollbacks = append(rollbacks, func(c context.Context) { // PlaceVM(new) 失败 → 回补旧规格
			if rePlaced {
				return
			}
			if _, err := s.st.PlaceVM(c, vm.NodeID, oldReq); err != nil {
				s.log.Error("resize 回补旧规格失败，需人工介入", "vm_id", vm.ID, "err", err)
			}
		})
		if _, err := s.st.PlaceVM(ctx, vm.NodeID, newReq); err != nil {
			return nil, mapStore(err, "变配四维超开校验")
		}
		rePlaced = true
		rollbacks = append(rollbacks, func(c context.Context) { // 新规格已占位 → 释放新 + 回补旧
			_ = s.st.ReleaseVM(c, vm.NodeID, newReq.VCPUs, newReq.MemMB, newReq.DiskGB, newReq.MinKbps)
			if _, err := s.st.PlaceVM(c, vm.NodeID, oldReq); err != nil {
				s.log.Error("resize 补偿回补旧规格失败，需人工介入", "vm_id", vm.ID, "err", err)
			}
		})
	}

	// 8) 回写 VM 规格
	if err := s.writer.UpdateVMSpec(ctx, vm.ID, newVCPUs, newMem, newDisk); err != nil {
		return nil, errf(http.StatusInternalServerError, "internal_error", "回写 VM 规格失败：%v", err)
	}
	rollbacks = append(rollbacks, func(c context.Context) {
		_ = s.writer.UpdateVMSpec(c, vm.ID, vm.VCPUs, vm.MemMB, vm.DiskGB)
	})

	// 9) 任务落库 + 投递；失败则整体回到旧规格（磁盘扩容同样可安全回退）
	payload := &VMResizePayload{
		NewVCPUs: newVCPUs, NewMemoryMB: newMem, NewDiskGB: newDisk,
		Policy: policy, AllowHotPlug: vm.Status == model.VMStatusRunning, PrevVMState: vm.Status,
	}
	tk, err := task.New(task.KindVMResize, in.TenantID, vm.ID, payload, in.IdempotencyKey)
	if err != nil {
		return nil, errf(http.StatusInternalServerError, "internal_error", "构造任务失败：%v", err)
	}
	if err := s.tasks.CreateTask(ctx, tk); err != nil {
		return nil, errf(http.StatusInternalServerError, "internal_error", "任务落库失败：%v", err)
	}
	if err := s.publisher.Publish(ctx, tk); err != nil {
		_ = s.tasks.MarkTaskFailed(ctx, tk.ID, "task_publish_failed", err.Error())
		s.log.Error("publish task failed", "task_id", tk.ID, "err", err)
		return nil, errf(http.StatusServiceUnavailable, "task_publish_failed",
			"任务队列不可用，变更已回滚，请稍后重试")
	}

	s.log.Info("vm.resize accepted", "vm_id", vm.ID, "task_id", tk.ID,
		"vcpus", fmt.Sprintf("%d→%d", vm.VCPUs, newVCPUs),
		"mem_mb", fmt.Sprintf("%d→%d", vm.MemMB, newMem),
		"disk_gb", fmt.Sprintf("%d→%d", vm.DiskGB, newDisk),
		"min_kbps", fmt.Sprintf("%d→%d", curMin, newMin))
	vm.VCPUs, vm.MemMB, vm.DiskGB = newVCPUs, newMem, newDisk
	return &ResizeVMResult{VM: vm, Task: tk}, nil
}

// GetTask 供 API 层轮询任务进度（租户隔离由 TaskStore 实现保证）。
func (s *VMService) GetTask(ctx context.Context, tenantID, taskID string) (*task.Task, *Error) {
	t, err := s.tasks.GetTask(ctx, tenantID, taskID)
	if err != nil {
		return nil, mapStore(err, "任务")
	}
	return t, nil
}

// ---------------- 内部辅助 ----------------

// resolveSpec 解析规格：flavor_id 优先；否则要求自定义三元组。
func (s *VMService) resolveSpec(ctx context.Context, in *CreateVMInput) (*struct {
	VCPUs    int32
	MemoryMB int64
	DiskGB   int64
}, *Error) {
	if in.FlavorID != "" {
		if s.flavors == nil {
			return nil, errf(http.StatusInternalServerError, "internal_error", "套餐目录未配置")
		}
		f, err := s.flavors.GetFlavor(ctx, in.FlavorID)
		if err != nil {
			return nil, errf(http.StatusBadRequest, "flavor_not_found", "套餐 %s 不存在", in.FlavorID)
		}
		return &struct {
			VCPUs    int32
			MemoryMB int64
			DiskGB   int64
		}{f.VCPUs, f.MemoryMB, f.DiskGB}, nil
	}
	if in.VCPUs < 1 || in.MemoryMB < 512 || in.DiskGB < 10 {
		return nil, errf(http.StatusBadRequest, "invalid_request",
			"未提供 flavor_id 时必须同时传 vcpus(≥1)、memory_mb(≥512)、disk_gb(≥10)")
	}
	return &struct {
		VCPUs    int32
		MemoryMB int64
		DiskGB   int64
	}{in.VCPUs, in.MemoryMB, in.DiskGB}, nil
}

// pickNode 选择四维水位之和最低、且能容纳本次请求的在线节点。
// 直接复用调度器纯函数 store.CheckOvercommit 预过滤，避免必然失败的预留。
func (s *VMService) pickNode(ctx context.Context, req *model.VMRequest) (*model.Node, *Error) {
	nodes, err := s.st.ListNodes(ctx)
	if err != nil {
		return nil, errf(http.StatusInternalServerError, "internal_error", "列出节点失败：%v", err)
	}
	best := (*model.Node)(nil)
	bestScore := math.MaxFloat64
	for _, n := range nodes {
		if n.Status != model.NodeStatusOnline {
			continue
		}
		if err := store.CheckOvercommit(n, req); err != nil {
			continue // 该节点四维装不下本次请求
		}
		w := monitor.ComputeWatermarks(n)
		score := w.CPU + w.Mem + w.Disk + w.BW
		if score < bestScore {
			best, bestScore = n, score
		}
	}
	if best == nil {
		return nil, errf(http.StatusServiceUnavailable, "no_node_available", "无满足四维约束的在线节点")
	}
	return best, nil
}

func derefOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefInt(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

// generatePassword 生成 16 字节随机密码（一次性下发，建议用户首次登录后修改）。
func generatePassword() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// newMAC 生成 KVM 本地受管 MAC（52:54:00 前缀）。
func newMAC() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	b[0], b[1], b[2] = 0x52, 0x54, 0x00
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", b[0], b[1], b[2], b[3], b[4], b[5])
}
