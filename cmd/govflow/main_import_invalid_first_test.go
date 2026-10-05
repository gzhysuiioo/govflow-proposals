package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// End-to-end regression for the error ORDER batch-import promises when one
// manifest holds both an illegal record and a batch-content conflict: the
// whole manifest is validated record by record first, and only a fully legal
// manifest ever reaches the conflict check. An illegal record is therefore
// always the reported failure — even when a conflicting record sits earlier
// in the same manifest, and no matter whether the conflict is against the
// registered records or against an earlier record of the manifest itself.
// Moving the conflict and the illegal record around only moves the reported
// 1-based position; it never lets the conflict win.
//
// Every scenario runs the real command in a child process (the test binary
// re-invoked with -wrap.batch-import, no fault injected) so the assertions
// cover exactly what the user sees: a non-zero exit code, the reason on
// stderr, an empty stdout — and the files afterwards: the existing registry
// keeps its original bytes (layout and record order included) and its
// modification time, batches the manifest could have created are nowhere to
// be found, and the read-only manifest is byte-for-byte as submitted.

// invalidFirstRegistryJSON is the registry every scenario starts from, in a
// compact layout with a member order Save would never write, so any rewrite
// is visible in the bytes. B1/P1/10/kg is the batch the manifests conflict
// with; B0 keeps a second record in place.
const invalidFirstRegistryJSON = `{"batches":[{"unit":"kg","quantity":10,"product":"P1","batch":"B1"},{"batch":"B0","product":"P0","quantity":5,"unit":"box"}],"version":1}`

// invalidFirstStoredBatches is what the registry must still hold after every
// rejected import: exactly the two original records, in their original order.
var invalidFirstStoredBatches = []registerResult{
	{Batch: "B1", Product: "P1", Quantity: 10, Unit: "kg"},
	{Batch: "B0", Product: "P0", Quantity: 5, Unit: "box"},
}

// invalidFirstPinnedModTime is pinned onto the registry in every scenario so
// any rewrite — even one restoring identical bytes — is observable.
var invalidFirstPinnedModTime = time.Date(2012, time.October, 18, 13, 14, 15, 0, time.UTC)

// assertInvalidFirstFailure checks the user-visible failure: exit code 1, an
// empty stdout, and stderr naming the manifest path, the 1-based record
// position, the field and the reason — with the batch id cited exactly when
// the id is legal and unambiguous, and never a conflict report.
func assertInvalidFirstFailure(t *testing.T, stderr []byte, manifest, recordPos, batch, field, reason string) {
	t.Helper()
	msg := string(stderr)
	for _, want := range []string{
		"govflow: batch-import:",
		"invalid input file",
		manifest,
		recordPos,
		strconv.Quote(field),
		reason,
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("stderr must contain %q:\n%s", want, msg)
		}
	}
	if batch != "" {
		if !strings.Contains(msg, "(batch "+strconv.Quote(batch)+")") {
			t.Fatalf("stderr must cite batch %q:\n%s", batch, msg)
		}
	} else if strings.Contains(msg, "(batch \"") {
		t.Fatalf("no unambiguous batch id exists; stderr must not cite one:\n%s", msg)
	}
	if strings.Contains(msg, "conflicts with") {
		t.Fatalf("an illegal record must be reported before any content conflict:\n%s", msg)
	}
}

// assertInvalidFirstFilesUntouched checks the file results every rejected
// import must leave behind: the registry's bytes and mtime exactly as pinned,
// still holding only the original records, the manifest untouched and no
// save leftovers.
func assertInvalidFirstFilesUntouched(t *testing.T, registry, manifest string) {
	t.Helper()
	after, err := os.ReadFile(registry)
	if err != nil {
		t.Fatalf("the original registry must still exist: %v", err)
	}
	if string(after) != invalidFirstRegistryJSON {
		t.Fatalf("failed import changed the registry bytes:\nbefore: %s\nafter:  %s", invalidFirstRegistryJSON, after)
	}
	info, err := os.Stat(registry)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(invalidFirstPinnedModTime) {
		t.Fatalf("failed import changed the registry mtime: got %v, want %v",
			info.ModTime(), invalidFirstPinnedModTime)
	}
	stored := readStoredBatches(t, registry)
	if len(stored) != len(invalidFirstStoredBatches) {
		t.Fatalf("rejected import left the registry with %d records, want the original %d: %+v",
			len(stored), len(invalidFirstStoredBatches), stored)
	}
	for i, want := range invalidFirstStoredBatches {
		got := stored[i]
		got.Status = ""
		if got != want {
			t.Fatalf("original record %d = %+v, want %+v (a would-be new batch must not land)", i+1, got, want)
		}
	}
	assertNoSaveLeftovers(t, filepath.Dir(registry), filepath.Base(registry), filepath.Base(manifest))
}

