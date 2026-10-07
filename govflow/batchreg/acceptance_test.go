package batchreg

import (
	"reflect"
	"testing"
)

// The shared acceptance rule both entry points run: a new id is appended and
// created; an identical repeat echoes the record ALREADY held (never the
// submitted copy) and is not a create; a differing repeat is rejected without
// changing the table, listing every mismatch in product, quantity, unit order.
func TestBatchTableAcceptRule(t *testing.T) {
	stored := []Batch{{Batch: "B0", Product: "P-0", Quantity: 5, Unit: "box"}}
	table := newBatchTable(stored)

	// New id: appended after the stored record, created.
	first := Input{Batch: "B1", Product: "P-7", Quantity: 10, Unit: "kg"}
	b, created, diffs := table.accept(first, 1)
	if !created || len(diffs) != 0 {
		t.Fatalf("new id: created=%v diffs=%v, want created with no diffs", created, diffs)
	}
	if b != (Batch{Batch: "B1", Product: "P-7", Quantity: 10, Unit: "kg"}) {
		t.Fatalf("created record = %+v", b)
	}

	// Identical repeat: the record echoed is the one already held; the table
	// neither grows nor moves the stored record.
	repeat := Input{Batch: "B1", Product: "P-7", Quantity: 10, Unit: "kg"}
	b, created, diffs = table.accept(repeat, 3)
	if created || len(diffs) != 0 {
		t.Fatalf("identical repeat: created=%v diffs=%v, want duplicate with no diffs", created, diffs)
	}
	if b != table.records[1] {
		t.Fatalf("duplicate must echo the held record %+v, got %+v", table.records[1], b)
	}

	// Conflicting repeat: all three fields, in fixed order; nothing is added.
	bad := Input{Batch: "B1", Product: "P-9", Quantity: 11, Unit: "g"}
	_, created, diffs = table.accept(bad, 5)
	if created {
		t.Fatalf("conflict must not be a create")
	}
	if want := []string{"product", "quantity", "unit"}; !reflect.DeepEqual(diffs, want) {
		t.Fatalf("diffs = %v, want %v", diffs, want)
	}

	// The rejected acceptance left the table exactly as it was: B0 then B1,
	// and the held B1 still carries its original values (never overwritten).
	wantRecords := []Batch{
		{Batch: "B0", Product: "P-0", Quantity: 5, Unit: "box"},
		{Batch: "B1", Product: "P-7", Quantity: 10, Unit: "kg"},
	}
	if !reflect.DeepEqual(table.records, wantRecords) {
		t.Fatalf("table after rejection = %+v, want %+v", table.records, wantRecords)
	}

	// A conflict against a stored id keeps the registry as its source even
	// after identical confirmations; a conflict against an id this run
	// introduced points at its FIRST occurrence, not an intermediate repeat.
	regTable := newBatchTable(wantRecords)
	for p, in := range []Input{
		{Batch: "B0", Product: "P-0", Quantity: 5, Unit: "box"}, // confirms stored
		{Batch: "B0", Product: "P-0", Quantity: 6, Unit: "box"}, // conflicts
	} {
		_, _, d := regTable.accept(in, p+1)
		if p == 0 && len(d) != 0 {
			t.Fatalf("stored confirmation diffs = %v, want none", d)
		}
		if p == 1 {
			if inManifest, _ := regTable.conflictSource("B0"); inManifest {
				t.Fatal("conflict with a stored id must keep the registry as its source after confirmations")
			}
			if !reflect.DeepEqual(d, []string{"quantity"}) {
				t.Fatalf("stored conflict diffs = %v, want [quantity]", d)
			}
		}
	}
	if inManifest, prev := table.conflictSource("B1"); !inManifest || prev != 1 {
		t.Fatalf("manifest source = (%v, %d), want (true, 1): the first occurrence is the source", inManifest, prev)
	}
	if inManifest, _ := table.conflictSource("B0"); inManifest {
		t.Fatal("an id present before the run must not be attributed to the manifest")
	}
}

// Ids compare byte for byte through the shared rule: casing and interior
// whitespace distinguish records, so padded or differently-cased spellings
// are separate batches rather than duplicates or conflicts.
func TestBatchTableIDsAreByteForByte(t *testing.T) {
	table := newBatchTable(nil)
	table.accept(Input{Batch: "B1", Product: "P", Quantity: 1, Unit: "u"}, 1)
	for i, in := range []Input{
		{Batch: "b1", Product: "P", Quantity: 1, Unit: "u"},  // different case
		{Batch: "B 1", Product: "P", Quantity: 1, Unit: "u"}, // interior space
	} {
		_, created, diffs := table.accept(in, i+2)
		if !created || len(diffs) != 0 {
			t.Fatalf("id %q must be a separate new batch, got created=%v diffs=%v", in.Batch, created, diffs)
		}
	}
	if got := len(table.records); got != 3 {
		t.Fatalf("table holds %d records, want 3 distinct ids", got)
	}
}
