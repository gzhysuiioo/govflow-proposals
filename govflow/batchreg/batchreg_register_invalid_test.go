package batchreg

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"
)

// This file is the regression net for the single-batch Register entry point
// as reached directly through the Go API: the caller hands in already-built
// text and an already-decoded int64 (the command line normalizes text and
// parses the quantity before constructing the Input), so Register itself must
// refuse invalid input ahead of the duplicate/conflict lookup — whatever the
// batch id's registration status.
//
// The error precedence pinned here is Register's own and must not drift toward
// the manifest rule (Import reports encoding before empty text): any empty
// text field first, then encoding in batch/product/unit order, then quantity.

// copyBatches snapshots the stored records so a test can prove a rejected call
// left every field, the count and the order exactly as they were.
func copyBatches(reg *Registry) []Batch {
	return append([]Batch(nil), reg.Batches...)
}

func assertBatchesIdentical(t *testing.T, got, want []Batch) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("record count changed: got %d (%+v), want %d (%+v)", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("record %d changed:\n got %+v\nwant %+v\nfull registry: %+v", i+1, got[i], want[i], got)
		}
	}
}

// assertRejectedBeforeLookup is the shared contract for every invalid input:
// the zero Outcome (no carried record, never Created), an error that is neither
// a duplicate confirmation nor a conflict, and a registry left byte-for-byte
// equivalent in content, count and order.
func assertRejectedBeforeLookup(t *testing.T, reg *Registry, before []Batch, out Outcome, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("invalid input must be rejected, got outcome %+v", out)
	}
	if out.Created {
		t.Fatalf("a rejection must not report a new record, got %+v", out.Batch)
	}
	if out.Batch != (Batch{}) {
		t.Fatalf("a rejection must carry no registered record, got %+v", out.Batch)
	}
	var ce *ConflictError
	if errors.As(err, &ce) {
		t.Fatalf("invalid input must be reported as invalid, not as an existing-batch conflict: %v", err)
	}
	assertBatchesIdentical(t, reg.Batches, before)
}

// Invalid input is rejected before the create/duplicate/conflict branch whether
// the batch id has never been seen or already belongs to a stored record — the
// validity check cannot be skipped by reaching a different lookup outcome.
func TestRegisterRejectsInvalidInputBeforeLookup(t *testing.T) {
	badText := string([]byte{'P', 0xff}) // malformed UTF-8
	valid := func() Input {
		return Input{Batch: "B1", Product: "P-7", Quantity: 10, Unit: "kg"}
	}

	cases := []struct {
		name     string
		mutate   func(*Input)
		classify func(t *testing.T, err error)
	}{
		{"empty batch", func(in *Input) { in.Batch = "" }, assertEmptyTextError},
		{"empty product", func(in *Input) { in.Product = "" }, assertEmptyTextError},
		{"empty unit", func(in *Input) { in.Unit = "" }, assertEmptyTextError},
		{"malformed utf8 batch", func(in *Input) { in.Batch = "B" + badText }, makeAssertEncodingError("batch")},
		{"malformed utf8 product", func(in *Input) { in.Product = badText }, makeAssertEncodingError("product")},
		{"malformed utf8 unit", func(in *Input) { in.Unit = "kg" + badText }, makeAssertEncodingError("unit")},
		{"quantity zero", func(in *Input) { in.Quantity = 0 }, assertQuantityError},
		{"quantity negative", func(in *Input) { in.Quantity = -1 }, assertQuantityError},
		{"quantity minimum int64", func(in *Input) { in.Quantity = math.MinInt64 }, assertQuantityError},
	}

	scenarios := []struct {
		name    string
		reg     []Batch
		prepare func(*Input) // align the bad record with the stored id before mutation
	}{
		{
			name:    "id not registered yet",
			reg:     nil,
			prepare: func(in *Input) {},
		},
		{
			name: "id already registered",
			reg:  []Batch{{Batch: "B1", Product: "P-7", Quantity: 10, Unit: "kg"}},
			// The record starts identical to the stored one; the mutation then
			// makes it invalid, so a lookup-only implementation would otherwise
			// call it a conflict (or, for an emptied field, compare on garbage).
			prepare: func(in *Input) {},
		},
		{
			name: "other batches present must be unaffected",
			reg: []Batch{
				{Batch: "A0", Product: "PA", Quantity: 1, Unit: "box"},
				{Batch: "B1", Product: "P-7", Quantity: 10, Unit: "kg"},
				{Batch: "C9", Product: "PC", Quantity: 7, Unit: "m"},
			},
			prepare: func(in *Input) {},
		},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), sc.reg...)}
					before := copyBatches(reg)
					in := valid()
					sc.prepare(&in)
					tc.mutate(&in)

					out, err := Register(reg, in)
					assertRejectedBeforeLookup(t, reg, before, out, err)
					tc.classify(t, err)
				})
			}
		})
	}
}