// pinInvalidFirstRegistry pins the distinctive mtime every scenario asserts
// on, so even a byte-identical rewrite would be caught.
func pinInvalidFirstRegistry(t *testing.T, registry string) {
	t.Helper()
	if err := os.Chtimes(registry, invalidFirstPinnedModTime, invalidFirstPinnedModTime); err != nil {
		t.Fatal(err)
	}
}

// TestBatchImportCLIInvalidRecordBeatsConflict runs the order guarantee
// through its decisive shapes: a conflict with the registered record or
// inside the manifest itself, sitting before or after the illegal record;
// several illegal records in one manifest; and an illegal record whose batch
// id cannot be cited. Whatever the shape, the illegal record is the answer.
func TestBatchImportCLIInvalidRecordBeatsConflict(t *testing.T) {
	cases := []struct {
		name      string
		manifest  string
		recordPos string // 1-based position wording expected on stderr
		batch     string  // id the error must cite; "" when none may be cited
		field     string
		reason    string
	}{
		{
			// The example shape: record 1 conflicts with the registered B1
			// (quantity 11 vs 10), record 2 writes its quantity as a string.
			name: "registry conflict before string quantity",
			manifest: `[
  {"batch":"B1","product":"P1","quantity":11,"unit":"kg"},
  {"batch":"B2","product":"P2","quantity":"2","unit":"box"}
]`,
			recordPos: "record 2",
			batch:     "B2",
			field:     "quantity",
			reason:    "must be a JSON integer",
		},
		{
			// Same pair, swapped: the illegal record now sits before the
			// conflict. The position in the error must follow the manifest.
			name: "string quantity before registry conflict",
			manifest: `[
  {"batch":"B2","product":"P2","quantity":"2","unit":"box"},
  {"batch":"B1","product":"P1","quantity":11,"unit":"kg"}
]`,
			recordPos: "record 1",
			batch:     "B2",
			field:     "quantity",
			reason:    "must be a JSON integer",
		},
		{
			// The conflict is introduced by the manifest itself (records 1
			// and 2 share BX with different quantities); the illegal record
			// 3 — a unit blank once trimmed — is still the reported failure,
			// and the would-be-new BX must not remain anywhere.
			name: "manifest conflict before blank unit",
			manifest: `[
  {"batch":"BX","product":"P1","quantity":10,"unit":"kg"},
  {"batch":"BX","product":"P1","quantity":99,"unit":"kg"},
  {"batch":"B2","product":"P2","quantity":1,"unit":"  "}
]`,
			recordPos: "record 3",
			batch:     "B2",
			field:     "unit",
			reason:    "must not be blank",
		},
		{
			// Two illegal records behind an earlier conflict: the first one
			// in manifest order is reported, not the later one and never the
			// conflict.
			name: "first of several illegal records wins",
			manifest: `[
  {"batch":"B1","product":"P1","quantity":11,"unit":"kg"},
  {"batch":"B2","product":"P2","quantity":1,"unit":" \t "},
  {"batch":"B3","product":"P3","quantity":"5","unit":"kg"}
]`,
			recordPos: "record 2",
			batch:     "B2",
			field:     "unit",
			reason:    "must not be blank",
		},
		{
			// The illegal record's own batch id is blank, so no id may be
			// cited — and the earlier B1 conflict still loses.
			name: "blank batch id is not cited",
			manifest: `[
  {"batch":"B1","product":"P1","quantity":11,"unit":"kg"},
  {"batch":"   ","product":"P2","quantity":1,"unit":"kg"}
]`,
			recordPos: "record 2",
			batch:     "",
			field:     "batch",
			reason:    "must not be blank",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			registry := filepath.Join(dir, "reg.json")
			manifest := filepath.Join(dir, "arrivals.json")
			writeFile(t, registry, invalidFirstRegistryJSON)
			writeFile(t, manifest, tc.manifest)
			pinInvalidFirstRegistry(t, registry)
			manifestSnapshot := readFileSnapshot(t, manifest)

			code, stdout, stderr := runImportChild(t, registry, manifest, faultNone, "", true)

			if code != 1 {
				t.Fatalf("exit code = %d, want 1; stderr:\n%s", code, stderr)
			}
			if len(stdout) != 0 {
				t.Fatalf("stdout must stay empty on failure, got %q", stdout)
			}
			assertInvalidFirstFailure(t, stderr, manifest, tc.recordPos, tc.batch, tc.field, tc.reason)
			assertInvalidFirstFilesUntouched(t, registry, manifest)
			assertFileUnchanged(t, manifest, manifestSnapshot)
		})
	}
}

