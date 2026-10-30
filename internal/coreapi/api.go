package coreapi

// 控制面 HTTP 框架：鉴权、租户事务、幂等、审计、错误脱敏。
// 安全基线（docs/api/core-design.md）：
//   - 身份只来自 Authenticate 钩子（OIDC/API Key 网关层），绝不信任请求体；
//   - 所有 SQL 经 squirrel 参数绑定；错误响应只含机器可读 code，不泄漏驱动文本；
//   - 事务内第一条语句绑定 app.tenant_id（RLS）+ lock/statement timeout。

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	sq "github.com/Masterminds/squirrel"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// Cache 抽象控制面用到的 Redis 命令；实现必须把 redis.Nil 映射为 ErrCacheMiss。
type Cache interface {
	Get(ctx context.Context, key string) (string, error)
	SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error)
	Eval(ctx context.Context, script string, keys []string, args ...any) (any, error)
}

// ErrCacheMiss 键不存在；errors.Is 判定。
var ErrCacheMiss = errors.New("cache miss")

// Principal 来自网关验签（OIDC / API Key SHA-256），禁止从请求体注入。
type Principal struct {
	ID       uuid.UUID
	TenantID uuid.UUID
	Scopes   map[string]bool
}

// API 租户面 HTTP 服务。DB 为 RLS 受限连接（kvmcloud_core_api），
// AdminDB 为接入签发专用连接（kvmcloud_core_enrollment）。
type API struct {
	DB              *sql.DB
	AdminDB         *sql.DB
	Redis           Cache
	Authenticate    func(ctx context.Context, r *http.Request) (Principal, error)
	InstallerURL    string
	InstallerSHA256 string
	Now             func() time.Time
	router          *gin.Engine
}

var query = sq.StatementBuilder.PlaceholderFormat(sq.Dollar)

// New 校验钉死的安装器配置并构建路由。
// 安装器 URL 会被内嵌进单引号 shell 命令并打印到终端：
// 必须 https、无 userinfo/query/fragment、路径与主机不含 shell 元字符。
func New(db, adminDB *sql.DB, cache Cache,
	authenticate func(context.Context, *http.Request) (Principal, error),
	installerURL, installerSHA256 string) (*API, error) {
	parsed, err := url.Parse(installerURL)
	if err != nil {
		return nil, fmt.Errorf("installer url: %w", err)
	}
	if parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" ||
		parsed.Path == "" || strings.ContainsAny(parsed.Host, "'\"`$&;<>\\") ||
		strings.ContainsAny(parsed.Path, "'\"`$&;<>\\") {
		return nil, errors.New("installer url must be https, without userinfo/query/fragment/shell metacharacters")
	}
	if !fingerprint.MatchString(installerSHA256) {
		return nil, errors.New("installer sha256 must be 64 hex characters")
	}
	if db == nil || adminDB == nil || cache == nil || authenticate == nil {
		return nil, errors.New("missing api dependencies")
	}
	a := &API{DB: db, AdminDB: adminDB, Redis: cache, Authenticate: authenticate,
		InstallerURL: installerURL, InstallerSHA256: installerSHA256,
		Now: func() time.Time { return time.Now().UTC() }}
	a.router = a.buildRouter()
	return a, nil
}

func (a *API) Router() *gin.Engine { return a.router }

func (a *API) buildRouter() *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery(), a.identity())
	v1 := r.Group("/api/v1")
	v1.GET("/tasks/:id", a.GetTask)
	v1.POST("/vms", a.CreateVM)
	v1.POST("/vms/adopt", a.AdoptVMs)
	v1.PUT("/vms/:id/bandwidth", a.SetVMBandwidth)
	v1.POST("/vms/:id/port-forwardings", a.CreatePortForwarding)
	v1.POST("/vms/:id/resize", a.ResizeVM)
	v1.POST("/vms/:id/migrate", a.MigrateVM)
	v1.DELETE("/vms/:id", a.DeleteVM)
	v1.PUT("/vms/:id/security-groups", a.SetVMSecurityGroups)
	v1.GET("/vms/:id/bandwidth-metrics", a.GetVMBandwidthMetrics)
	v1.POST("/volumes/:id/snapshots", a.CreateSnapshot)
	v1.GET("/nodes/:id/metrics", a.GetNodeMetrics)
	v1.GET("/tenants/:id/quotas", a.GetTenantQuotas)
	v1.POST("/tenants/:id/traffic-packages", a.BuyTrafficPackage)
	v1.GET("/dashboard/summary", a.GetDashboardSummary)
	v1.GET("/audit-logs", a.ListAuditLogs)
	v1.POST("/nodes/install-token", a.GenerateInstallToken)
	// 镜像源管理（模块 13）。
	v1.GET("/image-sources", a.ListImageSources)
	v1.POST("/image-sources", a.CreateImageSource)
	v1.PUT("/image-sources/:id", a.UpdateImageSource)
	v1.DELETE("/image-sources/:id", a.DeleteImageSource)
	v1.POST("/image-sources/:id/sync", a.SyncImageSources)
	v1.GET("/remote-images", a.ListRemoteImages)
	v1.POST("/local-images/pull", a.PullLocalImage)
	v1.GET("/local-images", a.ListLocalImages)
	return r
}

const (
	principalKey = "coreapi.principal"
	requestIDKey = "coreapi.request_id"
)

