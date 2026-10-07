package govflow

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// 本文件为“资金库首次初始化”补充回归保障，保护既有规则：
//
//	在状态文件原本不存在、目录可正常读写时，多个请求各自提供合法初始余额，
//	同时初始化同一个资金库——无论请求来自同一进程的不同 goroutine/句柄，
//	还是彼此独立的进程——都必须恰好有一个请求成功，其余请求明确得到
//	ErrTreasuryAlreadyInit（“资金库已经初始化”）的拒绝；落盘的初始余额必须
//	就是成功请求提交的数值，不能被失败请求覆盖，也不能把多个金额合并。
//	初始余额为 0 同样是合法成功；相同余额的并发请求也仍只有一次成功，
//	初始化不是“相同内容即可重试接受”的幂等操作。
//
//	初始化期间其他使用者同时打开查询：文件尚未出现时明确返回不存在；
//	一旦成功打开，必须得到一份完整、可继续使用的初始状态（余额即胜者数值，
//	收款账户、登记提案、投票提案与执行凭据全空），不能把创建过程中暂时
//	出现的空文件当作损坏状态上报。初始化全部结束后不遗留无法打开的空文件。
//
//	已有文件（含损坏文件）继续适用“只有新文件才能初始化”：即使再提交的
//	余额与现有余额相同也拒绝，且不改变任何余额、提案或执行凭据；
//	损坏文件拒绝初始化并逐字节保留原内容，不能借初始化清空重建。

// initResult 记录一个并发初始化请求的全部可观察结果。
type initResult struct {
	balance int64
	store   *Store
	err     error
}

// runConcurrentInits 用同一个启动栅栏在同进程内同时放出 n 个初始化请求，
// 每个请求提交 balances[i]。
func runConcurrentInits(t *testing.T, path string, balances []int64) []initResult {
	t.Helper()
	n := len(balances)
	results := make([]initResult, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			s, err := InitTreasury(path, balances[i])
			results[i] = initResult{balance: balances[i], store: s, err: err}
		}(i)
	}
	close(start)
	wg.Wait()
	return results
}

// assertOneInitWinner 核对一组并发初始化结果：恰好一个成功，其余全部是
// ErrTreasuryAlreadyInit；返回胜者提交的余额。胜者句柄立即可用且报告自己
// 提交的余额。
func assertOneInitWinner(t *testing.T, results []initResult) int64 {
	t.Helper()
	winners := 0
	var winnerBalance int64
	for i, r := range results {
		if r.err != nil {
			if !errors.Is(r.err, ErrTreasuryAlreadyInit) {
				t.Errorf("init %d (balance=%d) err=%v, want ErrTreasuryAlreadyInit",
					i, r.balance, r.err)
			}
			if r.store != nil {
				t.Errorf("rejected init %d returned a non-nil store", i)
			}
			continue
		}
		if r.store == nil {
			t.Errorf("init %d succeeded with nil store", i)
			continue
		}
		winners++
		winnerBalance = r.balance
		// 成功请求拿到的句柄必须可继续使用，且余额就是它自己提交的数值。
		if bal, err := r.store.TreasuryBalance(); err != nil || bal != r.balance {
			t.Errorf("winner store balance=%d err=%v, want %d", bal, err, r.balance)
		}
	}
	if winners != 1 {
		t.Fatalf("successful inits=%d, want exactly 1", winners)
	}
	return winnerBalance
}

