package batchreg

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file pins the reporting order when one decoded record carries several
// faults at once. The three entry points share one rule set but deliberately
// report faults in different orders, and the refactor that unified the checks
// must not make any of them converge.
func badUTF8() string { return string([]byte{'k', 'g', 0xff}) }

// Register: any empty text field outranks every encoding fault, which in turn
// outranks the quantity. The error type must follow that same ordering.
func TestRegisterFaultPrecedence(t *testing.T) {
	cases := []struct {
		name string
		in   Input
		want func(*testing.T, error)
	}{
		{
			name: "empty batch outranks bad product encoding",
			in:   Input{Batch: "", Product: badUTF8(), Quantity: 1, Unit: "kg"},
			want: wantPlainNonEmpty(t),
		},
		{
			name: "empty unit outranks bad batch encoding",
			in:   Input{Batch: badUTF8(), Product: "P", Quantity: 1, Unit: ""},
			want: wantPlainNonEmpty(t),
		},
		{
			name: "first bad encoding in field order is named",
			in:   Input{Batch: "B" + badUTF8(), Product: badUTF8(), Quantity: 1, Unit: "kg"},
			want: wantEncodingError("batch"),
		},
		{
			name: "empty text outranks a bad quantity",
			in:   Input{Batch: "B", Product: "", Quantity: 0, Unit: "kg"},
			want: wantPlainNonEmpty(t),
		},
		{
			name: "bad encoding outranks a bad quantity",
			in:   Input{Batch: "B", Product: "P", Quantity: 0, Unit: badUTF8()},
			want: wantEncodingError("unit"),
		},
		{
			name: "negative quantity gives the quantity error once text is fine",
			in:   Input{Batch: "B", Product: "P", Quantity: -7, Unit: "kg"},
			want: wantQuantityError(t),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := &Registry{Version: FormatVersion}
			_, err := Register(reg, tc.in)
			if err == nil {
				t.Fatal("expected rejection")
			}
			tc.want(t, err)
			if len(reg.Batches) != 0 {
				t.Fatalf("rejected Register appended a record: %+v", reg.Batches)
			}
		})
	}
}

func wantPlainNonEmpty(t *testing.T) func(*testing.T, error) {
	t.Helper()
	return func(t *testing.T, err error) {
		t.Helper()
		var ee *EncodingError
		if errors.As(err, &ee) {
			t.Fatalf("empty text must outrank encoding, got EncodingError: %v", err)
		}
		if !strings.Contains(err.Error(), "must be non-empty") {
			t.Fatalf("want the non-empty error, got %v", err)
		}
	}
}

func wantEncodingError(field string) func(*testing.T, error) {
	return func(t *testing.T, err error) {
		t.Helper()
		var ee *EncodingError
		if !errors.As(err, &ee) {
			t.Fatalf("want *EncodingError, got %v", err)
		}
		if ee.Field != field {
			t.Fatalf("EncodingError field = %q, want %q", ee.Field, field)
		}
	}
}

func wantQuantityError(t *testing.T) func(*testing.T, error) {
	t.Helper()
	return func(t *testing.T, err error) {
		t.Helper()
		var ee *EncodingError
		if errors.As(err, &ee) {
			t.Fatalf("quantity fault must not surface as EncodingError: %v", err)
		}
		if !strings.Contains(err.Error(), "quantity") {
			t.Fatalf("want a quantity error, got %v", err)
		}
	}
}

