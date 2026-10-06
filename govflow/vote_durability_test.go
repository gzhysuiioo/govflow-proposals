package govflow

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// delegatedVoteInput 是本文件回归场景共用的提案：甲、乙、丙的原始权重分别为
// 300、400、300，乙委托给甲（甲名下归集 700），法定人数 600，投票窗口
// [100,200)，时间锁 300。动作只用于创建提案；投票、查询与计票都不执行它。
func delegatedVoteInput(id string) *CreateVoteInput {
	return &CreateVoteInput{
		ID: id,
		Members: []VoteMember{
			{ID: "甲", Weight: 300},
			{ID: "乙", Weight: 400},
			{ID: "丙", Weight: 300},
		},
		Delegations: []Delegation{{From: "乙", To: "甲"}},
		Quorum:      600,
		StartAt:     100,
		Deadline:    200,
		TimelockEnd: 300,
		Actions:     []string{"transfer:audits:100"},
	}
}

// assertDelegatedMembers 断言成员视图保持创建时的内容：原始权重、完整委托
// 路径、最终代表与直接委托对象都不因投票或保存失败而改变。
func assertDelegatedMembers(t *testing.T, prefix string, v *VoteProposalView) {
	t.Helper()
	want := map[string]MemberView{
		"甲": {ID: "甲", Weight: 300, Path: []string{"甲"}, Delegate: "甲", Direct: ""},
		"乙": {ID: "乙", Weight: 400, Path: []string{"乙", "甲"}, Delegate: "甲", Direct: "甲"},
		"丙": {ID: "丙", Weight: 300, Path: []string{"丙"}, Delegate: "丙", Direct: ""},
	}
	assertMemberViews(t, prefix, memberPaths(v), want)
}

// assertNoFundsMoved 断言投票、查询与计票都不执行提案动作：资金余额保持
// 初始值，没有收款账户，也没有执行凭据。
func assertNoFundsMoved(t *testing.T, store *Store, treasury int64) {
	t.Helper()
	if bal, err := store.TreasuryBalance(); err != nil || bal != treasury {
		t.Fatalf("treasury = %d err=%v, want untouched %d", bal, err, treasury)
	}
	if all, err := store.Balances(); err != nil || len(all) != 0 {
		t.Fatalf("recipient balances = %v err=%v, want none", all, err)
	}
	if receipts, err := store.Receipts(); err != nil || len(receipts) != 0 {
		t.Fatalf("receipts = %v err=%v, voting must not produce receipts", receipts, err)
	}
}

// assertBingThenJiaBallots 断言查询视图恰好保留两张票：丙在 120 的 300 权重
// 反对票在前，甲在 150 的 700 权重赞成票（含乙委托的 400）在后；提案仍在
// 投票中，没有提前计票；创建参数与成员委托关系保持原样。
func assertBingThenJiaBallots(t *testing.T, store *Store, id string) {
	t.Helper()
	v, ok, err := store.VoteProposal(id)
	if err != nil || !ok {
		t.Fatalf("VoteProposal: ok=%v err=%v", ok, err)
	}
	if v.State != "voting" || v.Tally != nil {
		t.Fatalf("state=%s tally=%+v, want voting with no tally", v.State, v.Tally)
	}
	if len(v.Ballots) != 2 {
		t.Fatalf("ballots = %+v, want exactly 丙 then 甲", v.Ballots)
	}
	if b := v.Ballots[0]; b.Representative != "丙" || b.Weight != 300 || b.Support || b.VotedAt != 120 {
		t.Fatalf("first ballot = %+v, want 丙 weight=300 against voted_at=120", b)
	}
	if b := v.Ballots[1]; b.Representative != "甲" || b.Weight != 700 || !b.Support || b.VotedAt != 150 {
		t.Fatalf("second ballot = %+v, want 甲 weight=700 for voted_at=150", b)
	}
	if v.Quorum != 600 || v.StartAt != 100 || v.Deadline != 200 || v.TimelockEnd != 300 || v.TotalWeight != 1000 {
		t.Fatalf("proposal parameters changed: %+v", v)
	}
	if !sameActions(v.Actions, []string{"transfer:audits:100"}) {
		t.Fatalf("actions = %v, want original action text", v.Actions)
	}
	assertDelegatedMembers(t, "after durability warning", v)
}

