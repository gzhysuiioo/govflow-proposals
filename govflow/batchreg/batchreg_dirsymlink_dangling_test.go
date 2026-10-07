package batchreg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This file is the regression net for a dangling symbolic link on a DIRECTORY
// component of the registry path. With work/alias pointing at a directory that
// does not exist, reading work/alias/batches.json reaches no file the way the
// kernel resolves names — the missing target is not a missing registry file,
// and a ".." right after the link (work/alias/../batches.json) cannot step
// out of a directory that was never reached, so even an existing
// work/batches.json must not be read in its place. The rule under guard is
// that Load fails such a path at the read stage: a non-nil error quoting the
// path the caller passed verbatim and a nil registry, never the
// err == nil / existed == false signal of a first registration. The read
// creates nothing and changes nothing.
//
// Ordinary first registration stays intact: a plain path missing just the
// registry file, or missing not-yet-created ordinary parent directories,
// still yields an empty version-1 registry with existed == false; a valid
// directory link into a real directory behaves the same when the registry
// file is merely absent, and mixed with ".." it finds the real file.

// danglingDirLinkFixture builds, under root,
//
//	root/
//	  work/
//	    alias -> linkTarget   (a directory link whose target is missing)
//
// and returns the work directory, the link path and a registry path reached
// through the link (assembled by concatenation so later tests can keep a raw
// ".." segment). relTarget says whether the link target text is relative
// (resolved against work, not the process working directory).
func danglingDirLinkFixture(t *testing.T, root, linkTarget string) (workDir, aliasPath string) {
	t.Helper()
	workDir = filepath.Join(root, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	aliasPath = filepath.Join(workDir, "alias")
	if err := os.Symlink(linkTarget, aliasPath); err != nil {
		t.Fatal(err)
	}
	return workDir, aliasPath
}

// rawPath joins parts with the OS separator WITHOUT lexical cleaning, so a
// ".." deliberately placed behind a directory link survives to Load.
func rawPath(parts ...string) string {
	return strings.Join(parts, string(os.PathSeparator))
}

// assertDanglingDirLinkLoadFailure checks the Load contract for an
// unresolvable directory link: a non-nil error, a nil registry (never an
// empty fresh one), and an error that quotes the exact path the caller
// passed and explains that a directory symbolic link target cannot be
// resolved rather than merely announcing a not-yet-created registry file.
func assertDanglingDirLinkLoadFailure(t *testing.T, userPath string) {
	t.Helper()
	reg, existed, err := Load(userPath)
	if err == nil {
		t.Fatalf("Load(%q) must reject a path through a dangling directory link, got reg=%+v existed=%v", userPath, reg, existed)
	}
	if reg != nil {
		t.Fatalf("Load(%q) must return a nil registry on failure, got %+v", userPath, reg)
	}
	if !existed {
		t.Errorf("Load(%q) must not signal a first registration through an unresolvable link: existed=%v", userPath, existed)
	}
	msg := err.Error()
	if !strings.Contains(msg, userPath) {
		t.Errorf("error %q must quote the registry path the caller passed verbatim: %q", msg, userPath)
	}
	if !strings.Contains(msg, "symbolic link") {
		t.Errorf("error %q must name the unresolvable symbolic link", msg)
	}
	if !strings.Contains(msg, "target") || !strings.Contains(msg, "resolve") {
		t.Errorf("error %q must explain that the directory link target cannot be resolved", msg)
	}
	if strings.Contains(msg, "has not been created") {
		t.Errorf("error %q must not pretend the registry file has merely not been created", msg)
	}
}

// assertLinkAndFilesUnchanged fails if Load left any trace: the link target
// text changed, the named files differ from their pinned bytes/mtime, or a
// stray file (a created target, a temporary file) appeared under root.
func assertLinkAndFilesUnchanged(t *testing.T, root, aliasPath, linkTarget string, pinned map[string]pinnedFile) {
	t.Helper()
	assertLinkIntact(t, aliasPath, linkTarget)
	for path, want := range pinned {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want.content {
			t.Fatalf("Load changed %s:\nbefore %s\nafter  %s", path, want.content, got)
		}
		if info, err := os.Stat(path); err != nil {
			t.Fatal(err)
		} else if !info.ModTime().Equal(want.pinned) {
			t.Fatalf("Load moved the mtime of %s: got %v want %v", path, info.ModTime(), want.pinned)
		}
	}
	assertNoLeftoverTempFiles(t, root)
}

// pinnedFile records a file's exact bytes and pinned modification time.
type pinnedFile struct {
	content string
	pinned  time.Time
}

// Reading work/alias/batches.json through a dangling directory link fails for
// both relative and absolute link targets. A relative target is judged from
// the link's own directory; the fixture deliberately makes the same relative
// text unresolvable from the process working directory as well, so a
// working-directory-relative resolution cannot pass by accident.
func TestLoadRejectsDanglingDirectorySymlink(t *testing.T) {
	cases := []struct {
		name       string
		linkTarget func(root string) string
	}{
		{"relative target", func(root string) string {
			return filepath.Join("..", "store", "missing-child")
		}},
		{"absolute target", func(root string) string {
			return filepath.Join(root, "store", "missing-child")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			target := tc.linkTarget(root)
			workDir, aliasPath := danglingDirLinkFixture(t, root, target)

			userPath := filepath.Join(aliasPath, "batches.json")
			assertDanglingDirLinkLoadFailure(t, userPath)

			// Read-only: the missing target directory and registry file are
			// not brought into existence, the link is untouched, and no
			// temporary file is prepared anywhere.
			assertLinkIntact(t, aliasPath, target)
			for _, p := range []string{
				filepath.Join(root, "store"),
				filepath.Join(root, "store", "missing-child"),
				filepath.Join(aliasPath, "batches.json"),
			} {
				if _, err := os.Lstat(p); !os.IsNotExist(err) {
					t.Fatalf("Load must not create %s, stat err=%v", p, err)
				}
			}
			if entries, err := os.ReadDir(workDir); err != nil {
				t.Fatal(err)
			} else if len(entries) != 1 || entries[0].Name() != "alias" {
				t.Fatalf("Load created entries next to the link: %v", entries)
			}
			assertNoLeftoverTempFiles(t, root)
		})
	}
}

// A ".." behind the dangling link cannot launder the failure into reading the
// same-named file beside the link: work/alias/../batches.json stays
// unresolvable even though work/batches.json is a valid registry. The decoy
// keeps its bytes and modification time and is never returned.
func TestLoadRejectsDanglingDirectorySymlinkWithDotDot(t *testing.T) {
	cases := []struct {
		name       string
		linkTarget func(root string) string
	}{
		{"relative target", func(root string) string {
			return filepath.Join("..", "store", "missing-child")
		}},
		{"absolute target", func(root string) string {
			return filepath.Join(root, "store", "missing-child")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			target := tc.linkTarget(root)
			workDir, aliasPath := danglingDirLinkFixture(t, root, target)
			decoyPath := filepath.Join(workDir, "batches.json")
			decoy := `{"version":1,"batches":[{"batch":"DECOY","product":"P-0","quantity":1,"unit":"m"}]}`
			writeRegistry(t, decoyPath, decoy)
			decoyPinned := pinFile(t, decoyPath)

			userPath := rawPath(aliasPath, "..", "batches.json")
			assertDanglingDirLinkLoadFailure(t, userPath)

			assertLinkAndFilesUnchanged(t, root, aliasPath, target, map[string]pinnedFile{
				decoyPath: {content: decoy, pinned: decoyPinned},
			})
			if _, err := os.Lstat(filepath.Join(root, "store")); !os.IsNotExist(err) {
				t.Fatalf("the missing link target must not be created, stat err=%v", err)
			}
		})
	}
}

// Relative and absolute spellings of the user path follow the same rule, and
// the error always carries the spelling the caller actually used — never a
// cleaned or absolutized substitute.
func TestLoadDanglingDirectorySymlinkRelativeAndAbsolutePath(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join("..", "store", "missing-child")
	_, aliasPath := danglingDirLinkFixture(t, root, target)

	t.Run("relative user path", func(t *testing.T) {
		t.Chdir(root)
		userPath := rawPath("work", "alias", "batches.json")
		assertDanglingDirLinkLoadFailure(t, userPath)
		assertLinkIntact(t, aliasPath, target)
	})
	t.Run("relative user path with dotdot", func(t *testing.T) {
		t.Chdir(root)
		userPath := rawPath("work", "alias", "..", "batches.json")
		assertDanglingDirLinkLoadFailure(t, userPath)
		assertLinkIntact(t, aliasPath, target)
	})
	t.Run("absolute user path", func(t *testing.T) {
		userPath := filepath.Join(aliasPath, "batches.json")
		assertDanglingDirLinkLoadFailure(t, userPath)
		assertLinkIntact(t, aliasPath, target)
	})
}

// A relative link target is resolved against the directory holding the link.
// The fixture places a same-named reachable directory relative to the WORKING
// directory; resolving the link there instead would wrongly make it valid.
func TestLoadDanglingDirectorySymlinkRelativeTargetUsesLinkDirectory(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	aliasPath := filepath.Join(workDir, "alias")
	// From work's point of view "store/missing-child" must not exist; the
	// target is deliberately relative without a leading "..".
	if err := os.Symlink(filepath.Join("store", "missing-child"), aliasPath); err != nil {
		t.Fatal(err)
	}
	// Decoy that a working-directory-relative resolution would chase:
	// <cwd>/store/missing-child, created in a second temp dir used as cwd.
	cwd := t.TempDir()
	decoyDir := filepath.Join(cwd, "store", "missing-child")
	if err := os.MkdirAll(decoyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)

	userPath := filepath.Join(aliasPath, "batches.json")
	assertDanglingDirLinkLoadFailure(t, userPath)
	assertLinkIntact(t, aliasPath, filepath.Join("store", "missing-child"))
	if _, err := os.Lstat(filepath.Join(workDir, "store")); !os.IsNotExist(err) {
		t.Fatalf("resolution must be relative to the link directory, not the cwd; %s must not be created", filepath.Join(workDir, "store"))
	}
}

// A chain of several links whose final target is missing fails just like a
// single dangling link, at whichever hop the target vanishes.
func TestLoadRejectsChainOfLinksToMissingDirectory(t *testing.T) {
	root := t.TempDir()
	workDir, aliasPath := danglingDirLinkFixture(t, root, filepath.Join("..", "store", "missing-child"))
	// work/mid -> alias -> ../store/missing-child (missing).
	midPath := filepath.Join(workDir, "mid")
	if err := os.Symlink("alias", midPath); err != nil {
		t.Fatal(err)
	}

	assertDanglingDirLinkLoadFailure(t, filepath.Join(midPath, "batches.json"))
	assertDanglingDirLinkLoadFailure(t, rawPath(midPath, "..", "batches.json"))
	assertLinkIntact(t, midPath, "alias")
	assertLinkIntact(t, aliasPath, filepath.Join("..", "store", "missing-child"))
	assertNoLeftoverTempFiles(t, root)
}

// A loop of directory links likewise has no resolvable target.
func TestLoadRejectsDirectorySymlinkLoop(t *testing.T) {
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
	assertDanglingDirLinkLoadFailure(t, filepath.Join(linkA, "batches.json"))
	assertLinkIntact(t, linkA, "b")
	assertLinkIntact(t, linkB, "a")
}

// Normal first registration is preserved on an ordinary path: the registry
// file alone missing in an existing directory reads as an empty version-1
// registry with existed == false and no error, and the file is not created by
// the read.
func TestLoadMissingRegistryFileIsStillFirstRegistration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "batches.json")

	reg, existed, err := Load(path)
	if err != nil {
		t.Fatalf("a merely missing registry file must not be an error: %v", err)
	}
	if existed || reg == nil || reg.Version != FormatVersion || len(reg.Batches) != 0 {
		t.Fatalf("want empty version-1 registry with existed=false, got reg=%+v existed=%v", reg, existed)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("Load must not create the missing registry file, stat err=%v", err)
	}
}

