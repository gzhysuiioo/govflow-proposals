package batchreg

import (
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// Regression coverage for the single-batch Register entry point as a Go API:
// callers hand it an already constructed Input (text and int64), so unlike
// the command line there is no whitespace trimming or decimal parsing on the
// way in. Invalid input must be refused BEFORE the duplicate/conflict lookup,
// with Register's own error precedence — not the manifest import's — and the
// registry must come out byte-for-byte (field-for-field, count, order)
// unchanged.

// badUTF8 is text that is not valid UTF-8 (unlike a genuine U+FFFD, which is
// ordinary text).
var badUTF8 = string([]byte{'x', 0xff})

// registerGuardSeed is one starting registry against which every invalid
// record is attempted. want is the exact Batches content the rejection must
// leave behind.
type registerGuardSeed struct {
	name string
	reg  *Registry
	want []Batch
}

func registerGuardSeeds() []registerGuardSeed {
	b1 := Batch{Batch: "B1", Product: "P-1", Quantity: 10, Unit: "kg"}
	b2 := Batch{Batch: "B2", Product: "P-2", Quantity: 20, Unit: "box"}
	return []registerGuardSeed{
		{"empty registry", &Registry{Version: FormatVersion}, nil},
		{"other batches only", &Registry{Version: FormatVersion, Batches: []Batch{b2}}, []Batch{b2}},
		{"batch id already registered", &Registry{Version: FormatVersion, Batches: []Batch{b1, b2}}, []Batch{b1, b2}},
	}
}

// rejectKind says which rejection an invalid record must produce.
type rejectKind int

const (
	rejectEmpty rejectKind = iota
	rejectEncoding
	rejectQuantity
)

// TestRegisterRejectsInvalidInputBeforeLookup drives every invalid record
// against an unknown batch id AND against the already-registered B1: whether
// the id is new or stored, invalid input is the answer — never a conflict,
// never a duplicate, never a created record — even when a stored field (e.g.
// quantity) differs.
func TestRegisterRejectsInvalidInputBeforeLookup(t *testing.T) {
	cases := []struct {
		name  string
		in    Input
		kind  rejectKind
		field string // expected text field for an EncodingError
	}{
		// Empty text: the empty string alone is "empty"; whitespace-only text
		// is legal and therefore does not appear here.
		{"empty product on new id", Input{Batch: "B-NEW", Product: "", Quantity: 11, Unit: "kg"}, rejectEmpty, ""},
		{"empty batch", Input{Batch: "", Product: "P-1", Quantity: 10, Unit: "kg"}, rejectEmpty, ""},
		{"empty unit on new id", Input{Batch: "B-NEW", Product: "P-7", Quantity: 11, Unit: ""}, rejectEmpty, ""},
		{"several empty fields", Input{Batch: "B-NEW", Product: "", Quantity: 11, Unit: ""}, rejectEmpty, ""},
		{"all text empty", Input{Batch: "", Product: "", Quantity: -5, Unit: ""}, rejectEmpty, ""},
		// The headline scenario: B1 exists with quantity 10; resubmitting B1
		// with an empty product and quantity 11 is invalid input — the
		// quantity difference must not turn it into a batch conflict first.
		{"existing id with empty product and differing quantity", Input{Batch: "B1", Product: "", Quantity: 11, Unit: "kg"}, rejectEmpty, ""},
		{"existing id with empty unit", Input{Batch: "B1", Product: "P-1", Quantity: 10, Unit: ""}, rejectEmpty, ""},

		// Malformed UTF-8 bytes.
		{"bad batch encoding on new id", Input{Batch: badUTF8, Product: "P-7", Quantity: 11, Unit: "kg"}, rejectEncoding, "batch"},
		{"bad product encoding on new id", Input{Batch: "B-NEW", Product: badUTF8, Quantity: 11, Unit: "kg"}, rejectEncoding, "product"},
		{"bad unit encoding on new id", Input{Batch: "B-NEW", Product: "P-7", Quantity: 11, Unit: badUTF8}, rejectEncoding, "unit"},
		{"bad product encoding on existing id", Input{Batch: "B1", Product: badUTF8, Quantity: 11, Unit: "kg"}, rejectEncoding, "product"},
		{"bad unit encoding on existing id", Input{Batch: "B1", Product: "P-1", Quantity: 11, Unit: badUTF8}, rejectEncoding, "unit"},

		// Quantity outside 1..MaxQuantity.
		{"zero quantity on new id", Input{Batch: "B-NEW", Product: "P-7", Quantity: 0, Unit: "kg"}, rejectQuantity, ""},
		{"negative quantity on new id", Input{Batch: "B-NEW", Product: "P-7", Quantity: -1, Unit: "kg"}, rejectQuantity, ""},
		{"most negative quantity", Input{Batch: "B-NEW", Product: "P-7", Quantity: -1 << 63, Unit: "kg"}, rejectQuantity, ""},
		{"zero quantity on existing id", Input{Batch: "B1", Product: "P-1", Quantity: 0, Unit: "kg"}, rejectQuantity, ""},
		{"negative quantity on existing id", Input{Batch: "B1", Product: "P-1", Quantity: -7, Unit: "kg"}, rejectQuantity, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, seed := range registerGuardSeeds() {
				t.Run(seed.name, func(t *testing.T) {
					before := append([]Batch(nil), seed.reg.Batches...)
					out, err := Register(seed.reg, tc.in)
					if err == nil {
						t.Fatalf("expected rejection, got outcome %+v", out)
					}
					assertRegisterRejection(t, err, tc.kind, tc.field)
					if out != (Outcome{}) || out.Created {
						t.Fatalf("rejection must return the zero Outcome, got %+v", out)
					}
					if !reflect.DeepEqual(seed.reg.Batches, seed.want) {
						t.Fatalf("registry changed after rejection:\nbefore %+v\nafter  %+v", before, seed.reg.Batches)
					}
					if len(seed.reg.Batches) != len(seed.want) {
						t.Fatalf("record count changed: got %d, want %d", len(seed.reg.Batches), len(seed.want))
					}
				})
			}
		})
	}
}

