package coreapi

// 模块 10：云盘与快照。API 仅记录元数据并创建 volume.snapshot 任务；
// Agent 执行 qemu-img snapshot / RBD snap create（参数化，白名单命令）。
// 同一卷同时只允许一个活跃快照任务（tasks_volume_mutation 唯一索引）。

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

type CreateSnapshotRequest struct {
	Name string `json:"name"` // 人类可读名称；物理 backend_ref 以快照 UUID 生成
}

// CreateSnapshot POST /api/v1/volumes/{id}/snapshots
func (a *API) CreateSnapshot(c *gin.Context) {
	p, ok := principal(c, "volumes:write", true)
	if !ok {
		return
	}
	volumeID, err := uuid.Parse(c.Param("id"))
	if err != nil || volumeID == uuid.Nil {
		fail(c, http.StatusBadRequest, "invalid_volume_id")
		return
	}
	key := c.GetHeader("Idempotency-Key")
	if !idempotencyKey.MatchString(key) {
		fail(c, http.StatusBadRequest, "idempotency_key_required")
		return
	}
	var request CreateSnapshotRequest
	if !strictJSON(c, &request) {
		return
	}
	if !vmName.MatchString(request.Name) {
		fail(c, http.StatusUnprocessableEntity, "invalid_snapshot_name")
		return
	}
	_, hash, err := digestJSON(request)
	if err != nil {
		fail(c, http.StatusBadRequest, "invalid_snapshot_name")
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
	operation := fmt.Sprintf("POST /api/v1/volumes/%s/snapshots", volumeID)
	replayed, err := a.claimIdempotency(ctx, tx, c, p, operation, key, hash)
	if err != nil {
		databaseError(c, err)
		return
	}
	if replayed {
		return
	}

	var nodeID, backend, volumeState string
	err = row(ctx, tx, query.Select("node_id", "backend", "state").
		From("kvmcloud_core.volumes").
		Where(sq.Eq{"id": volumeID.String(), "tenant_id": p.TenantID.String()}),
		&nodeID, &backend, &volumeState)
	if errors.Is(err, sql.ErrNoRows) {
		fail(c, http.StatusNotFound, "volume_not_found")
		return
	}
	if err != nil {
		databaseError(c, err)
		return
	}
	if volumeState != "available" && volumeState != "attached" {
		fail(c, http.StatusConflict, "volume_state_conflict")
		return
	}

	snapshotID, taskID, eventID := uuid.New(), uuid.New(), uuid.New()
	// backend_ref 由 Agent 按 snapshot UUID 生成（受卷后端约束），API 不接收路径。
	body, _, err := digestJSON(struct {
		EventID    uuid.UUID `json:"event_id"`
		TaskID     uuid.UUID `json:"task_id"`
		VolumeID   uuid.UUID `json:"volume_id"`
		SnapshotID uuid.UUID `json:"snapshot_id"`
		Backend    string    `json:"backend"` // qcow2 | rbd
	}{eventID, taskID, volumeID, snapshotID, backend})
	if err != nil {
		fail(c, http.StatusInternalServerError, "serialization_failed")
		return
	}
	writes := []sq.Sqlizer{
		query.Insert("kvmcloud_core.volume_snapshots").Columns(
			"id", "tenant_id", "volume_id", "name", "consistency", "state").
			Values(snapshotID.String(), p.TenantID.String(), volumeID.String(),
				request.Name, "crash", "creating"),
		query.Insert("kvmcloud_core.tasks").Columns("id", "tenant_id", "volume_id", "node_id", "kind").
			Values(taskID.String(), p.TenantID.String(), volumeID.String(), nodeID, "volume.snapshot"),
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
	if err = audit(ctx, tx, c, p, p.TenantID.String(), "volumes.snapshot", "volume_snapshot", snapshotID); err != nil {
		databaseError(c, err)
		return
	}
	response, err := a.cacheIdempotentResponse(ctx, tx, p, operation, key,
		"/api/v1/tasks/"+taskID.String(), http.StatusAccepted,
		gin.H{"volume_id": volumeID, "snapshot_id": snapshotID, "task_id": taskID, "state": "creating"})
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
