package govflow

// 本文件保护“转账动作原文保留”这一治理记录不变量：
// transfer:audits:007 与 transfer:audits:7 的数值相同（都向 audits 转 7），
// 但动作内容不同。金额允许带前导零；提案保存、执行与凭据读取的全过程都以
// 用户提交的原文为准，只在扣款/加款时使用解析出的数值。整理金额格式（如
// 统一去掉前导零）绝不能悄悄改写已登记提案、首次执行凭据或余额对应关系。

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// leadingZeroVoteInput 构造一项动作带前导零的投票提案：同账户连续两笔收款
// （007 与 008），用于验证投票通过路径同样逐字保留原文。
func leadingZeroVoteInput(id string) *CreateVoteInput {
	in := baseVoteInput(id)
	// 总权重 1000；alice 归集 bob、carol 后权重 600，达到法定人数 600。
	in.Actions = []string{"transfer:audits:007", "transfer:audits:008"}
	return in
}

func passLeadingZeroVote(t *testing.T, store *Store, id string, executeAt int64) *Receipt {
	t.Helper()
	if _, err := store.CastVote(id, "alice", true, 150); err != nil {
		t.Fatalf("CastVote: %v", err)
	}
	tally, err := store.TallyVote(id, 200)
	if err != nil || !tally.Passed {
		t.Fatalf("TallyVote passed=%v err=%v, want passed", tally, err)
	}
	rcpt, err := store.Execute(id, int64(executeAt))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return rcpt
}

// assertActionTexts 逐字断言某份凭据的动作原文序列。
func assertActionTexts(t *testing.T, rcpt *Receipt, want []string) {
	t.Helper()
	if rcpt == nil || len(rcpt.Actions) != len(want) {
		t.Fatalf("receipt=%+v, want %d actions %v", rcpt, len(want), want)
	}
	for i, w := range want {
		if got := rcpt.Actions[i].Action; got != w {
			t.Fatalf("receipt action %d text=%q, want original text %q", i, got, w)
		}
		if rcpt.Actions[i].Index != int64(i) {
			t.Fatalf("receipt action %d index=%d, want %d", i, rcpt.Actions[i].Index, i)
		}
	}
}

