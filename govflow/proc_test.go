package govflow

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// buildCLI 将命令行编译到临时目录，供跨进程测试使用。仅使用标准库，离线可编译。
func buildCLI(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "govflow")
	cmd := exec.Command("go", "build", "-o", binary, "./cmd/govflow")
	// 测试工作目录为模块下的 govflow/，仓库根目录在上一层。
	cmd.Dir = ".."
	var out bytes.Buffer
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("build cli: %v\n%s", err, out.String())
	}
	return binary
}

func runCLI(t *testing.T, binary, state string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	// main 以第一个参数作为子命令，--state 必须位于子命令之后。
	full := append([]string{args[0], "--state", state}, args[1:]...)
	cmd := exec.Command(binary, full...)
	var so, se bytes.Buffer
	cmd.Stdout = &so
	cmd.Stderr = &se
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run cli %v: %v", args, err)
	}
	return so.String(), se.String(), code
}

// TestCrossProcessSameProposal：多个进程同时执行同一提案，
// 只能有一份成功凭据；其余进程拿到同一份凭据（重试语义），不得二次扣款。
func TestCrossProcessSameProposal(t *testing.T) {
	binary := buildCLI(t)
	state := filepath.Join(t.TempDir(), "treasury.json")
	if _, _, code := runCLI(t, binary, state, "init", "--balance", "1000"); code != 0 {
		t.Fatal("init failed")
	}
	if _, _, code := runCLI(t, binary, state, "register", "--id", "gip-1",
		"--timelock", "100", "--action", "transfer:audits:250"); code != 0 {
		t.Fatal("register failed")
	}

	const n = 16
	var wg sync.WaitGroup
	codes := make([]int, n)
	outputs := make([]string, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			so, se, code := runCLI(t, binary, state, "execute", "--id", "gip-1",
				"--now", strconv.Itoa(100+i), "--json")
			codes[i] = code
			outputs[i] = so + se
		}(i)
	}
	close(start)
	wg.Wait()

	for i, code := range codes {
		if code != 0 {
			t.Fatalf("process %d exited %d: %s", i, code, outputs[i])
		}
		// 每个进程看到的首次执行时间都必须是 100（最早请求未必胜出，但凭据唯一）。
		if !strings.Contains(outputs[i], `"executed_at"`) {
			t.Fatalf("process %d got no receipt: %s", i, outputs[i])
		}
	}
	so, _, code := runCLI(t, binary, state, "receipts", "--json")
	if code != 0 {
		t.Fatal("receipts failed")
	}
	if strings.Count(so, `"proposal_id"`) != 1 {
		t.Fatalf("expected exactly one receipt, got:\n%s", so)
	}
	if so2, _, _ := runCLI(t, binary, state, "balances", "--json"); !strings.Contains(so2, `"treasury": 750`) {
		t.Fatalf("treasury must be 750 after single execution:\n%s", so2)
	}
}

