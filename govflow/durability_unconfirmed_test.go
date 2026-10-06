package govflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// 本文件回归首次执行保存结果的两阶段表达：
//
//   - 状态文件已经原子替换成功、随后保存目录 fsync 失败：Execute 必须同时
//     返回完整的首次执行凭据与可程序识别的
//     ErrExecutionWrittenButDurabilityUnconfirmed（具体类型
//     *ExecutionWriteUnconfirmedError，保留原始保存失败原因）。这不是
//     “执行失败、没有写入”：不撤销转账、不退回 passed、不删除凭据；重开
//     后整项提案已执行，余额与动作留痕对应，投票与计票记录保留；再次执行
//     即使传入不同时间也只返回首次凭据。
//   - 保存失败发生在状态文件替换之前：仍按普通失败处理——具体错误、无凭据、
//     磁盘上的完整旧状态逐字节保留，提案条件满足后仍可正常执行。
//
// 两种来源（register 登记与投票通过）的提案适用同一规则。

// setFaultEnv 设置测试专用故障注入并在测试结束时清理。
func setFaultEnv(t *testing.T, name, value string) {
	t.Helper()
	if err := os.Setenv(name, value); err != nil {
		t.Fatalf("setenv %s: %v", name, err)
	}
	t.Cleanup(func() { _ = os.Unsetenv(name) })
}

// assertReceiptMatchesTransfer 断言一份首次执行凭据的字段与
// transfer:audits:100 + transfer:legal:50 在起始资金库 1000 上的结果一致。
func assertReceiptMatchesTransfer(t *testing.T, r *Receipt, id string, executedAt, order int64) {
	t.Helper()
	if r == nil {
		t.Fatal("receipt is nil, want the full first-execution receipt")
	}
	if r.ProposalID != id || r.ExecutedAt != executedAt || r.Order != order {
		t.Fatalf("receipt=%+v, want id=%s executed_at=%d order=%d", r, id, executedAt, order)
	}
	if len(r.Actions) != 2 {
		t.Fatalf("receipt actions=%d, want 2", len(r.Actions))
	}
	want := []struct {
		action            string
		trBefore, trAfter int64
		rcpt              string
		rcBefore, rcAfter int64
	}{
		{"transfer:audits:100", 1000, 900, "audits", 0, 100},
		{"transfer:legal:50", 900, 850, "legal", 0, 50},
	}
	for i, w := range want {
		ar := r.Actions[i]
		if ar.Index != int64(i) || ar.Action != w.action {
			t.Fatalf("action %d=%+v, want index=%d action=%q", i, ar, i, w.action)
		}
		if ar.Treasury.Account != "treasury" || ar.Treasury.Before != w.trBefore || ar.Treasury.After != w.trAfter {
			t.Fatalf("action %d treasury=%+v, want %d->%d", i, ar.Treasury, w.trBefore, w.trAfter)
		}
		if ar.Recipient.Account != w.rcpt || ar.Recipient.Before != w.rcBefore || ar.Recipient.After != w.rcAfter {
			t.Fatalf("action %d recipient=%+v, want %s %d->%d", i, ar.Recipient, w.rcpt, w.rcBefore, w.rcAfter)
		}
	}
}

// assertProposalFullyExecuted 断言资金库查询看到整项提案已经执行、余额与
// 全部动作留痕对应，且查询到的凭据与 Execute 返回的首次凭据逐字一致。
// source 为 "register" 或 "vote"，两种来源适用同一条保存结果规则。
func assertProposalFullyExecuted(t *testing.T, s *Store, id, source string, receipt *Receipt) {
	t.Helper()
	if bal, err := s.TreasuryBalance(); err != nil || bal != 850 {
		t.Fatalf("treasury=%d err=%v, want 850 (transfers not rolled back)", bal, err)
	}
	if bal, err := s.Balance("audits"); err != nil || bal != 100 {
		t.Fatalf("audits=%d err=%v, want 100", bal, err)
	}
	if bal, err := s.Balance("legal"); err != nil || bal != 50 {
		t.Fatalf("legal=%d err=%v, want 50", bal, err)
	}
	switch source {
	case "register":
		if p, ok, err := s.Proposal(id); err != nil || !ok || p.State != "executed" {
			t.Fatalf("registered proposal=%+v ok=%v err=%v, want executed", p, ok, err)
		}
	case "vote":
		v, ok, err := s.VoteProposal(id)
		if err != nil || !ok || v.State != "executed" {
			t.Fatalf("voting proposal=%+v ok=%v err=%v, want executed", v, ok, err)
		}
	default:
		t.Fatalf("unknown source %q", source)
	}
	got, ok, err := s.Receipt(id)
	if err != nil || !ok {
		t.Fatalf("receipt query ok=%v err=%v, want the written receipt", ok, err)
	}
	if !reflect.DeepEqual(got, receipt) {
		t.Fatalf("queried receipt differs from the one returned with the warning:\nwant=%+v\ngot =%+v",
			receipt, got)
	}
	all, err := s.Receipts()
	if err != nil || len(all) != 1 || !reflect.DeepEqual(all[0], receipt) {
		t.Fatalf("receipts=%v err=%v, want exactly the first receipt", all, err)
	}
}

