package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// End-to-end regression for a --registry that descends through a DIRECTORY
// symbolic link whose target cannot be resolved. The kernel reports an
// ordinary "no such file" for work/alias/batches.json and for
// work/alias/../batches.json when work/alias dangles, so batchreg.Load used
// to hand the command an empty first-registry result. Both commands must
// instead fail: an error naming the --registry path verbatim and the
// unresolvable directory link, no success JSON on stdout, nothing created
// anywhere, the link unchanged and any same-named file beside the link or in
// the target's parent directory left byte-for-byte and timestamp-for-timestamp.

// setupDanglingDirLinkCLIFixture builds
//
//	root/
//	  store/
//	    batches.json        (unrelated decoy, when withDecoys)
//	  work/
//	    batches.json        (unrelated decoy, when withDecoys)
//	    alias -> linkTarget (directory link with no existing target)
//
// and returns the raw user path work/alias/../batches.json when dotDot is set
// (assembled by concatenation so the link segment and ".." survive),
// otherwise work/alias/batches.json, plus the link path and target text.
func setupDanglingDirLinkCLIFixture(t *testing.T, linkTarget string, dotDot, withDecoys bool) (userPath, realDir, decoyWork, decoyStore, aliasPath string) {
	t.Helper()
	root := t.TempDir()
	workDir := filepath.Join(root, "work")
	storeDir := filepath.Join(root, "store")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	aliasPath = filepath.Join(workDir, "alias")
	if err := os.Symlink(linkTarget, aliasPath); err != nil {
		t.Fatal(err)
	}
	decoyWork = filepath.Join(workDir, "batches.json")
	decoyStore = filepath.Join(storeDir, "batches.json")
	if withDecoys {
		writeFile(t, decoyWork, `{"version":1,"batches":[{"batch":"WORK-DECOY","product":"P-0","quantity":1,"unit":"m"}]}`)
		writeFile(t, decoyStore, `{"version":1,"batches":[{"batch":"STORE-DECOY","product":"P-0","quantity":1,"unit":"m"}]}`)
	}
	if dotDot {
		userPath = aliasPath + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "batches.json"
	} else {
		userPath = filepath.Join(aliasPath, "batches.json")
	}
	return userPath, root, decoyWork, decoyStore, aliasPath
}

// assertDanglingDirLinkCommandRejected checks the shared command contract:
// failure naming the user path and the symbolic link, empty stdout, the link
// intact and the missing target still absent.
func assertDanglingDirLinkCommandRejected(t *testing.T, err error, stdout *bytes.Buffer, userPath, aliasPath, linkTarget, root string) {
	t.Helper()
	if err == nil {
		t.Fatalf("the command must reject a registry through an unresolvable directory link %q", userPath)
	}
	msg := err.Error()
	if !strings.Contains(msg, userPath) {
		t.Errorf("error %q must name the --registry path the user passed %q", msg, userPath)
	}
	if !strings.Contains(msg, "symbolic link") {
		t.Errorf("error %q must identify the symbolic link", msg)
	}
	if !strings.Contains(msg, "cannot be resolved") {
		t.Errorf("error %q must state the link target cannot be resolved", msg)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout must carry no success result, got %q", stdout.String())
	}
	assertStillSymlink(t, aliasPath, linkTarget)
	// No missing target directory or registry may materialize. The link text
	// is relative to work in every case here.
	missing := filepath.Join(root, "store", "missing")
	if _, statErr := os.Lstat(missing); !os.IsNotExist(statErr) {
		t.Fatalf("the missing link target must not be created, stat err=%v", statErr)
	}
	if _, statErr := os.Lstat(filepath.Join(missing, "batches.json")); !os.IsNotExist(statErr) {
		t.Fatalf("no registry may be created under the missing target, stat err=%v", statErr)
	}
	if entries, err := os.ReadDir(filepath.Dir(aliasPath)); err != nil {
		t.Fatal(err)
	} else {
		for _, e := range entries {
			name := e.Name()
			if name != "alias" && name != "batches.json" {
				t.Fatalf("the rejected command created %q next to the link", name)
			}
		}
	}
}

