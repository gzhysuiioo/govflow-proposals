package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// End-to-end regression for a --registry that would have to enter a directory
// that does not exist and then step back out of it through a later "..":
// store/missing/../batches.json with store/missing absent. The kernel cannot
// follow the path as written, so the same-named file the collapsed path would
// name is never the file the user reached. Both commands must fail non-zero
// with stderr naming the user-supplied registry path and the missing-directory
// plus ".." reason, print no success or partial-success result on stdout,
// leave the existing registry byte-for-byte and timestamp-for-timestamp, and
// create nothing — no missing directory, no registry file, no temporary file.
// batch-import additionally keeps the manifest read-only.

// rawJoinPath concatenates path segments with the path separator WITHOUT any
// lexical cleaning: filepath.Join would collapse the "missing/.." pair and
// erase exactly the path shape under test.
func rawJoinPath(segments ...string) string {
	return strings.Join(segments, string(os.PathSeparator))
}

// setupMissingDirCLIFixture builds root/store/batches.json with the given
// content and returns the root, the store directory and the real registry
// path. The user path itself is assembled by each test (relative and
// absolute spellings of the same directory relationship).
func setupMissingDirCLIFixture(t *testing.T, content string) (root, storeDir, realPath string) {
	t.Helper()
	root = t.TempDir()
	storeDir = filepath.Join(root, "store")
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	realPath = filepath.Join(storeDir, "batches.json")
	writeFile(t, realPath, content)
	return root, storeDir, realPath
}

// assertMissingDirCLIRejection checks the full aftermath of a rejected
// command: the error names the user-supplied path, the missing directory and
// the ".." reason; stdout carries nothing; the real registry is untouched;
// the missing directory was never created; no temporary file survives.
func assertMissingDirCLIRejection(t *testing.T, err error, stdout *bytes.Buffer, userPath, missingDir, realPath string, realBytes []byte, pinned time.Time, root string) {
	t.Helper()
	if err == nil {
		t.Fatal("a registry path through a missing directory followed by \"..\" must fail")
	}
	msg := err.Error()
	for _, want := range []string{userPath, missingDir, `".."`} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q must mention %q", msg, want)
		}
	}
	if !strings.Contains(msg, "does not exist") {
		t.Errorf("error %q must state that the directory does not exist", msg)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout must carry no success or partial-success result, got %q", stdout.String())
	}
	assertRegistryUntouched(t, realPath, realBytes, pinned)
	if _, serr := os.Lstat(missingDir); !os.IsNotExist(serr) {
		t.Fatalf("the missing directory must not be created, stat err=%v", serr)
	}
	assertNoLeftoverTempFilesCLI(t, root)
}

