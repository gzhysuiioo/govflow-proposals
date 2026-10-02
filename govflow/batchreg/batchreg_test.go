package batchreg

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
