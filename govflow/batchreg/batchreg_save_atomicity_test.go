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

// This file guards the atomicity contract of Save on the very file being
// replaced — unlike a failure raised on some other, unrelated path, the
// faults here strike the save of THIS registry file: either while the new
// content is still being written (see the unix-only file for the real
// write failure) or when the fully prepared content cannot be swapped into
// place. In both cases the caller must get an error naming the target and
// the cause; the original file must keep its bytes and modification time
// and still load with its original records in order; no temporary file may
// survive as a would-be successful registry; and the caller's table must be
// left exactly as submitted. A fault-free control save of the same table
// proves it is the injected fault — not the table — that blocks the save.
//
// Every existing-file scenario uses the same shape: a registry holding
// three old batches, and a legal submitted table that keeps those three and
// adds two clearly distinguishable new ones.

// atomicityOriginal is the registry content already on disk before the save.
const atomicityOriginal = `{"version":1,"batches":[` +
	`{"batch":"OLD-1","product":"P-1","quantity":11,"unit":"kg"},` +
	`{"batch":"OLD-2","product":"P-2","quantity":22,"unit":"box"},` +
	`{"batch":"OLD-3","product":"P-3","quantity":33,"unit":"m"}]}`

// atomicityOriginalBatches is what Load must keep returning for
// atomicityOriginal after any failed save.
var atomicityOriginalBatches = []Batch{
	saveBatch("OLD-1", "P-1", 11, "kg"),
	saveBatch("OLD-2", "P-2", 22, "box"),
	saveBatch("OLD-3", "P-3", 33, "m"),
}

// atomicitySubmission is the legal table handed to Save: the original
// records in their original order plus two new ones whose text cannot be
// mistaken for any of the old content. Every record satisfies the field,
// quantity and id rules, so no save may reject it before writing begins.
func atomicitySubmission() []Batch {
	return append(append([]Batch(nil), atomicityOriginalBatches...),
		saveBatch("NEW-α-1", "新品-甲", 101, "箱"),
		saveBatch("NEW-β-2", "新品-乙", 202, "托"),
	)
}

// pinAtomicityRegistry writes the original registry to path and pins its
// modification time to a fixed past moment, so a failed save can be checked
// for leaving both the bytes and the mtime exactly alone.
func pinAtomicityRegistry(t *testing.T, path string) (pinned time.Time) {
	t.Helper()
	writeRegistry(t, path, atomicityOriginal)
	pinned = time.Unix(1234567890, 0)
	if err := os.Chtimes(path, pinned, pinned); err != nil {
		t.Fatal(err)
	}
	return pinned
}

