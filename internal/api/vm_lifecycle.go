package api

// 租户面云主机生命周期 API：Bearer 鉴权 + 202 异步语义。
// 与 api.go 中内部/管理面端点的区别：本文件路由要求租户 API Key，
// 资源变更统一返回 202 + task_id（业务编排见 internal/service）。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/kvm-cloud/platform/internal/service"
	"github.com/kvm-cloud/platform/internal/task"
)

// IdentityStore 租户 API Key 认证端口。api_keys 表仅存 SHA-256 摘要
// （DDL 见 docs/api/design.md），实现侧按摘要主键等值查询（天然常量时间）。
type IdentityStore interface {
	ResolveAPIKey(ctx context.Context, keyHash string) (tenantID string, err error)
}

// LifecycleAPI 租户面生命周期端点集合。
type LifecycleAPI struct {
	vm   *service.VMService
	keys IdentityStore
	log  *slog.Logger
}

// NewLifecycleAPI 构造。
func NewLifecycleAPI(vm *service.VMService, keys IdentityStore, log *slog.Logger) *LifecycleAPI {
	return &LifecycleAPI{vm: vm, keys: keys, log: log}
}

// Register 挂载生命周期路由到 rg（通常为 /api/v1）。
//
// 注意：租户面 POST /vms 由本方法接管（202 异步）；api.go 中的同步放置端点
// api.POST("/vms", s.placeVM) 迁移为管理面端点（/api/v1/admin/vms），见 design.md。
// gin 同一路由树参数名必须一致，故此处沿用 :id。
func (l *LifecycleAPI) Register(rg *gin.RouterGroup) {
	auth := l.tenantAuth()
	vms := rg.Group("/vms", auth)
	{
		vms.POST("", l.createVM)
		vms.POST("/:id/resize", l.resizeVM)
	}
	rg.GET("/tasks/:id", auth, l.getTask)
}

// tenantAuth 校验 Authorization: Bearer <Tenant_API_Key> 并注入租户身份。
// 服务端只保存 SHA-256 摘要；明文 Key 仅在创建时展示一次，不落日志。
func (l *LifecycleAPI) tenantAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		const scheme = "Bearer "
		h := c.GetHeader("Authorization")
		if len(h) <= len(scheme) || h[:len(scheme)] != scheme {
			writeError(c, &service.Error{Status: http.StatusUnauthorized, Code: "unauthorized",
				Message: "missing or malformed bearer token"})
			return
		}
		sum := sha256.Sum256([]byte(h[len(scheme):]))
		tenantID, err := l.keys.ResolveAPIKey(c.Request.Context(), hex.EncodeToString(sum[:]))
		if err != nil || tenantID == "" {
			writeError(c, &service.Error{Status: http.StatusUnauthorized, Code: "unauthorized",
				Message: "invalid api key"})
			return
		}
		c.Set(ctxKeyTenant, tenantID)
		c.Next()
	}
}

const ctxKeyTenant = "kvmcloud.tenant_id"

func tenantIDOf(c *gin.Context) string {
	v, _ := c.Get(ctxKeyTenant)
	s, _ := v.(string)
	return s
}

// idempotencyKeyOf 读取幂等键（可选请求头）。
func idempotencyKeyOf(c *gin.Context) string {
	return c.GetHeader("Idempotency-Key")
}

// ---------------- 请求体（与 OpenAPI schemas 对齐） ----------------

// CreateVMRequest POST /api/v1/vms 请求体。
type CreateVMRequest struct {
	Name            string                       `json:"name" binding:"required,max=64"`
	ImageID         string                       `json:"image_id" binding:"required"`
	FlavorID        string                       `json:"flavor_id"`
	VCPUs           *int32                       `json:"vcpus"`
	MemoryMB        *int64                       `json:"memory_mb"`
	DiskGB          *int64                       `json:"disk_gb"`
	NetworkID       string                       `json:"network_id" binding:"required"`
	BandwidthPolicy *service.BandwidthPolicySpec `json:"bandwidth_policy"`
	EnableIPv6      bool                         `json:"enable_ipv6"`
	DiskIOPSLimit   int64                        `json:"disk_iops_limit"`
	Password        string                       `json:"password"`
	SSHKeys         []string                     `json:"ssh_keys"`
	NodeID          string                       `json:"node_id"`
}

