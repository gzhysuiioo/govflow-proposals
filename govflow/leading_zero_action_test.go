package govflow

import (
	"errors"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// 本文件回归“金额允许带前导零，但数值相同不等于动作内容相同”这一不变量，
// 贯穿提案保存、首次执行、凭据读取（重开校验）与同编号重试四条路径：
//
//   - transfer:audits:007 与 transfer:audits:7 表示同一笔 7 的数值扣款，
//     但提案内容与执行留痕始终以用户提交的原文为准，任何路径都不得悄悄
//         把 007 整理成 7；
//   - 同一账户连续收款逐项留痕，后项承接前项余额，不因对象相同或金额写法
//     相近而合并；
//   - 同编号重试只有“动作原文及次序完全一致”才算相同内容，007 改成 7 在
//     执行之前与之后都报内容冲突，已有动作、状态与资金记录保持原样；
//   - 读取执行凭据时逐字核对原文：只把凭据里的 007 改成 7、余额重放仍一致
//     也必须判状态损坏，且不自动统一写法覆盖原文件；
//   - 全零金额（000、00）沿用既有规则在首次执行时拒绝，不产生部分转账或
//     成功凭据；合法前导零不被误拒绝。

// assertReceiptAction 断言一条动作留痕的原文、序号与双方前后余额逐字/逐值一致。
func assertReceiptAction(t *testing.T, ar ActionReceipt, index int, raw string,
	treasuryBefore, treasuryAfter, recipientBefore, recipientAfter int64, account string) {
	t.Helper()
	if ar.Index != int64(index) || ar.Action != raw {
		t.Fatalf("action %d receipt = (index=%d text=%q), want index=%d text=%q",
			index, ar.Index, ar.Action, index, raw)
	}
	if ar.Treasury.Account != "treasury" ||
		ar.Treasury.Before != treasuryBefore || ar.Treasury.After != treasuryAfter {
		t.Fatalf("action %d (%q) treasury %+v, want treasury %d->%d",
			index, raw, ar.Treasury, treasuryBefore, treasuryAfter)
	}
	if ar.Recipient.Account != account ||
		ar.Recipient.Before != recipientBefore || ar.Recipient.After != recipientAfter {
		t.Fatalf("action %d (%q) recipient %+v, want %s %d->%d",
			index, raw, ar.Recipient, account, recipientBefore, recipientAfter)
	}
}

// TestLeadingZeroAmountRegisterSourcePreserved：直接登记的已通过提案，前导零
// 金额写法在保存、查询、执行与重开后逐字保留；扣款只按数值 7，同一账户
// 连续收款逐项留痕、余额逐项承接，不被合并。
func TestLeadingZeroAmountRegisterSourcePreserved(t *testing.T) {
	store, path := openTempStore(t, 1000)
	defer store.Close()

	const id = "gip-lz-register"
	actions := []string{"transfer:audits:007", "transfer:audits:007", "transfer:audits:7"}
	mustRegister(t, store, id, 100, actions...)

	// 查询提案时仍能看到最初的金额写法（含前导零与原顺序），而不是规范化后的 7。
	rec, ok, err := store.Proposal(id)
	if err != nil || !ok {
		t.Fatalf("Proposal ok=%v err=%v", ok, err)
	}
	if rec.State != "passed" || !reflect.DeepEqual(rec.Actions, actions) {
		t.Fatalf("stored proposal=%+v, want passed with verbatim actions %v", rec, actions)
	}

	// 达到时间锁后执行：只按实际数值扣款，三项动作各转 7，共 21。
	receipt, err := store.Execute(id, 100)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if receipt.Order != 0 || receipt.ExecutedAt != 100 || receipt.ProposalID != id ||
		len(receipt.Actions) != 3 {
		t.Fatalf("receipt=%+v, want order=0 executed_at=100 three actions", receipt)
	}
	// 动作文字保持原样；同账户连续收款分别留痕，后项起始余额承接前项结果。
	assertReceiptAction(t, receipt.Actions[0], 0, "transfer:audits:007", 1000, 993, 0, 7, "audits")
	assertReceiptAction(t, receipt.Actions[1], 1, "transfer:audits:007", 993, 986, 7, 14, "audits")
	assertReceiptAction(t, receipt.Actions[2], 2, "transfer:audits:7", 986, 979, 14, 21, "audits")
	if bal, _ := store.TreasuryBalance(); bal != 979 {
		t.Fatalf("treasury=%d, want 979", bal)
	}
	if bal, _ := store.Balance("audits"); bal != 21 {
		t.Fatalf("audits balance=%d, want 21", bal)
	}
	if exec, _, _ := store.Proposal(id); exec.State != "executed" ||
		!reflect.DeepEqual(exec.Actions, actions) {
		t.Fatalf("proposal after execute=%+v, want executed with verbatim original actions", exec)
	}

	// 磁盘上必须逐字保留前导零，而不是只在内存视图里好看。
	if raw, rerr := os.ReadFile(path); rerr != nil {
		t.Fatal(rerr)
	} else if strings.Count(string(raw), "transfer:audits:007") < 2 {
		t.Fatalf("on-disk state lost leading-zero action text:\n%s", raw)
	}

	// 正常关闭并重新打开：提案动作原文、首次执行凭据与余额的对应关系仍然成立。
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen leading-zero state: %v", err)
	}
	defer reopened.Close()
	reRec, ok, err := reopened.Proposal(id)
	if err != nil || !ok || !reflect.DeepEqual(reRec.Actions, actions) || reRec.State != "executed" {
		t.Fatalf("reopen proposal=%+v ok=%v err=%v", reRec, ok, err)
	}
	reReceipt, ok, err := reopened.Receipt(id)
	if err != nil || !ok || !reflect.DeepEqual(reReceipt, receipt) {
		t.Fatalf("reopen receipt=%+v ok=%v err=%v, want original receipt", reReceipt, ok, err)
	}
	if snap, err := reopened.BalanceSnapshot(); err != nil ||
		snap.Treasury != 979 || snap.Balances["audits"] != 21 || len(snap.Balances) != 1 {
		t.Fatalf("reopen snapshot=%+v err=%v, want treasury=979 audits=21 only", snap, err)
	}
}

