package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gzhysuiioo/govflow-proposals/govflow/batchreg"
)

// End-to-end regression for batch-register's promise that a new batch is a
// success only once the registration result is fully saved. The request used
// in every scenario is legal under every field rule — padded text fields that
// normalize away, a quantity carrying leading zeros — and names a batch id
// that does not exist yet, so the failure can only originate from the save
// itself: never from invalid parameters, a batch conflict, a corrupt registry
// or an unreadable path.
//
// Two real save failures are injected deterministically through batchreg's
// step hooks (no disk exhaustion, no filesystem fault, no special account):
//
//   - the new content write stops while the new registry is incomplete;
//   - the complete new registry is fully prepared in the temporary file, but
//     replacing the target registry file is refused.
//
// Each runs against an existing multi-batch registry and against a path where
// no registry exists yet. Whatever step fails, batch-register must report a
// command failure: non-zero exit, stderr naming the user's registry path and
// the concrete save reason (never wording that blames the input), empty
// stdout, the existing registry kept byte-for-byte and timestamp-for-timestamp
// in its own — deliberately different — layout, still readable with its
// original products/quantities/units/order and without the new batch, or the
// missing target still absent, and no half-record registry or save temporary
// file left behind. Success controls with the very same legal request prove it
// registers normally (created JSON on stdout, old batches first, the new batch
// appended last, a readable file created when none existed).
//
// The failure scenarios run in a real child process — the test binary
// re-invoked with -wrap.batch-register — so the assertions cover the actual
// process contract (exit code, stdout, stderr), while staying fully offline:
// the child is this same binary.

// The legal new batch, submitted in padded/leading-zero form so the command's
// own normalization is genuinely exercised (raw) and what must land in the
// registry and on stdout (normalized).
const (
	registerNewBatchRaw    = " B-NEW-1 "
	registerNewProductRaw  = " 新产品😀 "
	registerNewQuantityRaw = "000120"
	registerNewUnitRaw     = " 千克 "

	registerNewBatch   = "B-NEW-1"
	registerNewProduct = "新产品😀"
	registerNewUnit    = "千克"
)

const registerNewQuantity int64 = 120

// registerFailureRegistryJSON is the multi-batch registry every
// existing-target scenario starts from. Its layout is deliberately legal but
// unlike Save's own output — compact, records with members in reverse order
// and the root "batches" before "version" — so a failed save that "helpfully"
// rewrote the file to restore the records would be caught.
const registerFailureRegistryJSON = `{"batches":[` +
	`{"unit":"kg","quantity":7,"product":"P-1","batch":"B-OLD-1"},` +
	`{"unit":"箱","quantity":80,"product":"苹果","batch":"B-OLD-2"},` +
	`{"unit":"m","quantity":9223372036854775807,"product":"P-3","batch":"B-OLD-3"}],` +
	`"version":1}`

// registerFailureOldBatches are the fixture's records in stored order.
var registerFailureOldBatches = []batchreg.Batch{
	{Batch: "B-OLD-1", Product: "P-1", Quantity: 7, Unit: "kg"},
	{Batch: "B-OLD-2", Product: "苹果", Quantity: 80, Unit: "箱"},
	{Batch: "B-OLD-3", Product: "P-3", Quantity: 9223372036854775807, Unit: "m"},
}

// registerFailurePinnedModTime is pinned onto the existing registry so any
// rewrite — even one restoring identical bytes — is observable.
var registerFailurePinnedModTime = time.Date(2013, time.November, 19, 14, 15, 16, 0, time.UTC)

