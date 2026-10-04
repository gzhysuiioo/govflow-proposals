package govflow

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// 两份提案的最小合法状态：gip-1 全程保持干净，gip-2 用于注入坏字符串，
// 验证“坏字符串位于本次没有查询的另一项提案中也整份拒绝”。
const stringTextBaseState = `{
  "magic": "govflow-treasury-state",
  "version": 1,
  "initial_treasury": 100,
  "treasury": 100,
  "balances": {},
  "proposals": {
    "gip-1": {
      "id": "gip-1",
      "state": "passed",
      "timelock_end": 0,
      "actions": ["transfer:alpha:10"]
    },
    "gip-2": {
      "id": "gip-2",
      "state": "passed",
      "timelock_end": 0,
      "actions": ["transfer:beta:20"]
    }
  },
  "receipts": []
}
`

func writeStateVariant(t *testing.T, dir, name string, raw []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// assertCorruptOpen 断言 Open 以 ErrStateCorrupt 拒绝、错误信息包含全部
// 片段，且原文件内容保持原样。
func assertCorruptOpen(t *testing.T, path string, raw []byte, wants ...string) {
	t.Helper()
	s, err := Open(path)
	if !errors.Is(err, ErrStateCorrupt) {
		if s != nil {
			s.Close()
		}
		t.Fatalf("Open err=%v, want ErrStateCorrupt", err)
	}
	for _, w := range wants {
		if !strings.Contains(err.Error(), w) {
			t.Fatalf("error %q missing %q", err.Error(), w)
		}
	}
	got, rerr := os.ReadFile(path)
	if rerr != nil || !equalBytes(got, raw) {
		t.Fatalf("corrupt file was modified")
	}
}

// TestInvalidUTF8InStringsRejected：字符串（含对象键与字段值）中的非法
// UTF-8 字节序列必须整份判损坏，错误说明字节编码非法并给出从文件开头
// 按零计数的出错字节位置。
func TestInvalidUTF8InStringsRejected(t *testing.T) {
	dir := t.TempDir()
	base := []byte(stringTextBaseState)

	// 把另一项提案（gip-2）的动作原文中的 beta 换成含非法字节的写法。
	inject := func(old string, b []byte) []byte {
		raw := bytes.Replace(base, []byte(old), b, 1)
		if bytes.Equal(raw, base) {
			t.Fatalf("setup: %q not found", old)
		}
		return raw
	}

	cases := []struct {
		name string
		raw  []byte
		at   []byte // 出错字节，用于定位期望偏移
	}{
		{"lone 0xFF in value", inject("beta", []byte{'b', 'e', 0xFF, 'a'}), []byte{0xFF}},
		{"lone continuation in value", inject("beta", []byte{'b', 0x80, 't', 'a'}), []byte{0x80}},
		{"truncated multibyte in value", inject("beta", []byte{0xE4, 0xB8}), []byte{0xE4, 0xB8}},
		{"overlong encoding in value", inject("beta", []byte{0xC0, 0xAF}), []byte{0xC0, 0xAF}},
		{"utf8 of surrogate in value", inject("beta", []byte{0xED, 0xA0, 0x80}), []byte{0xED, 0xA0, 0x80}},
		// 对象键同样适用：gip-2 的键（第一次出现）含非法字节。
		{"invalid byte in object key", inject(`"gip-2"`, []byte{'"', 'g', 'i', 'p', 0xFF, '"'}), []byte{0xFF}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			offset := bytes.Index(tc.raw, tc.at)
			if offset < 0 {
				t.Fatalf("setup: offending byte not found")
			}
			path := writeStateVariant(t, dir, strings.ReplaceAll(tc.name, " ", "-")+".json", tc.raw)
			assertCorruptOpen(t, path, tc.raw, "invalid UTF-8", "byte offset "+strconv.Itoa(offset))
		})
	}
}

// TestUnpairedSurrogateEscapeRejected：\uXXXX 转义中的高、低代理项没有按
// 正确次序组成有效字符时整份判损坏，错误说明代理项未配对，位置指向该
// 转义开始的反斜杠。
func TestUnpairedSurrogateEscapeRejected(t *testing.T) {
	dir := t.TempDir()
	base := []byte(stringTextBaseState)

	inject := func(old, new string) []byte {
		raw := bytes.Replace(base, []byte(old), []byte(new), 1)
		if bytes.Equal(raw, base) {
			t.Fatalf("setup: %q not found", old)
		}
		return raw
	}

	cases := []struct {
		name   string
		raw    []byte
		escape string // 未配对的转义，位置指向其反斜杠
	}{
		{"lone high surrogate", inject("beta", `be\uD800ta`), `\uD800`},
		{"lone low surrogate", inject("beta", `be\uDC00ta`), `\uDC00`},
		{"low before high", inject("beta", `\uDC00\uD800`), `\uDC00`},
		{"high then bmp escape", inject("beta", `\uD800A`), `\uD800`},
		{"high then plain char", inject("beta", `\uD800x`), `\uD800`},
		{"high at end of string", inject("beta", `\uD800`), `\uD800`},
		{"high then high", inject("beta", `\uD800\uD801`), `\uD800`},
		// 对象键同样适用：gip-2 的键（第一次出现）含未配对代理项。
		{"unpaired surrogate in object key", inject(`"gip-2"`, `"gip-\uD800"`), `\uD800`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			offset := bytes.Index(tc.raw, []byte(tc.escape))
			if offset < 0 {
				t.Fatalf("setup: escape not found")
			}
			path := writeStateVariant(t, dir, strings.ReplaceAll(tc.name, " ", "-")+".json", tc.raw)
			assertCorruptOpen(t, path, tc.raw, "unpaired surrogate", "byte offset "+strconv.Itoa(offset))
		})
	}
}

