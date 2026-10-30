package coreapi

// 模块 7/8/11：在线变配（Resize）、无缝纳管（Adopt）、热迁移与回收站。
//
// 统一模式：参数校验 → scope/幂等 → 账本守卫变更（租户配额/节点四维）→ 状态机流转
// → 任务 + outbox → 202。底层动作（virsh 热插拔/迁移/清空回收站）全部由 Agent
// 在任务上下文内白名单执行；失败由任务状态机 failed 分支回退账本（见 03_security.md）。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	sq "github.com/Masterminds/squirrel"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// recycleGracePeriod 软删除后物理清理延迟（回收站宽限期）。
const recycleGracePeriod = 7 * 24 * time.Hour

// vmStateMutable 终态/回收站中的 VM 拒绝一切变更任务。
func vmStateMutable(state string) bool {
	switch state {
	case "deleted", "recycled", "recycling", "deleting":
		return false
	}
	return true
}

// ---------------- 模块 7：在线变配 -------------------------------------------

type ResizeVMRequest struct {
	VCPUs       *int64 `json:"vcpus"`
	MemoryMiB   *int64 `json:"memory_mib"`
	RootDiskGiB *int64 `json:"root_disk_gib"`
}

func (r ResizeVMRequest) valid() bool {
	if r.VCPUs == nil && r.MemoryMiB == nil && r.RootDiskGiB == nil {
		return false // 至少一维
	}
	spec := func(v *int64, lo, hi int64) bool { return v == nil || (*v >= lo && *v <= hi) }
	return spec(r.VCPUs, 1, 4096) && spec(r.MemoryMiB, 128, 16_777_216) && spec(r.RootDiskGiB, 1, 1_048_576)
}

