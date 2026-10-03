package batchreg

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestNormalizeFieldUTF8(t *testing.T) {
	// Invalid bytes are rejected outright, never rewritten to U+FFFD, even
	// when adjacent to leading or trailing whitespace.
	invalid := []string{
		"B\xff1",
		"  B\xff",
		"B\xff  ",
		"\tB\xff1\n",
		"\xff",
		"ok\xff",
	}
	for _, in := range invalid {
		got, err := NormalizeField(in)
		if err == nil {
			t.Fatalf("NormalizeField(% x) expected error, got %q", in, got)
		}
		if !errors.Is(err, ErrInvalidUTF8) {
			t.Fatalf("NormalizeField(% x) error must wrap ErrInvalidUTF8, got %v", in, err)
		}
	}

	// Valid Unicode keeps working: Chinese, emoji, an explicitly entered
	// U+FFFD, and ASCII must all pass; the real replacement character alone
	// must never be treated as an encoding error.
	valid := map[string]string{
		"批次-1": "批次-1",
		"a😀b":  "a😀b",
		"a�b":  "a�b",
		"�":    "�",
		" B1 ": "B1",
	}
	for in, want := range valid {
		got, err := NormalizeField(in)
		if err != nil {
			t.Fatalf("NormalizeField(%q) unexpected error: %v", in, err)
		}
		if got != want {
			t.Fatalf("NormalizeField(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeField(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		want  string
		valid bool
	}{
		{"plain", "B-001", "B-001", true},
		{"trims ends", "\t B-001 \n", "B-001", true},
		{"keeps interior", "B 001", "B 001", true},
		{"keeps case", "b1", "b1", true},
		{"empty", "", "", false},
		{"only whitespace", " \t\n", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeField(tc.in)
			if tc.valid {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got != tc.want {
					t.Fatalf("got %q, want %q", got, tc.want)
				}
			} else if err == nil {
				t.Fatalf("expected error for %q, got %q", tc.in, got)
			}
		})
	}
}

func TestParseQuantity(t *testing.T) {
	const max = "9223372036854775807"
	valid := map[string]int64{
		"1":                   1,
		"0007":                7,
		"120":                 120,
		max:                   MaxQuantity,
		"0000000000000000001": 1,
	}
	for in, want := range valid {
		got, err := ParseQuantity(in)
		if err != nil {
			t.Fatalf("ParseQuantity(%q) unexpected error: %v", in, err)
		}
		if got != want {
			t.Fatalf("ParseQuantity(%q) = %d, want %d", in, got, want)
		}
	}
	invalid := []string{
		"0", "0000", "-1", "+1", "1.0", "1e3", " 1", "1 ",
		"abc", "１２", "9223372036854775808", "99999999999999999999999", "",
	}
	for _, in := range invalid {
		if _, err := ParseQuantity(in); err == nil {
			t.Fatalf("ParseQuantity(%q) expected error", in)
		}
	}
}

func writeRegistry(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRegisterCreatesAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "registry.json")

	reg, existed, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if existed {
		t.Fatal("registry should not exist yet")
	}
	out, err := Register(reg, Input{Batch: "B-1", Product: "P-7", Quantity: 120, Unit: "kg"})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Created || out.Batch.Quantity != 120 {
		t.Fatalf("unexpected outcome: %+v", out)
	}
	if err := Save(path, reg); err != nil {
		t.Fatal(err)
	}

	// Restart: old batch must still be recognized.
	reg2, existed, err := Load(path)
	if err != nil || !existed {
		t.Fatalf("reload: existed=%v err=%v", existed, err)
	}
	out, err = Register(reg2, Input{Batch: "B-1", Product: "P-7", Quantity: 120, Unit: "kg"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Created {
		t.Fatal("identical repeat should be a duplicate, not a create")
	}

	// A second batch keeps the first one intact.
	_, err = Register(reg2, Input{Batch: "B-2", Product: "P-8", Quantity: 1, Unit: "box"})
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(path, reg2); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var parsed Registry
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Batches) != 2 || parsed.Version != FormatVersion {
		t.Fatalf("unexpected persisted content: %s", data)
	}
}

func TestDuplicateDoesNotRewriteFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	writeRegistry(t, path, `{"version":1,"batches":[{"batch":"B-1","product":"P-7","quantity":120,"unit":"kg"}]}`)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	reg, _, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Register(reg, Input{Batch: "B-1", Product: "P-7", Quantity: 120, Unit: "kg"})
	if err != nil || out.Created {
		t.Fatalf("expected duplicate, got %+v err=%v", out, err)
	}
	// CLI contract: a duplicate must not save. Verify in-memory state has
	// still exactly one record.
	if len(reg.Batches) != 1 {
		t.Fatalf("duplicate changed in-memory registry: %+v", reg.Batches)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.ModTime() != before.ModTime() || after.Size() != before.Size() {
		t.Fatal("duplicate registration rewrote the registry file")
	}
}

func TestConflictNamesFieldsAndDoesNotMutate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	writeRegistry(t, path, `{"version":1,"batches":[{"batch":"B-1","product":"P-7","quantity":120,"unit":"kg"},{"batch":"B-9","product":"P-9","quantity":3,"unit":"box"}]}`)

	cases := []struct {
		name       string
		in         Input
		wantFields []string
	}{
		{"product", Input{Batch: "B-1", Product: "P-8", Quantity: 120, Unit: "kg"}, []string{"product"}},
		{"quantity", Input{Batch: "B-1", Product: "P-7", Quantity: 121, Unit: "kg"}, []string{"quantity"}},
		{"unit", Input{Batch: "B-1", Product: "P-7", Quantity: 120, Unit: "g"}, []string{"unit"}},
		{"all", Input{Batch: "B-1", Product: "X", Quantity: 1, Unit: "Y"}, []string{"product", "quantity", "unit"}},
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg, _, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			_, err = Register(reg, tc.in)
			var ce *ConflictError
			if !errors.As(err, &ce) {
				t.Fatalf("expected ConflictError, got %v", err)
			}
			if ce.Batch != "B-1" {
				t.Errorf("conflict batch = %q", ce.Batch)
			}
			if strings.Join(ce.Fields, ",") != strings.Join(tc.wantFields, ",") {
				t.Errorf("fields = %v, want %v", ce.Fields, tc.wantFields)
			}
			if len(reg.Batches) != 2 || reg.Batches[0].Quantity != 120 || reg.Batches[0].Product != "P-7" {
				t.Fatalf("conflict mutated in-memory records: %+v", reg.Batches)
			}
		})
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatal("conflict path must not modify the file")
	}
}

