package coreapi

// 任务轮询出口：所有异步操作（202 + task_id）的状态查询端点（GET /api/v1/tasks/{id}）。
// 只读：不触发底层操作；safe_result 由写入侧保证仅含声明式元数据（无凭证/本地路径）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	sq "github.com/Masterminds/squirrel"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// TaskView 任务只读视图（与 tasks 状态机对齐：pending/running/succeeded/failed/unknown/cancelled）。
type TaskView struct {
	TaskID    uuid.UUID       `json:"task_id"`
	Kind      string          `json:"kind"`
	State     string          `json:"state"`
	Progress  int             `json:"progress"`
	ErrorCode string          `json:"error_code,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// GetTask GET /api/v1/tasks/{id}：租户内可见，跨租户与其他不存在同样 404。
func (a *API) GetTask(c *gin.Context) {
	p, ok := principal(c, "", true)
	if !ok {
		return
	}
	taskID, err := uuid.Parse(c.Param("id"))
	if err != nil || taskID == uuid.Nil {
		fail(c, http.StatusBadRequest, "invalid_task_id")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	tx, err := beginTenant(ctx, a.DB, p.TenantID)
	if err != nil {
		databaseError(c, err)
		return
	}
	defer tx.Rollback()
	var v TaskView
	var errorCode sql.NullString
	var result []byte
	err = row(ctx, tx, query.Select("id", "kind", "state", "progress", "error_code",
		"safe_result", "created_at", "updated_at").
		From("kvmcloud_core.tasks").
		Where(sq.Eq{"id": taskID.String(), "tenant_id": p.TenantID.String()}),
		&v.TaskID, &v.Kind, &v.State, &v.Progress, &errorCode, &result, &v.CreatedAt, &v.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		fail(c, http.StatusNotFound, "task_not_found")
		return
	}
	if err != nil {
		databaseError(c, err)
		return
	}
	if err = tx.Commit(); err != nil {
		databaseError(c, err)
		return
	}
	v.ErrorCode = errorCode.String
	v.Result = result
	c.JSON(http.StatusOK, v)
}
