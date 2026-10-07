package batchreg

import (
	"errors"
	"testing"
)

// A Registry handed directly to Register or Import may already hold two
// records under one batch id — Load refuses such files, but a Go caller can
// assemble the value itself. Both entry points must then reject the
// submission with a *DuplicateIDError describing the pre-existing duplicate,
// exactly as Load would, instead of one entry confirming a repeat and the
// other reporting a conflict against whichever record it happens to find
// first.
func dupRegistry() *Registry {
	return &Registry{Version: FormatVersion, Batches: []Batch{
		{Batch: "B1", Product: "P1", Quantity: 10, Unit: "kg"},
		{Batch: "B2", Product: "P2", Quantity: 3, Unit: "box"},
		{Batch: "B1", Product: "P1", Quantity: 20, Unit: "kg"},
	}}
}

func wantDupRegistry(t *testing.T, reg *Registry) {
	t.Helper()
	want := dupRegistry()
	if reg.Version != want.Version || len(reg.Batches) != len(want.Batches) {
		t.Fatalf("registry changed: %+v", reg)
	}
	for i, b := range want.Batches {
		if reg.Batches[i] != b {
			t.Fatalf("record %d changed: got %+v, want %+v", i+1, reg.Batches[i], b)
		}
	}
}

func checkDuplicateIDError(t *testing.T, err error, wantBatch string, wantFirst, wantSecond int) {
	t.Helper()
	var dup *DuplicateIDError
	if !errors.As(err, &dup) {
		t.Fatalf("got %v, want DuplicateIDError", err)
	}
	if dup.Batch != wantBatch || dup.First != wantFirst || dup.Second != wantSecond {
		t.Fatalf("got %+v, want batch %q records %d and %d", dup, wantBatch, wantFirst, wantSecond)
	}
}

func TestRegisterRejectsRegistryWithDuplicateIDs(t *testing.T) {
	cases := []struct {
		name string
		in   Input
	}{
		// Matches the first stored B1 record exactly: without the check this
		// would be confirmed as an idempotent duplicate.
		{"matching first record", Input{Batch: "B1", Product: "P1", Quantity: 10, Unit: "kg"}},
		// Matches the second stored B1 record exactly.
		{"matching second record", Input{Batch: "B1", Product: "P1", Quantity: 20, Unit: "kg"}},
		// Differs from both: without the check this would be a ConflictError;
		// the pre-existing duplicate outranks the content conflict.
		{"conflicting content", Input{Batch: "B1", Product: "P1", Quantity: 99, Unit: "kg"}},
		// A brand-new id is rejected too: the registry itself is ambiguous.
		{"new id", Input{Batch: "B3", Product: "P3", Quantity: 1, Unit: "kg"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := dupRegistry()
			out, err := Register(reg, tc.in)
			checkDuplicateIDError(t, err, "B1", 1, 3)
			if out != (Outcome{}) {
				t.Fatalf("rejected registration must return the zero Outcome, got %+v", out)
			}
			var ce *ConflictError
			if errors.As(err, &ce) {
				t.Fatalf("pre-existing duplicate must outrank a content conflict: %v", err)
			}
			wantDupRegistry(t, reg)
		})
	}
}

// Two stored records that are identical in every field are still a duplicate
// id, not a normal repeat submission: Register must not confirm one of them
// and must not drop or merge either.
func TestRegisterRejectsIdenticalDuplicateRecords(t *testing.T) {
	reg := &Registry{Version: FormatVersion, Batches: []Batch{
		{Batch: "B1", Product: "P1", Quantity: 10, Unit: "kg"},
		{Batch: "B1", Product: "P1", Quantity: 10, Unit: "kg"},
	}}
	out, err := Register(reg, Input{Batch: "B1", Product: "P1", Quantity: 10, Unit: "kg"})
	checkDuplicateIDError(t, err, "B1", 1, 2)
	if out != (Outcome{}) {
		t.Fatalf("rejected registration must return the zero Outcome, got %+v", out)
	}
	if len(reg.Batches) != 2 {
		t.Fatalf("duplicate records must be left untouched: %+v", reg.Batches)
	}
}

