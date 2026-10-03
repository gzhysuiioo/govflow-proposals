package govflow

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func openTempStore(t *testing.T, balance int64) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "treasury.json")
	store, err := InitTreasury(path, balance)
	if err != nil {
		t.Fatalf("InitTreasury: %v", err)
	}
	return store, path
}

func mustRegister(t *testing.T, store *Store, id string, timelock int64, actions ...string) {
	t.Helper()
	if existed, err := store.Register(id, timelock, actions); err != nil || existed {
		t.Fatalf("Register(%s) existed=%v err=%v", id, existed, err)
	}
}

func TestInitOnlyNewFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "treasury.json")
	store, err := InitTreasury(path, 1000)
	if err != nil {
		t.Fatalf("InitTreasury: %v", err)
	}
	store.Close()

	if _, err := InitTreasury(path, 9999); !errors.Is(err, ErrTreasuryAlreadyInit) {
		t.Fatalf("second init err=%v, want ErrTreasuryAlreadyInit", err)
	}
	// 已有文件不得重置余额。
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer reopened.Close()
	if bal, _ := reopened.TreasuryBalance(); bal != 1000 {
		t.Fatalf("treasury was reset: %d", bal)
	}

	// 初始化负数余额被拒绝。
	if _, err := InitTreasury(filepath.Join(t.TempDir(), "x.json"), -1); !errors.Is(err, ErrInvalidRegistration) {
		t.Fatalf("negative init err=%v", err)
	}
}

func TestRegisterIdempotentAndConflict(t *testing.T) {
	store, _ := openTempStore(t, 1000)
	defer store.Close()

	if _, err := store.Register("", 10, []string{"transfer:a:1"}); !errors.Is(err, ErrInvalidRegistration) {
		t.Fatalf("empty id err=%v", err)
	}

	mustRegister(t, store, "gip-1", 100, "transfer:audits:25000", "transfer:legal:5")

	// 完全相同的重试：返回 existed，不新增记录。
	existed, err := store.Register("gip-1", 100, []string{"transfer:audits:25000", "transfer:legal:5"})
	if err != nil || !existed {
		t.Fatalf("identical retry existed=%v err=%v", existed, err)
	}
	// nil 与空切片等价。
	existed, err = store.Register("gip-empty", 0, nil)
	if err != nil || existed {
		t.Fatalf("register empty actions: %v %v", existed, err)
	}
	existed, err = store.Register("gip-empty", 0, []string{})
	if err != nil || !existed {
		t.Fatalf("empty actions retry: %v %v", existed, err)
	}

	// 时间锁不同。
	if _, err := store.Register("gip-1", 101, []string{"transfer:audits:25000", "transfer:legal:5"}); !errors.Is(err, ErrProposalConflict) {
		t.Fatalf("timelock conflict err=%v", err)
	}
	// 动作原文不同（含顺序不同）。
	if _, err := store.Register("gip-1", 100, []string{"transfer:legal:5", "transfer:audits:25000"}); !errors.Is(err, ErrProposalConflict) {
		t.Fatalf("action order conflict err=%v", err)
	}
	if _, err := store.Register("gip-1", 100, []string{"transfer:audits:25000"}); !errors.Is(err, ErrProposalConflict) {
		t.Fatalf("action count conflict err=%v", err)
	}

	records, err := store.Proposals()
	if err != nil || len(records) != 2 {
		t.Fatalf("Proposals len=%d err=%v", len(records), err)
	}
	if records[0].ID != "gip-1" || records[0].State != "passed" {
		t.Fatalf("unexpected first record: %+v", records[0])
	}
}

