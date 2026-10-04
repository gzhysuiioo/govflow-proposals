package govflow

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestPlanTransfersChainsBalances：统一转账规则按动作原顺序扣款/加款，
// 未出现过的收款账户从零开始，同一账户连续收款时后一笔承接前一笔结果，
// 金额恰好等于资金库剩余余额时扣款到 0 仍成功。
func TestPlanTransfersChainsBalances(t *testing.T) {
	steps, treasury, balances, err := planTransfers(
		[]string{"transfer:audits:250", "transfer:audits:100", "transfer:legal:50"},
		1000, nil)
	if err != nil {
		t.Fatalf("planTransfers: %v", err)
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
		st := steps[i]
		if st.account != w.acct || st.treasuryBefore != w.tb || st.treasuryAfter != w.ta ||
			st.recipientBefore != w.rb || st.recipientAfter != w.ra || st.failure != nil {
			t.Fatalf("step %d = %+v, want acct=%s treasury=%d->%d recipient=%d->%d",
				i, st, w.acct, w.tb, w.ta, w.rb, w.ra)
		}
		if got := st.actionReceipt(i, "transfer:"+w.acct+":x"); got.Treasury.Account != "treasury" ||
			got.Recipient.Account != w.acct || got.Index != int64(i) {
			t.Fatalf("actionReceipt shape wrong: %+v", got)
		}
	}
	if treasury != 600 {
		t.Fatalf("treasury = %d, want 600", treasury)
	}
	if balances["audits"] != 350 || balances["legal"] != 50 || len(balances) != 2 {
		t.Fatalf("balances = %+v, want audits=350 legal=50", balances)
	}

	// 起始状态中已有的账户余额被承接，且起始表不被修改。
	start := map[string]int64{"audits": 7}
	steps, treasury, balances, err = planTransfers([]string{"transfer:audits:3"}, 10, start)
	if err != nil {
		t.Fatalf("planTransfers existing: %v", err)
	}
	if steps[0].recipientBefore != 7 || steps[0].recipientAfter != 10 ||
		treasury != 7 || balances["audits"] != 10 {
		t.Fatalf("existing balance not carried: %+v %d %+v", steps[0], treasury, balances)
	}
	if start["audits"] != 7 {
		t.Fatalf("planTransfers mutated starting balances: %+v", start)
	}

	// 金额恰好等于资金库剩余余额：可以成功，资金库扣到 0。
	steps, treasury, _, err = planTransfers([]string{"transfer:x:10"}, 10, nil)
	if err != nil || treasury != 0 || steps[0].treasuryAfter != 0 {
		t.Fatalf("exact-remaining transfer rejected: %v treasury=%d steps=%+v", err, treasury, steps)
	}
}

// TestPlanTransfersFailurePrecedence：动作非法、收款溢出、资金库不足的
// 分类、动作位置与先报告顺序沿用首次执行路径的既有行为；同一动作同时
// 触发收款溢出与资金库不足时先报告溢出。
func TestPlanTransfersFailurePrecedence(t *testing.T) {
	// 动作格式错误：同时匹配 ErrExecutionRejected 与 ErrInvalidAction。
	_, _, _, err := planTransfers([]string{"transfer:a:1", "withdraw:b:2"}, 1000, nil)
	if !errors.Is(err, ErrExecutionRejected) || !errors.Is(err, ErrInvalidAction) {
		t.Fatalf("malformed err=%v, want both sentinels", err)
	}
	if !strings.Contains(err.Error(), `action 1 ("withdraw:b:2")`) {
		t.Fatalf("malformed error lost position/text: %v", err)
	}

	// 收款溢出先于资金库不足：资金库只剩 5、账户已接近 int64 上限，
	// 一笔 10 同时满足两个条件，必须先报溢出并带动作位置。
	nearMax := map[string]int64{"a": math.MaxInt64 - 5}
	_, _, _, err = planTransfers([]string{"transfer:a:10"}, 5, nearMax)
	if !errors.Is(err, ErrExecutionRejected) || errors.Is(err, ErrInvalidAction) {
		t.Fatalf("overflow err=%v, want ErrExecutionRejected only", err)
	}
	if !strings.Contains(err.Error(), "recipient balance overflow at action 0") ||
		!strings.Contains(err.Error(), "account=a") {
		t.Fatalf("overflow error wrong/reordered: %v", err)
	}

	// 资金库不足（无溢出）：文案与位置保持不变。
	_, _, _, err = planTransfers([]string{"transfer:mida:600", "transfer:midb:500"}, 1000, nil)
	if !errors.Is(err, ErrExecutionRejected) ||
		!strings.Contains(err.Error(), "insufficient treasury balance at action 1") {
		t.Fatalf("insufficient err=%v", err)
	}
}