// TestExecuteDirSyncFailureReturnsReceiptAndUnconfirmedError（register 来源）：
// 状态文件替换成功后目录同步失败时，Go 调用方同时拿到完整首次凭据与可识别
// 错误；写入不回滚，重试只返回首次凭据。
func TestExecuteDirSyncFailureReturnsReceiptAndUnconfirmedError(t *testing.T) {
	store, path := openTempStore(t, 1000)
	defer store.Close()
	const id = "gip-durability-register"
	mustRegister(t, store, id, 0, "transfer:audits:100", "transfer:legal:50")

	setFaultEnv(t, "GOVFLOW_TEST_FAIL_DIR_SYNC", "simulated directory fsync failure")

	receipt, err := store.Execute(id, 10)
	if err == nil {
		t.Fatal("execute must return an error when directory sync fails after replace")
	}
	if receipt == nil {
		t.Fatal("execute must still return the full first receipt, got nil")
	}
	if !errors.Is(err, ErrExecutionWrittenButDurabilityUnconfirmed) {
		t.Fatalf("err=%v must match ErrExecutionWrittenButDurabilityUnconfirmed", err)
	}
	var uc *ExecutionWriteUnconfirmedError
	if !errors.As(err, &uc) {
		t.Fatalf("err=%v must be *ExecutionWriteUnconfirmedError", err)
	}
	if uc.ProposalID != id {
		t.Fatalf("error ProposalID=%q, want %s", uc.ProposalID, id)
	}
	if !reflect.DeepEqual(uc.Receipt, receipt) {
		t.Fatalf("error receipt=%+v differs from returned receipt %+v", uc.Receipt, receipt)
	}
	// 原始保存失败原因必须原样保留。
	if cause := errors.Unwrap(err); cause == nil || cause.Error() != "simulated directory fsync failure" {
		t.Fatalf("unwrapped cause=%v, want the original directory sync error", cause)
	}
	if !strings.Contains(err.Error(), id) || !strings.Contains(err.Error(), "simulated directory fsync failure") {
		t.Fatalf("error message %q must carry proposal id and original cause", err.Error())
	}
	// 该错误不是普通的执行资格拒绝：动作确实已经执行。
	if errors.Is(err, ErrExecutionRejected) {
		t.Fatalf("written-but-unconfirmed must not classify as ErrExecutionRejected: %v", err)
	}
	assertReceiptMatchesTransfer(t, receipt, id, 10, 0)

	// 同一进程内立即查询：转账与 executed 状态已生效，凭据逐字一致。
	assertProposalFullyExecuted(t, store, id, "register", receipt)

	// 再次执行即使传入不同时间，也只取回原来的首次凭据，不二次扣款、
	// 不追加成功记录（此时仍处于目录同步故障注入下）。
	again, aerr := store.Execute(id, 999)
	if aerr != nil {
		t.Fatalf("idempotent retry must not re-attempt the commit: %v", aerr)
	}
	if !reflect.DeepEqual(again, receipt) {
		t.Fatalf("retry receipt=%+v, want first receipt %+v", again, receipt)
	}
	assertProposalFullyExecuted(t, store, id, "register", receipt)

	// 重新打开同一资金库：严格校验通过，看到的仍是完整执行后的状态。
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after unconfirmed durability: %v", err)
	}
	defer reopened.Close()
	assertProposalFullyExecuted(t, reopened, id, "register", receipt)
	retry, err := reopened.Execute(id, 12345)
	if err != nil || !reflect.DeepEqual(retry, receipt) {
		t.Fatalf("retry after reopen receipt=%+v err=%v, want first receipt", retry, err)
	}
	if bal, _ := reopened.TreasuryBalance(); bal != 850 {
		t.Fatalf("treasury changed after reopen retry: %d", bal)
	}
}