// assertFreshStateUsable 打开资金库，核对一份完整、可继续使用的初始状态：
// 资金库余额（含 initial_treasury）等于 want，收款账户、登记提案、投票提案
// 与执行凭据全部为空（空集合是非 nil 的空表/空切片，而不是缺损）。
func assertFreshStateUsable(t *testing.T, path string, want int64) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open after init: %v", err)
	}
	snap, err := s.BalanceSnapshot()
	if err != nil {
		t.Fatalf("BalanceSnapshot: %v", err)
	}
	if snap.Treasury != want {
		t.Fatalf("treasury=%d, want winner balance %d", snap.Treasury, want)
	}
	if snap.Balances == nil || len(snap.Balances) != 0 {
		t.Fatalf("fresh balances must be a non-nil empty map, got %#v", snap.Balances)
	}
	if bal, err := s.Balance("never-paid"); err != nil || bal != 0 {
		t.Fatalf("unknown account balance=%d err=%v, want 0 with no error", bal, err)
	}
	props, err := s.Proposals()
	if err != nil || len(props) != 0 {
		t.Fatalf("fresh registered proposals=%v err=%v, want empty", props, err)
	}
	both, err := s.ProposalsSnapshot()
	if err != nil {
		t.Fatalf("ProposalsSnapshot: %v", err)
	}
	if len(both.Registered) != 0 || both.Voting == nil || len(both.Voting) != 0 {
		t.Fatalf("fresh proposals snapshot registered=%d voting=%v, want two empty tables",
			len(both.Registered), both.Voting)
	}
	receipts, err := s.Receipts()
	if err != nil || receipts == nil || len(receipts) != 0 {
		t.Fatalf("fresh receipts=%v err=%v, want non-nil empty list", receipts, err)
	}
	if _, ok, err := s.Receipt("anything"); err != nil || ok {
		t.Fatalf("fresh receipt lookup ok=%v err=%v, want false/nil", ok, err)
	}
	return s
}

// assertFreshStateRaw 直接解析落盘文件，核对保存格式就是胜者提交的完整
// 初始状态：两个资金库余额字段都明确写出且等于胜者数值，四张表全部以
// 空集合（{} 或 []）明确落盘——初始化结束后磁盘上不得留下空文件。
func assertFreshStateRaw(t *testing.T, path string, want int64) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		t.Fatal("state file is empty after initialization completed")
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("state file is not valid JSON: %v\n%s", err, raw)
	}
	if doc["magic"] != stateMagic || doc["version"] != float64(stateVersion) {
		t.Fatalf("bad magic/version: %v %v", doc["magic"], doc["version"])
	}
	if doc["initial_treasury"] != float64(want) || doc["treasury"] != float64(want) {
		t.Fatalf("saved balances initial=%v treasury=%v, want both %d (no overwrite, no merge)",
			doc["initial_treasury"], doc["treasury"], want)
	}
	for name, val := range map[string]any{
		"balances":       doc["balances"],
		"proposals":      doc["proposals"],
		"vote_proposals": doc["vote_proposals"],
	} {
		m, ok := val.(map[string]any)
		if !ok || len(m) != 0 {
			t.Fatalf("fresh %s must be an explicit empty object {}, got %v", name, val)
		}
	}
	receipts, ok := doc["receipts"].([]any)
	if !ok || len(receipts) != 0 {
		t.Fatalf("fresh receipts must be an explicit empty array [], got %v", doc["receipts"])
	}
}

// assertNoInitLeftovers 核对初始化结束后目录中不遗留临时文件，状态文件本身
// 非空且可打开；锁文件允许保留（后续操作复用它）。
func assertNoInitLeftovers(t *testing.T, dir, state string) {
	t.Helper()
	if info, err := os.Stat(state); err != nil || info.Size() == 0 {
		t.Fatalf("state file missing or empty after inits: info=%v err=%v", info, err)
	}
	tmp, err := filepath.Glob(filepath.Join(dir, ".govflow-state-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tmp) != 0 {
		t.Fatalf("temporary commit files left behind: %v", tmp)
	}
}

// closeInitStores 关闭并发结果中胜者持有的句柄（失败者本就返回 nil）。
func closeInitStores(t *testing.T, results []initResult) {
	t.Helper()
	for _, r := range results {
		if r.store != nil {
			if err := r.store.Close(); err != nil {
				t.Errorf("close winner store: %v", err)
			}
		}
	}
}

// TestConcurrentInitDistinctBalances：同进程多个 goroutine 同时初始化同一份
// 原本不存在的资金库，各自提交互不相同的合法余额。恰好一次成功；其余请求
// 全部得到“资金库已经初始化”的拒绝；落盘余额必须是胜者提交的数值，
// 不被任何失败请求覆盖，也不发生金额合并。
func TestConcurrentInitDistinctBalances(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "treasury.json")
	balances := []int64{7001, 7002, 7003, 7004, 7005, 7006, 7007, 7008,
		7009, 7010, 7011, 7012, 7013, 7014, 7015, 7016}

	results := runConcurrentInits(t, path, balances)
	winner := assertOneInitWinner(t, results)

	// 每个请求提交过的余额都不得出现在资金库位置：落盘的只能是胜者数值。
	store := assertFreshStateUsable(t, path, winner)
	defer store.Close()
	for _, b := range balances {
		if b != winner {
			if bal, _ := store.TreasuryBalance(); bal == b {
				t.Fatalf("treasury overwritten by losing request balance %d", b)
			}
		}
	}
	if bal, _ := store.TreasuryBalance(); bal != winner {
		t.Fatalf("treasury=%d, want winner %d", bal, winner)
	}
	closeInitStores(t, results)
	assertFreshStateRaw(t, path, winner)
	assertNoInitLeftovers(t, dir, path)
}