// TestLeadingZeroRawTextPreservedRegisteredAndVoted：直接登记与投票通过两条
// 路径都必须保留带前导零的动作原文；查询提案看到最初写法，执行只按数值
// 扣款/加款，凭据动作文字保持原样；同一账户连续收款分别留痕、后项承接前项
// 余额；关闭重开后提案动作、首次凭据与余额的对应关系仍然成立。
func TestLeadingZeroRawTextPreservedRegisteredAndVoted(t *testing.T) {
	// ---- 直接登记的已通过提案 ----
	reg, regPath := openTempStore(t, 100)
	regActions := []string{"transfer:audits:007", "transfer:audits:008"}
	mustRegister(t, reg, "gip-zero-r", 0, regActions...)

	if got, ok, err := reg.Proposal("gip-zero-r"); err != nil || !ok ||
		!reflect.DeepEqual(got.Actions, regActions) || got.State != "passed" {
		t.Fatalf("registered proposal=%+v ok=%v err=%v, want actions %v verbatim", got, ok, err, regActions)
	}
	rr, err := reg.Execute("gip-zero-r", 0)
	if err != nil {
		t.Fatalf("execute registered: %v", err)
	}
	assertActionTexts(t, rr, regActions)
	// 只按实际数值扣款 15，凭据余额逐笔承接：
	// 资金库 100->93->85；audits 0->7->15（两项动作分别留痕，不合并）。
	wantSteps := []struct{ tb, ta, rb, ra int64 }{
		{100, 93, 0, 7},
		{93, 85, 7, 15},
	}
	for i, w := range wantSteps {
		ar := rr.Actions[i]
		if ar.Treasury.Before != w.tb || ar.Treasury.After != w.ta ||
			ar.Recipient.Before != w.rb || ar.Recipient.After != w.ra {
			t.Fatalf("registered action %d balances=%+v, want treasury %d->%d recipient %d->%d",
				i, ar, w.tb, w.ta, w.rb, w.ra)
		}
	}
	if bal, _ := reg.TreasuryBalance(); bal != 85 {
		t.Fatalf("treasury=%d, want 85", bal)
	}
	if bal, _ := reg.Balance("audits"); bal != 15 {
		t.Fatalf("audits=%d, want 15", bal)
	}
	// 执行后查询，提案动作仍是最初的前导零写法。
	if got, _, _ := reg.Proposal("gip-zero-r"); got.State != "executed" ||
		!reflect.DeepEqual(got.Actions, regActions) {
		t.Fatalf("proposal after execute=%+v, want executed with original actions %v", got, regActions)
	}

	// ---- 经投票通过后执行的提案 ----
	vote, votePath := openTempStore(t, 100)
	in := leadingZeroVoteInput("gip-zero-v")
	view := mustCreateVote(t, vote, in)
	if !reflect.DeepEqual(view.Actions, in.Actions) {
		t.Fatalf("vote view actions=%v, want %v", view.Actions, in.Actions)
	}
	vr := passLeadingZeroVote(t, vote, "gip-zero-v", 300)
	assertActionTexts(t, vr, in.Actions)
	for i, w := range wantSteps {
		ar := vr.Actions[i]
		if ar.Treasury.Before != w.tb || ar.Treasury.After != w.ta ||
			ar.Recipient.Before != w.rb || ar.Recipient.After != w.ra {
			t.Fatalf("voted action %d balances=%+v, want treasury %d->%d recipient %d->%d",
				i, ar, w.tb, w.ta, w.rb, w.ra)
		}
	}
	if bal, _ := vote.TreasuryBalance(); bal != 85 {
		t.Fatalf("vote treasury=%d, want 85", bal)
	}
	if bal, _ := vote.Balance("audits"); bal != 15 {
		t.Fatalf("vote audits=%d, want 15", bal)
	}
	vv, ok, err := vote.VoteProposal("gip-zero-v")
	if err != nil || !ok || vv.State != "executed" || !reflect.DeepEqual(vv.Actions, in.Actions) {
		t.Fatalf("vote proposal after execute=%+v ok=%v err=%v, want original actions %v", vv, ok, err, in.Actions)
	}

	// ---- 关闭并重新打开：原文、首次凭据与余额的对应关系仍然成立 ----
	for _, pair := range []struct {
		name    string
		store   *Store
		path    string
		id      string
		actions []string
		first   *Receipt
		source  string
	}{
		{"registered", reg, regPath, "gip-zero-r", regActions, rr, "register"},
		{"voted", vote, votePath, "gip-zero-v", in.Actions, vr, "vote"},
	} {
		if err := pair.store.Close(); err != nil {
			t.Fatalf("%s close: %v", pair.name, err)
		}
		reopened, err := Open(pair.path)
		if err != nil {
			t.Fatalf("%s reopen: %v", pair.name, err)
		}
		defer reopened.Close()

		var gotActions []string
		if pair.source == "register" {
			p, ok, err := reopened.Proposal(pair.id)
			if err != nil || !ok || p.State != "executed" {
				t.Fatalf("%s proposal after reopen ok=%v err=%v", pair.name, ok, err)
			}
			gotActions = p.Actions
		} else {
			v, ok, err := reopened.VoteProposal(pair.id)
			if err != nil || !ok || v.State != "executed" {
				t.Fatalf("%s vote proposal after reopen ok=%v err=%v", pair.name, ok, err)
			}
			gotActions = v.Actions
		}
		if !reflect.DeepEqual(gotActions, pair.actions) {
			t.Fatalf("%s actions after reopen=%v, want original %v", pair.name, gotActions, pair.actions)
		}
		got, ok, err := reopened.Receipt(pair.id)
		if err != nil || !ok {
			t.Fatalf("%s receipt after reopen ok=%v err=%v", pair.name, ok, err)
		}
		if !reflect.DeepEqual(got, pair.first) {
			t.Fatalf("%s first receipt changed after reopen:\nwant=%+v\ngot =%+v", pair.name, pair.first, got)
		}
		if bal, _ := reopened.TreasuryBalance(); bal != 85 {
			t.Fatalf("%s treasury after reopen=%d, want 85", pair.name, bal)
		}
		if bal, _ := reopened.Balance("audits"); bal != 15 {
			t.Fatalf("%s audits after reopen=%d, want 15", pair.name, bal)
		}
		// 落盘字节里也必须保留 007/008 原文，而不是被规范化成 7/8。
		raw, err := os.ReadFile(pair.path)
		if err != nil {
			t.Fatalf("%s read file: %v", pair.name, err)
		}
		for _, text := range pair.actions {
			if !strings.Contains(string(raw), text) {
				t.Fatalf("%s state file lost verbatim action %q", pair.name, text)
			}
		}
		if strings.Contains(string(raw), `"transfer:audits:7"`) ||
			strings.Contains(string(raw), `"transfer:audits:8"`) {
			t.Fatalf("%s state file normalized leading-zero amounts away", pair.name)
		}
	}
}

