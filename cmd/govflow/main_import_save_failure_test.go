package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gzhysuiioo/govflow-proposals/govflow/batchreg"
)

// End-to-end regression for batch-import's all-records-or-none promise at the
// point where it is most easily broken: a SAVE failure. The manifest used here
// is legal under every field rule and conflicts with nothing — some records
// confirm batches already registered (one of them padded with whitespace), one
// record repeats a batch introduced earlier in the same manifest, and several
// batches are new — so a failure can only originate from the save itself,
// never from invalid input or a batch conflict.
//
// Two real save failures are injected deterministically through batchreg's
// step hooks (no disk exhaustion, no filesystem fault, no special account):
//
//   - the new content write stops while the new table is incomplete;
//   - the new table is fully prepared in the temporary file but replacing the
//     registry file is refused.
//
// Each is run against an existing registry and against a path where no
// registry exists yet. Whatever step fails, batch-import must report failure
// to its caller: non-zero exit code, stderr naming the target registry path
// and the concrete save reason, empty stdout, the existing registry kept
// byte-for-byte and timestamp-for-timestamp (or the missing target still
// absent), no partial table and no leftover temporary file, and the
// read-only manifest exactly as submitted. Success controls with the very
// same manifest prove the manifest imports normally and that the JSON results
// match what the registry finally holds, record by record and field by field.
//
// The failure scenarios run in a real child process — the test binary
// re-invoked with -wrap.batch-import — so the assertions cover the actual
// process contract (exit code, stdout, stderr) rather than an in-process
// error return, while staying fully offline: the child is this same binary.

var (
	wrapBatchImport   = flag.Bool("wrap.batch-import", false, "internal: run one batch-import invocation and exit")
	wrapBatchRegister = flag.Bool("wrap.batch-register", false, "internal: run one batch-register invocation and exit")
)

// TestMain runs the wrapped single-command mode when requested by the parent
// test, and the normal test suite otherwise.
func TestMain(m *testing.M) {
	flag.Parse()
	if *wrapBatchImport {
		os.Exit(runWrappedBatchImport())
	}
	if *wrapBatchRegister {
		os.Exit(runWrappedBatchRegister())
	}
	os.Exit(m.Run())
}

// saveFault selects which save step fails inside the wrapped child process.
type saveFault string

const (
	faultNone   saveFault = ""
	faultWrite  saveFault = "write"
	faultRename saveFault = "rename"
)

// runWrappedBatchImport performs exactly one batch-import against the files
// named in WRAP_REGISTRY/WRAP_INPUT, optionally failing one save step as
// selected by WRAP_FAULT. Exit codes mimic the real command: 1 for a reported
// batch-import failure (message written to stderr like main does), 3 for a
// harness problem inside the child, 0 for success.
func runWrappedBatchImport() int {
	registry := os.Getenv("WRAP_REGISTRY")
	input := os.Getenv("WRAP_INPUT")
	reason := os.Getenv("WRAP_REASON")
	harnessFail := func(format string, args ...any) int {
		fmt.Fprintf(os.Stderr, "wrap harness: "+format+"\n", args...)
		return 3
	}
	// harnessExit reports a broken test harness from inside an injected hook,
	// whose signature allows only an error; the parent treats code 3 as a
	// harness failure rather than a command failure.
	harnessExit := func(format string, args ...any) error {
		fmt.Fprintf(os.Stderr, "wrap harness: "+format+"\n", args...)
		os.Exit(3)
		return nil
	}
	switch saveFault(os.Getenv("WRAP_FAULT")) {
	case faultWrite:
		// Part of the new content genuinely reaches the temporary file, then
		// the write stops with the new table incomplete.
		batchreg.WriteTempContent = func(f *os.File, data []byte) (int, error) {
			half := len(data) / 2
			if _, err := f.Write(data[:half]); err != nil {
				fmt.Fprintf(os.Stderr, "wrap harness: setup write failed: %v\n", err)
				os.Exit(3)
			}
			return half, errors.New(reason)
		}
	case faultRename:
		// The complete new table must be ready before the replacement is
		// refused; markers are checked on the prepared temporary file.
		markers := strings.Split(os.Getenv("WRAP_MARKERS"), ",")
		batchreg.RenameTempFile = func(oldpath, newpath string) error {
			if newpath != registry {
				return harnessExit("rename must target %q, got %q", registry, newpath)
			}
			prepared, err := os.ReadFile(oldpath)
			if err != nil {
				return harnessExit("prepared temporary file is unreadable: %v", err)
			}
			for _, marker := range markers {
				if !bytes.Contains(prepared, []byte(marker)) {
					return harnessExit("prepared content is missing %q: %s", marker, prepared)
				}
			}
			return errors.New(reason)
		}
	case faultNone:
	default:
		return harnessFail("unknown fault %q", os.Getenv("WRAP_FAULT"))
	}
	if err := runBatchImport([]string{"--registry", registry, "--input", input}, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "govflow:", err)
		return 1
	}
	return 0
}

