package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// End-to-end regression for the member-fault precedence rule through the two
// real commands. A record that carries BOTH an unknown and a duplicated field
// must fail with a precise reason — and the first-reported problem differs by
// source even though both sources reject the record:
//
//   - a record read from the registry file names the duplicate first;
//   - a record read from the import manifest names the first anomaly in
//     source order (so an unknown field ahead of the repeat stays reported).
//
// Every failure must keep the source file path, the 1-based record position
// (here 2, after a legal record 1), the exact decoded field name and the
// batch id (trimmed for the manifest, verbatim for the registry), print no
// success output, leave the existing registry byte-for-byte untouched, and
// never write the read-only manifest.

// cliEscKey renders an ASCII name entirely as JSON \u00XX escapes, so the
// fixture JSON literally uses escaped key spellings.
func cliEscKey(name string) string {
	var b strings.Builder
	for _, r := range name {
		fmt.Fprintf(&b, `\u%04x`, r)
	}
	return b.String()
}

// readFileSnapshot returns a file's bytes so a later assertion can prove the
// supposedly read-only input was not rewritten.
func readFileSnapshot(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assertFileUnchanged(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("file %s was modified:\nbefore: %s\nafter:  %s", path, want, got)
	}
}

// legalManifestRecord1 is a valid first record; every offending record is
// record 2, so the reported position must be 2 and must never blame B1.
const legalManifestRecord1 = `{"batch":"B1","product":"P","quantity":1,"unit":"kg"}`

func TestBatchImportCLIMemberFaultPrecedence(t *testing.T) {
	const existingRegistry = `{"version":1,"batches":[{"batch":"B0","product":"P0","quantity":9,"unit":"box"}]}`
	cases := []struct {
		name      string
		record    string
		wantField string
		dup       bool
		wantBatch string // "" when the error must cite no batch id
	}{
		{
			"unknown supplier before duplicate product reports supplier unknown",
			`{"batch":"B2","supplier":"S","product":"P","product":"Q","quantity":1,"unit":"kg"}`,
			"supplier", false, "B2",
		},
		{
			"duplicate product moved ahead of unknown reports product duplicate",
			`{"batch":"B2","product":"P","product":"Q","quantity":1,"unit":"kg","supplier":"S"}`,
			"product", true, "B2",
		},
		{
			"escaped product duplicates direct name with equal values",
			`{"batch":"B2","` + cliEscKey("product") + `":"P","product":"P","quantity":1,"unit":"kg","supplier":"S"}`,
			"product", true, "B2",
		},
		{
			"escaped unknown supplier ahead of a later product repeat is still reported first",
			`{"batch":"B2","` + cliEscKey("supplier") + `":"S","product":"P","product":"P","quantity":1,"unit":"kg"}`,
			"supplier", false, "B2",
		},
		{
			"padded batch id is trimmed in the error",
			`{"batch":" B2 ","supplier":"S","product":"P","product":"P","quantity":1,"unit":"kg"}`,
			"supplier", false, "B2",
		},
		{
			"duplicated batch with equal values cites no id",
			`{"batch":"B2","batch":"B2","product":"P","quantity":1,"unit":"kg","supplier":"S"}`,
			"batch", true, "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			registry := filepath.Join(dir, "reg.json")
			manifest := filepath.Join(dir, "in.json")
			writeFile(t, registry, existingRegistry)
			manifestContent := `[` + legalManifestRecord1 + `,` + tc.record + `]`
			writeFile(t, manifest, manifestContent)
			regBytes, regPinned := pinRegistry(t, registry)
			manifestBytes := readFileSnapshot(t, manifest)

			var stdout bytes.Buffer
			err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout)
			if err == nil {
				t.Fatal("the record with an unknown and a duplicated field must be rejected")
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout must carry no success output, got %q", stdout.String())
			}
			msg := err.Error()
			for _, want := range []string{
				manifest,
				"record 2",
				strconv.Quote(tc.wantField),
			} {
				if !strings.Contains(msg, want) {
					t.Fatalf("error must contain %q: %v", want, msg)
				}
			}
			if strings.Contains(msg, "record 1") {
				t.Errorf("the legal first record must not be blamed: %v", msg)
			}
			if tc.dup {
				if !strings.Contains(msg, "more than once") {
					t.Errorf("error must report a DUPLICATE %q: %v", tc.wantField, msg)
				}
				if strings.Contains(msg, "unknown field") {
					t.Errorf("error must not mislabel the duplicate as unknown: %v", msg)
				}
			} else {
				if !strings.Contains(msg, "unknown field") {
					t.Errorf("error must report an UNKNOWN %q: %v", tc.wantField, msg)
				}
				if strings.Contains(msg, "more than once") {
					t.Errorf("error must not mislabel the unknown field as a duplicate: %v", msg)
				}
			}
			if tc.wantBatch != "" {
				if !strings.Contains(msg, "batch "+strconv.Quote(tc.wantBatch)) {
					t.Fatalf("error must name batch %q: %v", tc.wantBatch, msg)
				}
			} else if strings.Contains(msg, "batch \"") {
				t.Fatalf("error must not pick one of the duplicated batch values: %v", msg)
			}
			assertRegistryUntouched(t, registry, regBytes, regPinned)
			assertFileUnchanged(t, manifest, manifestBytes)
			if batches := readStoredBatches(t, registry); len(batches) != 1 || batches[0].Batch != "B0" {
				t.Fatalf("the rejected import must leave only the stored batch: %v", batches)
			}
		})
	}
}

