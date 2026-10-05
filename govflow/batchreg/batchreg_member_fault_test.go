package batchreg

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// This file is the regression net for one narrow rule: when a single batch
// record carries BOTH an unknown field and a duplicated field, each on-disk
// source must reject it with a stable, specific reason — never a vague "the
// record is bad". The two sources deliberately report a different first
// problem:
//
//   - the registry file reports any duplicated field first (the first field
//     name whose SECOND occurrence appears earliest in the source), and only
//     reports the first unknown field when nothing is repeated;
//   - the import manifest reports the first anomaly in source order, so an
//     unknown member seen before the first repeat stays the reported problem
//     even when a duplicate follows it.
//
// Field names are compared after JSON decoding: a direct spelling and a legal
// \uXXXX escape of the same name are one member, while a normal field written
// once through an escape is perfectly legal. Errors keep the source file, the
// 1-based record position, the exact decoded field name, and the batch id
// (verbatim in the registry, trimmed in the manifest) only when exactly one
// valid batch member exists.

// escKey renders an ASCII field name entirely as JSON \u00XX unicode escapes
// (escKey("x") == `x`), so the test JSON literally carries escaped key
// spellings that decode to the plain name. This is what proves comparison
// happens on the decoded text rather than the raw bytes.
func escKey(name string) string {
	var b strings.Builder
	for _, r := range name {
		fmt.Fprintf(&b, `\u%04x`, r)
	}
	return b.String()
}

var (
	escProduct  = escKey("product")  // decodes to "product"
	escBatch    = escKey("batch")    // decodes to "batch"
	escSupplier = escKey("supplier") // decodes to "supplier"
	escUnit     = escKey("unit")     // decodes to "unit"
)

// legalMemberFaultRecord is one valid record placed before every offending
// record, so the reported position must be 2 — never the preceding good batch.
const legalMemberFaultRecord = `{"batch":"B1","product":"P","quantity":1,"unit":"kg"}`

type memberFaultSource string

const (
	faultFromRegistry memberFaultSource = "registry"
	faultFromManifest memberFaultSource = "manifest"
)

type memberFaultExpectation struct {
	name   string
	source memberFaultSource
	// record is the offending JSON object, placed second after a legal record.
	record string
	field  string // exact decoded field name the error must carry
	dup    bool   // true: duplicate reason; false: unknown-field reason
	batch  string // batch id the error must cite; "" when it must cite none
}

