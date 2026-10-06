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

// This file is the regression net for a registry path whose ANCESTOR
// DIRECTORY is a symbolic link followed by "..": with work/alias ->
// store/child, the user names work/alias/../batches.json. The kernel
// resolves that path to store/batches.json, but lexical path cleaning folds
// it to work/batches.json — so a naive save reads the real registry yet
// prepares its temporary file (and its failure messages) against the wrong
// directory. The rule under guard:

//   - a save lands in the exact file Load read: store/batches.json, with the
//     old records first and new records appended;
//   - reading through the user's path and the real path shows the same
//     records;
//   - the temporary file is prepared in the real target directory store, so a
//     read-only work directory does not matter and no file, temporary file or
//     directory is ever created under work;
//   - work/batches.json, when it exists, is an unrelated decoy whose bytes,
//     modification time and permissions stay exactly as they were, and the
//     directory link keeps its exact target text;
//   - when the real target directory cannot be written, or a save step fails
//     there, the operation is rejected: the real registry keeps its bytes and
//     modification time, nothing is created on the link side, and no
//     temporary file survives;
//   - ordinary paths and paths ending in a file symlink keep their exact
//     previous resolution, and a dangling/looping link in a parent directory
//     is rejected rather than treated as a first registration.

// dotDotFixtureRegistry is the existing record the real registry starts with.
const dotDotFixtureRegistry = `{"version":1,"batches":[{"batch":"OLD-1","product":"P-1","quantity":10,"unit":"kg"}]}`

// dotDotFixture is built exactly like the product scenario: work/alias is a
// directory symlink to store/child (relative target text "../store/child"),
// the real registry is store/batches.json and work/batches.json is an
// unrelated decoy. userPath is assembled by concatenation on purpose:
// filepath.Join would lexically clean "alias/.." away and reproduce nothing.
type dotDotFixture struct {
	root      string
	storeDir  string
	workDir   string
	realPath  string
	userPath  string
	decoyPath string
	linkPath  string
}

func setupDotDotFixture(t *testing.T) dotDotFixture {
	t.Helper()
	f := dotDotFixture{root: t.TempDir()}
	f.storeDir = filepath.Join(f.root, "store")
	f.workDir = filepath.Join(f.root, "work")
	if err := os.MkdirAll(filepath.Join(f.storeDir, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	f.realPath = filepath.Join(f.storeDir, "batches.json")
	writeRegistry(t, f.realPath, dotDotFixtureRegistry)
	if err := os.Chmod(f.realPath, symlinkFixtureMode); err != nil {
		t.Fatal(err)
	}
	f.decoyPath = filepath.Join(f.workDir, "batches.json")
	writeRegistry(t, f.decoyPath, `{"unrelated":"decoy"}`)
	f.linkPath = filepath.Join(f.workDir, "alias")
	if err := os.Symlink(filepath.Join("..", "store", "child"), f.linkPath); err != nil {
		t.Fatal(err)
	}
	f.userPath = f.workDir + string(filepath.Separator) + "alias" +
		string(filepath.Separator) + ".." + string(filepath.Separator) + "batches.json"
	return f
}

// allowDirCleanup restores writable permissions so t.TempDir's removal can
// never be blocked by a mode a test deliberately narrowed.
func allowDirCleanup(t *testing.T, dirs ...string) {
	t.Helper()
	t.Cleanup(func() {
		for _, d := range dirs {
			_ = os.Chmod(d, 0o755)
		}
	})
}

// assertDecoyUntouched checks the unrelated work/batches.json byte for byte,
// including its modification time and permissions.
func assertDecoyUntouched(t *testing.T, path string, wantBytes []byte, wantPerm os.FileMode, wantMod time.Time) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the unrelated decoy must still exist: %v", err)
	}
	if !info.ModTime().Equal(wantMod) {
		t.Errorf("decoy modification time changed: got %v, want %v", info.ModTime(), wantMod)
	}
	if info.Mode().Perm() != wantPerm {
		t.Errorf("decoy permissions = %v, want %v", info.Mode().Perm(), wantPerm)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, wantBytes) {
		t.Errorf("decoy bytes changed:\nbefore %s\nafter  %s", wantBytes, got)
	}
}

