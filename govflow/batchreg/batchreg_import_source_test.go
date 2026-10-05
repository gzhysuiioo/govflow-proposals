package batchreg

import (
	"errors"
	"strings"
	"testing"
)

// These tests pin down which record a batch-import conflict is blamed on
// when the same batch id shows up several times before the differing one:
// the middle identical submissions are mere duplicate confirmations and can
// neither become a new source of truth nor overwrite the first content.

// assertConflict checks a *ManifestConflictError's full source attribution.
func assertConflict(t *testing.T, err error, position int, batch string, fields []string, source string, prevPos int) {
	t.Helper()
	var ce *ManifestConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("expected ManifestConflictError, got %v", err)
	}
	if ce.Position != position || ce.Batch != batch || ce.Source != source || ce.PrevPos != prevPos {
		t.Fatalf("unexpected conflict attribution: %+v", ce)
	}
	if strings.Join(ce.Fields, ",") != strings.Join(fields, ",") {
		t.Fatalf("conflict fields = %v, want %v", ce.Fields, fields)
	}
}

// A batch introduced by the manifest keeps its first occurrence as the sole
// source for every later comparison, no matter how many identical
// confirmations sit in between. The four-record scenario from the
// specification: record 1 introduces B1, record 2 handles another batch,
// record 3 confirms B1, and record 4 changes the quantity. The error must
// point at record 4 and name manifest record 1 — never record 3.
func TestImportConflictAfterRepeatedConfirmsAnchorsToFirstManifestRecord(t *testing.T) {
	reg := &Registry{Version: FormatVersion}
	_, err := Import(reg, []Input{
		{Batch: "B1", Product: "P", Quantity: 10, Unit: "kg"}, // 1: created
		{Batch: "OTHER", Product: "P2", Quantity: 1, Unit: "box"},
		{Batch: "B1", Product: "P", Quantity: 10, Unit: "kg"}, // 3: duplicate confirmation
		{Batch: "B1", Product: "P", Quantity: 99, Unit: "kg"}, // 4: conflicts
	})
	assertConflict(t, err, 4, "B1", []string{"quantity"}, "manifest", 1)
	if len(reg.Batches) != 0 {
		t.Fatalf("rejected manifest left new batches in the registry: %+v", reg.Batches)
	}
	if !strings.Contains(err.Error(), "manifest record 4") ||
		!strings.Contains(err.Error(), "conflicts with manifest record 1") ||
		!strings.Contains(err.Error(), "quantity") {
		t.Fatalf("error must cite record 4 and its conflict with record 1: %v", err)
	}
	if strings.Contains(err.Error(), "manifest record 3") {
		t.Fatalf("a duplicate confirmation must not become the cited source: %v", err)
	}
}

// Several identical confirmations in a row must not shift the anchor: the
// conflict still names the first manifest record.
func TestImportConflictAfterManyConsecutiveConfirmsKeepsFirstPosition(t *testing.T) {
	reg := &Registry{Version: FormatVersion}
	inputs := []Input{
		{Batch: "B1", Product: "P", Quantity: 10, Unit: "kg"}, // 1: created
	}
	for pos := 2; pos <= 6; pos++ {
		inputs = append(inputs, Input{Batch: "B1", Product: "P", Quantity: 10, Unit: "kg"})
	}
	inputs = append(inputs, Input{Batch: "B1", Product: "X", Quantity: 10, Unit: "kg"}) // 7: product conflict
	_, err := Import(reg, inputs)
	assertConflict(t, err, 7, "B1", []string{"product"}, "manifest", 1)
	if len(reg.Batches) != 0 {
		t.Fatalf("rejected manifest left new batches: %+v", reg.Batches)
	}
}

// A confirm-vs-first conflict after the new batch has already been appended
// is still an all-or-nothing rejection: every batch introduced earlier in
// the same manifest is withheld from the registry.
func TestImportConflictAfterConfirmsWithholdsEarlierCreatedBatches(t *testing.T) {
	original := []Batch{{Batch: "OLD", Product: "P0", Quantity: 1, Unit: "kg"}}
	reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), original...)}
	_, err := Import(reg, []Input{
		{Batch: "NEW1", Product: "P1", Quantity: 1, Unit: "kg"},
		{Batch: "B1", Product: "P", Quantity: 10, Unit: "kg"}, // 2: created
		{Batch: "NEW2", Product: "P2", Quantity: 1, Unit: "kg"},
		{Batch: "B1", Product: "P", Quantity: 10, Unit: "kg"}, // 4: confirmation
		{Batch: "B1", Product: "P", Quantity: 10, Unit: "m"},  // 5: unit conflict
	})
	assertConflict(t, err, 5, "B1", []string{"unit"}, "manifest", 2)
	if len(reg.Batches) != len(original) || reg.Batches[0] != original[0] {
		t.Fatalf("rejected manifest must leave only the pre-existing record: %+v", reg.Batches)
	}
}

