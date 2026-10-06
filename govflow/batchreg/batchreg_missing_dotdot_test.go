package batchreg

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This file is the regression net for the missing-directory-then-".." escape.
// With store/batches.json registered and store/missing absent, the user path
// store/missing/../batches.json does not name any file the way the kernel
// resolves names: the ".." cannot step out of "missing" while "missing" does
// not exist. A save that collapses the path lexically (or first creates
// "missing") would reach store/batches.json and overwrite the registry with
// just the new batch. The rule under guard is that Save refuses such a path
// before creating anything: the error names the user-supplied path and the
// reason, an existing same-named file keeps its bytes and modification time,
// no missing directory, registry file or temporary file appears, and the
// caller's submitted batches keep their content and order. Relative paths and
// the absolute paths with the same directory structure behave identically.
//
// Ordinary creation is preserved: a path whose parent merely does not exist
// without climbing back (store/new/batches.json) still creates the directory
// and the registry, and a ".." after a directory that really exists keeps
// resolving by real directory relations — including a directory symlink,
// covered in batchreg_dirsymlink_dotdot_test.go.

// missingDotDotRegistryJSON is the one-record registry the fixtures start
// with; a successful escape would leave only the new batch behind.
const missingDotDotRegistryJSON = `{"version":1,"batches":[{"batch":"OLD","product":"P-1","quantity":10,"unit":"kg"}]}`

// missingEscapeFixture builds
//
//	root/
//	  store/
//	    batches.json   (registry holding OLD, mtime pinned)
//
// with no store/missing directory, and returns root and the registry path.
func missingEscapeFixture(t *testing.T) (root, registry string, original []byte, pinned time.Time) {
	t.Helper()
	root = t.TempDir()
	storeDir := filepath.Join(root, "store")
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	registry = filepath.Join(storeDir, "batches.json")
	writeRegistry(t, registry, missingDotDotRegistryJSON)
	pinned = pinFile(t, registry)
	original, err := os.ReadFile(registry)
	if err != nil {
		t.Fatal(err)
	}
	return root, registry, original, pinned
}

// assertMissingDirAbsent fails if the missing directory named in the escape
// path was brought into existence by the rejected save.
func assertMissingDirAbsent(t *testing.T, root, rel string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(root, rel)); !os.IsNotExist(err) {
		t.Fatalf("the rejected save must not create %s, stat err=%v", rel, err)
	}
}

// rawJoin joins components with the OS separator WITHOUT lexical cleaning, so
// the deliberately unresolvable "missing/.." segment reaches Save exactly as
// a user would type it (filepath.Join/Clean would erase it).
func rawJoin(parts ...string) string {
	return strings.Join(parts, string(os.PathSeparator))
}

// assertEscapeRejected checks the shared error contract: a non-nil error that
// quotes the exact user path, names the missing directory and says the path
// climbs back with "..".
func assertEscapeRejected(t *testing.T, err error, userPath, missingName string) {
	t.Helper()
	if err == nil {
		t.Fatal("Save must reject a path that descends into a missing directory and climbs back with \"..\"")
	}
	msg := err.Error()
	for _, want := range []string{
		userPath,
		"does not exist",
		"..",
		"not written",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q must mention %q", msg, want)
		}
	}
	if missingName != "" && !strings.Contains(msg, missingName) {
		t.Errorf("error %q must name the missing directory %q", msg, missingName)
	}
}

// Saving a legal registry addressed through store/missing/../batches.json
// must fail while store/batches.json keeps OLD: exact bytes, pinned mtime, no
// store/missing directory, no replacement file anywhere and no temporary file.
// The submitted batches and their order are untouched.
func TestSaveRejectsMissingDirDotDotOverExistingRegistry(t *testing.T) {
	root, registry, original, pinned := missingEscapeFixture(t)
	userPath := rawJoin(root, "store", "missing", "..", "batches.json")

	submitted := []Batch{saveBatch("NEW", "P-9", 5, "box")}
	reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), submitted...)}
	err := Save(userPath, reg)

	assertEscapeRejected(t, err, userPath, "missing")
	after, statErr := os.ReadFile(registry)
	if statErr != nil {
		t.Fatalf("the existing registry must still be present: %v", statErr)
	}
	if !bytes.Equal(after, original) {
		t.Fatalf("the escape overwrote the existing registry:\nbefore %s\nafter  %s", original, after)
	}
	if info, err := os.Stat(registry); err != nil {
		t.Fatal(err)
	} else if !info.ModTime().Equal(pinned) {
		t.Fatalf("the escape moved the registry mtime: got %v want %v", info.ModTime(), pinned)
	}
	assertMissingDirAbsent(t, root, filepath.Join("store", "missing"))
	assertNoLeftoverTempFiles(t, root)
	assertBatchesEqual(t, reg.Batches, submitted)
	// The surviving registry still reads back with its single OLD record.
	assertBatchesEqual(t, mustLoad(t, registry).Batches,
		[]Batch{saveBatch("OLD", "P-1", 10, "kg")})
}

