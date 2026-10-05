package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gzhysuiioo/govflow-proposals/govflow/batchreg"
)

// This file guards the all-or-nothing promise of batch-import at the command
// entry when the SAVE of the merged registry fails — not the manifest parse,
// not a batch conflict, and not a neighboring unusable path. The manifest in
// every scenario is fully legal: every field valid, no conflicting record,
// mixing exact duplicates of stored batches with several genuinely new ones,
// so the import reaches a real save and only the save itself fails. Two
// mid-save failure points are injected deterministically through
// batchreg.SetSaveStepsForTest, on the save's actual target:
//
//   - the content write into the temporary file stops before the new content
//     is complete;
//   - the new content is fully prepared on disk, but the final replacement of
//     the registry file fails.
//
// Whichever step fails, the command must report failure to its caller (the
// returned error is what main prints to stderr as "govflow: ..." before
// exiting non-zero), the message must name the target registry path and the
// save's own reason, and stdout must stay empty. An existing registry keeps
// its exact bytes and modification time and still reads back with the
// original batches, fields and order — none of the manifest's new batches
// may remain. A registry that did not exist must still not exist: no empty
// registry, no file holding only the first few batches. The read-only
// manifest keeps its bytes and no temporary registry file survives. A
// success counterpart with the very same manifest proves the results the
// user sees match the batches that actually land in the file. The injections
// need no disk fault, no special filesystem and no missing permissions, so
// they reproduce offline under any account, root included.

// importSaveFailureRegistryJSON is the existing registry every existing-file
// scenario starts from: several batches whose order and fields must survive.
const importSaveFailureRegistryJSON = `{"version":1,"batches":[` +
	`{"batch":"OLD-1","product":"苹果","quantity":10,"unit":"kg"},` +
	`{"batch":"OLD-2","product":"P-2","quantity":20,"unit":"box"},` +
	`{"batch":"OLD-3","product":"P-3","quantity":30,"unit":"m"}]}`

// importSaveFailureManifest is the legal, conflict-free manifest imported in
// every scenario of this file, failure and success alike: an exact duplicate
// of the stored OLD-2 record between several new batches.
const importSaveFailureManifest = `[
  {"batch":"NEW-1","product":"新产品","quantity":99,"unit":"箱"},
  {"batch":"OLD-2","product":"P-2","quantity":20,"unit":"box"},
  {"batch":"NEW-2","product":"P-new","quantity":40,"unit":"pallet"},
  {"batch":"NEW-3","product":"P-x","quantity":7,"unit":"kg"}
]`

// importSaveFailureWantResults is the success output expected for the
// manifest above, in manifest order.
var importSaveFailureWantResults = []registerResult{
	{Batch: "NEW-1", Product: "新产品", Quantity: 99, Unit: "箱", Status: "created"},
	{Batch: "OLD-2", Product: "P-2", Quantity: 20, Unit: "box", Status: "duplicate"},
	{Batch: "NEW-2", Product: "P-new", Quantity: 40, Unit: "pallet", Status: "created"},
	{Batch: "NEW-3", Product: "P-x", Quantity: 7, Unit: "kg", Status: "created"},
}

// importSaveFailureWantStored is the full registry content after a
// successful import: the stored batches first, in their original order, then
// every new batch in first-occurrence order.
var importSaveFailureWantStored = []registerResult{
	{Batch: "OLD-1", Product: "苹果", Quantity: 10, Unit: "kg"},
	{Batch: "OLD-2", Product: "P-2", Quantity: 20, Unit: "box"},
	{Batch: "OLD-3", Product: "P-3", Quantity: 30, Unit: "m"},
	{Batch: "NEW-1", Product: "新产品", Quantity: 99, Unit: "箱"},
	{Batch: "NEW-2", Product: "P-new", Quantity: 40, Unit: "pallet"},
	{Batch: "NEW-3", Product: "P-x", Quantity: 7, Unit: "kg"},
}

// importSaveFailureOldBatches is what a failed import must leave readable in
// an existing registry: exactly the original records, in order.
var importSaveFailureOldBatches = importSaveFailureWantStored[:3]

// injectSaveWriteFailure makes the content write into the temporary file
// stop halfway through the new content and report cause. The returned
// probe reports whether the save really reached the write step.
func injectSaveWriteFailure(t *testing.T, cause error) (reached *bool) {
	t.Helper()
	hit := false
	restore := batchreg.SetSaveStepsForTest(func(f *os.File, data []byte) (int, error) {
		hit = true
		// Part of the new content genuinely reaches the temporary file before
		// the write fails, so the save aborts with the write incomplete.
		half := len(data) / 2
		if _, err := f.Write(data[:half]); err != nil {
			return 0, err
		}
		return half, cause
	}, nil)
	t.Cleanup(restore)
	return &hit
}