func TestCaseSensitiveAndInteriorWhitespace(t *testing.T) {
	reg := &Registry{Version: FormatVersion}
	if _, err := Register(reg, Input{Batch: "B-1", Product: "P-7", Quantity: 1, Unit: "kg"}); err != nil {
		t.Fatal(err)
	}
	// Different casing is a different batch.
	if _, err := Register(reg, Input{Batch: "b-1", Product: "P-7", Quantity: 1, Unit: "kg"}); err != nil {
		t.Fatalf("casing should distinguish batches: %v", err)
	}
	if len(reg.Batches) != 2 {
		t.Fatalf("got %d batches", len(reg.Batches))
	}
	// Interior whitespace is part of the product value.
	if _, err := Register(reg, Input{Batch: "B-1", Product: "P- 7", Quantity: 1, Unit: "kg"}); err == nil {
		t.Fatal("interior whitespace difference must conflict")
	}
}

func TestLoadRejectsUnusableFiles(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"empty":         "",
		"whitespace":    "   \n\t ",
		"not json":      "this is not json",
		"json array":    "[]",
		"wrong version": `{"version":2,"batches":[]}`,
		"unknown field": `{"version":1,"batches":[],"extra":1}`,
		"duplicate id":  `{"version":1,"batches":[{"batch":"B","product":"P","quantity":1,"unit":"kg"},{"batch":"B","product":"Q","quantity":2,"unit":"g"}]}`,
		"zero quantity": `{"version":1,"batches":[{"batch":"B","product":"P","quantity":0,"unit":"kg"}]}`,
		"empty unit":    `{"version":1,"batches":[{"batch":"B","product":"P","quantity":1,"unit":""}]}`,
		"trailing junk": `{"version":1,"batches":[]} garbage`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name+".json")
			writeRegistry(t, path, content)
			_, _, err := Load(path)
			if err == nil {
				t.Fatal("expected rejection")
			}
			if name == "duplicate id" {
				var dup *DuplicateIDError
				if !errors.As(err, &dup) || dup.Batch != "B" {
					t.Fatalf("expected DuplicateIDError for B, got %v", err)
				}
			}
		})
	}

	// Empty object has no version field and must be rejected rather than
	// treated as a fresh registry.
	t.Run("empty object", func(t *testing.T) {
		path := filepath.Join(dir, "obj.json")
		writeRegistry(t, path, `{}`)
		if _, _, err := Load(path); err == nil {
			t.Fatal("{} must not be accepted")
		}
	})
}

func TestLoadAcceptsEmptyBatchList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.json")
	writeRegistry(t, path, `{"version":1,"batches":[]}`)
	reg, existed, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !existed || len(reg.Batches) != 0 {
		t.Fatalf("existed=%v batches=%v", existed, reg.Batches)
	}
}

