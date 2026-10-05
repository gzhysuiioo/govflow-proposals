package batchreg

import (
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// This file guards how the two on-disk sources report one batch record that
// carries several member-name faults at once — an unknown field and a
// duplicated field in the same object. Both sources reject such a record, but
// they disagree on which problem is named, and that difference is part of the
// public behavior:
//
//   - the registry file reports any duplicated field before an unknown one;
//     with several duplicates it names the field whose second occurrence
//     comes first in the source text, and it reports an unknown field only
//     when nothing is repeated;
//   - an import manifest reports whichever anomaly appears first in source
//     order — a later duplicate never replaces an earlier unknown field.
//
// Field names are compared after JSON key decoding, so a name written
// directly and the same name behind a legal escape are one member (a
// duplicate even when both values are identical), while a single escaped
// spelling of a normal field stays legal. Every error keeps the 1-based
// record position (accurate when the bad record follows legal ones), the
// exact decoded field name, and the batch id when it is unambiguous —
// verbatim for the registry, trimmed for the manifest, and never one of two
// duplicated "batch" values.

// wantKind distinguishes the two fault kinds a mixed record can raise.
const (
	kindDuplicate = "duplicate"
	kindUnknown   = "unknown"
)

// checkFaultKind asserts that reason reports the expected fault kind and
// never blurs the two kinds into one another. The field name itself travels
// differently per source — the registry carries it in FormatError.Field, the
// manifest embeds it in the reason — so each caller checks that part itself.
func checkFaultKind(t *testing.T, reason, kind string) {
	t.Helper()
	switch kind {
	case kindDuplicate:
		if !strings.Contains(reason, "more than once") {
			t.Fatalf("reason %q must report a duplicate field", reason)
		}
		if strings.Contains(reason, "unknown field") {
			t.Fatalf("reason %q must not report an unknown field", reason)
		}
	case kindUnknown:
		if !strings.Contains(reason, "unknown field") {
			t.Fatalf("reason %q must report an unknown field", reason)
		}
		if strings.Contains(reason, "more than once") {
			t.Fatalf("reason %q must not report a duplicate field", reason)
		}
	default:
		t.Fatalf("unknown kind %q", kind)
	}
}

// The registry-file rule: a duplicated member anywhere in the object outranks
// an unknown one, whichever comes first in the source; several duplicates are
// resolved to the field whose second occurrence appears earliest. The bad
// record sits behind a legal record so the 1-based position and the batch id
// must point at record 2 — never at the legal record in front of it.
func TestLoadMixedFaultRecordReportsDuplicateFirst(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name      string
		record    string
		wantField string
		wantKind  string
		wantBatch string
	}{
		// The example from the format rules: unknown supplier first, product
		// repeated later — the registry still names the duplicate.
		{"unknown before duplicate", `{"batch":"B-2","supplier":"S","product":"P","product":"P","quantity":1,"unit":"kg"}`, "product", kindDuplicate, "B-2"},
		// Moving product's second occurrence ahead of supplier changes nothing
		// for the registry: the duplicate is reported either way.
		{"duplicate before unknown", `{"batch":"B-2","product":"P","product":"P","supplier":"S","quantity":1,"unit":"kg"}`, "product", kindDuplicate, "B-2"},
		// Two fields repeat; the one whose second occurrence comes first wins.
		{"earliest second occurrence wins", `{"batch":"B-2","product":"P","quantity":1,"unit":"kg","unit":"g","product":"Q"}`, "unit", kindDuplicate, "B-2"},
		// Only with no duplicate at all is the first unknown field reported.
		{"first unknown when nothing repeats", `{"batch":"B-2","product":"P","quantity":1,"unit":"kg","supplier":"S","note":"x"}`, "supplier", kindUnknown, "B-2"},
		// An escaped respelling is the same member: a duplicate even with
		// identical values, and it still outranks the unknown supplier.
		{"escaped duplicate identical values", `{"batch":"B-2","produc\u0074":"P","product":"P","quantity":1,"unit":"kg","supplier":"S"}`, "product", kindDuplicate, "B-2"},
		// The batch member may sit after the offending fields; the id is
		// still attached.
		{"batch id after the faults", `{"supplier":"S","product":"P","product":"P","quantity":1,"unit":"kg","batch":"B-2"}`, "product", kindDuplicate, "B-2"},
		// The registry keeps the id verbatim — no trimming.
		{"batch id kept verbatim", `{"batch":" B-2 ","supplier":"S","product":"P","product":"P","quantity":1,"unit":"kg"}`, "product", kindDuplicate, " B-2 "},
		// A duplicated batch id is ambiguous even when both values match: no
		// id may be picked to name the record.
		{"duplicated batch cites no id", `{"batch":"B-2","batch":"B-2","supplier":"S","product":"P","quantity":1,"unit":"kg"}`, "batch", kindDuplicate, ""},
		// An escaped unknown name is reported by its decoded spelling.
		{"escaped unknown reported decoded", `{"batch":"B-2","supplie\u0072":"S","product":"P","quantity":1,"unit":"kg"}`, "supplier", kindUnknown, "B-2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "_")+".json")
			writeRegistry(t, path, `{"version":1,"batches":[`+
				`{"batch":"B-1","product":"P","quantity":1,"unit":"kg"},`+tc.record+`]}`)
			_, _, err := Load(path)
			var fe *FormatError
			if !errors.As(err, &fe) {
				t.Fatalf("expected FormatError, got %v", err)
			}
			if fe.Position != 2 {
				t.Fatalf("position=%d, want 2 (the bad record behind the legal one)", fe.Position)
			}
			if fe.Field != tc.wantField {
				t.Fatalf("field=%q, want %q", fe.Field, tc.wantField)
			}
			if fe.Batch != tc.wantBatch {
				t.Fatalf("batch=%q, want %q", fe.Batch, tc.wantBatch)
			}
			checkFaultKind(t, fe.Reason, tc.wantKind)
			if !strings.Contains(err.Error(), path) {
				t.Fatalf("error must name the source file %q: %v", path, err)
			}
		})
	}
}