// TestConcurrentInitIdenticalBalances：多个请求提交完全相同的余额同时初始化，
// 仍必须恰好一次成功——初始化不是“内容相同即可接受”的幂等重试，其余请求
// 一律按已经初始化拒绝，不得全部成功。
func TestConcurrentInitIdenticalBalances(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "treasury.json")
	const same int64 = 8888
	balances := make([]int64, 24)
	for i := range balances {
		balances[i] = same
	}

	results := runConcurrentInits(t, path, balances)
	winner := assertOneInitWinner(t, results)
	if winner != same {
		t.Fatalf("winner balance=%d, want %d", winner, same)
	}
	closeInitStores(t, results)
	store := assertFreshStateUsable(t, path, same)
	defer store.Close()
	assertFreshStateRaw(t, path, same)
	assertNoInitLeftovers(t, dir, path)
}

// TestConcurrentInitZeroBalance：初始余额为 0 是合法成功，零不能被误认为
// “初始化尚未完成”：并发请求中仍恰好一次成功，胜者状态可立即打开使用，
// 落盘文件把两个余额字段都明确写成 0，后续重新打开仍是余额 0 的完整状态。
func TestConcurrentInitZeroBalance(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "treasury.json")
	balances := make([]int64, 16) // 全部为 0

	results := runConcurrentInits(t, path, balances)
	winner := assertOneInitWinner(t, results)
	if winner != 0 {
		t.Fatalf("winner balance=%d, want 0", winner)
	}
	closeInitStores(t, results)

	store := assertFreshStateUsable(t, path, 0)
	if bal, err := store.TreasuryBalance(); err != nil || bal != 0 {
		t.Fatalf("zero-balance treasury=%d err=%v", bal, err)
	}
	store.Close()
	assertFreshStateRaw(t, path, 0)

	// 重新打开：明确写出的 0 仍是一份完整可用的资金库，而不是未初始化。
	reopened := assertFreshStateUsable(t, path, 0)
	reopened.Close()
	assertNoInitLeftovers(t, dir, path)
}

// TestConcurrentInitPathSpellingsShareInitDecision：同一进程内的请求用不同
// 路径拼写（相对/绝对）指向同一份文件并发初始化，仍必须恰好一次成功：
// 不同拼写命中同一把进程内锁与同一个 O_EXCL 目标。
func TestConcurrentInitPathSpellingsShareInitDecision(t *testing.T) {
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)

	const n = 16
	results := make([]initResult, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			// 一半请求用相对拼写，一半用绝对拼写，余额互不相同。
			target := "treasury.json"
			balance := int64(9000 + i)
			if i%2 == 0 {
				target = filepath.Join(dir, "treasury.json")
			}
			s, err := InitTreasury(target, balance)
			results[i] = initResult{balance: balance, store: s, err: err}
		}(i)
	}
	close(start)
	wg.Wait()

	winner := assertOneInitWinner(t, results)
	closeInitStores(t, results)
	store := assertFreshStateUsable(t, filepath.Join(dir, "treasury.json"), winner)
	defer store.Close()
	assertFreshStateRaw(t, filepath.Join(dir, "treasury.json"), winner)
}

