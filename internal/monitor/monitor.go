// Package monitor 计算四维超开水位并触发告警（验收：任一维 ≥90% 写入 overcommit_alerts）。
package monitor

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/kvm-cloud/platform/internal/model"
	"github.com/kvm-cloud/platform/internal/store"
)

// DefaultAlertThreshold 水位告警阈值（0.90）。
const DefaultAlertThreshold = 0.90

// Cooldown 同一 (node, dimension) 告警冷却期，避免刷库。
const Cooldown = 10 * time.Minute

// Thresholds 可配置阈值。
type Thresholds struct {
	Alert float64 // 水位告警阈值，默认 0.90
	DiskFullProtection float64 // 存储写满保护阈值，默认 0.95
}

// Watermarks 节点四维水位快照。
type Watermarks struct {
	CPU  float64 `json:"cpu_watermark"`
	Mem  float64 `json:"mem_watermark"`
	Disk float64 `json:"disk_watermark"`
	BW   float64 `json:"bw_watermark"`
}

// ComputeWatermarks 四维水位（纯函数）：allocated / (physical × ratio)。
func ComputeWatermarks(n *model.Node) Watermarks {
	return Watermarks{
		CPU:  ratio(int64(n.AllocatedVCPUs), int64(n.PhysicalVCPUs), n.CPUOvercommitRatio),
		Mem:  ratio(n.AllocatedMemMB, n.PhysicalMemMB, n.MemOvercommitRatio),
		Disk: ratio(n.AllocatedDiskGB, n.PhysicalDiskGB, n.DiskOvercommitRatio),
		BW:   ratio(n.AllocatedMinKbps, n.PhysicalNICBwKbps, n.BWOvercommitRatio),
	}
}

func ratio(alloc, phys int64, r float64) float64 {
	if phys <= 0 || r <= 0 {
		return 0
	}
	return float64(alloc) / (float64(phys) * r)
}

// Monitor 周期扫描节点水位并落告警。
type Monitor struct {
	store      *store.Store
	thresholds Thresholds
	log        *slog.Logger
	interval   time.Duration

	mu       sync.Mutex
	lastSent map[string]time.Time // key: nodeID|dimension
}

// New 构造监控器。
func New(st *store.Store, thresholds Thresholds, log *slog.Logger, interval time.Duration) *Monitor {
	if thresholds.Alert <= 0 {
		thresholds.Alert = DefaultAlertThreshold
	}
	if thresholds.DiskFullProtection <= 0 {
		thresholds.DiskFullProtection = 0.95
	}
	if interval <= 0 {
		interval = 10 * time.Second
	}
	return &Monitor{store: st, thresholds: thresholds, log: log, interval: interval, lastSent: map[string]time.Time{}}
}

// EvaluateNode 评估单节点：返回水位；超过阈值的维度写入告警（含冷却）。
func (m *Monitor) EvaluateNode(ctx context.Context, n *model.Node) Watermarks {
	w := ComputeWatermarks(n)
	dims := []struct {
		dim  model.Dimension
		val  float64
		alloc, phys int64
		ratio float64
	}{
		{model.DimCPU, w.CPU, int64(n.AllocatedVCPUs), int64(n.PhysicalVCPUs), n.CPUOvercommitRatio},
		{model.DimMem, w.Mem, n.AllocatedMemMB, n.PhysicalMemMB, n.MemOvercommitRatio},
		{model.DimDisk, w.Disk, n.AllocatedDiskGB, n.PhysicalDiskGB, n.DiskOvercommitRatio},
		{model.DimBW, w.BW, n.AllocatedMinKbps, n.PhysicalNICBwKbps, n.BWOvercommitRatio},
	}
	for _, d := range dims {
		if d.val < m.thresholds.Alert {
			continue
		}
		if m.coolingDown(n.ID, string(d.dim)) {
			continue
		}
		severity := "warning"
		if d.val >= m.thresholds.DiskFullProtection {
			severity = "critical"
		}
		alert := &model.OvercommitAlert{
			NodeID:    n.ID,
			Dimension: d.dim,
			Allocated: d.alloc,
			Physical:  int64(math.Round(float64(d.phys) * d.ratio)),
			Ratio:     d.val,
			Severity:  severity,
		}
		if err := m.store.InsertAlert(ctx, alert); err != nil {
			m.log.Error("insert overcommit alert", "node", n.Hostname, "dim", d.dim, "err", err)
			continue
		}
		m.markSent(n.ID, string(d.dim))
		m.log.Warn("overcommit watermark alert",
			"node", n.Hostname, "dimension", d.dim,
			"watermark", d.val, "threshold", m.thresholds.Alert, "severity", severity)
	}
	return w
}

// Run 周期扫描所有节点直到 ctx 取消。
func (m *Monitor) Run(ctx context.Context) {
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			nodes, err := m.store.ListNodes(ctx)
			if err != nil {
				m.log.Error("monitor list nodes", "err", err)
				continue
			}
			for _, n := range nodes {
				if n.Status != model.NodeStatusOnline {
					continue
				}
				m.EvaluateNode(ctx, n)
			}
		}
	}
}

func (m *Monitor) coolingDown(nodeID, dim string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := nodeID + "|" + dim
	return time.Since(m.lastSent[key]) < Cooldown
}

func (m *Monitor) markSent(nodeID, dim string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastSent[nodeID+"|"+dim] = time.Now()
}
