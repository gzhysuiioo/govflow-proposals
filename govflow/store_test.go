package govflow

import (
	"encoding/json"
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

// TestTransferParityAcrossSourcesAndReplay：登记来源与投票通过来源的提案，
// 只要动作原文序列相同，成功执行产生的逐笔资金变动（资金库与收款账户前后余额）
// 与最终余额就必须一致；重新打开时凭据重放走同一条转账规则，结果仍一致。
func TestTransferParityAcrossSourcesAndReplay(t *testing.T) {
	actions := []string{"transfer:audits:250", "transfer:audits:100", "transfer:legal:50"}

	run := func(t *testing.T, viaVote bool) (*Receipt, *BalanceSnapshot) {
		store, _ := openTempStore(t, 1000)
		t.Cleanup(func() { store.Close() })
		if viaVote {
			in := baseVoteInput("gip-v")
			in.Actions = actions
			mustCreateVote(t, store, in)
			if _, err := store.CastVote("gip-v", "alice", true, 150); err != nil {
				t.Fatal(err)
			}
			if _, err := store.CastVote("gip-v", "dave", true, 150); err != nil {
				t.Fatal(err)
			}
			if _, err := store.TallyVote("gip-v", 200); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Execute("gip-v", 300); err != nil {
				t.Fatalf("execute vote proposal: %v", err)
			}
		} else {
			mustRegister(t, store, "gip-r", 300, actions...)
			if _, err := store.Execute("gip-r", 300); err != nil {
				t.Fatalf("execute registered proposal: %v", err)
			}
		}
		id := "gip-r"
		if viaVote {
			id = "gip-v"
		}
		r, ok, err := store.Receipt(id)
		if err != nil || !ok {
			t.Fatalf("receipt lookup: ok=%v err=%v", ok, err)
		}
		snap, err := store.BalanceSnapshot()
		if err != nil {
			t.Fatal(err)
		}
		return r, snap
	}

	regReceipt, regSnap := run(t, false)
	voteReceipt, voteSnap := run(t, true)
	if len(regReceipt.Actions) != len(voteReceipt.Actions) {
		t.Fatalf("action receipt length differs: %d vs %d", len(regReceipt.Actions), len(voteReceipt.Actions))
	}
	for i := range regReceipt.Actions {
		a, b := regReceipt.Actions[i], voteReceipt.Actions[i]
		if a.Action != b.Action || a.Index != b.Index ||
			a.Treasury != b.Treasury || a.Recipient != b.Recipient {
			t.Fatalf("action %d movement differs by source:\nregister=%+v\nvote=%+v", i, a, b)
		}
	}
	if regSnap.Treasury != voteSnap.Treasury || regSnap.Treasury != 600 {
		t.Fatalf("treasury differs: register=%d vote=%d, want 600", regSnap.Treasury, voteSnap.Treasury)
	}
	for acct, want := range map[string]int64{"audits": 350, "legal": 50} {
		if regSnap.Balances[acct] != want || voteSnap.Balances[acct] != want {
			t.Fatalf("account %s differs: register=%d vote=%d, want %d",
				acct, regSnap.Balances[acct], voteSnap.Balances[acct], want)
		}
	}

	// 端到端：金额恰好等于资金库剩余余额可以成功，重开后凭据与余额一致。
	path := filepath.Join(t.TempDir(), "treasury.json")
	store, err := InitTreasury(path, 250)
	if err != nil {
		t.Fatal(err)
	}
	mustRegister(t, store, "gip-exact", 0, "transfer:a:100", "transfer:b:150")
	r, err := store.Execute("gip-exact", 0)
	if err != nil {
		t.Fatalf("exact-remaining execute: %v", err)
	}
	if r.Actions[1].Treasury.Before != 150 || r.Actions[1].Treasury.After != 0 {
		t.Fatalf("final draw should take treasury 150->0: %+v", r.Actions[1].Treasury)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path) // 凭据重放走同一条转账规则：文件必须仍判为合法
	if err != nil {
		t.Fatalf("reopen after exact-remaining spend: %v", err)
	}
	defer reopened.Close()
	if bal, _ := reopened.TreasuryBalance(); bal != 0 {
		t.Fatalf("treasury after reopen = %d, want 0", bal)
	}
	if bal, _ := reopened.Balance("b"); bal != 150 {
		t.Fatalf("b after reopen = %d, want 150", bal)
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

// removeLineContaining 删除文本中包含子串的整行（含行尾换行）。
func removeLineContaining(s, substr string) string {
	idx := strings.Index(s, substr)
	if idx < 0 {
		return s
	}
	start := strings.LastIndex(s[:idx], "\n") + 1
	end := strings.Index(s[idx:], "\n")
	if end < 0 {
		return s[:start]
	}
	return s[:start] + s[idx+end+1:]
}

// TestReceiptExecutedAtValidated：保存凭据的 executed_at 与首次执行受同一条
// 执行资格规则约束——必须明确写出且不得早于所属提案的 timelock_end；
// 缺失、null、类型不符或早于时间锁的凭据让整份状态文件判为损坏。
func TestReceiptExecutedAtValidated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "treasury.json")
	store, err := InitTreasury(path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	mustRegister(t, store, "gip-1", 5000, "transfer:a:100")
	// 时间锁为 0、在 0 执行的旧凭据：明确写出的 0 是合法执行时间。
	mustRegister(t, store, "gip-zero", 0, "transfer:z:50")
	// 登记提案的负时间取值保持兼容。
	mustRegister(t, store, "gip-neg", -100, "transfer:n:1")
	if _, err := store.Execute("gip-1", 5001); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Execute("gip-zero", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Execute("gip-neg", -50); err != nil {
		t.Fatal(err)
	}
	// 投票通过的提案走到 executed：恰好等于时间锁的执行时间有效。
	mustCreateVote(t, store, baseVoteInput("gip-vote"))
	if _, err := store.CastVote("gip-vote", "alice", true, 150); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CastVote("gip-vote", "dave", true, 150); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TallyVote("gip-vote", 200); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Execute("gip-vote", 300); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 基线可读：合法凭据（含明确写出的 0 与负时间）照常返回。
	t.Run("baseline readable", func(t *testing.T) {
		s, err := Open(path)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer s.Close()
		r, ok, err := s.Receipt("gip-zero")
		if err != nil || !ok || r.ExecutedAt != 0 {
			t.Fatalf("Receipt(gip-zero) = %+v ok=%v err=%v, want executed_at 0", r, ok, err)
		}
		r, ok, err = s.Receipt("gip-neg")
		if err != nil || !ok || r.ExecutedAt != -50 {
			t.Fatalf("Receipt(gip-neg) = %+v ok=%v err=%v, want executed_at -50", r, ok, err)
		}
		// 合法已执行提案用更早的 now 重试：仍返回首次凭据，不重新扣款、
		// 不改写首次执行时间与凭据顺序。
		again, err := s.Execute("gip-1", 4000)
		if err != nil {
			t.Fatalf("retry with earlier now: %v", err)
		}
		if again.ExecutedAt != 5001 || again.Order != 0 {
			t.Fatalf("retry rewrote first receipt: %+v", again)
		}
		if bal, _ := s.TreasuryBalance(); bal != 749 {
			t.Fatalf("treasury after retry = %d, want 749", bal)
		}
		receipts, _ := s.Receipts()
		if len(receipts) != 4 || receipts[0].ProposalID != "gip-1" {
			t.Fatalf("receipts changed on retry: %v", receipts)
		}
	})

	// 时间恰好等于时间锁仍然有效。
	t.Run("equal to timelock readable", func(t *testing.T) {
		raw := strings.Replace(string(good), `"executed_at": 5001`, `"executed_at": 5000`, 1)
		p := filepath.Join(dir, "equal.json")
		if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		s, err := Open(p)
		if err != nil {
			t.Fatalf("executed_at == timelock_end rejected: %v", err)
		}
		defer s.Close()
		r, ok, _ := s.Receipt("gip-1")
		if !ok || r.ExecutedAt != 5000 {
			t.Fatalf("Receipt(gip-1) = %+v ok=%v", r, ok)
		}
	})

	corruptions := []struct {
		name    string
		raw     string
		wantMsg []string
	}{
		{
			name:    "executed_at before timelock",
			raw:     strings.Replace(string(good), `"executed_at": 5001`, `"executed_at": 4999`, 1),
			wantMsg: []string{`receipt 0`, `"gip-1"`, "4999", "5000", "before timelock_end"},
		},
		{
			name:    "executed_at missing",
			raw:     removeLineContaining(string(good), `"executed_at": 5001`),
			wantMsg: []string{`receipt 0`, `"gip-1"`, "executed_at", "missing"},
		},
		{
			name:    "executed_at null",
			raw:     strings.Replace(string(good), `"executed_at": 5001`, `"executed_at": null`, 1),
			wantMsg: []string{`receipt 0`, `"gip-1"`, "null"},
		},
		{
			name:    "executed_at wrong type",
			raw:     strings.Replace(string(good), `"executed_at": 5001`, `"executed_at": "5001"`, 1),
			wantMsg: []string{`receipt 0`, `"gip-1"`, "wrong type"},
		},
		{
			// 投票通过的提案与登记提案使用一致的判断。
			name:    "vote proposal executed_at before timelock",
			raw:     strings.Replace(string(good), `"executed_at": 300`, `"executed_at": 299`, 1),
			wantMsg: []string{`"gip-vote"`, "299", "300", "before timelock_end"},
		},
		{
			name:    "negative timelock executed_at before timelock",
			raw:     strings.Replace(string(good), `"executed_at": -50`, `"executed_at": -101`, 1),
			wantMsg: []string{`"gip-neg"`, "-101", "-100", "before timelock_end"},
		},
	}
	for _, tc := range corruptions {
		t.Run(tc.name, func(t *testing.T) {
			bad := filepath.Join(dir, "corrupt-"+strings.ReplaceAll(tc.name, " ", "-")+".json")
			if err := os.WriteFile(bad, []byte(tc.raw), 0o600); err != nil {
				t.Fatal(err)
			}
			// 文件中其余提案与凭据均正常，也不能掩盖一条非法凭据。
			s, err := Open(bad)
			if !errors.Is(err, ErrStateCorrupt) {
				if s != nil {
					s.Close()
				}
				t.Fatalf("Open err=%v, want ErrStateCorrupt", err)
			}
			for _, want := range tc.wantMsg {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not locate %q", err.Error(), want)
				}
			}
			// 原状态文件保持原样，不被修复或覆盖。
			got, rerr := os.ReadFile(bad)
			if rerr != nil || string(got) != tc.raw {
				t.Fatalf("corrupt file was modified")
			}
		})
	}

	// 已打开资金库之后的查询与执行同样识别该问题：文件在打开后被替换成
	// 含非法凭据的版本，后续每个操作都必须报损坏，且不得把非法凭据对应的
	// 提案当作未执行再扣一次款。
	t.Run("detected after open", func(t *testing.T) {
		p := filepath.Join(dir, "swap.json")
		if err := os.WriteFile(p, good, 0o600); err != nil {
			t.Fatal(err)
		}
		s, err := Open(p)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		corrupt := strings.Replace(string(good), `"executed_at": 5001`, `"executed_at": 4999`, 1)
		if err := os.WriteFile(p, []byte(corrupt), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Receipts(); !errors.Is(err, ErrStateCorrupt) {
			t.Fatalf("Receipts err=%v, want ErrStateCorrupt", err)
		}
		if _, _, err := s.Receipt("gip-zero"); !errors.Is(err, ErrStateCorrupt) {
			t.Fatalf("Receipt err=%v, want ErrStateCorrupt", err)
		}
		if _, err := s.BalanceSnapshot(); !errors.Is(err, ErrStateCorrupt) {
			t.Fatalf("BalanceSnapshot err=%v, want ErrStateCorrupt", err)
		}
		if _, err := s.Execute("gip-1", 5001); !errors.Is(err, ErrStateCorrupt) {
			t.Fatalf("Execute err=%v, want ErrStateCorrupt", err)
		}
		got, _ := os.ReadFile(p)
		if string(got) != corrupt {
			t.Fatalf("state file was modified by rejected operations")
		}
	})
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

// TestFixedFieldNamesMustMatchExactly 固定字段名必须与保存格式逐字一致：
// 大小写变体（Treasury、Support 等）、同一对象内的大小写双胞胎（state 与 State，
// 即使两个值相同）、转义写法的重复键，都导致整个文件被拒绝；
// 业务编号（账户、提案编号）的大小写变体是不同编号，不得被合并或拒绝。
func TestFixedFieldNamesMustMatchExactly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "treasury.json")
	store, err := InitTreasury(path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	// 两个只差大小写的提案编号与收款账户：合法数据，必须都能正常读回。
	mustRegister(t, store, "GIP-1", 0, "transfer:Audit:100")
	mustRegister(t, store, "gip-1", 0, "transfer:audit:50")
	if _, err := store.Execute("GIP-1", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Execute("gip-1", 0); err != nil {
		t.Fatal(err)
	}
	mustCreateVote(t, store, baseVoteInput("gip-vote"))
	if _, err := store.CastVote("gip-vote", "alice", true, 150); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TallyVote("gip-vote", 200); err != nil {
		t.Fatal(err)
	}
	store.Close()

	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 正常文件（含大小写不同的业务编号）原样可读。
	assertReadable := func(t *testing.T, raw []byte) {
		t.Helper()
		p := filepath.Join(dir, strings.ReplaceAll(t.Name(), "/", "-")+".json")
		if err := os.WriteFile(p, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		s, err := Open(p)
		if err != nil {
			t.Fatalf("readable variant rejected: %v", err)
		}
		defer s.Close()
		if got, err := s.Balance("Audit"); err != nil || got != 100 {
			t.Fatalf("Balance(Audit)=%d err=%v, want 100", got, err)
		}
		if got, err := s.Balance("audit"); err != nil || got != 50 {
			t.Fatalf("Balance(audit)=%d err=%v, want 50", got, err)
		}
		if _, ok, err := s.Proposal("GIP-1"); err != nil || !ok {
			t.Fatalf("Proposal(GIP-1) ok=%v err=%v", ok, err)
		}
		if _, ok, err := s.Proposal("gip-1"); err != nil || !ok {
			t.Fatalf("Proposal(gip-1) ok=%v err=%v", ok, err)
		}
	}

	t.Run("baseline", func(t *testing.T) { assertReadable(t, good) })

	t.Run("reordered and reindented", func(t *testing.T) {
		// 字段顺序与缩进不影响读取。
		var asMap map[string]any
		if err := json.Unmarshal(good, &asMap); err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(asMap) // map 重marshal后键序变为字典序、无缩进
		if err != nil {
			t.Fatal(err)
		}
		assertReadable(t, raw)
	})

	t.Run("escaped field names", func(t *testing.T) {
		// 转义拼写与普通拼写表示同一个字段：单独出现正常识别。
		raw := strings.Replace(string(good), `"version": 1`, `"\u0076ersion": 1`, 1)
		raw = strings.Replace(raw, `"timelock_end"`, `"\u0074imelock_end"`, 1)
		assertReadable(t, []byte(raw))
	})

	// ---- 拒绝变体：每个都整文件拒绝，错误指出字段与所在记录，文件不被改写 ----

	mutate := func(fn func(root map[string]any)) []byte {
		var clone map[string]any
		if err := json.Unmarshal(good, &clone); err != nil {
			t.Fatal(err)
		}
		fn(clone)
		raw, err := json.MarshalIndent(clone, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		return append(raw, '\n')
	}
	rename := func(obj map[string]any, old, new string) {
		v, ok := obj[old]
		if !ok {
			t.Fatalf("setup: key %q missing", old)
		}
		obj[new] = v
		delete(obj, old)
	}
	proposal := func(root map[string]any) map[string]any {
		return root["proposals"].(map[string]any)["GIP-1"].(map[string]any)
	}
	vote := func(root map[string]any) map[string]any {
		return root["vote_proposals"].(map[string]any)["gip-vote"].(map[string]any)
	}
	receipt := func(root map[string]any) map[string]any {
		return root["receipts"].([]any)[0].(map[string]any)
	}

	rejected := map[string]struct {
		raw  []byte
		want []string // 错误原因应包含的片段（字段名与所在记录）
	}{
		"top-level Treasury": {
			raw:  mutate(func(root map[string]any) { rename(root, "treasury", "Treasury") }),
			want: []string{`"Treasury"`},
		},
		"proposal State": {
			raw:  mutate(func(root map[string]any) { rename(proposal(root), "state", "State") }),
			want: []string{`"State"`, `proposals["GIP-1"]`},
		},
		"proposal state and State twins": {
			// 同一记录里同时放入 state 与 State，即使值相同也拒绝。
			raw: mutate(func(root map[string]any) {
				p := proposal(root)
				p["State"] = p["state"]
			}),
			want: []string{`"State"`, `proposals["GIP-1"]`},
		},
		"ballot Support": {
			raw: mutate(func(root map[string]any) {
				rename(vote(root)["ballots"].([]any)[0].(map[string]any), "support", "Support")
			}),
			want: []string{`"Support"`, `vote_proposals["gip-vote"].ballots[0]`},
		},
		"tally Tallied_at": {
			raw: mutate(func(root map[string]any) {
				rename(vote(root)["tally"].(map[string]any), "tallied_at", "Tallied_at")
			}),
			want: []string{`"Tallied_at"`, ".tally"},
		},
		"receipt Proposal_ID": {
			raw:  mutate(func(root map[string]any) { rename(receipt(root), "proposal_id", "Proposal_ID") }),
			want: []string{`"Proposal_ID"`, "receipts[0]"},
		},
		"receipt balance Before": {
			raw: mutate(func(root map[string]any) {
				rename(receipt(root)["actions"].([]any)[0].(map[string]any)["treasury"].(map[string]any), "before", "Before")
			}),
			want: []string{`"Before"`, ".treasury"},
		},
		"member Weight": {
			raw: mutate(func(root map[string]any) {
				rename(vote(root)["members"].([]any)[0].(map[string]any), "weight", "Weight")
			}),
			want: []string{`"Weight"`, ".members[0]"},
		},
		"delegation From": {
			raw: mutate(func(root map[string]any) {
				rename(vote(root)["delegations"].([]any)[0].(map[string]any), "from", "From")
			}),
			want: []string{`"From"`, ".delegations[0]"},
		},
		"unknown field still rejected": {
			raw:  mutate(func(root map[string]any) { proposal(root)["comment"] = "x" }),
			want: []string{`"comment"`, `proposals["GIP-1"]`},
		},
		"escaped duplicate state": {
			// 转义拼写与普通拼写是同一个键：同一对象出现两次属重复键。
			raw: []byte(strings.Replace(string(good),
				`"state": "executed"`, `"state": "executed", "st\u0061te": "executed"`, 1)),
			want: []string{`duplicate key "state"`, `proposals["GIP-1"]`},
		},
	}
	for name, tc := range rejected {
		t.Run(name, func(t *testing.T) {
			bad := filepath.Join(dir, "bad-"+strings.ReplaceAll(name, " ", "-")+".json")
			if err := os.WriteFile(bad, tc.raw, 0o600); err != nil {
				t.Fatal(err)
			}
			s, err := Open(bad)
			if !errors.Is(err, ErrStateCorrupt) {
				if s != nil {
					s.Close()
				}
				t.Fatalf("Open err=%v, want ErrStateCorrupt", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q should mention %s", err.Error(), want)
				}
			}
			// 损坏文件不被整理或写回。
			if got, rerr := os.ReadFile(bad); rerr != nil || string(got) != string(tc.raw) {
				t.Fatalf("corrupt file was modified")
			}
		})
	}

	t.Run("reads after corruption", func(t *testing.T) {
		// 打开后文件被改成非法字段：后续读取同样返回状态损坏，不提供部分信息。
		p := filepath.Join(dir, "live.json")
		if err := os.WriteFile(p, good, 0o600); err != nil {
			t.Fatal(err)
		}
		s, err := Open(p)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		bad := mutate(func(root map[string]any) { rename(root, "treasury", "Treasury") })
		if err := os.WriteFile(p, bad, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.BalanceSnapshot(); !errors.Is(err, ErrStateCorrupt) {
			t.Fatalf("BalanceSnapshot on corrupted file err=%v, want ErrStateCorrupt", err)
		}
		if _, _, err := s.Proposal("GIP-1"); !errors.Is(err, ErrStateCorrupt) {
			t.Fatalf("Proposal on corrupted file err=%v, want ErrStateCorrupt", err)
		}
	})
}
