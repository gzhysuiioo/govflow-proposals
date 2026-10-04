package govflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestStrictStringScannerAcceptsValidText：合法字符不能被误拒绝。
// 中文、表情、用户明确写出的 U+FFFD（直接字节与 `\uFFFD` 转义两种写法）、
// 合法高低代理项对、反斜杠转义后出现的字面 “\uD800” 文本都必须放行。
func TestStrictStringScannerAcceptsValidText(t *testing.T) {
	valid := [][]byte{
		[]byte(`{"magic":"govflow-treasury-state"}`),
		[]byte(`{"账户":"余额","动作":"transfer:审计:100"}`),
		[]byte(`{"emoji":"😀","pair":"😀","mixed":"a😀b中c"}`),
		// 用户明确写出的 U+FFFD：直接写出 EF BF BD 与 `\uFFFD` 转义都是合法字符。
		[]byte("{\"a\":\"�\",\"b\":\"�\"}"),
		// \uFFFD 转义拼写与直接字节是同一个合法字符。
		[]byte(`{"a":"\uFFFD","b":"prefix\uFFFDsuffix"}`),
		[]byte(`{"key�":"�"}`),
		// 合法高、低代理项对：U+1F600 与 U+10000。
		[]byte(`{"a":"😀","b":"𐀀"}`),
		// 反斜杠自身被转义后，“uD800”只是普通文本，不参与代理项配对。
		[]byte(`{"a":"\\uD800","b":"x\\uDE00y","c":"\\\\uD800"}`),
		// 转义引号、其它单字符转义与对象键中的代理项对。
		[]byte(`{"a\"b":"quote\"inside","c":"tab\tnewline\nslash\\/done"}`),
		// " 转义表示引号字符，只是解码后的普通数据，不能当成原始字符串的
		// 结束引号（反引号原始字面量保留 6 个转义字符）。
		[]byte(`{"a":"x\u0022y"}`),
		// \ 转义表示反斜杠字符，同样只是解码后的普通数据：它不转义紧随
		// 其后的原始 \u 转义，因此后面的高代理项仍需低代理项配对。
		[]byte(`{"a":"x\\u005c\uD83D\uDE00"}`),
		// 非 BMP 字符直接以 UTF-8 出现在对象键（业务编号表键）中。
		[]byte(`{"balances":{"😀":10}}`),
	}
	for i, doc := range valid {
		if err := checkStrictStrings(doc); err != nil {
			t.Fatalf("case %d valid document rejected: %v\n%s", i, err, doc)
		}
	}
}

// TestStrictStringScannerRejectsInvalidUTF8：非法 UTF-8 字节序列必须被拒绝，
// 位置从文件开头按零计数，指向首个非法字节。
func TestStrictStringScannerRejectsInvalidUTF8(t *testing.T) {
	cases := []struct {
		name      string
		doc       []byte
		wantOff   int
		wantInErr string
	}{
		{
			name:      "impossible byte in value",
			doc:       []byte("{\"a\":\"b\xff\"}"),
			wantOff:   7,
			wantInErr: "invalid UTF-8 encoding",
		},
		{
			name:      "impossible byte in object key",
			doc:       []byte("{\"k\xff\":1}"),
			wantOff:   3,
			wantInErr: "invalid UTF-8 encoding",
		},
		{
			name:      "truncated multibyte sequence",
			doc:       []byte("{\"a\":\"\xe4\xb8\"}"), // E4 B8 缺少尾字节
			wantOff:   6,
			wantInErr: "invalid UTF-8 encoding",
		},
		{
			name:      "WTF-8 encoded surrogate",
			doc:       []byte("{\"a\":\"\xed\xa0\x80\"}"), // U+D800 的 WTF-8 编码
			wantOff:   6,
			wantInErr: "invalid UTF-8 encoding",
		},
		{
			name:      "overlong encoding",
			doc:       []byte("{\"a\":\"\xc0\x80\"}"),
			wantOff:   6,
			wantInErr: "invalid UTF-8 encoding",
		},
		{
			name:      "code point beyond U+10FFFF",
			doc:       []byte("{\"a\":\"\xf4\x90\x80\x80\"}"),
			wantOff:   6,
			wantInErr: "invalid UTF-8 encoding",
		},
		{
			name:      "stray continuation byte in map key",
			doc:       []byte("{\"balances\":{\"\x80\":1}}"),
			wantOff:   14,
			wantInErr: "invalid UTF-8 encoding",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkStrictStrings(tc.doc)
			if err == nil {
				t.Fatalf("document accepted, want corrupt: %q", tc.doc)
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.wantInErr) {
				t.Fatalf("error %q does not mention %q", msg, tc.wantInErr)
			}
			want := "byte offset " + strconv.Itoa(tc.wantOff)
			if !strings.Contains(msg, want) {
				t.Fatalf("error %q does not contain %q", msg, want)
			}
		})
	}
}

