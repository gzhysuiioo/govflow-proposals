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

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

type importOutput struct {
	Results []struct {
		Batch    string `json:"batch"`
		Product  string `json:"product"`
		Quantity int64  `json:"quantity"`
		Unit     string `json:"unit"`
		Status   string `json:"status"`
	} `json:"results"`
}

func TestBatchImportCLICreatesRegistry(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "nested", "reg.json")
	manifest := filepath.Join(dir, "in.json")
	writeFile(t, manifest, `[
  {"batch": " B1 ", "product": "P-7", "quantity": 120, "unit": "kg"},
  {"batch": "B1", "product": "P-7", "quantity": 120, "unit": "kg"},
  {"batch": "B2", "product": "P-8", "quantity": 1, "unit": "box"}
]`)

	var stdout bytes.Buffer
	if err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout); err != nil {
		t.Fatalf("import failed: %v", err)
	}
	var out importOutput
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &out); err != nil {
		t.Fatalf("stdout is not the expected JSON: %v (%q)", err, stdout.String())
	}
	wantStatus := []string{"created", "duplicate", "created"}
	if len(out.Results) != len(wantStatus) {
		t.Fatalf("got %d results", len(out.Results))
	}
	for i, want := range wantStatus {
		if out.Results[i].Status != want {
			t.Errorf("result %d status = %q, want %q", i+1, out.Results[i].Status, want)
		}
		if out.Results[i].Batch != "B1" && i < 2 {
			t.Errorf("result %d batch = %q, want trimmed B1", i+1, out.Results[i].Batch)
		}
	}

	data, err := os.ReadFile(registry)
	if err != nil {
		t.Fatal(err)
	}
	var stored struct {
		Version int `json:"version"`
		Batches []struct {
			Batch    string `json:"batch"`
			Quantity int64  `json:"quantity"`
		} `json:"batches"`
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Version != 1 || len(stored.Batches) != 2 {
		t.Fatalf("unexpected registry content: %s", data)
	}
	if stored.Batches[0].Batch != "B1" || stored.Batches[1].Batch != "B2" {
		t.Fatalf("new batches not appended in first-occurrence order: %s", data)
	}
}

func TestBatchImportCLIAllDuplicatesLeavesFileUntouched(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "reg.json")
	manifest := filepath.Join(dir, "in.json")
	writeFile(t, registry, `{"version":1,"batches":[{"batch":"B1","product":"P","quantity":1,"unit":"kg"}]}`)
	writeFile(t, manifest, `[{"batch":"B1","product":"P","quantity":1,"unit":"kg"}]`)

	// Pin a distinctive past mtime so any rewrite is observable even on
	// filesystems with coarse timestamp granularity.
	pinned := time.Date(2001, time.February, 3, 4, 5, 6, 0, time.UTC)
	if err := os.Chtimes(registry, pinned, pinned); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(registry)
	if err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	if err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout); err != nil {
		t.Fatalf("import failed: %v", err)
	}
	if !strings.Contains(stdout.String(), `"status":"duplicate"`) {
		t.Fatalf("expected duplicate result, got %q", stdout.String())
	}

	after, err := os.ReadFile(registry)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("all-duplicate import rewrote the registry bytes")
	}
	info, err := os.Stat(registry)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(pinned) {
		t.Fatalf("all-duplicate import changed mtime: got %v, want %v", info.ModTime(), pinned)
	}
}

func TestBatchImportCLIRejectsAndPreserves(t *testing.T) {
	cases := map[string]string{
		"empty file":      "",
		"empty array":     "[]",
		"object":          "{}",
		"quantity string": `[{"batch":"B","product":"P","quantity":"1","unit":"kg"}]`,
		"quantity float":  `[{"batch":"B","product":"P","quantity":1.0,"unit":"kg"}]`,
		"missing field":   `[{"batch":"B","product":"P","quantity":1}]`,
		"unknown field":   `[{"batch":"B","product":"P","quantity":1,"unit":"kg","x":1}]`,
		"blank batch":     `[{"batch":"  ","product":"P","quantity":1,"unit":"kg"}]`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			registry := filepath.Join(dir, "reg.json")
			manifest := filepath.Join(dir, "in.json")
			writeFile(t, manifest, content)

			var stdout bytes.Buffer
			err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout)
			if err == nil {
				t.Fatal("expected rejection")
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout must stay empty on failure, got %q", stdout.String())
			}
			if _, statErr := os.Stat(registry); !os.IsNotExist(statErr) {
				t.Fatal("a rejected import must not create the registry file")
			}
		})
	}
}

