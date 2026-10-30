package coreapi

// 模块 9：分布式安全组。API 接收组绑定列表，校验归属后在同一事务内
// 全量替换 vm_security_groups，并把完整声明式规则集下发 outbox；
// Agent 转换为 nftables（internal/agentlib.NftApplier.ApplySecurityGroups），
// 与 NAT 分链共存（kvmcloud_nat / kvmcloud_sec 两个独立表）。

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

const (
	maxVMGroups = 50  // 单 VM 绑定组上限
	maxSGRules  = 200 // 下发规则集上限（防 outbox 膨胀）
)

type SetSecurityGroupsRequest struct {
	SecurityGroupIDs []uuid.UUID `json:"security_group_ids"`
}

func (r SetSecurityGroupsRequest) valid() bool {
	if len(r.SecurityGroupIDs) > maxVMGroups {
		return false
	}
	seen := make(map[uuid.UUID]struct{}, len(r.SecurityGroupIDs))
	for _, id := range r.SecurityGroupIDs {
		if id == uuid.Nil {
			return false
		}
		if _, dup := seen[id]; dup {
			return false
		}
		seen[id] = struct{}{}
	}
	return true
}

type sgRulePayload struct {
	GroupID    string `json:"group_id"`
	Direction  string `json:"direction"`
	EtherType  int    `json:"ethertype"`
	Protocol   string `json:"protocol"`
	RemoteCIDR string `json:"remote_cidr"`
	PortFrom   *int   `json:"port_from,omitempty"`
	PortTo     *int   `json:"port_to,omitempty"`
	ICMPType   *int   `json:"icmp_type,omitempty"`
	ICMPCode   *int   `json:"icmp_code,omitempty"`
}