func memberFaultCases() []memberFaultExpectation {
	return []memberFaultExpectation{
		// The canonical divergence: an unknown supplier first, product
		// repeated later. Registry names the duplicate; the manifest names
		// the unknown field that appeared first.
		{
			"registry unknown supplier before duplicate product reports product duplicate",
			faultFromRegistry,
			`{"batch":"B2","supplier":"S","product":"P","product":"Q","quantity":1,"unit":"kg"}`,
			"product", true, "B2",
		},
		{
			"manifest unknown supplier before duplicate product reports supplier unknown",
			faultFromManifest,
			`{"batch":"B2","supplier":"S","product":"P","product":"Q","quantity":1,"unit":"kg"}`,
			"supplier", false, "B2",
		},
		// Same record with the product repeat moved ahead of the unknown:
		// both sources report the duplicate.
		{
			"registry duplicate product ahead of unknown reports product duplicate",
			faultFromRegistry,
			`{"batch":"B2","product":"P","product":"Q","quantity":1,"unit":"kg","supplier":"S"}`,
			"product", true, "B2",
		},
		{
			"manifest duplicate product ahead of unknown reports product duplicate",
			faultFromManifest,
			`{"batch":"B2","product":"P","product":"Q","quantity":1,"unit":"kg","supplier":"S"}`,
			"product", true, "B2",
		},
		// One record contrasting both rules: the unknown supplier appears
		// first, but unit's second occurrence comes before product's second.
		// The registry reports the duplicate whose second occurrence is
		// earliest (unit); the manifest keeps source order and reports the
		// unknown supplier.
		{
			"registry reports earliest second occurrence (unit) over unknown and later product repeat",
			faultFromRegistry,
			`{"batch":"B2","product":"P","unit":"kg","supplier":"S","unit":"g","product":"Q","quantity":1}`,
			"unit", true, "B2",
		},
		{
			"manifest reports first unknown and is not moved by either later repeat",
			faultFromManifest,
			`{"batch":"B2","product":"P","unit":"kg","supplier":"S","unit":"g","product":"Q","quantity":1}`,
			"supplier", false, "B2",
		},
		// Two fields repeated and no unknown: the registry picks the one
		// whose second occurrence happens first.
		{
			"registry tie-break: product repeats before unit repeats",
			faultFromRegistry,
			`{"batch":"B2","product":"P","unit":"kg","quantity":1,"product":"P","unit":"kg"}`,
			"product", true, "B2",
		},
		{
			"registry tie-break: unit repeats before product repeats",
			faultFromRegistry,
			`{"batch":"B2","product":"P","unit":"kg","quantity":1,"unit":"kg","product":"P"}`,
			"unit", true, "B2",
		},
		// No duplicate anywhere: the registry reports the first unknown
		// member in source order.
		{
			"registry without duplicates reports first unknown field",
			faultFromRegistry,
			`{"batch":"B2","alpha":1,"beta":2,"product":"P","quantity":1,"unit":"kg"}`,
			"alpha", false, "B2",
		},
		// The manifest reports the first repeat and is not moved by a later
		// unknown.
		{
			"manifest reports the first repeat and ignores a later unknown",
			faultFromManifest,
			`{"batch":"B2","product":"P","product":"P","quantity":1,"unit":"kg","zz":1,"yy":2}`,
			"product", true, "B2",
		},
		// Names are judged on the decoded text: an escaped respelling is the
		// same member as the directly written one — a duplicate even when the
		// two values are identical — while an escape used alone stays legal
		// (covered by the acceptance test below).
		{
			"registry escaped product spelling duplicates direct name with equal values",
			faultFromRegistry,
			`{"batch":"B2","` + escProduct + `":"P","product":"P","quantity":1,"unit":"kg"}`,
			"product", true, "B2",
		},
		{
			"manifest escaped product spelling duplicates direct name with equal values",
			faultFromManifest,
			`{"batch":"B2","` + escProduct + `":"P","product":"P","quantity":1,"unit":"kg"}`,
			"product", true, "B2",
		},
		{
			"registry direct name duplicates escaped spelling with different values",
			faultFromRegistry,
			`{"batch":"B2","product":"P","` + escProduct + `":"Q","quantity":1,"unit":"kg"}`,
			"product", true, "B2",
		},
		// An escaped unknown name is reported under its decoded text, and the
		// registry still prefers a later duplicate to that earlier unknown.
		{
			"registry escaped unknown plus later duplicate reports the duplicate",
			faultFromRegistry,
			`{"batch":"B2","` + escSupplier + `":"S","product":"P","product":"Q","quantity":1,"unit":"kg"}`,
			"product", true, "B2",
		},
		{
			"manifest escaped unknown name is reported decoded",
			faultFromManifest,
			`{"batch":"B2","` + escSupplier + `":"S","product":"P","quantity":1,"unit":"kg"}`,
			"supplier", false, "B2",
		},
		// An escaped repeat participates in the earliest-second-occurrence
		// tie-break: the escaped unit repeat lands before the product repeat.
		{
			"registry escaped unit repeat outranks a later product repeat",
			faultFromRegistry,
			`{"batch":"B2","product":"P","unit":"kg","` + escUnit + `":"kg","product":"P","quantity":1}`,
			"unit", true, "B2",
		},
		// The single legal batch member may sit after the offending field;
		// its position is irrelevant to whether it identifies the record.
		{
			"registry cites batch id written after the offending fields",
			faultFromRegistry,
			`{"supplier":"S","product":"P","product":"Q","quantity":1,"unit":"kg","batch":"B9"}`,
			"product", true, "B9",
		},
		{
			"manifest cites batch id written after the offending field",
			faultFromManifest,
			`{"supplier":"S","batch":"B9","product":"P","quantity":1,"unit":"kg"}`,
			"supplier", false, "B9",
		},
		// The registry keeps the batch text verbatim; the manifest trims it.
		{
			"registry keeps the padded batch id verbatim",
			faultFromRegistry,
			`{"batch":" B2 ","supplier":"S","product":"P","product":"P","quantity":1,"unit":"kg"}`,
			"product", true, " B2 ",
		},
		{
			"manifest normalizes the padded batch id",
			faultFromManifest,
			`{"batch":" B2 ","supplier":"S","product":"P","quantity":1,"unit":"kg"}`,
			"supplier", false, "B2",
		},
		// A duplicated batch member — even with two identical values —
		// identifies no record: neither source may pick one value to name it.
		{
			"registry duplicated batch with equal values cites no id",
			faultFromRegistry,
			`{"batch":"B2","batch":"B2","product":"P","quantity":1,"unit":"kg","supplier":"S"}`,
			"batch", true, "",
		},
		{
			"manifest duplicated batch with equal values cites no id",
			faultFromManifest,
			`{"batch":"B2","batch":"B2","product":"P","quantity":1,"unit":"kg","supplier":"S"}`,
			"batch", true, "",
		},
		// A duplicated batch hidden behind an escape is equally unidentifiable,
		// even though an unknown member is present too (duplicate still wins
		// in the registry; the first repeat is the batch in the manifest).
		{
			"registry escaped duplicated batch cites no id over a later unknown",
			faultFromRegistry,
			`{"batch":"B2","` + escBatch + `":"B2","product":"P","quantity":1,"unit":"kg","supplier":"S"}`,
			"batch", true, "",
		},
	}
}

