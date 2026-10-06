package govflow

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeDirSyncFail 让状态文件所在目录可写可进入但不可读（0o300）：临时文件
// 写入、fsync 与原子改名仍然成功，只有 syncDir 打开目录失败，从而确定性地
// 复现“状态文件已替换成功、随后目录同步失败”。返回恢复函数。
func makeDirSyncFail(t *testing.T, statePath string) {
	t.Helper()
	dir := filepath.Dir(statePath)
	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
}

// makeDirUnwritable 让状态文件所在目录不可写（0o555）：提交在状态文件替换
// 之前（创建临时文件）就失败，复现“尚未写入”的保存失败。恢复由 Cleanup 负责。
func makeDirUnwritable(t *testing.T, statePath string) {
	t.Helper()
	dir := filepath.Dir(statePath)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
}

func assertReceiptShape(t *testing.T, r *Receipt, id string, executedAt, order int64, action string, treasuryBefore, treasuryAfter, recipientBefore, recipientAfter int64) {
	t.Helper()
	if r == nil {
		t.Fatal("receipt is nil")
	}
	if r.ProposalID != id || r.ExecutedAt != executedAt || r.Order != order {
		t.Fatalf("receipt header = %+v, want proposal=%s executed_at=%d order=%d", r, id, executedAt, order)
	}
	if len(r.Actions) != 1 {
		t.Fatalf("receipt actions = %+v, want exactly one", r.Actions)
	}
	ar := r.Actions[0]
	if ar.Index != 0 || ar.Action != action {
		t.Fatalf("action receipt = %+v, want index 0 action %q", ar, action)
	}
	if ar.Treasury.Before != treasuryBefore || ar.Treasury.After != treasuryAfter {
		t.Fatalf("treasury update = %d -> %d, want %d -> %d",
			ar.Treasury.Before, ar.Treasury.After, treasuryBefore, treasuryAfter)
	}
	if ar.Recipient.Before != recipientBefore || ar.Recipient.After != recipientAfter {
		t.Fatalf("recipient update = %d -> %d, want %d -> %d",
			ar.Recipient.Before, ar.Recipient.After, recipientBefore, recipientAfter)
	}
}

func assertSameReceipt(t *testing.T, got, want *Receipt) {
	t.Helper()
	if got == nil || want == nil {
		t.Fatalf("nil receipt: got=%v want=%v", got, want)
	}
	gj, _ := json.Marshal(got)
	wj, _ := json.Marshal(want)
	if string(gj) != string(wj) {
		t.Fatalf("receipts differ:\n got: %s\nwant: %s", gj, wj)
	}
}

// TestExecuteDirSyncFailureRegisterSource：登记来源提案在“状态文件已替换、
// 目录同步失败”时，Execute 必须同时返回完整首次凭据与可识别的告警错误；
// 告警不撤销转账、不退回 passed、不删除凭据，重试仍返回首次凭据。
func TestExecuteDirSyncFailureRegisterSource(t *testing.T) {
	store, path := openTempStore(t, 1000)
	defer store.Close()
	mustRegister(t, store, "gip-1", 100, "transfer:audits:250")

	makeDirSyncFail(t, path)
	receipt, err := store.Execute("gip-1", 150)
	if err == nil {
		t.Fatal("expected durability warning error, got nil")
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
	// 凭据完整：首次执行时间、提交序号、动作原文与各项余额变化。
	assertReceiptShape(t, receipt, "gip-1", 150, 0, "transfer:audits:250", 1000, 750, 0, 250)

	// 恢复目录后正常读取：整项提案已执行，余额与动作留痕对应。
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("restore dir: %v", err)
	}
	record, ok, err := store.Proposal("gip-1")
	if err != nil || !ok || record.State != "executed" {
		t.Fatalf("proposal = %+v ok=%v err=%v, want executed", record, ok, err)
	}
	snapshot, err := store.BalanceSnapshot()
	if err != nil {
		t.Fatalf("BalanceSnapshot: %v", err)
	}
	if snapshot.Treasury != 750 || snapshot.Balances["audits"] != 250 {
		t.Fatalf("snapshot = %+v, want treasury 750 audits 250", snapshot)
	}
	queried, ok, err := store.Receipt("gip-1")
	if err != nil || !ok {
		t.Fatalf("Receipt query ok=%v err=%v", ok, err)
	}
	// 告警时返回的凭据必须与之后正常查询取得的记录一致。
	assertSameReceipt(t, receipt, queried)

	// 再次执行（即使传入不同时间）只取得原来的首次凭据，不再次扣款、不追加记录。
	again, err := store.Execute("gip-1", 999)
	if err != nil {
		t.Fatalf("re-execute: %v", err)
	}
	assertSameReceipt(t, again, queried)
	receipts, err := store.Receipts()
	if err != nil || len(receipts) != 1 {
		t.Fatalf("receipts = %v err=%v, want exactly one", receipts, err)
	}
	if bal, _ := store.TreasuryBalance(); bal != 750 {
		t.Fatalf("treasury = %d after re-execute, want 750", bal)
	}
}

