package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
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

// A record rejected for an unknown, duplicated or case-mismatched field must
// name the manifest path, the 1-based record position, the normalized batch
// id and the offending field — and leave both files untouched.
func TestBatchImportCLIFieldErrorNamesBatch(t *testing.T) {
	cases := map[string]struct {
		record    string
		wantBatch string
		wantField string
	}{
		"unknown field":         {`{"batch":" B-002 ","product":"P","quantity":1,"unit":"kg","supplier":"S"}`, "B-002", "supplier"},
		"duplicate product":     {`{"batch":"B-002","product":"P","product":"P","quantity":1,"unit":"kg"}`, "B-002", "product"},
		"case-mismatched field": {`{"batch":"B-002","Product":"P","product":"P","quantity":1,"unit":"kg"}`, "B-002", "Product"},
		"duplicate batch":       {`{"batch":"B-002","batch":"B-002","product":"P","quantity":1,"unit":"kg"}`, "", "batch"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			registry := filepath.Join(dir, "reg.json")
			manifest := filepath.Join(dir, "in.json")
			original := `{"version":1,"batches":[{"batch":"B-001","product":"P","quantity":1,"unit":"kg"}]}`
			writeFile(t, registry, original)
			writeFile(t, manifest, `[
  {"batch":"B-001","product":"P","quantity":1,"unit":"kg"},
  `+tc.record+`
]`)

			var stdout bytes.Buffer
			err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout)
			if err == nil {
				t.Fatal("expected rejection")
			}
			msg := err.Error()
			for _, want := range []string{manifest, "record 2", strconv.Quote(tc.wantField)} {
				if !strings.Contains(msg, want) {
					t.Fatalf("error must contain %q: %v", want, msg)
				}
			}
			if tc.wantBatch != "" {
				if !strings.Contains(msg, "batch "+strconv.Quote(tc.wantBatch)) {
					t.Fatalf("error must name batch %q: %v", tc.wantBatch, msg)
				}
			} else if strings.Contains(msg, "batch \"") {
				t.Fatalf("error must not quote a batch id: %v", msg)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout must stay empty on failure, got %q", stdout.String())
			}
			after, rerr := os.ReadFile(registry)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if string(after) != original {
				t.Fatalf("registry changed: %s", after)
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

// batch-register must refuse malformed UTF-8 in any text flag, naming the
// offending flag, before creating or touching the registry.
func TestBatchRegisterRejectsBadUTF8Flags(t *testing.T) {
	bad := "\xff"
	cases := []struct {
		name    string
		batch   string
		product string
		unit    string
		flag    string
	}{
		{"batch", " B" + bad + " ", "P", "kg", "--batch"},
		{"product", "B1", "P" + bad, "kg", "--product"},
		{"unit", "B1", "P", " kg" + bad, "--unit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			registry := filepath.Join(dir, "reg.json")
			var stdout bytes.Buffer
			err := runBatchRegister([]string{
				"--registry", registry,
				"--batch", tc.batch,
				"--product", tc.product,
				"--quantity", "1",
				"--unit", tc.unit,
			}, &stdout)
			if err == nil {
				t.Fatal("malformed UTF-8 must be rejected")
			}
			if !strings.Contains(err.Error(), tc.flag) {
				t.Fatalf("error must name %q: %v", tc.flag, err)
			}
			if !strings.Contains(err.Error(), "UTF-8") {
				t.Fatalf("error must explain the encoding problem: %v", err)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout must stay empty: %q", stdout.String())
			}
			if _, statErr := os.Stat(registry); !os.IsNotExist(statErr) {
				t.Fatal("a rejected registration must not create the registry file")
			}
		})
	}
}

// Any malformed text field in any manifest record rejects the whole import,
// even after legal records; the error names file, 1-based position and field
// and only cites the batch id when that id itself is valid.
func TestBatchImportRejectsBadUTF8Manifest(t *testing.T) {
	cases := map[string]struct {
		content []byte
		pos     string
		field   string
		batch   string // empty when the batch must not be cited
	}{
		"bad unit after legal record": {
			[]byte("[{\"batch\":\"OK1\",\"product\":\"P\",\"quantity\":1,\"unit\":\"kg\"}," +
				"{\"batch\":\"BAD2\",\"product\":\"P\",\"quantity\":2,\"unit\":\"kg\xff\"}]"),
			"record 2", "unit", "BAD2",
		},
		"bad batch cites no id": {
			[]byte("[{\"batch\":\"B\xff\",\"product\":\"P\",\"quantity\":1,\"unit\":\"kg\"}]"),
			"record 1", "batch", "",
		},
		"bad product cites valid batch": {
			[]byte("[{\"batch\":\"B42\",\"product\":\"P\xff\",\"quantity\":1,\"unit\":\"kg\"}]"),
			"record 1", "product", "B42",
		},
		"lone surrogate escape": {
			[]byte(`[{"batch":"B\ud800","product":"P","quantity":1,"unit":"kg"}]`),
			"record 1", "batch", "",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			registry := filepath.Join(dir, "reg.json")
			manifest := filepath.Join(dir, "in.json")
			if err := os.WriteFile(manifest, tc.content, 0o644); err != nil {
				t.Fatal(err)
			}

			var stdout bytes.Buffer
			err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout)
			if err == nil {
				t.Fatal("malformed manifest must be rejected")
			}
			msg := err.Error()
			for _, want := range []string{manifest, tc.pos, tc.field} {
				if !strings.Contains(msg, want) {
					t.Fatalf("error must mention %q: %v", want, msg)
				}
			}
			if tc.batch != "" {
				if !strings.Contains(msg, tc.batch) {
					t.Fatalf("error must cite batch %q: %v", tc.batch, msg)
				}
			} else if strings.Contains(msg, "�") {
				t.Fatalf("error must not cite a substituted batch id: %v", msg)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout must stay empty: %q", stdout.String())
			}
			if _, statErr := os.Stat(registry); !os.IsNotExist(statErr) {
				t.Fatal("rejected import must not create the registry")
			}
			after, rerr := os.ReadFile(manifest)
			if rerr != nil || !bytes.Equal(after, tc.content) {
				t.Fatal("the read-only manifest was modified")
			}
		})
	}
}