// TestVoteDirSyncFailureKeepsWrittenBallot：最终代表在投票窗口内首次投票时，
// 状态文件替换成功、随后目录同步失败——Go 调用必须返回 nil 票据与可按
// ErrDurabilityUnconfirmed 识别的错误（保留具体保存原因），而不是普通成功，
// 也不能解释成投票资格不符。错误不撤销已写入的票：恢复读取后，丙的反对票与
// 甲的 700 权重赞成票都完整登记；同选择重试返回 150 的首次记录，改投按冲突
// 拒绝；到期计票为赞成 700、反对 300、参与 1000 的通过结论。
func TestVoteDirSyncFailureKeepsWrittenBallot(t *testing.T) {
	store, path := openTempStore(t, 1000)
	defer store.Close()
	const id = "gip-vote-dursync"
	mustCreateVote(t, store, delegatedVoteInput(id))
	if _, err := store.CastVote(id, "丙", false, 120); err != nil {
		t.Fatalf("丙 vote: %v", err)
	}

	// 目录可写可进入但不可读：临时文件写入、fsync 与原子改名成功，
	// 只有随后的目录同步失败。
	makeDirSyncFail(t, path)
	ballot, err := store.CastVote(id, "甲", true, 150)
	if err == nil {
		t.Fatal("expected durability warning error, got nil")
	}
	// 不是普通成功：票据结果为 nil，错误可按 ErrDurabilityUnconfirmed 识别。
	if ballot != nil {
		t.Fatalf("durability warning must not return a success ballot: %+v", ballot)
	}
	if !errors.Is(err, ErrDurabilityUnconfirmed) {
		t.Fatalf("err = %v, want errors.Is ErrDurabilityUnconfirmed", err)
	}
	var derr *DurabilityError
	if !errors.As(err, &derr) || derr.Cause == nil {
		t.Fatalf("err = %v (%T), want *DurabilityError with cause", err, err)
	}
	if !strings.Contains(err.Error(), derr.Cause.Error()) {
		t.Fatalf("err %q must preserve the underlying save failure %q", err, derr.Cause)
	}
	// 不能解释成投票资格不符或选择冲突。
	if errors.Is(err, ErrVoteRejected) {
		t.Fatalf("a valid first vote must not be rejected as a business rule: %v", err)
	}
	if errors.Is(err, ErrProposalConflict) {
		t.Fatalf("a first vote must not be reported as a conflict: %v", err)
	}

	// 恢复正常读取条件：报错时已写入的甲票完整登记——代表、归集权重、
	// 赞成选择与首次投票时间 150 都来自当次提交；丙的反对票保持原内容
	// 且排在其前；提案仍在投票中，没有提前计票。
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("restore dir: %v", err)
	}
	assertBingThenJiaBallots(t, store, id)
	assertNoFundsMoved(t, store, 1000)

	// 重新打开后读到完全相同的两张票与投票中状态。
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after durability warning: %v", err)
	}
	defer reopened.Close()
	assertBingThenJiaBallots(t, reopened, id)

	// 甲再次提交相同选择：即使提供的时间已经到达截止时刻，也返回 150 的
	// 首次记录且不追加票据。
	retry, err := reopened.CastVote(id, "甲", true, 200)
	if err != nil {
		t.Fatalf("same-choice retry at deadline: %v", err)
	}
	if retry.Representative != "甲" || retry.Weight != 700 || !retry.Support || retry.VotedAt != 150 {
		t.Fatalf("same-choice retry = %+v, want first record 甲 for 700 voted_at=150", retry)
	}
	// 改投反对按既有冲突规则拒绝，原票保持不变。
	if _, err := reopened.CastVote(id, "甲", false, 160); !errors.Is(err, ErrProposalConflict) {
		t.Fatalf("changing vote err=%v, want ErrProposalConflict", err)
	}
	assertBingThenJiaBallots(t, reopened, id)

	// 截止时刻计票：赞成 700、反对 300、参与 1000，达到法定人数且赞成严格
	// 多于反对 => 通过；报错时已写入的甲票必须计入，乙的权重不得重复累计。
	res, err := reopened.TallyVote(id, 200)
	if err != nil {
		t.Fatalf("tally: %v", err)
	}
	if !res.Passed || res.ForWeight != 700 || res.AgainstWeight != 300 ||
		res.Turnout != 1000 || res.Quorum != 600 || res.TalliedAt != 200 {
		t.Fatalf("tally = %+v, want passed for=700 against=300 turnout=1000 at 200", res)
	}
	// 计票同样不执行提案动作：资金余额与执行凭据保持原样。
	assertNoFundsMoved(t, reopened, 1000)
}