// assertNoLeftoverTempFilesCLI walks root and fails if any save temp file
// (".govflow-registry-*.tmp") survived the rejected command.
func assertNoLeftoverTempFilesCLI(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".govflow-registry-") {
			t.Errorf("rejected command left a temporary file behind: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// batch-register with --registry store/missing/../batches.json must not
// report "created" and must not reduce the existing registry to only the new
// batch: the command fails, OLD survives byte-for-byte, and nothing is
// created. Checked for the relative spelling (from the parent of store) and
// the absolute spelling of the same directory relationship.
func TestBatchRegisterThroughMissingDirectoryDotDotRejected(t *testing.T) {
	t.Run("relative", func(t *testing.T) {
		root, _, realPath := setupMissingDirCLIFixture(t,
			`{"version":1,"batches":[{"batch":"OLD","product":"P-1","quantity":10,"unit":"kg"}]}`)
		realBytes, pinned := pinRegistry(t, realPath)
		t.Chdir(root)
		userPath := rawJoinPath("store", "missing", "..", "batches.json")
		missingDir := rawJoinPath("store", "missing")

		var stdout bytes.Buffer
		err := runBatchRegister([]string{
			"--registry", userPath,
			"--batch", "NEW", "--product", "P-9", "--quantity", "5", "--unit", "box",
		}, &stdout)
		assertMissingDirCLIRejection(t, err, &stdout, userPath, missingDir, realPath, realBytes, pinned, root)
		if got := batchIDs(t, realPath); strings.Join(got, ",") != "OLD" {
			t.Fatalf("the registry must still hold only OLD, got %v", got)
		}
	})
	t.Run("absolute", func(t *testing.T) {
		root, storeDir, realPath := setupMissingDirCLIFixture(t,
			`{"version":1,"batches":[{"batch":"OLD","product":"P-1","quantity":10,"unit":"kg"}]}`)
		realBytes, pinned := pinRegistry(t, realPath)
		userPath := rawJoinPath(storeDir, "missing", "..", "batches.json")
		missingDir := rawJoinPath(storeDir, "missing")

		var stdout bytes.Buffer
		err := runBatchRegister([]string{
			"--registry", userPath,
			"--batch", "NEW", "--product", "P-9", "--quantity", "5", "--unit", "box",
		}, &stdout)
		assertMissingDirCLIRejection(t, err, &stdout, userPath, missingDir, realPath, realBytes, pinned, root)
		if got := batchIDs(t, realPath); strings.Join(got, ",") != "OLD" {
			t.Fatalf("the registry must still hold only OLD, got %v", got)
		}
	})
}

// batch-import with the same registry path shape writes none of the
// manifest's new batches: the command fails, the registry keeps only OLD,
// and the manifest stays read-only (byte-for-byte as supplied).
func TestBatchImportThroughMissingDirectoryDotDotRejected(t *testing.T) {
	root, storeDir, realPath := setupMissingDirCLIFixture(t,
		`{"version":1,"batches":[{"batch":"OLD","product":"P-1","quantity":10,"unit":"kg"}]}`)
	realBytes, pinned := pinRegistry(t, realPath)
	userPath := rawJoinPath(storeDir, "missing", "..", "batches.json")
	missingDir := rawJoinPath(storeDir, "missing")
	manifest := filepath.Join(root, "in.json")
	manifestBytes := `[{"batch":"NEW","product":"P-9","quantity":5,"unit":"box"}]`
	writeFile(t, manifest, manifestBytes)

	var stdout bytes.Buffer
	err := runBatchImport([]string{"--registry", userPath, "--input", manifest}, &stdout)
	assertMissingDirCLIRejection(t, err, &stdout, userPath, missingDir, realPath, realBytes, pinned, root)
	if got := batchIDs(t, realPath); strings.Join(got, ",") != "OLD" {
		t.Fatalf("no manifest batch may land, registry = %v", got)
	}
	after, rerr := os.ReadFile(manifest)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(after) != manifestBytes {
		t.Fatalf("the read-only manifest changed:\nbefore %s\nafter  %s", manifestBytes, after)
	}
}

// The preserved neighbor behavior at the command line: a first registration
// whose parent directory simply does not exist (no ".." involved) is still
// created, and a ".." crossing only existing directories registers normally.
func TestBatchRegisterMissingParentAndExistingDirectoryDotDotStillWork(t *testing.T) {
	t.Run("missing parent without dotdot", func(t *testing.T) {
		root := t.TempDir()
		userPath := filepath.Join(root, "store", "new", "batches.json")
		var stdout bytes.Buffer
		if err := runBatchRegister([]string{
			"--registry", userPath,
			"--batch", "B1", "--product", "P1", "--quantity", "1", "--unit", "kg",
		}, &stdout); err != nil {
			t.Fatalf("a first registration in a missing directory must succeed: %v", err)
		}
		if got := batchIDs(t, userPath); strings.Join(got, ",") != "B1" {
			t.Fatalf("registry = %v, want B1", got)
		}
	})
	t.Run("dotdot across existing directories", func(t *testing.T) {
		_, storeDir, realPath := setupMissingDirCLIFixture(t,
			`{"version":1,"batches":[{"batch":"OLD","product":"P-1","quantity":10,"unit":"kg"}]}`)
		userPath := rawJoinPath(storeDir, "..", "store", "batches.json")
		var stdout bytes.Buffer
		if err := runBatchRegister([]string{
			"--registry", userPath,
			"--batch", "NEW", "--product", "P-9", "--quantity", "5", "--unit", "box",
		}, &stdout); err != nil {
			t.Fatalf("a \"..\" crossing only existing directories must succeed: %v", err)
		}
		if got := batchIDs(t, realPath); strings.Join(got, ",") != "OLD,NEW" {
			t.Fatalf("registry = %v, want OLD then NEW", got)
		}
	})
}