// TestVerifyReceiptTransfersAcceptsPlannedAndRejectsTamper：执行预演产出的
// 逐笔结果构造的凭据必须被重放核对接受（两条路径同源）；篡改任一方的
// 前后余额都按既有原因与动作位置拒绝，且不推进重放状态。
func TestVerifyReceiptTransfersAcceptsPlannedAndRejectsTamper(t *testing.T) {
	actions := []string{"transfer:audits:250", "transfer:audits:100", "transfer:legal:50"}
	steps, _, _, err := planTransfers(actions, 1000, nil)
	if err != nil {
		t.Fatal(err)
	}
	good := &Receipt{ProposalID: "gip-1"}
	for i, st := range steps {
		good.Actions = append(good.Actions, st.actionReceipt(i, actions[i]))
	}
	balances := map[string]int64{}
	treasury, err := verifyReceiptTransfers(good, actions, 1000, balances)
	if err != nil {
		t.Fatalf("planned receipt must verify: %v", err)
	}
	if treasury != 600 || balances["audits"] != 350 || balances["legal"] != 50 {
		t.Fatalf("verify result treasury=%d balances=%+v", treasury, balances)
	}

	// 重放路径的字段核对顺序：条数/编号/原文 → 资金库前 → 资金库后
	// （不足在此）→ 收款前 → 收款后（溢出在此）。
	cases := []struct {
		name   string
		tamper func(*ActionReceipt)
		want   string
	}{
		{"treasury before", func(ar *ActionReceipt) { ar.Treasury.Before = 999 },
			"action 1 treasury before-balance mismatch"},
		{"treasury after", func(ar *ActionReceipt) { ar.Treasury.After = 651 },
			"action 1 treasury after-balance mismatch"},
		{"recipient before", func(ar *ActionReceipt) { ar.Recipient.Before = 249 },
			"action 1 recipient before-balance mismatch"},
		{"recipient after", func(ar *ActionReceipt) { ar.Recipient.After = 351 },
			"action 1 recipient after-balance mismatch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := &Receipt{ProposalID: "gip-1"}
			for i, st := range steps {
				ar := st.actionReceipt(i, actions[i])
				if i == 1 {
					tc.tamper(&ar)
				}
				bad.Actions = append(bad.Actions, ar)
			}
			fresh := map[string]int64{}
			start := int64(1000)
			gotTreasury, err := verifyReceiptTransfers(bad, actions, start, fresh)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("tamper %s err=%v, want %q", tc.name, err, tc.want)
			}
			// 出错动作（第 1 笔）及其后动作不得应用：第 0 笔已核对的结果
			// audits=250、资金库 750 保留，第 1 笔的 350 与 legal 的 50 不得出现。
			if len(fresh) != 1 || fresh["audits"] != 250 {
				t.Fatalf("failing action advanced replay state: %+v", fresh)
			}
			if gotTreasury != 750 {
				t.Fatalf("treasury advanced past failing action: %d", gotTreasury)
			}
		})
	}

	// 条数、编号、原文不一致沿用既有原因。
	if _, err := verifyReceiptTransfers(good, actions[:2], 1000, map[string]int64{}); err == nil ||
		!strings.Contains(err.Error(), "action count 3 does not match proposal 2") {
		t.Fatalf("count mismatch err=%v", err)
	}
	renumbered := *good
	renumbered.Actions = append([]ActionReceipt(nil), good.Actions...)
	renumbered.Actions[2].Index = 9
	if _, err := verifyReceiptTransfers(&renumbered, actions, 1000, map[string]int64{}); err == nil ||
		!strings.Contains(err.Error(), `action 2 has index 9`) {
		t.Fatalf("index mismatch err=%v", err)
	}
	differentText := append([]string(nil), actions...)
	differentText[0] = "transfer:audits:251"
	if _, err := verifyReceiptTransfers(good, differentText, 1000, map[string]int64{}); err == nil ||
		!strings.Contains(err.Error(), "text does not match registered action") {
		t.Fatalf("text mismatch err=%v", err)
	}

	// 重放侧先核对资金库：一笔同时“资金不足”且“收款溢出”的动作，
	// 读取路径先报告资金库一侧（与执行路径先报溢出各自保持不变）。
	// 凭据把资金库后余额写成重算出的 -5（恰好等于 treasury-amount）：
	// 若按首个失败原因推断会漏掉资金不足而落到收款侧，独立的不足标志
	// 必须仍让资金库一侧先报错。
	conflict := &Receipt{ProposalID: "gip-x", Actions: []ActionReceipt{{
		Index: 0, Action: "transfer:a:10",
		Treasury:  BalanceUpdate{Account: "treasury", Before: 5, After: -5},
		Recipient: BalanceUpdate{Account: "a", Before: math.MaxInt64 - 5, After: math.MaxInt64},
	}}}
	_, err = verifyReceiptTransfers(conflict, []string{"transfer:a:10"}, 5, map[string]int64{})
	if err == nil || !strings.Contains(err.Error(), "action 0 treasury after-balance mismatch") {
		t.Fatalf("replay precedence err=%v, want treasury-side mismatch first", err)
	}
}