// TestLeadingZeroAmountVoteSourcePreserved：经投票通过后执行的提案同样保留
// 前导零动作原文；投票、计票、执行全程不规范化金额写法。
func TestLeadingZeroAmountVoteSourcePreserved(t *testing.T) {
	store, path := openTempStore(t, 1000)
	defer store.Close()

	const id = "gip-lz-vote"
	in := baseVoteInput(id)
	in.Actions = []string{"transfer:audits:007", "transfer:audits:007"}
	mustCreateVote(t, store, in)

	if v, ok, err := store.VoteProposal(id); err != nil || !ok ||
		!reflect.DeepEqual(v.Actions, in.Actions) {
		t.Fatalf("vote proposal actions=%v ok=%v err=%v, want verbatim %v", v.Actions, ok, err, in.Actions)
	}
	if _, err := store.CastVote(id, "alice", true, 150); err != nil {
		t.Fatalf("alice vote: %v", err)
	}
	if _, err := store.CastVote(id, "dave", false, 150); err != nil {
		t.Fatalf("dave vote: %v", err)
	}
	tally, err := store.TallyVote(id, 200)
	if err != nil || !tally.Passed {
		t.Fatalf("tally=%+v err=%v, want passed", tally, err)
	}
	receipt, err := store.Execute(id, 300)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(receipt.Actions) != 2 {
		t.Fatalf("receipt=%+v, want two separately traced actions", receipt)
	}
	assertReceiptAction(t, receipt.Actions[0], 0, "transfer:audits:007", 1000, 993, 0, 7, "audits")
	assertReceiptAction(t, receipt.Actions[1], 1, "transfer:audits:007", 993, 986, 7, 14, "audits")

	// 通过投票路径查询，提案动作原文仍是带前导零的写法，状态 executed。
	v, ok, err := store.VoteProposal(id)
	if err != nil || !ok || v.State != "executed" ||
		!reflect.DeepEqual(v.Actions, []string{"transfer:audits:007", "transfer:audits:007"}) {
		t.Fatalf("vote view after execute=%+v ok=%v err=%v", v, ok, err)
	}

	// 重开后投票提案、凭据原文与余额仍然对应。
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	rv, ok, err := reopened.VoteProposal(id)
	if err != nil || !ok || rv.State != "executed" ||
		!reflect.DeepEqual(rv.Actions, in.Actions) {
		t.Fatalf("reopen vote proposal=%+v ok=%v err=%v", rv, ok, err)
	}
	rr, ok, err := reopened.Receipt(id)
	if err != nil || !ok || !reflect.DeepEqual(rr, receipt) {
		t.Fatalf("reopen receipt=%+v ok=%v err=%v", rr, ok, err)
	}
	if bal, _ := reopened.TreasuryBalance(); bal != 986 {
		t.Fatalf("reopen treasury=%d, want 986", bal)
	}
	if bal, _ := reopened.Balance("audits"); bal != 14 {
		t.Fatalf("reopen audits=%d, want 14", bal)
	}
}

