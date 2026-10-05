package batchreg

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This file guards the behavior of Load and Save when the registry path the
// caller passes is a symbolic link. The rules under test:
//
//   - a save through a link lands on the link's target, however the link
//     spells it (absolute, or relative to the link's own directory — never
//     the process working directory) and wherever the target lives; the link
//     stays a link to the same target and the target keeps its permissions;
//   - a duplicate-only confirmation writes nothing: target bytes and mtime
//     and the link all stay exactly as they were;
//   - a link whose target does not exist, or a link loop, rejects the
//     operation with an error naming the caller's path — it is never treated
//     as a first registration, the target is never created and the link is
//     never replaced by a regular file.

// linkRegistry creates path as a symbolic link spelled linkTarget and returns
// both paths.
func linkRegistry(t *testing.T, path, linkTarget string) {
	t.Helper()
	if err := os.Symlink(linkTarget, path); err != nil {
		t.Fatal(err)
	}
}

// assertLinkIntact verifies that path is still a symbolic link carrying
// exactly the target spelling it was created with.
func assertLinkIntact(t *testing.T, path, wantTarget string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("the link must still exist: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s must still be a symbolic link, got mode %v", path, info.Mode())
	}
	got, err := os.Readlink(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != wantTarget {
		t.Fatalf("link target = %q, want the original spelling %q", got, wantTarget)
	}
}

// A save through a link with an absolute target replaces the target's
// content, keeps the link a link with its original target spelling, and
// preserves the target's pre-existing permissions.
func TestSaveThroughAbsoluteSymlinkWritesTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real", "registry.json")
	writeRegistry(t, target, `{"version":1,"batches":[{"batch":"OLD","product":"P","quantity":5,"unit":"kg"}]}`)
	if err := os.Chmod(target, 0o640); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "links", "registry.json")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	linkRegistry(t, link, target)

	reg, existed, err := Load(link)
	if err != nil || !existed {
		t.Fatalf("load through the link must see the target: existed=%v err=%v", existed, err)
	}
	if _, err := Register(reg, Input{Batch: "NEW", Product: "新产品", Quantity: 7, Unit: "箱"}); err != nil {
		t.Fatal(err)
	}
	if err := Save(link, reg); err != nil {
		t.Fatalf("save through the link must succeed: %v", err)
	}

	assertLinkIntact(t, link, target)
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("target permissions = %o, want the original 640", info.Mode().Perm())
	}
	// The same table is visible from the link path and the real path.
	want := []Batch{
		saveBatch("OLD", "P", 5, "kg"),
		saveBatch("NEW", "新产品", 7, "箱"),
	}
	for _, path := range []string{link, target} {
		got, existed, err := Load(path)
		if err != nil || !existed {
			t.Fatalf("read back via %s: existed=%v err=%v", path, existed, err)
		}
		assertBatchesEqual(t, got.Batches, want)
	}
	assertNoLeftoverTempFiles(t, dir)
}

