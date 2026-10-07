package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// End-to-end regression for a --registry whose spelling denotes a directory
// rather than a file: one or more trailing separators ("store/new.json/") or a
// final component of "." / ".." ("store/new/.", "store/new/.."). Before the
// fix the command collapsed the tail lexically, created a regular file at the
// stripped name, and printed "created" even though the user could not read the
// registry through the path they had passed. The process contract under guard
// is shared by batch-register and batch-import:
//
//   - the command fails (in-process error, child exit code 1);
//   - the reason is written to stderr and quotes the registry path EXACTLY as
//     passed and states that the registry target must be a file while this
//     path denotes a directory;
//   - stdout stays empty — no "created", no "duplicate", no import results;
//   - nothing is created at the directory spelling: no missing parent
//     directory, no registry file at the stripped name, no temporary file;
//   - a regular file already sitting at the stripped name keeps its bytes and
//     pinned modification time, and an existing directory is not replaced;
//   - the read-only import manifest is never modified.
//
// Relative and absolute spellings behave identically. The rejection happens
// inside batchreg.Load (and is independently enforced by Save), so even an
// all-duplicate request cannot print "duplicate" through a directory spelling.

// explicitDirPath returns the raw directory spelling named by one of the
// shared shape labels, rooted at dir, without any lexical cleaning (Join/Clean
// would strip the trailing separator and erase the very shape under test).
func explicitDirPath(t *testing.T, dir, name string) string {
	t.Helper()
	sep := string(os.PathSeparator)
	jsonFile := filepath.Join(dir, "store", "new.json")
	newDir := filepath.Join(dir, "store", "new")
	switch name {
	case "trailing slash":
		return jsonFile + sep
	case "doubled slash":
		return jsonFile + sep + sep
	case "final dot":
		return newDir + sep + "."
	case "final dotdot":
		return newDir + sep + ".."
	case "dotdot with slash":
		return newDir + sep + ".." + sep
	default:
		t.Fatalf("unknown shape %q", name)
		return ""
	}
}

// assertDirPathCLIError checks the shared in-process error contract.
func assertDirPathCLIError(t *testing.T, err error, stdout *bytes.Buffer, userPath string) {
	t.Helper()
	if err == nil {
		t.Fatalf("a directory-denoting --registry %q must be rejected", userPath)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout must stay empty on the rejected path, got %q", stdout.String())
	}
	msg := err.Error()
	for _, want := range []string{userPath, "must be a file", "directory"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error must contain %q:\n%s", want, msg)
		}
	}
	for _, banned := range []string{"created", "duplicate", "results"} {
		if strings.Contains(msg, banned) {
			t.Errorf("the error must not report a success token %q:\n%s", banned, msg)
		}
	}
}

// assertDirPathChildFailure checks the real child-process contract: exit code
// 1, empty stdout, stderr carrying the govflow prefix, the exact registry path
// and the file/directory reason.
func assertDirPathChildFailure(t *testing.T, code int, stdout, stderr []byte, userPath string) {
	t.Helper()
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr:\n%s", code, stderr)
	}
	if len(bytes.TrimSpace(stdout)) != 0 {
		t.Fatalf("stdout must stay empty on the rejected path, got %q", stdout)
	}
	msg := string(stderr)
	for _, want := range []string{"govflow:", userPath, "must be a file", "directory"} {
		if !strings.Contains(msg, want) {
			t.Errorf("stderr must contain %q:\n%s", want, msg)
		}
	}
	for _, banned := range []string{`"status":"created"`, `"status":"duplicate"`} {
		if strings.Contains(msg, banned) {
			t.Errorf("stderr must not carry a success payload %q:\n%s", banned, msg)
		}
	}
}

// batch-register in-process, absent location through absent parents: every
// directory spelling fails and creates nothing at all.
func TestBatchRegisterCLIDirectoryDenotingRegistryRejected(t *testing.T) {
	for _, name := range []string{"trailing slash", "doubled slash", "final dot", "final dotdot", "dotdot with slash"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			userPath := explicitDirPath(t, dir, name)

			var stdout bytes.Buffer
			err := runBatchRegister([]string{
				"--registry", userPath,
				"--batch", "B1", "--product", "P-9", "--quantity", "5", "--unit", "box",
			}, &stdout)

			assertDirPathCLIError(t, err, &stdout, userPath)
			if _, statErr := os.Lstat(filepath.Join(dir, "store")); !os.IsNotExist(statErr) {
				t.Fatalf("the rejected registration must not create store/, stat err=%v", statErr)
			}
			assertNoSaveLeftovers(t, dir)
		})
	}
}