// Both commands reject a plain path through a dangling directory link.
func TestBatchCommandsRejectDanglingDirectorySymlink(t *testing.T) {
	for _, command := range []string{"batch-register", "batch-import"} {
		t.Run(command, func(t *testing.T) {
			linkTarget := filepath.Join("..", "store", "missing")
			userPath, root, _, _, aliasPath := setupDanglingDirLinkCLIFixture(t, linkTarget, false, false)
			manifestPath := filepath.Join(root, "in.json")
			writeFile(t, manifestPath, `[{"batch":"B1","product":"P-8","quantity":1,"unit":"box"}]`)

			var stdout bytes.Buffer
			var err error
			if command == "batch-register" {
				err = runBatchRegister([]string{
					"--registry", userPath,
					"--batch", "B1", "--product", "P-8", "--quantity", "1", "--unit", "box",
				}, &stdout)
			} else {
				err = runBatchImport([]string{"--registry", userPath, "--input", manifestPath}, &stdout)
			}
			assertDanglingDirLinkCommandRejected(t, err, &stdout, userPath, aliasPath, linkTarget, root)
		})
	}
}

// A ".." after the dangling directory link must not let either command read
// or write another same-named file: the work/ and store/ decoy registries
// survive byte-for-byte and timestamp-for-timestamp and the command fails.
func TestBatchCommandsRejectDanglingDirectorySymlinkDotDot(t *testing.T) {
	for _, command := range []string{"batch-register", "batch-import"} {
		t.Run(command, func(t *testing.T) {
			linkTarget := filepath.Join("..", "store", "missing")
			userPath, root, decoyWork, decoyStore, aliasPath := setupDanglingDirLinkCLIFixture(t, linkTarget, true, true)
			manifestPath := filepath.Join(root, "in.json")
			writeFile(t, manifestPath, `[{"batch":"B1","product":"P-8","quantity":1,"unit":"box"}]`)
			workBytes, workPinned := pinRegistry(t, decoyWork)
			storeBytes, storePinned := pinRegistry(t, decoyStore)

			var stdout bytes.Buffer
			var err error
			if command == "batch-register" {
				err = runBatchRegister([]string{
					"--registry", userPath,
					"--batch", "B1", "--product", "P-8", "--quantity", "1", "--unit", "box",
				}, &stdout)
			} else {
				err = runBatchImport([]string{"--registry", userPath, "--input", manifestPath}, &stdout)
			}
			assertDanglingDirLinkCommandRejected(t, err, &stdout, userPath, aliasPath, linkTarget, root)
			assertRegistryUntouched(t, decoyWork, workBytes, workPinned)
			assertRegistryUntouched(t, decoyStore, storeBytes, storePinned)
			// The decoys must not gain the new batch.
			for _, p := range []string{decoyWork, decoyStore} {
				if ids := batchIDs(t, p); len(ids) != 1 {
					t.Fatalf("decoy %s must keep its single record, got %v", p, ids)
				}
			}
		})
	}
}

// A chain of directory links ending in a missing target is rejected by both
// commands just like a single dangling link.
func TestBatchCommandsRejectDanglingDirectorySymlinkChain(t *testing.T) {
	for _, command := range []string{"batch-register", "batch-import"} {
		t.Run(command, func(t *testing.T) {
			root := t.TempDir()
			workDir := filepath.Join(root, "work")
			if err := os.MkdirAll(workDir, 0o755); err != nil {
				t.Fatal(err)
			}
			link1 := filepath.Join(workDir, "c1")
			link2 := filepath.Join(workDir, "c2")
			if err := os.Symlink("c2", link1); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("gone", link2); err != nil {
				t.Fatal(err)
			}
			userPath := filepath.Join(link1, "batches.json")
			manifestPath := filepath.Join(root, "in.json")
			writeFile(t, manifestPath, `[{"batch":"B1","product":"P-8","quantity":1,"unit":"box"}]`)

			var stdout bytes.Buffer
			var err error
			if command == "batch-register" {
				err = runBatchRegister([]string{
					"--registry", userPath,
					"--batch", "B1", "--product", "P-8", "--quantity", "1", "--unit", "box",
				}, &stdout)
			} else {
				err = runBatchImport([]string{"--registry", userPath, "--input", manifestPath}, &stdout)
			}
			if err == nil {
				t.Fatalf("%s must reject a registry through a dangling link chain", command)
			}
			if !strings.Contains(err.Error(), userPath) {
				t.Errorf("error %q must name the --registry path %q", err.Error(), userPath)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout must carry no success result, got %q", stdout.String())
			}
			assertStillSymlink(t, link1, "c2")
			assertStillSymlink(t, link2, "gone")
		})
	}
}