// Each differing member is reported against the first manifest record; when
// product, quantity and unit all differ they are listed together in that
// fixed order.
func TestImportConflictAfterConfirmsReportsAllFieldsAgainstFirstRecord(t *testing.T) {
	reg := &Registry{Version: FormatVersion}
	_, err := Import(reg, []Input{
		{Batch: "B1", Product: "P", Quantity: 10, Unit: "kg"}, // 1: created
		{Batch: "B1", Product: "P", Quantity: 10, Unit: "kg"}, // 2: confirmation
		{Batch: "B1", Product: "Q", Quantity: 11, Unit: "m"},  // 3: all differ
	})
	assertConflict(t, err, 3, "B1", []string{"product", "quantity", "unit"}, "manifest", 1)
	if got := err.Error(); !strings.Contains(got, "product, quantity, unit") {
		t.Fatalf("error must list product, quantity and unit in order: %v", got)
	}
}

// When the batch was already registered before the import, the identical
// manifest records are only confirmations of that stored record — even when
// there are several of them. A later differing record must always say the
// conflict is with the registered record, never with a manifest record.
func TestImportConflictWithRegistryAfterRepeatedConfirms(t *testing.T) {
	original := []Batch{{Batch: "B1", Product: "P", Quantity: 10, Unit: "kg"}}
	reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), original...)}
	_, err := Import(reg, []Input{
		{Batch: "B1", Product: "P", Quantity: 10, Unit: "kg"}, // 1: stored duplicate
		{Batch: "OTHER", Product: "P2", Quantity: 1, Unit: "box"},
		{Batch: "B1", Product: "P", Quantity: 10, Unit: "kg"}, // 3: another confirmation
		{Batch: "B1", Product: "P", Quantity: 99, Unit: "kg"}, // 4: conflicts
	})
	assertConflict(t, err, 4, "B1", []string{"quantity"}, "registry", 0)
	if !strings.Contains(err.Error(), "conflicts with the registered record") {
		t.Fatalf("error must blame the registered record: %v", err)
	}
	if strings.Contains(err.Error(), "manifest record 1") || strings.Contains(err.Error(), "manifest record 3") {
		t.Fatalf("repeated confirmations must not turn the source into a manifest record: %v", err)
	}
	if len(reg.Batches) != len(original) || reg.Batches[0] != original[0] {
		t.Fatalf("rejected manifest mutated the registry: %+v", reg.Batches)
	}
}

// All three fields differing against the registered record, after repeated
// confirmations, still reports product, quantity and unit in order and keeps
// the registered record as the source.
func TestImportConflictWithRegistryAfterConfirmsNamesAllFields(t *testing.T) {
	original := []Batch{{Batch: "B1", Product: "P", Quantity: 10, Unit: "kg"}}
	reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), original...)}
	_, err := Import(reg, []Input{
		{Batch: "B1", Product: "P", Quantity: 10, Unit: "kg"},
		{Batch: "B1", Product: "P", Quantity: 10, Unit: "kg"},
		{Batch: "B1", Product: "Q", Quantity: 11, Unit: "m"},
	})
	assertConflict(t, err, 3, "B1", []string{"product", "quantity", "unit"}, "registry", 0)
}

// Manifest text is compared after trimming leading and trailing whitespace,
// so whitespace-padded spellings of one id all resolve to the first
// occurrence and a later conflict is blamed on it; interior characters and
// casing stay significant and introduce separate batches instead.
func TestImportManifestTrimmingDeterminesConflictSource(t *testing.T) {
	t.Run("trimmed id anchors to first occurrence", func(t *testing.T) {
		inputs, perr := ParseManifest([]byte(`[
  {"batch": " B1 ", "product": "P", "quantity": 10, "unit": "kg"},
  {"batch": "OTHER", "product": "P2", "quantity": 1, "unit": "box"},
  {"batch": " B1 ", "product": "P", "quantity": 10, "unit": "kg"},
  {"batch": "B1", "product": "P", "quantity": 99, "unit": "kg"}
]`))
		if perr != nil {
			t.Fatal(perr)
		}
		reg := &Registry{Version: FormatVersion}
		_, err := Import(reg, inputs)
		assertConflict(t, err, 4, "B1", []string{"quantity"}, "manifest", 1)
	})

	t.Run("trimmed id conflicts with registered record", func(t *testing.T) {
		inputs, perr := ParseManifest([]byte(`[
  {"batch": "  B1  ", "product": "P", "quantity": 10, "unit": "kg"},
  {"batch": "B1", "product": "P", "quantity": 99, "unit": "kg"}
]`))
		if perr != nil {
			t.Fatal(perr)
		}
		reg := &Registry{Version: FormatVersion, Batches: []Batch{
			{Batch: "B1", Product: "P", Quantity: 10, Unit: "kg"},
		}}
		_, err := Import(reg, inputs)
		assertConflict(t, err, 2, "B1", []string{"quantity"}, "registry", 0)
	})

	t.Run("casing and interior whitespace stay distinct", func(t *testing.T) {
		inputs, perr := ParseManifest([]byte(`[
  {"batch": "B 1", "product": "P", "quantity": 10, "unit": "kg"},
  {"batch": "b 1", "product": "P", "quantity": 99, "unit": "kg"},
  {"batch": "B1", "product": "P", "quantity": 1, "unit": "kg"}
]`))
		if perr != nil {
			t.Fatal(perr)
		}
		reg := &Registry{Version: FormatVersion}
		out, err := Import(reg, inputs)
		if err != nil {
			t.Fatalf("interior-whitespace and case variants are distinct batches: %v", err)
		}
		if len(out) != 3 || !out[0].Created || !out[1].Created || !out[2].Created {
			t.Fatalf("all three ids must be created separately: %+v", out)
		}
		if len(reg.Batches) != 3 {
			t.Fatalf("registry should hold three distinct batches: %+v", reg.Batches)
		}
	})
}

