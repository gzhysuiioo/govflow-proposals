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

// End-to-end counterparts of the batch-import conflict-source regression
// tests: they drive runBatchImport exactly as the command does, so they also
// pin the error wording, the empty-stdout/non-zero-exit failure contract, the
// read-only manifest, and registry preservation (bytes and mtime) through the
// whole flag -> parse -> import -> save path.

// pinRegistryMtime stamps path with a distinctive past mtime so any rewrite
// is observable even on filesystems with coarse timestamp granularity.
func pinRegistryMtime(t *testing.T, path string) time.Time {
	t.Helper()
	pinned := time.Date(2001, time.February, 3, 4, 5, 6, 0, time.UTC)
	if err := os.Chtimes(path, pinned, pinned); err != nil {
		t.Fatal(err)
	}
	return pinned
}

func assertFileUnchanged(t *testing.T, path string, want []byte, wantMtime time.Time) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s was modified:\nbefore: %s\nafter:  %s", path, want, got)
	}
	if !wantMtime.IsZero() {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.ModTime().Equal(wantMtime) {
			t.Fatalf("%s mtime changed: got %v, want %v", path, info.ModTime(), wantMtime)
		}
	}
}

// The specification's four-record scenario: record 1 introduces B1 into a
// not-yet-existing registry, record 2 registers another batch, record 3
// submits B1 again identically, and record 4 changes B1's quantity. The
// whole import must be rejected and the error must name record 4, B1 and
// quantity, cite the conflict with manifest record 1 (not the record 3
// confirmation), and neither file may be created or altered.
func TestBatchImportCLIConflictAfterRepeatedConfirmsAnchorsToFirstRecord(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "reg.json")
	manifest := filepath.Join(dir, "in.json")
	manifestContent := `[
  {"batch": "B1", "product": "P-7", "quantity": 10, "unit": "kg"},
  {"batch": "OTHER", "product": "P-8", "quantity": 1, "unit": "box"},
  {"batch": "B1", "product": "P-7", "quantity": 10, "unit": "kg"},
  {"batch": "B1", "product": "P-7", "quantity": 99, "unit": "kg"}
]
`
	writeFile(t, manifest, manifestContent)

	var stdout bytes.Buffer
	err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout)
	if err == nil {
		t.Fatal("conflicting manifest must be rejected")
	}
	msg := err.Error()
	for _, want := range []string{
		`manifest record 4 (batch "B1")`,
		"conflicts with manifest record 1",
		"field(s): quantity",
		"the whole manifest is rejected",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error must contain %q: %v", want, msg)
		}
	}
	if strings.Contains(msg, "record 3") {
		t.Fatalf("the middle duplicate confirmation must not be cited as the source: %v", msg)
	}
	if strings.Contains(msg, "registered record") {
		t.Fatalf("a manifest-introduced batch must not be blamed on the registry: %v", msg)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout must stay empty on failure, got %q", stdout.String())
	}
	if _, statErr := os.Stat(registry); !os.IsNotExist(statErr) {
		t.Fatal("a rejected import must not create the registry file")
	}
	assertFileUnchanged(t, manifest, []byte(manifestContent), time.Time{})
}

// When B1 was registered before the import, the identical manifest records
// merely confirm that stored record — even repeatedly. A later record
// differing in product, quantity and unit must always report the conflict
// against the registered record (listing all three fields in order), never
// against a manifest record, and must leave the registry bytes and mtime
// untouched.
func TestBatchImportCLIConflictWithRegistryAfterRepeatedConfirms(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "reg.json")
	manifest := filepath.Join(dir, "in.json")
	original := `{"version":1,"batches":[{"batch":"B1","product":"P-7","quantity":10,"unit":"kg"}]}`
	writeFile(t, registry, original)
	pinned := pinRegistryMtime(t, registry)
	manifestContent := `[
  {"batch": "B1", "product": "P-7", "quantity": 10, "unit": "kg"},
  {"batch": "OTHER", "product": "P-8", "quantity": 1, "unit": "box"},
  {"batch": "B1", "product": "P-7", "quantity": 10, "unit": "kg"},
  {"batch": "B1", "product": "P-9", "quantity": 99, "unit": "m"}
]
`
	writeFile(t, manifest, manifestContent)

	var stdout bytes.Buffer
	err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout)
	if err == nil {
		t.Fatal("conflicting manifest must be rejected")
	}
	msg := err.Error()
	for _, want := range []string{
		`manifest record 4 (batch "B1")`,
		"conflicts with the registered record",
		"field(s): product, quantity, unit",
		"the whole manifest is rejected",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error must contain %q: %v", want, msg)
		}
	}
	if strings.Contains(msg, "conflicts with manifest record") {
		t.Fatalf("pre-registered batch must not be blamed on a manifest record: %v", msg)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout must stay empty on failure, got %q", stdout.String())
	}
	assertFileUnchanged(t, registry, []byte(original), pinned)
	assertFileUnchanged(t, manifest, []byte(manifestContent), time.Time{})
}

