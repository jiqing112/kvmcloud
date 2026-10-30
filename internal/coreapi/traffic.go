package coreapi

// 模块 5：流量包与计费引擎（购买侧）。
// 购买：钱包扣减 + 订单/包裹入库同事务；Agent 定时上报 Egress（usage_reports），
// 计费引擎按 FIFO（traffic_packages_fifo 索引序）抵扣并记 traffic_debits/billing_ledger。
// 抵扣侧幂等由 usage_reports 唯一键 + traffic_debits(report_id,package_id) 唯一索引保证。

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

type BuyTrafficPackageRequest struct {
	ProductID uuid.UUID `json:"product_id"`
	Quantity  int       `json:"quantity"`
}

const (
	maxPackageQuantity = 1000
	minorUnitOverflow  = int64(1) << 62
)

// BuyTrafficPackage POST /api/v1/tenants/{id}/traffic-packages
func (a *API) BuyTrafficPackage(c *gin.Context) {
	p, ok := principal(c, "billing:write", true)
	if !ok {
		return
	}
	tenantID, err := uuid.Parse(c.Param("id"))
	if err != nil || tenantID == uuid.Nil || tenantID != p.TenantID {
		// 非本租户路径一律 404（跨租户代购走平台管理员通道，另行设计）。
		fail(c, http.StatusNotFound, "tenant_not_found")
		return
	}
	key := c.GetHeader("Idempotency-Key")
	if !idempotencyKey.MatchString(key) {
		fail(c, http.StatusBadRequest, "idempotency_key_required")
		return
	}
	var request BuyTrafficPackageRequest
	if !strictJSON(c, &request) {
		return
	}
	if request.ProductID == uuid.Nil || request.Quantity < 1 || request.Quantity > maxPackageQuantity {
		fail(c, http.StatusUnprocessableEntity, "invalid_purchase")
		return
	}
	_, hash, err := digestJSON(request)
	if err != nil {
		fail(c, http.StatusBadRequest, "invalid_purchase")
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
	operation := "POST /api/v1/tenants/traffic-packages"
	replayed, err := a.claimIdempotency(ctx, tx, c, p, operation, key, hash)
	if err != nil {
		databaseError(c, err)
		return
	}
	if replayed {
		return
	}

	// 商品快照：仅 active 商品可售；金额在 Go 侧校验溢出后在 SQL 侧复算。
	var totalBytes, priceMinor, validitySeconds string
	var currency string
	err = row(ctx, tx, query.Select("bytes::text", "price_minor::text",
		"validity_seconds::text", "currency").
		From("kvmcloud_core.traffic_products").
		Where(sq.Eq{"id": request.ProductID.String(), "active": true}), &totalBytes, &priceMinor,
		&validitySeconds, &currency)
	if errors.Is(err, sql.ErrNoRows) {
		fail(c, http.StatusNotFound, "product_not_found")
		return
	}
	if err != nil {
		databaseError(c, err)
		return
	}
	var price int64
	if _, err = fmt.Sscan(priceMinor, &price); err != nil || price < 0 {
		databaseError(c, err)
		return
	}
	if price > minorUnitOverflow/int64(request.Quantity) {
		fail(c, http.StatusUnprocessableEntity, "amount_overflow")
		return
	}
	amountMinor := price * int64(request.Quantity)

	// 钱包守卫扣减（余额不足 / 无该币种钱包 → 0 行 → 409）。
	result, err := exec(ctx, tx, query.Update("kvmcloud_core.tenant_wallets").
		Set("balance_minor", sq.Expr("balance_minor - ?", amountMinor)).
		Set("version", sq.Expr("version + 1")).
		Where(sq.Eq{"tenant_id": p.TenantID.String(), "currency": currency}).
		Where(sq.Expr("balance_minor >= ?", amountMinor)))
	reserved, err := changed(result, err)
	if err != nil {
		databaseError(c, err)
		return
	}
	if !reserved {
		fail(c, http.StatusConflict, "insufficient_balance")
		return
	}

	var validity int64
	if _, err = fmt.Sscan(validitySeconds, &validity); err != nil {
		databaseError(c, err)
		return
	}
	orderID, packageID := uuid.New(), uuid.New()
	expiresAt := a.Now().Add(time.Duration(validity) * time.Second)
	writes := []sq.Sqlizer{
		query.Insert("kvmcloud_core.traffic_orders").Columns(
			"id", "tenant_id", "product_id", "quantity", "amount_minor", "currency", "state").
			Values(orderID.String(), p.TenantID.String(), request.ProductID.String(),
				request.Quantity, amountMinor, currency, "paid"),
		// 包裹总量/余量在 SQL 侧乘法（numeric(20,0)，避免 int64 溢出）。
		query.Insert("kvmcloud_core.traffic_packages").Columns(
			"id", "tenant_id", "order_id", "total_bytes", "remaining_bytes", "expires_at").
			Values(packageID.String(), p.TenantID.String(), orderID.String(),
				sq.Expr("?::numeric * ?", totalBytes, request.Quantity),
				sq.Expr("?::numeric * ?", totalBytes, request.Quantity), expiresAt),
		query.Insert("kvmcloud_core.billing_ledger").Columns(
			"id", "tenant_id", "currency", "order_id", "amount_minor", "kind").
			Values(uuid.NewString(), p.TenantID.String(), currency, orderID.String(), amountMinor, "purchase"),
	}
	for _, write := range writes {
		if _, err = exec(ctx, tx, write); err != nil {
			databaseError(c, err)
			return
		}
	}
	if err = audit(ctx, tx, c, p, p.TenantID.String(), "traffic.purchase", "traffic_package", packageID); err != nil {
		databaseError(c, err)
		return
	}
	response, err := a.cacheIdempotentResponse(ctx, tx, p, operation, key,
		"/api/v1/tenants/"+tenantID.String()+"/traffic-packages/"+packageID.String(),
		http.StatusCreated, gin.H{
			"package_id": packageID, "order_id": orderID,
			"product_id": request.ProductID, "quantity": request.Quantity,
			"amount_minor": amountMinor, "currency": currency, "expires_at": expiresAt,
		})
	if err != nil {
		databaseError(c, err)
		return
	}
	if err = tx.Commit(); err != nil {
		databaseError(c, err)
		return
	}
	c.Header("Location", "/api/v1/tenants/"+tenantID.String()+"/traffic-packages/"+packageID.String())
	c.Data(http.StatusCreated, "application/json", response)
}
