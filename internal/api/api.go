// Package api 控制面 REST API（Gin）。
// 路由：节点、VM 放置（四维超开校验）、带宽策略、共享池、借用仲裁、告警、事件。
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kvm-cloud/platform/internal/model"
	"github.com/kvm-cloud/platform/internal/monitor"
	"github.com/kvm-cloud/platform/internal/store"
)

// Server HTTP API 服务。
type Server struct {
	st      *store.Store
	mon     *monitor.Monitor
	log     *slog.Logger
	version string
}

// New 构造。
func New(st *store.Store, mon *monitor.Monitor, log *slog.Logger, version string) *Server {
	return &Server{st: st, mon: mon, log: log, version: version}
}

// Router 组装路由。
func (s *Server) Router() *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery(), requestLogger(s.log))

	r.GET("/healthz", func(c *gin.Context) {
		if err := s.st.Ping(c.Request.Context()); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "db_unreachable", "error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok", "version": s.version, "time": time.Now().UTC()})
	})

	api := r.Group("/api/v1")
	{
		// 节点
		api.GET("/nodes", s.listNodes)
		api.GET("/nodes/:id", s.getNode)
		api.POST("/nodes", s.createNode)
		api.PUT("/nodes/:id/ratios", s.setNodeRatios) // 超开比调优
		api.GET("/nodes/:id/watermarks", s.nodeWatermarks)

		// VM（放置 = 四维超开校验）
		api.POST("/vms", s.placeVM)
		api.GET("/vms", s.listVMs)
		api.GET("/vms/:id", s.getVM)
		api.POST("/vms/:id/status", s.setVMStatus)

		// 共享带宽池
		api.POST("/pools", s.createPool)
		api.GET("/pools", s.listPools)
		api.GET("/pools/:id", s.getPool)

		// 端口带宽策略（三场景之一：创建时保障总量校验）
		api.POST("/policies", s.createPolicy)
		api.GET("/policies", s.listPolicies)
		api.GET("/policies/:id", s.getPolicy)

		// 借用仲裁（场景 2/3）与回收
		api.POST("/policies/:id/borrow", s.requestBorrow)
		api.POST("/policies/:id/release", s.releaseBorrow)
		api.GET("/events", s.listEvents)

		// 超开水位告警
		api.GET("/alerts", s.listAlerts)
	}
	return r
}

// ListenAndServe 阻塞提供 HTTP 服务。
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: s.Router(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	s.log.Info("http listening", "addr", addr)
	err := srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func requestLogger(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		log.Info("http", "method", c.Request.Method, "path", c.Request.URL.Path,
			"status", c.Writer.Status(), "dur", time.Since(start).String())
	}
}

func errStatus(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	case errors.Is(err, store.ErrOvercommit):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error(), "code": "overcommit_exceeded"})
	case errors.Is(err, store.ErrInsufficientBW):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error(), "code": "insufficient_bandwidth"})
	case errors.Is(err, store.ErrConflict):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}

func queryLimit(c *gin.Context) int {
	n, _ := strconv.Atoi(c.DefaultQuery("limit", "200"))
	return n
}

// ---------------- nodes ----------------

func (s *Server) listNodes(c *gin.Context) {
	nodes, err := s.st.ListNodes(c.Request.Context())
	if err != nil {
		errStatus(c, err)
		return
	}
	// 附带水位
	type nodeWithWM struct {
		*model.Node
		Watermarks monitor.Watermarks `json:"watermarks"`
	}
	out := make([]nodeWithWM, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, nodeWithWM{Node: n, Watermarks: monitor.ComputeWatermarks(n)})
	}
	c.JSON(http.StatusOK, out)
}

func (s *Server) getNode(c *gin.Context) {
	n, err := s.st.GetNode(c.Request.Context(), c.Param("id"))
	if err != nil {
		errStatus(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"node": n, "watermarks": monitor.ComputeWatermarks(n)})
}

