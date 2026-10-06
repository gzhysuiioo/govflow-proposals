package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// End-to-end regression for JSON escape equivalence of batch ids in
// batch-import: an id written with a \uXXXX escape — including a UTF-16
// surrogate pair — is the same id as the characters written directly, so a
// respelled stored id confirms as a duplicate (registry bytes and mtime
// untouched) and a respelled new id is created only once. Hex letter case
// and direct/escaped mixing are spelling choices, not identity. A literal
// backslash inside the decoded id is ordinary text, a genuine U+FFFD is
// legal text, and malformed surrogate escapes fail the whole import with
// the manifest path, the 1-based record position and the batch field —
// never a U+FFFD substitution that gets registered.

// bs is one backslash; the esc* spellings below are built from it so the
// manifests visibly contain JSON escape text (backslash, u, four hex
// digits) that only the JSON parser decodes.
const bs = "\\"

var (
	// escCJK spells 批次-1 with both CJK characters escaped; escMixed
	// writes 批 directly and escapes 次.
	escCJK   = bs + "u6279" + bs + "u6b21" + "-1"
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

// A stored batch whose id the manifest respells with escapes — BMP escapes,
// surrogate pairs at the U+10000 and U+10FFFF boundaries, mixed hex letter
// case and a direct/escaped mix — is confirmed as a duplicate with the
// decoded id in the output, and an all-duplicate import leaves the registry
// file byte-for-byte and timestamp-for-timestamp untouched.
func TestBatchImportCLIEscapedSpellingsConfirmStoredBatches(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "reg.json")
	manifest := filepath.Join(dir, "in.json")

	// The registry spells every id with the characters written directly.
	registryJSON := `{"version":1,"batches":[` +
		`{"batch":"批次-1","product":"P-1","quantity":10,"unit":"kg"},` +
		`{"batch":"` + "\U00010000" + `","product":"P-2","quantity":2,"unit":"box"},` +
		`{"batch":"` + "\U0010FFFF" + `","product":"P-3","quantity":3,"unit":"m"},` +
		`{"batch":"😀","product":"P-4","quantity":4,"unit":"g"}]}`
	writeFile(t, registry, registryJSON)
	content, pinned := pinRegistry(t, registry)

	// Every record matches its stored product, quantity and unit, so each
	// one is a duplicate confirmation of the batch its spelling decodes to.
	manifestJSON := `[
  {"batch":"` + escCJK + `","product":"P-1","quantity":10,"unit":"kg"},
  {"batch":"` + esc10000 + `","product":"P-2","quantity":2,"unit":"box"},
  {"batch":"` + esc10FFFF + `","product":"P-3","quantity":3,"unit":"m"},
  {"batch":"` + escEmojiMixedHex + `","product":"P-4","quantity":4,"unit":"g"},
  {"batch":"` + escMixed + `","product":"P-1","quantity":10,"unit":"kg"}
]`
	writeFile(t, manifest, manifestJSON)

	var stdout bytes.Buffer
	if err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout); err != nil {
		t.Fatalf("escaped respellings of stored ids must confirm as duplicates: %v", err)
	}
	var out importOutput
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &out); err != nil {
		t.Fatalf("stdout is not the expected JSON: %v (%q)", err, stdout.String())
	}
	wantBatch := []string{"批次-1", "\U00010000", "\U0010FFFF", "😀", "批次-1"}
	if len(out.Results) != len(wantBatch) {
		t.Fatalf("got %d results, want %d", len(out.Results), len(wantBatch))
	}
	for i, want := range wantBatch {
		if out.Results[i].Status != "duplicate" {
			t.Errorf("result %d status = %q, want duplicate (the escaped spelling is the same id)", i+1, out.Results[i].Status)
		}
		if out.Results[i].Batch != want {
			t.Errorf("result %d batch = %q, want the decoded id %q", i+1, out.Results[i].Batch, want)
		}
	}

	assertRegistryUntouched(t, registry, content, pinned)
	if after, _ := os.ReadFile(manifest); string(after) != manifestJSON {
		t.Fatal("the read-only manifest was modified")
	}
}