// TestLeadingZeroRetryConflictBeforeAndAfterExecution：同编号提交、其他条件
// 不变时，只有动作原文及次序完全一致才算相同内容重试。把 007 改成 7 必须
// 报 ErrProposalConflict，已有动作、提案状态与资金记录保持原样——执行之前
// 与已经执行之后都成立。原文一致的重试返回已有记录：不把已执行提案改回
// passed，不增加凭据，不产生第二次扣款。
func TestLeadingZeroRetryConflictBeforeAndAfterExecution(t *testing.T) {
	original := []string{"transfer:audits:007", "transfer:audits:70"}
	normalized := []string{"transfer:audits:7", "transfer:audits:70"}

	// ---- 直接登记路径：执行前冲突 ----
	store, path := openTempStore(t, 100)
	if existed, err := store.Register("gip-c", 10, original); err != nil || existed {
		t.Fatalf("first register existed=%v err=%v", existed, err)
	}
	if _, err := store.Register("gip-c", 10, normalized); !errors.Is(err, ErrProposalConflict) {
		t.Fatalf("007->7 before execution err=%v, want ErrProposalConflict", err)
	}
	// 冲突后原样保留：动作仍是 007，状态仍为 passed，资金未动，无凭据。
	if p, _, _ := store.Proposal("gip-c"); p.State != "passed" ||
		!reflect.DeepEqual(p.Actions, original) {
		t.Fatalf("proposal after conflict=%+v, want passed with %v", p, original)
	}
	if bal, _ := store.TreasuryBalance(); bal != 100 {
		t.Fatalf("treasury changed after conflict: %d", bal)
	}
	if rc, ok, _ := store.Receipt("gip-c"); ok || rc != nil {
		t.Fatalf("receipt appeared after conflict: %+v", rc)
	}
	// 原文一致的重试返回已有记录（passed），不落盘。
	if rec, existed, err := store.RegisterProposal("gip-c", 10, original); err != nil || !existed ||
		rec.State != "passed" || !reflect.DeepEqual(rec.Actions, original) {
		t.Fatalf("identical retry rec=%+v existed=%v err=%v", rec, existed, err)
	}

	// 时间锁到期执行：只按数值扣 77，凭据保留 007 原文。
	first, err := store.Execute("gip-c", 10)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	assertActionTexts(t, first, original)
	if bal, _ := store.TreasuryBalance(); bal != 23 {
		t.Fatalf("treasury=%d, want 23", bal)
	}
	if bal, _ := store.Balance("audits"); bal != 77 {
		t.Fatalf("audits=%d, want 77", bal)
	}

	// ---- 执行之后：007->7 仍是内容冲突 ----
	if _, err := store.Register("gip-c", 10, normalized); !errors.Is(err, ErrProposalConflict) {
		t.Fatalf("007->7 after execution err=%v, want ErrProposalConflict", err)
	}
	if p, _, _ := store.Proposal("gip-c"); p.State != "executed" ||
		!reflect.DeepEqual(p.Actions, original) {
		t.Fatalf("executed proposal after conflict=%+v, want executed with %v", p, original)
	}
	// 原文一致重试：返回 executed 的已有记录，不改回 passed、不加凭据、不二次扣款。
	rec, existed, err := store.RegisterProposal("gip-c", 10, original)
	if err != nil || !existed || rec.State != "executed" {
		t.Fatalf("identical retry after execution rec=%+v existed=%v err=%v", rec, existed, err)
	}
	all, err := store.Receipts()
	if err != nil || len(all) != 1 {
		t.Fatalf("receipts=%v err=%v, want exactly 1", all, err)
	}
	if !reflect.DeepEqual(all[0], first) {
		t.Fatalf("receipt changed:\nwant=%+v\ngot =%+v", first, all[0])
	}
	if bal, _ := store.TreasuryBalance(); bal != 23 {
		t.Fatalf("treasury changed after identical retry: %d", bal)
	}
	// 再次执行同样只返回首次凭据，不产生第二次扣款。
	again, err := store.Execute("gip-c", 999)
	if err != nil || !reflect.DeepEqual(again, first) {
		t.Fatalf("re-execute rcpt=%+v err=%v, want first receipt", again, err)
	}
	if bal, _ := store.TreasuryBalance(); bal != 23 {
		t.Fatalf("treasury changed on re-execute: %d", bal)
	}

	// 重开后冲突结论与冻结记录不变。
	store.Close()
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if _, err := reopened.Register("gip-c", 10, normalized); !errors.Is(err, ErrProposalConflict) {
		t.Fatalf("007->7 after reopen err=%v, want ErrProposalConflict", err)
	}
	if p, _, _ := reopened.Proposal("gip-c"); p.State != "executed" ||
		!reflect.DeepEqual(p.Actions, original) {
		t.Fatalf("proposal after reopen conflict=%+v, want executed with %v", p, original)
	}
	if rc, ok, _ := reopened.Receipt("gip-c"); !ok || !reflect.DeepEqual(rc, first) {
		t.Fatalf("receipt after reopen=%+v ok=%v, want first receipt", rc, ok)
	}
	if n, _ := reopened.Receipts(); len(n) != 1 {
		t.Fatalf("receipt count after reopen=%d, want 1", len(n))
	}
	if bal, _ := reopened.TreasuryBalance(); bal != 23 {
		t.Fatalf("treasury after reopen=%d, want 23", bal)
	}
}