// A registry record containing malformed UTF-8 blocks both entry points; the
// file's bytes and mtime stay exactly as they were.
func TestBadUTF8RegistryBlocksBothCommands(t *testing.T) {
	registries := map[string]struct {
		content []byte
		pos     string
		field   string
		batch   string
	}{
		"bad batch": {
			[]byte("{\"version\":1,\"batches\":[{\"batch\":\"B\xff\",\"product\":\"P\",\"quantity\":1,\"unit\":\"kg\"}]}"),
			"record 1", "batch", "",
		},
		"bad unit second record": {
			[]byte("{\"version\":1,\"batches\":[" +
				"{\"batch\":\"B1\",\"product\":\"P\",\"quantity\":1,\"unit\":\"kg\"}," +
				"{\"batch\":\"B2\",\"product\":\"P\",\"quantity\":2,\"unit\":\"kg\xff\"}]}"),
			"record 2", "unit", "B2",
		},
		"lone surrogate": {
			[]byte(`{"version":1,"batches":[{"batch":"B\udc01","product":"P","quantity":1,"unit":"kg"}]}`),
			"record 1", "batch", "",
		},
	}
	manifestContent := []byte(`[{"batch":"NEW","product":"P","quantity":1,"unit":"kg"}]`)
	for name, tc := range registries {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			registry := filepath.Join(dir, "reg.json")
			manifest := filepath.Join(dir, "in.json")
			if err := os.WriteFile(registry, tc.content, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(manifest, manifestContent, 0o644); err != nil {
				t.Fatal(err)
			}
			pinned := time.Date(2003, time.April, 5, 6, 7, 8, 0, time.UTC)
			if err := os.Chtimes(registry, pinned, pinned); err != nil {
				t.Fatal(err)
			}

			assertBlocked := func(err error, stdout *bytes.Buffer) {
				t.Helper()
				if err == nil {
					t.Fatal("malformed registry must be rejected")
				}
				msg := err.Error()
				for _, want := range []string{registry, tc.pos, tc.field} {
					if !strings.Contains(msg, want) {
						t.Fatalf("error must mention %q: %v", want, msg)
					}
				}
				if tc.batch != "" && !strings.Contains(msg, tc.batch) {
					t.Fatalf("error must cite batch %q: %v", tc.batch, msg)
				}
				if tc.batch == "" && strings.Contains(msg, "�") {
					t.Fatalf("error must not cite a substituted id: %v", msg)
				}
				if stdout.Len() != 0 {
					t.Fatalf("stdout must stay empty: %q", stdout.String())
				}
			}

			var stdout bytes.Buffer
			err := runBatchRegister([]string{
				"--registry", registry, "--batch", "NEW", "--product", "P",
				"--quantity", "1", "--unit", "kg",
			}, &stdout)
			assertBlocked(err, &stdout)

			stdout.Reset()
			err = runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout)
			assertBlocked(err, &stdout)

			after, rerr := os.ReadFile(registry)
			if rerr != nil || !bytes.Equal(after, tc.content) {
				t.Fatalf("registry bytes changed: %q", after)
			}
			info, serr := os.Stat(registry)
			if serr != nil || !info.ModTime().Equal(pinned) {
				t.Fatalf("registry mtime changed: %v", serr)
			}
			mAfter, merr := os.ReadFile(manifest)
			if merr != nil || !bytes.Equal(mAfter, manifestContent) {
				t.Fatal("manifest was modified")
			}
		})
	}
}

