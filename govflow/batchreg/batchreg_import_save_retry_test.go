package batchreg

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This file guards the split-contract of the Go-library usage
// ParseManifest -> Load -> Import -> Save: a successful Import only means the
// manifest's batches have entered the *Registry the caller already holds; a
// successful Save is what makes the on-disk registry accept them. The
// regression target is what happens when a save fault lands between those two
// steps and the caller later retries with the very same *Registry.
//
// The manifest mixes every admit shape: a duplicate of a stored batch (padded
// with whitespace, as it arrives through ParseManifest), several new batches,
// and one of the new ids appearing AGAIN later with identical content. So one
// manifest exercises: first-occurrence creation, stored duplicate
// confirmation, within-manifest duplicate confirmation, dedup (one new id
// appended exactly once), field normalization and append order.
//
// Two real save faults are injected deterministically through the package
// step hooks — the new-content write stopping before the table is complete,
// and the fully prepared table being refused at the final replacement — each
// against an existing registry and against a path where no registry exists
// yet. Every scenario asserts:
//
//   - Save's error names the registry path and carries the concrete cause,
//     and is never worded as invalid input or a batch-id conflict (the
//     manifest is legal and conflict-free, and that is proven independently by
//     ParseManifest + Import succeeding before the fault is armed);
//   - after the error the merged table inside reg is still complete: the
//     stored batches keep all four fields and their positions, the new
//     batches append in first-occurrence order and the repeated new id exists
//     exactly once;
//   - the per-record results returned by Import are still intact: manifest
//     order, exact normalized fields, created/duplicate flags;
//   - the disk accepted nothing: an existing target keeps its exact bytes
//     and modification time and still loads with only its original batches,
//     a missing target stays missing, and no partial table or temporary file
//     is left;
//   - Import's "created" results are therefore explicitly NOT proof of disk
//     registration (the new ids exist in reg but are absent from disk).
//
// The fault is then lifted and the caller retries Save WITH THE SAME reg — no
// re-read of the manifest, no second ParseManifest/Import, no rebuilding the
// table. The retry must succeed, the file must then read back exactly equal
// to reg (counts, four field values and order), neither save pass may mutate
// the caller's batches, and neither the failed pass nor the successful one
// may leave a temporary file behind. Running the retry after the failure
// (rather than a healthy save in a fresh setup) is what proves the prepared
// content can still be saved once the fault clears.

// importRetryPinnedModTime is pinned onto an existing registry so any rewrite
// — even one writing identical bytes — shows up as a changed modification
// time.
var importRetryPinnedModTime = time.Date(2006, time.January, 2, 15, 4, 5, 0, time.UTC)

// importRetryRegistryJSON is the pre-existing registry for existing-target
// scenarios: multiple batches in a fixed order.
const importRetryRegistryJSON = `{"version":1,"batches":[` +
	`{"batch":"OLD-1","product":"P-1","quantity":11,"unit":"kg"},` +
	`{"batch":"OLD-2","product":"苹果","quantity":22,"unit":"箱"}]}`

// importRetryManifestJSON is the one legal manifest used by every scenario:
//   - record 1: stored OLD-1, padded with whitespace -> duplicate after trim;
//   - record 2: new NEW-1 -> created;
//   - record 3: stored OLD-2, Chinese text verbatim -> duplicate;
//   - record 4: new NEW-2 -> created;
//   - record 5: NEW-1 appearing AGAIN, identical content -> duplicate (the
//     same new id must land exactly once);
//   - record 6: new NEW-3 with a padded unit and emoji text -> created.
const importRetryManifestJSON = `[
  {"batch": " OLD-1 ", "product": "P-1", "quantity": 11, "unit": "kg"},
  {"batch": "NEW-1", "product": "P-3", "quantity": 33, "unit": "box"},
  {"batch": "OLD-2", "product": "苹果", "quantity": 22, "unit": "箱"},
  {"batch": "NEW-2", "product": "新产品😀", "quantity": 44, "unit": "千克"},
  {"batch": "NEW-1", "product": "P-3", "quantity": 33, "unit": "box"},
  {"batch": "NEW-3", "product": "P-5", "quantity": 55, "unit": " pallet "}
]`

// importRetryExpectedResults is what Import must return, in manifest order,
// when the registry pre-exists.
var importRetryExpectedResults = []ImportResult{
	{Batch: saveBatch("OLD-1", "P-1", 11, "kg"), Created: false},
	{Batch: saveBatch("NEW-1", "P-3", 33, "box"), Created: true},
	{Batch: saveBatch("OLD-2", "苹果", 22, "箱"), Created: false},
	{Batch: saveBatch("NEW-2", "新产品😀", 44, "千克"), Created: true},
	{Batch: saveBatch("NEW-1", "P-3", 33, "box"), Created: false},
	{Batch: saveBatch("NEW-3", "P-5", 55, "pallet"), Created: true},
}

