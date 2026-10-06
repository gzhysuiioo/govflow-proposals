package govflow

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// assertDaveOnlyAfterFailedAliceVote 汇总“alice 的保存失败投票不留任何痕迹”
// 的全部断言：查询仍只有 dave 在 120 投出的 400 权重反对票；alice 归集的
// 600 权重不能出现在任何已投记录里；提案仍处于投票中、没有计票结论；
// 成员权重、委托路径、窗口时间与动作原文都保持创建时的内容；
// 资金余额不变，也没有执行凭据。
func assertDaveOnlyAfterFailedAliceVote(t *testing.T, store *Store, id string) {
	t.Helper()
	v, ok, err := store.VoteProposal(id)
	if err != nil || !ok {
		t.Fatalf("VoteProposal: ok=%v err=%v", ok, err)
	}
	if v.State != "voting" || v.Tally != nil {
		t.Fatalf("state=%s tally=%+v, want voting with no tally", v.State, v.Tally)
	}
	if len(v.Ballots) != 1 {
		t.Fatalf("ballots = %+v, want only dave's ballot", v.Ballots)
	}
	b := v.Ballots[0]
	if b.Representative != "dave" || b.Weight != 400 || b.Support || b.VotedAt != 120 {
		t.Fatalf("dave ballot = %+v, want representative=dave weight=400 against voted_at=120", b)
	}
	// 创建参数全部保持：法定人数、窗口、时间锁、动作原文与总权重。
	if v.Quorum != 600 || v.StartAt != 100 || v.Deadline != 200 || v.TimelockEnd != 300 || v.TotalWeight != 1000 {
		t.Fatalf("proposal parameters changed: %+v", v)
	}
	if !sameActions(v.Actions, []string{"transfer:audits:100"}) {
		t.Fatalf("actions = %v, want original action text", v.Actions)
	}
	// 成员原始权重与完整委托路径不受失败尝试影响。
	got := memberPaths(v)
	want := map[string]MemberView{
		"alice": {ID: "alice", Weight: 300, Path: []string{"alice"}, Delegate: "alice", Direct: ""},
		"bob":   {ID: "bob", Weight: 200, Path: []string{"bob", "alice"}, Delegate: "alice", Direct: "alice"},
		"carol": {ID: "carol", Weight: 100, Path: []string{"carol", "alice"}, Delegate: "alice", Direct: "alice"},
		"dave":  {ID: "dave", Weight: 400, Path: []string{"dave"}, Delegate: "dave", Direct: ""},
	}
	assertMemberViews(t, "after failed save", got, want)
	// 投票与失败不改变资金余额，也不生成执行凭据。
	if bal, err := store.TreasuryBalance(); err != nil || bal != 1000 {
		t.Fatalf("treasury = %d err=%v, want untouched 1000", bal, err)
	}
	if all, err := store.Balances(); err != nil || len(all) != 0 {
		t.Fatalf("recipient balances = %v err=%v, want none", all, err)
	}
	if receipts, err := store.Receipts(); err != nil || len(receipts) != 0 {
		t.Fatalf("receipts = %v err=%v, voting must not produce receipts", receipts, err)
	}
}