func TestExecuteHappyPathAndReceiptShape(t *testing.T) {
	store, _ := openTempStore(t, 1000)
	defer store.Close()
	mustRegister(t, store, "gip-1", 100, "transfer:audits:250", "transfer:audits:100", "transfer:legal:50")

	// 时间未到。
	if _, err := store.Execute("gip-1", 99); !errors.Is(err, ErrExecutionRejected) ||
		!strings.Contains(err.Error(), "timelock not reached") {
		t.Fatalf("early execute err=%v", err)
	}
	// 边界：now == timelockEnd 即可执行。
	receipt, err := store.Execute("gip-1", 100)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if receipt.ExecutedAt != 100 || receipt.Order != 0 || len(receipt.Actions) != 3 {
		t.Fatalf("unexpected receipt: %+v", receipt)
	}
	want := []struct {
		acct           string
		tb, ta, rb, ra int64
	}{
		{"audits", 1000, 750, 0, 250},
		{"audits", 750, 650, 250, 350}, // 同一账户连续收款
		{"legal", 650, 600, 0, 50},
	}
	for i, w := range want {
		ar := receipt.Actions[i]
		if ar.Treasury.Before != w.tb || ar.Treasury.After != w.ta {
			t.Fatalf("action %d treasury %d->%d, want %d->%d", i, ar.Treasury.Before, ar.Treasury.After, w.tb, w.ta)
		}
		if ar.Recipient.Account != w.acct || ar.Recipient.Before != w.rb || ar.Recipient.After != w.ra {
			t.Fatalf("action %d recipient mismatch: %+v", i, ar.Recipient)
		}
	}
	if bal, _ := store.TreasuryBalance(); bal != 600 {
		t.Fatalf("treasury = %d", bal)
	}
	if bal, _ := store.Balance("audits"); bal != 350 {
		t.Fatalf("audits = %d", bal)
	}
	if bal, _ := store.Balance("ghost"); bal != 0 {
		t.Fatalf("unknown account = %d, want 0", bal)
	}
}

func TestExecuteIdempotent(t *testing.T) {
	store, _ := openTempStore(t, 1000)
	defer store.Close()
	mustRegister(t, store, "gip-1", 100, "transfer:a:500")

	first, err := store.Execute("gip-1", 100)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// 即使传入不同时间，也返回首次凭据，不重新扣款。
	second, err := store.Execute("gip-1", 999999)
	if err != nil {
		t.Fatalf("retry Execute: %v", err)
	}
	if second.ExecutedAt != first.ExecutedAt || second.Order != first.Order {
		t.Fatalf("receipt changed: first=%+v second=%+v", first, second)
	}
	if second.Actions[0].Treasury.Before != 1000 || second.Actions[0].Treasury.After != 500 {
		t.Fatalf("retry replayed transfers: %+v", second.Actions[0])
	}
	if bal, _ := store.TreasuryBalance(); bal != 500 {
		t.Fatalf("treasury changed on retry: %d", bal)
	}
	// 重试不追加凭据。
	receipts, _ := store.Receipts()
	if len(receipts) != 1 {
		t.Fatalf("receipt count = %d, want 1", len(receipts))
	}
	if got, ok, _ := store.Receipt("gip-1"); !ok || got.ExecutedAt != 100 {
		t.Fatalf("Receipt lookup failed: %+v %v", got, ok)
	}
	record, ok, _ := store.Proposal("gip-1")
	if !ok || record.State != "executed" {
		t.Fatalf("proposal state = %+v %v", record, ok)
	}
}