// A link spelled with a relative target is resolved against the directory
// holding the link, so the save reaches the same registry no matter what the
// process working directory is; link and target may live in different
// directories.
func TestSaveThroughRelativeSymlinkResolvesAgainstLinkDirectory(t *testing.T) {
	root := t.TempDir()
	linkDir := filepath.Join(root, "elsewhere", "links")
	target := filepath.Join(linkDir, "data", "store", "registry.json")
	writeRegistry(t, target, `{"version":1,"batches":[{"batch":"OLD","product":"P","quantity":5,"unit":"kg"}]}`)
	link := filepath.Join(linkDir, "registry.json")
	relTarget := filepath.Join("data", "store", "registry.json")
	linkRegistry(t, link, relTarget)

	// A decoy at the spot a working-directory-relative resolution would pick:
	// it must never be read or written.
	decoyDir := t.TempDir()
	decoy := filepath.Join(decoyDir, relTarget)
	writeRegistry(t, decoy, `{"version":1,"batches":[{"batch":"DECOY","product":"X","quantity":1,"unit":"u"}]}`)
	t.Chdir(decoyDir)

	reg, existed, err := Load(link)
	if err != nil || !existed {
		t.Fatalf("load through the relative link must see the target: existed=%v err=%v", existed, err)
	}
	if len(reg.Batches) != 1 || reg.Batches[0].Batch != "OLD" {
		t.Fatalf("load through the relative link read the wrong registry: %+v", reg.Batches)
	}
	if _, err := Register(reg, Input{Batch: "NEW", Product: "P2", Quantity: 9, Unit: "box"}); err != nil {
		t.Fatal(err)
	}
	if err := Save(link, reg); err != nil {
		t.Fatalf("save through the relative link must succeed: %v", err)
	}

	assertLinkIntact(t, link, relTarget)
	got, existed, err := Load(target)
	if err != nil || !existed {
		t.Fatalf("read back via the real path: existed=%v err=%v", existed, err)
	}
	assertBatchesEqual(t, got.Batches, []Batch{
		saveBatch("OLD", "P", 5, "kg"),
		saveBatch("NEW", "P2", 9, "box"),
	})
	// The decoy keeps its original content: the save never went near it.
	decoyReg, _, err := Load(decoy)
	if err != nil {
		t.Fatal(err)
	}
	assertBatchesEqual(t, decoyReg.Batches, []Batch{saveBatch("DECOY", "X", 1, "u")})
	assertNoLeftoverTempFiles(t, root)
}

// A chain of links (link -> link -> registry) resolves to the final target;
// every link in the chain survives the save unchanged.
func TestSaveThroughSymlinkChainWritesFinalTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "registry.json")
	writeRegistry(t, target, `{"version":1,"batches":[{"batch":"OLD","product":"P","quantity":5,"unit":"kg"}]}`)
	inner := filepath.Join(dir, "inner-link")
	linkRegistry(t, inner, "registry.json")
	outer := filepath.Join(dir, "outer-link")
	linkRegistry(t, outer, filepath.Base(inner))

	reg, existed, err := Load(outer)
	if err != nil || !existed {
		t.Fatalf("load through the chain must see the target: existed=%v err=%v", existed, err)
	}
	if _, err := Register(reg, Input{Batch: "NEW", Product: "P2", Quantity: 1, Unit: "m"}); err != nil {
		t.Fatal(err)
	}
	if err := Save(outer, reg); err != nil {
		t.Fatalf("save through the chain must succeed: %v", err)
	}

	assertLinkIntact(t, outer, filepath.Base(inner))
	assertLinkIntact(t, inner, "registry.json")
	got, _, err := Load(target)
	if err != nil {
		t.Fatal(err)
	}
	assertBatchesEqual(t, got.Batches, []Batch{
		saveBatch("OLD", "P", 5, "kg"),
		saveBatch("NEW", "P2", 1, "m"),
	})
}

// A confirmation of an already-registered batch writes nothing at all: the
// target keeps its exact bytes and modification time and the link is
// untouched.
func TestDuplicateThroughSymlinkLeavesTargetAndLinkUntouched(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "registry.json")
	writeRegistry(t, target, `{"version":1,"batches":[{"batch":"B1","product":"P","quantity":5,"unit":"kg"}]}`)
	pinned := time.Date(2008, time.July, 4, 3, 2, 1, 0, time.UTC)
	if err := os.Chtimes(target, pinned, pinned); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	linkRegistry(t, link, "registry.json")

	reg, existed, err := Load(link)
	if err != nil || !existed {
		t.Fatalf("load through the link: existed=%v err=%v", existed, err)
	}
	outcome, err := Register(reg, Input{Batch: "B1", Product: "P", Quantity: 5, Unit: "kg"})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Created {
		t.Fatal("an identical repeat must confirm the duplicate, not create")
	}
	// The command layer saves only when a record was created; a duplicate
	// confirmation reaches no write at all.
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(pinned) {
		t.Fatalf("duplicate confirmation touched the target mtime: %v", info.ModTime())
	}
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("duplicate confirmation changed the target bytes")
	}
	assertLinkIntact(t, link, "registry.json")
}

