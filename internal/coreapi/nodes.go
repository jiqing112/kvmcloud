package coreapi

// 模块 3：资源占用与状态查询（只读）。
// 数据源：nodes 心跳列（DB）与 Redis 聚合指标（与 GetVMBandwidthMetrics 同源），
// 绝不触发 libvirt / Agent / NATS / shell 操作。nodes 为平台级资源（无租户 RLS），
// 仍强制 nodes:read scope。

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	sq "github.com/Masterminds/squirrel"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// nodeHeartbeatTTL 与调度器一致的心跳新鲜度阈值。
const nodeHeartbeatTTL = 90 * time.Second

// GetNodeMetrics GET /api/v1/nodes/{id}/metrics
func (a *API) GetNodeMetrics(c *gin.Context) {
	p, ok := principal(c, "nodes:read", true)
	if !ok {
		return
	}
	nodeID, err := uuid.Parse(c.Param("id"))
	if err != nil || nodeID == uuid.Nil {
		fail(c, http.StatusBadRequest, "invalid_node_id")
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
	var hostname, state string
	var schedulable bool
	var physVCPUs, physMem, physDisk, physNic int64
	var cpuRatio, memRatio, diskRatio, bwRatio float64
	var usedVCPUs, usedMem, usedDisk, usedMinKbps int64
	var lastSeen time.Time
	err = row(ctx, tx, query.Select(
		"hostname", "state", "schedulable",
		"physical_vcpus", "physical_memory_mib", "physical_disk_gib", "physical_nic_kbps",
		"cpu_ratio", "memory_ratio", "disk_ratio", "bandwidth_ratio",
		"used_vcpus", "used_memory_mib", "used_disk_gib", "used_min_kbps", "last_seen_at").
		From("kvmcloud_core.nodes").Where(sq.Eq{"id": nodeID.String()}),
		&hostname, &state, &schedulable,
		&physVCPUs, &physMem, &physDisk, &physNic,
		&cpuRatio, &memRatio, &diskRatio, &bwRatio,
		&usedVCPUs, &usedMem, &usedDisk, &usedMinKbps, &lastSeen)
	if errors.Is(err, sql.ErrNoRows) {
		fail(c, http.StatusNotFound, "node_not_found")
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
	util := func(used, physical int64, ratio float64) float64 {
		denom := float64(physical) * ratio
		if denom <= 0 {
			return 0
		}
		return float64(used) / denom
	}
	c.JSON(http.StatusOK, gin.H{
		"node_id": nodeID, "hostname": hostname, "state": state, "schedulable": schedulable,
		"physical": gin.H{"vcpus": physVCPUs, "memory_mib": physMem,
			"disk_gib": physDisk, "nic_kbps": physNic},
		"overcommit": gin.H{"cpu": cpuRatio, "memory": memRatio,
			"disk": diskRatio, "bandwidth": bwRatio},
		"allocated": gin.H{"vcpus": usedVCPUs, "memory_mib": usedMem,
			"disk_gib": usedDisk, "min_kbps": usedMinKbps},
		"utilization": gin.H{
			"cpu":       util(usedVCPUs, physVCPUs, cpuRatio),
			"memory":    util(usedMem, physMem, memRatio),
			"disk":      util(usedDisk, physDisk, diskRatio),
			"bandwidth": util(usedMinKbps, physNic, bwRatio),
		},
		"last_seen_at": lastSeen,
		// 心跳超时由 Agent gRPC 心跳维持；此处仅标注新鲜度，不主动探测。
		"stale": a.Now().Sub(lastSeen) > nodeHeartbeatTTL,
	})
}

// GetTenantQuotas GET /api/v1/tenants/{id}/quotas：仅本租户（其余一律 404，不泄露存在性）。
func (a *API) GetTenantQuotas(c *gin.Context) {
	p, ok := principal(c, "tenants:read", true)
	if !ok {
		return
	}
	tenantID, err := uuid.Parse(c.Param("id"))
	if err != nil || tenantID == uuid.Nil || tenantID != p.TenantID {
		fail(c, http.StatusNotFound, "tenant_not_found")
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
	var mVms, mVcpus, mMem, mDisk, mKbps int64
	var uVms, uVcpus, uMem, uDisk, uKbps int64
	err = row(ctx, tx, query.Select(
		"max_vms", "max_vcpus", "max_memory_mib", "max_disk_gib", "max_min_kbps",
		"used_vms", "used_vcpus", "used_memory_mib", "used_disk_gib", "used_min_kbps").
		From("kvmcloud_core.tenant_quotas").Where(sq.Eq{"tenant_id": p.TenantID.String()}),
		&mVms, &mVcpus, &mMem, &mDisk, &mKbps, &uVms, &uVcpus, &uMem, &uDisk, &uKbps)
	if errors.Is(err, sql.ErrNoRows) {
		fail(c, http.StatusNotFound, "quota_not_found")
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
	c.JSON(http.StatusOK, gin.H{
		"tenant_id": tenantID,
		"quota": gin.H{
			"vms": gin.H{"max": mVms, "used": uVms},
			"vcpus": gin.H{"max": mVcpus, "used": uVcpus},
			"memory_mib": gin.H{"max": mMem, "used": uMem},
			"disk_gib": gin.H{"max": mDisk, "used": uDisk},
			"min_kbps": gin.H{"max": mKbps, "used": uKbps},
		},
	})
}

// GetDashboardSummary GET /api/v1/dashboard/summary：单连接聚合，不展开明细行。
func (a *API) GetDashboardSummary(c *gin.Context) {
	p, ok := principal(c, "dashboard:read", true)
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
	var activeVms, totalVms, errorVms int64
	err = row(ctx, tx, query.Select().From("kvmcloud_core.vms").
		Where(sq.Eq{"tenant_id": p.TenantID.String()}).
		Column(sq.Expr("count(*) FILTER (WHERE state IN ('running','creating','resizing','migrating','adopting'))")).
		Column(sq.Expr("count(*) FILTER (WHERE state NOT IN ('deleted','recycled'))")).
		Column(sq.Expr("count(*) FILTER (WHERE state = 'error')")),
		&activeVms, &totalVms, &errorVms)
	if err != nil {
		databaseError(c, err)
		return
	}
	var onlineNodes int64
	err = row(ctx, tx, query.Select().From("kvmcloud_core.nodes").
		Column(sq.Expr("count(*) FILTER (WHERE state = 'online' AND schedulable)")), &onlineNodes)
	if err != nil {
		databaseError(c, err)
		return
	}
	var remainingBytes string
	err = row(ctx, tx, query.Select().From("kvmcloud_core.traffic_packages").
		Where(sq.Eq{"tenant_id": p.TenantID.String()}).Where("expires_at > now()").
		Column(sq.Expr("COALESCE(sum(remaining_bytes), 0)::text")), &remainingBytes)
	if err != nil {
		databaseError(c, err)
		return
	}
	if err = tx.Commit(); err != nil {
		databaseError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"vms":          gin.H{"active": activeVms, "total": totalVms, "error": errorVms},
		"nodes_online": onlineNodes,
		"traffic":      gin.H{"remaining_bytes": remainingBytes},
	})
}
