package coreapi

// 镜像源管理 + 按需拉取（模块 13）。
//
// Handler 职责边界：参数校验 → principal/RLS → 元数据入库 → 任务 + outbox
// → 202。同步/下载/转换是异步任务，绝不在此发起 HTTP 抓取，也绝不接收任意
// URL 或文件路径——download_url 只由源同步 worker 写入 remote_images，
// storage_ref 只由镜像 worker 角色回报（列级授权隔离）。

import (
	"context"
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

const (
	imageSourceNameMax = 128
	sourceNameMax      = 64
	listHardLimit      = 500
)

var (
	// 镜像源规范名（image_sources.name，varchar(64)）。
	sourceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{1,63}$`)
	// 镜像规范名（remote_images.name / local_images.name，varchar(128)）。
	imageRefName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]+$`)
	// 源根地址：https + 主机 + 以 / 结尾的路径（目录）。
	baseURL = regexp.MustCompile(`^https://[A-Za-z0-9.:\[\]-]+(/[A-Za-z0-9/_.-]*)$`)
	// 下载域名白名单条目。
	domainLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)
)

type CreateImageSourceRequest struct {
	Name             string   `json:"name"`
	Kind             string   `json:"kind"` // domestic | overseas | custom
	BaseURL          string   `json:"base_url"`
	DomainAllowlist  []string `json:"domain_allowlist"`
	EgressBytesLimit int64    `json:"egress_bytes_limit"`
	Priority         int      `json:"priority"`
}

func (r CreateImageSourceRequest) valid() bool {
	if !sourceName.MatchString(r.Name) || !baseURL.MatchString(r.BaseURL) {
		return false
	}
	if r.Kind != "domestic" && r.Kind != "overseas" && r.Kind != "custom" {
		return false
	}
	if r.Priority < 0 || r.Priority > 1000 {
		return false
	}
	if r.EgressBytesLimit < 1 || r.EgressBytesLimit > 1<<50 {
		return false
	}
	// custom 源必须显式声明可下载域名（与 DDL CHECK 一致，空数组在 SQL 侧同样拒绝）。
	if r.Kind == "custom" && len(r.DomainAllowlist) == 0 {
		return false
	}
	for _, d := range r.DomainAllowlist {
		if !domainLabel.MatchString(d) {
			return false
		}
	}
	return true
}

type UpdateImageSourceRequest struct {
	Priority        *int     `json:"priority"`
	State           *string  `json:"state"` // active | disabled
	DomainAllowlist []string `json:"domain_allowlist"`
}

type PullLocalImageRequest struct {
	RemoteImageID uuid.UUID `json:"remote_image_id"`
	OSType        string    `json:"os_type"` // linux | windows（Cloud-Init / virtio 注入路径分叉）
	Platform      bool      `json:"platform"` // true = 产出平台公共镜像（仅平台管理员）
}

// handlerError 让 mutate 闭包内的业务错误以预期 HTTP 码/错误码呈现。
type handlerError struct {
	code   string
	status int
}

func (e handlerError) Error() string { return e.code }

func errNotFound(code string) error { return handlerError{code: code, status: http.StatusNotFound} }

// mutateTenantRow 开租户事务执行写路径：handlerError 精确映射，其余走统一
// 脱敏兜底；成功提交。返回 false 表示响应已写出，Handler 直接 return。
func (a *API) mutateTenantRow(c *gin.Context, p Principal, mutate func(tx *sql.Tx) error) bool {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 8*time.Second)
	defer cancel()
	tx, err := beginTenant(ctx, a.DB, p.TenantID)
	if err != nil {
		databaseError(c, err)
		return false
	}
	defer tx.Rollback()
	if err = mutate(tx); err != nil {
		var he handlerError
		if errors.As(err, &he) {
			fail(c, he.status, he.code)
			return false
		}
		databaseError(c, err)
		return false
	}
	if err = tx.Commit(); err != nil {
		databaseError(c, err)
		return false
	}
	return true
}