// Valid Unicode — CJK, emoji and a genuine U+FFFD — works end to end, and a
// JSON escape means the same text as the directly written character.
func TestUnicodeRoundTripsThroughBothCommands(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "reg.json")

	var stdout bytes.Buffer
	if err := runBatchRegister([]string{
		"--registry", registry, "--batch", " 批次-1 ", "--product", "产品😀",
		"--quantity", "7", "--unit", "千克",
	}, &stdout); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), `"status":"created"`) ||
		!strings.Contains(stdout.String(), `"batch":"批次-1"`) {
		t.Fatalf("unexpected output: %q", stdout.String())
	}

	// The same batch via a JSON \uXXXX escape pair for the emoji must be a
	// duplicate, while a direct genuine U+FFFD registers a distinct batch.
	manifest := filepath.Join(dir, "in.json")
	writeFile(t, manifest, `[
		{"batch":"批次-1","product":"产品😀","quantity":7,"unit":"千克"},
		{"batch":"B-FFFD","product":"P�","quantity":1,"unit":"kg"}
	]`)
	stdout.Reset()
	if err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout); err != nil {
		t.Fatal(err)
	}
	var out importOutput
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 2 ||
		out.Results[0].Status != "duplicate" || out.Results[0].Product != "产品😀" ||
		out.Results[1].Status != "created" || out.Results[1].Product != "P�" {
		t.Fatalf("unexpected results: %s", stdout.String())
	}

	// Re-registering the genuine U+FFFD batch directly is a duplicate, and a
	// malformed byte in a different flag is still refused.
	stdout.Reset()
	if err := runBatchRegister([]string{
		"--registry", registry, "--batch", "B-FFFD", "--product", "P�",
		"--quantity", "1", "--unit", "kg",
	}, &stdout); err != nil {
		t.Fatalf("genuine U+FFFD must stay usable: %v", err)
	}
	if !strings.Contains(stdout.String(), `"status":"duplicate"`) {
		t.Fatalf("expected duplicate: %q", stdout.String())
	}
}

// registerCLI runs one batch-register invocation and returns its stdout.
func registerCLI(t *testing.T, registry, batch, product, quantity, unit string) (string, error) {
	t.Helper()
	var stdout bytes.Buffer
	err := runBatchRegister([]string{
		"--registry", registry,
		"--batch", batch,
		"--product", product,
		"--quantity", quantity,
		"--unit", unit,
	}, &stdout)
	return stdout.String(), err
}