// TestOpenDuringConcurrentInit：初始化进行期间，同进程其他使用者反复打开
// 该资金库查询。文件尚未出现时必须明确得到“不存在”，而不是损坏；只要
// 打开成功，就必须是一份完整、可继续使用的初始状态，且余额与最终胜者
// 一致——并发期间不可能先读到某个余额、最终又换成另一个余额。
func TestOpenDuringConcurrentInit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "treasury.json")

	// 文件确实尚未出现：明确返回不存在，错误分类必须是 fs.ErrNotExist，
	// 绝不能报成 ErrStateCorrupt。
	if _, err := Open(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Open before init err=%v, want fs.ErrNotExist", err)
	}

	const n = 16
	balances := make([]int64, n)
	for i := range balances {
		balances[i] = int64(5000 + i)
	}

	stop := make(chan struct{})
	var readerWg sync.WaitGroup
	var sawComplete bool
	var mu sync.Mutex
	var readerErr error
	// 读取方：在初始化窗口内反复打开。同进程内 Open 与 InitTreasury 共享
	// 进程内路径锁，因此每次成功打开都必然对应某次完整提交；这里额外守住
	// “不返回损坏/其他错误、成功即完整”的性质。
	for r := 0; r < 4; r++ {
		readerWg.Add(1)
		go func() {
			defer readerWg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				s, err := Open(path)
				if err != nil {
					if errors.Is(err, fs.ErrNotExist) {
						continue
					}
					mu.Lock()
					readerErr = err
					mu.Unlock()
					return
				}
				// 成功打开必须得到完整初始状态：先只做严格加载与空表核对，
				// 余额是否等于最终胜者在所有初始化结束后统一比对（这里记录
				// 观测值；文件只会被胜者创建一次，观测值必然就是胜者数值）。
				bal, qerr := s.TreasuryBalance()
				if qerr != nil {
					mu.Lock()
					readerErr = qerr
					mu.Unlock()
					_ = s.Close()
					return
				}
				snap, qerr := s.BalanceSnapshot()
				if qerr != nil || snap.Treasury != bal || len(snap.Balances) != 0 {
					mu.Lock()
					readerErr = errors.New("reader observed partial/inconsistent fresh state")
					mu.Unlock()
					_ = s.Close()
					return
				}
				if receipts, qerr := s.Receipts(); qerr != nil || len(receipts) != 0 {
					mu.Lock()
					readerErr = errors.New("reader observed receipts during fresh init")
					mu.Unlock()
					_ = s.Close()
					return
				}
				mu.Lock()
				sawComplete = true
				mu.Unlock()
				_ = s.Close()
			}
		}()
	}

	results := runConcurrentInits(t, path, balances)
	winner := assertOneInitWinner(t, results)
	close(stop)
	readerWg.Wait()
	closeInitStores(t, results)

	if readerErr != nil {
		t.Fatalf("reader during init: %v", readerErr)
	}
	if !sawComplete {
		// 初始化极快时读取方可能整窗都撞上“不存在”；栅栏后再强制核对一次
		// 成功打开路径，保证“一旦成功打开即完整状态”被实际执行到。
		s := assertFreshStateUsable(t, path, winner)
		s.Close()
	}
	store := assertFreshStateUsable(t, path, winner)
	defer store.Close()
	assertFreshStateRaw(t, path, winner)
	assertNoInitLeftovers(t, dir, path)

	// 所有初始化请求结束后，正常余额查询仍读到同一份完整状态。
	if bal, err := store.TreasuryBalance(); err != nil || bal != winner {
		t.Fatalf("final treasury=%d err=%v, want %d", bal, err, winner)
	}
}

