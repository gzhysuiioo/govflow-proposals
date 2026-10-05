package govflow

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// 本文件回归“已通过投票提案执行失败时的整项原子性”：
// 后期动作才暴露问题（格式错误 / 前序转账消耗余额导致的资金不足）时，
// 资金、余额表、投票结论、票据、计票结果与既有凭据都不得发生部分变化；
// 投票入口不能被 register 绕过。另保留一个动作合法、余额足够的成功对照。
//
// 全部时间由调用方显式给出，不读取真实时钟、不依赖外部服务。

// passedVoteFixture 是各用例共用的、带真实多跳委托的提案构造：
// carol -> bob -> alice，alice 作为最终代表持有 alice+bob+carol 共 600 权重；
// dave 未委托，代表自己的 400 权重。成员总权重 1000，法定人数 700。
// alice 在窗口内投赞成（now=150），dave 在窗口内投反对（now=160）：
// 参与量 1000 达到法定人数，且赞成 600 严格多于反对 400，首次计票（now=200）
// 结论为 passed。返回的提案时间锁为 300。
func passedVoteFixture(t *testing.T, store *Store, id string, actions []string) *VoteProposalView {
	t.Helper()
	in := &CreateVoteInput{
		ID: id,
		Members: []VoteMember{
			{ID: "alice", Weight: 300},
			{ID: "bob", Weight: 200},
			{ID: "carol", Weight: 100},
			{ID: "dave", Weight: 400},
		},
		// 真实多跳委托：carol 经 bob 最终委托到 alice。
		Delegations: []Delegation{{From: "carol", To: "bob"}, {From: "bob", To: "alice"}},
		Quorum:      700,
		StartAt:     100,
		Deadline:    200,
		TimelockEnd: 300,
		Actions:     actions,
	}
	// 编码合法但格式错误的动作必须能原样保存到提案中（创建不拒绝），
	// 留待执行时按现有规则拒绝。
	created := mustCreateVote(t, store, in)
	if !reflect.DeepEqual(created.Actions, actions) {
		t.Fatalf("proposal %s actions not saved verbatim: got %v want %v", id, created.Actions, actions)
	}
	if b, err := store.CastVote(id, "alice", true, 150); err != nil || b.Weight != 600 {
		t.Fatalf("alice vote (weight must be 600 via delegation): %+v err=%v", b, err)
	}
	if b, err := store.CastVote(id, "dave", false, 160); err != nil || b.Weight != 400 {
		t.Fatalf("dave vote (weight must be 400): %+v err=%v", b, err)
	}
	tally, err := store.TallyVote(id, 200)
	if err != nil {
		t.Fatalf("TallyVote(%s): %v", id, err)
	}
	if !tally.Passed || tally.ForWeight != 600 || tally.AgainstWeight != 400 ||
		tally.Turnout != 1000 || tally.Quorum != 700 || tally.TalliedAt != 200 {
		t.Fatalf("unexpected tally: %+v", tally)
	}
	return created
}

// assertVoteConclusionPreserved 固定失败后全部投票治理记录保持首次值：
// 提案仍为 passed；成员原始权重与完整委托路径不变；逐票代表、票重、选择与
// 首次投票时间不变；首次计票权重与时间不变，再次计票只返回首次结论，
// 不会因为执行失败而删除票据或重新生成计票结论；动作原文（含格式错误项）不变。
func assertVoteConclusionPreserved(t *testing.T, store *Store, id string, actions []string) {
	t.Helper()
	v, ok, err := store.VoteProposal(id)
	if err != nil || !ok {
		t.Fatalf("VoteProposal(%s) ok=%v err=%v", id, ok, err)
	}
	if v.State != "passed" {
		t.Fatalf("proposal %s state=%s, want passed (failure must not change state)", id, v.State)
	}
	if v.Source != "vote" || v.Quorum != 700 || v.TotalWeight != 1000 ||
		v.StartAt != 100 || v.Deadline != 200 || v.TimelockEnd != 300 {
		t.Fatalf("proposal %s parameters changed: %+v", id, v)
	}
	if !reflect.DeepEqual(v.Actions, actions) {
		t.Fatalf("proposal %s actions changed: got %v want %v", id, v.Actions, actions)
	}

	wantMembers := []struct {
		id                 string
		weight             int64
		path               []string
		delegate, directTo string
	}{
		{"alice", 300, []string{"alice"}, "alice", ""},
		{"bob", 200, []string{"bob", "alice"}, "alice", "alice"},
		{"carol", 100, []string{"carol", "bob", "alice"}, "alice", "bob"},
		{"dave", 400, []string{"dave"}, "dave", ""},
	}
	if len(v.Members) != len(wantMembers) {
		t.Fatalf("member roster changed: %+v", v.Members)
	}
	for i, w := range wantMembers {
		m := v.Members[i]
		if m.ID != w.id || m.Weight != w.weight || m.Delegate != w.delegate ||
			m.Direct != w.directTo || !reflect.DeepEqual(m.Path, w.path) {
			t.Fatalf("member %d changed: got %+v want id=%s weight=%d path=%v delegate=%s direct=%s",
				i, m, w.id, w.weight, w.path, w.delegate, w.directTo)
		}
	}

	wantBallots := []BallotView{
		{Representative: "alice", Weight: 600, Support: true, VotedAt: 150},
		{Representative: "dave", Weight: 400, Support: false, VotedAt: 160},
	}
	if !reflect.DeepEqual(v.Ballots, wantBallots) {
		t.Fatalf("ballots changed after failed execution: got %+v want %+v", v.Ballots, wantBallots)
	}
	if v.Tally == nil {
		t.Fatalf("tally conclusion lost after failed execution")
	}
	if v.Tally.ForWeight != 600 || v.Tally.AgainstWeight != 400 || v.Tally.Turnout != 1000 ||
		!v.Tally.Passed || v.Tally.TalliedAt != 200 {
		t.Fatalf("first tally changed after failed execution: %+v", v.Tally)
	}

	// 再次计票必须返回首次结论：即使传入更晚时间，首次计票权重与时间保持原值。
	again, err := store.TallyVote(id, 999)
	if err != nil {
		t.Fatalf("repeat tally after failure: %v", err)
	}
	if again.ForWeight != 600 || again.AgainstWeight != 400 || again.TalliedAt != 200 || !again.Passed {
		t.Fatalf("repeat tally regenerated conclusion: %+v", again)
	}
}

