package govflow

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupExecutedStateFile 构造一份含两个已执行提案的状态文件：
// 登记提案 gip-5000 时间锁 5000、在 5001 执行；投票提案 gip-vote
// 时间锁 300、在 300 执行。两份凭据分别位于 receipts[0]、receipts[1]。
func setupExecutedStateFile(t *testing.T) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, "treasury.json")
	store, err := InitTreasury(path, 10000)
	if err != nil {
		t.Fatal(err)
	}
	mustRegister(t, store, "gip-5000", 5000, "transfer:audits:1000")
	if _, err := store.Execute("gip-5000", 5001); err != nil {
		t.Fatal(err)
	}
	// 一项尚未执行的 passed 提案，用于验证文件损坏后不会被“顺手”再扣一次款。
	mustRegister(t, store, "gip-pending", 0, "transfer:legal:7")

	in := baseVoteInput("gip-vote")
	mustCreateVote(t, store, in)
	if _, err := store.CastVote("gip-vote", "alice", true, 150); err != nil {
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
	return dir, path
}

// mutateReceipt 解码状态文件、定位指定提案的凭据并改写其 executed_at，
// 然后返回重新序列化后的整份文件。
func mutateReceipt(t *testing.T, good []byte, proposalID string, mutate func(rcpt map[string]any)) []byte {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(good, &root); err != nil {
		t.Fatal(err)
	}
	receipts := root["receipts"].([]any)
	for _, item := range receipts {
		rcpt := item.(map[string]any)
		if rcpt["proposal_id"] == proposalID {
			mutate(rcpt)
			raw, err := json.MarshalIndent(root, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			return append(raw, '\n')
		}
	}
	t.Fatalf("receipt for %s not found", proposalID)
	return nil
}

func writeVariant(t *testing.T, dir, name string, raw []byte) string {
	t.Helper()
	p := filepath.Join(dir, name+".json")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestReceiptEarlyExecutedAtIsCorrupt 是核心场景：提案时间锁为 5000、已在 5001
// 完成转账，若凭据的执行时间变成 4999——即使提案仍是 executed、资金库与收款
// 账户余额、动作明细全部能对应——整份状态文件也必须判为损坏。
func TestReceiptEarlyExecutedAtIsCorrupt(t *testing.T) {
	dir, path := setupExecutedStateFile(t)
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 边界仍然有效：executed_at == timelock_end（5001 > 5000 本就有效；
	// 这里另起文件验证恰好相等）。
	equalPath := filepath.Join(dir, "equal.json")
	eqStore, err := InitTreasury(equalPath, 100)
	if err != nil {
		t.Fatal(err)
	}
	mustRegister(t, eqStore, "gip-eq", 5000, "transfer:a:10")
	if _, err := eqStore.Execute("gip-eq", 5000); err != nil {
		t.Fatalf("execute exactly at timelock: %v", err)
	}
	eqStore.Close()
	if s, err := Open(equalPath); err != nil {
		t.Fatalf("receipt at exactly timelock_end must stay readable: %v", err)
	} else {
		s.Close()
	}

	cases := map[string]struct {
		proposal string
		mutate   func(rcpt map[string]any)
		want     []string // stderr/错误中必须出现的片段
	}{
		"executed_at before timelock (registered source)": {
			proposal: "gip-5000",
			mutate:   func(r map[string]any) { r["executed_at"] = 4999 },
			want:     []string{"gip-5000", "receipt 0", "executed_at", "4999", "5000", "before"},
		},
		"executed_at before timelock (vote source)": {
			proposal: "gip-vote",
			mutate:   func(r map[string]any) { r["executed_at"] = 299 },
			want:     []string{"gip-vote", "receipt 1", "executed_at", "299", "300", "before"},
		},
		"executed_at missing": {
			proposal: "gip-5000",
			mutate:   func(r map[string]any) { delete(r, "executed_at") },
			want:     []string{"gip-5000", "receipt 0", "executed_at", "missing"},
		},
		"executed_at explicit null": {
			proposal: "gip-5000",
			mutate:   func(r map[string]any) { r["executed_at"] = nil },
			want:     []string{"gip-5000", "receipt 0", "executed_at", "null"},
		},
		"executed_at wrong json type": {
			proposal: "gip-5000",
			mutate:   func(r map[string]any) { r["executed_at"] = "5001" },
			want:     []string{"gip-5000", "receipt 0", "executed_at", "wrong type", "string"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			raw := mutateReceipt(t, good, tc.proposal, tc.mutate)
			bad := writeVariant(t, dir, "bad-"+strings.ReplaceAll(name, " ", "-"), raw)
			s, err := Open(bad)
			if !errors.Is(err, ErrStateCorrupt) {
				if s != nil {
					s.Close()
				}
				t.Fatalf("Open err=%v, want ErrStateCorrupt", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q should locate %q", err.Error(), want)
				}
			}
			// 损坏文件既不被返回，也不被整理、覆盖或重建。
			if got, rerr := os.ReadFile(bad); rerr != nil || string(got) != string(raw) {
				t.Fatalf("corrupt file was modified")
			}
			// 损坏文件同样不能 init 覆盖。
			if _, err := InitTreasury(bad, 0); !errors.Is(err, ErrTreasuryAlreadyInit) {
				t.Fatalf("InitTreasury over corrupt err=%v", err)
			}
		})
	}
}

// TestCorruptReceiptNotMaskedByOthers：文件中其它正常提案/凭据不得掩盖
// 一条非法凭据；任一凭据非法，整份文件都拒绝打开。
func TestCorruptReceiptNotMaskedByOthers(t *testing.T) {
	dir, path := setupExecutedStateFile(t)
	good, _ := os.ReadFile(path)
	raw := mutateReceipt(t, good, "gip-vote", func(r map[string]any) { r["executed_at"] = 50 })
	bad := writeVariant(t, dir, "mask", raw)
	_, err := Open(bad)
	if !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("Open err=%v, want ErrStateCorrupt", err)
	}
	if !strings.Contains(err.Error(), "receipt 1") || !strings.Contains(err.Error(), "gip-vote") {
		t.Fatalf("error must point at the later bad receipt, got %q", err.Error())
	}
}

// TestExplicitZeroVsMissingExecutedAt：时间锁允许 0 时，明确写出的 0 是
// 可读旧凭据；字段缺失则必须拒绝，二者区别对待，绝不把缺失补成 0。
func TestExplicitZeroVsMissingExecutedAt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "zero.json")
	store, err := InitTreasury(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	mustRegister(t, store, "gip-zero", 0, "transfer:a:10")
	if _, err := store.Execute("gip-zero", 0); err != nil {
		t.Fatal(err)
	}
	store.Close()

	good, _ := os.ReadFile(path)
	if s, err := Open(path); err != nil {
		t.Fatalf("explicit executed_at=0 with timelock 0 must be readable: %v", err)
	} else {
		if r, ok, _ := s.Receipt("gip-zero"); !ok || r.ExecutedAt != 0 {
			t.Fatalf("receipt zero time not preserved: %+v ok=%v", r, ok)
		}
		s.Close()
	}

	missing := mutateReceipt(t, good, "gip-zero", func(r map[string]any) { delete(r, "executed_at") })
	bad := writeVariant(t, dir, "zero-missing", missing)
	_, err = Open(bad)
	if !errors.Is(err, ErrStateCorrupt) || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing executed_at must be corrupt, got %v", err)
	}
}

