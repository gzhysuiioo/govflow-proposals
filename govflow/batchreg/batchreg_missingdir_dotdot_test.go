package batchreg

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This file is the regression net for a registry path that would have to
// enter a directory that does not exist and then step back out of it through
// a later ".." component — store/missing/../batches.json with store/missing
// absent. The kernel cannot follow such a prefix, so the user path never
// reaches the same-named file the collapsed path would name. A save that
// collapsed "missing/.." lexically would silently overwrite that unrelated
// registry (losing every batch it holds) or create one the user never asked
// for; pre-creating the missing directory would make an unreachable path
// valid behind the user's back. The rule under guard: the save is rejected
// naming the user-supplied path, the missing directory and the ".." reason;
// the existing same-named file keeps its bytes and modification time; no
// directory, registry file or temporary file is left behind; and the
// caller's records are unchanged. Relative paths and absolute paths with the
// same directory relationship behave identically, while a ".." that only
// crosses directories which actually exist, and a first registration in a
// missing directory without any "..", keep working.

// rawJoin concatenates path segments with the path separator WITHOUT any
// lexical cleaning: filepath.Join would collapse the "missing/.." pair and
// erase exactly the path shape under test.
func rawJoin(segments ...string) string {
	return strings.Join(segments, string(os.PathSeparator))
}

// missingDirFixture builds, under root,
//
//	root/
//	  store/
//	    batches.json           (the real registry with one OLD record)
//
// and returns the store directory, the real registry path and its bytes.
func missingDirFixture(t *testing.T, root string) (storeDir, realPath string, original []byte) {
	t.Helper()
	storeDir = filepath.Join(root, "store")
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	realPath = filepath.Join(storeDir, "batches.json")
	writeRegistry(t, realPath, `{"version":1,"batches":[{"batch":"OLD","product":"P-1","quantity":10,"unit":"kg"}]}`)
	original, err := os.ReadFile(realPath)
	if err != nil {
		t.Fatal(err)
	}
	return storeDir, realPath, original
}

