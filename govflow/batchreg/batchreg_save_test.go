package batchreg

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// This file is the regression net for Save when the caller supplies the whole
// registry table directly, rather than building it through Register or
// Import. The rule under guard: a save that succeeds must read back through
// Load exactly as supplied, while a rejected save behaves as if it never ran —
// the target file's bytes and modification time stay untouched (or the file is
// never created), no temporary file remains, and the caller's records and
// their order are unchanged.
func saveBatch(batch, product string, quantity int64, unit string) Batch {
	return Batch{Batch: batch, Product: product, Quantity: quantity, Unit: unit}
}

func assertBatchesEqual(t *testing.T, got, want []Batch) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d records, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("record %d = %+v, want %+v", i+1, got[i], want[i])
		}
	}
}

// assertNoLeftoverTempFiles walks root and fails if any save temp file
// (".govflow-registry-*.tmp") survived a rejected save.
func assertNoLeftoverTempFiles(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".govflow-registry-") {
			t.Errorf("rejected save left a temporary file behind: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// With no batches, "batches" is written as an empty array, never null, whether
// the caller left the slice uninitialized or emptied it, and the file reads
// back as a usable empty version-1 registry.
func TestSaveEmptyRegistryWritesEmptyArrayAndReadsBack(t *testing.T) {
	cases := []struct {
		name    string
		batches []Batch
		wantNil bool
	}{
		{"uninitialized slice", nil, true},
		{"already empty slice", []Batch{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "registry.json")
			reg := &Registry{Version: FormatVersion, Batches: tc.batches}
			if err := Save(path, reg); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(data, []byte(`"batches": []`)) {
				t.Fatalf("empty registry must serialize batches as [], got %s", data)
			}
			if bytes.Contains(data, []byte("null")) {
				t.Fatalf("empty registry must not contain null: %s", data)
			}
			got, existed, err := Load(path)
			if err != nil || !existed {
				t.Fatalf("empty registry must read back: existed=%v err=%v", existed, err)
			}
			if got.Version != FormatVersion || len(got.Batches) != 0 {
				t.Fatalf("read-back registry = %+v", got)
			}
			// Encoding takes a shallow copy, so the caller's slice header
			// (nil included) must be left exactly as it was passed in.
			if (reg.Batches == nil) != tc.wantNil {
				t.Fatalf("Save mutated the Batches header: nil=%v, want nil=%v", reg.Batches == nil, tc.wantNil)
			}
		})
	}
}

// On success the records read back in submission order with all four fields
// intact: quantity bounds, legal Chinese, a genuine U+FFFD the caller typed,
// and whitespace of every kind kept verbatim.
func TestSaveRoundTripPreservesOrderAndFields(t *testing.T) {
	fffd := string(rune(0xFFFD))
	want := []Batch{
		// Leading/trailing whitespace and legal Chinese stay verbatim;
		// quantity at the inclusive lower bound.
		saveBatch(" B-1 ", " 产品 A ", 1, " 千克 "),
		// Interior whitespace is part of each text value.
		saveBatch("B 2", "P  7", 7, "b o x"),
		// Whitespace-only non-empty text; quantity at the inclusive upper bound.
		saveBatch(" ", "\t", MaxQuantity, "\n"),
		// A replacement character the caller genuinely entered is ordinary text.
		saveBatch("批次-"+fffd, "P"+fffd+"Q", 42, fffd),
	}
	path := filepath.Join(t.TempDir(), "registry.json")
	reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), want...)}
	if err := Save(path, reg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The genuine U+FFFD is written as its real UTF-8 bytes, never stripped
	// or re-encoded as an escape.
	if !bytes.Contains(data, []byte{0xEF, 0xBF, 0xBD}) {
		t.Fatalf("saved file lost the genuine U+FFFD: %s", data)
	}

	got, existed, err := Load(path)
	if err != nil || !existed {
		t.Fatalf("saved registry must read back: existed=%v err=%v", existed, err)
	}
	if got.Version != FormatVersion {
		t.Fatalf("read-back version = %d, want %d", got.Version, FormatVersion)
	}
	assertBatchesEqual(t, got.Batches, want)
	// The caller's own records survive the save unchanged and in order.
	assertBatchesEqual(t, reg.Batches, want)
}