// TestRecordMemberFaultPrecedenceAcrossSources drives one offending record
// through the actual reader of each source and asserts the full error shape:
// position counted from the very first record, the exact decoded field name,
// a reason that distinguishes "duplicate" from "unknown" (not mere rejection),
// and the batch id exactly under that source's text policy.
func TestRecordMemberFaultPrecedenceAcrossSources(t *testing.T) {
	for _, tc := range memberFaultCases() {
		t.Run(tc.name, func(t *testing.T) {
			switch tc.source {
			case faultFromRegistry:
				path := memberFaultRegistryFile(t, `{"version":1,"batches":[`+legalMemberFaultRecord+`,`+tc.record+`]}`)
				_, _, err := Load(path)
				var fe *FormatError
				if !errors.As(err, &fe) {
					t.Fatalf("expected FormatError, got %v", err)
				}
				if fe.Position != 2 {
					t.Errorf("position=%d, want 2: the legal first record must not be blamed", fe.Position)
				}
				if fe.Field != tc.field {
					t.Errorf("field=%q, want %q (error: %v)", fe.Field, tc.field, err)
				}
				if fe.Batch != tc.batch {
					t.Errorf("batch=%q, want %q (error: %v)", fe.Batch, tc.batch, err)
				}
				assertMemberFaultReason(t, err.Error(), tc)
			case faultFromManifest:
				content := []byte(`[` + legalMemberFaultRecord + `,` + tc.record + `]`)
				inputs, err := ParseManifest(content)
				var re *ManifestRecordError
				if !errors.As(err, &re) {
					t.Fatalf("expected ManifestRecordError, got %v", err)
				}
				if inputs != nil {
					t.Fatalf("a rejected manifest must yield no inputs, got %+v", inputs)
				}
				if re.Position != 2 {
					t.Errorf("position=%d, want 2: the legal first record must not be blamed", re.Position)
				}
				if re.Batch != tc.batch {
					t.Errorf("batch=%q, want %q (error: %v)", re.Batch, tc.batch, err)
				}
				assertMemberFaultReason(t, err.Error(), tc)
			default:
				t.Fatalf("unknown source %q", tc.source)
			}
		})
	}
}

// assertMemberFaultReason checks that the wording distinguishes the two fault
// kinds, quotes the exact field name, and cites/omits the batch id as
// required.
func assertMemberFaultReason(t *testing.T, msg string, tc memberFaultExpectation) {
	t.Helper()
	if tc.dup {
		if !strings.Contains(msg, "more than once") {
			t.Errorf("error must report a DUPLICATE field %q: %v", tc.field, msg)
		}
		if strings.Contains(msg, "unknown field") {
			t.Errorf("error must not mislabel the duplicate as unknown: %v", msg)
		}
	} else {
		if !strings.Contains(msg, "unknown field") {
			t.Errorf("error must report an UNKNOWN field %q: %v", tc.field, msg)
		}
		if strings.Contains(msg, "more than once") {
			t.Errorf("error must not mislabel the unknown field as a duplicate: %v", msg)
		}
	}
	if !strings.Contains(msg, strconv.Quote(tc.field)) {
		t.Errorf("error must name the exact decoded field %q: %v", tc.field, msg)
	}
	if tc.batch != "" {
		if !strings.Contains(msg, "(batch "+strconv.Quote(tc.batch)+")") {
			t.Errorf("error must cite batch %q: %v", tc.batch, msg)
		}
	} else if strings.Contains(msg, "(batch \"") {
		t.Errorf("error must not invent a batch id: %v", msg)
	}
}

// memberFaultRegistryFile writes content into a fresh registry file and
// returns its path.
func memberFaultRegistryFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "registry.json")
	writeRegistry(t, path, content)
	return path
}

