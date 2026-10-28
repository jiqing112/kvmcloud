// Package bandwidth 实现带宽仲裁三场景：保障总量校验、借用仲裁、竞争抢占。
// 核心算法为纯函数（Decide），事务与行锁由 store 层在执行阶段保证；
// 这样并发行为可单元测试，不依赖真实 PostgreSQL。
package bandwidth

import (
	"sort"
)

// PolicyView 仲裁所需的策略只读视图。
type PolicyView struct {
	ID           string
	MinKbps      int64
	MaxKbps      int64
	Priority     int
	CurrentCeil  int64 // 当前生效 ceil；无借用时等于 MinKbps
	State        string
}

// PoolView 共享池只读视图。
type PoolView struct {
	TotalCapacityKbps   int64
	AllocatedMinKbps    int64
	CurrentBorrowedKbps int64
}

// Decision 仲裁结果。
type Decision struct {
	Granted int64  // 实际授予带宽
	Reason  string // granted | preempted | denied
	Victims []Victim
}

// Victim 被抢占方及其压回后的 ceil。
type Victim struct {
	PolicyID  string
	NewCeil   int64
	Reclaimed int64
}

// Available 池当前可借用量。
func (p PoolView) Available() int64 {
	return p.TotalCapacityKbps - p.AllocatedMinKbps - p.CurrentBorrowedKbps
}

// Decide 借用仲裁（纯函数，三场景全覆盖）：
//
// 场景 2 借用：池余量足够 → 直接授予 min(requested, max-min)。
// 场景 3 抢占：池不足 → 按 priority 从高到低压回低优先级借用方（高 priority 数值优先保留），
//   每压回一个 victim 释放 ceil-min 的带宽，直至凑足或无 victim 可压。
//   凑足后授予；仍不足则 denied。
//
// 跨池隔离由调用方保证：只传入同一 pool 的策略视图。
func Decide(pool PoolView, requested int64, policy PolicyView, peers []PolicyView) Decision {
	want := requested
	if cap := policy.MaxKbps - policy.MinKbps; want > cap {
		want = cap
	}
	if want <= 0 {
		return Decision{Granted: 0, Reason: "denied"}
	}

	available := pool.Available()
	if available >= want {
		return Decision{Granted: want, Reason: "granted"}
	}

	// 抢占候选：优先级数值比请求方低（更小），且当前确有可回收借用
	cands := make([]PolicyView, 0, len(peers))
	for _, p := range peers {
		if p.ID == policy.ID {
			continue
		}
		if p.Priority >= policy.Priority {
			continue // 只能压回优先级更低的
		}
		reclaimable := p.CurrentCeil - p.MinKbps
		if reclaimable <= 0 {
			continue
		}
		cands = append(cands, p)
	}
	// 先压回优先级最低的（数值最小），同等优先级压回当前借用最多的
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].Priority != cands[j].Priority {
			return cands[i].Priority < cands[j].Priority
		}
		return (cands[i].CurrentCeil - cands[i].MinKbps) > (cands[j].CurrentCeil - cands[j].MinKbps)
	})

	need := want - available
	var victims []Victim
	freed := int64(0)
	for _, v := range cands {
		if freed >= need {
			break
		}
		reclaim := v.CurrentCeil - v.MinKbps
		if reclaim > need-freed {
			reclaim = need - freed
		}
		victims = append(victims, Victim{PolicyID: v.ID, NewCeil: v.CurrentCeil - reclaim, Reclaimed: reclaim})
		freed += reclaim
	}

	if freed+available < want {
		return Decision{Granted: 0, Reason: "denied", Victims: victims}
	}
	return Decision{Granted: want, Reason: "preempted", Victims: victims}
}