// TestRegisterRestyleConflictBeforeAndAfterExecution：直接登记来源，同编号、
// 时间锁相同，仅把动作金额 007 改成 7 即属内容冲突；该结论在执行之前与
// 已经执行之后都成立，冲突不改动已有动作、提案状态与资金记录。
func TestRegisterRestyleConflictBeforeAndAfterExecution(t *testing.T) {
	store, path := openTempStore(t, 1000)
	defer store.Close()

	const id = "gip-lz-conflict"
	original := []string{"transfer:audits:007"}
	mustRegister(t, store, id, 100, original...)

	// 执行之前：仅金额写法不同（007→7）报内容冲突，已有记录保持 007/passed。
	if _, existed, err := store.RegisterProposal(id, 100, []string{"transfer:audits:7"}); !errors.Is(err, ErrProposalConflict) || existed {
		t.Fatalf("restyle retry before execution: existed=%v err=%v, want ErrProposalConflict", existed, err)
	}
	if rec, _, _ := store.Proposal(id); rec.State != "passed" ||
		!reflect.DeepEqual(rec.Actions, original) {
		t.Fatalf("conflict mutated proposal=%+v, want passed with %v", rec, original)
	}
	// 次序或写法组合不同同样冲突，不能因为数值集合相同就当成重试。
	if _, existed, err := store.RegisterProposal(id, 100,
		[]string{"transfer:audits:7", "transfer:audits:007"}); !errors.Is(err, ErrProposalConflict) || existed {
		t.Fatalf("extra-action restyle retry: existed=%v err=%v, want conflict", existed, err)
	}
	// 原文一致的重试在执行前返回已有记录。
	if rec, existed, err := store.RegisterProposal(id, 100, original); err != nil ||
		!existed || rec.State != "passed" || !reflect.DeepEqual(rec.Actions, original) {
		t.Fatalf("verbatim retry before execution: rec=%+v existed=%v err=%v", rec, existed, err)
	}

	// 首次执行产生唯一凭据与一次扣款。
	receipt, err := store.Execute(id, 100)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if receipt.Actions[0].Action != "transfer:audits:007" {
		t.Fatalf("receipt lost raw text: %q", receipt.Actions[0].Action)
	}

	// 执行之后：仅改写金额写法仍是冲突，状态不退回 passed，凭据不增加、不二次扣款。
	if _, existed, err := store.RegisterProposal(id, 100, []string{"transfer:audits:7"}); !errors.Is(err, ErrProposalConflict) || existed {
		t.Fatalf("restyle retry after execution: existed=%v err=%v, want conflict", existed, err)
	}
	if rec, _, _ := store.Proposal(id); rec.State != "executed" ||
		!reflect.DeepEqual(rec.Actions, original) {
		t.Fatalf("post-exec conflict mutated proposal=%+v", rec)
	}
	if rs, _ := store.Receipts(); len(rs) != 1 {
		t.Fatalf("receipt count=%d after restyle conflict, want 1", len(rs))
	}
	if bal, _ := store.TreasuryBalance(); bal != 993 {
		t.Fatalf("treasury=%d after conflict, want 993", bal)
	}

	// 原文一致的重试在执行后返回已有记录，状态如实反映为 executed（不退回 passed）。
	if rec, existed, err := store.RegisterProposal(id, 100, original); err != nil ||
		!existed || rec.State != "executed" || !reflect.DeepEqual(rec.Actions, original) {
		t.Fatalf("verbatim retry after execution: rec=%+v existed=%v err=%v", rec, existed, err)
	}
	// 再次执行仍返回首次凭据，不产生第二次扣款。
	if again, err := store.Execute(id, 9999); err != nil || !reflect.DeepEqual(again, receipt) {
		t.Fatalf("execute retry=%+v err=%v, want first receipt", again, err)
	}
	if bal, _ := store.TreasuryBalance(); bal != 993 {
		t.Fatalf("treasury changed on execute retry: %d", bal)
	}

	// 重开后冲突与原文重试结论不变。
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if _, existed, err := reopened.RegisterProposal(id, 100, []string{"transfer:audits:7"}); !errors.Is(err, ErrProposalConflict) || existed {
		t.Fatalf("restyle retry after reopen: existed=%v err=%v, want conflict", existed, err)
	}
	if rec, existed, err := reopened.RegisterProposal(id, 100, original); err != nil ||
		!existed || rec.State != "executed" {
		t.Fatalf("verbatim retry after reopen: rec=%+v existed=%v err=%v", rec, existed, err)
	}
}