// A link whose target does not exist is not a missing registry: both Load
// and Save refuse it with an error naming the caller's path and the reason,
// the target is never created, and the link is never replaced.
func TestDanglingSymlinkIsRejected(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	relTarget := filepath.Join("missing", "registry.json")
	linkRegistry(t, link, relTarget)

	reg, existed, err := Load(link)
	if err == nil {
		t.Fatal("Load must reject a link whose target does not exist")
	}
	if existed || reg != nil {
		t.Fatalf("a rejected load must report no registry: existed=%v reg=%+v", existed, reg)
	}
	for _, want := range []string{link, relTarget, "does not exist"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("load error %q must mention %q", err.Error(), want)
		}
	}

	err = Save(link, &Registry{Version: FormatVersion, Batches: []Batch{saveBatch("B1", "P", 1, "kg")}})
	if err == nil {
		t.Fatal("Save must reject a link whose target does not exist")
	}
	if !strings.Contains(err.Error(), link) {
		t.Errorf("save error %q must name the caller's path %q", err.Error(), link)
	}

	assertLinkIntact(t, link, relTarget)
	if _, err := os.Stat(filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the missing target must stay missing, stat err=%v", err)
	}
	assertNoLeftoverTempFiles(t, dir)
}

// A link loop has no determinable target: the operation is refused with an
// error naming the caller's path, and no file is created anywhere.
func TestSymlinkLoopIsRejected(t *testing.T) {
	dir := t.TempDir()
	linkA := filepath.Join(dir, "link-a")
	linkB := filepath.Join(dir, "link-b")
	linkRegistry(t, linkA, filepath.Base(linkB))
	linkRegistry(t, linkB, filepath.Base(linkA))

	if _, _, err := Load(linkA); err == nil {
		t.Fatal("Load must reject a link loop")
	} else if !strings.Contains(err.Error(), linkA) {
		t.Errorf("load error %q must name the caller's path %q", err.Error(), linkA)
	}
	err := Save(linkA, &Registry{Version: FormatVersion, Batches: []Batch{saveBatch("B1", "P", 1, "kg")}})
	if err == nil {
		t.Fatal("Save must reject a link loop")
	}
	if !strings.Contains(err.Error(), linkA) {
		t.Errorf("save error %q must name the caller's path %q", err.Error(), linkA)
	}

	assertLinkIntact(t, linkA, filepath.Base(linkB))
	assertLinkIntact(t, linkB, filepath.Base(linkA))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("the rejected operation must not create files: %v", entries)
	}
}

// A save that fails midway through a link keeps the target's bytes and mtime
// and the link itself exactly as they were, and leaves no temporary file in
// either the link's or the target's directory.
func TestFailedSaveThroughSymlinkKeepsEverythingIntact(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "data", "registry.json")
	writeRegistry(t, target, savedFailureRegistryJSON)
	if err := os.Chtimes(target, failedSaveModTime, failedSaveModTime); err != nil {
		t.Fatal(err)
	}
	originalBytes, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(root, "links")
	if err := os.MkdirAll(linkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(linkDir, "registry.json")
	linkRegistry(t, link, target)

	cause := errors.New("injected rename failure through link")
	old := RenameTempFile
	RenameTempFile = func(oldpath, newpath string) error {
		if newpath == link {
			t.Errorf("the replacement must target the resolved registry, not the link %q", link)
		}
		return cause
	}
	t.Cleanup(func() { RenameTempFile = old })

	submitted := failureSubmission()
	reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), submitted...)}
	err = Save(link, reg)
	if err == nil || !errors.Is(err, cause) {
		t.Fatalf("Save must report the injected failure, got %v", err)
	}
	if !strings.Contains(err.Error(), link) {
		t.Errorf("error %q must name the caller's path %q", err.Error(), link)
	}

	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(failedSaveModTime) {
		t.Fatalf("failed save touched the target mtime: %v", info.ModTime())
	}
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(originalBytes) {
		t.Fatal("failed save changed the target bytes")
	}
	assertLinkIntact(t, link, target)
	assertNoLeftoverTempFiles(t, root)
	assertBatchesEqual(t, reg.Batches, submitted)
}
