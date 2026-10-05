package govflow

import (
	"bytes"
	"encoding/json"
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

// TestCLIRejectsMissingRegisteredTimelock：register 登记的提案缺失 timelock_end
// 时，文本与 JSON 的查询、执行都把整份文件判为损坏：原因写入 stderr（能看出
// 提案编号、timelock_end 字段与缺失/空值/类型不符），以域错误退出码 1 结束，
// stdout 不出现成功结果或部分查询结果，文件不被改写；即使查询的是另一项完整
// 提案也同样拒绝。缺失的时间锁不得被当作 0：now=0 执行也不能转账。
func TestCLIRejectsMissingRegisteredTimelock(t *testing.T) {
	binary := buildCLI(t)
	dir := t.TempDir()
	state := filepath.Join(dir, "treasury.json")
	if _, _, code := runCLI(t, binary, state, "init", "--balance", "1000"); code != 0 {
		t.Fatal("init failed")
	}
	if _, _, code := runCLI(t, binary, state, "register", "--id", "gip-1",
		"--timelock", "5000", "--action", "transfer:a:100"); code != 0 {
		t.Fatal("register gip-1 failed")
	}
	if _, _, code := runCLI(t, binary, state, "register", "--id", "gip-other",
		"--timelock", "0", "--action", "transfer:o:1"); code != 0 {
		t.Fatal("register gip-other failed")
	}
	raw, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := removeLineContaining(string(raw), `"timelock_end": 5000`)
	if corrupt == string(raw) {
		t.Fatal("setup: timelock_end field not found")
	}
	if err := os.WriteFile(state, []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}

	// 文本与 JSON 模式采用同一判定：均为退出码 1、原因在 stderr、stdout 为空。
	for _, args := range [][]string{
		{"proposal", "--id", "gip-1"},
		{"proposal", "--id", "gip-1", "--json"},
		{"proposal", "--id", "gip-other"},
		{"proposal", "--id", "gip-other", "--json"},
		{"proposals"},
		{"proposals", "--json"},
		{"execute", "--id", "gip-1", "--now", "0"},
		{"execute", "--id", "gip-1", "--now", "0", "--json"},
		{"balances"},
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
			for _, want := range []string{"corrupt", "gip-1", "timelock_end", "missing"} {
				if !strings.Contains(se, want) {
					t.Fatalf("stderr %q missing %q", se, want)
				}
			}
		})
	}
	// 执行不得扣款、追加凭据或改变提案状态：状态文件内容保持原样。
	if got, err := os.ReadFile(state); err != nil || string(got) != corrupt {
		t.Fatalf("corrupt file was modified or rewritten")
	}
}

