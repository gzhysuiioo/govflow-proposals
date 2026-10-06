package govflow

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// durabilityVoteInput 构造本题回归保障使用的带委托提案：
// jia、yi、bing 的原始权重分别为 300、400、300，yi 把投票权委托给 jia，
// jia 与 bing 不委托。因此两个最终代表归集票重：jia 300+400=700，
// bing 300，总权重 1000。法定人数 600，投票窗口 [100,200)，时间锁 300，
// 动作是一笔有效转账（投票与计票都不执行它）。
// 成员按 jia、yi、bing 的顺序提交，以便固定“先前票据保持原顺序、新票在其后”。
func durabilityVoteInput(id string) *CreateVoteInput {
	return &CreateVoteInput{
		ID: id,
		Members: []VoteMember{
			{ID: "jia", Weight: 300},
			{ID: "yi", Weight: 400},
			{ID: "bing", Weight: 300},
		},
		Delegations: []Delegation{{From: "yi", To: "jia"}},
		Quorum:      600,
		StartAt:     100,
		Deadline:    200,
		TimelockEnd: 300,
		Actions:     []string{"transfer:audits:100"},
	}
}

// durabilityExpectedMembers 返回提案创建时的完整成员/委托视图（保持创建
// 顺序），供保存失败后断言“成员、委托和动作内容不被失败尝试改写”。
func durabilityExpectedMembers() map[string]MemberView {
	return map[string]MemberView{
		"jia":  {ID: "jia", Weight: 300, Path: []string{"jia"}, Delegate: "jia", Direct: ""},
		"yi":   {ID: "yi", Weight: 400, Path: []string{"yi", "jia"}, Delegate: "jia", Direct: "jia"},
		"bing": {ID: "bing", Weight: 300, Path: []string{"bing"}, Delegate: "bing", Direct: ""},
	}
}

// assertDurabilityUntouchedFunds 断言投票与计票不执行动作、不动资金：
// 资金库 1000、没有任何收款账户余额、没有执行凭据。
func assertDurabilityUntouchedFunds(t *testing.T, store *Store) {
	t.Helper()
	if bal, err := store.TreasuryBalance(); err != nil || bal != 1000 {
		t.Fatalf("treasury = %d err=%v, voting/tally must not move funds, want 1000", bal, err)
	}
	if all, err := store.Balances(); err != nil || len(all) != 0 {
		t.Fatalf("recipient balances = %v err=%v, voting/tally must not credit recipients, want none", all, err)
	}
	if receipts, err := store.Receipts(); err != nil || len(receipts) != 0 {
		t.Fatalf("receipts = %v err=%v, voting/tally must not produce execution receipts, want none", receipts, err)
	}
}

// assertDurabilityProposalShape 断言窗口、时间锁、法定人数、总权重、动作原文
// 以及成员/委托内容仍是创建时的样子，不被任何保存失败改写。
func assertDurabilityProposalShape(t *testing.T, v *VoteProposalView) {
	t.Helper()
	if v.Quorum != 600 || v.StartAt != 100 || v.Deadline != 200 ||
		v.TimelockEnd != 300 || v.TotalWeight != 1000 {
		t.Fatalf("proposal parameters changed: %+v", v)
	}
	if !sameActions(v.Actions, []string{"transfer:audits:100"}) {
		t.Fatalf("actions = %v, want original transfer action text", v.Actions)
	}
	assertMemberViews(t, "vote durability", memberPaths(v), durabilityExpectedMembers())
}

// assertBingJiaBallots 断言两张票据的代表、归集权重、选择、首次时间与顺序
// 全部保留：bing 在 120 的 300 权重反对票在前，jia 在 150 的 700 权重赞成票
// 在后。700 = jia 自有 300 + yi 委托的 400，yi 的权重不得被重复累计。
func assertBingJiaBallots(t *testing.T, v *VoteProposalView) {
	t.Helper()
	if len(v.Ballots) != 2 {
		t.Fatalf("ballots = %+v, want exactly bing's ballot followed by jia's", v.Ballots)
	}
	first, second := v.Ballots[0], v.Ballots[1]
	if first.Representative != "bing" || first.Weight != 300 || first.Support || first.VotedAt != 120 {
		t.Fatalf("first ballot = %+v, want bing against weight=300 voted_at=120", first)
	}
	if second.Representative != "jia" || second.Weight != 700 || !second.Support || second.VotedAt != 150 {
		t.Fatalf("second ballot = %+v, want jia for weight=700 voted_at=150", second)
	}
}