type registerResult struct {
	Batch    string `json:"batch"`
	Product  string `json:"product"`
	Quantity int64  `json:"quantity"`
	Unit     string `json:"unit"`
	Status   string `json:"status"`
}

// decodeRegisterResult parses stdout as exactly one JSON object carrying
// exactly the four record fields plus status — nothing before, after or
// beyond it, and no extra or missing members.
func decodeRegisterResult(t *testing.T, stdout string) registerResult {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(stdout))
	var keys map[string]json.RawMessage
	if err := dec.Decode(&keys); err != nil {
		t.Fatalf("stdout is not a JSON object: %v (%q)", err, stdout)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		t.Fatalf("stdout must hold exactly one JSON object, got %q", stdout)
	}
	for _, want := range []string{"batch", "product", "quantity", "unit", "status"} {
		if _, ok := keys[want]; !ok {
			t.Fatalf("result object is missing field %q: %q", want, stdout)
		}
		delete(keys, want)
	}
	if len(keys) != 0 {
		t.Fatalf("result object carries unexpected field(s): %q", stdout)
	}
	var res registerResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("result object does not match the record shape: %v (%q)", err, stdout)
	}
	return res
}

// readStoredBatches decodes the registry file into its records, in file order.
func readStoredBatches(t *testing.T, path string) []registerResult {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored struct {
		Version int              `json:"version"`
		Batches []registerResult `json:"batches"`
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatalf("registry file does not decode: %v (%s)", err, data)
	}
	if stored.Version != 1 {
		t.Fatalf("registry version = %d, want 1", stored.Version)
	}
	return stored.Batches
}

// pinRegistry reads the registry and pins a distinctive past mtime so any
// rewrite — even one reproducing identical bytes — is observable.
func pinRegistry(t *testing.T, path string) (content []byte, pinned time.Time) {
	t.Helper()
	pinned = time.Date(2004, time.May, 6, 7, 8, 9, 0, time.UTC)
	if err := os.Chtimes(path, pinned, pinned); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return content, pinned
}

// assertRegistryUntouched fails if the file's bytes or mtime differ from the
// pinned snapshot.
func assertRegistryUntouched(t *testing.T, path string, content []byte, pinned time.Time) {
	t.Helper()
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, content) {
		t.Fatalf("registry bytes changed:\nbefore: %s\nafter:  %s", content, after)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(pinned) {
		t.Fatalf("registry mtime changed: got %v, want %v", info.ModTime(), pinned)
	}
}

// A first registration returns "created"; re-submitting the same record —
// spelled with padding and leading zeros that normalize away — confirms the
// stored record as "duplicate", echoing the registered four fields as the
// only JSON object on stdout. The file keeps exactly its earlier records,
// the other batch's fields and position included, and neither its bytes nor
// its mtime move during the confirmation.
func TestBatchRegisterCLIDuplicateConfirm(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "reg.json")

	stdout, err := registerCLI(t, registry, "B-000", "P-0", "5", "box")
	if err != nil {
		t.Fatal(err)
	}
	if res := decodeRegisterResult(t, stdout); res.Status != "created" {
		t.Fatalf("first registration of B-000 = %+v, want created", res)
	}

	stdout, err = registerCLI(t, registry, " B-001 ", " P-7 ", "000120", " kg ")
	if err != nil {
		t.Fatal(err)
	}
	created := decodeRegisterResult(t, stdout)
	want := registerResult{Batch: "B-001", Product: "P-7", Quantity: 120, Unit: "kg", Status: "created"}
	if created != want {
		t.Fatalf("first registration = %+v, want %+v", created, want)
	}

	content, pinned := pinRegistry(t, registry)

	// The same record with the padding and leading zeros already removed is
	// the identical effective value: a duplicate, not a new record.
	stdout, err = registerCLI(t, registry, "B-001", "P-7", "120", "kg")
	if err != nil {
		t.Fatalf("identical re-registration must succeed: %v", err)
	}
	dup := decodeRegisterResult(t, stdout)
	want.Status = "duplicate"
	if dup != want {
		t.Fatalf("duplicate confirmation = %+v, want %+v", dup, want)
	}
	assertRegistryUntouched(t, registry, content, pinned)

	// Still exactly one record per batch, the other batch first and unchanged.
	batches := readStoredBatches(t, registry)
	if len(batches) != 2 {
		t.Fatalf("registry holds %d records, want 2: %v", len(batches), batches)
	}
	if batches[0] != (registerResult{Batch: "B-000", Product: "P-0", Quantity: 5, Unit: "box"}) {
		t.Fatalf("other batch's record changed: %+v", batches[0])
	}
	if batches[1] != (registerResult{Batch: "B-001", Product: "P-7", Quantity: 120, Unit: "kg"}) {
		t.Fatalf("stored record changed: %+v", batches[1])
	}
}

