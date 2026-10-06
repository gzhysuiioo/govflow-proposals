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

// End-to-end regression for the batch commands when --registry names a path
// whose ANCESTOR DIRECTORY is a symbolic link followed by "..": work/alias
// points at store/child, but the user passes work/alias/../batches.json.
// The kernel resolves that to store/batches.json; a lexical cleanup would
// instead target work/batches.json. These tests pin the contract for both
// commands and the Go library save entry behind them:
//
//   - a creating registration or import appends to store/batches.json, old
//     records first, and the success JSON / import result order is unchanged;
//   - reading through the user path and the real path yields the same bytes;
//   - write permission is needed only on store: work may stay read-and-execute
//     and must never gain a registry file, a temporary file or a directory;
//   - work/batches.json, when present, is an unrelated decoy whose bytes,
//     modification time and permissions are preserved, as is the link text;
//   - when store cannot be written, or the save itself fails there, the
//     command exits non-zero with stderr naming the user path and the reason
//     and empty stdout, the real registry stays byte- and mtime-identical,
//     and nothing is left behind on either side of the link;
//   - a pure duplicate confirmation leaves both registries untouched.

// cliDotDotFixture builds work/alias -> ../store/child with the real
// registry at store/batches.json and an unrelated decoy at work/batches.json.
type cliDotDotFixture struct {
	root      string
	storeDir  string
	workDir   string
	realPath  string
	userPath  string
	decoyPath string
	linkPath  string
}

const cliDecoyContent = `{"unrelated":"decoy-content"}`

// cliDotDotLinkTarget is the exact text stored in the directory link.
var cliDotDotLinkTarget = filepath.Join("..", "store", "child")

