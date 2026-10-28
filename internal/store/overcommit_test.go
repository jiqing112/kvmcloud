package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/kvm-cloud/platform/internal/model"
)

func baseNode() *model.Node {
	return &model.Node{
		ID:                  "n1",
		Hostname:            "node-1",
		PhysicalVCPUs:       16,
		PhysicalMemMB:       32768,
		PhysicalDiskGB:      1024,
		PhysicalNICBwKbps:   1_000_000, // 1 Gbps
		CPUOvercommitRatio:  4.0,
		MemOvercommitRatio:  1.5,
		DiskOvercommitRatio: 2.0,
		BWOvercommitRatio:   1.0,
	}
}

// 验收：CPU 分配 vCPU 超 physical×ratio 被拒绝。
func TestCheckOvercommit_CPUExceeded(t *testing.T) {
	n := baseNode() // limit = 16*4 = 64
	n.AllocatedVCPUs = 63
	req := &model.VMRequest{VCPUs: 2, MemMB: 1024, DiskGB: 20, MinKbps: 0}
	err := CheckOvercommit(n, req)
	if err == nil || !strings.Contains(err.Error(), "cpu") {
		t.Fatalf("want cpu overcommit error, got %v", err)
	}
	if !errors.Is(err, ErrOvercommit) {
		t.Fatalf("want ErrOvercommit, got %v", err)
	}
}

// 验收：内存分配超 physical×ratio 被拒绝。
func TestCheckOvercommit_MemExceeded(t *testing.T) {
	n := baseNode() // limit = 32768*1.5 = 49152
	n.AllocatedMemMB = 49152
	req := &model.VMRequest{VCPUs: 1, MemMB: 1, DiskGB: 20}
	err := CheckOvercommit(n, req)
	if err == nil || !strings.Contains(err.Error(), "mem") {
		t.Fatalf("want mem overcommit error, got %v", err)
	}
}

// 验收：存储精简置备超 physical×ratio 被拒绝。
func TestCheckOvercommit_DiskExceeded(t *testing.T) {
	n := baseNode() // limit = 1024*2 = 2048
	n.AllocatedDiskGB = 2047
	req := &model.VMRequest{VCPUs: 1, MemMB: 512, DiskGB: 2}
	err := CheckOvercommit(n, req)
	if err == nil || !strings.Contains(err.Error(), "disk") {
		t.Fatalf("want disk overcommit error, got %v", err)
	}
}

// 验收：SUM(min_kbps) 超 physical×bw_ratio 被拒绝。
func TestCheckOvercommit_BWExceeded(t *testing.T) {
	n := baseNode() // limit = 1000000*1.0
	n.AllocatedMinKbps = 999_999
	req := &model.VMRequest{VCPUs: 1, MemMB: 512, DiskGB: 20, MinKbps: 2}
	err := CheckOvercommit(n, req)
	if err == nil || !strings.Contains(err.Error(), "bw") {
		t.Fatalf("want bw overcommit error, got %v", err)
	}
}

// 边界内应通过。
func TestCheckOvercommit_WithinLimitsAccepted(t *testing.T) {
	n := baseNode()
	n.AllocatedVCPUs = 60
	n.AllocatedMemMB = 48_000
	n.AllocatedDiskGB = 2_000
	n.AllocatedMinKbps = 999_000
	req := &model.VMRequest{VCPUs: 4, MemMB: 1024, DiskGB: 40, MinKbps: 1000}
	if err := CheckOvercommit(n, req); err != nil {
		t.Fatalf("want accept within limits, got %v", err)
	}
}

// 负数请求拒绝。
func TestCheckOvercommit_NegativeRejected(t *testing.T) {
	n := baseNode()
	req := &model.VMRequest{VCPUs: -1}
	err := CheckOvercommit(n, req)
	if err == nil || !strings.Contains(err.Error(), "negative") {
		t.Fatalf("want negative rejected, got %v", err)
	}
}

// 超开比可调语义：ratio=1 即不超卖。
func TestCheckOvercommit_RatioOne(t *testing.T) {
	n := baseNode()
	n.CPUOvercommitRatio = 1.0
	req := &model.VMRequest{VCPUs: 17, MemMB: 512, DiskGB: 20}
	if err := CheckOvercommit(n, req); err == nil {
		t.Fatal("want reject when ratio=1 and vcpus > physical")
	}
	req.VCPUs = 16
	if err := CheckOvercommit(n, req); err != nil {
		t.Fatalf("want accept at exactly limit, got %v", err)
	}
}