// ResizeVM POST /api/v1/vms/{id}/resize
func (a *API) ResizeVM(c *gin.Context) {
	p, ok := principal(c, "vms:write", true)
	if !ok {
		return
	}
	vmID, err := uuid.Parse(c.Param("id"))
	if err != nil || vmID == uuid.Nil {
		fail(c, http.StatusBadRequest, "invalid_vm_id")
		return
	}
	key := c.GetHeader("Idempotency-Key")
	if !idempotencyKey.MatchString(key) {
		fail(c, http.StatusBadRequest, "idempotency_key_required")
		return
	}
	var request ResizeVMRequest
	if !strictJSON(c, &request) {
		return
	}
	if !request.valid() {
		fail(c, http.StatusUnprocessableEntity, "invalid_resize_spec")
		return
	}
	_, hash, err := digestJSON(request)
	if err != nil {
		fail(c, http.StatusBadRequest, "invalid_resize_spec")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 8*time.Second)
	defer cancel()
	tx, err := beginTenant(ctx, a.DB, p.TenantID)
	if err != nil {
		databaseError(c, err)
		return
	}
	defer tx.Rollback()
	operation := fmt.Sprintf("POST /api/v1/vms/%s/resize", vmID)
	replayed, err := a.claimIdempotency(ctx, tx, c, p, operation, key, hash)
	if err != nil {
		databaseError(c, err)
		return
	}
	if replayed {
		return
	}

	var nodeID, vmState string
	var curVCPUs, curMem, curDisk int64
	err = row(ctx, tx, query.Select("node_id", "state", "vcpus", "memory_mib", "root_disk_gib").
		From("kvmcloud_core.vms").
		Where(sq.Eq{"id": vmID.String(), "tenant_id": p.TenantID.String()}), &nodeID, &vmState,
		&curVCPUs, &curMem, &curDisk)
	if errors.Is(err, sql.ErrNoRows) {
		fail(c, http.StatusNotFound, "vm_not_found")
		return
	}
	if err != nil {
		databaseError(c, err)
		return
	}
	if !vmStateMutable(vmState) || vmState == "creating" {
		fail(c, http.StatusConflict, "vm_state_conflict")
		return
	}
	// 只允许扩量：根盘缩容需走重装/新盘流程（回退语义复杂，直接在入口拒绝）。
	dVCPUs, dMem, dDisk := int64(0), int64(0), int64(0)
	if request.VCPUs != nil {
		dVCPUs = *request.VCPUs - curVCPUs
	}
	if request.MemoryMiB != nil {
		dMem = *request.MemoryMiB - curMem
	}
	if request.RootDiskGiB != nil {
		if *request.RootDiskGiB < curDisk {
			fail(c, http.StatusUnprocessableEntity, "disk_shrink_unsupported")
			return
		}
		dDisk = *request.RootDiskGiB - curDisk
	}
	if dVCPUs > 0 || dMem > 0 || dDisk > 0 {
		// 节点四维水位守卫扣减（负增量自然放行）。
		result, err := exec(ctx, tx, query.Update("kvmcloud_core.nodes").
			Set("used_vcpus", sq.Expr("used_vcpus + ?", dVCPUs)).
			Set("used_memory_mib", sq.Expr("used_memory_mib + ?", dMem)).
			Set("used_disk_gib", sq.Expr("used_disk_gib + ?", dDisk)).
			Where(sq.Eq{"id": nodeID}).
			Where(sq.Expr("used_vcpus + ? >= 0", dVCPUs)).
			Where(sq.Expr("used_memory_mib + ? >= 0", dMem)).
			Where(sq.Expr("used_disk_gib + ? >= 0", dDisk)).
			Where(sq.Expr("used_vcpus::numeric + ? <= floor(physical_vcpus::numeric * cpu_ratio)", dVCPUs)).
			Where(sq.Expr("used_memory_mib::numeric + ? <= floor(physical_memory_mib::numeric * memory_ratio)", dMem)).
			Where(sq.Expr("used_disk_gib::numeric + ? <= floor(physical_disk_gib::numeric * disk_ratio)", dDisk)))
		if _, err = changed(result, err); err != nil {
			databaseError(c, err)
			return
		}
	}
	// 目标规格与状态落库；Agent 热插拔失败由任务 failed 分支回退本事务账本。
	newVCPUs, newMem, newDisk := curVCPUs, curMem, curDisk
	if request.VCPUs != nil {
		newVCPUs = *request.VCPUs
	}
	if request.MemoryMiB != nil {
		newMem = *request.MemoryMiB
	}
	if request.RootDiskGiB != nil {
		newDisk = *request.RootDiskGiB
	}
	taskID, eventID := uuid.New(), uuid.New()
	body, _, err := digestJSON(struct {
		EventID     uuid.UUID `json:"event_id"`
		TaskID      uuid.UUID `json:"task_id"`
		VMID        uuid.UUID `json:"vm_id"`
		VCPUs       int64     `json:"vcpus"`
		MemoryMiB   int64     `json:"memory_mib"`
		RootDiskGiB int64     `json:"root_disk_gib"`
	}{eventID, taskID, vmID, newVCPUs, newMem, newDisk})
	if err != nil {
		fail(c, http.StatusInternalServerError, "serialization_failed")
		return
	}
	writes := []sq.Sqlizer{
		query.Update("kvmcloud_core.vms").
			Set("vcpus", newVCPUs).Set("memory_mib", newMem).Set("root_disk_gib", newDisk).
			Set("state", "resizing").Set("desired_version", sq.Expr("desired_version + 1")).
			Where(sq.Eq{"id": vmID.String()}).Where(sq.Eq{"state": vmState}),
		query.Insert("kvmcloud_core.tasks").Columns("id", "tenant_id", "vm_id", "node_id", "kind").
			Values(taskID.String(), p.TenantID.String(), vmID.String(), nodeID, "vm.resize"),
		query.Insert("kvmcloud_core.outbox_events").Columns(
			"id", "tenant_id", "task_id", "node_id", "subject", "payload").
			Values(eventID.String(), p.TenantID.String(), taskID.String(), nodeID,
				"kvmcloud.commands."+nodeID, string(body)),
	}
	for _, write := range writes {
		if _, err = exec(ctx, tx, write); err != nil {
			databaseError(c, err)
			return
		}
	}
	if err = audit(ctx, tx, c, p, p.TenantID.String(), "vms.resize", "vm", vmID); err != nil {
		databaseError(c, err)
		return
	}
	response, err := a.cacheIdempotentResponse(ctx, tx, p, operation, key,
		"/api/v1/tasks/"+taskID.String(), http.StatusAccepted,
		Accepted{VMID: vmID, TaskID: taskID, State: "resizing"})
	if err != nil {
		databaseError(c, err)
		return
	}
	if err = tx.Commit(); err != nil {
		databaseError(c, err)
		return
	}
	c.Header("Location", "/api/v1/tasks/"+taskID.String())
	c.Data(http.StatusAccepted, "application/json", response)
}