// identity 每请求鉴权一次，注入 Principal 与 request_id。
func (a *API) identity() gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := uuid.NewString()
		c.Set(requestIDKey, requestID)
		c.Header("X-Request-ID", requestID)
		p, err := a.Authenticate(c.Request.Context(), c.Request)
		if err != nil || p.ID == uuid.Nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized,
				gin.H{"error": gin.H{"code": "unauthorized", "request_id": requestID}})
			return
		}
		c.Set(principalKey, p)
		c.Next()
	}
}

// principal 返回当前主体并强制 scope / 租户归属。fail-closed。
func principal(c *gin.Context, scope string, requireTenant bool) (Principal, bool) {
	value, exists := c.Get(principalKey)
	p, valid := value.(Principal)
	if !exists || !valid || p.ID == uuid.Nil {
		fail(c, http.StatusUnauthorized, "unauthorized")
		return Principal{}, false
	}
	if scope != "" && !p.Scopes[scope] {
		fail(c, http.StatusForbidden, "forbidden")
		return Principal{}, false
	}
	if requireTenant && p.TenantID == uuid.Nil {
		fail(c, http.StatusForbidden, "forbidden")
		return Principal{}, false
	}
	return p, true
}

var jsonRequired = regexp.MustCompile(`^application/(hal\+)?json$`)

// strictJSON 拒绝未知字段与尾随 JSON；凭据形态字段（password/ssh_credentials/
// command 等）因 DisallowUnknownFields 直接 400。
func strictJSON(c *gin.Context, dst any) bool {
	if c.Request.Body == nil || !jsonRequired.MatchString(c.GetHeader("Content-Type")) {
		fail(c, http.StatusUnsupportedMediaType, "json_required")
		return false
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)
	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		fail(c, http.StatusBadRequest, "invalid_json")
		return false
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		fail(c, http.StatusBadRequest, "invalid_json")
		return false
	}
	return true
}

func fail(c *gin.Context, status int, code string) {
	c.JSON(status, gin.H{"error": gin.H{
		"code": code, "request_id": c.GetString(requestIDKey),
	}})
}

// databaseError 永不回传 SQL/驱动/宿主信息；唯一冲突单独成码。
func databaseError(c *gin.Context, err error) {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		fail(c, http.StatusConflict, "resource_conflict")
		return
	}
	fail(c, http.StatusServiceUnavailable, "transaction_retry")
}

// beginTenant 开启事务并绑定租户/超时；调用方 defer Rollback，成功路径显式 Commit。
func beginTenant(ctx context.Context, db *sql.DB, tenantID uuid.UUID) (*sql.Tx, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, err
	}
	sqlText, args, err := query.Select().
		Column(sq.Expr("set_config('app.tenant_id', ?, true)", tenantID.String())).
		Column(sq.Expr("set_config('lock_timeout', '2s', true)")).
		Column(sq.Expr("set_config('statement_timeout', '5s', true)")).ToSql()
	if err == nil {
		_, err = tx.ExecContext(ctx, sqlText, args...)
	}
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}

func row(ctx context.Context, tx *sql.Tx, b sq.Sqlizer, dst ...any) error {
	sqlText, args, err := b.ToSql()
	if err != nil {
		return err
	}
	return tx.QueryRowContext(ctx, sqlText, args...).Scan(dst...)
}

func exec(ctx context.Context, tx *sql.Tx, b sq.Sqlizer) (sql.Result, error) {
	sqlText, args, err := b.ToSql()
	if err != nil {
		return nil, err
	}
	return tx.ExecContext(ctx, sqlText, args...)
}

func changed(result sql.Result, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

// audit 追加审计（append-only，触发器拒绝 UPDATE/DELETE）。
// tenant 为 nil 表示平台级动作（接入签发）。
func audit(ctx context.Context, tx *sql.Tx, c *gin.Context, p Principal,
	tenant any, action, resourceType string, resourceID uuid.UUID) error {
	_, err := exec(ctx, tx, query.Insert("kvmcloud_core.audit_logs").
		Columns("id", "tenant_id", "actor_id", "request_id", "action",
			"resource_type", "resource_id", "outcome", "safe_metadata").
		Values(uuid.NewString(), tenant, p.ID.String(), c.GetString(requestIDKey),
			action, resourceType, resourceID.String(), "accepted", "{}"))
	return err
}

// digestJSON 返回规范化 JSON 与其 SHA-256（32 字节，与
// idempotency_keys.request_sha256 CHECK octet_length = 32 对齐）。
func digestJSON(v any) ([]byte, []byte, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, nil, err
	}
	sum := sha256.Sum256(body)
	return body, sum[:], nil
}

// idempotencyWhere 幂等作用域：租户 + 主体 + 操作 + 键。
func idempotencyWhere(p Principal, operation, key string) sq.Eq {
	return sq.Eq{"tenant_id": p.TenantID.String(), "principal_id": p.ID.String(),
		"operation": operation, "key": key}
}

// cacheIdempotentResponse 在提交前缓存成功响应（幂等重放源）。
// location 指向异步任务轮询地址（或 201 资源地址）。调用方随后 Commit 并输出 body。
func (a *API) cacheIdempotentResponse(ctx context.Context, tx *sql.Tx, p Principal,
	operation, key, location string, status int, v any) ([]byte, error) {
	body, _, err := digestJSON(v)
	if err != nil {
		return nil, err
	}
	_, err = exec(ctx, tx, query.Update("kvmcloud_core.idempotency_keys").
		Set("response_status", status).Set("response_body", string(body)).
		Set("location", location).Where(idempotencyWhere(p, operation, key)))
	return body, err
}