// TestStrictStringScannerRejectsUnpairedSurrogates：\u 转义的高、低代理项
// 必须按正确次序成对出现；未配对时报错位置指向该转义开始的反斜杠。
func TestStrictStringScannerRejectsUnpairedSurrogates(t *testing.T) {
	cases := []struct {
		name       string
		doc        []byte
		wantOff    int
		kind       string // "unpaired high surrogate" / "unpaired low surrogate"
		wantEscape string
	}{
		{
			name:       "lone high surrogate in value",
			doc:        []byte(`{"a":"\uD800"}`),
			wantOff:    6,
			kind:       "unpaired high surrogate",
			wantEscape: `\uD800`,
		},
		{
			name:       "lone low surrogate in value",
			doc:        []byte(`{"a":"\uDE00"}`),
			wantOff:    6,
			kind:       "unpaired low surrogate",
			wantEscape: `\uDE00`,
		},
		{
			name:       "lone surrogate in object key",
			doc:        []byte(`{"\uD800":1}`),
			wantOff:    2,
			kind:       "unpaired high surrogate",
			wantEscape: `\uD800`,
		},
		{
			name:       "high surrogate followed by BMP escape",
			doc:        []byte(`{"a":"\uD800A"}`),
			wantOff:    6,
			kind:       "unpaired high surrogate",
			wantEscape: `\uD800`,
		},
		{
			name:       "high surrogate followed by plain text",
			doc:        []byte(`{"a":"\uD800x"}`),
			wantOff:    6,
			kind:       "unpaired high surrogate",
			wantEscape: `\uD800`,
		},
		{
			name:       "high surrogate at end of string",
			doc:        []byte(`{"a":"\uD83D"}`),
			wantOff:    6,
			kind:       "unpaired high surrogate",
			wantEscape: `\uD83D`,
		},
		{
			name:       "two high surrogates",
			doc:        []byte(`{"a":"😀\uD800"}`),
			wantOff:    10,
			kind:       "unpaired high surrogate",
			wantEscape: `\uD800`,
		},
		{
			name:       "extra low surrogate after a valid pair",
			doc:        []byte(`{"a":"😀\uDE00"}`),
			wantOff:    10,
			kind:       "unpaired low surrogate",
			wantEscape: `\uDE00`,
		},
		{
			name:       "low surrogate uppercase hex digits",
			doc:        []byte(`{"a":"\ude00"}`),
			wantOff:    6,
			kind:       "unpaired low surrogate",
			wantEscape: `\uDE00`,
		},
		{
			// 反斜杠经 \ 转义得到（解码后只是普通数据），不会去转义紧随
			// 其后的原始高代理项转义：该高代理项仍未配对。
			name:       "decoded backslash does not escape next surrogate",
			doc:        []byte(`{"a":"x\\u005c\ud800"}`),
			wantOff:    14,
			kind:       "unpaired high surrogate",
			wantEscape: `\uD800`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkStrictStrings(tc.doc)
			if err == nil {
				t.Fatalf("document accepted, want corrupt: %s", tc.doc)
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.kind) {
				t.Fatalf("error %q does not classify as %q", msg, tc.kind)
			}
			want := "byte offset " + strconv.Itoa(tc.wantOff)
			if !strings.Contains(msg, want) {
				t.Fatalf("error %q does not contain %q", msg, want)
			}
			if !strings.Contains(msg, tc.wantEscape) {
				t.Fatalf("error %q does not quote escape %q", msg, tc.wantEscape)
			}
		})
	}
}