// When a regular file already sits at the stripped name, batch-register fails
// through the directory spelling and leaves the file byte- and mtime-identical
// — even when the request would otherwise be a duplicate confirmation.
func TestBatchRegisterCLIDirectoryDenotingPathPreservesExistingFile(t *testing.T) {
	cases := map[string]struct {
		existing string // existing file's path relative to the temp dir
		tail     string // directory spelling appended to the existing file path
	}{
		"trailing slash over registry file": {
			filepath.Join("store", "new.json"),
			string(os.PathSeparator),
		},
		"final dot over plain file": {
			filepath.Join("store", "new"),
			string(os.PathSeparator) + ".",
		},
		"final dotdot beside plain file": {
			filepath.Join("store", "new"),
			string(os.PathSeparator) + "..",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			registry := filepath.Join(dir, tc.existing)
			writeFile(t, registry, `{"version":1,"batches":[{"batch":"B1","product":"P-9","quantity":5,"unit":"box"}]}`)
			content, pinned := pinRegistry(t, registry)
			userPath := registry + tc.tail

			var stdout bytes.Buffer
			// B1 is already stored: without the guard this would print
			// "duplicate" after Load silently read the stripped file.
			err := runBatchRegister([]string{
				"--registry", userPath,
				"--batch", "B1", "--product", "P-9", "--quantity", "5", "--unit", "box",
			}, &stdout)

			assertDirPathCLIError(t, err, &stdout, userPath)
			assertRegistryUntouched(t, registry, content, pinned)
			if batches := readStoredBatches(t, registry); len(batches) != 1 || batches[0].Batch != "B1" {
				t.Fatalf("the existing file must keep exactly B1, got %+v", batches)
			}
			assertNoSaveLeftovers(t, dir, filepath.Base(registry))
		})
	}
}

// batch-import in-process: a NEW record in the manifest lands nowhere; the
// manifest stays read-only and no registry or directory appears.
func TestBatchImportCLIDirectoryDenotingRegistryRejectsWholeManifest(t *testing.T) {
	for _, name := range []string{"trailing slash", "final dot", "final dotdot"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			userPath := explicitDirPath(t, dir, name)
			manifest := filepath.Join(dir, "manifest.json")
			manifestJSON := `[{"batch":"NEW","product":"P-9","quantity":5,"unit":"box"}]`
			writeFile(t, manifest, manifestJSON)

			var stdout bytes.Buffer
			err := runBatchImport([]string{"--registry", userPath, "--input", manifest}, &stdout)

			assertDirPathCLIError(t, err, &stdout, userPath)
			if _, statErr := os.Lstat(filepath.Join(dir, "store")); !os.IsNotExist(statErr) {
				t.Fatalf("the rejected import must not create store/, stat err=%v", statErr)
			}
			assertNoSaveLeftovers(t, dir, "manifest.json")
			if after, _ := os.ReadFile(manifest); string(after) != manifestJSON {
				t.Fatal("the read-only manifest was modified")
			}
		})
	}
}

// The real process contract for batch-register: exit code 1, stderr naming the
// raw path and the reason, empty stdout, nothing created.
func TestBatchRegisterCLIChildDirectoryDenotingRegistryRejected(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"trailing slash", "final dot", "final dotdot"} {
		t.Run(name, func(t *testing.T) {
			userPath := explicitDirPath(t, dir, name)

			code, stdout, stderr := runRegisterChild(t, userPath, faultNone, "", false)

			assertDirPathChildFailure(t, code, stdout, stderr, userPath)
			if _, statErr := os.Lstat(filepath.Join(dir, "store")); !os.IsNotExist(statErr) {
				t.Fatalf("the rejected child must not create store/, stat err=%v", statErr)
			}
			assertNoSaveLeftovers(t, dir)
		})
	}
}