// TestPassedVoteProposalExecutionFailureIsAtomic：通过真实投票与计票取得 passed
// 结论的提案，在时间锁到期执行一组有序转账时，后面的动作才失败——
// 整项提案必须被整体拒绝：不留下任何部分资金变化、票据删除、计票重算或凭据。
func TestPassedVoteProposalExecutionFailureIsAtomic(t *testing.T) {
	store, path := openTempStore(t, 1000)
	defer store.Close()

	// 先放一份“此前其他提案”的成功凭据，并让 incumbent 账户预先存在余额 250：
	// 资金库 1000 -> 750。它用于核对失败不影响已有凭据的内容与提交次序，
	// 也用于核对已有余额的收款账户不会被增款。
	mustRegister(t, store, "gip-prior", 0, "transfer:incumbent:250")
	prior, err := store.Execute("gip-prior", 50)
	if err != nil {
		t.Fatalf("setup prior receipt: %v", err)
	}
	if prior.Order != 0 || prior.ExecutedAt != 50 || len(prior.Actions) != 1 {
		t.Fatalf("unexpected prior receipt: %+v", prior)
	}

	before, err := store.BalanceSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if before.Treasury != 750 || before.Balances["incumbent"] != 250 || len(before.Balances) != 1 {
		t.Fatalf("baseline balances wrong: %+v", before)
	}

	// assertPriorReceiptUnchanged：既有凭据的内容与提交次序保持原样，
	// 失败提案没有成功凭据，失败也不占用 order。
	assertPriorReceiptUnchanged := func(stage, failedID string) {
		t.Helper()
		rs, err := store.Receipts()
		if err != nil {
			t.Fatalf("%s: Receipts: %v", stage, err)
		}
		if len(rs) != 1 {
			t.Fatalf("%s: receipts changed (failures must not append/consume order): %+v", stage, rs)
		}
		r := rs[0]
		if r.ProposalID != "gip-prior" || r.Order != 0 || r.ExecutedAt != 50 || len(r.Actions) != 1 ||
			r.Actions[0].Action != "transfer:incumbent:250" || r.Actions[0].Index != 0 ||
			r.Actions[0].Treasury.Before != 1000 || r.Actions[0].Treasury.After != 750 ||
			r.Actions[0].Recipient.Account != "incumbent" ||
			r.Actions[0].Recipient.Before != 0 || r.Actions[0].Recipient.After != 250 {
			t.Fatalf("%s: prior receipt content/order changed: %+v", stage, r)
		}
		if got, ok, err := store.Receipt(failedID); err != nil || ok || got != nil {
			t.Fatalf("%s: failed proposal %s has a receipt: %+v ok=%v err=%v",
				stage, failedID, got, ok, err)
		}
	}

	// assertBalancesUnchanged：失败后余额快照必须与执行前逐字一致——
	// 资金库未扣款、已有余额账户未增款、前序动作涉及的新账户无余额记录。
	assertBalancesUnchanged := func(stage string, newAccounts []string) {
		t.Helper()
		after, err := store.BalanceSnapshot()
		if err != nil {
			t.Fatalf("%s: BalanceSnapshot: %v", stage, err)
		}
		if after.Treasury != before.Treasury {
			t.Fatalf("%s: treasury changed %d -> %d", stage, before.Treasury, after.Treasury)
		}
		if !reflect.DeepEqual(after.Balances, before.Balances) {
			t.Fatalf("%s: balances changed:\nbefore=%v\nafter =%v", stage, before.Balances, after.Balances)
		}
		for _, acct := range newAccounts {
			if bal, ok := after.Balances[acct]; ok {
				t.Fatalf("%s: preceding action left balance record for new account %s=%d", stage, acct, bal)
			}
		}
	}

	cases := []struct {
		name        string
		id          string
		actions     []string
		wantErrText string
		malformed   bool     // true：必须同时匹配 ErrInvalidAction；false：只能匹配 ErrExecutionRejected
		newAccounts []string // 预演中前序动作涉及、失败后不得留下记录的新账户
	}{
		{
			name: "malformed action rejected at execution",
			id:   "gip-badfmt",
			// 前两笔发给同一个新账户 newa（两笔都不得留下变化），第三笔才是格式错误。
			actions:     []string{"transfer:newa:100", "transfer:newa:50", "withdraw:x:10"},
			wantErrText: `action 2 ("withdraw:x:10")`,
			malformed:   true,
			newAccounts: []string{"newa"},
		},
		{
			name: "insufficient balance only after earlier transfers consume treasury",
			id:   "gip-badfunds",
			// 前两笔发给同一个已有余额账户 incumbent（两笔都不得留下变化）；
			// 资金库预演 750 -> 550 -> 350 后，第三笔 400 才不足。
			// 400 本身不超过初始资金库 750：不足只能由前序转账消耗余额触发。
			actions:     []string{"transfer:incumbent:200", "transfer:incumbent:200", "transfer:newb:400"},
			wantErrText: "insufficient treasury balance at action 2: treasury=350 amount=400",
			malformed:   false,
			newAccounts: []string{"newb"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			passedVoteFixture(t, store, tc.id, tc.actions)

			// 投票入口的整项拒绝必须固定：不能用直接登记替代真正经过投票与
			// 计票的提案——同编号 register 一律冲突，投票结论不被覆盖或绕过。
			if _, err := store.Register(tc.id, 300, tc.actions); !errors.Is(err, ErrProposalConflict) {
				t.Fatalf("register over vote proposal: err=%v, want ErrProposalConflict", err)
			}

			_, err := store.Execute(tc.id, 300)
			if !errors.Is(err, ErrExecutionRejected) {
				t.Fatalf("Execute err=%v, want ErrExecutionRejected", err)
			}
			if tc.malformed {
				// 格式问题必须能与资金不足区分：同时匹配 ErrInvalidAction。
				if !errors.Is(err, ErrInvalidAction) {
					t.Fatalf("malformed err=%v, also want ErrInvalidAction", err)
				}
			} else if errors.Is(err, ErrInvalidAction) {
				t.Fatalf("insufficient-balance err=%v must NOT match ErrInvalidAction", err)
			}
			if !strings.Contains(err.Error(), tc.wantErrText) {
				t.Fatalf("error %q does not contain %q (must locate failing action position)",
					err.Error(), tc.wantErrText)
			}

			assertBalancesUnchanged("after failed execute", tc.newAccounts)
			assertVoteConclusionPreserved(t, store, tc.id, tc.actions)
			assertPriorReceiptUnchanged("after failed execute", tc.id)

			// 失败不消耗执行机会但动作仍非法/仍不足：再次执行必须以同样的分类与
			// 位置被拒绝，且依旧没有任何变化。
			_, err = store.Execute(tc.id, 401)
			if !errors.Is(err, ErrExecutionRejected) ||
				(tc.malformed && !errors.Is(err, ErrInvalidAction)) ||
				(!tc.malformed && errors.Is(err, ErrInvalidAction)) ||
				!strings.Contains(err.Error(), tc.wantErrText) {
				t.Fatalf("retry after failure changed classification/position: %v", err)
			}
			assertBalancesUnchanged("after retried execute", tc.newAccounts)
			assertPriorReceiptUnchanged("after retried execute", tc.id)

			// 失败后 register 仍不能接管该编号。
			if _, err := store.Register(tc.id, 300, tc.actions); !errors.Is(err, ErrProposalConflict) {
				t.Fatalf("register after failed execute: err=%v, want ErrProposalConflict", err)
			}
		})
	}

	// 重开状态文件：失败从未提交任何部分内容，磁盘上仍是完整一致状态，
	// 两份失败提案仍可查到 passed、票据与首次计票结论完整，既有凭据原样。
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after failed executions: %v", err)
	}
	defer reopened.Close()

	snap, err := reopened.BalanceSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snap.Treasury != 750 || !reflect.DeepEqual(snap.Balances, before.Balances) {
		t.Fatalf("balances on reopen: %+v", snap)
	}
	for _, tc := range cases {
		assertVoteConclusionPreserved(t, reopened, tc.id, tc.actions)
		if got, ok, err := reopened.Receipt(tc.id); err != nil || ok || got != nil {
			t.Fatalf("receipt for %s on reopen: %+v ok=%v err=%v", tc.id, got, ok, err)
		}
	}
	rs, err := reopened.Receipts()
	if err != nil || len(rs) != 1 || rs[0].ProposalID != "gip-prior" || rs[0].Order != 0 {
		t.Fatalf("receipts on reopen changed: %+v err=%v", rs, err)
	}
}

