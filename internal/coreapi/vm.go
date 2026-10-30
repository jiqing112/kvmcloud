package coreapi

// CreateVM：四维超开调度（模块 2）。
//
// 全流程在单个 PostgreSQL 事务内完成（脚本化测试 creationSteps 逐条断言顺序）：
//   set_config → active 租户校验 → 幂等键占用/重放 → 网络归属 → 镜像可见性/大小
//   → 租户配额五维原子扣减 → [共享池保障余量] → 节点四维 FOR UPDATE SKIP LOCKED
//   → 节点预留 → VM/NIC/QoS/预留/根卷 元数据 → task + outbox + audit
//   → 幂等响应缓存 → COMMIT。
//
// 锁顺序固定：tenant quota → shared pool → node（SKIP LOCKED）→ VM 元数据。
// COMMIT 回执丢失是歧义状态：返回 503，客户端必须用原幂等键重试，绝不补偿。

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	sq "github.com/Masterminds/squirrel"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const opCreateVM = "POST /api/v1/vms"

var (
	vmName         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	idempotencyKey = regexp.MustCompile(`^[\x21-\x7e]{16,128}$`)
)

type BandwidthSpec struct {
	MinKbps      int64      `json:"min_kbps"`
	MaxKbps      int64      `json:"max_kbps"`
	SharedPoolID *uuid.UUID `json:"shared_pool_id"`
	Priority     int        `json:"priority"`
}

func (b BandwidthSpec) valid() bool {
	const maxKbpsLimit = 1_000_000_000_000
	return b.MinKbps >= 0 && b.MaxKbps >= 1 && b.MaxKbps <= maxKbpsLimit &&
		b.MinKbps <= b.MaxKbps && b.Priority >= 0 && b.Priority <= 100 &&
		(b.SharedPoolID == nil || *b.SharedPoolID != uuid.Nil)
}

type CreateVMRequest struct {
	Name string `json:"name"`
	// ImageID 与 ImageSourceName 二选一：UUID 直选已就绪镜像，
	// 或镜像源规范名（local_images.name，须 available 且对本租户可见）。
	ImageID         uuid.UUID     `json:"image_id"`
	ImageSourceName string        `json:"image_source_name,omitempty"`
	NetworkID       uuid.UUID     `json:"network_id"`
	VCPUs           int64         `json:"vcpus"`
	MemoryMiB       int64         `json:"memory_mib"`
	RootDiskGiB     int64         `json:"root_disk_gib"`
	Bandwidth       BandwidthSpec `json:"bandwidth"`
}

func (r CreateVMRequest) valid() bool {
	if !vmName.MatchString(r.Name) || r.NetworkID == uuid.Nil {
		return false
	}
	// 恰好提供一种镜像指定方式：两者同给或都不给均拒绝。
	if (r.ImageID != uuid.Nil) == (r.ImageSourceName != "") {
		return false
	}
	if r.ImageSourceName != "" &&
		(len(r.ImageSourceName) > imageSourceNameMax || !imageRefName.MatchString(r.ImageSourceName)) {
		return false
	}
	return r.VCPUs >= 1 && r.VCPUs <= 4096 && r.MemoryMiB >= 128 && r.MemoryMiB <= 16_777_216 &&
		r.RootDiskGiB >= 1 && r.RootDiskGiB <= 1_048_576 && r.Bandwidth.valid()
}

type Accepted struct {
	VMID   uuid.UUID `json:"vm_id"`
	TaskID uuid.UUID `json:"task_id"`
	State  string    `json:"state"`
}

