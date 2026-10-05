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

// This file is the regression net for using a registry through a symbolic
// link. The rule under guard: a save addressed at a link must land in the
// file the link points at — the link stays a link to the same target, the
// target keeps its permissions, and reads through either path see the same
// records — while a link whose target cannot be resolved (dangling or
// looping) rejects the operation instead of being treated as a first
// registration.

// symlinkFixtureRegistry is the registry content every linked-target fixture
// starts from.
const symlinkFixtureRegistry = `{"version":1,"batches":[{"batch":"OLD-1","product":"P-1","quantity":10,"unit":"kg"}]}`

// symlinkFixtureMode is a distinctive permission set for the target file, so
// a save that recreates the file with default permissions is observable.
const symlinkFixtureMode = os.FileMode(0o640)

// setupLinkedRegistry writes the fixture registry to targetPath with
// symlinkFixtureMode permissions and points a symbolic link at it. linkTarget
// is the exact text stored in the link, absolute or relative to the link's
// directory. It returns the link path and the original target bytes.
func setupLinkedRegistry(t *testing.T, targetPath, linkPath, linkTarget string) (originalBytes []byte) {
	t.Helper()
	writeRegistry(t, targetPath, symlinkFixtureRegistry)
	if err := os.Chmod(targetPath, symlinkFixtureMode); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(linkTarget, linkPath); err != nil {
		t.Fatal(err)
	}
	originalBytes, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	return originalBytes
}

// assertLinkIntact fails unless path is still a symbolic link storing exactly
// the link target text wantTarget.
func assertLinkIntact(t *testing.T, path, wantTarget string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("the link must still exist: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s must still be a symbolic link, got mode %v", path, info.Mode())
	}
	target, err := os.Readlink(path)
	if err != nil {
		t.Fatal(err)
	}
	if target != wantTarget {
		t.Fatalf("link target = %q, want unchanged %q", target, wantTarget)
	}
}

// TestSaveThroughSymlinkWritesTarget covers the accepted link shapes —
// absolute target, relative target, target in a different directory — and
// checks the whole contract: the new content lands in the target file, the
// link survives as a link to the same target, the target keeps its
// permissions, and both paths read back the same records in order.
func TestSaveThroughSymlinkWritesTarget(t *testing.T) {
	cases := []struct {
		name       string
		linkPath   func(root string) string
		linkTarget func(root, targetPath string) string
	}{
		{
			"absolute target, link beside target",
			func(root string) string { return filepath.Join(root, "link.json") },
			func(root, targetPath string) string { return targetPath },
		},
		{
			"relative target, link beside target",
			func(root string) string { return filepath.Join(root, "link.json") },
			func(root, targetPath string) string { return "real-registry.json" },
		},
		{
			"relative target, link in another directory",
			func(root string) string { return filepath.Join(root, "links", "deep", "link.json") },
			func(root, targetPath string) string {
				return filepath.Join("..", "..", "real-registry.json")
			},
		},
		{
			"absolute target, link in another directory",
			func(root string) string { return filepath.Join(root, "elsewhere", "link.json") },
			func(root, targetPath string) string { return targetPath },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			targetPath := filepath.Join(root, "real-registry.json")
			linkPath := tc.linkPath(root)
			linkTarget := tc.linkTarget(root, targetPath)
			setupLinkedRegistry(t, targetPath, linkPath, linkTarget)

			reg, existed, err := Load(linkPath)
			if err != nil || !existed {
				t.Fatalf("reading through the link must load the target: existed=%v err=%v", existed, err)
			}
			if len(reg.Batches) != 1 || reg.Batches[0].Batch != "OLD-1" {
				t.Fatalf("reading through the link must see the target records: %+v", reg.Batches)
			}
			if _, err := Register(reg, Input{Batch: "NEW-1", Product: "P-9", Quantity: 5, Unit: "box"}); err != nil {
				t.Fatal(err)
			}
			if err := Save(linkPath, reg); err != nil {
				t.Fatalf("saving through the link must succeed: %v", err)
			}

			assertLinkIntact(t, linkPath, linkTarget)
			info, err := os.Stat(targetPath)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != symlinkFixtureMode {
				t.Fatalf("target permissions = %v, want preserved %v", info.Mode().Perm(), symlinkFixtureMode)
			}

			want := []Batch{
				saveBatch("OLD-1", "P-1", 10, "kg"),
				saveBatch("NEW-1", "P-9", 5, "box"),
			}
			viaTarget, existed, err := Load(targetPath)
			if err != nil || !existed {
				t.Fatalf("target must load: existed=%v err=%v", existed, err)
			}
			assertBatchesEqual(t, viaTarget.Batches, want)
			viaLink, existed, err := Load(linkPath)
			if err != nil || !existed {
				t.Fatalf("link must load: existed=%v err=%v", existed, err)
			}
			assertBatchesEqual(t, viaLink.Batches, want)
			assertNoLeftoverTempFiles(t, root)
		})
	}
}