// Every invalid record rejects the whole save, and the error names the target
// file, the 1-based record position, the field and the reason. The batch id is
// cited verbatim only when the batch field itself is non-empty valid UTF-8; an
// empty or malformed id is never cited, and never replaced with U+FFFD to
// identify the record.
func TestSaveRejectsInvalidRecords(t *testing.T) {
	fffd := string(rune(0xFFFD))
	good := func() []Batch {
		return []Batch{
			saveBatch("B1", "P1", 1, "kg"),
			saveBatch("B2", "P2", 2, "box"),
			saveBatch("B3", "P3", 3, "m"),
		}
	}
	cases := []struct {
		name   string
		mutate func([]Batch)
		pos    int
		field  string
		batch  string // "" when the error must carry no batch id
		reason string
	}{
		{"empty batch on first record", func(b []Batch) { b[0].Batch = "" }, 1, "batch", "", "must not be empty"},
		{"empty product", func(b []Batch) { b[0].Product = "" }, 1, "product", "B1", "must not be empty"},
		{"empty unit on second record", func(b []Batch) { b[1].Unit = "" }, 2, "unit", "B2", "must not be empty"},
		{"zero quantity on second record", func(b []Batch) { b[1].Quantity = 0 }, 2, "quantity", "B2", "between 1 and"},
		{"negative quantity on third record", func(b []Batch) { b[2].Quantity = -5 }, 3, "quantity", "B3", "between 1 and"},
		{"malformed utf8 batch", func(b []Batch) { b[0].Batch = string([]byte{'B', 0xFF}) }, 1, "batch", "", "valid UTF-8"},
		{"malformed utf8 product names valid batch", func(b []Batch) { b[2].Product = string([]byte{'P', 0xFF}) }, 3, "product", "B3", "valid UTF-8"},
		{"malformed utf8 unit", func(b []Batch) { b[1].Unit = string([]byte{'k', 'g', 0xFF}) }, 2, "unit", "B2", "valid UTF-8"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "registry.json")
			batches := good()
			tc.mutate(batches)
			submitted := append([]Batch(nil), batches...)
			reg := &Registry{Version: FormatVersion, Batches: batches}

			err := Save(path, reg)
			if err == nil {
				t.Fatal("Save must reject the invalid table")
			}
			var fe *FormatError
			if !errors.As(err, &fe) {
				t.Fatalf("want *FormatError, got %v", err)
			}
			if fe.Position != tc.pos || fe.Field != tc.field || fe.Batch != tc.batch {
				t.Fatalf("FormatError = position %d field %q batch %q, want position %d field %q batch %q",
					fe.Position, fe.Field, fe.Batch, tc.pos, tc.field, tc.batch)
			}
			msg := err.Error()
			for _, want := range []string{
				path,
				fmt.Sprintf("batches record %d", tc.pos),
				strconv.Quote(tc.field),
				tc.reason,
			} {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q must mention %q", msg, want)
				}
			}
			if tc.batch == "" {
				if strings.Contains(msg, "(batch \"") {
					t.Errorf("error must not invent a batch id: %q", msg)
				}
			} else if !strings.Contains(msg, "(batch "+strconv.Quote(tc.batch)+")") {
				t.Errorf("error must cite batch %q verbatim: %q", tc.batch, msg)
			}
			if strings.Contains(msg, fffd) {
				t.Errorf("error must not identify a record via a substituted U+FFFD: %q", msg)
			}

			// Rejection happens before any file system change.
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("rejected save must not create the target file, stat err=%v", err)
			}
			assertNoLeftoverTempFiles(t, dir)
			assertBatchesEqual(t, reg.Batches, submitted)
		})
	}
}