// importRetryExpectedResultsWhenMissing is the same manifest's outcome with
// no prior registry: OLD-1 and OLD-2 are first-created there, and only the
// repeated NEW-1 (record 5) is a duplicate.
var importRetryExpectedResultsWhenMissing = []ImportResult{
	{Batch: saveBatch("OLD-1", "P-1", 11, "kg"), Created: true},
	{Batch: saveBatch("NEW-1", "P-3", 33, "box"), Created: true},
	{Batch: saveBatch("OLD-2", "苹果", 22, "箱"), Created: true},
	{Batch: saveBatch("NEW-2", "新产品😀", 44, "千克"), Created: true},
	{Batch: saveBatch("NEW-1", "P-3", 33, "box"), Created: false},
	{Batch: saveBatch("NEW-3", "P-5", 55, "pallet"), Created: true},
}

// importRetryOriginalBatches is what the existing registry holds.
var importRetryOriginalBatches = []Batch{
	saveBatch("OLD-1", "P-1", 11, "kg"),
	saveBatch("OLD-2", "苹果", 22, "箱"),
}

// importRetryMergedAgainstExisting is the complete table Import leaves in reg
// when the registry pre-exists: stored batches keep position and fields, new
// batches append in first-occurrence order, NEW-1 exactly once.
var importRetryMergedAgainstExisting = []Batch{
	saveBatch("OLD-1", "P-1", 11, "kg"),
	saveBatch("OLD-2", "苹果", 22, "箱"),
	saveBatch("NEW-1", "P-3", 33, "box"),
	saveBatch("NEW-2", "新产品😀", 44, "千克"),
	saveBatch("NEW-3", "P-5", 55, "pallet"),
}

// importRetryMergedWhenMissing is the merged table with no prior file: the
// formerly stored ids are created too, still in manifest first-occurrence
// order.
var importRetryMergedWhenMissing = []Batch{
	saveBatch("OLD-1", "P-1", 11, "kg"),
	saveBatch("NEW-1", "P-3", 33, "box"),
	saveBatch("OLD-2", "苹果", 22, "箱"),
	saveBatch("NEW-2", "新产品😀", 44, "千克"),
	saveBatch("NEW-3", "P-5", 55, "pallet"),
}

// importRetryFault selects which step of the atomic save fails.
type importRetryFault string

const (
	importRetryFaultWrite  importRetryFault = "write"
	importRetryFaultRename importRetryFault = "rename"
)

// armImportRetryFault makes the chosen save step fail once the manifest's
// complete merged table has been prepared, so the fault really lands between
// Import and disk acceptance. The returned restore function must run in
// cleanup; lifting it is exactly what "the fault is cleared" means for the
// retry.
func armImportRetryFault(t *testing.T, fault importRetryFault, cause error, path string, completeMarkers []string) (restore func()) {
	t.Helper()
	switch fault {
	case importRetryFaultWrite:
		old := WriteTempContent
		WriteTempContent = func(f *os.File, data []byte) (int, error) {
			// Part of the new content genuinely reaches the temporary file,
			// then the write stops with the table incomplete.
			half := len(data) / 2
			if _, err := f.Write(data[:half]); err != nil {
				t.Errorf("fault setup write failed: %v", err)
				return 0, err
			}
			return half, cause
		}
		return func() { WriteTempContent = old }
	case importRetryFaultRename:
		old := RenameTempFile
		RenameTempFile = func(oldpath, newpath string) error {
			if newpath != path {
				t.Errorf("rename must target the registry path %q, got %q", path, newpath)
				return cause
			}
			// Prove the whole merged table — stored and new batches alike,
			// including non-ASCII text — is prepared before the replacement is
			// refused, so the failure is a replace failure rather than a
			// partially prepared table.
			prepared, err := os.ReadFile(oldpath)
			if err != nil {
				t.Errorf("prepared temporary file is unreadable: %v", err)
				return cause
			}
			for _, marker := range completeMarkers {
				if !bytes.Contains(prepared, []byte(marker)) {
					t.Errorf("prepared content is missing %q; the full new table was not ready:\n%s", marker, prepared)
					return cause
				}
			}
			return cause
		}
		return func() { RenameTempFile = old }
	default:
		t.Fatalf("unknown fault %q", fault)
		return nil
	}
}