// TestRegisterExistingB1ExampleNotConflict locks the exact scenario from the
// requirement: B1 is stored as P-1/10/kg; B1 + empty product + quantity 11
// must be reported as invalid input. It must surface neither the quantity
// conflict nor any overwrite of the stored product or quantity.
func TestRegisterExistingB1ExampleNotConflict(t *testing.T) {
	stored := []Batch{
		{Batch: "B1", Product: "P-1", Quantity: 10, Unit: "kg"},
		{Batch: "B2", Product: "P-2", Quantity: 20, Unit: "box"},
	}
	reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), stored...)}

	out, err := Register(reg, Input{Batch: "B1", Product: "", Quantity: 11, Unit: "kg"})
	if err == nil {
		t.Fatalf("expected rejection, got %+v", out)
	}
	if msg := err.Error(); !strings.Contains(msg, "non-empty") {
		t.Fatalf("must report the empty text problem, got %v", err)
	}
	var ce *ConflictError
	if errors.As(err, &ce) {
		t.Fatalf("must not be reported as a batch conflict: %+v", ce)
	}
	var ee *EncodingError
	if errors.As(err, &ee) {
		t.Fatalf("must not be reported as an encoding problem: %+v", ee)
	}
	if out.Created || out.Batch != (Batch{}) {
		t.Fatalf("outcome must be the zero value, got %+v", out)
	}
	want := append([]Batch(nil), stored...)
	if !reflect.DeepEqual(reg.Batches, want) {
		t.Fatalf("stored records must be untouched:\nwant %+v\ngot  %+v", want, reg.Batches)
	}
	if reg.Batches[0].Product != "P-1" || reg.Batches[0].Quantity != 10 {
		t.Fatalf("B1 was overwritten: %+v", reg.Batches[0])
	}
}

