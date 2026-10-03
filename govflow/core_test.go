package govflow

import (
	"math"
	"strings"
	"testing"
)

// TestVoteWeightOverflow：单侧累计恰好到上限必须接受；超过上限整票拒绝且
// 提案原样保留，后续能容纳的投票不受影响。
func TestVoteWeightOverflow(t *testing.T) {
	p := &Proposal{ID: "gip-1", State: "voting", Quorum: 1}
	if err := Vote(p, math.MaxInt64, true); err != nil {
		t.Fatalf("vote reaching exactly MaxInt64 must be accepted: %v", err)
	}
	if p.ForVotes != math.MaxInt64 {
		t.Fatalf("for votes = %d, want %d", p.ForVotes, int64(math.MaxInt64))
	}
	err := Vote(p, 1, true)
	if err == nil || !strings.Contains(err.Error(), "overflow") {
		t.Fatalf("overflow vote must be rejected with overflow error, got %v", err)
	}
	if p.ForVotes != math.MaxInt64 || p.AgainstVotes != 0 || p.State != "voting" {
		t.Fatalf("proposal changed after overflow: %+v", p)
	}
	// 溢出失败不妨碍另一侧正常投票。
	if err := Vote(p, 5, false); err != nil {
		t.Fatalf("against vote after overflow must succeed: %v", err)
	}
	if p.AgainstVotes != 5 {
		t.Fatalf("against votes = %d, want 5", p.AgainstVotes)
	}
}

// TestVoteOverflowAgainstSide：反对侧同样适用溢出规则。
func TestVoteOverflowAgainstSide(t *testing.T) {
	p := &Proposal{ID: "gip-2", State: "voting", Quorum: 1}
	if err := Vote(p, math.MaxInt64, false); err != nil {
		t.Fatalf("against vote reaching MaxInt64 must be accepted: %v", err)
	}
	if err := Vote(p, 1, false); err == nil {
		t.Fatal("overflowing against vote must be rejected")
	}
	if p.AgainstVotes != math.MaxInt64 {
		t.Fatalf("against votes = %d, want %d", p.AgainstVotes, int64(math.MaxInt64))
	}
}

// TestVoteExistingRejections：非 voting 状态与非正权重的拒绝保持原样。
func TestVoteExistingRejections(t *testing.T) {
	p := &Proposal{ID: "gip-3", State: "passed", Quorum: 1}
	if err := Vote(p, 10, true); err == nil {
		t.Fatal("vote on non-voting proposal must be rejected")
	}
	p.State = "voting"
	for _, w := range []int64{0, -1, math.MinInt64} {
		if err := Vote(p, w, true); err == nil {
			t.Fatalf("vote with weight %d must be rejected", w)
		}
	}
	if p.ForVotes != 0 || p.AgainstVotes != 0 {
		t.Fatalf("rejected votes changed tallies: %+v", p)
	}
}

// TestTallySumOverflow：两侧各自可表示、合计超出 int64 上限时，
// 仍按真实参与权重判断法定人数。
func TestTallySumOverflow(t *testing.T) {
	// 法定人数=MaxInt64，赞成=MaxInt64，反对=1：参与量真实超过上限，必须 passed。
	p := &Proposal{ID: "gip-4", State: "voting", Quorum: math.MaxInt64,
		ForVotes: math.MaxInt64, AgainstVotes: 1}
	if got := Tally(p); got != "passed" {
		t.Fatalf("tally = %s, want passed", got)
	}
	// 两侧都是 MaxInt64：参与量极大但平票，必须 rejected。
	p = &Proposal{ID: "gip-5", State: "voting", Quorum: math.MaxInt64,
		ForVotes: math.MaxInt64, AgainstVotes: math.MaxInt64}
	if got := Tally(p); got != "rejected" {
		t.Fatalf("tally = %s, want rejected (tie)", got)
	}
	// 合计超上限但参与不足法定人数（两侧各 MaxInt64-1，quorum=MaxInt64 仍达到）：
	// for+against = 2*MaxInt64-2 >= MaxInt64，赞成领先 => passed。
	p = &Proposal{ID: "gip-6", State: "voting", Quorum: math.MaxInt64,
		ForVotes: math.MaxInt64 - 1, AgainstVotes: math.MaxInt64 - 2}
	if got := Tally(p); got != "passed" {
		t.Fatalf("tally = %s, want passed", got)
	}
}

// TestTallyNormalRulesUnchanged：正常权重下规则保持原样。
func TestTallyNormalRulesUnchanged(t *testing.T) {
	// 参与量等于法定人数算达到。
	p := &Proposal{ID: "gip-7", State: "voting", Quorum: 100, ForVotes: 60, AgainstVotes: 40}
	if got := Tally(p); got != "passed" {
		t.Fatalf("tally = %s, want passed", got)
	}
	// 参与不足即使赞成领先也拒绝。
	p = &Proposal{ID: "gip-8", State: "voting", Quorum: 100, ForVotes: 60, AgainstVotes: 10}
	if got := Tally(p); got != "rejected" {
		t.Fatalf("tally = %s, want rejected (below quorum)", got)
	}
	// 非 voting 提案返回当前状态，且不改写任何内容。
	p = &Proposal{ID: "gip-9", State: "executed", Quorum: 100, ForVotes: 60, AgainstVotes: 40}
	if got := Tally(p); got != "executed" {
		t.Fatalf("tally = %s, want executed", got)
	}
	// 计票不改写提案状态与票数。
	p = &Proposal{ID: "gip-10", State: "voting", Quorum: 10, ForVotes: 20, AgainstVotes: 5}
	if got := Tally(p); got != "passed" {
		t.Fatalf("tally = %s, want passed", got)
	}
	if p.State != "voting" || p.ForVotes != 20 || p.AgainstVotes != 5 {
		t.Fatalf("tally mutated proposal: %+v", p)
	}
}