// TestExecuteDirSyncFailureVoteSource：投票通过来源的提案适用同一规则，
// 且原有的投票与计票记录在告警后继续保留。
func TestExecuteDirSyncFailureVoteSource(t *testing.T) {
	store, path := openTempStore(t, 1000)
	defer store.Close()
	in := baseVoteInput("gip-v1")
	mustCreateVote(t, store, in)
	if _, err := store.CastVote("gip-v1", "alice", true, 150); err != nil {
		t.Fatalf("CastVote: %v", err)
	}
	if _, err := store.CastVote("gip-v1", "dave", false, 150); err != nil {
		t.Fatalf("CastVote: %v", err)
	}
	result, err := store.TallyVote("gip-v1", 200)
	if err != nil || !result.Passed {
		t.Fatalf("TallyVote = %+v err=%v, want passed", result, err)
	}

	makeDirSyncFail(t, path)
	receipt, err := store.Execute("gip-v1", 300)
	if !errors.Is(err, ErrDurabilityUnconfirmed) {
		t.Fatalf("err = %v, want ErrDurabilityUnconfirmed", err)
	}
	assertReceiptShape(t, receipt, "gip-v1", 300, 0, "transfer:audits:100", 1000, 900, 0, 100)

	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("restore dir: %v", err)
	}
	view, ok, err := store.VoteProposal("gip-v1")
	if err != nil || !ok {
		t.Fatalf("VoteProposal ok=%v err=%v", ok, err)
	}
	if view.State != "executed" {
		t.Fatalf("vote proposal state = %s, want executed", view.State)
	}
	// 投票与计票记录继续保留。
	if len(view.Ballots) != 2 {
		t.Fatalf("ballots = %+v, want the two recorded ballots", view.Ballots)
	}
	if view.Tally == nil || !view.Tally.Passed || view.Tally.TalliedAt != 200 {
		t.Fatalf("tally = %+v, want first passed tally at 200", view.Tally)
	}
	queried, ok, err := store.Receipt("gip-v1")
	if err != nil || !ok {
		t.Fatalf("Receipt query ok=%v err=%v", ok, err)
	}
	assertSameReceipt(t, receipt, queried)

	again, err := store.Execute("gip-v1", 12345)
	if err != nil {
		t.Fatalf("re-execute: %v", err)
	}
	assertSameReceipt(t, again, queried)
	if bal, _ := store.TreasuryBalance(); bal != 900 {
		t.Fatalf("treasury = %d after re-execute, want 900", bal)
	}
}

// TestExecutePreReplaceFailureKeepsPriorState：保存失败发生在状态文件替换
// 之前时仍按原来的失败规则处理：具体错误、无凭据、状态完整、提案保持
// passed，条件满足后仍可执行。
func TestExecutePreReplaceFailureKeepsPriorState(t *testing.T) {
	store, path := openTempStore(t, 1000)
	defer store.Close()
	mustRegister(t, store, "gip-2", 100, "transfer:audits:250")

	makeDirUnwritable(t, path)
	receipt, err := store.Execute("gip-2", 150)
	if err == nil {
		t.Fatal("expected save failure, got nil error")
	}
	if errors.Is(err, ErrDurabilityUnconfirmed) {
		t.Fatalf("pre-replacement failure must not be reported as durability warning: %v", err)
	}
	if receipt != nil {
		t.Fatalf("pre-replacement failure must not return a success receipt: %+v", receipt)
	}

	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("restore dir: %v", err)
	}
	// 之前的完整状态保留：提案仍 passed，余额不变，没有凭据。
	record, ok, err := store.Proposal("gip-2")
	if err != nil || !ok || record.State != "passed" {
		t.Fatalf("proposal = %+v ok=%v err=%v, want passed", record, ok, err)
	}
	if bal, _ := store.TreasuryBalance(); bal != 1000 {
		t.Fatalf("treasury = %d, want untouched 1000", bal)
	}
	if _, ok, _ := store.Receipt("gip-2"); ok {
		t.Fatal("no receipt may exist after pre-replacement failure")
	}
	// 条件满足后仍可执行。
	executed, err := store.Execute("gip-2", 200)
	if err != nil {
		t.Fatalf("execute after recovery: %v", err)
	}
	assertReceiptShape(t, executed, "gip-2", 200, 0, "transfer:audits:250", 1000, 750, 0, 250)
}

