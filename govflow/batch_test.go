package govflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func register(t *testing.T, path, batch, product, quantity, unit string) *Registration {
	t.Helper()
	result, err := RegisterBatch(path, batch, product, quantity, unit)
	if err != nil {
		t.Fatalf("RegisterBatch(%q, %q, %q, %q): %v", batch, product, quantity, unit, err)
	}
	return result
}

func TestRegisterNewAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reg.jsonl")

	result := register(t, path, "B001", "P100", "100", "箱")
	if result.Duplicate {
		t.Fatal("first registration must not be a duplicate")
	}
	if result.Record != (BatchRecord{Batch: "B001", Product: "P100", Quantity: 100, Unit: "箱"}) {
		t.Fatalf("unexpected record: %+v", result.Record)
	}

	// Simulate a process restart: read the file back from disk.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("registry file not created: %v", err)
	}
	want := "{\"batch\":\"B001\",\"product\":\"P100\",\"quantity\":100,\"unit\":\"箱\"}\n"
	if string(data) != want {
		t.Fatalf("file content = %q, want %q", data, want)
	}
}

func TestRegisterDuplicateIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reg.jsonl")
	register(t, path, "B001", "P100", "100", "箱")

	before, _ := os.ReadFile(path)
	result := register(t, path, "B001", "P100", "100", "箱")
	if !result.Duplicate {
		t.Fatal("identical resubmission must be reported as duplicate")
	}
	if result.Record != (BatchRecord{Batch: "B001", Product: "P100", Quantity: 100, Unit: "箱"}) {
		t.Fatalf("duplicate must return the existing record, got %+v", result.Record)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("duplicate registration must not modify the registry file")
	}
}

func TestDuplicateWithLeadingZerosAndWhitespace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reg.jsonl")
	register(t, path, "  B001  ", " P100 ", "007", " 箱 ")

	// Normalizes to the same values: leading zeros compare by integer value,
	// surrounding whitespace is stripped.
	result := register(t, path, "B001", "P100", "7", "箱")
	if !result.Duplicate {
		t.Fatal("whitespace/leading-zero normalization should match the existing record")
	}
	if result.Record.Quantity != 7 {
		t.Fatalf("stored quantity = %d, want 7", result.Record.Quantity)
	}
}

func TestBatchNumbersAreCaseSensitive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reg.jsonl")
	register(t, path, "b1", "P1", "1", "个")
	result := register(t, path, "B1", "P1", "1", "个")
	if result.Duplicate {
		t.Fatal("batch numbers differing only in case must be distinct")
	}
	if result.Record.Batch != "B1" {
		t.Fatalf("got %q", result.Record.Batch)
	}
}

func TestConflictRejectedAndRecordUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reg.jsonl")
	register(t, path, "B001", "P100", "100", "箱")

	before, _ := os.ReadFile(path)
	_, err := RegisterBatch(path, "B001", "P100", "200", "箱")
	if err == nil {
		t.Fatal("quantity change must be rejected")
	}
	if !strings.Contains(err.Error(), "B001") || !strings.Contains(err.Error(), "quantity") {
		t.Fatalf("error must name batch and differing field, got: %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("rejected registration must not modify the registry file")
	}

	// Product and unit differences are reported too.
	_, err = RegisterBatch(path, "B001", "P200", "100", "箱")
	if err == nil || !strings.Contains(err.Error(), "product") {
		t.Fatalf("product mismatch must be reported, got: %v", err)
	}
	_, err = RegisterBatch(path, "B001", "P100", "100", "个")
	if err == nil || !strings.Contains(err.Error(), "unit") {
		t.Fatalf("unit mismatch must be reported, got: %v", err)
	}
	// Multiple differing fields are all named.
	_, err = RegisterBatch(path, "B001", "P200", "200", "个")
	if err == nil || !strings.Contains(err.Error(), "product") || !strings.Contains(err.Error(), "quantity") || !strings.Contains(err.Error(), "unit") {
		t.Fatalf("all differing fields must be named, got: %v", err)
	}
}

func TestMultipleBatchesPreserved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reg.jsonl")
	register(t, path, "B001", "P100", "100", "箱")
	register(t, path, "B002", "P200", "200", "个")

	data, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("file must contain both records, got %d lines: %q", len(lines), data)
	}
	if !strings.Contains(lines[0], `"batch":"B001"`) || !strings.Contains(lines[1], `"batch":"B002"`) {
		t.Fatalf("records not preserved in order: %q", data)
	}
}

func TestRegistriesAreIndependent(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.jsonl")
	b := filepath.Join(dir, "b.jsonl")
	register(t, a, "B001", "P100", "100", "箱")

	// Same batch number in a different file is a new registration.
	result := register(t, b, "B001", "P999", "1", "个")
	if result.Duplicate {
		t.Fatal("different registry files must manage batch numbers independently")
	}
}

