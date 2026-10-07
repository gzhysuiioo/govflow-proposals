package govflow

// 首次初始化的并发回归保障：多个使用者同时初始化同一份本地资金库时，
// 无论请求来自同一进程的不同句柄还是不同进程，都必须恰好一次成功；其余请求
// 得到明确的“资金库已经初始化”拒绝。落盘余额必须等于成功请求提交的值，
// 失败请求既不能覆盖也不能合并进去。初始化期间并发打开资金库的使用者只能
// 看到两种结果：文件尚不存在（明确不存在），或一份完整可用的初始状态，
// 绝不允许把创建过程中暂时出现的空文件当作损坏状态上报。
//
// 本文件覆盖同进程（goroutine）场景；跨进程 CLI 场景见
// init_concurrency_proc_test.go。

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// concurrentInitScenario 描述一组同时发起的首次初始化请求各自提交的合法余额。
type concurrentInitScenario struct {
	name     string
	balances []int64
}

// concurrentInitScenarios 构造并发初始化的三种请求组合：
//   - 互不相同的余额：任何失败请求的余额一旦覆盖成功值都会被立刻发现；
//   - 全部为零：零是合法的完成状态，不能被误认为“尚未初始化”而接受第二次；
//   - 完全相同的非零余额：相同内容也不是可接受的重试，仍只能一次成功。
func concurrentInitScenarios(n int) []concurrentInitScenario {
	distinct := make([]int64, n)
	for i := range distinct {
		distinct[i] = int64(100 + i)
	}
	zeros := make([]int64, n)
	same := make([]int64, n)
	for i := range same {
		same[i] = 777
	}
	return []concurrentInitScenario{
		{name: "distinct balances", balances: distinct},
		{name: "all zero balances", balances: zeros},
		{name: "identical nonzero balances", balances: same},
	}
}

// assertFreshInitialState 校验一个句柄读出的正是资金库刚初始化完成时的完整
// 初始状态：资金库余额等于 treasury，收款账户、登记提案、投票提案与执行凭据
// 全部为空。初始化成功返回的句柄与重新打开的句柄都必须满足同一形状。
func assertFreshInitialState(t *testing.T, st *Store, treasury int64, where string) {
	t.Helper()
	snap, err := st.BalanceSnapshot()
	if err != nil {
		t.Fatalf("%s: BalanceSnapshot: %v", where, err)
	}
	if snap.Treasury != treasury {
		t.Fatalf("%s: treasury=%d, want winning initial balance %d", where, snap.Treasury, treasury)
	}
	if len(snap.Balances) != 0 {
		t.Fatalf("%s: recipient accounts=%v, want empty", where, snap.Balances)
	}
	proposals, err := st.ProposalsSnapshot()
	if err != nil {
		t.Fatalf("%s: ProposalsSnapshot: %v", where, err)
	}
	if len(proposals.Registered) != 0 || len(proposals.Voting) != 0 {
		t.Fatalf("%s: proposals=%+v, want registered and voting tables empty", where, proposals)
	}
	receipts, err := st.Receipts()
	if err != nil {
		t.Fatalf("%s: Receipts: %v", where, err)
	}
	if len(receipts) != 0 {
		t.Fatalf("%s: receipts=%d, want none", where, len(receipts))
	}
}

