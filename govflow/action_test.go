package govflow

import (
	"errors"
	"math"
	"strconv"
	"testing"
)

func TestParseTransfer(t *testing.T) {
	cases := []struct {
		name    string
		action  string
		account string
		amount  int64
		wantErr error
	}{
		{name: "ok", action: "transfer:audits:25000", account: "audits", amount: 25000},
		{name: "zero", action: "transfer:a:0", wantErr: ErrInvalidAction},
		{name: "negative", action: "transfer:a:-5", wantErr: ErrInvalidAction},
		{name: "decimal point", action: "transfer:a:1.5", wantErr: ErrInvalidAction},
		{name: "leading plus", action: "transfer:a:+5", wantErr: ErrInvalidAction},
		{name: "spaces", action: "transfer:a: 5", wantErr: ErrInvalidAction},
		{name: "hex", action: "transfer:a:0x10", wantErr: ErrInvalidAction},
		{name: "empty account", action: "transfer::10", wantErr: ErrInvalidAction},
		{name: "colon in account", action: "transfer:a:b:10", wantErr: ErrInvalidAction},
		{name: "wrong verb", action: "withdraw:a:10", wantErr: ErrInvalidAction},
		{name: "missing amount", action: "transfer:a:", wantErr: ErrInvalidAction},
		{name: "missing segment", action: "transfer:a", wantErr: ErrInvalidAction},
		{name: "empty string", action: "", wantErr: ErrInvalidAction},
		{name: "max int64", action: "transfer:a:9223372036854775807", account: "a", amount: math.MaxInt64},
		{name: "overflow", action: "transfer:a:9223372036854775808", wantErr: ErrInvalidAction},
		{name: "huge overflow", action: "transfer:a:99999999999999999999999999", wantErr: ErrInvalidAction},
		// 账户允许数字开头；金额位允许 007 这样的前导零（仍是十进制正整数）。
		{name: "leading zeros", action: "transfer:acme:007", account: "acme", amount: 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			account, amount, err := ParseTransfer(tc.action)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ParseTransfer(%q) err=%v, want %v", tc.action, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTransfer(%q) unexpected error: %v", tc.action, err)
			}
			if account != tc.account || amount != tc.amount {
				t.Fatalf("ParseTransfer(%q) = %q,%d; want %q,%d", tc.action, account, amount, tc.account, tc.amount)
			}
		})
	}
}

func TestAddInt64(t *testing.T) {
	if v, ok := addInt64(math.MaxInt64-1, 1); !ok || v != math.MaxInt64 {
		t.Fatalf("max boundary add failed: %d %v", v, ok)
	}
	if _, ok := addInt64(math.MaxInt64, 1); ok {
		t.Fatal("expected overflow adding 1 to MaxInt64")
	}
	if v, ok := addInt64(math.MinInt64+1, -1); !ok || v != math.MinInt64 {
		t.Fatalf("min boundary add failed: %d %v", v, ok)
	}
	if _, ok := addInt64(math.MinInt64, -1); ok {
		t.Fatal("expected overflow subtracting 1 from MinInt64")
	}
}

// TestTransferLedgerSharedRule 锁定首次执行预演与凭据重放共用的唯一转账规则：
// 资金库 1000，依次向同一账户转 250、100，再向另一账户转 50，最终资金库 600，
// 两账户分别 350 和 50，且每笔前后余额承接上一笔结果。
func TestTransferLedgerSharedRule(t *testing.T) {
	l := newTransferLedger(1000, nil)
	want := []struct {
		acct                   string
		amount                 int64
		tb, ta, rb, ra         int64
		overflow, insufficient bool
	}{
		{"audits", 250, 1000, 750, 0, 250, false, false},
		{"audits", 100, 750, 650, 250, 350, false, false}, // 同账户连续收款，起始承接 250
		{"legal", 50, 650, 600, 0, 50, false, false},      // 新账户从零开始
	}
	for i, w := range want {
		m := l.apply("transfer:" + w.acct + ":" + strconvInt(w.amount))
		if m.parseErr != nil {
			t.Fatalf("movement %d parse error: %v", i, m.parseErr)
		}
		if m.account != w.acct || m.amount != w.amount {
			t.Fatalf("movement %d account/amount = %s/%d", i, m.account, m.amount)
		}
		if m.treasuryBefore != w.tb || m.treasuryAfter != w.ta {
			t.Fatalf("movement %d treasury %d->%d, want %d->%d", i, m.treasuryBefore, m.treasuryAfter, w.tb, w.ta)
		}
		if m.recipientBefore != w.rb || m.recipientAfter != w.ra {
			t.Fatalf("movement %d recipient %d->%d, want %d->%d", i, m.recipientBefore, m.recipientAfter, w.rb, w.ra)
		}
		if m.recipientOverflow != w.overflow || m.insufficient != w.insufficient {
			t.Fatalf("movement %d flags overflow=%v insufficient=%v", i, m.recipientOverflow, m.insufficient)
		}
	}
	if got := l.Treasury(); got != 600 {
		t.Fatalf("treasury = %d, want 600", got)
	}
	if l.Balances()["audits"] != 350 || l.Balances()["legal"] != 50 {
		t.Fatalf("balances = %+v, want audits=350 legal=50", l.Balances())
	}
	if _, seen := l.Balances()["ghost"]; seen {
		t.Fatal("ledger must not materialize untouched accounts")
	}
}

