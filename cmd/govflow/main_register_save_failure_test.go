package main

import (
	"bytes"
	"errors"
	"flag"
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

// End-to-end regression for batch-register's promise that a success output is
// printed only after the new record is completely saved: the batch submitted
// here is legal under every flag rule and its id is not registered yet, so a
// failure can only originate from the save itself — never from invalid input,
// a batch conflict, a corrupt registry or an unreadable path.
//
// Two real save failures are injected deterministically through batchreg's
// step hooks (no disk exhaustion, no filesystem fault, no special account):
//
//   - the new content write stops while the new table is incomplete;
//   - the new table is fully prepared in the temporary file but replacing the
//     registry file is refused.
//
// Each is run against an existing multi-batch registry — written in a layout
// distinct from Save's own output — and against a path where no registry
// exists yet. Whatever step fails, batch-register must report failure to its
// caller: non-zero exit code, stderr naming the user-supplied registry path
// and the concrete save reason (never worded as an input error), empty
// stdout, the existing registry kept byte-for-byte and timestamp-for-
// timestamp with its records, fields and order intact and still readable (a
// failure must not reformat the file to "restore" it), or the missing target
// still absent — no half-written registry and no leftover temporary file in
// either case. Success controls with the very same request prove it registers
// normally: exit code zero, one JSON object carrying the normalized four
// fields plus status "created", old records keeping their order and the new
// batch appended last, or a readable registry created when none existed.
//
// The failure scenarios run in a real child process — the test binary
// re-invoked with -wrap.batch-register — so the assertions cover the actual
// process contract (exit code, stdout, stderr) rather than an in-process
// error return, while staying fully offline: the child is this same binary.

var wrapBatchRegister = flag.Bool("wrap.batch-register", false, "internal: run one batch-register invocation and exit")

// runWrappedBatchRegister performs exactly one batch-register against the
// registry named in WRAP_REGISTRY with the record from WRAP_BATCH /
// WRAP_PRODUCT / WRAP_QUANTITY / WRAP_UNIT, optionally failing one save step
// as selected by WRAP_FAULT. Exit codes mimic the real command: 1 for a
// reported batch-register failure (message written to stderr like main does,
// usage text included for input errors), 3 for a harness problem inside the
// child, 0 for success.
func runWrappedBatchRegister() int {
	registry := os.Getenv("WRAP_REGISTRY")
	reason := os.Getenv("WRAP_REASON")
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
			// With WRAP_REAL_TARGET the registry is addressed through a
			// directory symlink plus "..": the temporary file must still be
			// prepared next to the physical target, never on the link side.
			if rt := os.Getenv("WRAP_REAL_TARGET"); rt != "" {
				td, e1 := os.Stat(filepath.Dir(f.Name()))
				rd, e2 := os.Stat(filepath.Dir(rt))
				if e1 != nil || e2 != nil || !os.SameFile(td, rd) {
					return 0, harnessExit("temporary file prepared in %q, want the real target directory of %q", filepath.Dir(f.Name()), rt)
				}
			}
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
			if rt := os.Getenv("WRAP_REAL_TARGET"); rt != "" {
				// Through a directory link the rename must replace the
				// physical registry and the prepared file must sit beside it.
				got, e1 := os.Stat(newpath)
				want, e2 := os.Stat(rt)
				if e1 != nil || e2 != nil || !os.SameFile(got, want) {
					return harnessExit("rename must replace the real target %q, got %q", rt, newpath)
				}
				td, e3 := os.Stat(filepath.Dir(oldpath))
				rd, e4 := os.Stat(filepath.Dir(rt))
				if e3 != nil || e4 != nil || !os.SameFile(td, rd) {
					return harnessExit("temporary file %q must be prepared next to the real target", oldpath)
				}
			} else if newpath != registry {
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
		fmt.Fprintf(os.Stderr, "wrap harness: unknown fault %q\n", os.Getenv("WRAP_FAULT"))
		return 3
	}
	args := []string{
		"--registry", registry,
		"--batch", os.Getenv("WRAP_BATCH"),
		"--product", os.Getenv("WRAP_PRODUCT"),
		"--quantity", os.Getenv("WRAP_QUANTITY"),
		"--unit", os.Getenv("WRAP_UNIT"),
	}
	if err := runBatchRegister(args, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "govflow:", err)
		var ue *usageError
		if errors.As(err, &ue) {
			fmt.Fprint(os.Stderr, ue.usage)
		}
		return 1
	}
	return 0
}