// TestNegativeTimelockStaysCompatible：登记提案现有的有符号 64 位时间取值
// 保持兼容，不额外限制合法负值；负时间锁下、不早于时间锁执行的凭据可读，
// 而早于该（负）时间锁的凭据仍判损坏。
func TestNegativeTimelockStaysCompatible(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "neg.json")
	store, err := InitTreasury(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	mustRegister(t, store, "gip-neg", -5, "transfer:a:1")
	if _, err := store.Execute("gip-neg", -5); err != nil {
		t.Fatalf("execute at negative time equal to negative timelock: %v", err)
	}
	store.Close()

	if s, err := Open(path); err != nil {
		t.Fatalf("negative timelock receipt must be readable: %v", err)
	} else {
		r, ok, _ := s.Receipt("gip-neg")
		if !ok || r.ExecutedAt != -5 {
			t.Fatalf("negative executed_at not preserved: %+v", r)
		}
		s.Close()
	}

	good, _ := os.ReadFile(path)
	early := mutateReceipt(t, good, "gip-neg", func(r map[string]any) { r["executed_at"] = -6 })
	bad := writeVariant(t, dir, "neg-early", early)
	_, err = Open(bad)
	if !errors.Is(err, ErrStateCorrupt) || !strings.Contains(err.Error(), "-6") {
		t.Fatalf("receipt before negative timelock must be corrupt, got %v", err)
	}
}

