package batchreg

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// This file is the regression net for a registry path that the user spells as
// a DIRECTORY location rather than a file. The rule under guard:
//
//   - a path ending in one or more separators ("store/new.json/"), or whose
//     final component is "." or ".." ("store/new/.", "store/new/.."), denotes
//     a directory and can never name the registry file;
//   - Load and Save refuse it on the spelling alone, with a
//     *DirectoryPathError carrying the exact caller-supplied path, whether
//     the location does not exist yet, already is a directory, or has a
//     regular file directly beneath the spelled directory name;
//   - the refusal creates nothing: no missing parent directory, no registry
//     file or temporary file at the name left after stripping the trailing
//     separator or the final "."/"..", and an existing regular file keeps its
//     bytes, permissions and modification time while an existing directory or
//     symbolic link is never replaced;
//   - the caller's submitted records keep their content and order;
//   - relative and absolute spellings of the same directory structure behave
//     identically.
//
// Ordinary file paths are preserved, including one without a ".json" suffix,
// a doubled separator in the middle, a first registration whose parent does
// not exist, and a "." or ".." in a MIDDLE component.

// assertDirectoryPathError checks the shared error contract: a
// *DirectoryPathError whose Path is the exact user spelling, and a message
// quoting that path, stating that the target must be a file and that this
// path denotes a directory.
func assertDirectoryPathError(t *testing.T, err error, userPath string) {
	t.Helper()
	if err == nil {
		t.Fatalf("the directory-denoting path %q must be rejected", userPath)
	}
	var de *DirectoryPathError
	if !errors.As(err, &de) {
		t.Fatalf("want *DirectoryPathError for %q, got %T: %v", userPath, err, err)
	}
	if de.Path != userPath {
		t.Errorf("DirectoryPathError.Path = %q, want the exact supplied path %q", de.Path, userPath)
	}
	msg := err.Error()
	for _, want := range []string{
		strconv.Quote(userPath),
		"must be a file",
		"directory",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q must contain %q", msg, want)
		}
	}
}

// assertNothingCreated fails if any file (other than the explicitly allowed
// names, matched by base name) exists under root — this catches both the
// stripped-name registry file and a leftover temporary file.
func assertNothingCreated(t *testing.T, root string, allowed ...string) {
	t.Helper()
	keep := map[string]bool{}
	for _, n := range allowed {
		keep[n] = true
	}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		// A symbolic link is not a created regular file; the link cases assert
		// its target text themselves.
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".govflow-registry-") {
			t.Errorf("the rejected save left a temporary file: %s", path)
		}
		if !keep[d.Name()] {
			t.Errorf("the rejected operation created a file: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The directory-marker shapes, assembled WITHOUT filepath.Clean/Join so the
// trailing separator and the final "."/".." reach the package exactly as a
// user types them. stripped is the plain file a stripping save would wrongly
// create ("store/new.json/" would create "store/new.json"; "store/new/."
// would create "store/new"), and missingDir is the directory that would have
// to be brought into existence first ("new"); it stays "" when the marker
// hangs directly off the store directory.
func directoryShapeCases(store string) []struct {
	name       string
	path       string
	stripped   string
	missingDir string
} {
	sep := string(os.PathSeparator)
	return []struct {
		name       string
		path       string
		stripped   string
		missingDir string
	}{
		{"trailing slash", store + sep + "new.json" + sep, filepath.Join(store, "new.json"), ""},
		{"two trailing slashes", store + sep + "new.json" + sep + sep, filepath.Join(store, "new.json"), ""},
		{"final dot", store + sep + "new" + sep + ".", filepath.Join(store, "new"), "new"},
		{"final dot-dot", store + sep + "new" + sep + "..", filepath.Join(store, "new"), "new"},
	}
}

// Save into a location that does not exist must reject every directory-marker
// shape and create nothing — not the missing parent, and no plain file at the
// name left after stripping the marker.
func TestSaveRejectsDirectorySpellingWhenNothingExists(t *testing.T) {
	for _, tc := range []struct {
		name string
		abs  bool
	}{
		{"absolute", true},
		{"relative", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			store := filepath.Join(root, "store")
			if err := os.MkdirAll(store, 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.abs {
				t.Chdir(t.TempDir()) // unrelated cwd: absolute paths stand alone
			} else {
				t.Chdir(root)
			}
			storeArg := store
			if !tc.abs {
				storeArg = "store"
			}
			for _, shape := range directoryShapeCases(storeArg) {
				t.Run(shape.name, func(t *testing.T) {
					submitted := []Batch{saveBatch("NEW", "P-9", 5, "box")}
					reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), submitted...)}
					err := Save(shape.path, reg)

					assertDirectoryPathError(t, err, shape.path)
					// The stripped location is not written and any directory
					// that would first have to exist is not brought into being.
					if _, statErr := os.Lstat(shape.stripped); !os.IsNotExist(statErr) {
						t.Fatalf("no file may be created at the stripped name %q, stat err=%v", shape.stripped, statErr)
					}
					if shape.missingDir != "" {
						if _, statErr := os.Lstat(shape.missingDir); !os.IsNotExist(statErr) {
							t.Fatalf("the rejected save must not create missing parent %q, stat err=%v", shape.missingDir, statErr)
						}
					}
					assertNothingCreated(t, root)
					assertBatchesEqual(t, reg.Batches, submitted)
				})
			}
		})
	}
}