func created(c *gin.Context, id string) {
	c.Header("Location", "/api/v1/image-sources/"+id)
	c.JSON(http.StatusCreated, gin.H{"id": id, "request_id": c.GetString(requestIDKey)})
}

func acceptedTask(c *gin.Context, taskID uuid.UUID) {
	c.Header("Location", "/api/v1/tasks/"+taskID.String())
	c.JSON(http.StatusAccepted, gin.H{
		"task_id": taskID, "state": "pending", "request_id": c.GetString(requestIDKey),
	})
}

// CreateImageSource POST /api/v1/image-sources（租户 custom 源；平台源由运维种子）。
func (a *API) CreateImageSource(c *gin.Context) {
	p, ok := principal(c, "images:write", false)
	if !ok {
		return
	}
	var request CreateImageSourceRequest
	if !strictJSON(c, &request) {
		return
	}
	if !request.valid() {
		fail(c, http.StatusUnprocessableEntity, "invalid_image_source")
		return
	}
	sourceID := uuid.NewString()
	if !a.mutateTenantRow(c, p, func(tx *sql.Tx) error {
		var tenant any
		if p.TenantID != uuid.Nil {
			tenant = p.TenantID.String()
		}
		_, err := exec(c.Request.Context(), tx, query.Insert("kvmcloud_core.image_sources").
			Columns("id", "tenant_id", "name", "kind", "base_url",
				"egress_bytes_limit", "domain_allowlist", "priority", "state").
			Values(sourceID, tenant, request.Name, request.Kind, request.BaseURL,
				request.EgressBytesLimit, normalizeDomains(request.DomainAllowlist),
				request.Priority, "active"))
		return err
	}) {
		return
	}
	created(c, sourceID)
}

