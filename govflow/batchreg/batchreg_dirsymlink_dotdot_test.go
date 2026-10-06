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

// This file is the regression net for a registry reached through a DIRECTORY
// symbolic link followed by "..". With work/alias -> store/child, the user
// path work/alias/../batches.json names store/batches.json: the ".." leaves
// the directory the link really points at, not the directory holding the
// link. A save resolved lexically collapses alias/.. to work and writes next
// to the link; the rule under guard is that read and save resolve the path
// the same physical way the kernel does, so the new record lands in the real
// file, nothing is created next to the link (write access there is not even
// required), and an unrelated same-named file beside the link is untouched.

// dirLinkFixtureAt builds, under root,
//
//	root/
//	  store/
//	    child/                 (real directory)
//	    batches.json           (the real registry with one OLD-1 record)
//	  work/
//	    alias -> linkTarget    (directory link into store)
//	    batches.json           (an unrelated decoy registry)
//
// linkTarget is the exact text stored in the link. It returns the registry
// path a user would type (work/alias/../batches.json, built WITHOUT lexical
// cleaning so the link segment and the ".." both survive), the real registry
// path, the decoy path and the original real-registry bytes.
func dirLinkFixtureAt(t *testing.T, root, linkTarget string) (userPath, realPath, decoyPath, aliasPath string, original []byte) {
	t.Helper()
	storeDir := filepath.Join(root, "store")
	childDir := filepath.Join(storeDir, "child")
	if err := os.MkdirAll(childDir, 0o755); err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Join(root, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	realPath = filepath.Join(storeDir, "batches.json")
	writeRegistry(t, realPath, symlinkFixtureRegistry)
	decoyPath = filepath.Join(workDir, "batches.json")
	writeRegistry(t, decoyPath, `{"version":1,"batches":[{"batch":"DECOY","product":"P-0","quantity":1,"unit":"m"}]}`)
	aliasPath = filepath.Join(workDir, "alias")
	if err := os.Symlink(linkTarget, aliasPath); err != nil {
		t.Fatal(err)
	}
	// String concatenation on purpose: filepath.Join/Clean would collapse
	// "alias/.." to "work" and erase exactly the path shape under test.
	userPath = aliasPath + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "batches.json"
	original, err := os.ReadFile(realPath)
	if err != nil {
		t.Fatal(err)
	}
	return userPath, realPath, decoyPath, aliasPath, original
}

// dirLinkFixture builds the layout in a fresh temporary directory.
func dirLinkFixture(t *testing.T, linkTarget string) (root, userPath, realPath, decoyPath, aliasPath string, original []byte) {
	t.Helper()
	root = t.TempDir()
	userPath, realPath, decoyPath, aliasPath, original = dirLinkFixtureAt(t, root, linkTarget)
	return root, userPath, realPath, decoyPath, aliasPath, original
}

// pinFile stamps a past mtime on path so any rewrite is observable.
func pinFile(t *testing.T, path string) time.Time {
	t.Helper()
	pinned := time.Date(2008, time.August, 9, 10, 11, 12, 0, time.UTC)
	if err := os.Chtimes(path, pinned, pinned); err != nil {
		t.Fatal(err)
	}
	return pinned
}

// Reading the registry through work/alias/../batches.json sees the real
// store/batches.json, and a save of a newly registered batch appends to that
// real file: old record first, new record last, visible through the user path
// and the real path alike. The decoy beside the link keeps its bytes, the
// link keeps its target text, and no temporary file survives anywhere.
func TestSaveThroughDirectorySymlinkDotDotLandsInRealFile(t *testing.T) {
	cases := []struct {
		name   string
		target func(root string) string
	}{
		{"relative target", func(root string) string { return filepath.Join("..", "store", "child") }},
		{"absolute target", func(root string) string { return filepath.Join(root, "store", "child") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			userPath, realPath, decoyPath, aliasPath, original := dirLinkFixtureAt(t, root, tc.target(root))

			decoyBytes, err := os.ReadFile(decoyPath)
			if err != nil {
				t.Fatal(err)
			}
			realPinned := pinFile(t, realPath)
			decoyPinned := pinFile(t, decoyPath)

			reg, existed, err := Load(userPath)
			if err != nil || !existed {
				t.Fatalf("reading through the directory link must load the real registry: existed=%v err=%v", existed, err)
			}
			if len(reg.Batches) != 1 || reg.Batches[0].Batch != "OLD-1" {
				t.Fatalf("the user path must see the real records, got %+v", reg.Batches)
			}
			if _, err := Register(reg, Input{Batch: "NEW-1", Product: "P-9", Quantity: 5, Unit: "box"}); err != nil {
				t.Fatal(err)
			}
			if err := Save(userPath, reg); err != nil {
				t.Fatalf("saving through the directory link must succeed: %v", err)
			}

			want := []Batch{
				saveBatch("OLD-1", "P-1", 10, "kg"),
				saveBatch("NEW-1", "P-9", 5, "box"),
			}
			assertBatchesEqual(t, mustLoad(t, realPath).Batches, want)
			assertBatchesEqual(t, mustLoad(t, userPath).Batches, want)

			// The real file is the one rewritten: its pinned mtime must move.
			if info, err := os.Stat(realPath); err != nil {
				t.Fatal(err)
			} else if info.ModTime().Equal(realPinned) {
				t.Fatal("the real registry was not rewritten; the save landed elsewhere")
			}
			// The decoy beside the link is unrelated: bytes and mtime intact.
			after, err := os.ReadFile(decoyPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, decoyBytes) {
				t.Fatalf("the decoy beside the link changed:\nbefore %s\nafter  %s", decoyBytes, after)
			}
			if info, err := os.Stat(decoyPath); err != nil {
				t.Fatal(err)
			} else if !info.ModTime().Equal(decoyPinned) {
				t.Fatalf("the decoy mtime moved: got %v want %v", info.ModTime(), decoyPinned)
			}
			// Nothing was written next to the link: only the link and the
			// decoy may exist there.
			entries, err := os.ReadDir(filepath.Dir(aliasPath))
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if e.Name() != "alias" && e.Name() != "batches.json" {
					t.Fatalf("save created %q next to the directory link", e.Name())
				}
			}
			assertLinkIntact(t, aliasPath, tc.target(root))
			assertNoLeftoverTempFiles(t, root)
			_ = original
		})
	}
}

