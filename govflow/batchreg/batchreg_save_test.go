package batchreg

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Zero batches is a legal state: nil and empty slices both serialize as
// "batches": [] and reload as empty registries.
func TestSaveEmptyWritesBracketsAndReloads(t *testing.T) {
	cases := map[string][]Batch{
		"nil batches":   nil,
		"empty batches": {},
		"made empty":    make([]Batch, 0, 4),
	}
	for name, batches := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "nested", "registry.json")
			reg := &Registry{Version: FormatVersion, Batches: batches}
			if err := Save(path, reg); err != nil {
				t.Fatalf("Save: %v", err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), `"batches": []`) {
				t.Fatalf("empty registry must serialize batches as [], got:\n%s", data)
			}
			if strings.Contains(string(data), "null") {
				t.Fatalf("empty registry must not contain null:\n%s", data)
			}
			loaded, existed, err := Load(path)
			if err != nil || !existed {
				t.Fatalf("reload: existed=%v err=%v", existed, err)
			}
			if len(loaded.Batches) != 0 || loaded.Version != FormatVersion {
				t.Fatalf("reloaded registry not empty: %+v", loaded)
			}
			// The caller's slice state is untouched.
			if (reg.Batches == nil) != (batches == nil) {
				t.Fatalf("Save changed nil-ness of the caller's Batches slice")
			}
		})
	}
}

// Directly assembled records must satisfy the public format rules before
// anything is written: empty text fields, malformed UTF-8 and out-of-range
// quantities reject the whole save, naming file, 1-based position and field.
func TestSaveRejectsInvalidRecords(t *testing.T) {
	valid := func() Batch { return Batch{Batch: "B-1", Product: "P-7", Quantity: 120, Unit: "kg"} }
	cases := []struct {
		name      string
		mutate    func(*Batch)
		pos       int
		wantField string
		wantBatch string
	}{
		{"empty batch", func(b *Batch) { b.Batch = "" }, 1, "batch", ""},
		{"empty product", func(b *Batch) { b.Product = "" }, 1, "product", "B-1"},
		{"empty unit", func(b *Batch) { b.Unit = "" }, 1, "unit", "B-1"},
		{"quantity zero", func(b *Batch) { b.Quantity = 0 }, 1, "quantity", "B-1"},
		{"quantity negative", func(b *Batch) { b.Quantity = -7 }, 1, "quantity", "B-1"},
		{"min int64 quantity", func(b *Batch) { b.Quantity = -1 << 63 }, 1, "quantity", "B-1"},
		{"bad utf8 batch", func(b *Batch) { b.Batch = "B-\xff" }, 1, "batch", ""},
		{"bad utf8 product names batch", func(b *Batch) { b.Product = "P\xff" }, 1, "product", "B-1"},
		{"bad utf8 unit names batch", func(b *Batch) { b.Unit = "kg\xff" }, 1, "unit", "B-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "registry.json")
			bad := valid()
			tc.mutate(&bad)
			reg := &Registry{Version: FormatVersion, Batches: []Batch{bad}}

			err := Save(path, reg)
			var se *SaveError
			if !errors.As(err, &se) {
				t.Fatalf("got %v, want SaveError", err)
			}
			if se.Path != path || se.Position != tc.pos || se.Field != tc.wantField {
				t.Fatalf("unexpected SaveError: %+v", se)
			}
			if !strings.Contains(err.Error(), path) {
				t.Fatalf("error must name the target file: %v", err)
			}
			if !strings.Contains(err.Error(), "record 1") {
				t.Fatalf("error must name record position: %v", err)
			}
			if !strings.Contains(err.Error(), `"`+tc.wantField+`"`) {
				t.Fatalf("error must name field %q: %v", tc.wantField, err)
			}
			if tc.wantBatch != "" {
				if !strings.Contains(err.Error(), `batch "`+tc.wantBatch+`"`) {
					t.Fatalf("error must cite batch %q: %v", tc.wantBatch, err)
				}
			}
			if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
				t.Fatalf("rejected save must not create the registry, stat err=%v", statErr)
			}
			if leftovers, _ := filepath.Glob(filepath.Join(dir, ".govflow-registry-*.tmp")); len(leftovers) != 0 {
				t.Fatalf("rejected save left temp files: %v", leftovers)
			}
		})
	}
}