// importFailureRegistryJSON is the multi-batch registry every existing-target
// scenario starts from, in a layout distinct from Save's own output.
const importFailureRegistryJSON = `{"version":1,"batches":[` +
	`{"batch":"B-OLD-1","product":"P-1","quantity":7,"unit":"kg"},` +
	`{"batch":"B-OLD-2","product":"苹果","quantity":80,"unit":"箱"},` +
	`{"batch":"B-OLD-3","product":"P-3","quantity":9223372036854775807,"unit":"m"}]}`

// importFailurePinnedModTime is pinned onto the existing registry so any
// rewrite — even one restoring identical bytes — is observable.
var importFailurePinnedModTime = time.Date(2011, time.September, 17, 12, 13, 14, 0, time.UTC)

// manifestRow builds one manifest JSON element; the values are written
// verbatim, so edge whitespace survives into the file and trimming is really
// exercised by the command.
type manifestRow struct {
	Batch    string `json:"batch"`
	Product  string `json:"product"`
	Quantity int64  `json:"quantity"`
	Unit     string `json:"unit"`
}

// importFailureManifestRows is the one legal, conflict-free manifest used by
// every scenario in this file: a padded duplicate of a stored batch, several
// new batches (including CJK/emoji text and a padded unit), an identical
// repeat of a batch introduced earlier in the manifest, and a duplicate of
// another stored batch interleaved among the new ones.
var importFailureManifestRows = []manifestRow{
	{Batch: " B-OLD-1 ", Product: "P-1", Quantity: 7, Unit: "kg"},     // duplicate of stored
	{Batch: "B-NEW-1", Product: "P-4", Quantity: 120, Unit: "box"},    // created
	{Batch: "B-OLD-2", Product: "苹果", Quantity: 80, Unit: "箱"},        // duplicate of stored
	{Batch: "B-NEW-2", Product: "新产品😀", Quantity: 3, Unit: "千克"},      // created
	{Batch: "B-NEW-1", Product: "P-4", Quantity: 120, Unit: "box"},    // same-manifest duplicate
	{Batch: "B-NEW-3", Product: "P-9", Quantity: 1, Unit: " pallet "}, // created, unit trimmed
}

// expectedImportRow is one normalized results entry in manifest order.
type expectedImportRow struct {
	batch    string
	product  string
	quantity int64
	unit     string
	status   string
}

func row(batch, product string, quantity int64, unit, status string) expectedImportRow {
	return expectedImportRow{batch: batch, product: product, quantity: quantity, unit: unit, status: status}
}

// manifestResultsAgainstExisting is what the same manifest must yield when
// the registry already holds B-OLD-1/2/3.
var manifestResultsAgainstExisting = []expectedImportRow{
	row("B-OLD-1", "P-1", 7, "kg", "duplicate"),
	row("B-NEW-1", "P-4", 120, "box", "created"),
	row("B-OLD-2", "苹果", 80, "箱", "duplicate"),
	row("B-NEW-2", "新产品😀", 3, "千克", "created"),
	row("B-NEW-1", "P-4", 120, "box", "duplicate"),
	row("B-NEW-3", "P-9", 1, "pallet", "created"),
}

