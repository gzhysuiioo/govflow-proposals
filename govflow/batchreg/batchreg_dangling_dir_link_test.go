package batchreg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This file is the regression net for reading a registry through a directory
// symbolic link whose target cannot be resolved. The kernel reports an
// ordinary "no such file" when a dangling link sits on a directory component
// — work/alias -> ../store/missing with work/alias/batches.json — and also
// when a ".." follows the unresolvable link, so Load used to answer with an
// empty registry and existed == false, handing the caller a first
// registration it must never get. The rule under guard: Load rejects such a
// path with a non-nil error and a nil registry, the error quotes the registry
// path the caller passed verbatim and says the directory link target cannot
// be resolved, nothing is created and no existing file is touched; while a
// valid directory link (with or without "..") and an ordinary missing file
// or missing parent directory keep working as before.

// danglingDirLinkLayout builds
//
//	root/
//	  store/                  (real, but no "missing" child)
//	  work/                   (real directory ready to hold the link)
//
// and returns work/alias for the caller to create as a link. Decoy registries
// are added with putDecoyRegistries where the test needs them.
func danglingDirLinkLayout(t *testing.T) (root, workDir, storeDir, aliasPath string) {
	t.Helper()
	root = t.TempDir()
	workDir = filepath.Join(root, "work")
	storeDir = filepath.Join(root, "store")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return root, workDir, storeDir, filepath.Join(workDir, "alias")
}

// putDecoyRegistries writes the two unrelated same-named registries used to
// prove a rejected read never falls back onto another file: one beside the
// link (work/batches.json) and one in the target's parent directory
// (store/batches.json).
func putDecoyRegistries(t *testing.T, workDir, storeDir string) {
	t.Helper()
	writeRegistry(t, filepath.Join(workDir, "batches.json"),
		`{"version":1,"batches":[{"batch":"LINK-DIR-DECOY","product":"P-0","quantity":1,"unit":"m"}]}`)
	writeRegistry(t, filepath.Join(storeDir, "batches.json"),
		`{"version":1,"batches":[{"batch":"TARGET-DIR-DECOY","product":"P-0","quantity":1,"unit":"m"}]}`)
}

// assertDanglingDirLinkRejected checks the shared Load contract for a path
// that descends through an unresolvable directory link: non-nil error, nil
// registry, no first-registration signal, the user path quoted verbatim and
// the reason identifying the directory symbolic link.
func assertDanglingDirLinkRejected(t *testing.T, userPath, linkPath string, reg *Registry, existed bool, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("Load(%q) must reject a path through an unresolvable directory link, got reg=%+v existed=%v", userPath, reg, existed)
	}
	if reg != nil {
		t.Errorf("Load(%q) must return a nil registry on rejection, got %+v", userPath, reg)
	}
	if !existed {
		t.Errorf("Load(%q) must not signal a first registration (existed=false)", userPath)
	}
	msg := err.Error()
	if !strings.Contains(msg, userPath) {
		t.Errorf("error %q must quote the registry path the caller passed %q verbatim", msg, userPath)
	}
	if !strings.Contains(msg, "symbolic link") {
		t.Errorf("error %q must identify the symbolic link", msg)
	}
	if !strings.Contains(msg, linkPath) {
		t.Errorf("error %q must name the offending link %q", msg, linkPath)
	}
	if !strings.Contains(msg, "cannot be resolved") {
		t.Errorf("error %q must state the link target cannot be resolved", msg)
	}
	if strings.Contains(msg, "has not been created") || strings.Contains(msg, "not yet created") {
		t.Errorf("error %q must not describe the path as a merely uncreated registry", msg)
	}
}

// assertFileUnchanged fails unless path still holds want bytes and the pinned
// modification time.
func assertFileUnchanged(t *testing.T, path string, want []byte, pinned time.Time) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the existing file %s must survive: %v", path, err)
	}
	if !info.ModTime().Equal(pinned) {
		t.Errorf("file %s modification time moved: got %v, want %v", path, info.ModTime(), pinned)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("file %s changed:\nbefore %s\nafter  %s", path, want, got)
	}
}

