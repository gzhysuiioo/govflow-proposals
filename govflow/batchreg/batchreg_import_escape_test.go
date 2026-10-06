package batchreg

import (
	"errors"
	"strings"
	"testing"
)

// Escape-equivalence regression for manifest batch ids: a JSON escape —
// including a UTF-16 surrogate pair — names the same character as the one
// written directly, so respelling a batch id never creates a second batch.
// Hex letter case and direct/escaped mixing are spelling choices, not
// identity, and a literal backslash in the decoded text is ordinary text
// that no escape decoding may merge with the character an escape names.

// bs is one backslash. The manifests below build their escape text out of
// it, so escCJK below is the twelve ASCII characters of two JSON unicode
// escapes (backslash, u, 6, 2, 7, 9, backslash, u, 6, b, 2, 1) — the JSON
// parser is what decodes them into the two CJK characters.
const bs = "\\"

var (
	// escCJK spells 批次-1 with both CJK characters escaped.
	escCJK = bs + "u6279" + bs + "u6b21" + "-1"
	// escMixed spells 批次-1 with 批 written directly and 次 escaped.
	escMixed = "批" + bs + "u6b21" + "-1"
	// escNew spells 批次-新 fully escaped.
	escNew = bs + "u6279" + bs + "u6b21" + "-" + bs + "u65b0"
	// esc10000 and esc10FFFF are the surrogate-pair spellings of the two
	// boundary characters a UTF-16 pair can express.
	esc10000  = bs + "ud800" + bs + "udc00" // U+10000
	esc10FFFF = bs + "udbff" + bs + "udfff" // U+10FFFF
	// escEmoji spells 😀 (U+1F600) as a surrogate pair; escEmojiMixedHex
	// is the same pair with the hex letter case varied.
	escEmoji         = bs + "ud83d" + bs + "ude00"
	escEmojiMixedHex = bs + "uD83d" + bs + "uDE00"
	// escFFFD spells a genuine U+FFFD replacement character as an escape.
	escFFFD = bs + "ufffd"
	// escLiteralBackslashID spells, in JSON, an id whose decoded text is
	// the twelve literal characters backslash-u-d-8-3-d-backslash-u-d-e-0-0:
	// each backslash is itself escaped in the JSON source.
	escLiteralBackslashID = bs + bs + "ud83d" + bs + bs + "ude00"
	// literalBackslashID is what escLiteralBackslashID decodes to.
	literalBackslashID = bs + "ud83d" + bs + "ude00"
)

// TestParseManifestEscapeSpellingsDecodeToSameIDs feeds one manifest whose
// batch ids are spelled every legal way — directly, as BMP escapes, as
// surrogate pairs at both boundaries (U+10000 and U+10FFFF), with mixed
// hex letter case and mixed direct/escaped characters — and requires every
// spelling of one id to decode to the very same text.
func TestParseManifestEscapeSpellingsDecodeToSameIDs(t *testing.T) {
	manifest := `[
		{"batch":"批次-1","product":"P","quantity":1,"unit":"kg"},
		{"batch":"` + escCJK + `","product":"P","quantity":1,"unit":"kg"},
		{"batch":"` + escMixed + `","product":"P","quantity":1,"unit":"kg"},
		{"batch":"` + "\U00010000" + `","product":"P","quantity":1,"unit":"kg"},
		{"batch":"` + esc10000 + `","product":"P","quantity":1,"unit":"kg"},
		{"batch":"` + "\U0010FFFF" + `","product":"P","quantity":1,"unit":"kg"},
		{"batch":"` + esc10FFFF + `","product":"P","quantity":1,"unit":"kg"},
		{"batch":"😀","product":"P","quantity":1,"unit":"kg"},
		{"batch":"` + escEmojiMixedHex + `","product":"P","quantity":1,"unit":"kg"}
	]`
	inputs, err := ParseManifest([]byte(manifest))
	if err != nil {
		t.Fatalf("every spelling is legal JSON text: %v", err)
	}
	want := []string{
		"批次-1", "批次-1", "批次-1",
		"\U00010000", "\U00010000",
		"\U0010FFFF", "\U0010FFFF",
		"😀", "😀",
	}
	if len(inputs) != len(want) {
		t.Fatalf("got %d inputs, want %d", len(inputs), len(want))
	}
	for i, w := range want {
		if inputs[i].Batch != w {
			t.Errorf("record %d batch = %q, want %q (escape spelling must decode identically)", i+1, inputs[i].Batch, w)
		}
	}
}

