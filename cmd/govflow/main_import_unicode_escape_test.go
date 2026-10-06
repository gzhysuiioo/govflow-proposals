package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Regression net for JSON \uXXXX escape spellings of batch ids in the import
// manifest. An escape and the character it decodes to are the SAME text, so a
// manifest that spells a registered id through escapes must confirm a
// duplicate — never register the batch a second time — and a manifest that
// introduces a new id must merge every later legal respelling of it into one
// stored record. Equivalence covers ordinary non-ASCII characters, valid
// UTF-16 surrogate pairs (the U+10000 and U+10FFFF boundaries included),
// either hex-letter casing and any mix of direct characters and escapes.
//
// The mirror image is pinned too: an id whose decoded text literally contains
// backslash characters is a different batch from the id the same source bytes
// would decode to without the doubled backslashes, a genuinely entered U+FFFD
// is ordinary text, and a lone or misordered surrogate escape rejects the
// whole import — nothing is registered under a U+FFFD substitution.

// TestBatchImportCLIEscapedIDsConfirmRegisteredBatch registers each batch id
// directly, then re-submits the identical record with the id spelled through
// \uXXXX escapes. Every case must come back "duplicate" echoing the decoded
// id, and the registry's bytes and mtime must not move.
func TestBatchImportCLIEscapedIDsConfirmRegisteredBatch(t *testing.T) {
	cases := map[string]struct {
		stored  string // id written directly into the registry file
		spelled string // the same id as spelled in the manifest
	}{
		"chinese written as escapes":      {"批次-1", `\u6279\u6B21-1`},
		"escape hex letters are caseless": {"批次-1", `\u6279\u6b21-1`},
		"direct and escaped chars mixed":  {"批次-1", `批\u6b21-1`},
		"surrogate pair at U+10000":       {"A\U00010000", `A\uD800\uDC00`},
		"surrogate pair at U+10FFFF":      {"Z\U0010FFFF", `Z\uDBFF\uDFFF`},
		"surrogate pair hex caseless":     {"B😀", `B\ud83d\ude00`},
		"genuine U+FFFD written escaped":  {"B-�", `B-\ufffd`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			registry := filepath.Join(dir, "reg.json")
			manifest := filepath.Join(dir, "in.json")
			writeFile(t, registry, `{"version":1,"batches":[{"batch":"`+tc.stored+
				`","product":"P-7","quantity":10,"unit":"kg"}]}`)
			manifestContent := `[{"batch":"` + tc.spelled +
				`","product":"P-7","quantity":10,"unit":"kg"}]`
			writeFile(t, manifest, manifestContent)
			manifestBytes := readFileSnapshot(t, manifest)
			content, pinned := pinRegistry(t, registry)

			var stdout bytes.Buffer
			if err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout); err != nil {
				t.Fatalf("an escaped respelling of a registered id must import: %v", err)
			}
			assertImportResults(t, stdout.Bytes(), []expectedImportRow{
				row(tc.stored, "P-7", 10, "kg", "duplicate"),
			})
			assertRegistryUntouched(t, registry, content, pinned)
			assertFileUnchanged(t, manifest, manifestBytes)
		})
	}
}

// TestBatchImportCLIEscapedIDsCreateThenDuplicate introduces new batch ids in
// one manifest: the first spelling creates, every later legal respelling of
// the same text confirms a duplicate, results keep manifest order, and the
// saved registry holds exactly one decoded record per id.
func TestBatchImportCLIEscapedIDsCreateThenDuplicate(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "reg.json")
	manifest := filepath.Join(dir, "in.json")
	manifestContent := `[
  {"batch":"批次-2","product":"P-7","quantity":10,"unit":"kg"},
  {"batch":"\u6279\u6b21-2","product":"P-7","quantity":10,"unit":"kg"},
  {"batch":"B😀","product":"P-8","quantity":1,"unit":"box"},
  {"batch":"B\ud83d\ude00","product":"P-8","quantity":1,"unit":"box"}
]`
	writeFile(t, manifest, manifestContent)
	manifestBytes := readFileSnapshot(t, manifest)

	var stdout bytes.Buffer
	if err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout); err != nil {
		t.Fatalf("import failed: %v", err)
	}
	assertImportResults(t, stdout.Bytes(), []expectedImportRow{
		row("批次-2", "P-7", 10, "kg", "created"),
		row("批次-2", "P-7", 10, "kg", "duplicate"),
		row("B😀", "P-8", 1, "box", "created"),
		row("B😀", "P-8", 1, "box", "duplicate"),
	})
	assertStoredBatches(t, registry, []registerResult{
		{Batch: "批次-2", Product: "P-7", Quantity: 10, Unit: "kg"},
		{Batch: "B😀", Product: "P-8", Quantity: 1, Unit: "box"},
	})
	assertFileUnchanged(t, manifest, manifestBytes)
}