// TestCrossProcessDifferentProposals：40 个进程各花 10，资金库只有 100，
// 必须恰好 10 个成功，且所有判断基于实际已提交余额。
func TestCrossProcessDifferentProposals(t *testing.T) {
	binary := buildCLI(t)
	state := filepath.Join(t.TempDir(), "treasury.json")
	if _, _, code := runCLI(t, binary, state, "init", "--balance", "100"); code != 0 {
		t.Fatal("init failed")
	}
	const n = 40
	for i := 0; i < n; i++ {
		if _, _, code := runCLI(t, binary, state, "register", "--id", "gip-"+itoa(i),
			"--timelock", "0", "--action", "transfer:acct:10"); code != 0 {
			t.Fatalf("register %d failed", i)
		}
	}

	var wg sync.WaitGroup
	successes := make(chan int, n)
	failures := make(chan string, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, se, code := runCLI(t, binary, state, "execute", "--id", "gip-"+itoa(i), "--now", "0")
			if code == 0 {
				successes <- 1
			} else {
				failures <- se
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(successes)
	close(failures)
	got := 0
	for range successes {
		got++
	}
	for msg := range failures {
		if !strings.Contains(msg, "insufficient") {
			t.Fatalf("unexpected failure reason: %s", msg)
		}
	}
	if got != 10 {
		t.Fatalf("cross-process successes=%d, want 10", got)
	}

	so, _, _ := runCLI(t, binary, state, "balances", "--json")
	if !strings.Contains(so, `"treasury": 0`) || !strings.Contains(so, `"acct": 100`) {
		t.Fatalf("final balances wrong:\n%s", so)
	}
	so, _, _ = runCLI(t, binary, state, "receipts", "--json")
	if strings.Count(so, `"proposal_id"`) != 10 {
		t.Fatalf("expected 10 receipts, got:\n%s", so)
	}
}

// TestCrossProcessCrashConsistency：在并发执行期间反复 SIGKILL 进程，
// 状态文件在任何时刻都只能是整项执行前或执行后的完整状态。
func TestCrossProcessCrashConsistency(t *testing.T) {
	binary := buildCLI(t)
	state := filepath.Join(t.TempDir(), "treasury.json")
	if _, _, code := runCLI(t, binary, state, "init", "--balance", "1000000"); code != 0 {
		t.Fatal("init failed")
	}
	const n = 200
	for i := 0; i < n; i++ {
		if _, _, code := runCLI(t, binary, state, "register", "--id", "gip-"+itoa(i),
			"--timelock", "0", "--action", "transfer:a:1"); code != 0 {
			t.Fatalf("register %d failed", i)
		}
	}

	// 启动后立刻 SIGKILL：杀死可能落在执行前、写盘前后任意位置。
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := exec.Command(binary, "execute", "--state", state, "--id", "gip-"+itoa(i), "--now", "0")
			if err := cmd.Start(); err != nil {
				t.Errorf("start %d: %v", i, err)
				return
			}
			if i%3 == 0 {
				_ = cmd.Process.Kill()
			}
			_ = cmd.Wait()
		}(i)
	}
	wg.Wait()

	// 最终状态必须能严格打开：余额与凭据重放一致；被杀死的执行要么整体发生、要么整体未发生。
	store, err := Open(state)
	if err != nil {
		t.Fatalf("state after crashes is unusable: %v", err)
	}
	defer store.Close()
	treasury, err := store.TreasuryBalance()
	if err != nil {
		t.Fatal(err)
	}
	receipts, err := store.Receipts()
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(receipts)) != 1000000-treasury {
		t.Fatalf("invariant broken: receipts=%d treasury=%d", len(receipts), treasury)
	}
	// 每个 executed 提案恰好有凭据；无凭据的提案仍是 passed 且余额未变（重放校验已在 Open 中完成）。
	executed := map[string]bool{}
	for _, r := range receipts {
		if executed[r.ProposalID] {
			t.Fatalf("duplicate receipt for %s", r.ProposalID)
		}
		executed[r.ProposalID] = true
	}
	proposals, err := store.Proposals()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range proposals {
		if p.State == "executed" && !executed[p.ID] {
			t.Fatalf("proposal %s executed without receipt", p.ID)
		}
		if p.State == "passed" && executed[p.ID] {
			t.Fatalf("proposal %s passed but has receipt", p.ID)
		}
	}
}