// TestVotePreReplaceFailureLeavesNoPartialRecord：带委托的提案上，dave 已在
// 120 投出反对票；alice 在 150 的首次赞成若在状态文件被替换之前保存失败，
// 必须返回具体保存原因而不是成功票据，失败尝试不落任何部分记录、不占用
// alice 的首次投票机会。保存条件恢复后，160 的赞成被当作首次有效投票接受，
// 票据按 dave、alice 的顺序归集；截止计票得到赞成 600、反对 400、参与 1000
// 并通过。同选择重试与改投冲突的既有规则保持不变。
func TestVotePreReplaceFailureLeavesNoPartialRecord(t *testing.T) {
	store, path := openTempStore(t, 1000)
	defer store.Close()
	const id = "gip-vote-savefail"
	mustCreateVote(t, store, baseVoteInput(id))
	if _, err := store.CastVote(id, "dave", false, 120); err != nil {
		t.Fatalf("dave vote: %v", err)
	}

	// 记录失败前的完整状态文件字节：保存失败后它必须原样保留。
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 目录不可写：临时文件都无法创建，提交在状态文件替换之前失败。
	makeDirUnwritable(t, path)
	ballot, err := store.CastVote(id, "alice", true, 150)
	if err == nil {
		t.Fatal("expected save failure, got nil error")
	}
	// 返回的是具体保存原因，而不是成功票据、投票业务拒绝或“已写入”告警。
	if ballot != nil {
		t.Fatalf("pre-replacement failure must not return a ballot: %+v", ballot)
	}
	if errors.Is(err, ErrDurabilityUnconfirmed) {
		t.Fatalf("pre-replacement failure must not be reported as durability warning: %v", err)
	}
	if errors.Is(err, ErrVoteRejected) {
		t.Fatalf("a valid first vote must not be rejected as a business rule: %v", err)
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("error must carry the concrete save cause, got %v", err)
	}

	// 保存失败后不能遗留临时文件或部分文件。
	if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".govflow-state-*")); len(leftovers) != 0 {
		t.Fatalf("failed commit left temporary file(s): %v", leftovers)
	}

	// 即使目录仍处于不可写状态，只读查询看到的也是失败前的完整状态。
	assertDaveOnlyAfterFailedAliceVote(t, store, id)

	// 原状态文件保留失败前的完整内容（字节一致）。
	if after, rerr := os.ReadFile(path); rerr != nil || string(after) != string(before) {
		t.Fatalf("state file changed despite pre-replacement save failure")
	}

	// 恢复保存条件后，重新打开必须读到完全相同的结果。
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("restore dir: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after failed save: %v", err)
	}
	defer reopened.Close()
	assertDaveOnlyAfterFailedAliceVote(t, reopened, id)

	// 160 的赞成被当作首次有效投票：600 权重归集给 alice，时间是 160，
	// 绝不沿用失败尝试的 150，也不能报“已经投过票”。
	first, err := reopened.CastVote(id, "alice", true, 160)
	if err != nil {
		t.Fatalf("alice first valid vote after recovery: %v", err)
	}
	if first.Representative != "alice" || first.Weight != 600 || !first.Support || first.VotedAt != 160 {
		t.Fatalf("first ballot = %+v, want alice weight=600 for voted_at=160", first)
	}

	// 同选择重试（即使传入更晚时间）始终返回这张首次记录。
	retry, err := reopened.CastVote(id, "alice", true, 180)
	if err != nil {
		t.Fatalf("same-choice retry: %v", err)
	}
	if retry.VotedAt != 160 || retry.Weight != 600 || !retry.Support {
		t.Fatalf("same-choice retry = %+v, want first record voted_at=160", retry)
	}
	// 改投反对仍按既有冲突规则拒绝，不静默覆盖。
	if _, err := reopened.CastVote(id, "alice", false, 160); !errors.Is(err, ErrProposalConflict) {
		t.Fatalf("changing vote err=%v, want ErrProposalConflict", err)
	}

	// 票据顺序仍是 dave 在前、alice 在后；失败尝试没有留下另一张票。
	v, _, err := reopened.VoteProposal(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Ballots) != 2 {
		t.Fatalf("ballots = %+v, want exactly two", v.Ballots)
	}
	if v.Ballots[0].Representative != "dave" || v.Ballots[0].VotedAt != 120 || v.Ballots[0].Weight != 400 || v.Ballots[0].Support {
		t.Fatalf("first ballot = %+v, want dave against 400 at 120", v.Ballots[0])
	}
	if v.Ballots[1].Representative != "alice" || v.Ballots[1].VotedAt != 160 || v.Ballots[1].Weight != 600 || !v.Ballots[1].Support {
		t.Fatalf("second ballot = %+v, want alice for 600 at 160", v.Ballots[1])
	}

	// 截止时刻计票：赞成 600、反对 400、参与 1000，达到法定人数且赞成严格
	// 多于反对 => 通过；没有失败尝试带来的额外票据或权重。
	res, err := reopened.TallyVote(id, 200)
	if err != nil {
		t.Fatalf("tally: %v", err)
	}
	if !res.Passed || res.ForWeight != 600 || res.AgainstWeight != 400 ||
		res.Turnout != 1000 || res.Quorum != 600 || res.TalliedAt != 200 {
		t.Fatalf("tally = %+v, want passed for=600 against=400 turnout=1000 at 200", res)
	}
	// 投票与计票本身不改变资金余额，也不生成执行凭据。
	if bal, _ := reopened.TreasuryBalance(); bal != 1000 {
		t.Fatalf("treasury = %d after tally, want 1000", bal)
	}
	if receipts, _ := reopened.Receipts(); len(receipts) != 0 {
		t.Fatalf("tally produced receipts: %+v", receipts)
	}
	if all, _ := reopened.Balances(); len(all) != 0 {
		t.Fatalf("tally changed recipient balances: %v", all)
	}

	// 重开后仍是同两张票与同一个通过结论。
	if err := reopened.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after tally: %v", err)
	}
	defer reopened2.Close()
	v2, ok, err := reopened2.VoteProposal(id)
	if err != nil || !ok {
		t.Fatalf("query after reopen: ok=%v err=%v", ok, err)
	}
	if v2.State != "passed" || len(v2.Ballots) != 2 ||
		v2.Ballots[0].Representative != "dave" || v2.Ballots[1].Representative != "alice" {
		t.Fatalf("reopened view = %+v", v2)
	}
	if v2.Tally == nil || !v2.Tally.Passed || v2.Tally.ForWeight != 600 ||
		v2.Tally.AgainstWeight != 400 || v2.Tally.Turnout != 1000 {
		t.Fatalf("reopened tally = %+v", v2.Tally)
	}
}