// assertOnlyExpectedWorkEntries fails if the link-side directory holds
// anything beyond the directory link and the decoy registry: a save must
// never create a registry file, a temporary file or a new directory there.
func assertOnlyExpectedWorkEntries(t *testing.T, workDir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(workDir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name()] = true
	}
	if len(got) != len(want) {
		t.Fatalf("link-side directory %s holds %v, want only %v", workDir, entries, want)
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("link-side directory %s is missing expected entry %q; entries: %v", workDir, name, entries)
		}
	}
}

// TestSaveThroughDirSymlinkDotDotAppendsToRealRegistry is the headline
// scenario: registering through work/alias/../batches.json appends to
// store/batches.json, old record first; both read paths agree; the decoy,
// its metadata and the link are untouched; no temporary file survives.
func TestSaveThroughDirSymlinkDotDotAppendsToRealRegistry(t *testing.T) {
	f := setupDotDotFixture(t)
	pinned := time.Date(2008, time.May, 6, 7, 8, 9, 0, time.UTC)
	if err := os.Chtimes(f.realPath, pinned, pinned); err != nil {
		t.Fatal(err)
	}
	decoyBytes, err := os.ReadFile(f.decoyPath)
	if err != nil {
		t.Fatal(err)
	}
	decoyInfo, err := os.Stat(f.decoyPath)
	if err != nil {
		t.Fatal(err)
	}
	decoyMod := decoyInfo.ModTime()

	reg, existed, err := Load(f.userPath)
	if err != nil || !existed {
		t.Fatalf("Load through the user path must read the real registry: existed=%v err=%v", existed, err)
	}
	if len(reg.Batches) != 1 || reg.Batches[0].Batch != "OLD-1" {
		t.Fatalf("Load must see the existing record: %+v", reg.Batches)
	}
	if _, err := Register(reg, Input{Batch: "NEW-1", Product: "P-9", Quantity: 5, Unit: "box"}); err != nil {
		t.Fatal(err)
	}
	if err := Save(f.userPath, reg); err != nil {
		t.Fatalf("saving through the directory link and .. must succeed: %v", err)
	}

	want := []Batch{
		saveBatch("OLD-1", "P-1", 10, "kg"),
		saveBatch("NEW-1", "P-9", 5, "box"),
	}
	viaReal, existed, err := Load(f.realPath)
	if err != nil || !existed {
		t.Fatalf("the real registry must load: existed=%v err=%v", existed, err)
	}
	assertBatchesEqual(t, viaReal.Batches, want)
	viaUser, existed, err := Load(f.userPath)
	if err != nil || !existed {
		t.Fatalf("the user path must load: existed=%v err=%v", existed, err)
	}
	assertBatchesEqual(t, viaUser.Batches, want)

	// The real file was really replaced (mtime moves), keeping its
	// permissions, while the raw bytes of both read paths are identical.
	realInfo, err := os.Stat(f.realPath)
	if err != nil {
		t.Fatal(err)
	}
	if realInfo.ModTime().Equal(pinned) {
		t.Fatal("a creating save must replace the real registry; the pinned mtime survived")
	}
	if realInfo.Mode().Perm() != symlinkFixtureMode {
		t.Fatalf("real registry permissions = %v, want preserved %v", realInfo.Mode().Perm(), symlinkFixtureMode)
	}
	realBytes, err := os.ReadFile(f.realPath)
	if err != nil {
		t.Fatal(err)
	}
	userBytes, err := os.ReadFile(f.userPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(realBytes, userBytes) {
		t.Fatalf("bytes read through the user path and the real path differ:\n%s\n%s", userBytes, realBytes)
	}

	assertLinkIntact(t, f.linkPath, filepath.Join("..", "store", "child"))
	assertDecoyUntouched(t, f.decoyPath, decoyBytes, decoyInfo.Mode().Perm(), decoyMod)
	assertOnlyExpectedWorkEntries(t, f.workDir, "alias", "batches.json")
	assertNoLeftoverTempFiles(t, f.root)
}

// TestSaveThroughDirSymlinkDotDotNeedsNoWriteOnLinkSide proves the write
// permission requirement: with work read-and-execute only, the save still
// succeeds because the temporary file is prepared in the real directory
// store, and work gains nothing. Skipped for root, whose permission checks
// are bypassed by the kernel.
func TestSaveThroughDirSymlinkDotDotNeedsNoWriteOnLinkSide(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permission bits are not enforced for root")
	}
	f := setupDotDotFixture(t)
	if err := os.Chmod(f.workDir, 0o555); err != nil {
		t.Fatal(err)
	}
	allowDirCleanup(t, f.workDir)

	reg, existed, err := Load(f.userPath)
	if err != nil || !existed {
		t.Fatalf("Load must succeed with read+execute on work: existed=%v err=%v", existed, err)
	}
	if _, err := Register(reg, Input{Batch: "NEW-1", Product: "P-9", Quantity: 5, Unit: "box"}); err != nil {
		t.Fatal(err)
	}
	if err := Save(f.userPath, reg); err != nil {
		t.Fatalf("the save must need write permission only on the real directory, got: %v", err)
	}
	loaded, existed, err := Load(f.realPath)
	if err != nil || !existed || len(loaded.Batches) != 2 {
		t.Fatalf("real registry must hold two records: existed=%v err=%v batches=%+v", existed, err, loaded)
	}
	assertOnlyExpectedWorkEntries(t, f.workDir, "alias", "batches.json")
	assertNoLeftoverTempFiles(t, f.root)
}

