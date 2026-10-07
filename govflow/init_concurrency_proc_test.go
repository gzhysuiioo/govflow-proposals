package govflow

// 跨进程（多个独立 CLI 进程）的首次初始化并发回归保障：
// 恰好一个进程初始化成功，其余进程以域错误退出并明确告知“资金库已经初始化”，
// 最终余额查询读到的必须是成功进程提交的初始余额。初始化期间其他进程反复
// 查询时，只能看到“文件不存在”或一份完整初始状态，绝不允许看到损坏/空文件。
//
// 同进程 goroutine 场景见 init_concurrency_regression_test.go。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// runInitCLI 直接启动一个 init 子进程（不经 runCLI 的 args 重组），返回
// stdout、stderr 与退出码。
func runInitCLI(t *testing.T, binary, state string, balance int64, asJSON bool) (string, string, int) {
	t.Helper()
	args := []string{"init", "--balance", strconv.FormatInt(balance, 10)}
	if asJSON {
		args = append(args, "--json")
	}
	return runCLI(t, binary, state, args...)
}

// initCLIResult 记录一个并发 init 进程的结果与其提交的余额。
type initCLIResult struct {
	balance int64
	stdout  string
	stderr  string
	code    int
}

// runConcurrentInitProcesses 让 n 个独立进程在同一起跑信号后同时初始化
// state，返回每个进程的结果。
func runConcurrentInitProcesses(t *testing.T, binary, state string, balances []int64) []initCLIResult {
	t.Helper()
	n := len(balances)
	results := make([]initCLIResult, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			so, se, code := runInitCLI(t, binary, state, balances[i], true)
			results[i] = initCLIResult{balance: balances[i], stdout: so, stderr: se, code: code}
		}(i)
	}
	close(start)
	wg.Wait()
	return results
}

// assertCLIInitialStateComplete 通过 CLI 校验 state 是一份完整的刚初始化
// 状态：资金库余额为 treasury，收款账户、登记提案/投票提案、执行凭据全空。
func assertCLIInitialStateComplete(t *testing.T, binary, state string, treasury int64, where string) {
	t.Helper()
	so, se, code := runCLI(t, binary, state, "balances", "--json")
	if code != 0 {
		t.Fatalf("%s: balances exit=%d stderr=%s", where, code, se)
	}
	var balances struct {
		Treasury int64            `json:"treasury"`
		Balances map[string]int64 `json:"balances"`
	}
	if err := json.Unmarshal([]byte(so), &balances); err != nil {
		t.Fatalf("%s: balances output not JSON: %v\n%s", where, err, so)
	}
	if balances.Treasury != treasury {
		t.Fatalf("%s: treasury=%d, want winning balance %d", where, balances.Treasury, treasury)
	}
	if len(balances.Balances) != 0 {
		t.Fatalf("%s: recipient accounts=%v, want empty", where, balances.Balances)
	}

	po, pse, pcode := runCLI(t, binary, state, "proposals", "--json")
	if pcode != 0 || strings.TrimSpace(po) != "[]" {
		t.Fatalf("%s: proposals exit=%d output=%q stderr=%s, want empty []", where, pcode, po, pse)
	}
	ro, rse, rcode := runCLI(t, binary, state, "receipts", "--json")
	if rcode != 0 || strings.TrimSpace(ro) != "[]" {
		t.Fatalf("%s: receipts exit=%d output=%q stderr=%s, want empty []", where, rcode, ro, rse)
	}
}