// TestVotePreReplaceFailureThenDeadlineRejectsFirstVote：同一次保存失败后，
// 若直到截止时刻 200 才再次提交，alice 的首次票必须按截止边界（窗口为
// [100,200)）拒绝，dave 的旧反对票照常保留；到期计票只能得到参与权重 400
// 的拒绝结论。投票与计票同样不改变资金余额、不生成执行凭据。
func TestVotePreReplaceFailureThenDeadlineRejectsFirstVote(t *testing.T) {
	store, path := openTempStore(t, 1000)
	defer store.Close()
	const id = "gip-vote-savefail-late"
	mustCreateVote(t, store, baseVoteInput(id))
	if _, err := store.CastVote(id, "dave", false, 120); err != nil {
		t.Fatalf("dave vote: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	makeDirUnwritable(t, path)
	if ballot, err := store.CastVote(id, "alice", true, 150); err == nil {
		t.Fatalf("save must fail, got ballot %+v", ballot)
	} else if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("error must carry the concrete save cause, got %v", err)
	}
	assertDaveOnlyAfterFailedAliceVote(t, store, id)
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("restore dir: %v", err)
	}
	if got, rerr := os.ReadFile(path); rerr != nil || string(got) != string(before) {
		t.Fatalf("state file changed despite save failure")
	}

	// 一直到截止时刻 200 才再次提交：首次票按截止边界拒绝，仍不占用投票机会
	// 的语义在这里体现为“它从未成功过”，但此刻窗口已关，任何票都进不来。
	if _, err := store.CastVote(id, "alice", true, 200); !errors.Is(err, ErrVoteRejected) {
		t.Fatalf("vote at deadline (exclusive) err=%v, want ErrVoteRejected", err)
	}
	// 旧反对票照常保留，alice 的 600 权重仍不在任何已投记录里。
	assertDaveOnlyAfterFailedAliceVote(t, store, id)

	// 到期计票：赞成 0、反对 400、参与 400，未达法定人数 => 拒绝。
	res, err := store.TallyVote(id, 200)
	if err != nil {
		t.Fatalf("tally: %v", err)
	}
	if res.Passed || res.ForWeight != 0 || res.AgainstWeight != 400 ||
		res.Turnout != 400 || res.Quorum != 600 || res.TalliedAt != 200 {
		t.Fatalf("tally = %+v, want rejected for=0 against=400 turnout=400 at 200", res)
	}
	v, _, _ := store.VoteProposal(id)
	if v.State != "rejected" || v.Tally == nil || v.Tally.Passed {
		t.Fatalf("state=%s tally=%+v, want rejected/not-passed", v.State, v.Tally)
	}
	if len(v.Ballots) != 1 || v.Ballots[0].Representative != "dave" {
		t.Fatalf("ballots = %+v, want dave's ballot only", v.Ballots)
	}
	// 资金余额不变，也没有执行凭据。
	if bal, _ := store.TreasuryBalance(); bal != 1000 {
		t.Fatalf("treasury = %d, want 1000", bal)
	}
	if receipts, _ := store.Receipts(); len(receipts) != 0 {
		t.Fatalf("receipts = %+v, want none", receipts)
	}
}