func TestBatchImportCLIConflictPreservesRegistry(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "reg.json")
	manifest := filepath.Join(dir, "in.json")
	original := `{"version":1,"batches":[{"batch":"B1","product":"P","quantity":1,"unit":"kg"}]}`
	writeFile(t, registry, original)
	writeFile(t, manifest, `[
  {"batch":"NEW","product":"P","quantity":1,"unit":"kg"},
  {"batch":"B1","product":"P","quantity":2,"unit":"kg"}
]`)

	var stdout bytes.Buffer
	err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout)
	if err == nil {
		t.Fatal("conflict must fail")
	}
	if !strings.Contains(err.Error(), "record 2") || !strings.Contains(err.Error(), "B1") ||
		!strings.Contains(err.Error(), "quantity") {
		t.Fatalf("error must name position, batch and field: %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout must stay empty: %q", stdout.String())
	}
	after, rerr := os.ReadFile(registry)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(after) != original {
		t.Fatalf("registry changed: %s", after)
	}
}

func TestBatchImportCLICorruptRegistryRejected(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "reg.json")
	manifest := filepath.Join(dir, "in.json")
	writeFile(t, registry, "not json at all")
	writeFile(t, manifest, `[{"batch":"B","product":"P","quantity":1,"unit":"kg"}]`)

	var stdout bytes.Buffer
	if err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout); err == nil {
		t.Fatal("corrupt registry must be rejected")
	}
	after, _ := os.ReadFile(registry)
	if string(after) != "not json at all" {
		t.Fatalf("corrupt registry was touched: %q", after)
	}
}

func TestBatchImportCLIMissingFlags(t *testing.T) {
	var stdout bytes.Buffer
	if err := runBatchImport([]string{"--registry", "x.json"}, &stdout); err == nil {
		t.Fatal("missing --input must fail")
	}
}

// A registry whose earlier batches are followed by a second, empty "batches"
// member must be rejected outright: before the fix, the later empty array
// silently replaced the stored records and the next registration lost them.
func TestStructurallyInvalidRegistryBlocksBothCommands(t *testing.T) {
	registries := map[string]string{
		"duplicate empty batches": `{"version":1,"batches":[{"batch":"B1","product":"P","quantity":1,"unit":"kg"}],"batches":[]}`,
		"duplicate version":       `{"version":1,"version":1,"batches":[]}`,
		"null batches":            `{"version":1,"batches":null}`,
		"missing batches":         `{"version":1}`,
		"capitalized fields":      `{"Version":1,"Batches":[]}`,
		"record duplicate field":  `{"version":1,"batches":[{"batch":"B1","batch":"B1","product":"P","quantity":1,"unit":"kg"}]}`,
	}
	for name, content := range registries {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			registry := filepath.Join(dir, "reg.json")
			manifest := filepath.Join(dir, "in.json")
			writeFile(t, registry, content)
			manifestContent := `[{"batch":"NEW","product":"P","quantity":1,"unit":"kg"}]`
			writeFile(t, manifest, manifestContent)

			pinned := time.Date(2002, time.March, 4, 5, 6, 7, 0, time.UTC)
			if err := os.Chtimes(registry, pinned, pinned); err != nil {
				t.Fatal(err)
			}

			// batch-register must refuse.
			var stdout bytes.Buffer
			err := runBatchRegister([]string{
				"--registry", registry, "--batch", "NEW", "--product", "P",
				"--quantity", "1", "--unit", "kg",
			}, &stdout)
			if err == nil {
				t.Fatal("batch-register must reject the registry")
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout must stay empty, got %q", stdout.String())
			}
			for _, want := range []string{registry} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error must name the registry path %q: %v", want, err)
				}
			}

			// batch-import with a valid manifest must refuse just the same.
			stdout.Reset()
			err = runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout)
			if err == nil {
				t.Fatal("batch-import must reject the registry")
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout must stay empty, got %q", stdout.String())
			}

			// Neither command may have touched either file.
			after, rerr := os.ReadFile(registry)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if string(after) != content {
				t.Fatalf("registry bytes changed: %s", after)
			}
			info, serr := os.Stat(registry)
			if serr != nil {
				t.Fatal(serr)
			}
			if !info.ModTime().Equal(pinned) {
				t.Fatalf("registry mtime changed: %v", info.ModTime())
			}
			manifestAfter, merr := os.ReadFile(manifest)
			if merr != nil {
				t.Fatal(merr)
			}
			if string(manifestAfter) != manifestContent {
				t.Fatal("read-only manifest was modified")
			}
		})
	}
}

// The error for a bad record inside the registry must name the file, the
// 1-based record position, the field and — when unambiguous — the batch id.
func TestRegistryRecordErrorNamesPositionAndBatch(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "reg.json")
	writeFile(t, registry, `{"version":1,"batches":[
		{"batch":"B1","product":"P","quantity":1,"unit":"kg"},
		{"batch":"B2","product":"P","quantity":1,"unit":"kg","extra":1}]}`)

	var stdout bytes.Buffer
	err := runBatchRegister([]string{
		"--registry", registry, "--batch", "NEW", "--product", "P",
		"--quantity", "1", "--unit", "kg",
	}, &stdout)
	if err == nil {
		t.Fatal("must reject the registry")
	}
	msg := err.Error()
	for _, want := range []string{registry, "record 2", "B2", "extra"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error must mention %q: %v", want, msg)
		}
	}
}