// The manifest rule: member-name anomalies are reported in source order, so
// an unknown field ahead of a duplicate stays the reported problem — a later
// repeat never replaces it — and only a duplicate that occurs first is named.
// The batch id rides along trimmed, wherever the "batch" member sits.
func TestParseManifestMixedFaultRecordReportsSourceOrder(t *testing.T) {
	cases := []struct {
		name      string
		record    string
		wantField string
		wantKind  string
		wantBatch string
	}{
		// The same record the registry faults on product: the manifest names
		// the unknown supplier, because it appears first.
		{"unknown before duplicate stays unknown", `{"batch":" B-2 ","supplier":"S","product":"P","product":"P","quantity":1,"unit":"kg"}`, "supplier", kindUnknown, "B-2"},
		// With product's second occurrence moved ahead of supplier, the
		// manifest names the duplicate — matching the registry on this record.
		{"duplicate before unknown stays duplicate", `{"batch":"B-2","product":"P","product":"P","supplier":"S","quantity":1,"unit":"kg"}`, "product", kindDuplicate, "B-2"},
		// An escaped respelling of product is a duplicate even with identical
		// values; nothing earlier in the source outranks it.
		{"escaped duplicate identical values", `{"batch":"B-2","produc\u0074":"P","product":"P","quantity":1,"unit":"kg"}`, "product", kindDuplicate, "B-2"},
		// The batch member may sit after the offending field; the trimmed id
		// is still attached.
		{"batch id after the fault", `{"supplier":"S","product":"P","product":"P","quantity":1,"unit":"kg","batch":" B-2 "}`, "supplier", kindUnknown, "B-2"},
		// A duplicated batch id is ambiguous even when both values match.
		{"duplicated batch cites no id", `{"batch":"B-2","batch":"B-2","supplier":"S","product":"P","quantity":1,"unit":"kg"}`, "batch", kindDuplicate, ""},
		// An escaped unknown name is reported by its decoded spelling.
		{"escaped unknown reported decoded", `{"batch":"B-2","supplie\u0072":"S","product":"P","quantity":1,"unit":"kg"}`, "supplier", kindUnknown, "B-2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			content := `[{"batch":"B-1","product":"P","quantity":1,"unit":"kg"},` + tc.record + `]`
			_, err := ParseManifest([]byte(content))
			var re *ManifestRecordError
			if !errors.As(err, &re) {
				t.Fatalf("expected ManifestRecordError, got %v", err)
			}
			if re.Position != 2 {
				t.Fatalf("position=%d, want 2 (the bad record behind the legal one)", re.Position)
			}
			if re.Batch != tc.wantBatch {
				t.Fatalf("batch=%q, want %q", re.Batch, tc.wantBatch)
			}
			checkFaultKind(t, re.Reason, tc.wantKind)
			if !strings.Contains(re.Reason, strconv.Quote(tc.wantField)) {
				t.Fatalf("reason %q must name field %q", re.Reason, tc.wantField)
			}
			if tc.wantBatch != "" && !strings.Contains(err.Error(), "batch "+strconv.Quote(tc.wantBatch)) {
				t.Fatalf("error must cite the trimmed batch id %q: %v", tc.wantBatch, err)
			}
			if tc.wantBatch == "" && strings.Contains(err.Error(), "batch \"") {
				t.Fatalf("error must not quote a batch id: %v", err)
			}
		})
	}
}

