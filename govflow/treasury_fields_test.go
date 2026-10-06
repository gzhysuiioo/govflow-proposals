package govflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// rewriteTopLevelField 读取 path 处的状态文件，按 mutate 改写顶层字段后写回，
// 返回改写前的字节。数字以 json.Number 保留词面写法，使指数形式、超界整数
// 等非法写法能逐字落盘，不被 float64 折叠。
func rewriteTopLevelField(t *testing.T, path string, mutate func(doc map[string]any)) []byte {
	t.Helper()
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(good))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	mutate(doc)
	bad, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	bad = append(bad, '\n')
	if err := os.WriteFile(path, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	return good
}

// TestTreasuryFieldsRequired：顶层的 initial_treasury 与 treasury 都必须明确
// 写出且为 int64 范围内的 JSON 整数。字段缺失、为 null、写成字符串/布尔/
// 小数/指数形式或超出范围，都判整份状态文件损坏，错误指出具体是哪一个
// 字段；金额为负的既有拒绝保持不变。资金记录是否完整独立判断：即使初始
// 金额为零、尚无执行凭据、其余记录恰好能与零核对一致，缺失字段也不得被
// 折叠成 0 再认可；明确写出的两个 0 仍是合法资金库。
func TestTreasuryFieldsRequired(t *testing.T) {
	mutations := map[string]struct {
		mutate func(doc map[string]any)
		want   string
	}{
		"initial missing": {
			mutate: func(doc map[string]any) { delete(doc, "initial_treasury") },
			want:   `field "initial_treasury" is missing`,
		},
		"treasury missing": {
			mutate: func(doc map[string]any) { delete(doc, "treasury") },
			want:   `field "treasury" is missing`,
		},
		"initial null": {
			mutate: func(doc map[string]any) { doc["initial_treasury"] = nil },
			want:   `field "initial_treasury" is null`,
		},
		"treasury null": {
			mutate: func(doc map[string]any) { doc["treasury"] = nil },
			want:   `field "treasury" is null`,
		},
		"initial string": {
			mutate: func(doc map[string]any) { doc["initial_treasury"] = "0" },
			want:   `field "initial_treasury" has wrong type: want integer, got string`,
		},
		"treasury string": {
			mutate: func(doc map[string]any) { doc["treasury"] = "0" },
			want:   `field "treasury" has wrong type: want integer, got string`,
		},
		"initial boolean": {
			mutate: func(doc map[string]any) { doc["initial_treasury"] = false },
			want:   `field "initial_treasury" has wrong type: want integer, got boolean`,
		},
		"treasury boolean": {
			mutate: func(doc map[string]any) { doc["treasury"] = true },
			want:   `field "treasury" has wrong type: want integer, got boolean`,
		},
		"initial fraction": {
			mutate: func(doc map[string]any) { doc["initial_treasury"] = json.Number("0.5") },
			want:   `field "initial_treasury" has wrong type: want integer, got number`,
		},
		"treasury fraction": {
			mutate: func(doc map[string]any) { doc["treasury"] = json.Number("1.5") },
			want:   `field "treasury" has wrong type: want integer, got number`,
		},
		"initial exponent": {
			mutate: func(doc map[string]any) { doc["initial_treasury"] = json.Number("1e2") },
			want:   `field "initial_treasury" has wrong type: want integer, got number`,
		},
		"treasury exponent": {
			mutate: func(doc map[string]any) { doc["treasury"] = json.Number("1e2") },
			want:   `field "treasury" has wrong type: want integer, got number`,
		},
		"initial out of range": {
			mutate: func(doc map[string]any) { doc["initial_treasury"] = json.Number("9223372036854775808") },
			want:   `field "initial_treasury" has wrong type: want integer, got number`,
		},
		"treasury out of range": {
			mutate: func(doc map[string]any) { doc["treasury"] = json.Number("-9223372036854775809") },
			want:   `field "treasury" has wrong type: want integer, got number`,
		},
		"initial negative": {
			mutate: func(doc map[string]any) { doc["initial_treasury"] = json.Number("-1") },
			want:   `initial treasury balance is negative`,
		},
		"treasury negative": {
			mutate: func(doc map[string]any) { doc["treasury"] = json.Number("-1") },
			want:   `treasury balance is negative`,
		},
	}
	for name, tc := range mutations {
		t.Run(name, func(t *testing.T) {
			// 初始金额为零且尚无执行凭据：缺损字段恰好能与零核对一致，
			// 也必须判损坏，不能凭计算结果相等认可缺损文件。
			store, path := openTempStore(t, 0)
			store.Close()
			good := rewriteTopLevelField(t, path, tc.mutate)

			if _, err := Open(path); !errors.Is(err, ErrStateCorrupt) {
				t.Fatalf("Open err=%v, want ErrStateCorrupt", err)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Open err=%q, want substring %q", err, tc.want)
			}
			// 损坏文件保持原样，不被重建或覆盖。
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) == string(good) {
				t.Fatalf("corrupt file was overwritten")
			}
		})
	}
}