func TestLoadRejectsStructuralViolations(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		// Duplicate fields are rejected even when both values are identical,
		// and even when the repeat hides behind a JSON escape.
		"root duplicate version":   `{"version":1,"version":1,"batches":[]}`,
		"root duplicate batches":   `{"version":1,"batches":[{"batch":"B","product":"P","quantity":1,"unit":"kg"}],"batches":[]}`,
		"root duplicate escaped":   `{"version":1,"\u0076ersion":1,"batches":[]}`,
		"record duplicate field":   `{"version":1,"batches":[{"batch":"B","product":"P","quantity":1,"unit":"kg","unit":"kg"}]}`,
		"record duplicate escaped": `{"version":1,"batches":[{"batch":"B","produc\u0074":"P","product":"P","quantity":1,"unit":"kg"}]}`,
		// Missing or null members of the root object.
		"missing version": `{"batches":[]}`,
		"missing batches": `{"version":1}`,
		"null version":    `{"version":null,"batches":[]}`,
		"null batches":    `{"version":1,"batches":null}`,
		// Wrong types.
		"string version": `{"version":"1","batches":[]}`,
		"float version":  `{"version":1.0,"batches":[]}`,
		"batches object": `{"version":1,"batches":{}}`,
		"batches string": `{"version":1,"batches":"[]"}`,
		// Field names are matched case-sensitively.
		"capitalized root":   `{"Version":1,"batches":[]}`,
		"capitalized record": `{"version":1,"batches":[{"Batch":"B","product":"P","quantity":1,"unit":"kg"}]}`,
		// Record-level structural problems.
		"record not object":      `{"version":1,"batches":[null]}`,
		"record missing field":   `{"version":1,"batches":[{"batch":"B","product":"P","quantity":1}]}`,
		"record null field":      `{"version":1,"batches":[{"batch":"B","product":null,"quantity":1,"unit":"kg"}]}`,
		"record quantity string": `{"version":1,"batches":[{"batch":"B","product":"P","quantity":"1","unit":"kg"}]}`,
		"record quantity float":  `{"version":1,"batches":[{"batch":"B","product":"P","quantity":1.0,"unit":"kg"}]}`,
		"record quantity null":   `{"version":1,"batches":[{"batch":"B","product":"P","quantity":null,"unit":"kg"}]}`,
		"record quantity minus":  `{"version":1,"batches":[{"batch":"B","product":"P","quantity":-3,"unit":"kg"}]}`,
		"record unknown field":   `{"version":1,"batches":[{"batch":"B","product":"P","quantity":1,"unit":"kg","note":"x"}]}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name+".json")
			writeRegistry(t, path, content)
			if _, _, err := Load(path); err == nil {
				t.Fatalf("expected rejection of %s", content)
			}
		})
	}
}

func TestLoadFormatErrorDetails(t *testing.T) {
	dir := t.TempDir()

	// A record-level error names the 1-based position, the batch id when it
	// is unambiguous, the field and the reason.
	t.Run("record position and batch", func(t *testing.T) {
		path := filepath.Join(dir, "pos.json")
		writeRegistry(t, path, `{"version":1,"batches":[
			{"batch":"B1","product":"P","quantity":1,"unit":"kg"},
			{"batch":"B2","product":"P","quantity":1,"unit":"kg","extra":1}]}`)
		_, _, err := Load(path)
		var fe *FormatError
		if !errors.As(err, &fe) {
			t.Fatalf("expected FormatError, got %v", err)
		}
		if fe.Position != 2 || fe.Batch != "B2" || fe.Field != "extra" {
			t.Fatalf("unexpected FormatError: %+v", fe)
		}
	})

	// When the batch member itself is duplicated, no id is picked.
	t.Run("ambiguous batch id", func(t *testing.T) {
		path := filepath.Join(dir, "amb.json")
		writeRegistry(t, path, `{"version":1,"batches":[{"batch":"B1","batch":"B2","product":"P","quantity":1,"unit":"kg"}]}`)
		_, _, err := Load(path)
		var fe *FormatError
		if !errors.As(err, &fe) {
			t.Fatalf("expected FormatError, got %v", err)
		}
		if fe.Position != 1 || fe.Batch != "" || fe.Field != "batch" {
			t.Fatalf("unexpected FormatError: %+v", fe)
		}
	})

	// Root-level problems carry the field but no record position.
	t.Run("root field", func(t *testing.T) {
		path := filepath.Join(dir, "root.json")
		writeRegistry(t, path, `{"version":1,"batches":[],"batches":[]}`)
		_, _, err := Load(path)
		var fe *FormatError
		if !errors.As(err, &fe) {
			t.Fatalf("expected FormatError, got %v", err)
		}
		if fe.Position != 0 || fe.Field != "batches" {
			t.Fatalf("unexpected FormatError: %+v", fe)
		}
	})
}

func TestLoadAcceptsLegalFormatting(t *testing.T) {
	// Field order, indentation and JSON escapes are part of the format, not
	// of the data: this file is legal and its values load unchanged.
	path := filepath.Join(t.TempDir(), "r.json")
	writeRegistry(t, path, "{\n  \"batches\": [\n    {\"unit\": \"kg\", \"quantity\": 120, \"product\": \"P-7\", \"batch\": \"B-001\"},\n    {\"batch\": \"B\\u002d002\", \"product\": \"P 8\", \"quantity\": 1, \"unit\": \"box\"}\n  ],\n  \"version\": 1\n}")
	reg, existed, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !existed || len(reg.Batches) != 2 {
		t.Fatalf("existed=%v batches=%+v", existed, reg.Batches)
	}
	want := []Batch{
		{Batch: "B-001", Product: "P-7", Quantity: 120, Unit: "kg"},
		{Batch: "B-002", Product: "P 8", Quantity: 1, Unit: "box"},
	}
	for i, w := range want {
		if reg.Batches[i] != w {
			t.Errorf("record %d = %+v, want %+v", i+1, reg.Batches[i], w)
		}
	}
}

func TestSaveFailureLeavesExistingRegistryUsable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")
	writeRegistry(t, path, `{"version":1,"batches":[{"batch":"OLD","product":"P","quantity":5,"unit":"kg"}]}`)

	// A regular file standing where the temp file's parent directory should
	// be makes CreateTemp fail with ENOTDIR. The atomic save must surface the
	// error and leave every other file untouched.
	blocker := filepath.Join(dir, "blocked")
	writeRegistry(t, blocker, "x")
	badPath := filepath.Join(blocker, "registry.json")

	reg := &Registry{Version: FormatVersion, Batches: []Batch{{Batch: "B", Product: "P", Quantity: 1, Unit: "kg"}}}
	if err := Save(badPath, reg); err == nil {
		t.Fatal("saving through a non-directory path must fail")
	}

	reg2, existed, err := Load(path)
	if err != nil || !existed || len(reg2.Batches) != 1 || reg2.Batches[0].Batch != "OLD" {
		t.Fatalf("existing registry not preserved: %+v err=%v", reg2, err)
	}
}

func TestIndependentRegistries(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.json")
	b := filepath.Join(dir, "b.json")
	for _, p := range []string{a, b} {
		reg, _, err := Load(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Register(reg, Input{Batch: "B-1", Product: "P", Quantity: 1, Unit: "kg"}); err != nil {
			t.Fatal(err)
		}
		if err := Save(p, reg); err != nil {
			t.Fatal(err)
		}
	}
	ra, _, err := Load(a)
	if err != nil {
		t.Fatal(err)
	}
	rb, _, err := Load(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(ra.Batches) != 1 || len(rb.Batches) != 1 {
		t.Fatal("each registry must manage its own ids")
	}
}

func TestParseManifestValid(t *testing.T) {
	content := `[
  {"batch": " B1 ", "product": "P 7", "quantity": 120, "unit": "kg"},
  {"batch":"B2","product":"Q","quantity":9223372036854775807,"unit":"box"}
]`
	got, err := ParseManifest([]byte(content))
	if err != nil {
		t.Fatal(err)
	}
	want := []Input{
		{Batch: "B1", Product: "P 7", Quantity: 120, Unit: "kg"},
		{Batch: "B2", Product: "Q", Quantity: MaxQuantity, Unit: "box"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d records, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("record %d = %+v, want %+v", i+1, got[i], want[i])
		}
	}
}

func TestParseManifestRejects(t *testing.T) {
	cases := map[string]string{
		"empty file":        "",
		"whitespace":        "   \n\t",
		"object":            `{"batch":"B","product":"P","quantity":1,"unit":"kg"}`,
		"json null":         "null",
		"json string":       `"[]"`,
		"json number":       "42",
		"empty array":       "[]",
		"trailing data":     `[{"batch":"B","product":"P","quantity":1,"unit":"kg"}] {}`,
		"truncated":         `[{"batch":"B"`,
		"null element":      "[null]",
		"number element":    "[1]",
		"string element":    `["x"]`,
		"missing batch":     `[{"product":"P","quantity":1,"unit":"kg"}]`,
		"missing product":   `[{"batch":"B","quantity":1,"unit":"kg"}]`,
		"missing quantity":  `[{"batch":"B","product":"P","unit":"kg"}]`,
		"missing unit":      `[{"batch":"B","product":"P","quantity":1}]`,
		"unknown field":     `[{"batch":"B","product":"P","quantity":1,"unit":"kg","extra":1}]`,
		"duplicate field":   `[{"batch":"B","batch":"X","product":"P","quantity":1,"unit":"kg"}]`,
		"numeric batch":     `[{"batch":1,"product":"P","quantity":1,"unit":"kg"}]`,
		"null batch":        `[{"batch":null,"product":"P","quantity":1,"unit":"kg"}]`,
		"blank product":     `[{"batch":"B","product":"  ","quantity":1,"unit":"kg"}]`,
		"quantity string":   `[{"batch":"B","product":"P","quantity":"1","unit":"kg"}]`,
		"quantity float":    `[{"batch":"B","product":"P","quantity":1.5,"unit":"kg"}]`,
		"quantity exponent": `[{"batch":"B","product":"P","quantity":1e3,"unit":"kg"}]`,
		"quantity bool":     `[{"batch":"B","product":"P","quantity":true,"unit":"kg"}]`,
		"quantity null":     `[{"batch":"B","product":"P","quantity":null,"unit":"kg"}]`,
		"quantity negative": `[{"batch":"B","product":"P","quantity":-1,"unit":"kg"}]`,
		"quantity zero":     `[{"batch":"B","product":"P","quantity":0,"unit":"kg"}]`,
		"quantity overflow": `[{"batch":"B","product":"P","quantity":9223372036854775808,"unit":"kg"}]`,
		"quantity huge":     `[{"batch":"B","product":"P","quantity":99999999999999999999999,"unit":"kg"}]`,
		"bad record 2":      `[{"batch":"B1","product":"P","quantity":1,"unit":"kg"},{"batch":"B2","product":"P","quantity":"x","unit":"kg"}]`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseManifest([]byte(content))
			if err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestParseManifestRecordErrorPosition(t *testing.T) {
	content := `[{"batch":"B1","product":"P","quantity":1,"unit":"kg"},
                {"batch":"B2","product":"P","quantity":"bad","unit":"kg"}]`
	_, err := ParseManifest([]byte(content))
	var re *ManifestRecordError
	if !errors.As(err, &re) {
		t.Fatalf("expected ManifestRecordError, got %v", err)
	}
	if re.Position != 2 || re.Batch != "B2" {
		t.Fatalf("position=%d batch=%q, want 2/B2", re.Position, re.Batch)
	}
}

func TestImportCreatesAndConfirmsRepeats(t *testing.T) {
	reg := &Registry{Version: FormatVersion, Batches: []Batch{
		{Batch: "OLD", Product: "P-1", Quantity: 3, Unit: "kg"},
	}}
	inputs := []Input{
		{Batch: "NEW1", Product: "P-2", Quantity: 10, Unit: "box"},
		{Batch: "NEW1", Product: "P-2", Quantity: 10, Unit: "box"}, // same-manifest duplicate
		{Batch: "OLD", Product: "P-1", Quantity: 3, Unit: "kg"},    // stored duplicate
		{Batch: "NEW2", Product: "P-3", Quantity: 1, Unit: "m"},
	}
	out, err := Import(reg, inputs)
	if err != nil {
		t.Fatal(err)
	}
	wantCreated := []bool{true, false, false, true}
	for i, want := range wantCreated {
		if out[i].Created != want {
			t.Errorf("record %d created=%v, want %v", i+1, out[i].Created, want)
		}
		if out[i].Batch.Batch != inputs[i].Batch {
			t.Errorf("record %d batch=%q", i+1, out[i].Batch.Batch)
		}
	}
	// Stored record first, new batches appended in first-occurrence order.
	wantBatches := []string{"OLD", "NEW1", "NEW2"}
	if len(reg.Batches) != len(wantBatches) {
		t.Fatalf("got %d stored batches: %+v", len(reg.Batches), reg.Batches)
	}
	for i, want := range wantBatches {
		if reg.Batches[i].Batch != want {
			t.Errorf("stored batch %d = %q, want %q", i, reg.Batches[i].Batch, want)
		}
	}
}

func TestImportConflictWithRegistry(t *testing.T) {
	original := []Batch{{Batch: "B1", Product: "P-1", Quantity: 3, Unit: "kg"}}
	reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), original...)}
	_, err := Import(reg, []Input{
		{Batch: "NEW", Product: "P", Quantity: 1, Unit: "kg"},
		{Batch: "B1", Product: "P-1", Quantity: 4, Unit: "kg"},
	})
	var ce *ManifestConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("expected ManifestConflictError, got %v", err)
	}
	if ce.Position != 2 || ce.Batch != "B1" || ce.Source != "registry" {
		t.Fatalf("unexpected conflict: %+v", ce)
	}
	if len(ce.Fields) != 1 || ce.Fields[0] != "quantity" {
		t.Fatalf("fields = %v", ce.Fields)
	}
	if len(reg.Batches) != len(original) || reg.Batches[0] != original[0] {
		t.Fatalf("rejected import mutated the registry: %+v", reg.Batches)
	}
}

func TestImportConflictWithinManifestLeavesNothing(t *testing.T) {
	// The registry does not exist yet: two identical B1 records must add it
	// once on success, but a differing later record must leave the registry
	// with no new batches at all.
	reg := &Registry{Version: FormatVersion}
	_, err := Import(reg, []Input{
		{Batch: "B1", Product: "P", Quantity: 1, Unit: "kg"},
		{Batch: "B2", Product: "P", Quantity: 1, Unit: "kg"},
		{Batch: "B1", Product: "P", Quantity: 2, Unit: "kg"},
	})
	var ce *ManifestConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("expected ManifestConflictError, got %v", err)
	}
	if ce.Position != 3 || ce.Batch != "B1" || ce.Source != "manifest" || ce.PrevPos != 1 {
		t.Fatalf("unexpected conflict: %+v", ce)
	}
	if len(ce.Fields) != 1 || ce.Fields[0] != "quantity" {
		t.Fatalf("fields = %v", ce.Fields)
	}
	if len(reg.Batches) != 0 {
		t.Fatalf("rejected manifest left new batches: %+v", reg.Batches)
	}
}

func TestImportConflictNamesAllFields(t *testing.T) {
	reg := &Registry{Version: FormatVersion, Batches: []Batch{
		{Batch: "B1", Product: "P", Quantity: 1, Unit: "kg"},
	}}
	_, err := Import(reg, []Input{{Batch: "B1", Product: "Q", Quantity: 2, Unit: "g"}})
	var ce *ManifestConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("expected conflict, got %v", err)
	}
	if got := strings.Join(ce.Fields, ","); got != "product,quantity,unit" {
		t.Fatalf("fields = %q", got)
	}
}

func TestImportEmpty(t *testing.T) {
	if _, err := Import(&Registry{Version: FormatVersion}, nil); err == nil {
		t.Fatal("empty manifest must be rejected")
	}
}

// invalidUTF8Byte is one byte that can never start a valid UTF-8 sequence.
const invalidUTF8Byte = "\xff"

func TestJSONStringTokenValidation(t *testing.T) {
	valid := []string{
		`""`,
		`"abc"`,
		`"批次"`,
		`"😀"`,
		`"a�b"`, // a literal, genuine U+FFFD
		`"a�b"`, // the same character written as an escape
		`"x😀y"`, // a proper surrogate pair -> emoji
		`"line\nbreak"`,
		`"quote\"q"`,
		`"A"`,
	}
	for _, tok := range valid {
		if err := jsonStringTokenError([]byte(tok)); err != nil {
			t.Errorf("token %s unexpectedly rejected: %v", tok, err)
		}
	}
	invalidEncoding := []string{
		`"` + invalidUTF8Byte + `"`,
		`"a` + invalidUTF8Byte + `b"`,
		`"\ud800"`,   // lone high surrogate
		`"\udc00"`,   // lone low surrogate
		`"x\ud800y"`, // high surrogate not followed by a low
		`"x\ud800A"`, // high surrogate followed by a non-low escape
	}
	for _, tok := range invalidEncoding {
		err := jsonStringTokenError([]byte(tok))
		if !errors.Is(err, ErrInvalidUTF8) {
			t.Errorf("token %s expected ErrInvalidUTF8, got %v", tok, err)
		}
	}
	malformed := []string{
		`"abc`,     // unterminated
		`abc"`,     // no opening quote
		`"a"b"`,    // unescaped quote
		`"\q"`,     // bad escape
		`"\u12"`,   // truncated \u escape
		`"\ud800"`, // (also) covered above
	}
	for _, tok := range malformed {
		if err := jsonStringTokenError([]byte(tok)); err == nil {
			t.Errorf("malformed token %s expected an error", tok)
		}
	}
}

func TestParseManifestRejectsInvalidUTF8(t *testing.T) {
	cases := map[string]string{
		"invalid batch":   `[{"batch":"B` + invalidUTF8Byte + `1","product":"P","quantity":1,"unit":"kg"}]`,
		"invalid product": `[{"batch":"B1","product":"P` + invalidUTF8Byte + `","quantity":1,"unit":"kg"}]`,
		"invalid unit":    `[{"batch":"B1","product":"P","quantity":1,"unit":"k` + invalidUTF8Byte + `g"}]`,
		"lone surrogate":  `[{"batch":"x\ud800","product":"P","quantity":1,"unit":"kg"}]`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			inputs, err := ParseManifest([]byte(content))
			if err == nil {
				t.Fatalf("expected rejection, got %+v", inputs)
			}
			var re *ManifestRecordError
			if !errors.As(err, &re) {
				t.Fatalf("expected ManifestRecordError, got %v", err)
			}
			if re.Position != 1 {
				t.Errorf("position = %d, want 1", re.Position)
			}
			if !strings.Contains(err.Error(), "UTF-8") {
				t.Errorf("error must mention the encoding problem: %v", err)
			}
		})
	}
}

