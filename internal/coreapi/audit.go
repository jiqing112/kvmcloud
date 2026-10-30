package coreapi

// 模块 12：审计查询（只读）。写入统一发生在各写路径事务内的 audit() 调用，
// audit_logs 为 append-only（触发器拒绝 UPDATE/DELETE/TRUNCATE，见 001_schema.sql）。
// 本 Handler 仅做 keyset 分页读取，绝不导出 safe_metadata 之外的字段。

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	sq "github.com/Masterminds/squirrel"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// ListAuditLogs GET /api/v1/audit-logs?limit=&before=
// before 为 RFC3339 时间戳（keyset 游标），limit 上限 200。
func (a *API) ListAuditLogs(c *gin.Context) {
	p, ok := principal(c, "audit:read", true)
	if !ok {
		return
	}
	limit := 50
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			fail(c, http.StatusBadRequest, "invalid_limit")
			return
		}
		limit = n
	}
	if limit > listHardLimit {
		limit = listHardLimit
	}
	b := query.Select("id", "actor_id", "request_id", "action", "resource_type",
		"resource_id", "outcome", "safe_metadata", "created_at").
		From("kvmcloud_core.audit_logs").
		Where(sq.Eq{"tenant_id": p.TenantID.String()}).
		OrderBy("created_at DESC, id DESC").Limit(uint64(limit))
	if raw := c.Query("before"); raw != "" {
		before, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			fail(c, http.StatusBadRequest, "invalid_before")
			return
		}
		b = b.Where(sq.Lt{"created_at": before})
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	tx, err := beginTenant(ctx, a.DB, p.TenantID)
	if err != nil {
		databaseError(c, err)
		return
	}
	defer tx.Rollback()
	sqlText, args, err := b.ToSql()
	if err != nil {
		databaseError(c, err)
		return
	}
	rows, err := tx.QueryContext(ctx, sqlText, args...)
	if err != nil {
		databaseError(c, err)
		return
	}
	defer rows.Close()
	out := make([]gin.H, 0, limit)
	for rows.Next() {
		var id, actorID, requestID, action, resourceType, resourceID, outcome string
		var metadata []byte
		var createdAt time.Time
		if err = rows.Scan(&id, &actorID, &requestID, &action, &resourceType,
			&resourceID, &outcome, &metadata, &createdAt); err != nil {
			databaseError(c, err)
			return
		}
		parsed, _ := uuid.Parse(id)
		item := gin.H{
			"id": parsed, "actor_id": actorID, "request_id": requestID,
			"action": action, "resource_type": resourceType, "resource_id": resourceID,
			"outcome": outcome, "created_at": createdAt,
		}
		var safe json.RawMessage
		if json.Unmarshal(metadata, &safe) == nil {
			item["metadata"] = safe
		}
		out = append(out, item)
	}
	if err = rows.Err(); err != nil {
		databaseError(c, err)
		return
	}
	if err = tx.Commit(); err != nil {
		databaseError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": out, "request_id": c.GetString(requestIDKey)})
}