// TestPassedVoteProposalSuccessControlExecutesTransfers：成功对照——
// 经过真实委托归集、窗口内投票与首次计票通过的提案，在时间锁到期、动作
// 合法且余额足够时正常逐笔转账，状态转为 executed 并产生成功凭据；
// 重复执行返回首次凭据，不二次扣款；重开后结论与资金变动保持。
func TestPassedVoteProposalSuccessControlExecutesTransfers(t *testing.T) {
	store, path := openTempStore(t, 1000)
	defer store.Close()

	// 同一账户连续收款 + 新账户收款，金额合计 250，资金库 1000 完全足够。
	actions := []string{"transfer:audits:100", "transfer:audits:150", "transfer:legal:50"}
	passedVoteFixture(t, store, "gip-ok", actions)

	r, err := store.Execute("gip-ok", 300)
	if err != nil {
		t.Fatalf("execute legal, funded passed proposal: %v", err)
	}
	if r.Order != 0 || r.ExecutedAt != 300 || len(r.Actions) != 3 {
		t.Fatalf("unexpected receipt: %+v", r)
	}
	want := []struct {
		action         string
		tb, ta, rb, ra int64
		account        string
	}{
		{"transfer:audits:100", 1000, 900, 0, 100, "audits"},
		{"transfer:audits:150", 900, 750, 100, 250, "audits"}, // 承接前一笔
		{"transfer:legal:50", 750, 700, 0, 50, "legal"},
	}
	for i, w := range want {
		ar := r.Actions[i]
		if ar.Index != int64(i) || ar.Action != w.action ||
			ar.Treasury.Before != w.tb || ar.Treasury.After != w.ta ||
			ar.Recipient.Account != w.account ||
			ar.Recipient.Before != w.rb || ar.Recipient.After != w.ra {
			t.Fatalf("receipt action %d = %+v, want %s treasury %d->%d %s %d->%d",
				i, ar, w.action, w.tb, w.ta, w.account, w.rb, w.ra)
		}
	}
	if bal, _ := store.TreasuryBalance(); bal != 700 {
		t.Fatalf("treasury=%d, want 700", bal)
	}
	if bal, _ := store.Balance("audits"); bal != 250 {
		t.Fatalf("audits=%d, want 250", bal)
	}
	if bal, _ := store.Balance("legal"); bal != 50 {
		t.Fatalf("legal=%d, want 50", bal)
	}
	v, _, _ := store.VoteProposal("gip-ok")
	if v.State != "executed" || v.Tally == nil || !v.Tally.Passed {
		t.Fatalf("state=%s tally=%+v, want executed with passed first tally", v.State, v.Tally)
	}

	// 重复执行即使传入不同时间，也返回首次凭据且不产生二次资金变动。
	again, err := store.Execute("gip-ok", 99999)
	if err != nil {
		t.Fatalf("re-execute: %v", err)
	}
	if again.Order != r.Order || again.ExecutedAt != r.ExecutedAt ||
		!reflect.DeepEqual(again.Actions, r.Actions) {
		t.Fatalf("re-execute did not return first receipt:\nfirst=%+v\nagain=%+v", r, again)
	}
	if bal, _ := store.TreasuryBalance(); bal != 700 {
		t.Fatalf("treasury changed on retry: %d", bal)
	}

	// 重开后已执行状态、首次计票结论与凭据保持。
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	v2, ok, err := reopened.VoteProposal("gip-ok")
	if err != nil || !ok || v2.State != "executed" || v2.Tally == nil || !v2.Tally.Passed {
		t.Fatalf("reopened proposal: %+v ok=%v err=%v", v2, ok, err)
	}
	r2, ok, err := reopened.Receipt("gip-ok")
	if err != nil || !ok || r2.ExecutedAt != 300 || r2.Order != 0 {
		t.Fatalf("reopened receipt: %+v ok=%v err=%v", r2, ok, err)
	}
	if bal, _ := reopened.TreasuryBalance(); bal != 700 {
		t.Fatalf("treasury on reopen=%d, want 700", bal)
	}
}