func TestParseManifestInvalidUTF8PositionAndBatch(t *testing.T) {
	// The first record is fine; the second has invalid bytes in "unit", so
	// the error must point at record 2 and name the batch only when that
	// field itself is valid.
	content := `[{"batch":"B1","product":"P","quantity":1,"unit":"kg"},` +
		`{"batch":"B2","product":"P","quantity":1,"unit":"k` + invalidUTF8Byte + `g"}]`
	_, err := ParseManifest([]byte(content))
	var re *ManifestRecordError
	if !errors.As(err, &re) {
		t.Fatalf("expected ManifestRecordError, got %v", err)
	}
	if re.Position != 2 || re.Batch != "B2" {
		t.Fatalf("position=%d batch=%q, want 2/B2", re.Position, re.Batch)
	}
	if !strings.Contains(re.Error(), `field "unit"`) {
		t.Fatalf("error must name field unit: %v", re)
	}

	// When the batch field itself carries the invalid bytes, the error must
	// not identify the batch using a substituted replacement character.
	content = `[{"batch":"B` + invalidUTF8Byte + `1","product":"P","quantity":1,"unit":"kg"}]`
	_, err = ParseManifest([]byte(content))
	if !errors.As(err, &re) {
		t.Fatalf("expected ManifestRecordError, got %v", err)
	}
	if re.Position != 1 || re.Batch != "" {
		t.Fatalf("position=%d batch=%q, want 1 with empty batch", re.Position, re.Batch)
	}
	if !strings.Contains(re.Error(), `field "batch"`) {
		t.Fatalf("error must name field batch: %v", re)
	}
	// A batch id must not be echoed at all (and so never as a substituted
	// replacement character); only the trailing explanation may mention U+FFFD.
	if strings.Contains(re.Error(), "(batch ") {
		t.Fatalf("error must not name a substituted batch id: %v", re)
	}
}