// runWrappedBatchRegister performs exactly one batch-register against the
// registry named in WRAP_REGISTRY, using the request fields carried by
// WRAP_BATCH/WRAP_PRODUCT/WRAP_QUANTITY/WRAP_UNIT and optionally failing one
// save step as selected by WRAP_FAULT. Exit codes mimic the real command: 1
// for a reported batch-register failure (message written to stderr like main
// does), 3 for a harness problem inside the child, 0 for success.
func runWrappedBatchRegister() int {
	registry := os.Getenv("WRAP_REGISTRY")
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
		// the write stops with the new registry incomplete.
		batchreg.WriteTempContent = func(f *os.File, data []byte) (int, error) {
			half := len(data) / 2
			if _, err := f.Write(data[:half]); err != nil {
				fmt.Fprintf(os.Stderr, "wrap harness: setup write failed: %v\n", err)
				os.Exit(3)
			}
			return half, errors.New(reason)
		}
	case faultRename:
		// The complete new registry must be ready before the replacement is
		// refused: the prepared temporary file is parsed and checked record by
		// record, including the new batch appended last.
		wantCount, err := strconv.Atoi(os.Getenv("WRAP_WANTCOUNT"))
		if err != nil {
			return harnessFail("bad WRAP_WANTCOUNT: %v", err)
		}
		batchreg.RenameTempFile = func(oldpath, newpath string) error {
			if newpath != registry {
				return harnessExit("rename must target %q, got %q", registry, newpath)
			}
			prepared, err := os.ReadFile(oldpath)
			if err != nil {
				return harnessExit("prepared temporary file is unreadable: %v", err)
			}
			var ready batchreg.Registry
			if err := json.Unmarshal(prepared, &ready); err != nil {
				return harnessExit("prepared content is not a complete registry: %v: %s", err, prepared)
			}
			wantBatches := []batchreg.Batch(nil)
			if wantCount == len(registerFailureOldBatches)+1 {
				wantBatches = append(wantBatches, registerFailureOldBatches...)
			}
			wantBatches = append(wantBatches, batchreg.Batch{
				Batch: registerNewBatch, Product: registerNewProduct,
				Quantity: registerNewQuantity, Unit: registerNewUnit,
			})
			if len(ready.Batches) != wantCount {
				return harnessExit("prepared content holds %d batches, want %d: %s",
					len(ready.Batches), wantCount, prepared)
			}
			for i, want := range wantBatches {
				if ready.Batches[i] != want {
					return harnessExit("prepared record %d = %+v, want %+v", i+1, ready.Batches[i], want)
				}
			}
			return errors.New(reason)
		}
	case faultNone:
	default:
		return harnessFail("unknown fault %q", os.Getenv("WRAP_FAULT"))
	}
	if err := runBatchRegister([]string{
		"--registry", registry,
		"--batch", os.Getenv("WRAP_BATCH"),
		"--product", os.Getenv("WRAP_PRODUCT"),
		"--quantity", os.Getenv("WRAP_QUANTITY"),
		"--unit", os.Getenv("WRAP_UNIT"),
	}, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "govflow:", err)
		return 1
	}
	return 0
}

