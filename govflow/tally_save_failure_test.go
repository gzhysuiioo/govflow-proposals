package govflow

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tallySplitInput 构造本回归专用的投票提案：窗口 [100,200)、时间锁 300、
// 法定人数 600；bob、carol 把权重委托给 alice，使两个最终代表分别归集
// 700（alice）与 300（dave）权重，总权重 1000；带一项有效转账动作。
func tallySplitInput(id string) *CreateVoteInput {
	return &CreateVoteInput{
		ID: id,
		Members: []VoteMember{
			{ID: "alice", Weight: 400},
			{ID: "bob", Weight: 200},
			{ID: "carol", Weight: 100},
			{ID: "dave", Weight: 300},
		},
		Delegations: []Delegation{{From: "bob", To: "alice"}, {From: "carol", To: "alice"}},
		Quorum:      600,
		StartAt:     100,
		Deadline:    200,
		TimelockEnd: 300,
		Actions:     []string{"transfer:audits:100"},
	}
}

// castSplitBallots 让两个最终代表在窗口内完成投票：alice 在 150 投出归集
// 权重 700 的赞成票，dave 在 160 投出 300 的反对票，顺序即 alice 在前。
func castSplitBallots(t *testing.T, store *Store, id string) {
	t.Helper()
	b, err := store.CastVote(id, "alice", true, 150)
	if err != nil {
		t.Fatalf("alice vote: %v", err)
	}
	if b.Weight != 700 || !b.Support || b.VotedAt != 150 {
		t.Fatalf("alice ballot = %+v, want weight=700 for voted_at=150", b)
	}
	b, err = store.CastVote(id, "dave", false, 160)
	if err != nil {
		t.Fatalf("dave vote: %v", err)
	}
	if b.Weight != 300 || b.Support || b.VotedAt != 160 {
		t.Fatalf("dave ballot = %+v, want weight=300 against voted_at=160", b)
	}
}

// assertSplitRoster 断言提案创建时的成员、委托与动作内容原样保留：
// 四名成员原始权重与完整委托路径、法定人数、窗口、时间锁、动作原文与总权重。
func assertSplitRoster(t *testing.T, prefix string, v *VoteProposalView) {
	t.Helper()
	if v.Source != "vote" {
		t.Fatalf("%s: source=%s, want vote", prefix, v.Source)
	}
	if v.Quorum != 600 || v.StartAt != 100 || v.Deadline != 200 ||
		v.TimelockEnd != 300 || v.TotalWeight != 1000 {
		t.Fatalf("%s: proposal parameters changed: %+v", prefix, v)
	}
	if !sameActions(v.Actions, []string{"transfer:audits:100"}) {
		t.Fatalf("%s: actions=%v, want original transfer action", prefix, v.Actions)
	}
	want := map[string]MemberView{
		"alice": {ID: "alice", Weight: 400, Path: []string{"alice"}, Delegate: "alice", Direct: ""},
		"bob":   {ID: "bob", Weight: 200, Path: []string{"bob", "alice"}, Delegate: "alice", Direct: "alice"},
		"carol": {ID: "carol", Weight: 100, Path: []string{"carol", "alice"}, Delegate: "alice", Direct: "alice"},
		"dave":  {ID: "dave", Weight: 300, Path: []string{"dave"}, Delegate: "dave", Direct: ""},
	}
	assertMemberViews(t, prefix, memberPaths(v), want)
}

// assertSplitBallots 断言两张已保存票据的代表、归集权重、选择、首次投票
// 时间与先后顺序全部保留：alice 700 赞成（150）在前，dave 300 反对（160）在后。
func assertSplitBallots(t *testing.T, prefix string, v *VoteProposalView) {
	t.Helper()
	if len(v.Ballots) != 2 {
		t.Fatalf("%s: ballots=%+v, want exactly the two recorded ballots", prefix, v.Ballots)
	}
	b0, b1 := v.Ballots[0], v.Ballots[1]
	if b0.Representative != "alice" || b0.Weight != 700 || !b0.Support || b0.VotedAt != 150 {
		t.Fatalf("%s: ballot 0=%+v, want alice/700/for/150", prefix, b0)
	}
	if b1.Representative != "dave" || b1.Weight != 300 || b1.Support || b1.VotedAt != 160 {
		t.Fatalf("%s: ballot 1=%+v, want dave/300/against/160", prefix, b1)
	}
}

