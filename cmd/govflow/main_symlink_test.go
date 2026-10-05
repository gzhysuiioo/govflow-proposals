package main

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file guards the two batch commands when --registry names a symbolic
// link. A registration or import through the link must land on the registry
// the link names — visible identically from the link path and the real path
// — while the link itself stays a link with its original target spelling. A
// duplicate-only run touches nothing, and a link whose target does not exist
// (or loops) is refused with an error naming the path the user passed: it is
// never treated as a first registration.

// assertSymlinkIntact fails unless path is still a symlink carrying exactly
// the target spelling it was created with.
func assertSymlinkIntact(t *testing.T, path, wantTarget string) {
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

// batch-register through a link appends to the linked registry: the new
// batch is visible from both the link path and the real path, the stored
// records keep their order, and the link survives unchanged.
func TestBatchRegisterCLIThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real", "reg.json")
	writeFile(t, target, `{"version":1,"batches":[{"batch":"B-1","product":"P-7","quantity":120,"unit":"kg"}]}`)
	link := filepath.Join(dir, "reg-link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	stdout, err := registerCLI(t, link, "B-2", "P-8", "3", "box")
	if err != nil {
		t.Fatalf("register through the link must succeed: %v", err)
	}
	res := decodeRegisterResult(t, stdout)
	if res.Status != "created" || res.Batch != "B-2" {
		t.Fatalf("unexpected result: %+v", res)
	}

	assertSymlinkIntact(t, link, target)
	want := []registerResult{
		{Batch: "B-1", Product: "P-7", Quantity: 120, Unit: "kg"},
		{Batch: "B-2", Product: "P-8", Quantity: 3, Unit: "box"},
	}
	for _, path := range []string{link, target} {
		got := readStoredBatches(t, path)
		if len(got) != len(want) {
			t.Fatalf("registry read via %s holds %d records, want %d", path, len(got), len(want))
		}
		for i := range want {
			got[i].Status = ""
			if got[i] != want[i] {
				t.Errorf("record %d via %s = %+v, want %+v", i+1, path, got[i], want[i])
			}
		}
	}
}

// Re-registering the identical batch through a link confirms the duplicate
// and writes nothing: the target keeps its exact bytes and mtime and the
// link is untouched.
func TestBatchRegisterCLIDuplicateThroughSymlinkKeepsEverything(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "reg.json")
	writeFile(t, target, `{"version":1,"batches":[{"batch":"B-1","product":"P-7","quantity":120,"unit":"kg"}]}`)
	content, pinned := pinRegistry(t, target)
	link := filepath.Join(dir, "reg-link.json")
	if err := os.Symlink("reg.json", link); err != nil {
		t.Fatal(err)
	}

	stdout, err := registerCLI(t, link, "B-1", "P-7", "120", "kg")
	if err != nil {
		t.Fatalf("duplicate confirmation through the link must succeed: %v", err)
	}
	if res := decodeRegisterResult(t, stdout); res.Status != "duplicate" {
		t.Fatalf("status = %q, want duplicate", res.Status)
	}

	assertRegistryUntouched(t, target, content, pinned)
	assertSymlinkIntact(t, link, "reg.json")
}

// batch-import through a link with a relative target appends every new
// manifest record to the linked registry in first-occurrence order and
// reports the usual per-record statuses; the link keeps its spelling.
func TestBatchImportCLIThroughRelativeSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "store", "reg.json")
	writeFile(t, target, `{"version":1,"batches":[{"batch":"B1","product":"P","quantity":1,"unit":"kg"}]}`)
	link := filepath.Join(dir, "links", "reg.json")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	relTarget := filepath.Join("..", "store", "reg.json")
	if err := os.Symlink(relTarget, link); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(dir, "in.json")
	manifestBytes := []byte(`[
  {"batch": "B1", "product": "P", "quantity": 1, "unit": "kg"},
  {"batch": "B2", "product": "P-8", "quantity": 2, "unit": "box"},
  {"batch": "B3", "product": "P-9", "quantity": 3, "unit": "m"}
]`)
	writeFile(t, manifest, string(manifestBytes))

	var stdout bytes.Buffer
	if err := runBatchImport([]string{"--registry", link, "--input", manifest}, &stdout); err != nil {
		t.Fatalf("import through the link must succeed: %v", err)
	}
	var out importOutput
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &out); err != nil {
		t.Fatalf("stdout is not the expected JSON: %v (%q)", err, stdout.String())
	}
	wantStatus := []string{"duplicate", "created", "created"}
	if len(out.Results) != len(wantStatus) {
		t.Fatalf("got %d results", len(out.Results))
	}
	for i, want := range wantStatus {
		if out.Results[i].Status != want {
			t.Errorf("result %d status = %q, want %q", i+1, out.Results[i].Status, want)
		}
	}

	assertSymlinkIntact(t, link, relTarget)
	stored := readStoredBatches(t, target)
	wantBatches := []string{"B1", "B2", "B3"}
	if len(stored) != len(wantBatches) {
		t.Fatalf("registry holds %d records, want %d", len(stored), len(wantBatches))
	}
	for i, want := range wantBatches {
		if stored[i].Batch != want {
			t.Errorf("record %d batch = %q, want %q", i+1, stored[i].Batch, want)
		}
	}
	// The manifest is read-only: its bytes survive the import unchanged.
	after, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, manifestBytes) {
		t.Fatal("the import rewrote the read-only manifest")
	}
}