// The exact motivating case: B1 already holds quantity 10; re-submitting B1
// with an empty product and quantity 11 must report invalid input — never a
// conflict merely because the quantity differs — and must not overwrite the
// stored product or quantity.
func TestRegisterEmptyProductOnExistingBatchOutranksQuantityConflict(t *testing.T) {
	existing := []Batch{
		{Batch: "A0", Product: "PA", Quantity: 4, Unit: "box"},
		{Batch: "B1", Product: "P-7", Quantity: 10, Unit: "kg"},
	}
	reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), existing...)}

	out, err := Register(reg, Input{Batch: "B1", Product: "", Quantity: 11, Unit: "kg"})
	assertRejectedBeforeLookup(t, reg, existing, out, err)
	assertEmptyTextError(t, err)

	// Explicitly pin the stored B1 record against an overwrite.
	if got := reg.Batches[1]; got != existing[1] {
		t.Fatalf("stored B1 was overwritten: got %+v, want %+v", got, existing[1])
	}
}

// When one record carries several problems Register keeps its own order: an
// empty text field wins over everything (even a malformed field that comes
// first in batch/product/unit order); with no empty field, encoding beats
// quantity; among several malformed text fields the first in batch, product,
// unit order is named. The manifest ordering (encoding before empty) must not
// leak in here.
func TestRegisterInvalidInputErrorPrecedence(t *testing.T) {
	bad := string([]byte{0xff})
	cases := []struct {
		name string
		in   Input
		want func(t *testing.T, err error)
	}{
		// Any empty field outranks any encoding and any quantity problem.
		{"empty product with malformed unit and zero quantity",
			Input{Batch: "B1", Product: "", Quantity: 0, Unit: "kg" + bad}, assertEmptyTextError},
		{"empty unit despite malformed batch",
			Input{Batch: "B" + bad, Product: "P", Quantity: 1, Unit: ""}, assertEmptyTextError},
		{"empty product despite malformed batch",
			Input{Batch: "B" + bad, Product: "", Quantity: -9, Unit: "kg"}, assertEmptyTextError},
		{"all three text fields empty",
			Input{Batch: "", Product: "", Quantity: 0, Unit: ""}, assertEmptyTextError},

		// No empty field: encoding is reported before quantity, field order
		// batch, product, unit deciding which encoding error is named.
		{"malformed product outranks zero quantity",
			Input{Batch: "B1", Product: "P" + bad, Quantity: 0, Unit: "kg"}, makeAssertEncodingError("product")},
		{"malformed batch outranks malformed product",
			Input{Batch: "B" + bad, Product: "P" + bad, Quantity: 1, Unit: "kg"}, makeAssertEncodingError("batch")},
		{"malformed batch outranks malformed unit and negative quantity",
			Input{Batch: "B" + bad, Product: "P", Quantity: -1, Unit: "u" + bad}, makeAssertEncodingError("batch")},
		{"malformed product outranks malformed unit",
			Input{Batch: "B1", Product: "P" + bad, Quantity: 1, Unit: "u" + bad}, makeAssertEncodingError("product")},
		{"all three text fields malformed names batch",
			Input{Batch: "b" + bad, Product: "p" + bad, Quantity: 1, Unit: "u" + bad}, makeAssertEncodingError("batch")},

		// Only a quantity problem: text is present and valid.
		{"zero quantity on otherwise valid record",
			Input{Batch: "B1", Product: "P", Quantity: 0, Unit: "kg"}, assertQuantityError},
		{"negative quantity on otherwise valid record",
			Input{Batch: "B1", Product: "P", Quantity: -42, Unit: "kg"}, assertQuantityError},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Run each against both an empty registry and one holding B1, so a
			// future change that validates only on the create branch is caught.
			for _, existing := range [][]Batch{
				nil,
				{{Batch: "B1", Product: "OLD", Quantity: 123, Unit: "old"}},
			} {
				reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), existing...)}
				before := copyBatches(reg)
				out, err := Register(reg, tc.in)
				assertRejectedBeforeLookup(t, reg, before, out, err)
				tc.want(t, err)
			}
		})
	}
}