// TestSaveFirstRegistrationThroughDirSymlinkDotDot covers a registry that
// does not exist yet under the real directory: it is created next to the
// real location even though work is read-only, never at work/batches.json.
func TestSaveFirstRegistrationThroughDirSymlinkDotDot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permission bits are not enforced for root")
	}
	root := t.TempDir()
	storeDir := filepath.Join(root, "store")
	workDir := filepath.Join(root, "work")
	if err := os.MkdirAll(filepath.Join(storeDir, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(workDir, "alias")
	if err := os.Symlink(filepath.Join("..", "store", "child"), linkPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(workDir, 0o555); err != nil {
		t.Fatal(err)
	}
	allowDirCleanup(t, workDir)
	userPath := workDir + string(filepath.Separator) + "alias" + string(filepath.Separator) +
		".." + string(filepath.Separator) + "brand-new.json"
	realPath := filepath.Join(storeDir, "brand-new.json")

	reg, existed, err := Load(userPath)
	if err != nil || existed {
		t.Fatalf("Load of a missing real registry signals a fresh one: reg=%+v existed=%v err=%v", reg, existed, err)
	}
	if _, err := Register(reg, Input{Batch: "B-1", Product: "P-1", Quantity: 1, Unit: "kg"}); err != nil {
		t.Fatal(err)
	}
	if err := Save(userPath, reg); err != nil {
		t.Fatalf("first registration must create the file in the real directory: %v", err)
	}
	if _, err := os.Stat(realPath); err != nil {
		t.Fatalf("the registry must be created at the real path %s: %v", realPath, err)
	}
	loaded, existed, err := Load(userPath)
	if err != nil || !existed || len(loaded.Batches) != 1 || loaded.Batches[0].Batch != "B-1" {
		t.Fatalf("reading the new registry through the user path failed: existed=%v err=%v", existed, err)
	}
	assertLinkIntact(t, linkPath, filepath.Join("..", "store", "child"))
	assertOnlyExpectedWorkEntries(t, workDir, "alias")
	assertNoLeftoverTempFiles(t, root)
}

// TestImportThroughDirSymlinkDotDotSavesToRealRegistry exercises the Go
// library import entry: an all-or-nothing manifest mixing a duplicate and a
// new batch lands in the real registry, results keep manifest order and the
// link side stays untouched.
func TestImportThroughDirSymlinkDotDotSavesToRealRegistry(t *testing.T) {
	f := setupDotDotFixture(t)
	reg, existed, err := Load(f.userPath)
	if err != nil || !existed {
		t.Fatalf("Load through the user path must read the real registry: existed=%v err=%v", existed, err)
	}
	inputs, err := ParseManifest([]byte(`[
		{"batch":"OLD-1","product":"P-1","quantity":10,"unit":"kg"},
		{"batch":"NEW-2","product":"P-8","quantity":3,"unit":"box"}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	results, err := Import(reg, inputs)
	if err != nil {
		t.Fatalf("the legal manifest must import: %v", err)
	}
	if len(results) != 2 || results[0].Created || !results[1].Created {
		t.Fatalf("results = %+v, want duplicate then created", results)
	}
	if err := Save(f.userPath, reg); err != nil {
		t.Fatalf("saving the import must reach the real registry: %v", err)
	}
	want := []Batch{
		saveBatch("OLD-1", "P-1", 10, "kg"),
		saveBatch("NEW-2", "P-8", 3, "box"),
	}
	loaded, existed, err := Load(f.realPath)
	if err != nil || !existed {
		t.Fatalf("real registry must load: existed=%v err=%v", existed, err)
	}
	assertBatchesEqual(t, loaded.Batches, want)
	assertLinkIntact(t, f.linkPath, filepath.Join("..", "store", "child"))
	assertOnlyExpectedWorkEntries(t, f.workDir, "alias", "batches.json")
	assertNoLeftoverTempFiles(t, f.root)
}

// TestSaveFailureThroughDirSymlinkDotDotLeavesNoTrace injects both mid-save
// failures through the user path. Whichever step fails, the error names the
// path the user passed and carries the cause, the real registry keeps its
// bytes and pinned modification time, the decoy is untouched and no
// temporary file survives on either side of the link.
func TestSaveFailureThroughDirSymlinkDotDotLeavesNoTrace(t *testing.T) {
	cause := errors.New("injected failure through directory link")
	cases := []struct {
		name   string
		fault  func(t *testing.T, f dotDotFixture)
		reason string
	}{
		{
			name:   "content write incomplete",
			reason: cause.Error(),
			fault: func(t *testing.T, f dotDotFixture) {
				old := WriteTempContent
				WriteTempContent = func(file *os.File, data []byte) (int, error) {
					half := len(data) / 2
					if _, err := file.Write(data[:half]); err != nil {
						return 0, err
					}
					return half, cause
				}
				t.Cleanup(func() { WriteTempContent = old })
			},
		},
		{
			name:   "replacement refused",
			reason: cause.Error(),
			fault: func(t *testing.T, f dotDotFixture) {
				old := RenameTempFile
				RenameTempFile = func(oldpath, newpath string) error {
					// The prepared file must sit in the real directory and the
					// replacement must target the real registry — never
					// work/batches.json.
					gotDir, err := os.Stat(filepath.Dir(oldpath))
					if err != nil {
						t.Errorf("temporary directory must be readable: %v", err)
					}
					wantDir, err := os.Stat(f.storeDir)
					if err != nil {
						t.Fatal(err)
					}
					if !os.SameFile(gotDir, wantDir) {
						t.Errorf("temporary file %q must be prepared in the real directory %s", oldpath, f.storeDir)
					}
					gotTarget, err := os.Stat(newpath)
					if err != nil {
						t.Errorf("rename target must be the existing real registry: %v", err)
					}
					wantTarget, err := os.Stat(f.realPath)
					if err != nil {
						t.Fatal(err)
					}
					if !os.SameFile(gotTarget, wantTarget) {
						t.Errorf("rename target %q must be the real registry %s", newpath, f.realPath)
					}
					return cause
				}
				t.Cleanup(func() { RenameTempFile = old })
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := setupDotDotFixture(t)
			pinned := time.Date(2007, time.April, 5, 6, 7, 8, 0, time.UTC)
			if err := os.Chtimes(f.realPath, pinned, pinned); err != nil {
				t.Fatal(err)
			}
			originalBytes, err := os.ReadFile(f.realPath)
			if err != nil {
				t.Fatal(err)
			}
			decoyBytes, err := os.ReadFile(f.decoyPath)
			if err != nil {
				t.Fatal(err)
			}
			decoyInfo, err := os.Stat(f.decoyPath)
			if err != nil {
				t.Fatal(err)
			}
			tc.fault(t, f)

			reg, existed, err := Load(f.userPath)
			if err != nil || !existed {
				t.Fatalf("fixture must load: existed=%v err=%v", existed, err)
			}
			if _, err := Register(reg, Input{Batch: "NEW-1", Product: "P-9", Quantity: 5, Unit: "box"}); err != nil {
				t.Fatal(err)
			}
			saveErr := Save(f.userPath, reg)
			if saveErr == nil {
				t.Fatal("the injected failure must be reported")
			}
			if !errors.Is(saveErr, cause) {
				t.Errorf("error %v must carry the cause %v", saveErr, cause)
			}
			msg := saveErr.Error()
			if !strings.Contains(msg, f.userPath) {
				t.Errorf("error %q must name the registry path the user passed %q", msg, f.userPath)
			}

			info, err := os.Stat(f.realPath)
			if err != nil {
				t.Fatal(err)
			}
			if !info.ModTime().Equal(pinned) {
				t.Errorf("failed save changed the real registry mtime: got %v, want %v", info.ModTime(), pinned)
			}
			after, err := os.ReadFile(f.realPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, originalBytes) {
				t.Fatalf("failed save changed the real registry bytes:\nbefore %s\nafter  %s", originalBytes, after)
			}
			assertDecoyUntouched(t, f.decoyPath, decoyBytes, decoyInfo.Mode().Perm(), decoyInfo.ModTime())
			assertLinkIntact(t, f.linkPath, filepath.Join("..", "store", "child"))
			assertOnlyExpectedWorkEntries(t, f.workDir, "alias", "batches.json")
			assertNoLeftoverTempFiles(t, f.root)
		})
	}
}

// TestSaveRejectsWhenRealTargetDirectoryUnwritable proves the failure side:
// with work writable but the REAL directory store read-only, a creating save
// must be refused with an error naming the user path and the reason; the
// real registry is untouched, no file appears in work and no temporary file
// survives. Skipped for root.
func TestSaveRejectsWhenRealTargetDirectoryUnwritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permission bits are not enforced for root")
	}
	f := setupDotDotFixture(t)
	pinned := time.Date(2006, time.March, 4, 5, 6, 7, 0, time.UTC)
	if err := os.Chtimes(f.realPath, pinned, pinned); err != nil {
		t.Fatal(err)
	}
	originalBytes, err := os.ReadFile(f.realPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f.storeDir, 0o555); err != nil {
		t.Fatal(err)
	}
	allowDirCleanup(t, f.storeDir, filepath.Join(f.storeDir, "child"))

	reg, existed, err := Load(f.userPath)
	if err != nil || !existed {
		t.Fatalf("reading must still work through the read-only real directory: existed=%v err=%v", existed, err)
	}
	if _, err := Register(reg, Input{Batch: "NEW-1", Product: "P-9", Quantity: 5, Unit: "box"}); err != nil {
		t.Fatal(err)
	}
	saveErr := Save(f.userPath, reg)
	if saveErr == nil {
		t.Fatal("a save into an unwritable real target directory must be rejected")
	}
	msg := saveErr.Error()
	if !strings.Contains(msg, f.userPath) {
		t.Errorf("error %q must name the registry path the user passed %q", msg, f.userPath)
	}
	if !strings.Contains(msg, "permission denied") {
		t.Errorf("error %q must state the concrete reason", msg)
	}

	info, err := os.Stat(f.realPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(pinned) {
		t.Errorf("rejected save changed the real registry mtime: got %v, want %v", info.ModTime(), pinned)
	}
	after, err := os.ReadFile(f.realPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, originalBytes) {
		t.Fatalf("rejected save changed the real registry bytes:\nbefore %s\nafter  %s", originalBytes, after)
	}
	assertOnlyExpectedWorkEntries(t, f.workDir, "alias", "batches.json")
	assertNoLeftoverTempFiles(t, f.root)
}

// TestResolveRegistryTargetShapes pins the resolution rules directly:
// ordinary paths keep their spelling, the directory-link-plus-".." crossing
// diverges to the physical file and directory, and a dangling or looping
// link in a parent directory is an error rather than a first registration.
func TestResolveRegistryTargetShapes(t *testing.T) {
	f := setupDotDotFixture(t)

	sameFile := func(t *testing.T, got, want string) {
		t.Helper()
		g, err := os.Stat(got)
		if err != nil {
			t.Fatalf("resolved path %s must be usable: %v", got, err)
		}
		w, err := os.Stat(want)
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(g, w) {
			t.Errorf("resolved %q must identify %q", got, want)
		}
	}

	t.Run("directory link with .. resolves to the real file", func(t *testing.T) {
		target, dir, err := resolveRegistryTarget(f.userPath)
		if err != nil {
			t.Fatal(err)
		}
		sameFile(t, target, f.realPath)
		d, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		wantDir, err := os.Stat(f.storeDir)
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(d, wantDir) {
			t.Errorf("save directory %q must be the real directory %s", dir, f.storeDir)
		}
	})

	t.Run("plain relative path keeps its spelling", func(t *testing.T) {
		rel := filepath.Join(f.storeDir, "plain.json")
		target, dir, err := resolveRegistryTarget(rel)
		if err != nil {
			t.Fatal(err)
		}
		if target != rel || dir != filepath.Dir(rel) {
			t.Errorf("plain path = (%q, %q), want unchanged (%q, %q)", target, dir, rel, filepath.Dir(rel))
		}
	})

	t.Run("missing nested directory keeps the relative spelling", func(t *testing.T) {
		rel := filepath.Join(f.storeDir, "a", "b", "new.json")
		target, dir, err := resolveRegistryTarget(rel)
		if err != nil {
			t.Fatal(err)
		}
		if target != rel || dir != filepath.Dir(rel) {
			t.Errorf("missing-tail path = (%q, %q), want unchanged (%q, %q)", target, dir, rel, filepath.Dir(rel))
		}
	})

	t.Run("missing nested directory behind a harmless .. keeps its spelling", func(t *testing.T) {
		// store/child/../a/b/new.json: no link, child exists, a/b missing.
		// Physical and lexical readings agree, so the caller's spelling and
		// the legacy MkdirAll/CreateTemp resolution are preserved exactly.
		rel := f.storeDir + string(filepath.Separator) + "child" + string(filepath.Separator) +
			".." + string(filepath.Separator) + "a" + string(filepath.Separator) +
			"b" + string(filepath.Separator) + "new.json"
		target, dir, err := resolveRegistryTarget(rel)
		if err != nil {
			t.Fatal(err)
		}
		if target != rel || dir != filepath.Dir(rel) {
			t.Errorf("harmless-.. missing-tail path = (%q, %q), want unchanged (%q, %q)", target, dir, rel, filepath.Dir(rel))
		}
	})

	t.Run("valid crossing then missing dir before .. is rejected", func(t *testing.T) {
		// work/alias/no-such/../x.json: alias resolves into store/child, but
		// "no-such" does not exist, so the ".." cannot be resolved to any real
		// directory ahead of the save. It must not be folded into store.
		user := f.linkPath + string(filepath.Separator) + "no-such" +
			string(filepath.Separator) + ".." + string(filepath.Separator) + "x.json"
		if _, _, err := resolveRegistryTarget(user); err == nil {
			t.Fatal("a path whose \"..\" follows a missing component past a link must be rejected")
		}
	})

	t.Run("valid crossing then a new directory without .. points at the real side", func(t *testing.T) {
		// work/alias/../brand-dir/new.json: the crossing is real, brand-dir
		// is a genuinely new directory. The save must create it under store,
		// and the computed target lives there.
		user := f.linkPath + string(filepath.Separator) + ".." + string(filepath.Separator) +
			"brand-dir" + string(filepath.Separator) + "new.json"
		target, dir, err := resolveRegistryTarget(user)
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Dir(target) != dir {
			t.Fatalf("target %q must sit in the save directory %q", target, dir)
		}
		wantDir := filepath.Join(f.storeDir, "brand-dir")
		if dir != wantDir {
			t.Errorf("save directory = %q, want the real-side new directory %q", dir, wantDir)
		}
	})

	t.Run("harmless .. without a crossing link keeps its spelling", func(t *testing.T) {
		// store/child/.. points at store itself; no link is crossed.
		rel := f.storeDir + string(filepath.Separator) + "child" +
			string(filepath.Separator) + ".." + string(filepath.Separator) + "batches.json"
		target, dir, err := resolveRegistryTarget(rel)
		if err != nil {
			t.Fatal(err)
		}
		if target != rel {
			t.Errorf("harmless-dotdot path = %q, want unchanged %q", target, rel)
		}
		sameFile(t, target, f.realPath)
		sameFile(t, dir, f.storeDir)
	})

	t.Run("a final file symlink resolves to its target", func(t *testing.T) {
		link := filepath.Join(f.workDir, "direct-link.json")
		if err := os.Symlink(f.realPath, link); err != nil {
			t.Fatal(err)
		}
		target, dir, err := resolveRegistryTarget(link)
		if err != nil {
			t.Fatal(err)
		}
		sameFile(t, target, f.realPath)
		sameFile(t, dir, f.storeDir)
	})

	t.Run("dangling link in a parent directory is rejected", func(t *testing.T) {
		bad := filepath.Join(f.workDir, "bad-alias")
		if err := os.Symlink(filepath.Join("..", "no-such-store", "child"), bad); err != nil {
			t.Fatal(err)
		}
		user := bad + string(filepath.Separator) + ".." + string(filepath.Separator) + "batches.json"
		if _, _, err := resolveRegistryTarget(user); err == nil {
			t.Fatal("a dangling directory link in the ancestry must reject the save")
		}
		// The lexical sibling must not be offered as a fallback target: a
		// failed resolution creates nothing, and the decoy stays the only
		// batches.json on the link side.
		assertOnlyExpectedWorkEntries(t, f.workDir, "alias", "bad-alias", "batches.json", "direct-link.json")
	})

	t.Run("link loop in a parent directory is rejected", func(t *testing.T) {
		loopA := filepath.Join(f.workDir, "loop-a")
		loopB := filepath.Join(f.workDir, "loop-b")
		if err := os.Symlink("loop-b", loopA); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("loop-a", loopB); err != nil {
			t.Fatal(err)
		}
		user := loopA + string(filepath.Separator) + ".." + string(filepath.Separator) + "batches.json"
		if _, _, err := resolveRegistryTarget(user); err == nil {
			t.Fatal("a link loop in the ancestry must reject the save")
		}
	})

	t.Run("dangling link hidden before the tail .. is rejected", func(t *testing.T) {
		// The link resolves on its own but the component after it does not
		// exist, so the kernel cannot resolve "x/.."; a lexical cleanup would
		// silently drop both and save into the wrong directory.
		user := f.linkPath + string(filepath.Separator) + "no-such" +
			string(filepath.Separator) + ".." + string(filepath.Separator) + "batches.json"
		if _, _, err := resolveRegistryTarget(user); err == nil {
			t.Fatal("a path whose kernel resolution fails on a missing component before .. must be rejected")
		}
		// The resolver is read-only: no regular registry file appears on the
		// link side, only the directory links from the earlier subtests.
		assertOnlyExpectedWorkEntries(t, f.workDir,
			"alias", "bad-alias", "loop-a", "loop-b", "batches.json", "direct-link.json")
	})

	t.Run("dangling link followed by a deeper tail is rejected", func(t *testing.T) {
		bad := filepath.Join(f.workDir, "deep-bad")
		if err := os.Symlink(filepath.Join("..", "gone", "child"), bad); err != nil {
			t.Fatal(err)
		}
		user := bad + string(filepath.Separator) + "x" +
			string(filepath.Separator) + ".." + string(filepath.Separator) + "batches.json"
		if _, _, err := resolveRegistryTarget(user); err == nil {
			t.Fatal("a dangling directory link anywhere in the ancestry must reject the save")
		}
	})
}
