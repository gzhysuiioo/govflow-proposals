package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The whole manifest is validated before any batch content is compared, so an
// invalid record is always reported before a content conflict — even when the
// conflicting record sits earlier in the manifest. The error names the
// manifest path, the invalid record's 1-based position, the offending field
// and the reason; stdout stays empty, the exit is a failure, and neither file
// changes. Moving the conflict and the invalid record around must not change
// the invalid-first rule, only the position the error reports.
func TestBatchImportCLIInvalidRecordReportedBeforeConflict(t *testing.T) {
	// The registry layout is deliberately not what Save would write (compact,
	// two records, specific order) so any rewrite is visible in the bytes.
	registryContent := `{"version":1,"batches":[{"batch":"B1","product":"P1","quantity":10,"unit":"kg"},{"batch":"B0","product":"P0","quantity":5,"unit":"box"}]}`
	cases := map[string]struct {
		manifest   string
		wantPos    string
		wantField  string
		wantReason string
		wantBatch  string // empty: the error must not cite any batch id
		notWant    []string
	}{
		"registry conflict earlier, quantity string later": {
			manifest: `[
  {"batch":"B1","product":"P1","quantity":11,"unit":"kg"},
  {"batch":"B2","product":"P2","quantity":"2","unit":"kg"}
]`,
			wantPos: "record 2", wantField: "quantity", wantReason: "must be a JSON integer", wantBatch: "B2",
		},
		"manifest-internal conflict earlier, blank unit later": {
			manifest: `[
  {"batch":"B1","product":"P1","quantity":10,"unit":"kg"},
  {"batch":"B1","product":"P1","quantity":11,"unit":"kg"},
  {"batch":"B2","product":"P2","quantity":2,"unit":"  "}
]`,
			wantPos: "record 3", wantField: "unit", wantReason: "must not be blank", wantBatch: "B2",
		},
		"invalid record first, registry conflict later": {
			manifest: `[
  {"batch":"B2","product":"P2","quantity":"2","unit":"kg"},
  {"batch":"B1","product":"P1","quantity":11,"unit":"kg"}
]`,
			wantPos: "record 1", wantField: "quantity", wantReason: "must be a JSON integer", wantBatch: "B2",
		},
		"invalid record first, manifest-internal conflict later": {
			manifest: `[
  {"batch":"B2","product":"P2","quantity":2,"unit":"\t"},
  {"batch":"B1","product":"P1","quantity":10,"unit":"kg"},
  {"batch":"B1","product":"P1","quantity":11,"unit":"kg"}
]`,
			wantPos: "record 1", wantField: "unit", wantReason: "must not be blank", wantBatch: "B2",
		},
		"several invalid records, earliest in manifest order reported": {
			manifest: `[
  {"batch":"B1","product":"P1","quantity":11,"unit":"kg"},
  {"batch":"B2","product":"P2","quantity":"2","unit":"kg"},
  {"batch":"B3","product":"P3","quantity":3,"unit":" "}
]`,
			wantPos: "record 2", wantField: "quantity", wantReason: "must be a JSON integer", wantBatch: "B2",
			notWant: []string{"record 3"},
		},
		"invalid record without a citable batch id": {
			manifest: `[
  {"batch":"B1","product":"P1","quantity":11,"unit":"kg"},
  {"batch":"  ","product":"P2","quantity":2,"unit":"kg"}
]`,
			wantPos: "record 2", wantField: "batch", wantReason: "must not be blank", wantBatch: "",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			registry := filepath.Join(dir, "reg.json")
			manifest := filepath.Join(dir, "in.json")
			writeFile(t, registry, registryContent)
			writeFile(t, manifest, tc.manifest)
			content, pinned := pinRegistry(t, registry)

			var stdout bytes.Buffer
			err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout)
			if err == nil {
				t.Fatal("a manifest holding an invalid record must fail (non-zero exit)")
			}
			msg := err.Error()
			for _, want := range []string{manifest, tc.wantPos, strconv.Quote(tc.wantField), tc.wantReason} {
				if !strings.Contains(msg, want) {
					t.Fatalf("error must contain %q: %v", want, msg)
				}
			}
			if tc.wantBatch != "" {
				if !strings.Contains(msg, "batch "+strconv.Quote(tc.wantBatch)) {
					t.Fatalf("error must cite batch %q: %v", tc.wantBatch, msg)
				}
			} else if strings.Contains(msg, "batch \"") {
				t.Fatalf("error must not cite a batch id that is not unambiguously valid: %v", msg)
			}
			if strings.Contains(msg, "conflicts with") {
				t.Fatalf("the invalid record must be reported before any content conflict: %v", msg)
			}
			for _, unwanted := range tc.notWant {
				if strings.Contains(msg, unwanted) {
					t.Fatalf("error must not contain %q: %v", unwanted, msg)
				}
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout must stay empty on failure, got %q", stdout.String())
			}
			assertRegistryUntouched(t, registry, content, pinned)
			if after, _ := os.ReadFile(registry); string(after) != registryContent {
				t.Fatalf("registry layout or record order changed: %s", after)
			}
			if after, _ := os.ReadFile(manifest); string(after) != tc.manifest {
				t.Fatal("the read-only manifest was modified")
			}
			batches := readStoredBatches(t, registry)
			if len(batches) != 2 || batches[0].Batch != "B1" || batches[1].Batch != "B0" {
				t.Fatalf("rejected import left new batches behind or reordered records: %v", batches)
			}
		})
	}
}