// TestValidStringTextPreserved：合法文本不得被误拒绝，也不得被改写——
// 中文、表情等有效 UTF-8 与合法代理项对转义照常读取；同一字符直接写出
// 或采用合法转义是同一文本；明确写出的 U+FFFD 是合法字符；反斜杠转义
// 后的 Unicode 转义字样是普通文本。
func TestValidStringTextPreserved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "treasury.json")
	store, err := InitTreasury(path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	// 中文编号与账户、表情编号、含 U+FFFD 的编号、含“转义字样”普通文本的动作。
	mustRegister(t, store, "提案-😀", 0, "transfer:审计院:100")
	mustRegister(t, store, "bad-�", 0, `plain \uD800 text`)
	if _, err := store.Execute("提案-😀", 0); err != nil {
		t.Fatal(err)
	}
	store.Close()

	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	assertReadable := func(t *testing.T, raw []byte) {
		t.Helper()
		p := writeStateVariant(t, dir, strings.ReplaceAll(t.Name(), "/", "-")+".json", raw)
		s, err := Open(p)
		if err != nil {
			t.Fatalf("valid text rejected: %v", err)
		}
		defer s.Close()
		// 直接写出的中文与表情照常匹配编号与账户。
		if got, err := s.Balance("审计院"); err != nil || got != 100 {
			t.Fatalf("Balance(审计院)=%d err=%v, want 100", got, err)
		}
		rec, ok, err := s.Proposal("提案-😀")
		if err != nil || !ok {
			t.Fatalf("Proposal(提案-😀) ok=%v err=%v", ok, err)
		}
		if len(rec.Actions) != 1 || rec.Actions[0] != "transfer:审计院:100" {
			t.Fatalf("actions rewritten: %q", rec.Actions)
		}
		// 明确写出的 U+FFFD 是合法字符，编号照常匹配。
		if _, ok, err := s.Proposal("bad-�"); err != nil || !ok {
			t.Fatalf("Proposal(bad-�) ok=%v err=%v", ok, err)
		}
		// 反斜杠转义后的 \uD800 字样是普通文本，逐字保留。
		rec2, ok, err := s.Proposal("bad-�")
		if err != nil || !ok || len(rec2.Actions) != 1 || rec2.Actions[0] != `plain \uD800 text` {
			t.Fatalf("escaped-escape text rewritten: %+v ok=%v err=%v", rec2, ok, err)
		}
	}

	t.Run("baseline", func(t *testing.T) { assertReadable(t, good) })

	t.Run("legal escapes spell the same text", func(t *testing.T) {
		// 同一字符直接写出或采用合法转义不影响读取与编号匹配：
		// 中文用 \u63d0、表情用合法高低代理项对 \ud83d\ude00、U+FFFD 用 \ufffd。
		raw := bytes.ReplaceAll(good, []byte("提"), []byte(`\u63d0`))
		raw = bytes.ReplaceAll(raw, []byte("😀"), []byte(`\ud83d\ude00`))
		raw = bytes.ReplaceAll(raw, []byte("�"), []byte(`\ufffd`))
		assertReadable(t, raw)
	})

	t.Run("escaped key spelling still detects duplicates", func(t *testing.T) {
		// 合法转义拼写与普通拼写是同一个键：balances 中同时写出
		// "audit" 与 "\u0061udit" 仍按重复键拒绝，而不是被文本校验放行。
		raw := strings.Replace(string(stringTextBaseState), `"balances": {}`,
			`"balances": {"audit": 1, "\u0061udit": 2}`, 1)
		p := writeStateVariant(t, dir, "dup-escaped-key.json", []byte(raw))
		assertCorruptOpen(t, p, []byte(raw), "duplicate key", "audit")
	})
}

// TestCorruptStringInUnqueriedProposalRejectsAll：坏字符串即使位于本次
// 没有查询的另一项提案中，也整份拒绝读取；命令行将原因写到 stderr，
// 退出码为 1，stdout 不输出提案、余额或凭据的部分结果。
func TestCorruptStringInUnqueriedProposalRejectsAll(t *testing.T) {
	dir := t.TempDir()
	// 只在 gip-2 的动作原文里注入未配对代理项；gip-1 全程干净。
	bad := []byte(strings.Replace(stringTextBaseState, "transfer:beta:20", `transfer:\uD800ops:20`, 1))
	path := writeStateVariant(t, dir, "treasury.json", bad)

	s, err := Open(path)
	if !errors.Is(err, ErrStateCorrupt) {
		if s != nil {
			s.Close()
		}
		t.Fatalf("Open err=%v, want ErrStateCorrupt", err)
	}

	binary := buildCLI(t)
	for _, args := range [][]string{
		{"balances"},
		{"balances", "--json"},
		{"proposal", "--id", "gip-1"}, // 查询的是干净提案，仍须整份拒绝
		{"proposals"},
		{"receipts"},
	} {
		stdout, stderr, code := runCLI(t, binary, path, args...)
		if code != 1 {
			t.Fatalf("%v exited %d, want 1 (stderr=%s)", args, code, stderr)
		}
		if stdout != "" {
			t.Fatalf("%v wrote partial results to stdout: %q", args, stdout)
		}
		if !strings.Contains(stderr, "unpaired surrogate") {
			t.Fatalf("%v stderr missing corrupt reason: %q", args, stderr)
		}
	}
	// 原文件内容保持原样。
	got, rerr := os.ReadFile(path)
	if rerr != nil || !equalBytes(got, bad) {
		t.Fatalf("corrupt file was modified")
	}
}