// runRegisterChild re-executes the test binary in wrapped one-command mode
// with the chosen fault and returns the child's exit code plus captured
// output. existingRegistry says whether the target file pre-exists, which
// decides how many records the fully prepared new registry must hold.
func runRegisterChild(t *testing.T, registry string, fault saveFault, reason string, existingRegistry bool) (code int, stdout, stderr []byte) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wantCount := "1"
	if existingRegistry {
		wantCount = strconv.Itoa(len(registerFailureOldBatches) + 1)
	}
	cmd := exec.Command(exe, "-wrap.batch-register")
	cmd.Env = append(os.Environ(),
		"WRAP_REGISTRY="+registry,
		"WRAP_FAULT="+string(fault),
		"WRAP_REASON="+reason,
		"WRAP_WANTCOUNT="+wantCount,
		"WRAP_BATCH="+registerNewBatchRaw,
		"WRAP_PRODUCT="+registerNewProductRaw,
		"WRAP_QUANTITY="+registerNewQuantityRaw,
		"WRAP_UNIT="+registerNewUnitRaw,
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

// TestBatchRegisterCLISaveFailureReportedAndPreserves runs both injected save
// failures against both target states. The request is the shared legal,
// not-yet-registered one; the failure must be reported as a save failure and
// leave the world exactly as it was before the command.
func TestBatchRegisterCLISaveFailureReportedAndPreserves(t *testing.T) {
	causes := map[saveFault]string{
		faultWrite:  "injected save failure: new registry content not fully written",
		faultRename: "injected save failure: complete new registry cannot replace the target",
	}
	for _, fault := range []saveFault{faultWrite, faultRename} {
		for _, existing := range []bool{true, false} {
			name := string(fault) + "/"
			if existing {
				name += "existing_registry"
			} else {
				name += "missing_registry"
			}
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				registry := filepath.Join(dir, "reg.json")
				var originalBytes []byte
				if existing {
					writeFile(t, registry, registerFailureRegistryJSON)
					if err := os.Chtimes(registry, registerFailurePinnedModTime, registerFailurePinnedModTime); err != nil {
						t.Fatal(err)
					}
					originalBytes = readFileSnapshot(t, registry)
				}

				code, stdout, stderr := runRegisterChild(t, registry, fault, causes[fault], existing)

				// The caller must observe a command failure, not a success.
				if code != 1 {
					t.Fatalf("exit code = %d, want 1; stderr:\n%s", code, stderr)
				}
				if len(bytes.TrimSpace(stdout)) != 0 {
					t.Fatalf("stdout must stay empty on save failure, got %q", stdout)
				}
				msg := string(stderr)
				// stderr must identify the user-given registry path and the
				// concrete save reason.
				for _, want := range []string{
					"govflow: cannot save registry",
					registry,
					causes[fault],
				} {
					if !strings.Contains(msg, want) {
						t.Fatalf("stderr must contain %q:\n%s", want, msg)
					}
				}
				// The request is legal and new (proven by the success controls
				// below); the failure wording must never masquerade as invalid
				// input, a duplicate or a conflict.
				for _, mustNot := range []string{
					"usage:", "invalid ", "missing required", "conflicts with", "already registered",
				} {
					if strings.Contains(msg, mustNot) {
						t.Fatalf("a save failure must not be reported as %q:\n%s", mustNot, msg)
					}
				}

				if existing {
					// The existing registry survives with exact bytes and
					// modification time — its foreign layout must not be
					// "restored" by reformatting — and still loads with its
					// original records, fields and order; the new batch is
					// absent.
					info, err := os.Stat(registry)
					if err != nil {
						t.Fatalf("the original registry must still exist: %v", err)
					}
					if !info.ModTime().Equal(registerFailurePinnedModTime) {
						t.Fatalf("failed registration changed the registry mtime: got %v, want %v",
							info.ModTime(), registerFailurePinnedModTime)
					}
					after := readFileSnapshot(t, registry)
					if !bytes.Equal(after, originalBytes) {
						t.Fatalf("failed registration changed the registry bytes:\nbefore: %s\nafter:  %s",
							originalBytes, after)
					}
					if bytes.Contains(after, []byte(registerNewBatch)) {
						t.Fatalf("the new batch landed in the registry: %s", after)
					}
					loaded, existed, err := batchreg.Load(registry)
					if err != nil || !existed {
						t.Fatalf("the original registry must still read normally: existed=%v err=%v", existed, err)
					}
					if len(loaded.Batches) != len(registerFailureOldBatches) {
						t.Fatalf("registry must keep exactly its original batches, got %+v", loaded.Batches)
					}
					for i, want := range registerFailureOldBatches {
						if loaded.Batches[i] != want {
							t.Fatalf("original record %d = %+v, want %+v", i+1, loaded.Batches[i], want)
						}
					}
					assertNoSaveLeftovers(t, dir, filepath.Base(registry))
				} else {
					// With no prior registry there must still be no file: not a
					// created registry, not a half-written first record, and no
					// save temporary file.
					if _, err := os.Stat(registry); !os.IsNotExist(err) {
						t.Fatalf("failed registration must not create the registry, stat err=%v", err)
					}
					assertNoSaveLeftovers(t, dir)
				}
			})
		}
	}
}

// registerSuccessCommand runs the shared legal request in-process, the same
// way the success controls of the batch-import regression do.
func registerSuccessCommand(t *testing.T, registry string) string {
	t.Helper()
	var stdout bytes.Buffer
	err := runBatchRegister([]string{
		"--registry", registry,
		"--batch", registerNewBatchRaw,
		"--product", registerNewProductRaw,
		"--quantity", registerNewQuantityRaw,
		"--unit", registerNewUnitRaw,
	}, &stdout)
	if err != nil {
		t.Fatalf("the legal new-batch request must register when the save does not fail: %v", err)
	}
	return stdout.String()
}