// Once the invalid record is fixed, the same manifest must surface the
// content conflict that the invalid record previously outranked: the error
// names the batch, the differing field(s) and the conflict's source — the
// registered record, or the batch's first manifest occurrence. Still no
// success output and no new batch is saved.
func TestBatchImportCLIFixingInvalidRecordExposesConflict(t *testing.T) {
	t.Run("conflict with the registered record", func(t *testing.T) {
		dir := t.TempDir()
		registry := filepath.Join(dir, "reg.json")
		manifest := filepath.Join(dir, "in.json")
		registryContent := `{"version":1,"batches":[{"batch":"B1","product":"P1","quantity":10,"unit":"kg"}]}`
		writeFile(t, registry, registryContent)
		content, pinned := pinRegistry(t, registry)

		// Step 1: the invalid second record is reported, not the conflict.
		writeFile(t, manifest, `[
  {"batch":"B1","product":"P1","quantity":11,"unit":"kg"},
  {"batch":"B2","product":"P2","quantity":"2","unit":"kg"}
]`)
		var stdout bytes.Buffer
		err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout)
		if err == nil {
			t.Fatal("the invalid record must fail the import")
		}
		if msg := err.Error(); !strings.Contains(msg, "record 2") || strings.Contains(msg, "conflicts with") {
			t.Fatalf("step 1 must report the invalid record 2, got: %v", msg)
		}
		if stdout.Len() != 0 {
			t.Fatalf("stdout must stay empty, got %q", stdout.String())
		}

		// Step 2: only the invalid record is fixed; the conflict remains and
		// must now be the reported error.
		fixedManifest := `[
  {"batch":"B1","product":"P1","quantity":11,"unit":"kg"},
  {"batch":"B2","product":"P2","quantity":2,"unit":"kg"}
]`
		writeFile(t, manifest, fixedManifest)
		stdout.Reset()
		err = runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout)
		if err == nil {
			t.Fatal("the remaining conflict must fail the import")
		}
		msg := err.Error()
		for _, want := range []string{"record 1", strconv.Quote("B1"), "quantity", "conflicts with the registered record"} {
			if !strings.Contains(msg, want) {
				t.Fatalf("error must contain %q: %v", want, msg)
			}
		}
		if stdout.Len() != 0 {
			t.Fatalf("stdout must stay empty, got %q", stdout.String())
		}
		assertRegistryUntouched(t, registry, content, pinned)
		if after, _ := os.ReadFile(registry); string(after) != registryContent {
			t.Fatalf("registry changed: %s", after)
		}
		if after, _ := os.ReadFile(manifest); string(after) != fixedManifest {
			t.Fatal("the read-only manifest was modified")
		}
		if batches := readStoredBatches(t, registry); len(batches) != 1 || batches[0].Batch != "B1" {
			t.Fatalf("no new batch may be saved: %v", batches)
		}
	})

	t.Run("conflict with an earlier manifest record", func(t *testing.T) {
		dir := t.TempDir()
		registry := filepath.Join(dir, "reg.json")
		manifest := filepath.Join(dir, "in.json")
		registryContent := `{"version":1,"batches":[{"batch":"B0","product":"P0","quantity":5,"unit":"box"}]}`
		writeFile(t, registry, registryContent)
		content, pinned := pinRegistry(t, registry)

		// Step 1: records 1 and 2 conflict with each other, but the invalid
		// record 3 is reported first.
		writeFile(t, manifest, `[
  {"batch":"B1","product":"P1","quantity":10,"unit":"kg"},
  {"batch":"B1","product":"P1","quantity":11,"unit":"kg"},
  {"batch":"B2","product":"P2","quantity":"2","unit":"kg"}
]`)
		var stdout bytes.Buffer
		err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout)
		if err == nil {
			t.Fatal("the invalid record must fail the import")
		}
		if msg := err.Error(); !strings.Contains(msg, "record 3") || strings.Contains(msg, "conflicts with") {
			t.Fatalf("step 1 must report the invalid record 3, got: %v", msg)
		}
		if stdout.Len() != 0 {
			t.Fatalf("stdout must stay empty, got %q", stdout.String())
		}

		// Step 2: with record 3 fixed, the manifest-internal conflict
		// surfaces, sourced at the batch's first manifest occurrence.
		fixedManifest := `[
  {"batch":"B1","product":"P1","quantity":10,"unit":"kg"},
  {"batch":"B1","product":"P1","quantity":11,"unit":"kg"},
  {"batch":"B2","product":"P2","quantity":2,"unit":"kg"}
]`
		writeFile(t, manifest, fixedManifest)
		stdout.Reset()
		err = runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout)
		if err == nil {
			t.Fatal("the remaining conflict must fail the import")
		}
		msg := err.Error()
		for _, want := range []string{"record 2", strconv.Quote("B1"), "quantity", "conflicts with manifest record 1"} {
			if !strings.Contains(msg, want) {
				t.Fatalf("error must contain %q: %v", want, msg)
			}
		}
		if stdout.Len() != 0 {
			t.Fatalf("stdout must stay empty, got %q", stdout.String())
		}
		assertRegistryUntouched(t, registry, content, pinned)
		if after, _ := os.ReadFile(registry); string(after) != registryContent {
			t.Fatalf("registry changed: %s", after)
		}
		if after, _ := os.ReadFile(manifest); string(after) != fixedManifest {
			t.Fatal("the read-only manifest was modified")
		}
		if batches := readStoredBatches(t, registry); len(batches) != 1 || batches[0].Batch != "B0" {
			t.Fatalf("no new batch may be saved: %v", batches)
		}
	})
}