// Load of the same shapes is a refusal too — never an empty "fresh" registry
// that the command could go on to merge into.
func TestLoadRejectsDirectorySpellingWhenNothingExists(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "store")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, shape := range directoryShapeCases(store) {
		t.Run(shape.name, func(t *testing.T) {
			reg, existed, err := Load(shape.path)
			assertDirectoryPathError(t, err, shape.path)
			if reg != nil {
				t.Errorf("Load must return a nil registry on refusal, got %+v", reg)
			}
			if !existed {
				t.Error("a directory-denoting path must not be reported as a registry that merely does not exist")
			}
			assertNothingCreated(t, root)
		})
	}
}

// When the location already IS a directory (or names one through "."), both
// Load and Save reject it with the file-vs-directory rule and leave the
// directory and its contents exactly as they were.
func TestDirectorySpellingOverExistingDirectoryRejected(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "store")
	dir := filepath.Join(store, "dir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A file inside the directory proves the directory itself survives.
	inside := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(inside, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	sep := string(os.PathSeparator)
	paths := []string{
		dir + sep,
		dir + sep + sep,
		dir + sep + ".",
		dir + sep + "..",
	}
	for i, userPath := range paths {
		t.Run("shape-"+strconv.Itoa(i), func(t *testing.T) {
			submitted := []Batch{saveBatch("NEW", "P-9", 5, "box")}
			reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), submitted...)}

			_, existed, lerr := Load(userPath)
			assertDirectoryPathError(t, lerr, userPath)
			if !existed {
				t.Error("a path over an existing directory must not count as a missing registry")
			}
			serr := Save(userPath, reg)
			assertDirectoryPathError(t, serr, userPath)

			info, err := os.Stat(dir)
			if err != nil || !info.IsDir() {
				t.Fatalf("the existing directory must survive as a directory: %+v err=%v", info, err)
			}
			if data, err := os.ReadFile(inside); err != nil || string(data) != "x" {
				t.Fatalf("directory contents changed: data=%q err=%v", data, err)
			}
			assertNothingCreated(t, root, "keep.txt")
			assertBatchesEqual(t, reg.Batches, submitted)
		})
	}
}