// TestVotePreReplaceFailureStillLeavesNoBallot：必要区分——同一提案场景下，
// 保存失败发生在状态文件替换之前时，行为与目录同步失败不同：没有新票据、
// 不占用首次投票机会，后续首次投票仍受窗口约束（截止时刻的首次票被拒绝），
// 到期计票只含丙的反对票，得出参与 300 的拒绝结论。
func TestVotePreReplaceFailureStillLeavesNoBallot(t *testing.T) {
	store, path := openTempStore(t, 1000)
	defer store.Close()
	const id = "gip-vote-pre-replace"
	mustCreateVote(t, store, delegatedVoteInput(id))
	if _, err := store.CastVote(id, "丙", false, 120); err != nil {
		t.Fatalf("丙 vote: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 目录不可写：提交在状态文件替换之前失败，甲的首次票没有写入。
	makeDirUnwritable(t, path)
	ballot, err := store.CastVote(id, "甲", true, 150)
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

	// 查询仍只有丙的反对票；状态文件字节与失败前完全一致。
	assertBingOnlyBallot := func(s *Store) {
		t.Helper()
		v, ok, err := s.VoteProposal(id)
		if err != nil || !ok {
			t.Fatalf("VoteProposal: ok=%v err=%v", ok, err)
		}
		if v.State != "voting" || v.Tally != nil {
			t.Fatalf("state=%s tally=%+v, want voting with no tally", v.State, v.Tally)
		}
		if len(v.Ballots) != 1 {
			t.Fatalf("ballots = %+v, want only 丙's ballot", v.Ballots)
		}
		if b := v.Ballots[0]; b.Representative != "丙" || b.Weight != 300 || b.Support || b.VotedAt != 120 {
			t.Fatalf("ballot = %+v, want 丙 weight=300 against voted_at=120", b)
		}
		assertDelegatedMembers(t, "after pre-replacement failure", v)
	}
	assertBingOnlyBallot(store)
	if after, rerr := os.ReadFile(path); rerr != nil || string(after) != string(before) {
		t.Fatalf("state file changed despite pre-replacement save failure")
	}

	// 恢复保存条件后，首次投票仍受窗口约束：截止时刻 200 的首次票按
	// 窗口边界 [100,200) 拒绝，失败尝试不曾占用这次机会。
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("restore dir: %v", err)
	}
	if _, err := store.CastVote(id, "甲", true, 200); !errors.Is(err, ErrVoteRejected) {
		t.Fatalf("first vote at deadline (exclusive) err=%v, want ErrVoteRejected", err)
	}
	assertBingOnlyBallot(store)

	// 到期计票只含丙的反对票：赞成 0、反对 300、参与 300，未达法定人数
	// => 拒绝；与目录同步失败场景（甲票已写入、参与 1000 通过）形成对照。
	res, err := store.TallyVote(id, 200)
	if err != nil {
		t.Fatalf("tally: %v", err)
	}
	if res.Passed || res.ForWeight != 0 || res.AgainstWeight != 300 ||
		res.Turnout != 300 || res.Quorum != 600 || res.TalliedAt != 200 {
		t.Fatalf("tally = %+v, want rejected for=0 against=300 turnout=300 at 200", res)
	}
	assertNoFundsMoved(t, store, 1000)
}
