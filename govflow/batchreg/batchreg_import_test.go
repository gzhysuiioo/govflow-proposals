package batchreg

import (
	"errors"
	"strings"
	"testing"
)

// Directly submitted Input records must satisfy the same effective-value
// rules as single Register calls and parsed file records: the three text
// fields are non-empty valid UTF-8 and quantity is positive — regardless of
// whether the batch id is new, already stored or seen earlier in the same
// submission, so the validity check can never be skipped by the create,
// duplicate-confirmation or conflict branch.
func TestImportRejectsInvalidDirectRecords(t *testing.T) {
	valid := func() Input { return Input{Batch: "B1", Product: "P-7", Quantity: 10, Unit: "kg"} }

	cases := []struct {
		name      string
		mutate    func(*Input)
		wantField string
	}{
		{"empty batch", func(in *Input) { in.Batch = "" }, "batch"},
		{"empty product", func(in *Input) { in.Product = "" }, "product"},
		{"empty unit", func(in *Input) { in.Unit = "" }, "unit"},
		{"quantity zero", func(in *Input) { in.Quantity = 0 }, "quantity"},
		{"quantity negative", func(in *Input) { in.Quantity = -4 }, "quantity"},
	}

	// Each scenario places the offending record where, before the fix, a
	// different branch used to accept it or classify it as a conflict: a
	// brand-new id, an id already stored (an empty text field would have
	// looked like a registry conflict), and an id introduced earlier in the
	// same submission. prepare aligns the record with the stored/earlier id
	// before the mutation empties the batch field itself.
	scenarios := []struct {
		name    string
		reg     []Batch
		pos     int
		prepare func(in *Input)
		prefix  func() []Input // records preceding the bad one
	}{
		{
			name:    "new id",
			pos:     1,
			prepare: func(in *Input) {},
			prefix:  func() []Input { return nil },
		},
		{
			name: "id already registered",
			reg:  []Batch{{Batch: "B1", Product: "P-7", Quantity: 10, Unit: "kg"}},
			pos:  2,
			prepare: func(in *Input) {
				in.Batch = "B1"
				in.Product = "P-7"
				in.Unit = "kg"
			},
			// Record 1 confirms the stored duplicate before record 2 fails.
			prefix: func() []Input {
				return []Input{{Batch: "B1", Product: "P-7", Quantity: 10, Unit: "kg"}}
			},
		},
		{
			name:    "id seen earlier in this submission",
			pos:     2,
			prepare: func(in *Input) { in.Batch = "B1" },
			prefix: func() []Input {
				return []Input{{Batch: "B1", Product: "P-7", Quantity: 10, Unit: "kg"}}
			},
		},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), sc.reg...)}
					bad := valid()
					sc.prepare(&bad)
					tc.mutate(&bad)
					inputs := append(sc.prefix(), bad)

					results, err := Import(reg, inputs)
					var re *ManifestRecordError
					if !errors.As(err, &re) {
						t.Fatalf("got %v, want ManifestRecordError", err)
					}
					var ce *ManifestConflictError
					if errors.As(err, &ce) {
						t.Fatalf("invalid record must not be reported as a conflict: %v", err)
					}
					if results != nil {
						t.Fatalf("failed import must report no results, got %+v", results)
					}
					if re.Position != sc.pos {
						t.Errorf("position=%d, want %d", re.Position, sc.pos)
					}
					if !strings.Contains(re.Reason, tc.wantField) {
						t.Errorf("reason %q must name field %q", re.Reason, tc.wantField)
					}
					// Registry keeps exactly its pre-import content.
					if len(reg.Batches) != len(sc.reg) {
						t.Fatalf("rejected import mutated the registry: %+v", reg.Batches)
					}
					for i, b := range sc.reg {
						if reg.Batches[i] != b {
							t.Fatalf("rejected import mutated record %d: %+v", i+1, reg.Batches[i])
						}
					}
				})
			}
		})
	}
}