// importRetrySetup performs the successful ParseManifest -> Load -> Import
// sequence and hands back everything the caller needs for the failed save and
// the retry. existing determines whether the registry file pre-exists. The
// fault is deliberately NOT armed here: Import must first have succeeded
// cleanly, exactly as the documented usage order requires.
func importRetrySetup(t *testing.T, existing bool) (dir, registryPath string, reg *Registry, results, expectedResults []ImportResult, merged []Batch, originalBytes []byte) {
	t.Helper()
	dir = t.TempDir()
	registryPath = filepath.Join(dir, "registry.json")

	if existing {
		writeRegistry(t, registryPath, importRetryRegistryJSON)
		if err := os.Chtimes(registryPath, importRetryPinnedModTime, importRetryPinnedModTime); err != nil {
			t.Fatal(err)
		}
		var err error
		originalBytes, err = os.ReadFile(registryPath)
		if err != nil {
			t.Fatal(err)
		}
	}

	inputs, err := ParseManifest([]byte(importRetryManifestJSON))
	if err != nil {
		t.Fatalf("the manifest must be legal before the save fault is armed: %v", err)
	}
	var existed bool
	reg, existed, err = Load(registryPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if existed != existing {
		t.Fatalf("Load existed=%v, want %v", existed, existing)
	}
	results, err = Import(reg, inputs)
	if err != nil {
		t.Fatalf("Import must succeed before the save fault: %v", err)
	}
	if existing {
		expectedResults = importRetryExpectedResults
		merged = importRetryMergedAgainstExisting
	} else {
		expectedResults = importRetryExpectedResultsWhenMissing
		merged = importRetryMergedWhenMissing
	}

	// Baseline assertions establish the in-memory half of the split contract
	// before the fault: results and the merged table are exactly what the
	// later failure and retry assertions compare against, and the created
	// flags identify what the disk does not yet hold.
	assertImportRetryResults(t, results, expectedResults)
	assertBatchesEqual(t, reg.Batches, merged)
	return dir, registryPath, reg, results, expectedResults, merged, originalBytes
}

// assertImportRetryResults compares per-record results in manifest order:
// exact four-field values and created/duplicate flags.
func assertImportRetryResults(t *testing.T, got, want []ImportResult) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d results, want %d (manifest order): %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("result %d = %+v, want %+v", i+1, got[i], w)
		}
	}
}

// assertSaveFailureIsSaveFault checks that the error reports a SAVE failure
// against the registry path with the concrete cause — never invalid-input or
// conflict wording, which would mislead the caller about what remains
// unregistered.
func assertSaveFailureIsSaveFault(t *testing.T, err error, cause error, path string) {
	t.Helper()
	if err == nil {
		t.Fatal("Save must return an error when the save step fails")
	}
	if !errors.Is(err, cause) {
		t.Errorf("error %v must carry the injected cause %v", err, cause)
	}
	msg := err.Error()
	for _, want := range []string{
		"cannot save registry",
		path,
		cause.Error(),
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q must contain %q", msg, want)
		}
	}
	// The manifest is legal and conflict-free (proven by Import succeeding);
	// the failure must never masquerade as bad input or an id conflict.
	for _, mustNot := range []string{"manifest record", "conflicts with", "must be", "unknown field"} {
		if strings.Contains(msg, mustNot) {
			t.Errorf("a save fault must not be reported as %q: %q", mustNot, msg)
		}
	}
}

// assertDiskStillHoldsOnly verifies the disk accepted none of the new batches
// after the failed save: an existing target keeps exact bytes and pinned
// modification time and loads with only its original batches; a missing
// target stays missing.
func assertDiskStillHoldsOnly(t *testing.T, path string, existing bool, original []Batch, originalBytes []byte) {
	t.Helper()
	if existing {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("the original registry must still exist: %v", err)
		}
		if !info.ModTime().Equal(importRetryPinnedModTime) {
			t.Errorf("failed save changed the registry mtime: got %v, want %v", info.ModTime(), importRetryPinnedModTime)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(after, originalBytes) {
			t.Fatalf("failed save changed the registry bytes:\nbefore: %s\nafter:  %s", originalBytes, after)
		}
		if bytes.Contains(after, []byte("NEW-")) {
			t.Fatalf("a new batch reached the registry despite the failed save: %s", after)
		}
		loaded, existed, err := Load(path)
		if err != nil || !existed {
			t.Fatalf("original registry must still load: existed=%v err=%v", existed, err)
		}
		assertBatchesEqual(t, loaded.Batches, original)
		return
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("failed save must not create a missing registry, stat err=%v", err)
	}
}