// manifestResultsWhenMissing is what the same manifest yields when no
// registry exists: B-OLD-1 and B-OLD-2 are new there, and only the repeated
// B-NEW-1 record is a duplicate.
var manifestResultsWhenMissing = []expectedImportRow{
	row("B-OLD-1", "P-1", 7, "kg", "created"),
	row("B-NEW-1", "P-4", 120, "box", "created"),
	row("B-OLD-2", "苹果", 80, "箱", "created"),
	row("B-NEW-2", "新产品😀", 3, "千克", "created"),
	row("B-NEW-1", "P-4", 120, "box", "duplicate"),
	row("B-NEW-3", "P-9", 1, "pallet", "created"),
}

// storedBatchesAfterExisting is the final registry order on success against
// the existing fixture: the old records keep their position, the new batches
// append in first-occurrence order.
var storedBatchesAfterExisting = []registerResult{
	{Batch: "B-OLD-1", Product: "P-1", Quantity: 7, Unit: "kg"},
	{Batch: "B-OLD-2", Product: "苹果", Quantity: 80, Unit: "箱"},
	{Batch: "B-OLD-3", Product: "P-3", Quantity: 9223372036854775807, Unit: "m"},
	{Batch: "B-NEW-1", Product: "P-4", Quantity: 120, Unit: "box"},
	{Batch: "B-NEW-2", Product: "新产品😀", Quantity: 3, Unit: "千克"},
	{Batch: "B-NEW-3", Product: "P-9", Quantity: 1, Unit: "pallet"},
}

// storedBatchesWhenMissing is the final registry order on success with no
// prior file: records land in manifest first-occurrence order.
var storedBatchesWhenMissing = []registerResult{
	{Batch: "B-OLD-1", Product: "P-1", Quantity: 7, Unit: "kg"},
	{Batch: "B-NEW-1", Product: "P-4", Quantity: 120, Unit: "box"},
	{Batch: "B-OLD-2", Product: "苹果", Quantity: 80, Unit: "箱"},
	{Batch: "B-NEW-2", Product: "新产品😀", Quantity: 3, Unit: "千克"},
	{Batch: "B-NEW-3", Product: "P-9", Quantity: 1, Unit: "pallet"},
}

// writeManifest serializes the shared rows as one JSON array file.
func writeManifest(t *testing.T, path string, rows []manifestRow) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, r := range rows {
		if i > 0 {
			buf.WriteByte(',')
		}
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(data)
	}
	buf.WriteByte(']')
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// runImportChild re-executes the test binary in wrapped one-command mode with
// the chosen fault and returns the child's exit code plus captured output.
// existingRegistry says whether the target file pre-exists, which decides
// which batch ids the fully prepared new table must contain.
func runImportChild(t *testing.T, registry, manifest string, fault saveFault, reason string, existingRegistry bool) (code int, stdout, stderr []byte) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Every manifest batch must be present in the prepared temporary file
	// before the replacement is refused; B-OLD-3 additionally appears only
	// when it was already stored.
	markers := []string{
		"B-OLD-1", "B-OLD-2", "B-NEW-1", "B-NEW-2", "B-NEW-3", "苹果", "新产品😀",
	}
	if existingRegistry {
		markers = append(markers, "B-OLD-3")
	}
	cmd := exec.Command(exe, "-wrap.batch-import")
	cmd.Env = append(os.Environ(),
		"WRAP_REGISTRY="+registry,
		"WRAP_INPUT="+manifest,
		"WRAP_FAULT="+string(fault),
		"WRAP_REASON="+reason,
		"WRAP_MARKERS="+strings.Join(markers, ","),
	)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	runErr := cmd.Run()
	stdout = outBuf.Bytes()
	stderr = errBuf.Bytes()
	if runErr == nil {
		return 0, stdout, stderr
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return exitErr.ExitCode(), stdout, stderr
	}
	t.Fatalf("child process did not start: %v", runErr)
	return 0, nil, nil
}