// TestCrossProcessConcurrentInitExactlyOneWinner：多个独立进程同时初始化同
// 一份尚不存在的资金库时，无论提交的是互不相同的余额、全部为零还是完全
// 相同的余额，都必须恰好一个进程成功：成功进程退出码为 0 且 JSON 响应
// initialized=true、treasury 为其提交值；其余进程退出码为 1 并明确报告
// 资金库已初始化。最终余额查询必须等于成功进程的值，失败进程的余额既不
// 能覆盖也不能合并；目录不遗留空状态文件或临时文件。
func TestCrossProcessConcurrentInitExactlyOneWinner(t *testing.T) {
	binary := buildCLI(t)

	for _, sc := range concurrentInitScenarios(16) {
		t.Run(sc.name, func(t *testing.T) {
			var sum int64
			for _, b := range sc.balances {
				sum += b
			}
			for round := 0; round < 3; round++ {
				dir := t.TempDir()
				state := filepath.Join(dir, "treasury.json")
				results := runConcurrentInitProcesses(t, binary, state, sc.balances)

				winners := 0
				var winnerBalance int64
				for i, r := range results {
					switch {
					case r.code == 0:
						var resp struct {
							State       string `json:"state"`
							Treasury    int64  `json:"treasury"`
							Initialized bool   `json:"initialized"`
						}
						if err := json.Unmarshal([]byte(r.stdout), &resp); err != nil {
							t.Fatalf("round %d winner %d output not JSON: %v\n%s", round, i, err, r.stdout)
						}
						if !resp.Initialized || resp.Treasury != r.balance {
							t.Fatalf("round %d winner %d response=%+v, want initialized with submitted balance %d",
								round, i, resp, r.balance)
						}
						winners++
						winnerBalance = r.balance
					case r.code == 1:
						if !strings.Contains(r.stderr, "state file already exists") {
							t.Fatalf("round %d loser %d stderr=%q, want already-initialized rejection",
								round, i, r.stderr)
						}
						if strings.Contains(r.stderr, "corrupt") {
							t.Fatalf("round %d loser %d reported corrupt instead of already-init: %s",
								round, i, r.stderr)
						}
						if r.stdout != "" {
							t.Fatalf("round %d loser %d wrote stdout=%q", round, i, r.stdout)
						}
					default:
						t.Fatalf("round %d init process %d exited %d: stderr=%s", round, i, r.code, r.stderr)
					}
				}
				if winners != 1 {
					t.Fatalf("round %d: %d init processes succeeded, want exactly 1", round, winners)
				}

				// 最终查询读到的余额等于成功进程提交值本身；三张记录表全空。
				assertCLIInitialStateComplete(t, binary, state, winnerBalance, "final query")
				assertPersistedInitialBalance(t, state, winnerBalance)
				if sc.name == "distinct balances" && winnerBalance == sum {
					t.Fatalf("persisted treasury equals sum %d of submitted amounts, must not merge", sum)
				}
				assertNoInitLeftovers(t, dir, state)
			}
		})
	}
}