// TestCLIRejectsIncompleteDelegations：投票提案的 delegations 字段缺损（缺失或
// null）时，文本与 JSON 的查询、投票、计票、执行都把整份文件判为损坏：原因写入
// stderr（能看出提案编号、delegations 字段与缺失/空值原因），以域错误退出码 1
// 结束，stdout 不出现成功记录或部分结果，文件不改写；请求同库内另一项委托记录
// 完整的提案也同样拒绝。缺少委托记录不等于撤回委托。
func TestCLIRejectsIncompleteDelegations(t *testing.T) {
	binary := buildCLI(t)
	dir := t.TempDir()
	state := filepath.Join(dir, "treasury.json")
	if _, _, code := runCLI(t, binary, state, "init", "--balance", "1000"); code != 0 {
		t.Fatal("init failed")
	}
	// alice 权重 300、bob 权重 200，bob 已委托 alice，两人尚未投票。
	if _, _, code := runCLI(t, binary, state, "create-vote", "--id", "gip-cli-del",
		"--member", "alice:300", "--member", "bob:200", "--delegate", "bob:alice",
		"--quorum", "300", "--start", "0", "--deadline", "10", "--timelock", "10",
		"--action", "transfer:a:1"); code != 0 {
		t.Fatal("create-vote failed")
	}
	// 同库另一项委托记录完整的提案，用于验证损坏不被选择性忽略。
	if _, _, code := runCLI(t, binary, state, "create-vote", "--id", "gip-other",
		"--member", "alice:300", "--member", "bob:200",
		"--quorum", "300", "--start", "0", "--deadline", "10", "--timelock", "10"); code != 0 {
		t.Fatal("create-vote gip-other failed")
	}
	raw, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	delete(doc["vote_proposals"].(map[string]any)["gip-cli-del"].(map[string]any), "delegations")
	corruptRaw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	corrupt := string(append(corruptRaw, '\n'))
	if err := os.WriteFile(state, []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}

	// 文本与 JSON 采用同一判定：均为退出码 1、原因在 stderr、stdout 为空。
	for _, args := range [][]string{
		{"proposal", "--id", "gip-cli-del"},
		{"proposal", "--id", "gip-cli-del", "--json"},
		{"proposal", "--id", "gip-other"},
		{"proposals"},
		{"proposals", "--json"},
		{"vote", "--id", "gip-cli-del", "--voter", "bob", "--choice", "for", "--now", "5"},
		{"tally", "--id", "gip-cli-del", "--now", "10"},
		{"tally", "--id", "gip-cli-del", "--now", "10", "--json"},
		{"execute", "--id", "gip-cli-del", "--now", "10"},
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
			for _, want := range []string{"corrupt", "gip-cli-del", `"delegations"`, "missing"} {
				if !strings.Contains(se, want) {
					t.Fatalf("stderr %q missing %q", se, want)
				}
			}
		})
	}
	if got, err := os.ReadFile(state); err != nil || string(got) != corrupt {
		t.Fatalf("corrupt file was modified or rewritten")
	}

	// null 与缺失同属缺损：同样判损坏，原因指出空值。
	nullDoc := doc
	nullDoc["vote_proposals"].(map[string]any)["gip-cli-del"].(map[string]any)["delegations"] = nil
	nullRaw, err := json.MarshalIndent(nullDoc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state, append(nullRaw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	so, se, code := runCLI(t, binary, state, "proposals", "--json")
	if code != 1 || so != "" {
		t.Fatalf("null delegations: exit=%d want 1, stdout=%q", code, so)
	}
	if !strings.Contains(se, `"delegations"`) || !strings.Contains(se, "null") {
		t.Fatalf("stderr %q must name delegations and null", se)
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

// TestCLIRejectsIncompleteReceiptOrderIndex：第一份凭据删掉 order、或第一项动作
// 删掉 index 时，即使资金变动与动作原文完全一致，所有读取命令也把整份状态判为
// 损坏：原因写入 stderr（含提案编号、字段与缺失原因；index 还含动作位置），以
// 域错误退出码 1 结束，stdout 不出现任何提案的部分查询结果，文件不被改写。
func TestCLIRejectsIncompleteReceiptOrderIndex(t *testing.T) {
	binary := buildCLI(t)
	dir := t.TempDir()
	state := filepath.Join(dir, "treasury.json")
	if _, _, code := runCLI(t, binary, state, "init", "--balance", "1000"); code != 0 {
		t.Fatal("init failed")
	}
	// 同一收款账户连续两项动作：缺损修复不得合并或重排记录。
	if _, _, code := runCLI(t, binary, state, "register", "--id", "gip-1",
		"--timelock", "0",
		"--action", "transfer:acct:100", "--action", "transfer:acct:50"); code != 0 {
		t.Fatal("register failed")
	}
	if so, _, code := runCLI(t, binary, state, "execute", "--id", "gip-1", "--now", "0", "--json"); code != 0 ||
		!strings.Contains(so, `"order": 0`) || !strings.Contains(so, `"index": 0`) ||
		!strings.Contains(so, `"index": 1`) {
		t.Fatalf("first execute should emit full order/index, code=%d so=%s", code, so)
	}
	// 正常查询保持不变：文本与 JSON 都输出明确保存的序号。
	if so, _, code := runCLI(t, binary, state, "receipts"); code != 0 ||
		!strings.Contains(so, "order=0") || !strings.Contains(so, "action 0:") ||
		!strings.Contains(so, "action 1:") {
		t.Fatalf("text receipts query changed: %s", so)
	}

	raw, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		edit    func(string) string
		wantMsg []string
	}{
		{
			name:    "order-missing",
			edit:    func(s string) string { return strings.Replace(s, `"order": 0,`, ``, 1) },
			wantMsg: []string{"corrupt", "gip-1", `receipt 0`, `"order"`, "missing"},
		},
		{
			name:    "index-missing",
			edit:    func(s string) string { return strings.Replace(s, `"index": 0,`, ``, 1) },
			wantMsg: []string{"corrupt", "gip-1", "action 0", `"index"`, "missing"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			corrupt := tc.edit(string(raw))
			if corrupt == string(raw) {
				t.Fatalf("setup: target field not found")
			}
			bad := filepath.Join(dir, tc.name+".json")
			if err := os.WriteFile(bad, []byte(corrupt), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{
				{"receipt", "--id", "gip-1"},
				{"receipt", "--id", "gip-1", "--json"},
				{"receipts"},
				{"receipts", "--json"},
				{"balances", "--json"},
				{"proposals", "--json"},
			} {
				so, se, code := runCLI(t, binary, bad, args...)
				if code != 1 {
					t.Fatalf("%v exit=%d want 1, stdout=%q stderr=%q", args, code, so, se)
				}
				if so != "" {
					t.Fatalf("%v stdout must stay empty on corruption, got %q", args, so)
				}
				for _, want := range tc.wantMsg {
					if !strings.Contains(se, want) {
						t.Fatalf("%v stderr %q missing %q", args, se, want)
					}
				}
			}
			// 不整理、补齐或覆盖缺损文件。
			if got, err := os.ReadFile(bad); err != nil || string(got) != corrupt {
				t.Fatalf("corrupt file was modified or rewritten")
			}
		})
	}
}

// createVoteArgs 汇集一次 create-vote 的参数，便于在各用例间只替换委托原文。
type createVoteArgs struct {
	id        string
	members   []string // 每项形如 ID:WEIGHT，ID 自身可含冒号
	delegates []string // 委托原文 FROM:TO，切分点由名单唯一确定
	quorum    string
}

func runCreateVote(t *testing.T, binary, state string, a createVoteArgs) (string, string, int) {
	t.Helper()
	args := []string{"create-vote", "--id", a.id}
	for _, m := range a.members {
		args = append(args, "--member", m)
	}
	for _, d := range a.delegates {
		args = append(args, "--delegate", d)
	}
	args = append(args, "--quorum", a.quorum, "--start", "0", "--deadline", "100", "--timelock", "200",
		"--action", "transfer:audits:10")
	return runCLI(t, binary, state, args...)
}

// TestCLIColonsInMemberIDs：成员编号允许冒号出现在开头（":alice"）或中间
// （"team:bob"）。委托原文 ":alice:team:bob" 必须被理解为 :alice 把权重
// 委托给 team:bob，而不是按第一个冒号误切成空委托人。查询完整保留两个编号
// 与委托路径；team:bob 作为最终代表投出票重 100，:alice 已委托出去不能直投。
func TestCLIColonsInMemberIDs(t *testing.T) {
	binary := buildCLI(t)
	state := filepath.Join(t.TempDir(), "treasury.json")
	if _, _, code := runCLI(t, binary, state, "init", "--balance", "1000"); code != 0 {
		t.Fatal("init failed")
	}
	base := createVoteArgs{
		id:        "gip-colon",
		members:   []string{":alice:70", "team:bob:30"},
		delegates: []string{":alice:team:bob"},
		quorum:    "100",
	}
	if _, se, code := runCreateVote(t, binary, state, base); code != 0 {
		t.Fatalf("create with colon-leading delegator failed, exit=%d: %s", code, se)
	}

	// 文本查询：两个编号逐字保留，委托路径从 :alice 到 team:bob。
	so, se, code := runCLI(t, binary, state, "proposal", "--id", base.id)
	if code != 0 {
		t.Fatalf("text proposal query exit=%d: %s", code, se)
	}
	for _, want := range []string{
		"member :alice weight=70 path=:alice->team:bob",
		"member team:bob weight=30 representative=self",
	} {
		if !strings.Contains(so, want) {
			t.Fatalf("text query missing %q:\n%s", want, so)
		}
	}

	// JSON 查询：编号大小写、冒号位置逐字保留，路径含首尾两个完整编号。
	jo, je, jcode := runCLI(t, binary, state, "proposal", "--id", base.id, "--json")
	if jcode != 0 {
		t.Fatalf("json proposal query exit=%d: %s", jcode, je)
	}
	for _, want := range []string{
		`"id": ":alice"`,
		`"id": "team:bob"`,
		`"path": [
        ":alice",
        "team:bob"
      ]`,
		`"delegate": "team:bob"`,
		`"direct_to": "team:bob"`,
	} {
		if !strings.Contains(jo, want) {
			t.Fatalf("json query missing %q:\n%s", want, jo)
		}
	}

	// 最终代表 team:bob 代表沿途全部 70+30=100 权重投票。
	if vo, ve, vcode := runCLI(t, binary, state, "vote", "--id", base.id,
		"--voter", "team:bob", "--choice", "for", "--now", "50", "--json"); vcode != 0 ||
		!strings.Contains(vo, `"representative": "team:bob"`) || !strings.Contains(vo, `"weight": 100`) {
		t.Fatalf("team:bob vote exit=%d so=%s se=%s", vcode, vo, ve)
	}
	// :alice 已委托出去，直接投票被域错误拒绝。
	if _, ve, vcode := runCLI(t, binary, state, "vote", "--id", base.id,
		"--voter", ":alice", "--choice", "for", "--now", "50"); vcode != 1 ||
		!strings.Contains(ve, "may not vote directly") {
		t.Fatalf(":alice direct vote exit=%d want 1: %s", vcode, ve)
	}
	// 计票：100 赞成达到法定人数 => passed；结论中代表编号完整。
	if to, te, tcode := runCLI(t, binary, state, "tally", "--id", base.id, "--now", "100", "--json"); tcode != 0 ||
		!strings.Contains(to, `"passed": true`) || !strings.Contains(to, `"for_weight": 100`) {
		t.Fatalf("tally exit=%d so=%s se=%s", tcode, to, te)
	}
}

// TestCLIDelegationColonParsing：委托原文在冒号出现在开头、末尾、连续出现时
// 的失败分类与成功识别。判定只依据切分语法与成员名单：
//   - 无冒号，或不存在两端均非空的写法：格式错误，退出码 2；
//   - 有非空写法但没有任何一对都在名单：退出码 1；
//   - 两对或更多同时命中（含某种解释为自委托）：歧义，退出码 2 并列出成员对；
//   - 唯一确定成员对之后，自委托、循环仍按业务规则拒绝（退出码 1）。
func TestCLIDelegationColonParsing(t *testing.T) {
	binary := buildCLI(t)
	state := filepath.Join(t.TempDir(), "treasury.json")
	if _, _, code := runCLI(t, binary, state, "init", "--balance", "1000"); code != 0 {
		t.Fatal("init failed")
	}

	cases := []struct {
		name      string
		id        string
		members   []string
		delegates []string
		wantCode  int
		wantMsgs  []string // stderr 必须包含的片段
	}{
		{
			name:      "ambiguous two roster pairs",
			id:        "gip-amb",
			members:   []string{"a:10", "a:b:10", "b:c:10", "c:10"},
			delegates: []string{"a:b:c"}, // (a)->(b:c) 与 (a:b)->(c) 同时命中
			wantCode:  2,
			wantMsgs:  []string{"ambiguous", `"a:b:c"`, `"a" delegating to "b:c"`, `"a:b" delegating to "c"`},
		},
		{
			name:      "ambiguous even though one reading is self-delegation",
			id:        "gip-amb-self",
			members:   []string{"a:10", "a:b:10", "b:a:b:10"},
			delegates: []string{"a:b:a:b"}, // (a)->(b:a:b) 与 (a:b)->(a:b)（自委托）同时命中
			wantCode:  2,
			wantMsgs:  []string{"ambiguous", `"a" delegating to "b:a:b"`, `"a:b" delegating to "a:b"`},
		},
		{
			name:      "no colon at all is a format error",
			id:        "gip-nocolon",
			members:   []string{"a:10"},
			delegates: []string{"nocolon"},
			wantCode:  2,
			wantMsgs:  []string{"malformed", `"nocolon"`},
		},
		{
			name:      "only a leading colon has no non-empty split",
			id:        "gip-leading",
			members:   []string{"alice:10"}, // 注意名单里没有 ":alice"
			delegates: []string{":alice"},
			wantCode:  2,
			wantMsgs:  []string{"malformed", `":alice"`},
		},
		{
			name:      "trailing colon still has a non-empty split but no roster match",
			id:        "gip-trailing",
			members:   []string{"a:10", "b:10"},
			delegates: []string{"a:b:"}, // (a)->(b:) 非空但不命中；结尾切分空 to 不算写法
			wantCode:  1,
			wantMsgs:  []string{"does not match any pair of members", `"a:b:"`},
		},
		{
			name:      "well-formed split matching nobody is a domain error",
			id:        "gip-nomatch",
			members:   []string{"a:10"},
			delegates: []string{"zzz:yyy"},
			wantCode:  1,
			wantMsgs:  []string{"does not match any pair of members", `"zzz:yyy"`},
		},
		{
			name:      "unique self-delegation stays rejected after identification",
			id:        "gip-self",
			members:   []string{"a:b:10"},
			delegates: []string{"a:b:a:b"}, // 唯一命中 (a:b)->(a:b)
			wantCode:  1,
			wantMsgs:  []string{"must not delegate to itself"},
		},
		{
			name:      "unique cycle stays rejected after identification",
			id:        "gip-cycle",
			members:   []string{"a:b:10", "c:a:10"},
			delegates: []string{"a:b:c:a", "c:a:a:b"}, // (a:b)->(c:a)->(a:b) 成环
			wantCode:  1,
			wantMsgs:  []string{"delegation cycle"},
		},
		{
			name:      "case variant must not match a colon-leading member",
			id:        "gip-case",
			members:   []string{":Alice:70", "team:bob:30"},
			delegates: []string{":alice:team:bob"},
			wantCode:  1,
			wantMsgs:  []string{"does not match any pair of members"},
		},
		{
			name:      "deleted character must not match",
			id:        "gip-delchar",
			members:   []string{":alice:70", "team:bob:30"},
			delegates: []string{"alic:team:bob"},
			wantCode:  1,
			wantMsgs:  []string{"does not match any pair of members"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := createVoteArgs{id: tc.id, members: tc.members, delegates: tc.delegates, quorum: "1"}
			_, se, code := runCreateVote(t, binary, state, a)
			if code != tc.wantCode {
				t.Fatalf("exit=%d want %d, stderr=%s", code, tc.wantCode, se)
			}
			for _, want := range tc.wantMsgs {
				if !strings.Contains(se, want) {
					t.Fatalf("stderr %q missing %q", se, want)
				}
			}
		})
	}

	// 连续冒号：编号 "a:"（冒号结尾）与 ":b"（冒号开头）经原文 "a:::b"
	// 唯一切成 (a:)->(:b)，委托创建成功且查询逐字保留两个编号。
	ok := createVoteArgs{
		id:        "gip-dcolon",
		members:   []string{"a::10", ":b:10"},
		delegates: []string{"a:::b"},
		quorum:    "1",
	}
	if _, se, code := runCreateVote(t, binary, state, ok); code != 0 {
		t.Fatalf("consecutive-colon delegation exit=%d: %s", code, se)
	}
	if so, _, code := runCLI(t, binary, state, "proposal", "--id", ok.id); code != 0 ||
		!strings.Contains(so, "member a: weight=10 path=a:->:b") ||
		!strings.Contains(so, "member :b weight=10 representative=self") {
		t.Fatalf("consecutive-colon query exit=%d:\n%s", code, so)
	}

	// 上面所有失败创建都不得留下提案，也不得改变资金库余额。
	so, _, code := runCLI(t, binary, state, "proposals", "--json")
	if code != 0 {
		t.Fatalf("proposals query failed")
	}
	for _, rejected := range []string{"gip-amb", "gip-amb-self", "gip-nocolon", "gip-leading",
		"gip-trailing", "gip-nomatch", "gip-self", "gip-cycle", "gip-case", "gip-delchar"} {
		if strings.Contains(so, rejected) {
			t.Fatalf("rejected proposal %q must not be persisted:\n%s", rejected, so)
		}
	}
	if bo, _, code := runCLI(t, binary, state, "balances", "--json"); code != 0 ||
		!strings.Contains(bo, `"treasury": 1000`) {
		t.Fatalf("treasury changed after failed creates:\n%s", bo)
	}
}

// TestCLIDelegationParamOrderIrrelevant：成员与委托参数的排列先后（包括
// --delegate 整体出现在 --member 之前、成员与委托各自换序）不改变同一项
// 委托的含义，相同内容重试幂等返回且不改动状态。
func TestCLIDelegationParamOrderIrrelevant(t *testing.T) {
	binary := buildCLI(t)
	state := filepath.Join(t.TempDir(), "treasury.json")
	if _, _, code := runCLI(t, binary, state, "init", "--balance", "1000"); code != 0 {
		t.Fatal("init failed")
	}
	first := []string{"create-vote", "--id", "gip-order",
		"--member", ":alice:70", "--member", "team:bob:30",
		"--delegate", ":alice:team:bob",
		"--quorum", "100", "--start", "0", "--deadline", "100", "--timelock", "200"}
	if _, se, code := runCLI(t, binary, state, first...); code != 0 {
		t.Fatalf("first create exit=%d: %s", code, se)
	}
	// 成员换序、委托标记出现在成员之前，内容仍逐项相同。
	second := []string{"create-vote", "--id", "gip-order",
		"--delegate", ":alice:team:bob",
		"--member", "team:bob:30", "--member", ":alice:70",
		"--quorum", "100", "--start", "0", "--deadline", "100", "--timelock", "200"}
	if so, se, code := runCLI(t, binary, state, second...); code != 0 ||
		!strings.Contains(so, "already exists") {
		t.Fatalf("reordered identical retry should be idempotent, exit=%d so=%s se=%s", code, so, se)
	}
	// 普通不含冒号的编号继续按原有方式工作，且冒号编号与普通编号可混用。
	mixed := []string{"create-vote", "--id", "gip-mixed",
		"--member", "plain:40", "--member", ":alice:30", "--member", "team:bob:30",
		"--delegate", ":alice:team:bob", "--delegate", "plain:team:bob",
		"--quorum", "100", "--start", "0", "--deadline", "100", "--timelock", "200"}
	if _, se, code := runCLI(t, binary, state, mixed...); code != 0 {
		t.Fatalf("mixed plain/colon members create exit=%d: %s", code, se)
	}
	if vo, _, code := runCLI(t, binary, state, "vote", "--id", "gip-mixed",
		"--voter", "team:bob", "--choice", "for", "--now", "50", "--json"); code != 0 ||
		!strings.Contains(vo, `"weight": 100`) {
		t.Fatalf("team:bob should carry all 100 weight: exit=%d so=%s", code, vo)
	}
}

// TestCLIRegisterReportsAcknowledgedState：register 的成功响应必须反映这次登记
// 实际认可的记录——首次登记 already_registered=false、state=passed；相同内容
// 重试 already_registered=true 且保留实际状态：未执行为 passed，已执行为
// executed，不得把已执行提案报成 passed。编号、时间锁、动作原文及顺序与认可
// 的记录一致；已执行提案的内容冲突仍然失败。
func TestCLIRegisterReportsAcknowledgedState(t *testing.T) {
	binary := buildCLI(t)
	state := filepath.Join(t.TempDir(), "treasury.json")
	if _, _, code := runCLI(t, binary, state, "init", "--balance", "1000"); code != 0 {
		t.Fatal("init failed")
	}
	register := []string{"register", "--id", "gip-1", "--timelock", "100",
		"--action", "transfer:audits:250", "--action", "transfer:legal:5", "--json"}

	type registerResponse struct {
		ID                string   `json:"id"`
		State             string   `json:"state"`
		TimelockEnd       int64    `json:"timelock_end"`
		Actions           []string `json:"actions"`
		AlreadyRegistered bool     `json:"already_registered"`
	}
	runRegister := func() registerResponse {
		t.Helper()
		so, se, code := runCLI(t, binary, state, register...)
		if code != 0 {
			t.Fatalf("register exit=%d: %s", code, se)
		}
		var resp registerResponse
		if err := json.Unmarshal([]byte(so), &resp); err != nil {
			t.Fatalf("register output is not JSON: %v\n%s", err, so)
		}
		return resp
	}
	wantActions := []string{"transfer:audits:250", "transfer:legal:5"}
	check := func(resp registerResponse, state string, existed bool) {
		t.Helper()
		if resp.ID != "gip-1" || resp.TimelockEnd != 100 || resp.State != state ||
			resp.AlreadyRegistered != existed || strings.Join(resp.Actions, ",") != strings.Join(wantActions, ",") {
			t.Fatalf("register response = %+v, want state=%s already_registered=%v actions=%v",
				resp, state, existed, wantActions)
		}
	}

	// 首次登记：already_registered=false，state=passed。
	check(runRegister(), "passed", false)
	// 尚未执行的相同内容重试：already_registered=true，state=passed。
	check(runRegister(), "passed", true)

	// 执行后相同内容重试：state 必须是 executed，不能报成 passed。
	if _, se, code := runCLI(t, binary, state, "execute", "--id", "gip-1", "--now", "100"); code != 0 {
		t.Fatalf("execute failed: %s", se)
	}
	check(runRegister(), "executed", true)

	// 普通文本输出同样明确实际状态。
	if so, se, code := runCLI(t, binary, state, "register", "--id", "gip-1", "--timelock", "100",
		"--action", "transfer:audits:250", "--action", "transfer:legal:5"); code != 0 ||
		!strings.Contains(so, "already registered") || !strings.Contains(so, "state=executed") {
		t.Fatalf("text retry after execute exit=%d so=%s se=%s", code, so, se)
	}

	// 已执行提案的内容冲突（时间锁、动作原文、动作顺序）仍然失败，不返回成功对象。
	if so, se, code := runCLI(t, binary, state, "register", "--id", "gip-1", "--timelock", "101",
		"--action", "transfer:audits:250", "--action", "transfer:legal:5", "--json"); code != 1 ||
		!strings.Contains(se, "different timelock or actions") || strings.Contains(so, "already_registered") {
		t.Fatalf("timelock conflict after execute exit=%d so=%s se=%s", code, so, se)
	}
	if _, se, code := runCLI(t, binary, state, "register", "--id", "gip-1", "--timelock", "100",
		"--action", "transfer:legal:5", "--action", "transfer:audits:250"); code != 1 ||
		!strings.Contains(se, "different timelock or actions") {
		t.Fatalf("action order conflict after execute exit=%d: %s", code, se)
	}

	// 重试只确认已有提案：余额与首次执行凭据不被改写。
	if bo, _, code := runCLI(t, binary, state, "balances", "--json"); code != 0 ||
		!strings.Contains(bo, `"treasury": 745`) {
		t.Fatalf("treasury changed by register retry:\n%s", bo)
	}
	if ro, _, code := runCLI(t, binary, state, "receipt", "--id", "gip-1", "--json"); code != 0 ||
		!strings.Contains(ro, `"executed_at": 100`) {
		t.Fatalf("first execution receipt changed:\n%s", ro)
	}
}
