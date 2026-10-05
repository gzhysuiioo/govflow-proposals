package govflow

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// TestExecutedProposalVoteRetryReturnsFirstBallot：已执行投票提案的投票重试
// 回归保障。重点保留一种真实情况：
//
//	frank→erin→dave 多跳委托，最终代表 dave 归集沿途成员权重投反对票；
//	alice 归集 bob、carol 的委托权重投赞成，赞成权重仍达到法定人数并形成
//	严格多数，提案经成员投票、截止计票、时间锁到期执行后变为 executed。
//
// 执行之后：
//   - 投过票的最终代表（赞成者与反对者）再次提交同一选择，无论传入时间早于
//     开始还是晚于截止，都必须取回首次票据：原代表编号、完整归集权重（含
//     沿途委托成员，不退回本人权重）、原选择、首次投票时间；票据与提案查询
//     中的原记录逐字一致，重试不落盘任何变化。
//   - 反对代表改为赞成必须明确报改投冲突（ErrProposalConflict），不能把
//     反对票覆盖成赞成，也不能仅因提案已执行就改报普通的投票关闭错误。
//   - 始终没有投票的最终代表此时提交首次选择，即使传入原投票窗口内的时间，
//     也必须因已经计票被拒绝（ErrVoteRejected），不能借旧时间追加票据。
//   - 两种失败都不得返回看似成功的新票。
//
// 全部重试与失败之后：提案仍为 executed；票据数量、顺序、选择、权重与首次
// 时间保持原样；首次计票的赞成、反对、参与权重与计票时间保持原样；资金库
// 与收款账户余额停留在首次执行后的数值；成功执行凭据仍是原来的一份，
// order/执行时间/动作留痕不变。重开状态文件后结论一致。
//
// 直接 register 登记并执行的 passed 提案没有投票留痕：投票入口在该编号上
// 只能得到“提案不存在”，不能代替本用例要求的投票→计票→执行使用条件。
func TestExecutedProposalVoteRetryReturnsFirstBallot(t *testing.T) {
	store, path := openTempStore(t, 1000)
	defer store.Close()

	const id = "gip-exec-retry"
	in := &CreateVoteInput{
		ID: id,
		Members: []VoteMember{
			{ID: "alice", Weight: 300},
			{ID: "bob", Weight: 200},
			{ID: "carol", Weight: 100},
			// 多跳链 frank→erin→dave：dave 是最终代表，归集 50+50+50=150。
			{ID: "dave", Weight: 50},
			{ID: "erin", Weight: 50},
			{ID: "frank", Weight: 50},
			// 始终没有投票的最终代表，执行之后再尝试投首次票。
			{ID: "gina", Weight: 100},
		},
		Delegations: []Delegation{
			{From: "bob", To: "alice"},
			{From: "carol", To: "alice"},
			{From: "erin", To: "dave"},
			{From: "frank", To: "erin"},
		},
		// 总权重 850；参与只有 alice(600)+dave(150)=750，达到法定人数 600，
		// 且赞成 600 严格多于反对 150。
		Quorum:      600,
		StartAt:     100,
		Deadline:    200,
		TimelockEnd: 300,
		Actions:     []string{"transfer:audits:100"},
	}
	view := mustCreateVote(t, store, in)
	if view.State != "voting" {
		t.Fatalf("new proposal state=%s, want voting", view.State)
	}

	// 委托路径、最终代表与归集权重：frank 的两跳链必须完整保留到 dave。
	wantPath := map[string][]string{
		"alice": {"alice"},
		"bob":   {"bob", "alice"},
		"carol": {"carol", "alice"},
		"dave":  {"dave"},
		"erin":  {"erin", "dave"},
		"frank": {"frank", "erin", "dave"},
		"gina":  {"gina"},
	}
	wantHead := map[string]string{
		"alice": "alice", "bob": "alice", "carol": "alice",
		"dave": "dave", "erin": "dave", "frank": "dave", "gina": "gina",
	}
	for _, m := range view.Members {
		if !equalStrings(m.Path, wantPath[m.ID]) {
			t.Fatalf("member %s path=%v, want %v", m.ID, m.Path, wantPath[m.ID])
		}
		if m.Delegate != wantHead[m.ID] {
			t.Fatalf("member %s final representative=%s, want %s", m.ID, m.Delegate, wantHead[m.ID])
		}
	}

	// 只有最终代表能投票：委托链上的 erin、frank 不能代替 dave 投票。
	if _, err := store.CastVote(id, "erin", false, 150); !errors.Is(err, ErrVoteRejected) {
		t.Fatalf("delegated erin voted directly: %v", err)
	}
	if _, err := store.CastVote(id, "frank", false, 150); !errors.Is(err, ErrVoteRejected) {
		t.Fatalf("delegated frank voted directly: %v", err)
	}

	// 反对票先投（150），赞成票后投（160）：固定票据顺序为 dave、alice。
	againstFirst, err := store.CastVote(id, "dave", false, 150)
	if err != nil {
		t.Fatalf("dave against vote: %v", err)
	}
	// 归集权重必须包含沿途委托成员 erin、frank：150，而不是 dave 本人的 50。
	if againstFirst.Representative != "dave" || againstFirst.Weight != 150 ||
		againstFirst.Support || againstFirst.VotedAt != 150 {
		t.Fatalf("dave first ballot=%+v, want dave/150/against/150", againstFirst)
	}
	forFirst, err := store.CastVote(id, "alice", true, 160)
	if err != nil {
		t.Fatalf("alice for vote: %v", err)
	}
	if forFirst.Representative != "alice" || forFirst.Weight != 600 ||
		!forFirst.Support || forFirst.VotedAt != 160 {
		t.Fatalf("alice first ballot=%+v, want alice/600/for/160", forFirst)
	}

	// 截止计票：赞成 600、反对 150、参与 750，达到法定人数且严格多数 => 通过。
	tally, err := store.TallyVote(id, 200)
	if err != nil {
		t.Fatalf("tally: %v", err)
	}
	if !tally.Passed || tally.ForWeight != 600 || tally.AgainstWeight != 150 ||
		tally.Turnout != 750 || tally.Quorum != 600 || tally.TalliedAt != 200 {
		t.Fatalf("tally=%+v, want passed for=600 against=150 turnout=750 quorum=600 at=200", tally)
	}
	passed, ok, err := store.VoteProposal(id)
	if err != nil || !ok || passed.State != "passed" {
		t.Fatalf("state after tally state=%s ok=%v err=%v, want passed", passed.State, ok, err)
	}

	// 时间锁到期执行：真实转账一次，状态转为 executed。
	receipt, err := store.Execute(id, 300)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if receipt.Order != 0 || receipt.ExecutedAt != 300 || receipt.ProposalID != id ||
		len(receipt.Actions) != 1 || receipt.Actions[0].Index != 0 ||
		receipt.Actions[0].Action != "transfer:audits:100" {
		t.Fatalf("first receipt=%+v, want order=0 executed_at=300 one action index=0", receipt)
	}
	if bal, _ := store.TreasuryBalance(); bal != 900 {
		t.Fatalf("treasury after execute=%d, want 900", bal)
	}
	if bal, _ := store.Balance("audits"); bal != 100 {
		t.Fatalf("audits balance=%d, want 100", bal)
	}
	execView, _, err := store.VoteProposal(id)
	if err != nil || execView.State != "executed" || !execView.Tally.Passed {
		t.Fatalf("state after execute state=%s tally=%+v err=%v, want executed/passed",
			execView.State, execView.Tally, err)
	}

	// ---- 执行之后：原代表同选择重试始终取回首次票据 ----

	// 反对代表：传入早于开始时刻的时间，不能替换原票时间，也不能再套
	// 首次投票的时间窗口把它判成窗口外失败。
	againEarly, err := store.CastVote(id, "dave", false, 50)
	if err != nil {
		t.Fatalf("dave identical retry before start: %v", err)
	}
	if !reflect.DeepEqual(againEarly, againstFirst) {
		t.Fatalf("dave retry(50)=%+v, want first ballot %+v", againEarly, againstFirst)
	}
	// 反对代表：传入晚于截止（也晚于执行）的时间，结果完全相同。
	againLate, err := store.CastVote(id, "dave", false, 9999)
	if err != nil {
		t.Fatalf("dave identical retry after deadline: %v", err)
	}
	if !reflect.DeepEqual(againLate, againstFirst) {
		t.Fatalf("dave retry(9999)=%+v, want first ballot %+v", againLate, againstFirst)
	}
	// 赞成代表同样能从原投票入口取回自己的记录（早于开始的时间）。
	againFor, err := store.CastVote(id, "alice", true, 99)
	if err != nil {
		t.Fatalf("alice identical retry before start: %v", err)
	}
	if !reflect.DeepEqual(againFor, forFirst) {
		t.Fatalf("alice retry(99)=%+v, want first ballot %+v", againFor, forFirst)
	}

	// 重试取回的票据必须与提案查询中的原记录逐字一致（代表、归集权重、
	// 选择、首次时间），而不是新构造的票。
	queried, _, err := store.VoteProposal(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(queried.Ballots) != 2 {
		t.Fatalf("ballot count after retries=%d, want 2", len(queried.Ballots))
	}
	if !reflect.DeepEqual(queried.Ballots[0], *againLate) {
		t.Fatalf("retried dave ballot=%+v differs from query record=%+v",
			againLate, queried.Ballots[0])
	}
	if !reflect.DeepEqual(queried.Ballots[1], *againFor) {
		t.Fatalf("retried alice ballot=%+v differs from query record=%+v",
			againFor, queried.Ballots[1])
	}

	// ---- 执行之后：改投必须明确报改投冲突 ----

	// 反对代表改为赞成：ErrProposalConflict，错误必须说明“已投过反对、
	// 不允许改投”；不能仅因已执行就退化成普通的投票关闭错误，
	// 也不能把反对票覆盖成赞成。
	change, err := store.CastVote(id, "dave", true, 150)
	if !errors.Is(err, ErrProposalConflict) {
		t.Fatalf("change vote err=%v, want ErrProposalConflict", err)
	}
	if change != nil {
		t.Fatalf("change vote returned a success-looking ballot: %+v", change)
	}
	if msg := err.Error(); !containsAll(msg, "already voted against", "changing the vote is not allowed") {
		t.Fatalf("change-vote error %q must report the vote-change conflict, not a plain closed error", msg)
	}
	if errors.Is(err, ErrVoteRejected) {
		t.Fatalf("change vote must stay ErrProposalConflict, got vote-rejected: %v", err)
	}
	// 即便传入晚于执行的时间，分类仍是改投冲突。
	if _, err := store.CastVote(id, "dave", true, 9999); !errors.Is(err, ErrProposalConflict) {
		t.Fatalf("change vote after execution err=%v, want ErrProposalConflict", err)
	}
	// 赞成代表反向改投同样是冲突。
	if _, err := store.CastVote(id, "alice", false, 160); !errors.Is(err, ErrProposalConflict) {
		t.Fatalf("alice change vote err=%v, want ErrProposalConflict", err)
	}

	// ---- 执行之后：从未投票的最终代表不能借旧时间追加首次票据 ----

	// gina 传原投票窗口内的时间：提案已经计票，必须拒绝，且不能返回新票。
	late, err := store.CastVote(id, "gina", true, 150)
	if !errors.Is(err, ErrVoteRejected) {
		t.Fatalf("first vote after tally err=%v, want ErrVoteRejected", err)
	}
	if late != nil {
		t.Fatalf("rejected first vote returned a success-looking ballot: %+v", late)
	}
	if msg := err.Error(); !containsAll(msg, "already been tallied", "voting is closed") {
		t.Fatalf("late first-vote error %q must report tally-closed, not a window/conflict error", msg)
	}
	if errors.Is(err, ErrProposalConflict) {
		t.Fatalf("late first vote must be ErrVoteRejected, not conflict: %v", err)
	}
	// 投反对、窗口边界时间同样被拒，旧时间不能追加票据。
	if b, err := store.CastVote(id, "gina", false, 199); !errors.Is(err, ErrVoteRejected) || b != nil {
		t.Fatalf("gina against at 199: ballot=%+v err=%v, want rejection with no ballot", b, err)
	}

	// ---- 全部重试与失败之后：治理记录、资金与凭据保持首次执行后的原样 ----
	frozen := BalanceExpectation{Treasury: 900, Balances: map[string]int64{"audits": 100}}
	assertExecutedGovernanceFrozen(t, store, id, receipt, "after retries", frozen)

	// 成功执行重试仍是同一份凭据，不二次扣款（动作留痕也逐字一致）。
	execAgain, err := store.Execute(id, 9999)
	if err != nil || !reflect.DeepEqual(execAgain, receipt) {
		t.Fatalf("execute retry receipt=%+v err=%v, want original receipt %+v", execAgain, err, receipt)
	}
	if bal, _ := store.TreasuryBalance(); bal != 900 {
		t.Fatalf("treasury changed on execute retry: %d", bal)
	}
	assertExecutedGovernanceFrozen(t, store, id, receipt, "after execute retry", frozen)

	// ---- 直接登记的已通过提案没有投票留痕，不能代替本使用条件 ----
	mustRegister(t, store, "gip-direct", 0, "transfer:legal:5")
	if _, err := store.Execute("gip-direct", 0); err != nil {
		t.Fatalf("execute directly registered proposal: %v", err)
	}
	// register 来源在投票入口不可见：既没有票据可重试，也不接受新票。
	if v, ok, err := store.VoteProposal("gip-direct"); err != nil || ok || v != nil {
		t.Fatalf("registered proposal must not appear as a voting proposal: %+v ok=%v err=%v", v, ok, err)
	}
	if b, err := store.CastVote("gip-direct", "alice", true, 0); !errors.Is(err, ErrProposalNotFound) || b != nil {
		t.Fatalf("vote on registered-only proposal: ballot=%+v err=%v, want ErrProposalNotFound", b, err)
	}
	// 登记提案自身按登记来源可查且已执行。
	if rp, ok, err := store.Proposal("gip-direct"); err != nil || !ok || rp.State != "executed" {
		t.Fatalf("registered proposal query=%+v ok=%v err=%v", rp, ok, err)
	}

	// 直登提案的活动之后，受保护提案的票据重试与冻结状态仍然不变：
	// 治理记录与原凭据逐字保留（直登提案的合法转账不改变它们）。
	postDirect, err := store.CastVote(id, "dave", false, 1)
	if err != nil || !reflect.DeepEqual(postDirect, againstFirst) {
		t.Fatalf("dave retry after unrelated activity=%+v err=%v, want first ballot", postDirect, err)
	}
	assertExecutedGovernanceFrozen(t, store, id, receipt, "after unrelated direct proposal",
		BalanceExpectation{Treasury: 895, Balances: map[string]int64{"audits": 100, "legal": 5}})

	// 凭据表新增的只是直登提案那一份；原提案凭据仍是 order=0 的同一份。
	all, err := store.Receipts()
	if err != nil || len(all) != 2 {
		t.Fatalf("receipts=%v err=%v, want 2", all, err)
	}
	if !reflect.DeepEqual(all[0], receipt) || all[0].Order != 0 || all[0].ExecutedAt != 300 {
		t.Fatalf("original receipt changed:\nwant=%+v\ngot =%+v", receipt, all[0])
	}
	if all[1].ProposalID != "gip-direct" || all[1].Order != 1 || all[1].ExecutedAt != 0 {
		t.Fatalf("direct receipt=%+v, want gip-direct order=1 executed_at=0", all[1])
	}

	// ---- 重开状态文件：重试结论、失败分类与全部冻结记录逐项一致 ----
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	postReopen := BalanceExpectation{Treasury: 895, Balances: map[string]int64{"audits": 100, "legal": 5}}
	assertExecutedGovernanceFrozen(t, reopened, id, receipt, "after reopen", postReopen)
	// 重开后同选择重试仍取回首次票据（含完整归集权重与首次时间）。
	reAgainst, err := reopened.CastVote(id, "dave", false, 0)
	if err != nil || !reflect.DeepEqual(reAgainst, againstFirst) {
		t.Fatalf("dave retry after reopen=%+v err=%v, want first ballot", reAgainst, err)
	}
	reFor, err := reopened.CastVote(id, "alice", true, 100000)
	if err != nil || !reflect.DeepEqual(reFor, forFirst) {
		t.Fatalf("alice retry after reopen=%+v err=%v, want first ballot", reFor, err)
	}
	// 重开后改投仍是冲突，迟到代表的首次票仍被拒绝，且都不返回票据。
	if b, err := reopened.CastVote(id, "dave", true, 150); !errors.Is(err, ErrProposalConflict) || b != nil {
		t.Fatalf("change vote after reopen: ballot=%+v err=%v", b, err)
	}
	if b, err := reopened.CastVote(id, "gina", true, 150); !errors.Is(err, ErrVoteRejected) || b != nil {
		t.Fatalf("gina first vote after reopen: ballot=%+v err=%v", b, err)
	}
	assertExecutedGovernanceFrozen(t, reopened, id, receipt, "after reopen retries", postReopen)
}

// containsAll 判断 msg 是否同时包含全部子串。
func containsAll(msg string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(msg, sub) {
			return false
		}
	}
	return true
}