func TestParseManifestAcceptsValidUnicode(t *testing.T) {
	content := `[
  {"batch":"批次-1","product":"产品","quantity":3,"unit":"箱"},
  {"batch":"a😀b","product":"P","quantity":1,"unit":"kg"},
  {"batch":"r` + "�" + `","product":"P","quantity":1,"unit":"kg"},
  {"batch":"r�","product":"P","quantity":1,"unit":"kg"}
]`
	got, err := ParseManifest([]byte(content))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d records", len(got))
	}
	// A literal U+FFFD and the � escape are the same batch text, so the
	// last two records share an id and differ nowhere (duplicate confirmation
	// is handled by Import); here they just parse to equal strings.
	if got[2].Batch != got[3].Batch || got[2].Batch != "r�" {
		t.Fatalf("U+FFFD escape and literal must match: %q vs %q", got[2].Batch, got[3].Batch)
	}
	if got[1].Batch != "a😀b" {
		t.Fatalf("surrogate pair must decode to emoji, got %q", got[1].Batch)
	}
	if !utf8.ValidString(got[0].Batch) {
		t.Fatal("Chinese batch must be valid UTF-8")
	}
}

func TestLoadRejectsInvalidUTF8Registry(t *testing.T) {
	cases := map[string]string{
		"invalid batch":   `{"version":1,"batches":[{"batch":"B` + invalidUTF8Byte + `","product":"P","quantity":1,"unit":"kg"}]}`,
		"invalid product": `{"version":1,"batches":[{"batch":"B1","product":"P` + invalidUTF8Byte + `","quantity":1,"unit":"kg"}]}`,
		"invalid unit":    `{"version":1,"batches":[{"batch":"B1","product":"P","quantity":1,"unit":"k` + invalidUTF8Byte + `"}]}`,
		"invalid record 2": `{"version":1,"batches":[
			{"batch":"B1","product":"P","quantity":1,"unit":"kg"},
			{"batch":"B2","product":"P` + invalidUTF8Byte + `","quantity":1,"unit":"kg"}]}`,
		"lone surrogate": `{"version":1,"batches":[{"batch":"x\ud800","product":"P","quantity":1,"unit":"kg"}]}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "reg.json")
			writeRegistry(t, path, content)
			_, _, err := Load(path)
			if err == nil {
				t.Fatal("expected the registry to be rejected")
			}
			var fe *FormatError
			if !errors.As(err, &fe) {
				t.Fatalf("expected FormatError, got %v", err)
			}
			if strings.Contains(fe.Batch, "�") && fe.Field == "batch" {
				t.Fatalf("an invalid batch id must not be echoed back replaced: %+v", fe)
			}
			if !strings.Contains(err.Error(), "UTF-8") {
				t.Fatalf("error must mention the encoding problem: %v", err)
			}
		})
	}
}

func TestLoadInvalidUTF8PointsAtRecordAndField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reg.json")
	// Record 2 has an invalid "unit"; its valid batch id stays attached.
	writeRegistry(t, path, `{"version":1,"batches":[
		{"batch":"B1","product":"P","quantity":1,"unit":"kg"},
		{"batch":"B2","product":"P","quantity":1,"unit":"k`+invalidUTF8Byte+`g"}]}`)
	_, _, err := Load(path)
	var fe *FormatError
	if !errors.As(err, &fe) {
		t.Fatalf("expected FormatError, got %v", err)
	}
	if fe.Position != 2 || fe.Batch != "B2" || fe.Field != "unit" {
		t.Fatalf("unexpected FormatError: %+v", fe)
	}

	// An invalid batch id in record 1 yields no batch and names the field.
	path2 := filepath.Join(t.TempDir(), "reg2.json")
	writeRegistry(t, path2, `{"version":1,"batches":[{"batch":"B`+invalidUTF8Byte+`1","product":"P","quantity":1,"unit":"kg"}]}`)
	_, _, err = Load(path2)
	if !errors.As(err, &fe) {
		t.Fatalf("expected FormatError, got %v", err)
	}
	if fe.Position != 1 || fe.Batch != "" || fe.Field != "batch" {
		t.Fatalf("unexpected FormatError: %+v", fe)
	}
}

func TestLoadAcceptsRegistryWithValidUnicode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reg.json")
	writeRegistry(t, path, `{"version":1,"batches":[
		{"batch":"批次-1","product":"产品","quantity":3,"unit":"箱"},
		{"batch":"a�b","product":"P","quantity":1,"unit":"kg"}
	]}`)
	reg, existed, err := Load(path)
	if err != nil || !existed || len(reg.Batches) != 2 {
		t.Fatalf("existed=%v batches=%+v err=%v", existed, reg, err)
	}
	if reg.Batches[0].Batch != "批次-1" || reg.Batches[0].Unit != "箱" {
		t.Fatalf("Chinese values must round-trip: %+v", reg.Batches[0])
	}
	if reg.Batches[1].Batch != "a�b" {
		t.Fatalf("genuine U+FFFD must round-trip: %q", reg.Batches[1].Batch)
	}
}

func TestRegisterAndImportRejectInvalidUTF8Directly(t *testing.T) {
	// Inputs bypassing ParseManifest/NormalizeField must still be refused so
	// they can never reach Save as replacement characters.
	reg := &Registry{Version: FormatVersion, Batches: []Batch{
		{Batch: "OLD", Product: "P", Quantity: 1, Unit: "kg"},
	}}
	original := append([]Batch(nil), reg.Batches...)

	bad := Input{Batch: "B\xff", Product: "P", Quantity: 1, Unit: "kg"}
	if _, err := Register(reg, bad); !errors.Is(err, ErrInvalidUTF8) {
		t.Fatalf("Register error = %v, want ErrInvalidUTF8", err)
	}
	bad = Input{Batch: "NEW", Product: "P\xff", Quantity: 1, Unit: "kg"}
	if _, err := Register(reg, bad); !errors.Is(err, ErrInvalidUTF8) {
		t.Fatalf("Register product error = %v, want ErrInvalidUTF8", err)
	}
	if len(reg.Batches) != len(original) || reg.Batches[0] != original[0] {
		t.Fatalf("Register mutated the registry: %+v", reg.Batches)
	}

	_, err := Import(reg, []Input{
		{Batch: "NEW", Product: "P", Quantity: 1, Unit: "kg"},
		{Batch: "B2", Product: "P", Quantity: 1, Unit: "k\xffg"},
	})
	var mre *ManifestRecordError
	if !errors.As(err, &mre) {
		t.Fatalf("Import error = %v, want ManifestRecordError", err)
	}
	if mre.Position != 2 || mre.Batch != "B2" {
		t.Fatalf("position=%d batch=%q, want 2/B2", mre.Position, mre.Batch)
	}
	if len(reg.Batches) != len(original) {
		t.Fatalf("Import must not add records on an encoding error: %+v", reg.Batches)
	}

	// Save is the final backstop: a hand-built invalid record is not written.
	reg.Batches = append(reg.Batches, Batch{Batch: "X\xff", Product: "P", Quantity: 1, Unit: "kg"})
	path := filepath.Join(t.TempDir(), "reg.json")
	if err := Save(path, reg); err == nil {
		t.Fatal("Save must reject a record with invalid UTF-8")
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatal("a failed Save must leave no registry file")
	}
}

func TestDistinctInvalidInputsDoNotCollide(t *testing.T) {
	// Two values that both used to be normalized to "B�1" must each be
	// rejected, rather than accepted and merged into one batch.
	reg := &Registry{Version: FormatVersion}
	for _, in := range []Input{
		{Batch: "B\xff1", Product: "P", Quantity: 1, Unit: "kg"},
		{Batch: "B\xfe1", Product: "P", Quantity: 1, Unit: "kg"},
	} {
		if _, err := Register(reg, in); !errors.Is(err, ErrInvalidUTF8) {
			t.Fatalf("expected rejection of % x, got %v (batches=%+v)", in.Batch, err, reg.Batches)
		}
	}
	if len(reg.Batches) != 0 {
		t.Fatalf("no invalid batch may be stored, got %+v", reg.Batches)
	}
}