// TestCrossProcessReadsDuringInitSeeAbsentOrComplete：多个进程并发初始化期
// 间，另一批进程持续查询余额与提案、凭据。查询进程只能遇到两种结果：
//   - 状态文件尚未出现：退出码 1，stderr 明确是“没有该文件”，而不是损坏；
//   - 状态文件已成功创建：退出码 0，得到完整初始状态，余额与最终胜者一致，
//     收款账户、提案与凭据为空。
//
// 任何“损坏/空文件/半成品”响应都视为初始化窗口处理失败。全部初始化完成
// 后，正常查询仍读到同一份完整状态，不遗留空文件。
func TestCrossProcessReadsDuringInitSeeAbsentOrComplete(t *testing.T) {
	binary := buildCLI(t)
	sc := concurrentInitScenarios(16)[0] // 互不相同的余额，便于识别胜者

	for round := 0; round < 3; round++ {
		dir := t.TempDir()
		state := filepath.Join(dir, "treasury.json")

		// 确定性前提：文件不存在时查询必须明确报不存在（exit 1），而不是损坏。
		if _, se, code := runCLI(t, binary, state, "balances", "--json"); code != 1 ||
			!strings.Contains(se, "no such file") || strings.Contains(se, "corrupt") {
			t.Fatalf("round %d: missing-file query code=%d stderr=%q", round, code, se)
		}

		start := make(chan struct{})
		stop := make(chan struct{})
		var iwg, rwg sync.WaitGroup
		initResults := make([]initCLIResult, len(sc.balances))
		for i := 0; i < len(sc.balances); i++ {
			iwg.Add(1)
			go func(i int) {
				defer iwg.Done()
				<-start
				so, se, code := runInitCLI(t, binary, state, sc.balances[i], true)
				initResults[i] = initCLIResult{balance: sc.balances[i], stdout: so, stderr: se, code: code}
			}(i)
		}

		var mu sync.Mutex
		var seenTreasureies []int64
		var sawMissing int
		var badReads []string
		reader := func(id int) {
			defer rwg.Done()
			<-start
			for {
				select {
				case <-stop:
					return
				default:
				}
				so, se, code := runCLI(t, binary, state, "balances", "--json")
				switch code {
				case 0:
					var balances struct {
						Treasury int64            `json:"treasury"`
						Balances map[string]int64 `json:"balances"`
					}
					if err := json.Unmarshal([]byte(so), &balances); err != nil {
						mu.Lock()
						badReads = append(badReads, "balances output not JSON: "+so)
						mu.Unlock()
						continue
					}
					if len(balances.Balances) != 0 {
						mu.Lock()
						badReads = append(badReads, "partial state had recipient accounts: "+so)
						mu.Unlock()
						continue
					}
					mu.Lock()
					seenTreasureies = append(seenTreasureies, balances.Treasury)
					mu.Unlock()
				case 1:
					if strings.Contains(se, "corrupt") || strings.Contains(se, "empty") {
						mu.Lock()
						badReads = append(badReads, "reader hit corrupt/empty state: "+se)
						mu.Unlock()
						continue
					}
					if !strings.Contains(se, "no such file") {
						mu.Lock()
						badReads = append(badReads, "reader unexpected domain error: "+se)
						mu.Unlock()
						continue
					}
					mu.Lock()
					sawMissing++
					mu.Unlock()
				default:
					mu.Lock()
					badReads = append(badReads, "reader exited "+strconv.Itoa(code)+": "+se)
					mu.Unlock()
				}
			}
		}
		for r := 0; r < 8; r++ {
			rwg.Add(1)
			go reader(r)
		}

		close(start)
		iwg.Wait()
		winners := 0
		var winnerBalance int64
		for i, r := range initResults {
			if r.code == 0 {
				winners++
				winnerBalance = r.balance
			} else if r.code != 1 || !strings.Contains(r.stderr, "state file already exists") {
				t.Fatalf("round %d loser %d code=%d stderr=%s", round, i, r.code, r.stderr)
			}
		}
		if winners != 1 {
			close(stop)
			rwg.Wait()
			t.Fatalf("round %d: %d init processes succeeded, want exactly 1", round, winners)
		}

		// 初始化全部完成后再停止读取方，覆盖“期间”与“完成后”两个阶段。
		close(stop)
		rwg.Wait()

		if len(badReads) != 0 {
			t.Fatalf("round %d: readers observed %d corrupt/partial states:\n%s",
				round, len(badReads), strings.Join(badReads, "\n"))
		}
		if len(seenTreasureies) == 0 {
			t.Fatalf("round %d: no reader observed an initialized state", round)
		}
		for _, got := range seenTreasureies {
			if got != winnerBalance {
				t.Fatalf("round %d: reader treasury=%d, want winning balance %d", round, got, winnerBalance)
			}
		}
		t.Logf("round %d: %d reader calls saw a missing file, %d saw the complete state",
			round, sawMissing, len(seenTreasureies))

		assertCLIInitialStateComplete(t, binary, state, winnerBalance, "post-init query")
		assertPersistedInitialBalance(t, state, winnerBalance)
		assertNoInitLeftovers(t, dir, state)

		// 成功创建后再发起的初始化仍是域拒绝，且文件内容、余额、提案、凭据不变。
		rawBefore, err := os.ReadFile(state)
		if err != nil {
			t.Fatal(err)
		}
		if _, se, code := runInitCLI(t, binary, state, winnerBalance, false); code != 1 ||
			!strings.Contains(se, "state file already exists") {
			t.Fatalf("post-init re-init code=%d stderr=%s", code, se)
		}
		rawAfter, err := os.ReadFile(state)
		if err != nil {
			t.Fatal(err)
		}
		if string(rawBefore) != string(rawAfter) {
			t.Fatalf("state file changed after refused re-initialization")
		}
	}
}