// TestExecuteExactRemainingAndOverflowBeforeInsufficient：端到端验证
// “恰好等于余额可成功”与“溢出先于不足报告”，失败不留任何资金变动/凭据/状态。
func TestExecuteExactRemainingAndOverflowBeforeInsufficient(t *testing.T) {
	store, _ := openTempStore(t, 10)
	defer store.Close()
	mustRegister(t, store, "gip-eq", 0, "transfer:a:10")
	r, err := store.Execute("gip-eq", 0)
	if err != nil {
		t.Fatalf("exact-remaining execute: %v", err)
	}
	if r.Actions[0].Treasury.Before != 10 || r.Actions[0].Treasury.After != 0 ||
		r.Actions[0].Recipient.After != 10 {
		t.Fatalf("exact-remaining receipt: %+v", r.Actions[0])
	}
	if bal, _ := store.TreasuryBalance(); bal != 0 {
		t.Fatalf("treasury = %d, want 0", bal)
	}
	if bal, _ := store.Balance("a"); bal != 10 {
		t.Fatalf("a = %d, want 10", bal)
	}

	// 资金库 MaxInt64：先把某账户打到 MaxInt64-5（资金库剩 5），
	// 再向同一账户转 10 —— 同时溢出且不足，必须先报溢出。
	big, path2 := openTempStore(t, math.MaxInt64)
	defer big.Close()
	mustRegister(t, big, "gip-near", 0, "transfer:a:9223372036854775802")
	if _, err := big.Execute("gip-near", 0); err != nil {
		t.Fatalf("setup execute: %v", err)
	}
	mustRegister(t, big, "gip-both", 0, "transfer:a:10")
	_, err = big.Execute("gip-both", 0)
	if !errors.Is(err, ErrExecutionRejected) || !strings.Contains(err.Error(), "overflow") {
		t.Fatalf("both-conditions err=%v, want overflow reported first", err)
	}
	if bal, _ := big.TreasuryBalance(); bal != 5 {
		t.Fatalf("treasury changed after failure: %d", bal)
	}
	if bal, _ := big.Balance("a"); bal != math.MaxInt64-5 {
		t.Fatalf("recipient changed after failure: %d", bal)
	}
	if rec, ok, _ := big.Receipt("gip-both"); ok || rec != nil {
		t.Fatalf("receipt left after failure: %+v", rec)
	}
	if p, _, _ := big.Proposal("gip-both"); p.State != "passed" {
		t.Fatalf("state changed after failure: %s", p.State)
	}
	// 重开后仍为完整状态：成功凭据与最终余额一致。
	big.Close()
	reopened, err := Open(path2)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if bal, _ := reopened.TreasuryBalance(); bal != 5 {
		t.Fatalf("treasury after reopen = %d", bal)
	}
}