// mustLoad loads path or fails the test; it keeps the fixtures readable.
func mustLoad(t *testing.T, path string) *Registry {
	t.Helper()
	reg, existed, err := Load(path)
	if err != nil || !existed {
		t.Fatalf("Load(%q): existed=%v err=%v", path, existed, err)
	}
	return reg
}

// A save through work/alias/../batches.json must not require write access in
// work: making the link's own directory read-and-execute-only leaves the save
// working, because the temporary file is prepared in the real target
// directory. Skipped under root, which bypasses directory permission bits.
func TestSaveThroughDirectorySymlinkDotDotNeedsNoWriteInLinkDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permission bits")
	}
	root, userPath, realPath, decoyPath, aliasPath, _ := dirLinkFixture(t, filepath.Join("..", "store", "child"))
	workDir := filepath.Dir(aliasPath)
	if err := os.Chmod(workDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(workDir, 0o755) })

	reg := mustLoad(t, userPath)
	if _, err := Register(reg, Input{Batch: "NEW-1", Product: "P-9", Quantity: 5, Unit: "box"}); err != nil {
		t.Fatal(err)
	}
	if err := Save(userPath, reg); err != nil {
		t.Fatalf("a read-only link directory must not block the save: %v", err)
	}
	assertBatchesEqual(t, mustLoad(t, realPath).Batches, []Batch{
		saveBatch("OLD-1", "P-1", 10, "kg"),
		saveBatch("NEW-1", "P-9", 5, "box"),
	})
	assertBatchesEqual(t, mustLoad(t, userPath).Batches, mustLoad(t, realPath).Batches)
	assertLinkIntact(t, aliasPath, filepath.Join("..", "store", "child"))
	assertNoLeftoverTempFiles(t, root)
	// The read-only directory must contain only what it started with.
	entries, err := os.ReadDir(workDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "alias" && e.Name() != "batches.json" {
			t.Fatalf("save created %q in the read-only link directory", e.Name())
		}
	}
	_ = decoyPath
}