// TestTreasuryZeroExplicitlyWrittenIsLegal：初始金额为零、两个余额字段都明确
// 写出 0 且尚无执行凭据时，状态文件照常读取，余额查询返回资金库零。
func TestTreasuryZeroExplicitlyWrittenIsLegal(t *testing.T) {
	store, path := openTempStore(t, 0)
	store.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer reopened.Close()
	snap, err := reopened.BalanceSnapshot()
	if err != nil {
		t.Fatalf("BalanceSnapshot: %v", err)
	}
	if snap.Treasury != 0 || len(snap.Balances) != 0 {
		t.Fatalf("snapshot=%+v, want treasury 0 and no accounts", snap)
	}
}

// TestTreasuryZeroAfterFullTransfer：初始金额一百、成功向某账户转出一百后，
// 明确写出的 treasury 为零照常读取：余额查询显示资金库零、收款账户一百，
// 首次执行凭据完整保留。省略该字段后，即使转账留痕与账户余额能核对一致，
// 也必须拒绝读取；省略 initial_treasury 同理。
func TestTreasuryZeroAfterFullTransfer(t *testing.T) {
	store, path := openTempStore(t, 100)
	mustRegister(t, store, "gip-1", 0, "transfer:audits:100")
	receipt, err := store.Execute("gip-1", 0)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	store.Close()

	// 完整文件：treasury 明确写出 0，照常读取。
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	snap, err := reopened.BalanceSnapshot()
	if err != nil {
		t.Fatalf("BalanceSnapshot: %v", err)
	}
	if snap.Treasury != 0 || snap.Balances["audits"] != 100 || len(snap.Balances) != 1 {
		t.Fatalf("snapshot=%+v, want treasury 0 and audits 100", snap)
	}
	got, ok, err := reopened.Receipt("gip-1")
	if err != nil || !ok {
		t.Fatalf("Receipt ok=%v err=%v", ok, err)
	}
	if got.ProposalID != receipt.ProposalID || got.ExecutedAt != receipt.ExecutedAt ||
		len(got.Actions) != 1 || got.Actions[0].Treasury.Before != 100 || got.Actions[0].Treasury.After != 0 ||
		got.Actions[0].Recipient.After != 100 {
		t.Fatalf("receipt=%+v, want first execution receipt preserved", got)
	}
	reopened.Close()

	// 省略 treasury：转账留痕与账户余额仍与零核对一致，也必须拒绝。
	rewriteTopLevelField(t, path, func(doc map[string]any) { delete(doc, "treasury") })
	if _, err := Open(path); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("Open without treasury err=%v, want ErrStateCorrupt", err)
	} else if !strings.Contains(err.Error(), `field "treasury" is missing`) {
		t.Fatalf("Open without treasury err=%q, want field named", err)
	}

	// 省略 initial_treasury：同样拒绝，并指出缺失的是 initial_treasury。
	rewriteTopLevelField(t, path, func(doc map[string]any) {
		doc["treasury"] = json.Number("0")
		delete(doc, "initial_treasury")
	})
	if _, err := Open(path); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("Open without initial_treasury err=%v, want ErrStateCorrupt", err)
	} else if !strings.Contains(err.Error(), `field "initial_treasury" is missing`) {
		t.Fatalf("Open without initial_treasury err=%q, want field named", err)
	}
}