func TestBatchRegisterCLIMemberFaultInRegistry(t *testing.T) {
	cases := []struct {
		name      string
		record    string
		wantField string
		dup       bool
		wantBatch string // "" when the error must cite no batch id
	}{
		{
			// The registry rule diverges from the manifest rule: even though
			// supplier is the first unknown member in source order, the
			// duplicated product is reported first.
			"unknown supplier before duplicate product reports product duplicate",
			`{"batch":"B2","supplier":"S","product":"P","product":"Q","quantity":1,"unit":"kg"}`,
			"product", true, "B2",
		},
		{
			"duplicate product ahead of unknown reports product duplicate",
			`{"batch":"B2","product":"P","product":"Q","quantity":1,"unit":"kg","supplier":"S"}`,
			"product", true, "B2",
		},
		{
			"escaped duplicate product outranks a later or earlier unknown",
			`{"batch":"B2","supplier":"S","product":"P","` + cliEscKey("product") + `":"P","quantity":1,"unit":"kg"}`,
			"product", true, "B2",
		},
		{
			"earliest second occurrence (unit) wins over unknown and later product repeat",
			`{"batch":"B2","product":"P","unit":"kg","supplier":"S","unit":"g","product":"Q","quantity":1}`,
			"unit", true, "B2",
		},
		{
			"padded batch id is kept verbatim in the registry error",
			`{"batch":" B2 ","supplier":"S","product":"P","product":"P","quantity":1,"unit":"kg"}`,
			"product", true, " B2 ",
		},
		{
			"duplicated batch with equal values cites no id",
			`{"batch":"B2","batch":"B2","product":"P","quantity":1,"unit":"kg","supplier":"S"}`,
			"batch", true, "",
		},
		{
			"no duplicate anywhere reports the first unknown field",
			`{"batch":"B2","alpha":1,"beta":2,"product":"P","quantity":1,"unit":"kg"}`,
			"alpha", false, "B2",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			registry := filepath.Join(dir, "reg.json")
			registryContent := `{"version":1,"batches":[` + legalManifestRecord1 + `,` + tc.record + `]}`
			writeFile(t, registry, registryContent)
			regBytes, regPinned := pinRegistry(t, registry)

			var stdout bytes.Buffer
			err := runBatchRegister([]string{
				"--registry", registry, "--batch", "NEW", "--product", "P",
				"--quantity", "1", "--unit", "kg",
			}, &stdout)
			if err == nil {
				t.Fatal("batch-register must refuse a registry holding the malformed record")
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout must carry no success output, got %q", stdout.String())
			}
			msg := err.Error()
			for _, want := range []string{
				registry,
				"record 2",
				strconv.Quote(tc.wantField),
			} {
				if !strings.Contains(msg, want) {
					t.Fatalf("error must contain %q: %v", want, msg)
				}
			}
			if strings.Contains(msg, "record 1") {
				t.Errorf("the legal first record must not be blamed: %v", msg)
			}
			if tc.dup {
				if !strings.Contains(msg, "more than once") {
					t.Errorf("error must report a DUPLICATE %q: %v", tc.wantField, msg)
				}
				if strings.Contains(msg, "unknown field") {
					t.Errorf("error must not mislabel the duplicate as unknown: %v", msg)
				}
			} else {
				if !strings.Contains(msg, "unknown field") {
					t.Errorf("error must report an UNKNOWN %q: %v", tc.wantField, msg)
				}
				if strings.Contains(msg, "more than once") {
					t.Errorf("error must not mislabel the unknown field as a duplicate: %v", msg)
				}
			}
			if tc.wantBatch != "" {
				if !strings.Contains(msg, "(batch "+strconv.Quote(tc.wantBatch)+")") {
					t.Fatalf("error must name batch %q verbatim: %v", tc.wantBatch, msg)
				}
			} else if strings.Contains(msg, "batch \"") {
				t.Fatalf("error must not pick one of the duplicated batch values: %v", msg)
			}
			assertRegistryUntouched(t, registry, regBytes, regPinned)
		})
	}
}

