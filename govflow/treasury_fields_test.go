package govflow

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mutateTopLevel 读出状态文件，对顶层文档应用 fn，把结果写到新路径并返回。
// 用于构造“顶层字段缺失/写错类型”的损坏文件，原文件保持原样。
func mutateTopLevel(t *testing.T, goodPath string, fn func(root map[string]any)) string {
	t.Helper()
	raw, err := os.ReadFile(goodPath)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	fn(root)
	bad, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "mutated.json")
	if err := os.WriteFile(path, append(bad, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestMissingTopLevelTreasuryFieldsRejected：初始金额为零且尚无执行凭据时，
// 删除 initial_treasury 或 treasury 任一字段都必须判整份状态损坏——即使其余
// 记录恰好能与零核对一致；错误指出缺失的是哪一个字段。明确写出两个零仍是
// 合法资金库。
func TestMissingTopLevelTreasuryFieldsRejected(t *testing.T) {
	store, goodPath := openTempStore(t, 0)
	store.Close()

	// 基线：明确写出的两个零是合法资金库。
	base, err := Open(goodPath)
	if err != nil {
		t.Fatalf("explicit zero balances must be readable: %v", err)
	}
	if bal, err := base.TreasuryBalance(); err != nil || bal != 0 {
		t.Fatalf("treasury=%d err=%v, want explicit zero", bal, err)
	}
	base.Close()

	cases := []struct {
		name      string
		field     string
		wantInErr string
	}{
		{"missing initial_treasury", "initial_treasury", `"initial_treasury"`},
		{"missing treasury", "treasury", `"treasury"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := mutateTopLevel(t, goodPath, func(root map[string]any) {
				delete(root, tc.field)
			})
			s, err := Open(bad)
			if !errors.Is(err, ErrStateCorrupt) {
				if s != nil {
					s.Close()
				}
				t.Fatalf("err=%v, want ErrStateCorrupt", err)
			}
			if !strings.Contains(err.Error(), tc.wantInErr) {
				t.Fatalf("error should name the missing field %s: %v", tc.wantInErr, err)
			}
			// 损坏文件不得被改写。
			raw, rerr := os.ReadFile(bad)
			if rerr != nil {
				t.Fatal(rerr)
			}
			var root map[string]any
			if err := json.Unmarshal(raw, &root); err != nil {
				t.Fatal(err)
			}
			if _, present := root[tc.field]; present {
				t.Fatalf("corrupt file was rewritten: %s reappeared", tc.field)
			}
		})
	}
}

// TestExecutedZeroTreasuryReadableButMissingRejected：初始一百、成功转出一百后，
// 明确写出的 treasury 为零照常读取（资金库零、收款账户一百、凭据完整）；
// 省略该字段后即使转账留痕与账户余额能核对一致，也必须拒绝读取。
func TestExecutedZeroTreasuryReadableButMissingRejected(t *testing.T) {
	store, goodPath := openTempStore(t, 100)
	mustRegister(t, store, "gip-1", 0, "transfer:acct:100")
	receipt, err := store.Execute("gip-1", 0)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	store.Close()

	// 基线：明确写出的 treasury 0 照常读取。
	base, err := Open(goodPath)
	if err != nil {
		t.Fatalf("explicit zero treasury must be readable: %v", err)
	}
	snapshot, err := base.BalanceSnapshot()
	if err != nil {
		t.Fatalf("BalanceSnapshot: %v", err)
	}
	if snapshot.Treasury != 0 || snapshot.Balances["acct"] != 100 {
		t.Fatalf("snapshot=%+v, want treasury 0 and acct 100", snapshot)
	}
	got, ok, err := base.Receipt("gip-1")
	if err != nil || !ok {
		t.Fatalf("Receipt ok=%v err=%v", ok, err)
	}
	if got.ExecutedAt != receipt.ExecutedAt || got.Order != receipt.Order || len(got.Actions) != 1 ||
		got.Actions[0].Treasury.Before != 100 || got.Actions[0].Treasury.After != 0 ||
		got.Actions[0].Recipient.After != 100 {
		t.Fatalf("receipt not preserved: %+v", got)
	}
	base.Close()

	// 省略 treasury：转账留痕与账户余额仍核对一致，也必须拒绝。
	bad := mutateTopLevel(t, goodPath, func(root map[string]any) {
		delete(root, "treasury")
	})
	s, err := Open(bad)
	if !errors.Is(err, ErrStateCorrupt) {
		if s != nil {
			s.Close()
		}
		t.Fatalf("err=%v, want ErrStateCorrupt", err)
	}
	if !strings.Contains(err.Error(), `"treasury"`) {
		t.Fatalf("error should name treasury: %v", err)
	}
}

// TestTopLevelTreasuryFieldTypesRejected：两个余额字段为 null、字符串、布尔、
// 小数、指数形式或超出 int64 范围，以及金额为负，都继续整份拒绝。
func TestTopLevelTreasuryFieldTypesRejected(t *testing.T) {
	store, goodPath := openTempStore(t, 100)
	store.Close()

	cases := []struct {
		name  string
		field string
		value any
	}{
		{"treasury null", "treasury", nil},
		{"treasury string", "treasury", "100"},
		{"treasury boolean", "treasury", true},
		{"treasury decimal", "treasury", 99.5},
		{"treasury negative", "treasury", -1},
		{"initial_treasury null", "initial_treasury", nil},
		{"initial_treasury string", "initial_treasury", "100"},
		{"initial_treasury boolean", "initial_treasury", false},
		{"initial_treasury decimal", "initial_treasury", 99.5},
		{"initial_treasury negative", "initial_treasury", -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := mutateTopLevel(t, goodPath, func(root map[string]any) {
				root[tc.field] = tc.value
			})
			s, err := Open(bad)
			if !errors.Is(err, ErrStateCorrupt) {
				if s != nil {
					s.Close()
				}
				t.Fatalf("err=%v, want ErrStateCorrupt", err)
			}
		})
	}

	// 指数形式与超出 int64 范围的写法经文本替换构造（encoding/json 会改写字面量）。
	for _, tc := range []struct {
		name    string
		literal string
	}{
		{"treasury exponent", `"treasury": 1e2`},
		{"treasury out of range", `"treasury": 9223372036854775808`},
		{"initial_treasury exponent", `"initial_treasury": 1e2`},
		{"initial_treasury out of range", `"initial_treasury": 9223372036854775808`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := os.ReadFile(goodPath)
			if err != nil {
				t.Fatal(err)
			}
			field := strings.SplitN(tc.literal, ":", 2)[0]
			var corrupted string
			for _, line := range strings.Split(string(raw), "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), field+":") {
					line = "  " + tc.literal + ","
				}
				corrupted += line + "\n"
			}
			bad := filepath.Join(t.TempDir(), "bad.json")
			if err := os.WriteFile(bad, []byte(corrupted), 0o600); err != nil {
				t.Fatal(err)
			}
			s, err := Open(bad)
			if !errors.Is(err, ErrStateCorrupt) {
				if s != nil {
					s.Close()
				}
				t.Fatalf("err=%v, want ErrStateCorrupt", err)
			}
		})
	}
}

// TestCorruptTreasuryBlocksAllAccess：损坏文件上的余额、提案与凭据查询不能
// 返回看似有效的数据，登记与执行也不能继续更改资金或提案；原状态文件保持原样。
func TestCorruptTreasuryBlocksAllAccess(t *testing.T) {
	store, goodPath := openTempStore(t, 100)
	mustRegister(t, store, "gip-1", 0, "transfer:acct:40")

	// 在已打开的句柄之下把文件改坏（删除 treasury），原句柄的后续操作
	// 每次都重新读取状态文件，必须全部报损坏而不是返回看似有效的数据。
	bad, err := os.ReadFile(goodPath)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(bad, &root); err != nil {
		t.Fatal(err)
	}
	delete(root, "treasury")
	corrupted, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(goodPath, append(corrupted, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := store.BalanceSnapshot(); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("BalanceSnapshot err=%v, want ErrStateCorrupt", err)
	}
	if _, err := store.TreasuryBalance(); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("TreasuryBalance err=%v, want ErrStateCorrupt", err)
	}
	if _, _, err := store.Proposal("gip-1"); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("Proposal err=%v, want ErrStateCorrupt", err)
	}
	if _, err := store.Proposals(); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("Proposals err=%v, want ErrStateCorrupt", err)
	}
	if _, _, err := store.Receipt("gip-1"); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("Receipt err=%v, want ErrStateCorrupt", err)
	}
	if _, err := store.Receipts(); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("Receipts err=%v, want ErrStateCorrupt", err)
	}
	if _, err := store.Register("gip-2", 0, []string{"transfer:acct:1"}); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("Register err=%v, want ErrStateCorrupt", err)
	}
	if _, err := store.Execute("gip-1", 0); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("Execute err=%v, want ErrStateCorrupt", err)
	}

	// 原状态文件内容保持原样：损坏后没有任何操作改写或补写它。
	after, err := os.ReadFile(goodPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(append(corrupted, '\n')) {
		t.Fatal("corrupt state file was modified by rejected operations")
	}
}

// TestMissingTreasuryFieldCLI：命令行在缺损文件上以域错误退出码 1 失败，
// 只向标准错误说明缺失字段，标准输出没有部分结果。
func TestMissingTreasuryFieldCLI(t *testing.T) {
	binary := buildCLI(t)
	state := filepath.Join(t.TempDir(), "treasury.json")
	if _, _, code := runCLI(t, binary, state, "init", "--balance", "0"); code != 0 {
		t.Fatal("init failed")
	}
	raw, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	delete(root, "treasury")
	corrupted, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state, append(corrupted, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{
		{"balances"},
		{"balances", "--json"},
		{"proposals"},
		{"receipts", "--json"},
		{"register", "--id", "gip-1", "--timelock", "0", "--action", "transfer:a:1"},
	} {
		so, se, code := runCLI(t, binary, state, args...)
		if code != 1 {
			t.Fatalf("%v: exit code %d, want 1 (stderr=%s)", args, code, se)
		}
		if so != "" {
			t.Fatalf("%v: stdout must be empty on corrupt state, got %q", args, so)
		}
		if !strings.Contains(se, `"treasury"`) {
			t.Fatalf("%v: stderr should name the missing field: %q", args, se)
		}
	}
}
