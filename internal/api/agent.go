package api

// 内部 Agent API：计算节点拉取建机任务与回报结果。
//
// 链路：UI placeVM 落库 VM(creating) → Agent 周期拉取本节点 pending 任务 →
// 本地 qemu-img/virsh 建机 → 回报结果 → 控制面状态机 creating→running|error。
// 成功失败都回报；失败置 error 并释放节点四维预留（不留悬空容量）。
//
// 鉴权现状：与既有 /api/v1 管理面一致（内网信任域，Agent 只能拉取/回报
// 本 node_id 的任务）；节点级凭据随 auth 演进补齐。
// 演进方向：并入 internal/task 任务层（NATS），Agent 拉取仅作兜底通道。

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/kvm-cloud/platform/internal/model"
	"github.com/kvm-cloud/platform/internal/store"
)

// pendingTask 下发给 Agent 的一次建机任务。
type pendingTask struct {
	VMID    string `json:"vm_id"`
	Name    string `json:"name"`
	VCPUs   int32  `json:"vcpus"`
	MemMB   int64  `json:"mem_mb"`
	DiskGB  int64  `json:"disk_gb"`
	MinKbps int64  `json:"min_kbps"` // egress 保障带宽；无策略为 0
	MaxKbps int64  `json:"max_kbps"` // egress 借用上限；无策略为 0
}

// agentPendingTasks GET /api/v1/agent/tasks/pending?node_id=
func (s *Server) agentPendingTasks(c *gin.Context) {
	nodeID := c.Query("node_id")
	if nodeID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "node_id is required"})
		return
	}
	ctx := c.Request.Context()
	if _, err := s.st.GetNode(ctx, nodeID); err != nil {
		errStatus(c, err)
		return
	}
	vms, err := s.st.ListPendingVMs(ctx, nodeID)
	if err != nil {
		errStatus(c, err)
		return
	}
	tasks := make([]pendingTask, 0, len(vms))
	for _, vm := range vms {
		t := pendingTask{
			VMID: vm.ID, Name: vm.Name,
			VCPUs: vm.VCPUs, MemMB: vm.MemMB, DiskGB: vm.DiskGB,
		}
		if min, max, has, perr := s.st.GetVMPolicyKbps(ctx, vm.ID); perr == nil && has {
			t.MinKbps, t.MaxKbps = min, max
		}
		tasks = append(tasks, t)
	}
	c.JSON(http.StatusOK, gin.H{"tasks": tasks})
}

// agentReport Agent 回报建机结果。
type agentReport struct {
	NodeID string `json:"node_id" binding:"required"`
	VMID   string `json:"vm_id" binding:"required"`
	Ok     bool   `json:"ok"`
	Error  string `json:"error"`
}

// agentReportTask POST /api/v1/agent/tasks/report
func (s *Server) agentReportTask(c *gin.Context) {
	var q agentReport
	if err := c.ShouldBindJSON(&q); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx := c.Request.Context()
	vm, err := s.st.GetVM(ctx, q.VMID)
	if err != nil {
		errStatus(c, err)
		return
	}
	if vm.NodeID != q.NodeID {
		c.JSON(http.StatusForbidden, gin.H{"error": "vm belongs to another node"})
		return
	}

	if q.Ok {
		if err := s.st.UpdateVMStatus(ctx, q.VMID, model.VMStatusRunning); err != nil && !errors.Is(err, store.ErrNotFound) {
			errStatus(c, err)
			return
		}
		s.log.Info("vm provisioned", "vm_id", q.VMID, "name", vm.Name, "node_id", q.NodeID)
		c.JSON(http.StatusOK, gin.H{"ok": true})
		return
	}

	// 建机失败：置 error 记录原因，并释放节点四维预留（容量不留悬空）。
	msg := q.Error
	if msg == "" {
		msg = "provision failed"
	}
	if err := s.st.SetVMError(ctx, q.VMID, msg); err != nil && !errors.Is(err, store.ErrNotFound) {
		errStatus(c, err)
		return
	}
	minK := int64(0)
	if min, _, has, perr := s.st.GetVMPolicyKbps(ctx, q.VMID); perr == nil && has {
		minK = min
	}
	if err := s.st.ReleaseVM(ctx, vm.NodeID, vm.VCPUs, vm.MemMB, vm.DiskGB, minK); err != nil {
		s.log.Error("release node capacity after provision failure", "vm_id", q.VMID, "node_id", vm.NodeID, "err", err)
	}
	s.log.Warn("vm provision failed", "vm_id", q.VMID, "name", vm.Name, "node_id", q.NodeID, "err", msg)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