// Manifest batch ids are compared with leading and trailing whitespace
// stripped, so padded spellings still resolve to the same first occurrence;
// the differing fourth record must conflict with manifest record 1 even
// though the padding differs between records.
func TestBatchImportCLIPaddedBatchIDConflictAnchorsToFirstRecord(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "reg.json")
	manifest := filepath.Join(dir, "in.json")
	manifestContent := "[\n" +
		"  {\"batch\": \" B1 \", \"product\": \"P-7\", \"quantity\": 10, \"unit\": \"kg\"},\n" +
		"  {\"batch\": \"OTHER\", \"product\": \"P-8\", \"quantity\": 1, \"unit\": \"box\"},\n" +
		"  {\"batch\": \"B1\", \"product\": \"P-7\", \"quantity\": 10, \"unit\": \"kg\"},\n" +
		"  {\"batch\": \"\\tB1\\t\", \"product\": \"P-7\", \"quantity\": 99, \"unit\": \"kg\"}\n" +
		"]\n"
	writeFile(t, manifest, manifestContent)

	var stdout bytes.Buffer
	err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout)
	if err == nil {
		t.Fatal("conflicting manifest must be rejected")
	}
	msg := err.Error()
	for _, want := range []string{
		`manifest record 4 (batch "B1")`,
		"conflicts with manifest record 1",
		"quantity",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error must contain %q: %v", want, msg)
		}
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout must stay empty on failure, got %q", stdout.String())
	}
	if _, statErr := os.Stat(registry); !os.IsNotExist(statErr) {
		t.Fatal("a rejected import must not create the registry file")
	}
	assertFileUnchanged(t, manifest, []byte(manifestContent), time.Time{})
}

// Success counterpart: with the final record changed back to the identical
// content, the import returns one JSON object whose results follow manifest
// order — the first occurrence is "created", later identical records and the
// pre-registered record are "duplicate" — and the registry keeps exactly one
// copy of each new batch, stored content unchanged, old records first and new
// ones appended in first-occurrence order. The manifest stays read-only.
func TestBatchImportCLIRepeatedConfirmsThenIdenticalSucceeds(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "reg.json")
	manifest := filepath.Join(dir, "in.json")
	original := `{"version":1,"batches":[{"batch":"OLD","product":"P0","quantity":7,"unit":"box"}]}`
	writeFile(t, registry, original)
	manifestContent := `[
  {"batch": " B1 ", "product": "P-7", "quantity": 10, "unit": "kg"},
  {"batch": "OTHER", "product": "P-8", "quantity": 1, "unit": "box"},
  {"batch": "B1", "product": "P-7", "quantity": 10, "unit": "kg"},
  {"batch": "OLD", "product": "P0", "quantity": 7, "unit": "box"},
  {"batch": " B1 ", "product": "P-7", "quantity": 10, "unit": "kg"}
]
`
	writeFile(t, manifest, manifestContent)

	var stdout bytes.Buffer
	if err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout); err != nil {
		t.Fatalf("an all-matching manifest must import: %v", err)
	}
	var out importOutput
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &out); err != nil {
		t.Fatalf("stdout is not the expected JSON: %v (%q)", err, stdout.String())
	}
	want := []struct {
		batch   string
		status  string
		product string
		qty     int64
		unit    string
	}{
		{"B1", "created", "P-7", 10, "kg"},
		{"OTHER", "created", "P-8", 1, "box"},
		{"B1", "duplicate", "P-7", 10, "kg"},
		{"OLD", "duplicate", "P0", 7, "box"},
		{"B1", "duplicate", "P-7", 10, "kg"},
	}
	if len(out.Results) != len(want) {
		t.Fatalf("got %d results, want %d", len(out.Results), len(want))
	}
	for i, w := range want {
		r := out.Results[i]
		if r.Batch != w.batch || r.Status != w.status || r.Product != w.product ||
			r.Quantity != w.qty || r.Unit != w.unit {
			t.Errorf("result %d = %+v, want batch %q status %q %s/%d/%s",
				i+1, r, w.batch, w.status, w.product, w.qty, w.unit)
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
			Product  string `json:"product"`
			Quantity int64  `json:"quantity"`
			Unit     string `json:"unit"`
		} `json:"batches"`
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Version != 1 {
		t.Fatalf("unexpected version: %s", data)
	}
	gotIDs := make([]string, len(stored.Batches))
	b1Count := 0
	for i, b := range stored.Batches {
		gotIDs[i] = b.Batch
		if b.Batch == "B1" {
			b1Count++
			if b.Product != "P-7" || b.Quantity != 10 || b.Unit != "kg" {
				t.Errorf("B1 stored with altered content: %+v", b)
			}
		}
	}
	wantIDs := []string{"OLD", "B1", "OTHER"}
	if strings.Join(gotIDs, ",") != strings.Join(wantIDs, ",") {
		t.Fatalf("stored batches = %v, want %v (old first, new in first-occurrence order)", gotIDs, wantIDs)
	}
	if b1Count != 1 {
		t.Fatalf("B1 stored %d times, want exactly once: %s", b1Count, data)
	}
	assertFileUnchanged(t, manifest, []byte(manifestContent), time.Time{})
}