// Success counterpart of the conflict scenario: once the final record is
// changed back to the exact same content, the import succeeds. Results keep
// manifest order with the first occurrence "created" and every later
// identical record "duplicate" (stored records included); the batch is
// stored exactly once with its first-occurrence content.
func TestImportRepeatedConfirmsThenIdenticalSucceedsInOrder(t *testing.T) {
	original := []Batch{{Batch: "OLD", Product: "P0", Quantity: 7, Unit: "box"}}
	reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), original...)}
	inputs := []Input{
		{Batch: "B1", Product: "P", Quantity: 10, Unit: "kg"}, // 1: created
		{Batch: "OTHER", Product: "P2", Quantity: 1, Unit: "box"},
		{Batch: "B1", Product: "P", Quantity: 10, Unit: "kg"},   // 3: duplicate
		{Batch: "OLD", Product: "P0", Quantity: 7, Unit: "box"}, // 4: stored duplicate
		{Batch: "B1", Product: "P", Quantity: 10, Unit: "kg"},   // 5: identical again
	}
	out, err := Import(reg, inputs)
	if err != nil {
		t.Fatalf("an all-matching manifest must import: %v", err)
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
			t.Errorf("record %d batch = %q, want %q", i+1, out[i].Batch.Batch, inputs[i].Batch)
		}
	}
	// Stored record first, new batches appended in first-occurrence order,
	// each id present exactly once.
	wantBatches := []Batch{
		{Batch: "OLD", Product: "P0", Quantity: 7, Unit: "box"},
		{Batch: "B1", Product: "P", Quantity: 10, Unit: "kg"},
		{Batch: "OTHER", Product: "P2", Quantity: 1, Unit: "box"},
	}
	if len(reg.Batches) != len(wantBatches) {
		t.Fatalf("registry holds %d batches, want %d: %+v", len(reg.Batches), len(wantBatches), reg.Batches)
	}
	for i, want := range wantBatches {
		if reg.Batches[i] != want {
			t.Errorf("stored batch %d = %+v, want %+v", i+1, reg.Batches[i], want)
		}
	}
}

// A brand-new batch confirmed identically several times, including a
// pre-registered id interleaved between confirmations, is still stored once
// and every confirmation beyond the first is "duplicate".
func TestImportNewBatchConfirmedSeveralTimesStoredOnce(t *testing.T) {
	reg := &Registry{Version: FormatVersion, Batches: []Batch{
		{Batch: "OLD", Product: "P0", Quantity: 7, Unit: "box"},
	}}
	out, err := Import(reg, []Input{
		{Batch: "B1", Product: "P", Quantity: 10, Unit: "kg"},
		{Batch: "B1", Product: "P", Quantity: 10, Unit: "kg"},
		{Batch: "OLD", Product: "P0", Quantity: 7, Unit: "box"},
		{Batch: "B1", Product: "P", Quantity: 10, Unit: "kg"},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantCreated := []bool{true, false, false, false}
	for i, want := range wantCreated {
		if out[i].Created != want {
			t.Errorf("record %d created=%v, want %v", i+1, out[i].Created, want)
		}
	}
	count := 0
	for _, b := range reg.Batches {
		if b.Batch == "B1" {
			count++
			if b != (Batch{Batch: "B1", Product: "P", Quantity: 10, Unit: "kg"}) {
				t.Errorf("B1 stored with changed content: %+v", b)
			}
		}
	}
	if count != 1 {
		t.Fatalf("B1 stored %d times, want exactly once: %+v", count, reg.Batches)
	}
}