type createNodeReq struct {
	Hostname          string  `json:"hostname" binding:"required"`
	PhysicalVCPUs     int32   `json:"physical_vcpus" binding:"required,min=1"`
	PhysicalMemMB     int64   `json:"physical_mem_mb" binding:"required,min=1"`
	PhysicalDiskGB    int64   `json:"physical_disk_gb" binding:"required,min=1"`
	PhysicalNICBwKbps int64   `json:"physical_nic_bandwidth_kbps" binding:"required,min=1"`
	CPU_ratio         float64 `json:"cpu_overcommit_ratio"`
	Mem_ratio         float64 `json:"mem_overcommit_ratio"`
	Disk_ratio        float64 `json:"disk_overcommit_ratio"`
	BW_ratio          float64 `json:"bw_overcommit_ratio"`
}

func (s *Server) createNode(c *gin.Context) {
	var q createNodeReq
	if err := c.ShouldBindJSON(&q); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	n := &model.Node{
		Hostname: q.Hostname, Status: model.NodeStatusOnline,
		PhysicalVCPUs: q.PhysicalVCPUs, PhysicalMemMB: q.PhysicalMemMB,
		PhysicalDiskGB: q.PhysicalDiskGB, PhysicalNICBwKbps: q.PhysicalNICBwKbps,
		CPUOvercommitRatio: orDefault(q.CPU_ratio, 4.0), MemOvercommitRatio: orDefault(q.Mem_ratio, 1.5),
		DiskOvercommitRatio: orDefault(q.Disk_ratio, 2.0), BWOvercommitRatio: orDefault(q.BW_ratio, 1.0),
	}
	if err := s.st.CreateNode(c.Request.Context(), n); err != nil {
		errStatus(c, err)
		return
	}
	c.JSON(http.StatusCreated, n)
}

func orDefault(v, def float64) float64 {
	if v < 1 {
		return def
	}
	return v
}

type setRatiosReq struct {
	CPU  float64 `json:"cpu_overcommit_ratio" binding:"required,gte=1"`
	Mem  float64 `json:"mem_overcommit_ratio" binding:"required,gte=1"`
	Disk float64 `json:"disk_overcommit_ratio" binding:"required,gte=1"`
	BW   float64 `json:"bw_overcommit_ratio" binding:"required,gte=1"`
}

func (s *Server) setNodeRatios(c *gin.Context) {
	var q setRatiosReq
	if err := c.ShouldBindJSON(&q); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := s.st.SetNodeRatios(c.Request.Context(), c.Param("id"), q.CPU, q.Mem, q.Disk, q.BW); err != nil {
		errStatus(c, err)
		return
	}
	n, err := s.st.GetNode(c.Request.Context(), c.Param("id"))
	if err != nil {
		errStatus(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"node": n, "watermarks": monitor.ComputeWatermarks(n)})
}

func (s *Server) nodeWatermarks(c *gin.Context) {
	n, err := s.st.GetNode(c.Request.Context(), c.Param("id"))
	if err != nil {
		errStatus(c, err)
		return
	}
	c.JSON(http.StatusOK, monitor.ComputeWatermarks(n))
}

// ---------------- vms ----------------

type placeVMReq struct {
	TenantName   string `json:"tenant_name" binding:"required"`
	Name         string `json:"name" binding:"required"`
	Flavor       string `json:"flavor"`
	VCPUs        int32  `json:"vcpus" binding:"required,min=1"`
	MemMB        int64  `json:"mem_mb" binding:"required,min=1"`
	DiskGB       int64  `json:"disk_gb" binding:"required,min=1"`
	MinKbps      int64  `json:"min_kbps" binding:"gte=0"`
	MaxKbps      int64  `json:"max_kbps" binding:"gte=0"`
	SharedPoolID string `json:"shared_pool_id"`
	NodeID       string `json:"node_id"` // 可选：指定节点；否则选首个水位最低节点
}