// TestReceiptBalanceFieldsMustBePresent：凭据每项动作的四个余额字段
// （资金库/收款账户 × before/after）都必须明确写出。任何一侧任一字段缺失、
// 为 null 或类型不符——包括按转账金额推算本来应为 0 的字段——都判整份状态
// 损坏，错误指出提案编号、动作下标（0 起）、所在侧与字段名；不能根据转账
// 金额、账户余额或其它凭据补出。明确写出的整数 0 是合法记录，照常可读。
func TestReceiptBalanceFieldsMustBePresent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "treasury.json")
	store, err := InitTreasury(path, 400)
	if err != nil {
		t.Fatal(err)
	}
	// 同一账户连续收款：第二笔的 before 承接第一笔的 after。
	// 凭据逐笔记为 资金库 400->250->0、收款账户 0->150->300。
	mustRegister(t, store, "gip-1", 0, "transfer:new:150", "transfer:new:150")
	if _, err := store.Execute("gip-1", 0); err != nil {
		t.Fatal(err)
	}
	// 投票通过的提案产生的凭据受同一条字段规则约束；其资金库 after 明确写出 0。
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

	// 基线：明确写出 0 的完整凭据照常可读，查询与执行重试行为不变。
	t.Run("baseline readable with explicit zeros", func(t *testing.T) {
		s, err := Open(path)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer s.Close()
		r, ok, err := s.Receipt("gip-1")
		if err != nil || !ok {
			t.Fatalf("Receipt(gip-1) ok=%v err=%v", ok, err)
		}
		want := []struct{ tb, ta, rb, ra int64 }{
			{400, 250, 0, 150},
			{250, 100, 150, 300}, // 同一账户连续收款，承接上一笔结果
		}
		for i, w := range want {
			ar := r.Actions[i]
			if ar.Treasury.Before != w.tb || ar.Treasury.After != w.ta ||
				ar.Recipient.Before != w.rb || ar.Recipient.After != w.ra {
				t.Fatalf("action %d = %+v, want treasury %d->%d recipient %d->%d",
					i, ar, w.tb, w.ta, w.rb, w.ra)
			}
		}
		// 投票凭据：资金库 100->0、收款账户 0->100，两个明确写出的 0。
		vr, ok, err := s.Receipt("gip-vote")
		if err != nil || !ok {
			t.Fatalf("Receipt(gip-vote) ok=%v err=%v", ok, err)
		}
		if va := vr.Actions[0]; va.Treasury.Before != 100 || va.Treasury.After != 0 ||
			va.Recipient.Before != 0 || va.Recipient.After != 100 {
			t.Fatalf("unexpected vote action receipt: %+v", va)
		}
		if bal, _ := s.TreasuryBalance(); bal != 0 {
			t.Fatalf("treasury = %d, want 0", bal)
		}
		if bal, _ := s.Balance("new"); bal != 300 {
			t.Fatalf("new = %d, want 300", bal)
		}
		again, err := s.Execute("gip-1", 999)
		if err != nil || again.ExecutedAt != 0 {
			t.Fatalf("retry execute: %+v err=%v", again, err)
		}
	})

	// mutateReceipt 改写第 ri 张凭据第 ai 个动作的余额字段；
	// value 为 nil 时删除该字段（模拟缺损凭据）。
	mutateReceipt := func(ri, ai int, side, field string, value any) []byte {
		var doc map[string]any
		if err := json.Unmarshal(good, &doc); err != nil {
			t.Fatal(err)
		}
		entry := doc["receipts"].([]any)[ri].(map[string]any)["actions"].([]any)[ai].(map[string]any)[side].(map[string]any)
		if value == nil {
			delete(entry, field)
		} else {
			entry[field] = value
		}
		raw, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		return append(raw, '\n')
	}

	corruptions := []struct {
		name string
		raw  []byte
		want []string
	}{
		// 任务示例：删除资金库侧的 after（本来应为 0）必须拒绝；
		// 投票通过提案的凭据与登记提案适用同一规则。
		{"treasury after missing (would be 0)", mutateReceipt(1, 0, "treasury", "after", nil),
			[]string{`"gip-vote"`, "action 0", "treasury", `"after"`, "missing"}},
		// 任务示例：删除收款账户侧的 before（本来应为 0）必须拒绝。
		{"recipient before missing (would be 0)", mutateReceipt(0, 0, "recipient", "before", nil),
			[]string{`"gip-1"`, "action 0", "recipient", `"before"`, "missing"}},
		// 非零字段缺失适用同一规则。
		{"treasury before missing (non-zero)", mutateReceipt(0, 0, "treasury", "before", nil),
			[]string{`"gip-1"`, "action 0", "treasury", `"before"`, "missing"}},
		{"recipient after missing (non-zero)", mutateReceipt(0, 1, "recipient", "after", nil),
			[]string{`"gip-1"`, "action 1", "recipient", `"after"`, "missing"}},
		// null 与类型不符同样不得折叠成 0。
		{"treasury after null", mutateReceipt(0, 1, "treasury", "after", json.RawMessage("null")),
			[]string{`"gip-1"`, "action 1", "treasury", `"after"`, "null"}},
		{"recipient before wrong type", mutateReceipt(0, 0, "recipient", "before", "0"),
			[]string{`"gip-1"`, "action 0", "recipient", `"before"`, "wrong type"}},
		// 投票凭据的收款侧同样适用。
		{"vote receipt recipient after missing", mutateReceipt(1, 0, "recipient", "after", nil),
			[]string{`"gip-vote"`, "action 0", "recipient", `"after"`, "missing"}},
	}
	for _, tc := range corruptions {
		t.Run(tc.name, func(t *testing.T) {
			bad := filepath.Join(dir, "missing-"+strings.ReplaceAll(strings.ReplaceAll(tc.name, " ", "-"), "(", "")+".json")
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
					t.Fatalf("error %q does not locate %q", err.Error(), want)
				}
			}
			// 缺损凭据不被删除或重写，原文件保持原样。
			if got, rerr := os.ReadFile(bad); rerr != nil || string(got) != string(tc.raw) {
				t.Fatalf("corrupt file was modified")
			}
		})
	}

	// 打开后文件被换成缺损凭据：余额、提案、凭据查询与继续执行都失败，
	// 不输出文件中其他正常记录的部分结果，也不改写原文件。
	t.Run("all operations fail after corruption", func(t *testing.T) {
		p := filepath.Join(dir, "swap.json")
		if err := os.WriteFile(p, good, 0o600); err != nil {
			t.Fatal(err)
		}
		s, err := Open(p)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		corrupt := mutateReceipt(0, 0, "treasury", "after", nil)
		if err := os.WriteFile(p, corrupt, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.BalanceSnapshot(); !errors.Is(err, ErrStateCorrupt) {
			t.Fatalf("BalanceSnapshot err=%v, want ErrStateCorrupt", err)
		}
		if _, _, err := s.Proposal("gip-1"); !errors.Is(err, ErrStateCorrupt) {
			t.Fatalf("Proposal err=%v, want ErrStateCorrupt", err)
		}
		if _, _, err := s.Receipt("gip-vote"); !errors.Is(err, ErrStateCorrupt) {
			t.Fatalf("Receipt err=%v, want ErrStateCorrupt", err)
		}
		if _, err := s.Receipts(); !errors.Is(err, ErrStateCorrupt) {
			t.Fatalf("Receipts err=%v, want ErrStateCorrupt", err)
		}
		if _, err := s.Execute("gip-1", 500); !errors.Is(err, ErrStateCorrupt) {
			t.Fatalf("Execute err=%v, want ErrStateCorrupt", err)
		}
		if got, _ := os.ReadFile(p); string(got) != string(corrupt) {
			t.Fatalf("state file was modified by rejected operations")
		}
	})
}

