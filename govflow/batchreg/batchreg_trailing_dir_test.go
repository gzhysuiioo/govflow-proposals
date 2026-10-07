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

// fileSnapshot is a pinned byte/mtime view of a file a rejected operation must
// leave exactly as it was.
type fileSnapshot struct {
	path    string
	bytes   []byte
	modTime time.Time
}

func snapshotFile(t *testing.T, path string) *fileSnapshot {
	t.Helper()
	modTime := pinFile(t, path)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return &fileSnapshot{path: path, bytes: data, modTime: modTime}
}

func (s *fileSnapshot) assertUnchanged(t *testing.T) {
	t.Helper()
	after, err := os.ReadFile(s.path)
	if err != nil || !bytes.Equal(after, s.bytes) {
		t.Fatalf("file %s changed: %q (err=%v)", s.path, after, err)
	}
	info, err := os.Stat(s.path)
	if err != nil || !info.ModTime().Equal(s.modTime) {
		t.Fatalf("file %s mtime changed: got %v want %v (err=%v)", s.path, info.ModTime(), s.modTime, err)
	}
}

// This file is the regression net for registry paths whose spelling denotes a
// directory rather than a file: one or more trailing separators
// ("store/new.json/") or a final component of "." or ".." ("store/new/.",
// "store/new/.."). The kernel resolves every such spelling to a directory, so
// it can never itself hold the registry the caller would read back. Before the
// fix Save collapsed the tail lexically and created a regular file at
// "store/new.json" or "store/new", reported success, and left the user unable
// to read their registry through the path they had passed. The rule under
// guard: Save (and Load) refuse on the spelling alone, before touching the
// filesystem — the error quotes the path exactly as passed and states that the
// target must be a file while the path denotes a directory. Nothing is created
// (no missing parent directory, no registry file, no temporary file), an
// existing file keeps its bytes, permissions and modification time, existing
// directories and symbolic links are not replaced, and the caller's submitted
// records and their order are unchanged. Relative and absolute paths give the
// same verdict.

// assertDirPathRejected checks the shared error contract: a non-nil error that
// quotes the exact user path, says the target must be a file and that the path
// denotes a directory, and carries the package-level reason sentinel.
func assertDirPathRejected(t *testing.T, err error, userPath string) {
	t.Helper()
	if err == nil {
		t.Fatalf("Save/Load must reject the directory-denoting path %q", userPath)
	}
	if !errors.Is(err, errPathDenotesDirectory) {
		t.Fatalf("error %q must wrap errPathDenotesDirectory", err.Error())
	}
	msg := err.Error()
	for _, want := range []string{
		userPath,
		"must be a file",
		"directory",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q must mention %q", msg, want)
		}
	}
}

// sepPath joins parts with the OS separator without lexical cleaning, then
// appends exactly suffix separators (rawJoin/Clean would strip a trailing
// slash), so the directory spellings reach Save exactly as a user types them.
func sepPath(parts []string, trailingSlashes int) string {
	p := rawJoin(parts...)
	return p + strings.Repeat(string(os.PathSeparator), trailingSlashes)
}

// TestSaveRejectsDirectoryDenotingPathsWhenAbsent drives every directory
// spelling at a location that currently does not exist and through parents
// that do not exist either: nothing must be brought into being.
func TestSaveRejectsDirectoryDenotingPathsWhenAbsent(t *testing.T) {
	shapes := map[string]func(root string) string{
		"trailing slash": func(root string) string {
			return sepPath([]string{root, "store", "new.json"}, 1)
		},
		"two trailing slashes": func(root string) string {
			return sepPath([]string{root, "store", "new.json"}, 2)
		},
		"final dot": func(root string) string {
			return rawJoin(root, "store", "new", ".")
		},
		"final dotdot": func(root string) string {
			return rawJoin(root, "store", "new", "..")
		},
		"final dotdot with trailing slash": func(root string) string {
			return sepPath([]string{root, "store", "new", ".."}, 1)
		},
	}
	for name, shape := range shapes {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			userPath := shape(root)

			submitted := []Batch{
				saveBatch("B1", "P-1", 3, "kg"),
				saveBatch("B2", "P-2", 9, "box"),
			}
			reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), submitted...)}
			err := Save(userPath, reg)

			assertDirPathRejected(t, err, userPath)
			// Neither the stripped name nor any parent appears.
			for _, p := range []string{
				filepath.Join(root, "store"),
				filepath.Join(root, "store", "new.json"),
				filepath.Join(root, "store", "new"),
			} {
				if _, statErr := os.Lstat(p); !os.IsNotExist(statErr) {
					t.Fatalf("the rejected save must not create %s, stat err=%v", p, statErr)
				}
			}
			assertNoLeftoverTempFiles(t, root)
			assertBatchesEqual(t, reg.Batches, submitted)
		})
	}
}