// CreateVM 提交配额、节点/池容量、VM 元数据、任务、outbox、幂等响应与审计于同一事务。
func (a *API) CreateVM(c *gin.Context) {
	p, ok := principal(c, "vms:write", true)
	if !ok {
		return
	}
	key := c.GetHeader("Idempotency-Key")
	if !idempotencyKey.MatchString(key) {
		fail(c, http.StatusBadRequest, "idempotency_key_required")
		return
	}
	var request CreateVMRequest
	if !strictJSON(c, &request) {
		return
	}
	if !request.valid() {
		fail(c, http.StatusUnprocessableEntity, "invalid_vm_spec")
		return
	}
	// priority > 50 属抢占优待，需要独立授权。
	if request.Bandwidth.Priority > 50 && !p.Scopes["bandwidth:priority:high"] {
		fail(c, http.StatusForbidden, "priority_not_entitled")
		return
	}
	_, hash, err := digestJSON(request)
	if err != nil {
		fail(c, http.StatusBadRequest, "invalid_vm_spec")
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

	var tenantID string
	err = row(ctx, tx, query.Select("id").From("kvmcloud_core.tenants").
		Where(sq.Eq{"id": p.TenantID.String(), "state": "active"}).
		Suffix("FOR SHARE"), &tenantID)
	if errors.Is(err, sql.ErrNoRows) {
		fail(c, http.StatusForbidden, "tenant_inactive")
		return
	}
	if err != nil {
		databaseError(c, err)
		return
	}
	replayed, err := a.claimIdempotency(ctx, tx, c, p, key, hash)
	if err != nil {
		databaseError(c, err)
		return
	}
	if replayed {
		return
	}

	var clusterID string
	err = row(ctx, tx, query.Select("cluster_id").From("kvmcloud_core.networks").
		Where(sq.Eq{"id": request.NetworkID.String(), "tenant_id": p.TenantID.String(), "state": "ready"}).
		Suffix("FOR SHARE"), &clusterID)
	if errors.Is(err, sql.ErrNoRows) {
		fail(c, http.StatusNotFound, "network_not_found")
		return
	}
	if err != nil {
		databaseError(c, err)
		return
	}

	var imageID string
	var imageSourceName any
	if request.ImageSourceName != "" {
		// 规范名路径：本地镜像库中可用（available + 已知大小）且对租户可见。
		err = row(ctx, tx, query.Select("i.id").From("kvmcloud_core.local_images i").
			Where(sq.Eq{"i.name": request.ImageSourceName, "i.pull_state": "available"}).
			Where(sq.Or{sq.Eq{"i.tenant_id": nil}, sq.Eq{"i.tenant_id": p.TenantID.String()}}).
			Where("i.size_bytes IS NOT NULL").
			Suffix("FOR SHARE"), &imageID)
		if errors.Is(err, sql.ErrNoRows) {
			fail(c, http.StatusNotFound, "image_not_found_or_not_ready")
			return
		}
		if err != nil {
			databaseError(c, err)
			return
		}
		imageSourceName = request.ImageSourceName
	} else {
		err = row(ctx, tx, query.Select("id").From("kvmcloud_core.images").
			Where(sq.Eq{"id": request.ImageID.String(), "state": "ready"}).
			Where(sq.Or{sq.Eq{"tenant_id": nil}, sq.Eq{"tenant_id": p.TenantID.String()}}).
			Where(sq.LtOrEq{"min_disk_gib": request.RootDiskGiB}), &imageID)
		if errors.Is(err, sql.ErrNoRows) {
			fail(c, http.StatusNotFound, "image_not_found_or_too_large")
			return
		}
		if err != nil {
			databaseError(c, err)
			return
		}
	}

	// 租户配额五维原子扣减（任一维超限 → 0 行）。
	result, err := exec(ctx, tx, reserveQuota(p.TenantID, request))
	reserved, err := changed(result, err)
	if err != nil {
		databaseError(c, err)
		return
	}
	if !reserved {
		fail(c, http.StatusConflict, "quota_exceeded")
		return
	}
	if request.Bandwidth.SharedPoolID != nil {
		// 共享池保障余量扣减（乐观 version 递增，容量守卫在 WHERE 内）。
		result, err = exec(ctx, tx, query.Update("kvmcloud_core.shared_bandwidth_pools").
			Set("guaranteed_kbps", sq.Expr("guaranteed_kbps + ?", request.Bandwidth.MinKbps)).
			Set("version", sq.Expr("version + 1")).
			Where(sq.Eq{"id": request.Bandwidth.SharedPoolID.String(), "tenant_id": p.TenantID.String()}).
			Where(sq.Expr("guaranteed_kbps::numeric + leased_kbps + ? <= capacity_kbps", request.Bandwidth.MinKbps)))
		reserved, err = changed(result, err)
		if err != nil {
			databaseError(c, err)
			return
		}
		if !reserved {
			fail(c, http.StatusConflict, "pool_unavailable")
			return
		}
	}

	// 节点选择：集群内四维水位最低者，SKIP LOCKED 避免并发争抢。
	var nodeID string
	err = row(ctx, tx, candidateNode(clusterID, request), &nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		c.Header("Retry-After", "1")
		fail(c, http.StatusConflict, "capacity_unavailable")
		return
	}
	if err != nil {
		databaseError(c, err)
		return
	}
	result, err = exec(ctx, tx, reserveNode(nodeID, request))
	reserved, err = changed(result, err)
	if err != nil {
		databaseError(c, err)
		return
	}
	if !reserved {
		fail(c, http.StatusConflict, "capacity_unavailable")
		return
	}

	vmID, nicID, volumeID := uuid.New(), uuid.New(), uuid.New()
	taskID, eventID, reservationID := uuid.New(), uuid.New(), uuid.New()
	mac, err := randomMAC()
	if err != nil {
		fail(c, http.StatusServiceUnavailable, "entropy_unavailable")
		return
	}
	var poolID any
	if request.Bandwidth.SharedPoolID != nil {
		poolID = request.Bandwidth.SharedPoolID.String()
	}
	writes := []sq.Sqlizer{
		query.Insert("kvmcloud_core.vms").Columns(
			"id", "tenant_id", "node_id", "name", "image_id", "image_source_name",
			"domain_uuid", "network_id", "vcpus", "memory_mib", "root_disk_gib", "state").
			Values(vmID.String(), p.TenantID.String(), nodeID, request.Name,
				nullableText(imageID), imageSourceName, vmID.String(), request.NetworkID.String(),
				request.VCPUs, request.MemoryMiB, request.RootDiskGiB, "creating"),
		query.Insert("kvmcloud_core.vm_nics").Columns(
			"id", "tenant_id", "vm_id", "network_id", "mac").
			Values(nicID.String(), p.TenantID.String(), vmID.String(), request.NetworkID.String(), mac),
		query.Insert("kvmcloud_core.bandwidth_policies").Columns(
			"id", "tenant_id", "nic_id", "min_kbps", "max_kbps", "shared_pool_id", "priority").
			Values(uuid.NewString(), p.TenantID.String(), nicID.String(),
				request.Bandwidth.MinKbps, request.Bandwidth.MaxKbps, poolID, request.Bandwidth.Priority),
		query.Insert("kvmcloud_core.resource_reservations").Columns(
			"id", "tenant_id", "vm_id", "node_id", "purpose", "vcpus", "memory_mib", "disk_gib", "min_kbps").
			Values(reservationID.String(), p.TenantID.String(), vmID.String(), nodeID, "create",
				request.VCPUs, request.MemoryMiB, request.RootDiskGiB, request.Bandwidth.MinKbps),
		query.Insert("kvmcloud_core.volumes").Columns(
			"id", "tenant_id", "node_id", "vm_id", "name", "backend", "backend_ref", "role", "size_gib", "state").
			Values(volumeID.String(), p.TenantID.String(), nodeID, vmID.String(), "root",
				"qcow2", volumeID.String(), "root", request.RootDiskGiB, "creating"),
		query.Insert("kvmcloud_core.tasks").Columns(
			"id", "tenant_id", "vm_id", "node_id", "kind").
			Values(taskID.String(), p.TenantID.String(), vmID.String(), nodeID, "vm.create"),
	}
	for _, write := range writes {
		if _, err = exec(ctx, tx, write); err != nil {
			databaseError(c, err)
			return
		}
	}
	// Outbox 负载只含声明式标识与规格：无 SSH、无密码、无 shell、无任意 URL/路径。
	payload, _, err := digestJSON(struct {
		EventID      uuid.UUID       `json:"event_id"`
		TaskID       uuid.UUID       `json:"task_id"`
		TenantID     uuid.UUID       `json:"tenant_id"`
		NodeID       string          `json:"node_id"`
		VMID         uuid.UUID       `json:"vm_id"`
		NICID        uuid.UUID       `json:"nic_id"`
		RootVolumeID uuid.UUID       `json:"root_volume_id"`
		Generation   int64           `json:"generation"`
		Kind         string          `json:"kind"`
		MAC          string          `json:"mac"`
		Spec         CreateVMRequest `json:"spec"`
	}{eventID, taskID, p.TenantID, nodeID, vmID, nicID, volumeID, 1, "vm.create", mac, request})
	if err != nil {
		fail(c, http.StatusInternalServerError, "serialization_failed")
		return
	}
	if _, err = exec(ctx, tx, query.Insert("kvmcloud_core.outbox_events").
		Columns("id", "tenant_id", "task_id", "node_id", "subject", "payload").
		Values(eventID.String(), p.TenantID.String(), taskID.String(), nodeID,
			"kvmcloud.commands."+nodeID, string(payload))); err != nil {
		databaseError(c, err)
		return
	}
	if err = audit(ctx, tx, c, p, p.TenantID.String(), "vms.create", "vm", vmID); err != nil {
		databaseError(c, err)
		return
	}
	response, _, err := digestJSON(Accepted{VMID: vmID, TaskID: taskID, State: "pending"})
	if err != nil {
		fail(c, http.StatusInternalServerError, "serialization_failed")
		return
	}
	location := "/api/v1/tasks/" + taskID.String()
	if _, err = exec(ctx, tx, query.Update("kvmcloud_core.idempotency_keys").
		Set("response_status", http.StatusAccepted).Set("response_body", string(response)).
		Set("location", location).Where(idempotencyWhere(p, opCreateVM, key))); err != nil {
		databaseError(c, err)
		return
	}
	if err = tx.Commit(); err != nil {
		// COMMIT 回执丢失是歧义状态：用同一幂等键重试，不做外部补偿。
		databaseError(c, err)
		return
	}
	c.Header("Location", location)
	c.Data(http.StatusAccepted, "application/json", response)
}

// claimIdempotency 先 INSERT 占位（ON CONFLICT DO NOTHING）；命中已有键时锁定
// 原行比较摘要并重放已提交响应。返回 replayed=true 表示响应已写出。
func (a *API) claimIdempotency(ctx context.Context, tx *sql.Tx, c *gin.Context,
	p Principal, key string, hash []byte) (bool, error) {
	result, err := exec(ctx, tx, query.Insert("kvmcloud_core.idempotency_keys").
		Columns("tenant_id", "principal_id", "operation", "key", "request_sha256").
		Values(p.TenantID.String(), p.ID.String(), opCreateVM, key, hash).
		Suffix("ON CONFLICT DO NOTHING"))
	inserted, err := changed(result, err)
	if err != nil || inserted {
		return false, err
	}
	// ON CONFLICT 会等待首个事务；READ COMMITTED 下本次 SELECT 能看到其已提交响应（含崩溃后）。
	var storedHash, response []byte
	var status sql.NullInt64
	var location sql.NullString
	var expiresAt time.Time
	err = row(ctx, tx, query.Select("request_sha256", "response_status", "response_body", "location", "expires_at").
		From("kvmcloud_core.idempotency_keys").Where(idempotencyWhere(p, opCreateVM, key)).Suffix("FOR UPDATE"),
		&storedHash, &status, &response, &location, &expiresAt)
	if err != nil {
		return false, err
	}
	if !bytes.Equal(hash, storedHash) {
		fail(c, http.StatusConflict, "idempotency_conflict")
		return true, nil
	}
	if !expiresAt.After(a.Now()) {
		fail(c, http.StatusConflict, "idempotency_key_expired")
		return true, nil
	}
	if !status.Valid || len(response) == 0 || !location.Valid {
		return false, errors.New("incomplete committed idempotency record")
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	c.Header("Location", location.String)
	c.Header("Idempotency-Replayed", "true")
	c.Data(int(status.Int64), "application/json", response)
	return true, nil
}

// reserveQuota 租户配额五维守卫扣减。
func reserveQuota(tenantID uuid.UUID, r CreateVMRequest) sq.UpdateBuilder {
	return query.Update("kvmcloud_core.tenant_quotas").
		Set("used_vms", sq.Expr("used_vms + 1")).
		Set("used_vcpus", sq.Expr("used_vcpus + ?", r.VCPUs)).
		Set("used_memory_mib", sq.Expr("used_memory_mib + ?", r.MemoryMiB)).
		Set("used_disk_gib", sq.Expr("used_disk_gib + ?", r.RootDiskGiB)).
		Set("used_min_kbps", sq.Expr("used_min_kbps + ?", r.Bandwidth.MinKbps)).
		Where(sq.Eq{"tenant_id": tenantID.String()}).
		Where("used_vms < max_vms").
		Where(sq.Expr("used_vcpus::numeric + ? <= max_vcpus", r.VCPUs)).
		Where(sq.Expr("used_memory_mib::numeric + ? <= max_memory_mib", r.MemoryMiB)).
		Where(sq.Expr("used_disk_gib::numeric + ? <= max_disk_gib", r.RootDiskGiB)).
		Where(sq.Expr("used_min_kbps::numeric + ? <= max_min_kbps", r.Bandwidth.MinKbps))
}

// nodeGuards 四维超开 + 在线心跳约束（SELECT 与 UPDATE 复用，预留与校验同语句）。
func nodeGuards(r CreateVMRequest) sq.And {
	return sq.And{
		sq.Eq{"state": "online", "schedulable": true},
		sq.Expr("last_seen_at > now() - interval '90 seconds'"),
		sq.Expr("used_vcpus::numeric + ? <= floor(physical_vcpus::numeric * cpu_ratio)", r.VCPUs),
		sq.Expr("used_memory_mib::numeric + ? <= floor(physical_memory_mib::numeric * memory_ratio)", r.MemoryMiB),
		sq.Expr("used_disk_gib::numeric + ? <= floor(physical_disk_gib::numeric * disk_ratio)", r.RootDiskGiB),
		sq.Expr("used_min_kbps::numeric + ? <= floor(physical_nic_kbps::numeric * bandwidth_ratio)", r.Bandwidth.MinKbps),
		sq.GtOrEq{"physical_nic_kbps": r.Bandwidth.MaxKbps},
	}
}

// candidateNode 集群内四维水位（CPU 占比）最低节点。
func candidateNode(clusterID string, r CreateVMRequest) sq.SelectBuilder {
	return query.Select("id").From("kvmcloud_core.nodes").
		Where(sq.Eq{"cluster_id": clusterID}).Where(nodeGuards(r)).
		OrderBy("used_vcpus::numeric / (physical_vcpus::numeric * cpu_ratio), id").
		Limit(1).Suffix("FOR UPDATE SKIP LOCKED")
}

func reserveNode(nodeID string, r CreateVMRequest) sq.UpdateBuilder {
	return query.Update("kvmcloud_core.nodes").
		Set("used_vcpus", sq.Expr("used_vcpus + ?", r.VCPUs)).
		Set("used_memory_mib", sq.Expr("used_memory_mib + ?", r.MemoryMiB)).
		Set("used_disk_gib", sq.Expr("used_disk_gib + ?", r.RootDiskGiB)).
		Set("used_min_kbps", sq.Expr("used_min_kbps + ?", r.Bandwidth.MinKbps)).
		Where(sq.Eq{"id": nodeID}).Where(nodeGuards(r))
}

// randomMAC 本地管理单播 MAC（52 前缀习惯位清除组播位）。
func randomMAC() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[0] = b[0]&0xfc | 0x02
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", b[0], b[1], b[2], b[3], b[4], b[5]), nil
}