// The atomic rename — the step that actually chooses which file is replaced —
// must target the real store/batches.json, never the same-named decoy beside
// the link. This works under every account, root included, because it
// observes Save's own resolved target.
func TestSaveThroughDirectorySymlinkDotDotRenamesRealFile(t *testing.T) {
	root, userPath, realPath, decoyPath, aliasPath, original := dirLinkFixture(t, filepath.Join("..", "store", "child"))
	pinned := pinFile(t, realPath)
	decoyBytes, err := os.ReadFile(decoyPath)
	if err != nil {
		t.Fatal(err)
	}

	cause := errors.New("injected replacement refusal through directory link")
	var renamedTo string
	old := RenameTempFile
	RenameTempFile = func(oldpath, newpath string) error {
		renamedTo = newpath
		return cause
	}
	t.Cleanup(func() { RenameTempFile = old })

	reg := mustLoad(t, userPath)
	if _, err := Register(reg, Input{Batch: "NEW-1", Product: "P-9", Quantity: 5, Unit: "box"}); err != nil {
		t.Fatal(err)
	}
	err = Save(userPath, reg)
	if err == nil {
		t.Fatal("Save must surface the injected rename failure")
	}
	if !errors.Is(err, cause) {
		t.Errorf("error %v must carry the cause %v", err, cause)
	}
	if !strings.Contains(err.Error(), userPath) {
		t.Errorf("error %q must name the user-supplied path %q", err.Error(), userPath)
	}
	if renamedTo != realPath {
		t.Fatalf("rename target = %q, want the real registry %q", renamedTo, realPath)
	}
	if renamedTo == decoyPath {
		t.Fatal("the save must never rename onto the decoy beside the link")
	}

	// Failed save: real registry keeps bytes and mtime, decoy untouched, no
	// temporary file survives, the link is unchanged.
	if info, err := os.Stat(realPath); err != nil {
		t.Fatal(err)
	} else if !info.ModTime().Equal(pinned) {
		t.Fatalf("failed save moved the real registry mtime: got %v want %v", info.ModTime(), pinned)
	}
	after, err := os.ReadFile(realPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatalf("failed save changed the real registry bytes:\nbefore %s\nafter  %s", original, after)
	}
	decoyAfter, err := os.ReadFile(decoyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoyAfter, decoyBytes) {
		t.Fatalf("failed save changed the decoy bytes:\nbefore %s\nafter  %s", decoyBytes, decoyAfter)
	}
	assertLinkIntact(t, aliasPath, filepath.Join("..", "store", "child"))
	assertNoLeftoverTempFiles(t, root)
}

// A first registration addressed through work/alias/../fresh.json creates the
// file in the real store directory, not next to the link.
func TestSaveNewFileThroughDirectorySymlinkDotDotLandsInRealDirectory(t *testing.T) {
	root, _, _, decoyPath, aliasPath, _ := dirLinkFixture(t, filepath.Join("..", "store", "child"))
	userPath := aliasPath + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "fresh.json"
	realFresh := filepath.Join(root, "store", "fresh.json")
	linkDirFresh := filepath.Join(root, "work", "fresh.json")

	reg := &Registry{Version: FormatVersion, Batches: []Batch{saveBatch("B1", "P1", 1, "kg")}}
	if err := Save(userPath, reg); err != nil {
		t.Fatalf("first registration through the directory link must save: %v", err)
	}
	if _, err := os.Stat(realFresh); err != nil {
		t.Fatalf("the new registry must be created in the real directory: %v", err)
	}
	if _, err := os.Stat(linkDirFresh); !os.IsNotExist(err) {
		t.Fatalf("no file may be created next to the link, stat err=%v", err)
	}
	assertBatchesEqual(t, mustLoad(t, userPath).Batches, mustLoad(t, realFresh).Batches)
	assertLinkIntact(t, aliasPath, filepath.Join("..", "store", "child"))
	assertNoLeftoverTempFiles(t, root)
	_ = decoyPath
}

