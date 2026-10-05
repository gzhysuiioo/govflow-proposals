package batchreg

import (
	"errors"
	"strings"
	"testing"
)

// When a batch id is introduced by the manifest itself, a later conflicting
// record must be reported against the id's first manifest occurrence — never
// against an intermediate identical record. Duplicate confirmations in
// between neither become the new source nor replace the first record's
// content, no matter how often the identical record is repeated.
func TestImportConflictSourceIsFirstManifestOccurrence(t *testing.T) {
	cases := []struct {
		name       string
		inputs     []Input
		wantPos    int
		wantFields []string
	}{
		{
			name: "one intermediate duplicate, quantity differs",
			inputs: []Input{
				{Batch: "B1", Product: "P-7", Quantity: 10, Unit: "kg"}, // first occurrence
				{Batch: "B2", Product: "P-8", Quantity: 1, Unit: "box"},
				{Batch: "B1", Product: "P-7", Quantity: 10, Unit: "kg"}, // identical repeat
				{Batch: "B1", Product: "P-7", Quantity: 11, Unit: "kg"}, // conflict
			},
			wantPos:    4,
			wantFields: []string{"quantity"},
		},
		{
			name: "several intermediate duplicates, product differs",
			inputs: []Input{
				{Batch: "B1", Product: "P-7", Quantity: 10, Unit: "kg"},
				{Batch: "B1", Product: "P-7", Quantity: 10, Unit: "kg"},
				{Batch: "B1", Product: "P-7", Quantity: 10, Unit: "kg"},
				{Batch: "B1", Product: "P-9", Quantity: 10, Unit: "kg"},
			},
			wantPos:    4,
			wantFields: []string{"product"},
		},
		{
			name: "unit differs after a duplicate",
			inputs: []Input{
				{Batch: "B1", Product: "P-7", Quantity: 10, Unit: "kg"},
				{Batch: "B1", Product: "P-7", Quantity: 10, Unit: "kg"},
				{Batch: "B1", Product: "P-7", Quantity: 10, Unit: "g"},
			},
			wantPos:    3,
			wantFields: []string{"unit"},
		},
		{
			name: "all three fields differ after duplicates",
			inputs: []Input{
				{Batch: "B1", Product: "P-7", Quantity: 10, Unit: "kg"},
				{Batch: "B1", Product: "P-7", Quantity: 10, Unit: "kg"},
				{Batch: "B1", Product: "P-9", Quantity: 11, Unit: "g"},
			},
			wantPos:    3,
			wantFields: []string{"product", "quantity", "unit"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := &Registry{Version: FormatVersion}
			results, err := Import(reg, tc.inputs)
			var ce *ManifestConflictError
			if !errors.As(err, &ce) {
				t.Fatalf("got %v, want ManifestConflictError", err)
			}
			if ce.Position != tc.wantPos || ce.Batch != "B1" {
				t.Errorf("conflict = record %d batch %q, want record %d batch B1", ce.Position, ce.Batch, tc.wantPos)
			}
			if ce.Source != "manifest" || ce.PrevPos != 1 {
				t.Errorf("source = %q prev %d, want manifest record 1 (the first occurrence, not an intermediate duplicate)", ce.Source, ce.PrevPos)
			}
			if strings.Join(ce.Fields, ",") != strings.Join(tc.wantFields, ",") {
				t.Errorf("fields = %v, want %v", ce.Fields, tc.wantFields)
			}
			msg := err.Error()
			if !strings.Contains(msg, "conflicts with manifest record 1") {
				t.Errorf("error must name manifest record 1 as the conflict source: %v", msg)
			}
			if strings.Contains(msg, "conflicts with manifest record 2") || strings.Contains(msg, "conflicts with manifest record 3") {
				t.Errorf("an intermediate duplicate must not become the conflict source: %v", msg)
			}
			if results != nil {
				t.Errorf("rejected import must report no results, got %+v", results)
			}
			if len(reg.Batches) != 0 {
				t.Errorf("rejected import left new batches in the registry: %+v", reg.Batches)
			}
		})
	}
}

// When the batch id was already registered before the import, identical
// manifest records only confirm the stored record — however often they
// repeat. A later differing record conflicts with the registered record, and
// the error must say so; the confirmations must not turn the source into a
// manifest record.
func TestImportConflictSourceStaysRegistryAfterConfirmations(t *testing.T) {
	stored := Batch{Batch: "B1", Product: "P-7", Quantity: 10, Unit: "kg"}
	reg := &Registry{Version: FormatVersion, Batches: []Batch{stored}}
	inputs := []Input{
		{Batch: "B1", Product: "P-7", Quantity: 10, Unit: "kg"}, // confirms the stored record
		{Batch: "B2", Product: "P-8", Quantity: 1, Unit: "box"},
		{Batch: "B1", Product: "P-7", Quantity: 10, Unit: "kg"}, // confirms again
		{Batch: "B1", Product: "P-7", Quantity: 10, Unit: "kg"}, // and again
		{Batch: "B1", Product: "P-7", Quantity: 12, Unit: "kg"}, // conflict
	}
	results, err := Import(reg, inputs)
	var ce *ManifestConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("got %v, want ManifestConflictError", err)
	}
	if ce.Position != 5 || ce.Batch != "B1" {
		t.Errorf("conflict = record %d batch %q, want record 5 batch B1", ce.Position, ce.Batch)
	}
	if ce.Source != "registry" {
		t.Errorf("source = %q, want %q: the conflict is with the registered record", ce.Source, "registry")
	}
	if ce.PrevPos != 0 {
		t.Errorf("PrevPos = %d, want 0: no manifest record is the source", ce.PrevPos)
	}
	if strings.Join(ce.Fields, ",") != "quantity" {
		t.Errorf("fields = %v, want [quantity]", ce.Fields)
	}
	msg := err.Error()
	if !strings.Contains(msg, "conflicts with the registered record") {
		t.Errorf("error must name the registered record as the conflict source: %v", msg)
	}
	if strings.Contains(msg, "conflicts with manifest record") {
		t.Errorf("a duplicate confirmation must not become the conflict source: %v", msg)
	}
	if results != nil {
		t.Errorf("rejected import must report no results, got %+v", results)
	}
	if len(reg.Batches) != 1 || reg.Batches[0] != stored {
		t.Errorf("rejected import mutated the registry: %+v", reg.Batches)
	}
}