// With several duplicated ids, the id whose repeat appears earliest in stored
// order is reported, with the position of its first record.
func TestDuplicateIDErrorReportsEarliestRepeat(t *testing.T) {
	reg := &Registry{Version: FormatVersion, Batches: []Batch{
		{Batch: "B1", Product: "P1", Quantity: 1, Unit: "kg"},
		{Batch: "B2", Product: "P2", Quantity: 1, Unit: "kg"},
		{Batch: "B2", Product: "P2", Quantity: 2, Unit: "kg"}, // earliest repeat: position 3
		{Batch: "B1", Product: "P1", Quantity: 3, Unit: "kg"},
	}}
	_, err := Register(reg, Input{Batch: "NEW", Product: "P", Quantity: 1, Unit: "kg"})
	checkDuplicateIDError(t, err, "B2", 2, 3)
}

// Ids compare byte for byte: casing and edge whitespace make different ids,
// so such a registry has no duplicate and registration proceeds normally.
func TestRegisterDistinctIDsByCaseAndWhitespace(t *testing.T) {
	reg := &Registry{Version: FormatVersion, Batches: []Batch{
		{Batch: "B1", Product: "P1", Quantity: 1, Unit: "kg"},
		{Batch: "b1", Product: "P1", Quantity: 1, Unit: "kg"},
		{Batch: " B1 ", Product: "P1", Quantity: 1, Unit: "kg"},
	}}
	out, err := Register(reg, Input{Batch: "B1", Product: "P1", Quantity: 1, Unit: "kg"})
	if err != nil {
		t.Fatalf("distinct ids must not be reported as duplicates: %v", err)
	}
	if out.Created {
		t.Fatal("identical repeat should be confirmed, not created")
	}
	if len(reg.Batches) != 3 {
		t.Fatalf("registry changed: %+v", reg.Batches)
	}
}

// The established validation order is kept: a bad version or an invalid
// submission is still reported before the registry's own duplicate ids.
func TestRegisterValidationPrecedesDuplicateCheck(t *testing.T) {
	t.Run("version", func(t *testing.T) {
		reg := dupRegistry()
		reg.Version = 2
		_, err := Register(reg, Input{Batch: "B1", Product: "P1", Quantity: 10, Unit: "kg"})
		var dup *DuplicateIDError
		if err == nil || errors.As(err, &dup) {
			t.Fatalf("version error must come first, got %v", err)
		}
	})
	t.Run("empty field", func(t *testing.T) {
		reg := dupRegistry()
		_, err := Register(reg, Input{Batch: "B1", Product: "", Quantity: 10, Unit: "kg"})
		var dup *DuplicateIDError
		if err == nil || errors.As(err, &dup) {
			t.Fatalf("empty-field error must come first, got %v", err)
		}
	})
	t.Run("bad quantity", func(t *testing.T) {
		reg := dupRegistry()
		_, err := Register(reg, Input{Batch: "B1", Product: "P1", Quantity: 0, Unit: "kg"})
		var dup *DuplicateIDError
		if err == nil || errors.As(err, &dup) {
			t.Fatalf("quantity error must come first, got %v", err)
		}
	})
}

