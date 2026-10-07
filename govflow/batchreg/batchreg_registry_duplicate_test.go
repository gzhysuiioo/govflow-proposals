package batchreg

import (
	"errors"
	"reflect"
	"testing"
)

// A registry handed to Register or Import that already holds two records
// under one batch id must be rejected identically at both entry points: the
// pre-existing duplicate is never read as a duplicate confirmation, never
// reported as a conflict and never merged or dropped. The error is the
// public *DuplicateIDError naming the id and the 1-based positions of the
// first two occurrences in the pre-call registry.
func TestRegisterAndImportRejectRegistryWithDuplicateIDs(t *testing.T) {
	stored := []Batch{
		{Batch: "B1", Product: "P1", Quantity: 10, Unit: "kg"},
		{Batch: "B2", Product: "P2", Quantity: 3, Unit: "box"},
		{Batch: "B1", Product: "P1", Quantity: 20, Unit: "kg"},
	}
	submissions := []struct {
		name  string
		input Input
	}{
		{"matches the first stored record", Input{Batch: "B1", Product: "P1", Quantity: 10, Unit: "kg"}},
		{"matches the second stored record", Input{Batch: "B1", Product: "P1", Quantity: 20, Unit: "kg"}},
		{"conflicts with both stored records", Input{Batch: "B1", Product: "P9", Quantity: 1, Unit: "g"}},
		{"brand new id", Input{Batch: "B3", Product: "P3", Quantity: 5, Unit: "box"}},
	}
	for _, tc := range submissions {
		t.Run("register/"+tc.name, func(t *testing.T) {
			reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), stored...)}
			out, err := Register(reg, tc.input)
			var de *DuplicateIDError
			if !errors.As(err, &de) {
				t.Fatalf("got %v, want DuplicateIDError", err)
			}
			if de.Batch != "B1" || de.First != 1 || de.Second != 3 {
				t.Errorf("duplicate = batch %q records %d and %d, want B1 records 1 and 3", de.Batch, de.First, de.Second)
			}
			if out != (Outcome{}) {
				t.Errorf("rejected register returned %+v, want zero Outcome", out)
			}
			if !reflect.DeepEqual(reg.Batches, stored) {
				t.Errorf("rejected register mutated the registry: %+v", reg.Batches)
			}
		})
		t.Run("import/"+tc.name, func(t *testing.T) {
			reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), stored...)}
			results, err := Import(reg, []Input{tc.input})
			var de *DuplicateIDError
			if !errors.As(err, &de) {
				t.Fatalf("got %v, want DuplicateIDError", err)
			}
			if de.Batch != "B1" || de.First != 1 || de.Second != 3 {
				t.Errorf("duplicate = batch %q records %d and %d, want B1 records 1 and 3", de.Batch, de.First, de.Second)
			}
			if results != nil {
				t.Errorf("rejected import must report no results, got %+v", results)
			}
			if !reflect.DeepEqual(reg.Batches, stored) {
				t.Errorf("rejected import mutated the registry: %+v", reg.Batches)
			}
		})
	}
}

// Two stored records that are identical in every field still make the
// registry unusable: the duplicate id is a defect of the registry itself, not
// a normal repeat submission, and neither entry point may confirm, drop or
// merge one of the records.
func TestDuplicateIDsRejectedEvenWhenRecordsAreIdentical(t *testing.T) {
	stored := []Batch{
		{Batch: "B1", Product: "P1", Quantity: 10, Unit: "kg"},
		{Batch: "B1", Product: "P1", Quantity: 10, Unit: "kg"},
	}
	reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), stored...)}
	out, err := Register(reg, Input{Batch: "B1", Product: "P1", Quantity: 10, Unit: "kg"})
	var de *DuplicateIDError
	if !errors.As(err, &de) {
		t.Fatalf("Register got %v, want DuplicateIDError", err)
	}
	if de.Batch != "B1" || de.First != 1 || de.Second != 2 {
		t.Errorf("duplicate = batch %q records %d and %d, want B1 records 1 and 2", de.Batch, de.First, de.Second)
	}
	if out != (Outcome{}) {
		t.Errorf("rejected register returned %+v, want zero Outcome", out)
	}
	if !reflect.DeepEqual(reg.Batches, stored) {
		t.Errorf("rejected register mutated the registry: %+v", reg.Batches)
	}

	reg = &Registry{Version: FormatVersion, Batches: append([]Batch(nil), stored...)}
	results, err := Import(reg, []Input{{Batch: "B1", Product: "P1", Quantity: 10, Unit: "kg"}})
	if !errors.As(err, &de) {
		t.Fatalf("Import got %v, want DuplicateIDError", err)
	}
	if de.Batch != "B1" || de.First != 1 || de.Second != 2 {
		t.Errorf("duplicate = batch %q records %d and %d, want B1 records 1 and 2", de.Batch, de.First, de.Second)
	}
	if results != nil {
		t.Errorf("rejected import must report no results, got %+v", results)
	}
	if !reflect.DeepEqual(reg.Batches, stored) {
		t.Errorf("rejected import mutated the registry: %+v", reg.Batches)
	}
}