// TestStrictStringScannerEscapeSpellingsEquivalent：同一字符直接写出与采用
// 合法转义，扫描放行后交给 encoding/json 得到完全相同的字符串；两种拼写
// 若出现在同一对象中，结构扫描按解读后的键判为重复键。
func TestStrictStringScannerEscapeSpellingsEquivalent(t *testing.T) {
	direct := []byte(`{"😀":1}`)
	escaped := []byte(`{"😀":1}`)
	if err := checkStrictStrings(direct); err != nil {
		t.Fatalf("direct: %v", err)
	}
	if err := checkStrictStrings(escaped); err != nil {
		t.Fatalf("escaped: %v", err)
	}
	var d, e map[string]int
	if err := json.Unmarshal(direct, &d); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(escaped, &e); err != nil {
		t.Fatal(err)
	}
	if len(d) != 1 || len(e) != 1 {
		t.Fatalf("unexpected maps: %v %v", d, e)
	}
	for k := range d {
		if _, ok := e[k]; !ok {
			t.Fatalf("direct key %q not equal to escaped-spelled key", k)
		}
	}
	// 两种拼写在同一对象中是同一个键：字符串扫描放行，结构扫描必须判重。
	// 重复键检查放在 balances 业务编号表内（根对象按固定字段名校验形状）。
	dup := []byte(`{"balances":{"😀":1,"😀":2}}`)
	if err := checkStrictStrings(dup); err != nil {
		t.Fatalf("strict scan of duplicate spellings: %v", err)
	}
	if err := checkStateStructure(dup); err == nil ||
		!strings.Contains(err.Error(), "duplicate key") {
		t.Fatalf("want duplicate key error, got %v", err)
	}
}