// injectSaveRenameFailure lets the save prepare the full new content and
// then refuses the replacement of target with cause. Before failing, the
// injection proves the new content was complete by checking that the file it
// is asked to move holds every marker. The returned probe reports whether
// the save really reached the replacement step.
func injectSaveRenameFailure(t *testing.T, cause error, target string, markers ...string) (reached *bool) {
	t.Helper()
	hit := false
	restore := batchreg.SetSaveStepsForTest(nil, func(oldpath, newpath string) error {
		hit = true
		if newpath != target {
			t.Errorf("rename must target the actual registry path %q, got %q", target, newpath)
		}
		prepared, err := os.ReadFile(oldpath)
		if err != nil {
			t.Errorf("the prepared content must be readable before the replacement: %v", err)
			return cause
		}
		for _, marker := range markers {
			if !bytes.Contains(prepared, []byte(marker)) {
				t.Errorf("prepared content is missing %q; the save did not get the full new table ready: %s", marker, prepared)
			}
		}
		return cause
	})
	t.Cleanup(restore)
	return &hit
}

// saveFailureInjections are the two mid-save failure points, shared by the
// existing-registry and the missing-registry scenarios. Each installs its
// injection against the actual registry path and returns the probe reporting
// whether the save really reached the failing step.
func saveFailureInjections() map[string]func(t *testing.T, cause error, target string, markers ...string) *bool {
	return map[string]func(t *testing.T, cause error, target string, markers ...string) *bool{
		"content write incomplete": func(t *testing.T, cause error, target string, markers ...string) *bool {
			return injectSaveWriteFailure(t, cause)
		},
		"replacement refused": func(t *testing.T, cause error, target string, markers ...string) *bool {
			return injectSaveRenameFailure(t, cause, target, markers...)
		},
	}
}

// assertImportSaveFailed checks the caller-visible contract of a failed
// save: a non-nil error (what main turns into a non-zero exit code and a
// stderr line) that carries the injected cause and names both the target
// registry path and the concrete save reason, with nothing on stdout.
func assertImportSaveFailed(t *testing.T, err, cause error, registry string, stdout *bytes.Buffer) {
	t.Helper()
	if err == nil {
		t.Fatal("a failed save must make batch-import report failure to its caller")
	}
	if !errors.Is(err, cause) {
		t.Errorf("error %v must carry the save failure cause %v", err, cause)
	}
	msg := err.Error()
	if !strings.Contains(msg, registry) {
		t.Errorf("error %q must name the target registry path %q", msg, registry)
	}
	if !strings.Contains(msg, cause.Error()) {
		t.Errorf("error %q must state the save failure reason %q", msg, cause.Error())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout must stay empty when the save fails, got %q", stdout.String())
	}
}

// assertOnlyFiles fails unless dir holds exactly the named entries — in
// particular no leftover temporary registry file from the aborted save.
func assertOnlyFiles(t *testing.T, dir string, names ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]bool, len(entries))
	for _, e := range entries {
		got[e.Name()] = true
	}
	for _, name := range names {
		if !got[name] {
			t.Fatalf("%s is missing from %s; directory holds %v", name, dir, got)
		}
		delete(got, name)
	}
	if len(got) != 0 {
		t.Fatalf("the failed save left unexpected file(s) behind in %s: %v", dir, got)
	}
}

// assertRegisterResultsEqual compares full records — batch, product,
// quantity, unit and status — element by element and in order.
func assertRegisterResultsEqual(t *testing.T, what string, got, want []registerResult) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d records, want %d: %v", what, len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s: record %d = %+v, want %+v", what, i+1, got[i], want[i])
		}
	}
}

// decodeImportResults parses stdout as exactly one JSON object carrying
// exactly the "results" array — nothing before, after or beyond it.
func decodeImportResults(t *testing.T, stdout string) []registerResult {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(stdout))
	var root map[string]json.RawMessage
	if err := dec.Decode(&root); err != nil {
		t.Fatalf("stdout is not a JSON object: %v (%q)", err, stdout)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		t.Fatalf("stdout must hold exactly one JSON object, got %q", stdout)
	}
	raw, ok := root["results"]
	if !ok || len(root) != 1 {
		t.Fatalf("result object must carry exactly the \"results\" array: %q", stdout)
	}
	var results []registerResult
	if err := json.Unmarshal(raw, &results); err != nil {
		t.Fatalf("results do not match the record shape: %v (%q)", err, stdout)
	}
	return results
}

