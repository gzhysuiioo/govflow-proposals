package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This file is the end-to-end regression net for duplicate handling of the
// batch-register command: arguments go in through the real runBatchRegister
// entry point (flag parsing, normalization and Load/Register/Save included),
// and both the stdout result and the registry file on disk are asserted.
//
// The rules under guard:
//
//   - the first legal registration reports "created"; repeating the same
//     record reports the stored four fields and "duplicate", with exactly one
//     JSON object on stdout, and neither adds a record nor rewrites the file;
//   - duplicates are matched on effective values: end whitespace is trimmed
//     from the text flags and a leading-zero quantity compares by integer
//     value, while interior whitespace and casing stay significant (and a
//     differently cased batch id is a different batch, not a conflict);
//   - any differing product/quantity/unit fails the registration with empty
//     stdout and an error naming the batch and EVERY differing field, without
//     overwriting the stored record or touching other batches' fields or
//     order;
//   - a duplicate must not re-serialize the registry even when its original
//     indentation or field order differs from how Save would write it, so
//     bytes and modification time both stay put;
//   - quantity is compared as an integer right up to the signed 64-bit
//     maximum; one unit less conflicts, one unit more is an invalid
//     --quantity parameter rather than a success or a duplicate.

// registerResult is the single JSON object batch-register prints.
type registerResult struct {
	Batch    string `json:"batch"`
	Product  string `json:"product"`
	Quantity int64  `json:"quantity"`
	Unit     string `json:"unit"`
	Status   string `json:"status"`
}

// registerStoredFile reads the registry file back; Status stays zero for
// stored records.
type registerStoredFile struct {
	Version int              `json:"version"`
	Batches []registerResult `json:"batches"`
}

func registerArgs(registry, batch, product, quantity, unit string) []string {
	return []string{
		"--registry", registry,
		"--batch", batch,
		"--product", product,
		"--quantity", quantity,
		"--unit", unit,
	}
}

// decodeSingleRegisterResult fails the test unless stdout holds exactly one
// JSON object (a second JSON value after the first is an error), and returns
// it. Trailing whitespace, including the printed newline, is allowed.
func decodeSingleRegisterResult(t *testing.T, stdout *bytes.Buffer) registerResult {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	var got registerResult
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("stdout must contain one JSON object: %v (stdout=%q)", err, stdout.String())
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		t.Fatalf("stdout must contain exactly one JSON object, but more followed: %q (stdout=%q)", extra, stdout.String())
	}
	return got
}

func readRegisterFile(t *testing.T, path string) registerStoredFile {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored registerStoredFile
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatalf("registry is not valid JSON: %v (bytes=%s)", err, data)
	}
	if stored.Version != 1 {
		t.Fatalf("registry version = %d, want 1", stored.Version)
	}
	return stored
}

// pinRegisterFileTime stamps path with a distinctive past mtime so any
// rewrite is visible even on filesystems with coarse timestamp granularity,
// and returns the stamp to compare against later.
func pinRegisterFileTime(t *testing.T, path string) time.Time {
	t.Helper()
	pinned := time.Date(2001, time.February, 3, 4, 5, 6, 0, time.UTC)
	if err := os.Chtimes(path, pinned, pinned); err != nil {
		t.Fatal(err)
	}
	return pinned
}