// TestStrictStateRejectsInvalidStringAnywhere：坏字符串即使出现在本次没有
// 查询的另一项提案中，也拒绝读取整份文件，原文件保持原样，查询不给出
// 余额、提案或凭据的部分结果。
func TestStrictStateRejectsInvalidStringAnywhere(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "treasury.json")
	store, err := InitTreasury(path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	mustRegister(t, store, "gip-1", 0, "transfer:audits:100")
	if _, err := store.Execute("gip-1", 0); err != nil {
		t.Fatal(err)
	}
	// gip-2 已登记但未执行：它的动作原文不参与凭据重放，旧逻辑下坏字符
	// 被改写成 U+FFFD 后整份文件仍会被接受。
	mustRegister(t, store, "gip-2", 0, "transfer:other:5")
	store.Close()

	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	corruptions := map[string][]byte{
		"invalid utf8 in unrelated proposal action": bytesReplaceAll(good,
			[]byte("transfer:other:5"), []byte("transfer:o\xffher:5")),
		"lone surrogate in unrelated proposal action": bytesReplaceAll(good,
			[]byte("transfer:other:5"), []byte(`transfer:other:5\uD800`)),
		"invalid utf8 in unrelated proposal id": bytesReplaceAll(good,
			[]byte(`"gip-2"`), []byte("\"gip-2\xff\"")),
	}
	for name, raw := range corruptions {
		t.Run(name, func(t *testing.T) {
			bad := filepath.Join(dir, "strict-"+strings.ReplaceAll(name, " ", "-")+".json")
			if err := os.WriteFile(bad, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			s, err := Open(bad)
			if !errors.Is(err, ErrStateCorrupt) {
				if s != nil {
					s.Close()
				}
				t.Fatalf("Open err=%v, want ErrStateCorrupt", err)
			}
			if !strings.Contains(err.Error(), "byte offset") {
				t.Fatalf("error missing byte offset: %v", err)
			}
			// 原文件保持原样，不被重建或覆盖。
			if got, rerr := os.ReadFile(bad); rerr != nil || !equalBytes(got, raw) {
				t.Fatalf("corrupt file was modified")
			}
			// 对损坏文件初始化同样被拒绝（文件已存在）。
			if _, err := InitTreasury(bad, 0); !errors.Is(err, ErrTreasuryAlreadyInit) {
				t.Fatalf("InitTreasury over corrupt err=%v", err)
			}
		})
	}
}

// TestStrictStateCoherentRenameWithInvalidByte：把账户名在提案动作、凭据
// 动作原文、凭据收款账户与余额表键中一致地改成含非法字节的拼写——旧逻辑
// 下凭据重放仍然一致，查询却会展示 U+FFFD 账户名；新逻辑必须仅凭字符串
// 合法性整份拒绝。
func TestStrictStateCoherentRenameWithInvalidByte(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "treasury.json")
	store, err := InitTreasury(path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	mustRegister(t, store, "gip-1", 0, "transfer:audits:100")
	if _, err := store.Execute("gip-1", 0); err != nil {
		t.Fatal(err)
	}
	store.Close()

	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw := bytesReplaceAll(good, []byte("audits"), []byte("\xffudits"))
	bad := filepath.Join(dir, "coherent-rename.json")
	if err := os.WriteFile(bad, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(bad)
	if !errors.Is(err, ErrStateCorrupt) {
		if s != nil {
			s.Close()
		}
		t.Fatalf("Open err=%v, want ErrStateCorrupt (replay alone would accept this)", err)
	}
	if !strings.Contains(err.Error(), "invalid UTF-8 encoding") {
		t.Fatalf("error does not identify byte encoding problem: %v", err)
	}
}

// TestStrictStatePreservesValidUnicodeRoundTrip：合法 Unicode 文本在编号匹配、
// 大小写区分与查询输出中逐字保留；用户明确写出的 U+FFFD 也是合法账户名。
func TestStrictStatePreservesValidUnicodeRoundTrip(t *testing.T) {
	store, path := openTempStore(t, 1000)
	defer store.Close()
	mustRegister(t, store, "提案-甲", 0, "transfer:账户甲:100", "transfer:😀:50", "transfer:�:5")
	if _, err := store.Execute("提案-甲", 0); err != nil {
		t.Fatalf("execute unicode proposal: %v", err)
	}
	// 同内容重试仍按精确匹配判定为幂等。
	if existed, err := store.Register("提案-甲", 0,
		[]string{"transfer:账户甲:100", "transfer:😀:50", "transfer:�:5"}); err != nil || !existed {
		t.Fatalf("idempotent retry existed=%v err=%v", existed, err)
	}
	// 大小写区分：Audit 与 audit 互不影响（ASCII 回归）。
	mustRegister(t, store, "gip-ascii", 0, "transfer:Audit:7")
	if _, err := store.Execute("gip-ascii", 0); err != nil {
		t.Fatal(err)
	}

	if got, err := store.Balance("账户甲"); err != nil || got != 100 {
		t.Fatalf("Balance(账户甲)=%d err=%v", got, err)
	}
	if got, err := store.Balance("😀"); err != nil || got != 50 {
		t.Fatalf("Balance(😀)=%d err=%v", got, err)
	}
	if got, err := store.Balance("�"); err != nil || got != 5 {
		t.Fatalf("Balance(U+FFFD)=%d err=%v", got, err)
	}
	if got, err := store.Balance("Audit"); err != nil || got != 7 {
		t.Fatalf("Balance(Audit)=%d err=%v", got, err)
	}
	if got, err := store.Balance("audit"); err != nil || got != 0 {
		t.Fatalf("Balance(audit)=%d err=%v, want 0 (case-sensitive)", got, err)
	}
	rec, ok, err := store.Receipt("提案-甲")
	if err != nil || !ok {
		t.Fatalf("Receipt ok=%v err=%v", ok, err)
	}
	if len(rec.Actions) != 3 || rec.Actions[1].Recipient.Account != "😀" {
		t.Fatalf("receipt actions not preserved verbatim: %+v", rec.Actions)
	}

	// 重新打开：落盘文件中的合法 UTF-8 必须照常读取，查询形状不变。
	store.Close()
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	snap, err := reopened.BalanceSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snap.Treasury != 838 || snap.Balances["账户甲"] != 100 || snap.Balances["�"] != 5 {
		t.Fatalf("snapshot after reopen lost unicode text: %+v", snap.Balances)
	}
}

// TestStrictStateCLIBehavior：命令行沿用既有损坏文件处理方式——原因写
// stderr（含字节偏移）、退出码 1、stdout 不输出任何提案、余额或凭据；
// 查询的不是坏串所在提案也同样拒绝。
func TestStrictStateCLIBehavior(t *testing.T) {
	binary := buildCLI(t)
	dir := t.TempDir()
	state := filepath.Join(dir, "treasury.json")
	if _, _, code := runCLI(t, binary, state, "init", "--balance", "1000"); code != 0 {
		t.Fatal("init failed")
	}
	if _, _, code := runCLI(t, binary, state, "register", "--id", "gip-1",
		"--timelock", "0", "--action", "transfer:audits:100"); code != 0 {
		t.Fatal("register gip-1 failed")
	}
	if _, _, code := runCLI(t, binary, state, "execute", "--id", "gip-1", "--now", "0"); code != 0 {
		t.Fatal("execute gip-1 failed")
	}
	if _, _, code := runCLI(t, binary, state, "register", "--id", "gip-2",
		"--timelock", "0", "--action", "transfer:other:5"); code != 0 {
		t.Fatal("register gip-2 failed")
	}

	good, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"invalid utf8": bytesReplaceAll(good,
			[]byte("transfer:other:5"), []byte("transfer:o\xffher:5")),
		"unpaired surrogate": bytesReplaceAll(good,
			[]byte("transfer:other:5"), []byte(`transfer:other\uD800:5`)),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			bad := filepath.Join(dir, "cli-"+strings.ReplaceAll(name, " ", "-")+".json")
			if err := os.WriteFile(bad, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			for _, query := range [][]string{
				{"balances", "--json"},
				{"proposal", "--id", "gip-1", "--json"},
				{"proposals"},
				{"receipts", "--json"},
			} {
				stdout, stderr, code := runCLI(t, binary, bad, query...)
				if code != 1 {
					t.Fatalf("%v exit=%d stderr=%s", query, code, stderr)
				}
				if stdout != "" {
					t.Fatalf("%v emitted partial stdout:\n%s", query, stdout)
				}
				if !strings.Contains(stderr, "treasury state file is corrupt") {
					t.Fatalf("%v stderr missing corrupt prefix: %s", query, stderr)
				}
				if !strings.Contains(stderr, "byte offset") {
					t.Fatalf("%v stderr missing byte offset: %s", query, stderr)
				}
			}
			if got, rerr := os.ReadFile(bad); rerr != nil || !bytes.Equal(got, raw) {
				t.Fatalf("corrupt file was modified for %s", name)
			}
		})
	}

	// 未被污染的文件仍正常查询，输出形状不变。
	stdout, _, code := runCLI(t, binary, state, "balances", "--account", "audits")
	if code != 0 || !strings.Contains(stdout, "audits 100") {
		t.Fatalf("clean file query broken: code=%d stdout=%s", code, stdout)
	}
}
