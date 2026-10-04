//go:build unix

package batchreg

import (
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
)

// This file covers the other half of Save's atomicity contract: the content
// write itself fails partway. The failure is real, not simulated — the
// process file-size limit is dropped to zero so the next write to a regular
// file fails with EFBIG exactly the way a full disk would make it fail.
// That reproduces offline and deterministically on this machine, for any
// executing account (the limit binds root just the same), unlike a chmod
// that only works when the account happens to lack permissions.

// withoutFileSpace lowers the process file-size limit to zero and returns a
// restore function. The kernel raises SIGXFSZ on the offending write, so
// that signal is ignored for the duration to keep the process alive; both
// the limit and the signal disposition are restored by the returned
// function (also registered with t.Cleanup as a backstop).
func withoutFileSpace(t *testing.T) (restore func()) {
	t.Helper()
	signal.Ignore(syscall.SIGXFSZ)
	var previous syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &previous); err != nil {
		t.Fatal(err)
	}
	limited := previous
	limited.Cur = 0
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limited); err != nil {
		t.Fatal(err)
	}
	restored := false
	restore = func() {
		if restored {
			return
		}
		restored = true
		if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &previous); err != nil {
			t.Errorf("cannot restore the file-size limit: %v", err)
		}
		signal.Reset(syscall.SIGXFSZ)
	}
	t.Cleanup(restore)
	return restore
}

// The content write cannot even complete: the save must end with an error
// naming the target and the cause, the temporary file must be cleaned up,
// and the existing registry must remain byte-for-byte intact with its
// original records and order.
func TestSaveWriteFailureKeepsExistingRegistryUsable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")
	pinned := pinAtomicityRegistry(t, path)

	submitted := atomicitySubmission()
	reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), submitted...)}

	restore := withoutFileSpace(t)
	err := Save(path, reg)
	restore()

	assertSaveFailureError(t, err, path, syscall.EFBIG)
	assertRegistryUntouched(t, path, pinned)
	assertNoLeftoverTempFiles(t, dir)
	assertBatchesEqual(t, reg.Batches, submitted)
}

// The same write failure against a target that does not exist yet must
// leave it non-existent, with no temporary or partial file anywhere.
func TestSaveWriteFailureKeepsMissingTargetMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")

	submitted := atomicitySubmission()
	reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), submitted...)}

	restore := withoutFileSpace(t)
	err := Save(path, reg)
	restore()

	assertSaveFailureError(t, err, path, syscall.EFBIG)

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
