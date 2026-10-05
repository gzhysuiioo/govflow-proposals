package govflow

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

// passVote 让一项带真实委托的投票提案由最终代表在窗口内完成投票与计票：
// alice 归集 bob、carol 的委托权重（300+200+100=600）投赞成，dave（400）投
// 反对；参与量 1000 达到法定人数 600 且赞成严格多于反对，首次计票结论为 passed。
// 全部时间由调用方给定，不使用真实时钟。
func passVote(t *testing.T, store *Store, id string, voteFor, voteAgainst, tallyAt int64) {
	t.Helper()
	// 已委托出去的成员不能代替最终代表投票：入口必须明确拒绝，
	// 证明通过结论只能由最终代表本人的票据产生。
	if _, err := store.CastVote(id, "bob", true, voteFor); !errors.Is(err, ErrVoteRejected) {
		t.Fatalf("delegated member bob voted directly: %v", err)
	}
	b, err := store.CastVote(id, "alice", true, voteFor)
	if err != nil {
		t.Fatalf("alice vote: %v", err)
	}
	if b.Weight != 600 {
		t.Fatalf("alice delegated ballot weight=%d, want 600", b.Weight)
	}
	if _, err := store.CastVote(id, "dave", false, voteAgainst); err != nil {
		t.Fatalf("dave vote: %v", err)
	}
	res, err := store.TallyVote(id, tallyAt)
	if err != nil {
		t.Fatalf("tally %s: %v", id, err)
	}
	if !res.Passed || res.ForWeight != 600 || res.AgainstWeight != 400 || res.Turnout != 1000 {
		t.Fatalf("%s tally=%+v, want passed for=600 against=400 turnout=1000", id, res)
	}
}

// assertGovernanceUnchanged 断言执行失败后提案查询仍展示 passed，且成员原始
// 权重、完整委托路径、逐票代表与首次投票时间、首次计票权重与时间都保持原值：
// 失败不得删除票据、不得重新生成计票结论。
func assertGovernanceUnchanged(t *testing.T, store *Store, id string, actions []string, forAt, againstAt, tallyAt int64) {
	t.Helper()
	v, ok, err := store.VoteProposal(id)
	if err != nil || !ok {
		t.Fatalf("VoteProposal(%s): ok=%v err=%v", id, ok, err)
	}
	if v.State != "passed" || v.Source != "vote" {
		t.Fatalf("%s state=%s source=%s, want passed/vote", id, v.State, v.Source)
	}
	if v.Quorum != 600 || v.TotalWeight != 1000 {
		t.Fatalf("%s quorum=%d total=%d, want 600/1000", id, v.Quorum, v.TotalWeight)
	}
	if !sameActions(v.Actions, actions) {
		t.Fatalf("%s actions=%v, want original raw actions %v", id, v.Actions, actions)
	}
	// 成员原始权重与完整委托路径逐字保留（bob→alice、carol→alice）。
	wantWeight := map[string]int64{"alice": 300, "bob": 200, "carol": 100, "dave": 400}
	wantPath := map[string][]string{
		"alice": {"alice"},
		"bob":   {"bob", "alice"},
		"carol": {"carol", "alice"},
		"dave":  {"dave"},
	}
	if len(v.Members) != 4 {
		t.Fatalf("%s members=%+v, want 4", id, v.Members)
	}
	for _, m := range v.Members {
		if m.Weight != wantWeight[m.ID] {
			t.Fatalf("%s member %s weight=%d, want %d", id, m.ID, m.Weight, wantWeight[m.ID])
		}
		if !equalStrings(m.Path, wantPath[m.ID]) {
			t.Fatalf("%s member %s path=%v, want %v", id, m.ID, m.Path, wantPath[m.ID])
		}
	}
	// 最终代表：alice 归集 bob、carol；alice 与 dave 各自代表自己。
	gotPaths := memberPaths(v)
	for id2, wantHead := range map[string]string{"alice": "alice", "bob": "alice", "carol": "alice", "dave": "dave"} {
		if gotPaths[id2].Delegate != wantHead {
			t.Fatalf("%s member %s final representative=%s, want %s", id, id2, gotPaths[id2].Delegate, wantHead)
		}
	}
	// 逐票代表、票重、选择与首次投票时间保持首次记录（按首次投票先后）。
	if len(v.Ballots) != 2 {
		t.Fatalf("%s ballots=%+v, want 2", id, v.Ballots)
	}
	b0, b1 := v.Ballots[0], v.Ballots[1]
	if b0.Representative != "alice" || b0.Weight != 600 || !b0.Support || b0.VotedAt != forAt {
		t.Fatalf("%s ballot 0=%+v, want alice/600/for/%d", id, b0, forAt)
	}
	if b1.Representative != "dave" || b1.Weight != 400 || b1.Support || b1.VotedAt != againstAt {
		t.Fatalf("%s ballot 1=%+v, want dave/400/against/%d", id, b1, againstAt)
	}
	// 首次计票权重与时间保持原值，查询结论仍为通过。
	if v.Tally == nil {
		t.Fatalf("%s tally missing after failed execution", id)
	}
	if v.Tally.ForWeight != 600 || v.Tally.AgainstWeight != 400 || v.Tally.Turnout != 1000 ||
		!v.Tally.Passed || v.Tally.TalliedAt != tallyAt {
		t.Fatalf("%s tally=%+v, want for=600 against=400 turnout=1000 passed tallied=%d",
			id, v.Tally, tallyAt)
	}
	// 再次计票仍返回首次结论，而不是重新生成。
	again, err := store.TallyVote(id, tallyAt+999)
	if err != nil || again.TalliedAt != tallyAt || again.ForWeight != 600 || again.AgainstWeight != 400 {
		t.Fatalf("%s repeat tally=%+v err=%v, want first verdict at %d", id, again, err, tallyAt)
	}
}

