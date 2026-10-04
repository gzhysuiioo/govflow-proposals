package batchreg

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// assertNoTempFiles fails when dir holds a leftover save-temp file.
func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".govflow-registry-") {
			t.Fatalf("temporary file left behind: %s", e.Name())
		}
	}
}

// pinFile rewrites path with content and fixes its modification time, so a
// later save attempt can be checked for leaving both bytes and mtime alone.
func pinFile(t *testing.T, path, content string) (mtime time.Time) {
	t.Helper()
	writeRegistry(t, path, content)
	mtime = time.Unix(1234567890, 0)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return mtime
}

// assertFilePinned verifies that path still holds content with the mtime set
// by pinFile.
func assertFilePinned(t *testing.T, path, content string, mtime time.Time) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != content {
		t.Fatalf("rejected save changed the file bytes: %s", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(mtime) {
		t.Fatalf("rejected save changed the modification time: %v -> %v", mtime, info.ModTime())
	}
}

// A registry with no batches at all is legal: whether the caller never
// initialized the slice or emptied it, the file must hold an empty JSON
// array — never null — and read back as an empty registry. Save must not
// patch the caller's slice to get there.
func TestSaveEmptyRegistryWritesEmptyArray(t *testing.T) {
	cases := []struct {
		name    string
		batches []Batch
		wantNil bool
	}{
		{"uninitialized batches", nil, true},
		{"already empty batches", []Batch{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "registry.json")
			reg := &Registry{Version: FormatVersion, Batches: tc.batches}

			if err := Save(path, reg); err != nil {
				t.Fatal(err)
			}

			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var probe struct {
				Batches json.RawMessage `json:"batches"`
			}
			if err := json.Unmarshal(data, &probe); err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(string(probe.Batches)); got != "[]" {
				t.Fatalf("batches must be written as an empty array, got %s", got)
			}

			loaded, existed, err := Load(path)
			if err != nil || !existed {
				t.Fatalf("saved empty registry must read back: existed=%v err=%v", existed, err)
			}
			if len(loaded.Batches) != 0 {
				t.Fatalf("expected no batches, got %+v", loaded.Batches)
			}

			if (reg.Batches == nil) != tc.wantNil {
				t.Fatalf("Save mutated the caller's batch slice: %+v", reg.Batches)
			}
			assertNoTempFiles(t, dir)
		})
	}
}

// A successful save persists records exactly as the caller supplied them:
// order and all four fields survive the round trip through Load, including
// quantity bounds, non-ASCII text, a genuine U+FFFD the user really typed,
// and every kind of whitespace — Save never trims or folds text.
func TestSaveRoundTripsRecordsVerbatim(t *testing.T) {
	fffd := string(rune(0xfffd))
	records := []Batch{
		{Batch: "B-001", Product: "P-7", Quantity: 1, Unit: "kg"},
		{Batch: "B-002", Product: "P-8", Quantity: MaxQuantity, Unit: "box"},
		{Batch: "批次-🚚", Product: "产品 甲", Quantity: 42, Unit: "箱"},
		{Batch: "B" + fffd, Product: fffd + "P", Quantity: 3, Unit: fffd},
		{Batch: " B-003 ", Product: " P 7 ", Quantity: 7, Unit: " kg "},
		{Batch: " ", Product: "\t", Quantity: 5, Unit: "\n"},
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")
	reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), records...)}

	if err := Save(path, reg); err != nil {
		t.Fatal(err)
	}

	loaded, existed, err := Load(path)
	if err != nil || !existed {
		t.Fatalf("saved registry must read back: existed=%v err=%v", existed, err)
	}
	if len(loaded.Batches) != len(records) {
		t.Fatalf("got %d records, want %d", len(loaded.Batches), len(records))
	}
	for i, want := range records {
		if loaded.Batches[i] != want {
			t.Errorf("record %d = %+v, want %+v", i+1, loaded.Batches[i], want)
		}
	}

	// The caller's registry is untouched by the save.
	for i, want := range records {
		if reg.Batches[i] != want {
			t.Fatalf("Save mutated caller record %d: %+v", i+1, reg.Batches[i])
		}
	}
	assertNoTempFiles(t, dir)
}

