package govflow

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tallyInput 构造本题回归保障使用的带委托转账提案：
// alice/bob/carol/dave 权重 400/200/100/300；bob、carol 委托给 alice，
// dave 不委托。因此两个最终代表归集票重：alice 400+200+100=700，
// dave 300，总权重 1000。窗口 [100,200)、时间锁 300、法定人数 600，
// 动作是一笔有效转账 transfer:grants:250。
func tallyInput(id string) *CreateVoteInput {
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
		Actions:     []string{"transfer:grants:250"},
	}
}

// tallySetup 完成提案创建与两张窗口内票据：dave 在 130 投出归集 300 权重
// 反对票，alice 在 170 投出归集 700 权重赞成票。返回 store 与提案编号。
func tallySetup(t *testing.T, id string) (*Store, string) {
	t.Helper()
	store, path := openTempStore(t, 1000)
	mustCreateVote(t, store, tallyInput(id))
	if _, err := store.CastVote(id, "dave", false, 130); err != nil {
		t.Fatalf("dave against ballot: %v", err)
	}
	if _, err := store.CastVote(id, "alice", true, 170); err != nil {
		t.Fatalf("alice for ballot: %v", err)
	}
	return store, path
}

// tallyExpectedMembers 返回提案创建时的完整成员/委托视图（保持创建顺序），
// 供保存失败后断言“成员、委托和动作内容不被失败尝试改写”。
func tallyExpectedMembers() map[string]MemberView {
	return map[string]MemberView{
		"alice": {ID: "alice", Weight: 400, Path: []string{"alice"}, Delegate: "alice", Direct: ""},
		"bob":   {ID: "bob", Weight: 200, Path: []string{"bob", "alice"}, Delegate: "alice", Direct: "alice"},
		"carol": {ID: "carol", Weight: 100, Path: []string{"carol", "alice"}, Delegate: "alice", Direct: "alice"},
		"dave":  {ID: "dave", Weight: 300, Path: []string{"dave"}, Delegate: "dave", Direct: ""},
	}
}

// assertTallyUntouchedFunds 断言计票不执行动作、不动资金：资金库 1000、
// 没有任何收款账户余额、没有执行凭据。
func assertTallyUntouchedFunds(t *testing.T, store *Store) {
	t.Helper()
	if bal, err := store.TreasuryBalance(); err != nil || bal != 1000 {
		t.Fatalf("treasury = %d err=%v, tally must not move funds, want 1000", bal, err)
	}
	if all, err := store.Balances(); err != nil || len(all) != 0 {
		t.Fatalf("recipient balances = %v err=%v, tally must not credit recipients, want none", all, err)
	}
	if receipts, err := store.Receipts(); err != nil || len(receipts) != 0 {
		t.Fatalf("receipts = %v err=%v, tally must not produce execution receipts, want none", receipts, err)
	}
}

// assertTallyBallotsPreserved 断言两张已保存票据的代表、权重、选择、首次
// 时间与顺序全部保留：dave 反对 300@130 在前，alice 赞成 700@170 在后。
func assertTallyBallotsPreserved(t *testing.T, v *VoteProposalView) {
	t.Helper()
	if len(v.Ballots) != 2 {
		t.Fatalf("ballots = %+v, want exactly the two saved ballots", v.Ballots)
	}
	first, second := v.Ballots[0], v.Ballots[1]
	if first.Representative != "dave" || first.Weight != 300 || first.Support || first.VotedAt != 130 {
		t.Fatalf("first ballot = %+v, want dave against weight=300 voted_at=130", first)
	}
	if second.Representative != "alice" || second.Weight != 700 || !second.Support || second.VotedAt != 170 {
		t.Fatalf("second ballot = %+v, want alice for weight=700 voted_at=170", second)
	}
}

// assertTallyProposalShape 断言提案的窗口、时间锁、法定人数、总权重、动作
// 原文以及成员/委托内容仍是创建时的样子，不被任何保存失败改写。
func assertTallyProposalShape(t *testing.T, v *VoteProposalView) {
	t.Helper()
	if v.Quorum != 600 || v.StartAt != 100 || v.Deadline != 200 ||
		v.TimelockEnd != 300 || v.TotalWeight != 1000 {
		t.Fatalf("proposal parameters changed: %+v", v)
	}
	if !sameActions(v.Actions, []string{"transfer:grants:250"}) {
		t.Fatalf("actions = %v, want original transfer action text", v.Actions)
	}
	assertMemberViews(t, "after tally save failure", memberPaths(v), tallyExpectedMembers())
}

