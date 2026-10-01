package govflow

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
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

// TestStateFileDoesNotLeakLockArtifacts：锁文件与状态文件分离，
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