// Direct Import: an encoding fault in any of the three text fields outranks
// every empty text field, which outranks the quantity. Position (1-based) and
// the safe batch-id attachment come along unchanged.
func TestImportFaultPrecedence(t *testing.T) {
	cases := []struct {
		name      string
		in        Input
		pos       int
		batch     string
		reasonHas string
	}{
		{
			name: "bad unit encoding outranks empty product",
			in:   Input{Batch: "B7", Product: "", Quantity: 1, Unit: badUTF8()},
			pos:  1, batch: "B7", reasonHas: `"unit"`,
		},
		{
			name: "first encoding fault follows field order",
			in:   Input{Batch: "B" + badUTF8(), Product: badUTF8(), Quantity: 1, Unit: badUTF8()},
			pos:  1, batch: "", reasonHas: `"batch"`,
		},
		{
			name: "empty product outranks bad quantity",
			in:   Input{Batch: "B7", Product: "", Quantity: 0, Unit: "kg"},
			pos:  1, batch: "B7", reasonHas: `"product"`,
		},
		{
			name: "encoding fault outranks bad quantity",
			in:   Input{Batch: "B7", Product: "P", Quantity: 0, Unit: badUTF8()},
			pos:  1, batch: "B7", reasonHas: `"unit"`,
		},
		{
			name: "quantity zero gives the zero wording",
			in:   Input{Batch: "B7", Product: "P", Quantity: 0, Unit: "kg"},
			pos:  1, batch: "B7", reasonHas: `must be greater than zero`,
		},
		{
			name: "negative quantity gives the same non-positive wording",
			in:   Input{Batch: "B7", Product: "P", Quantity: -1, Unit: "kg"},
			pos:  1, batch: "B7", reasonHas: `must be greater than zero`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := &Registry{Version: FormatVersion}
			_, err := Import(reg, []Input{tc.in})
			var re *ManifestRecordError
			if !errors.As(err, &re) {
				t.Fatalf("want *ManifestRecordError, got %v", err)
			}
			if re.Position != tc.pos || re.Batch != tc.batch {
				t.Fatalf("Position=%d Batch=%q, want %d/%q (err: %v)", re.Position, re.Batch, tc.pos, tc.batch, err)
			}
			if !strings.Contains(re.Reason, tc.reasonHas) {
				t.Fatalf("reason %q must contain %q", re.Reason, tc.reasonHas)
			}
			if len(reg.Batches) != 0 {
				t.Fatalf("rejected Import appended records: %+v", reg.Batches)
			}
		})
	}
}

// Save: fields are scanned batch, product, unit with a field's encoding fault
// outranking its own empty value; text faults outrank quantity; and any
// validity fault outranks the duplicate-id rule. Nothing is written and the
// caller's table is untouched.
func TestSaveFaultPrecedence(t *testing.T) {
	cases := []struct {
		name    string
		batches []Batch
		pos     int
		field   string
		batch   string
		reason  string
	}{
		{
			name: "empty batch outranks bad product encoding",
			batches: []Batch{
				{Batch: "", Product: badUTF8(), Quantity: 1, Unit: "kg"},
			},
			pos: 1, field: "batch", batch: "", reason: "must not be empty",
		},
		{
			name: "empty earlier field outranks bad encoding on a later field",
			batches: []Batch{
				{Batch: "B1", Product: "", Quantity: 1, Unit: badUTF8()},
			},
			pos: 1, field: "product", batch: "B1", reason: "must not be empty",
		},
		{
			name: "encoding on batch outranks empty product",
			batches: []Batch{
				{Batch: "B" + badUTF8(), Product: "", Quantity: 1, Unit: "kg"},
			},
			pos: 1, field: "batch", batch: "", reason: "valid UTF-8",
		},
		{
			name: "text fault outranks bad quantity",
			batches: []Batch{
				{Batch: "B1", Product: "P", Quantity: 0, Unit: ""},
			},
			pos: 1, field: "unit", batch: "B1", reason: "must not be empty",
		},
		{
			name: "validity of an earlier record outranks a later duplicate id",
			batches: []Batch{
				{Batch: "DUP", Product: "", Quantity: 1, Unit: "kg"},
				{Batch: "DUP", Product: "", Quantity: 1, Unit: "kg"},
			},
			pos: 1, field: "product", batch: "DUP", reason: "must not be empty",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "nested", "registry.json")
			submitted := append([]Batch(nil), tc.batches...)
			reg := &Registry{Version: FormatVersion, Batches: tc.batches}
			err := Save(path, reg)
			var fe *FormatError
			if !errors.As(err, &fe) {
				t.Fatalf("want *FormatError, got %v", err)
			}
			if fe.Position != tc.pos || fe.Field != tc.field || fe.Batch != tc.batch {
				t.Fatalf("FormatError = pos %d field %q batch %q, want %d %q %q",
					fe.Position, fe.Field, fe.Batch, tc.pos, tc.field, tc.batch)
			}
			if !strings.Contains(fe.Reason, tc.reason) {
				t.Fatalf("reason %q must contain %q", fe.Reason, tc.reason)
			}
			var dup *DuplicateIDError
			if errors.As(err, &dup) {
				t.Fatalf("validity fault must outrank duplicate-id reporting: %v", err)
			}
			// A rejected save creates nothing and leaves the table untouched.
			if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
				t.Fatalf("rejected save must not create the target file, stat err=%v", statErr)
			}
			if _, statErr := os.Stat(filepath.Dir(path)); !os.IsNotExist(statErr) {
				t.Fatalf("rejected save must not create directories, stat err=%v", statErr)
			}
			assertNoLeftoverTempFiles(t, filepath.Dir(filepath.Dir(path)))
			assertBatchesEqual(t, reg.Batches, submitted)
		})
	}
}