// TestExecuteDirSyncFailureVoteProposalSameRule（投票通过来源）：同样的告警
// 结果适用于投票通过的提案；告警后原有的投票与计票记录继续保留。
func TestExecuteDirSyncFailureVoteProposalSameRule(t *testing.T) {
	store, path := openTempStore(t, 1000)
	defer store.Close()
	const id = "gip-durability-vote"
	in := baseVoteInput(id)
	in.Actions = []string{"transfer:audits:100", "transfer:legal:50"}
	mustCreateVote(t, store, in)
	passVote(t, store, id, 150, 160, 200)

	setFaultEnv(t, "GOVFLOW_TEST_FAIL_DIR_SYNC", "vote-source dir sync boom")

	receipt, err := store.Execute(id, 300)
	if !errors.Is(err, ErrExecutionWrittenButDurabilityUnconfirmed) || receipt == nil {
		t.Fatalf("vote-source execute receipt=%v err=%v, want receipt + unconfirmed error", receipt, err)
	}
	assertReceiptMatchesTransfer(t, receipt, id, 300, 0)

	// 整项提案已执行；投票与计票记录原样保留，没有因告警退回 passed 或删除票据。
	v, ok, verr := store.VoteProposal(id)
	if verr != nil || !ok {
		t.Fatalf("VoteProposal ok=%v err=%v", ok, verr)
	}
	if v.State != "executed" {
		t.Fatalf("vote proposal state=%s, want executed (not rolled back to passed)", v.State)
	}
	if len(v.Ballots) != 2 {
		t.Fatalf("ballots=%+v, want the original 2 ballots preserved", v.Ballots)
	}
	if v.Tally == nil || !v.Tally.Passed || v.Tally.ForWeight != 600 ||
		v.Tally.AgainstWeight != 400 || v.Tally.TalliedAt != 200 {
		t.Fatalf("tally=%+v, want original passed tally preserved", v.Tally)
	}
	assertProposalFullyExecuted(t, store, id, "vote", receipt)
	// 投票来源的提案不在 register 表中，但凭据查询与余额表现一致。
	if all, _ := store.Receipts(); len(all) != 1 || all[0].ProposalID != id {
		t.Fatalf("receipts=%v, want the single vote-source receipt", all)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	v2, ok, _ := reopened.VoteProposal(id)
	if !ok || v2.State != "executed" || len(v2.Ballots) != 2 || v2.Tally == nil || !v2.Tally.Passed {
		t.Fatalf("governance records after reopen=%+v ok=%v, want executed with ballots and passed tally",
			v2, ok)
	}
	assertProposalFullyExecuted(t, reopened, id, "vote", receipt)
}

// TestExecuteFailureBeforeStateFileReplaceFullyReverts：保存失败发生在状态
// 文件替换之前时，沿用原有失败规则：具体错误、无凭据、旧状态逐字节保留，
// 提案在故障消除后正常执行。
func TestExecuteFailureBeforeStateFileReplaceFullyReverts(t *testing.T) {
	store, path := openTempStore(t, 1000)
	defer store.Close()
	const id = "gip-durability-before"
	mustRegister(t, store, id, 0, "transfer:audits:100", "transfer:legal:50")

	beforeBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	setFaultEnv(t, "GOVFLOW_TEST_FAIL_COMMIT_BEFORE_RENAME", "simulated pre-rename write failure")

	receipt, err := store.Execute(id, 10)
	if err == nil || !strings.Contains(err.Error(), "simulated pre-rename write failure") {
		t.Fatalf("execute err=%v, want the concrete pre-rename failure", err)
	}
	if receipt != nil {
		t.Fatalf("no receipt may be returned before the state file is replaced, got %+v", receipt)
	}
	if errors.Is(err, ErrExecutionWrittenButDurabilityUnconfirmed) {
		t.Fatalf("pre-rename failure must not classify as written-but-unconfirmed: %v", err)
	}

	afterBytes, err := os.ReadFile(path)
	if err != nil || string(afterBytes) != string(beforeBytes) {
		t.Fatal("state file changed despite pre-rename failure")
	}
	// 提案仍可执行：passed、无凭据、余额与动作留痕完全停留在执行前。
	if p, ok, _ := store.Proposal(id); !ok || p.State != "passed" {
		t.Fatalf("proposal=%+v ok=%v, want still passed", p, ok)
	}
	if r, ok, _ := store.Receipt(id); ok || r != nil {
		t.Fatalf("receipt=%+v ok=%v, want no receipt", r, ok)
	}
	if bal, _ := store.TreasuryBalance(); bal != 1000 {
		t.Fatalf("treasury=%d, want 1000", bal)
	}
	if bal, _ := store.Balance("audits"); bal != 0 {
		t.Fatalf("audits=%d, want 0", bal)
	}

	// 故障消除后正常执行：退出码路径的成功行为与凭据内容保持兼容。
	_ = os.Unsetenv("GOVFLOW_TEST_FAIL_COMMIT_BEFORE_RENAME")
	receipt, err = store.Execute(id, 10)
	if err != nil {
		t.Fatalf("execute after fault cleared: %v", err)
	}
	assertReceiptMatchesTransfer(t, receipt, id, 10, 0)
	assertProposalFullyExecuted(t, store, id, "register", receipt)
}

// runCLIWithEnv 在跨进程测试中以附加环境变量运行一次命令行。
func runCLIWithEnv(t *testing.T, binary, state string, env map[string]string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	full := append([]string{args[0], "--state", state}, args[1:]...)
	cmd := exec.Command(binary, full...)
	cmdEnv := os.Environ()
	for k, v := range env {
		cmdEnv = append(cmdEnv, k+"="+v)
	}
	cmd.Env = cmdEnv
	var so, se bytes.Buffer
	cmd.Stdout = &so
	cmd.Stderr = &se
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("run cli %v: %v", args, err)
		}
	}
	return so.String(), se.String(), code
}