// When the REAL target directory is not writable, the save is rejected: the
// error names the user-supplied path (not the resolved internal one) and the
// reason, the real registry keeps bytes and mtime, the decoy and link are
// untouched and no temporary file remains. Permission bits are root-bypassed,
// so the unwritable-directory case is skipped for root and covered portably by
// the injected rename test above.
func TestSaveThroughDirectorySymlinkDotDotFailsWhenRealDirectoryUnwritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permission bits")
	}
	root, userPath, realPath, decoyPath, aliasPath, original := dirLinkFixture(t, filepath.Join("..", "store", "child"))
	storeDir := filepath.Join(root, "store")
	pinned := pinFile(t, realPath)
	decoyBytes, err := os.ReadFile(decoyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(storeDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(storeDir, 0o755) })

	reg := mustLoad(t, userPath)
	if _, err := Register(reg, Input{Batch: "NEW-1", Product: "P-9", Quantity: 5, Unit: "box"}); err != nil {
		t.Fatal(err)
	}
	err = Save(userPath, reg)
	if err == nil {
		t.Fatal("a save into an unwritable real target directory must fail")
	}
	msg := err.Error()
	if !strings.Contains(msg, userPath) {
		t.Errorf("error %q must name the user-supplied registry path %q", msg, userPath)
	}
	if !strings.Contains(msg, "permission denied") {
		t.Errorf("error %q must state the concrete reason", msg)
	}
	if info, err := os.Stat(realPath); err != nil {
		t.Fatal(err)
	} else if !info.ModTime().Equal(pinned) {
		t.Fatalf("failed save moved the real registry mtime: got %v want %v", info.ModTime(), pinned)
	}
	after, err := os.ReadFile(realPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatalf("failed save changed the real registry bytes:\nbefore %s\nafter  %s", original, after)
	}
	decoyAfter, err := os.ReadFile(decoyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoyAfter, decoyBytes) {
		t.Fatalf("the decoy beside the link changed:\nbefore %s\nafter  %s", decoyBytes, decoyAfter)
	}
	assertLinkIntact(t, aliasPath, filepath.Join("..", "store", "child"))
	assertNoLeftoverTempFiles(t, root)
}

// A dangling directory link followed by ".." is not a first registration: the
// save rejects it as an unresolvable symbolic link instead of dropping a file
// next to the link, and nothing is created anywhere.
func TestSaveThroughDanglingDirectorySymlinkDotDotRejected(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A decoy same-named file beside the link is what a lexical save would
	// overwrite; it must survive byte-for-byte.
	decoyPath := filepath.Join(workDir, "batches.json")
	decoy := `{"version":1,"batches":[{"batch":"DECOY","product":"P","quantity":1,"unit":"kg"}]}`
	writeRegistry(t, decoyPath, decoy)
	aliasPath := filepath.Join(workDir, "alias")
	missingDir := filepath.Join("..", "store", "missing-child")
	if err := os.Symlink(missingDir, aliasPath); err != nil {
		t.Fatal(err)
	}
	userPath := aliasPath + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "batches.json"
	pinned := pinFile(t, decoyPath)

	reg := &Registry{Version: FormatVersion, Batches: []Batch{saveBatch("B1", "P1", 1, "kg")}}
	err := Save(userPath, reg)
	if err == nil {
		t.Fatal("a save through a dangling directory link must be rejected")
	}
	msg := err.Error()
	if !strings.Contains(msg, userPath) {
		t.Errorf("error %q must name the user-supplied path %q", msg, userPath)
	}
	if !strings.Contains(msg, "symbolic link") {
		t.Errorf("error %q must state the path is an unresolvable symbolic link", msg)
	}
	assertLinkIntact(t, aliasPath, missingDir)
	after, err := os.ReadFile(decoyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != decoy {
		t.Fatalf("the lexical-resolution decoy was overwritten:\nwant %s\ngot  %s", decoy, after)
	}
	if info, err := os.Stat(decoyPath); err != nil {
		t.Fatal(err)
	} else if !info.ModTime().Equal(pinned) {
		t.Fatalf("the decoy mtime moved: got %v want %v", info.ModTime(), pinned)
	}
	assertNoLeftoverTempFiles(t, root)
}

// A loop of directory links reached with ".." likewise rejects the save
// without creating anything next to either link.
func TestSaveThroughDirectorySymlinkLoopDotDotRejected(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	linkA := filepath.Join(workDir, "a")
	linkB := filepath.Join(workDir, "b")
	if err := os.Symlink("b", linkA); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a", linkB); err != nil {
		t.Fatal(err)
	}
	userPath := linkA + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "batches.json"

	reg := &Registry{Version: FormatVersion, Batches: []Batch{saveBatch("B1", "P1", 1, "kg")}}
	err := Save(userPath, reg)
	if err == nil {
		t.Fatal("a save through a directory-link loop must be rejected")
	}
	if !strings.Contains(err.Error(), userPath) {
		t.Errorf("error %q must name the user-supplied path %q", err.Error(), userPath)
	}
	assertLinkIntact(t, linkA, "b")
	assertLinkIntact(t, linkB, "a")
	assertNoLeftoverTempFiles(t, root)
	if entries, err := os.ReadDir(workDir); err != nil {
		t.Fatal(err)
	} else if len(entries) != 2 {
		t.Fatalf("nothing may be created next to the looping links, got %v", entries)
	}
}
