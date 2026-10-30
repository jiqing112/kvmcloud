package coreapi

// 模块 1：端口级带宽调度。API 仅接收声明式配置（min_kbps/max_kbps/shared_pool_id/priority）。
//
// 职责链：参数校验 → scope/幂等 → 池余量账本变更（守卫 UPDATE）→ 策略落库
// → vm.bandwidth 任务 + outbox 事件 → 202。TC 规则由 Agent 内部安全执行
//（internal/agentlib.QoSApplier）；借用授予/抢占压回由任务执行器调用
// internal/bandwidth.Decide 纯函数仲裁。

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

// SetBandwidthRequest 复用 CreateVM 的带宽规格（字段与校验一致）。
type SetBandwidthRequest struct {
	BandwidthSpec
}

// SetVMBandwidth PUT /api/v1/vms/{id}/bandwidth
func (a *API) SetVMBandwidth(c *gin.Context) {
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
	var request SetBandwidthRequest
	if !strictJSON(c, &request) {
		return
	}
	if !request.BandwidthSpec.valid() {
		fail(c, http.StatusUnprocessableEntity, "invalid_bandwidth_spec")
		return
	}
	// priority > 50 属抢占优待，需独立授权（与 CreateVM 同门槛）。
	if request.Priority > 50 && !p.Scopes["bandwidth:priority:high"] {
		fail(c, http.StatusForbidden, "priority_not_entitled")
		return
	}
	_, hash, err := digestJSON(request)
	if err != nil {
		fail(c, http.StatusBadRequest, "invalid_bandwidth_spec")
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
	operation := fmt.Sprintf("PUT /api/v1/vms/%s/bandwidth", vmID)
	replayed, err := a.claimIdempotency(ctx, tx, c, p, operation, key, hash)
	if err != nil {
		databaseError(c, err)
		return
	}
	if replayed {
		return
	}

	// VM + 主 NIC + 当前策略一次读取（策略行改写由守卫 UPDATE 收敛；adopted VM 可能无策略）。
	var vmState, nicID, nodeID string
	var policyID, oldMin sql.NullString
	var oldPool sql.NullString
	err = row(ctx, tx, query.Select("v.state", "v.node_id", "n.id AS nic", "bp.id AS policy",
		"bp.min_kbps::text AS min", "bp.shared_pool_id::text AS pool").
		From("kvmcloud_core.vms v").
		Join("kvmcloud_core.vm_nics n ON n.vm_id = v.id AND n.primary_nic").
		LeftJoin("kvmcloud_core.bandwidth_policies bp ON bp.nic_id = n.id AND bp.direction = 'egress'").
		Where(sq.Eq{"v.id": vmID.String(), "v.tenant_id": p.TenantID.String()}),
		&vmState, &nodeID, &nicID, &policyID, &oldMin, &oldPool)
	if errors.Is(err, sql.ErrNoRows) {
		fail(c, http.StatusNotFound, "vm_not_found")
		return
	}
	if err != nil {
		databaseError(c, err)
		return
	}
	if !vmStateMutable(vmState) {
		fail(c, http.StatusConflict, "vm_state_conflict")
		return
	}
	oldMinKbps := int64(0)
	if oldMin.Valid {
		if _, err = fmt.Sscan(oldMin.String, &oldMinKbps); err != nil {
			databaseError(c, err)
			return
		}
	}
	// 池账本：同池净额变更 / 异池一还一借 / 池与裸端口互转（容量守卫在 WHERE 内）。
	if !a.reconcilePool(c, tx, p, request.SharedPoolID, oldPool, request.MinKbps, oldMinKbps) {
		return
	}
	if policyID.Valid {
		result, err := exec(ctx, tx, query.Update("kvmcloud_core.bandwidth_policies").
			Set("min_kbps", request.MinKbps).Set("max_kbps", request.MaxKbps).
			Set("shared_pool_id", nullableText(poolIDText(request.SharedPoolID))).
			Set("priority", request.Priority).
			Set("desired_version", sq.Expr("desired_version + 1")).
			Where(sq.Eq{"id": policyID.String}))
		if _, err = changed(result, err); err != nil {
			databaseError(c, err)
			return
		}
	} else {
		// 纳管（adopt）VM 首次声明带宽策略。
		if _, err = exec(ctx, tx, query.Insert("kvmcloud_core.bandwidth_policies").Columns(
			"id", "tenant_id", "nic_id", "min_kbps", "max_kbps", "shared_pool_id", "priority").
			Values(uuid.NewString(), p.TenantID.String(), nicID, request.MinKbps, request.MaxKbps,
				nullableText(poolIDText(request.SharedPoolID)), request.Priority)); err != nil {
			databaseError(c, err)
			return
		}
	}

	taskID, eventID := uuid.New(), uuid.New()
	body, _, err := digestJSON(struct {
		EventID      uuid.UUID  `json:"event_id"`
		TaskID       uuid.UUID  `json:"task_id"`
		VMID         uuid.UUID  `json:"vm_id"`
		NICID        string     `json:"nic_id"`
		MinKbps      int64      `json:"min_kbps"`
		MaxKbps      int64      `json:"max_kbps"`
		SharedPoolID *uuid.UUID `json:"shared_pool_id,omitempty"`
		Priority     int        `json:"priority"`
	}{eventID, taskID, vmID, nicID, request.MinKbps, request.MaxKbps, request.SharedPoolID, request.Priority})
	if err != nil {
		fail(c, http.StatusInternalServerError, "serialization_failed")
		return
	}
	writes := []sq.Sqlizer{
		query.Insert("kvmcloud_core.tasks").Columns("id", "tenant_id", "vm_id", "node_id", "kind").
			Values(taskID.String(), p.TenantID.String(), vmID.String(), nodeID, "vm.bandwidth"),
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
	if err = audit(ctx, tx, c, p, p.TenantID.String(), "vms.bandwidth", "vm", vmID); err != nil {
		databaseError(c, err)
		return
	}
	response, err := a.cacheIdempotentResponse(ctx, tx, p, operation, key,
		"/api/v1/tasks/"+taskID.String(), http.StatusAccepted,
		Accepted{VMID: vmID, TaskID: taskID, State: "pending"})
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

// reconcilePool 池余量账本对账：容量守卫失败（0 行）→ 409 pool_unavailable。
// 返回 false 表示响应已写出，调用方直接 return（defer Rollback 撤销已完成扣减）。
func (a *API) reconcilePool(c *gin.Context, tx *sql.Tx, p Principal,
	newPool *uuid.UUID, oldPool sql.NullString, newMin, oldMin int64) bool {
	newPoolText := poolIDText(newPool)
	samePool := oldPool.String == newPoolText && (oldPool.Valid == (newPool != nil))
	ctx := c.Request.Context()
	switch {
	case samePool && newPool != nil && newMin != oldMin:
		return a.guardedPoolDelta(c, tx, p, newPool.String(), newMin-oldMin)
	case samePool:
		return true
	case oldPool.Valid:
		// 归还旧池保障额度（不变量守卫：不得减为负值）。
		result, err := exec(ctx, tx, query.Update("kvmcloud_core.shared_bandwidth_pools").
			Set("guaranteed_kbps", sq.Expr("guaranteed_kbps - ?", oldMin)).
			Set("version", sq.Expr("version + 1")).
			Where(sq.Eq{"id": oldPool.String, "tenant_id": p.TenantID.String()}).
			Where(sq.Expr("guaranteed_kbps >= ?", oldMin)))
		released, err := changed(result, err)
		if err != nil {
			databaseError(c, err)
			return false
		}
		if !released {
			databaseError(c, errPoolLedgerCorrupt)
			return false
		}
	}
	if newPool != nil {
		// 新池保障额度预留（与 CreateVM 同守卫）。
		result, err := exec(ctx, tx, query.Update("kvmcloud_core.shared_bandwidth_pools").
			Set("guaranteed_kbps", sq.Expr("guaranteed_kbps + ?", newMin)).
			Set("version", sq.Expr("version + 1")).
			Where(sq.Eq{"id": newPool.String(), "tenant_id": p.TenantID.String()}).
			Where(sq.Expr("guaranteed_kbps::numeric + leased_kbps + ? <= capacity_kbps", newMin)))
		reserved, err := changed(result, err)
		if err != nil {
			databaseError(c, err)
			return false
		}
		if !reserved {
			fail(c, http.StatusConflict, "pool_unavailable")
			return false
		}
	}
	return true
}

// guardedPoolDelta 同池净额变更（delta 可为负，容量守卫覆盖双向）。
func (a *API) guardedPoolDelta(c *gin.Context, tx *sql.Tx, p Principal, poolID string, delta int64) bool {
	result, err := exec(c.Request.Context(), tx, query.Update("kvmcloud_core.shared_bandwidth_pools").
		Set("guaranteed_kbps", sq.Expr("guaranteed_kbps + ?", delta)).
		Set("version", sq.Expr("version + 1")).
		Where(sq.Eq{"id": poolID, "tenant_id": p.TenantID.String()}).
		Where(sq.Expr("guaranteed_kbps + ? >= 0", delta)).
		Where(sq.Expr("guaranteed_kbps::numeric + leased_kbps + ? <= capacity_kbps", delta)))
	updated, err := changed(result, err)
	if err != nil {
		databaseError(c, err)
		return false
	}
	if !updated {
		fail(c, http.StatusConflict, "pool_unavailable")
		return false
	}
	return true
}

func poolIDText(pool *uuid.UUID) string {
	if pool == nil {
		return ""
	}
	return pool.String()
}

var errPoolLedgerCorrupt = errors.New("bandwidth pool ledger invariant violated")