// Manifest text is compared after trimming leading and trailing whitespace,
// so padded spellings of one id share a single source: the conflict is
// reported against the id's first (padded) occurrence. Ids whose interior
// characters or casing differ remain different batches and never conflict.
func TestImportConflictSourceWithTrimmedIDs(t *testing.T) {
	manifest := []byte(`[
		{"batch": " B1 ", "product": "P-7", "quantity": 10, "unit": "kg"},
		{"batch": "B 1", "product": "P-8", "quantity": 1, "unit": "box"},
		{"batch": "b1", "product": "P-9", "quantity": 2, "unit": "g"},
		{"batch": "B1", "product": "P-7", "quantity": 10, "unit": "kg"},
		{"batch": "\tB1\n", "product": "P-7", "quantity": 11, "unit": "kg"}
	]`)
	inputs, err := ParseManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	reg := &Registry{Version: FormatVersion}
	_, err = Import(reg, inputs)
	var ce *ManifestConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("got %v, want ManifestConflictError", err)
	}
	if ce.Position != 5 || ce.Batch != "B1" {
		t.Errorf("conflict = record %d batch %q, want record 5 batch B1", ce.Position, ce.Batch)
	}
	if ce.Source != "manifest" || ce.PrevPos != 1 {
		t.Errorf("source = %q prev %d, want manifest record 1: trimming must not move the source", ce.Source, ce.PrevPos)
	}
	if strings.Join(ce.Fields, ",") != "quantity" {
		t.Errorf("fields = %v, want [quantity]", ce.Fields)
	}
	if len(reg.Batches) != 0 {
		t.Errorf("rejected import left new batches in the registry: %+v", reg.Batches)
	}
}

// The success mirror of the conflict scenarios: when the last record matches
// the first occurrence again, the whole import goes through. Results keep
// manifest order — the first occurrence is created, every identical repeat
// and every already-registered record is a duplicate — and the registry ends
// up holding exactly one record per batch id.
func TestImportSucceedsWhenLateRecordMatchesFirstOccurrence(t *testing.T) {
	stored := Batch{Batch: "B0", Product: "P-0", Quantity: 5, Unit: "box"}
	reg := &Registry{Version: FormatVersion, Batches: []Batch{stored}}
	inputs := []Input{
		{Batch: "B1", Product: "P-7", Quantity: 10, Unit: "kg"}, // created
		{Batch: "B2", Product: "P-8", Quantity: 1, Unit: "box"}, // created
		{Batch: "B1", Product: "P-7", Quantity: 10, Unit: "kg"}, // duplicate of record 1
		{Batch: "B0", Product: "P-0", Quantity: 5, Unit: "box"}, // duplicate of the stored record
		{Batch: "B1", Product: "P-7", Quantity: 10, Unit: "kg"}, // duplicate of record 1
	}
	out, err := Import(reg, inputs)
	if err != nil {
		t.Fatal(err)
	}
	wantCreated := []bool{true, true, false, false, false}
	if len(out) != len(wantCreated) {
		t.Fatalf("got %d results, want %d", len(out), len(wantCreated))
	}
	for i, want := range wantCreated {
		if out[i].Created != want {
			t.Errorf("record %d created=%v, want %v", i+1, out[i].Created, want)
		}
		if out[i].Batch.Batch != inputs[i].Batch {
			t.Errorf("record %d batch=%q, want %q (manifest order)", i+1, out[i].Batch.Batch, inputs[i].Batch)
		}
	}
	wantBatches := []Batch{
		stored,
		{Batch: "B1", Product: "P-7", Quantity: 10, Unit: "kg"},
		{Batch: "B2", Product: "P-8", Quantity: 1, Unit: "box"},
	}
	if len(reg.Batches) != len(wantBatches) {
		t.Fatalf("registry holds %d records, want one per batch id: %+v", len(reg.Batches), reg.Batches)
	}
	for i, want := range wantBatches {
		if reg.Batches[i] != want {
			t.Errorf("stored record %d = %+v, want %+v", i+1, reg.Batches[i], want)
		}
	}
}