// When the location already IS a directory (or a symbolic link to one), the
// spelling is refused just the same and the directory — or link — keeps
// exactly its existing identity and contents.
func TestSaveRejectsDirectoryDenotingPathsAtExistingDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "store", "d", "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "store", "d", "keep", "marker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	realDir := filepath.Join(root, "store", "realdir")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "store", "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}

	cases := map[string]string{
		"directory trailing slash": sepPath([]string{root, "store", "d"}, 1),
		"directory final dot":      rawJoin(root, "store", "d", "."),
		"directory final dotdot":   rawJoin(root, "store", "d", ".."),
		"dir-link trailing slash":  sepPath([]string{link}, 1),
		"dir-link final dot":       rawJoin(link, "."),
	}
	for name, userPath := range cases {
		t.Run(name, func(t *testing.T) {
			reg := &Registry{Version: FormatVersion, Batches: []Batch{saveBatch("B1", "P-1", 1, "kg")}}
			err := Save(userPath, reg)
			assertDirPathRejected(t, err, userPath)

			info, lerr := os.Lstat(filepath.Join(root, "store", "d"))
			if lerr != nil || !info.IsDir() {
				t.Fatalf("store/d must remain a directory, info=%v err=%v", info, lerr)
			}
			if _, err := os.ReadFile(filepath.Join(root, "store", "d", "keep", "marker")); err != nil {
				t.Fatalf("directory contents must survive: %v", err)
			}
			linfo, lerr2 := os.Lstat(link)
			if lerr2 != nil || linfo.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("the directory symlink must not be replaced, mode=%v err=%v", linfo, lerr2)
			}
			assertNoLeftoverTempFiles(t, root)
		})
	}
}

// When the name left after dropping the trailing spelling is already a regular
// file, that file keeps its bytes, permissions and modification time: the
// rejection happens before the filesystem is inspected at all, so Save never
// even opens the path through the directory spelling.
func TestSaveRejectsDirectoryDenotingPathsOverExistingFile(t *testing.T) {
	const originalJSON = `{"version":1,"batches":[{"batch":"OLD","product":"P-1","quantity":10,"unit":"kg"}]}`
	cases := map[string]struct {
		base string // existing regular file's name under store/
		tail string // directory spelling appended after the file's parent
	}{
		"trailing slash over a registry file": {"new.json", string(os.PathSeparator)},
		"final dot over a plain file":         {"new", string(os.PathSeparator) + "."},
		"final dotdot beside a plain file":    {"new", string(os.PathSeparator) + ".."},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			storeDir := filepath.Join(root, "store")
			if err := os.MkdirAll(storeDir, 0o755); err != nil {
				t.Fatal(err)
			}
			existing := filepath.Join(storeDir, tc.base)
			if err := os.WriteFile(existing, []byte(originalJSON), 0o600); err != nil {
				t.Fatal(err)
			}
			pinned := pinFile(t, existing)
			userPath := existing + tc.tail

			submitted := []Batch{saveBatch("NEW", "P-9", 5, "box")}
			reg := &Registry{Version: FormatVersion, Batches: append([]Batch(nil), submitted...)}
			err := Save(userPath, reg)
			assertDirPathRejected(t, err, userPath)

			after, rerr := os.ReadFile(existing)
			if rerr != nil || !bytes.Equal(after, []byte(originalJSON)) {
				t.Fatalf("existing file changed: %q (err=%v)", after, rerr)
			}
			info, serr := os.Stat(existing)
			if serr != nil {
				t.Fatal(serr)
			}
			if !info.ModTime().Equal(pinned) {
				t.Fatalf("mtime changed: got %v want %v", info.ModTime(), pinned)
			}
			if perm := info.Mode().Perm(); perm != 0o600 {
				t.Fatalf("permissions changed: got %o want 0600", perm)
			}
			if info.IsDir() {
				t.Fatal("the existing file must not become a directory")
			}
			assertBatchesEqual(t, mustLoad(t, existing).Batches,
				[]Batch{saveBatch("OLD", "P-1", 10, "kg")})
			assertNoLeftoverTempFiles(t, root)
			assertBatchesEqual(t, reg.Batches, submitted)
		})
	}
}