// The real process contract for batch-import.
func TestBatchImportCLIChildDirectoryDenotingRegistryRejected(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "manifest.json")
	manifestJSON := `[{"batch":"NEW","product":"P-9","quantity":5,"unit":"box"}]`
	writeFile(t, manifest, manifestJSON)
	for _, name := range []string{"trailing slash", "final dot", "final dotdot"} {
		t.Run(name, func(t *testing.T) {
			userPath := explicitDirPath(t, dir, name)

			code, stdout, stderr := runImportChild(t, userPath, manifest, faultNone, "", false)

			assertDirPathChildFailure(t, code, stdout, stderr, userPath)
			assertNoSaveLeftovers(t, dir, "manifest.json")
			if after, _ := os.ReadFile(manifest); string(after) != manifestJSON {
				t.Fatal("the read-only manifest was modified")
			}
		})
	}
}

// A relative --registry with a directory spelling is rejected in-process the
// same as the absolute one; nothing appears in the working directory.
func TestBatchCommandsCLIDirectoryDenotingPathRelativeRejected(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "manifest.json")
	manifestJSON := `[{"batch":"NEW","product":"P-9","quantity":5,"unit":"box"}]`
	writeFile(t, manifest, manifestJSON)
	t.Chdir(dir)
	sep := string(os.PathSeparator)
	relativeShapes := map[string]string{
		"trailing slash": "store" + sep + "new.json" + sep,
		"final dot":      "store" + sep + "new" + sep + ".",
		"final dotdot":   "store" + sep + "new" + sep + "..",
	}
	for name, userPath := range relativeShapes {
		t.Run(name, func(t *testing.T) {
			t.Run("register", func(t *testing.T) {
				var stdout bytes.Buffer
				err := runBatchRegister([]string{
					"--registry", userPath,
					"--batch", "NEW", "--product", "P-9", "--quantity", "5", "--unit", "box",
				}, &stdout)
				assertDirPathCLIError(t, err, &stdout, userPath)
			})
			t.Run("import", func(t *testing.T) {
				var stdout bytes.Buffer
				err := runBatchImport([]string{"--registry", userPath, "--input", manifest}, &stdout)
				assertDirPathCLIError(t, err, &stdout, userPath)
				if after, _ := os.ReadFile(manifest); string(after) != manifestJSON {
					t.Fatal("the read-only manifest was modified")
				}
			})
			if _, statErr := os.Lstat(filepath.Join(dir, "store")); !os.IsNotExist(statErr) {
				t.Fatalf("the rejected command must not create store/, stat err=%v", statErr)
			}
			assertNoSaveLeftovers(t, dir, "manifest.json")
		})
	}
}

// An existing directory reached through its own name plus a trailing slash or
// "/." must not be replaced or emptied: the command fails and the directory's
// contents survive.
func TestBatchCommandsCLIDirectoryDenotingPathAtExistingDirectory(t *testing.T) {
	dir := t.TempDir()
	registryDir := filepath.Join(dir, "store", "d")
	if err := os.MkdirAll(filepath.Join(registryDir, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(registryDir, "keep", "marker"), "x")
	manifest := filepath.Join(dir, "manifest.json")
	writeFile(t, manifest, `[{"batch":"NEW","product":"P-9","quantity":5,"unit":"box"}]`)
	sep := string(os.PathSeparator)

	for name, userPath := range map[string]string{
		"trailing slash": registryDir + sep,
		"final dot":      registryDir + sep + ".",
	} {
		t.Run(name, func(t *testing.T) {
			t.Run("register", func(t *testing.T) {
				var stdout bytes.Buffer
				err := runBatchRegister([]string{
					"--registry", userPath,
					"--batch", "NEW", "--product", "P-9", "--quantity", "5", "--unit", "box",
				}, &stdout)
				assertDirPathCLIError(t, err, &stdout, userPath)
			})
			t.Run("import", func(t *testing.T) {
				var stdout bytes.Buffer
				err := runBatchImport([]string{"--registry", userPath, "--input", manifest}, &stdout)
				assertDirPathCLIError(t, err, &stdout, userPath)
			})
			info, err := os.Lstat(registryDir)
			if err != nil || !info.IsDir() {
				t.Fatalf("the existing directory must survive, info=%v err=%v", info, err)
			}
			if _, err := os.ReadFile(filepath.Join(registryDir, "keep", "marker")); err != nil {
				t.Fatalf("directory contents must survive: %v", err)
			}
			assertNoSaveLeftovers(t, dir, "manifest.json", "marker")
		})
	}
}
