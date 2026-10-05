// Command govflow is the DAO 治理提案与执行平台 entry point.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gzhysuiioo/govflow-proposals/govflow"
	"github.com/gzhysuiioo/govflow-proposals/govflow/batchreg"
)

const batchRegisterUsage = `usage: govflow batch-register --registry FILE --batch ID --product ID --quantity N --unit UNIT
run 'govflow help' for the full description and the registry file format.
`

const batchImportUsage = `usage: govflow batch-import --registry FILE --input FILE
run 'govflow help' for the full description and the registry file format.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "govflow:", err)
		var ue *usageError
		if errors.As(err, &ue) {
			fmt.Fprint(os.Stderr, ue.usage)
		}
		os.Exit(1)
	}
}

type usageError struct {
	msg   string
	usage string
}

func (e *usageError) Error() string { return e.msg }

func run(args []string) error {
	command := "demo"
	if len(args) > 0 {
		command = args[0]
	}
	switch command {
	case "demo":
		runDemo()
		return nil
	case "version":
		fmt.Println("govflow 0.1.0")
		return nil
	case "help", "-h", "--help":
		fmt.Print(rootUsage())
		return nil
	case "batch-register":
		return runBatchRegister(args[1:], os.Stdout)
	case "batch-import":
		return runBatchImport(args[1:], os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", command)
		fmt.Fprint(os.Stderr, rootUsage())
		os.Exit(2)
		return nil
	}
}

func rootUsage() string {
	return `govflow - DAO 治理提案与执行平台 / supply-chain batch registry

usage:
  govflow                                  run the built-in demo (default)
  govflow demo                             run the built-in demo
  govflow version                          print the version
  govflow help                             show this help
  govflow batch-register --registry FILE \
      --batch ID --product ID --quantity N --unit UNIT
                                           register one supply-chain batch in
                                           the registry FILE (see below)
  govflow batch-import --registry FILE --input FILE
                                           register every batch listed in the
                                           JSON manifest FILE into the registry
                                           FILE, all records or none

batch-register flags:
  --registry FILE   registry file to read and write (required)
  --batch ID        batch number; unique within one registry file (required)
  --product ID      product number the batch belongs to (required)
  --quantity N      positive decimal integer, digits 0-9 only, leading
                    zeros allowed, max 9223372036854775807 (required)
  --unit UNIT       measurement unit, e.g. kg or box (required)

batch-import flags:
  --registry FILE   registry file to read and write (required)
  --input FILE      read-only JSON manifest (required); a non-empty array of
                    objects carrying exactly batch, product, quantity and
                    unit. Text fields must be non-blank after trimming, and
                    quantity must be a JSON integer from 1 to
                    9223372036854775807 (no strings, fractions or exponents)

The whole manifest is rejected unless every record can be registered or
confirmed as a duplicate: a batch id already stored or seen earlier in the
manifest must match product, quantity and unit exactly, or none of the new
batches are saved and the error names the 1-based record position, the batch
id and the mismatching field(s). On success stdout contains one JSON object
whose results array keeps manifest order, each entry carrying the normalized
four fields plus status "created" or "duplicate"; new batches are appended
in first-occurrence order and an all-duplicate manifest leaves the registry
file byte-for-byte untouched.

Batch, product and unit values have leading and trailing whitespace removed
and must be non-empty afterwards; interior characters and casing are kept
("B1" and "b1" are different batches). One invocation registers one batch.

Batch, product and unit must be valid UTF-8 text in every source: command
line flags, import manifest records and stored registry records. A value
containing malformed UTF-8 bytes (or a JSON string holding a lone surrogate
escape such as "\ud800") is rejected and never replaced with the U+FFFD
replacement character, so distinct inputs cannot collapse onto one batch
id; the error names the parameter, or the manifest/registry file with the
1-based record position and field (plus the batch id only when it is itself
valid and unambiguous). A genuinely entered U+FFFD character is ordinary
text, non-ASCII ids (Chinese text, emoji, ...) are welcome, and a JSON
escape means the same text as the character written directly.

On success stdout contains a single JSON object, e.g.
  {"batch":"B-001","product":"P-7","quantity":120,"unit":"kg","status":"created"}
status is "created" for a new record and "duplicate" when the same batch was
already registered with the identical product, quantity and unit; a duplicate
does not rewrite the file and is safe to retry. Registering an existing batch
with any different field is rejected; the error names the batch and the
mismatching field(s) and no record is changed.