// assertCreatedNotOnDisk expresses the core split-contract fact: every result
// Import marked created now lives in reg, but after the failed save none of
// those ids may be readable from disk.
func assertCreatedNotOnDisk(t *testing.T, path string, existing bool, results []ImportResult) {
	t.Helper()
	loaded, existed, err := Load(path)
	if err != nil {
		t.Fatalf("disk must still be loadable (or cleanly absent) after the failure: %v", err)
	}
	if existing != existed {
		t.Fatalf("disk existence changed: got existed=%v want %v", existed, existing)
	}
	onDisk := map[string]bool{}
	for _, b := range loaded.Batches {
		onDisk[b.Batch] = true
	}
	for i, r := range results {
		if !r.Created {
			continue
		}
		if onDisk[r.Batch.Batch] {
			t.Fatalf("result %d marked created for %q is already readable on disk after a failed save; "+
				"a created ImportResult must not be proof of disk registration", i+1, r.Batch.Batch)
		}
	}
}

// TestImportThenSaveFailureThenRetryWithSameRegistry runs the full lifecycle
// for both save faults and both target states: Import succeeds, the first Save
// fails at the chosen step, then a retry with the SAME *Registry succeeds
// once the fault is lifted — with no re-read of the manifest and no second
// Import.
func TestImportThenSaveFailureThenRetryWithSameRegistry(t *testing.T) {
	causes := map[importRetryFault]error{
		importRetryFaultWrite:  errors.New("injected write fault: new content not fully written"),
		importRetryFaultRename: errors.New("injected replace fault: complete new content cannot replace the registry"),
	}
	for _, fault := range []importRetryFault{importRetryFaultWrite, importRetryFaultRename} {
		for _, existing := range []bool{true, false} {
			name := string(fault) + "/" + map[bool]string{true: "existing_registry", false: "missing_registry"}[existing]
			t.Run(name, func(t *testing.T) {
				cause := causes[fault]
				dir, path, reg, results, expectedResults, merged, originalBytes := importRetrySetup(t, existing)

				// Markers prove the prepared content holds every merged batch
				// (checked inside the rename hook; harmless for the write hook
				// which does not use them).
				markers := []string{"OLD-1", "OLD-2", "NEW-1", "NEW-2", "NEW-3", "苹果", "新产品😀"}
				restore := armImportRetryFault(t, fault, cause, path, markers)

				// First save: fault armed, content prepared fully (rename) or
				// partially flushed (write), then failure.
				err := Save(path, reg)
				restore()

				assertSaveFailureIsSaveFault(t, err, cause, path)

				// In-memory state survives the failed save completely: the
				// merged table and the already-returned per-record results are
				// both still exactly what Import produced.
				assertBatchesEqual(t, reg.Batches, merged)
				assertImportRetryResults(t, results, expectedResults)
				assertCreatedNotOnDisk(t, path, existing, results)
				assertDiskStillHoldsOnly(t, path, existing, importRetryOriginalBatches, originalBytes)
				assertNoLeftoverTempFiles(t, dir)

				// The fault is cleared. Retry with the SAME registry instance:
				// no ParseManifest, no Load, no Import, no rebuilding.
				if err := Save(path, reg); err != nil {
					t.Fatalf("retrying the prepared registry after the fault cleared must succeed: %v", err)
				}

				// The disk now accepts exactly the table held in reg: counts,
				// four field values and order all agree.
				saved, existed, err := Load(path)
				if err != nil || !existed {
					t.Fatalf("retried registry must exist and load: existed=%v err=%v", existed, err)
				}
				if saved.Version != FormatVersion {
					t.Fatalf("read-back version = %d, want %d", saved.Version, FormatVersion)
				}
				assertBatchesEqual(t, saved.Batches, merged)
				assertBatchesEqual(t, saved.Batches, reg.Batches)

				// The retried save must not have mutated the caller's batches.
				assertBatchesEqual(t, reg.Batches, merged)
				assertImportRetryResults(t, results, expectedResults)

				// Every created result is now genuinely registered, and the
				// repeated new id still occupies exactly one record.
				onDisk := map[string]Batch{}
				countByID := map[string]int{}
				for _, b := range saved.Batches {
					onDisk[b.Batch] = b
					countByID[b.Batch]++
				}
				for i, r := range results {
					got, ok := onDisk[r.Batch.Batch]
					if !ok {
						t.Fatalf("result %d (%q) is absent from the registry after the successful retry", i+1, r.Batch.Batch)
					}
					if got != r.Batch {
						t.Errorf("result %d = %+v disagrees with stored %+v after the retry", i+1, r.Batch, got)
					}
					if countByID[r.Batch.Batch] != 1 {
						t.Fatalf("batch %q stored %d times; the retry must not turn a duplicate into an extra record",
							r.Batch.Batch, countByID[r.Batch.Batch])
					}
				}
				assertNoLeftoverTempFiles(t, dir)
			})
		}
	}
}

