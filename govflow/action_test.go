package govflow

import (
	"errors"
	"math"
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