// Even with no same-named file at the lexically cleaned location, the path is
// not a place to create a fresh registry: the save is rejected and neither
// the file nor the missing directory appears.
func TestSaveRejectsMissingDirDotDotWithoutExistingFileCreatesNothing(t *testing.T) {
	root := t.TempDir()
	storeDir := filepath.Join(root, "store")
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	userPath := rawJoin(root, "store", "missing", "..", "fresh.json")

	reg := &Registry{Version: FormatVersion, Batches: []Batch{saveBatch("NEW", "P-9", 5, "box")}}
	err := Save(userPath, reg)

	assertEscapeRejected(t, err, userPath, "missing")
	if _, statErr := os.Lstat(filepath.Join(storeDir, "fresh.json")); !os.IsNotExist(statErr) {
		t.Fatalf("no registry may be created at the cleaned name, stat err=%v", statErr)
	}
	assertMissingDirAbsent(t, root, filepath.Join("store", "missing"))
	if entries, err := os.ReadDir(storeDir); err != nil {
		t.Fatal(err)
	} else if len(entries) != 0 {
		t.Fatalf("the rejected save left entries in store: %v", entries)
	}
	assertNoLeftoverTempFiles(t, root)
}

// A path that climbs past the missing directory more than once
// (store/missing/../../root-file) is rejected just the same and creates
// nothing outside store either.
func TestSaveRejectsMissingDirDotDotClimbingPastParent(t *testing.T) {
	root := t.TempDir()
	storeDir := filepath.Join(root, "store")
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	userPath := rawJoin(root, "store", "missing", "..", "..", "outside.json")

	reg := &Registry{Version: FormatVersion, Batches: []Batch{saveBatch("NEW", "P-9", 5, "box")}}
	err := Save(userPath, reg)

	assertEscapeRejected(t, err, userPath, "missing")
	for _, p := range []string{
		filepath.Join(root, "outside.json"),
		filepath.Join(storeDir, "outside.json"),
		filepath.Join(storeDir, "missing"),
	} {
		if _, statErr := os.Lstat(p); !os.IsNotExist(statErr) {
			t.Fatalf("the rejected save must not create %s, stat err=%v", p, statErr)
		}
	}
	assertNoLeftoverTempFiles(t, root)
}

// A relative registry path and the absolute path spelling the same directory
// structure must give the same verdict and leave the same files behind.
func TestSaveMissingDirDotDotRelativeAndAbsoluteAgree(t *testing.T) {
	setup := func(t *testing.T) string {
		t.Helper()
		root := t.TempDir()
		writeRegistry(t, filepath.Join(root, "store", "batches.json"), missingDotDotRegistryJSON)
		t.Chdir(root)
		return root
	}

	t.Run("relative", func(t *testing.T) {
		root := setup(t)
		userPath := rawJoin("store", "missing", "..", "batches.json")
		reg := &Registry{Version: FormatVersion, Batches: []Batch{saveBatch("NEW", "P-9", 5, "box")}}
		err := Save(userPath, reg)
		assertEscapeRejected(t, err, userPath, "missing")
		assertMissingDirAbsent(t, root, filepath.Join("store", "missing"))
		assertBatchesEqual(t, mustLoad(t, filepath.Join(root, "store", "batches.json")).Batches,
			[]Batch{saveBatch("OLD", "P-1", 10, "kg")})
	})
	t.Run("absolute", func(t *testing.T) {
		root := setup(t)
		userPath := rawJoin(root, "store", "missing", "..", "batches.json")
		reg := &Registry{Version: FormatVersion, Batches: []Batch{saveBatch("NEW", "P-9", 5, "box")}}
		err := Save(userPath, reg)
		assertEscapeRejected(t, err, userPath, "missing")
		assertMissingDirAbsent(t, root, filepath.Join("store", "missing"))
		assertBatchesEqual(t, mustLoad(t, filepath.Join(root, "store", "batches.json")).Batches,
			[]Batch{saveBatch("OLD", "P-1", 10, "kg")})
	})
}

