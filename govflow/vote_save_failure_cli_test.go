package govflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cliVoteCreateArgs 构造规格中的带委托提案：
// alice/bob/carol/dave 权重 300/200/100/400，bob、carol 委托给 alice，
// 法定人数 600，窗口 [100,200)，时间锁 300。
func cliVoteCreateArgs(id string) []string {
	return []string{"create-vote", "--id", id,
		"--member", "alice:300", "--member", "bob:200", "--member", "carol:100", "--member", "dave:400",
		"--delegate", "bob:alice", "--delegate", "carol:alice",
		"--quorum", "600", "--start", "100", "--deadline", "200", "--timelock", "300",
		"--action", "transfer:audits:100"}
}

// TestCLIVotePreReplaceFailureNoPartialRecord：跨进程复现标题场景。
// alice 在 150 的首次赞成因状态文件替换之前的保存错误失败：命令以域错误
// 退出码 1 结束、stdout 没有任何票据，stderr 给出具体保存原因；重新查询仍
// 只有 dave 在 120 的反对票。保存恢复后 160 的赞成被当作首次有效投票，
// 截止计票 600 赞成、400 反对、参与 1000 并通过；全过程不动资金余额、
// 不产生执行凭据。
func TestCLIVotePreReplaceFailureNoPartialRecord(t *testing.T) {
	binary := buildCLI(t)
	state := filepath.Join(t.TempDir(), "treasury.json")
	if _, _, code := runCLI(t, binary, state, "init", "--balance", "1000"); code != 0 {
		t.Fatal("init failed")
	}
	const id = "gip-cli-savefail"
	if _, se, code := runCLI(t, binary, state, cliVoteCreateArgs(id)...); code != 0 {
		t.Fatalf("create-vote failed: %s", se)
	}
	if _, se, code := runCLI(t, binary, state, "vote", "--id", id,
		"--voter", "dave", "--choice", "against", "--now", "120"); code != 0 {
		t.Fatalf("dave vote failed: %s", se)
	}
	good, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}

	// 目录不可写：CLI 进程无法创建临时文件，保存失败发生在状态文件替换之前。
	makeDirUnwritable(t, state)
	so, se, code := runCLI(t, binary, state, "vote", "--id", id,
		"--voter", "alice", "--choice", "for", "--now", "150", "--json")
	if code != 1 {
		t.Fatalf("failed save exit=%d, want 1 (stdout=%q stderr=%q)", code, so, se)
	}
	// 不能返回成功票据；stderr 必须带具体保存原因，而不是投票业务拒绝。
	if so != "" {
		t.Fatalf("stdout must stay empty on save failure, got %q", so)
	}
	if !strings.Contains(se, "permission denied") {
		t.Fatalf("stderr must carry the concrete save cause, got %q", se)
	}
	if strings.Contains(se, "vote rejected") {
		t.Fatalf("a valid first vote must not be reported as a business rejection: %q", se)
	}

	// 恢复保存条件：原文件字节未变，没有遗留临时文件。
	if err := os.Chmod(filepath.Dir(state), 0o700); err != nil {
		t.Fatalf("restore dir: %v", err)
	}
	if got, rerr := os.ReadFile(state); rerr != nil || string(got) != string(good) {
		t.Fatalf("state file changed despite save failure")
	}
	if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(state), ".govflow-state-*")); len(leftovers) != 0 {
		t.Fatalf("failed vote left temporary file(s): %v", leftovers)
	}

	// 查询仍只有 dave 的原记录：代表、400 权重、反对与首次时间 120；
	// 提案仍在投票中且没有计票结论。
	so, se, code = runCLI(t, binary, state, "proposal", "--id", id, "--json")
	if code != 0 {
		t.Fatalf("proposal query failed: %s", se)
	}
	if !strings.Contains(so, `"state": "voting"`) {
		t.Fatalf("proposal must stay voting:\n%s", so)
	}
	if strings.Contains(so, `"tally"`) {
		t.Fatalf("no tally conclusion may exist:\n%s", so)
	}
	if strings.Count(so, `"representative"`) != 1 ||
		!strings.Contains(so, `"representative": "dave"`) ||
		!strings.Contains(so, `"weight": 400`) ||
		!strings.Contains(so, `"support": false`) ||
		!strings.Contains(so, `"voted_at": 120`) {
		t.Fatalf("query must show only dave's original ballot:\n%s", so)
	}
	if strings.Contains(so, `"representative": "alice"`) {
		t.Fatalf("alice's 600 weight must not appear in any cast record:\n%s", so)
	}

	// 160 再投赞成：首次有效投票，600 权重、时间 160，不能报已投过票。
	so, se, code = runCLI(t, binary, state, "vote", "--id", id,
		"--voter", "alice", "--choice", "for", "--now", "160", "--json")
	if code != 0 {
		t.Fatalf("alice first valid vote exit=%d: %s", code, se)
	}
	if !strings.Contains(so, `"representative": "alice"`) ||
		!strings.Contains(so, `"weight": 600`) ||
		!strings.Contains(so, `"voted_at": 160`) {
		t.Fatalf("first valid vote receipt wrong:\n%s", so)
	}
	// 同选择重试（更晚时间）仍返回首次记录 160，不追加票据。
	so, _, code = runCLI(t, binary, state, "vote", "--id", id,
		"--voter", "alice", "--choice", "for", "--now", "180", "--json")
	if code != 0 || !strings.Contains(so, `"voted_at": 160`) {
		t.Fatalf("same-choice retry must return first record, code=%d:\n%s", code, so)
	}
	// 改投反对仍按既有冲突规则拒绝。
	if _, se, code = runCLI(t, binary, state, "vote", "--id", id,
		"--voter", "alice", "--choice", "against", "--now", "160"); code != 1 ||
		!strings.Contains(se, "changing the vote") {
		t.Fatalf("change vote code=%d stderr=%q, want conflict", code, se)
	}

	// 票据顺序 dave 在前、alice 在后。
	so, _, code = runCLI(t, binary, state, "proposal", "--id", id, "--json")
	if code != 0 {
		t.Fatal("proposal query failed")
	}
	davePos := strings.Index(so, `"representative": "dave"`)
	alicePos := strings.Index(so, `"representative": "alice"`)
	if davePos < 0 || alicePos < 0 || davePos > alicePos {
		t.Fatalf("ballot order must be dave then alice:\n%s", so)
	}

	// 截止计票：600 赞成、400 反对、参与 1000，通过。
	so, se, code = runCLI(t, binary, state, "tally", "--id", id, "--now", "200", "--json")
	if code != 0 {
		t.Fatalf("tally failed: %s", se)
	}
	for _, want := range []string{
		`"for_weight": 600`, `"against_weight": 400`, `"turnout": 1000`,
		`"quorum": 600`, `"passed": true`, `"tallied_at": 200`,
	} {
		if !strings.Contains(so, want) {
			t.Fatalf("tally output missing %s:\n%s", want, so)
		}
	}

	// 投票与计票不改变资金余额，也不生成执行凭据。
	so, _, code = runCLI(t, binary, state, "balances", "--json")
	if code != 0 || !strings.Contains(so, `"treasury": 1000`) || strings.Contains(so, "audits") {
		t.Fatalf("voting/tally must not move funds, code=%d:\n%s", code, so)
	}
	so, _, code = runCLI(t, binary, state, "receipts", "--json")
	if code != 0 || strings.TrimSpace(so) != "[]" {
		t.Fatalf("voting/tally must not produce receipts, code=%d:\n%s", code, so)
	}
}

