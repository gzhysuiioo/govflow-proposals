package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// End-to-end regression for the missing-directory-then-".." escape at the
// command boundary. store/batches.json is registered and store/missing does
// not exist; the --registry given on the command line is
// store/missing/../batches.json. That raw path reaches no file the way the
// kernel resolves names, so both commands must fail rather than report
// "created": a non-zero child exit code, stderr naming the complete
// user-supplied registry path and stating that the path first descends into a
// non-existent directory and then climbs back with "..", empty stdout, the
// existing registry left byte-for-byte and timestamp-for-timestamp with its
// original batch, no store/missing directory or replacement file created, no
// save temporary file left behind, and the import manifest kept read-only.
//
// Normal creation through a not-yet-existing parent without a ".." remains a
// successful registration, so the rejection is specific to the escape shape.
// The failure needs no injected fault, so the child runs the wrapped command
// with all save hooks healthy — this guards the command's own path check.

// missingEscapeCLIFixture builds root/store/batches.json (pinned, holding one
// OLD batch) with no store/missing, and returns the raw user registry path
// (assembled without lexical cleaning so "missing/.." survives), the real
// registry path and its pinned bytes/mtime.
func missingEscapeCLIFixture(t *testing.T) (root, userPath, registry string, content []byte, pinned time.Time) {
	t.Helper()
	root = t.TempDir()
	storeDir := filepath.Join(root, "store")
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	registry = filepath.Join(storeDir, "batches.json")
	writeFile(t, registry, `{"version":1,"batches":[{"batch":"OLD","product":"P-1","quantity":10,"unit":"kg"}]}`)
	content, pinned = pinRegistry(t, registry)
	userPath = strings.Join([]string{storeDir, "missing", "..", "batches.json"}, string(os.PathSeparator))
	return root, userPath, registry, content, pinned
}

// assertEscapeChildFailure checks the process contract shared by both
// commands: exit code 1, empty stdout, and stderr carrying the command prefix,
// the exact user registry path and the missing-directory/".." reason.
func assertEscapeChildFailure(t *testing.T, code int, stdout, stderr []byte, userPath string) {
	t.Helper()
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr:\n%s", code, stderr)
	}
	if len(bytes.TrimSpace(stdout)) != 0 {
		t.Fatalf("stdout must stay empty on the rejected path, got %q", stdout)
	}
	msg := string(stderr)
	for _, want := range []string{
		"govflow:",
		userPath,
		"does not exist",
		"..",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("stderr must contain %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, `"status":"created"`) {
		t.Errorf("stderr must not carry a success payload:\n%s", msg)
	}
}

// assertEscapeFilesystemUntouched verifies the rejected command created
// nothing: the real registry keeps its pinned bytes/mtime and its OLD batch,
// store/missing is absent and no save temporary file survives anywhere.
// extraAllowed names files (besides the registry) the fixture legitimately
// holds, such as the read-only import manifest.
func assertEscapeFilesystemUntouched(t *testing.T, root, registry string, content []byte, pinned time.Time, extraAllowed ...string) {
	t.Helper()
	assertRegistryUntouched(t, registry, content, pinned)
	if got := readStoredBatches(t, registry); len(got) != 1 || got[0].Batch != "OLD" {
		t.Fatalf("registry must keep exactly OLD, got %+v", got)
	}
	missing := filepath.Join(root, "store", "missing")
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Fatalf("the rejected command must not create the missing directory, stat err=%v", err)
	}
	allowed := append([]string{"batches.json"}, extraAllowed...)
	assertNoSaveLeftovers(t, root, allowed...)
}

// batch-register of a legal NEW batch through the escape path fails the
// process contract instead of replacing the OLD registry with NEW.
func TestBatchRegisterCLIMissingDirDotDotRejected(t *testing.T) {
	root, userPath, registry, content, pinned := missingEscapeCLIFixture(t)

	code, stdout, stderr := runRegisterChild(t, userPath, faultNone, "", true)

	assertEscapeChildFailure(t, code, stdout, stderr, userPath)
	assertEscapeFilesystemUntouched(t, root, registry, content, pinned)
}

