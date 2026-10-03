package batchreg

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnmarshalStringStrict(t *testing.T) {
	fffd := string(rune(0xfffd))
	valid := []struct{ raw, want string }{
		{`"abc"`, "abc"},
		{`"中文"`, "中文"},
		{`"😀"`, "😀"},
		{`"a�b"`, "a" + fffd + "b"}, // genuine U+FFFD written directly
		{`"�"`, fffd},               // genuine U+FFFD written as an escape
		{`"B-001"`, "B-001"},
		{`"😀"`, "😀"}, // emoji as a matched surrogate-pair escape
		{`"\n\t\"\\"`, "\n\t\"\\"},
		{"  \"x\"\r\n", "x"}, // JSON whitespace around the token
	}
	for _, tc := range valid {
		got, err := unmarshalStringStrict([]byte(tc.raw))
		if err != nil {
			t.Errorf("unmarshalStringStrict(%s) unexpected error: %v", tc.raw, err)
			continue
		}
		if got != tc.want {
			t.Errorf("unmarshalStringStrict(%s) = %q, want %q", tc.raw, got, tc.want)
		}
	}

	encodingBad := []string{
		"\"B\xff\"",      // raw invalid byte
		"\"B\xe4\xb8\"",  // truncated three-byte sequence
		"\"B\xc0\x80\"",  // overlong NUL
		`"\ud800"`,       // lone high surrogate
		`"\uD800A"`,      // high surrogate followed by a plain character
		`"\udc00"`,       // lone low surrogate
		`"\ud83d\ud83d"`, // two high surrogates
		`"\ud83d�"`,      // high surrogate not followed by a low one
		"\" x \xff \"",   // invalid byte wrapped in trim-able spaces
	}
	for _, raw := range encodingBad {
		got, err := unmarshalStringStrict([]byte(raw))
		if !errors.Is(err, errStringEncoding) {
			t.Errorf("unmarshalStringStrict(% x) err = %v, want encoding error (got %q)", raw, err, got)
		}
	}

	syntaxBad := []string{
		`null`,     // not a string
		`1`,        // not a string
		`"abc"x`,   // trailing data
		`"abc`,     // unterminated
		`"\q"`,     // bad escape
		"\"a\tb\"", // raw control byte
		"\"x\"\v",  // U+000B is not JSON whitespace
		`"\u12"`,   // short escape
		`"\u00zz"`, // non-hex code unit
	}
	for _, raw := range syntaxBad {
		if _, err := unmarshalStringStrict([]byte(raw)); !errors.Is(err, errStringNotJSON) {
			t.Errorf("unmarshalStringStrict(%q) err = %v, want syntax error", raw, err)
		}
	}
}

func TestNormalizeFieldEncoding(t *testing.T) {
	// Valid Unicode of every flavor is kept verbatim (after trimming).
	for _, in := range []string{"批次-A", "😀", "B" + string(rune(0xfffd)), "\tB-1 \n"} {
		if _, err := NormalizeField(in); err != nil {
			t.Errorf("NormalizeField(%q) unexpected error: %v", in, err)
		}
	}
	got, err := NormalizeField(" B" + string(rune(0xfffd)) + " ")
	if err != nil || got != "B"+string(rune(0xfffd)) {
		t.Fatalf("genuine U+FFFD must be ordinary text, got %q err=%v", got, err)
	}

	// Malformed bytes are refused, including when whitespace frames them.
	for _, in := range []string{
		string([]byte{'B', 0xff}),
		string([]byte{' ', 'B', 0xff, ' '}),
		string([]byte{0xe4, 0xb8}),
	} {
		if got, err := NormalizeField(in); err == nil {
			t.Fatalf("NormalizeField(% x) = %q, expected rejection", in, got)
		}
	}
}