// TestAllReadsAndExecuteRejectCorruptReceipt：首次打开之外，已打开资金库之后
// 的查询与执行都必须识别被篡改的执行时间：只报状态文件损坏，不返回成功凭据或
// 部分查询结果，不把 executed 提案当作未执行再扣一次款，原文件保持原样。
func TestAllReadsAndExecuteRejectCorruptReceipt(t *testing.T) {
	_, path := setupExecutedStateFile(t)
	good, _ := os.ReadFile(path)

	store, err := Open(path) // 先以完好文件打开
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	tampered := mutateReceipt(t, good, "gip-5000", func(r map[string]any) { r["executed_at"] = 4999 })
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}

	checks := map[string]func() error{
		"Receipts":          func() error { _, e := store.Receipts(); return e },
		"Receipt(bad)":      func() error { _, _, e := store.Receipt("gip-5000"); return e },
		"Receipt(other)":    func() error { _, _, e := store.Receipt("gip-vote"); return e },
		"BalanceSnapshot":   func() error { _, e := store.BalanceSnapshot(); return e },
		"Balances":          func() error { _, e := store.Balances(); return e },
		"Balance":           func() error { _, e := store.Balance("audits"); return e },
		"TreasuryBalance":   func() error { _, e := store.TreasuryBalance(); return e },
		"Proposals":         func() error { _, e := store.Proposals(); return e },
		"Proposal(bad)":     func() error { _, _, e := store.Proposal("gip-5000"); return e },
		"ProposalsSnapshot": func() error { _, e := store.ProposalsSnapshot(); return e },
		"VoteProposal":      func() error { _, _, e := store.VoteProposal("gip-vote"); return e },
		"VoteProposals":     func() error { _, e := store.VoteProposals(); return e },
	}
	for name, call := range checks {
		if err := call(); !errors.Is(err, ErrStateCorrupt) {
			t.Fatalf("%s on corrupt receipt: err=%v, want ErrStateCorrupt", name, err)
		}
	}

	// 重试已执行提案：不得返回凭据，也不得当作未执行重新扣款。
	if _, err := store.Execute("gip-5000", 5001); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("re-executing executed proposal on corrupt file: %v", err)
	}
	if _, err := store.Execute("gip-5000", 4000); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("re-executing with earlier time on corrupt file: %v", err)
	}
	// 连一项尚未执行的 passed 提案也不得在损坏文件上发生任何扣款。
	if _, err := store.Execute("gip-pending", 0); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("executing an unrelated pending proposal on corrupt file: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != string(tampered) {
		t.Fatalf("corrupt file was modified by queries/execute")
	}
	store.Close()

	// 恢复完好文件后：gip-pending 仍是 passed、未被扣过款，资金库停留在
	// 两次合法执行后的 8900（10000-1000-100），证明损坏期间没有产生部分转账。
	if err := os.WriteFile(path, good, 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if bal, _ := reopened.TreasuryBalance(); bal != 8900 {
		t.Fatalf("treasury changed while file was corrupt: %d, want 8900", bal)
	}
	if rec, ok, _ := reopened.Proposal("gip-pending"); !ok || rec.State != "passed" {
		t.Fatalf("pending proposal changed while file was corrupt: %+v ok=%v", rec, ok)
	}
	if _, ok, _ := reopened.Receipt("gip-pending"); ok {
		t.Fatal("pending proposal gained a receipt during corruption")
	}
}