// assertRegistryUntouched proves the failed save behaved as if it never ran:
// the target file still holds exactly the original bytes under the pinned
// modification time, none of the submitted new content shows up in it, and
// it loads back to the original records in their original order.
func assertRegistryUntouched(t *testing.T, path string, pinned time.Time) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != atomicityOriginal {
		t.Fatalf("failed save changed the target bytes:\nwant %s\ngot  %s", atomicityOriginal, data)
	}
	if bytes.Contains(data, []byte("NEW-")) {
		t.Fatalf("target holds part of the submitted table: %s", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(pinned) {
		t.Fatalf("failed save changed the modification time: %v -> %v", pinned, info.ModTime())
	}
	reg, existed, err := Load(path)
	if err != nil || !existed {
		t.Fatalf("original registry must still load: existed=%v err=%v", existed, err)
	}
	assertBatchesEqual(t, reg.Batches, atomicityOriginalBatches)
}

// assertSaveFailureError checks the error of a save that failed partway: it
// must name the target file, carry the injected failure cause, and must not
// be a validation rejection — the submitted table is legal, so the failure
// has to come from the save process itself.
func assertSaveFailureError(t *testing.T, err error, path string, cause error) {
	t.Helper()
	if err == nil {
		t.Fatal("Save must report the failure to the caller")
	}
	if !errors.Is(err, cause) {
		t.Fatalf("error must carry the failure cause %v, got %v", cause, err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error must name the target file %q: %v", path, err)
	}
	var fe *FormatError
	if errors.As(err, &fe) {
		t.Fatalf("a legal table must not be rejected before saving: %v", err)
	}
	var dup *DuplicateIDError
	if errors.As(err, &dup) {
		t.Fatalf("a legal table must not be rejected before saving: %v", err)
	}
}

// renameAttempt records one call to Save's final replacement step.
type renameAttempt struct{ temp, target string }

// interceptRename replaces Save's final replacement step with stub for the
// rest of the test and records every attempted replacement.
func interceptRename(t *testing.T, stub func(temp, target string) error) (attempts *[]renameAttempt) {
	t.Helper()
	attempts = new([]renameAttempt)
	original := osRename
	osRename = func(oldpath, newpath string) error {
		*attempts = append(*attempts, renameAttempt{temp: oldpath, target: newpath})
		return stub(oldpath, newpath)
	}
	t.Cleanup(func() { osRename = original })
	return attempts
}

// The new content is fully prepared but the final replacement of the target
// file fails: the save must end with an error, the temporary file must be
// cleaned up rather than left behind as a would-be registry, and the
// existing target must remain byte-for-byte the original registry.
func TestSaveReplaceFailureKeepsExistingRegistryUsable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")
	pinned := pinAtomicityRegistry(t, path)

	submitted := atomicitySubmission()
	reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), submitted...)}

	injected := errors.New("injected replace failure")
	var prepared []Batch
	attempts := interceptRename(t, func(temp, target string) error {
		// The replacement must aim at this save's own target file — not at
		// some other file next to it — and by the time it is attempted the
		// new content must be fully prepared in the temporary file.
		if target != path {
			t.Errorf("replacement must target the save's own file %q, got %q", path, target)
		}
		data, err := os.ReadFile(temp)
		if err != nil {
			t.Errorf("new content must be fully written before the replace: %v", err)
			return injected
		}
		ready, err := decode(data)
		if err != nil {
			t.Errorf("prepared content must decode as a legal registry: %v", err)
			return injected
		}
		prepared = ready.Batches
		return injected
	})

	err := Save(path, reg)
	assertSaveFailureError(t, err, path, injected)

	if len(*attempts) != 1 {
		t.Fatalf("expected exactly one replacement attempt, got %d", len(*attempts))
	}
	temp := (*attempts)[0].temp
	if filepath.Dir(temp) != dir || !strings.HasPrefix(filepath.Base(temp), ".govflow-registry-") {
		t.Errorf("replacement source %q is not the save's temporary file in %q", temp, dir)
	}
	// What was about to land is exactly the submitted table.
	assertBatchesEqual(t, prepared, submitted)

	assertRegistryUntouched(t, path, pinned)
	assertNoLeftoverTempFiles(t, dir)
	assertBatchesEqual(t, reg.Batches, submitted)
}

// The same replace failure against a target that does not exist yet must
// leave it non-existent, with no temporary or partial file anywhere.
func TestSaveReplaceFailureKeepsMissingTargetMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")

	submitted := atomicitySubmission()
	reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), submitted...)}

	injected := errors.New("injected replace failure")
	interceptRename(t, func(temp, target string) error { return injected })

	err := Save(path, reg)
	assertSaveFailureError(t, err, path, injected)

	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("failed save must not create the target, stat err=%v", statErr)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("failed save left files behind: %v", names)
	}
	assertBatchesEqual(t, reg.Batches, submitted)
}

// Control: with no fault injected, the very same legal table saves
// successfully — the read-back records match the submission field by field
// and in order, and the original file is genuinely replaced (new bytes, new
// modification time). This is what distinguishes "the failure protection
// works" from "the save never ran".
func TestSaveWithoutFaultsReplacesExistingRegistry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")
	pinned := pinAtomicityRegistry(t, path)

	submitted := atomicitySubmission()
	reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), submitted...)}

	if err := Save(path, reg); err != nil {
		t.Fatalf("the same legal table must save when no fault strikes: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == atomicityOriginal {
		t.Fatal("successful save must replace the original bytes")
	}
	if !bytes.Contains(data, []byte("NEW-")) {
		t.Fatalf("saved file must hold the submitted new batches: %s", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.ModTime().Equal(pinned) {
		t.Fatal("successful save must replace the file, but the pinned modification time survived")
	}

	got, existed, err := Load(path)
	if err != nil || !existed {
		t.Fatalf("saved registry must read back: existed=%v err=%v", existed, err)
	}
	assertBatchesEqual(t, got.Batches, submitted)
	assertNoLeftoverTempFiles(t, dir)
	assertBatchesEqual(t, reg.Batches, submitted)
}
