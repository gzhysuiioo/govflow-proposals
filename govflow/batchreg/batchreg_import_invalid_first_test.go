package batchreg

import (
	"errors"
	"strings"
	"testing"
)

// Import validates every record of the submission before comparing any batch
// content, so an invalid record is reported before a content conflict even
// when the conflicting record sits earlier — and regardless of whether the
// conflict is against the registry or against an earlier manifest record.
// With several invalid records the earliest in submission order is named.
// The registry is left untouched either way.
func TestImportReportsInvalidRecordsBeforeConflicts(t *testing.T) {
	stored := Batch{Batch: "B1", Product: "P1", Quantity: 10, Unit: "kg"}
	cases := []struct {
		name       string
		inputs     []Input
		wantPos    int
		wantBatch  string
		wantReason string
	}{
		{
			name: "registry conflict earlier, invalid quantity later",
			inputs: []Input{
				{Batch: "B1", Product: "P1", Quantity: 11, Unit: "kg"}, // conflicts with the stored record
				{Batch: "B2", Product: "P2", Quantity: 0, Unit: "kg"},  // invalid
			},
			wantPos: 2, wantBatch: "B2", wantReason: `field "quantity" must be greater than zero`,
		},
		{
			name: "invalid record earlier, registry conflict later",
			inputs: []Input{
				{Batch: "B2", Product: "", Quantity: 2, Unit: "kg"},    // invalid
				{Batch: "B1", Product: "P1", Quantity: 11, Unit: "kg"}, // conflicts with the stored record
			},
			wantPos: 1, wantBatch: "B2", wantReason: `field "product" must not be empty`,
		},
		{
			name: "manifest-internal conflict earlier, invalid record later",
			inputs: []Input{
				{Batch: "B2", Product: "P2", Quantity: 10, Unit: "kg"},
				{Batch: "B2", Product: "P2", Quantity: 11, Unit: "kg"}, // conflicts with record 1
				{Batch: "B3", Product: "P3", Quantity: -1, Unit: "kg"}, // invalid
			},
			wantPos: 3, wantBatch: "B3", wantReason: `field "quantity" must be greater than zero`,
		},
		{
			name: "several invalid records, earliest in submission order reported",
			inputs: []Input{
				{Batch: "B1", Product: "P1", Quantity: 11, Unit: "kg"}, // conflicts with the stored record
				{Batch: "B2", Product: "P2", Quantity: 0, Unit: "kg"},  // invalid
				{Batch: "B3", Product: "", Quantity: 3, Unit: "kg"},    // invalid
			},
			wantPos: 2, wantBatch: "B2", wantReason: `field "quantity" must be greater than zero`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := &Registry{Version: FormatVersion, Batches: []Batch{stored}}
			results, err := Import(reg, tc.inputs)
			var re *ManifestRecordError
			if !errors.As(err, &re) {
				t.Fatalf("got %v, want ManifestRecordError (an invalid record outranks any conflict)", err)
			}
			var ce *ManifestConflictError
			if errors.As(err, &ce) {
				t.Fatalf("got a conflict error, want the invalid record reported first: %v", err)
			}
			if re.Position != tc.wantPos || re.Batch != tc.wantBatch {
				t.Errorf("record error = position %d batch %q, want position %d batch %q",
					re.Position, re.Batch, tc.wantPos, tc.wantBatch)
			}
			if !strings.Contains(re.Reason, tc.wantReason) {
				t.Errorf("reason = %q, want it to contain %q", re.Reason, tc.wantReason)
			}
			if results != nil {
				t.Errorf("rejected import must report no results, got %+v", results)
			}
			if len(reg.Batches) != 1 || reg.Batches[0] != stored {
				t.Errorf("rejected import mutated the registry: %+v", reg.Batches)
			}
		})
	}
}

// The invalid record of a submission is reported even when every other record
// would be a clean duplicate or a new batch: validation covers the whole
// submission before anything is registered.
func TestImportInvalidRecordReportedWithoutAnyConflict(t *testing.T) {
	stored := Batch{Batch: "B1", Product: "P1", Quantity: 10, Unit: "kg"}
	reg := &Registry{Version: FormatVersion, Batches: []Batch{stored}}
	inputs := []Input{
		{Batch: "B1", Product: "P1", Quantity: 10, Unit: "kg"}, // clean duplicate
		{Batch: "B2", Product: "P2", Quantity: 2, Unit: "kg"},  // new batch
		{Batch: "B3", Product: "P3", Quantity: 3, Unit: ""},    // invalid
	}
	results, err := Import(reg, inputs)
	var re *ManifestRecordError
	if !errors.As(err, &re) {
		t.Fatalf("got %v, want ManifestRecordError", err)
	}
	if re.Position != 3 || re.Batch != "B3" {
		t.Errorf("record error = position %d batch %q, want position 3 batch B3", re.Position, re.Batch)
	}
	if results != nil {
		t.Errorf("rejected import must report no results, got %+v", results)
	}
	if len(reg.Batches) != 1 || reg.Batches[0] != stored {
		t.Errorf("rejected import mutated the registry: %+v", reg.Batches)
	}
}