// TestVoteRestyleConflictBeforeAndAfterExecution：投票来源，成员、委托、时间
// 锁与其余内容不变，仅动作金额 007→7 即报创建冲突；执行前后都成立，票据、
// 计票、状态与资金记录保持原样。
func TestVoteRestyleConflictBeforeAndAfterExecution(t *testing.T) {
	store, path := openTempStore(t, 1000)
	defer store.Close()

	const id = "gip-lz-vote-conflict"
	in := baseVoteInput(id)
	in.Actions = []string{"transfer:audits:007"}
	mustCreateVote(t, store, in)

	restyled := func() *CreateVoteInput {
		c := *in
		c.Actions = []string{"transfer:audits:7"}
		return &c
	}

	// 执行（计票）之前：仅金额写法不同报冲突，原提案保留 007、仍为 voting。
	if v, existed, err := store.CreateVoteProposal(restyled()); !errors.Is(err, ErrProposalConflict) ||
		existed || v != nil {
		t.Fatalf("restyle create before voting: view=%+v existed=%v err=%v", v, existed, err)
	}
	if v, _, _ := store.VoteProposal(id); v.State != "voting" ||
		!reflect.DeepEqual(v.Actions, in.Actions) {
		t.Fatalf("conflict mutated voting proposal=%+v, want voting with %v", v, in.Actions)
	}
	// 原文一致重试返回已有提案且不改状态。
	if v, existed, err := store.CreateVoteProposal(in); err != nil || !existed || v.State != "voting" {
		t.Fatalf("verbatim create retry: v=%+v existed=%v err=%v", v, existed, err)
	}

	// 投票、计票通过、执行。
	if _, err := store.CastVote(id, "alice", true, 150); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CastVote(id, "dave", false, 150); err != nil {
		t.Fatal(err)
	}
	if tally, err := store.TallyVote(id, 200); err != nil || !tally.Passed {
		t.Fatalf("tally=%+v err=%v", tally, err)
	}
	receipt, err := store.Execute(id, 300)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	// 执行之后：仅金额写法不同仍报冲突，票据、计票、状态与资金冻结。
	if v, existed, err := store.CreateVoteProposal(restyled()); !errors.Is(err, ErrProposalConflict) ||
		existed || v != nil {
		t.Fatalf("restyle create after execution: view=%+v existed=%v err=%v", v, existed, err)
	}
	cur, _, err := store.VoteProposal(id)
	if err != nil || cur.State != "executed" || len(cur.Ballots) != 2 ||
		cur.Tally == nil || !reflect.DeepEqual(cur.Actions, in.Actions) {
		t.Fatalf("post-exec conflict mutated proposal: %+v", cur)
	}
	if rs, _ := store.Receipts(); len(rs) != 1 || !reflect.DeepEqual(rs[0], receipt) {
		t.Fatalf("receipts=%v after conflict, want original single receipt", rs)
	}
	if bal, _ := store.TreasuryBalance(); bal != 993 {
		t.Fatalf("treasury=%d after conflict, want 993", bal)
	}
	// 原文一致重试仍返回已执行记录，不退回 passed。
	if v, existed, err := store.CreateVoteProposal(in); err != nil || !existed || v.State != "executed" {
		t.Fatalf("verbatim retry after execution: v=%+v existed=%v err=%v", v, existed, err)
	}

	// 重开后冲突结论与冻结记录不变。
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if _, existed, err := reopened.CreateVoteProposal(restyled()); !errors.Is(err, ErrProposalConflict) || existed {
		t.Fatalf("restyle create after reopen: existed=%v err=%v, want conflict", existed, err)
	}
	if v, existed, err := reopened.CreateVoteProposal(in); err != nil || !existed || v.State != "executed" {
		t.Fatalf("verbatim retry after reopen: v=%+v existed=%v err=%v", v, existed, err)
	}
}