// The positive controls: escaped key spellings on their own are legal, and a
// normal two-record manifest still imports. These guard against an
// implementation that rejects every record (and would still pass the
// failure-only assertions above).
func TestBatchImportCLIEscapedAndPlainRecordsStillAccepted(t *testing.T) {
	t.Run("escaped keys import, create and confirm a duplicate", func(t *testing.T) {
		dir := t.TempDir()
		registry := filepath.Join(dir, "reg.json")
		manifest := filepath.Join(dir, "in.json")
		manifestContent := `[
  {"` + cliEscKey("batch") + `":" B1 ","` + cliEscKey("product") + `":"P-7","quantity":120,"` + cliEscKey("unit") + `":"kg"},
  {"batch":"B1","product":"P-7","quantity":120,"unit":"kg"}
]`
		writeFile(t, manifest, manifestContent)
		manifestBytes := readFileSnapshot(t, manifest)

		var stdout bytes.Buffer
		if err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout); err != nil {
			t.Fatalf("legal escaped-key records must import: %v", err)
		}
		var out importOutput
		if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &out); err != nil {
			t.Fatalf("stdout is not the expected JSON: %v (%q)", err, stdout.String())
		}
		if len(out.Results) != 2 ||
			out.Results[0].Status != "created" || out.Results[0].Batch != "B1" ||
			out.Results[1].Status != "duplicate" || out.Results[1].Batch != "B1" {
			t.Fatalf("unexpected results: %s", stdout.String())
		}
		batches := readStoredBatches(t, registry)
		if len(batches) != 1 || batches[0].Batch != "B1" || batches[0].Product != "P-7" ||
			batches[0].Quantity != 120 || batches[0].Unit != "kg" {
			t.Fatalf("escaped-key record not stored decoded and trimmed: %v", batches)
		}
		assertFileUnchanged(t, manifest, manifestBytes)
	})

	t.Run("plain legal records after the rejected shape still import", func(t *testing.T) {
		dir := t.TempDir()
		registry := filepath.Join(dir, "reg.json")
		manifest := filepath.Join(dir, "in.json")
		manifestContent := `[` + legalManifestRecord1 + `,
  {"batch":"B2","product":"P2","quantity":2,"unit":"box"}]`
		writeFile(t, manifest, manifestContent)

		var stdout bytes.Buffer
		if err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout); err != nil {
			t.Fatalf("the repaired two-record manifest must import: %v", err)
		}
		var out importOutput
		if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &out); err != nil {
			t.Fatalf("stdout is not the expected JSON: %v (%q)", err, stdout.String())
		}
		if len(out.Results) != 2 ||
			out.Results[0].Status != "created" || out.Results[1].Status != "created" {
			t.Fatalf("both legal records must be created: %s", stdout.String())
		}
		batches := readStoredBatches(t, registry)
		if len(batches) != 2 || batches[0].Batch != "B1" || batches[1].Batch != "B2" {
			t.Fatalf("both legal batches must be stored in order: %v", batches)
		}
	})
}