// assertOnlyBingAfterFailedJiaVote 断言 jia 的失败投票不留任何痕迹：查询仍
// 只有 bing 在 120 投出的 300 权重反对票，jia 归集的 700 权重不出现在任何
// 已投记录里（yi 也没有独立票据），提案仍处于投票中、没有计票结论；成员
// 权重、委托路径、窗口时间与动作原文保持创建时内容；资金余额不变、无凭据。
func assertOnlyBingAfterFailedJiaVote(t *testing.T, store *Store, id string) {
	t.Helper()
	v, ok, err := store.VoteProposal(id)
	if err != nil || !ok {
		t.Fatalf("VoteProposal: ok=%v err=%v", ok, err)
	}
	if v.State != "voting" || v.Tally != nil {
		t.Fatalf("state=%s tally=%+v, want voting with no tally", v.State, v.Tally)
	}
	if len(v.Ballots) != 1 {
		t.Fatalf("ballots = %+v, want only bing's ballot", v.Ballots)
	}
	b := v.Ballots[0]
	if b.Representative != "bing" || b.Weight != 300 || b.Support || b.VotedAt != 120 {
		t.Fatalf("bing ballot = %+v, want representative=bing weight=300 against voted_at=120", b)
	}
	assertDurabilityProposalShape(t, v)
	assertDurabilityUntouchedFunds(t, store)
}