// TestCLIDirSyncFailurePrintsReceiptToStdoutAndWarningToStderr：命令行在目录
// 同步失败时退出码仍为 1，但标准输出给出已写入的凭据（文本与 --json 都与
// 成功时同一份对象），stderr 明确说明“执行记录已写入，但持久性未确认”，
// 并带上提案编号与原始失败原因。之后的正常查询看到已执行状态与原凭据。
func TestCLIDirSyncFailurePrintsReceiptToStdoutAndWarningToStderr(t *testing.T) {
	binary := buildCLI(t) // 先编译，避免故障环境变量被 go build 子进程继承。
	state := filepath.Join(t.TempDir(), "treasury.json")
	if _, _, code := runCLI(t, binary, state, "init", "--balance", "1000"); code != 0 {
		t.Fatal("init failed")
	}
	const id = "gip-cli-durability"
	if _, _, code := runCLI(t, binary, state, "register", "--id", id,
		"--timelock", "5", "--action", "transfer:audits:100", "--action", "transfer:legal:50"); code != 0 {
		t.Fatal("register failed")
	}
	fault := map[string]string{"GOVFLOW_TEST_FAIL_DIR_SYNC": "cli dir sync failure detail"}

	// 普通文本：stdout 沿用现有凭据展示，stderr 给出告警，退出码 1。
	so, se, code := runCLIWithEnv(t, binary, state, fault, "execute", "--id", id, "--now", "7")
	if code != 1 {
		t.Fatalf("exit code=%d, want 1\nstdout=%s\nstderr=%s", code, so, se)
	}
	if !strings.Contains(so, "executed proposal "+id+" at 7") ||
		!strings.Contains(so, "treasury:  1000 -> 900") ||
		!strings.Contains(so, "audits: 0 -> 100") ||
		!strings.Contains(so, "treasury:  900 -> 850") ||
		!strings.Contains(so, "legal: 0 -> 50") {
		t.Fatalf("stdout must present the written receipt using the existing text layout:\n%s", so)
	}
	if !strings.Contains(se, "执行记录已写入，但持久性未确认") {
		t.Fatalf("stderr must state written-but-durability-unconfirmed in Chinese:\n%s", se)
	}
	if !strings.Contains(se, id) || !strings.Contains(se, "cli dir sync failure detail") {
		t.Fatalf("stderr must carry proposal id and original failure cause:\n%s", se)
	}

	// --json：stdout 仍是现有的凭据对象本身，不是 null、失败对象或失败前余额。
	const id2 = "gip-cli-durability-json"
	if _, _, code := runCLI(t, binary, state, "register", "--id", id2,
		"--timelock", "5", "--action", "transfer:audits:25"); code != 0 {
		t.Fatal("register second failed")
	}
	fault2 := map[string]string{"GOVFLOW_TEST_FAIL_DIR_SYNC": "json dir sync failure detail"}
	jso, jse, jcode := runCLIWithEnv(t, binary, state, fault2, "execute", "--id", id2, "--now", "8", "--json")
	if jcode != 1 {
		t.Fatalf("json exit code=%d, want 1\nstdout=%s\nstderr=%s", jcode, jso, jse)
	}
	var rcpt Receipt
	if err := json.Unmarshal([]byte(strings.TrimSpace(jso)), &rcpt); err != nil {
		t.Fatalf("json stdout must be the existing receipt object: %v\n%s", err, jso)
	}
	if rcpt.ProposalID != id2 || rcpt.ExecutedAt != 8 || rcpt.Order != 1 || len(rcpt.Actions) != 1 {
		t.Fatalf("json receipt=%+v, want second receipt order=1 executed_at=8 one action", rcpt)
	}
	if ar := rcpt.Actions[0]; ar.Treasury.Before != 850 || ar.Treasury.After != 825 ||
		ar.Recipient.Account != "audits" || ar.Recipient.Before != 100 || ar.Recipient.After != 125 {
		t.Fatalf("json receipt action balances=%+v must reflect the actually written transfer", ar)
	}
	if !strings.Contains(jse, "执行记录已写入，但持久性未确认") || !strings.Contains(jse, id2) ||
		!strings.Contains(jse, "json dir sync failure detail") {
		t.Fatalf("json stderr must carry the same warning fields:\n%s", jse)
	}

	// 随后正常读取同一资金库：两项提案均已执行，余额与动作留痕对应。
	if bo, _, code := runCLI(t, binary, state, "balances", "--json"); code != 0 ||
		!strings.Contains(bo, `"treasury": 825`) || !strings.Contains(bo, `"audits": 125`) ||
		!strings.Contains(bo, `"legal": 50`) {
		t.Fatalf("balances after warning code=%d:\n%s", code, bo)
	}
	if po, _, code := runCLI(t, binary, state, "proposal", "--id", id, "--json"); code != 0 ||
		!strings.Contains(po, `"state": "executed"`) {
		t.Fatalf("proposal query code=%d:\n%s", code, po)
	}
	if ro, _, code := runCLI(t, binary, state, "receipt", "--id", id, "--json"); code != 0 ||
		!strings.Contains(ro, `"executed_at": 7`) || !strings.Contains(ro, `"order": 0`) {
		t.Fatalf("receipt query code=%d:\n%s", code, ro)
	}
	if ro, _, code := runCLI(t, binary, state, "receipts", "--json"); code != 0 ||
		strings.Count(ro, `"proposal_id"`) != 2 {
		t.Fatalf("receipts code=%d:\n%s", code, ro)
	}

	// 用户再次执行该编号（不带故障注入、传入不同时间）：只取回原首次凭据，
	// 退出码 0，不再次扣款、不追加成功记录。
	retry, _, code := runCLI(t, binary, state, "execute", "--id", id, "--now", "999", "--json")
	if code != 0 {
		t.Fatalf("retry execute code=%d:\n%s", code, retry)
	}
	var retryRcpt Receipt
	if err := json.Unmarshal([]byte(strings.TrimSpace(retry)), &retryRcpt); err != nil {
		t.Fatalf("retry output: %v\n%s", err, retry)
	}
	if retryRcpt.ExecutedAt != 7 || retryRcpt.Order != 0 {
		t.Fatalf("retry receipt=%+v, want first receipt executed_at=7 order=0", retryRcpt)
	}
	if bo, _, _ := runCLI(t, binary, state, "balances", "--json"); !strings.Contains(bo, `"treasury": 825`) {
		t.Fatalf("treasury must stay 825 after retry:\n%s", bo)
	}
	if ro, _, _ := runCLI(t, binary, state, "receipts", "--json"); strings.Count(ro, `"proposal_id"`) != 2 {
		t.Fatalf("retry must not append a receipt:\n%s", ro)
	}
}