// TestReExecuteKeepsFirstReceiptDespiteEarlierNow：合法的已执行提案再次执行，
// 即使重试传入更早（甚至早于时间锁）的时间，仍返回首次凭据、不再转账，
// 不重新检查资格，也不改写首次执行时间或凭据顺序。
func TestReExecuteKeepsFirstReceiptDespiteEarlierNow(t *testing.T) {
	store, _ := openTempStore(t, 1000)
	defer store.Close()
	mustRegister(t, store, "gip-1", 5000, "transfer:a:100")
	first, err := store.Execute("gip-1", 5001)
	if err != nil {
		t.Fatal(err)
	}
	for _, now := range []int64{5001, 5000, 4999, 0} {
		again, err := store.Execute("gip-1", now)
		if err != nil {
			t.Fatalf("retry now=%d: %v", now, err)
		}
		if again.ExecutedAt != first.ExecutedAt || again.Order != first.Order {
			t.Fatalf("retry now=%d changed receipt: %+v vs %+v", now, again, first)
		}
	}
	if bal, _ := store.TreasuryBalance(); bal != 900 {
		t.Fatalf("retry charged again: treasury=%d", bal)
	}
	all, _ := store.Receipts()
	if len(all) != 1 {
		t.Fatalf("receipt count=%d, want 1", len(all))
	}
}

// TestCLIRejectsEarlyExecutedAt：命令行对“凭据执行时间早于时间锁”的损坏文件
// 沿用既有域错误输出：所有查询与执行命令都以退出码 1 失败，原因写入 stderr
// 且能定位到提案编号、凭据下标、字段与执行时间（并区分早于时间锁），
// stdout 不出现成功凭据或部分查询结果，原状态文件保持原样。
func TestCLIRejectsEarlyExecutedAt(t *testing.T) {
	binary := buildCLI(t)
	dir, path := setupExecutedStateFile(t)
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// gip-5000 时间锁 5000、已于 5001 转账；把 executed_at 改成 4999。
	corrupt := mutateReceipt(t, good, "gip-5000", func(r map[string]any) { r["executed_at"] = 4999 })
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{
		{"receipt", "--id", "gip-5000"},
		{"receipt", "--id", "gip-5000", "--json"},
		{"receipts"},
		{"receipts", "--json"},
		{"execute", "--id", "gip-5000", "--now", "9999"},
		{"execute", "--id", "gip-5000", "--now", "9999", "--json"},
		{"execute", "--id", "gip-pending", "--now", "0"},
		{"balances"},
		{"balances", "--json"},
		{"proposal", "--id", "gip-5000"},
		{"proposal", "--id", "gip-5000", "--json"},
		{"proposals"},
		{"proposals", "--json"},
	} {
		name := strings.Join(args, "_")
		t.Run(name, func(t *testing.T) {
			so, se, code := runCLI(t, binary, path, args...)
			if code != 1 {
				t.Fatalf("exit=%d want 1, stdout=%q stderr=%q", code, so, se)
			}
			if so != "" {
				t.Fatalf("stdout must stay empty on corruption, got %q", so)
			}
			for _, want := range []string{"corrupt", "gip-5000", "receipt 0", "executed_at", "4999", "5000", "before"} {
				if !strings.Contains(se, want) {
					t.Fatalf("stderr %q missing %q", se, want)
				}
			}
		})
	}
	if got, rerr := os.ReadFile(path); rerr != nil || string(got) != string(corrupt) {
		t.Fatalf("corrupt file was modified or rewritten")
	}

	// 对照一：完好文件的 receipt 查询正常（executed_at=5001 不早于时间锁）。
	goodPath := filepath.Join(dir, "good.json")
	if err := os.WriteFile(goodPath, good, 0o600); err != nil {
		t.Fatal(err)
	}
	if so, se, code := runCLI(t, binary, goodPath, "receipt", "--id", "gip-5000", "--json"); code != 0 ||
		!strings.Contains(so, `"executed_at": 5001`) {
		t.Fatalf("valid receipt query code=%d stdout=%q stderr=%q", code, so, se)
	}

	// 对照二：缺 executed_at 的错误措辞与“早于时间锁”不同（标 missing 而非数值）。
	missing := mutateReceipt(t, good, "gip-5000", func(r map[string]any) { delete(r, "executed_at") })
	missingPath := filepath.Join(dir, "missing.json")
	if err := os.WriteFile(missingPath, missing, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, se, code := runCLI(t, binary, missingPath, "receipts"); code != 1 ||
		!strings.Contains(se, "missing") || strings.Contains(se, "before timelock") {
		t.Fatalf("missing-field error wording wrong, code=%d stderr=%q", code, se)
	}
}