// Batch ids are compared byte for byte: ids that differ only in casing or in
// leading/trailing whitespace are different batches and save side by side.
func TestSaveDistinguishesSimilarBatchIDs(t *testing.T) {
	records := []Batch{
		{Batch: "B1", Product: "P", Quantity: 1, Unit: "kg"},
		{Batch: "b1", Product: "P", Quantity: 2, Unit: "kg"},
		{Batch: " B1", Product: "P", Quantity: 3, Unit: "kg"},
		{Batch: "B1 ", Product: "P", Quantity: 4, Unit: "kg"},
	}
	path := filepath.Join(t.TempDir(), "registry.json")
	reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), records...)}
	if err := Save(path, reg); err != nil {
		t.Fatalf("similar but distinct ids must save: %v", err)
	}
	loaded, _, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Batches) != len(records) {
		t.Fatalf("got %d records, want %d", len(loaded.Batches), len(records))
	}
	for i, want := range records {
		if loaded.Batches[i] != want {
			t.Errorf("record %d = %+v, want %+v", i+1, loaded.Batches[i], want)
		}
	}
}

// A hand-assembled registry whose records violate the version-1 value rules —
// an empty text field or a quantity outside 1..MaxQuantity — must be rejected
// as a whole: the error names the target file, the 1-based record position,
// the field and the reason; the batch id is cited only when it is itself
// non-empty, valid UTF-8 text, never substituted. No record lands on disk:
// an existing target keeps its bytes and modification time, a missing target
// stays missing, no temporary file remains, and the caller's registry is
// left exactly as it was.
func TestSaveRejectsInvalidRecords(t *testing.T) {
	good := Batch{Batch: "B1", Product: "P-7", Quantity: 10, Unit: "kg"}
	cases := []struct {
		name      string
		bad       Batch
		wantField string
		wantBatch string // id the error must cite; "" means none
	}{
		{"empty batch", Batch{Batch: "", Product: "P", Quantity: 1, Unit: "kg"}, "batch", ""},
		{"empty product", Batch{Batch: "B2", Product: "", Quantity: 1, Unit: "kg"}, "product", "B2"},
		{"empty unit", Batch{Batch: "B2", Product: "P", Quantity: 1, Unit: ""}, "unit", "B2"},
		{"zero quantity", Batch{Batch: "B2", Product: "P", Quantity: 0, Unit: "kg"}, "quantity", "B2"},
		{"negative quantity", Batch{Batch: "B2", Product: "P", Quantity: -9, Unit: "kg"}, "quantity", "B2"},
		{"invalid utf8 batch not cited", Batch{Batch: "B-\xff", Product: "P", Quantity: 1, Unit: "kg"}, "batch", ""},
		{"invalid utf8 product cites batch", Batch{Batch: "B2", Product: "P\xff", Quantity: 1, Unit: "kg"}, "product", "B2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The offending record sits behind a valid one at position 2, so
			// the position in the error proves the first record was checked
			// and passed, and nothing of it may be left behind either.
			reg := &Registry{Version: FormatVersion, Batches: []Batch{good, tc.bad}}

			t.Run("existing target preserved", func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "registry.json")
				const original = `{"version":1,"batches":[{"batch":"OLD","product":"P","quantity":5,"unit":"kg"}]}`
				mtime := pinFile(t, path, original)

				err := Save(path, reg)
				assertInvalidRecordError(t, err, path, 2, tc.wantField, tc.wantBatch)
				assertFilePinned(t, path, original, mtime)
				assertNoTempFiles(t, dir)
			})

			t.Run("missing target stays missing", func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "registry.json")

				err := Save(path, reg)
				assertInvalidRecordError(t, err, path, 2, tc.wantField, tc.wantBatch)
				if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
					t.Fatalf("rejected save must not create the target, stat err=%v", statErr)
				}
				assertNoTempFiles(t, dir)
			})

			if len(reg.Batches) != 2 || reg.Batches[0] != good {
				t.Fatalf("rejected save mutated the caller's registry: %+v", reg.Batches)
			}
		})
	}
}