// TestTallyFirstCountBaseline：正常在截止时刻 200 首次计票的基线：
// 赞成 700、反对 300、参与 1000，达到法定人数 600 且赞成严格多于反对 => 通过，
// 首次计票时间为 200；计票只确定状态，不执行转账，资金库、收款账户余额与
// 执行凭据保持原样。这是两类保存失败场景共同的对照。
func TestTallyFirstCountBaseline(t *testing.T) {
	store, _ := tallySetup(t, "gip-tally-ok")
	defer store.Close()

	res, err := store.TallyVote("gip-tally-ok", 200)
	if err != nil {
		t.Fatalf("first tally at deadline: %v", err)
	}
	if !res.Passed || res.ForWeight != 700 || res.AgainstWeight != 300 ||
		res.Turnout != 1000 || res.Quorum != 600 || res.TalliedAt != 200 {
		t.Fatalf("tally = %+v, want passed for=700 against=300 turnout=1000 tallied_at=200", res)
	}

	v, ok, err := store.VoteProposal("gip-tally-ok")
	if err != nil || !ok {
		t.Fatalf("query after tally: ok=%v err=%v", ok, err)
	}
	if v.State != "passed" || v.Tally == nil {
		t.Fatalf("state=%s tally=%+v, want passed with first conclusion", v.State, v.Tally)
	}
	if !v.Tally.Passed || v.Tally.ForWeight != 700 || v.Tally.AgainstWeight != 300 ||
		v.Tally.Turnout != 1000 || v.Tally.Quorum != 600 || v.Tally.TalliedAt != 200 {
		t.Fatalf("queried tally = %+v, want the 200 passed conclusion", v.Tally)
	}
	assertTallyBallotsPreserved(t, v)
	assertTallyProposalShape(t, v)
	assertTallyUntouchedFunds(t, store)

	// 再次计票（即使传入更晚的 210）始终返回首次 200 结论，不重新计票、
	// 不改变首次时间，也不执行动作。
	again, err := store.TallyVote("gip-tally-ok", 210)
	if err != nil {
		t.Fatalf("repeat tally: %v", err)
	}
	if !again.Passed || again.ForWeight != 700 || again.AgainstWeight != 300 ||
		again.Turnout != 1000 || again.TalliedAt != 200 {
		t.Fatalf("repeat tally = %+v, want the first 200 conclusion", again)
	}
	assertTallyUntouchedFunds(t, store)
}