func TestLoadRejectsInvalidUTF8Registry(t *testing.T) {
	dir := t.TempDir()
	// BYTE stands in for a real invalid UTF-8 byte inside the raw strings;
	// the surrogate cases write the JSON escape literally.
	const BYTE = "\x00BYTE\x00"
	cases := map[string]struct {
		content    string
		position   int
		field      string
		batchShown string
	}{
		"bad batch bytes": {
			`{"version":1,"batches":[{"batch":"B` + BYTE + `","product":"P","quantity":1,"unit":"kg"}]}`,
			1, "batch", "",
		},
		"bad product bytes shows valid batch": {
			`{"version":1,"batches":[{"batch":"B42","product":"P` + BYTE + `","quantity":1,"unit":"kg"}]}`,
			1, "product", "B42",
		},
		"bad unit on second record": {
			`{"version":1,"batches":[
			 {"batch":"B1","product":"P","quantity":1,"unit":"kg"},
			 {"batch":"B2","product":"P","quantity":2,"unit":"kg` + BYTE + `"}]}`,
			2, "unit", "B2",
		},
		"lone surrogate escape in batch": {
			`{"version":1,"batches":[{"batch":"B\ud800","product":"P","quantity":1,"unit":"kg"}]}`,
			1, "batch", "",
		},
		"lone low surrogate escape in unit": {
			`{"version":1,"batches":[{"batch":"B9","product":"P","quantity":1,"unit":"u\udc01"}]}`,
			1, "unit", "B9",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name+".json")
			content := []byte(strings.ReplaceAll(tc.content, BYTE, string([]byte{0xff})))
			if err := os.WriteFile(path, content, 0o644); err != nil {
				t.Fatal(err)
			}
			_, _, err := Load(path)
			var fe *FormatError
			if !errors.As(err, &fe) {
				t.Fatalf("expected FormatError, got %v", err)
			}
			if fe.Position != tc.position || fe.Field != tc.field || fe.Batch != tc.batchShown {
				t.Fatalf("FormatError = %+v, want position=%d field=%s batch=%q",
					fe, tc.position, tc.field, tc.batchShown)
			}
		})
	}
}