func assertRegisterFileUnchanged(t *testing.T, path string, wantBytes []byte, wantMtime time.Time) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(wantBytes, got) {
		t.Fatalf("registry bytes changed:\nbefore: %s\nafter:  %s", wantBytes, got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(wantMtime) {
		t.Fatalf("registry mtime changed: got %v, want %v", info.ModTime(), wantMtime)
	}
}

func assertStoredBatch(t *testing.T, got registerResult, want registerResult) {
	t.Helper()
	if got.Batch != want.Batch || got.Product != want.Product ||
		got.Quantity != want.Quantity || got.Unit != want.Unit {
		t.Fatalf("stored record for %q = {batch:%q product:%q quantity:%d unit:%q}, want {batch:%q product:%q quantity:%d unit:%q}",
			want.Batch, got.Batch, got.Product, got.Quantity, got.Unit,
			want.Batch, want.Product, want.Quantity, want.Unit)
	}
}

// First registration creates; the effective-value repeat confirms a duplicate
// and leaves the one record for that batch and all other batches (fields and
// order) exactly where they were, without rewriting the file.
func TestBatchRegisterDuplicateEndToEnd(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "reg.json")
	writeFile(t, registry, `{"version":1,"batches":[
  {"batch":"A-1","product":"P-1","quantity":5,"unit":"box"},
  {"batch":"A-2","product":"P-2","quantity":9,"unit":"L"}
]}`)

	var stdout bytes.Buffer
	if err := runBatchRegister(registerArgs(registry, " B-001 ", " P-7 ", "000120", " kg "), &stdout); err != nil {
		t.Fatalf("first registration failed: %v", err)
	}
	if got := decodeSingleRegisterResult(t, &stdout); got != (registerResult{
		Batch: "B-001", Product: "P-7", Quantity: 120, Unit: "kg", Status: "created",
	}) {
		t.Fatalf("first result = %+v", got)
	}

	stored := readRegisterFile(t, registry)
	if len(stored.Batches) != 3 {
		t.Fatalf("got %d records after create: %+v", len(stored.Batches), stored.Batches)
	}
	if stored.Batches[0].Batch != "A-1" || stored.Batches[1].Batch != "A-2" || stored.Batches[2].Batch != "B-001" {
		t.Fatalf("new batch not appended after existing ones: %+v", stored.Batches)
	}

	// Snapshot after the create; the duplicate confirmation must not rewrite.
	before, err := os.ReadFile(registry)
	if err != nil {
		t.Fatal(err)
	}
	pinned := pinRegisterFileTime(t, registry)

	// Trimmed text flags and the quantity without leading zeros still name
	// the same record once normalized.
	stdout.Reset()
	if err := runBatchRegister(registerArgs(registry, "B-001", "P-7", "120", "kg"), &stdout); err != nil {
		t.Fatalf("identical repeat must succeed: %v", err)
	}
	if got := decodeSingleRegisterResult(t, &stdout); got != (registerResult{
		Batch: "B-001", Product: "P-7", Quantity: 120, Unit: "kg", Status: "duplicate",
	}) {
		t.Fatalf("duplicate result = %+v", got)
	}

	assertRegisterFileUnchanged(t, registry, before, pinned)
	stored = readRegisterFile(t, registry)
	if len(stored.Batches) != 3 {
		t.Fatalf("duplicate must not add a record, got %d: %+v", len(stored.Batches), stored.Batches)
	}
	other := map[string]registerResult{
		"A-1": {Batch: "A-1", Product: "P-1", Quantity: 5, Unit: "box"},
		"A-2": {Batch: "A-2", Product: "P-2", Quantity: 9, Unit: "L"},
	}
	for i, want := range []registerResult{
		other["A-1"], other["A-2"],
		{Batch: "B-001", Product: "P-7", Quantity: 120, Unit: "kg"},
	} {
		assertStoredBatch(t, stored.Batches[i], want)
	}
}

// A duplicate must not re-save the registry just to canonicalize its output:
// a file whose indentation, spacing and field order differ from Save's own
// style keeps its exact bytes and mtime, and the printed fields come from the
// stored record.
func TestBatchRegisterDuplicateKeepsUnusualFileStyle(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "reg.json")
	content := "{\n" +
		"\t\"batches\": [\n" +
		"\t\t{\"unit\": \"box\", \"quantity\": 7, \"product\": \"P-8\", \"batch\": \"A-9\"},\n" +
		"\t\t{ \"batch\" : \"B-001\" , \"product\" : \"P-7\" , \"quantity\" : 120 , \"unit\" : \"kg\" }\n" +
		"\t],\n" +
		"\t\"version\": 1\n" +
		"}\n"
	writeFile(t, registry, content)
	pinned := pinRegisterFileTime(t, registry)

	var stdout bytes.Buffer
	if err := runBatchRegister(registerArgs(registry, " B-001 ", " P-7 ", "000120", " kg "), &stdout); err != nil {
		t.Fatalf("duplicate against an unusually formatted file failed: %v", err)
	}
	if got := decodeSingleRegisterResult(t, &stdout); got != (registerResult{
		Batch: "B-001", Product: "P-7", Quantity: 120, Unit: "kg", Status: "duplicate",
	}) {
		t.Fatalf("duplicate result = %+v", got)
	}

	assertRegisterFileUnchanged(t, registry, []byte(content), pinned)
	stored := readRegisterFile(t, registry)
	if len(stored.Batches) != 2 {
		t.Fatalf("got %d records: %+v", len(stored.Batches), stored.Batches)
	}
	// Original field content and ordering survive untouched.
	assertStoredBatch(t, stored.Batches[0], registerResult{Batch: "A-9", Product: "P-8", Quantity: 7, Unit: "box"})
	assertStoredBatch(t, stored.Batches[1], registerResult{Batch: "B-001", Product: "P-7", Quantity: 120, Unit: "kg"})
}