// TestTransferLedgerExactRemainingAmount：金额恰好等于资金库剩余余额时成功，
// 资金库归零，收款账户全额到账。
func TestTransferLedgerExactRemainingAmount(t *testing.T) {
	l := newTransferLedger(100, map[string]int64{})
	m := l.apply("transfer:a:100")
	if m.parseErr != nil || m.insufficient || m.recipientOverflow {
		t.Fatalf("exact-amount spend rejected: %+v", m)
	}
	if m.treasuryBefore != 100 || m.treasuryAfter != 0 || m.recipientBefore != 0 || m.recipientAfter != 100 {
		t.Fatalf("exact-amount movement = %+v", m)
	}
	if l.Treasury() != 0 || l.Balances()["a"] != 100 {
		t.Fatalf("ledger after exact spend: treasury=%d balances=%+v", l.Treasury(), l.Balances())
	}
	// 归零后再支取任何正金额都不足，且账本不被推进。
	m = l.apply("transfer:a:1")
	if !m.insufficient || m.treasuryAfter != -1 {
		t.Fatalf("zero-treasury spend: %+v", m)
	}
	if l.Treasury() != 0 || l.Balances()["a"] != 100 {
		t.Fatalf("insufficient spend advanced the ledger: treasury=%d a=%d", l.Treasury(), l.Balances()["a"])
	}
}

// TestTransferLedgerFailuresDoNotAdvance：收款溢出与资金库不足都不推进账本，
// 动作原文非法时返回解析错误。同一动作可同时具备溢出与不足两个标志，
// 由两条调用路径各自决定先报哪个，但两者都不产生资金变动。
func TestTransferLedgerFailuresDoNotAdvance(t *testing.T) {
	// 收款账户已在 int64 上限附近：再收款必溢出。
	l := newTransferLedger(math.MaxInt64, map[string]int64{"a": math.MaxInt64 - 10})
	m := l.apply("transfer:a:11")
	if !m.recipientOverflow {
		t.Fatalf("expected recipient overflow, got %+v", m)
	}
	if l.Treasury() != math.MaxInt64 || l.Balances()["a"] != math.MaxInt64-10 {
		t.Fatalf("overflow advanced the ledger: treasury=%d a=%d", l.Treasury(), l.Balances()["a"])
	}

	// 资金库不足且收款会溢出同时成立：两个标志同时给出，账本仍不推进。
	l = newTransferLedger(1, map[string]int64{"a": math.MaxInt64})
	m = l.apply("transfer:a:" + strconvInt(math.MaxInt64))
	if !m.insufficient || !m.recipientOverflow {
		t.Fatalf("want both insufficient and overflow, got %+v", m)
	}
	if l.Treasury() != 1 || l.Balances()["a"] != math.MaxInt64 {
		t.Fatalf("failed movement advanced the ledger")
	}

	// 资金库不足：报不足，账本不推进。
	l = newTransferLedger(5, nil)
	m = l.apply("transfer:b:6")
	if !m.insufficient || m.recipientOverflow {
		t.Fatalf("want insufficient only, got %+v", m)
	}
	if l.Treasury() != 5 {
		t.Fatalf("insufficient spend changed treasury: %d", l.Treasury())
	}
	if _, seen := l.Balances()["b"]; seen {
		t.Fatal("insufficient spend created the recipient account")
	}

	// 动作原文非法：解析错误透传给调用方，账本不动。
	m = l.apply("withdraw:b:1")
	if m.parseErr == nil {
		t.Fatal("malformed action must surface a parse error")
	}
	if l.Treasury() != 5 {
		t.Fatalf("malformed action changed treasury: %d", l.Treasury())
	}
}

func strconvInt(v int64) string { return strconv.FormatInt(v, 10) }