// The one legal registration request every scenario in this file submits:
// a batch id nothing registers, padded text and leading zeros that
// normalization removes, and CJK/emoji text that must survive verbatim.
const (
	registerFailureBatchFlag    = " B-NEW-1 "
	registerFailureProductFlag  = "新产品😀"
	registerFailureQuantityFlag = "00042"
	registerFailureUnitFlag     = " 千克 "
)

// registerFailureRegistryJSON is the multi-batch registry every
// existing-target scenario starts from, in a legal layout Save would never
// produce: compact, members out of order, no trailing newline. A failed save
// must not reformat it.
const registerFailureRegistryJSON = `{"batches":[` +
	`{"unit":"kg","quantity":7,"product":"P-1","batch":"B-OLD-1"},` +
	`{"batch":"B-OLD-2","product":"苹果","quantity":80,"unit":"箱"},` +
	`{"batch":"B-OLD-3","product":"P-3","quantity":9223372036854775807,"unit":"m"}],"version":1}`

// registerFailurePinnedModTime is pinned onto the existing registry so any
// rewrite — even one restoring identical bytes — is observable.
var registerFailurePinnedModTime = time.Date(2013, time.July, 19, 8, 9, 10, 0, time.UTC)

// registerFailureOriginalBatches is what the fixture registry holds, in
// stored order; a failed save must preserve exactly these records.
var registerFailureOriginalBatches = []registerResult{
	{Batch: "B-OLD-1", Product: "P-1", Quantity: 7, Unit: "kg"},
	{Batch: "B-OLD-2", Product: "苹果", Quantity: 80, Unit: "箱"},
	{Batch: "B-OLD-3", Product: "P-3", Quantity: 9223372036854775807, Unit: "m"},
}

// runRegisterChild re-executes the test binary in wrapped one-command mode
// with the chosen fault and returns the child's exit code plus captured
// output. existingRegistry says whether the target file pre-exists, which
// decides which batch ids the fully prepared new table must contain.
func runRegisterChild(t *testing.T, registry string, fault saveFault, reason string, existingRegistry bool) (code int, stdout, stderr []byte) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// The new batch must be present in the prepared temporary file before the
	// replacement is refused, alongside every batch already stored.
	markers := []string{"B-NEW-1", "新产品😀", "千克"}
	if existingRegistry {
		markers = append(markers, "B-OLD-1", "B-OLD-2", "B-OLD-3", "苹果")
	}
	cmd := exec.Command(exe, "-wrap.batch-register")
	cmd.Env = append(os.Environ(),
		"WRAP_REGISTRY="+registry,
		"WRAP_BATCH="+registerFailureBatchFlag,
		"WRAP_PRODUCT="+registerFailureProductFlag,
		"WRAP_QUANTITY="+registerFailureQuantityFlag,
		"WRAP_UNIT="+registerFailureUnitFlag,
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

// TestBatchRegisterCLISaveFailuresLeaveNoTrace runs both injected save
// failures against both target states. The request is the shared legal one;
// the failure must be reported as a save failure and leave the world exactly
// as it was before the command.
func TestBatchRegisterCLISaveFailuresLeaveNoTrace(t *testing.T) {
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
				existing := target == "existing registry"
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
				// stderr must identify the command, the user-supplied registry
				// path and the concrete save reason.
				for _, want := range []string{
					"govflow:",
					"cannot save registry",
					registry,
					causes[fault],
				} {
					if !strings.Contains(msg, want) {
						t.Fatalf("stderr must contain %q:\n%s", want, msg)
					}
				}
				// The request is legal and conflict-free (proven by the success
				// controls below); the failure wording must never masquerade as
				// invalid input or a batch conflict.
				for _, mustNot := range []string{
					"usage:", "missing required flag", "invalid --",
					"already registered", "conflicting field",
				} {
					if strings.Contains(msg, mustNot) {
						t.Fatalf("a save failure must not be reported as %q:\n%s", mustNot, msg)
					}
				}

				if existing {
					// The existing registry survives with exact bytes and
					// modification time — no reformatting to "restore" it — and
					// still loads with its original records, fields and order.
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
						t.Fatalf("failed registration changed the registry bytes:\nbefore: %s\nafter:  %s", originalBytes, after)
					}
					if bytes.Contains(after, []byte("B-NEW-1")) {
						t.Fatalf("the new batch landed in the registry: %s", after)
					}
					stored := readStoredBatches(t, registry)
					if len(stored) != len(registerFailureOriginalBatches) {
						t.Fatalf("registry must keep exactly its original batches, got %+v", stored)
					}
					for i, want := range registerFailureOriginalBatches {
						got := stored[i]
						got.Status = ""
						if got != want {
							t.Fatalf("original record %d = %+v, want %+v", i+1, got, want)
						}
					}
				} else {
					// With no prior registry there must still be no file: not
					// an empty registry, not a half-written record.
					if _, err := os.Stat(registry); !os.IsNotExist(err) {
						t.Fatalf("failed registration must not create the registry, stat err=%v", err)
					}
				}
				assertNoSaveLeftovers(t, dir, filepath.Base(registry))
			})
		}
	}
}