// TestLeadingZeroVoteRetryConflictAfterExecution：投票通过路径在执行之后，
// 同编号仅把动作 007 改成 7（成员、委托、时间锁与其余内容不变）也必须报
// 内容冲突；原投票提案动作、状态、票据与资金记录保持原样。
func TestLeadingZeroVoteRetryConflictAfterExecution(t *testing.T) {
	store, path := openTempStore(t, 100)
	in := leadingZeroVoteInput("gip-vc")
	mustCreateVote(t, store, in)
	first := passLeadingZeroVote(t, store, "gip-vc", 300)
	if bal, _ := store.TreasuryBalance(); bal != 85 {
		t.Fatalf("treasury=%d, want 85", bal)
	}

	changed := *in
	changed.Actions = []string{"transfer:audits:7", "transfer:audits:008"}
	if v, existed, err := store.CreateVoteProposal(&changed); !errors.Is(err, ErrProposalConflict) ||
		existed || v != nil {
		t.Fatalf("vote 007->7 conflict: view=%+v existed=%v err=%v", v, existed, err)
	}
	// 原文一致的重试返回已存在的 executed 视图，不新增票据/凭据，不二次扣款。
	same, existed, err := store.CreateVoteProposal(in)
	if err != nil || !existed || same.State != "executed" ||
		!reflect.DeepEqual(same.Actions, in.Actions) {
		t.Fatalf("vote identical retry view=%+v existed=%v err=%v", same, existed, err)
	}
	v, _, _ := store.VoteProposal("gip-vc")
	if len(v.Ballots) != 1 {
		t.Fatalf("ballots=%v, want still 1", v.Ballots)
	}
	if n, _ := store.Receipts(); len(n) != 1 || !reflect.DeepEqual(n[0], first) {
		t.Fatalf("receipts=%v, want the single first receipt", n)
	}
	if bal, _ := store.TreasuryBalance(); bal != 85 {
		t.Fatalf("treasury changed after vote retries: %d", bal)
	}

	store.Close()
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if v, _, err := reopened.CreateVoteProposal(&changed); !errors.Is(err, ErrProposalConflict) || v != nil {
		t.Fatalf("vote 007->7 conflict after reopen: %+v %v", v, err)
	}
	if rv, ok, _ := reopened.Receipt("gip-vc"); !ok || !reflect.DeepEqual(rv, first) {
		t.Fatalf("vote receipt after reopen=%+v ok=%v, want first receipt", rv, ok)
	}
}