// assertNoSaveLeftovers walks dir and fails if a save temporary file
// (".govflow-registry-*") or any file other than allowed survived.
func assertNoSaveLeftovers(t *testing.T, dir string, allowed ...string) {
	t.Helper()
	keep := map[string]bool{}
	for _, name := range allowed {
		keep[name] = true
	}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".govflow-registry-") {
			t.Errorf("failed import left a temporary registry file behind: %s", path)
		}
		if !keep[d.Name()] {
			t.Errorf("failed import left an unexpected file behind: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestBatchImportCLISaveFailuresAreAllOrNothing runs both injected save
// failures against both target states. The manifest is the shared legal one;
// the failure must be reported as a save failure and leave the world exactly
// as it was before the command.
func TestBatchImportCLISaveFailuresAreAllOrNothing(t *testing.T) {
	causes := map[saveFault]string{
		faultWrite:  "injected save failure: new registry content not fully written",
		faultRename: "injected save failure: complete new registry cannot replace the target",
	}
	for _, fault := range []saveFault{faultWrite, faultRename} {
		for _, target := range []string{"existing registry", "missing registry"} {
			name := string(fault) + "/" + strings.ReplaceAll(target, " ", "_")
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				registry := filepath.Join(dir, "reg.json")
				manifest := filepath.Join(dir, "arrivals.json")
				existing := target == "existing registry"
				var originalBytes []byte
				if existing {
					writeFile(t, registry, importFailureRegistryJSON)
					if err := os.Chtimes(registry, importFailurePinnedModTime, importFailurePinnedModTime); err != nil {
						t.Fatal(err)
					}
					originalBytes = readFileSnapshot(t, registry)
				}
				manifestBytes := writeManifest(t, manifest, importFailureManifestRows)

				code, stdout, stderr := runImportChild(t, registry, manifest, fault, causes[fault], existing)

				// The caller must observe a command failure, not a success.
				if code != 1 {
					t.Fatalf("exit code = %d, want 1; stderr:\n%s", code, stderr)
				}
				if len(bytes.TrimSpace(stdout)) != 0 {
					t.Fatalf("stdout must stay empty on save failure, got %q", stdout)
				}
				msg := string(stderr)
				// stderr must identify the command, the target registry path
				// and the concrete save reason.
				for _, want := range []string{
					"govflow: batch-import:",
					"cannot save registry",
					registry,
					causes[fault],
				} {
					if !strings.Contains(msg, want) {
						t.Fatalf("stderr must contain %q:\n%s", want, msg)
					}
				}
				// The manifest is legal and conflict-free (proven by the
				// success controls below); the failure wording must never
				// masquerade as invalid input or a batch conflict.
				for _, mustNot := range []string{"invalid input", "conflicts with"} {
					if strings.Contains(msg, mustNot) {
						t.Fatalf("a save failure must not be reported as %q:\n%s", mustNot, msg)
					}
				}

				if existing {
					// The existing registry survives with exact bytes and
					// modification time and still loads with its original
					// records, fields and order.
					info, err := os.Stat(registry)
					if err != nil {
						t.Fatalf("the original registry must still exist: %v", err)
					}
					if !info.ModTime().Equal(importFailurePinnedModTime) {
						t.Fatalf("failed import changed the registry mtime: got %v, want %v",
							info.ModTime(), importFailurePinnedModTime)
					}
					after := readFileSnapshot(t, registry)
					if !bytes.Equal(after, originalBytes) {
						t.Fatalf("failed import changed the registry bytes:\nbefore: %s\nafter:  %s", originalBytes, after)
					}
					if bytes.Contains(after, []byte("B-NEW-")) {
						t.Fatalf("part of the manifest landed in the registry: %s", after)
					}
					stored := readStoredBatches(t, registry)
					wantStored := []registerResult{
						{Batch: "B-OLD-1", Product: "P-1", Quantity: 7, Unit: "kg"},
						{Batch: "B-OLD-2", Product: "苹果", Quantity: 80, Unit: "箱"},
						{Batch: "B-OLD-3", Product: "P-3", Quantity: 9223372036854775807, Unit: "m"},
					}
					if len(stored) != len(wantStored) {
						t.Fatalf("registry must keep exactly its original batches, got %+v", stored)
					}
					for i, want := range wantStored {
						got := stored[i]
						got.Status = ""
						if got != want {
							t.Fatalf("original record %d = %+v, want %+v", i+1, got, want)
						}
					}
				} else {
					// With no prior registry there must still be no file: not
					// an empty registry, not the first few manifest rows.
					if _, err := os.Stat(registry); !os.IsNotExist(err) {
						t.Fatalf("failed import must not create the registry, stat err=%v", err)
					}
				}
				assertNoSaveLeftovers(t, dir, filepath.Base(manifest), filepath.Base(registry))
				assertFileUnchanged(t, manifest, manifestBytes)
			})
		}
	}
}