// ---------------- 模块 11：热迁移 --------------------------------------------

type MigrateVMRequest struct {
	TargetNodeID *uuid.UUID `json:"target_node_id"` // 缺省 = 调度器在集群内自选
}

// MigrateVM POST /api/v1/vms/{id}/migrate
func (a *API) MigrateVM(c *gin.Context) {
	p, ok := principal(c, "vms:write", true)
	if !ok {
		return
	}
	vmID, err := uuid.Parse(c.Param("id"))
	if err != nil || vmID == uuid.Nil {
		fail(c, http.StatusBadRequest, "invalid_vm_id")
		return
	}
	key := c.GetHeader("Idempotency-Key")
	if !idempotencyKey.MatchString(key) {
		fail(c, http.StatusBadRequest, "idempotency_key_required")
		return
	}
	var request MigrateVMRequest
	if !strictJSON(c, &request) {
		return
	}
	_, hash, err := digestJSON(request)
	if err != nil {
		fail(c, http.StatusBadRequest, "invalid_migrate_request")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 8*time.Second)
	defer cancel()
	tx, err := beginTenant(ctx, a.DB, p.TenantID)
	if err != nil {
		databaseError(c, err)
		return
	}
	defer tx.Rollback()
	operation := fmt.Sprintf("POST /api/v1/vms/%s/migrate", vmID)
	replayed, err := a.claimIdempotency(ctx, tx, c, p, operation, key, hash)
	if err != nil {
		databaseError(c, err)
		return
	}
	if replayed {
		return
	}

	var sourceNode, vmState, clusterID string
	var vcpus, mem, disk, minKbps int64
	err = row(ctx, tx, query.Select("v.node_id", "v.state", "v.vcpus", "v.memory_mib",
		"v.root_disk_gib", "cur.cluster_id", "COALESCE(bp.min_kbps, 0)").
		From("kvmcloud_core.vms v").
		Join("kvmcloud_core.nodes cur ON cur.id = v.node_id").
		LeftJoin("kvmcloud_core.vm_nics n ON n.vm_id = v.id AND n.primary_nic").
		LeftJoin("kvmcloud_core.bandwidth_policies bp ON bp.nic_id = n.id").
		Where(sq.Eq{"v.id": vmID.String(), "v.tenant_id": p.TenantID.String()}),
		&sourceNode, &vmState, &vcpus, &mem, &disk, &clusterID, &minKbps)
	if errors.Is(err, sql.ErrNoRows) {
		fail(c, http.StatusNotFound, "vm_not_found")
		return
	}
	if err != nil {
		databaseError(c, err)
		return
	}
	if vmState != "running" && vmState != "stopped" {
		fail(c, http.StatusConflict, "vm_state_conflict")
		return
	}
	spec := CreateVMRequest{ // 复用调度器四维守卫
		VCPUs: vcpus, MemoryMiB: mem, RootDiskGiB: disk,
		Bandwidth: BandwidthSpec{MinKbps: minKbps, MaxKbps: minKbps},
	}
	var targetNode string
	if request.TargetNodeID != nil && *request.TargetNodeID != uuid.Nil {
		// 指定目标：必须与本节点同集群（跨集群迁移另走存储复制通道）。
		var targetCluster string
		err = row(ctx, tx, query.Select("cluster_id").From("kvmcloud_core.nodes").
			Where(sq.Eq{"id": request.TargetNodeID.String()}), &targetCluster)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && targetCluster != clusterID) {
			fail(c, http.StatusUnprocessableEntity, "invalid_target_node")
			return
		}
		if err != nil {
			databaseError(c, err)
			return
		}
		targetNode = request.TargetNodeID.String()
	} else {
		err = row(ctx, tx, candidateNode(clusterID, spec), &targetNode)
		if errors.Is(err, sql.ErrNoRows) {
			c.Header("Retry-After", "1")
			fail(c, http.StatusConflict, "capacity_unavailable")
			return
		}
		if err != nil {
			databaseError(c, err)
			return
		}
	}
	// 目标预留 + 源释放（同一事务；源释放失败视为账本损坏，整体回滚）。
	result, err := exec(ctx, tx, reserveNode(targetNode, spec))
	if _, err = changed(result, err); err != nil {
		databaseError(c, err)
		return
	}
	result, err = exec(ctx, tx, query.Update("kvmcloud_core.nodes").
		Set("used_vcpus", sq.Expr("used_vcpus - ?", vcpus)).
		Set("used_memory_mib", sq.Expr("used_memory_mib - ?", mem)).
		Set("used_disk_gib", sq.Expr("used_disk_gib - ?", disk)).
		Set("used_min_kbps", sq.Expr("used_min_kbps - ?", minKbps)).
		Where(sq.Eq{"id": sourceNode}).
		Where(sq.Expr("used_vcpus >= ?", vcpus)).
		Where(sq.Expr("used_memory_mib >= ?", mem)).
		Where(sq.Expr("used_disk_gib >= ?", disk)).
		Where(sq.Expr("used_min_kbps >= ?", minKbps)))
	released, err := changed(result, err)
	if err != nil {
		databaseError(c, err)
		return
	}
	if !released {
		databaseError(c, errors.New("node ledger invariant violated"))
		return
	}
	taskID, eventID := uuid.New(), uuid.New()
	body, _, err := digestJSON(struct {
		EventID      uuid.UUID `json:"event_id"`
		TaskID       uuid.UUID `json:"task_id"`
		VMID         uuid.UUID `json:"vm_id"`
		SourceNodeID string    `json:"source_node_id"`
		TargetNodeID string    `json:"target_node_id"`
	}{eventID, taskID, vmID, sourceNode, targetNode})
	if err != nil {
		fail(c, http.StatusInternalServerError, "serialization_failed")
		return
	}
	writes := []sq.Sqlizer{
		query.Update("kvmcloud_core.vms").
			Set("node_id", targetNode).Set("state", "migrating").
			Set("desired_version", sq.Expr("desired_version + 1")).
			Where(sq.Eq{"id": vmID.String()}),
		query.Insert("kvmcloud_core.tasks").Columns("id", "tenant_id", "vm_id", "node_id", "kind").
			Values(taskID.String(), p.TenantID.String(), vmID.String(), targetNode, "vm.migrate"),
		query.Insert("kvmcloud_core.outbox_events").Columns(
			"id", "tenant_id", "task_id", "node_id", "subject", "payload").
			Values(eventID.String(), p.TenantID.String(), taskID.String(), targetNode,
				"kvmcloud.commands."+targetNode, string(body)),
	}
	for _, write := range writes {
		if _, err = exec(ctx, tx, write); err != nil {
			databaseError(c, err)
			return
		}
	}
	if err = audit(ctx, tx, c, p, p.TenantID.String(), "vms.migrate", "vm", vmID); err != nil {
		databaseError(c, err)
		return
	}
	response, err := a.cacheIdempotentResponse(ctx, tx, p, operation, key,
		"/api/v1/tasks/"+taskID.String(), http.StatusAccepted,
		Accepted{VMID: vmID, TaskID: taskID, State: "migrating"})
	if err != nil {
		databaseError(c, err)
		return
	}
	if err = tx.Commit(); err != nil {
		databaseError(c, err)
		return
	}
	c.Header("Location", "/api/v1/tasks/"+taskID.String())
	c.Data(http.StatusAccepted, "application/json", response)
}