// With several duplicated ids in the registry, the one whose second
// occurrence comes earliest in registry order is reported, with the position
// of its first occurrence.
func TestDuplicateIDErrorReportsEarliestReappearingID(t *testing.T) {
	reg := &Registry{Version: FormatVersion, Batches: []Batch{
		{Batch: "B1", Product: "P1", Quantity: 1, Unit: "kg"},
		{Batch: "B2", Product: "P2", Quantity: 2, Unit: "kg"},
		{Batch: "B2", Product: "P2", Quantity: 3, Unit: "kg"}, // B2 reappears at 3
		{Batch: "B1", Product: "P1", Quantity: 4, Unit: "kg"}, // B1 reappears at 4
	}}
	_, err := Register(reg, Input{Batch: "B9", Product: "P9", Quantity: 1, Unit: "box"})
	var de *DuplicateIDError
	if !errors.As(err, &de) {
		t.Fatalf("got %v, want DuplicateIDError", err)
	}
	if de.Batch != "B2" || de.First != 2 || de.Second != 3 {
		t.Errorf("duplicate = batch %q records %d and %d, want B2 records 2 and 3", de.Batch, de.First, de.Second)
	}
}

// The duplicate-id check runs only after the entry point's own version and
// argument validation: an unsupported version, an invalid submission and (for
// Import) an invalid manifest record keep their established errors even when
// the registry also holds duplicate ids.
func TestDuplicateIDCheckKeepsValidationOrder(t *testing.T) {
	stored := []Batch{
		{Batch: "B1", Product: "P1", Quantity: 10, Unit: "kg"},
		{Batch: "B1", Product: "P1", Quantity: 10, Unit: "kg"},
	}

	reg := &Registry{Version: 99, Batches: append([]Batch(nil), stored...)}
	if _, err := Register(reg, Input{Batch: "B3", Product: "P3", Quantity: 1, Unit: "kg"}); err == nil ||
		err.Error() != "unsupported registry version 99" {
		t.Errorf("Register with bad version got %v, want the version error", err)
	}
	if _, err := Import(reg, []Input{{Batch: "B3", Product: "P3", Quantity: 1, Unit: "kg"}}); err == nil ||
		err.Error() != "unsupported registry version 99" {
		t.Errorf("Import with bad version got %v, want the version error", err)
	}

	reg = &Registry{Version: FormatVersion, Batches: append([]Batch(nil), stored...)}
	if _, err := Register(reg, Input{Batch: "", Product: "P3", Quantity: 1, Unit: "kg"}); err == nil ||
		err.Error() != "batch, product and unit must be non-empty" {
		t.Errorf("Register with empty batch got %v, want the empty-field error", err)
	}
	if _, err := Register(reg, Input{Batch: "B3", Product: "P3", Quantity: 0, Unit: "kg"}); err == nil {
		t.Errorf("Register with zero quantity got nil, want the quantity error")
	}

	reg = &Registry{Version: FormatVersion, Batches: append([]Batch(nil), stored...)}
	if _, err := Import(reg, nil); err == nil || err.Error() != "manifest must contain at least one record" {
		t.Errorf("Import with no records got %v, want the empty-manifest error", err)
	}
	var mre *ManifestRecordError
	if _, err := Import(reg, []Input{{Batch: "B3", Product: "", Quantity: 1, Unit: "kg"}}); !errors.As(err, &mre) {
		t.Errorf("Import with an invalid record got %v, want ManifestRecordError", err)
	}
}

// Duplicate detection compares the stored ids byte for byte: B1, b1 and a
// padded " B1 " are three different ids, so a registry holding them is not
// rejected and submissions against it behave as usual.
func TestDistinctIDSpellingsAreNotDuplicates(t *testing.T) {
	reg := &Registry{Version: FormatVersion, Batches: []Batch{
		{Batch: "B1", Product: "P1", Quantity: 1, Unit: "kg"},
		{Batch: "b1", Product: "P2", Quantity: 2, Unit: "kg"},
		{Batch: " B1 ", Product: "P3", Quantity: 3, Unit: "kg"},
	}}
	out, err := Register(reg, Input{Batch: "B1", Product: "P1", Quantity: 1, Unit: "kg"})
	if err != nil {
		t.Fatalf("Register got %v, want a duplicate confirmation", err)
	}
	if out.Created {
		t.Errorf("Register created a new record for the exact repeat of B1")
	}
	if len(reg.Batches) != 3 {
		t.Errorf("registry holds %d records, want the original 3", len(reg.Batches))
	}
}

// A rejected import must not leave behind other manifest records that would
// have been new: the whole submission is refused together with the duplicate
// registry.
func TestImportRejectionLeavesNoNewBatchesBehind(t *testing.T) {
	stored := []Batch{
		{Batch: "B1", Product: "P1", Quantity: 10, Unit: "kg"},
		{Batch: "B1", Product: "P1", Quantity: 20, Unit: "kg"},
	}
	reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), stored...)}
	results, err := Import(reg, []Input{
		{Batch: "B8", Product: "P8", Quantity: 1, Unit: "box"},
		{Batch: "B9", Product: "P9", Quantity: 2, Unit: "box"},
	})
	var de *DuplicateIDError
	if !errors.As(err, &de) {
		t.Fatalf("got %v, want DuplicateIDError", err)
	}
	if results != nil {
		t.Errorf("rejected import must report no results, got %+v", results)
	}
	if !reflect.DeepEqual(reg.Batches, stored) {
		t.Errorf("rejected import left new batches in the registry: %+v", reg.Batches)
	}
}