// A new id that appears twice in one manifest — first written directly,
// then respelled with escapes — is created by the first occurrence and
// confirmed by the respelling; the saved registry holds it exactly once.
func TestBatchImportCLINewEscapedIDCreatedOnce(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "reg.json")
	manifest := filepath.Join(dir, "in.json")
	writeFile(t, registry, `{"version":1,"batches":[{"batch":"B-0","product":"P-0","quantity":5,"unit":"box"}]}`)
	manifestJSON := `[
  {"batch":"批次-新","product":"P-1","quantity":1,"unit":"kg"},
  {"batch":"` + escNew + `","product":"P-1","quantity":1,"unit":"kg"},
  {"batch":"😀-2","product":"P-2","quantity":2,"unit":"g"},
  {"batch":"` + escEmoji + `-2","product":"P-2","quantity":2,"unit":"g"}
]`
	writeFile(t, manifest, manifestJSON)

	var stdout bytes.Buffer
	if err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout); err != nil {
		t.Fatalf("the manifest is legal and must import: %v", err)
	}
	var out importOutput
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &out); err != nil {
		t.Fatalf("stdout is not the expected JSON: %v (%q)", err, stdout.String())
	}
	wantStatus := []string{"created", "duplicate", "created", "duplicate"}
	wantBatch := []string{"批次-新", "批次-新", "😀-2", "😀-2"}
	if len(out.Results) != len(wantStatus) {
		t.Fatalf("got %d results, want %d", len(out.Results), len(wantStatus))
	}
	for i := range wantStatus {
		if out.Results[i].Status != wantStatus[i] || out.Results[i].Batch != wantBatch[i] {
			t.Errorf("result %d = %q/%q, want %q/%q (manifest order)",
				i+1, out.Results[i].Batch, out.Results[i].Status, wantBatch[i], wantStatus[i])
		}
	}

	stored := readStoredBatches(t, registry)
	wantStored := []registerResult{
		{Batch: "B-0", Product: "P-0", Quantity: 5, Unit: "box"},
		{Batch: "批次-新", Product: "P-1", Quantity: 1, Unit: "kg"},
		{Batch: "😀-2", Product: "P-2", Quantity: 2, Unit: "g"},
	}
	if len(stored) != len(wantStored) {
		t.Fatalf("registry holds %d records, want one per id: %v", len(stored), stored)
	}
	for i, want := range wantStored {
		got := stored[i]
		got.Status = ""
		if got != want {
			t.Errorf("stored record %d = %+v, want %+v", i+1, got, want)
		}
	}
	if after, _ := os.ReadFile(manifest); string(after) != manifestJSON {
		t.Fatal("the read-only manifest was modified")
	}
}

// The escape decodes to the emoji, but an id whose decoded text literally
// contains the characters backslash-u-d-8-3-d-backslash-u-d-e-0-0 (written
// in JSON with escaped backslashes) is a different batch: both are created
// side by side, and a re-import confirms each as a duplicate of itself.
func TestBatchImportCLILiteralBackslashIDStaysDistinct(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "reg.json")
	manifest := filepath.Join(dir, "in.json")
	manifestJSON := `[
  {"batch":"` + escEmoji + `","product":"P","quantity":1,"unit":"kg"},
  {"batch":"` + escLiteralBackslashID + `","product":"P","quantity":1,"unit":"kg"}
]`
	writeFile(t, manifest, manifestJSON)

	var stdout bytes.Buffer
	if err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout); err != nil {
		t.Fatal(err)
	}
	var out importOutput
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &out); err != nil {
		t.Fatalf("stdout is not the expected JSON: %v (%q)", err, stdout.String())
	}
	if len(out.Results) != 2 || out.Results[0].Status != "created" || out.Results[1].Status != "created" {
		t.Fatalf("the emoji id and the literal-backslash id are distinct batches, both must be created: %s", stdout.String())
	}
	if out.Results[0].Batch != "😀" || out.Results[1].Batch != literalBackslashID {
		t.Fatalf("decoded ids = %q and %q, want %q and %q",
			out.Results[0].Batch, out.Results[1].Batch, "😀", literalBackslashID)
	}
	stored := readStoredBatches(t, registry)
	if len(stored) != 2 || stored[0].Batch != "😀" || stored[1].Batch != literalBackslashID {
		t.Fatalf("registry must hold both ids as separate records: %v", stored)
	}

	// Re-importing the same manifest confirms both as duplicates and
	// leaves the saved file byte-for-byte untouched.
	saved, err := os.ReadFile(registry)
	if err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), `"status":"duplicate"`) || strings.Contains(stdout.String(), `"status":"created"`) {
		t.Fatalf("re-import must confirm both ids as duplicates: %s", stdout.String())
	}
	if after, _ := os.ReadFile(registry); !bytes.Equal(after, saved) {
		t.Fatal("an all-duplicate re-import rewrote the registry")
	}
	if after, _ := os.ReadFile(manifest); string(after) != manifestJSON {
		t.Fatal("the read-only manifest was modified")
	}
}