// An empty registry stays empty after any rejection; with records present the
// whole slice — fields, count and order — is preserved and no id is affected.
func TestRegisterRejectionsLeaveRegistryUntouched(t *testing.T) {
	t.Run("empty registry stays empty", func(t *testing.T) {
		reg := &Registry{Version: FormatVersion}
		invalid := []Input{
			{Batch: "", Product: "P", Quantity: 1, Unit: "kg"},
			{Batch: "B", Product: "P", Quantity: 0, Unit: "kg"},
			{Batch: "B", Product: "P" + string([]byte{0xff}), Quantity: 1, Unit: "kg"},
			{Batch: "B", Product: "P", Quantity: -1, Unit: ""},
		}
		for i, in := range invalid {
			out, err := Register(reg, in)
			if err == nil {
				t.Fatalf("case %d must be rejected", i)
			}
			if out.Created || len(reg.Batches) != 0 {
				t.Fatalf("case %d changed the empty registry: out=%+v batches=%+v", i, out, reg.Batches)
			}
		}
	})

	t.Run("populated registry keeps fields count and order", func(t *testing.T) {
		original := []Batch{
			{Batch: "B1", Product: "P-7", Quantity: 10, Unit: "kg"},
			{Batch: "B2", Product: "P-8", Quantity: 20, Unit: "box"},
		}
		// Each case targets either the existing id B1 or a new id, but is
		// invalid; none of these may overwrite B1, reorder B2 or append.
		cases := []struct {
			name string
			in   Input
		}{
			{"empty product differing quantity", Input{Batch: "B1", Product: "", Quantity: 11, Unit: "kg"}},
			{"zero quantity", Input{Batch: "B1", Product: "P-7", Quantity: 0, Unit: "kg"}},
			{"malformed product differing quantity", Input{Batch: "B1", Product: "P" + string([]byte{0xff}), Quantity: 11, Unit: "kg"}},
			{"empty unit", Input{Batch: "B1", Product: "P-7", Quantity: 11, Unit: ""}},
			{"negative quantity new id", Input{Batch: "B3", Product: "P", Quantity: -5, Unit: "kg"}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), original...)}
				out, err := Register(reg, tc.in)
				assertRejectedBeforeLookup(t, reg, original, out, err)
			})
		}
	})
}

// Direct callers submit already-built text: it is not trimmed and not case
// folded, unlike the command-line path. Only the empty string is "empty";
// whitespace-only text is an ordinary non-empty value and registers.
func TestRegisterKeepsDirectTextVerbatim(t *testing.T) {
	reg := &Registry{Version: FormatVersion}

	in := Input{Batch: " B1 ", Product: "\tP 7\n", Quantity: 1, Unit: " kg "}
	out, err := Register(reg, in)
	if err != nil {
		t.Fatalf("leading/trailing/interior whitespace is ordinary text: %v", err)
	}
	if !out.Created || out.Batch != (Batch{Batch: " B1 ", Product: "\tP 7\n", Quantity: 1, Unit: " kg "}) {
		t.Fatalf("text must be stored verbatim: %+v", out)
	}

	// The identical verbatim record confirms a duplicate; trimming it mentally
	// to "B1"/"P 7"/"kg" must not happen.
	if out, err := Register(reg, in); err != nil || out.Created {
		t.Fatalf("verbatim repeat must be a duplicate: out=%+v err=%v", out, err)
	}

	// Whitespace-only values are non-empty on the direct path and create a
	// distinct batch rather than failing the non-empty rule.
	ws := Input{Batch: " ", Product: "\t", Quantity: 1, Unit: "\n"}
	if out, err := Register(reg, ws); err != nil || !out.Created {
		t.Fatalf("whitespace-only text must be accepted as non-empty: out=%+v err=%v", out, err)
	}
	if got := reg.Batches[1]; got != (Batch{Batch: " ", Product: "\t", Quantity: 1, Unit: "\n"}) {
		t.Fatalf("whitespace-only text not kept verbatim: %+v", got)
	}

	// Chinese text, emoji and a genuinely entered U+FFFD are all legal text.
	for _, in := range []Input{
		{Batch: "批次-1", Product: "产品😀", Quantity: 1, Unit: "千克"},
		{Batch: "B" + string(rune(0xfffd)), Product: "P", Quantity: 2, Unit: "kg"},
	} {
		if out, err := Register(reg, in); err != nil || !out.Created {
			t.Fatalf("legal Unicode %+v must register: out=%+v err=%v", in, out, err)
		}
	}
	if len(reg.Batches) != 4 {
		t.Fatalf("unexpected stored records: %+v", reg.Batches)
	}
}