// Correcting only the illegal record while keeping the conflicting one sends
// the error back to the conflict — now the manifest is fully legal, so the
// conflict check is reached. The error names the batch id, every differing
// field and the conflict's source: the registered record, or the earlier
// manifest record by its 1-based position. Still no success output, and
// still no new batch saved.
func TestBatchImportCLIFixingInvalidRecordSurfacesConflict(t *testing.T) {
	cases := []struct {
		name        string
		manifest    string
		recordPos   string
		batch       string
		fields      []string
		sourceWants []string
	}{
		{
			// The first scenario's manifest with the quantity fixed to a
			// JSON integer: record 1's clash with the registered B1 is now
			// the failure, and the legal B2 record behind it must not land.
			name: "conflict with the registered record",
			manifest: `[
  {"batch":"B1","product":"P1","quantity":11,"unit":"kg"},
  {"batch":"B2","product":"P2","quantity":2,"unit":"box"}
]`,
			recordPos:   "record 1",
			batch:       "B1",
			fields:      []string{"quantity"},
			sourceWants: []string{"conflicts with the registered record"},
		},
		{
			// The manifest-internal conflict with the unit fixed: record 3
			// now clashes with record 1, the batch's first occurrence —
			// product, quantity and unit all differ.
			name: "conflict with an earlier manifest record",
			manifest: `[
  {"batch":"BX","product":"P1","quantity":10,"unit":"kg"},
  {"batch":"B2","product":"P2","quantity":1,"unit":"box"},
  {"batch":"BX","product":"P9","quantity":99,"unit":"g"}
]`,
			recordPos:   "record 3",
			batch:       "BX",
			fields:      []string{"product", "quantity", "unit"},
			sourceWants: []string{"conflicts with manifest record 1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			registry := filepath.Join(dir, "reg.json")
			manifest := filepath.Join(dir, "arrivals.json")
			writeFile(t, registry, invalidFirstRegistryJSON)
			writeFile(t, manifest, tc.manifest)
			pinInvalidFirstRegistry(t, registry)
			manifestSnapshot := readFileSnapshot(t, manifest)

			code, stdout, stderr := runImportChild(t, registry, manifest, faultNone, "", true)

			if code != 1 {
				t.Fatalf("exit code = %d, want 1; stderr:\n%s", code, stderr)
			}
			if len(stdout) != 0 {
				t.Fatalf("stdout must stay empty on failure, got %q", stdout)
			}
			msg := string(stderr)
			wants := []string{
				"govflow: batch-import:",
				tc.recordPos,
				"(batch " + strconv.Quote(tc.batch) + ")",
				"the whole manifest is rejected",
			}
			wants = append(wants, tc.sourceWants...)
			for _, field := range tc.fields {
				wants = append(wants, field)
			}
			for _, want := range wants {
				if !strings.Contains(msg, want) {
					t.Fatalf("stderr must contain %q:\n%s", want, msg)
				}
			}
			if strings.Contains(msg, "invalid input file") {
				t.Fatalf("the manifest is now legal; the failure must be the conflict, not invalid input:\n%s", msg)
			}
			assertInvalidFirstFilesUntouched(t, registry, manifest)
			assertFileUnchanged(t, manifest, manifestSnapshot)
		})
	}
}