// TestReceiptAmountRestyleIsCorruption：一份原本合法的已执行状态，只把凭据中
// 某项动作的金额从 007 改成 7（所属提案原文、全部余额、序号与时间保持不变，
// 重放的资金变动完全相同），仍必须判状态损坏并定位到所属提案与动作；不得因
// 数值一致而接受，也不得自动统一写法后覆盖原文件。
func TestReceiptAmountRestyleIsCorruption(t *testing.T) {
	t.Run("single action", func(t *testing.T) {
		dir := t.TempDir()
		path := dir + "/single.json"
		store, err := InitTreasury(path, 1000)
		if err != nil {
			t.Fatal(err)
		}
		mustRegister(t, store, "gip-rcpt", 0, "transfer:audits:007")
		if _, err := store.Execute("gip-rcpt", 0); err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		// 只改凭据（"action" 键）里的写法，不动提案动作数组（"actions" 键）。
		tampered, n := replaceReceiptAction(t, path, `"action": "transfer:audits:007"`,
			`"action": "transfer:audits:7"`)
		if n != 1 {
			t.Fatalf("replaced %d receipt actions, want 1", n)
		}
		assertRestyleCorruption(t, path, tampered, "gip-rcpt", 0)
	})

	t.Run("second of consecutive same-recipient actions", func(t *testing.T) {
		dir := t.TempDir()
		path := dir + "/multi.json"
		store, err := InitTreasury(path, 1000)
		if err != nil {
			t.Fatal(err)
		}
		mustRegister(t, store, "gip-rcpt-multi", 0,
			"transfer:audits:007", "transfer:audits:007")
		if _, err := store.Execute("gip-rcpt-multi", 0); err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		// 凭据中有两条 007 留痕：只改第二处（动作 index=1），第一处保持 007。
		tampered, n := replaceNthReceiptAction(t, path,
			`"action": "transfer:audits:007"`, `"action": "transfer:audits:7"`, 2)
		if n != 2 { // 替换发生在第 2 次出现处
			t.Fatalf("occurrence replaced=%d, want 2", n)
		}
		assertRestyleCorruption(t, path, tampered, "gip-rcpt-multi", 1)
	})
}

// replaceReceiptAction 精确替换凭据动作原文（old→new）一次，返回写入的字节
// 与替换次数。只匹配凭据的 "action" 键，不触碰提案 "actions" 数组原文。
func replaceReceiptAction(t *testing.T, path, old, nw string) ([]byte, int) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, old) {
		t.Fatalf("state file does not contain %q", old)
	}
	s2 := strings.Replace(s, old, nw, 1)
	out := []byte(s2)
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
	return out, 1
}

// replaceNthReceiptAction 替换第 n 次（1 起）出现的凭据动作原文，返回写入字节
// 与实际命中的出现序号 n。用于精确定位多动作凭据中被改写的那一项。
func replaceNthReceiptAction(t *testing.T, path, old, nw string, n int) ([]byte, int) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	idx := -1
	for c := 0; c < n; c++ {
		next := strings.Index(s[idx+1:], old)
		if next < 0 {
			t.Fatalf("occurrence %d of %q not found", n, old)
		}
		idx = idx + 1 + next
	}
	s2 := s[:idx] + nw + s[idx+len(old):]
	out := []byte(s2)
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
	return out, n
}

