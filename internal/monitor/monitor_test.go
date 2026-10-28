package monitor

import (
	"testing"

	"github.com/kvm-cloud/platform/internal/model"
)

func node() *model.Node {
	return &model.Node{
		PhysicalVCPUs: 16, PhysicalMemMB: 32768, PhysicalDiskGB: 1024, PhysicalNICBwKbps: 1_000_000,
		CPUOvercommitRatio: 4.0, MemOvercommitRatio: 1.5, DiskOvercommitRatio: 2.0, BWOvercommitRatio: 1.0,
	}
}

func TestComputeWatermarks_Zero(t *testing.T) {
	w := ComputeWatermarks(node())
	if w.CPU != 0 || w.Mem != 0 || w.Disk != 0 || w.BW != 0 {
		t.Fatalf("want zeros, got %+v", w)
	}
}

func TestComputeWatermarks_Known(t *testing.T) {
	n := node()
	n.AllocatedVCPUs = 32 // 32/(16*4)=0.5
	n.AllocatedMemMB = 24576
	n.AllocatedDiskGB = 1024
	n.AllocatedMinKbps = 900_000
	w := ComputeWatermarks(n)
	if w.CPU != 0.5 {
		t.Fatalf("cpu want 0.5 got %v", w.CPU)
	}
	if w.Mem != 0.5 { // 24576/(32768*1.5)
		t.Fatalf("mem want 0.5 got %v", w.Mem)
	}
	if w.Disk != 0.5 { // 1024/(1024*2)
		t.Fatalf("disk want 0.5 got %v", w.Disk)
	}
	if w.BW != 0.9 {
		t.Fatalf("bw want 0.9 got %v", w.BW)
	}
}

func TestComputeWatermarks_OverOne(t *testing.T) {
	n := node()
	n.CPUOvercommitRatio = 1.0
	n.AllocatedVCPUs = 20 // > physical
	w := ComputeWatermarks(n)
	if w.CPU <= 1.0 {
		t.Fatalf("want >1, got %v", w.CPU)
	}
}
