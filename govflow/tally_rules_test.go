package govflow

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 计票规则只维护一份：首次计票、打开校验、查询投影都必须得到相同结论。
// 本文件聚焦该规则在数值边界、重复调用与状态损坏下的行为。

// 合法成员总权重恰好为有符号 64 位整数上限时，汇总参与量与通过结论不得越界改变。
func TestTallyAtInt64WeightBoundary(t *testing.T) {
	max := int64(math.MaxInt64)

	// 单成员持有全部 MaxInt64 并投赞成：参与量恰好 MaxInt64 == 法定人数，
	// 赞成严格多于反对（0），必须通过；重开与执行后结论保持。
	path := filepath.Join(t.TempDir(), "treasury.json")
	store, err := InitTreasury(path, max)
	if err != nil {
		t.Fatal(err)
	}
	allFor := &CreateVoteInput{
		ID: "gip-max-for", Members: []VoteMember{{ID: "a", Weight: max}},
		Quorum: max, StartAt: 0, Deadline: 10, TimelockEnd: 10,
		Actions: []string{"transfer:x:1"},
	}
	mustCreateVote(t, store, allFor)
	if _, err := store.CastVote("gip-max-for", "a", true, 5); err != nil {
		t.Fatal(err)
	}
	res, err := store.TallyVote("gip-max-for", 10)
	if err != nil {
		t.Fatalf("tally max-weight for: %v", err)
	}
	if !res.Passed || res.ForWeight != max || res.AgainstWeight != 0 || res.Turnout != max || res.Quorum != max {
		t.Fatalf("max-weight tally = %+v, want passed with turnout=MaxInt64", res)
	}
	if _, err := store.Execute("gip-max-for", 10); err != nil {
		t.Fatalf("execute max-weight proposal: %v", err)
	}
	if bal, _ := store.TreasuryBalance(); bal != max-1 {
		t.Fatalf("treasury after 1 transfer = %d, want MaxInt64-1", bal)
	}
	store.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen max-weight state: %v", err)
	}
	defer reopened.Close()
	v, ok, err := reopened.VoteProposal("gip-max-for")
	if err != nil || !ok {
		t.Fatalf("query after reopen: %v ok=%v", err, ok)
	}
	if v.State != "executed" || v.Tally == nil || !v.Tally.Passed || v.Tally.Turnout != max {
		t.Fatalf("max-weight verdict changed after reopen: state=%s tally=%+v", v.State, v.Tally)
	}
	// 已执行后再次计票仍返回原通过结论，提案状态不得退回 passed。
	again, err := reopened.TallyVote("gip-max-for", 5) // 即使传入截止前时间
	if err != nil || !again.Passed || again.TalliedAt != 10 {
		t.Fatalf("repeat tally after execute: %+v err=%v", again, err)
	}
	if v2, _, _ := reopened.VoteProposal("gip-max-for"); v2.State != "executed" {
		t.Fatalf("state regressed to %s", v2.State)
	}

	// 同样的总权重上限，但没有任何人投票：参与量为 0，即使赞成“领先”也必须拒绝。
	store2, _ := openTempStore(t, 0)
	defer store2.Close()
	noVotes := &CreateVoteInput{
		ID: "gip-max-none", Members: []VoteMember{{ID: "a", Weight: max}},
		Quorum: max, StartAt: 0, Deadline: 10, TimelockEnd: 10,
	}
	mustCreateVote(t, store2, noVotes)
	res2, err := store2.TallyVote("gip-max-none", 10)
	if err != nil || res2.Passed || res2.Turnout != 0 {
		t.Fatalf("no-vote max-weight tally = %+v err=%v, want rejected turnout=0", res2, err)
	}
	if v3, _, _ := store2.VoteProposal("gip-max-none"); v3.State != "rejected" || v3.Tally.Passed {
		t.Fatalf("no-vote proposal state=%s tally.Passed=%v, want rejected/false", v3.State, v3.Tally.Passed)
	}

	// 两侧合计恰好 MaxInt64、赞成仅多 1：达法定人数且严格多数 => 通过。
	hi := max / 2   // 4611686018427387903
	big := max - hi // 4611686018427387904
	store3, _ := openTempStore(t, 0)
	defer store3.Close()
	squeaked := &CreateVoteInput{
		ID: "gip-max-squeak", Members: []VoteMember{{ID: "a", Weight: big}, {ID: "b", Weight: hi}},
		Quorum: max, StartAt: 0, Deadline: 10, TimelockEnd: 10,
	}
	mustCreateVote(t, store3, squeaked)
	if _, err := store3.CastVote("gip-max-squeak", "a", true, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := store3.CastVote("gip-max-squeak", "b", false, 5); err != nil {
		t.Fatal(err)
	}
	res3, err := store3.TallyVote("gip-max-squeak", 10)
	if err != nil {
		t.Fatalf("tally squeaked majority: %v", err)
	}
	if !res3.Passed || res3.Turnout != max || res3.ForWeight != big || res3.AgainstWeight != hi {
		t.Fatalf("squeaked majority = %+v, want passed with turnout=MaxInt64", res3)
	}

	// 两侧各持 (MaxInt64-1)/2，合计 MaxInt64-1 且平票：参与量巨大但平票，必须拒绝，
	// 整个过程不得出现任何上溢加法。
	store4, _ := openTempStore(t, 0)
	defer store4.Close()
	giantTie := &CreateVoteInput{
		ID: "gip-max-tie", Members: []VoteMember{{ID: "x", Weight: hi}, {ID: "y", Weight: hi}},
		Quorum: 2 * hi, StartAt: 0, Deadline: 10, TimelockEnd: 10,
	}
	mustCreateVote(t, store4, giantTie)
	if _, err := store4.CastVote("gip-max-tie", "x", true, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := store4.CastVote("gip-max-tie", "y", false, 5); err != nil {
		t.Fatal(err)
	}
	res4, err := store4.TallyVote("gip-max-tie", 10)
	if err != nil || res4.Passed {
		t.Fatalf("giant tie = %+v err=%v, want rejected", res4, err)
	}
	if res4.Turnout != 2*hi || res4.ForWeight != hi || res4.AgainstWeight != hi {
		t.Fatalf("giant tie aggregates = %+v, want equal MaxInt64-1 halves", res4)
	}
}

// 已有首次结论后，即使再次传入截止前的时间，也返回首次结论与首次计票时间。
func TestRepeatTallyBeforeDeadlineReturnsFirstResult(t *testing.T) {
	store, _ := openTempStore(t, 1000)
	defer store.Close()
	mustCreateVote(t, store, baseVoteInput("gip-early-repeat"))
	if _, err := store.CastVote("gip-early-repeat", "alice", true, 100); err != nil {
		t.Fatal(err)
	}
	first, err := store.TallyVote("gip-early-repeat", 200)
	if err != nil || !first.Passed || first.TalliedAt != 200 {
		t.Fatalf("first tally: %+v err=%v", first, err)
	}
	// now=100 甚至 now=0 都早于截止 200，但已有结论必须原样返回，且不报“时间未到”。
	for _, early := range []int64{199, 100, 0} {
		got, err := store.TallyVote("gip-early-repeat", early)
		if err != nil {
			t.Fatalf("repeat tally at now=%d: %v", early, err)
		}
		if got.TalliedAt != 200 || !got.Passed || got.ForWeight != 600 || got.AgainstWeight != 0 {
			t.Fatalf("repeat tally at now=%d changed verdict: %+v", early, got)
		}
	}
	v, _, _ := store.VoteProposal("gip-early-repeat")
	if v.State != "passed" || v.Tally == nil || v.Tally.TalliedAt != 200 {
		t.Fatalf("state after early repeat: %s %+v", v.State, v.Tally)
	}
}

// 首次计票只保存结论与时间：票据明细（内容与顺序）与资金库余额都不改变。
func TestTallyDoesNotTouchBallotsOrFunds(t *testing.T) {
	store, _ := openTempStore(t, 1000)
	defer store.Close()
	in := baseVoteInput("gip-readonly-tally")
	mustCreateVote(t, store, in)
	if _, err := store.CastVote("gip-readonly-tally", "dave", false, 120); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CastVote("gip-readonly-tally", "alice", true, 130); err != nil {
		t.Fatal(err)
	}
	before, _, _ := store.VoteProposal("gip-readonly-tally")
	wantBallots := make([]BallotView, len(before.Ballots))
	copy(wantBallots, before.Ballots)
	treasuryBefore, _ := store.TreasuryBalance()
	balancesBefore, _ := store.Balances()

	if _, err := store.TallyVote("gip-readonly-tally", 200); err != nil {
		t.Fatalf("tally: %v", err)
	}

	after, _, _ := store.VoteProposal("gip-readonly-tally")
	if len(after.Ballots) != len(wantBallots) {
		t.Fatalf("ballot count changed: %d -> %d", len(wantBallots), len(after.Ballots))
	}
	for i := range wantBallots {
		if after.Ballots[i] != wantBallots[i] {
			t.Fatalf("ballot %d changed: %+v -> %+v", i, wantBallots[i], after.Ballots[i])
		}
	}
	if got, _ := store.TreasuryBalance(); got != treasuryBefore {
		t.Fatalf("treasury changed on tally: %d -> %d", treasuryBefore, got)
	}
	balancesAfter, _ := store.Balances()
	if len(balancesAfter) != len(balancesBefore) {
		t.Fatalf("recipient balances changed on tally: %v -> %v", balancesBefore, balancesAfter)
	}
	for acct, bal := range balancesBefore {
		if balancesAfter[acct] != bal {
			t.Fatalf("balance of %q changed on tally", acct)
		}
	}
}

// 计票结果与提案查询（含快照）在通过、参与不足、平票三种结论上始终一致；
// 通过提案执行后再次计票与查询仍显示原通过结论，状态不退回 passed。
func TestTallyResultAndQueryAlwaysAgree(t *testing.T) {
	store, _ := openTempStore(t, 1000)
	defer store.Close()

	// 通过：alice 600 赞成，quorum 600。
	pass := baseVoteInput("gip-agree-pass")
	mustCreateVote(t, store, pass)
	if _, err := store.CastVote("gip-agree-pass", "alice", true, 150); err != nil {
		t.Fatal(err)
	}
	// 参与不足拒绝：quorum 1000，仅 alice 600。
	low := baseVoteInput("gip-agree-low")
	low.Quorum = 1000
	mustCreateVote(t, store, low)
	if _, err := store.CastVote("gip-agree-low", "alice", true, 150); err != nil {
		t.Fatal(err)
	}
	// 平票拒绝：对称 500/500，quorum 1000。
	tie := &CreateVoteInput{
		ID: "gip-agree-tie", Members: []VoteMember{{ID: "x", Weight: 500}, {ID: "y", Weight: 500}},
		Quorum: 1000, StartAt: 0, Deadline: 10, TimelockEnd: 300,
		Actions: []string{"transfer:audits:100"},
	}
	mustCreateVote(t, store, tie)
	if _, err := store.CastVote("gip-agree-tie", "x", true, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CastVote("gip-agree-tie", "y", false, 5); err != nil {
		t.Fatal(err)
	}

	want := map[string]bool{"gip-agree-pass": true, "gip-agree-low": false, "gip-agree-tie": false}
	wantState := map[string]string{"gip-agree-pass": "passed", "gip-agree-low": "rejected", "gip-agree-tie": "rejected"}
	for id, passed := range want {
		res, err := store.TallyVote(id, 200)
		if err != nil {
			t.Fatalf("tally %s: %v", id, err)
		}
		if res.Passed != passed {
			t.Fatalf("%s tally.Passed=%v want %v", id, res.Passed, passed)
		}
		v, _, _ := store.VoteProposal(id)
		if v.Tally == nil || v.Tally.Passed != res.Passed ||
			v.Tally.ForWeight != res.ForWeight || v.Tally.AgainstWeight != res.AgainstWeight ||
			v.Tally.Turnout != res.Turnout {
			t.Fatalf("%s query tally %+v disagrees with result %+v", id, v.Tally, res)
		}
		if v.State != wantState[id] {
			t.Fatalf("%s state=%s want %s", id, v.State, wantState[id])
		}
	}

	// 快照查询中的结论同样一致。
	snap, err := store.ProposalsSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]*VoteProposalView{}
	for _, v := range snap.Voting {
		byID[v.ID] = v
	}
	for id, passed := range want {
		if v := byID[id]; v.Tally == nil || v.Tally.Passed != passed {
			t.Fatalf("snapshot %s tally=%+v, want Passed=%v", id, v.Tally, passed)
		}
	}

	// 通过提案执行后：状态 executed，首次通过结论不变；再次计票不回退状态。
	if _, err := store.Execute("gip-agree-pass", 300); err != nil {
		t.Fatalf("execute: %v", err)
	}
	v, _, _ := store.VoteProposal("gip-agree-pass")
	if v.State != "executed" || !v.Tally.Passed {
		t.Fatalf("post-execute state=%s passed=%v", v.State, v.Tally.Passed)
	}
	again, err := store.TallyVote("gip-agree-pass", 0) // 传入截止前时间
	if err != nil || !again.Passed {
		t.Fatalf("post-execute repeat tally: %+v err=%v", again, err)
	}
	if v2, _, _ := store.VoteProposal("gip-agree-pass"); v2.State != "executed" {
		t.Fatalf("state regressed from executed to %q after repeat tally", v2.State)
	}
}

// 打开已保存提案时必须依据名单、委托与逐票明细确认结论可信：
// 保存的状态/赞成反对权重/票重与明细不一致，一律拒绝读取，
// 错误原因可区分，且原文件保持不变（不用重算结果修正）。
func TestReopenRejectsInconsistentTallyConclusion(t *testing.T) {
	dir := t.TempDir()

	// 构造一份已通过（alice 600 赞成）与一份平票拒绝的状态文件作为篡改基底。
	passPath := filepath.Join(dir, "passed.json")
	passStore, err := InitTreasury(passPath, 1000)
	if err != nil {
		t.Fatal(err)
	}
	mustCreateVote(t, passStore, baseVoteInput("gip-pass"))
	if _, err := passStore.CastVote("gip-pass", "alice", true, 150); err != nil {
		t.Fatal(err)
	}
	if _, err := passStore.TallyVote("gip-pass", 200); err != nil {
		t.Fatal(err)
	}
	passStore.Close()

	tiePath := filepath.Join(dir, "tie.json")
	tieStore, err := InitTreasury(tiePath, 1000)
	if err != nil {
		t.Fatal(err)
	}
	tieIn := &CreateVoteInput{
		ID: "gip-tie", Members: []VoteMember{{ID: "x", Weight: 500}, {ID: "y", Weight: 500}},
		Quorum: 1000, StartAt: 0, Deadline: 10, TimelockEnd: 10,
	}
	mustCreateVote(t, tieStore, tieIn)
	if _, err := tieStore.CastVote("gip-tie", "x", true, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := tieStore.CastVote("gip-tie", "y", false, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := tieStore.TallyVote("gip-tie", 10); err != nil {
		t.Fatal(err)
	}
	tieStore.Close()

	mutate := func(t *testing.T, base []byte, id string, fn func(p map[string]any)) []byte {
		t.Helper()
		var doc map[string]any
		if err := json.Unmarshal(base, &doc); err != nil {
			t.Fatal(err)
		}
		fn(doc["vote_proposals"].(map[string]any)[id].(map[string]any))
		raw, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		return append(raw, '\n')
	}
	passGood, _ := os.ReadFile(passPath)
	tieGood, _ := os.ReadFile(tiePath)

	cases := []struct {
		name    string
		base    []byte
		id      string
		fn      func(p map[string]any)
		wantSub string // 必须出现在错误信息中，用于区分损坏原因
	}{
		{
			name: "passed ballots but state flipped to rejected",
			base: passGood, id: "gip-pass",
			fn:      func(p map[string]any) { p["state"] = "rejected" },
			wantSub: "does not match quorum/majority replay",
		},
		{
			name: "tie rejected but state flipped to passed",
			base: tieGood, id: "gip-tie",
			fn:      func(p map[string]any) { p["state"] = "passed" },
			wantSub: "does not match quorum/majority replay",
		},
		{
			name: "tally for-weight disagrees with ballots",
			base: passGood, id: "gip-pass",
			fn:      func(p map[string]any) { p["tally"].(map[string]any)["for_weight"] = 601 },
			wantSub: "do not replay ballots",
		},
		{
			name: "ballot weight does not match delegated roster weight",
			base: passGood, id: "gip-pass",
			fn:      func(p map[string]any) { p["ballots"].([]any)[0].(map[string]any)["weight"] = 601 },
			wantSub: "does not match roster delegation",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := mutate(t, tc.base, tc.id, tc.fn)
			bad := filepath.Join(dir, "bad-"+strings.ReplaceAll(tc.name, " ", "-")+".json")
			if err := os.WriteFile(bad, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			s, err := Open(bad)
			if !errors.Is(err, ErrStateCorrupt) {
				if s != nil {
					s.Close()
				}
				t.Fatalf("Open err=%v, want ErrStateCorrupt", err)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not distinguish cause %q", err.Error(), tc.wantSub)
			}
			// 不接受“重算修正后继续”：原损坏文件必须原样保留。
			if got, rerr := os.ReadFile(bad); rerr != nil || string(got) != string(raw) {
				t.Fatalf("corrupt file was modified or repaired")
			}
		})
	}

	// 合法文件（含已计票提案）仍可正常打开。
	if s, err := Open(passPath); err != nil {
		t.Fatalf("valid tallied file rejected: %v", err)
	} else {
		defer s.Close()
	}
}
