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
run 'govflow help' for the full description and the input file format.
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
                                           input FILE into the registry FILE in
                                           one atomic submission (see below)

batch-register flags:
  --registry FILE   registry file to read and write (required)
  --batch ID        batch number; unique within one registry file (required)
  --product ID      product number the batch belongs to (required)
  --quantity N      positive decimal integer, digits 0-9 only, leading
                    zeros allowed, max 9223372036854775807 (required)
  --unit UNIT       measurement unit, e.g. kg or box (required)

Batch, product and unit values have leading and trailing whitespace removed
and must be non-empty afterwards; interior characters and casing are kept
("B1" and "b1" are different batches). One invocation registers one batch.

On success stdout contains a single JSON object, e.g.
  {"batch":"B-001","product":"P-7","quantity":120,"unit":"kg","status":"created"}
status is "created" for a new record and "duplicate" when the same batch was
already registered with the identical product, quantity and unit; a duplicate
does not rewrite the file and is safe to retry. Registering an existing batch
with any different field is rejected; the error names the batch and the
mismatching field(s) and no record is changed.

batch-import flags:
  --registry FILE   registry file to read and write (required)
  --input FILE      read-only file holding the batch list (required)

The input file is a non-empty JSON array; each element is an object with
exactly the four fields batch, product, quantity, unit. The three text fields
have leading and trailing whitespace removed and must be non-empty afterwards;
quantity must be a JSON integer in 1..9223372036854775807 (strings, fractions
and exponents are rejected). The input file is never written.

Every record in the list is checked in order against the registry and against
records already seen in the same list: identical product, quantity and unit
means "duplicate"; any difference fails the whole import. A failing record is
reported with its 1-based position, batch number and mismatching field(s),
and no new batch from the list is saved. Only when every record is accepted
are new batches appended (in first-occurrence order) and the registry file
written; an all-duplicate list leaves the registry file untouched.

On success stdout contains a single JSON object, e.g.
  {"results":[{"batch":"B-001","product":"P-7","quantity":120,"unit":"kg","status":"created"}]}
The results array lists every record in list order with its normalized fields
and status ("created" or "duplicate").

registry file format (UTF-8 JSON, human-inspectable):
  {
    "version": 1,
    "batches": [
      {"batch": "B-001", "product": "P-7", "quantity": 120, "unit": "kg"}
    ]
  }
A missing file is created on first registration. An existing file that is
empty, cannot be read in this format, or contains several records with the
same batch number is rejected outright and never overwritten. Failures print
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
				os.Remove(*registryPath)
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
	inputPath := fs.String("input", "", "read-only `file` holding the batch list")
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

	// The input file is read-only and must never be created or modified.
	inputData, err := os.ReadFile(*inputPath)
	if err != nil {
		return fmt.Errorf("batch-import: cannot read input file %q: %w", *inputPath, err)
	}
	records, err := batchreg.ParseImportInput(inputData)
	if err != nil {
		return fmt.Errorf("batch-import: %w", err)
	}

	reg, existed, err := batchreg.Load(*registryPath)
	if err != nil {
		return err
	}
	results, err := batchreg.Import(reg, records)
	if err != nil {
		return err
	}

	// Save only when at least one record is new; an all-duplicate import
	// leaves the registry file's bytes and mtime untouched.
	anyCreated := false
	for _, r := range results {
		if r.Status == "created" {
			anyCreated = true
			break
		}
	}
	if anyCreated {
		if err := batchreg.Save(*registryPath, reg); err != nil {
			if !existed {
				// Nothing was registered before; drop any partial new file.
				os.Remove(*registryPath)
			}
			return err
		}
	}

	payload, err := json.Marshal(struct {
		Results []batchreg.ImportResult `json:"results"`
	}{Results: results})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, string(payload))
	return err
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
