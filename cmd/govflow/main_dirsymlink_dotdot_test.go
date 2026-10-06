package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// End-to-end regression for a --registry that reaches the file through a
// DIRECTORY symbolic link followed by "..": work/alias -> store/child makes
// work/alias/../batches.json name store/batches.json. Both commands must read
// the real registry and save back to that same real file, appending new
// batches after the existing ones, even though the directory holding the link
// is not writable. An unrelated work/batches.json and the link itself must be
// left exactly as they were; nothing (file, temporary file or directory) may
// be created under work.

// setupDirLinkCLIFixture builds the store/work layout: a real registry with
// one record, a decoy same-named registry beside the link, and the directory
// link. It returns the raw user path work/alias/../batches.json (assembled by
// concatenation so the link segment and ".." survive lexical cleaning), the
// real and decoy registry paths, the link path and the link target text.
func setupDirLinkCLIFixture(t *testing.T, realContent string) (userPath, realPath, decoyPath, aliasPath, aliasTarget string) {
	t.Helper()
	root := t.TempDir()
	storeDir := filepath.Join(root, "store")
	if err := os.MkdirAll(filepath.Join(storeDir, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Join(root, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	realPath = filepath.Join(storeDir, "batches.json")
	writeFile(t, realPath, realContent)
	decoyPath = filepath.Join(workDir, "batches.json")
	writeFile(t, decoyPath, `{"version":1,"batches":[{"batch":"DECOY","product":"P-0","quantity":1,"unit":"m"}]}`)
	aliasTarget = filepath.Join("..", "store", "child")
	aliasPath = filepath.Join(workDir, "alias")
	if err := os.Symlink(aliasTarget, aliasPath); err != nil {
		t.Fatal(err)
	}
	userPath = aliasPath + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "batches.json"
	return userPath, realPath, decoyPath, aliasPath, aliasTarget
}

// assertOnlyLinkAndDecoy fails if anything besides the directory link and the
// pre-existing decoy exists in the link's own directory.
func assertOnlyLinkAndDecoy(t *testing.T, linkDir string) {
	t.Helper()
	entries, err := os.ReadDir(linkDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "alias" && e.Name() != "batches.json" {
			t.Fatalf("the command created %q next to the directory link", e.Name())
		}
	}
}

// batchIDs reads the batch ids of a registry in stored order.
func batchIDs(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored struct {
		Batches []struct {
			Batch string `json:"batch"`
		} `json:"batches"`
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatalf("registry %s is not valid JSON: %v (%s)", path, err, data)
	}
	ids := make([]string, len(stored.Batches))
	for i, b := range stored.Batches {
		ids[i] = b.Batch
	}
	return ids
}

// batch-register through work/alias/../batches.json appends the new batch to
// the real store/batches.json: stdout is the normal "created" JSON, the old
// record keeps its place, the real file and the user path read identically,
// the decoy and the link are untouched and nothing appears under work.
func TestBatchRegisterThroughDirectorySymlinkDotDotSavesRealFile(t *testing.T) {
	userPath, realPath, decoyPath, aliasPath, aliasTarget := setupDirLinkCLIFixture(t,
		`{"version":1,"batches":[{"batch":"OLD","product":"P-1","quantity":10,"unit":"kg"}]}`)
	decoy, pinned := pinRegistry(t, decoyPath)

	var stdout bytes.Buffer
	if err := runBatchRegister([]string{
		"--registry", userPath,
		"--batch", "NEW-1", "--product", "P-9", "--quantity", "5", "--unit", "box",
	}, &stdout); err != nil {
		t.Fatalf("registering through the directory link must succeed: %v", err)
	}
	var out struct {
		Batch  string `json:"batch"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &out); err != nil {
		t.Fatalf("stdout is not the expected JSON: %v (%q)", err, stdout.String())
	}
	if out.Batch != "NEW-1" || out.Status != "created" {
		t.Fatalf("stdout = %+v, want NEW-1/created", out)
	}

	want := []string{"OLD", "NEW-1"}
	if got := batchIDs(t, realPath); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("real registry = %v, want %v (old order kept, new appended)", got, want)
	}
	// Reading through the user-given path must show exactly the real file.
	if got := batchIDs(t, userPath); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("registry read through the user path = %v, want %v", got, want)
	}
	assertRegistryUntouched(t, decoyPath, decoy, pinned)
	assertStillSymlink(t, aliasPath, aliasTarget)
	assertOnlyLinkAndDecoy(t, filepath.Dir(aliasPath))
}

// batch-import through the directory link saves every new batch to the real
// registry and reports statuses in manifest order exactly as for a direct
// path; the decoy and link are untouched.
func TestBatchImportThroughDirectorySymlinkDotDotSavesRealFile(t *testing.T) {
	userPath, realPath, decoyPath, aliasPath, aliasTarget := setupDirLinkCLIFixture(t,
		`{"version":1,"batches":[{"batch":"B1","product":"P-7","quantity":120,"unit":"kg"}]}`)
	manifest := filepath.Join(filepath.Dir(filepath.Dir(aliasPath)), "in.json")
	writeFile(t, manifest, `[
  {"batch": "B1", "product": "P-7", "quantity": 120, "unit": "kg"},
  {"batch": "B2", "product": "P-8", "quantity": 1, "unit": "box"}
]`)
	decoy, pinned := pinRegistry(t, decoyPath)

	var stdout bytes.Buffer
	if err := runBatchImport([]string{"--registry", userPath, "--input", manifest}, &stdout); err != nil {
		t.Fatalf("importing through the directory link must succeed: %v", err)
	}
	var out importOutput
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &out); err != nil {
		t.Fatalf("stdout is not the expected JSON: %v (%q)", err, stdout.String())
	}
	if len(out.Results) != 2 ||
		out.Results[0].Batch != "B1" || out.Results[0].Status != "duplicate" ||
		out.Results[1].Batch != "B2" || out.Results[1].Status != "created" {
		t.Fatalf("results = %+v, want B1 duplicate then B2 created in manifest order", out.Results)
	}
	if got := batchIDs(t, realPath); strings.Join(got, ",") != "B1,B2" {
		t.Fatalf("real registry = %v, want B1 then B2", got)
	}
	if got := batchIDs(t, userPath); strings.Join(got, ",") != "B1,B2" {
		t.Fatalf("user-path registry = %v, want B1 then B2", got)
	}
	assertRegistryUntouched(t, decoyPath, decoy, pinned)
	assertStillSymlink(t, aliasPath, aliasTarget)
	assertOnlyLinkAndDecoy(t, filepath.Dir(aliasPath))
}

// Making work (the directory holding the link) read-and-execute-only must not
// block either command: the save prepares its temporary file in the real
// target directory, so write access beside the link is never needed. Skipped
// under root, which bypasses directory permission bits.
func TestBatchCommandsThroughDirectorySymlinkDotDotNeedNoWriteInLinkDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permission bits")
	}
	for _, command := range []string{"batch-register", "batch-import"} {
		t.Run(command, func(t *testing.T) {
			userPath, realPath, _, aliasPath, aliasTarget := setupDirLinkCLIFixture(t,
				`{"version":1,"batches":[{"batch":"B1","product":"P-7","quantity":120,"unit":"kg"}]}`)
			workDir := filepath.Dir(aliasPath)
			manifest := filepath.Join(filepath.Dir(workDir), "in.json")
			writeFile(t, manifest, `[{"batch":"B2","product":"P-8","quantity":1,"unit":"box"}]`)
			if err := os.Chmod(workDir, 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(workDir, 0o755) })

			var stdout bytes.Buffer
			var err error
			if command == "batch-register" {
				err = runBatchRegister([]string{
					"--registry", userPath,
					"--batch", "B2", "--product", "P-8", "--quantity", "1", "--unit", "box",
				}, &stdout)
			} else {
				err = runBatchImport([]string{"--registry", userPath, "--input", manifest}, &stdout)
			}
			if err != nil {
				t.Fatalf("a read-only link directory must not block %s: %v", command, err)
			}
			if stdout.Len() == 0 {
				t.Fatal("stdout must carry the normal success JSON")
			}
			if got := batchIDs(t, realPath); strings.Join(got, ",") != "B1,B2" {
				t.Fatalf("real registry = %v, want B1 then B2", got)
			}
			if got := batchIDs(t, userPath); strings.Join(got, ",") != "B1,B2" {
				t.Fatalf("user-path registry = %v, want B1 then B2", got)
			}
			assertStillSymlink(t, aliasPath, aliasTarget)
			assertOnlyLinkAndDecoy(t, workDir)
		})
	}
}

// When the REAL target directory is not writable, both commands must reject
// the operation like any save failure: an error naming the user-supplied
// registry path and the reason, the real registry left byte-for-byte and
// timestamp-for-timestamp, the decoy and link untouched and no file created
// under work. Skipped under root, which bypasses directory permission bits.
func TestBatchCommandsThroughDirectorySymlinkDotDotFailWhenRealDirectoryUnwritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permission bits")
	}
	for _, command := range []string{"batch-register", "batch-import"} {
		t.Run(command, func(t *testing.T) {
			userPath, realPath, decoyPath, aliasPath, aliasTarget := setupDirLinkCLIFixture(t,
				`{"version":1,"batches":[{"batch":"B1","product":"P-7","quantity":120,"unit":"kg"}]}`)
			root := filepath.Dir(filepath.Dir(aliasPath))
			storeDir := filepath.Join(root, "store")
			manifest := filepath.Join(root, "in.json")
			writeFile(t, manifest, `[{"batch":"B2","product":"P-8","quantity":1,"unit":"box"}]`)
			realBytes, realPinned := pinRegistry(t, realPath)
			decoyBytes, decoyPinned := pinRegistry(t, decoyPath)
			if err := os.Chmod(storeDir, 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(storeDir, 0o755) })

			var stdout bytes.Buffer
			var err error
			if command == "batch-register" {
				err = runBatchRegister([]string{
					"--registry", userPath,
					"--batch", "B2", "--product", "P-8", "--quantity", "1", "--unit", "box",
				}, &stdout)
			} else {
				err = runBatchImport([]string{"--registry", userPath, "--input", manifest}, &stdout)
			}
			if err == nil {
				t.Fatalf("%s must fail when the real target directory is not writable", command)
			}
			msg := err.Error()
			if !strings.Contains(msg, userPath) {
				t.Errorf("error %q must name the user-supplied registry path %q", msg, userPath)
			}
			if !strings.Contains(msg, "permission denied") {
				t.Errorf("error %q must state the concrete reason", msg)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout must carry no success result, got %q", stdout.String())
			}
			assertRegistryUntouched(t, realPath, realBytes, realPinned)
			assertRegistryUntouched(t, decoyPath, decoyBytes, decoyPinned)
			if got := batchIDs(t, realPath); strings.Join(got, ",") != "B1" {
				t.Fatalf("the rejected operation must not add a batch, got %v", got)
			}
			assertStillSymlink(t, aliasPath, aliasTarget)
			assertOnlyLinkAndDecoy(t, filepath.Dir(aliasPath))
		})
	}
}