// A bad record at the very end still rejects the whole save and leaves the
// existing target's bytes and mtime exactly as they were.
func TestSaveLateInvalidRecordPreservesExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")
	original := `{"version":1,"batches":[{"batch":"OLD","product":"P","quantity":5,"unit":"kg"}]}`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	pinned := time.Date(2004, time.May, 6, 7, 8, 9, 0, time.UTC)
	if err := os.Chtimes(path, pinned, pinned); err != nil {
		t.Fatal(err)
	}

	reg := &Registry{Version: FormatVersion, Batches: []Batch{
		{Batch: "A", Product: "P", Quantity: 1, Unit: "kg"},
		{Batch: "B", Product: "P", Quantity: 2, Unit: "kg"},
		{Batch: "C", Product: "P", Quantity: 0, Unit: "kg"},
	}}
	err := Save(path, reg)
	var se *SaveError
	if !errors.As(err, &se) || se.Position != 3 || se.Field != "quantity" || se.Batch != "C" {
		t.Fatalf("unexpected error: %v", err)
	}

	after, rerr := os.ReadFile(path)
	if rerr != nil || string(after) != original {
		t.Fatalf("existing registry changed: %q", after)
	}
	info, serr := os.Stat(path)
	if serr != nil || !info.ModTime().Equal(pinned) {
		t.Fatalf("existing registry mtime changed: %v", serr)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, ".govflow-registry-*.tmp")); len(leftovers) != 0 {
		t.Fatalf("rejected save left temp files: %v", leftovers)
	}
}

// Repeated batch ids reject the save even when the two records are completely
// identical; a repeat is never accepted as a duplicate confirmation. The
// error names the id and both 1-based positions.
func TestSaveRejectsDuplicateBatchIDs(t *testing.T) {
	cases := map[string][]Batch{
		"identical records": {
			{Batch: "B1", Product: "P", Quantity: 1, Unit: "kg"},
			{Batch: "B1", Product: "P", Quantity: 1, Unit: "kg"},
		},
		"different records": {
			{Batch: "B1", Product: "P", Quantity: 1, Unit: "kg"},
			{Batch: "B2", Product: "Q", Quantity: 2, Unit: "g"},
			{Batch: "B1", Product: "R", Quantity: 3, Unit: "m"},
		},
	}
	for name, batches := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "registry.json")
			err := Save(path, &Registry{Version: FormatVersion, Batches: batches})
			var se *SaveError
			if !errors.As(err, &se) {
				t.Fatalf("got %v, want SaveError", err)
			}
			wantSecond := 2
			if name == "different records" {
				wantSecond = 3
			}
			if se.Field != "batch-id" || se.First != 1 || se.Position != wantSecond || se.Batch != "B1" {
				t.Fatalf("unexpected SaveError: %+v", se)
			}
			msg := err.Error()
			for _, want := range []string{path, `"B1"`, "records 1 and"} {
				if !strings.Contains(msg, want) {
					t.Fatalf("error must contain %q: %v", want, msg)
				}
			}
			if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
				t.Fatal("rejected save must not create the registry")
			}
		})
	}
}

// Ids are compared as their original Go strings: trimming and case folding
// are not applied, so values differing only by whitespace or casing are
// different batches — while an exact repeat is rejected.
func TestSaveDuplicateComparisonIsExact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	reg := &Registry{Version: FormatVersion, Batches: []Batch{
		{Batch: "B1", Product: "P", Quantity: 1, Unit: "kg"},
		{Batch: "b1", Product: "P", Quantity: 1, Unit: "kg"},
		{Batch: " B1", Product: "P", Quantity: 1, Unit: "kg"},
		{Batch: "B1 ", Product: "P", Quantity: 1, Unit: "kg"},
		{Batch: "批次", Product: "P", Quantity: 1, Unit: "kg"},
	}}
	if err := Save(path, reg); err != nil {
		t.Fatalf("distinct verbatim ids must save: %v", err)
	}
	reg.Batches = append(reg.Batches, Batch{Batch: "b1", Product: "X", Quantity: 9, Unit: "m"})
	err := Save(path, reg)
	var se *SaveError
	if !errors.As(err, &se) || se.Field != "batch-id" || se.First != 2 || se.Position != 6 || se.Batch != "b1" {
		t.Fatalf("exact repeat must be rejected, got %v", err)
	}
}

