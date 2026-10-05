package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This file guards the two batch commands when --registry names a symbolic
// link: a registration or import that adds batches must save into the file
// the link points at — the link stays a link to the same target, the target
// keeps its permissions, and both paths read the same records — while a link
// whose target cannot be resolved rejects the command without creating the
// target or touching the link.

// assertStillSymlink fails unless path is still a symbolic link storing
// exactly wantTarget.
func assertStillSymlink(t *testing.T, path, wantTarget string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("the link must still exist: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s must still be a symbolic link, got mode %v", path, info.Mode())
	}
	target, err := os.Readlink(path)
	if err != nil {
		t.Fatal(err)
	}
	if target != wantTarget {
		t.Fatalf("link target = %q, want unchanged %q", target, wantTarget)
	}
}

// setupLinkedCLIRegistry writes content to targetPath with the given
// permissions and points a symbolic link at it, returning the link path.
func setupLinkedCLIRegistry(t *testing.T, targetPath, linkPath, linkTarget, content string, mode os.FileMode) {
	t.Helper()
	writeFile(t, targetPath, content)
	if err := os.Chmod(targetPath, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(linkTarget, linkPath); err != nil {
		t.Fatal(err)
	}
}

// Registering a new batch through a link appends it to the linked registry:
// the target file gains the record after the existing ones, the link is
// still a link to the same target, the target keeps its permissions, and
// reading back through either path sees the same two batches.
func TestBatchRegisterThroughSymlinkSavesToTarget(t *testing.T) {
	cases := []struct {
		name       string
		linkPath   func(root string) string
		linkTarget func(root, targetPath string) string
	}{
		{
			"absolute target",
			func(root string) string { return filepath.Join(root, "link.json") },
			func(root, targetPath string) string { return targetPath },
		},
		{
			"relative target, link in another directory",
			func(root string) string { return filepath.Join(root, "links", "link.json") },
			func(root, targetPath string) string { return filepath.Join("..", "real.json") },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			targetPath := filepath.Join(root, "real.json")
			linkPath := tc.linkPath(root)
			linkTarget := tc.linkTarget(root, targetPath)
			setupLinkedCLIRegistry(t, targetPath, linkPath, linkTarget,
				`{"version":1,"batches":[{"batch":"OLD","product":"P-1","quantity":10,"unit":"kg"}]}`, 0o640)

			var stdout bytes.Buffer
			err := runBatchRegister([]string{
				"--registry", linkPath,
				"--batch", "NEW-1", "--product", "P-9", "--quantity", "5", "--unit", "box",
			}, &stdout)
			if err != nil {
				t.Fatalf("registering through the link must succeed: %v", err)
			}
			var out struct {
				Batch  string `json:"batch"`
				Status string `json:"status"`
			}
			if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &out); err != nil {
				t.Fatalf("stdout is not the expected JSON: %v (%q)", err, stdout.String())
			}
			if out.Batch != "NEW-1" || out.Status != "created" {
				t.Fatalf("stdout = %+v, want batch NEW-1 status created", out)
			}

			assertStillSymlink(t, linkPath, linkTarget)
			info, err := os.Stat(targetPath)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o640 {
				t.Fatalf("target permissions = %v, want preserved 0640", info.Mode().Perm())
			}

			// The full result — old record first, new record appended — is
			// visible through the link path and the real path alike.
			for _, path := range []string{linkPath, targetPath} {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var stored struct {
					Version int `json:"version"`
					Batches []struct {
						Batch string `json:"batch"`
					} `json:"batches"`
				}
				if err := json.Unmarshal(data, &stored); err != nil {
					t.Fatalf("registry read via %s is not valid JSON: %v", path, err)
				}
				if stored.Version != 1 || len(stored.Batches) != 2 ||
					stored.Batches[0].Batch != "OLD" || stored.Batches[1].Batch != "NEW-1" {
					t.Fatalf("registry read via %s = %s, want OLD then NEW-1", path, data)
				}
			}
		})
	}
}