// TestImportEscapedRespellingConfirmsStoredIDs registers ids written
// directly, then re-submits them escaped: each one is a duplicate
// confirmation of the stored record and the registry gains nothing.
func TestImportEscapedRespellingConfirmsStoredIDs(t *testing.T) {
	stored := []Batch{
		{Batch: "批次-1", Product: "P-1", Quantity: 10, Unit: "kg"},
		{Batch: "\U00010000", Product: "P-2", Quantity: 2, Unit: "box"},
		{Batch: "\U0010FFFF", Product: "P-3", Quantity: 3, Unit: "m"},
		{Batch: "😀", Product: "P-4", Quantity: 4, Unit: "g"},
	}
	reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), stored...)}
	inputs, err := ParseManifest([]byte(`[
		{"batch":"` + escCJK + `","product":"P-1","quantity":10,"unit":"kg"},
		{"batch":"` + esc10000 + `","product":"P-2","quantity":2,"unit":"box"},
		{"batch":"` + esc10FFFF + `","product":"P-3","quantity":3,"unit":"m"},
		{"batch":"` + escEmoji + `","product":"P-4","quantity":4,"unit":"g"},
		{"batch":"` + escMixed + `","product":"P-1","quantity":10,"unit":"kg"}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	results, err := Import(reg, inputs)
	if err != nil {
		t.Fatalf("escaped respellings of stored ids must confirm as duplicates: %v", err)
	}
	wantBatch := []string{"批次-1", "\U00010000", "\U0010FFFF", "😀", "批次-1"}
	if len(results) != len(wantBatch) {
		t.Fatalf("got %d results, want %d", len(results), len(wantBatch))
	}
	for i, w := range wantBatch {
		if results[i].Created {
			t.Errorf("result %d created a new batch for a respelled stored id: %+v", i+1, results[i])
		}
		if results[i].Batch.Batch != w {
			t.Errorf("result %d batch = %q, want decoded %q", i+1, results[i].Batch.Batch, w)
		}
	}
	if len(reg.Batches) != len(stored) {
		t.Fatalf("duplicate confirmations added records: %+v", reg.Batches)
	}
	for i, b := range stored {
		if reg.Batches[i] != b {
			t.Errorf("stored record %d = %+v, want %+v", i+1, reg.Batches[i], b)
		}
	}
}

// TestImportNewEscapedIDCreatedOnce introduces a new id twice in one
// manifest — first written directly, then respelled with escapes — so only
// the first occurrence creates a record and the respelling confirms it.
func TestImportNewEscapedIDCreatedOnce(t *testing.T) {
	reg := &Registry{Version: FormatVersion}
	inputs, err := ParseManifest([]byte(`[
		{"batch":"批次-新","product":"P-1","quantity":1,"unit":"kg"},
		{"batch":"` + escNew + `","product":"P-1","quantity":1,"unit":"kg"},
		{"batch":"😀-2","product":"P-2","quantity":2,"unit":"g"},
		{"batch":"` + escEmoji + `-2","product":"P-2","quantity":2,"unit":"g"}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	results, err := Import(reg, inputs)
	if err != nil {
		t.Fatal(err)
	}
	wantCreated := []bool{true, false, true, false}
	wantBatch := []string{"批次-新", "批次-新", "😀-2", "😀-2"}
	if len(results) != len(wantCreated) {
		t.Fatalf("got %d results, want %d", len(results), len(wantCreated))
	}
	for i := range wantCreated {
		if results[i].Created != wantCreated[i] || results[i].Batch.Batch != wantBatch[i] {
			t.Errorf("result %d = created:%v batch %q, want created:%v batch %q (manifest order)",
				i+1, results[i].Created, results[i].Batch.Batch, wantCreated[i], wantBatch[i])
		}
	}
	wantStored := []Batch{
		{Batch: "批次-新", Product: "P-1", Quantity: 1, Unit: "kg"},
		{Batch: "😀-2", Product: "P-2", Quantity: 2, Unit: "g"},
	}
	if len(reg.Batches) != len(wantStored) {
		t.Fatalf("registry holds %d records, want one per id: %+v", len(reg.Batches), reg.Batches)
	}
	for i, w := range wantStored {
		if reg.Batches[i] != w {
			t.Errorf("stored record %d = %+v, want %+v", i+1, reg.Batches[i], w)
		}
	}
}

// TestImportLiteralBackslashIDIsNotThePairEscape separates decoding from
// text: the escape escEmoji decodes to the emoji, while
// escLiteralBackslashID decodes to twelve literal characters starting with
// a backslash. The two are different batches and must never be merged.
func TestImportLiteralBackslashIDIsNotThePairEscape(t *testing.T) {
	reg := &Registry{Version: FormatVersion}
	manifest := `[
		{"batch":"` + escEmoji + `","product":"P","quantity":1,"unit":"kg"},
		{"batch":"` + escLiteralBackslashID + `","product":"P","quantity":1,"unit":"kg"}
	]`
	inputs, err := ParseManifest([]byte(manifest))
	if err != nil {
		t.Fatal(err)
	}
	if inputs[0].Batch != "😀" || inputs[1].Batch != literalBackslashID {
		t.Fatalf("decoded ids = %q and %q, want %q and %q",
			inputs[0].Batch, inputs[1].Batch, "😀", literalBackslashID)
	}
	results, err := Import(reg, inputs)
	if err != nil {
		t.Fatal(err)
	}
	if !results[0].Created || !results[1].Created {
		t.Fatalf("the emoji id and the literal-backslash id are distinct batches, both must be created: %+v", results)
	}
	if len(reg.Batches) != 2 || reg.Batches[0].Batch != "😀" || reg.Batches[1].Batch != literalBackslashID {
		t.Fatalf("registry must hold both ids as separate records: %+v", reg.Batches)
	}

	// Re-importing the same manifest confirms each id as a duplicate of
	// itself — never of the other — and adds nothing.
	again, err := Import(reg, inputs)
	if err != nil {
		t.Fatal(err)
	}
	if again[0].Created || again[1].Created {
		t.Fatalf("re-import must confirm both ids as duplicates: %+v", again)
	}
	if len(reg.Batches) != 2 {
		t.Fatalf("duplicate confirmations added records: %+v", reg.Batches)
	}
}

// TestImportGenuineFFFDIsOrdinaryText proves a U+FFFD the user actually
// entered — written directly or as an escape — is a legal id like any
// other, unrelated to the rejected lone-surrogate escapes.
func TestImportGenuineFFFDIsOrdinaryText(t *testing.T) {
	fffd := string(rune(0xfffd))
	reg := &Registry{Version: FormatVersion, Batches: []Batch{
		{Batch: "B-" + fffd, Product: "P", Quantity: 1, Unit: "kg"},
	}}
	inputs, err := ParseManifest([]byte(`[
		{"batch":"B-` + escFFFD + `","product":"P","quantity":1,"unit":"kg"},
		{"batch":"C-` + escFFFD + `","product":"P","quantity":2,"unit":"box"}
	]`))
	if err != nil {
		t.Fatalf("a genuine U+FFFD is legal text: %v", err)
	}
	results, err := Import(reg, inputs)
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Created || !results[1].Created {
		t.Fatalf("escaped genuine U+FFFD must confirm the stored id and create the new one: %+v", results)
	}
	if results[0].Batch.Batch != "B-"+fffd || results[1].Batch.Batch != "C-"+fffd {
		t.Fatalf("decoded ids wrong: %+v", results)
	}
	if len(reg.Batches) != 2 || reg.Batches[0].Batch != "B-"+fffd || reg.Batches[1].Batch != "C-"+fffd {
		t.Fatalf("registry must hold exactly the two genuine-FFFD ids: %+v", reg.Batches)
	}
}

// TestParseManifestRejectsLoneSurrogateShapes covers every malformed
// surrogate shape: a lone high or low surrogate, a reversed pair, a high
// surrogate followed by a plain character, and two high surrogates. Each
// fails the manifest at the record's 1-based position naming the batch
// field — never substituted with U+FFFD and registered, and never cited
// back as an undecodable id — even when a legal record precedes it.
func TestParseManifestRejectsLoneSurrogateShapes(t *testing.T) {
	cases := map[string]string{
		"lone high surrogate":              "B" + bs + "ud83d",
		"lone low surrogate":               "B" + bs + "ude00",
		"reversed surrogate pair":          "B" + bs + "ude00" + bs + "ud83d",
		"high surrogate then plain letter": "B" + bs + "ud83d" + "X",
		"two high surrogates":              "B" + bs + "ud83d" + bs + "ud83d",
	}
	for name, id := range cases {
		t.Run(name, func(t *testing.T) {
			manifest := `[
				{"batch":"OK-1","product":"P","quantity":1,"unit":"kg"},
				{"batch":"` + id + `","product":"P","quantity":2,"unit":"kg"}
			]`
			_, err := ParseManifest([]byte(manifest))
			var re *ManifestRecordError
			if !errors.As(err, &re) {
				t.Fatalf("got %v, want ManifestRecordError", err)
			}
			if re.Position != 2 {
				t.Errorf("position = %d, want 2", re.Position)
			}
			if re.Batch != "" {
				t.Errorf("an undecodable id must not be cited, got %q", re.Batch)
			}
			if !strings.Contains(re.Reason, `"batch"`) {
				t.Errorf("reason must name the batch field: %q", re.Reason)
			}
			if strings.Contains(err.Error(), string(rune(0xfffd))) {
				t.Errorf("error must not substitute U+FFFD for the bad escape: %v", err)
			}
		})
	}
}
