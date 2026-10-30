package coreapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"regexp"
	"time"

	sq "github.com/Masterminds/squirrel"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type BandwidthSample struct {
	At          time.Time `json:"at"`
	IngressKbps float64   `json:"ingress_kbps"`
	EgressKbps  float64   `json:"egress_kbps"`
	EgressBytes string    `json:"egress_bytes"` // Decimal string: safe for JavaScript.
}

type BandwidthMetrics struct {
	CollectedAt time.Time         `json:"collected_at"`
	Samples     []BandwidthSample `json:"samples"`
	Stale       bool              `json:"stale"`
}

func metricsKey(tenantID, vmID uuid.UUID) string {
	return "kvmcloud:metrics:v1:" + tenantID.String() + ":" + vmID.String() + ":bandwidth"
}

var unsignedDecimal = regexp.MustCompile(`^(0|[1-9][0-9]{0,19})$`)

func (m BandwidthMetrics) valid(now time.Time) bool {
	if m.CollectedAt.IsZero() || m.CollectedAt.After(now.Add(5*time.Second)) ||
		len(m.Samples) == 0 || len(m.Samples) > 720 {
		return false
	}
	var previous time.Time
	for _, sample := range m.Samples {
		if sample.At.IsZero() || sample.At.After(m.CollectedAt) ||
			!previous.IsZero() && !sample.At.After(previous) ||
			sample.IngressKbps < 0 || sample.EgressKbps < 0 ||
			math.IsNaN(sample.IngressKbps) || math.IsNaN(sample.EgressKbps) ||
			math.IsInf(sample.IngressKbps, 0) || math.IsInf(sample.EgressKbps, 0) ||
			!unsignedDecimal.MatchString(sample.EgressBytes) {
			return false
		}
		previous = sample.At
	}
	return true
}

// GetVMBandwidthMetrics performs authorization SELECTs and Redis GET only.
// It does not contact libvirt, Agent, NATS, TC, nftables or a shell.
func (a *API) GetVMBandwidthMetrics(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	p, ok := principal(c, "vms:read", true)
	if !ok {
		return
	}
	vmID, err := uuid.Parse(c.Param("id"))
	if err != nil || vmID == uuid.Nil {
		fail(c, http.StatusBadRequest, "invalid_vm_id")
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
	var ownedID string
	err = row(ctx, tx, query.Select("id").From("kvmcloud_core.vms").
		Where(sq.Eq{"id": vmID.String(), "tenant_id": p.TenantID.String()}).
		Where(sq.NotEq{"state": "deleted"}), &ownedID)
	if errors.Is(err, sql.ErrNoRows) {
		// Identical response for nonexistent and other-tenant VMs.
		fail(c, http.StatusNotFound, "vm_not_found")
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
	cacheCtx, cacheCancel := context.WithTimeout(ctx, 750*time.Millisecond)
	defer cacheCancel()
	raw, err := a.Redis.Get(cacheCtx, metricsKey(p.TenantID, vmID))
	if err != nil {
		c.Header("Retry-After", "5")
		fail(c, http.StatusServiceUnavailable, "metrics_unavailable")
		return
	}
	var metrics BandwidthMetrics
	now := a.Now()
	if len(raw) > 256<<10 || json.Unmarshal([]byte(raw), &metrics) != nil || !metrics.valid(now) {
		fail(c, http.StatusBadGateway, "invalid_metrics")
		return
	}
	metrics.Stale = now.Sub(metrics.CollectedAt) > 30*time.Second
	c.JSON(http.StatusOK, metrics)
}