// Validity is settled for every record before any conflict is weighed: an
// invalid record is the reported failure even when an earlier record already
// clashes with a stored record or with a record earlier in the same
// submission. Swapping the invalid record and the conflicting one only moves
// the reported position — the precedence never changes.
func TestImportInvalidRecordOutranksConflict(t *testing.T) {
	stored := Batch{Batch: "B1", Product: "P1", Quantity: 10, Unit: "kg"}
	cases := []struct {
		name    string
		inputs  []Input
		wantPos int
	}{
		{
			name: "registry conflict before invalid record",
			inputs: []Input{
				{Batch: "B1", Product: "P1", Quantity: 11, Unit: "kg"}, // conflicts with the stored record
				{Batch: "B2", Product: "P2", Quantity: 1, Unit: ""},    // invalid
			},
			wantPos: 2,
		},
		{
			name: "invalid record before registry conflict",
			inputs: []Input{
				{Batch: "B2", Product: "P2", Quantity: 1, Unit: ""},    // invalid
				{Batch: "B1", Product: "P1", Quantity: 11, Unit: "kg"}, // would conflict with the stored record
			},
			wantPos: 1,
		},
		{
			name: "same-submission conflict before invalid record",
			inputs: []Input{
				{Batch: "BX", Product: "P1", Quantity: 10, Unit: "kg"}, // would create
				{Batch: "BX", Product: "P1", Quantity: 99, Unit: "kg"}, // conflicts with record 1
				{Batch: "B2", Product: "P2", Quantity: 0, Unit: "kg"},  // invalid
			},
			wantPos: 3,
		},
		{
			name: "first of several invalid records wins over earlier conflict",
			inputs: []Input{
				{Batch: "B1", Product: "P1", Quantity: 11, Unit: "kg"}, // conflicts with the stored record
				{Batch: "B2", Product: "", Quantity: 1, Unit: "kg"},    // invalid
				{Batch: "B3", Product: "P3", Quantity: -1, Unit: "kg"}, // invalid
			},
			wantPos: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := &Registry{Version: FormatVersion, Batches: []Batch{stored}}
			results, err := Import(reg, tc.inputs)
			var re *ManifestRecordError
			if !errors.As(err, &re) {
				t.Fatalf("got %v, want ManifestRecordError", err)
			}
			var ce *ManifestConflictError
			if errors.As(err, &ce) {
				t.Fatalf("an invalid record must never be reported as a conflict: %v", err)
			}
			if re.Position != tc.wantPos {
				t.Errorf("position=%d, want %d", re.Position, tc.wantPos)
			}
			if results != nil {
				t.Errorf("failed import must report no results, got %+v", results)
			}
			if len(reg.Batches) != 1 || reg.Batches[0] != stored {
				t.Errorf("rejected import mutated the registry: %+v", reg.Batches)
			}
		})
	}
}

// A late invalid record fails the whole submission even when the earlier
// records have already resolved as creates, same-submission duplicates and
// stored duplicates: nothing new may remain and the caller gets no partial
// result.
func TestImportLateInvalidRecordFailsWholeSubmission(t *testing.T) {
	original := []Batch{{Batch: "OLD", Product: "P-1", Quantity: 3, Unit: "kg"}}
	reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), original...)}
	results, err := Import(reg, []Input{
		{Batch: "NEW1", Product: "P-2", Quantity: 10, Unit: "box"}, // would create
		{Batch: "NEW1", Product: "P-2", Quantity: 10, Unit: "box"}, // same-submission duplicate
		{Batch: "OLD", Product: "P-1", Quantity: 3, Unit: "kg"},    // stored duplicate
		{Batch: "NEW2", Product: "P-3", Quantity: 1, Unit: "m"},    // would create
		{Batch: "NEW3", Product: "P-4", Quantity: 1, Unit: ""},     // invalid
	})
	var re *ManifestRecordError
	if !errors.As(err, &re) {
		t.Fatalf("got %v, want ManifestRecordError", err)
	}
	if re.Position != 5 || re.Batch != "NEW3" {
		t.Fatalf("unexpected error position/batch: %+v", re)
	}
	if !strings.Contains(re.Reason, `"unit"`) {
		t.Fatalf("reason must name field %q: %s", "unit", re.Reason)
	}
	if results != nil {
		t.Fatalf("failed import must report no results, got %+v", results)
	}
	if len(reg.Batches) != 1 || reg.Batches[0] != original[0] {
		t.Fatalf("rejected import left new batches in the registry: %+v", reg.Batches)
	}
}