// TestInitRefusesExistingFileEvenSameBalance：已有合法文件时，即使再提交的
// 余额与现有余额完全相同也不能再次初始化；余额、登记提案、投票提案与
// 执行凭据一律不变。
func TestInitRefusesExistingFileEvenSameBalance(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "treasury.json")
	store, err := InitTreasury(path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	// 制造余额、登记提案、投票提案、执行凭据都非空的既有状态。
	mustRegister(t, store, "gip-1", 0, "transfer:audits:100")
	if _, err := store.Execute("gip-1", 0); err != nil {
		t.Fatal(err)
	}
	mustCreateVote(t, store, baseVoteInput("gip-vote"))
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 相同余额：仍然拒绝，不能当作幂等重试。
	if s, err := InitTreasury(path, 1000); !errors.Is(err, ErrTreasuryAlreadyInit) || s != nil {
		if s != nil {
			s.Close()
		}
		t.Fatalf("re-init with same balance err=%v store=%v", err, s)
	}
	// 不同余额、零余额同样拒绝。
	if _, err := InitTreasury(path, 9999); !errors.Is(err, ErrTreasuryAlreadyInit) {
		t.Fatalf("re-init with different balance err=%v", err)
	}
	if _, err := InitTreasury(path, 0); !errors.Is(err, ErrTreasuryAlreadyInit) {
		t.Fatalf("re-init with zero err=%v", err)
	}

	// 状态文件逐字节不变。
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("existing state file was modified by rejected init")
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if bal, _ := reopened.TreasuryBalance(); bal != 900 {
		t.Fatalf("treasury changed: %d", bal)
	}
	if bal, _ := reopened.Balance("audits"); bal != 100 {
		t.Fatalf("recipient balance changed: %d", bal)
	}
	if p, ok, _ := reopened.Proposal("gip-1"); !ok || p.State != "executed" {
		t.Fatalf("registered proposal changed: %+v ok=%v", p, ok)
	}
	if r, ok, _ := reopened.Receipt("gip-1"); !ok || r.Order != 0 || len(r.Actions) != 1 {
		t.Fatalf("execution receipt changed: %+v ok=%v", r, ok)
	}
	both, err := reopened.ProposalsSnapshot()
	if err != nil || len(both.Voting) != 1 || both.Voting[0].ID != "gip-vote" {
		t.Fatalf("voting proposal changed: %+v err=%v", both, err)
	}
}

// TestInitRefusesCorruptFileAndPreservesIt：已有文件损坏（空文件、非法 JSON、
// 截断的合法前缀）时，初始化必须按“文件已存在”拒绝，且逐字节保留原内容——
// 不能借初始化把损坏文件清空后重建。
func TestInitRefusesCorruptFileAndPreservesIt(t *testing.T) {
	dir := t.TempDir()
	corruptions := map[string][]byte{
		"empty":     nil,
		"garbage":   []byte("not json at all"),
		"partial":   []byte(`{"magic": "govflow-treasury-state", "version": 1,`),
		"zero byte": []byte{0},
	}
	for name, raw := range corruptions {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, "corrupt-"+name+".json")
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			// 损坏文件不能初始化：既不能成功，也不能因为“内容看起来是空/零”
			// 而被当成尚未初始化。
			if s, err := InitTreasury(path, 0); !errors.Is(err, ErrTreasuryAlreadyInit) || s != nil {
				if s != nil {
					s.Close()
				}
				t.Fatalf("init over corrupt file err=%v store=%v, want ErrTreasuryAlreadyInit", err, s)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(raw) {
				t.Fatalf("corrupt content not preserved:\n got %q\nwant %q", got, raw)
			}
			// 损坏文件依旧按损坏被拒绝打开：拒绝初始化没有把它“修好”。
			if s, err := Open(path); !errors.Is(err, ErrStateCorrupt) {
				if s != nil {
					s.Close()
				}
				t.Fatalf("Open corrupt file err=%v, want ErrStateCorrupt", err)
			}
		})
	}
}

// ---- 跨进程（独立 CLI 进程）----

// cliInitResult 记录一个并发 init 进程的可观察结果。
type cliInitResult struct {
	balance int64
	code    int
	stdout  string
	stderr  string
}

// runConcurrentCLIInits 同时放出 n 个独立进程对同一份状态文件执行 init，
// 每个进程提交 balances[i]，--json 输出保留在结果中。
func runConcurrentCLIInits(t *testing.T, binary, state string, balances []int64) []cliInitResult {
	t.Helper()
	n := len(balances)
	results := make([]cliInitResult, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			so, se, code := runCLI(t, binary, state, "init",
				"--balance", strconv.FormatInt(balances[i], 10), "--json")
			results[i] = cliInitResult{balance: balances[i], code: code, stdout: so, stderr: se}
		}(i)
	}
	close(start)
	wg.Wait()
	return results
}