// The success control against an existing registry: with the save healthy the
// very request that failed above registers. stdout holds exactly the one
// created JSON object with the normalized fields, the old batches keep their
// order and the new batch appends last, and a real save happened (new mtime,
// new bytes) producing a readable file.
func TestBatchRegisterCLISaveSuccessAgainstExistingRegistry(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "reg.json")
	writeFile(t, registry, registerFailureRegistryJSON)
	if err := os.Chtimes(registry, registerFailurePinnedModTime, registerFailurePinnedModTime); err != nil {
		t.Fatal(err)
	}
	originalBytes := readFileSnapshot(t, registry)

	stdout := registerSuccessCommand(t, registry)

	wantResult := registerResult{
		Batch: registerNewBatch, Product: registerNewProduct,
		Quantity: registerNewQuantity, Unit: registerNewUnit, Status: "created",
	}
	if got := decodeRegisterResult(t, stdout); got != wantResult {
		t.Fatalf("stdout result = %+v, want %+v", got, wantResult)
	}

	info, err := os.Stat(registry)
	if err != nil {
		t.Fatal(err)
	}
	if info.ModTime().Equal(registerFailurePinnedModTime) {
		t.Fatal("a creating registration must replace the file; the pinned mtime survived")
	}
	after := readFileSnapshot(t, registry)
	if bytes.Equal(after, originalBytes) {
		t.Fatal("a creating registration must replace the old content; the original bytes survived")
	}

	loaded, existed, err := batchreg.Load(registry)
	if err != nil || !existed {
		t.Fatalf("saved registry must read back: existed=%v err=%v", existed, err)
	}
	wantBatches := append(append([]batchreg.Batch(nil), registerFailureOldBatches...), batchreg.Batch{
		Batch: registerNewBatch, Product: registerNewProduct,
		Quantity: registerNewQuantity, Unit: registerNewUnit,
	})
	if len(loaded.Batches) != len(wantBatches) {
		t.Fatalf("registry holds %d records, want %d: %+v", len(loaded.Batches), len(wantBatches), loaded.Batches)
	}
	for i, want := range wantBatches {
		if loaded.Batches[i] != want {
			t.Fatalf("stored record %d = %+v, want %+v (old batches first, new batch last)",
				i+1, loaded.Batches[i], want)
		}
	}
	assertNoSaveLeftovers(t, dir, filepath.Base(registry))
}

// The success control with no prior registry: the file is created readable and
// holds exactly the one normalized new batch.
func TestBatchRegisterCLISaveSuccessCreatesMissingRegistry(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "reg.json")
	if _, err := os.Stat(registry); !os.IsNotExist(err) {
		t.Fatalf("test precondition: registry must not exist yet, stat err=%v", err)
	}

	stdout := registerSuccessCommand(t, registry)

	wantResult := registerResult{
		Batch: registerNewBatch, Product: registerNewProduct,
		Quantity: registerNewQuantity, Unit: registerNewUnit, Status: "created",
	}
	if got := decodeRegisterResult(t, stdout); got != wantResult {
		t.Fatalf("stdout result = %+v, want %+v", got, wantResult)
	}
	loaded, existed, err := batchreg.Load(registry)
	if err != nil || !existed {
		t.Fatalf("the registry must be created and readable: existed=%v err=%v", existed, err)
	}
	wantBatches := []batchreg.Batch{{
		Batch: registerNewBatch, Product: registerNewProduct,
		Quantity: registerNewQuantity, Unit: registerNewUnit,
	}}
	if len(loaded.Batches) != 1 || loaded.Batches[0] != wantBatches[0] {
		t.Fatalf("created registry holds %+v, want %+v", loaded.Batches, wantBatches)
	}
	assertNoSaveLeftovers(t, dir, filepath.Base(registry))
}

// The save-failure path must surface the quoted target path verbatim; this
// guards the wording the failure scenarios assert on, in a direct check that
// reads naturally when it breaks.
func TestBatchRegisterCLISaveFailureWordingNamesPath(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "reg.json")
	writeFile(t, registry, registerFailureRegistryJSON)

	code, stdout, stderr := runRegisterChild(t, registry, faultRename, "injected replacement refusal", true)
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