// mutateReceiptActionText 只改写第 ri 张凭据第 ai 项动作的 action 文本，
// 其余内容（所属提案原文、全部余额、序号、时间）逐字保留。
func mutateReceiptActionText(t *testing.T, good []byte, ri, ai int, text string) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(good, &doc); err != nil {
		t.Fatal(err)
	}
	entry := doc["receipts"].([]any)[ri].(map[string]any)["actions"].([]any)[ai].(map[string]any)
	entry["action"] = text
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(raw, '\n')
}

// TestLeadingZeroReceiptTextTamperIsStateCorruption：一份原本合法的状态记录，
// 只把凭据中某项动作的金额从 007 改成 7（数值相同、重放的资金变动完全一致），
// 仍必须判为状态损坏，原因能定位到所属提案及动作；不能因重算资金变动相同
// 就接受，也不能自动统一写法后覆盖原文件。
func TestLeadingZeroReceiptTextTamperIsStateCorruption(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "treasury.json")
	store, err := InitTreasury(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	mustRegister(t, store, "gip-tamper", 0, "transfer:audits:007", "transfer:audits:008")
	if _, err := store.Execute("gip-tamper", 0); err != nil {
		t.Fatal(err)
	}
	store.Close()

	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// 第 0 张凭据、第 1 项动作 008->8；提案里的 008 原文与所有余额保持不动。
	tampered := mutateReceiptActionText(t, good, 0, 1, "transfer:audits:8")
	bad := filepath.Join(dir, "tampered.json")
	if err := os.WriteFile(bad, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(bad)
	if !errors.Is(err, ErrStateCorrupt) {
		if s != nil {
			s.Close()
		}
		t.Fatalf("Open tampered err=%v, want ErrStateCorrupt", err)
	}
	// 原因定位到所属提案与动作下标，且是“凭据原文与登记原文不一致”。
	for _, want := range []string{`"gip-tamper"`, "action 1", "text does not match registered action"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not locate %q", err.Error(), want)
		}
	}
	// 不自动统一写法、不覆盖原文件。
	if got, rerr := os.ReadFile(bad); rerr != nil || string(got) != string(tampered) {
		t.Fatalf("corrupt file was modified")
	}
	// 篡改第 0 项动作 007->7 适用同一判定。
	tampered0 := mutateReceiptActionText(t, good, 0, 0, "transfer:audits:7")
	bad0 := filepath.Join(dir, "tampered0.json")
	if err := os.WriteFile(bad0, tampered0, 0o600); err != nil {
		t.Fatal(err)
	}
	s0, err := Open(bad0)
	if !errors.Is(err, ErrStateCorrupt) {
		if s0 != nil {
			s0.Close()
		}
		t.Fatalf("Open tampered(007->7) err=%v, want ErrStateCorrupt", err)
	}
	if !strings.Contains(err.Error(), "action 0") || !strings.Contains(err.Error(), `"gip-tamper"`) {
		t.Fatalf("error %q must locate proposal and action 0", err.Error())
	}

	// 对照：未篡改的原文件（合法前导零）正常打开，凭据原文仍是 007/008。
	clean, err := Open(path)
	if err != nil {
		t.Fatalf("legit leading-zero file rejected: %v", err)
	}
	defer clean.Close()
	rc, ok, err := clean.Receipt("gip-tamper")
	if err != nil || !ok {
		t.Fatalf("receipt ok=%v err=%v", ok, err)
	}
	assertActionTexts(t, rc, []string{"transfer:audits:007", "transfer:audits:008"})
}