// ---------------- 模块 11：删除 → 回收站 -------------------------------------

// DeleteVM DELETE /api/v1/vms/{id}：软删除（recycling + purge_after 宽限期），
// 物理清理由 Agent 在宽限期后执行 vm.purge；期间可恢复（vm.restore）。
// 幂等：已进入回收站/终态的 VM 再次 DELETE 返回 204。
func (a *API) DeleteVM(c *gin.Context) {
	p, ok := principal(c, "vms:write", true)
	if !ok {
		return
	}
	vmID, err := uuid.Parse(c.Param("id"))
	if err != nil || vmID == uuid.Nil {
		fail(c, http.StatusBadRequest, "invalid_vm_id")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 8*time.Second)
	defer cancel()
	tx, err := beginTenant(ctx, a.DB, p.TenantID)
	if err != nil {
		databaseError(c, err)
		return
	}
	defer tx.Rollback()
	var nodeID, vmState string
	err = row(ctx, tx, query.Select("node_id", "state").
		From("kvmcloud_core.vms").
		Where(sq.Eq{"id": vmID.String(), "tenant_id": p.TenantID.String()}), &nodeID, &vmState)
	if errors.Is(err, sql.ErrNoRows) {
		fail(c, http.StatusNotFound, "vm_not_found")
		return
	}
	if err != nil {
		databaseError(c, err)
		return
	}
	if !vmStateMutable(vmState) {
		if err = tx.Commit(); err != nil {
			databaseError(c, err)
			return
		}
		c.Status(http.StatusNoContent) // 幂等重删
		return
	}
	taskID, eventID := uuid.New(), uuid.New()
	body, _, err := digestJSON(struct {
		EventID uuid.UUID `json:"event_id"`
		TaskID  uuid.UUID `json:"task_id"`
		VMID    uuid.UUID `json:"vm_id"`
	}{eventID, taskID, vmID})
	if err != nil {
		fail(c, http.StatusInternalServerError, "serialization_failed")
		return
	}
	writes := []sq.Sqlizer{
		query.Update("kvmcloud_core.vms").
			Set("state", "recycling").
			Set("deleted_at", sq.Expr("now()")).
			Set("purge_after", sq.Expr("now() + interval '7 days'")).
			Set("desired_version", sq.Expr("desired_version + 1")).
			Where(sq.Eq{"id": vmID.String()}).Where(sq.Eq{"state": vmState}),
		query.Insert("kvmcloud_core.tasks").Columns("id", "tenant_id", "vm_id", "node_id", "kind").
			Values(taskID.String(), p.TenantID.String(), vmID.String(), nodeID, "vm.recycle"),
		query.Insert("kvmcloud_core.outbox_events").Columns(
			"id", "tenant_id", "task_id", "node_id", "subject", "payload").
			Values(eventID.String(), p.TenantID.String(), taskID.String(), nodeID,
				"kvmcloud.commands."+nodeID, string(body)),
	}
	for _, write := range writes {
		if _, err = exec(ctx, tx, write); err != nil {
			databaseError(c, err)
			return
		}
	}
	if err = audit(ctx, tx, c, p, p.TenantID.String(), "vms.delete", "vm", vmID); err != nil {
		databaseError(c, err)
		return
	}
	if err = tx.Commit(); err != nil {
		databaseError(c, err)
		return
	}
	c.Header("Location", "/api/v1/tasks/"+taskID.String())
	c.JSON(http.StatusAccepted, Accepted{VMID: vmID, TaskID: taskID, State: "recycling"})
}

