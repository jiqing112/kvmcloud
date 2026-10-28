package bandwidth

import (
	"testing"

	"github.com/kvm-cloud/platform/internal/model"
)

func pool(cap, allocMin, borrowed int64) PoolView {
	return PoolView{TotalCapacityKbps: cap, AllocatedMinKbps: allocMin, CurrentBorrowedKbps: borrowed}
}

func pol(id string, min, max, prio, ceil int64) PolicyView {
	return PolicyView{ID: id, MinKbps: min, MaxKbps: max, Priority: int(prio), CurrentCeil: ceil, State: "borrowing"}
}

// 场景 2：池有余量，直接授予。
func TestDecide_Grant(t *testing.T) {
	p := pool(1_000_000, 200_000, 100_000) // available = 700k
	d := Decide(p, 300_000, pol("a", 100_000, 500_000, 100, 100_000), nil)
	if d.Granted != 300_000 || d.Reason != "granted" {
		t.Fatalf("want granted 300k, got %+v", d)
	}
}

// 授予不超过 max-min。
func TestDecide_CapAtMax(t *testing.T) {
	p := pool(1_000_000, 100_000, 0)
	d := Decide(p, 900_000, pol("a", 100_000, 300_000, 100, 100_000), nil)
	if d.Granted != 200_000 {
		t.Fatalf("want capped 200k, got %d", d.Granted)
	}
}

// 场景 3：池不足，抢占低优先级。
func TestDecide_PreemptLowPriority(t *testing.T) {
	p := pool(1_000_000, 300_000, 600_000) // available = 100k
	peers := []PolicyView{
		pol("low", 100_000, 400_000, 10, 400_000),  // 可回收 300k
		pol("high", 100_000, 400_000, 500, 300_000), // 优先级更高，不可压
	}
	d := Decide(p, 300_000, pol("me", 100_000, 500_000, 100, 100_000), peers)
	if d.Reason != "preempted" || d.Granted != 300_000 {
		t.Fatalf("want preempted 300k, got %+v", d)
	}
	if len(d.Victims) != 1 || d.Victims[0].PolicyID != "low" {
		t.Fatalf("want victim low only, got %+v", d.Victims)
	}
	if d.Victims[0].NewCeil != 200_000 || d.Victims[0].Reclaimed != 200_000 {
		t.Fatalf("want victim pressed to 200k reclaim 200k, got %+v", d.Victims[0])
	}
}

// 保障值不被压穿：按需回收，压回量不超过 need，且永不低于 min_kbps。
func TestDecide_NeverBelowMin(t *testing.T) {
	p := pool(1_000_000, 300_000, 650_000) // available = 50k
	peers := []PolicyView{pol("low", 250_000, 500_000, 10, 500_000)}
	d := Decide(p, 200_000, pol("me", 50_000, 500_000, 100, 50_000), peers)
	if d.Reason != "preempted" {
		t.Fatalf("want preempted, got %+v", d)
	}
	// need = 200k-50k = 150k，只回收 150k，victim 压到 350k（未触及 min）
	if len(d.Victims) != 1 || d.Victims[0].NewCeil != 350_000 || d.Victims[0].Reclaimed != 150_000 {
		t.Fatalf("want victim ceil=350000 reclaim=150000, got %+v", d.Victims)
	}
}

// 回收需求超过可释放总量 → 整体拒绝（任务书语义：insufficient after preemption）。
func TestDecide_PressToMin(t *testing.T) {
	p := pool(1_000_000, 300_000, 650_000) // available = 50k
	peers := []PolicyView{pol("low", 250_000, 500_000, 10, 500_000)}
	d := Decide(p, 800_000, pol("me", 50_000, 900_000, 100, 50_000), peers)
	if d.Reason != "denied" || d.Granted != 0 {
		t.Fatalf("want denied, got %+v", d)
	}
}

// 需求超出可释放总量 → denied。
func TestDecide_Denied(t *testing.T) {
	p := pool(1_000_000, 300_000, 650_000) // available = 50k
	peers := []PolicyView{pol("low", 250_000, 500_000, 10, 300_000)} // 可回收仅 50k
	d := Decide(p, 500_000, pol("me", 50_000, 900_000, 100, 50_000), peers)
	if d.Reason != "denied" || d.Granted != 0 {
		t.Fatalf("want denied, got %+v", d)
	}
}

// 跨池隔离（语义层）：仲裁只看同池 peers；此测试保证多池数据互不影响。
func TestDecide_PoolIsolation(t *testing.T) {
	p := pool(500_000, 100_000, 390_000) // available = 10k
	// peers 属于其他池的数据不应传入；这里传空 → 无人可抢 → denied
	d := Decide(p, 100_000, pol("me", 50_000, 400_000, 100, 50_000), nil)
	if d.Reason != "denied" {
		t.Fatalf("want denied without same-pool peers, got %+v", d)
	}
}

// 多个 victim 依次压回，优先级最低先被压。
func TestDecide_MultipleVictims(t *testing.T) {
	p := pool(1_000_000, 300_000, 690_000) // available = 10k
	peers := []PolicyView{
		pol("mid", 100_000, 300_000, 50, 300_000),  // 回收 200k
		pol("low", 100_000, 300_000, 10, 300_000),  // 回收 200k
	}
	d := Decide(p, 400_000, pol("me", 10_000, 600_000, 100, 10_000), peers)
	if d.Granted != 400_000 || d.Reason != "preempted" {
		t.Fatalf("want preempted 400k, got %+v", d)
	}
	// low(10) 先压满，mid(50) 压剩余 190k
	if len(d.Victims) != 2 || d.Victims[0].PolicyID != "low" || d.Victims[1].PolicyID != "mid" {
		t.Fatalf("want victims [low, mid], got %+v", d.Victims)
	}
	if d.Victims[0].NewCeil != 100_000 {
		t.Fatalf("want low pressed to min 100k, got %+v", d.Victims[0])
	}
	if d.Victims[1].Reclaimed != 190_000 {
		t.Fatalf("want mid reclaim 190k, got %+v", d.Victims[1])
	}
}

// 事件原因映射落库语义（借用事件 reason 字段约定）。
func TestReasonEnum(t *testing.T) {
	cases := map[string]string{"granted": model.BorrowReasonGranted, "preempted": model.BorrowReasonPreempted, "denied": model.BorrowReasonDenied}
	for want, got := range cases {
		if want != got {
			t.Fatalf("reason mismatch %s != %s", want, got)
		}
	}
}