func TestImportRejectsRegistryWithDuplicateIDs(t *testing.T) {
	cases := []struct {
		name   string
		inputs []Input
	}{
		// A single record matching one of the stored B1 records: without the
		// check this would be confirmed as a duplicate.
		{"matching record", []Input{{Batch: "B1", Product: "P1", Quantity: 10, Unit: "kg"}}},
		// A record conflicting with the stored B1 records: the pre-existing
		// duplicate outranks the content conflict.
		{"conflicting record", []Input{{Batch: "B1", Product: "P1", Quantity: 99, Unit: "kg"}}},
		// Only brand-new ids: still rejected, and nothing may be appended.
		{"new ids only", []Input{
			{Batch: "B3", Product: "P3", Quantity: 1, Unit: "kg"},
			{Batch: "B4", Product: "P4", Quantity: 2, Unit: "box"},
		}},
		// New ids mixed with a repeat of the duplicated id.
		{"new ids and repeat", []Input{
			{Batch: "B3", Product: "P3", Quantity: 1, Unit: "kg"},
			{Batch: "B1", Product: "P1", Quantity: 20, Unit: "kg"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := dupRegistry()
			results, err := Import(reg, tc.inputs)
			checkDuplicateIDError(t, err, "B1", 1, 3)
			if results != nil {
				t.Fatalf("rejected import must report no results, got %+v", results)
			}
			var ce *ManifestConflictError
			if errors.As(err, &ce) {
				t.Fatalf("pre-existing duplicate must outrank a content conflict: %v", err)
			}
			wantDupRegistry(t, reg)
		})
	}
}

// Identical duplicate records in the registry reject the import too; the
// registry keeps both records and gains nothing.
func TestImportRejectsIdenticalDuplicateRecords(t *testing.T) {
	reg := &Registry{Version: FormatVersion, Batches: []Batch{
		{Batch: "B1", Product: "P1", Quantity: 10, Unit: "kg"},
		{Batch: "B1", Product: "P1", Quantity: 10, Unit: "kg"},
	}}
	results, err := Import(reg, []Input{
		{Batch: "B1", Product: "P1", Quantity: 10, Unit: "kg"},
		{Batch: "B2", Product: "P2", Quantity: 1, Unit: "kg"},
	})
	checkDuplicateIDError(t, err, "B1", 1, 2)
	if results != nil {
		t.Fatalf("rejected import must report no results, got %+v", results)
	}
	if len(reg.Batches) != 2 {
		t.Fatalf("registry changed: %+v", reg.Batches)
	}
}

// Import keeps its established validation order: version, empty manifest and
// per-record validity are all settled before the registry's own duplicate
// ids are reported.
func TestImportValidationPrecedesDuplicateCheck(t *testing.T) {
	t.Run("version", func(t *testing.T) {
		reg := dupRegistry()
		reg.Version = 2
		_, err := Import(reg, []Input{{Batch: "B3", Product: "P3", Quantity: 1, Unit: "kg"}})
		var dup *DuplicateIDError
		if err == nil || errors.As(err, &dup) {
			t.Fatalf("version error must come first, got %v", err)
		}
	})
	t.Run("empty manifest", func(t *testing.T) {
		reg := dupRegistry()
		_, err := Import(reg, nil)
		var dup *DuplicateIDError
		if err == nil || errors.As(err, &dup) {
			t.Fatalf("empty-manifest error must come first, got %v", err)
		}
	})
	t.Run("invalid record", func(t *testing.T) {
		reg := dupRegistry()
		_, err := Import(reg, []Input{{Batch: "B3", Product: "", Quantity: 1, Unit: "kg"}})
		var re *ManifestRecordError
		if !errors.As(err, &re) {
			t.Fatalf("invalid record must come first, got %v", err)
		}
		var dup *DuplicateIDError
		if errors.As(err, &dup) {
			t.Fatalf("invalid record must outrank the registry duplicate: %v", err)
		}
	})
}

// A registry whose ids are unique is unaffected: creates, duplicate
// confirmations and conflicts behave exactly as before, including repeated
// identical records within one manifest.
func TestUniqueRegistryBehaviorUnchanged(t *testing.T) {
	reg := &Registry{Version: FormatVersion, Batches: []Batch{
		{Batch: "B1", Product: "P1", Quantity: 10, Unit: "kg"},
	}}
	out, err := Import(reg, []Input{
		{Batch: "B2", Product: "P2", Quantity: 1, Unit: "kg"},
		{Batch: "B2", Product: "P2", Quantity: 1, Unit: "kg"},
		{Batch: "B1", Product: "P1", Quantity: 10, Unit: "kg"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 || !out[0].Created || out[1].Created || out[2].Created {
		t.Fatalf("unexpected results: %+v", out)
	}
	if len(reg.Batches) != 2 {
		t.Fatalf("unexpected stored records: %+v", reg.Batches)
	}
	if _, err := Register(reg, Input{Batch: "B1", Product: "P1", Quantity: 10, Unit: "kg"}); err != nil {
		t.Fatalf("duplicate confirmation must still work: %v", err)
	}
	var ce *ConflictError
	if _, err := Register(reg, Input{Batch: "B1", Product: "PX", Quantity: 10, Unit: "kg"}); !errors.As(err, &ce) {
		t.Fatalf("conflict must still be reported: %v", err)
	}
}