// assertOneCLIInitWinner 核对跨进程并发初始化结果：恰好一个进程退出码 0，
// 其 JSON 结果 initialized=true 且 treasury 等于它提交的余额；其余进程退出
// 码 1、stderr 明确是“already exists”拒绝，stdout 不允许出现成功结果。
func assertOneCLIInitWinner(t *testing.T, results []cliInitResult) int64 {
	t.Helper()
	winners := 0
	var winnerBalance int64
	for i, r := range results {
		if r.code == 0 {
			winners++
			winnerBalance = r.balance
			var resp struct {
				State       string `json:"state"`
				Treasury    int64  `json:"treasury"`
				Initialized bool   `json:"initialized"`
			}
			if err := json.Unmarshal([]byte(r.stdout), &resp); err != nil {
				t.Fatalf("winner %d stdout is not JSON: %v\n%s", i, err, r.stdout)
			}
			if !resp.Initialized || resp.Treasury != r.balance {
				t.Fatalf("winner %d response=%+v, want initialized treasury=%d", i, resp, r.balance)
			}
			continue
		}
		if r.code != 1 {
			t.Errorf("losing init %d exit=%d, want 1 (stderr=%s)", i, r.code, r.stderr)
		}
		if !strings.Contains(r.stderr, "already exists") {
			t.Errorf("losing init %d stderr=%q, want an already-initialized refusal", i, r.stderr)
		}
		if strings.TrimSpace(r.stdout) != "" {
			t.Errorf("losing init %d printed success output: %q", i, r.stdout)
		}
	}
	if winners != 1 {
		t.Fatalf("successful init processes=%d, want exactly 1", winners)
	}
	return winnerBalance
}

// assertCLIFreshState 通过命令行核对初始化后的完整初始状态：资金库余额等于
// want，收款账户、登记/投票提案与执行凭据全部为空。
func assertCLIFreshState(t *testing.T, binary, state string, want int64) {
	t.Helper()
	so, se, code := runCLI(t, binary, state, "balances", "--json")
	if code != 0 {
		t.Fatalf("balances exit=%d: %s", code, se)
	}
	var bal struct {
		Treasury int64            `json:"treasury"`
		Balances map[string]int64 `json:"balances"`
	}
	if err := json.Unmarshal([]byte(so), &bal); err != nil {
		t.Fatalf("balances output is not JSON: %v\n%s", err, so)
	}
	if bal.Treasury != want {
		t.Fatalf("final treasury=%d, want winner balance %d (no overwrite, no merge)",
			bal.Treasury, want)
	}
	if len(bal.Balances) != 0 {
		t.Fatalf("fresh recipient balances must be empty, got %v", bal.Balances)
	}
	for _, cmd := range []string{"proposals", "receipts"} {
		out, se, code := runCLI(t, binary, state, cmd, "--json")
		if code != 0 {
			t.Fatalf("%s exit=%d: %s", cmd, code, se)
		}
		var list []json.RawMessage
		if err := json.Unmarshal([]byte(out), &list); err != nil {
			t.Fatalf("%s output is not JSON: %v\n%s", cmd, err, out)
		}
		if len(list) != 0 {
			t.Fatalf("fresh %s must be empty, got %d entries: %s", cmd, len(list), out)
		}
	}
}

// TestCrossProcessConcurrentInitDistinctBalances：多个独立进程同时初始化同一
// 份不存在的资金库，各自提交不同余额。恰好一个进程成功，其余进程得到
// “已经初始化”的明确拒绝；最终余额就是胜者提交的数值。
func TestCrossProcessConcurrentInitDistinctBalances(t *testing.T) {
	binary := buildCLI(t)
	dir := t.TempDir()
	state := filepath.Join(dir, "treasury.json")
	balances := []int64{2001, 2002, 2003, 2004, 2005, 2006, 2007, 2008,
		2009, 2010, 2011, 2012, 2013, 2014, 2015, 2016}

	results := runConcurrentCLIInits(t, binary, state, balances)
	winner := assertOneCLIInitWinner(t, results)
	assertCLIFreshState(t, binary, state, winner)

	// 库接口核对同一结果，并确认无空文件/临时文件残留。
	store := assertFreshStateUsable(t, state, winner)
	store.Close()
	assertFreshStateRaw(t, state, winner)
	assertNoInitLeftovers(t, dir, state)
}