// TestImportSaveFailureRetryIsIdempotentAcrossRepeatedFailures pushes the
// retry contract further: the first save fails, a second attempt fails again
// (a different fault), and only the third attempt — same reg throughout —
// succeeds. The prepared content must remain saveable after repeated
// failures, and the disk must change exactly once: at the final success.
func TestImportSaveFailureRetryIsIdempotentAcrossRepeatedFailures(t *testing.T) {
	dir, path, reg, results, expectedResults, merged, originalBytes := importRetrySetup(t, true)

	writeCause := errors.New("injected write fault on attempt 1")
	renameCause := errors.New("injected replace fault on attempt 2")

	restore1 := armImportRetryFault(t, importRetryFaultWrite, writeCause, path, nil)
	if err := Save(path, reg); !errors.Is(err, writeCause) {
		t.Fatalf("attempt 1 error = %v, want %v", err, writeCause)
	}
	restore1()
	assertBatchesEqual(t, reg.Batches, merged)
	assertDiskStillHoldsOnly(t, path, true, importRetryOriginalBatches, originalBytes)
	assertNoLeftoverTempFiles(t, dir)

	markers := []string{"OLD-1", "OLD-2", "NEW-1", "NEW-2", "NEW-3", "苹果", "新产品😀"}
	restore2 := armImportRetryFault(t, importRetryFaultRename, renameCause, path, markers)
	if err := Save(path, reg); !errors.Is(err, renameCause) {
		t.Fatalf("attempt 2 error = %v, want %v", err, renameCause)
	}
	restore2()
	assertBatchesEqual(t, reg.Batches, merged)
	assertImportRetryResults(t, results, expectedResults)
	assertDiskStillHoldsOnly(t, path, true, importRetryOriginalBatches, originalBytes)
	assertNoLeftoverTempFiles(t, dir)

	if err := Save(path, reg); err != nil {
		t.Fatalf("attempt 3 with the same reg must succeed: %v", err)
	}
	saved, existed, err := Load(path)
	if err != nil || !existed {
		t.Fatalf("final registry must load: existed=%v err=%v", existed, err)
	}
	assertBatchesEqual(t, saved.Batches, merged)
	assertBatchesEqual(t, reg.Batches, merged)
	assertNoLeftoverTempFiles(t, dir)
}

// TestImportResultsAreNotDiskProofButBecomeItAfterSuccessfulRetry states the
// caller-facing rule directly rather than through the lifecycle above:
// immediately after Import (and again after the failed save) the new ids are
// in reg but not on disk; after the successful retry of the same reg they
// are.
func TestImportResultsAreNotDiskProofButBecomeItAfterSuccessfulRetry(t *testing.T) {
	_, path, reg, results, _, merged, _ := importRetrySetup(t, false)

	created := map[string]bool{}
	for _, r := range results {
		if r.Created {
			created[r.Batch.Batch] = true
		}
	}
	if len(created) != 5 {
		t.Fatalf("against a missing registry the manifest first-creates five ids (OLD-1, OLD-2, NEW-1, NEW-2, NEW-3), got %d: %v", len(created), created)
	}
	// Right after Import nothing has been saved at all.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("Import alone must not touch the registry, stat err=%v", err)
	}

	cause := errors.New("injected write fault for the disk-proof check")
	restore := armImportRetryFault(t, importRetryFaultWrite, cause, path, nil)
	if err := Save(path, reg); !errors.Is(err, cause) {
		t.Fatalf("save error = %v, want %v", err, cause)
	}
	restore()
	// Still not on disk: created results are not proof.
	assertCreatedNotOnDisk(t, path, false, results)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("failed save must not create the registry, stat err=%v", err)
	}

	// Same reg, no re-import.
	if err := Save(path, reg); err != nil {
		t.Fatalf("retry must succeed: %v", err)
	}
	saved, existed, err := Load(path)
	if err != nil || !existed {
		t.Fatalf("registry must exist after the retry: existed=%v err=%v", existed, err)
	}
	assertBatchesEqual(t, saved.Batches, merged)
}