// assertRestyleCorruption 断言仅改写凭据金额写法的文件被判损坏：错误归类为
// ErrStateCorrupt、定位到所属提案编号与动作下标，且原文件逐字节保留、不被
// 自动“修复”重写。
func assertRestyleCorruption(t *testing.T, path string, tampered []byte, id string, action int) {
	t.Helper()
	s, err := Open(path)
	if s != nil {
		s.Close()
	}
	if !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("Open err=%v, want ErrStateCorrupt", err)
	}
	for _, want := range []string{
		`receipt "` + id + `"`,
		"action " + strconv.Itoa(action),
		"text does not match registered action",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must contain %q (locate proposal and action)", err.Error(), want)
		}
	}
	// 反复打开结论一致，不接受“重算资金变动相同”的凭据，也不统一写法。
	if again, aerr := Open(path); again != nil || !errors.Is(aerr, ErrStateCorrupt) {
		again.Close()
		t.Fatalf("second Open store=%v err=%v, want nil + ErrStateCorrupt", again, aerr)
	}
	// 原文件保持篡改后的原样：应用不得自动统一写法并覆盖落盘。
	if got, gerr := os.ReadFile(path); gerr != nil || string(got) != string(tampered) {
		t.Fatalf("corrupt file was rewritten: readErr=%v", gerr)
	}
}

// TestAllZeroAmountWithLeadingZerosRejectedAtExecute：全部为零的金额即使带
// 前导零（000、00），仍按既有“正整数”规则在首次执行时拒绝；不产生部分
// 转账或成功凭据，失败不消耗执行机会，提案保持 passed，合法前导零照常执行。
func TestAllZeroAmountWithLeadingZerosRejectedAtExecute(t *testing.T) {
	for _, zero := range []string{"transfer:audits:000", "transfer:audits:00", "transfer:audits:0"} {
		t.Run(zero, func(t *testing.T) {
			store, _ := openTempStore(t, 50)
			defer store.Close()
			// 后随一笔合法转账：零金额被拒时整项预演，不得先转出后项或部分扣款。
			mustRegister(t, store, "gip-zero", 0, zero, "transfer:audits:5")
			rcpt, err := store.Execute("gip-zero", 0)
			if !errors.Is(err, ErrExecutionRejected) || !errors.Is(err, ErrInvalidAction) {
				t.Fatalf("execute %s: rcpt=%+v err=%v, want ErrExecutionRejected+ErrInvalidAction",
					zero, rcpt, err)
			}
			if !strings.Contains(err.Error(), "positive integer") {
				t.Fatalf("error %q must explain amount must be positive", err.Error())
			}
			if rcpt != nil {
				t.Fatalf("rejected execute returned a receipt: %+v", rcpt)
			}
			if bal, _ := store.TreasuryBalance(); bal != 50 {
				t.Fatalf("treasury=%d after zero rejection, want 50 (no partial transfer)", bal)
			}
			if bal, _ := store.Balance("audits"); bal != 0 {
				t.Fatalf("audits=%d after zero rejection, want 0", bal)
			}
			if rs, _ := store.Receipts(); len(rs) != 0 {
				t.Fatalf("receipts=%v, want none on rejected execute", rs)
			}
			if rec, _, _ := store.Proposal("gip-zero"); rec.State != "passed" {
				t.Fatalf("proposal state=%s after rejection, want passed", rec.State)
			}
			// 失败不消耗执行机会：再次执行仍按同一原因被拒。
			if rcpt2, err2 := store.Execute("gip-zero", 1); !errors.Is(err2, ErrExecutionRejected) || rcpt2 != nil {
				t.Fatalf("retry execute: rcpt=%+v err=%v, want rejection with no receipt", rcpt2, err2)
			}
		})
	}

	// 对照：合法前导零（007）不被误拒绝，正常执行并只按数值扣款。
	store, path := openTempStore(t, 50)
	defer store.Close()
	mustRegister(t, store, "gip-ok", 0, "transfer:audits:007")
	rcpt, err := store.Execute("gip-ok", 0)
	if err != nil || rcpt == nil {
		t.Fatalf("legal leading-zero execute: rcpt=%+v err=%v", rcpt, err)
	}
	if bal, _ := store.TreasuryBalance(); bal != 43 {
		t.Fatalf("treasury=%d, want 43", bal)
	}
	if bal, _ := store.Balance("audits"); bal != 7 {
		t.Fatalf("audits=%d, want 7", bal)
	}
	store.Close()
	if reopened, err := Open(path); err != nil {
		t.Fatalf("legal leading-zero state must reopen: %v", err)
	} else {
		defer reopened.Close()
		if rr, ok, err := reopened.Receipt("gip-ok"); err != nil || !ok ||
			rr.Actions[0].Action != "transfer:audits:007" {
			t.Fatalf("reopen receipt=%+v ok=%v err=%v", rr, ok, err)
		}
	}
}