// TestAllZeroAmountsLeadingZerosRejectedAtExecution：全部为零的金额（含 000
// 这类前导零写法）仍按现有规则在首次执行时拒绝：匹配 ErrExecutionRejected，
// 不产生部分转账，不留成功凭据，提案保持 passed；前一项已合法的连续动作也
// 不能部分生效。合法的前导零金额不被误拒绝。
func TestAllZeroAmountsLeadingZerosRejectedAtExecution(t *testing.T) {
	store, _ := openTempStore(t, 100)
	mustRegister(t, store, "gip-zero-amt", 0, "transfer:audits:000")
	if _, err := store.Execute("gip-zero-amt", 0); !errors.Is(err, ErrExecutionRejected) {
		t.Fatalf("execute 000 err=%v, want ErrExecutionRejected", err)
	}
	if bal, _ := store.TreasuryBalance(); bal != 100 {
		t.Fatalf("treasury changed after zero-amount rejection: %d", bal)
	}
	if bal, _ := store.Balance("audits"); bal != 0 {
		t.Fatalf("audits changed after zero-amount rejection: %d", bal)
	}
	if rc, ok, _ := store.Receipt("gip-zero-amt"); ok || rc != nil {
		t.Fatalf("receipt left after zero-amount rejection: %+v", rc)
	}
	if p, _, _ := store.Proposal("gip-zero-amt"); p.State != "passed" {
		t.Fatalf("proposal state=%s, want passed", p.State)
	}

	// 第一项合法、第二项为零：整项拒绝，第一项也不得部分生效。
	mustRegister(t, store, "gip-zero-mid", 0, "transfer:audits:007", "transfer:audits:000")
	_, err := store.Execute("gip-zero-mid", 0)
	if !errors.Is(err, ErrExecutionRejected) || !errors.Is(err, ErrInvalidAction) {
		t.Fatalf("execute trailing 000 err=%v, want rejection matching both sentinels", err)
	}
	if !strings.Contains(err.Error(), `action 1`) {
		t.Fatalf("error %q must locate action 1", err.Error())
	}
	if bal, _ := store.TreasuryBalance(); bal != 100 {
		t.Fatalf("partial transfer after rejection: treasury=%d, want 100", bal)
	}
	if bal, _ := store.Balance("audits"); bal != 0 {
		t.Fatalf("partial transfer after rejection: audits=%d, want 0", bal)
	}
	if p, _, _ := store.Proposal("gip-zero-mid"); p.State != "passed" {
		t.Fatalf("proposal state=%s, want passed", p.State)
	}
	if rc, ok, _ := store.Receipt("gip-zero-mid"); ok || rc != nil {
		t.Fatalf("receipt left after partial rejection: %+v", rc)
	}

	// 对照：合法前导零金额正常执行，不被全零规则误伤。
	mustRegister(t, store, "gip-ok-lz", 0, "transfer:audits:007")
	rc, err := store.Execute("gip-ok-lz", 0)
	if err != nil {
		t.Fatalf("legal 007 rejected: %v", err)
	}
	assertActionTexts(t, rc, []string{"transfer:audits:007"})
	if bal, _ := store.TreasuryBalance(); bal != 93 {
		t.Fatalf("treasury=%d, want 93", bal)
	}
	if bal, _ := store.Balance("audits"); bal != 7 {
		t.Fatalf("audits=%d, want 7", bal)
	}
}