// ListImageSources GET /api/v1/image-sources?kind=&state=
// 可见范围：平台源（tenant_id IS NULL）+ 本租户源；不含 base_url 与 allowlist。
func (a *API) ListImageSources(c *gin.Context) {
	p, ok := principal(c, "images:read", false)
	if !ok {
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
	sqlText, args, err := listImageSourcesSQL(p.TenantID, c.Query("kind"), c.Query("state"))
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
	count := 0
	out := make([]gin.H, 0, 16)
	for rows.Next() {
		if count++; count > 200 {
			fail(c, http.StatusBadRequest, "too_many_results")
			return
		}
		var id, name, kind, state string
		var priority int
		var lastSynced sql.NullTime
		if err := rows.Scan(&id, &name, &kind, &state, &priority, &lastSynced); err != nil {
			databaseError(c, err)
			return
		}
		item := gin.H{"id": id, "name": name, "kind": kind, "state": state, "priority": priority}
		if lastSynced.Valid {
			item["last_synced_at"] = lastSynced.Time
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		databaseError(c, err)
		return
	}
	if err = tx.Commit(); err != nil {
		databaseError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": out, "request_id": c.GetString(requestIDKey)})
}

// UpdateImageSource PUT /api/v1/image-sources/{id}（仅本租户源；平台源由运维管）。
func (a *API) UpdateImageSource(c *gin.Context) {
	p, ok := principal(c, "images:write", false)
	if !ok {
		return
	}
	sourceID, err := uuid.Parse(c.Param("id"))
	if err != nil || sourceID == uuid.Nil {
		fail(c, http.StatusBadRequest, "invalid_source_id")
		return
	}
	var request UpdateImageSourceRequest
	if !strictJSON(c, &request) {
		return
	}
	if request.State != nil && *request.State != "active" && *request.State != "disabled" {
		fail(c, http.StatusUnprocessableEntity, "invalid_state")
		return
	}
	if request.Priority != nil && (*request.Priority < 0 || *request.Priority > 1000) {
		fail(c, http.StatusUnprocessableEntity, "invalid_priority")
		return
	}
	var domains any
	if request.DomainAllowlist != nil {
		domains = normalizeDomains(request.DomainAllowlist)
	}
	var updated bool
	if !a.mutateTenantRow(c, p, func(tx *sql.Tx) error {
		result, err := exec(c.Request.Context(), tx, query.Update("kvmcloud_core.image_sources").
			Set("priority", sq.Expr("COALESCE(?, priority)", nullInt(request.Priority))).
			Set("state", sq.Expr("COALESCE(?, state)", nullStr(request.State))).
			Set("domain_allowlist", sq.Expr("COALESCE(?, domain_allowlist)", domains)).
			// RLS 限定本租户；平台级源对租户不可更新（0 行 → 404，不泄露存在性）。
			Where(sq.Eq{"id": sourceID.String()}))
		updated, err = changed(result, err)
		return err
	}) {
		return
	}
	if !updated {
		fail(c, http.StatusNotFound, "image_source_not_found")
		return
	}
	c.Status(http.StatusNoContent)
}

// SyncImageSources POST /api/v1/image-sources/{id}/sync
// 入队 image.source.sync 任务 + outbox 事件；目录抓取与 upsert 由同步 worker 执行。
func (a *API) SyncImageSources(c *gin.Context) {
	p, ok := principal(c, "images:write", false)
	if !ok {
		return
	}
	sourceID, err := uuid.Parse(c.Param("id"))
	if err != nil || sourceID == uuid.Nil {
		fail(c, http.StatusBadRequest, "invalid_source_id")
		return
	}
	taskID := uuid.New()
	if !a.mutateTenantRow(c, p, func(tx *sql.Tx) error {
		// 源必须对调用者可见，否则 404（不泄露存在性）。
		var id string
		if err := row(c.Request.Context(), tx, query.Select("id").
			From("kvmcloud_core.image_sources").
			Where(sq.Eq{"id": sourceID.String()}).
			Where("tenant_id IS NULL OR tenant_id = ?", platformTenant(p)), &id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errNotFound("image_source_not_found")
			}
			return err
		}
		if _, err := exec(c.Request.Context(), tx, query.Insert("kvmcloud_core.tasks").
			Columns("id", "tenant_id", "node_id", "kind", "safe_result").
			Values(taskID.String(), platformTenant(p), controlNodeID(), "image.source.sync",
				fmt.Sprintf(`{"image_source_id":%q}`, sourceID.String()))); err != nil {
			return err
		}
		// 控制面 worker 订阅自己的 subject；node_id 为控制面占位节点（部署时种子）。
		_, err := exec(c.Request.Context(), tx, query.Insert("kvmcloud_core.outbox_events").
			Columns("id", "tenant_id", "task_id", "node_id", "subject", "payload").
			Values(uuid.NewString(), platformTenant(p), taskID.String(), controlNodeID(),
				"kvmcloud.commands."+controlNodeID(),
				fmt.Sprintf(`{"task_id":%q,"source_id":%q}`, taskID.String(), sourceID.String())))
		return err
	}) {
		return
	}
	acceptedTask(c, taskID)
}

// ListRemoteImages GET /api/v1/remote-images?source_id=&name=&arch=&format=
func (a *API) ListRemoteImages(c *gin.Context) {
	p, ok := principal(c, "images:read", false)
	if !ok {
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
	sqlText, args, err := listRemoteImagesSQL(p.TenantID,
		c.Query("source_id"), c.Query("name"), c.Query("arch"), c.Query("format"))
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
	count := 0
	out := make([]gin.H, 0, 32)
	for rows.Next() {
		if count++; count > listHardLimit {
			fail(c, http.StatusBadRequest, "too_many_results")
			return
		}
		var id, name, version, arch, format string
		var size sql.NullInt64
		var sha sql.NullString
		if err := rows.Scan(&id, &name, &version, &arch, &format, &size, &sha); err != nil {
			databaseError(c, err)
			return
		}
		item := gin.H{"id": id, "name": name, "version": version, "arch": arch, "format": format}
		if size.Valid {
			item["size_bytes"] = size.Int64
		}
		if sha.Valid {
			item["sha256"] = sha.String
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		databaseError(c, err)
		return
	}
	if err = tx.Commit(); err != nil {
		databaseError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": out, "request_id": c.GetString(requestIDKey)})
}

// PullLocalImage POST /api/v1/local-images/pull
// 按远端镜像发起本地拉取；同名活跃拉取幂等返回既有任务。
func (a *API) PullLocalImage(c *gin.Context) {
	p, ok := principal(c, "images:write", false)
	if !ok {
		return
	}
	var request PullLocalImageRequest
	if !strictJSON(c, &request) {
		return
	}
	if request.RemoteImageID == uuid.Nil ||
		(request.OSType != "linux" && request.OSType != "windows") {
		fail(c, http.StatusUnprocessableEntity, "invalid_pull_request")
		return
	}
	taskID := uuid.New()
	if !a.mutateTenantRow(c, p, func(tx *sql.Tx) (err error) {
		tenantID := p.TenantID.String()
		if request.Platform {
			tenantID = "" // 平台公共镜像：tenant_id NULL（仅平台管理员 scope 可达）
		}
		// 远端镜像必须对调用者可见（平台源或本租户源），否则 404 不泄露存在性。
		var remoteID, sourceID, name string
		err = row(c.Request.Context(), tx, query.Select("r.id", "r.source_id", "r.name").
			From("kvmcloud_core.remote_images r").
			Join("kvmcloud_core.image_sources s ON s.id = r.source_id").
			Where(sq.Eq{"r.id": request.RemoteImageID.String()}).
			Where("s.tenant_id IS NULL OR s.tenant_id = ?", platformTenant(p)),
			&remoteID, &sourceID, &name)
		if errors.Is(err, sql.ErrNoRows) {
			return errNotFound("remote_image_not_found")
		}
		if err != nil {
			return err
		}
		// 同名活跃拉取幂等：活跃唯一索引挡并发，这里返回既有记录。
		var existing string
		err = row(c.Request.Context(), tx, query.Select("id").
			From("kvmcloud_core.local_images").
			Where(sq.Eq{"name": name}).
			Where("tenant_id IS NOT DISTINCT FROM ?", nullableText(tenantID)).
			Where("pull_state NOT IN ('failed','retired')"), &existing)
		if err == nil {
			parsed, perr := uuid.Parse(existing)
			if perr != nil {
				return perr
			}
			taskID = parsed
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err = exec(c.Request.Context(), tx, query.Insert("kvmcloud_core.local_images").
			Columns("id", "tenant_id", "source_id", "remote_image_id", "name", "os_type", "pull_state").
			Values(uuid.NewString(), nullableText(tenantID), sourceID, remoteID,
				name, request.OSType, "pending")); err != nil {
			return err
		}
		_, err = exec(c.Request.Context(), tx, query.Insert("kvmcloud_core.tasks").
			Columns("id", "tenant_id", "node_id", "kind", "safe_result").
			Values(taskID.String(), platformTenant(p), controlNodeID(), "image.pull",
				fmt.Sprintf(`{"local_image_name":%q,"remote_image_id":%q}`, name, remoteID)))
		return err
	}) {
		return
	}
	acceptedTask(c, taskID)
}

// ListLocalImages GET /api/v1/local-images?state=（不含 storage_ref 本地路径）。
func (a *API) ListLocalImages(c *gin.Context) {
	p, ok := principal(c, "images:read", false)
	if !ok {
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
	sqlText, args, err := listLocalImagesSQL(p.TenantID, c.Query("state"))
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
	count := 0
	out := make([]gin.H, 0, 32)
	for rows.Next() {
		if count++; count > listHardLimit {
			fail(c, http.StatusBadRequest, "too_many_results")
			return
		}
		var id, name, osType, pullState string
		var size sql.NullInt64
		if err := rows.Scan(&id, &name, &osType, &pullState, &size); err != nil {
			databaseError(c, err)
			return
		}
		item := gin.H{"id": id, "name": name, "os_type": osType, "pull_state": pullState}
		if size.Valid {
			item["size_bytes"] = size.Int64
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		databaseError(c, err)
		return
	}
	if err = tx.Commit(); err != nil {
		databaseError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": out, "request_id": c.GetString(requestIDKey)})
}

// ---------------- SQL 组装（全部参数绑定） -------------------------------------

func listImageSourcesSQL(tenant uuid.UUID, kind, state string) (string, []any, error) {
	b := query.Select("id", "name", "kind", "state", "priority", "last_synced_at").
		From("kvmcloud_core.image_sources").
		Where("tenant_id IS NULL OR tenant_id = ?", tenant.String())
	if kind != "" {
		b = b.Where(sq.Eq{"kind": kind})
	}
	if state != "" {
		b = b.Where(sq.Eq{"state": state})
	}
	b = b.OrderBy("priority", "name").Limit(200)
	return b.ToSql()
}

func listRemoteImagesSQL(tenant uuid.UUID, sourceID, name, arch, format string) (string, []any, error) {
	b := query.Select("r.id", "r.name", "r.version", "r.arch", "r.format", "r.size_bytes", "r.sha256").
		From("kvmcloud_core.remote_images r").
		Join("kvmcloud_core.image_sources s ON s.id = r.source_id").
		Where("s.tenant_id IS NULL OR s.tenant_id = ?", tenant.String())
	if sourceID != "" {
		b = b.Where(sq.Eq{"s.id": sourceID})
	}
	if name != "" {
		b = b.Where(sq.Like{"r.name": name + "%"})
	}
	if arch != "" {
		b = b.Where(sq.Eq{"r.arch": arch})
	}
	if format != "" {
		b = b.Where(sq.Eq{"r.format": format})
	}
	b = b.OrderBy("r.name", "r.version").Limit(listHardLimit)
	return b.ToSql()
}

func listLocalImagesSQL(tenant uuid.UUID, state string) (string, []any, error) {
	b := query.Select("id", "name", "os_type", "pull_state", "size_bytes").
		From("kvmcloud_core.local_images").
		Where("tenant_id IS NULL OR tenant_id = ?", tenant.String())
	if state != "" {
		b = b.Where(sq.Eq{"pull_state": state})
	}
	b = b.OrderBy("id").Limit(listHardLimit)
	return b.ToSql()
}

// ---------------- 小工具 -------------------------------------------------------

// platformTenant 平台级操作占位租户（部署时以真实平台租户替换）。
func platformTenant(p Principal) string {
	if p.TenantID == uuid.Nil {
		return "00000000-0000-0000-0000-000000000000"
	}
	return p.TenantID.String()
}

// controlNodePlaceholder 控制面占位节点：镜像同步任务无数据面节点，
// 部署时须种子该节点行以满足 tasks.node_id 外键。
const controlNodePlaceholder = "00000000-0000-0000-0000-000000000001"

func controlNodeID() string { return controlNodePlaceholder }

func nullableText(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func nullable(ns sql.NullString) any {
	if ns.Valid {
		return ns.String
	}
	return nil
}

func nullInt(i *int) any {
	if i == nil {
		return nil
	}
	return *i
}

func nullStr(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// normalizeDomains 域名数组 → postgres 数组字面量；空切片 → NULL（COALESCE 保留旧值）。
func normalizeDomains(domains []string) any {
	clean := make([]string, 0, len(domains))
	for _, d := range domains {
		if domainLabel.MatchString(d) {
			clean = append(clean, d)
		}
	}
	if len(clean) == 0 {
		return nil
	}
	return "{" + joinQuoted(clean) + "}"
}

func joinQuoted(items []string) string {
	out := ""
	for i, item := range items {
		if i > 0 {
			out += ","
		}
		out += item
	}
	return out
}