registry file format (UTF-8 JSON, human-inspectable):
  {
    "version": 1,
    "batches": [
      {"batch": "B-001", "product": "P-7", "quantity": 120, "unit": "kg"}
    ]
  }
A missing file is created on first registration. The registry path may be a
symbolic link to an existing registry file: new batches are saved into the
linked file (a relative link target is resolved against the link's own
directory), the link itself is left untouched and the target keeps its
permissions. A link whose target does not exist, or a loop of links, is
rejected — the target is never created and the link is never replaced. An
existing file that is empty, cannot be read in this format, or contains
several records with the same batch number is rejected outright and never
overwritten. "This format"
is strict: the root object must carry exactly version (the integer 1) and
batches (an array, empty allowed), each record exactly the four lowercase
fields batch, product, quantity and unit; missing, null, mistyped, unknown
or case-variant fields (Version, Batch, ...) are rejected, and so is any
field appearing twice in one object — even with identical values, even when
one spelling hides behind a JSON escape. The error names the file, the
field and the reason, plus the 1-based record position (and the batch id
when it is unambiguous) for record-level problems. Failures print
the reason to stderr and exit non-zero without touching the registry.
`
}

func runBatchRegister(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("batch-register", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	registryPath := fs.String("registry", "", "registry `file` to read and write")
	batchRaw := fs.String("batch", "", "batch number (unique within the registry)")
	productRaw := fs.String("product", "", "product number")
	quantityRaw := fs.String("quantity", "", "positive decimal integer quantity")
	unitRaw := fs.String("unit", "", "measurement unit")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, rootUsage())
			return nil
		}
		return &usageError{msg: "batch-register: " + err.Error(), usage: batchRegisterUsage}
	}
	if fs.NArg() != 0 {
		return &usageError{
			msg:   fmt.Sprintf("batch-register: unexpected positional argument(s): %s", strings.Join(fs.Args(), " ")),
			usage: batchRegisterUsage,
		}
	}

	// Missing or malformed input must never create or modify the registry
	// file, so required flags are checked before reading or writing anything.
	required := []struct {
		name  string
		value string
	}{
		{"--registry", *registryPath},
		{"--batch", *batchRaw},
		{"--product", *productRaw},
		{"--quantity", *quantityRaw},
		{"--unit", *unitRaw},
	}
	var missing []string
	for _, f := range required {
		if f.value == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return &usageError{
			msg:   "batch-register: missing required flag(s): " + strings.Join(missing, ", "),
			usage: batchRegisterUsage,
		}
	}

	batch, err := batchreg.NormalizeField(*batchRaw)
	if err != nil {
		return &usageError{msg: "batch-register: invalid --batch: " + err.Error(), usage: batchRegisterUsage}
	}
	product, err := batchreg.NormalizeField(*productRaw)
	if err != nil {
		return &usageError{msg: "batch-register: invalid --product: " + err.Error(), usage: batchRegisterUsage}
	}
	unit, err := batchreg.NormalizeField(*unitRaw)
	if err != nil {
		return &usageError{msg: "batch-register: invalid --unit: " + err.Error(), usage: batchRegisterUsage}
	}
	quantity, err := batchreg.ParseQuantity(*quantityRaw)
	if err != nil {
		return &usageError{msg: "batch-register: invalid --quantity: " + err.Error(), usage: batchRegisterUsage}
	}

	reg, existed, err := batchreg.Load(*registryPath)
	if err != nil {
		return err
	}
	outcome, err := batchreg.Register(reg, batchreg.Input{
		Batch: batch, Product: product, Quantity: quantity, Unit: unit,
	})
	if err != nil {
		return err
	}
	if outcome.Created {
		if err := batchreg.Save(*registryPath, reg); err != nil {
			if !existed {
				// Nothing was registered before; drop any partial new file.
				removePartialNewRegistry(*registryPath)
			}
			return err
		}
	}

	status := "duplicate"
	if outcome.Created {
		status = "created"
	}
	result := struct {
		Batch    string `json:"batch"`
		Product  string `json:"product"`
		Quantity int64  `json:"quantity"`
		Unit     string `json:"unit"`
		Status   string `json:"status"`
	}{
		Batch:    outcome.Batch.Batch,
		Product:  outcome.Batch.Product,
		Quantity: outcome.Batch.Quantity,
		Unit:     outcome.Batch.Unit,
		Status:   status,
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, string(payload))
	return err
}

func runBatchImport(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("batch-import", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	registryPath := fs.String("registry", "", "registry `file` to read and write")
	inputPath := fs.String("input", "", "read-only JSON manifest `file` to import")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, rootUsage())
			return nil
		}
		return &usageError{msg: "batch-import: " + err.Error(), usage: batchImportUsage}
	}
	if fs.NArg() != 0 {
		return &usageError{
			msg:   fmt.Sprintf("batch-import: unexpected positional argument(s): %s", strings.Join(fs.Args(), " ")),
			usage: batchImportUsage,
		}
	}
	if *registryPath == "" || *inputPath == "" {
		var missing []string
		if *registryPath == "" {
			missing = append(missing, "--registry")
		}
		if *inputPath == "" {
			missing = append(missing, "--input")
		}
		return &usageError{
			msg:   "batch-import: missing required flag(s): " + strings.Join(missing, ", "),
			usage: batchImportUsage,
		}
	}

	// The manifest is read-only and is parsed (and fully validated) before
	// the registry is loaded or written, so bad input cannot touch it.
	data, err := os.ReadFile(*inputPath)
	if err != nil {
		return fmt.Errorf("batch-import: cannot read input file %q: %w", *inputPath, err)
	}
	inputs, err := batchreg.ParseManifest(data)
	if err != nil {
		return fmt.Errorf("batch-import: invalid input file %q: %w", *inputPath, err)
	}

	reg, existed, err := batchreg.Load(*registryPath)
	if err != nil {
		return fmt.Errorf("batch-import: %w", err)
	}
	outcomes, err := batchreg.Import(reg, inputs)
	if err != nil {
		return fmt.Errorf("batch-import: %w", err)
	}

	type importResultJSON struct {
		Batch    string `json:"batch"`
		Product  string `json:"product"`
		Quantity int64  `json:"quantity"`
		Unit     string `json:"unit"`
		Status   string `json:"status"`
	}
	results := make([]importResultJSON, len(outcomes))
	anyCreated := false
	for i, o := range outcomes {
		status := "duplicate"
		if o.Created {
			status = "created"
			anyCreated = true
		}
		results[i] = importResultJSON{
			Batch:    o.Batch.Batch,
			Product:  o.Batch.Product,
			Quantity: o.Batch.Quantity,
			Unit:     o.Batch.Unit,
			Status:   status,
		}
	}

	// An all-duplicate manifest must leave bytes and mtime untouched, so the
	// registry is written only when at least one record was newly created.
	if anyCreated {
		if err := batchreg.Save(*registryPath, reg); err != nil {
			if !existed {
				// The registry never existed before this import; drop any
				// partial new file so a failed save leaves nothing behind.
				removePartialNewRegistry(*registryPath)
			}
			return fmt.Errorf("batch-import: %w", err)
		}
	}

	payload, err := json.Marshal(struct {
		Results []importResultJSON `json:"results"`
	}{Results: results})
	if err != nil {
		return fmt.Errorf("batch-import: %w", err)
	}
	_, err = fmt.Fprintln(stdout, string(payload))
	if err != nil {
		return fmt.Errorf("batch-import: %w", err)
	}
	return nil
}

// removePartialNewRegistry drops a partially written new registry file after
// a failed first save. A symbolic link is never removed: the link is the
// user's pointer to the real registry file, not a partial creation of this
// run.
func removePartialNewRegistry(path string) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return
	}
	os.Remove(path)
}

func runDemo() {
	proposal := govflow.Proposal{ID: "gip-7", Title: "fund the audit workstream", State: "voting", Quorum: 1000,
		TimelockEnd: 5000, Actions: []string{"transfer:audits:25000"}}
	if err := govflow.Vote(&proposal, 700, true); err != nil {
		fmt.Println("vote refused:", err)
	}
	if err := govflow.Vote(&proposal, 200, false); err != nil {
		fmt.Println("vote refused:", err)
	}
	next := govflow.Tally(&proposal)
	fmt.Printf("proposal=%s for=%d against=%d quorum=%d next=%s\n", proposal.ID, proposal.ForVotes, proposal.AgainstVotes, proposal.Quorum, next)
	proposal.State = next
	proposal.TimelockEnd = 400
	fmt.Println("executable now:", govflow.Executable([]govflow.Proposal{proposal}, 1000))
}