// TestTallyPreReplaceFailureLeavesNoConclusion：首次计票的保存失败发生在
// 状态文件替换之前（目录不可写，临时文件都无法创建）时，TallyVote 必须：
//   - 返回 nil 计票结果与具体保存原因，错误不能被识别成
//     ErrDurabilityUnconfirmed（这是“尚未写入”，不是“已写入但持久性未确认”）；
//   - 随后查询仍看到 voting 状态、没有计票结论；已保存票据的代表、权重、
//     选择、首次时间及顺序全部保留，成员、委托与动作内容不变；
//   - 保存条件恢复后在 210 再次计票才形成首次成功结论，首次计票时间为 210，
//     不沿用失败尝试的 200，也不因先前已经算出通过而跳过保存。
func TestTallyPreReplaceFailureLeavesNoConclusion(t *testing.T) {
	store, path := tallySetup(t, "gip-tally-pre-replace")
	defer store.Close()
	const id = "gip-tally-pre-replace"

	// 记录失败前的完整状态文件字节：保存失败后它必须原样保留。
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 目录不可写：提交在状态文件替换之前（创建临时文件）失败。
	makeDirUnwritable(t, path)
	res, err := store.TallyVote(id, 200)
	if err == nil {
		t.Fatal("expected save failure, got nil error")
	}
	// 返回的是具体保存原因，而不是成功结论、计票业务拒绝或“已写入”告警。
	if res != nil {
		t.Fatalf("pre-replacement failure must not return a tally result: %+v", res)
	}
	if errors.Is(err, ErrDurabilityUnconfirmed) {
		t.Fatalf("pre-replacement failure must not be reported as durability warning: %v", err)
	}
	if errors.Is(err, ErrTallyRejected) {
		t.Fatalf("a deadline-reached tally must not be rejected as a business rule: %v", err)
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("error must carry the concrete save cause, got %v", err)
	}

	// 保存失败后不能遗留临时文件或部分文件。
	if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".govflow-state-*")); len(leftovers) != 0 {
		t.Fatalf("failed commit left temporary file(s): %v", leftovers)
	}

	// 即使目录仍不可写，只读查询看到的也是失败前的完整状态：voting、无结论。
	v, ok, qerr := store.VoteProposal(id)
	if qerr != nil || !ok {
		t.Fatalf("query after failed save: ok=%v err=%v", ok, qerr)
	}
	if v.State != "voting" || v.Tally != nil {
		t.Fatalf("state=%s tally=%+v, want voting with no tally conclusion", v.State, v.Tally)
	}
	assertTallyBallotsPreserved(t, v)
	assertTallyProposalShape(t, v)
	assertTallyUntouchedFunds(t, store)

	// 原状态文件保留失败前的完整内容（字节一致）。
	if after, rerr := os.ReadFile(path); rerr != nil || string(after) != string(before) {
		t.Fatalf("state file changed despite pre-replacement save failure")
	}

	// 恢复保存条件，并以新进程视角重开：读到的仍是 voting、两张原票据。
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
	rv, ok, err := reopened.VoteProposal(id)
	if err != nil || !ok {
		t.Fatalf("query after reopen: ok=%v err=%v", ok, err)
	}
	if rv.State != "voting" || rv.Tally != nil {
		t.Fatalf("after reopen state=%s tally=%+v, want voting with no conclusion", rv.State, rv.Tally)
	}
	assertTallyBallotsPreserved(t, rv)
	assertTallyProposalShape(t, rv)

	// 210 再次计票才形成首次成功结论：通过、700/300/1000，首次计票时间是 210，
	// 绝不沿用失败尝试的 200；它必须真正落盘（先前内存里已经算出通过，不能
	// 因此跳过保存或把这次调用当成“已有结论”的重试）。
	res210, err := reopened.TallyVote(id, 210)
	if err != nil {
		t.Fatalf("first successful tally after recovery at 210: %v", err)
	}
	if !res210.Passed || res210.ForWeight != 700 || res210.AgainstWeight != 300 ||
		res210.Turnout != 1000 || res210.Quorum != 600 || res210.TalliedAt != 210 {
		t.Fatalf("tally at 210 = %+v, want passed for=700 against=300 turnout=1000 tallied_at=210", res210)
	}
	sv, ok, err := reopened.VoteProposal(id)
	if err != nil || !ok {
		t.Fatalf("query after 210 tally: ok=%v err=%v", ok, err)
	}
	if sv.State != "passed" || sv.Tally == nil || !sv.Tally.Passed ||
		sv.Tally.ForWeight != 700 || sv.Tally.AgainstWeight != 300 ||
		sv.Tally.Turnout != 1000 || sv.Tally.TalliedAt != 210 {
		t.Fatalf("after 210 tally state=%s tally=%+v, want passed with tallied_at=210", sv.State, sv.Tally)
	}
	assertTallyBallotsPreserved(t, sv)
	assertTallyProposalShape(t, sv)
	assertTallyUntouchedFunds(t, reopened)

	// 再次计票返回 210 的首次结论，不随新时间改变。
	again, err := reopened.TallyVote(id, 300)
	if err != nil {
		t.Fatalf("repeat tally after success: %v", err)
	}
	if !again.Passed || again.TalliedAt != 210 || again.ForWeight != 700 || again.AgainstWeight != 300 {
		t.Fatalf("repeat tally = %+v, want first successful 210 conclusion", again)
	}
}

// assertNoTallyLeftovers 断言没有计票提交遗留的临时状态文件。
func assertNoTallyLeftovers(t *testing.T, path string) {
	t.Helper()
	if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".govflow-state-*")); len(leftovers) != 0 {
		t.Fatalf("failed commit left temporary file(s): %v", leftovers)
	}
}