// A regular file sitting directly beneath the spelled directory name is the
// exact shape that used to be created or collided with: "store/new.json/"
// beside plain file "store/new.json", or "store/new/." beside plain file
// "store/new". The operation is rejected and the file keeps its bytes,
// permissions and pinned modification time.
func TestDirectorySpellingOverRegularFileBeneathIsRejected(t *testing.T) {
	const oldContent = `{"version":1,"batches":[{"batch":"OLD","product":"P-1","quantity":10,"unit":"kg"}]}`
	sep := string(os.PathSeparator)
	cases := []struct {
		name string
		file string
		path string
	}{
		{
			"trailing slash beside new.json",
			"new.json", "new.json" + sep,
		},
		{
			"two trailing slashes beside new.json",
			"new.json", "new.json" + sep + sep,
		},
		{
			"dot component beside new.json",
			"new.json", "new.json" + sep + ".",
		},
		{
			"dot-dot component beside new.json",
			"new.json", "new.json" + sep + "..",
		},
		{
			"dot component beside plain new",
			"new", "new" + sep + ".",
		},
		{
			"dot-dot component beside plain new",
			"new", "new" + sep + "..",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			store := filepath.Join(root, "store")
			if err := os.MkdirAll(store, 0o755); err != nil {
				t.Fatal(err)
			}
			existing := filepath.Join(store, tc.file)
			writeRegistry(t, existing, oldContent)
			if err := os.Chmod(existing, 0o600); err != nil {
				t.Fatal(err)
			}
			pinned := pinFile(t, existing)
			// Raw join: filepath.Join/Clean would erase the trailing marker.
			userPath := store + sep + tc.path

			submitted := []Batch{saveBatch("NEW", "P-9", 5, "box")}
			reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), submitted...)}

			_, existed, lerr := Load(userPath)
			assertDirectoryPathError(t, lerr, userPath)
			if !existed {
				t.Error("the occupied location must not be treated as a missing registry")
			}
			serr := Save(userPath, reg)
			assertDirectoryPathError(t, serr, userPath)

			info, err := os.Stat(existing)
			if err != nil {
				t.Fatal(err)
			}
			if !info.Mode().IsRegular() {
				t.Fatalf("the existing file must stay a regular file, mode %v", info.Mode())
			}
			if info.Mode().Perm() != 0o600 {
				t.Errorf("the existing file permissions changed: got %o want 0600", info.Mode().Perm())
			}
			if !info.ModTime().Equal(pinned) {
				t.Errorf("the existing file mtime changed: got %v want %v", info.ModTime(), pinned)
			}
			data, err := os.ReadFile(existing)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != oldContent {
				t.Fatalf("the existing file bytes changed:\n%s", data)
			}
			assertNothingCreated(t, root, filepath.Base(existing))
			assertBatchesEqual(t, mustLoad(t, existing).Batches,
				[]Batch{saveBatch("OLD", "P-1", 10, "kg")})
			assertBatchesEqual(t, reg.Batches, submitted)
		})
	}
}

// A symbolic link is never replaced or chased into a write: a link to a file
// addressed with a directory marker, and a link to a directory addressed the
// same way, are both rejected while the link and its target stay intact.
func TestDirectorySpellingThroughSymlinkRejected(t *testing.T) {
	t.Run("symlink to registry file", func(t *testing.T) {
		root := t.TempDir()
		store := filepath.Join(root, "store")
		if err := os.MkdirAll(store, 0o755); err != nil {
			t.Fatal(err)
		}
		const oldContent = `{"version":1,"batches":[{"batch":"OLD","product":"P-1","quantity":10,"unit":"kg"}]}`
		target := filepath.Join(root, "real-registry.json")
		writeRegistry(t, target, oldContent)
		pinned := pinFile(t, target)
		link := filepath.Join(store, "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		for _, userPath := range []string{
			link + string(os.PathSeparator),
			link + string(os.PathSeparator) + ".",
		} {
			submitted := []Batch{saveBatch("NEW", "P-9", 5, "box")}
			reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), submitted...)}
			assertDirectoryPathError(t, Save(userPath, reg), userPath)
			got, err := os.Readlink(link)
			if err != nil || got != target {
				t.Fatalf("the link must be untouched: target=%q err=%v", got, err)
			}
			if info, err := os.Stat(target); err != nil || !info.ModTime().Equal(pinned) {
				t.Fatalf("the link target must be untouched: info=%+v err=%v", info, err)
			}
			if data, _ := os.ReadFile(target); string(data) != oldContent {
				t.Fatalf("the link target bytes changed:\n%s", data)
			}
			assertBatchesEqual(t, reg.Batches, submitted)
		}
		assertNothingCreated(t, root, filepath.Base(target))
	})

	t.Run("symlink to directory", func(t *testing.T) {
		root := t.TempDir()
		store := filepath.Join(root, "store")
		realDir := filepath.Join(root, "realdir")
		if err := os.MkdirAll(realDir, 0o755); err != nil {
			t.Fatal(err)
		}
		registry := filepath.Join(realDir, "batches.json")
		const oldContent = `{"version":1,"batches":[{"batch":"OLD","product":"P-1","quantity":10,"unit":"kg"}]}`
		writeRegistry(t, registry, oldContent)
		pinned := pinFile(t, registry)
		if err := os.MkdirAll(store, 0o755); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(store, "dlink")
		if err := os.Symlink(realDir, link); err != nil {
			t.Fatal(err)
		}
		userPath := link + string(os.PathSeparator)
		reg := &Registry{Version: FormatVersion, Batches: []Batch{saveBatch("NEW", "P-9", 5, "box")}}
		assertDirectoryPathError(t, Save(userPath, reg), userPath)
		if got, err := os.Readlink(link); err != nil || got != realDir {
			t.Fatalf("the directory link must be untouched: target=%q err=%v", got, err)
		}
		if info, err := os.Stat(registry); err != nil || !info.ModTime().Equal(pinned) {
			t.Fatalf("a registry inside the linked directory must be untouched: info=%+v err=%v", info, err)
		}
		assertNothingCreated(t, root, "batches.json")
	})
}