// assertInvalidRecordError checks the common shape of a Save rejection caused
// by one bad record: a *FormatError (wrapped with the target path) carrying
// the 1-based position, the field, a non-empty reason, and the batch id only
// when wantBatch is non-empty.
func assertInvalidRecordError(t *testing.T, err error, path string, pos int, field, batch string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected the save to be rejected")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error must name the target file %q: %v", path, err)
	}
	var fe *FormatError
	if !errors.As(err, &fe) {
		t.Fatalf("expected FormatError, got %v", err)
	}
	if fe.Position != pos {
		t.Errorf("position=%d, want %d", fe.Position, pos)
	}
	if fe.Field != field {
		t.Errorf("field=%q, want %q", fe.Field, field)
	}
	if fe.Reason == "" {
		t.Error("reason must not be empty")
	}
	if fe.Batch != batch {
		t.Errorf("cited batch=%q, want %q", fe.Batch, batch)
	}
	if batch == "" && strings.Contains(err.Error(), "(batch") {
		t.Errorf("error must not cite a batch id: %v", err)
	}
	if batch != "" && !strings.Contains(err.Error(), `(batch "`+batch+`")`) {
		t.Errorf("error must cite batch %q verbatim: %v", batch, err)
	}
}

// Two records sharing one batch id reject the whole save — even when product,
// quantity and unit are identical. Save persists a complete registry; the
// duplicate-confirmation rule of Register and Import does not apply here.
// The error names the id and both 1-based positions.
func TestSaveRejectsDuplicateBatchIDs(t *testing.T) {
	cases := []struct {
		name    string
		records []Batch
		first   int
		second  int
	}{
		{
			name: "identical records",
			records: []Batch{
				{Batch: "B1", Product: "P", Quantity: 1, Unit: "kg"},
				{Batch: "B1", Product: "P", Quantity: 1, Unit: "kg"},
			},
			first: 1, second: 2,
		},
		{
			name: "same id different fields",
			records: []Batch{
				{Batch: "B1", Product: "P", Quantity: 1, Unit: "kg"},
				{Batch: "B2", Product: "P", Quantity: 2, Unit: "kg"},
				{Batch: "B1", Product: "Q", Quantity: 9, Unit: "g"},
			},
			first: 1, second: 3,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "registry.json")
			const original = `{"version":1,"batches":[{"batch":"OLD","product":"P","quantity":5,"unit":"kg"}]}`
			mtime := pinFile(t, path, original)

			reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), tc.records...)}
			err := Save(path, reg)
			if err == nil {
				t.Fatal("duplicate batch ids must reject the save")
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("error must name the target file %q: %v", path, err)
			}
			var dup *DuplicateIDError
			if !errors.As(err, &dup) {
				t.Fatalf("expected DuplicateIDError, got %v", err)
			}
			if dup.Batch != "B1" || dup.First != tc.first || dup.Second != tc.second {
				t.Fatalf("unexpected duplicate report: %+v", dup)
			}
			if !strings.Contains(err.Error(), `"B1"`) {
				t.Errorf("error must name the duplicated id: %v", err)
			}

			assertFilePinned(t, path, original, mtime)
			assertNoTempFiles(t, dir)
			for i, want := range tc.records {
				if reg.Batches[i] != want {
					t.Fatalf("rejected save mutated caller record %d: %+v", i+1, reg.Batches[i])
				}
			}
		})
	}
}