// Reading work/alias/batches.json through a dangling directory link is an
// error, never a first registration — for relative and absolute link targets
// and for relative and absolute user paths. Nothing is created and the link
// is left exactly as made.
func TestLoadRejectsDanglingDirectorySymlink(t *testing.T) {
	cases := []struct {
		name       string
		linkTarget func(root string) string
		relative   bool // user path and working directory are relative
	}{
		{"relative link target, absolute user path", func(root string) string { return filepath.Join("..", "store", "missing") }, false},
		{"absolute link target, absolute user path", func(root string) string { return filepath.Join(root, "store", "missing") }, false},
		{"relative link target, relative user path", func(root string) string { return filepath.Join("..", "store", "missing") }, true},
		{"absolute link target, relative user path", func(root string) string { return filepath.Join(root, "store", "missing") }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, workDir, storeDir, aliasPath := danglingDirLinkLayout(t)
			target := tc.linkTarget(root)
			if err := os.Symlink(target, aliasPath); err != nil {
				t.Fatal(err)
			}

			var userPath, wantLink string
			if tc.relative {
				t.Chdir(workDir)
				userPath = filepath.Join("alias", "batches.json")
				wantLink = "alias"
			} else {
				userPath = filepath.Join(aliasPath, "batches.json")
				wantLink = aliasPath
			}

			reg, existed, err := Load(userPath)
			assertDanglingDirLinkRejected(t, userPath, wantLink, reg, existed, err)

			// Read-only failure: the link keeps its target text and no
			// missing directory, registry or temporary file appears.
			assertLinkIntact(t, aliasPath, target)
			if _, statErr := os.Lstat(filepath.Join(storeDir, "missing")); !os.IsNotExist(statErr) {
				t.Fatalf("the missing link target directory must not be created, stat err=%v", statErr)
			}
			if _, statErr := os.Lstat(filepath.Join(storeDir, "missing", "batches.json")); !os.IsNotExist(statErr) {
				t.Fatalf("no registry may be created under the missing target, stat err=%v", statErr)
			}
			assertNoLeftoverTempFiles(t, root)
		})
	}
}

// A ".." after the dangling directory link must not launder the failure into
// reading another file. work/alias/../batches.json is unresolvable while
// alias's target directory does not exist: Load errors rather than reading
// either the decoy beside the link (work/batches.json) or the same-named file
// in the target's parent directory (store/batches.json), and both decoys keep
// their bytes and modification times. Relative and absolute link targets and
// a relative user path all obey the same rule.
func TestLoadRejectsDanglingDirectorySymlinkDotDot(t *testing.T) {
	cases := []struct {
		name       string
		linkTarget func(root string) string
	}{
		{"relative target", func(root string) string { return filepath.Join("..", "store", "missing") }},
		{"absolute target", func(root string) string { return filepath.Join(root, "store", "missing") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, workDir, _, aliasPath := danglingDirLinkLayout(t)
			putDecoyRegistries(t, workDir, filepath.Join(root, "store"))
			target := tc.linkTarget(root)
			if err := os.Symlink(target, aliasPath); err != nil {
				t.Fatal(err)
			}

			linkDecoy := filepath.Join(workDir, "batches.json")
			storeDecoy := filepath.Join(root, "store", "batches.json")
			linkBytes, err := os.ReadFile(linkDecoy)
			if err != nil {
				t.Fatal(err)
			}
			storeBytes, err := os.ReadFile(storeDecoy)
			if err != nil {
				t.Fatal(err)
			}
			linkPinned := pinFile(t, linkDecoy)
			storePinned := pinFile(t, storeDecoy)

			// Concatenation on purpose: filepath.Join/Clean would erase the
			// alias/.. shape under test.
			userPath := aliasPath + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "batches.json"
			reg, existed, lerr := Load(userPath)
			assertDanglingDirLinkRejected(t, userPath, aliasPath, reg, existed, lerr)
			assertFileUnchanged(t, linkDecoy, linkBytes, linkPinned)
			assertFileUnchanged(t, storeDecoy, storeBytes, storePinned)
			assertLinkIntact(t, aliasPath, target)
			assertNoLeftoverTempFiles(t, root)

			// The relative spelling, run from the link's own directory, obeys
			// the same rule and must not read the decoy sitting beside it.
			t.Chdir(workDir)
			relPath := "alias" + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "batches.json"
			reg2, existed2, err2 := Load(relPath)
			assertDanglingDirLinkRejected(t, relPath, "alias", reg2, existed2, err2)
			assertFileUnchanged(t, linkDecoy, linkBytes, linkPinned)
		})
	}
}

// A chain of several links whose final target is missing is rejected too:
// work/c1 -> c2 -> gone, reading work/c1/batches.json errors with a nil
// registry instead of an empty first-registry result; the same chain followed
// by ".." fails identically.
func TestLoadRejectsDanglingDirectorySymlinkChain(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	link1 := filepath.Join(work, "c1")
	link2 := filepath.Join(work, "c2")
	if err := os.Symlink("c2", link1); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("gone", link2); err != nil {
		t.Fatal(err)
	}

	for _, userPath := range []string{
		filepath.Join(link1, "batches.json"),
		link1 + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "batches.json",
	} {
		reg, existed, err := Load(userPath)
		assertDanglingDirLinkRejected(t, userPath, link1, reg, existed, err)
	}
	assertLinkIntact(t, link1, "c2")
	assertLinkIntact(t, link2, "gone")
	if _, statErr := os.Lstat(filepath.Join(work, "gone")); !os.IsNotExist(statErr) {
		t.Fatalf("the missing target must not be created, stat err=%v", statErr)
	}
	assertNoLeftoverTempFiles(t, root)
}