// assertPersistedInitialBalance 直接解析状态文件，确认实际保存下来的
// initial_treasury 与 treasury 都是成功请求提交的余额；文件不是空文件。
func assertPersistedInitialBalance(t *testing.T, path string, want int64) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		t.Fatalf("state file is empty after initialization")
	}
	var doc struct {
		InitialTreasury int64 `json:"initial_treasury"`
		Treasury        int64 `json:"treasury"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("state file is not valid JSON: %v", err)
	}
	if doc.InitialTreasury != want || doc.Treasury != want {
		t.Fatalf("persisted balances initial=%d treasury=%d, want both %d",
			doc.InitialTreasury, doc.Treasury, want)
	}
}

// assertNoInitLeftovers 确认初始化没有遗留无法打开的空状态文件或临时文件：
// 目录中只允许出现状态文件本身与其锁文件。
func assertNoInitLeftovers(t *testing.T, dir, state string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	want := map[string]bool{
		filepath.Base(state):              true,
		filepath.Base(state) + lockSuffix: true,
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".govflow-state-") {
			t.Fatalf("temp file %q was left behind in %s", e.Name(), dir)
		}
		if !want[e.Name()] {
			t.Fatalf("unexpected leftover file %q in %s", e.Name(), dir)
		}
	}
	info, err := os.Stat(state)
	if err != nil {
		t.Fatalf("stat state file: %v", err)
	}
	if info.Size() == 0 {
		t.Fatalf("state file left behind empty")
	}
}

// TestConcurrentInitTreasuryExactlyOneWinner：状态文件原本不存在、目录可正常
// 读写时，多个 goroutine 经同一进程路径锁同时发起 InitTreasury，必须恰好
// 一个成功，其余全部得到 ErrTreasuryAlreadyInit；成功请求拿到的句柄与最终
// 文件都是完整初始状态，实际保存的初始余额等于胜者提交值，不被失败请求
// 覆盖，也不是若干请求金额的合并。零余额与相同余额组合遵守同一结论。
func TestConcurrentInitTreasuryExactlyOneWinner(t *testing.T) {
	for _, sc := range concurrentInitScenarios(24) {
		t.Run(sc.name, func(t *testing.T) {
			var sum int64
			for _, b := range sc.balances {
				sum += b
			}
			for round := 0; round < 5; round++ {
				path := filepath.Join(t.TempDir(), "treasury.json")
				n := len(sc.balances)
				start := make(chan struct{})
				type initResult struct {
					store *Store
					err   error
				}
				results := make([]initResult, n)
				var wg sync.WaitGroup
				for i := 0; i < n; i++ {
					wg.Add(1)
					go func(i int) {
						defer wg.Done()
						<-start
						results[i].store, results[i].err = InitTreasury(path, sc.balances[i])
					}(i)
				}
				close(start)
				wg.Wait()

				winners := 0
				var winnerBalance int64
				for i, r := range results {
					if r.err == nil {
						if r.store == nil {
							t.Fatalf("request %d succeeded with nil store", i)
						}
						winners++
						winnerBalance = sc.balances[i]
						// 成功请求自己拿到的句柄必须是完整、可继续使用的初始状态。
						assertFreshInitialState(t, r.store, winnerBalance, "winning handle")
						if err := r.store.Close(); err != nil {
							t.Fatalf("close winning handle: %v", err)
						}
						continue
					}
					if r.store != nil {
						t.Fatalf("rejected request %d returned a non-nil store", i)
					}
					if !errors.Is(r.err, ErrTreasuryAlreadyInit) {
						t.Fatalf("losing request %d err=%v, want ErrTreasuryAlreadyInit", i, r.err)
					}
				}
				if winners != 1 {
					t.Fatalf("round %d: %d init requests succeeded, want exactly 1", round, winners)
				}

				// 最终查询读到的余额必须是胜者提交值本身。
				final, err := Open(path)
				if err != nil {
					t.Fatalf("Open after concurrent init: %v", err)
				}
				assertFreshInitialState(t, final, winnerBalance, "reopened state")
				if err := final.Close(); err != nil {
					t.Fatalf("close reopened state: %v", err)
				}
				assertPersistedInitialBalance(t, path, winnerBalance)
				if sc.name == "distinct balances" {
					// 互不相同的余额下，合并/累加/被失败者覆盖都会偏离这个等式。
					if winnerBalance == sum {
						t.Fatalf("persisted treasury equals sum of requests %d, amounts must not be merged", sum)
					}
				}
				assertNoInitLeftovers(t, filepath.Dir(path), path)
			}
		})
	}
}

// TestConcurrentReadsDuringInitSeeAbsentOrComplete：初始化进行期间，同进程的
// 其他使用者反复打开同一资金库查询。文件尚未出现时必须明确返回不存在
// （fs.ErrNotExist）；一旦打开成功，就只能读到与最终胜者一致的完整初始
// 状态——绝不允许把创建过程中暂时出现的空文件作为 ErrStateCorrupt 报告，
// 也不允许读到半成品。全部初始化结束后，文件仍是同一份完整状态。
func TestConcurrentReadsDuringInitSeeAbsentOrComplete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "treasury.json")

	// 确定性前提：文件不存在时打开必须是明确的“不存在”，而不是损坏或其它错误。
	if _, err := Open(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Open missing file err=%v, want fs.ErrNotExist", err)
	}

	sc := concurrentInitScenarios(16)[0] // 互不相同的余额，便于识别胜者
	n := len(sc.balances)
	start := make(chan struct{})
	stop := make(chan struct{})
	type initResult struct {
		store *Store
		err   error
	}
	results := make([]initResult, n)
	var iwg, rwg sync.WaitGroup
	for i := 0; i < n; i++ {
		iwg.Add(1)
		go func(i int) {
			defer iwg.Done()
			<-start
			results[i].store, results[i].err = InitTreasury(path, sc.balances[i])
		}(i)
	}

	var mu sync.Mutex
	var seenTreasureies []int64
	var badReads []string
	reader := func() {
		defer rwg.Done()
		<-start
		for {
			select {
			case <-stop:
				return
			default:
			}
			st, err := Open(path)
			if err != nil {
				if !errors.Is(err, fs.ErrNotExist) {
					mu.Lock()
					badReads = append(badReads, err.Error())
					mu.Unlock()
				}
				continue
			}
			snap, qerr := st.BalanceSnapshot()
			proposals, perr := st.ProposalsSnapshot()
			receipts, rerr := st.Receipts()
			cerr := st.Close()
			if qerr != nil || perr != nil || rerr != nil || cerr != nil {
				mu.Lock()
				badReads = append(badReads,
					"query on opened store failed: "+joinErrs(qerr, perr, rerr, cerr))
				mu.Unlock()
				continue
			}
			if len(snap.Balances) != 0 || len(proposals.Registered) != 0 ||
				len(proposals.Voting) != 0 || len(receipts) != 0 {
				mu.Lock()
				badReads = append(badReads, "opened store was not a fresh initial state")
				mu.Unlock()
				continue
			}
			mu.Lock()
			seenTreasureies = append(seenTreasureies, snap.Treasury)
			mu.Unlock()
		}
	}
	for r := 0; r < 8; r++ {
		rwg.Add(1)
		go reader()
	}

	close(start)
	iwg.Wait()

	winners := 0
	var winnerBalance int64
	for i, r := range results {
		if r.err == nil {
			winners++
			winnerBalance = sc.balances[i]
			if err := r.store.Close(); err != nil {
				t.Fatalf("close winner %d: %v", i, err)
			}
		} else if !errors.Is(r.err, ErrTreasuryAlreadyInit) {
			t.Fatalf("losing init %d: %v", i, r.err)
		}
	}
	if winners != 1 {
		t.Fatalf("%d init requests succeeded, want exactly 1", winners)
	}

	// 初始化全部结束后再让读取方停止，保证它们覆盖初始化期间与完成之后。
	close(stop)
	rwg.Wait()

	if len(badReads) != 0 {
		t.Fatalf("readers observed %d corrupt/partial states, want only not-exist or complete state:\n%s",
			len(badReads), strings.Join(badReads, "\n"))
	}
	if len(seenTreasureies) == 0 {
		t.Fatalf("no reader ever observed an initialized state")
	}
	for _, got := range seenTreasureies {
		if got != winnerBalance {
			t.Fatalf("reader saw treasury=%d, want winning balance %d", got, winnerBalance)
		}
	}

	final, err := Open(path)
	if err != nil {
		t.Fatalf("final Open: %v", err)
	}
	assertFreshInitialState(t, final, winnerBalance, "final state")
	if err := final.Close(); err != nil {
		t.Fatalf("final Close: %v", err)
	}
	assertPersistedInitialBalance(t, path, winnerBalance)
	assertNoInitLeftovers(t, filepath.Dir(path), path)
}

func joinErrs(errs ...error) string {
	var parts []string
	for _, e := range errs {
		if e != nil {
			parts = append(parts, e.Error())
		}
	}
	return strings.Join(parts, "; ")
}

// TestInitRefusedOverExistingStateEvenWithSameBalance：已有文件继续适用既有
// 拒绝规则——即使再次提交的余额与现有资金库余额完全相同，也不能重新初始化，
// 更不能改动任何余额、提案或执行凭据。文件内容必须逐字节保持原样。
func TestInitRefusedOverExistingStateEvenWithSameBalance(t *testing.T) {
	store, path := openTempStore(t, 1000)
	mustRegister(t, store, "gip-1", 0, "transfer:audits:100")
	receipt0, err := store.Execute("gip-1", 0)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	rawBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 900 与现有 treasury 相同；1000 与初始余额相同；0 是合法初始余额。
	// 无论哪一种，已存在文件上的初始化请求都只能被拒绝。
	for _, balance := range []int64{900, 1000, 0} {
		st, ierr := InitTreasury(path, balance)
		if !errors.Is(ierr, ErrTreasuryAlreadyInit) {
			t.Fatalf("re-init with %d err=%v, want ErrTreasuryAlreadyInit", balance, ierr)
		}
		if st != nil {
			_ = st.Close()
			t.Fatalf("rejected re-init with %d returned a store", balance)
		}
	}
	rawAfter, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rawBefore, rawAfter) {
		t.Fatalf("state file changed after rejected re-init attempts")
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	snap, err := reopened.BalanceSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snap.Treasury != 900 || len(snap.Balances) != 1 || snap.Balances["audits"] != 100 {
		t.Fatalf("balances changed: %+v", snap)
	}
	record, ok, err := reopened.Proposal("gip-1")
	if err != nil || !ok || record.State != "executed" {
		t.Fatalf("proposal state changed: record=%+v ok=%v err=%v", record, ok, err)
	}
	got, ok, err := reopened.Receipt("gip-1")
	if err != nil || !ok {
		t.Fatalf("receipt lost: ok=%v err=%v", ok, err)
	}
	if got.ProposalID != receipt0.ProposalID || got.ExecutedAt != receipt0.ExecutedAt ||
		got.Order != receipt0.Order || len(got.Actions) != len(receipt0.Actions) {
		t.Fatalf("receipt changed: got=%+v want=%+v", got, receipt0)
	}
}

// TestInitRefusedOverCorruptFilePreservesBytes：已有文件损坏（以空文件为
// 代表）时，初始化同样被拒绝且原内容保留，不能借“初始化”清空后重建；
// 随后读取仍把它判为损坏而不是当成尚未初始化。
func TestInitRefusedOverCorruptFilePreservesBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "treasury.json")
	corrupt := []byte("   \n  ") // 只有空白：loadLocked 会判为空文件损坏
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := InitTreasury(path, 0)
	if !errors.Is(err, ErrTreasuryAlreadyInit) {
		t.Fatalf("init over corrupt file err=%v, want ErrTreasuryAlreadyInit", err)
	}
	if st != nil {
		_ = st.Close()
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, corrupt) {
		t.Fatalf("corrupt file content was modified")
	}
	if _, err := Open(path); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("Open after refused init err=%v, want ErrStateCorrupt", err)
	}
}