// assertMissingDirRejection checks the full aftermath of a rejected save
// through a missing-directory/".." path: the error names the user-supplied
// path, the missing directory and the ".." reason; the real registry keeps
// its bytes and pinned mtime; the missing directory was never created; no
// temporary file survives; and the caller's records are unchanged.
func assertMissingDirRejection(t *testing.T, err error, userPath, missingDir string, reg *Registry, submitted []Batch, realPath string, original []byte, pinned time.Time, root string) {
	t.Helper()
	if err == nil {
		t.Fatal("a save through a missing directory followed by \"..\" must be rejected")
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
	after, rerr := os.ReadFile(realPath)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !bytes.Equal(after, original) {
		t.Fatalf("the existing registry changed:\nbefore %s\nafter  %s", original, after)
	}
	if info, serr := os.Stat(realPath); serr != nil {
		t.Fatal(serr)
	} else if !info.ModTime().Equal(pinned) {
		t.Fatalf("the existing registry mtime moved: got %v want %v", info.ModTime(), pinned)
	}
	if _, serr := os.Lstat(missingDir); !os.IsNotExist(serr) {
		t.Fatalf("the missing directory must not be created, stat err=%v", serr)
	}
	assertNoLeftoverTempFiles(t, root)
	assertBatchesEqual(t, reg.Batches, submitted)
}

// A save addressed through store/missing/../batches.json — where the missing
// directory would have to be entered before ".." steps back out of it — is
// rejected even though the collapsed path names a real registry: that file
// keeps its OLD record byte-for-byte, no NEW record lands, and nothing is
// created. The relative path and the absolute path with the same directory
// relationship are rejected identically.
func TestSaveThroughMissingDirectoryDotDotRejected(t *testing.T) {
	t.Run("relative", func(t *testing.T) {
		root := t.TempDir()
		_, realPath, original := missingDirFixture(t, root)
		pinned := pinFile(t, realPath)
		t.Chdir(root)
		userPath := rawJoin("store", "missing", "..", "batches.json")
		missingDir := rawJoin("store", "missing")

		batches := []Batch{saveBatch("NEW", "P-9", 5, "box")}
		submitted := append([]Batch(nil), batches...)
		reg := &Registry{Version: FormatVersion, Batches: batches}
		err := Save(userPath, reg)
		assertMissingDirRejection(t, err, userPath, missingDir, reg, submitted, realPath, original, pinned, root)
		if got := mustLoad(t, realPath).Batches; len(got) != 1 || got[0].Batch != "OLD" {
			t.Fatalf("the real registry must still hold only OLD, got %+v", got)
		}
	})
	t.Run("absolute", func(t *testing.T) {
		root := t.TempDir()
		storeDir, realPath, original := missingDirFixture(t, root)
		pinned := pinFile(t, realPath)
		userPath := rawJoin(storeDir, "missing", "..", "batches.json")
		missingDir := rawJoin(storeDir, "missing")

		batches := []Batch{saveBatch("NEW", "P-9", 5, "box")}
		submitted := append([]Batch(nil), batches...)
		reg := &Registry{Version: FormatVersion, Batches: batches}
		err := Save(userPath, reg)
		assertMissingDirRejection(t, err, userPath, missingDir, reg, submitted, realPath, original, pinned, root)
		if got := mustLoad(t, realPath).Batches; len(got) != 1 || got[0].Batch != "OLD" {
			t.Fatalf("the real registry must still hold only OLD, got %+v", got)
		}
	})
}

// The rejection does not depend on the collapsed path naming an existing
// file: store/missing/../fresh.json, where no same-named file exists, must
// not be created either, and the missing directory must not appear.
func TestSaveNewFileThroughMissingDirectoryDotDotRejected(t *testing.T) {
	root := t.TempDir()
	storeDir, realPath, original := missingDirFixture(t, root)
	pinned := pinFile(t, realPath)
	userPath := rawJoin(storeDir, "missing", "..", "fresh.json")
	missingDir := rawJoin(storeDir, "missing")
	collapsed := filepath.Join(storeDir, "fresh.json")

	batches := []Batch{saveBatch("NEW", "P-9", 5, "box")}
	submitted := append([]Batch(nil), batches...)
	reg := &Registry{Version: FormatVersion, Batches: batches}
	err := Save(userPath, reg)
	assertMissingDirRejection(t, err, userPath, missingDir, reg, submitted, realPath, original, pinned, root)
	if _, serr := os.Lstat(collapsed); !os.IsNotExist(serr) {
		t.Fatalf("the collapsed target must not be created, stat err=%v", serr)
	}
}

// Extra ".." components do not sneak past the rule: store/missing/../../x/
// batches.json still requires entering the missing store/missing before
// stepping back, so it is rejected and nothing (no directory, no file) is
// created anywhere.
func TestSaveThroughMissingDirectoryDoubleDotDotRejected(t *testing.T) {
	root := t.TempDir()
	storeDir, realPath, original := missingDirFixture(t, root)
	pinned := pinFile(t, realPath)
	userPath := rawJoin(storeDir, "missing", "..", "..", "x", "batches.json")
	missingDir := rawJoin(storeDir, "missing")

	batches := []Batch{saveBatch("NEW", "P-9", 5, "box")}
	submitted := append([]Batch(nil), batches...)
	reg := &Registry{Version: FormatVersion, Batches: batches}
	err := Save(userPath, reg)
	assertMissingDirRejection(t, err, userPath, missingDir, reg, submitted, realPath, original, pinned, root)
	if _, serr := os.Lstat(filepath.Join(root, "x")); !os.IsNotExist(serr) {
		t.Fatalf("no directory may be created for the collapsed path, stat err=%v", serr)
	}
}

// The preserved behaviors around the new rule, in one place: a first
// registration whose parent directory simply does not exist yet (no ".." at
// all) is still created normally, and a ".." that only crosses directories
// which actually exist keeps its ordinary physical meaning.
func TestSaveMissingParentAndExistingDirectoryDotDotStillWork(t *testing.T) {
	t.Run("missing parent without dotdot", func(t *testing.T) {
		root := t.TempDir()
		userPath := filepath.Join(root, "store", "new", "batches.json")
		reg := &Registry{Version: FormatVersion, Batches: []Batch{saveBatch("B1", "P1", 1, "kg")}}
		if err := Save(userPath, reg); err != nil {
			t.Fatalf("a first registration in a missing directory must save: %v", err)
		}
		assertBatchesEqual(t, mustLoad(t, userPath).Batches, []Batch{saveBatch("B1", "P1", 1, "kg")})
	})
	t.Run("dotdot across existing directories", func(t *testing.T) {
		root := t.TempDir()
		storeDir, realPath, _ := missingDirFixture(t, root)
		userPath := rawJoin(storeDir, "..", "store", "batches.json")
		reg := mustLoad(t, userPath)
		if _, err := Register(reg, Input{Batch: "NEW", Product: "P-9", Quantity: 5, Unit: "box"}); err != nil {
			t.Fatal(err)
		}
		if err := Save(userPath, reg); err != nil {
			t.Fatalf("a \"..\" crossing only existing directories must save: %v", err)
		}
		assertBatchesEqual(t, mustLoad(t, realPath).Batches, []Batch{
			saveBatch("OLD", "P-1", 10, "kg"),
			saveBatch("NEW", "P-9", 5, "box"),
		})
	})
}