// decodeImportEnvelope parses stdout strictly as one JSON object carrying
// exactly one member, "results", whose elements are generic objects for
// per-key assertions.
func decodeImportEnvelope(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(raw)))
	var envelope map[string]json.RawMessage
	if err := dec.Decode(&envelope); err != nil {
		t.Fatalf("stdout is not a JSON object: %v (%q)", err, raw)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		t.Fatalf("stdout must hold exactly one JSON object, got %q", raw)
	}
	if len(envelope) != 1 {
		t.Fatalf("result object must carry exactly \"results\", got keys %v from %q", envelope, raw)
	}
	resultsRaw, ok := envelope["results"]
	if !ok {
		t.Fatalf("result object is missing \"results\": %q", raw)
	}
	var rows []map[string]any
	if err := json.Unmarshal(resultsRaw, &rows); err != nil {
		t.Fatalf("results is not an array of objects: %v (%s)", err, resultsRaw)
	}
	return rows
}

// assertImportResults checks every result entry against its manifest-order
// expectation: exactly the five public keys, normalized four fields and the
// created/duplicate status.
func assertImportResults(t *testing.T, stdout []byte, want []expectedImportRow) {
	t.Helper()
	rows := decodeImportEnvelope(t, stdout)
	if len(rows) != len(want) {
		t.Fatalf("got %d results, want %d (manifest order): %s", len(rows), len(want), stdout)
	}
	for i, w := range want {
		got := rows[i]
		if len(got) != 5 {
			t.Fatalf("result %d carries %d keys, want exactly 5: %v", i+1, len(got), got)
		}
		for _, key := range []string{"batch", "product", "quantity", "unit", "status"} {
			if _, ok := got[key]; !ok {
				t.Fatalf("result %d is missing key %q: %v", i+1, key, got)
			}
		}
		if got["batch"] != w.batch || got["product"] != w.product || got["unit"] != w.unit {
			t.Errorf("result %d text fields = %q/%q/%q, want %q/%q/%q",
				i+1, got["batch"], got["product"], got["unit"], w.batch, w.product, w.unit)
		}
		quantity, ok := got["quantity"].(float64)
		if !ok || int64(quantity) != w.quantity {
			t.Errorf("result %d quantity = %v, want %d", i+1, got["quantity"], w.quantity)
		}
		if got["status"] != w.status {
			t.Errorf("result %d status = %v, want %q", i+1, got["status"], w.status)
		}
	}
}

// assertResultsMatchRegistry cross-checks every result entry against the
// record with the same batch id in the final registry, so completion is
// established by field-level content agreement rather than result count.
func assertResultsMatchRegistry(t *testing.T, want []expectedImportRow, stored []registerResult) {
	t.Helper()
	byID := make(map[string]registerResult, len(stored))
	for _, b := range stored {
		byID[b.Batch] = b
	}
	for i, r := range want {
		s, ok := byID[r.batch]
		if !ok {
			t.Fatalf("result %d names batch %q which is absent from the final registry", i+1, r.batch)
		}
		if s.Product != r.product || s.Quantity != r.quantity || s.Unit != r.unit {
			t.Fatalf("result %d (%+v) disagrees with the stored record %+v", i+1, r, s)
		}
	}
}