// assertTallyDoesNotMoveFunds 断言计票只确定状态、不执行动作：资金库余额、
// 收款账户余额与执行凭据全部保持计票前的样子。
func assertTallyDoesNotMoveFunds(t *testing.T, prefix string, store *Store) {
	t.Helper()
	if bal, err := store.TreasuryBalance(); err != nil || bal != 1000 {
		t.Fatalf("%s: treasury=%d err=%v, want untouched 1000", prefix, bal, err)
	}
	if all, err := store.Balances(); err != nil || len(all) != 0 {
		t.Fatalf("%s: recipient balances=%v err=%v, tally must not transfer funds", prefix, all, err)
	}
	if receipts, err := store.Receipts(); err != nil || len(receipts) != 0 {
		t.Fatalf("%s: receipts=%v err=%v, tally must not produce execution receipts", prefix, receipts, err)
	}
}

// TestTallyPassedBaselineDoesNotExecuteActions：正常在 200 首次计票应得到
// 赞成 700、反对 300、参与 1000 并通过；计票本身不执行动作，资金库、
// 收款账户余额与执行凭据保持原样；再次计票始终返回首次结论。
func TestTallyPassedBaselineDoesNotExecuteActions(t *testing.T) {
	store, _ := openTempStore(t, 1000)
	defer store.Close()
	const id = "gip-tally-base"
	mustCreateVote(t, store, tallySplitInput(id))
	castSplitBallots(t, store, id)

	res, err := store.TallyVote(id, 200)
	if err != nil {
		t.Fatalf("tally: %v", err)
	}
	if !res.Passed || res.ForWeight != 700 || res.AgainstWeight != 300 ||
		res.Turnout != 1000 || res.Quorum != 600 || res.TalliedAt != 200 {
		t.Fatalf("tally=%+v, want passed for=700 against=300 turnout=1000 at 200", res)
	}

	v, ok, err := store.VoteProposal(id)
	if err != nil || !ok {
		t.Fatalf("query: ok=%v err=%v", ok, err)
	}
	if v.State != "passed" {
		t.Fatalf("state=%s, want passed", v.State)
	}
	assertSplitRoster(t, "baseline", v)
	assertSplitBallots(t, "baseline", v)
	assertTallyDoesNotMoveFunds(t, "baseline", store)

	// 再次计票（即使传入更晚时间）只返回首次结论，不重新保存。
	again, err := store.TallyVote(id, 210)
	if err != nil {
		t.Fatalf("repeat tally: %v", err)
	}
	if !again.Passed || again.ForWeight != 700 || again.AgainstWeight != 300 ||
		again.Turnout != 1000 || again.TalliedAt != 200 {
		t.Fatalf("repeat tally=%+v, want first verdict at 200", again)
	}
}