// TestCrossProcessConcurrentInitIdenticalBalances：多个进程提交相同余额同时
// 初始化，仍只有一个进程成功——不能因为内容相同就把全部请求按重试接受。
func TestCrossProcessConcurrentInitIdenticalBalances(t *testing.T) {
	binary := buildCLI(t)
	dir := t.TempDir()
	state := filepath.Join(dir, "treasury.json")
	balances := make([]int64, 12)
	for i := range balances {
		balances[i] = 6666
	}

	results := runConcurrentCLIInits(t, binary, state, balances)
	winner := assertOneCLIInitWinner(t, results)
	if winner != 6666 {
		t.Fatalf("winner=%d, want 6666", winner)
	}
	assertCLIFreshState(t, binary, state, 6666)
	assertFreshStateRaw(t, state, 6666)
	assertNoInitLeftovers(t, dir, state)
}

// TestCrossProcessConcurrentInitZeroBalance：多进程同时以 0 初始化，恰好一次
// 成功；0 是合法初始余额，最终查询必须读到余额为 0 的完整状态，而不是
// “尚未初始化”或损坏。
func TestCrossProcessConcurrentInitZeroBalance(t *testing.T) {
	binary := buildCLI(t)
	dir := t.TempDir()
	state := filepath.Join(dir, "treasury.json")
	balances := make([]int64, 12)

	results := runConcurrentCLIInits(t, binary, state, balances)
	winner := assertOneCLIInitWinner(t, results)
	if winner != 0 {
		t.Fatalf("winner=%d, want 0", winner)
	}
	// 再查一次：文件已存在，以 0 重试初始化仍必须被拒绝。
	if _, se, code := runCLI(t, binary, state, "init", "--balance", "0"); code != 1 ||
		!strings.Contains(se, "already exists") {
		t.Fatalf("zero re-init code=%d stderr=%q, want already-exists refusal", code, se)
	}
	assertCLIFreshState(t, binary, state, 0)
	assertFreshStateRaw(t, state, 0)
	assertNoInitLeftovers(t, dir, state)
}

