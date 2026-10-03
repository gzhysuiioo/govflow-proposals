package govflow

import (
	"math"
	"strings"
	"testing"
)

func TestVoteAccumulatesChosenSideOnly(t *testing.T) {
	p := &Proposal{ID: "p", State: "voting", Quorum: 1}
	if err := Vote(p, 700, true); err != nil {
		t.Fatalf("vote for: %v", err)
	}
	if err := Vote(p, 200, false); err != nil {
		t.Fatalf("vote against: %v", err)
	}
	if p.ForVotes != 700 || p.AgainstVotes != 200 {
		t.Fatalf("sides = %d/%d, want 700/200", p.ForVotes, p.AgainstVotes)
	}
}

func TestVoteRejectsNonPositiveWeightAndClosedProposal(t *testing.T) {
	p := &Proposal{ID: "p", State: "voting", Quorum: 1}
	if err := Vote(p, 0, true); err == nil {
		t.Fatal("zero weight must be rejected")
	}
	if err := Vote(p, -5, true); err == nil {
		t.Fatal("negative weight must be rejected")
	}
	if p.ForVotes != 0 || p.AgainstVotes != 0 {
		t.Fatalf("rejected votes changed totals: %d/%d", p.ForVotes, p.AgainstVotes)
	}

	closed := &Proposal{ID: "c", State: "passed", Quorum: 1}
	if err := Vote(closed, 1, true); err == nil {
		t.Fatal("non-voting proposal must reject votes")
	}
	if closed.ForVotes != 0 {
		t.Fatalf("vote on closed proposal changed totals: %d", closed.ForVotes)
	}
}

func TestVoteSideOverflowAtInt64Boundary(t *testing.T) {
	max := int64(math.MaxInt64)

	// 恰好达到上限必须接受，不能误拒绝。
	p := &Proposal{ID: "p", State: "voting", Quorum: 1}
	if err := Vote(p, max, true); err != nil {
		t.Fatalf("vote reaching exactly MaxInt64 rejected: %v", err)
	}
	if p.ForVotes != max || p.AgainstVotes != 0 {
		t.Fatalf("totals = %d/%d, want %d/0", p.ForVotes, p.AgainstVotes, max)
	}

	// 再多 1：明确报权重溢出，两侧与提案其余内容保持原样。
	err := Vote(p, 1, true)
	if err == nil || !strings.Contains(err.Error(), "overflow") {
		t.Fatalf("overflow vote must report weight overflow, got %v", err)
	}
	if p.ForVotes != max || p.AgainstVotes != 0 {
		t.Fatalf("failed overflow vote changed totals: %d/%d", p.ForVotes, p.AgainstVotes)
	}
	if p.State != "voting" {
		t.Fatalf("failed overflow vote changed state: %s", p.State)
	}

	// 失败不挪到另一侧：反对侧仍能被容纳。
	if err := Vote(p, 1, false); err != nil {
		t.Fatalf("against-side vote after for-side overflow must be accepted: %v", err)
	}
	if p.ForVotes != max || p.AgainstVotes != 1 {
		t.Fatalf("totals = %d/%d, want %d/1", p.ForVotes, p.AgainstVotes, max)
	}

	// 先差 10 到顶：11 溢出失败后，10 仍应被接受并恰好到顶，不截断也不变负。
	q := &Proposal{ID: "q", State: "voting", Quorum: 1}
	if err := Vote(q, max-10, false); err != nil {
		t.Fatal(err)
	}
	if err := Vote(q, 11, false); err == nil {
		t.Fatal("vote exceeding the cap must be rejected")
	}
	if q.AgainstVotes != max-10 {
		t.Fatalf("rejected vote saturated/clamped totals: %d", q.AgainstVotes)
	}
	if err := Vote(q, 10, false); err != nil {
		t.Fatalf("fittable vote after overflow failure must be accepted: %v", err)
	}
	if q.AgainstVotes != max {
		t.Fatalf("against = %d, want %d", q.AgainstVotes, max)
	}
}

func TestTallyQuorumWithOverflowingTurnout(t *testing.T) {
	max := int64(math.MaxInt64)

	// 赞成=上限、反对=1、法定人数=上限：两侧合计溢出但参与量真实达标，passed。
	p := &Proposal{ID: "p", State: "voting", ForVotes: max, AgainstVotes: 1, Quorum: max}
	if got := Tally(p); got != "passed" {
		t.Fatalf("Tally = %q, want passed", got)
	}

	// 两侧均为上限：参与量很大但平票，必须 rejected。
	tie := &Proposal{ID: "tie", State: "voting", ForVotes: max, AgainstVotes: max, Quorum: max}
	if got := Tally(tie); got != "rejected" {
		t.Fatalf("Tally = %q, want rejected on tie", got)
	}

	// 反对侧单独已超法定人数但赞成领先不可能；赞成 1、反对上限：达标但赞成少，rejected。
	againstLead := &Proposal{ID: "a", State: "voting", ForVotes: 1, AgainstVotes: max, Quorum: max}
	if got := Tally(againstLead); got != "rejected" {
		t.Fatalf("Tally = %q, want rejected", got)
	}

	// 参与量恰好比法定人数少 1：即使赞成领先也拒绝（合计不溢出）。
	below := &Proposal{ID: "e", State: "voting", ForVotes: max - 2, AgainstVotes: 1, Quorum: max}
	if got := Tally(below); got != "rejected" {
		t.Fatalf("Tally = %q, want rejected (turnout == quorum-1)", got)
	}
	// 参与量等于法定人数算达到；赞成严格多 => passed（合计恰好到上限）。
	atQuorum := &Proposal{ID: "q", State: "voting", ForVotes: max - 1, AgainstVotes: 1, Quorum: max}
	if got := Tally(atQuorum); got != "passed" {
		t.Fatalf("Tally = %q, want passed (turnout == quorum)", got)
	}

	// 参与不足即使赞成领先也拒绝（合计不溢出的普通情形）。
	low := &Proposal{ID: "l", State: "voting", ForVotes: 400, AgainstVotes: 0, Quorum: 700}
	if got := Tally(low); got != "rejected" {
		t.Fatalf("Tally = %q, want rejected on low turnout", got)
	}

	// 计票只返回结论：不改写状态、票数。
	if p.State != "voting" || p.ForVotes != max || p.AgainstVotes != 1 {
		t.Fatalf("Tally mutated proposal: %+v", p)
	}
}

func TestTallyReturnsCurrentStateWhenNotVoting(t *testing.T) {
	for _, state := range []string{"passed", "rejected", "executed"} {
		p := &Proposal{ID: "p", State: state, ForVotes: 1, Quorum: 1}
		if got := Tally(p); got != state {
			t.Fatalf("Tally on %s = %q, want %s", state, got, state)
		}
	}
}