func TestInvalidInputsRejectedBeforeFileCreation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "reg.jsonl")

	cases := []struct {
		name     string
		batch    string
		product  string
		quantity string
		unit     string
	}{
		{"empty batch", "   ", "P1", "1", "箱"},
		{"empty product", "B1", "\t", "1", "箱"},
		{"empty unit", "B1", "P1", "1", " \n "},
		{"empty quantity", "B1", "P1", "", "箱"},
		{"zero", "B1", "P1", "0", "箱"},
		{"negative", "B1", "P1", "-5", "箱"},
		{"fraction", "B1", "P1", "1.5", "箱"},
		{"exponent", "B1", "P1", "1e3", "箱"},
		{"plus sign", "B1", "P1", "+5", "箱"},
		{"whitespace in quantity", "B1", "P1", " 5", "箱"},
		{"out of range", "B1", "P1", "9223372036854775808", "箱"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := RegisterBatch(path, tc.batch, tc.product, tc.quantity, tc.unit); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("registry file must not be created on invalid input, stat err=%v", err)
	}
}

func TestQuantityBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reg.jsonl")
	result := register(t, path, "MAX", "P1", "9223372036854775807", "箱")
	if result.Record.Quantity != 9223372036854775807 {
		t.Fatalf("max int64 quantity = %d", result.Record.Quantity)
	}
	if _, err := RegisterBatch(path, "OVER", "P1", "9223372036854775808", "箱"); err == nil {
		t.Fatal("quantity above max int64 must be rejected")
	}
}

func TestExistingEmptyFileRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reg.jsonl")
	if err := os.WriteFile(path, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := RegisterBatch(path, "B1", "P1", "1", "箱"); err == nil {
		t.Fatal("existing empty file must be rejected, not overwritten")
	}
}

func TestExistingWhitespaceOnlyFileRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reg.jsonl")
	if err := os.WriteFile(path, []byte("\n  \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := RegisterBatch(path, "B1", "P1", "1", "箱"); err == nil {
		t.Fatal("whitespace-only file must be rejected")
	}
}

func TestMalformedFileRejected(t *testing.T) {
	dir := t.TempDir()
	for _, content := range []string{
		"not json\n",
		"{\"batch\":\"B1\"}\n",
		"{\"batch\":\"B1\",\"product\":\"P1\",\"quantity\":1,\"unit\":\"箱\",\"extra\":2}\n",
		"{\"batch\":\"B1\",\"product\":\"P1\",\"quantity\":1.5,\"unit\":\"箱\"}\n",
		"{\"batch\":\"B1\",\"product\":\"P1\",\"quantity\":0,\"unit\":\"箱\"}\n",
		"{\"batch\":\"\",\"product\":\"P1\",\"quantity\":1,\"unit\":\"箱\"}\n",
		"{\"batch\":\"B1\",\"product\":\"P1\",\"quantity\":1,\"unit\":\"箱\"}\n\n",
	} {
		path := filepath.Join(dir, "reg.jsonl")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := RegisterBatch(path, "B2", "P2", "2", "个"); err == nil {
			t.Fatalf("malformed content %q must be rejected", content)
		}
	}
}

func TestDuplicateBatchInFileRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reg.jsonl")
	content := "{\"batch\":\"B1\",\"product\":\"P1\",\"quantity\":1,\"unit\":\"箱\"}\n" +
		"{\"batch\":\"B1\",\"product\":\"P2\",\"quantity\":2,\"unit\":\"个\"}\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := RegisterBatch(path, "B2", "P3", "3", "箱"); err == nil {
		t.Fatal("file containing duplicate batch numbers must be rejected")
	}
	// The file must be untouched after the rejected read.
	data, _ := os.ReadFile(path)
	if string(data) != content {
		t.Fatal("rejected registration must not modify the file")
	}
}

func TestUnreadableFileReported(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "reg.jsonl")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := RegisterBatch(path, "B1", "P1", "1", "箱"); err == nil {
		t.Fatal("unreadable registry path must be reported")
	}
}

func TestSavedRecordRemainsAvailableAfterFailedSave(t *testing.T) {
	// Point the registry at a path whose parent directory does not exist:
	// reading fails with IsNotExist, then creating the temp file fails.
	path := filepath.Join(t.TempDir(), "missing-dir", "reg.jsonl")
	if _, err := RegisterBatch(path, "B1", "P1", "1", "箱"); err == nil {
		t.Fatal("save failure must be reported")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("no registry file should be left behind on save failure")
	}
}
