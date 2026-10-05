package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// CLI-level guard for the mixed-fault reporting rules covered package-internally
// in batchreg_mixed_faults_test.go: one record carrying both an unknown field
// and a duplicated field must fail the import with the source-specific reason —
// the manifest names whichever anomaly comes first in source order, the
// registry file names the duplicate — while stdout stays empty, the existing
// registry keeps its bytes and mtime, and the read-only manifest is untouched.

// assertFaultWording checks that msg reports the expected fault kind for
// field: the two kinds must stay distinguishable in the user-facing text.
func assertFaultWording(t *testing.T, msg, field, kind string) {
	t.Helper()
	if !strings.Contains(msg, strconv.Quote(field)) {
		t.Fatalf("error must name field %q: %v", field, msg)
	}
	switch kind {
	case "duplicate":
		if !strings.Contains(msg, "more than once") || strings.Contains(msg, "unknown field") {
			t.Fatalf("error must report a duplicate field, not an unknown one: %v", msg)
		}
	case "unknown":
		if !strings.Contains(msg, "unknown field") || strings.Contains(msg, "more than once") {
			t.Fatalf("error must report an unknown field, not a duplicate: %v", msg)
		}
	default:
		t.Fatalf("unknown kind %q", kind)
	}
}

// A manifest record holding both an unknown field and a duplicated field
// fails the whole import: the error names the manifest file, the 1-based
// record position, the trimmed batch id and the anomaly that comes first in
// source order — never the later one. Nothing is created or rewritten.
func TestBatchImportCLIMixedFaultRecordFailsAndPreserves(t *testing.T) {
	cases := map[string]struct {
		record    string
		wantField string
		wantKind  string
	}{
		// Unknown supplier ahead of the product repeat: the manifest rule
		// reports the unknown field, not the later duplicate.
		"unknown reported before later duplicate": {`{"batch":" B-2 ","supplier":"S","product":"P","product":"P","quantity":1,"unit":"kg"}`, "supplier", "unknown"},
		// The product repeat ahead of supplier: the duplicate is first in
		// source order and gets reported.
		"duplicate reported when it comes first": {`{"batch":"B-2","product":"P","product":"P","supplier":"S","quantity":1,"unit":"kg"}`, "product", "duplicate"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			registry := filepath.Join(dir, "reg.json")
			manifest := filepath.Join(dir, "in.json")
			writeFile(t, registry, `{"version":1,"batches":[{"batch":"B-1","product":"P","quantity":1,"unit":"kg"}]}`)
			manifestContent := `[
  {"batch":"B-1","product":"P","quantity":1,"unit":"kg"},
  ` + tc.record + `
]`
			writeFile(t, manifest, manifestContent)
			content, pinned := pinRegistry(t, registry)

			var stdout bytes.Buffer
			err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout)
			if err == nil {
				t.Fatal("a record with an unknown and a duplicated field must be rejected")
			}
			msg := err.Error()
			for _, want := range []string{manifest, "record 2", "batch " + strconv.Quote("B-2")} {
				if !strings.Contains(msg, want) {
					t.Fatalf("error must contain %q: %v", want, msg)
				}
			}
			assertFaultWording(t, msg, tc.wantField, tc.wantKind)
			if stdout.Len() != 0 {
				t.Fatalf("stdout must stay empty on failure, got %q", stdout.String())
			}
			assertRegistryUntouched(t, registry, content, pinned)
			if after, _ := os.ReadFile(manifest); string(after) != manifestContent {
				t.Fatal("the read-only manifest was modified")
			}
			if batches := readStoredBatches(t, registry); len(batches) != 1 || batches[0].Batch != "B-1" {
				t.Fatalf("rejected import left new batches behind: %v", batches)
			}
		})
	}
}

// The same mixed record inside the registry file blocks both commands: the
// registry rule reports the duplicated field even though the unknown one
// appears earlier in the record, naming the file, the 1-based position, the
// batch id and the field. Neither file changes. A clean registry accepting
// the same manifest is the control that the protection is the fault rule,
// not a blanket refusal.
func TestMixedFaultRegistryBlocksBothCommands(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "reg.json")
	manifest := filepath.Join(dir, "in.json")
	// Record 2 carries an unknown supplier before the product repeat; the
	// registry still reports the duplicate, at record 2, citing B-2.
	registryContent := `{"version":1,"batches":[` +
		`{"batch":"B-1","product":"P","quantity":1,"unit":"kg"},` +
		`{"batch":"B-2","supplier":"S","product":"P","product":"P","quantity":1,"unit":"kg"}]}`
	writeFile(t, registry, registryContent)
	manifestContent := `[{"batch":"NEW","product":"P","quantity":1,"unit":"kg"}]`
	writeFile(t, manifest, manifestContent)
	content, pinned := pinRegistry(t, registry)

	assertBlocked := func(err error, stdout *bytes.Buffer) {
		t.Helper()
		if err == nil {
			t.Fatal("a registry holding a mixed-fault record must be rejected")
		}
		msg := err.Error()
		for _, want := range []string{registry, "record 2", strconv.Quote("B-2")} {
			if !strings.Contains(msg, want) {
				t.Fatalf("error must contain %q: %v", want, msg)
			}
		}
		assertFaultWording(t, msg, "product", "duplicate")
		if stdout.Len() != 0 {
			t.Fatalf("stdout must stay empty, got %q", stdout.String())
		}
	}

	var stdout bytes.Buffer
	err := runBatchRegister([]string{
		"--registry", registry, "--batch", "NEW", "--product", "P",
		"--quantity", "1", "--unit", "kg",
	}, &stdout)
	assertBlocked(err, &stdout)

	stdout.Reset()
	err = runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout)
	assertBlocked(err, &stdout)

	assertRegistryUntouched(t, registry, content, pinned)
	if after, _ := os.ReadFile(manifest); string(after) != manifestContent {
		t.Fatal("the read-only manifest was modified")
	}

	// Control: a clean registry plus the same manifest imports fine, so the
	// rejections above are the mixed-fault rule firing, not a blanket refusal.
	cleanRegistry := filepath.Join(dir, "clean.json")
	writeFile(t, cleanRegistry, `{"version":1,"batches":[{"batch":"B-1","product":"P","quantity":1,"unit":"kg"}]}`)
	stdout.Reset()
	if err := runBatchImport([]string{"--registry", cleanRegistry, "--input", manifest}, &stdout); err != nil {
		t.Fatalf("a clean registry must accept the same manifest: %v", err)
	}
	if !strings.Contains(stdout.String(), `"status":"created"`) {
		t.Fatalf("expected a created result, got %q", stdout.String())
	}
	if batches := readStoredBatches(t, cleanRegistry); len(batches) != 2 || batches[1].Batch != "NEW" {
		t.Fatalf("control import did not append the new batch: %v", batches)
	}
}
