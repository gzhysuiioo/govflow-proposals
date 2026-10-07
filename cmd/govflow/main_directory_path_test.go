package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// End-to-end regression for a --registry path that the user spells as a
// DIRECTORY location rather than a file: it ends in one or more separators
// ("store/new.json/"), or its final component is "." or ".."
// ("store/new/.", "store/new/.."). Both commands must reject such a path
// instead of reporting "created": a non-zero exit code, stderr carrying the
// exact user-supplied registry path and stating that the registry target
// must be a file while this path denotes a directory, and empty stdout with
// no "created", "duplicate" or import "results" payload anywhere.
//
// The rejection must hold whether the location does not exist yet, already is
// a directory, or has a regular file directly beneath the spelled name; it
// must create no missing parent directory, no plain file at the name left
// after stripping the trailing marker, and no save temporary file. Relative
// and absolute spellings behave alike. A normal file path — one without a
// ".json" suffix included — keeps registering successfully.

// cliDirShapes returns the directory-marker shapes over the store directory
// plus the stripped plain-file name a stripping save would wrongly create and
// the directory that would first have to be brought into existence. The
// paths are raw-joined so the trailing separator and final "."/".." survive.
func cliDirShapes(store string) []struct {
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
		{"final dot", store + sep + "new" + sep + ".", filepath.Join(store, "new"), filepath.Join(store, "new")},
		{"final dot-dot", store + sep + "new" + sep + "..", filepath.Join(store, "new"), filepath.Join(store, "new")},
	}
}

// assertDirectoryChildFailure checks the process contract shared by both
// commands: exit code 1, empty stdout, and stderr carrying the "govflow:"
// prefix, the exact user registry path and the file-vs-directory reason — with
// no success token anywhere in either stream. The wrapped child does not add a
// per-command prefix, so none is required.
func assertDirectoryChildFailure(t *testing.T, code int, stdout, stderr []byte, userPath string) {
	t.Helper()
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr:\n%s", code, stderr)
	}
	if len(bytes.TrimSpace(stdout)) != 0 {
		t.Fatalf("stdout must stay empty for the directory path, got %q", stdout)
	}
	msg := string(stderr)
	for _, want := range []string{
		"govflow:",
		userPath,
		"must be a file",
		"directory",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("stderr must contain %q:\n%s", want, msg)
		}
	}
	for _, banned := range []string{`"status":"created"`, `"status":"duplicate"`, `"results"`} {
		if strings.Contains(msg, banned) || bytes.Contains(stdout, []byte(banned)) {
			t.Errorf("no success payload %q may appear: stdout=%q stderr=%s", banned, stdout, msg)
		}
	}
}

// assertStrippedNameNotCreated verifies nothing appeared at the stripped file
// name or at the directory the marker descended into.
func assertStrippedNameNotCreated(t *testing.T, stripped, missingDir string) {
	t.Helper()
	if _, err := os.Lstat(stripped); !os.IsNotExist(err) {
		t.Fatalf("the rejected command must not create the stripped name %q, stat err=%v", stripped, err)
	}
	if missingDir != "" {
		if _, err := os.Lstat(missingDir); !os.IsNotExist(err) {
			t.Fatalf("the rejected command must not create the missing directory %q, stat err=%v", missingDir, err)
		}
	}
}

// batch-register of a legal NEW batch against every directory-marker shape,
// over a location that does not exist, fails the process contract and creates
// neither the stripped plain file nor the missing parent directory.
func TestBatchRegisterCLIDirectorySpellingMissingRejected(t *testing.T) {
	for _, tc := range []struct {
		name string
		rel  bool
	}{
		{"absolute", false},
		{"relative", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			storeAbs := filepath.Join(root, "store")
			if err := os.MkdirAll(storeAbs, 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.rel {
				t.Chdir(root)
			}
			store := storeAbs
			if tc.rel {
				store = "store"
			}
			for _, shape := range cliDirShapes(store) {
				t.Run(shape.name, func(t *testing.T) {
					var code int
					var stdout, stderr []byte
					if tc.rel {
						var buf bytes.Buffer
						err := runBatchRegister([]string{
							"--registry", shape.path,
							"--batch", "NEW", "--product", "P-9", "--quantity", "5", "--unit", "box",
						}, &buf)
						if err == nil {
							t.Fatal("registering against a directory-denoting relative path must fail")
						}
						assertDirectoryInProcessFailure(t, err, buf.Bytes(), shape.path, "batch-register")
						stdout = buf.Bytes()
					} else {
						code, stdout, stderr = runRegisterChild(t, shape.path, faultNone, "", false)
						assertDirectoryChildFailure(t, code, stdout, stderr, shape.path)
					}
					if len(bytes.TrimSpace(stdout)) != 0 {
						t.Fatalf("stdout must stay empty, got %q", stdout)
					}
					stripped := shape.stripped
					missing := shape.missingDir
					if tc.rel && missing != "" {
						missing = filepath.Join(root, missing)
					}
					assertStrippedNameNotCreated(t, stripped, missing)
					assertNoSaveLeftovers(t, root)
				})
			}
		})
	}
}