// A registry file whose layout differs from what Save would write — compact,
// members in another order — is still not re-saved to confirm a duplicate:
// bytes and mtime stay exactly as they were.
func TestBatchRegisterCLIDuplicateKeepsForeignFileLayout(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "reg.json")
	original := `{"batches":[{"unit":"kg","quantity":120,"product":"P-7","batch":"B-001"}],"version":1}`
	writeFile(t, registry, original)
	content, pinned := pinRegistry(t, registry)

	stdout, err := registerCLI(t, registry, "B-001", "P-7", "120", "kg")
	if err != nil {
		t.Fatalf("duplicate confirmation must succeed: %v", err)
	}
	if res := decodeRegisterResult(t, stdout); res.Status != "duplicate" {
		t.Fatalf("got %+v, want duplicate", res)
	}
	assertRegistryUntouched(t, registry, content, pinned)
	if after, _ := os.ReadFile(registry); string(after) != original {
		t.Fatalf("foreign layout was rewritten:\nbefore: %s\nafter:  %s", original, after)
	}
}

// Re-registering a batch id with any differing field fails: stdout stays
// empty, the error names the batch and every mismatching field — never just
// one of several — and the file keeps its bytes and mtime. Interior
// whitespace and casing are significant, so "p-7" and "k g" are conflicts,
// not duplicates.
func TestBatchRegisterCLIConflictReportsAllDifferingFields(t *testing.T) {
	cases := map[string]struct {
		product  string
		quantity string
		unit     string
		fields   []string
	}{
		"product casing differs":      {"p-7", "120", "kg", []string{"product"}},
		"unit interior space differs": {"P-7", "120", "k g", []string{"unit"}},
		"quantity differs":            {"P-7", "121", "kg", []string{"quantity"}},
		"all three differ":            {"P-8", "121", "g", []string{"product", "quantity", "unit"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			registry := filepath.Join(dir, "reg.json")
			writeFile(t, registry, `{"version":1,"batches":[`+
				`{"batch":"B-000","product":"P-0","quantity":5,"unit":"box"},`+
				`{"batch":"B-001","product":"P-7","quantity":120,"unit":"kg"}]}`)
			content, pinned := pinRegistry(t, registry)

			stdout, err := registerCLI(t, registry, "B-001", tc.product, tc.quantity, tc.unit)
			if err == nil {
				t.Fatal("a conflicting re-registration must fail")
			}
			if stdout != "" {
				t.Fatalf("stdout must stay empty on conflict, got %q", stdout)
			}
			msg := err.Error()
			if !strings.Contains(msg, strconv.Quote("B-001")) {
				t.Fatalf("error must name the batch: %v", msg)
			}
			for _, field := range tc.fields {
				if !strings.Contains(msg, field) {
					t.Fatalf("error must report differing field %q: %v", field, msg)
				}
			}
			assertRegistryUntouched(t, registry, content, pinned)

			batches := readStoredBatches(t, registry)
			if len(batches) != 2 || batches[1].Product != "P-7" ||
				batches[1].Quantity != 120 || batches[1].Unit != "kg" {
				t.Fatalf("the conflict must not overwrite the stored record: %v", batches)
			}
		})
	}
}