// The control for the escape rules above: a record whose four field names are
// each written once behind a legal JSON escape is ordinary input, not an
// error — escapes alone never make a name unknown or duplicated. Both sources
// must read it back with the decoded names and values.
func TestLoneEscapedFieldNamesAreAccepted(t *testing.T) {
	const record = `{"bat\u0063h":"B-1","produc\u0074":"P","quantit\u0079":1,"uni\u0074":"kg"}`

	t.Run("registry", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "r.json")
		writeRegistry(t, path, `{"version":1,"batches":[`+record+`]}`)
		reg, existed, err := Load(path)
		if err != nil {
			t.Fatalf("a lone escaped spelling of each field must load: %v", err)
		}
		if !existed || len(reg.Batches) != 1 {
			t.Fatalf("existed=%v batches=%+v", existed, reg.Batches)
		}
		want := Batch{Batch: "B-1", Product: "P", Quantity: 1, Unit: "kg"}
		if reg.Batches[0] != want {
			t.Fatalf("record = %+v, want %+v", reg.Batches[0], want)
		}
	})

	t.Run("manifest", func(t *testing.T) {
		inputs, err := ParseManifest([]byte(`[` + record + `]`))
		if err != nil {
			t.Fatalf("a lone escaped spelling of each field must parse: %v", err)
		}
		want := Input{Batch: "B-1", Product: "P", Quantity: 1, Unit: "kg"}
		if len(inputs) != 1 || inputs[0] != want {
			t.Fatalf("inputs = %+v, want [%+v]", inputs, want)
		}
	})
}

// The legal-record control for the mixed-fault rules: a record carrying
// exactly the four known fields once each — the same shape the faulty records
// above deviate from — still reads back fine through both sources, so the
// rejections above are the fault rules firing, not a blanket refusal.
func TestCleanRecordStillReadsThroughBothSources(t *testing.T) {
	const record = `{"batch":"B-1","product":"P","quantity":1,"unit":"kg"}`

	path := filepath.Join(t.TempDir(), "r.json")
	writeRegistry(t, path, `{"version":1,"batches":[`+record+`]}`)
	if _, _, err := Load(path); err != nil {
		t.Fatalf("a clean registry record must load: %v", err)
	}
	if _, err := ParseManifest([]byte(`[` + record + `]`)); err != nil {
		t.Fatalf("a clean manifest record must parse: %v", err)
	}
}