// TestEscapedFieldNamesAcceptedWhenNotRepeated is the acceptance mirror of the
// duplicate-escape cases: escaping alone is never an error. Every normal field
// written once through a legal \uXXXX escape must load and parse normally —
// otherwise "every escaped name is wrong" would pass the rejection-only
// assertions above while breaking the public format.
func TestEscapedFieldNamesAcceptedWhenNotRepeated(t *testing.T) {
	t.Run("registry accepts escaped member names", func(t *testing.T) {
		content := `{"version":1,"batches":[` + legalMemberFaultRecord + `,
			{"` + escBatch + `":"B2","` + escProduct + `":"P 8","quantity":2,"` + escUnit + `":"kg"}]}`
		reg, existed, err := Load(memberFaultRegistryFile(t, content))
		if err != nil {
			t.Fatalf("legal escaped member names must load: %v", err)
		}
		if !existed || len(reg.Batches) != 2 {
			t.Fatalf("existed=%v batches=%+v", existed, reg.Batches)
		}
		want := Batch{Batch: "B2", Product: "P 8", Quantity: 2, Unit: "kg"}
		if reg.Batches[1] != want {
			t.Fatalf("escaped record decoded as %+v, want %+v", reg.Batches[1], want)
		}
		// The escaped record is genuinely stored: re-registering its exact
		// values confirms a duplicate rather than failing on the spellings.
		if out, err := Register(reg, Input{Batch: "B2", Product: "P 8", Quantity: 2, Unit: "kg"}); err != nil || out.Created {
			t.Fatalf("escaped-name record should be a stored, confirmable record: %+v err=%v", out, err)
		}
	})

	t.Run("manifest accepts, decodes and trims escaped member names", func(t *testing.T) {
		content := []byte(`[` + legalMemberFaultRecord + `,
			{"` + escBatch + `":" B2 ","` + escProduct + `":"P 8","quantity":2,"` + escUnit + `":"kg"}]`)
		inputs, err := ParseManifest(content)
		if err != nil {
			t.Fatalf("legal escaped member names must parse: %v", err)
		}
		if len(inputs) != 2 {
			t.Fatalf("got %d inputs, want 2", len(inputs))
		}
		want := Input{Batch: "B2", Product: "P 8", Quantity: 2, Unit: "kg"}
		if inputs[1] != want {
			t.Fatalf("escaped manifest record decoded as %+v, want %+v", inputs[1], want)
		}
		// And the all-legal control actually imports — rejection must not be
		// the only outcome under test.
		reg := &Registry{Version: FormatVersion}
		out, err := Import(reg, inputs)
		if err != nil {
			t.Fatalf("legal control manifest must import: %v", err)
		}
		if len(out) != 2 || !out[0].Created || !out[1].Created {
			t.Fatalf("unexpected import outcomes: %+v", out)
		}
		if len(reg.Batches) != 2 || reg.Batches[1].Batch != "B2" {
			t.Fatalf("registry should hold both legal batches: %+v", reg.Batches)
		}
	})
}

// TestLegalControlRecordStillReadsThroughBothSources pairs the same good
// record that precedes every bad one above with a repaired second record: the
// bad combination must fail, but the repaired manifest must parse and import
// cleanly and the repaired registry must load. This stops a blanket-reject
// implementation from masquerading as the guarantee.
func TestLegalControlRecordStillReadsThroughBothSources(t *testing.T) {
	repaired := `{"batch":"B2","product":"P","quantity":1,"unit":"kg"}`

	t.Run("repaired registry loads and keeps both records", func(t *testing.T) {
		content := `{"version":1,"batches":[` + legalMemberFaultRecord + `,` + repaired + `]}`
		reg, existed, err := Load(memberFaultRegistryFile(t, content))
		if err != nil {
			t.Fatalf("the repaired registry must load: %v", err)
		}
		if !existed || len(reg.Batches) != 2 || reg.Batches[0].Batch != "B1" || reg.Batches[1].Batch != "B2" {
			t.Fatalf("unexpected registry: %+v", reg.Batches)
		}
	})

	t.Run("repaired manifest parses, imports and confirms repeats", func(t *testing.T) {
		content := []byte(`[` + legalMemberFaultRecord + `,` + repaired + `,` + repaired + `]`)
		inputs, err := ParseManifest(content)
		if err != nil {
			t.Fatalf("the repaired manifest must parse: %v", err)
		}
		reg := &Registry{Version: FormatVersion, Batches: []Batch{
			{Batch: "B1", Product: "P", Quantity: 1, Unit: "kg"},
		}}
		out, err := Import(reg, inputs)
		if err != nil {
			t.Fatalf("the repaired manifest must import: %v", err)
		}
		wantCreated := []bool{false, true, false} // B1 stored, B2 created, B2 repeat confirmed
		if len(out) != len(wantCreated) {
			t.Fatalf("got %d outcomes, want %d", len(out), len(wantCreated))
		}
		for i, want := range wantCreated {
			if out[i].Created != want {
				t.Errorf("record %d created=%v, want %v", i+1, out[i].Created, want)
			}
		}
		if len(reg.Batches) != 2 {
			t.Fatalf("registry should keep B1 and add B2 once: %+v", reg.Batches)
		}
	})
}