// TestRegisterInvalidInputErrorOrder locks Register's own precedence for a
// record with several problems:
//
//  1. any empty text field outranks everything (the message covers all three
//     text values);
//  2. with no empty field, an encoding problem outranks a quantity problem;
//  3. with several malformed text fields, batch is named before product,
//     product before unit;
//  4. a quantity problem explains the accepted positive-integer range.
func TestRegisterInvalidInputErrorOrder(t *testing.T) {
	// 1. Empty text outranks bad encoding AND a bad quantity at once.
	in := Input{Batch: badUTF8, Product: "", Quantity: 0, Unit: ""}
	_, err := Register(&Registry{Version: FormatVersion}, in)
	if err == nil {
		t.Fatal("expected rejection")
	}
	if msg := err.Error(); msg != "batch, product and unit must be non-empty" {
		t.Fatalf("empty fields must produce the shared non-empty message naming all three values, got %q", msg)
	}
	var ee *EncodingError
	if errors.As(err, &ee) {
		t.Fatalf("empty text must outrank encoding: got %+v", ee)
	}

	// 2. Encoding outranks quantity: unit is malformed and quantity is zero.
	_, err = Register(&Registry{Version: FormatVersion},
		Input{Batch: "B9", Product: "P", Quantity: 0, Unit: badUTF8})
	if !assertEncodingError(t, err, "unit") {
		t.Fatalf("encoding must outrank quantity, got %v", err)
	}

	// 3. The first malformed field in batch, product, unit order is named.
	encodingOrder := []struct {
		name  string
		in    Input
		field string
	}{
		{"batch before product and unit", Input{Batch: badUTF8, Product: badUTF8, Quantity: 1, Unit: badUTF8}, "batch"},
		{"product before unit", Input{Batch: "B9", Product: badUTF8, Quantity: 1, Unit: badUTF8}, "product"},
		{"unit alone", Input{Batch: "B9", Product: "P", Quantity: 1, Unit: badUTF8}, "unit"},
	}
	for _, tc := range encodingOrder {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Register(&Registry{Version: FormatVersion}, tc.in)
			assertEncodingError(t, err, tc.field)
		})
	}

	// 4. A pure quantity problem explains the positive-integer window.
	for _, q := range []int64{0, -1, -1 << 63} {
		_, qerr := Register(&Registry{Version: FormatVersion},
			Input{Batch: "B9", Product: "P", Quantity: q, Unit: "kg"})
		if qerr == nil {
			t.Fatalf("quantity %d must be rejected", q)
		}
		msg := qerr.Error()
		if !strings.Contains(msg, "positive integer") || !strings.Contains(msg, strconv.FormatInt(MaxQuantity, 10)) {
			t.Fatalf("quantity error must state the accepted positive-integer range, got %q", msg)
		}
		if errors.As(qerr, &ee) {
			t.Fatalf("a quantity problem must not be an EncodingError: %v", qerr)
		}
	}
}

// TestRegisterErrorOrderingIsNotTheManifestOrdering guards against copying
// the manifest import's precedence onto single registration: Import reports
// an encoding problem before an empty-text one; Register reports empty text
// first for the very same Input.
func TestRegisterErrorOrderingIsNotTheManifestOrdering(t *testing.T) {
	in := Input{Batch: badUTF8, Product: "", Quantity: 0, Unit: "kg"}

	_, regErr := Register(&Registry{Version: FormatVersion}, in)
	if regErr == nil || !strings.Contains(regErr.Error(), "must be non-empty") {
		t.Fatalf("Register must report empty text first, got %v", regErr)
	}

	manErr := validateManifestInput(in, 1)
	var re *ManifestRecordError
	if !errors.As(manErr, &re) {
		t.Fatalf("manifest validation must reject too, got %v", manErr)
	}
	if !strings.Contains(re.Reason, "batch") || !strings.Contains(re.Reason, "UTF-8") {
		t.Fatalf("manifest ordering reports encoding first and must name batch/UTF-8, got %q", re.Reason)
	}
}

// TestRegisterKeepsDirectCallTextVerbatim pins direct-call semantics: no
// trimming happens, the empty string alone counts as empty, whitespace-only
// text is an ordinary non-empty value, and Chinese, emoji and a genuinely
// entered U+FFFD are all legal text.
func TestRegisterKeepsDirectCallTextVerbatim(t *testing.T) {
	reg := &Registry{Version: FormatVersion}

	padded := Input{Batch: "  B1  ", Product: "\tP 7\n", Quantity: 1, Unit: " 千克 "}
	out, err := Register(reg, padded)
	if err != nil {
		t.Fatalf("padded legal text must register verbatim: %v", err)
	}
	if !out.Created || out.Batch != (Batch{Batch: "  B1  ", Product: "\tP 7\n", Quantity: 1, Unit: " 千克 "}) {
		t.Fatalf("leading/trailing whitespace must be kept, got %+v", out)
	}

	// The exact same bytes confirm a duplicate; trimming is not applied.
	out, err = Register(reg, padded)
	if err != nil {
		t.Fatalf("identical padded record must be a duplicate: %v", err)
	}
	if out.Created {
		t.Fatalf("padded record must confirm a duplicate, got %+v", out)
	}

	// Whitespace-only values are non-empty text and register as their own
	// record; only the empty string is rejected as empty.
	blank := Input{Batch: " ", Product: "\t\n", Quantity: 1, Unit: " "}
	if _, err := Register(reg, blank); err != nil {
		t.Fatalf("whitespace-only text is non-empty for a direct call: %v", err)
	}
	if _, err := Register(reg, Input{Batch: "", Product: "", Quantity: 1, Unit: ""}); err == nil {
		t.Fatal("the empty string must still be rejected")
	}

	// Chinese, emoji and a genuine replacement character are all legal.
	fffd := string(rune(0xfffd))
	for _, in := range []Input{
		{Batch: "批次-1", Product: "产品甲", Quantity: 1, Unit: "千克"},
		{Batch: "B-😀", Product: "P😀", Quantity: 2, Unit: "箱"},
		{Batch: "B" + fffd, Product: "P" + fffd, Quantity: 3, Unit: fffd},
	} {
		if _, err := Register(reg, in); err != nil {
			t.Fatalf("legal Unicode %+v must register: %v", in, err)
		}
	}

	// The padded spelling is a different batch id from the trimmed spelling:
	// direct calls never collapse "  B1  " onto "B1".
	trimmed := Input{Batch: "B1", Product: "P 7", Quantity: 1, Unit: "千克"}
	if _, err := Register(reg, trimmed); err != nil {
		t.Fatalf("the untrimmed spelling must be its own batch id: %v", err)
	}
	if len(reg.Batches) != 6 {
		t.Fatalf("expected 6 verbatim records, got %d: %+v", len(reg.Batches), reg.Batches)
	}
}