// TestBatchImportCLILiteralBackslashIDStaysDistinct separates decoding from
// text: the escape pair "\ud83d\ude00" decodes to the emoji 😀, while the
// id spelled "B\\ud83d\\ude00" decodes to the literal backslash text
// B\ud83d\ude00. The two are different batches and must never merge — not
// when first registered, and not when confirmed again afterwards.
func TestBatchImportCLILiteralBackslashIDStaysDistinct(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "reg.json")
	manifest := filepath.Join(dir, "in.json")
	const literal = `B\ud83d\ude00` // decoded id text: backslashes included

	manifestContent := `[
  {"batch":"B\ud83d\ude00","product":"P","quantity":1,"unit":"kg"},
  {"batch":"B\\ud83d\\ude00","product":"P","quantity":1,"unit":"kg"},
  {"batch":"B😀","product":"P","quantity":1,"unit":"kg"}
]`
	writeFile(t, manifest, manifestContent)
	manifestBytes := readFileSnapshot(t, manifest)

	var stdout bytes.Buffer
	if err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout); err != nil {
		t.Fatalf("import failed: %v", err)
	}
	assertImportResults(t, stdout.Bytes(), []expectedImportRow{
		row("B😀", "P", 1, "kg", "created"),
		row(literal, "P", 1, "kg", "created"),
		row("B😀", "P", 1, "kg", "duplicate"),
	})
	assertStoredBatches(t, registry, []registerResult{
		{Batch: "B😀", Product: "P", Quantity: 1, Unit: "kg"},
		{Batch: literal, Product: "P", Quantity: 1, Unit: "kg"},
	})
	assertFileUnchanged(t, manifest, manifestBytes)

	// Re-importing either spelling confirms its own batch as a duplicate;
	// neither collapses onto the other, and the registry is not rewritten.
	content, pinned := pinRegistry(t, registry)
	confirmContent := `[
  {"batch":"B\uD83D\uDE00","product":"P","quantity":1,"unit":"kg"},
  {"batch":"B\\ud83d\\ude00","product":"P","quantity":1,"unit":"kg"}
]`
	writeFile(t, manifest, confirmContent)
	confirmBytes := readFileSnapshot(t, manifest)

	stdout.Reset()
	if err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout); err != nil {
		t.Fatalf("re-import failed: %v", err)
	}
	assertImportResults(t, stdout.Bytes(), []expectedImportRow{
		row("B😀", "P", 1, "kg", "duplicate"),
		row(literal, "P", 1, "kg", "duplicate"),
	})
	assertRegistryUntouched(t, registry, content, pinned)
	assertStoredBatches(t, registry, []registerResult{
		{Batch: "B😀", Product: "P", Quantity: 1, Unit: "kg"},
		{Batch: literal, Product: "P", Quantity: 1, Unit: "kg"},
	})
	assertFileUnchanged(t, manifest, confirmBytes)
}

// TestBatchImportCLIRejectsBadSurrogateEscapes pins the failure side: a lone
// high surrogate, a lone low surrogate, a reversed pair or a high surrogate
// followed by an ordinary character is not text and must fail the whole
// import — the bad id is never replaced with U+FFFD and registered. The error
// names the manifest path, the 1-based record position and the batch field,
// and never quotes the undecodable id. Legal records earlier in the manifest
// (a confirmable duplicate, a genuinely new id, a genuine U+FFFD) leave
// nothing behind: an existing registry keeps bytes and mtime, a missing one
// is not created, and the manifest keeps its submitted bytes.
func TestBatchImportCLIRejectsBadSurrogateEscapes(t *testing.T) {
	badIDs := map[string]string{
		"lone high surrogate":                  `B\ud83d`,
		"lone low surrogate":                   `B\ude00`,
		"reversed surrogate pair":              `B\ude00\ud83d`,
		"high surrogate then plain character":  `B\ud83dx`,
		"high surrogate then escaped BMP char": `B\ud83d`,
	}
	for name, badID := range badIDs {
		t.Run(name, func(t *testing.T) {
			scenarios := map[string]bool{
				"existing registry stays untouched": true,
				"missing registry stays missing":    false,
			}
			for scenario, registryExists := range scenarios {
				t.Run(scenario, func(t *testing.T) {
					dir := t.TempDir()
					registry := filepath.Join(dir, "reg.json")
					manifest := filepath.Join(dir, "in.json")
					// Record 1 confirms the registered batch, record 2 is a
					// legal new id carrying a genuine U+FFFD; neither may be
					// left behind when record 3 fails.
					manifestContent := `[
  {"batch":"B0","product":"P-0","quantity":5,"unit":"box"},
  {"batch":"NEW-�","product":"P","quantity":1,"unit":"kg"},
  {"batch":"` + badID + `","product":"P","quantity":2,"unit":"kg"}
]`
					writeFile(t, manifest, manifestContent)
					manifestBytes := readFileSnapshot(t, manifest)

					var regContent []byte
					var regPinned time.Time
					if registryExists {
						writeFile(t, registry, `{"version":1,"batches":[{"batch":"B0","product":"P-0","quantity":5,"unit":"box"}]}`)
						regContent, regPinned = pinRegistry(t, registry)
					}

					var stdout bytes.Buffer
					err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout)
					if err == nil {
						t.Fatal("a bad surrogate escape must reject the whole import")
					}
					msg := err.Error()
					for _, want := range []string{manifest, "record 3", `"batch"`, "UTF-8"} {
						if !strings.Contains(msg, want) {
							t.Errorf("error must mention %q: %v", want, msg)
						}
					}
					// The undecodable id is never quoted back — neither as a
					// substituted U+FFFD nor as its raw escape spelling.
					if strings.Contains(msg, "�") {
						t.Errorf("error must not substitute U+FFFD for the bad id: %v", msg)
					}
					if strings.Contains(msg, `\u`) {
						t.Errorf("error must not echo the raw escape spelling: %v", msg)
					}
					if stdout.Len() != 0 {
						t.Fatalf("a rejected import must print no success result, got %q", stdout.String())
					}
					if registryExists {
						assertRegistryUntouched(t, registry, regContent, regPinned)
					} else if _, statErr := os.Stat(registry); !os.IsNotExist(statErr) {
						t.Fatal("a rejected first import must not create the registry")
					}
					assertFileUnchanged(t, manifest, manifestBytes)
				})
			}
		})
	}
}