// TestTallyDirSyncFailureWrittenButUnconfirmed：首次计票时状态文件已替换
// 成功、只在之后的目录同步失败（目录可写可进入但不可读，0o300）时，
// TallyVote 必须：
//   - 仍没有成功计票结果返回（nil），但错误能识别为
//     ErrDurabilityUnconfirmed（*DurabilityError），并保留具体失败原因；
//   - 查询应看到 passed 状态与 200 的首次结论——首次结论、计票时间与提案
//     状态其实已一起写入；票据与资金记录仍保持一致；
//   - 保存条件恢复后在 210 再次计票，应返回已经写入的 200 结论，而不是
//     重新算一个 210，也不报错。
func TestTallyDirSyncFailureWrittenButUnconfirmed(t *testing.T) {
	store, path := tallySetup(t, "gip-tally-dirsync")
	const id = "gip-tally-dirsync"
	defer store.Close()

	makeDirSyncFail(t, path)
	res, err := store.TallyVote(id, 200)
	if err == nil {
		t.Fatal("expected durability warning error, got nil")
	}
	// 调用本身没有成功计票结果：结论已落盘，但这次写入的持久性未确认。
	if res != nil {
		t.Fatalf("durability-uncertain call must not report a successful result: %+v", res)
	}
	if !errors.Is(err, ErrDurabilityUnconfirmed) {
		t.Fatalf("err = %v, want errors.Is ErrDurabilityUnconfirmed", err)
	}
	var derr *DurabilityError
	if !errors.As(err, &derr) || derr.Cause == nil {
		t.Fatalf("err = %v (%T), want *DurabilityError with concrete cause", err, err)
	}
	if !strings.Contains(err.Error(), derr.Cause.Error()) {
		t.Fatalf("err %q must preserve the underlying save failure %q", err, derr.Cause)
	}
	assertNoTallyLeftovers(t, path)

	// 恢复目录后查询：passed 状态与 200 的首次结论已写入，票据保持一致。
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("restore dir: %v", err)
	}
	v, ok, qerr := store.VoteProposal(id)
	if qerr != nil || !ok {
		t.Fatalf("query after durability warning: ok=%v err=%v", ok, qerr)
	}
	if v.State != "passed" || v.Tally == nil {
		t.Fatalf("state=%s tally=%+v, want passed with the written 200 conclusion", v.State, v.Tally)
	}
	if !v.Tally.Passed || v.Tally.ForWeight != 700 || v.Tally.AgainstWeight != 300 ||
		v.Tally.Turnout != 1000 || v.Tally.Quorum != 600 || v.Tally.TalliedAt != 200 {
		t.Fatalf("written tally = %+v, want passed for=700 against=300 turnout=1000 tallied_at=200", v.Tally)
	}
	assertTallyBallotsPreserved(t, v)
	assertTallyProposalShape(t, v)
	assertTallyUntouchedFunds(t, store)

	// 以全新进程视角重开，写入的结论必须能跨进程读到，且通过严格重放校验。
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after durability-uncertain tally: %v", err)
	}
	defer reopened.Close()
	rv, ok, err := reopened.VoteProposal(id)
	if err != nil || !ok {
		t.Fatalf("query after reopen: ok=%v err=%v", ok, err)
	}
	if rv.State != "passed" || rv.Tally == nil || !rv.Tally.Passed || rv.Tally.TalliedAt != 200 {
		t.Fatalf("after reopen state=%s tally=%+v, want passed with tallied_at=200", rv.State, rv.Tally)
	}
	assertTallyBallotsPreserved(t, rv)

	// 210 再次计票：返回已经写入的 200 结论，不重新计票、不改首次时间。
	again, err := reopened.TallyVote(id, 210)
	if err != nil {
		t.Fatalf("repeat tally after recovery: %v", err)
	}
	if !again.Passed || again.ForWeight != 700 || again.AgainstWeight != 300 ||
		again.Turnout != 1000 || again.Quorum != 600 || again.TalliedAt != 200 {
		t.Fatalf("repeat tally = %+v, want the already-written 200 conclusion", again)
	}
	// 查询结论与首次结论一致，资金仍未被计票触动。
	fv, _, err := reopened.VoteProposal(id)
	if err != nil {
		t.Fatal(err)
	}
	if fv.State != "passed" || fv.Tally == nil || fv.Tally.TalliedAt != 200 {
		t.Fatalf("state=%s tally=%+v, want passed tallied_at=200", fv.State, fv.Tally)
	}
	assertTallyUntouchedFunds(t, reopened)
}