// BalanceExpectation 是断言时刻资金库与全部收款账户应有的余额。
type BalanceExpectation struct {
	Treasury int64
	Balances map[string]int64
}

// assertExecutedGovernanceFrozen 断言已执行提案在投票重试/失败之后治理与资金
// 记录完全停留在首次执行后的状态：
//   - 状态 executed、来源 vote；两张票据的顺序/代表/归集权重/选择/首次时间不变；
//   - 首次计票的赞成 600、反对 150、参与 750、法定人数 600 与计票时间 200 不变；
//   - 资金库与全部收款账户余额与 want 完全一致（没有借重试产生的陌生账户）；
//   - 原提案的成功凭据仍是传入的同一份（order=0、executed_at=300、一项动作）。
func assertExecutedGovernanceFrozen(t *testing.T, s *Store, id string, receipt *Receipt, phase string, want BalanceExpectation) {
	t.Helper()
	v, ok, err := s.VoteProposal(id)
	if err != nil || !ok {
		t.Fatalf("%s: VoteProposal(%s) ok=%v err=%v", phase, id, ok, err)
	}
	if v.Source != "vote" || v.State != "executed" {
		t.Fatalf("%s: source=%s state=%s, want vote/executed", phase, v.Source, v.State)
	}
	if v.Quorum != 600 || v.TotalWeight != 850 {
		t.Fatalf("%s: quorum=%d total=%d, want 600/850", phase, v.Quorum, v.TotalWeight)
	}
	// 票据数量、顺序、选择、权重与首次时间保持原样。
	if len(v.Ballots) != 2 {
		t.Fatalf("%s: ballots=%+v, want exactly 2", phase, v.Ballots)
	}
	b0, b1 := v.Ballots[0], v.Ballots[1]
	if b0.Representative != "dave" || b0.Weight != 150 || b0.Support || b0.VotedAt != 150 {
		t.Fatalf("%s: ballot 0=%+v, want dave/150/against/150", phase, b0)
	}
	if b1.Representative != "alice" || b1.Weight != 600 || !b1.Support || b1.VotedAt != 160 {
		t.Fatalf("%s: ballot 1=%+v, want alice/600/for/160", phase, b1)
	}
	// 首次计票结论不因重试或执行而改变。
	if v.Tally == nil {
		t.Fatalf("%s: tally missing", phase)
	}
	if v.Tally.ForWeight != 600 || v.Tally.AgainstWeight != 150 || v.Tally.Turnout != 750 ||
		v.Tally.Quorum != 600 || !v.Tally.Passed || v.Tally.TalliedAt != 200 {
		t.Fatalf("%s: tally=%+v, want for=600 against=150 turnout=750 quorum=600 passed at=200",
			phase, v.Tally)
	}
	// 再次计票返回首次结论，时间不被替换。
	again, err := s.TallyVote(id, 9999)
	if err != nil || again.TalliedAt != 200 || again.ForWeight != 600 ||
		again.AgainstWeight != 150 || again.Turnout != 750 {
		t.Fatalf("%s: repeat tally=%+v err=%v, want first verdict at 200", phase, again, err)
	}
	// 资金停留在断言时刻应有的数值，账户集合逐字一致。
	if bal, _ := s.TreasuryBalance(); bal != want.Treasury {
		t.Fatalf("%s: treasury=%d, want %d", phase, bal, want.Treasury)
	}
	snap, err := s.BalanceSnapshot()
	if err != nil {
		t.Fatalf("%s: snapshot: %v", phase, err)
	}
	if len(snap.Balances) != len(want.Balances) {
		t.Fatalf("%s: balance accounts=%+v, want %+v", phase, snap.Balances, want.Balances)
	}
	for account, balance := range want.Balances {
		if got := snap.Balances[account]; got != balance {
			t.Fatalf("%s: balance of %s=%d, want %d", phase, account, got, balance)
		}
	}
	// 原提案凭据仍是同一份；执行时间与动作留痕不变。
	got, ok, err := s.Receipt(id)
	if err != nil || !ok {
		t.Fatalf("%s: receipt ok=%v err=%v", phase, ok, err)
	}
	if !reflect.DeepEqual(got, receipt) {
		t.Fatalf("%s: receipt changed:\nwant=%+v\ngot =%+v", phase, receipt, got)
	}
}