// TestCorruptTreasuryBlocksQueriesAndMutations：余额字段缺损的状态文件上，
// 余额、提案与凭据查询不能返回看似有效的数据，登记、投票、计票与执行也不
// 能继续更改资金或提案；一切操作报可识别为状态损坏的错误，文件保持原样。
func TestCorruptTreasuryBlocksQueriesAndMutations(t *testing.T) {
	store, path := openTempStore(t, 100)
	mustRegister(t, store, "gip-1", 0, "transfer:audits:100")
	in := &CreateVoteInput{
		ID:          "gip-v",
		Members:     []VoteMember{{ID: "alice", Weight: 300}},
		Quorum:      100,
		StartAt:     0,
		Deadline:    100,
		TimelockEnd: 200,
		Actions:     []string{"transfer:audits:50"},
	}
	mustCreateVote(t, store, in)

	// 句柄保持打开，仅把磁盘上的文件改坏：后续每次操作各自重读状态文件，
	// 都必须在缺损余额字段上失败。
	rewriteTopLevelField(t, path, func(doc map[string]any) { delete(doc, "treasury") })
	corrupt, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	checks := map[string]func() error{
		"BalanceSnapshot": func() error { _, err := store.BalanceSnapshot(); return err },
		"TreasuryBalance": func() error { _, err := store.TreasuryBalance(); return err },
		"Balance":         func() error { _, err := store.Balance("audits"); return err },
		"Balances":        func() error { _, err := store.Balances(); return err },
		"Proposal":        func() error { _, _, err := store.Proposal("gip-1"); return err },
		"Proposals":       func() error { _, err := store.Proposals(); return err },
		"Receipt":         func() error { _, _, err := store.Receipt("gip-1"); return err },
		"Receipts":        func() error { _, err := store.Receipts(); return err },
		"Register":        func() error { _, err := store.Register("gip-2", 0, []string{"transfer:x:1"}); return err },
		"CastVote":        func() error { _, err := store.CastVote("gip-v", "alice", true, 50); return err },
		"TallyVote":       func() error { _, err := store.TallyVote("gip-v", 100); return err },
		"Execute":         func() error { _, err := store.Execute("gip-1", 0); return err },
	}
	for name, fn := range checks {
		if err := fn(); !errors.Is(err, ErrStateCorrupt) {
			t.Fatalf("%s err=%v, want ErrStateCorrupt", name, err)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, corrupt) {
		t.Fatalf("corrupt state file was modified by rejected operations")
	}
}

// TestTreasuryFieldsLegacyFileWithoutVoteTable：旧格式仅因没有整张投票提案表
// 而继续可读的约定保留，但两个余额字段的完整性要求不因此放宽。
func TestTreasuryFieldsLegacyFileWithoutVoteTable(t *testing.T) {
	store, path := openTempStore(t, 500)
	store.Close()

	// 删掉整张 vote_proposals 表：旧格式文件照常可读。
	rewriteTopLevelField(t, path, func(doc map[string]any) { delete(doc, "vote_proposals") })
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("Open legacy file: %v", err)
	}
	if bal, err := reopened.TreasuryBalance(); err != nil || bal != 500 {
		t.Fatalf("TreasuryBalance=%d err=%v", bal, err)
	}
	reopened.Close()

	// 同一文件再缺 treasury：仍必须拒绝。
	rewriteTopLevelField(t, path, func(doc map[string]any) { delete(doc, "treasury") })
	if _, err := Open(path); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("Open err=%v, want ErrStateCorrupt", err)
	} else if !strings.Contains(err.Error(), `field "treasury" is missing`) {
		t.Fatalf("Open err=%q, want field named", err)
	}
}

// TestTreasuryFieldsPreservedAcrossOperations：登记、投票、计票与执行落盘后，
// 两个余额字段始终明确写出，重开文件照常读取。
func TestTreasuryFieldsPreservedAcrossOperations(t *testing.T) {
	store, path := openTempStore(t, 100)
	mustRegister(t, store, "gip-1", 0, "transfer:audits:100")
	if _, err := store.Execute("gip-1", 0); err != nil {
		t.Fatalf("Execute: %v err", err)
	}
	store.Close()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if string(doc["initial_treasury"]) != "100" {
		t.Fatalf("initial_treasury=%s, want explicitly written 100", doc["initial_treasury"])
	}
	if string(doc["treasury"]) != "0" {
		t.Fatalf("treasury=%s, want explicitly written 0", doc["treasury"])
	}
	if _, err := Open(path); err != nil {
		t.Fatalf("Open after execute: %v", err)
	}
}
