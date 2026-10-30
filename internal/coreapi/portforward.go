package coreapi

// 模块 6：单 IP NAT 转发。API 仅存储映射关系并下发事件；
// Agent 侧由 internal/agentlib.NftApplier.ApplyNAT 全量收敛 nftables，
// NAT 作用于 netfilter hook、TC 限速作用于 qdisc，二者分层共存互不破坏。

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

type CreatePortForwardingRequest struct {
	PublicIPID   uuid.UUID `json:"public_ip_id"`
	Protocol     string    `json:"protocol"` // tcp | udp
	ExternalPort int       `json:"external_port"`
	InternalPort int       `json:"internal_port"`
}

func (r CreatePortForwardingRequest) valid() bool {
	return r.PublicIPID != uuid.Nil && (r.Protocol == "tcp" || r.Protocol == "udp") &&
		r.ExternalPort >= 1 && r.ExternalPort <= 65535 &&
		r.InternalPort >= 1 && r.InternalPort <= 65535
}

// CreatePortForwarding POST /api/v1/vms/{vm_id}/port-forwardings
func (a *API) CreatePortForwarding(c *gin.Context) {
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
	var request CreatePortForwardingRequest
	if !strictJSON(c, &request) {
		return
	}
	if !request.valid() {
		fail(c, http.StatusUnprocessableEntity, "invalid_port_forwarding")
		return
	}
	_, hash, err := digestJSON(request)
	if err != nil {
		fail(c, http.StatusBadRequest, "invalid_port_forwarding")
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
	operation := fmt.Sprintf("POST /api/v1/vms/%s/port-forwardings", vmID)
	replayed, err := a.claimIdempotency(ctx, tx, c, p, operation, key, hash)
	if err != nil {
		databaseError(c, err)
		return
	}
	if replayed {
		return
	}

	// VM + 主 NIC（NAT 目标必须有私网地址）。
	var vmState, nicID, nodeID string
	var privateIPv4 sql.NullString
	err = row(ctx, tx, query.Select("v.state", "v.node_id", "n.id AS nic", "n.private_ipv4::text AS ip").
		From("kvmcloud_core.vms v").
		Join("kvmcloud_core.vm_nics n ON n.vm_id = v.id AND n.primary_nic").
		Where(sq.Eq{"v.id": vmID.String(), "v.tenant_id": p.TenantID.String()}),
		&vmState, &nodeID, &nicID, &privateIPv4)
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
	if !privateIPv4.Valid {
		fail(c, http.StatusConflict, "nic_address_missing")
		return
	}
	// 公网 IP 归属校验（RLS + 显式租户条件）。
	var publicAddress, gatewayNodeID string
	err = row(ctx, tx, query.Select("address::text", "gateway_node_id").
		From("kvmcloud_core.public_ips").
		Where(sq.Eq{"id": request.PublicIPID.String(), "tenant_id": p.TenantID.String()}),
		&publicAddress, &gatewayNodeID)
	if errors.Is(err, sql.ErrNoRows) {
		fail(c, http.StatusNotFound, "public_ip_not_found")
		return
	}
	if err != nil {
		databaseError(c, err)
		return
	}

	forwardingID, taskID, eventID := uuid.New(), uuid.New(), uuid.New()
	body, _, err := digestJSON(struct {
		EventID       uuid.UUID `json:"event_id"`
		TaskID        uuid.UUID `json:"task_id"`
		VMID          uuid.UUID `json:"vm_id"`
		NICID         string    `json:"nic_id"`
		ForwardingID  uuid.UUID `json:"forwarding_id"`
		PublicAddress string    `json:"public_address"`
		Protocol      string    `json:"protocol"`
		ExternalPort  int       `json:"external_port"`
		InternalIPv4  string    `json:"internal_ipv4"`
		InternalPort  int       `json:"internal_port"`
	}{eventID, taskID, vmID, nicID, forwardingID, publicAddress,
		request.Protocol, request.ExternalPort, privateIPv4.String, request.InternalPort})
	if err != nil {
		fail(c, http.StatusInternalServerError, "serialization_failed")
		return
	}
	writes := []sq.Sqlizer{
		// (public_ip, protocol, external_port) 唯一索引：端口占用 → 23505 → resource_conflict。
		query.Insert("kvmcloud_core.port_forwardings").Columns(
			"id", "tenant_id", "public_ip_id", "nic_id", "protocol",
			"external_port", "internal_port", "state").
			Values(forwardingID.String(), p.TenantID.String(), request.PublicIPID.String(),
				nicID, request.Protocol, request.ExternalPort, request.InternalPort, "pending"),
		query.Insert("kvmcloud_core.tasks").Columns("id", "tenant_id", "vm_id", "node_id", "kind").
			Values(taskID.String(), p.TenantID.String(), vmID.String(), nodeID, "vm.nat"),
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
	if err = audit(ctx, tx, c, p, p.TenantID.String(), "vms.nat.create", "port_forwarding", forwardingID); err != nil {
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