// An empty or malformed batch id is never quoted back in the error (no
// U+FFFD-substituted identification).
func TestSaveErrorOmitsAmbiguousBatchID(t *testing.T) {
	cases := map[string]Batch{
		"empty batch":        {Batch: "", Product: "P", Quantity: 1, Unit: "kg"},
		"malformed batch":    {Batch: "B-\xff", Product: "P", Quantity: 1, Unit: "kg"},
		"empty dup-like":     {Batch: "", Product: "P", Quantity: 1, Unit: "kg"},
		"malformed dup-like": {Batch: "\xff", Product: "P", Quantity: 1, Unit: "kg"},
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "registry.json")
			err := Save(path, &Registry{Version: FormatVersion, Batches: []Batch{b}})
			var se *SaveError
			if !errors.As(err, &se) {
				t.Fatalf("got %v, want SaveError", err)
			}
			if se.Batch != "" {
				t.Fatalf("no batch id may be attached, got %q", se.Batch)
			}
			if strings.Contains(err.Error(), "batch \"") {
				t.Fatalf("error must not quote a batch id: %v", err)
			}
			if strings.Contains(err.Error(), "�") {
				t.Fatalf("error must not contain a substituted id: %v", err)
			}
		})
	}
}

// Save neither reorders nor rewrites caller memory, on success or failure.
func TestSaveDoesNotMutateCaller(t *testing.T) {
	batches := []Batch{
		{Batch: " B1 ", Product: " P 7 ", Quantity: 1, Unit: " kg "},
		{Batch: "B2", Product: "Q", Quantity: MaxQuantity, Unit: "box"},
	}
	reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), batches...)}
	path := filepath.Join(t.TempDir(), "registry.json")
	if err := Save(path, reg); err != nil {
		t.Fatal(err)
	}
	if len(reg.Batches) != len(batches) {
		t.Fatalf("Save changed record count: %+v", reg.Batches)
	}
	for i := range batches {
		if reg.Batches[i] != batches[i] {
			t.Fatalf("Save changed record %d: %+v", i+1, reg.Batches[i])
		}
	}

	// A failed validation must leave the caller's records exactly as given.
	bad := append([]Batch(nil), batches...)
	bad = append(bad, Batch{Batch: "B2", Product: "Q", Quantity: MaxQuantity, Unit: "box"})
	reg2 := &Registry{Version: FormatVersion, Batches: bad}
	if err := Save(filepath.Join(t.TempDir(), "other.json"), reg2); err == nil {
		t.Fatal("duplicate must fail")
	}
	if len(reg2.Batches) != 3 {
		t.Fatalf("failed Save changed record count: %+v", reg2.Batches)
	}
	for i := range bad {
		if reg2.Batches[i] != bad[i] {
			t.Fatalf("failed Save changed record %d: %+v", i+1, reg2.Batches[i])
		}
	}
}

// Legal records persist in submission order; every one of the four values
// reads back byte-for-byte, including edge whitespace, CJK, emoji and a
// genuinely entered U+FFFD.
func TestSaveRoundTripsValuesAndOrder(t *testing.T) {
	fffd := string(rune(0xfffd))
	want := []Batch{
		{Batch: "  ", Product: "\t", Quantity: 1, Unit: "\n"},
		{Batch: "批次-1", Product: "产品😀", Quantity: 7, Unit: "千克"},
		{Batch: "B-" + fffd, Product: "P" + fffd, Quantity: MaxQuantity, Unit: "x" + fffd},
		{Batch: "last", Product: "p", Quantity: 42, Unit: "u"},
	}
	path := filepath.Join(t.TempDir(), "registry.json")
	if err := Save(path, &Registry{Version: FormatVersion, Batches: want}); err != nil {
		t.Fatal(err)
	}
	loaded, existed, err := Load(path)
	if err != nil || !existed {
		t.Fatalf("Load: existed=%v err=%v", existed, err)
	}
	if len(loaded.Batches) != len(want) {
		t.Fatalf("got %d records, want %d", len(loaded.Batches), len(want))
	}
	for i, w := range want {
		if loaded.Batches[i] != w {
			t.Errorf("record %d = %+v, want %+v", i+1, loaded.Batches[i], w)
		}
	}
}

// An unsupported version is rejected before any directory or file is created.
func TestSaveRejectsUnsupportedVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "registry.json")
	err := Save(path, &Registry{Version: 2, Batches: nil})
	var se *SaveError
	if !errors.As(err, &se) || se.Field != "" || se.Position != 0 {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "version") {
		t.Fatalf("error must name file and explain version: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "nested")); !os.IsNotExist(statErr) {
		t.Fatal("rejected save must not create the parent directory")
	}
}