// The save fails against an existing registry: whichever step breaks, the
// command reports the failure naming the registry path and the save reason,
// stdout stays empty, and the registry keeps its exact bytes, its pinned
// modification time and its original records in order — no new batch from
// the manifest may remain, and no temporary file may survive.
func TestBatchImportCLISaveFailureKeepsExistingRegistry(t *testing.T) {
	markers := []string{"OLD-1", "OLD-3", "NEW-1", "NEW-2", "NEW-3", "新产品"}
	for name, inject := range saveFailureInjections() {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			registry := filepath.Join(dir, "reg.json")
			manifest := filepath.Join(dir, "in.json")
			writeFile(t, registry, importSaveFailureRegistryJSON)
			writeFile(t, manifest, importSaveFailureManifest)
			content, pinned := pinRegistry(t, registry)
			manifestBytes := readFileSnapshot(t, manifest)

			cause := errors.New("injected save failure: " + name)
			reached := inject(t, cause, registry, markers...)

			var stdout bytes.Buffer
			err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout)

			assertImportSaveFailed(t, err, cause, registry, &stdout)
			if !*reached {
				t.Fatal("the save never reached the failing step; the failure protection was not exercised")
			}
			assertRegistryUntouched(t, registry, content, pinned)
			after, rerr := os.ReadFile(registry)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if bytes.Contains(after, []byte("NEW-")) {
				t.Fatalf("part of the manifest's new batches landed in the registry: %s", after)
			}
			// Re-reading must still yield exactly the original batches, fields
			// and order.
			assertRegisterResultsEqual(t, "registry after failed save", readStoredBatches(t, registry), importSaveFailureOldBatches)
			assertFileUnchanged(t, manifest, manifestBytes)
			assertOnlyFiles(t, dir, "reg.json", "in.json")
		})
	}
}

// The same two mid-save failures against a registry that does not exist yet:
// the command reports the failure, stdout stays empty, the target file still
// does not exist — no empty registry and no file holding only the first few
// batches — and no temporary file is left behind.
func TestBatchImportCLISaveFailureWithMissingRegistryLeavesNoFile(t *testing.T) {
	markers := []string{"NEW-1", "NEW-2", "NEW-3", "新产品"}
	for name, inject := range saveFailureInjections() {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			registry := filepath.Join(dir, "reg.json")
			manifest := filepath.Join(dir, "in.json")
			writeFile(t, manifest, importSaveFailureManifest)
			manifestBytes := readFileSnapshot(t, manifest)

			cause := errors.New("injected save failure: " + name)
			reached := inject(t, cause, registry, markers...)

			var stdout bytes.Buffer
			err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout)

			assertImportSaveFailed(t, err, cause, registry, &stdout)
			if !*reached {
				t.Fatal("the save never reached the failing step; the failure protection was not exercised")
			}
			if _, statErr := os.Stat(registry); !os.IsNotExist(statErr) {
				t.Fatalf("failed save must not create the registry file, stat err=%v", statErr)
			}
			assertFileUnchanged(t, manifest, manifestBytes)
			assertOnlyFiles(t, dir, "in.json")
		})
	}
}

// The success counterpart for the very same manifest: with no failure
// injected, the import reports one JSON object whose results keep manifest
// order — the stored batch confirmed as a duplicate, the new batches created
// — and the registry keeps its old batches in order with every new batch
// appended in first-occurrence order. Every result entry is checked field by
// field against the record that actually landed in the file, so a matching
// result count alone can never masquerade as a completed import.
func TestBatchImportCLISaveSuccessMatchesReportedResults(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "reg.json")
	manifest := filepath.Join(dir, "in.json")
	writeFile(t, registry, importSaveFailureRegistryJSON)
	writeFile(t, manifest, importSaveFailureManifest)
	manifestBytes := readFileSnapshot(t, manifest)

	var stdout bytes.Buffer
	if err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout); err != nil {
		t.Fatalf("the legal manifest must import when the save does not fail: %v", err)
	}

	results := decodeImportResults(t, stdout.String())
	assertRegisterResultsEqual(t, "reported results", results, importSaveFailureWantResults)

	stored := readStoredBatches(t, registry)
	assertRegisterResultsEqual(t, "stored registry", stored, importSaveFailureWantStored)

	// Every reported batch, product, quantity and unit must match the record
	// finally stored under that batch id — the output is checked against the
	// file, not merely against its own length.
	storedByID := make(map[string]registerResult, len(stored))
	for _, b := range stored {
		storedByID[b.Batch] = b
	}
	for i, res := range results {
		storedRec, ok := storedByID[res.Batch]
		if !ok {
			t.Errorf("result %d (%q) has no record in the final registry", i+1, res.Batch)
			continue
		}
		got := res
		got.Status = ""
		if got != storedRec {
			t.Errorf("result %d = %+v, but the registry holds %+v for batch %q", i+1, res, storedRec, res.Batch)
		}
	}

	assertFileUnchanged(t, manifest, manifestBytes)
	assertOnlyFiles(t, dir, "reg.json", "in.json")
}