// batch-import of a manifest containing a NEW batch through the same path
// writes none of it: the manifest stays read-only and the registry keeps OLD.
func TestBatchImportCLIMissingDirDotDotRejectsWholeManifest(t *testing.T) {
	root, userPath, registry, content, pinned := missingEscapeCLIFixture(t)
	manifest := filepath.Join(root, "manifest.json")
	manifestJSON := `[{"batch":"NEW","product":"P-9","quantity":5,"unit":"box"}]`
	writeFile(t, manifest, manifestJSON)

	code, stdout, stderr := runImportChild(t, userPath, manifest, faultNone, "", true)

	assertEscapeChildFailure(t, code, stdout, stderr, userPath)
	assertEscapeFilesystemUntouched(t, root, registry, content, pinned, "manifest.json")
	if after, _ := os.ReadFile(manifest); string(after) != manifestJSON {
		t.Fatal("the read-only manifest was modified")
	}
}

// A relative --registry with the same directory relationship as the absolute
// one is rejected identically; this runs the command in-process with the
// working directory set to the fixture root.
func TestBatchCommandsCLIMissingDirDotDotRelativeRejected(t *testing.T) {
	root := t.TempDir()
	registry := filepath.Join(root, "store", "batches.json")
	writeFile(t, registry, `{"version":1,"batches":[{"batch":"OLD","product":"P-1","quantity":10,"unit":"kg"}]}`)
	content, pinned := pinRegistry(t, registry)
	manifest := filepath.Join(root, "manifest.json")
	manifestJSON := `[{"batch":"NEW","product":"P-9","quantity":5,"unit":"box"}]`
	writeFile(t, manifest, manifestJSON)
	t.Chdir(root)
	userPath := strings.Join([]string{"store", "missing", "..", "batches.json"}, string(os.PathSeparator))

	t.Run("register", func(t *testing.T) {
		var stdout bytes.Buffer
		err := runBatchRegister([]string{
			"--registry", userPath,
			"--batch", "NEW", "--product", "P-9", "--quantity", "5", "--unit", "box",
		}, &stdout)
		if err == nil {
			t.Fatal("registering through the relative escape path must fail")
		}
		if !strings.Contains(err.Error(), userPath) || !strings.Contains(err.Error(), "..") {
			t.Errorf("error %q must name the relative path and the \"..\" reason", err.Error())
		}
		if stdout.Len() != 0 {
			t.Fatalf("stdout must stay empty, got %q", stdout.String())
		}
		assertRegistryUntouched(t, registry, content, pinned)
	})
	t.Run("import", func(t *testing.T) {
		var stdout bytes.Buffer
		err := runBatchImport([]string{"--registry", userPath, "--input", manifest}, &stdout)
		if err == nil {
			t.Fatal("importing through the relative escape path must fail")
		}
		if !strings.Contains(err.Error(), userPath) || !strings.Contains(err.Error(), "..") {
			t.Errorf("error %q must name the relative path and the \"..\" reason", err.Error())
		}
		if stdout.Len() != 0 {
			t.Fatalf("stdout must stay empty, got %q", stdout.String())
		}
		assertRegistryUntouched(t, registry, content, pinned)
		if after, _ := os.ReadFile(manifest); string(after) != manifestJSON {
			t.Fatal("the read-only manifest was modified")
		}
	})
}

// Preserved normal behavior: a first registration into store/new/batches.json,
// whose parent does not exist but which never climbs back, creates the
// directory and the registry and reports created.
func TestBatchRegisterCLINewDirectoryWithoutDotDotStillCreated(t *testing.T) {
	root := t.TempDir()
	registry := filepath.Join(root, "store", "new", "batches.json")

	stdout, err := registerCLI(t, registry, "NEW", "P-9", "5", "box")
	if err != nil {
		t.Fatalf("a first registration in a new directory must succeed: %v", err)
	}
	if got := decodeRegisterResult(t, stdout); got.Status != "created" || got.Batch != "NEW" {
		t.Fatalf("unexpected result: %+v", got)
	}
	if got := readStoredBatches(t, registry); len(got) != 1 || got[0].Batch != "NEW" {
		t.Fatalf("the new registry must hold NEW, got %+v", got)
	}
	assertNoSaveLeftovers(t, filepath.Join(root, "store"), "batches.json")
}
