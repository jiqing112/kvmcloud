// Package model 定义平台核心数据结构与四维超开模型。
package model

import (
	"time"
)

// Dimension 超开维度。
type Dimension string

const (
	DimCPU  Dimension = "cpu"
	DimMem  Dimension = "mem"
	DimDisk Dimension = "disk"
	DimBW   Dimension = "bw"
)

// AllDimensions 四维超开的全部维度。
var AllDimensions = []Dimension{DimCPU, DimMem, DimDisk, DimBW}

// Node 计算节点，携带四维容量/超开比/已分配。
type Node struct {
	ID        string    `json:"id"`
	Hostname  string    `json:"hostname"`
	ClusterID *string   `json:"cluster_id,omitempty"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`

	// 物理容量
	PhysicalVCPUs     int32 `json:"physical_vcpus"`
	PhysicalMemMB     int64 `json:"physical_mem_mb"`
	PhysicalDiskGB    int64 `json:"physical_disk_gb"`
	PhysicalNICBwKbps int64 `json:"physical_nic_bandwidth_kbps"`

	// 超开比
	CPUOvercommitRatio  float64 `json:"cpu_overcommit_ratio"`
	MemOvercommitRatio  float64 `json:"mem_overcommit_ratio"`
	DiskOvercommitRatio float64 `json:"disk_overcommit_ratio"`
	BWOvercommitRatio   float64 `json:"bw_overcommit_ratio"`

	// 已分配
	AllocatedVCPUs   int32 `json:"allocated_vcpus"`
	AllocatedMemMB   int64 `json:"allocated_mem_mb"`
	AllocatedDiskGB  int64 `json:"allocated_disk_gb"`
	AllocatedMinKbps int64 `json:"allocated_min_kbps"`
}

// VMRequest 放置 VM 时的四维资源请求。
type VMRequest struct {
	TenantID     string `json:"tenant_id"`
	Name         string `json:"name"`
	Flavor       string `json:"flavor"`
	VCPUs        int32  `json:"vcpus"`
	MemMB        int64  `json:"mem_mb"`
	DiskGB       int64  `json:"disk_gb"`
	MinKbps      int64  `json:"min_kbps"`      // 保障带宽需求（egress）
	MaxKbps      int64  `json:"max_kbps"`      // 借用上限
	SharedPoolID string `json:"shared_pool_id"` // 可为空：无共享池则无借用
}

// VM 虚拟机。
type VM struct {
	ID        string    `json:"id"`
	TenantID  string    `json:"tenant_id"`
	NodeID    string    `json:"node_id"`
	Name      string    `json:"name"`
	Flavor    string    `json:"flavor"`
	VCPUs     int32     `json:"vcpus"`
	MemMB     int64     `json:"mem_mb"`
	DiskGB    int64     `json:"disk_gb"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

// VNIC 虚拟网卡。
type VNIC struct {
	ID        string    `json:"id"`
	VMID      string    `json:"vm_id"`
	MAC       string    `json:"mac"`
	IPv4      string    `json:"ipv4,omitempty"`
	IPv6      string    `json:"ipv6,omitempty"`
	Bridge    string    `json:"bridge"`
	CreatedAt time.Time `json:"created_at"`
}

// PortBandwidthPolicy 端口带宽策略：保障 + 借用 + 抢占优先级。
type PortBandwidthPolicy struct {
	ID             string     `json:"id"`
	VNICID         string     `json:"vnic_id"`
	Direction      string     `json:"direction"` // ingress | egress
	MinKbps        int64      `json:"min_kbps"`
	MaxKbps        int64      `json:"max_kbps"`
	SharedPoolID   *string    `json:"shared_pool_id,omitempty"`
	Priority       int        `json:"priority"` // 数值越大优先级越高，抢占时优先压回低 priority
	CurrentCeilKbps *int64    `json:"current_ceil_kbps,omitempty"`
	State          string     `json:"state"` // guaranteed | borrowing | preempted
	CreatedAt      time.Time  `json:"created_at"`
}

// SharedBandwidthPool 共享带宽池。
type SharedBandwidthPool struct {
	ID                  string    `json:"id"`
	TenantID            string    `json:"tenant_id"`
	Name                string    `json:"name"`
	TotalCapacityKbps   int64     `json:"total_capacity_kbps"`
	AllocatedMinKbps    int64     `json:"allocated_min_kbps"`
	CurrentBorrowedKbps int64     `json:"current_borrowed_kbps"`
	CreatedAt           time.Time `json:"created_at"`
}

// BorrowEvent 借用/抢占/回收事件。
type BorrowEvent struct {
	ID            int64     `json:"id"`
	PolicyID      string    `json:"policy_id"`
	RequestedKbps int64     `json:"requested_kbps"`
	GrantedKbps   int64     `json:"granted_kbps"`
	Reason        string    `json:"reason"` // granted | preempted | denied | released
	CreatedAt     time.Time `json:"created_at"`
}

// OvercommitAlert 超开水位告警。
type OvercommitAlert struct {
	ID        int64     `json:"id"`
	NodeID    string    `json:"node_id"`
	Dimension Dimension `json:"dimension"`
	Allocated int64     `json:"allocated"`
	Physical  int64     `json:"physical"`
	Ratio     float64   `json:"ratio"`
	Severity  string    `json:"severity"` // warning | critical
	CreatedAt time.Time `json:"created_at"`
}

// Watermark 节点单维超开水位（0~1+）。
type Watermark struct {
	Dimension  Dimension `json:"dimension"`
	Allocated  int64     `json:"allocated"`
	Physical   int64     `json:"physical"`
	Limit      int64     `json:"limit"` // physical * ratio
	Watermark  float64   `json:"watermark"`
	Threshold  float64   `json:"threshold"`
}

// Borrow 事件 reason 取值（borrow_events.reason）。
const (
	BorrowReasonGranted   = "granted"   // 池余量直接授予
	BorrowReasonPreempted = "preempted" // 抢占低优先级后授予
	BorrowReasonDenied    = "denied"    // 抢占后仍不足
	BorrowReasonReleased  = "released"  // 主动归还
)

// 节点 / VM / 策略状态取值。
const (
	NodeStatusOnline  = "online"
	NodeStatusOffline = "offline"

	VMStatusCreating = "creating"
	VMStatusRunning  = "running"
	VMStatusStopped  = "stopped"
	VMStatusDeleted  = "deleted"

	PolicyStateGuaranteed = "guaranteed"
	PolicyStateBorrowing  = "borrowing"
	PolicyStatePreempted  = "preempted"
)