// When earlier records are legal and a later one invalidates the table, no
// part of the earlier records may land: validation finishes (and fails)
// before any directory is created or any byte is written.
func TestSaveRejectsLateInvalidRecordWithoutPartialWrite(t *testing.T) {
	cases := []struct {
		name    string
		batches []Batch
		pos     int
		field   string
		batch   string
		reason  string
	}{
		{
			"empty unit on record 3",
			[]Batch{
				saveBatch("B1", "P1", 1, "kg"),
				saveBatch("B2", "P2", 2, "box"),
				saveBatch("B3", "P3", 3, ""),
			},
			3, "unit", "B3", "must not be empty",
		},
		{
			"zero quantity on record 2",
			[]Batch{
				saveBatch("B1", "P1", 10, "kg"),
				saveBatch("B2", "P2", 0, "box"),
			},
			2, "quantity", "B2", "between 1 and",
		},
		{
			"malformed product on record 2",
			[]Batch{
				saveBatch("B1", "P1", 1, "kg"),
				saveBatch("B2", string([]byte{'P', 0xFF}), 2, "box"),
			},
			2, "product", "B2", "valid UTF-8",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// The parent directories do not exist either; a rejected save
			// must not get far enough to create them.
			path := filepath.Join(dir, "nested", "deep", "registry.json")
			submitted := append([]Batch(nil), tc.batches...)
			reg := &Registry{Version: FormatVersion, Batches: tc.batches}

			err := Save(path, reg)
			if err == nil {
				t.Fatal("Save must reject the table")
			}
			var fe *FormatError
			if !errors.As(err, &fe) {
				t.Fatalf("want *FormatError, got %v", err)
			}
			if fe.Position != tc.pos || fe.Field != tc.field || fe.Batch != tc.batch {
				t.Fatalf("FormatError = position %d field %q batch %q, want %d %q %q",
					fe.Position, fe.Field, fe.Batch, tc.pos, tc.field, tc.batch)
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("error %q must mention %q", err.Error(), tc.reason)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("rejected save must not create the target file, stat err=%v", err)
			}
			if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
				t.Fatalf("rejected save must not create directories, stat err=%v", err)
			}
			assertNoLeftoverTempFiles(t, dir)
			assertBatchesEqual(t, reg.Batches, submitted)
		})
	}
}

// Two records sharing one byte-for-byte identical batch id reject the whole
// save even when product, quantity and unit are identical too. Save persists
// a complete table; it never applies Register/Import's duplicate-confirmation
// rule. The error names the id and both 1-based positions.
func TestSaveRejectsDuplicateBatchIDs(t *testing.T) {
	cases := []struct {
		name    string
		batches []Batch
		first   int
		second  int
		batch   string
	}{
		{
			"identical records",
			[]Batch{
				saveBatch("DUP", "P", 5, "kg"),
				saveBatch("DUP", "P", 5, "kg"),
			},
			1, 2, "DUP",
		},
		{
			"first and third records",
			[]Batch{
				saveBatch("DUP", "P", 5, "kg"),
				saveBatch("OTHER", "Q", 6, "g"),
				saveBatch("DUP", "P", 5, "kg"),
			},
			1, 3, "DUP",
		},
		{
			"identical whitespace-bearing id",
			[]Batch{
				saveBatch(" DUP ", "P", 5, "kg"),
				saveBatch(" DUP ", "P", 5, "kg"),
			},
			1, 2, " DUP ",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "registry.json")
			submitted := append([]Batch(nil), tc.batches...)
			reg := &Registry{Version: FormatVersion, Batches: tc.batches}

			err := Save(path, reg)
			if err == nil {
				t.Fatal("Save must reject duplicate batch ids")
			}
			var dup *DuplicateIDError
			if !errors.As(err, &dup) {
				t.Fatalf("want *DuplicateIDError, got %v", err)
			}
			if dup.Batch != tc.batch || dup.First != tc.first || dup.Second != tc.second {
				t.Fatalf("DuplicateIDError = batch %q records %d,%d, want %q records %d,%d",
					dup.Batch, dup.First, dup.Second, tc.batch, tc.first, tc.second)
			}
			msg := err.Error()
			for _, want := range []string{
				path,
				strconv.Quote(tc.batch),
				fmt.Sprintf("records %d and %d", tc.first, tc.second),
			} {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q must mention %q", msg, want)
				}
			}

			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("rejected save must not create the target file, stat err=%v", err)
			}
			assertNoLeftoverTempFiles(t, dir)
			assertBatchesEqual(t, reg.Batches, submitted)
		})
	}
}