// Missing ordinary parent directories that are never climbed out of are still
// a first registration, and the read creates none of them.
func TestLoadMissingParentDirectoriesIsStillFirstRegistration(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "new", "nested", "batches.json")

	reg, existed, err := Load(path)
	if err != nil {
		t.Fatalf("missing ordinary parent directories must not be an error: %v", err)
	}
	if existed || reg == nil || reg.Version != FormatVersion || len(reg.Batches) != 0 {
		t.Fatalf("want empty version-1 registry with existed=false, got reg=%+v existed=%v", reg, existed)
	}
	if _, err := os.Lstat(filepath.Join(root, "new")); !os.IsNotExist(err) {
		t.Fatalf("Load must not create missing parent directories, stat err=%v", err)
	}
}

// A valid directory link into a real directory where the registry file simply
// does not exist yet is an ordinary first registration: empty version-1
// registry, existed == false, nothing created.
func TestLoadThroughValidDirectorySymlinkMissingFileIsFirstRegistration(t *testing.T) {
	root := t.TempDir()
	storeChild := filepath.Join(root, "store", "child")
	if err := os.MkdirAll(storeChild, 0o755); err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Join(root, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	aliasPath := filepath.Join(workDir, "alias")
	target := filepath.Join("..", "store", "child")
	if err := os.Symlink(target, aliasPath); err != nil {
		t.Fatal(err)
	}

	userPath := filepath.Join(aliasPath, "batches.json")
	reg, existed, err := Load(userPath)
	if err != nil {
		t.Fatalf("a valid directory link with the registry file absent must not fail: %v", err)
	}
	if existed || reg == nil || reg.Version != FormatVersion || len(reg.Batches) != 0 {
		t.Fatalf("want empty version-1 registry with existed=false, got reg=%+v existed=%v", reg, existed)
	}
	if _, err := os.Lstat(filepath.Join(storeChild, "batches.json")); !os.IsNotExist(err) {
		t.Fatalf("Load must not create the registry through the link, stat err=%v", err)
	}
	assertLinkIntact(t, aliasPath, target)

	// The same with ".." after the valid link: work/alias/../fresh.json names
	// store/fresh.json, which is absent, and is still a first registration.
	freshPath := rawPath(aliasPath, "..", "fresh.json")
	reg2, existed2, err := Load(freshPath)
	if err != nil {
		t.Fatalf("a \"..\" after a valid directory link with the file absent must not fail: %v", err)
	}
	if existed2 || reg2 == nil || reg2.Version != FormatVersion || len(reg2.Batches) != 0 {
		t.Fatalf("want empty version-1 registry with existed=false, got reg=%+v existed=%v", reg2, existed2)
	}
	if _, err := os.Lstat(filepath.Join(root, "store", "fresh.json")); !os.IsNotExist(err) {
		t.Fatalf("Load must not create store/fresh.json, stat err=%v", err)
	}
}

// A valid directory link mixed with ".." resolves by real directory
// relations: work/alias/../batches.json reads store/batches.json, records and
// order intact — links and ".." must not be rejected wholesale.
func TestLoadThroughValidDirectorySymlinkDotDotReadsRealFile(t *testing.T) {
	root := t.TempDir()
	userPath, realPath, decoyPath, aliasPath, _ := dirLinkFixtureAt(t, root, filepath.Join("..", "store", "child"))
	pinned := pinFile(t, realPath)
	decoyPinned := pinFile(t, decoyPath)

	reg, existed, err := Load(userPath)
	if err != nil || !existed {
		t.Fatalf("reading through a valid directory link must load the real registry: existed=%v err=%v", existed, err)
	}
	assertBatchesEqual(t, reg.Batches, []Batch{saveBatch("OLD-1", "P-1", 10, "kg")})

	// Read-only: neither the real registry nor the decoy is touched.
	if info, err := os.Stat(realPath); err != nil {
		t.Fatal(err)
	} else if !info.ModTime().Equal(pinned) {
		t.Errorf("the real registry mtime moved: got %v want %v", info.ModTime(), pinned)
	}
	if info, err := os.Stat(decoyPath); err != nil {
		t.Fatal(err)
	} else if !info.ModTime().Equal(decoyPinned) {
		t.Errorf("the decoy mtime moved: got %v want %v", info.ModTime(), decoyPinned)
	}
	assertLinkIntact(t, aliasPath, filepath.Join("..", "store", "child"))
	assertNoLeftoverTempFiles(t, root)
}