// TestVoteDirSyncFailureBallotWrittenButUnconfirmed：最终代表 jia 在投票窗口内
// 首次投票（150 赞成）时，状态文件已替换成功、只在之后的目录同步失败（目录
// 可写可进入但不可读，0o300）。固定当前功能在这种“票据已经写入，但持久性
// 未确认”情况下的实际行为：
//   - CastVote 返回 nil 票据，错误可按 ErrDurabilityUnconfirmed 识别
//     （*DurabilityError），Cause 保留具体保存失败原因；既不能返回普通成功，
//     也不能把它解释成投票资格不符（ErrVoteRejected）；
//   - 恢复正常读取条件后查询应看到 jia 的首次票据已完整登记：代表 jia、
//     归集权重 700（自有 300 + yi 委托的 400，不重复累计）、赞成、首次
//     时间 150 全部来自当次提交；先前 bing 的 300 权重反对票（120）保持
//     原内容与顺序，新票位于其后；提案仍在投票中、没有提前计票；
//   - 告警不撤销已写入的票：jia 再次提交相同选择，即使提供的时间已到截止
//     时刻 200，也返回 150 的首次记录且不追加票据；改投反对按既有冲突规则
//     拒绝、原票不变；委托方 yi 仍不能直接投票；
//   - 到期计票得到赞成 700、反对 300、参与 1000 的通过结论，不漏掉报错时
//     已写入的 jia 票，也不重复累计 yi 的权重。
//
// 上述投票、查询与计票都不执行提案动作：资金余额与执行凭据保持原样。
func TestVoteDirSyncFailureBallotWrittenButUnconfirmed(t *testing.T) {
	store, path := openTempStore(t, 1000)
	defer store.Close()
	const id = "gip-vote-dirsync"
	mustCreateVote(t, store, durabilityVoteInput(id))
	if _, err := store.CastVote(id, "bing", false, 120); err != nil {
		t.Fatalf("bing vote: %v", err)
	}

	// 目录可写可进入但不可读：临时文件写入、fsync 与原子改名都成功，只有
	// syncDir 打开目录失败，确定性复现“替换成功后目录同步失败”。
	makeDirSyncFail(t, path)
	ballot, err := store.CastVote(id, "jia", true, 150)
	if err == nil {
		t.Fatal("expected durability warning error, got nil")
	}
	// 调用方收到错误时拿不到成功票据：票据已落盘，但这次保存的持久性未确认。
	if ballot != nil {
		t.Fatalf("durability-uncertain call must not return a successful ballot: %+v", ballot)
	}
	if !errors.Is(err, ErrDurabilityUnconfirmed) {
		t.Fatalf("err = %v, want errors.Is ErrDurabilityUnconfirmed", err)
	}
	// 一次合法的窗口内首次投票不能被解释成资格不符或窗口外投票。
	if errors.Is(err, ErrVoteRejected) {
		t.Fatalf("a valid in-window first vote must not be rejected as a business rule: %v", err)
	}
	var derr *DurabilityError
	if !errors.As(err, &derr) || derr.Cause == nil {
		t.Fatalf("err = %v (%T), want *DurabilityError with concrete cause", err, err)
	}
	if !strings.Contains(err.Error(), derr.Cause.Error()) {
		t.Fatalf("err %q must preserve the underlying save failure %q", err, derr.Cause)
	}

	// 恢复目录到正常读取条件；原子改名已经成功，不应遗留临时文件。
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("restore dir: %v", err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".govflow-state-*")); len(leftovers) != 0 {
		t.Fatalf("durability-uncertain commit left temporary file(s): %v", leftovers)
	}

	// 同一进程立即查询：jia 的首次票已完整登记，提案仍在投票中、没有计票结论。
	v, ok, qerr := store.VoteProposal(id)
	if qerr != nil || !ok {
		t.Fatalf("query after durability warning: ok=%v err=%v", ok, qerr)
	}
	if v.State != "voting" || v.Tally != nil {
		t.Fatalf("state=%s tally=%+v, want voting with no tally (no early counting)", v.State, v.Tally)
	}
	assertBingJiaBallots(t, v)
	assertDurabilityProposalShape(t, v)
	assertDurabilityUntouchedFunds(t, store)

	// 以全新进程视角重开：写入的票据必须能跨进程读到，且通过打开时的严格
	// 重放校验（代表资格、归集票重、票据字段、首次时间窗口）。
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after durability-uncertain vote: %v", err)
	}
	defer reopened.Close()
	rv, ok, err := reopened.VoteProposal(id)
	if err != nil || !ok {
		t.Fatalf("query after reopen: ok=%v err=%v", ok, err)
	}
	if rv.State != "voting" || rv.Tally != nil {
		t.Fatalf("after reopen state=%s tally=%+v, want voting with no tally", rv.State, rv.Tally)
	}
	assertBingJiaBallots(t, rv)
	assertDurabilityProposalShape(t, rv)

	// 相同选择重试，即使提供的时间已经到达截止时刻 200（窗口为 [100,200)），
	// 仍返回 150 的首次记录，不追加票据、不改首次时间。
	retry, err := reopened.CastVote(id, "jia", true, 200)
	if err != nil {
		t.Fatalf("same-choice retry at deadline: %v", err)
	}
	if retry.Representative != "jia" || retry.Weight != 700 || !retry.Support || retry.VotedAt != 150 {
		t.Fatalf("same-choice retry = %+v, want first record jia for weight=700 voted_at=150", retry)
	}
	afterRetry, _, err := reopened.VoteProposal(id)
	if err != nil {
		t.Fatal(err)
	}
	assertBingJiaBallots(t, afterRetry)

	// 改投反对应按既有冲突规则拒绝，原赞成票保持不变。
	if _, err := reopened.CastVote(id, "jia", false, 160); !errors.Is(err, ErrProposalConflict) {
		t.Fatalf("changing vote err=%v, want ErrProposalConflict", err)
	}
	// 委托方 yi 仍不能直接投票，也不能因此产生第二张归集票。
	if _, err := reopened.CastVote(id, "yi", true, 150); !errors.Is(err, ErrVoteRejected) {
		t.Fatalf("delegated yi direct vote err=%v, want ErrVoteRejected", err)
	}
	stillVoting, _, err := reopened.VoteProposal(id)
	if err != nil {
		t.Fatal(err)
	}
	if stillVoting.State != "voting" || stillVoting.Tally != nil {
		t.Fatalf("state=%s tally=%+v, want still voting", stillVoting.State, stillVoting.Tally)
	}
	assertBingJiaBallots(t, stillVoting)

	// 到期计票：赞成 700、反对 300、参与 1000，达到法定人数且赞成严格多于
	// 反对 => 通过；报错时已写入的 jia 票被计入，yi 的 400 权重没有被重复
	// 累计（否则参与量会超过总权重 1000）。
	res, err := reopened.TallyVote(id, 200)
	if err != nil {
		t.Fatalf("tally at deadline: %v", err)
	}
	if !res.Passed || res.ForWeight != 700 || res.AgainstWeight != 300 ||
		res.Turnout != 1000 || res.Quorum != 600 || res.TalliedAt != 200 {
		t.Fatalf("tally = %+v, want passed for=700 against=300 turnout=1000 tallied_at=200", res)
	}
	tallied, _, err := reopened.VoteProposal(id)
	if err != nil {
		t.Fatal(err)
	}
	if tallied.State != "passed" || tallied.Tally == nil || !tallied.Tally.Passed {
		t.Fatalf("state=%s tally=%+v, want passed", tallied.State, tallied.Tally)
	}
	assertBingJiaBallots(t, tallied)
	assertDurabilityProposalShape(t, tallied)
	assertDurabilityUntouchedFunds(t, reopened)

	// 再次计票始终返回同一个首次结论，不重新计票、不改首次时间。
	again, err := reopened.TallyVote(id, 210)
	if err != nil {
		t.Fatalf("repeat tally: %v", err)
	}
	if !again.Passed || again.ForWeight != 700 || again.AgainstWeight != 300 ||
		again.Turnout != 1000 || again.TalliedAt != 200 {
		t.Fatalf("repeat tally = %+v, want the first 200 conclusion", again)
	}

	// 再以全新进程视角重开：通过结论、两张票据与资金记录都持久可读。
	if err := reopened.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after tally: %v", err)
	}
	defer reopened2.Close()
	fv, ok, err := reopened2.VoteProposal(id)
	if err != nil || !ok {
		t.Fatalf("query after final reopen: ok=%v err=%v", ok, err)
	}
	if fv.State != "passed" || fv.Tally == nil || !fv.Tally.Passed ||
		fv.Tally.ForWeight != 700 || fv.Tally.AgainstWeight != 300 ||
		fv.Tally.Turnout != 1000 || fv.Tally.TalliedAt != 200 {
		t.Fatalf("final view = %+v, want passed tally 700/300/1000 at 200", fv)
	}
	assertBingJiaBallots(t, fv)
	assertDurabilityUntouchedFunds(t, reopened2)
}