// Importing through a link saves every new batch into the linked registry
// and reports the per-record statuses on stdout exactly as for a direct
// path.
func TestBatchImportThroughSymlinkSavesToTarget(t *testing.T) {
	root := t.TempDir()
	targetPath := filepath.Join(root, "real.json")
	linkPath := filepath.Join(root, "link.json")
	setupLinkedCLIRegistry(t, targetPath, linkPath, targetPath,
		`{"version":1,"batches":[{"batch":"B1","product":"P-7","quantity":120,"unit":"kg"}]}`, 0o644)
	manifest := filepath.Join(root, "in.json")
	writeFile(t, manifest, `[
  {"batch": "B1", "product": "P-7", "quantity": 120, "unit": "kg"},
  {"batch": "B2", "product": "P-8", "quantity": 1, "unit": "box"}
]`)

	var stdout bytes.Buffer
	if err := runBatchImport([]string{"--registry", linkPath, "--input", manifest}, &stdout); err != nil {
		t.Fatalf("importing through the link must succeed: %v", err)
	}
	var out importOutput
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &out); err != nil {
		t.Fatalf("stdout is not the expected JSON: %v (%q)", err, stdout.String())
	}
	if len(out.Results) != 2 || out.Results[0].Status != "duplicate" || out.Results[1].Status != "created" {
		t.Fatalf("results = %+v, want duplicate then created", out.Results)
	}

	assertStillSymlink(t, linkPath, targetPath)
	data, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	var stored struct {
		Batches []struct {
			Batch string `json:"batch"`
		} `json:"batches"`
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.Batches) != 2 || stored.Batches[0].Batch != "B1" || stored.Batches[1].Batch != "B2" {
		t.Fatalf("linked registry = %s, want B1 then B2", data)
	}
}

// Confirming an already-registered batch through a link stays a pure
// duplicate: the success JSON says "duplicate", and the linked registry's
// bytes and modification time are exactly as before.
func TestBatchRegisterDuplicateThroughSymlinkLeavesTargetUntouched(t *testing.T) {
	root := t.TempDir()
	targetPath := filepath.Join(root, "real.json")
	linkPath := filepath.Join(root, "link.json")
	const content = `{"version":1,"batches":[{"batch":"B1","product":"P","quantity":1,"unit":"kg"}]}`
	setupLinkedCLIRegistry(t, targetPath, linkPath, targetPath, content, 0o644)
	pinned := time.Date(2002, time.March, 4, 5, 6, 7, 0, time.UTC)
	if err := os.Chtimes(targetPath, pinned, pinned); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	err := runBatchRegister([]string{
		"--registry", linkPath,
		"--batch", "B1", "--product", "P", "--quantity", "1", "--unit", "kg",
	}, &stdout)
	if err != nil {
		t.Fatalf("confirming a duplicate through the link must succeed: %v", err)
	}
	var out struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &out); err != nil {
		t.Fatalf("stdout is not the expected JSON: %v (%q)", err, stdout.String())
	}
	if out.Status != "duplicate" {
		t.Fatalf("status = %q, want duplicate", out.Status)
	}

	info, err := os.Stat(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(pinned) {
		t.Errorf("duplicate confirmation touched the target's modification time: got %v, want %v", info.ModTime(), pinned)
	}
	data, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != content {
		t.Errorf("duplicate confirmation changed the target bytes:\nbefore %s\nafter  %s", content, data)
	}
	assertStillSymlink(t, linkPath, targetPath)
}

// A link whose target does not exist is rejected by both commands: the error
// names the registry path the user passed and why it is unusable, stdout
// carries no success result, the link survives and the target is not
// created — the call is never treated as a first registration.
func TestBatchCommandsRejectDanglingSymlink(t *testing.T) {
	manifestContent := `[{"batch":"B1","product":"P","quantity":1,"unit":"kg"}]`
	cases := []struct {
		name string
		run  func(registryPath, manifestPath string, stdout *bytes.Buffer) error
	}{
		{"batch-register", func(registryPath, manifestPath string, stdout *bytes.Buffer) error {
			return runBatchRegister([]string{
				"--registry", registryPath,
				"--batch", "B1", "--product", "P", "--quantity", "1", "--unit", "kg",
			}, stdout)
		}},
		{"batch-import", func(registryPath, manifestPath string, stdout *bytes.Buffer) error {
			return runBatchImport([]string{"--registry", registryPath, "--input", manifestPath}, stdout)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			linkPath := filepath.Join(root, "link.json")
			missingTarget := filepath.Join(root, "missing.json")
			if err := os.Symlink(missingTarget, linkPath); err != nil {
				t.Fatal(err)
			}
			manifestPath := filepath.Join(root, "in.json")
			writeFile(t, manifestPath, manifestContent)

			var stdout bytes.Buffer
			err := tc.run(linkPath, manifestPath, &stdout)
			if err == nil {
				t.Fatal("a dangling registry link must reject the command")
			}
			msg := err.Error()
			if !strings.Contains(msg, linkPath) {
				t.Errorf("error %q must name the registry path the user passed", msg)
			}
			if !strings.Contains(msg, "symbolic link") {
				t.Errorf("error %q must state the path is an unusable symbolic link", msg)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout must carry no success result, got %q", stdout.String())
			}
			assertStillSymlink(t, linkPath, missingTarget)
			if _, statErr := os.Stat(missingTarget); !os.IsNotExist(statErr) {
				t.Errorf("the missing target must not be created, stat err=%v", statErr)
			}
			// The manifest is read-only input and must survive untouched.
			manifest, readErr := os.ReadFile(manifestPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(manifest) != manifestContent {
				t.Errorf("the input manifest changed: %s", manifest)
			}
		})
	}
}