// Bare "." and ".." (the whole path is one directory component) and a trailing
// slash on the working directory are refused too.
func TestSaveRejectsBareDirectoryComponents(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	for _, userPath := range []string{".", "..", "." + string(os.PathSeparator)} {
		submitted := []Batch{saveBatch("NEW", "P-9", 5, "box")}
		reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), submitted...)}
		assertDirectoryPathError(t, Save(userPath, reg), userPath)
		assertDirectoryPathError(t, func() error {
			_, _, err := Load(userPath)
			return err
		}(), userPath)
		assertNothingCreated(t, root)
		assertBatchesEqual(t, reg.Batches, submitted)
	}
}

// Normal file paths keep working exactly as before: no ".json" suffix
// required, a doubled separator in the middle is harmless, a first
// registration into a not-yet-existing parent creates the directory and the
// file, and a "." or ".." in a MIDDLE component resolves ordinarily.
func TestSaveNormalFilePathsStillWork(t *testing.T) {
	want := []Batch{saveBatch("NEW", "P-9", 5, "box")}

	t.Run("no extension and missing parent", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "store", "new", "batches")
		reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), want...)}
		if err := Save(path, reg); err != nil {
			t.Fatalf("a normal file path without a .json suffix must save: %v", err)
		}
		assertBatchesEqual(t, mustLoad(t, path).Batches, want)
	})

	t.Run("doubled middle separator", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "store") + string(os.PathSeparator) + string(os.PathSeparator) + "b.json"
		reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), want...)}
		if err := Save(path, reg); err != nil {
			t.Fatalf("a doubled separator in the middle must not reject the save: %v", err)
		}
		assertBatchesEqual(t, mustLoad(t, filepath.Join(dir, "store", "b.json")).Batches, want)
	})

	t.Run("middle dot and dot-dot", func(t *testing.T) {
		root := t.TempDir()
		registry := filepath.Join(root, "store", "batches.json")
		writeRegistry(t, registry, `{"version":1,"batches":[{"batch":"OLD","product":"P-1","quantity":10,"unit":"kg"}]}`)
		if err := os.MkdirAll(filepath.Join(root, "store", "child"), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, userPath := range []string{
			filepath.Join(root, "store", ".", "batches.json"),
			filepath.Join(root, "store", "child", "..", "batches.json"),
		} {
			reg := mustLoad(t, userPath)
			if _, err := Register(reg, Input{Batch: "NEW", Product: "P-9", Quantity: 5, Unit: "box"}); err != nil {
				t.Fatal(err)
			}
			if err := Save(userPath, reg); err != nil {
				t.Fatalf("a middle %q must resolve by the ordinary rules: %v", userPath, err)
			}
		}
		assertBatchesEqual(t, mustLoad(t, registry).Batches, []Batch{
			saveBatch("OLD", "P-1", 10, "kg"),
			saveBatch("NEW", "P-9", 5, "box"),
		})
	})
}