// assertDirectoryInProcessFailure is the in-process counterpart of
// assertDirectoryChildFailure: an error naming the exact path and the
// file-vs-directory reason, with nothing written to stdout.
func assertDirectoryInProcessFailure(t *testing.T, err error, stdout []byte, userPath, command string) {
	t.Helper()
	if err == nil {
		t.Fatal("the directory-denoting path must be rejected")
	}
	if len(stdout) != 0 {
		t.Fatalf("stdout must stay empty, got %q", stdout)
	}
	msg := err.Error()
	for _, want := range []string{userPath, "must be a file", "directory"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q must contain %q (command %s)", msg, want, command)
		}
	}
}

// When the location already IS a directory, both commands reject it and the
// directory survives; when a plain file sits directly beneath the spelled
// name, that file's bytes and pinned modification time are untouched.
func TestBatchCommandsCLIDirectorySpellingOverExistingFsObjects(t *testing.T) {
	const oldJSON = `{"version":1,"batches":[{"batch":"OLD","product":"P-1","quantity":10,"unit":"kg"}]}`

	t.Run("register over existing directory", func(t *testing.T) {
		root := t.TempDir()
		store := filepath.Join(root, "store")
		dir := filepath.Join(store, "dir")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, userPath := range []string{
			dir + string(os.PathSeparator),
			dir + string(os.PathSeparator) + ".",
			dir + string(os.PathSeparator) + "..",
		} {
			code, stdout, stderr := runRegisterChild(t, userPath, faultNone, "", false)
			assertDirectoryChildFailure(t, code, stdout, stderr, userPath)
			if info, err := os.Stat(dir); err != nil || !info.IsDir() {
				t.Fatalf("the existing directory must survive: info=%+v err=%v", info, err)
			}
			assertNoSaveLeftovers(t, root)
		}
	})

	t.Run("import over existing directory", func(t *testing.T) {
		root := t.TempDir()
		store := filepath.Join(root, "store")
		dir := filepath.Join(store, "dir")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		manifest := filepath.Join(root, "manifest.json")
		writeFile(t, manifest, `[{"batch":"NEW","product":"P-9","quantity":5,"unit":"box"}]`)
		userPath := dir + string(os.PathSeparator)

		code, stdout, stderr := runImportChild(t, userPath, manifest, faultNone, "", false)
		assertDirectoryChildFailure(t, code, stdout, stderr, userPath)
		assertNoSaveLeftovers(t, root, "manifest.json")
	})

	t.Run("register over regular file beneath the name", func(t *testing.T) {
		for _, shape := range []struct {
			name string
			file string
			path string
		}{
			{"trailing slash", "new.json", "new.json" + string(os.PathSeparator)},
			{"final dot", "new", "new" + string(os.PathSeparator) + "."},
			{"final dot-dot", "new", "new" + string(os.PathSeparator) + ".."},
		} {
			t.Run(shape.name, func(t *testing.T) {
				root := t.TempDir()
				store := filepath.Join(root, "store")
				if err := os.MkdirAll(store, 0o755); err != nil {
					t.Fatal(err)
				}
				existing := filepath.Join(store, shape.file)
				writeFile(t, existing, oldJSON)
				content, pinned := pinRegistry(t, existing)
				userPath := store + string(os.PathSeparator) + shape.path

				code, stdout, stderr := runRegisterChild(t, userPath, faultNone, "", true)
				assertDirectoryChildFailure(t, code, stdout, stderr, userPath)
				assertRegistryUntouched(t, existing, content, pinned)
				if got := readStoredBatches(t, existing); len(got) != 1 || got[0].Batch != "OLD" {
					t.Fatalf("the file beneath the name must keep exactly OLD, got %+v", got)
				}
				assertNoSaveLeftovers(t, root, shape.file)
			})
		}
	})

	t.Run("import all-duplicate manifest over directory still rejected", func(t *testing.T) {
		// Even when every manifest row would merely confirm a duplicate, a
		// directory-denoting registry path must fail before stdout can carry
		// a single "duplicate" result.
		root := t.TempDir()
		store := filepath.Join(root, "store")
		if err := os.MkdirAll(filepath.Join(store, "dir"), 0o755); err != nil {
			t.Fatal(err)
		}
		registry := filepath.Join(store, "batches.json")
		writeFile(t, registry, oldJSON)
		manifest := filepath.Join(root, "manifest.json")
		writeFile(t, manifest, `[{"batch":"OLD","product":"P-1","quantity":10,"unit":"kg"}]`)
		userPath := filepath.Join(store, "dir") + string(os.PathSeparator)

		code, stdout, stderr := runImportChild(t, userPath, manifest, faultNone, "", false)
		assertDirectoryChildFailure(t, code, stdout, stderr, userPath)
		if got := readStoredBatches(t, registry); len(got) != 1 || got[0].Batch != "OLD" {
			t.Fatalf("the real registry must keep exactly OLD, got %+v", got)
		}
		assertNoSaveLeftovers(t, root, "batches.json", "manifest.json")
	})
}