// A relative registry path and the absolute path with the same shape must give
// the same verdict; neither spelling creates anything. The paths are assembled
// by raw concatenation: filepath.Join/Clean would erase the trailing slash.
func TestSaveDirectoryDenotingPathsRelativeAndAbsoluteAgree(t *testing.T) {
	shapes := map[string]struct {
		rel []string // raw components joined, trailing slash carried by ""
	}{
		"trailing slash": {[]string{"store", "new.json", ""}},
		"final dot":      {[]string{"store", "new", "."}},
		"final dotdot":   {[]string{"store", "new", ".."}},
	}
	for name, tc := range shapes {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "store"), 0o755); err != nil {
				t.Fatal(err)
			}
			// A file sits at the stripped name: the rejection must preserve it.
			existing := filepath.Join(root, "store", "new.json")
			const originalJSON = `{"version":1,"batches":[]}`
			writeRegistry(t, existing, originalJSON)
			pinned := pinFile(t, existing)

			try := func(t *testing.T, userPath string) {
				reg := &Registry{Version: FormatVersion, Batches: []Batch{saveBatch("B1", "P-1", 1, "kg")}}
				err := Save(userPath, reg)
				assertDirPathRejected(t, err, userPath)
				after, rerr := os.ReadFile(existing)
				if rerr != nil || string(after) != originalJSON {
					t.Fatalf("the stripped-name file must be preserved: %q err=%v", after, rerr)
				}
				info, err2 := os.Stat(existing)
				if err2 != nil || !info.ModTime().Equal(pinned) {
					t.Fatalf("the stripped-name file's mtime must not move, info=%v err=%v", info, err2)
				}
				assertNoLeftoverTempFiles(t, root)
			}

			t.Run("relative", func(t *testing.T) {
				t.Chdir(root)
				try(t, rawJoin(tc.rel...))
			})
			t.Run("absolute", func(t *testing.T) {
				try(t, rawJoin(append([]string{root}, tc.rel...)...))
			})
		})
	}
}

// Load refuses the same spellings instead of treating them as either a
// readable registry or a missing first-registration file: it reports an error
// with a nil registry and never existed == false, in every filesystem state —
// absent, already a directory, or a valid file sitting at the stripped name.
func TestLoadRejectsDirectoryDenotingPaths(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T) (root, userPath string, preserve *fileSnapshot)
	}{
		{
			"absent location",
			func(t *testing.T) (string, string, *fileSnapshot) {
				root := t.TempDir()
				return root, sepPath([]string{root, "store", "new.json"}, 1), nil
			},
		},
		{
			"existing directory",
			func(t *testing.T) (string, string, *fileSnapshot) {
				root := t.TempDir()
				dir := filepath.Join(root, "store", "d")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				return root, rawJoin(dir, "."), nil
			},
		},
		{
			"registry file at the stripped name",
			func(t *testing.T) (string, string, *fileSnapshot) {
				root := t.TempDir()
				file := filepath.Join(root, "store", "new.json")
				writeRegistry(t, file, `{"version":1,"batches":[]}`)
				return root, file + string(os.PathSeparator), snapshotFile(t, file)
			},
		},
		{
			"final dotdot",
			func(t *testing.T) (string, string, *fileSnapshot) {
				root := t.TempDir()
				if err := os.MkdirAll(filepath.Join(root, "store"), 0o755); err != nil {
					t.Fatal(err)
				}
				return root, rawJoin(root, "store", "new", ".."), nil
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, userPath, preserve := tc.setup(t)

			reg, existed, err := Load(userPath)
			if reg != nil {
				t.Fatalf("Load must return a nil registry on the directory spelling, got %+v", reg)
			}
			if !existed {
				t.Fatal("Load must not signal a fresh first registration for a directory location")
			}
			assertDirPathRejected(t, err, userPath)
			if preserve != nil {
				preserve.assertUnchanged(t)
			}
			assertNoLeftoverTempFiles(t, root)
		})
	}
}

// Preserved normal behavior: ordinary file names without a directory spelling
// — no .json suffix needed — still save, including a middle "." or ".." that
// resolves against a real directory and a first save into a missing parent.
func TestSaveFileSpellingsStillWork(t *testing.T) {
	t.Run("no suffix and missing parent", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "store", "new", "registry-data")
		reg := &Registry{Version: FormatVersion, Batches: []Batch{saveBatch("B1", "P-1", 1, "kg")}}
		if err := Save(path, reg); err != nil {
			t.Fatalf("a normal file name must save: %v", err)
		}
		assertBatchesEqual(t, mustLoad(t, path).Batches, reg.Batches)
	})
	t.Run("middle dot and dotdot against real directories", func(t *testing.T) {
		root := t.TempDir()
		real := filepath.Join(root, "store", "child")
		if err := os.MkdirAll(real, 0o755); err != nil {
			t.Fatal(err)
		}
		path := rawJoin(root, "store", ".", "child", "..", "reg.json")
		reg := &Registry{Version: FormatVersion, Batches: []Batch{saveBatch("B1", "P-1", 1, "kg")}}
		if err := Save(path, reg); err != nil {
			t.Fatalf("a middle . or .. over a real directory must resolve and save: %v", err)
		}
		assertBatchesEqual(t, mustLoad(t, filepath.Join(root, "store", "reg.json")).Batches, reg.Batches)
		assertNoLeftoverTempFiles(t, root)
	})
}