func TestExecuteFailures(t *testing.T) {
	store, _ := openTempStore(t, 1000)
	defer store.Close()

	if _, err := store.Execute("missing", 1000); !errors.Is(err, ErrProposalNotFound) {
		t.Fatalf("missing proposal err=%v", err)
	}

	mustRegister(t, store, "empty", 0)
	if _, err := store.Execute("empty", 1000); !errors.Is(err, ErrExecutionRejected) ||
		!strings.Contains(err.Error(), "no actions") {
		t.Fatalf("empty actions err=%v", err)
	}

	// 登记时保留动作原文（含非法格式），执行时报明确原因。
	mustRegister(t, store, "bad", 0, "transfer:a:1", "withdraw:b:2")
	if _, err := store.Execute("bad", 1000); !errors.Is(err, ErrInvalidAction) {
		t.Fatalf("bad action err=%v, want ErrInvalidAction", err)
	}
	// 失败后整项提案无任何转账。
	if bal, _ := store.TreasuryBalance(); bal != 1000 {
		t.Fatalf("partial transfer after failure: treasury=%d", bal)
	}
	if bal, _ := store.Balance("a"); bal != 0 {
		t.Fatalf("partial transfer after failure: a=%d", bal)
	}
	record, _, _ := store.Proposal("bad")
	if record.State != "passed" {
		t.Fatalf("state changed after failure: %s", record.State)
	}
	// 失败不消耗执行机会：修正时间语义后仍可执行（这里登记另一提案验证可继续）。
	mustRegister(t, store, "ok", 0, "transfer:a:1")
	if _, err := store.Execute("ok", 1000); err != nil {
		t.Fatalf("execute after previous failure: %v", err)
	}

	// 余额不足：第一条动作本身超余额。
	mustRegister(t, store, "broke", 0, "transfer:a:2000")
	if _, err := store.Execute("broke", 1000); !errors.Is(err, ErrExecutionRejected) ||
		!strings.Contains(err.Error(), "insufficient") {
		t.Fatalf("insufficient err=%v", err)
	}
	// 余额不足发生在中间动作：前一条不得留下部分转账。
	mustRegister(t, store, "mid", 0, "transfer:mida:600", "transfer:midb:500")
	if _, err := store.Execute("mid", 1000); err == nil ||
		!strings.Contains(err.Error(), "action 1") {
		t.Fatalf("mid-insufficient err=%v", err)
	}
	if bal, _ := store.Balance("mida"); bal != 0 {
		t.Fatalf("partial transfer: mida=%d", bal)
	}
	if bal, _ := store.Balance("midb"); bal != 0 {
		t.Fatalf("partial transfer: midb=%d", bal)
	}
	if bal, _ := store.TreasuryBalance(); bal != 999 { // 只有 ok 提案扣过 1
		t.Fatalf("treasury after failed mid = %d, want 999", bal)
	}
}

func TestExecuteOverflow(t *testing.T) {
	store, _ := openTempStore(t, math.MaxInt64)
	defer store.Close()
	// 两笔收款打到同一账户，合计超过 MaxInt64：第二条拒绝，且第一条不落盘。
	mustRegister(t, store, "gip-over", 0, "transfer:a:9223372036854775000", "transfer:a:900")
	_, err := store.Execute("gip-over", 0)
	if !errors.Is(err, ErrExecutionRejected) || !strings.Contains(err.Error(), "overflow") {
		t.Fatalf("overflow err=%v", err)
	}
	if bal, _ := store.TreasuryBalance(); bal != math.MaxInt64 {
		t.Fatalf("treasury changed after overflow: %d", bal)
	}
	if bal, _ := store.Balance("a"); bal != 0 {
		t.Fatalf("recipient changed after overflow: %d", bal)
	}
}

func TestReceiptOrderAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "treasury.json")
	store, err := InitTreasury(path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	mustRegister(t, store, "gip-b", 0, "transfer:b:100")
	mustRegister(t, store, "gip-a", 0, "transfer:a:200")
	if _, err := store.Execute("gip-b", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Execute("gip-a", 2); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// 重新打开：余额、状态、凭据均可继续查询；已执行提案重试返回原凭据。
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if bal, _ := reopened.TreasuryBalance(); bal != 700 {
		t.Fatalf("treasury after reopen = %d", bal)
	}
	if bal, _ := reopened.Balance("a"); bal != 200 {
		t.Fatalf("a after reopen = %d", bal)
	}
	receipts, err := reopened.Receipts()
	if err != nil || len(receipts) != 2 {
		t.Fatalf("receipts after reopen: %v %v", receipts, err)
	}
	// 按成功提交先后，而非编号字典序。
	if receipts[0].ProposalID != "gip-b" || receipts[0].Order != 0 ||
		receipts[1].ProposalID != "gip-a" || receipts[1].Order != 1 {
		t.Fatalf("receipt order wrong: %s then %s", receipts[0].ProposalID, receipts[1].ProposalID)
	}
	again, err := reopened.Execute("gip-b", 123456)
	if err != nil {
		t.Fatal(err)
	}
	if again.ExecutedAt != 1 || again.Order != 0 {
		t.Fatalf("retry after reopen returned different receipt: %+v", again)
	}
	// 未执行提案仍可在重开后首次执行。
	mustRegister(t, reopened, "gip-c", 50, "transfer:c:1")
	if _, err := reopened.Execute("gip-c", 50); err != nil {
		t.Fatalf("execute new proposal after reopen: %v", err)
	}
}

func TestCorruptStateRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "treasury.json")
	store, err := InitTreasury(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	mustRegister(t, store, "gip-1", 0, "transfer:a:10")
	if _, err := store.Execute("gip-1", 0); err != nil {
		t.Fatal(err)
	}
	store.Close()

	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	corruptions := map[string][]byte{
		"truncated":      good[:len(good)/2],
		"garbage":        []byte("not json at all"),
		"empty":          []byte(""),
		"bad magic":      bytesReplaceAll(good, []byte(stateMagic), []byte("something-else")),
		"dup key":        dupKeyInject(good),
		"trailing json":  append(append([]byte{}, good...), []byte(" {}")...),
		"balance tamper": bytesReplaceAll(good, []byte("\"treasury\": 90"), []byte("\"treasury\": 9999")),
	}
	for name, raw := range corruptions {
		t.Run(name, func(t *testing.T) {
			bad := filepath.Join(dir, "corrupt-"+name+".json")
			if err := os.WriteFile(bad, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			s, err := Open(bad)
			if !errors.Is(err, ErrStateCorrupt) {
				if s != nil {
					s.Close()
				}
				t.Fatalf("Open corrupt(%s) err=%v, want ErrStateCorrupt", name, err)
			}
			// 损坏文件没有被覆盖或重建。
			if got, rerr := os.ReadFile(bad); rerr != nil || (len(raw) > 0 && !equalBytes(got, raw)) {
				t.Fatalf("corrupt file was modified for %s", name)
			}
			// 对损坏文件初始化也必须拒绝（文件已存在）。
			if _, err := InitTreasury(bad, 0); !errors.Is(err, ErrTreasuryAlreadyInit) {
				t.Fatalf("InitTreasury over corrupt err=%v", err)
			}
		})
	}
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func bytesReplaceAll(src, old, new []byte) []byte {
	return []byte(strings.ReplaceAll(string(src), string(old), string(new)))
}

// dupKeyInject 在 balances 对象里注入一个重复键。
func dupKeyInject(good []byte) []byte {
	s := string(good)
	marker := "\"balances\": {"
	idx := strings.Index(s, marker)
	if idx < 0 {
		// 空 balances 被渲染成 {}，改为含重复键的对象。
		s = strings.Replace(s, "\"balances\": {}", "\"balances\": {\"x\": 1, \"x\": 2}", 1)
		return []byte(s)
	}
	insertAt := idx + len(marker)
	return []byte(s[:insertAt] + "\"dup\": 1, " + s[insertAt:])
}

func TestSeparateStateFilesIsolated(t *testing.T) {
	dir := t.TempDir()
	s1, err := InitTreasury(filepath.Join(dir, "a.json"), 100)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := InitTreasury(filepath.Join(dir, "b.json"), 200)
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Close()
	defer s2.Close()
	mustRegister(t, s1, "gip-1", 0, "transfer:x:100")
	if _, err := s1.Execute("gip-1", 0); err != nil {
		t.Fatal(err)
	}
	if bal, _ := s2.TreasuryBalance(); bal != 200 {
		t.Fatalf("state files leaked: s2 treasury=%d", bal)
	}
	if _, err := s2.Execute("gip-1", 0); !errors.Is(err, ErrProposalNotFound) {
		t.Fatalf("proposal leaked across files: %v", err)
	}
}

func TestConcurrentSameProposalOneReceipt(t *testing.T) {
	store, _ := openTempStore(t, 10000)
	defer store.Close()
	mustRegister(t, store, "gip-1", 0, "transfer:a:5")

	const n = 32
	var wg sync.WaitGroup
	results := make([]error, n)
	receipts := make([]*Receipt, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			receipts[i], results[i] = store.Execute("gip-1", int64(100+i))
		}(i)
	}
	close(start)
	wg.Wait()
	successes := 0
	for i, err := range results {
		if err != nil {
			t.Errorf("concurrent execute %d: %v", i, err)
			continue
		}
		successes++
		if receipts[i].ExecutedAt != receipts[0].ExecutedAt || receipts[i].Order != 0 {
			t.Errorf("receipt mismatch at %d: %+v vs %+v", i, receipts[i], receipts[0])
		}
	}
	if successes != n {
		t.Fatalf("successes=%d, want %d (retries must return the same receipt, not error)", successes, n)
	}
	all, _ := store.Receipts()
	if len(all) != 1 {
		t.Fatalf("receipt count = %d, want 1", len(all))
	}
	if bal, _ := store.TreasuryBalance(); bal != 9995 {
		t.Fatalf("treasury = %d, want 9995", bal)
	}
}

func TestConcurrentDifferentProposalsSpendCommittedBalance(t *testing.T) {
	store, _ := openTempStore(t, 100)
	defer store.Close()
	const n = 40
	// 每个提案花 10，资金库只够 10 个成功。
	for i := 0; i < n; i++ {
		mustRegister(t, store, "gip-"+itoa(i), 0, "transfer:a:10")
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	successes := make([]bool, n)
	var counterMu sync.Mutex
	successCount := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := store.Execute("gip-"+itoa(i), 0)
			if err == nil {
				counterMu.Lock()
				successes[i] = true
				successCount++
				counterMu.Unlock()
			} else if !errors.Is(err, ErrExecutionRejected) || !strings.Contains(err.Error(), "insufficient") {
				t.Errorf("proposal %d unexpected err: %v", i, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if successCount != 10 {
		t.Fatalf("successCount=%d, want exactly 10", successCount)
	}
	if bal, _ := store.TreasuryBalance(); bal != 0 {
		t.Fatalf("treasury=%d, want 0", bal)
	}
	if bal, _ := store.Balance("a"); bal != 100 {
		t.Fatalf("a=%d, want 100", bal)
	}
	receipts, _ := store.Receipts()
	if len(receipts) != 10 {
		t.Fatalf("receipts=%d, want 10", len(receipts))
	}
}

func TestBalanceSnapshotMatchesCommittedState(t *testing.T) {
	store, _ := openTempStore(t, 1000)
	defer store.Close()

	snap, err := store.BalanceSnapshot()
	if err != nil {
		t.Fatalf("BalanceSnapshot: %v", err)
	}
	if snap.Treasury != 1000 || len(snap.Balances) != 0 {
		t.Fatalf("initial snapshot = %+v, want treasury=1000 empty balances", snap)
	}
	if snap.Balances == nil {
		t.Fatal("Balances must be a non-nil empty map")
	}

	mustRegister(t, store, "gip-1", 0, "transfer:audits:100", "transfer:legal:50")
	if _, err := store.Execute("gip-1", 10); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// 已取得的快照不受后续执行影响；新一次查询看到新的已提交状态。
	if snap.Treasury != 1000 || len(snap.Balances) != 0 {
		t.Fatalf("snapshot mutated by later execute: %+v", snap)
	}
	next, err := store.BalanceSnapshot()
	if err != nil {
		t.Fatalf("BalanceSnapshot after execute: %v", err)
	}
	if next.Treasury != 850 || next.Balances["audits"] != 100 || next.Balances["legal"] != 50 || len(next.Balances) != 2 {
		t.Fatalf("post-execute snapshot = %+v, want treasury=850 audits=100 legal=50", next)
	}
	// 返回的是副本：改写不影响后续查询。
	next.Balances["audits"] = -1
	again, _ := store.BalanceSnapshot()
	if again.Balances["audits"] != 100 {
		t.Fatalf("snapshot shares state with store: audits=%d", again.Balances["audits"])
	}
	// 未出现过的账户余额为 0，且快照查询不新增账户记录。
	if again.Balances["ghost"] != 0 {
		t.Fatalf("ghost balance = %d, want 0", again.Balances["ghost"])
	}
	if _, ok := again.Balances["ghost"]; ok {
		t.Fatal("snapshot query must not create account entries")
	}
}

func TestBalanceSnapshotConsistentUnderConcurrentExecute(t *testing.T) {
	store, _ := openTempStore(t, 1000)
	defer store.Close()
	// 一项提案按顺序转 audits 100、legal 50；快照只允许出现
	// “转账前”或“转账后”两种完整组合，不得出现中间态。
	mustRegister(t, store, "gip-1", 0, "transfer:audits:100", "transfer:legal:50")

	var wg sync.WaitGroup
	start := make(chan struct{})
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		_, _ = store.Execute("gip-1", 0)
		close(stop)
	}()
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for {
				select {
				case <-stop:
					return
				default:
				}
				snap, err := store.BalanceSnapshot()
				if err != nil {
					t.Errorf("BalanceSnapshot: %v", err)
					return
				}
				pre := snap.Treasury == 1000 && len(snap.Balances) == 0
				post := snap.Treasury == 850 && len(snap.Balances) == 2 &&
					snap.Balances["audits"] == 100 && snap.Balances["legal"] == 50
				if !pre && !post {
					t.Errorf("inconsistent snapshot: %+v", snap)
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
}

func TestBalanceSnapshotSeesCrossHandleCommits(t *testing.T) {
	// 同一状态文件的另一个句柄提交的转账，下一次快照必须可见。
	store, path := openTempStore(t, 1000)
	defer store.Close()
	mustRegister(t, store, "gip-1", 0, "transfer:audits:100")

	other, err := Open(path)
	if err != nil {
		t.Fatalf("Open second handle: %v", err)
	}
	defer other.Close()
	if _, err := other.Execute("gip-1", 0); err != nil {
		t.Fatalf("Execute via second handle: %v", err)
	}

	snap, err := store.BalanceSnapshot()
	if err != nil {
		t.Fatalf("BalanceSnapshot: %v", err)
	}
	if snap.Treasury != 900 || snap.Balances["audits"] != 100 {
		t.Fatalf("snapshot = %+v, want treasury=900 audits=100", snap)
	}
}

func TestProposalsSnapshotConsistentUnderConcurrentExecute(t *testing.T) {
	store, _ := openTempStore(t, 1000)
	defer store.Close()
	// 登记提案与投票提案最初都为 passed；另一个句柄先执行登记提案、
	// 再执行投票提案。并发的列表快照只允许出现“都未执行”“仅登记提案
	// 已执行”“都已执行”三种完整组合，不得出现登记提案仍为 passed
	// 而投票提案已 executed 的混杂状态。
	mustRegister(t, store, "reg-1", 0, "transfer:audits:100")
	voteIn := baseVoteInput("vote-1")
	voteIn.Actions = []string{"transfer:legal:50"}
	mustCreateVote(t, store, voteIn)
	if _, err := store.CastVote("vote-1", "alice", true, 100); err != nil {
		t.Fatalf("CastVote: %v", err)
	}
	if _, err := store.CastVote("vote-1", "dave", true, 100); err != nil {
		t.Fatalf("CastVote: %v", err)
	}
	if _, err := store.TallyVote("vote-1", 200); err != nil {
		t.Fatalf("TallyVote: %v", err)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		if _, err := store.Execute("reg-1", 0); err != nil {
			t.Errorf("Execute reg-1: %v", err)
			return
		}
		_, _ = store.Execute("vote-1", 300)
		close(stop)
	}()
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for {
				select {
				case <-stop:
					return
				default:
				}
				snap, err := store.ProposalsSnapshot()
				if err != nil {
					t.Errorf("ProposalsSnapshot: %v", err)
					return
				}
				if len(snap.Registered) != 1 || len(snap.Voting) != 1 {
					t.Errorf("snapshot sizes = %d registered, %d voting; want 1 and 1",
						len(snap.Registered), len(snap.Voting))
					return
				}
				regState := snap.Registered[0].State
				voteState := snap.Voting[0].State
				if regState == "passed" && voteState == "executed" {
					t.Errorf("inconsistent snapshot: registered=%s voting=%s", regState, voteState)
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
}

func TestProposalsSnapshotEmptyAndLegacyState(t *testing.T) {
	// 没有提案时两个列表都为空（非 nil）。
	store, _ := openTempStore(t, 1000)
	defer store.Close()
	snap, err := store.ProposalsSnapshot()
	if err != nil {
		t.Fatalf("ProposalsSnapshot: %v", err)
	}
	if snap.Registered == nil || snap.Voting == nil || len(snap.Registered) != 0 || len(snap.Voting) != 0 {
		t.Fatalf("empty snapshot = %+v, want two empty non-nil lists", snap)
	}

	// 旧状态文件缺少 vote_proposals 部分时仍可列出登记提案。
	path := filepath.Join(t.TempDir(), "legacy.json")
	legacy := `{
  "magic": "govflow-treasury-state",
  "version": 1,
  "initial_treasury": 500,
  "treasury": 500,
  "balances": {},
  "proposals": {"gip-1": {"id": "gip-1", "state": "passed", "timelock_end": 0, "actions": ["transfer:audits:100"]}},
  "receipts": []
}
`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatalf("write legacy state: %v", err)
	}
	old, err := Open(path)
	if err != nil {
		t.Fatalf("Open legacy state: %v", err)
	}
	defer old.Close()
	snap, err = old.ProposalsSnapshot()
	if err != nil {
		t.Fatalf("ProposalsSnapshot on legacy state: %v", err)
	}
	if len(snap.Registered) != 1 || snap.Registered[0].ID != "gip-1" || len(snap.Voting) != 0 {
		t.Fatalf("legacy snapshot = %+v, want only gip-1 registered", snap)
	}
}

func TestPathSpellingsShareLock(t *testing.T) {
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)

	// 相对拼写初始化。
	store, err := InitTreasury("treasury.json", 10)
	if err != nil {
		t.Fatal(err)
	}
	mustRegister(t, store, "gip-1", 0, "transfer:a:10")
	if _, err := store.Execute("gip-1", 0); err != nil {
		t.Fatal(err)
	}
	store.Close()

	// 绝对拼写重新打开，并验证相对/绝对命中同一把进程内锁。
	abs := filepath.Join(dir, "treasury.json")
	reopened, err := Open(abs)
	if err != nil {
		t.Fatalf("open via absolute path: %v", err)
	}
	defer reopened.Close()
	if bal, _ := reopened.TreasuryBalance(); bal != 0 {
		t.Fatalf("balance through absolute path = %d, want 0", bal)
	}
	relKey, err := canonicalPath("treasury.json")
	if err != nil {
		t.Fatal(err)
	}
	absKey, err := canonicalPath(abs)
	if err != nil {
		t.Fatal(err)
	}
	if relKey != absKey {
		t.Fatalf("canonical paths differ: %q vs %q", relKey, absKey)
	}
	if inProcessLock(relKey) != inProcessLock(absKey) {
		t.Fatal("relative and absolute spellings resolved to different in-process locks")
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

// TestNoPartialReadsDuringWrite 通过检查文件始终是合法 JSON 来观察原子性。
func TestAtomicFileIsAlwaysValid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "treasury.json")
	store, err := InitTreasury(path, 100000)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for i := 0; i < 200; i++ {
		id := "gip-" + itoa(i)
		mustRegister(t, store, id, 0, "transfer:a:1")
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // 写入方
		defer wg.Done()
		for i := 200; i < 400; i++ {
			select {
			case <-stop:
				return
			default:
			}
			id := "gip-" + itoa(i)
			if existed, err := store.Register(id, 0, []string{"transfer:a:1"}); err == nil && !existed {
				_, _ = store.Execute(id, 0)
			}
		}
	}()
	go func() { // 读取方：任何时刻打开都只能看到完整状态
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			s, err := Open(path)
			if err != nil {
				if !errors.Is(err, ErrStateCorrupt) {
					t.Errorf("reader unexpected err: %v", err)
				}
				// rename 之间不应存在损坏窗口；若观察到损坏说明非原子。
				t.Errorf("observed non-atomic state file: %v", err)
				return
			}
			if _, err := s.TreasuryBalance(); err != nil {
				t.Errorf("read balance: %v", err)
			}
			s.Close()
		}
	}()
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}