// ---------------- 模块 8：无缝纳管 -------------------------------------------

type AdoptVMsRequest struct {
	NodeID uuid.UUID `json:"node_id"` // 目标计算节点（Agent 发起本地 libvirt 扫描）
}

// AdoptVMs POST /api/vms/adopt：API 只发起扫描任务；Agent 上报域 XML 元数据
//（经 gRPC 通道），控制面入库后为每个纳管 VM 补发 QoS（vm.bandwidth）。
func (a *API) AdoptVMs(c *gin.Context) {
	p, ok := principal(c, "vms:write", true)
	if !ok {
		return
	}
	key := c.GetHeader("Idempotency-Key")
	if !idempotencyKey.MatchString(key) {
		fail(c, http.StatusBadRequest, "idempotency_key_required")
		return
	}
	var request AdoptVMsRequest
	if !strictJSON(c, &request) {
		return
	}
	if request.NodeID == uuid.Nil {
		fail(c, http.StatusUnprocessableEntity, "invalid_adopt_request")
		return
	}
	_, hash, err := digestJSON(request)
	if err != nil {
		fail(c, http.StatusBadRequest, "invalid_adopt_request")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 8*time.Second)
	defer cancel()
	tx, err := beginTenant(ctx, a.DB, p.TenantID)
	if err != nil {
		databaseError(c, err)
		return
	}
	defer tx.Rollback()
	operation := "POST /api/v1/vms/adopt"
	replayed, err := a.claimIdempotency(ctx, tx, c, p, operation, key, hash)
	if err != nil {
		databaseError(c, err)
		return
	}
	if replayed {
		return
	}
	var nodeID string
	err = row(ctx, tx, query.Select("id").From("kvmcloud_core.nodes").
		Where(sq.Eq{"id": request.NodeID.String()}), &nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		fail(c, http.StatusNotFound, "node_not_found")
		return
	}
	if err != nil {
		databaseError(c, err)
		return
	}
	taskID, eventID := uuid.New(), uuid.New()
	body, _, err := digestJSON(struct {
		EventID uuid.UUID `json:"event_id"`
		TaskID  uuid.UUID `json:"task_id"`
		NodeID  uuid.UUID `json:"node_id"`
	}{eventID, taskID, request.NodeID})
	if err != nil {
		fail(c, http.StatusInternalServerError, "serialization_failed")
		return
	}
	writes := []sq.Sqlizer{
		query.Insert("kvmcloud_core.tasks").Columns("id", "tenant_id", "node_id", "kind").
			Values(taskID.String(), p.TenantID.String(), nodeID, "vm.adopt"),
		query.Insert("kvmcloud_core.outbox_events").Columns(
			"id", "tenant_id", "task_id", "node_id", "subject", "payload").
			Values(eventID.String(), p.TenantID.String(), taskID.String(), nodeID,
				"kvmcloud.commands."+nodeID, string(body)),
	}
	for _, write := range writes {
		if _, err = exec(ctx, tx, write); err != nil {
			databaseError(c, err)
			return
		}
	}
	if err = audit(ctx, tx, c, p, p.TenantID.String(), "vms.adopt.scan", "node", request.NodeID); err != nil {
		databaseError(c, err)
		return
	}
	response, err := a.cacheIdempotentResponse(ctx, tx, p, operation, key,
		"/api/v1/tasks/"+taskID.String(), http.StatusAccepted,
		Accepted{TaskID: taskID, State: "pending"})
	if err != nil {
		databaseError(c, err)
		return
	}
	if err = tx.Commit(); err != nil {
		databaseError(c, err)
		return
	}
	c.Header("Location", "/api/v1/tasks/"+taskID.String())
	c.Data(http.StatusAccepted, "application/json", response)
}