// assertStoredBatches compares the registry's records in file order.
func assertStoredBatches(t *testing.T, registry string, want []registerResult) {
	t.Helper()
	stored := readStoredBatches(t, registry)
	if len(stored) != len(want) {
		t.Fatalf("registry holds %d records, want %d: %+v", len(stored), len(want), stored)
	}
	for i, w := range want {
		got := stored[i]
		got.Status = ""
		if got != w {
			t.Errorf("stored record %d = %+v, want %+v", i+1, got, w)
		}
	}
}

// The success control against an existing registry: with the save healthy the
// very manifest that failed above imports. Results arrive in manifest order
// with the right statuses and normalized fields, the old records keep their
// order and the new batches append in first-occurrence order, and every result
// agrees field by field with the final registry.
func TestBatchImportCLISaveSuccessAgainstExistingRegistry(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "reg.json")
	manifest := filepath.Join(dir, "arrivals.json")
	writeFile(t, registry, importFailureRegistryJSON)
	if err := os.Chtimes(registry, importFailurePinnedModTime, importFailurePinnedModTime); err != nil {
		t.Fatal(err)
	}
	manifestBytes := writeManifest(t, manifest, importFailureManifestRows)

	var stdout bytes.Buffer
	if err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout); err != nil {
		t.Fatalf("the legal manifest must import when the save does not fail: %v", err)
	}

	// A real save must have happened (new batches exist), observable as a new
	// mtime rather than the pinned one.
	info, err := os.Stat(registry)
	if err != nil {
		t.Fatal(err)
	}
	if info.ModTime().Equal(importFailurePinnedModTime) {
		t.Fatal("a creating import must replace the file; the pinned mtime survived")
	}

	assertImportResults(t, stdout.Bytes(), manifestResultsAgainstExisting)
	stored := readStoredBatches(t, registry)
	assertStoredBatches(t, registry, storedBatchesAfterExisting)
	assertResultsMatchRegistry(t, manifestResultsAgainstExisting, stored)
	assertFileUnchanged(t, manifest, manifestBytes)
	assertNoSaveLeftovers(t, dir, filepath.Base(manifest), filepath.Base(registry))
}

// The success control with no prior registry: the file is created, the
// formerly-stored batches of the shared manifest are now created too, the
// within-manifest repeat is the only duplicate, and results again match the
// created file field by field.
func TestBatchImportCLISaveSuccessWithMissingRegistry(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "reg.json")
	manifest := filepath.Join(dir, "arrivals.json")
	manifestBytes := writeManifest(t, manifest, importFailureManifestRows)

	var stdout bytes.Buffer
	if err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout); err != nil {
		t.Fatalf("the legal manifest must import and create the registry: %v", err)
	}

	assertImportResults(t, stdout.Bytes(), manifestResultsWhenMissing)
	stored := readStoredBatches(t, registry)
	assertStoredBatches(t, registry, storedBatchesWhenMissing)
	assertResultsMatchRegistry(t, manifestResultsWhenMissing, stored)
	assertFileUnchanged(t, manifest, manifestBytes)
	assertNoSaveLeftovers(t, dir, filepath.Base(manifest), filepath.Base(registry))
}

// The save-failure path must surface the quoted target path verbatim; this
// guards the wording the failure scenarios assert on, in a direct check that
// reads naturally when it breaks.
func TestBatchImportCLISaveFailureWordingNamesPath(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "reg.json")
	manifest := filepath.Join(dir, "arrivals.json")
	writeFile(t, registry, importFailureRegistryJSON)
	writeManifest(t, manifest, importFailureManifestRows)

	code, stdout, stderr := runImportChild(t, registry, manifest, faultRename, "injected replacement refusal", true)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr:\n%s", code, stderr)
	}
	if len(stdout) != 0 {
		t.Fatalf("stdout must be empty, got %q", stdout)
	}
	want := "cannot save registry " + strconv.Quote(registry) + ": injected replacement refusal"
	if !strings.Contains(string(stderr), want) {
		t.Fatalf("stderr must contain %q:\n%s", want, stderr)
	}
}