func setupCLIDotDotFixture(t *testing.T, registryContent string) cliDotDotFixture {
	t.Helper()
	f := cliDotDotFixture{root: t.TempDir()}
	f.storeDir = filepath.Join(f.root, "store")
	f.workDir = filepath.Join(f.root, "work")
	if err := os.MkdirAll(filepath.Join(f.storeDir, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	f.realPath = filepath.Join(f.storeDir, "batches.json")
	writeFile(t, f.realPath, registryContent)
	f.decoyPath = filepath.Join(f.workDir, "batches.json")
	writeFile(t, f.decoyPath, cliDecoyContent)
	f.linkPath = filepath.Join(f.workDir, "alias")
	if err := os.Symlink(cliDotDotLinkTarget, f.linkPath); err != nil {
		t.Fatal(err)
	}
	// Concatenated, never filepath.Join: lexical cleaning would erase the
	// alias/.. crossing this whole regression is about.
	f.userPath = f.workDir + string(filepath.Separator) + "alias" +
		string(filepath.Separator) + ".." + string(filepath.Separator) + "batches.json"
	return f
}

// allowCLIDirCleanup makes dirs writable again so TempDir removal works.
func allowCLIDirCleanup(t *testing.T, dirs ...string) {
	t.Helper()
	t.Cleanup(func() {
		for _, d := range dirs {
			_ = os.Chmod(d, 0o755)
		}
	})
}

// assertCLIDecoyUntouched verifies the unrelated link-side file in full.
func assertCLIDecoyUntouched(t *testing.T, f cliDotDotFixture, wantPerm os.FileMode, wantMod time.Time) {
	t.Helper()
	info, err := os.Stat(f.decoyPath)
	if err != nil {
		t.Fatalf("the decoy must still exist: %v", err)
	}
	if !info.ModTime().Equal(wantMod) {
		t.Errorf("decoy mtime changed: got %v, want %v", info.ModTime(), wantMod)
	}
	if info.Mode().Perm() != wantPerm {
		t.Errorf("decoy permissions = %v, want %v", info.Mode().Perm(), wantPerm)
	}
	got, err := os.ReadFile(f.decoyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != cliDecoyContent {
		t.Errorf("decoy bytes changed:\nwant %s\ngot  %s", cliDecoyContent, got)
	}
}

// assertNoRegistryTempFiles fails if a save temporary file survives anywhere
// under root — on either side of the directory link.
func assertNoRegistryTempFiles(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".govflow-registry-") {
			t.Errorf("command left a save temporary file behind: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// assertWorkSideShrinkWrapped fails unless work holds only the directory
// link and the decoy: the command must create nothing there.
func assertWorkSideShrinkWrapped(t *testing.T, f cliDotDotFixture) {
	t.Helper()
	assertStillSymlink(t, f.linkPath, cliDotDotLinkTarget)
	entries, err := os.ReadDir(f.workDir)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name()] = true
	}
	if len(names) != 2 || !names["alias"] || !names["batches.json"] {
		t.Fatalf("work must hold only the link and the decoy, got %v", entries)
	}
}

// cliDotDotRegistry is the one-existing-batch fixture the headline tests use.
const cliDotDotRegistry = `{"version":1,"batches":[{"batch":"OLD-1","product":"P-1","quantity":10,"unit":"kg"}]}`

// TestBatchRegisterCLIDirSymlinkDotDotSavesToRealRegistry: with work
// read-and-execute only, a new batch registered through the user path lands
// in store/batches.json (old record first, new appended), stdout reports
// "created", both read paths agree byte for byte, and the link side is
// untouched.
func TestBatchRegisterCLIDirSymlinkDotDotSavesToRealRegistry(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permission bits are not enforced for root")
	}
	f := setupCLIDotDotFixture(t, cliDotDotRegistry)
	if err := os.Chmod(f.realPath, 0o640); err != nil {
		t.Fatal(err)
	}
	pinned := time.Date(2005, time.February, 3, 4, 5, 6, 0, time.UTC)
	if err := os.Chtimes(f.realPath, pinned, pinned); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f.workDir, 0o555); err != nil {
		t.Fatal(err)
	}
	allowCLIDirCleanup(t, f.workDir)
	decoyInfo, err := os.Stat(f.decoyPath)
	if err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	err = runBatchRegister([]string{
		"--registry", f.userPath,
		"--batch", "NEW-1", "--product", "P-9", "--quantity", "5", "--unit", "box",
	}, &stdout)
	if err != nil {
		t.Fatalf("registration through the directory link must succeed: %v", err)
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

	info, err := os.Stat(f.realPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.ModTime().Equal(pinned) {
		t.Fatal("the real registry must be replaced; its pinned mtime survived")
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("real registry permissions = %v, want preserved 0640", info.Mode().Perm())
	}
	for _, path := range []string{f.userPath, f.realPath} {
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
			t.Fatalf("registry read via %s is not valid JSON: %v", path, err)
		}
		if len(stored.Batches) != 2 || stored.Batches[0].Batch != "OLD-1" || stored.Batches[1].Batch != "NEW-1" {
			t.Fatalf("registry read via %s = %s, want OLD-1 then NEW-1", path, data)
		}
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
		t.Fatal("bytes read through the user path and the real path must be identical")
	}
	assertCLIDecoyUntouched(t, f, decoyInfo.Mode().Perm(), decoyInfo.ModTime())
	assertWorkSideShrinkWrapped(t, f)
	assertNoRegistryTempFiles(t, f.root)
}

// TestBatchImportCLIDirSymlinkDotDotSavesToRealRegistry: an import mixing a
// duplicate and a new batch saves the new one to store/batches.json and
// reports manifest-order results, needing no write permission on work.
func TestBatchImportCLIDirSymlinkDotDotSavesToRealRegistry(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permission bits are not enforced for root")
	}
	f := setupCLIDotDotFixture(t, cliDotDotRegistry)
	manifest := filepath.Join(f.root, "arrivals.json")
	writeFile(t, manifest, `[
  {"batch": "OLD-1", "product": "P-1", "quantity": 10, "unit": "kg"},
  {"batch": "NEW-2", "product": "P-8", "quantity": 3, "unit": "box"}
]`)
	if err := os.Chmod(f.workDir, 0o555); err != nil {
		t.Fatal(err)
	}
	allowCLIDirCleanup(t, f.workDir)
	decoyInfo, err := os.Stat(f.decoyPath)
	if err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	if err := runBatchImport([]string{"--registry", f.userPath, "--input", manifest}, &stdout); err != nil {
		t.Fatalf("import through the directory link must succeed: %v", err)
	}
	var out importOutput
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &out); err != nil {
		t.Fatalf("stdout is not the expected JSON: %v (%q)", err, stdout.String())
	}
	if len(out.Results) != 2 || out.Results[0].Status != "duplicate" || out.Results[1].Status != "created" {
		t.Fatalf("results = %+v, want duplicate then created", out.Results)
	}

	data, err := os.ReadFile(f.realPath)
	if err != nil {
		t.Fatal(err)
	}
	var stored struct {
		Batches []struct {
			Batch string `json:"batch"`
		} `json:"batches"`
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatalf("real registry is not valid JSON: %v", err)
	}
	if len(stored.Batches) != 2 || stored.Batches[0].Batch != "OLD-1" || stored.Batches[1].Batch != "NEW-2" {
		t.Fatalf("real registry = %s, want OLD-1 then NEW-2", data)
	}
	assertCLIDecoyUntouched(t, f, decoyInfo.Mode().Perm(), decoyInfo.ModTime())
	assertWorkSideShrinkWrapped(t, f)
	assertNoRegistryTempFiles(t, f.root)
}