// TestTamperedReceiptBalancesRejectWithCause：凭据中资金库/收款账户的
// 前后余额被篡改时，整份状态判为损坏，错误沿用既有原因与动作位置，
// 原文件保持原样，不改成重算结果后继续提供查询。
func TestTamperedReceiptBalancesRejectWithCause(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "treasury.json")
	store, err := InitTreasury(path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	mustRegister(t, store, "gip-1", 0,
		"transfer:audits:250", "transfer:audits:100", "transfer:legal:50")
	if _, err := store.Execute("gip-1", 0); err != nil {
		t.Fatal(err)
	}
	store.Close()

	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	mutate := func(fn func(actions []any)) []byte {
		var doc map[string]any
		if err := json.Unmarshal(good, &doc); err != nil {
			t.Fatal(err)
		}
		rcpt := doc["receipts"].([]any)[0].(map[string]any)
		fn(rcpt["actions"].([]any))
		raw, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		return append(raw, '\n')
	}
	setField := func(actions []any, i int, side string, field string, value any) {
		entry := actions[i].(map[string]any)[side].(map[string]any)
		entry[field] = value
	}
	cases := []struct {
		name string
		raw  []byte
		want string
	}{
		{"treasury-before", mutate(func(a []any) { setField(a, 1, "treasury", "before", 999) }),
			"action 1 treasury before-balance mismatch"},
		{"treasury-after", mutate(func(a []any) { setField(a, 1, "treasury", "after", 651) }),
			"action 1 treasury after-balance mismatch"},
		{"recipient-before", mutate(func(a []any) { setField(a, 1, "recipient", "before", 249) }),
			"action 1 recipient before-balance mismatch"},
		{"recipient-after", mutate(func(a []any) { setField(a, 1, "recipient", "after", 351) }),
			"action 1 recipient after-balance mismatch"},
		{"recipient-account", mutate(func(a []any) { setField(a, 0, "recipient", "account", "other") }),
			"action 0 recipient before-balance mismatch"},
		{"treasury-account", mutate(func(a []any) { setField(a, 0, "treasury", "account", "vault") }),
			"action 0 treasury before-balance mismatch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := filepath.Join(dir, "tamper-"+tc.name+".json")
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
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q missing %q", err.Error(), tc.want)
			}
			// 原文件保持原样，不被重算结果覆盖。
			if got, rerr := os.ReadFile(bad); rerr != nil || string(got) != string(tc.raw) {
				t.Fatalf("corrupt file was modified")
			}
		})
	}
}