// Ids are compared by their original text: casing and leading or trailing
// whitespace distinguish batches, and tables using only such distinct ids
// save and read back normally.
func TestSaveDistinguishesBatchIDsVerbatim(t *testing.T) {
	want := []Batch{
		saveBatch("B-1", "P", 1, "kg"),
		saveBatch("b-1", "P", 2, "kg"), // different case
		saveBatch("B-1 ", "P", 3, "kg"),
		saveBatch(" B-1", "P", 4, "kg"),
	}
	path := filepath.Join(t.TempDir(), "registry.json")
	reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), want...)}
	if err := Save(path, reg); err != nil {
		t.Fatalf("ids differing only by case or edge whitespace are distinct: %v", err)
	}
	got, existed, err := Load(path)
	if err != nil || !existed {
		t.Fatalf("saved registry must read back: existed=%v err=%v", existed, err)
	}
	assertBatchesEqual(t, got.Batches, want)
}

// When the target already exists, any rejected save (invalid record, bad
// encoding or duplicate id) leaves its bytes and modification time exactly as
// they were; the old registry still loads with its original record.
func TestSaveRejectionKeepsExistingFileUnchanged(t *testing.T) {
	const oldContent = `{"version":1,"batches":[{"batch":"OLD","product":"P","quantity":5,"unit":"kg"}]}`
	bad := string([]byte{0xFF})
	cases := map[string][]Batch{
		"invalid later record": {
			saveBatch("NEW1", "P", 1, "kg"),
			saveBatch("NEW2", "", 2, "kg"),
		},
		"duplicate identical records": {
			saveBatch("DUP", "P", 1, "kg"),
			saveBatch("DUP", "P", 1, "kg"),
		},
		"malformed utf8": {
			saveBatch("NEW1", "P", 1, "kg"),
			saveBatch("N"+bad, "P", 2, "kg"),
		},
	}
	for name, batches := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "registry.json")
			writeRegistry(t, path, oldContent)

			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			beforeBytes, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			reg := &Registry{Version: FormatVersion, Batches: batches}
			if err := Save(path, reg); err == nil {
				t.Fatal("Save must reject the table")
			}

			after, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if after.ModTime() != before.ModTime() || after.Size() != before.Size() {
				t.Fatalf("rejected save touched the file: before %+v, after %+v", before, after)
			}
			afterBytes, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(afterBytes) != string(beforeBytes) {
				t.Fatalf("rejected save changed the file bytes:\nbefore %s\nafter  %s", beforeBytes, afterBytes)
			}
			loaded, existed, err := Load(path)
			if err != nil || !existed || len(loaded.Batches) != 1 || loaded.Batches[0].Batch != "OLD" {
				t.Fatalf("old registry not preserved: %+v existed=%v err=%v", loaded, existed, err)
			}
			assertNoLeftoverTempFiles(t, dir)
		})
	}
}

// Neither success nor failure may alter the records the caller supplied,
// including their order; the failure tables keep records around and after the
// offending one exactly as passed.
func TestSaveDoesNotMutateCallerRegistry(t *testing.T) {
	fffd := string(rune(0xFFFD))
	records := []Batch{
		saveBatch(" B ", "P 1", 9, "kg"),
		saveBatch("批次", "产品", MaxQuantity, "单位"),
		saveBatch("x"+fffd, "y", 1, "z"),
	}

	t.Run("successful save", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "registry.json")
		snapshot := append([]Batch(nil), records...)
		reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), records...)}
		if err := Save(path, reg); err != nil {
			t.Fatal(err)
		}
		assertBatchesEqual(t, reg.Batches, snapshot)
		if reg.Version != FormatVersion {
			t.Fatalf("Version = %d", reg.Version)
		}
	})

	failures := map[string][]Batch{
		"duplicate id in the middle": {
			records[0], records[1], records[1], records[2],
		},
		"invalid record after valid prefix": {
			records[0],
			saveBatch("B2", "P", 0, "kg"),
			records[2],
		},
	}
	for name, batches := range failures {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "registry.json")
			snapshot := append([]Batch(nil), batches...)
			reg := &Registry{Version: FormatVersion, Batches: batches}
			if err := Save(path, reg); err == nil {
				t.Fatal("Save must reject the table")
			}
			assertBatchesEqual(t, reg.Batches, snapshot)
		})
	}
}