func TestLoadAcceptsUnicodeAndSurrogatePairs(t *testing.T) {
	content := `{"version":1,"batches":[
		{"batch":"批次-1","product":"产品😀","quantity":1,"unit":"千克"},
		{"batch":"B-2","product":"P�","quantity":2,"unit":"box"},
		{"batch":"pair","product":"😀","quantity":3,"unit":"x"}
	]}`
	path := filepath.Join(t.TempDir(), "r.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, existed, err := Load(path)
	if err != nil || !existed {
		t.Fatalf("Load: %v", err)
	}
	want := []Batch{
		{Batch: "批次-1", Product: "产品😀", Quantity: 1, Unit: "千克"},
		{Batch: "B-2", Product: "P" + string(rune(0xfffd)), Quantity: 2, Unit: "box"},
		{Batch: "pair", Product: "😀", Quantity: 3, Unit: "x"},
	}
	for i, w := range want {
		if reg.Batches[i] != w {
			t.Errorf("record %d = %+v, want %+v", i+1, reg.Batches[i], w)
		}
	}
}

func TestParseManifestEncoding(t *testing.T) {
	// One bad field rejects the whole manifest even when an earlier record
	// is perfectly legal; position is 1-based and the batch id follows the
	// "only when itself valid" rule. The BYTE placeholder is replaced with
	// an actual invalid UTF-8 byte, while SUR stays a literal lone-surrogate
	// JSON escape.
	const BYTE = "\x00BYTE\x00"
	const SUR = "\x00SUR\x00"
	replace := func(s string) []byte {
		r := strings.NewReplacer(BYTE, string([]byte{0xff}), SUR, `\ud800`)
		return []byte(r.Replace(s))
	}
	cases := map[string]struct {
		content string
		pos     int
		field   string
		batch   string
	}{
		"bad unit on record 2": {
			`[{"batch":"OK1","product":"P","quantity":1,"unit":"kg"},
			  {"batch":"BAD2","product":"P","quantity":2,"unit":"kg` + BYTE + `"}]`,
			2, "unit", "BAD2",
		},
		"bad batch shows no id": {
			`[{"batch":"B` + BYTE + `","product":"P","quantity":1,"unit":"kg"}]`,
			1, "batch", "",
		},
		"bad product shows valid batch": {
			`[{"batch":"B7","product":"P` + BYTE + `","quantity":1,"unit":"kg"}]`,
			1, "product", "B7",
		},
		"lone surrogate batch": {
			`[{"batch":"B` + SUR + `","product":"P","quantity":1,"unit":"kg"}]`,
			1, "batch", "",
		},
		"bad bytes after leading spaces": {
			`[{"batch":"  B` + BYTE + `  ","product":"P","quantity":1,"unit":"kg"}]`,
			1, "batch", "",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseManifest(replace(tc.content))
			var re *ManifestRecordError
			if !errors.As(err, &re) {
				t.Fatalf("expected ManifestRecordError, got %v", err)
			}
			if re.Position != tc.pos || re.Batch != tc.batch {
				t.Fatalf("position=%d batch=%q, want %d/%q", re.Position, re.Batch, tc.pos, tc.batch)
			}
			if !strings.Contains(re.Reason, tc.field) {
				t.Fatalf("reason %q must name field %q", re.Reason, tc.field)
			}
		})
	}

	// Genuine U+FFFD, non-ASCII text and matched pairs are accepted, and an
	// escape decodes to the same text as the character written directly.
	inputs, err := ParseManifest([]byte(`[
		{"batch":"批次 A","product":"P�","quantity":1,"unit":"千克"},
		{"batch":"批次 A","product":"P�","quantity":1,"unit":"千克"}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	if inputs[0] != inputs[1] {
		t.Fatalf("escaped and direct U+FFFD must be the same text: %+v vs %+v", inputs[0], inputs[1])
	}
}

func TestImportEncodingGuardLeavesRegistryUntouched(t *testing.T) {
	original := []Batch{{Batch: "OLD", Product: "P", Quantity: 1, Unit: "kg"}}
	reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), original...)}
	bad := string([]byte{'N', 0xff})
	_, err := Import(reg, []Input{
		{Batch: "NEW", Product: "P", Quantity: 2, Unit: "kg"},
		{Batch: bad, Product: "P", Quantity: 3, Unit: "kg"},
	})
	var re *ManifestRecordError
	if !errors.As(err, &re) {
		t.Fatalf("expected ManifestRecordError, got %v", err)
	}
	if re.Position != 2 || re.Batch != "" {
		t.Fatalf("position=%d batch=%q, want 2/empty", re.Position, re.Batch)
	}
	if len(reg.Batches) != 1 || reg.Batches[0] != original[0] {
		t.Fatalf("registry mutated by rejected import: %+v", reg.Batches)
	}
}

func TestRegisterEncodingGuard(t *testing.T) {
	reg := &Registry{Version: FormatVersion}
	bad := string([]byte{'k', 'g', 0xff})
	cases := []struct {
		name  string
		in    Input
		field string
	}{
		{"batch", Input{Batch: bad, Product: "P", Quantity: 1, Unit: "kg"}, "batch"},
		{"product", Input{Batch: "B", Product: bad, Quantity: 1, Unit: "kg"}, "product"},
		{"unit", Input{Batch: "B", Product: "P", Quantity: 1, Unit: bad}, "unit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Register(reg, tc.in)
			var ee *EncodingError
			if !errors.As(err, &ee) || ee.Field != tc.field {
				t.Fatalf("got %v, want EncodingError on field %q", err, tc.field)
			}
			if len(reg.Batches) != 0 {
				t.Fatalf("rejected register appended a record: %+v", reg.Batches)
			}
		})
	}
}

func TestSaveEncodingGuardCreatesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "registry.json")
	reg := &Registry{Version: FormatVersion, Batches: []Batch{
		{Batch: "OK", Product: "P", Quantity: 1, Unit: "kg"},
		// A genuine U+FFFD is valid text and saves fine.
		{Batch: "B" + string(rune(0xfffd)), Product: "P", Quantity: 2, Unit: "kg"},
		{Batch: string([]byte{'X', 0xff}), Product: "P", Quantity: 3, Unit: "kg"},
	}}
	if err := Save(path, reg); err == nil {
		t.Fatal("Save must refuse to persist malformed UTF-8")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("failed Save must leave no registry, stat err=%v", err)
	}
}