// The success control against an existing registry: with the save healthy the
// very request that failed above registers. Stdout is exactly one JSON object
// carrying the normalized four fields plus status "created", the old records
// keep their order and the new batch is appended last.
func TestBatchRegisterCLISaveSuccessAgainstExistingRegistry(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "reg.json")
	writeFile(t, registry, registerFailureRegistryJSON)
	if err := os.Chtimes(registry, registerFailurePinnedModTime, registerFailurePinnedModTime); err != nil {
		t.Fatal(err)
	}

	stdout, err := registerCLI(t, registry,
		registerFailureBatchFlag, registerFailureProductFlag,
		registerFailureQuantityFlag, registerFailureUnitFlag)
	if err != nil {
		t.Fatalf("the legal request must register when the save does not fail: %v", err)
	}

	created := decodeRegisterResult(t, stdout)
	want := registerResult{Batch: "B-NEW-1", Product: "新产品😀", Quantity: 42, Unit: "千克", Status: "created"}
	if created != want {
		t.Fatalf("registration result = %+v, want %+v", created, want)
	}

	// A real save must have happened, observable as a new mtime rather than
	// the pinned one, and the new record agrees field by field with what the
	// registry finally holds, appended after the untouched originals.
	info, err := os.Stat(registry)
	if err != nil {
		t.Fatal(err)
	}
	if info.ModTime().Equal(registerFailurePinnedModTime) {
		t.Fatal("a creating registration must replace the file; the pinned mtime survived")
	}
	stored := readStoredBatches(t, registry)
	wantStored := append(append([]registerResult(nil), registerFailureOriginalBatches...),
		registerResult{Batch: "B-NEW-1", Product: "新产品😀", Quantity: 42, Unit: "千克"})
	if len(stored) != len(wantStored) {
		t.Fatalf("registry holds %d records, want %d: %+v", len(stored), len(wantStored), stored)
	}
	for i, w := range wantStored {
		got := stored[i]
		got.Status = ""
		if got != w {
			t.Fatalf("stored record %d = %+v, want %+v", i+1, got, w)
		}
	}
	assertNoSaveLeftovers(t, dir, filepath.Base(registry))
}

// The success control with no prior registry: the file is created, readable,
// and holds exactly the one new batch.
func TestBatchRegisterCLISaveSuccessWithMissingRegistry(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "reg.json")

	stdout, err := registerCLI(t, registry,
		registerFailureBatchFlag, registerFailureProductFlag,
		registerFailureQuantityFlag, registerFailureUnitFlag)
	if err != nil {
		t.Fatalf("the legal request must register and create the registry: %v", err)
	}

	created := decodeRegisterResult(t, stdout)
	want := registerResult{Batch: "B-NEW-1", Product: "新产品😀", Quantity: 42, Unit: "千克", Status: "created"}
	if created != want {
		t.Fatalf("registration result = %+v, want %+v", created, want)
	}

	stored := readStoredBatches(t, registry)
	if len(stored) != 1 {
		t.Fatalf("created registry holds %d records, want 1: %+v", len(stored), stored)
	}
	got := stored[0]
	got.Status = ""
	if got != (registerResult{Batch: "B-NEW-1", Product: "新产品😀", Quantity: 42, Unit: "千克"}) {
		t.Fatalf("stored record = %+v, want the registered batch", got)
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