// placeVM 四维超开放置：指定 node_id 或简单选择四维水位最低的在线节点。
func (s *Server) placeVM(c *gin.Context) {
	var q placeVMReq
	if err := c.ShouldBindJSON(&q); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if q.MaxKbps < q.MinKbps {
		c.JSON(http.StatusBadRequest, gin.H{"error": "max_kbps < min_kbps"})
		return
	}
	ctx := c.Request.Context()

	tenantID, err := s.st.EnsureTenant(ctx, q.TenantName)
	if err != nil {
		errStatus(c, err)
		return
	}

	nodeID := q.NodeID
	if nodeID == "" {
		nodes, err := s.st.ListNodes(ctx)
		if err != nil || len(nodes) == 0 {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "no nodes available"})
			return
		}
		best := nodes[0]
		bestScore := 0.0
		for _, n := range nodes {
			if n.Status != model.NodeStatusOnline {
				continue
			}
			w := monitor.ComputeWatermarks(n)
			score := w.CPU + w.Mem + w.Disk + w.BW
			if score < bestScore || best == nodes[0] && score <= bestScore {
				if bestScore == 0 || score < bestScore {
					best, bestScore = n, score
				}
			}
		}
		nodeID = best.ID
	}

	req := &model.VMRequest{
		TenantID: tenantID, Name: q.Name, Flavor: q.Flavor,
		VCPUs: q.VCPUs, MemMB: q.MemMB, DiskGB: q.DiskGB,
		MinKbps: q.MinKbps, MaxKbps: q.MaxKbps, SharedPoolID: q.SharedPoolID,
	}
	node, err := s.st.PlaceVM(ctx, nodeID, req)
	if err != nil {
		errStatus(c, err)
		return
	}
	vm := &model.VM{TenantID: tenantID, NodeID: nodeID, Name: q.Name, Flavor: q.Flavor, VCPUs: q.VCPUs, MemMB: q.MemMB, DiskGB: q.DiskGB}
	if err := s.st.CreateVM(ctx, vm); err != nil {
		errStatus(c, err)
		return
	}
	// egress 策略随 VM 创建（有保障或入池时）
	var policy *model.PortBandwidthPolicy
	if q.MinKbps > 0 || q.SharedPoolID != "" {
		if q.MaxKbps == 0 {
			q.MaxKbps = q.MinKbps
		}
		var vnicID string
		vnic := &model.VNIC{VMID: vm.ID, MAC: randomMAC(), Bridge: "br0"}
		if err := s.st.CreateVNIC(ctx, vnic); err != nil {
			errStatus(c, err)
			return
		}
		vnicID = vnic.ID
		poolID := q.SharedPoolID
		var poolPtr *string
		if poolID != "" {
			poolPtr = &poolID
		}
		policy, err = s.st.CreatePolicy(ctx, &model.PortBandwidthPolicy{
			VNICID: vnicID, Direction: "egress",
			MinKbps: q.MinKbps, MaxKbps: q.MaxKbps, SharedPoolID: poolPtr, Priority: 100,
		})
		if err != nil {
			errStatus(c, err)
			return
		}
	}
	statusCode := http.StatusCreated
	c.JSON(statusCode, gin.H{"vm": vm, "node": node, "policy": policy})
}

func randomMAC() string {
	n := time.Now().UnixNano()
	b := []byte{0x52, 0x54, 0x00, byte(n >> 16), byte(n >> 8), byte(n)}
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", b[0], b[1], b[2], b[3], b[4], b[5])
}

func (s *Server) listVMs(c *gin.Context) {
	vms, err := s.st.ListVMs(c.Request.Context(), c.Query("node_id"))
	if err != nil {
		errStatus(c, err)
		return
	}
	c.JSON(http.StatusOK, vms)
}

func (s *Server) getVM(c *gin.Context) {
	vm, err := s.st.GetVM(c.Request.Context(), c.Param("id"))
	if err != nil {
		errStatus(c, err)
		return
	}
	c.JSON(http.StatusOK, vm)
}

type setStatusReq struct {
	Status string `json:"status" binding:"required"`
}