// TestPassedVoteProposalExecutionFailureFullyReverts：投票通过提案在时间锁到期
// 执行一组有序转账，后面的动作才暴露出问题（格式错误 / 前序消耗导致余额不足）时，
// 整项拒绝必须固定下来——不能只看到返回了错误，也不能用 register 直接登记的
// 提案冒充真正经过投票与计票的提案。失败后资金、票据、委托与首次计票结论都不得
// 发生部分变化，失败不产生成功凭据，也不消耗执行机会；合法且余额足够的投票提案
// 仍能正常转账并转为 executed。
func TestPassedVoteProposalExecutionFailureFullyReverts(t *testing.T) {
	store, path := openTempStore(t, 1000)
	defer store.Close()

	// 此前已有另一项提案的成功凭据（order=0），并给两个收款账户留下既有余额：
	// priora=40、priorb=60，资金库剩 900。失败后这份凭据的内容与提交次序必须原样。
	mustRegister(t, store, "gip-prior", 0, "transfer:priora:40", "transfer:priorb:60")
	if _, err := store.Execute("gip-prior", 0); err != nil {
		t.Fatalf("execute prior receipt: %v", err)
	}
	priorReceipt, ok, err := store.Receipt("gip-prior")
	if err != nil || !ok {
		t.Fatalf("prior receipt: ok=%v err=%v", ok, err)
	}
	if bal, _ := store.TreasuryBalance(); bal != 900 {
		t.Fatalf("setup treasury=%d, want 900", bal)
	}

	// 失败提案一：余额不足必须由前序转账消耗后触发。
	// 资金库 900，前 3 笔共支出 600（前两笔连续发给同一新账户 newa，第三笔发给
	// 已有余额的 priora），第 4 笔 400 在剩余 300 上才不足——它本身（400）并未
	// 超过执行前资金库（900），不是“第一笔就超额”的退化情形。
	fundActions := []string{
		"transfer:newa:100",
		"transfer:newa:200",
		"transfer:priora:300",
		"transfer:newb:400",
	}
	fundInput := baseVoteInput("gip-fail-funds")
	fundInput.Actions = fundActions
	mustCreateVote(t, store, fundInput)
	passVote(t, store, "gip-fail-funds", 150, 160, 200)

	// 失败提案二：格式错误按现有行为在创建时允许保存原文，执行时才拒绝。
	// 前两笔同样连续发给同一新账户 newc，第三笔发给已有余额的 priorb。
	badText := "withdraw:newd:9"
	formActions := []string{
		"transfer:newc:100",
		"transfer:newc:150",
		"transfer:priorb:50",
		badText,
	}
	formInput := baseVoteInput("gip-fail-form")
	formInput.Actions = formActions
	mustCreateVote(t, store, formInput)
	passVote(t, store, "gip-fail-form", 150, 160, 200)

	// register 与投票共用编号：直接登记不能覆盖或冒充真正投票+计票的提案。
	if _, err := store.Register("gip-fail-funds", 300, fundActions); !errors.Is(err, ErrProposalConflict) {
		t.Fatalf("register over voting proposal err=%v, want ErrProposalConflict", err)
	}

	// 时间锁到期执行：两项都在后面的动作上失败。状态文件在失败前后必须逐字节一致
	// （失败路径根本不得提交）。
	beforeBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, fundErr := store.Execute("gip-fail-funds", 300)
	if !errors.Is(fundErr, ErrExecutionRejected) || errors.Is(fundErr, ErrInvalidAction) {
		t.Fatalf("funds err=%v, want ErrExecutionRejected without ErrInvalidAction", fundErr)
	}
	if !strings.Contains(fundErr.Error(), "insufficient treasury balance at action 3") ||
		!strings.Contains(fundErr.Error(), "treasury=300") || !strings.Contains(fundErr.Error(), "amount=400") {
		t.Fatalf("funds error must classify cause and locate action 3: %v", fundErr)
	}
	_, formErr := store.Execute("gip-fail-form", 300)
	if !errors.Is(formErr, ErrExecutionRejected) || !errors.Is(formErr, ErrInvalidAction) {
		t.Fatalf("form err=%v, want both ErrExecutionRejected and ErrInvalidAction", formErr)
	}
	if !strings.Contains(formErr.Error(), `action 3 ("withdraw:newd:9")`) {
		t.Fatalf("form error must locate action 3 with original text: %v", formErr)
	}
	afterBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterBytes) != string(beforeBytes) {
		t.Fatal("failed executions wrote partial changes to the state file")
	}

	// 失败不消耗执行机会：再次执行仍是同一类整项拒绝，而不是成功或找不到提案。
	if _, err := store.Execute("gip-fail-funds", 301); !errors.Is(err, ErrExecutionRejected) ||
		!strings.Contains(err.Error(), "action 3") {
		t.Fatalf("retry after funds failure err=%v", err)
	}
	if _, err := store.Execute("gip-fail-form", 301); !errors.Is(err, ErrInvalidAction) {
		t.Fatalf("retry after form failure err=%v", err)
	}

	// ---- 余额查询必须与执行前完全一致 ----
	snap, err := store.BalanceSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snap.Treasury != 900 {
		t.Fatalf("treasury changed after failed executions: %d, want 900", snap.Treasury)
	}
	// 原本已有余额的收款账户没有增款。
	if snap.Balances["priora"] != 40 {
		t.Fatalf("existing priora balance=%d, want 40", snap.Balances["priora"])
	}
	if snap.Balances["priorb"] != 60 {
		t.Fatalf("existing priorb balance=%d, want 60", snap.Balances["priorb"])
	}
	// 前序动作涉及的新账户不能留下余额记录：即使前两笔发给同一个账户，
	// 也不能留下其中一笔的变化；键本身不得出现在余额表中。
	for _, acct := range []string{"newa", "newb", "newc", "newd"} {
		if bal, ok := snap.Balances[acct]; ok || bal != 0 {
			t.Fatalf("new account %s left a balance record: %d", acct, bal)
		}
		if bal, _ := store.Balance(acct); bal != 0 {
			t.Fatalf("Balance(%s)=%d, want 0", acct, bal)
		}
	}
	if len(snap.Balances) != 2 {
		t.Fatalf("balances table=%+v, want only priora/priorb", snap.Balances)
	}

	// ---- 提案仍展示 passed；成员、委托路径、逐票与首次计票结论保持原值 ----
	assertGovernanceUnchanged(t, store, "gip-fail-funds", fundActions, 150, 160, 200)
	assertGovernanceUnchanged(t, store, "gip-fail-form", formActions, 150, 160, 200)

	// 两项失败提案都没有成功执行凭据；此前的凭据仍是唯一一份，内容与次序不变。
	if r, ok, _ := store.Receipt("gip-fail-funds"); ok || r != nil {
		t.Fatalf("funds failure left a receipt: %+v", r)
	}
	if r, ok, _ := store.Receipt("gip-fail-form"); ok || r != nil {
		t.Fatalf("form failure left a receipt: %+v", r)
	}
	receipts, err := store.Receipts()
	if err != nil || len(receipts) != 1 {
		t.Fatalf("receipts=%v err=%v, want exactly the prior one", receipts, err)
	}
	if !reflect.DeepEqual(receipts[0], priorReceipt) {
		t.Fatalf("prior receipt changed:\nbefore=%+v\nafter =%+v", priorReceipt, receipts[0])
	}

	// 重开状态文件：失败提案仍是 passed、无凭据、治理记录原值、余额原值。
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after failed executions: %v", err)
	}
	defer reopened.Close()
	if bal, _ := reopened.TreasuryBalance(); bal != 900 {
		t.Fatalf("treasury after reopen=%d, want 900", bal)
	}
	reSnap, _ := reopened.BalanceSnapshot()
	if len(reSnap.Balances) != 2 || reSnap.Balances["priora"] != 40 || reSnap.Balances["priorb"] != 60 {
		t.Fatalf("balances after reopen=%+v", reSnap.Balances)
	}
	assertGovernanceUnchanged(t, reopened, "gip-fail-funds", fundActions, 150, 160, 200)
	assertGovernanceUnchanged(t, reopened, "gip-fail-form", formActions, 150, 160, 200)
	reopenedPrior, ok, err := reopened.Receipt("gip-prior")
	if err != nil || !ok || !reflect.DeepEqual(reopenedPrior, priorReceipt) {
		t.Fatalf("prior receipt after reopen differs: ok=%v err=%v", ok, err)
	}
	if rs, _ := reopened.Receipts(); len(rs) != 1 || rs[0].Order != 0 || rs[0].ProposalID != "gip-prior" {
		t.Fatalf("receipt order changed after failed executions: %+v", rs)
	}

	// ---- 成功对照：动作合法且余额足够的投票通过提案仍正常转账并转为已执行 ----
	okActions := []string{"transfer:ok1:250", "transfer:ok1:100", "transfer:ok2:50"}
	okInput := baseVoteInput("gip-ok")
	okInput.Actions = okActions
	okInput.StartAt, okInput.Deadline, okInput.TimelockEnd = 400, 500, 600
	mustCreateVote(t, reopened, okInput)
	passVote(t, reopened, "gip-ok", 420, 430, 500)
	okReceipt, err := reopened.Execute("gip-ok", 600)
	if err != nil {
		t.Fatalf("execute legal vote proposal: %v", err)
	}
	if okReceipt.Order != 1 || okReceipt.ExecutedAt != 600 || len(okReceipt.Actions) != 3 {
		t.Fatalf("success receipt=%+v, want order=1 executed_at=600 3 actions", okReceipt)
	}
	if bal, _ := reopened.TreasuryBalance(); bal != 500 {
		t.Fatalf("treasury after success=%d, want 500", bal)
	}
	if bal, _ := reopened.Balance("ok1"); bal != 350 {
		t.Fatalf("ok1=%d, want 350 (two transfers to same account)", bal)
	}
	if bal, _ := reopened.Balance("ok2"); bal != 50 {
		t.Fatalf("ok2=%d, want 50", bal)
	}
	// 既有账户不被成功对照影响；失败提案的新账户依然没有记录。
	if bal, _ := reopened.Balance("priora"); bal != 40 {
		t.Fatalf("priora=%d, want 40", bal)
	}
	for _, acct := range []string{"newa", "newb", "newc", "newd"} {
		if bal, _ := reopened.Balance(acct); bal != 0 {
			t.Fatalf("%s=%d after success control, want 0", acct, bal)
		}
	}
	v, _, _ := reopened.VoteProposal("gip-ok")
	if v.State != "executed" || !v.Tally.Passed {
		t.Fatalf("gip-ok state=%s tally=%+v, want executed/passed", v.State, v.Tally)
	}
	// 已执行投票提案重试返回首次凭据，不二次扣款。
	again, err := reopened.Execute("gip-ok", 9999)
	if err != nil || again.Order != 1 || again.ExecutedAt != 600 ||
		!reflect.DeepEqual(again.Actions, okReceipt.Actions) {
		t.Fatalf("success retry receipt changed: %+v err=%v", again, err)
	}
	if bal, _ := reopened.TreasuryBalance(); bal != 500 {
		t.Fatalf("treasury changed on success retry: %d", bal)
	}

	// 凭据按成功提交次序排列：gip-prior(order=0) 内容与次序原样，gip-ok(order=1) 在后。
	all, err := reopened.Receipts()
	if err != nil || len(all) != 2 {
		t.Fatalf("receipts=%v err=%v, want 2", all, err)
	}
	if all[0].ProposalID != "gip-prior" || all[0].Order != 0 ||
		!reflect.DeepEqual(all[0], priorReceipt) {
		t.Fatalf("prior receipt content/order changed after later activity:\nwant=%+v\ngot =%+v",
			priorReceipt, all[0])
	}
	if all[1].ProposalID != "gip-ok" || all[1].Order != 1 {
		t.Fatalf("success receipt ordering wrong: %+v", all[1])
	}
}