// The batch id is cited only when the batch field itself is non-empty and
// valid UTF-8; an empty or malformed id is never invented or replaced. The
// offending field always rides in the reason text.
func TestImportInvalidRecordBatchAttachment(t *testing.T) {
	fffd := string(rune(0xfffd))
	cases := []struct {
		name            string
		in              Input
		wantBatch       string
		wantReasonField string
	}{
		{"empty batch", Input{Batch: "", Product: "P", Quantity: 1, Unit: "kg"}, "", "batch"},
		{"invalid utf8 batch", Input{Batch: "B-\xff", Product: "P", Quantity: 1, Unit: "kg"}, "", "batch"},
		{"empty product names batch", Input{Batch: "B7", Product: "", Quantity: 1, Unit: "kg"}, "B7", "product"},
		{"empty unit names batch", Input{Batch: "B7", Product: "P", Quantity: 1, Unit: ""}, "B7", "unit"},
		{"zero quantity names batch", Input{Batch: "B7", Product: "P", Quantity: 0, Unit: "kg"}, "B7", "quantity"},
		{"bad product bytes names valid batch", Input{Batch: "B7", Product: "P\xff", Quantity: 1, Unit: "kg"}, "B7", "product"},
		{"whitespace batch cited verbatim", Input{Batch: " B ", Product: "P", Quantity: 0, Unit: "kg"}, " B ", "quantity"},
		{"genuine replacement char is a valid id", Input{Batch: fffd, Product: "P", Quantity: 0, Unit: "kg"}, fffd, "quantity"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := &Registry{Version: FormatVersion}
			_, err := Import(reg, []Input{tc.in})
			var re *ManifestRecordError
			if !errors.As(err, &re) {
				t.Fatalf("got %v, want ManifestRecordError", err)
			}
			if re.Position != 1 {
				t.Errorf("position=%d, want 1", re.Position)
			}
			if re.Batch != tc.wantBatch {
				t.Errorf("batch=%q, want %q", re.Batch, tc.wantBatch)
			}
			if !strings.Contains(re.Reason, tc.wantReasonField) {
				t.Errorf("reason %q must name field %q", re.Reason, tc.wantReasonField)
			}
			if tc.wantBatch == "" && strings.Contains(err.Error(), "(batch \"") {
				t.Errorf("error must not cite a batch id: %v", err)
			}
			if tc.wantBatch != "" && !strings.Contains(err.Error(), `(batch "`+tc.wantBatch+`")`) {
				t.Errorf("error must cite batch %q verbatim: %v", tc.wantBatch, err)
			}
			if len(reg.Batches) != 0 {
				t.Fatalf("rejected import appended records: %+v", reg.Batches)
			}
		})
	}
}

// Direct submissions keep text verbatim: no trimming and no case folding.
// Edge and interior whitespace stay part of the value, so ids that differ
// only by such characters are different batches.
func TestImportKeepsDirectTextVerbatim(t *testing.T) {
	reg := &Registry{Version: FormatVersion}
	inputs := []Input{
		{Batch: " B1 ", Product: " P 7 ", Quantity: 1, Unit: " kg "},
		{Batch: " B1 ", Product: " P 7 ", Quantity: 1, Unit: " kg "}, // verbatim repeat -> duplicate
		{Batch: "B1", Product: "P7", Quantity: 1, Unit: "kg"},        // distinct id and values
	}
	out, err := Import(reg, inputs)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 || !out[0].Created || out[1].Created || !out[2].Created {
		t.Fatalf("unexpected outcomes: %+v", out)
	}
	want := []Batch{
		{Batch: " B1 ", Product: " P 7 ", Quantity: 1, Unit: " kg "},
		{Batch: "B1", Product: "P7", Quantity: 1, Unit: "kg"},
	}
	for i, b := range want {
		if reg.Batches[i] != b {
			t.Errorf("stored record %d = %+v, want %+v", i+1, reg.Batches[i], b)
		}
	}
}

// Whitespace-only text is a legal non-empty direct value: the no-trimming
// rule belongs to the direct path, and such a record is accepted the same
// way it would be through the single-record Register entry.
func TestImportAcceptsWhitespaceOnlyDirectText(t *testing.T) {
	reg := &Registry{Version: FormatVersion}
	in := Input{Batch: " ", Product: "\t", Quantity: 1, Unit: "\n"}
	out, err := Import(reg, []Input{in, in})
	if err != nil {
		t.Fatalf("whitespace-only text is non-empty and must be accepted: %v", err)
	}
	if !out[0].Created || out[1].Created {
		t.Fatalf("unexpected outcomes: %+v", out)
	}
	if len(reg.Batches) != 1 || reg.Batches[0].Batch != " " || reg.Batches[0].Product != "\t" || reg.Batches[0].Unit != "\n" {
		t.Fatalf("text not kept verbatim: %+v", reg.Batches)
	}
}