// TestTallyPreReplaceFailureLeavesNoVerdict：首次计票若在状态文件替换之前
// 保存失败，Go 调用必须返回具体保存原因、计票结果为 nil，且该错误不能被
// 识别成 ErrDurabilityUnconfirmed（这是“尚未写入”，不是“已写入但持久性
// 未确认”）。随后查询仍看到 voting 状态、没有计票结论；已保存票据的代表、
// 权重、选择、首次时间与顺序全部保留；成员、委托与动作内容不变。保存条件
// 恢复后在 210 再次计票才形成首次成功结论，首次计票时间为 210，绝不沿用
// 失败尝试的 200，也不能因先前已算出通过而跳过保存。
func TestTallyPreReplaceFailureLeavesNoVerdict(t *testing.T) {
	store, path := openTempStore(t, 1000)
	defer store.Close()
	const id = "gip-tally-savefail-prereplace"
	mustCreateVote(t, store, tallySplitInput(id))
	castSplitBallots(t, store, id)

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 目录不可写：临时文件都无法创建，提交在状态文件替换之前失败。
	makeDirUnwritable(t, path)
	res, err := store.TallyVote(id, 200)
	if err == nil {
		t.Fatal("expected save failure, got nil error")
	}
	if res != nil {
		t.Fatalf("pre-replacement failure must not return a tally result: %+v", res)
	}
	if errors.Is(err, ErrDurabilityUnconfirmed) {
		t.Fatalf("pre-replacement failure must not be reported as durability warning: %v", err)
	}
	if errors.Is(err, ErrTallyRejected) {
		t.Fatalf("a tally at the deadline must not be rejected as a business rule: %v", err)
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("error must carry the concrete save cause, got %v", err)
	}

	// 失败提交不得遗留临时文件。
	if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".govflow-state-*")); len(leftovers) != 0 {
		t.Fatalf("failed tally left temporary file(s): %v", leftovers)
	}

	// 目录仍不可写时查询：仍是 voting、无计票结论，票据与提案内容完整保留。
	v, ok, qerr := store.VoteProposal(id)
	if qerr != nil || !ok {
		t.Fatalf("query while save broken: ok=%v err=%v", ok, qerr)
	}
	if v.State != "voting" || v.Tally != nil {
		t.Fatalf("state=%s tally=%+v, want voting with no tally", v.State, v.Tally)
	}
	assertSplitRoster(t, "after failed tally", v)
	assertSplitBallots(t, "after failed tally", v)
	assertTallyDoesNotMoveFunds(t, "after failed tally", store)

	// 状态文件字节必须与失败前完全一致：失败尝试不落任何部分记录。
	if after, rerr := os.ReadFile(path); rerr != nil || string(after) != string(before) {
		t.Fatalf("state file changed despite pre-replacement save failure")
	}

	// 恢复保存条件并重开：看到的仍是失败前的完整状态。
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("restore dir: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after failed tally: %v", err)
	}
	defer reopened.Close()
	v, ok, err = reopened.VoteProposal(id)
	if err != nil || !ok {
		t.Fatalf("query after reopen: ok=%v err=%v", ok, err)
	}
	if v.State != "voting" || v.Tally != nil {
		t.Fatalf("after reopen state=%s tally=%+v, want voting/no tally", v.State, v.Tally)
	}
	assertSplitRoster(t, "reopened after failed tally", v)
	assertSplitBallots(t, "reopened after failed tally", v)

	// 210 再次计票才形成首次成功结论：时间必须是 210，不能沿用失败尝试的
	// 200，也不能因内存中曾算出通过而跳过保存。
	res, err = reopened.TallyVote(id, 210)
	if err != nil {
		t.Fatalf("tally after recovery: %v", err)
	}
	if !res.Passed || res.ForWeight != 700 || res.AgainstWeight != 300 ||
		res.Turnout != 1000 || res.Quorum != 600 || res.TalliedAt != 210 {
		t.Fatalf("tally=%+v, want passed for=700 against=300 turnout=1000 tallied_at=210", res)
	}
	v, _, _ = reopened.VoteProposal(id)
	if v.State != "passed" || v.Tally == nil || v.Tally.TalliedAt != 210 || !v.Tally.Passed {
		t.Fatalf("state=%s tally=%+v, want passed with first tallied_at=210", v.State, v.Tally)
	}
	assertSplitRoster(t, "after recovered tally", v)
	assertSplitBallots(t, "after recovered tally", v)
	assertTallyDoesNotMoveFunds(t, "after recovered tally", reopened)

	// 重开后首次结论仍为 210：恢复后的计票确实已经持久化。
	if err := reopened.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after recovered tally: %v", err)
	}
	defer reopened2.Close()
	v2, ok, err := reopened2.VoteProposal(id)
	if err != nil || !ok {
		t.Fatalf("query after second reopen: ok=%v err=%v", ok, err)
	}
	if v2.State != "passed" || v2.Tally == nil || v2.Tally.TalliedAt != 210 ||
		v2.Tally.ForWeight != 700 || v2.Tally.AgainstWeight != 300 || v2.Tally.Turnout != 1000 {
		t.Fatalf("persisted proposal=%+v tally=%+v, want passed tallied_at=210 700/300/1000",
			v2.State, v2.Tally)
	}
	assertSplitBallots(t, "after second reopen", v2)
}