// A loop of directory links has no determinable target at all: reading
// through it is rejected, whether or not a ".." follows, and the links are
// left exactly as made.
func TestLoadRejectsDirectorySymlinkLoop(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	linkA := filepath.Join(work, "a")
	linkB := filepath.Join(work, "b")
	if err := os.Symlink("b", linkA); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a", linkB); err != nil {
		t.Fatal(err)
	}
	for _, userPath := range []string{
		filepath.Join(linkA, "batches.json"),
		linkA + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "batches.json",
	} {
		reg, existed, err := Load(userPath)
		if err == nil {
			t.Fatalf("Load(%q) through a link loop must fail, got reg=%+v existed=%v", userPath, reg, existed)
		}
		if reg != nil {
			t.Errorf("Load(%q) must return a nil registry, got %+v", userPath, reg)
		}
		if !existed {
			t.Errorf("Load(%q) must not signal a first registration", userPath)
		}
		if !strings.Contains(err.Error(), userPath) {
			t.Errorf("error %q must quote the user path %q", err.Error(), userPath)
		}
	}
	assertLinkIntact(t, linkA, "b")
	assertLinkIntact(t, linkB, "a")
	assertNoLeftoverTempFiles(t, root)
}

// Normal first registration is preserved through a VALID directory link: the
// registry file merely missing beyond a real target directory yields an empty
// version-1 registry with existed == false and no error, and Load creates
// nothing.
func TestLoadMissingFileThroughValidDirectorySymlinkIsFresh(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "store")
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(filepath.Join(store, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(work, "alias")
	if err := os.Symlink(filepath.Join("..", "store", "child"), alias); err != nil {
		t.Fatal(err)
	}
	userPath := filepath.Join(alias, "batches.json")

	reg, existed, err := Load(userPath)
	if err != nil {
		t.Fatalf("a missing file beyond a valid directory link must be a fresh registry: %v", err)
	}
	if existed || reg == nil || reg.Version != FormatVersion || len(reg.Batches) != 0 {
		t.Fatalf("want empty version-1 registry, existed=false; got reg=%+v existed=%v", reg, existed)
	}
	// Load stays read-only: nothing was created in the target directory.
	if entries, err := os.ReadDir(filepath.Join(store, "child")); err != nil {
		t.Fatal(err)
	} else if len(entries) != 0 {
		t.Fatalf("Load must not create the registry, found %v", entries)
	}
}

// A valid directory link mixed with ".." resolves by the real directory
// relations: work/alias -> store/child makes work/alias/../batches.json name
// store/batches.json, and Load reads its records in order. The fix must not
// reject paths merely for carrying a link or "..".
func TestLoadReadsThroughValidDirectorySymlinkDotDot(t *testing.T) {
	root, userPath, realPath, _, aliasPath, _ := dirLinkFixture(t, filepath.Join("..", "store", "child"))

	reg, existed, err := Load(userPath)
	if err != nil || !existed {
		t.Fatalf("a valid directory link with \"..\" must read the real registry: existed=%v err=%v", existed, err)
	}
	assertBatchesEqual(t, reg.Batches, []Batch{saveBatch("OLD-1", "P-1", 10, "kg")})
	assertBatchesEqual(t, mustLoad(t, realPath).Batches, reg.Batches)
	assertLinkIntact(t, aliasPath, filepath.Join("..", "store", "child"))
	assertNoLeftoverTempFiles(t, root)
}

// Plain missing paths keep the established first-registration answer: a
// missing registry file in an existing directory, and a path whose ordinary
// parent directories do not exist yet, both give an empty version-1 registry
// with existed == false and create nothing.
func TestLoadOrdinaryMissingPathsStayFresh(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "dir")
	if err := os.MkdirAll(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, userPath := range []string{
		filepath.Join(root, "batches.json"),
		filepath.Join(existing, "batches.json"),
		filepath.Join(root, "not", "yet", "created", "batches.json"),
	} {
		reg, existed, err := Load(userPath)
		if err != nil {
			t.Fatalf("Load(%q): %v", userPath, err)
		}
		if existed || reg == nil || reg.Version != FormatVersion || len(reg.Batches) != 0 {
			t.Fatalf("Load(%q): want empty version-1 registry existed=false, got reg=%+v existed=%v", userPath, reg, existed)
		}
	}
	if _, err := os.Lstat(filepath.Join(root, "not")); !os.IsNotExist(err) {
		t.Fatalf("Load must not create missing parent directories, stat err=%v", err)
	}
}