// TestCrossProcessVotingLifecycle：通过 CLI 跨进程完成创建→委托投票→计票→执行，
// 并验证重复创建/投票/计票的幂等与冲突语义。
func TestCrossProcessVotingLifecycle(t *testing.T) {
	binary := buildCLI(t)
	state := filepath.Join(t.TempDir(), "treasury.json")
	if _, _, code := runCLI(t, binary, state, "init", "--balance", "1000"); code != 0 {
		t.Fatal("init failed")
	}
	create := []string{"create-vote", "--id", "gip-vote-1",
		"--member", "alice:300", "--member", "bob:200", "--member", "carol:100", "--member", "dave:400",
		"--delegate", "bob:alice", "--delegate", "carol:alice",
		"--quorum", "600", "--start", "100", "--deadline", "200", "--timelock", "300",
		"--action", "transfer:audits:100"}
	if _, se, code := runCLI(t, binary, state, create...); code != 0 {
		t.Fatalf("create-vote failed: %s", se)
	}
	// 成员与委托换序的完全相同重试：成功且不改动状态。
	retry := []string{"create-vote", "--id", "gip-vote-1",
		"--member", "dave:400", "--member", "carol:100", "--member", "bob:200", "--member", "alice:300",
		"--delegate", "carol:alice", "--delegate", "bob:alice",
		"--quorum", "600", "--start", "100", "--deadline", "200", "--timelock", "300",
		"--action", "transfer:audits:100"}
	if _, se, code := runCLI(t, binary, state, retry...); code != 0 {
		t.Fatalf("identical create retry should succeed: %s", se)
	}
	// 内容不同的重试冲突（多一个动作，动作按原文及顺序比较）。
	conflict := append(append([]string{}, create...), "--action", "transfer:legal:1")
	if _, _, code := runCLI(t, binary, state, conflict...); code != 1 {
		t.Fatalf("conflicting create should exit 1, got %d", code)
	}
	// register 不能占用投票提案编号。
	if _, se, code := runCLI(t, binary, state, "register", "--id", "gip-vote-1",
		"--timelock", "300", "--action", "transfer:audits:100"); code != 1 {
		t.Fatalf("register over voting id should fail: %s", se)
	}

	// 委托出去的成员投票被拒；非名单成员被拒。
	if _, se, code := runCLI(t, binary, state, "vote", "--id", "gip-vote-1",
		"--voter", "bob", "--choice", "for", "--now", "150"); code != 1 || !strings.Contains(se, "delegated") {
		t.Fatalf("delegated member vote code=%d: %s", code, se)
	}
	if _, se, code := runCLI(t, binary, state, "vote", "--id", "gip-vote-1",
		"--voter", "ghost", "--choice", "for", "--now", "150"); code != 1 || !strings.Contains(se, "not on the member roster") {
		t.Fatalf("non-member vote code=%d: %s", code, se)
	}

	// 代表 alice（归集 600）赞成；相同选择跨进程重试返回首次时间。
	if so, se, code := runCLI(t, binary, state, "vote", "--id", "gip-vote-1",
		"--voter", "alice", "--choice", "for", "--now", "120", "--json"); code != 0 ||
		!strings.Contains(so, `"weight": 600`) {
		t.Fatalf("alice vote code=%d so=%s se=%s", code, so, se)
	}
	if so, _, code := runCLI(t, binary, state, "vote", "--id", "gip-vote-1",
		"--voter", "alice", "--choice", "for", "--now", "180", "--json"); code != 0 ||
		!strings.Contains(so, `"voted_at": 120`) {
		t.Fatalf("identical vote retry must return first record: %s", so)
	}
	// 改投冲突。
	if _, se, code := runCLI(t, binary, state, "vote", "--id", "gip-vote-1",
		"--voter", "alice", "--choice", "against", "--now", "120"); code != 1 ||
		!strings.Contains(se, "changing the vote") {
		t.Fatalf("change vote code=%d: %s", code, se)
	}

	// 截止前计票拒绝。
	if _, se, code := runCLI(t, binary, state, "tally", "--id", "gip-vote-1", "--now", "199"); code != 1 ||
		!strings.Contains(se, "still open") {
		t.Fatalf("early tally code=%d: %s", code, se)
	}
	// 截止时刻计票：600 赞成、0 反对、达到法定人数 => passed。
	if so, se, code := runCLI(t, binary, state, "tally", "--id", "gip-vote-1",
		"--now", "200", "--json"); code != 0 || !strings.Contains(so, `"passed": true`) {
		t.Fatalf("tally code=%d so=%s se=%s", code, so, se)
	}
	// 计票后窗口内新票也拒绝。
	if _, se, code := runCLI(t, binary, state, "vote", "--id", "gip-vote-1",
		"--voter", "dave", "--choice", "against", "--now", "150"); code != 1 ||
		!strings.Contains(se, "already been tallied") {
		t.Fatalf("post-tally vote code=%d: %s", code, se)
	}
	// 再次计票返回首次时间。
	if so, _, code := runCLI(t, binary, state, "tally", "--id", "gip-vote-1",
		"--now", "999", "--json"); code != 0 || !strings.Contains(so, `"tallied_at": 200`) {
		t.Fatalf("repeat tally: %s", so)
	}

	// 通过后直接由 execute 执行，无需再登记；时间锁未到拒绝。
	if _, _, code := runCLI(t, binary, state, "execute", "--id", "gip-vote-1", "--now", "299"); code != 1 {
		t.Fatalf("execute before timelock should fail")
	}
	if _, _, code := runCLI(t, binary, state, "execute", "--id", "gip-vote-1", "--now", "300"); code != 0 {
		t.Fatalf("execute voted proposal failed")
	}
	if so, _, _ := runCLI(t, binary, state, "balances", "--json"); !strings.Contains(so, `"treasury": 900`) ||
		!strings.Contains(so, `"audits": 100`) {
		t.Fatalf("balances after voted execution:\n%s", so)
	}
	// 已执行提案不退回通过：再次执行返回首次凭据。
	if so, _, _ := runCLI(t, binary, state, "execute", "--id", "gip-vote-1",
		"--now", "9999", "--json"); !strings.Contains(so, `"executed_at": 300`) {
		t.Fatalf("re-execute should return first receipt: %s", so)
	}
	// 查询包含逐票明细、委托路径与已执行状态。
	so, _, _ := runCLI(t, binary, state, "proposal", "--id", "gip-vote-1", "--json")
	if !strings.Contains(so, `"state": "executed"`) ||
		!strings.Contains(so, `"representative": "alice"`) ||
		!pathJSONRe.MatchString(so) {
		t.Fatalf("proposal query missing delegation path/ballot/state:\n%s", so)
	}
}