// TestTallyDirSyncFailureKeepsWrittenVerdict：状态文件已替换成功、只在随后的
// 目录同步失败时，Go 调用仍没有成功计票结果，但错误必须能识别成
// ErrDurabilityUnconfirmed 并保留具体失败原因（*DurabilityError.Cause）。
// 查询应看到 passed 状态与 200 的首次结论（700/300/1000 通过），票据、成员、
// 委托、动作与资金记录保持一致。保存条件恢复后在 210 再次计票，应返回已经
// 写入的 200 结论，而不是重新计票或改写首次计票时间。
func TestTallyDirSyncFailureKeepsWrittenVerdict(t *testing.T) {
	store, path := openTempStore(t, 1000)
	defer store.Close()
	const id = "gip-tally-savefail-dirsync"
	mustCreateVote(t, store, tallySplitInput(id))
	castSplitBallots(t, store, id)

	// 目录只写可进入不可读：临时文件写入、fsync 与改名都成功，只有目录同步失败。
	makeDirSyncFail(t, path)
	res, err := store.TallyVote(id, 200)
	if err == nil {
		t.Fatal("expected durability warning error, got nil")
	}
	if res != nil {
		t.Fatalf("no successful tally result may accompany the warning: %+v", res)
	}
	if !errors.Is(err, ErrDurabilityUnconfirmed) {
		t.Fatalf("err=%v, want errors.Is ErrDurabilityUnconfirmed", err)
	}
	if errors.Is(err, ErrTallyRejected) {
		t.Fatalf("a tally at the deadline must not be rejected as a business rule: %v", err)
	}
	var derr *DurabilityError
	if !errors.As(err, &derr) || derr.Cause == nil {
		t.Fatalf("err=%v (%T), want *DurabilityError with concrete cause", err, err)
	}
	if !strings.Contains(err.Error(), derr.Cause.Error()) {
		t.Fatalf("err %q must preserve the underlying save failure %q", err, derr.Cause)
	}

	// 即使目录仍处于降级状态，同一存储句柄的查询也应读到已写入的 200 结论。
	v, ok, qerr := store.VoteProposal(id)
	if qerr != nil || !ok {
		t.Fatalf("query while durability unconfirmed: ok=%v err=%v", ok, qerr)
	}
	if v.State != "passed" {
		t.Fatalf("state=%s, want passed already written", v.State)
	}
	if v.Tally == nil || !v.Tally.Passed || v.Tally.ForWeight != 700 ||
		v.Tally.AgainstWeight != 300 || v.Tally.Turnout != 1000 ||
		v.Tally.Quorum != 600 || v.Tally.TalliedAt != 200 {
		t.Fatalf("tally=%+v, want written passed verdict 700/300/1000 at 200", v.Tally)
	}
	assertSplitRoster(t, "after dir-sync failure", v)
	assertSplitBallots(t, "after dir-sync failure", v)
	assertTallyDoesNotMoveFunds(t, "after dir-sync failure", store)

	// 恢复目录并重开：同一结论与全部治理、资金记录仍在。
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("restore dir: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after dir-sync failure: %v", err)
	}
	defer reopened.Close()
	v, ok, err = reopened.VoteProposal(id)
	if err != nil || !ok {
		t.Fatalf("query after reopen: ok=%v err=%v", ok, err)
	}
	if v.State != "passed" || v.Tally == nil || v.Tally.TalliedAt != 200 ||
		v.Tally.ForWeight != 700 || v.Tally.AgainstWeight != 300 || !v.Tally.Passed {
		t.Fatalf("after reopen state=%s tally=%+v, want passed tallied_at=200", v.State, v.Tally)
	}
	assertSplitRoster(t, "reopened after dir-sync failure", v)
	assertSplitBallots(t, "reopened after dir-sync failure", v)
	assertTallyDoesNotMoveFunds(t, "reopened after dir-sync failure", reopened)

	// 210 再次计票：返回已经写入的 200 首次结论，不重新计票、不把时间改成 210。
	again, err := reopened.TallyVote(id, 210)
	if err != nil {
		t.Fatalf("repeat tally after recovery: %v", err)
	}
	if !again.Passed || again.ForWeight != 700 || again.AgainstWeight != 300 ||
		again.Turnout != 1000 || again.Quorum != 600 || again.TalliedAt != 200 {
		t.Fatalf("repeat tally=%+v, want the already-written verdict at 200", again)
	}
	v, _, _ = reopened.VoteProposal(id)
	if v.State != "passed" || v.Tally == nil || v.Tally.TalliedAt != 200 {
		t.Fatalf("state=%s tally=%+v, repeat tally must not rewrite the first verdict",
			v.State, v.Tally)
	}
	assertSplitRoster(t, "after repeat tally", v)
	assertSplitBallots(t, "after repeat tally", v)
	assertTallyDoesNotMoveFunds(t, "after repeat tally", reopened)
}