// Relative in-process spellings of the existing-directory and
// file-beneath shapes fail for both commands with the same contract.
func TestBatchCommandsCLIDirectorySpellingRelativeRejected(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "store")
	if err := os.MkdirAll(filepath.Join(store, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(store, "new.json")
	writeFile(t, existing, `{"version":1,"batches":[{"batch":"OLD","product":"P-1","quantity":10,"unit":"kg"}]}`)
	content, pinned := pinRegistry(t, existing)
	manifest := filepath.Join(root, "manifest.json")
	manifestJSON := `[{"batch":"NEW","product":"P-9","quantity":5,"unit":"box"}]`
	writeFile(t, manifest, manifestJSON)
	t.Chdir(root)

	paths := []string{
		filepath.Join("store", "dir") + string(os.PathSeparator),
		filepath.Join("store", "new.json") + string(os.PathSeparator),
		"store" + string(os.PathSeparator) + "new" + string(os.PathSeparator) + ".",
	}
	for _, userPath := range paths {
		t.Run("register/"+strings.ReplaceAll(userPath, string(os.PathSeparator), "_"), func(t *testing.T) {
			var stdout bytes.Buffer
			err := runBatchRegister([]string{
				"--registry", userPath,
				"--batch", "NEW", "--product", "P-9", "--quantity", "5", "--unit", "box",
			}, &stdout)
			assertDirectoryInProcessFailure(t, err, stdout.Bytes(), userPath, "batch-register")
			assertRegistryUntouched(t, existing, content, pinned)
		})
		t.Run("import/"+strings.ReplaceAll(userPath, string(os.PathSeparator), "_"), func(t *testing.T) {
			var stdout bytes.Buffer
			err := runBatchImport([]string{"--registry", userPath, "--input", manifest}, &stdout)
			assertDirectoryInProcessFailure(t, err, stdout.Bytes(), userPath, "batch-import")
			assertRegistryUntouched(t, existing, content, pinned)
			if after, _ := os.ReadFile(manifest); string(after) != manifestJSON {
				t.Fatal("the read-only manifest was modified")
			}
		})
	}
}

// Preserved normal behavior: a file path without a ".json" suffix, whose
// parent does not exist, still creates the directory and the registry and
// reports created — the new rule is specific to directory-marker spellings.
func TestBatchRegisterCLINormalFileWithoutSuffixStillCreated(t *testing.T) {
	root := t.TempDir()
	registry := filepath.Join(root, "store", "new", "registry-data")

	stdout, err := registerCLI(t, registry, "NEW", "P-9", "5", "box")
	if err != nil {
		t.Fatalf("a normal file path without a .json suffix must register: %v", err)
	}
	if got := decodeRegisterResult(t, stdout); got.Status != "created" || got.Batch != "NEW" {
		t.Fatalf("unexpected result: %+v", got)
	}
	if got := readStoredBatches(t, registry); len(got) != 1 || got[0].Batch != "NEW" {
		t.Fatalf("the new registry must hold NEW, got %+v", got)
	}
	assertNoSaveLeftovers(t, filepath.Join(root, "store"), "registry-data")
}