// TestVotePreReplaceFailureLeavesNoBallot：同一委托规格下，jia 在 150 的首次
// 赞成因状态文件替换之前的保存错误（目录 0o555 不可写，临时文件都无法创建）
// 失败时，必须与“已写入但持久性未确认”严格区分：返回 nil 票据与具体保存
// 原因，错误不能按 ErrDurabilityUnconfirmed 识别，也不是投票资格不符；没有
// 新票据、不占用 jia 的首次投票机会，状态文件字节与失败前完全一致，没有
// 遗留临时文件。恢复保存条件后，窗口内 160 的赞成才是首次有效投票（不沿用
// 150，也不报“已投过”）；到期计票仍是赞成 700、反对 300、参与 1000 通过。
// 全过程不执行提案动作，资金余额与执行凭据保持原样。
func TestVotePreReplaceFailureLeavesNoBallot(t *testing.T) {
	store, path := openTempStore(t, 1000)
	defer store.Close()
	const id = "gip-vote-prereplace"
	mustCreateVote(t, store, durabilityVoteInput(id))
	if _, err := store.CastVote(id, "bing", false, 120); err != nil {
		t.Fatalf("bing vote: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	makeDirUnwritable(t, path)
	ballot, err := store.CastVote(id, "jia", true, 150)
	if err == nil {
		t.Fatal("expected save failure, got nil error")
	}
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

	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("restore dir: %v", err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".govflow-state-*")); len(leftovers) != 0 {
		t.Fatalf("failed commit left temporary file(s): %v", leftovers)
	}
	if after, rerr := os.ReadFile(path); rerr != nil || string(after) != string(before) {
		t.Fatalf("state file changed despite pre-replacement save failure")
	}

	// 同一进程与全新进程视角都只能看到 bing 的票；jia 的 700 权重无任何记录。
	assertOnlyBingAfterFailedJiaVote(t, store, id)
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after failed save: %v", err)
	}
	defer reopened.Close()
	assertOnlyBingAfterFailedJiaVote(t, reopened, id)

	// 窗口内 160 的赞成才是首次有效投票：700 权重、首次时间 160，绝不沿用
	// 失败尝试的 150，也不能报“已经投过票”。
	first, err := reopened.CastVote(id, "jia", true, 160)
	if err != nil {
		t.Fatalf("jia first valid vote after recovery: %v", err)
	}
	if first.Representative != "jia" || first.Weight != 700 || !first.Support || first.VotedAt != 160 {
		t.Fatalf("first ballot = %+v, want jia weight=700 for voted_at=160", first)
	}
	v, _, err := reopened.VoteProposal(id)
	if err != nil {
		t.Fatal(err)
	}
	if v.State != "voting" || v.Tally != nil {
		t.Fatalf("state=%s tally=%+v, want voting", v.State, v.Tally)
	}
	if len(v.Ballots) != 2 {
		t.Fatalf("ballots = %+v, want bing then jia", v.Ballots)
	}
	if b0 := v.Ballots[0]; b0.Representative != "bing" || b0.Weight != 300 || b0.Support || b0.VotedAt != 120 {
		t.Fatalf("first ballot = %+v, want bing against 300 at 120", b0)
	}
	if b1 := v.Ballots[1]; b1.Representative != "jia" || b1.Weight != 700 || !b1.Support || b1.VotedAt != 160 {
		t.Fatalf("second ballot = %+v, want jia for 700 at 160", b1)
	}

	// 到期计票：赞成 700、反对 300、参与 1000，通过。
	res, err := reopened.TallyVote(id, 200)
	if err != nil {
		t.Fatalf("tally: %v", err)
	}
	if !res.Passed || res.ForWeight != 700 || res.AgainstWeight != 300 ||
		res.Turnout != 1000 || res.Quorum != 600 || res.TalliedAt != 200 {
		t.Fatalf("tally = %+v, want passed for=700 against=300 turnout=1000 at 200", res)
	}
	assertDurabilityUntouchedFunds(t, reopened)
}