// TestRegisterValidQuantityBoundariesAndDuplicate pins the success behavior
// the rejection paths must not disturb: both ends of the quantity window
// register, an identical stored record comes back as a non-created duplicate,
// and record order is append-only.
func TestRegisterValidQuantityBoundariesAndDuplicate(t *testing.T) {
	reg := &Registry{Version: FormatVersion}

	first := Input{Batch: "B1", Product: "P-1", Quantity: MaxQuantity, Unit: "kg"}
	out, err := Register(reg, first)
	if err != nil || !out.Created || out.Batch.Quantity != MaxQuantity {
		t.Fatalf("MaxQuantity must register: out=%+v err=%v", out, err)
	}
	out, err = Register(reg, first)
	if err != nil {
		t.Fatalf("the identical MaxQuantity record must confirm a duplicate: %v", err)
	}
	if out.Created || out.Batch.Quantity != MaxQuantity {
		t.Fatalf("want duplicate of MaxQuantity, got %+v", out)
	}

	lowest := Input{Batch: "B2", Product: "P-2", Quantity: 1, Unit: "box"}
	out, err = Register(reg, lowest)
	if err != nil || !out.Created || out.Batch.Quantity != 1 {
		t.Fatalf("quantity 1 must register: out=%+v err=%v", out, err)
	}
	out, err = Register(reg, lowest)
	if err != nil || out.Created {
		t.Fatalf("the identical quantity-1 record must be a duplicate: out=%+v err=%v", out, err)
	}

	want := []Batch{
		{Batch: "B1", Product: "P-1", Quantity: MaxQuantity, Unit: "kg"},
		{Batch: "B2", Product: "P-2", Quantity: 1, Unit: "box"},
	}
	if !reflect.DeepEqual(reg.Batches, want) {
		t.Fatalf("records and order changed: %+v", reg.Batches)
	}
}

// assertRegisterRejection verifies the error category and, importantly, that
// invalid input is never misreported as a duplicate or an existing-batch
// conflict.
func assertRegisterRejection(t *testing.T, err error, kind rejectKind, field string) {
	t.Helper()
	var ce *ConflictError
	if errors.As(err, &ce) {
		t.Fatalf("invalid input must not be reported as a batch conflict: %+v", ce)
	}
	if strings.Contains(err.Error(), "already registered") {
		t.Fatalf("invalid input must not be reported as a duplicate/conflict: %v", err)
	}
	switch kind {
	case rejectEmpty:
		if msg := err.Error(); msg != "batch, product and unit must be non-empty" {
			t.Fatalf("want the non-empty message naming all three text values, got %q", msg)
		}
		var ee *EncodingError
		if errors.As(err, &ee) {
			t.Fatalf("empty text must outrank encoding: %+v", ee)
		}
	case rejectEncoding:
		assertEncodingError(t, err, field)
	case rejectQuantity:
		msg := err.Error()
		if !strings.Contains(msg, "positive integer") || !strings.Contains(msg, strconv.FormatInt(MaxQuantity, 10)) {
			t.Fatalf("quantity error must state the accepted range, got %q", msg)
		}
	}
}

// assertEncodingError checks for *EncodingError naming field and explaining
// the UTF-8 cause; it returns true when it matched.
func assertEncodingError(t *testing.T, err error, field string) bool {
	t.Helper()
	var ee *EncodingError
	if !errors.As(err, &ee) {
		t.Errorf("want EncodingError on field %q, got %v", field, err)
		return false
	}
	if ee.Field != field {
		t.Errorf("EncodingError field = %q, want %q", ee.Field, field)
	}
	msg := err.Error()
	if !strings.Contains(msg, strconv.Quote(field)) || !strings.Contains(msg, "UTF-8") {
		t.Errorf("encoding error must name field %q and the UTF-8 cause, got %q", field, msg)
	}
	return true
}