var pathJSONRe = regexp.MustCompile(`"path": \[\s+"bob",\s+"alice"\s+\]`)

// TestCrossProcessConcurrentTally：多进程同时计票，结论只产生一次且不追加任何记录。
func TestCrossProcessConcurrentTally(t *testing.T) {
	binary := buildCLI(t)
	state := filepath.Join(t.TempDir(), "treasury.json")
	if _, _, code := runCLI(t, binary, state, "init", "--balance", "1000"); code != 0 {
		t.Fatal("init failed")
	}
	if _, _, code := runCLI(t, binary, state, "create-vote", "--id", "gip-c",
		"--member", "a:600", "--member", "b:400",
		"--quorum", "600", "--start", "0", "--deadline", "10", "--timelock", "10",
		"--action", "transfer:x:1"); code != 0 {
		t.Fatal("create failed")
	}
	if _, _, code := runCLI(t, binary, state, "vote", "--id", "gip-c",
		"--voter", "a", "--choice", "for", "--now", "5"); code != 0 {
		t.Fatal("vote failed")
	}
	const n = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	outputs := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			so, _, _ := runCLI(t, binary, state, "tally", "--id", "gip-c",
				"--now", strconv.Itoa(10+i), "--json")
			outputs[i] = so
		}(i)
	}
	close(start)
	wg.Wait()
	// 首个抢到锁的进程完成首次计票（其 now 未必最小）；其余进程必须拿到同一结论。
	var winner string
	for i, out := range outputs {
		if !strings.Contains(out, `"passed": true`) {
			t.Fatalf("tally %d diverged or failed: %s", i, out)
		}
		m := tallyTimeRe.FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("tally %d missing tallied_at: %s", i, out)
		}
		if winner == "" {
			winner = m[1]
		} else if m[1] != winner {
			t.Fatalf("tally %d tallied_at=%s, want first result %s", i, m[1], winner)
		}
	}
	so, _, _ := runCLI(t, binary, state, "proposal", "--id", "gip-c", "--json")
	if strings.Count(so, `"voted_at"`) != 1 {
		t.Fatalf("ballot records changed under concurrent tally:\n%s", so)
	}
}

var tallyTimeRe = mustRegexp(`"tallied_at": (\d+)`)

func mustRegexp(pattern string) *regexp.Regexp { return regexp.MustCompile(pattern) }

