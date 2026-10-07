package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// End-to-end regression for a --registry that reaches the file through a
// DIRECTORY symbolic link whose target directory does not exist. With
// work/alias -> ../store/missing-child and that target absent, neither
// work/alias/batches.json nor work/alias/../batches.json names a file the
// kernel can open; the latter must not collapse onto the valid
// work/batches.json beside the link. Load must reject the path before
// registration proceeds, so both commands fail: the error names the exact
// user-supplied registry path and the unresolvable symbolic link (it does
// not announce a merely not-yet-created registry), stdout carries no success
// result, the link is untouched, the missing target is not created, and an
// existing same-named file keeps its bytes and modification time.

// danglingDirLinkCLIFixture builds root/work/alias -> linkTarget with the
// target missing and returns the link directory and path.
func danglingDirLinkCLIFixture(t *testing.T, root, linkTarget string) (workDir, aliasPath string) {
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

// assertDanglingDirLinkCommandFailure checks the process-visible contract
// shared by both commands: a non-nil error quoting the exact registry path
// and naming the symbolic link, no success payload on stdout, the link
// intact and nothing created at the missing target.
func assertDanglingDirLinkCommandFailure(t *testing.T, err error, stdout *bytes.Buffer, userPath, aliasPath, linkTarget, root string, allowedFiles ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("a --registry through a dangling directory link must reject the command")
	}
	msg := err.Error()
	if !strings.Contains(msg, userPath) {
		t.Errorf("error %q must name the user-supplied registry path %q", msg, userPath)
	}
	if !strings.Contains(msg, "symbolic link") {
		t.Errorf("error %q must state the path runs into an unusable symbolic link", msg)
	}
	if strings.Contains(msg, "has not been created") {
		t.Errorf("error %q must not pretend the registry file has merely not been created", msg)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout must carry no success result, got %q", stdout.String())
	}
	assertStillSymlink(t, aliasPath, linkTarget)
	if _, statErr := os.Lstat(filepath.Join(root, "store")); !os.IsNotExist(statErr) {
		t.Errorf("the missing link target must not be created, stat err=%v", statErr)
	}
	// The directory link itself is not a leftover save artifact.
	allowed := append([]string{"alias"}, allowedFiles...)
	assertNoSaveLeftovers(t, root, allowed...)
}

// Both commands reject a registry reached straight through a dangling
// directory link, for relative and absolute link targets alike.
func TestBatchCommandsRejectDanglingDirectorySymlink(t *testing.T) {
	manifestContent := `[{"batch":"B1","product":"P","quantity":1,"unit":"kg"}]`
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
			for _, command := range []string{"batch-register", "batch-import"} {
				t.Run(command, func(t *testing.T) {
					root := t.TempDir()
					linkTarget := tc.linkTarget(root)
					workDir, aliasPath := danglingDirLinkCLIFixture(t, root, linkTarget)
					manifestPath := filepath.Join(root, "in.json")
					writeFile(t, manifestPath, manifestContent)
					userPath := filepath.Join(aliasPath, "batches.json")

					var stdout bytes.Buffer
					var err error
					if command == "batch-register" {
						err = runBatchRegister([]string{
							"--registry", userPath,
							"--batch", "B1", "--product", "P", "--quantity", "1", "--unit", "kg",
						}, &stdout)
					} else {
						err = runBatchImport([]string{"--registry", userPath, "--input", manifestPath}, &stdout)
					}
					assertDanglingDirLinkCommandFailure(t, err, &stdout, userPath, aliasPath, linkTarget, root, "in.json")
					// The read-only manifest survives untouched.
					if after, readErr := os.ReadFile(manifestPath); readErr != nil || string(after) != manifestContent {
						t.Fatalf("the input manifest must survive unchanged: %s (err=%v)", after, readErr)
					}
					if entries, readErr := os.ReadDir(workDir); readErr != nil {
						t.Fatal(readErr)
					} else if len(entries) != 1 || entries[0].Name() != "alias" {
						t.Fatalf("the command created entries next to the link: %v", entries)
					}
				})
			}
		})
	}
}

