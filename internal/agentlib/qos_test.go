package agentlib

import (
	"testing"
	"strings"
)

// 验收：ApplyQoS 生成 tc qdisc/class/filter 三步。
func TestApplyQoS_Commands(t *testing.T) {
	f := &FakeRunner{Out: ""}
	q := &QoSApplier{R: f}
	if err := q.ApplyQoS("tap0", 100_000, 200_000); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 3 {
		t.Fatalf("want 3 tc calls, got %d: %v", len(f.Calls), f.Calls)
	}
	if !strings.Contains(f.Calls[0], "qdisc replace dev tap0 root handle 1: htb default 10") {
		t.Fatalf("qdisc cmd wrong: %s", f.Calls[0])
	}
	if !strings.Contains(f.Calls[1], "rate 100000kbit") || !strings.Contains(f.Calls[1], "ceil 200000kbit") {
		t.Fatalf("class cmd wrong: %s", f.Calls[1])
	}
	if !strings.Contains(f.Calls[2], "flowid 1:10") {
		t.Fatalf("filter cmd wrong: %s", f.Calls[2])
	}
}

// 验收：UpdateCeil 只改 ceil。
func TestUpdateCeil(t *testing.T) {
	f := &FakeRunner{}
	q := &QoSApplier{R: f}
	if err := q.UpdateCeil("tap0", 300_000); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 1 || !strings.Contains(f.Calls[0], "ceil 300000kbit") || strings.Contains(f.Calls[0], "rate") {
		t.Fatalf("ceil cmd wrong: %v", f.Calls)
	}
	if err := q.UpdateCeil("tap0", 0); err == nil {
		t.Fatal("want error for ceil=0")
	}
}

// 非法参数拒绝。
func TestApplyQoS_Invalid(t *testing.T) {
	q := &QoSApplier{R: &FakeRunner{}}
	if err := q.ApplyQoS("tap0", 0, 100); err == nil {
		t.Fatal("want error min=0")
	}
	if err := q.ApplyQoS("tap0", 200, 100); err == nil {
		t.Fatal("want error ceil<min")
	}
}