// TestCLIVotePreReplaceFailureThenDeadlineRejects：保存失败后直到 200 才再次
// 提交时，alice 的首次票按截止边界拒绝；dave 的旧反对票保留，到期计票只能
// 得到参与权重 400 的拒绝结论，资金余额不变、无执行凭据。
func TestCLIVotePreReplaceFailureThenDeadlineRejects(t *testing.T) {
	binary := buildCLI(t)
	state := filepath.Join(t.TempDir(), "treasury.json")
	if _, _, code := runCLI(t, binary, state, "init", "--balance", "1000"); code != 0 {
		t.Fatal("init failed")
	}
	const id = "gip-cli-savefail-late"
	if _, se, code := runCLI(t, binary, state, cliVoteCreateArgs(id)...); code != 0 {
		t.Fatalf("create-vote failed: %s", se)
	}
	if _, se, code := runCLI(t, binary, state, "vote", "--id", id,
		"--voter", "dave", "--choice", "against", "--now", "120"); code != 0 {
		t.Fatalf("dave vote failed: %s", se)
	}

	makeDirUnwritable(t, state)
	if _, se, code := runCLI(t, binary, state, "vote", "--id", id,
		"--voter", "alice", "--choice", "for", "--now", "150"); code != 1 ||
		!strings.Contains(se, "permission denied") {
		t.Fatalf("failed save code=%d stderr=%q, want concrete save cause", code, se)
	}
	if err := os.Chmod(filepath.Dir(state), 0o700); err != nil {
		t.Fatalf("restore dir: %v", err)
	}

	// 截止时刻 200 才提交：按窗口 [start,deadline) 边界拒绝。
	_, se, code := runCLI(t, binary, state, "vote", "--id", id,
		"--voter", "alice", "--choice", "for", "--now", "200")
	if code != 1 || !strings.Contains(se, "not open for voting") {
		t.Fatalf("vote at deadline code=%d stderr=%q, want window rejection", code, se)
	}

	// 旧反对票照常保留，alice 没有任何记录。
	so, _, code := runCLI(t, binary, state, "proposal", "--id", id, "--json")
	if code != 0 {
		t.Fatal("proposal query failed")
	}
	if strings.Count(so, `"representative"`) != 1 ||
		!strings.Contains(so, `"representative": "dave"`) ||
		!strings.Contains(so, `"voted_at": 120`) ||
		strings.Contains(so, `"representative": "alice"`) {
		t.Fatalf("only dave's ballot must remain:\n%s", so)
	}

	// 到期计票：参与 400、未达法定人数，拒绝。
	so, se, code = runCLI(t, binary, state, "tally", "--id", id, "--now", "200", "--json")
	if code != 0 {
		t.Fatalf("tally failed: %s", se)
	}
	for _, want := range []string{
		`"for_weight": 0`, `"against_weight": 400`, `"turnout": 400`,
		`"quorum": 600`, `"passed": false`,
	} {
		if !strings.Contains(so, want) {
			t.Fatalf("tally output missing %s:\n%s", want, so)
		}
	}
	so, _, code = runCLI(t, binary, state, "balances", "--json")
	if code != 0 || !strings.Contains(so, `"treasury": 1000`) {
		t.Fatalf("balances must stay unchanged, code=%d:\n%s", code, so)
	}
	if so, _, code = runCLI(t, binary, state, "receipts", "--json"); code != 0 ||
		strings.TrimSpace(so) != "[]" {
		t.Fatalf("no receipts may exist, code=%d:\n%s", code, so)
	}
}