// SetVMSecurityGroups PUT /api/v1/vms/{vm_id}/security-groups
func (a *API) SetVMSecurityGroups(c *gin.Context) {
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
	var request SetSecurityGroupsRequest
	if !strictJSON(c, &request) {
		return
	}
	if !request.valid() {
		fail(c, http.StatusUnprocessableEntity, "invalid_security_groups")
		return
	}
	_, hash, err := digestJSON(request)
	if err != nil {
		fail(c, http.StatusBadRequest, "invalid_security_groups")
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
	operation := fmt.Sprintf("PUT /api/v1/vms/%s/security-groups", vmID)
	replayed, err := a.claimIdempotency(ctx, tx, c, p, operation, key, hash)
	if err != nil {
		databaseError(c, err)
		return
	}
	if replayed {
		return
	}

	var vmState, nodeID, nicID string
	err = row(ctx, tx, query.Select("v.state", "v.node_id", "n.id AS nic").
		From("kvmcloud_core.vms v").
		Join("kvmcloud_core.vm_nics n ON n.vm_id = v.id AND n.primary_nic").
		Where(sq.Eq{"v.id": vmID.String(), "v.tenant_id": p.TenantID.String()}),
		&vmState, &nodeID, &nicID)
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
	// 组归属校验：逐一可见性由 RLS 兜底，这里显式计数防部分越权。
	ids := make([]string, 0, len(request.SecurityGroupIDs))
	for _, id := range request.SecurityGroupIDs {
		ids = append(ids, id.String())
	}
	var owned int
	err = row(ctx, tx, query.Select("count(*)").
		From("kvmcloud_core.security_groups").
		Where(sq.Eq{"tenant_id": p.TenantID.String()}).
		Where(sq.Eq{"id": ids}), &owned)
	if err != nil {
		databaseError(c, err)
		return
	}
	if int(owned) != len(ids) {
		fail(c, http.StatusNotFound, "security_group_not_found")
		return
	}
	// 下发完整规则集（声明式元数据；上限保护 outbox）。
	rulesSQL, rulesArgs, err := query.Select("r.group_id", "r.direction", "r.ethertype",
		"r.protocol", "r.remote_cidr::text", "r.port_from", "r.port_to", "r.icmp_type", "r.icmp_code").
		From("kvmcloud_core.security_group_rules r").
		Where(sq.Eq{"r.tenant_id": p.TenantID.String()}).
		Where(sq.Eq{"r.group_id": ids}).
		OrderBy("r.group_id", "r.id").Limit(uint64(maxSGRules + 1)).ToSql()
	if err != nil {
		databaseError(c, err)
		return
	}
	rows, err := tx.QueryContext(ctx, rulesSQL, rulesArgs...)
	if err != nil {
		databaseError(c, err)
		return
	}
	defer rows.Close()
	rules := make([]sgRulePayload, 0, maxSGRules)
	for rows.Next() {
		var rule sgRulePayload
		var from, to, icmpType, icmpCode sql.NullInt64
		if err = rows.Scan(&rule.GroupID, &rule.Direction, &rule.EtherType, &rule.Protocol,
			&rule.RemoteCIDR, &from, &to, &icmpType, &icmpCode); err != nil {
			databaseError(c, err)
			return
		}
		if from.Valid {
			v := int(from.Int64)
			rule.PortFrom = &v
		}
		if to.Valid {
			v := int(to.Int64)
			rule.PortTo = &v
		}
		if icmpType.Valid {
			v := int(icmpType.Int64)
			rule.ICMPType = &v
		}
		if icmpCode.Valid {
			v := int(icmpCode.Int64)
			rule.ICMPCode = &v
		}
		rules = append(rules, rule)
	}
	if err = rows.Err(); err != nil {
		databaseError(c, err)
		return
	}
	if len(rules) > maxSGRules {
		fail(c, http.StatusUnprocessableEntity, "too_many_rules")
		return
	}
	taskID, eventID := uuid.New(), uuid.New()
	body, _, err := digestJSON(struct {
		EventID uuid.UUID      `json:"event_id"`
		TaskID  uuid.UUID      `json:"task_id"`
		VMID    uuid.UUID      `json:"vm_id"`
		NICID   string         `json:"nic_id"`
		Rules   []sgRulePayload `json:"rules"`
	}{eventID, taskID, vmID, nicID, rules})
	if err != nil {
		fail(c, http.StatusInternalServerError, "serialization_failed")
		return
	}
	writes := []sq.Sqlizer{
		query.Delete("kvmcloud_core.vm_security_groups").
			Where(sq.Eq{"tenant_id": p.TenantID.String(), "vm_id": vmID.String()}),
		query.Insert("kvmcloud_core.vm_security_groups").Columns("tenant_id", "vm_id", "group_id"),
		query.Insert("kvmcloud_core.tasks").Columns("id", "tenant_id", "vm_id", "node_id", "kind").
			Values(taskID.String(), p.TenantID.String(), vmID.String(), nodeID, "vm.security"),
		query.Insert("kvmcloud_core.outbox_events").Columns(
			"id", "tenant_id", "task_id", "node_id", "subject", "payload").
			Values(eventID.String(), p.TenantID.String(), taskID.String(), nodeID,
				"kvmcloud.commands."+nodeID, string(body)),
	}
	if _, err = exec(ctx, tx, writes[0]); err != nil {
		databaseError(c, err)
		return
	}
	// squirrel 批量 INSERT：一条语句写入全部绑定。
	insert := writes[1].(sq.InsertBuilder)
	for _, id := range ids {
		insert = insert.Values(p.TenantID.String(), vmID.String(), id)
	}
	if _, err = exec(ctx, tx, insert); err != nil {
		databaseError(c, err)
		return
	}
	for _, write := range writes[2:] {
		if _, err = exec(ctx, tx, write); err != nil {
			databaseError(c, err)
			return
		}
	}
	if err = audit(ctx, tx, c, p, p.TenantID.String(), "vms.security.replace", "vm", vmID); err != nil {
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