// A bare relative path whose first component is the missing directory
// ("missing/../b.json" run from a directory without "missing") is refused too:
// the no-separator termination must not let the shape escape the check.
func TestSaveRejectsBareRelativeMissingDirDotDot(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	userPath := rawJoin("missing", "..", "fresh.json")

	reg := &Registry{Version: FormatVersion, Batches: []Batch{saveBatch("NEW", "P-9", 5, "box")}}
	err := Save(userPath, reg)

	assertEscapeRejected(t, err, userPath, "missing")
	assertMissingDirAbsent(t, root, "missing")
	if _, statErr := os.Lstat(filepath.Join(root, "fresh.json")); !os.IsNotExist(statErr) {
		t.Fatalf("no file may be created at the cleaned name, stat err=%v", statErr)
	}
	assertNoLeftoverTempFiles(t, root)
}

// Normal first registration is preserved: creating store/new/batches.json,
// whose parent does not exist and which never climbs back, still makes the
// directory and writes the registry.
func TestSaveCreatesNewDirectoriesWithoutDotDot(t *testing.T) {
	root := t.TempDir()
	userPath := filepath.Join(root, "store", "new", "batches.json")

	want := []Batch{saveBatch("NEW", "P-9", 5, "box")}
	reg := &Registry{Version: FormatVersion, Batches: want}
	if err := Save(userPath, reg); err != nil {
		t.Fatalf("a first registration in a new directory must succeed: %v", err)
	}
	assertBatchesEqual(t, mustLoad(t, userPath).Batches, want)
	assertNoLeftoverTempFiles(t, root)
}

// A ".." after a directory that really exists keeps resolving by real
// directory relations and must not be mistaken for the rejected shape.
func TestSaveAllowsDotDotAfterExistingDirectory(t *testing.T) {
	root := t.TempDir()
	registry := filepath.Join(root, "store", "batches.json")
	writeRegistry(t, registry, missingDotDotRegistryJSON)
	// store/child is a genuine directory; store/child/../batches.json is the
	// real registry.
	if err := os.MkdirAll(filepath.Join(root, "store", "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	userPath := filepath.Join(root, "store", "child", "..", "batches.json")

	reg := mustLoad(t, userPath)
	if _, err := Register(reg, Input{Batch: "NEW", Product: "P-9", Quantity: 5, Unit: "box"}); err != nil {
		t.Fatal(err)
	}
	if err := Save(userPath, reg); err != nil {
		t.Fatalf("a \"..\" after a real directory must resolve and save: %v", err)
	}
	assertBatchesEqual(t, mustLoad(t, registry).Batches, []Batch{
		saveBatch("OLD", "P-1", 10, "kg"),
		saveBatch("NEW", "P-9", 5, "box"),
	})
	assertNoLeftoverTempFiles(t, root)

	// A missing directory that appears only AFTER a physical ".." is ordinary
	// new-directory creation: store/../store/newer/batches.json.
	newer := filepath.Join(root, "store", "newer", "batches.json")
	userPath2 := filepath.Join(root, "store", "..", "store", "newer", "batches.json")
	reg2 := &Registry{Version: FormatVersion, Batches: []Batch{saveBatch("B1", "P1", 1, "kg")}}
	if err := Save(userPath2, reg2); err != nil {
		t.Fatalf("a missing directory after a physical \"..\" must be creatable: %v", err)
	}
	assertBatchesEqual(t, mustLoad(t, newer).Batches, []Batch{saveBatch("B1", "P1", 1, "kg")})
}

// A "." interspersed in the not-yet-existing tail does not launder the escape:
// store/./missing/../fresh.json is still rejected, while store/./child/.. with
// child real is an ordinary save.
func TestSaveDotSegmentDoesNotLaunderMissingDirDotDot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "store"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Assemble with separators directly; filepath.Join would keep the "." but
	// be explicit about the raw shape under test.
	userPath := filepath.Join(root, "store") + string(os.PathSeparator) +
		"." + string(os.PathSeparator) + "missing" + string(os.PathSeparator) +
		".." + string(os.PathSeparator) + "fresh.json"

	reg := &Registry{Version: FormatVersion, Batches: []Batch{saveBatch("NEW", "P-9", 5, "box")}}
	err := Save(userPath, reg)

	assertEscapeRejected(t, err, userPath, "missing")
	assertMissingDirAbsent(t, root, filepath.Join("store", "missing"))
	if _, statErr := os.Lstat(filepath.Join(root, "store", "fresh.json")); !os.IsNotExist(statErr) {
		t.Fatalf("no file may be created at the cleaned name, stat err=%v", statErr)
	}
	assertNoLeftoverTempFiles(t, root)
}