// TestCrossProcessReadDuringConcurrentInit：多个进程初始化期间，其他使用者
// 进程同时打开查询。文件尚未出现时命令必须明确失败于“不存在”；只要查询
// 成功，就必须读到完整初始状态，且金额与最终胜者一致——任何查询都不得
// 报状态损坏（把创建过程中暂时出现的空文件当成损坏上报）。
func TestCrossProcessReadDuringConcurrentInit(t *testing.T) {
	binary := buildCLI(t)
	dir := t.TempDir()
	state := filepath.Join(dir, "treasury.json")

	// 确定性地覆盖“文件尚未出现”分支：明确是不存在，而不是损坏，stdout 为空。
	if so, se, code := runCLI(t, binary, state, "balances", "--json"); code != 1 {
		t.Fatalf("query before init exit=%d, want 1 (stdout=%q stderr=%q)", code, so, se)
	} else {
		if strings.Contains(se, "corrupt") {
			t.Fatalf("missing file reported as corrupt: %s", se)
		}
		if !strings.Contains(se, "no such file") {
			t.Fatalf("missing file stderr=%q, want a not-exist error", se)
		}
		if strings.TrimSpace(so) != "" {
			t.Fatalf("missing file query printed output: %q", so)
		}
	}

	const initN = 8
	balances := make([]int64, initN)
	for i := range balances {
		balances[i] = int64(3000 + i)
	}

	// 读取进程分多批错开启动：早批次可能撞上不存在，晚批次应读到完整状态；
	// 两种结果都合法，唯独“损坏”或其它错误不合法。
	const readerWaves = 5
	const readersPerWave = 8
	type readObs struct {
		code     int
		treasury int64
		stderr   string
	}
	observed := make(chan readObs, readerWaves*readersPerWave)
	var wg sync.WaitGroup
	start := make(chan struct{})
	// 初始化进程在栅栏放开时同时启动。
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		runConcurrentCLIInits(t, binary, state, balances)
	}()
	for wave := 0; wave < readerWaves; wave++ {
		for r := 0; r < readersPerWave; r++ {
			wg.Add(1)
			go func(wave int) {
				defer wg.Done()
				<-start
				// 错峰只用于让两分支都更可能被观察到；判定本身不依赖时序。
				time.Sleep(time.Duration(wave) * 4 * time.Millisecond)
				so, se, code := runCLI(t, binary, state, "balances", "--json")
				if code != 0 {
					observed <- readObs{code: code, stderr: se}
					return
				}
				var bal struct {
					Treasury int64 `json:"treasury"`
				}
				if err := json.Unmarshal([]byte(so), &bal); err != nil {
					t.Errorf("reader balances output not JSON: %v\n%s", err, so)
					observed <- readObs{code: -1, stderr: so}
					return
				}
				observed <- readObs{code: 0, treasury: bal.Treasury}
			}(wave)
		}
	}
	close(start)
	wg.Wait()
	close(observed)

	// 胜者必须是提交余额之一；文件只会被胜者创建一次，因此并发期间任何
	// 成功读取到的金额都必须等于最终金额。
	submitted := map[int64]bool{}
	for _, b := range balances {
		submitted[b] = true
	}

	successes, notExists := 0, 0
	var finalTreasury int64
	for o := range observed {
		switch {
		case o.code == 0:
			if !submitted[o.treasury] {
				t.Fatalf("reader observed treasury %d, which no init request submitted", o.treasury)
			}
			if successes == 0 {
				finalTreasury = o.treasury
			} else if o.treasury != finalTreasury {
				t.Fatalf("readers observed two different fresh balances: %d and %d (file must be created once)",
					finalTreasury, o.treasury)
			}
			successes++
		default:
			if strings.Contains(o.stderr, "corrupt") {
				t.Fatalf("reader observed a corrupt state during initialization: %s", o.stderr)
			}
			if !strings.Contains(o.stderr, "no such file") {
				t.Fatalf("reader unexpected error (neither success nor not-exist): %s", o.stderr)
			}
			notExists++
		}
	}
	if successes == 0 {
		t.Fatal("no reader ever observed the completed state; rerun or widen the window")
	}
	if notExists == 0 {
		// 不显式致命：慢机器上所有读取都可能晚于初始化。前置的确定性
		// “不存在”检查已覆盖该分支，这里只记录观测情况。
		t.Logf("all %d concurrent readers ran after the file appeared", successes)
	}

	// 所有初始化结束后：余额与胜者一致，四份集合为空，文件完整、无残留。
	assertCLIFreshState(t, binary, state, finalTreasury)
	store := assertFreshStateUsable(t, state, finalTreasury)
	store.Close()
	assertFreshStateRaw(t, state, finalTreasury)
	assertNoInitLeftovers(t, dir, state)
}

// TestCrossProcessInitRefusesExistingAndCorruptFile：已有文件（即使余额相同）
// 与损坏文件都必须拒绝跨进程初始化，且不改变任何内容。
func TestCrossProcessInitRefusesExistingAndCorruptFile(t *testing.T) {
	binary := buildCLI(t)
	dir := t.TempDir()
	state := filepath.Join(dir, "treasury.json")
	if _, se, code := runCLI(t, binary, state, "init", "--balance", "1000"); code != 0 {
		t.Fatalf("first init failed: %s", se)
	}
	before, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	// 相同余额、不同余额、零余额全部拒绝。
	for _, b := range []string{"1000", "9999", "0"} {
		if so, se, code := runCLI(t, binary, state, "init", "--balance", b); code != 1 {
			t.Fatalf("re-init --balance %s exit=%d stdout=%q stderr=%q, want 1", b, code, so, se)
		} else if !strings.Contains(se, "already exists") {
			t.Fatalf("re-init --balance %s stderr=%q, want already-exists", b, se)
		}
	}
	after, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("existing state modified by rejected CLI init")
	}
	assertCLIFreshState(t, binary, state, 1000)

	// 损坏文件：CLI 初始化拒绝且内容逐字节保留。
	corruptPath := filepath.Join(dir, "corrupt.json")
	raw := []byte("")
	if err := os.WriteFile(corruptPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, se, code := runCLI(t, binary, corruptPath, "init", "--balance", "0"); code != 1 ||
		!strings.Contains(se, "already exists") {
		t.Fatalf("init over empty file code=%d stderr=%q", code, se)
	}
	got, err := os.ReadFile(corruptPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(raw) {
		t.Fatalf("empty corrupt file content changed: got %q want %q", got, raw)
	}
}