// TestRegisteredAndVotedProposalsTransferIdentically：相同动作与起始余额下，
// 登记来源与投票通过来源的提案产生相同的资金变动与逐笔凭据，且已执行
// 提案再次执行仍返回首次凭据。
func TestRegisteredAndVotedProposalsTransferIdentically(t *testing.T) {
	actions := []string{"transfer:audits:250", "transfer:audits:100", "transfer:legal:50"}

	registered, _ := openTempStore(t, 1000)
	defer registered.Close()
	mustRegister(t, registered, "gip-r", 0, actions...)
	rr, err := registered.Execute("gip-r", 0)
	if err != nil {
		t.Fatal(err)
	}

	voted, _ := openTempStore(t, 1000)
	defer voted.Close()
	in := baseVoteInput("gip-v")
	in.Actions = actions
	in.StartAt, in.Deadline, in.TimelockEnd = 0, 1, 1
	mustCreateVote(t, voted, in)
	if _, err := voted.CastVote("gip-v", "alice", true, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := voted.TallyVote("gip-v", 1); err != nil {
		t.Fatal(err)
	}
	vr, err := voted.Execute("gip-v", 1)
	if err != nil {
		t.Fatal(err)
	}

	if len(rr.Actions) != len(vr.Actions) {
		t.Fatalf("receipt action counts differ: %d vs %d", len(rr.Actions), len(vr.Actions))
	}
	for i := range rr.Actions {
		// 两个来源的凭据只有动作原文（编号相同）与双方前后余额可比。
		ra, va := rr.Actions[i], vr.Actions[i]
		if ra.Action != va.Action || !reflect.DeepEqual(ra.Treasury, va.Treasury) ||
			!reflect.DeepEqual(ra.Recipient, va.Recipient) || ra.Index != va.Index {
			t.Fatalf("action %d differs by source:\nregistered=%+v\nvoted=%+v", i, ra, va)
		}
	}
	for _, pair := range []struct {
		name    string
		store   *Store
		receipt *Receipt
		id      string
	}{
		{"registered", registered, rr, "gip-r"},
		{"voted", voted, vr, "gip-v"},
	} {
		if bal, _ := pair.store.TreasuryBalance(); bal != 600 {
			t.Fatalf("%s treasury=%d, want 600", pair.name, bal)
		}
		if bal, _ := pair.store.Balance("audits"); bal != 350 {
			t.Fatalf("%s audits=%d, want 350", pair.name, bal)
		}
		if bal, _ := pair.store.Balance("legal"); bal != 50 {
			t.Fatalf("%s legal=%d, want 50", pair.name, bal)
		}
		// 再次执行返回首次凭据，不产生二次资金变动。
		again, err := pair.store.Execute(pair.id, 99999)
		if err != nil {
			t.Fatalf("%s re-execute: %v", pair.name, err)
		}
		if again.Order != pair.receipt.Order || again.ExecutedAt != pair.receipt.ExecutedAt ||
			!reflect.DeepEqual(again.Actions, pair.receipt.Actions) {
			t.Fatalf("%s re-execute changed receipt:\nfirst=%+v\nagain=%+v", pair.name, pair.receipt, again)
		}
		if bal, _ := pair.store.TreasuryBalance(); bal != 600 {
			t.Fatalf("%s treasury changed on retry: %d", pair.name, bal)
		}
	}
}