// Batch ids compare byte for byte: "b-001" is a different id from "B-001" and
// registers as its own record, after which it confirms duplicates of its own.
func TestBatchRegisterCLIBatchIDCaseSensitive(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "reg.json")

	if _, err := registerCLI(t, registry, "B-001", "P-7", "120", "kg"); err != nil {
		t.Fatal(err)
	}
	stdout, err := registerCLI(t, registry, "b-001", "P-7", "120", "kg")
	if err != nil {
		t.Fatalf("a case-distinct batch id must register: %v", err)
	}
	if res := decodeRegisterResult(t, stdout); res.Status != "created" || res.Batch != "b-001" {
		t.Fatalf("got %+v, want created b-001", res)
	}

	batches := readStoredBatches(t, registry)
	if len(batches) != 2 || batches[0].Batch != "B-001" || batches[1].Batch != "b-001" {
		t.Fatalf("want B-001 and b-001 as separate records in order, got %v", batches)
	}

	stdout, err = registerCLI(t, registry, "b-001", "P-7", "120", "kg")
	if err != nil {
		t.Fatal(err)
	}
	if res := decodeRegisterResult(t, stdout); res.Status != "duplicate" {
		t.Fatalf("got %+v, want duplicate", res)
	}
	if batches := readStoredBatches(t, registry); len(batches) != 2 {
		t.Fatalf("duplicate confirmation added a record: %v", batches)
	}
}

// Quantities compare as integers right up to the int64 ceiling: MaxQuantity
// registers and confirms as a duplicate, one less conflicts on quantity, and
// anything above the ceiling is rejected as an illegal --quantity value —
// never a created or duplicate result.
func TestBatchRegisterCLIQuantityBoundaries(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "reg.json")
	const max = "9223372036854775807"

	stdout, err := registerCLI(t, registry, "B-MAX", "P-7", max, "kg")
	if err != nil {
		t.Fatalf("the maximum quantity must register: %v", err)
	}
	if res := decodeRegisterResult(t, stdout); res.Status != "created" || res.Quantity != 9223372036854775807 {
		t.Fatalf("got %+v, want created with quantity 9223372036854775807", res)
	}

	stdout, err = registerCLI(t, registry, "B-MAX", "P-7", max, "kg")
	if err != nil {
		t.Fatalf("the maximum quantity must confirm as duplicate: %v", err)
	}
	if res := decodeRegisterResult(t, stdout); res.Status != "duplicate" || res.Quantity != 9223372036854775807 {
		t.Fatalf("got %+v, want duplicate with quantity 9223372036854775807", res)
	}

	content, pinned := pinRegistry(t, registry)

	stdout, err = registerCLI(t, registry, "B-MAX", "P-7", "9223372036854775806", "kg")
	if err == nil {
		t.Fatal("one less than the stored quantity must conflict, not confirm")
	}
	if stdout != "" {
		t.Fatalf("stdout must stay empty on conflict, got %q", stdout)
	}
	if msg := err.Error(); !strings.Contains(msg, strconv.Quote("B-MAX")) || !strings.Contains(msg, "quantity") {
		t.Fatalf("error must name the batch and the quantity field: %v", msg)
	}
	assertRegistryUntouched(t, registry, content, pinned)

	stdout, err = registerCLI(t, registry, "B-OVER", "P-7", "9223372036854775808", "kg")
	if err == nil {
		t.Fatal("a quantity above the maximum must be rejected")
	}
	if stdout != "" {
		t.Fatalf("stdout must stay empty on invalid input, got %q", stdout)
	}
	if msg := err.Error(); !strings.Contains(msg, "--quantity") || !strings.Contains(msg, max) {
		t.Fatalf("error must reject --quantity against the maximum %s: %v", max, msg)
	}
	assertRegistryUntouched(t, registry, content, pinned)

	if batches := readStoredBatches(t, registry); len(batches) != 1 || batches[0].Quantity != 9223372036854775807 {
		t.Fatalf("failures must not add or alter records: %v", batches)
	}
}
