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

// This file guards the atomicity of Save when the save itself — not the
// validation phase and not a neighboring unusable path — fails on the file
// being replaced. Two mid-save failure points are covered, each injected
// deterministically through the package-level step variables:
//
//   - the content write into the temporary file stops before the new content
//     is complete;
//   - the new content is fully prepared on disk, but the final replacement of
//     the target file fails.
//
// In both cases the caller must get an error naming the target path and the
// cause, an existing target must keep its exact bytes and modification time
// and still load with its original records in order, a missing target must
// stay missing, no temporary file may survive, and the caller's submitted
// records must be left untouched. A success counterpart with the very same
// submission proves the protection is what keeps the old content in place,
// not a save that never ran. The injections need no disk fault, no special
// filesystem and no missing permissions, so they reproduce offline under any
// account, root included.

// savedFailureRegistryJSON is the existing registry every failure scenario
// starts from: multiple batches, loaded back for comparison in their stored
// order.
const savedFailureRegistryJSON = `{"version":1,"batches":[` +
	`{"batch":"OLD-1","product":"苹果","quantity":10,"unit":"kg"},` +
	`{"batch":"OLD-2","product":"P-2","quantity":20,"unit":"box"},` +
	`{"batch":"OLD-3","product":"P-3","quantity":30,"unit":"m"}]}`

// failedSaveModTime is pinned onto the existing registry file so that any
// rewrite — even one restoring identical bytes — is visible as a changed
// modification time.
var failedSaveModTime = time.Date(2009, time.February, 13, 23, 31, 30, 0, time.UTC)

// failureSubmission is a legal batch table under every existing field,
// quantity and id rule: the three stored records plus two clearly
// distinguishable new ones. It is what the caller hands to Save in every
// scenario of this file, failure and success alike.
func failureSubmission() []Batch {
	return []Batch{
		saveBatch("OLD-1", "苹果", 10, "kg"),
		saveBatch("OLD-2", "P-2", 20, "box"),
		saveBatch("OLD-3", "P-3", 30, "m"),
		saveBatch("NEW-1", "新产品", 99, "箱"),
		saveBatch("NEW-2", "P-new", MaxQuantity, " pallet "),
	}
}

// setupExistingRegistry writes the multi-batch registry, pins its modification
// time to failedSaveModTime and returns the target path, the original records
// in stored order and the original file bytes.
func setupExistingRegistry(t *testing.T) (dir, path string, original []Batch, originalBytes []byte) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, "registry.json")
	writeRegistry(t, path, savedFailureRegistryJSON)
	if err := os.Chtimes(path, failedSaveModTime, failedSaveModTime); err != nil {
		t.Fatal(err)
	}
	reg, existed, err := Load(path)
	if err != nil || !existed {
		t.Fatalf("fixture registry must load: existed=%v err=%v", existed, err)
	}
	original = reg.Batches
	if len(original) != 3 {
		t.Fatalf("fixture registry must hold multiple batches, got %+v", original)
	}
	originalBytes, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return dir, path, original, originalBytes
}

// assertSaveFailure checks the contract every rejected mid-save save must
// honor towards its caller: a non-nil error that names the actual target path
// and carries the injected cause.
func assertSaveFailure(t *testing.T, err, cause error, path string) {
	t.Helper()
	if err == nil {
		t.Fatal("Save must report the failed save to its caller")
	}
	if !errors.Is(err, cause) {
		t.Errorf("error %v must carry the failure cause %v", err, cause)
	}
	msg := err.Error()
	if !strings.Contains(msg, path) {
		t.Errorf("error %q must name the target path %q", msg, path)
	}
	if !strings.Contains(msg, cause.Error()) {
		t.Errorf("error %q must state the failure reason %q", msg, cause.Error())
	}
}

// assertExistingTargetUntouched verifies that the failed save behaved as if it
// never ran for the actual target: identical bytes, the pinned modification
// time, the original records in their original order when read back, no
// temporary file left anywhere under dir, and the caller's submitted records
// unchanged in content and order.
func assertExistingTargetUntouched(t *testing.T, dir, path string, original []Batch, originalBytes []byte, reg *Registry, submitted []Batch) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the original registry file must still exist: %v", err)
	}
	if !info.ModTime().Equal(failedSaveModTime) {
		t.Errorf("failed save touched the target's modification time: got %v, want %v", info.ModTime(), failedSaveModTime)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, originalBytes) {
		t.Fatalf("failed save changed the target bytes:\nbefore %s\nafter  %s", originalBytes, after)
	}
	if bytes.Contains(after, []byte("NEW-")) {
		t.Fatalf("part of the submitted content landed in the target: %s", after)
	}
	loaded, existed, err := Load(path)
	if err != nil || !existed {
		t.Fatalf("original registry must still be usable: existed=%v err=%v", existed, err)
	}
	assertBatchesEqual(t, loaded.Batches, original)
	assertNoLeftoverTempFiles(t, dir)
	assertBatchesEqual(t, reg.Batches, submitted)
}