// TestBatchCommandsCLIRejectWhenRealDirectoryUnwritable: work is writable
// but store is read-and-execute only. Both commands must fail with exit code
// 1, stderr naming the user path and the permission reason, empty stdout,
// the real registry byte- and mtime-identical, and nothing created in work.
// Runs in the wrapped child process to cover the actual exit/stdout/stderr
// contract.
func TestBatchCommandsCLIRejectWhenRealDirectoryUnwritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permission bits are not enforced for root")
	}
	cases := []struct {
		name string
		run  func(t *testing.T, f cliDotDotFixture, manifest string) (int, []byte, []byte)
	}{
		{
			"batch-register",
			func(t *testing.T, f cliDotDotFixture, manifest string) (int, []byte, []byte) {
				return runRegisterChild(t, f.userPath, faultNone, "", true)
			},
		},
		{
			"batch-import",
			func(t *testing.T, f cliDotDotFixture, manifest string) (int, []byte, []byte) {
				return runImportChild(t, f.userPath, manifest, faultNone, "", true)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := setupCLIDotDotFixture(t, cliDotDotRegistry)
			manifest := filepath.Join(f.root, "arrivals.json")
			writeManifest(t, manifest, []manifestRow{
				{Batch: "B-NEW-9", Product: "P", Quantity: 1, Unit: "kg"},
			})
			pinned := time.Date(2004, time.January, 2, 3, 4, 5, 0, time.UTC)
			if err := os.Chtimes(f.realPath, pinned, pinned); err != nil {
				t.Fatal(err)
			}
			original := readFileSnapshot(t, f.realPath)
			decoyInfo, err := os.Stat(f.decoyPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(f.storeDir, 0o555); err != nil {
				t.Fatal(err)
			}
			allowCLIDirCleanup(t, f.storeDir, filepath.Join(f.storeDir, "child"))

			code, stdout, stderr := tc.run(t, f, manifest)

			if code != 1 {
				t.Fatalf("exit code = %d, want 1; stderr:\n%s", code, stderr)
			}
			if len(bytes.TrimSpace(stdout)) != 0 {
				t.Fatalf("stdout must stay empty, got %q", stdout)
			}
			msg := string(stderr)
			for _, want := range []string{"govflow:", "cannot save registry", f.userPath, "permission denied"} {
				if !strings.Contains(msg, want) {
					t.Fatalf("stderr must contain %q:\n%s", want, msg)
				}
			}

			info, err := os.Stat(f.realPath)
			if err != nil {
				t.Fatal(err)
			}
			if !info.ModTime().Equal(pinned) {
				t.Errorf("rejected command changed the real registry mtime: got %v, want %v", info.ModTime(), pinned)
			}
			if after := readFileSnapshot(t, f.realPath); !bytes.Equal(after, original) {
				t.Fatalf("rejected command changed the real registry bytes:\nbefore %s\nafter  %s", original, after)
			}
			assertCLIDecoyUntouched(t, f, decoyInfo.Mode().Perm(), decoyInfo.ModTime())
			assertWorkSideShrinkWrapped(t, f)
			assertNoRegistryTempFiles(t, f.root)
		})
	}
}