// TestCLIExecuteDirSyncFailure：命令行在“已写入但持久性未确认”时仍以退出码 1
// 结束，标准输出给出已写入的凭据，标准错误说明告警；随后查询与重试行为不变。
func TestCLIExecuteDirSyncFailure(t *testing.T) {
	binary := buildCLI(t)
	state := filepath.Join(t.TempDir(), "treasury.json")
	if _, _, code := runCLI(t, binary, state, "init", "--balance", "1000"); code != 0 {
		t.Fatal("init failed")
	}
	if _, _, code := runCLI(t, binary, state, "register", "--id", "gip-1",
		"--timelock", "100", "--action", "transfer:audits:250"); code != 0 {
		t.Fatal("register failed")
	}
	if _, _, code := runCLI(t, binary, state, "register", "--id", "gip-2",
		"--timelock", "100", "--action", "transfer:legal:50"); code != 0 {
		t.Fatal("register failed")
	}

	makeDirSyncFail(t, state)

	// 普通文本：退出码 1，stdout 是已写入的凭据，stderr 说明告警。
	so, se, code := runCLI(t, binary, state, "execute", "--id", "gip-1", "--now", "150")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (stdout=%q stderr=%q)", code, so, se)
	}
	if !strings.Contains(so, "executed proposal gip-1 at 150") ||
		!strings.Contains(so, "treasury:  1000 -> 750") ||
		!strings.Contains(so, "audits: 0 -> 250") {
		t.Fatalf("stdout must show the written receipt, got:\n%s", so)
	}
	if !strings.Contains(se, "gip-1") ||
		!strings.Contains(se, "durability not confirmed") {
		t.Fatalf("stderr must state the durability warning with the proposal id, got:\n%s", se)
	}

	// --json：stdout 仍是凭据对象，不是空值或失败前余额。
	so, se, code = runCLI(t, binary, state, "execute", "--id", "gip-2", "--now", "160", "--json")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (stdout=%q stderr=%q)", code, so, se)
	}
	var receipt struct {
		ProposalID string `json:"proposal_id"`
		ExecutedAt int64  `json:"executed_at"`
		Order      int64  `json:"order"`
		Actions    []struct {
			Action   string `json:"action"`
			Treasury struct {
				Before int64 `json:"before"`
				After  int64 `json:"after"`
			} `json:"treasury"`
		} `json:"actions"`
	}
	if err := json.Unmarshal([]byte(so), &receipt); err != nil {
		t.Fatalf("stdout must be the receipt JSON object: %v\n%s", err, so)
	}
	if receipt.ProposalID != "gip-2" || receipt.ExecutedAt != 160 || receipt.Order != 1 ||
		len(receipt.Actions) != 1 || receipt.Actions[0].Action != "transfer:legal:50" ||
		receipt.Actions[0].Treasury.Before != 750 || receipt.Actions[0].Treasury.After != 700 {
		t.Fatalf("unexpected receipt from warning execute: %+v", receipt)
	}
	if !strings.Contains(se, "gip-2") {
		t.Fatalf("stderr must name the proposal, got:\n%s", se)
	}

	if err := os.Chmod(filepath.Dir(state), 0o700); err != nil {
		t.Fatalf("restore dir: %v", err)
	}

	// 告警不撤销转账：两份凭据都可查询，余额与动作留痕对应。
	so, _, code = runCLI(t, binary, state, "balances", "--json")
	if code != 0 || !strings.Contains(so, `"treasury": 700`) ||
		!strings.Contains(so, `"audits": 250`) || !strings.Contains(so, `"legal": 50`) {
		t.Fatalf("balances after warning = code %d:\n%s", code, so)
	}
	so, _, code = runCLI(t, binary, state, "receipts", "--json")
	if code != 0 || strings.Count(so, `"proposal_id"`) != 2 {
		t.Fatalf("receipts after warning = code %d:\n%s", code, so)
	}
	// 再次执行同一编号（不同时间）只取得原来的首次凭据，退出码 0，不再次扣款。
	so, _, code = runCLI(t, binary, state, "execute", "--id", "gip-1", "--now", "999", "--json")
	if code != 0 || !strings.Contains(so, `"executed_at": 150`) {
		t.Fatalf("re-execute must return the first receipt, code %d:\n%s", code, so)
	}
	so, _, _ = runCLI(t, binary, state, "balances", "--json")
	if !strings.Contains(so, `"treasury": 700`) {
		t.Fatalf("re-execute must not debit again:\n%s", so)
	}
}