// A U+FFFD replacement character the user actually entered is ordinary
// legal text: written directly in the registry and as an escape in the
// manifest it is the same id, and a new genuine-FFFD id registers normally.
func TestBatchImportCLIGenuineFFFDIsOrdinaryText(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "reg.json")
	manifest := filepath.Join(dir, "in.json")
	fffd := string(rune(0xfffd))
	writeFile(t, registry, `{"version":1,"batches":[{"batch":"B-`+fffd+`","product":"P","quantity":1,"unit":"kg"}]}`)
	manifestJSON := `[
  {"batch":"B-` + escFFFD + `","product":"P","quantity":1,"unit":"kg"},
  {"batch":"C-` + escFFFD + `","product":"P","quantity":2,"unit":"box"}
]`
	writeFile(t, manifest, manifestJSON)

	var stdout bytes.Buffer
	if err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout); err != nil {
		t.Fatalf("a genuine U+FFFD is legal text: %v", err)
	}
	var out importOutput
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &out); err != nil {
		t.Fatalf("stdout is not the expected JSON: %v (%q)", err, stdout.String())
	}
	if len(out.Results) != 2 ||
		out.Results[0].Status != "duplicate" || out.Results[0].Batch != "B-"+fffd ||
		out.Results[1].Status != "created" || out.Results[1].Batch != "C-"+fffd {
		t.Fatalf("unexpected results: %s", stdout.String())
	}
	stored := readStoredBatches(t, registry)
	if len(stored) != 2 || stored[0].Batch != "B-"+fffd || stored[1].Batch != "C-"+fffd {
		t.Fatalf("registry must hold exactly the two genuine-FFFD ids: %v", stored)
	}
	if after, _ := os.ReadFile(manifest); string(after) != manifestJSON {
		t.Fatal("the read-only manifest was modified")
	}
}