// Effective-value conflicts: casing and interior whitespace in product/unit
// matter, every differing field must be named in the same error, stdout must
// stay empty, and the stored record plus unrelated batches must survive in
// place. A differently cased batch id is a separate batch instead.
func TestBatchRegisterDuplicateConflictsAreRejected(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "reg.json")

	create := func(batch, product, quantity, unit string) {
		t.Helper()
		var stdout bytes.Buffer
		if err := runBatchRegister(registerArgs(registry, batch, product, quantity, unit), &stdout); err != nil {
			t.Fatalf("setup registration %q failed: %v", batch, err)
		}
	}
	create(" B-001 ", " P-7 ", "000120", " kg ")
	create("C-1", "P-9", "3", "box")

	before, err := os.ReadFile(registry)
	if err != nil {
		t.Fatal(err)
	}
	pinned := pinRegisterFileTime(t, registry)

	cases := []struct {
		name             string
		product          string
		quantity         string
		unit             string
		wantFieldListing string
	}{
		{"product casing differs", "p-7", "120", "kg", "product"},
		{"product interior whitespace", "P- 7", "120", "kg", "product"},
		{"unit interior whitespace", "P-7", "120", "k g", "unit"},
		{"quantity integer differs", "P-7", "121", "kg", "quantity"},
		{"all three fields differ", "P-8", "4", "g", "product, quantity, unit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			err := runBatchRegister(registerArgs(registry, "B-001", tc.product, tc.quantity, tc.unit), &stdout)
			if err == nil {
				t.Fatal("conflicting registration must fail")
			}
			msg := err.Error()
			if !strings.Contains(msg, `batch "B-001"`) {
				t.Fatalf("error must name the batch: %v", err)
			}
			wantFragment := "conflicting field(s): " + tc.wantFieldListing + ";"
			if !strings.Contains(msg, wantFragment) {
				t.Fatalf("error must list exactly %q: %v", wantFragment, err)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout must stay empty on failure, got %q", stdout.String())
			}
			assertRegisterFileUnchanged(t, registry, before, pinned)
		})
	}

	// Every rejection left the stored record and the unrelated batch intact.
	stored := readRegisterFile(t, registry)
	if len(stored.Batches) != 2 {
		t.Fatalf("conflicts must not add or remove records: %+v", stored.Batches)
	}
	assertStoredBatch(t, stored.Batches[0], registerResult{Batch: "B-001", Product: "P-7", Quantity: 120, Unit: "kg"})
	assertStoredBatch(t, stored.Batches[1], registerResult{Batch: "C-1", Product: "P-9", Quantity: 3, Unit: "box"})

	// Casing in the batch id distinguishes batches: "b-001" is created rather
	// than treated as a conflict on "B-001", and then confirms as its own
	// duplicate.
	var stdout bytes.Buffer
	if err := runBatchRegister(registerArgs(registry, "b-001", "P-7", "120", "kg"), &stdout); err != nil {
		t.Fatalf("differently cased batch id must register: %v", err)
	}
	if got := decodeSingleRegisterResult(t, &stdout); got.Status != "created" || got.Batch != "b-001" {
		t.Fatalf("lower-cased batch result = %+v", got)
	}
	stdout.Reset()
	if err := runBatchRegister(registerArgs(registry, "b-001", "P-7", "120", "kg"), &stdout); err != nil {
		t.Fatalf("repeat of the lower-cased batch failed: %v", err)
	}
	if got := decodeSingleRegisterResult(t, &stdout); got.Status != "duplicate" {
		t.Fatalf("repeat status = %q, want duplicate", got.Status)
	}

	stored = readRegisterFile(t, registry)
	if len(stored.Batches) != 3 {
		t.Fatalf("got %d records: %+v", len(stored.Batches), stored.Batches)
	}
	if stored.Batches[0].Batch != "B-001" || stored.Batches[1].Batch != "C-1" || stored.Batches[2].Batch != "b-001" {
		t.Fatalf("batch order changed: %+v", stored.Batches)
	}
	assertStoredBatch(t, stored.Batches[0], registerResult{Batch: "B-001", Product: "P-7", Quantity: 120, Unit: "kg"})
	assertStoredBatch(t, stored.Batches[2], registerResult{Batch: "b-001", Product: "P-7", Quantity: 120, Unit: "kg"})
}