func (r *CreateVMRequest) toInput(tenantID, idemKey string) *service.CreateVMInput {
	in := &service.CreateVMInput{
		TenantID: tenantID, Name: r.Name, ImageID: r.ImageID, FlavorID: r.FlavorID,
		NetworkID: r.NetworkID, IPv6: r.EnableIPv6, IOPSLimit: r.DiskIOPSLimit,
		Password: r.Password, SSHKeys: r.SSHKeys,
		Policy: r.BandwidthPolicy, NodeID: r.NodeID, IdempotencyKey: idemKey,
	}
	if r.VCPUs != nil {
		in.VCPUs = *r.VCPUs
	}
	if r.MemoryMB != nil {
		in.MemoryMB = *r.MemoryMB
	}
	if r.DiskGB != nil {
		in.DiskGB = *r.DiskGB
	}
	return in
}

// ResizeVMRequest POST /api/v1/vms/:id/resize 请求体。
type ResizeVMRequest struct {
	NewVCPUs        *int32                       `json:"new_vcpus" binding:"omitempty,min=1"`
	NewMemoryMB     *int64                       `json:"new_memory_mb" binding:"omitempty,min=512"`
	NewDiskGB       *int64                       `json:"new_disk_gb" binding:"omitempty,min=10"`
	BandwidthPolicy *service.BandwidthPolicySpec `json:"bandwidth_policy"`
}

func (r *ResizeVMRequest) toInput(tenantID, vmID, idemKey string) *service.ResizeVMInput {
	return &service.ResizeVMInput{
		TenantID: tenantID, VMID: vmID,
		NewVCPUs: r.NewVCPUs, NewMemoryMB: r.NewMemoryMB, NewDiskGB: r.NewDiskGB,
		Policy: r.BandwidthPolicy, IdempotencyKey: idemKey,
	}
}

// ---------------- Handlers ----------------

// createVM POST /api/v1/vms：同步校验与预留后返回 202 + task_id。
func (l *LifecycleAPI) createVM(c *gin.Context) {
	var req CreateVMRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, &service.Error{Status: http.StatusBadRequest, Code: "invalid_request", Message: err.Error()})
		return
	}
	res, serr := l.vm.CreateVM(c.Request.Context(), req.toInput(tenantIDOf(c), idempotencyKeyOf(c)))
	if serr != nil {
		writeError(c, serr)
		return
	}
	taskAccepted(c, res.Task, res.VM.ID)
}

// resizeVM POST /api/v1/vms/:id/resize。
func (l *LifecycleAPI) resizeVM(c *gin.Context) {
	var req ResizeVMRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, &service.Error{Status: http.StatusBadRequest, Code: "invalid_request", Message: err.Error()})
		return
	}
	res, serr := l.vm.ResizeVM(c.Request.Context(), req.toInput(tenantIDOf(c), c.Param("id"), idempotencyKeyOf(c)))
	if serr != nil {
		writeError(c, serr)
		return
	}
	taskAccepted(c, res.Task, res.VM.ID)
}

// getTask GET /api/v1/tasks/:id：任务轮询。
// 响应剥离 payload（可能携带初始密码等敏感负载），仅暴露进度与产物。
func (l *LifecycleAPI) getTask(c *gin.Context) {
	t, serr := l.vm.GetTask(c.Request.Context(), tenantIDOf(c), c.Param("id"))
	if serr != nil {
		writeError(c, serr)
		return
	}
	body := gin.H{
		"task_id":    t.ID,
		"kind":       t.Kind,
		"state":      t.State,
		"progress":   t.Progress,
		"vm_id":      t.VMID,
		"created_at": t.CreatedAt,
		"updated_at": t.UpdatedAt,
	}
	if len(t.Result) > 0 {
		body["result"] = t.Result
	}
	if t.Error != "" {
		body["error"] = gin.H{"code": "task_failed", "message": t.Error}
	}
	c.JSON(http.StatusOK, body)
}

// ---------------- 响应辅助 ----------------

// taskAccepted 统一 202 响应：Location 指向任务轮询地址。
func taskAccepted(c *gin.Context, t *task.Task, vmID string) {
	c.Header("Location", "/api/v1/tasks/"+t.ID)
	body := gin.H{"task_id": t.ID, "kind": t.Kind, "state": t.State, "created_at": t.CreatedAt}
	if vmID != "" {
		body["vm_id"] = vmID
	}
	c.JSON(http.StatusAccepted, body)
}

// writeError 统一错误响应：service.Error 已携带 HTTP 状态与机器可读 code。
func writeError(c *gin.Context, e *service.Error) {
	body := gin.H{"code": e.Code, "message": e.Message}
	if len(e.Details) > 0 {
		body["details"] = e.Details
	}
	c.AbortWithStatusJSON(e.Status, body)
}