// Every malformed surrogate shape — a lone high or low surrogate, a
// reversed pair, a high surrogate followed by a plain character — fails the
// whole import: the error names the manifest path, the 1-based record
// position and the batch field, never substitutes U+FFFD and never cites
// the undecodable id. Legal records before the bad one (a confirmable
// duplicate and a would-be new batch) leave nothing behind: an existing
// registry keeps its bytes and mtime, a missing one is not created, and
// the manifest stays as submitted.
func TestBatchImportCLIRejectsLoneSurrogateEscapes(t *testing.T) {
	badIDs := map[string]string{
		"lone high surrogate":              "B" + bs + "ud83d",
		"lone low surrogate":               "B" + bs + "ude00",
		"reversed surrogate pair":          "B" + bs + "ude00" + bs + "ud83d",
		"high surrogate then plain letter": "B" + bs + "ud83d" + "X",
	}
	for name, badID := range badIDs {
		t.Run(name+"/existing registry", func(t *testing.T) {
			dir := t.TempDir()
			registry := filepath.Join(dir, "reg.json")
			manifest := filepath.Join(dir, "in.json")
			registryJSON := `{"version":1,"batches":[{"batch":"B-0","product":"P-0","quantity":5,"unit":"box"}]}`
			writeFile(t, registry, registryJSON)
			content, pinned := pinRegistry(t, registry)
			// Record 1 confirms the stored batch, record 2 would create a
			// new one; neither may survive the failure at record 3.
			manifestJSON := `[
  {"batch":"B-0","product":"P-0","quantity":5,"unit":"box"},
  {"batch":"NEW-1","product":"P-1","quantity":1,"unit":"kg"},
  {"batch":"` + badID + `","product":"P-2","quantity":2,"unit":"kg"}
]`
			writeFile(t, manifest, manifestJSON)

			var stdout bytes.Buffer
			err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout)
			if err == nil {
				t.Fatal("a lone surrogate escape must fail the import")
			}
			msg := err.Error()
			for _, want := range []string{manifest, "record 3", `"batch"`, "UTF-8"} {
				if !strings.Contains(msg, want) {
					t.Errorf("error must mention %s: %v", want, msg)
				}
			}
			if strings.Contains(msg, string(rune(0xfffd))) {
				t.Errorf("error must not substitute U+FFFD for the bad escape: %v", msg)
			}
			if strings.Contains(msg, "(batch \"") {
				t.Errorf("the undecodable id must not be cited: %v", msg)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout must stay empty on failure, got %q", stdout.String())
			}
			assertRegistryUntouched(t, registry, content, pinned)
			if after, _ := os.ReadFile(manifest); string(after) != manifestJSON {
				t.Fatal("the read-only manifest was modified")
			}
		})
		t.Run(name+"/missing registry", func(t *testing.T) {
			dir := t.TempDir()
			registry := filepath.Join(dir, "reg.json")
			manifest := filepath.Join(dir, "in.json")
			// Record 1 would create a batch; the failure at record 2 must
			// leave no registry at all.
			manifestJSON := `[
  {"batch":"NEW-1","product":"P-1","quantity":1,"unit":"kg"},
  {"batch":"` + badID + `","product":"P-2","quantity":2,"unit":"kg"}
]`
			writeFile(t, manifest, manifestJSON)

			var stdout bytes.Buffer
			err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout)
			if err == nil {
				t.Fatal("a lone surrogate escape must fail the import")
			}
			msg := err.Error()
			for _, want := range []string{manifest, "record 2", `"batch"`, "UTF-8"} {
				if !strings.Contains(msg, want) {
					t.Errorf("error must mention %s: %v", want, msg)
				}
			}
			if strings.Contains(msg, string(rune(0xfffd))) {
				t.Errorf("error must not substitute U+FFFD for the bad escape: %v", msg)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout must stay empty on failure, got %q", stdout.String())
			}
			if _, statErr := os.Stat(registry); !os.IsNotExist(statErr) {
				t.Fatal("a rejected first import must not create the registry")
			}
			if after, _ := os.ReadFile(manifest); string(after) != manifestJSON {
				t.Fatal("the read-only manifest was modified")
			}
		})
	}
}

// A lone surrogate escape is not turned into U+FFFD: with a genuine-FFFD
// batch registered, a manifest id that differs from it only by a bad
// surrogate escape is an encoding failure, never a duplicate confirmation
// of the genuine-FFFD batch.
func TestBatchImportCLILoneSurrogateDoesNotMatchFFFDBatch(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "reg.json")
	manifest := filepath.Join(dir, "in.json")
	fffd := string(rune(0xfffd))
	writeFile(t, registry, `{"version":1,"batches":[{"batch":"B-`+fffd+`","product":"P","quantity":1,"unit":"kg"}]}`)
	content, pinned := pinRegistry(t, registry)
	manifestJSON := `[{"batch":"B` + bs + `ud83d","product":"P","quantity":1,"unit":"kg"}]`
	writeFile(t, manifest, manifestJSON)

	var stdout bytes.Buffer
	err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout)
	if err == nil {
		t.Fatal("a lone surrogate escape must fail, not confirm the genuine-FFFD batch")
	}
	msg := err.Error()
	for _, want := range []string{manifest, "record 1", `"batch"`, "UTF-8"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error must mention %s: %v", want, msg)
		}
	}
	if strings.Contains(msg, fffd) {
		t.Errorf("the error must not involve the genuine-FFFD id: %v", msg)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout must stay empty on failure, got %q", stdout.String())
	}
	assertRegistryUntouched(t, registry, content, pinned)
	if after, _ := os.ReadFile(manifest); string(after) != manifestJSON {
		t.Fatal("the read-only manifest was modified")
	}
}