// TestVotePreReplaceFailureThenDeadlineStillRejects：同一次替换前保存失败后，
// 若直到截止时刻 200 才再次提交，jia 的首次票仍按截止边界（窗口 [100,200)）
// 拒绝——失败尝试没有写入、不占用首次投票机会，但窗口约束照常生效。bing 的
// 300 权重反对票保持唯一记录；到期计票只能得到赞成 0、反对 300、参与 300、
// 未达法定人数的拒绝结论。资金余额不变，也没有执行凭据。
func TestVotePreReplaceFailureThenDeadlineStillRejects(t *testing.T) {
	store, path := openTempStore(t, 1000)
	defer store.Close()
	const id = "gip-vote-prereplace-late"
	mustCreateVote(t, store, durabilityVoteInput(id))
	if _, err := store.CastVote(id, "bing", false, 120); err != nil {
		t.Fatalf("bing vote: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	makeDirUnwritable(t, path)
	if ballot, err := store.CastVote(id, "jia", true, 150); err == nil {
		t.Fatalf("save must fail, got ballot %+v", ballot)
	} else if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("error must carry the concrete save cause, got %v", err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("restore dir: %v", err)
	}
	if got, rerr := os.ReadFile(path); rerr != nil || string(got) != string(before) {
		t.Fatalf("state file changed despite pre-replacement save failure")
	}

	// 一直到截止时刻 200 才再次提交：首次票按截止边界拒绝，窗口约束照常生效。
	if ballot, err := store.CastVote(id, "jia", true, 200); !errors.Is(err, ErrVoteRejected) {
		t.Fatalf("vote at deadline (exclusive) ballot=%+v err=%v, want ErrVoteRejected", ballot, err)
	}
	assertOnlyBingAfterFailedJiaVote(t, store, id)

	// 到期计票：赞成 0、反对 300、参与 300，未达法定人数 600 => 拒绝。
	res, err := store.TallyVote(id, 200)
	if err != nil {
		t.Fatalf("tally: %v", err)
	}
	if res.Passed || res.ForWeight != 0 || res.AgainstWeight != 300 ||
		res.Turnout != 300 || res.Quorum != 600 || res.TalliedAt != 200 {
		t.Fatalf("tally = %+v, want rejected for=0 against=300 turnout=300 at 200", res)
	}
	v, _, err := store.VoteProposal(id)
	if err != nil {
		t.Fatal(err)
	}
	if v.State != "rejected" || v.Tally == nil || v.Tally.Passed {
		t.Fatalf("state=%s tally=%+v, want rejected/not-passed", v.State, v.Tally)
	}
	if len(v.Ballots) != 1 || v.Ballots[0].Representative != "bing" {
		t.Fatalf("ballots = %+v, want bing's ballot only", v.Ballots)
	}
	assertDurabilityUntouchedFunds(t, store)
}