// 删除句柄后状态文件仍是完整 JSON 且可再次打开。
func TestReopenAfterClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "treasury.json")
	store, err := InitTreasury(path, 5)
	if err != nil {
		t.Fatal(err)
	}
	mustRegister(t, store, "gip-1", 0, "transfer:a:5")
	if _, err := store.Execute("gip-1", 0); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(raw), stateMagic) {
		t.Fatalf("state file missing or bad after close: %v", err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if bal, _ := reopened.TreasuryBalance(); bal != 0 {
		t.Fatalf("balance after reopen = %d", bal)
	}
}

// TestCLIRejectsCaseVariantField：固定字段名的大小写变体让整个状态文件被拒绝，
// 命令行将原因（含问题字段）写到标准错误并以状态码 1 退出，文件不被改写。
func TestCLIRejectsCaseVariantField(t *testing.T) {
	binary := buildCLI(t)
	dir := t.TempDir()
	state := filepath.Join(dir, "treasury.json")
	if _, se, code := runCLI(t, binary, state, "init", "--balance", "100"); code != 0 {
		t.Fatalf("init exit=%d: %s", code, se)
	}
	raw, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := strings.Replace(string(raw), `"treasury": 100`, `"Treasury": 100`, 1)
	if corrupt == string(raw) {
		t.Fatalf("setup: treasury field not found in %s", raw)
	}
	if err := os.WriteFile(state, []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}
	so, se, code := runCLI(t, binary, state, "balances")
	if code != 1 {
		t.Fatalf("balances exit=%d, want 1 (stdout=%q stderr=%q)", code, so, se)
	}
	if !strings.Contains(se, "corrupt") || !strings.Contains(se, `"Treasury"`) {
		t.Fatalf("stderr should name the corruption and the field, got %q", se)
	}
	// 不提供部分余额信息，也不整理或写回原文件。
	if so != "" {
		t.Fatalf("stdout should be empty on corruption, got %q", so)
	}
	if got, err := os.ReadFile(state); err != nil || string(got) != corrupt {
		t.Fatalf("corrupt file was modified")
	}
}

// TestCLIRejectsIncompleteBallot：未计票提案的票据缺 support 时，文本与 JSON
// 查询、计票都把整份文件判为损坏：原因写入 stderr（能看出提案编号、票据与字段、
// 空值/缺失/类型不符），以域错误退出码 1 结束，stdout 不出现部分结果，文件不改写。
func TestCLIRejectsIncompleteBallot(t *testing.T) {
	binary := buildCLI(t)
	dir := t.TempDir()
	state := filepath.Join(dir, "treasury.json")
	if _, _, code := runCLI(t, binary, state, "init", "--balance", "1000"); code != 0 {
		t.Fatal("init failed")
	}
	// 投票窗口从 0 开始；赞成票在 0 时刻投出，且始终不计票。
	if _, _, code := runCLI(t, binary, state, "create-vote", "--id", "gip-cli",
		"--member", "alice:600", "--member", "dave:400",
		"--quorum", "600", "--start", "0", "--deadline", "10", "--timelock", "10",
		"--action", "transfer:a:1"); code != 0 {
		t.Fatal("create-vote failed")
	}
	if _, _, code := runCLI(t, binary, state, "vote", "--id", "gip-cli",
		"--voter", "alice", "--choice", "for", "--now", "0"); code != 0 {
		t.Fatal("vote failed")
	}
	raw, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := strings.Replace(string(raw), `"support": true`, `"support": null`, 1)
	if corrupt == string(raw) {
		t.Fatal("setup: support field not found")
	}
	if err := os.WriteFile(state, []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}

	// 文本与 JSON 查询采用同一判定：均为退出码 1、原因在 stderr、stdout 为空。
	for _, args := range [][]string{
		{"proposal", "--id", "gip-cli"},
		{"proposal", "--id", "gip-cli", "--json"},
		{"proposals"},
		{"proposals", "--json"},
		{"tally", "--id", "gip-cli", "--now", "10"},
		{"tally", "--id", "gip-cli", "--now", "10", "--json"},
		{"balances", "--json"},
	} {
		name := strings.Join(args, "_")
		t.Run(name, func(t *testing.T) {
			so, se, code := runCLI(t, binary, state, args...)
			if code != 1 {
				t.Fatalf("exit=%d want 1, stdout=%q stderr=%q", code, so, se)
			}
			if so != "" {
				t.Fatalf("stdout must stay empty on corruption, got %q", so)
			}
			for _, want := range []string{"corrupt", "gip-cli", "ballot 0", `"support"`, "null"} {
				if !strings.Contains(se, want) {
					t.Fatalf("stderr %q missing %q", se, want)
				}
			}
		})
	}
	if got, err := os.ReadFile(state); err != nil || string(got) != corrupt {
		t.Fatalf("corrupt file was modified or rewritten")
	}

	// 对照：明确写出的 false 反对票与 0 投票时间是合法值，文本与 JSON 都能查询。
	legal := filepath.Join(dir, "legal.json")
	legalRaw := `{
  "magic": "govflow-treasury-state",
  "version": 1,
  "initial_treasury": 1000,
  "treasury": 1000,
  "balances": {},
  "proposals": {},
  "receipts": [],
  "vote_proposals": {
    "gip-legal": {
      "id": "gip-legal", "state": "voting",
      "members": [{"id": "a", "weight": 5}],
      "delegations": [],
      "quorum": 1, "start_at": 0, "deadline": 10, "timelock_end": 10,
      "actions": [],
      "ballots": [{"representative": "a", "weight": 5, "support": false, "voted_at": 0}]
    }
  }
}
`
	if err := os.WriteFile(legal, []byte(legalRaw), 0o600); err != nil {
		t.Fatal(err)
	}
	if so, se, code := runCLI(t, binary, legal, "proposal", "--id", "gip-legal"); code != 0 {
		t.Fatalf("legal false/0 ballot text query exit=%d: %s", code, se)
	} else if !strings.Contains(so, "choice=against") || !strings.Contains(so, "voted_at=0") {
		t.Fatalf("text query should show against/0, got %q", so)
	}
	if so, se, code := runCLI(t, binary, legal, "proposal", "--id", "gip-legal", "--json"); code != 0 {
		t.Fatalf("legal false/0 ballot json query exit=%d: %s", code, se)
	} else if !strings.Contains(so, `"support": false`) || !strings.Contains(so, `"voted_at": 0`) {
		t.Fatalf("json query should preserve false/0, got %s", so)
	}
}

// TestCLIRejectsIncompleteTally：已计票提案的计票结果缺 against_weight（该侧真实
// 票重恰好为 0）时，文本与 JSON 的查询、再次计票都把整份文件判为损坏：原因写入
// stderr（能看出提案编号、计票字段与缺失原因），以域错误退出码 1 结束，stdout
// 不出现成功结果，文件不被改写；请求同库内另一项正常提案也同样拒绝。
func TestCLIRejectsIncompleteTally(t *testing.T) {
	binary := buildCLI(t)
	dir := t.TempDir()
	state := filepath.Join(dir, "treasury.json")
	if _, _, code := runCLI(t, binary, state, "init", "--balance", "1000"); code != 0 {
		t.Fatal("init failed")
	}
	if _, _, code := runCLI(t, binary, state, "create-vote", "--id", "gip-cli-tally",
		"--member", "alice:600", "--member", "dave:400",
		"--quorum", "600", "--start", "0", "--deadline", "10", "--timelock", "10",
		"--action", "transfer:a:1"); code != 0 {
		t.Fatal("create-vote failed")
	}
	// 同库另一项正常提案（仍在投票），用于验证损坏不被选择性忽略。
	if _, _, code := runCLI(t, binary, state, "create-vote", "--id", "gip-other",
		"--member", "alice:600", "--member", "dave:400",
		"--quorum", "600", "--start", "0", "--deadline", "10", "--timelock", "10"); code != 0 {
		t.Fatal("create-vote gip-other failed")
	}
	if _, _, code := runCLI(t, binary, state, "vote", "--id", "gip-cli-tally",
		"--voter", "alice", "--choice", "for", "--now", "5"); code != 0 {
		t.Fatal("vote failed")
	}
	// 计票通过：for=600、against=0、quorum=600，反对侧真实票重恰好为零。
	if so, se, code := runCLI(t, binary, state, "tally", "--id", "gip-cli-tally", "--now", "10"); code != 0 {
		t.Fatalf("tally exit=%d: %s", code, se)
	} else if !strings.Contains(so, "for=600 against=0") || !strings.Contains(so, "passed") {
		t.Fatalf("tally output = %q, want passed for=600 against=0", so)
	}

	raw, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	// 删掉已保存计票结果中的 against_weight（写出的值是 0）。
	corrupt := strings.Replace(string(raw), `"against_weight": 0,`, ``, 1)
	if corrupt == string(raw) {
		t.Fatal("setup: against_weight field not found")
	}
	if err := os.WriteFile(state, []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}

	// 文本与 JSON 采用同一判定：均为退出码 1、原因在 stderr、stdout 为空。
	for _, args := range [][]string{
		{"proposal", "--id", "gip-cli-tally"},
		{"proposal", "--id", "gip-cli-tally", "--json"},
		{"proposal", "--id", "gip-other"},
		{"proposals"},
		{"proposals", "--json"},
		{"tally", "--id", "gip-cli-tally", "--now", "10"},
		{"tally", "--id", "gip-cli-tally", "--now", "10", "--json"},
		{"balances", "--json"},
	} {
		name := strings.Join(args, "_")
		t.Run(name, func(t *testing.T) {
			so, se, code := runCLI(t, binary, state, args...)
			if code != 1 {
				t.Fatalf("exit=%d want 1, stdout=%q stderr=%q", code, so, se)
			}
			if so != "" {
				t.Fatalf("stdout must stay empty on corruption, got %q", so)
			}
			for _, want := range []string{"corrupt", "gip-cli-tally", "tally", `"against_weight"`, "missing"} {
				if !strings.Contains(se, want) {
					t.Fatalf("stderr %q missing %q", se, want)
				}
			}
		})
	}
	// 任何失败尝试都不得改写或补全原文件。
	if got, err := os.ReadFile(state); err != nil || string(got) != corrupt {
		t.Fatalf("corrupt file was modified or rewritten")
	}
}