// A ".." behind the dangling directory link cannot redirect either command
// to the valid same-named registry beside the link: work/alias/../batches.json
// fails while work/batches.json keeps its bytes, mtime and DECOY record.
func TestBatchCommandsRejectDanglingDirectorySymlinkWithDotDot(t *testing.T) {
	const decoy = `{"version":1,"batches":[{"batch":"DECOY","product":"P-0","quantity":1,"unit":"m"}]}`
	for _, command := range []string{"batch-register", "batch-import"} {
		t.Run(command, func(t *testing.T) {
			root := t.TempDir()
			linkTarget := filepath.Join("..", "store", "missing-child")
			workDir, aliasPath := danglingDirLinkCLIFixture(t, root, linkTarget)
			decoyPath := filepath.Join(workDir, "batches.json")
			writeFile(t, decoyPath, decoy)
			decoyBytes, decoyPinned := pinRegistry(t, decoyPath)
			manifestPath := filepath.Join(root, "in.json")
			manifestContent := `[{"batch":"B1","product":"P","quantity":1,"unit":"kg"}]`
			writeFile(t, manifestPath, manifestContent)
			userPath := aliasPath + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "batches.json"

			var stdout bytes.Buffer
			var err error
			if command == "batch-register" {
				err = runBatchRegister([]string{
					"--registry", userPath,
					"--batch", "B1", "--product", "P", "--quantity", "1", "--unit", "kg",
				}, &stdout)
			} else {
				err = runBatchImport([]string{"--registry", userPath, "--input", manifestPath}, &stdout)
			}
			assertDanglingDirLinkCommandFailure(t, err, &stdout, userPath, aliasPath, linkTarget, root, "in.json", "batches.json")
			assertRegistryUntouched(t, decoyPath, decoyBytes, decoyPinned)
			if got := batchIDs(t, decoyPath); len(got) != 1 || got[0] != "DECOY" {
				t.Fatalf("the lexical-resolution decoy must keep its record, got %v", got)
			}
			if after, readErr := os.ReadFile(manifestPath); readErr != nil || string(after) != manifestContent {
				t.Fatalf("the input manifest must survive unchanged: %s (err=%v)", after, readErr)
			}
		})
	}
}

// A relative --registry through the dangling link is rejected identically to
// the absolute spelling, and the error carries the relative spelling.
func TestBatchCommandsRejectDanglingDirectorySymlinkRelativeRegistry(t *testing.T) {
	for _, command := range []string{"batch-register", "batch-import"} {
		t.Run(command, func(t *testing.T) {
			root := t.TempDir()
			linkTarget := filepath.Join("..", "store", "missing-child")
			_, aliasPath := danglingDirLinkCLIFixture(t, root, linkTarget)
			manifestPath := filepath.Join(root, "in.json")
			manifestContent := `[{"batch":"B1","product":"P","quantity":1,"unit":"kg"}]`
			writeFile(t, manifestPath, manifestContent)
			t.Chdir(root)
			userPath := filepath.Join("work", "alias", "batches.json")

			var stdout bytes.Buffer
			var err error
			if command == "batch-register" {
				err = runBatchRegister([]string{
					"--registry", userPath,
					"--batch", "B1", "--product", "P", "--quantity", "1", "--unit", "kg",
				}, &stdout)
			} else {
				err = runBatchImport([]string{"--registry", userPath, "--input", manifestPath}, &stdout)
			}
			assertDanglingDirLinkCommandFailure(t, err, &stdout, userPath, aliasPath, linkTarget, root, "in.json")
		})
	}
}

// A valid directory link into a real directory where the registry simply does
// not exist yet remains an ordinary first registration for both commands —
// the new rejection must not fire on resolvable links.
func TestBatchCommandsFirstRegistrationThroughValidDirectorySymlinkStillCreated(t *testing.T) {
	for _, command := range []string{"batch-register", "batch-import"} {
		t.Run(command, func(t *testing.T) {
			root := t.TempDir()
			realDir := filepath.Join(root, "store", "child")
			if err := os.MkdirAll(realDir, 0o755); err != nil {
				t.Fatal(err)
			}
			workDir := filepath.Join(root, "work")
			if err := os.MkdirAll(workDir, 0o755); err != nil {
				t.Fatal(err)
			}
			aliasPath := filepath.Join(workDir, "alias")
			linkTarget := filepath.Join("..", "store", "child")
			if err := os.Symlink(linkTarget, aliasPath); err != nil {
				t.Fatal(err)
			}
			userPath := filepath.Join(aliasPath, "batches.json")
			realRegistry := filepath.Join(realDir, "batches.json")
			manifestPath := filepath.Join(root, "in.json")
			writeFile(t, manifestPath, `[{"batch":"B1","product":"P","quantity":1,"unit":"kg"}]`)

			var stdout bytes.Buffer
			var err error
			if command == "batch-register" {
				err = runBatchRegister([]string{
					"--registry", userPath,
					"--batch", "B1", "--product", "P", "--quantity", "1", "--unit", "kg",
				}, &stdout)
			} else {
				err = runBatchImport([]string{"--registry", userPath, "--input", manifestPath}, &stdout)
			}
			if err != nil {
				t.Fatalf("a first registration through a valid directory link must succeed: %v", err)
			}
			if stdout.Len() == 0 {
				t.Fatal("stdout must carry the normal success result")
			}
			if got := batchIDs(t, realRegistry); len(got) != 1 || got[0] != "B1" {
				t.Fatalf("the registry must be created in the real directory with B1, got %v", got)
			}
			if got := batchIDs(t, userPath); len(got) != 1 || got[0] != "B1" {
				t.Fatalf("the new registry must read back through the link, got %v", got)
			}
			assertStillSymlink(t, aliasPath, linkTarget)
		})
	}
}