func (s *Server) setVMStatus(c *gin.Context) {
	var q setStatusReq
	if err := c.ShouldBindJSON(&q); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := s.st.UpdateVMStatus(c.Request.Context(), c.Param("id"), q.Status); err != nil {
		errStatus(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---------------- pools / policies / borrow ----------------

type createPoolReq struct {
	TenantName        string `json:"tenant_name" binding:"required"`
	Name              string `json:"name" binding:"required"`
	TotalCapacityKbps int64  `json:"total_capacity_kbps" binding:"required,min=1"`
}

func (s *Server) createPool(c *gin.Context) {
	var q createPoolReq
	if err := c.ShouldBindJSON(&q); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	tenantID, err := s.st.EnsureTenant(c.Request.Context(), q.TenantName)
	if err != nil {
		errStatus(c, err)
		return
	}
	p := &model.SharedBandwidthPool{TenantID: tenantID, Name: q.Name, TotalCapacityKbps: q.TotalCapacityKbps}
	if err := s.st.CreatePool(c.Request.Context(), p); err != nil {
		errStatus(c, err)
		return
	}
	c.JSON(http.StatusCreated, p)
}

func (s *Server) listPools(c *gin.Context) {
	pools, err := s.st.ListPools(c.Request.Context(), c.Query("tenant_id"))
	if err != nil {
		errStatus(c, err)
		return
	}
	c.JSON(http.StatusOK, pools)
}

func (s *Server) getPool(c *gin.Context) {
	p, err := s.st.GetPool(c.Request.Context(), c.Param("id"))
	if err != nil {
		errStatus(c, err)
		return
	}
	c.JSON(http.StatusOK, p)
}

type createPolicyReq struct {
	VNICID       string  `json:"vnic_id" binding:"required"`
	Direction    string  `json:"direction" binding:"required"`
	MinKbps      int64   `json:"min_kbps" binding:"gte=0"`
	MaxKbps      int64   `json:"max_kbps" binding:"required,min=1"`
	SharedPoolID *string `json:"shared_pool_id"`
	Priority     int     `json:"priority"`
}

func (s *Server) createPolicy(c *gin.Context) {
	var q createPolicyReq
	if err := c.ShouldBindJSON(&q); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	p, err := s.st.CreatePolicy(c.Request.Context(), &model.PortBandwidthPolicy{
		VNICID: q.VNICID, Direction: q.Direction,
		MinKbps: q.MinKbps, MaxKbps: q.MaxKbps, SharedPoolID: q.SharedPoolID, Priority: q.Priority,
	})
	if err != nil {
		errStatus(c, err)
		return
	}
	c.JSON(http.StatusCreated, p)
}

func (s *Server) listPolicies(c *gin.Context) {
	list, err := s.st.ListPolicies(c.Request.Context(), c.Query("vnic_id"))
	if err != nil {
		errStatus(c, err)
		return
	}
	c.JSON(http.StatusOK, list)
}

func (s *Server) getPolicy(c *gin.Context) {
	p, err := s.st.GetPolicy(c.Request.Context(), c.Param("id"))
	if err != nil {
		errStatus(c, err)
		return
	}
	c.JSON(http.StatusOK, p)
}

type borrowReq struct {
	RequestedKbps int64 `json:"requested_kbps" binding:"required,min=1"`
}

// requestBorrow 场景 2（直接授予）与场景 3（抢占）统一入口。
func (s *Server) requestBorrow(c *gin.Context) {
	var q borrowReq
	if err := c.ShouldBindJSON(&q); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	d, pol, err := s.st.RequestBorrow(c.Request.Context(), c.Param("id"), q.RequestedKbps)
	if err != nil {
		errStatus(c, err)
		return
	}
	httpStatus := http.StatusOK
	if d.Reason == model.BorrowReasonDenied {
		httpStatus = http.StatusConflict
	}
	c.JSON(httpStatus, gin.H{"decision": d, "policy": pol})
}

func (s *Server) releaseBorrow(c *gin.Context) {
	pol, err := s.st.ReleaseBorrow(c.Request.Context(), c.Param("id"))
	if err != nil {
		errStatus(c, err)
		return
	}
	c.JSON(http.StatusOK, pol)
}

func (s *Server) listEvents(c *gin.Context) {
	events, err := s.st.ListBorrowEvents(c.Request.Context(), c.Query("policy_id"), queryLimit(c))
	if err != nil {
		errStatus(c, err)
		return
	}
	c.JSON(http.StatusOK, events)
}

func (s *Server) listAlerts(c *gin.Context) {
	alerts, err := s.st.ListAlerts(c.Request.Context(), c.Query("node_id"), queryLimit(c))
	if err != nil {
		errStatus(c, err)
		return
	}
	c.JSON(http.StatusOK, alerts)
}