// TestBatchCommandsCLISaveFailureThroughDirSymlinkDotDot injects both
// mid-save failures in the wrapped child while the registry is addressed by
// the directory-link-plus-".." path. WRAP_REAL_TARGET pins the save at the
// real file: the prepared temporary file must sit in store and rename must
// replace store/batches.json — never work/batches.json. After the refused
// save the real registry and the decoy are untouched and nothing survives.
func TestBatchCommandsCLISaveFailureThroughDirSymlinkDotDot(t *testing.T) {
	causes := map[saveFault]string{
		faultWrite:  "injected save failure through directory link: content incomplete",
		faultRename: "injected save failure through directory link: replacement refused",
	}
	cases := []struct {
		name    string
		content string
		pinned  time.Time
		run     func(t *testing.T, f cliDotDotFixture, manifest, fault string, reason string) (int, []byte, []byte)
	}{
		{
			name:    "batch-register",
			content: registerFailureRegistryJSON,
			pinned:  registerFailurePinnedModTime,
			run: func(t *testing.T, f cliDotDotFixture, manifest, fault string, reason string) (int, []byte, []byte) {
				return runRegisterChild(t, f.userPath, saveFault(fault), reason, true)
			},
		},
		{
			name:    "batch-import",
			content: importFailureRegistryJSON,
			pinned:  importFailurePinnedModTime,
			run: func(t *testing.T, f cliDotDotFixture, manifest, fault string, reason string) (int, []byte, []byte) {
				return runImportChild(t, f.userPath, manifest, saveFault(fault), reason, true)
			},
		},
	}
	for _, tc := range cases {
		for _, fault := range []saveFault{faultWrite, faultRename} {
			t.Run(tc.name+"/"+string(fault), func(t *testing.T) {
				f := setupCLIDotDotFixture(t, tc.content)
				manifest := filepath.Join(f.root, "arrivals.json")
				if tc.name == "batch-import" {
					writeManifest(t, manifest, importFailureManifestRows)
				}
				if err := os.Chtimes(f.realPath, tc.pinned, tc.pinned); err != nil {
					t.Fatal(err)
				}
				original := readFileSnapshot(t, f.realPath)
				decoyInfo, err := os.Stat(f.decoyPath)
				if err != nil {
					t.Fatal(err)
				}
				t.Setenv("WRAP_REAL_TARGET", f.realPath)

				code, stdout, stderr := tc.run(t, f, manifest, string(fault), causes[fault])

				if code != 1 {
					t.Fatalf("exit code = %d, want 1; stderr:\n%s", code, stderr)
				}
				if len(bytes.TrimSpace(stdout)) != 0 {
					t.Fatalf("stdout must stay empty on save failure, got %q", stdout)
				}
				msg := string(stderr)
				for _, want := range []string{"govflow:", "cannot save registry", f.userPath, causes[fault]} {
					if !strings.Contains(msg, want) {
						t.Fatalf("stderr must contain %q:\n%s", want, msg)
					}
				}

				info, err := os.Stat(f.realPath)
				if err != nil {
					t.Fatal(err)
				}
				if !info.ModTime().Equal(tc.pinned) {
					t.Errorf("failed save changed the real registry mtime: got %v, want %v", info.ModTime(), tc.pinned)
				}
				if after := readFileSnapshot(t, f.realPath); !bytes.Equal(after, original) {
					t.Fatalf("failed save changed the real registry bytes:\nbefore %s\nafter  %s", original, after)
				}
				assertCLIDecoyUntouched(t, f, decoyInfo.Mode().Perm(), decoyInfo.ModTime())
				assertWorkSideShrinkWrapped(t, f)
				assertNoRegistryTempFiles(t, f.root)
			})
		}
	}
}

// TestBatchRegisterCLIDuplicateThroughDirSymlinkDotDot: an all-duplicate
// confirmation prints "duplicate" and leaves the real registry, the decoy
// and the link exactly as they were — no write is attempted anywhere.
func TestBatchRegisterCLIDuplicateThroughDirSymlinkDotDot(t *testing.T) {
	f := setupCLIDotDotFixture(t, cliDotDotRegistry)
	pinned := time.Date(2003, time.December, 1, 2, 3, 4, 0, time.UTC)
	if err := os.Chtimes(f.realPath, pinned, pinned); err != nil {
		t.Fatal(err)
	}
	original := readFileSnapshot(t, f.realPath)
	decoyInfo, err := os.Stat(f.decoyPath)
	if err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	if err := runBatchRegister([]string{
		"--registry", f.userPath,
		"--batch", "OLD-1", "--product", "P-1", "--quantity", "10", "--unit", "kg",
	}, &stdout); err != nil {
		t.Fatalf("a duplicate through the directory link must succeed: %v", err)
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

	info, err := os.Stat(f.realPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(pinned) {
		t.Errorf("duplicate confirmation changed the real registry mtime: got %v, want %v", info.ModTime(), pinned)
	}
	if after := readFileSnapshot(t, f.realPath); !bytes.Equal(after, original) {
		t.Errorf("duplicate confirmation changed the real registry bytes:\nbefore %s\nafter  %s", original, after)
	}
	assertCLIDecoyUntouched(t, f, decoyInfo.Mode().Perm(), decoyInfo.ModTime())
	assertWorkSideShrinkWrapped(t, f)
	assertNoRegistryTempFiles(t, f.root)
}