// The content write stops halfway through the new content. The save must
// abort, clean up its temporary file and leave the existing registry — the
// actual replacement target — byte-for-byte and timestamp-for-timestamp
// untouched, loading with exactly the original records.
func TestSaveWriteFailureKeepsExistingRegistryIntact(t *testing.T) {
	dir, path, original, originalBytes := setupExistingRegistry(t)

	cause := errors.New("injected write failure: disk full")
	writeAttempted := false
	old := WriteTempContent
	WriteTempContent = func(f *os.File, data []byte) (int, error) {
		writeAttempted = true
		// Part of the new content genuinely reaches the temporary file before
		// the write fails, so the save aborts with the write incomplete.
		half := len(data) / 2
		if _, err := f.Write(data[:half]); err != nil {
			return 0, err
		}
		return half, cause
	}
	t.Cleanup(func() { WriteTempContent = old })

	submitted := failureSubmission()
	reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), submitted...)}
	err := Save(path, reg)

	assertSaveFailure(t, err, cause, path)
	if !writeAttempted {
		t.Fatal("the save never reached the content write; the failure protection was not exercised")
	}
	assertExistingTargetUntouched(t, dir, path, original, originalBytes, reg, submitted)
}

// The new content is fully prepared in the temporary file — the injected
// rename proves it by reading the complete new table, including the new
// batches, from the file it is asked to move — but the replacement of the
// target fails. The existing registry must survive exactly as it was and the
// prepared temporary file must be removed, not left behind as a would-be
// registry.
func TestSaveRenameFailureKeepsExistingRegistryIntact(t *testing.T) {
	dir, path, original, originalBytes := setupExistingRegistry(t)

	cause := errors.New("injected rename failure: cannot replace target")
	renameAttempted := false
	old := RenameTempFile
	RenameTempFile = func(oldpath, newpath string) error {
		renameAttempted = true
		if newpath != path {
			t.Errorf("rename must target the actual save path %q, got %q", path, newpath)
		}
		prepared, err := os.ReadFile(oldpath)
		if err != nil {
			t.Errorf("the prepared content must be readable before the replacement: %v", err)
			return cause
		}
		for _, marker := range []string{"OLD-1", "NEW-1", "NEW-2", "新产品"} {
			if !bytes.Contains(prepared, []byte(marker)) {
				t.Errorf("prepared content is missing %q; the save did not get the full new table ready: %s", marker, prepared)
			}
		}
		return cause
	}
	t.Cleanup(func() { RenameTempFile = old })

	submitted := failureSubmission()
	reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), submitted...)}
	err := Save(path, reg)

	assertSaveFailure(t, err, cause, path)
	if !renameAttempted {
		t.Fatal("the save never reached the replacement step; the failure protection was not exercised")
	}
	assertExistingTargetUntouched(t, dir, path, original, originalBytes, reg, submitted)
}

// The same two mid-save failures against a target that does not exist yet:
// the save must end with an error, the target must still not exist, no file
// holding part of the submitted batches may appear, and no temporary file may
// survive.
func TestSaveFailureOnMissingTargetLeavesNoFile(t *testing.T) {
	injectWriteFailure := func(t *testing.T, cause error) {
		old := WriteTempContent
		WriteTempContent = func(f *os.File, data []byte) (int, error) {
			half := len(data) / 2
			if _, err := f.Write(data[:half]); err != nil {
				return 0, err
			}
			return half, cause
		}
		t.Cleanup(func() { WriteTempContent = old })
	}
	injectRenameFailure := func(t *testing.T, cause error) {
		old := RenameTempFile
		RenameTempFile = func(oldpath, newpath string) error { return cause }
		t.Cleanup(func() { RenameTempFile = old })
	}
	cases := []struct {
		name   string
		inject func(t *testing.T, cause error)
	}{
		{"content write incomplete", injectWriteFailure},
		{"replacement refused", injectRenameFailure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "registry.json")
			cause := errors.New("injected save failure")
			tc.inject(t, cause)

			submitted := failureSubmission()
			reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), submitted...)}
			err := Save(path, reg)

			assertSaveFailure(t, err, cause, path)
			if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
				t.Fatalf("failed save must not create the target file, stat err=%v", statErr)
			}
			entries, readErr := os.ReadDir(dir)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 0 {
				t.Fatalf("failed save left files behind in the target directory: %v", entries)
			}
			assertNoLeftoverTempFiles(t, dir)
			assertBatchesEqual(t, reg.Batches, submitted)
		})
	}
}

// The success counterpart for the same legal submission: with no failure
// injected, Save replaces the existing registry — the bytes and the pinned
// modification time both change, the new content is really there — and the
// file reads back with all four public fields per record in submission order.
// This is what distinguishes a working failure protection from a save that
// never executed.
func TestSaveSuccessReplacesExistingRegistry(t *testing.T) {
	dir, path, _, originalBytes := setupExistingRegistry(t)

	submitted := failureSubmission()
	reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), submitted...)}
	if err := Save(path, reg); err != nil {
		t.Fatalf("the legal submission must save when nothing fails: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.ModTime().Equal(failedSaveModTime) {
		t.Fatal("successful save must replace the file; the pinned modification time survived")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(after, originalBytes) {
		t.Fatal("successful save must replace the old content; the original bytes survived")
	}
	for _, marker := range []string{"NEW-1", "NEW-2", "新产品"} {
		if !bytes.Contains(after, []byte(marker)) {
			t.Fatalf("saved file is missing the new content %q: %s", marker, after)
		}
	}

	loaded, existed, err := Load(path)
	if err != nil || !existed {
		t.Fatalf("saved registry must read back: existed=%v err=%v", existed, err)
	}
	if loaded.Version != FormatVersion {
		t.Fatalf("read-back version = %d, want %d", loaded.Version, FormatVersion)
	}
	assertBatchesEqual(t, loaded.Batches, submitted)
	assertNoLeftoverTempFiles(t, dir)
	assertBatchesEqual(t, reg.Batches, submitted)
}