// Quantity compares by integer value at the signed 64-bit ceiling: the max
// registers and confirms a duplicate (also under leading zeros and padded
// text), max-1 is a quantity conflict, and anything above the max is an
// invalid --quantity parameter that never reaches a success or duplicate.
func TestBatchRegisterQuantityAtUpperBound(t *testing.T) {
	const (
		maxQuantity = int64(9223372036854775807)
		maxText     = "9223372036854775807"
		maxMinusOne = "9223372036854775806"
	)
	dir := t.TempDir()
	registry := filepath.Join(dir, "reg.json")

	var stdout bytes.Buffer
	if err := runBatchRegister(registerArgs(registry, "B-max", "P-7", maxText, "kg"), &stdout); err != nil {
		t.Fatalf("registering the upper bound failed: %v", err)
	}
	if got := decodeSingleRegisterResult(t, &stdout); got != (registerResult{
		Batch: "B-max", Product: "P-7", Quantity: maxQuantity, Unit: "kg", Status: "created",
	}) {
		t.Fatalf("created result = %+v", got)
	}

	before, err := os.ReadFile(registry)
	if err != nil {
		t.Fatal(err)
	}
	pinned := pinRegisterFileTime(t, registry)

	// Same integer under leading zeros, with whitespace-padded text flags.
	stdout.Reset()
	if err := runBatchRegister(registerArgs(registry, " B-max ", " P-7 ", "000"+maxText, " kg "), &stdout); err != nil {
		t.Fatalf("max quantity repeat must confirm: %v", err)
	}
	if got := decodeSingleRegisterResult(t, &stdout); got != (registerResult{
		Batch: "B-max", Product: "P-7", Quantity: maxQuantity, Unit: "kg", Status: "duplicate",
	}) {
		t.Fatalf("duplicate result = %+v", got)
	}
	assertRegisterFileUnchanged(t, registry, before, pinned)

	// One less is a content conflict on quantity only.
	stdout.Reset()
	err = runBatchRegister(registerArgs(registry, "B-max", "P-7", maxMinusOne, "kg"), &stdout)
	if err == nil {
		t.Fatal("max-1 must conflict")
	}
	msg := err.Error()
	if !strings.Contains(msg, `batch "B-max"`) ||
		!strings.Contains(msg, "conflicting field(s): quantity;") {
		t.Fatalf("error must name batch and quantity conflict: %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout must stay empty, got %q", stdout.String())
	}
	assertRegisterFileUnchanged(t, registry, before, pinned)
	stored := readRegisterFile(t, registry)
	if len(stored.Batches) != 1 {
		t.Fatalf("conflict must not add a record: %+v", stored.Batches)
	}
	assertStoredBatch(t, stored.Batches[0], registerResult{Batch: "B-max", Product: "P-7", Quantity: maxQuantity, Unit: "kg"})

	// Anything above the bound is an invalid parameter: it must not be parsed
	// as a record, reported as a conflict, or touch the file.
	for _, overflow := range []string{"9223372036854775808", "99999999999999999999999"} {
		t.Run("overflow "+overflow, func(t *testing.T) {
			var stdout bytes.Buffer
			err := runBatchRegister(registerArgs(registry, "B-max", "P-7", overflow, "kg"), &stdout)
			if err == nil {
				t.Fatal("above-max quantity must be rejected")
			}
			msg := err.Error()
			if !strings.Contains(msg, "invalid --quantity") {
				t.Fatalf("error must identify the invalid --quantity parameter: %v", err)
			}
			if strings.Contains(msg, "already registered") {
				t.Fatalf("out-of-range quantity must not reach duplicate/conflict handling: %v", err)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout must stay empty, got %q", stdout.String())
			}
			assertRegisterFileUnchanged(t, registry, before, pinned)
		})
	}
}