// The quantity window stays usable at both inclusive bounds through the direct
// API, and an exactly identical stored record still comes back as a duplicate
// rather than a create.
func TestRegisterQuantityBoundsAndDuplicateStillSucceed(t *testing.T) {
	reg := &Registry{Version: FormatVersion}

	lo := Input{Batch: "LO", Product: "P", Quantity: 1, Unit: "kg"}
	if out, err := Register(reg, lo); err != nil || !out.Created {
		t.Fatalf("quantity 1 must register: out=%+v err=%v", out, err)
	}
	hi := Input{Batch: "HI", Product: "P", Quantity: MaxQuantity, Unit: "kg"}
	if out, err := Register(reg, hi); err != nil || !out.Created || out.Batch.Quantity != MaxQuantity {
		t.Fatalf("quantity %d must register: out=%+v err=%v", MaxQuantity, out, err)
	}

	// Exactly identical repeats are duplicate confirmations and add nothing.
	for _, in := range []Input{lo, hi} {
		before := len(reg.Batches)
		out, err := Register(reg, in)
		if err != nil {
			t.Fatalf("identical repeat must not error: %v", err)
		}
		if out.Created || out.Batch.Quantity != in.Quantity {
			t.Fatalf("identical repeat must be a duplicate: %+v", out)
		}
		if len(reg.Batches) != before {
			t.Fatalf("duplicate confirmation changed the record count: %+v", reg.Batches)
		}
	}
}

// --- error-shape assertions -------------------------------------------------

func assertEmptyTextError(t *testing.T, err error) {
	t.Helper()
	var ee *EncodingError
	if errors.As(err, &ee) {
		t.Fatalf("empty text must not be reported as an encoding error: %v", err)
	}
	const want = "batch, product and unit must be non-empty"
	if err.Error() != want {
		t.Fatalf("empty-text error = %q, want %q", err.Error(), want)
	}
}

func makeAssertEncodingError(wantField string) func(*testing.T, error) {
	return func(t *testing.T, err error) {
		t.Helper()
		var ee *EncodingError
		if !errors.As(err, &ee) {
			t.Fatalf("got %v, want EncodingError on field %q", err, wantField)
		}
		if ee.Field != wantField {
			t.Fatalf("EncodingError field = %q, want %q (error: %v)", ee.Field, wantField, err)
		}
		if !strings.Contains(err.Error(), strconv.Quote(wantField)) {
			t.Fatalf("encoding error must name field %q: %v", wantField, err)
		}
		if !strings.Contains(err.Error(), "UTF-8") {
			t.Fatalf("encoding error must explain the encoding problem: %v", err)
		}
	}
}

func assertQuantityError(t *testing.T, err error) {
	t.Helper()
	var ee *EncodingError
	if errors.As(err, &ee) {
		t.Fatalf("a quantity problem must not be reported as a text encoding error: %v", err)
	}
	msg := err.Error()
	// Register's wording pins the accepted positive-integer window: a positive
	// integer (so zero and negatives are out) no greater than MaxQuantity.
	for _, want := range []string{"quantity", "positive integer", strconv.FormatInt(MaxQuantity, 10)} {
		if !strings.Contains(msg, want) {
			t.Fatalf("quantity error %q must state the allowed range and mention %q", msg, want)
		}
	}
}