// A link whose target does not exist refuses both commands: the error names
// the registry path the user passed, stdout carries no success result, the
// missing target is never created and the link is never replaced.
func TestBatchCommandsRejectDanglingSymlink(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "reg-link.json")
	relTarget := filepath.Join("missing", "reg.json")
	if err := os.Symlink(relTarget, link); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(dir, "in.json")
	writeFile(t, manifest, `[{"batch":"B1","product":"P","quantity":1,"unit":"kg"}]`)

	stdout, err := registerCLI(t, link, "B1", "P", "1", "kg")
	if err == nil {
		t.Fatal("batch-register must reject a link whose target does not exist")
	}
	if !strings.Contains(err.Error(), link) {
		t.Errorf("register error %q must name the registry path %q", err.Error(), link)
	}
	if stdout != "" {
		t.Errorf("stdout must carry no success result, got %q", stdout)
	}

	var importStdout bytes.Buffer
	err = runBatchImport([]string{"--registry", link, "--input", manifest}, &importStdout)
	if err == nil {
		t.Fatal("batch-import must reject a link whose target does not exist")
	}
	if !strings.Contains(err.Error(), link) {
		t.Errorf("import error %q must name the registry path %q", err.Error(), link)
	}
	if importStdout.Len() != 0 {
		t.Errorf("stdout must carry no success result, got %q", importStdout.String())
	}

	assertSymlinkIntact(t, link, relTarget)
	if _, err := os.Stat(filepath.Join(dir, "missing")); !os.IsNotExist(err) {
		t.Fatalf("the missing target must stay missing, stat err=%v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".govflow-registry-") {
			t.Errorf("rejected operation left a temporary file behind: %s", e.Name())
		}
	}
}

// A link loop refuses the command with an error naming the user's registry
// path; nothing is created and the loop is left as it was.
func TestBatchRegisterCLIRejectsSymlinkLoop(t *testing.T) {
	dir := t.TempDir()
	linkA := filepath.Join(dir, "a.json")
	linkB := filepath.Join(dir, "b.json")
	if err := os.Symlink(filepath.Base(linkB), linkA); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(linkA), linkB); err != nil {
		t.Fatal(err)
	}

	stdout, err := registerCLI(t, linkA, "B1", "P", "1", "kg")
	if err == nil {
		t.Fatal("batch-register must reject a link loop")
	}
	if !strings.Contains(err.Error(), linkA) {
		t.Errorf("error %q must name the registry path %q", err.Error(), linkA)
	}
	if stdout != "" {
		t.Errorf("stdout must carry no success result, got %q", stdout)
	}
	assertSymlinkIntact(t, linkA, filepath.Base(linkB))
	assertSymlinkIntact(t, linkB, filepath.Base(linkA))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("the rejected command must not create files: %v", entries)
	}
}

// A conflict reported through a link follows the usual failure contract: the
// command fails, and the linked registry keeps its bytes and mtime.
func TestBatchRegisterCLIConflictThroughSymlinkKeepsTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "reg.json")
	writeFile(t, target, `{"version":1,"batches":[{"batch":"B-1","product":"P-7","quantity":120,"unit":"kg"}]}`)
	content, pinned := pinRegistry(t, target)
	link := filepath.Join(dir, "reg-link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	stdout, err := registerCLI(t, link, "B-1", "P-OTHER", "120", "kg")
	if err == nil {
		t.Fatal("a conflicting re-registration must fail")
	}
	if stdout != "" {
		t.Errorf("stdout must carry no success result, got %q", stdout)
	}
	assertRegistryUntouched(t, target, content, pinned)
	assertSymlinkIntact(t, link, target)
}

// A first registration through a path that is a plain missing file still
// creates the registry exactly as before — the symlink handling must not
// change the plain-file contract.
func TestBatchRegisterCLIPlainMissingPathStillCreates(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "fresh", "reg.json")

	stdout, err := registerCLI(t, registry, "B1", "P", "1", "kg")
	if err != nil {
		t.Fatalf("first registration on a plain path must succeed: %v", err)
	}
	if res := decodeRegisterResult(t, stdout); res.Status != "created" {
		t.Fatalf("status = %q, want created", res.Status)
	}
	info, err := os.Lstat(registry)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.Mode().IsRegular() {
		t.Fatalf("the created registry must be a regular file, got mode %v", info.Mode())
	}
	stored := readStoredBatches(t, registry)
	if len(stored) != 1 || stored[0].Batch != "B1" {
		t.Fatalf("unexpected registry content: %+v", stored)
	}
}