// A relative link target is resolved against the directory holding the link,
// never against the process working directory: saving through the link from
// an unrelated working directory must still update the one registry the link
// points at.
func TestSaveThroughRelativeSymlinkIgnoresWorkingDirectory(t *testing.T) {
	root := t.TempDir()
	targetPath := filepath.Join(root, "regdir", "registry.json")
	linkPath := filepath.Join(root, "links", "registry-link.json")
	setupLinkedRegistry(t, targetPath, linkPath, filepath.Join("..", "regdir", "registry.json"))

	// A decoy registry with the same relative path under the working
	// directory: a save resolving the link against the working directory
	// would land here instead of in the real target.
	workDir := t.TempDir()
	decoyPath := filepath.Join(workDir, "regdir", "registry.json")
	writeRegistry(t, decoyPath, `{"version":1,"batches":[{"batch":"DECOY","product":"P","quantity":1,"unit":"kg"}]}`)
	decoyBytes, err := os.ReadFile(decoyPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(workDir)

	reg, existed, err := Load(linkPath)
	if err != nil || !existed {
		t.Fatalf("reading through the link must load the target: existed=%v err=%v", existed, err)
	}
	if _, err := Register(reg, Input{Batch: "NEW-1", Product: "P-9", Quantity: 5, Unit: "box"}); err != nil {
		t.Fatal(err)
	}
	if err := Save(linkPath, reg); err != nil {
		t.Fatalf("saving through the link must succeed: %v", err)
	}

	loaded, existed, err := Load(targetPath)
	if err != nil || !existed {
		t.Fatalf("target must load: existed=%v err=%v", existed, err)
	}
	assertBatchesEqual(t, loaded.Batches, []Batch{
		saveBatch("OLD-1", "P-1", 10, "kg"),
		saveBatch("NEW-1", "P-9", 5, "box"),
	})
	after, err := os.ReadFile(decoyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, decoyBytes) {
		t.Fatalf("the decoy registry under the working directory must stay untouched:\nbefore %s\nafter  %s", decoyBytes, after)
	}
	assertLinkIntact(t, linkPath, filepath.Join("..", "regdir", "registry.json"))
}

// A link whose target does not exist is not a first registration: Load
// rejects it with an error naming the link path the user passed and the
// reason, and neither the target nor any stand-in file is created.
func TestLoadRejectsDanglingSymlink(t *testing.T) {
	dir := t.TempDir()
	linkPath := filepath.Join(dir, "link.json")
	missingTarget := filepath.Join(dir, "missing.json")
	if err := os.Symlink(missingTarget, linkPath); err != nil {
		t.Fatal(err)
	}

	reg, existed, err := Load(linkPath)
	if err == nil {
		t.Fatalf("Load must reject a dangling link, got reg=%+v existed=%v", reg, existed)
	}
	if !existed {
		t.Errorf("a dangling link must not signal a fresh registry: existed=%v", existed)
	}
	msg := err.Error()
	if !strings.Contains(msg, linkPath) {
		t.Errorf("error %q must name the registry path the user passed", msg)
	}
	if !strings.Contains(msg, "symbolic link") || !strings.Contains(msg, "does not exist") {
		t.Errorf("error %q must state the link target does not exist", msg)
	}
	assertLinkIntact(t, linkPath, missingTarget)
	if _, statErr := os.Stat(missingTarget); !os.IsNotExist(statErr) {
		t.Errorf("the missing target must not be created, stat err=%v", statErr)
	}
}

// Saving to a dangling link is rejected the same way: the error names the
// user's registry path, the link is left exactly as it was, the target is
// not created and no temporary file survives.
func TestSaveRejectsDanglingSymlink(t *testing.T) {
	dir := t.TempDir()
	linkPath := filepath.Join(dir, "link.json")
	missingTarget := filepath.Join(dir, "missing.json")
	if err := os.Symlink(missingTarget, linkPath); err != nil {
		t.Fatal(err)
	}

	reg := &Registry{Version: FormatVersion, Batches: []Batch{saveBatch("B1", "P1", 1, "kg")}}
	err := Save(linkPath, reg)
	if err == nil {
		t.Fatal("Save must reject a dangling link")
	}
	msg := err.Error()
	if !strings.Contains(msg, linkPath) {
		t.Errorf("error %q must name the registry path the user passed", msg)
	}
	if !strings.Contains(msg, "symbolic link") {
		t.Errorf("error %q must state the path is an unresolvable symbolic link", msg)
	}
	assertLinkIntact(t, linkPath, missingTarget)
	if _, statErr := os.Stat(missingTarget); !os.IsNotExist(statErr) {
		t.Errorf("the missing target must not be created, stat err=%v", statErr)
	}
	assertNoLeftoverTempFiles(t, dir)
}

// A chain of links forming a loop has no determinable target: both Load and
// Save reject it with an error naming the user's registry path, and the
// links are left untouched.
func TestLoadAndSaveRejectSymlinkLoop(t *testing.T) {
	dir := t.TempDir()
	linkA := filepath.Join(dir, "a.json")
	linkB := filepath.Join(dir, "b.json")
	if err := os.Symlink(linkB, linkA); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(linkA, linkB); err != nil {
		t.Fatal(err)
	}

	_, _, loadErr := Load(linkA)
	if loadErr == nil {
		t.Fatal("Load must reject a link loop")
	}
	if !strings.Contains(loadErr.Error(), linkA) {
		t.Errorf("load error %q must name the registry path the user passed", loadErr.Error())
	}

	reg := &Registry{Version: FormatVersion, Batches: []Batch{saveBatch("B1", "P1", 1, "kg")}}
	saveErr := Save(linkA, reg)
	if saveErr == nil {
		t.Fatal("Save must reject a link loop")
	}
	if !strings.Contains(saveErr.Error(), linkA) {
		t.Errorf("save error %q must name the registry path the user passed", saveErr.Error())
	}
	assertLinkIntact(t, linkA, linkB)
	assertLinkIntact(t, linkB, linkA)
	assertNoLeftoverTempFiles(t, dir)
}

// A save failing midway through a link keeps the linked registry exactly as
// it was: the target's bytes and modification time are untouched and the
// link survives, just as for a direct path.
func TestSaveFailureThroughSymlinkKeepsTargetIntact(t *testing.T) {
	root := t.TempDir()
	targetPath := filepath.Join(root, "real-registry.json")
	linkPath := filepath.Join(root, "link.json")
	originalBytes := setupLinkedRegistry(t, targetPath, linkPath, targetPath)
	pinned := time.Date(2010, time.March, 4, 5, 6, 7, 0, time.UTC)
	if err := os.Chtimes(targetPath, pinned, pinned); err != nil {
		t.Fatal(err)
	}

	cause := errors.New("injected write failure through link")
	old := WriteTempContent
	WriteTempContent = func(f *os.File, data []byte) (int, error) {
		half := len(data) / 2
		if _, err := f.Write(data[:half]); err != nil {
			return 0, err
		}
		return half, cause
	}
	t.Cleanup(func() { WriteTempContent = old })

	reg, existed, err := Load(linkPath)
	if err != nil || !existed {
		t.Fatalf("fixture must load through the link: existed=%v err=%v", existed, err)
	}
	if _, err := Register(reg, Input{Batch: "NEW-1", Product: "P-9", Quantity: 5, Unit: "box"}); err != nil {
		t.Fatal(err)
	}
	err = Save(linkPath, reg)
	if err == nil {
		t.Fatal("Save must report the injected failure")
	}
	if !errors.Is(err, cause) {
		t.Errorf("error %v must carry the failure cause %v", err, cause)
	}
	if !strings.Contains(err.Error(), linkPath) {
		t.Errorf("error %q must name the registry path the user passed %q", err.Error(), linkPath)
	}

	info, err := os.Stat(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(pinned) {
		t.Errorf("failed save touched the target's modification time: got %v, want %v", info.ModTime(), pinned)
	}
	after, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, originalBytes) {
		t.Fatalf("failed save changed the target bytes:\nbefore %s\nafter  %s", originalBytes, after)
	}
	assertLinkIntact(t, linkPath, targetPath)
	assertNoLeftoverTempFiles(t, root)
}
