// Command govflow is the DAO 治理提案与执行平台 entry point.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gzhysuiioo/govflow-proposals/govflow"
)

func main() {
	command := "demo"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	switch command {
	case "demo":
		runDemo()
	case "version":
		fmt.Println("govflow 0.1.0")
	case "help", "-h", "--help":
		usage(os.Stdout)
	case "batch-register":
		os.Exit(runBatchRegister(os.Args[2:], os.Stdout, os.Stderr))
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", command)
		usage(os.Stderr)
		os.Exit(2)
	}
}

// usage writes the top-level command overview. The registry file format is
// part of the user-facing contract and is documented here as well as in the
// batch-register help.
func usage(w io.Writer) {
	fmt.Fprint(w, `usage: govflow <command> [arguments]

commands:
  demo             run the built-in governance demo
  version          print the govflow version
  help             show this help
  batch-register   register one batch in a local registry file

run 'govflow batch-register -h' for flags, the registry file format and the
rules for duplicate registration.
`)
}

// batchUsage is the full batch-register manual, including the public
// registry file format so users can inspect saved results and tell a duplicate
// registration apart from a batch-number conflict.
const batchUsage = `usage: govflow batch-register --registry FILE --batch ID --product ID --quantity N --unit UNIT

Register one batch in the registry file FILE. All flags are required and may
be given in any order; the operation registers exactly one batch.

flags:
  --registry FILE   path to the registry file (created on first registration)
  --batch ID        batch number; unique within the file; leading/trailing
                    whitespace is stripped; case-sensitive
  --product ID      product number; leading/trailing whitespace is stripped
  --quantity N      positive decimal integer (digits 0-9, optional leading
                    zeros); stored and compared by integer value; maximum
                    9223372036854775807
  --unit UNIT       unit of measure; leading/trailing whitespace is stripped

On success one JSON object is printed to stdout:
  {"batch":"B001","product":"P100","quantity":100,"unit":"箱","status":"new"}
status is "new" for a first registration and "duplicate" for an idempotent
retry. Nothing else is printed on stdout; failures print to stderr and exit
non-zero.

Registry file format (one JSON object per line; this is the public format):
  {"batch":"B001","product":"P100","quantity":100,"unit":"箱"}
- batch: batch number, unique within the file; whitespace stripped;
  case-sensitive.
- product: product number; whitespace stripped.
- quantity: positive decimal integer; stored by integer value.
- unit: unit of measure; whitespace stripped.

Duplicate registration: submitting the same batch number with identical
product, quantity and unit returns the existing record with status
"duplicate" and does not modify the file. Submitting with any field different
is rejected; the error names the batch number and the differing field(s), and
the existing record is left unchanged.

A new registry file is created only when the file does not exist. An existing
file that is empty, unreadable, malformed, or contains duplicate batch
numbers is rejected and never overwritten.
`

// batchOutput is the single JSON object written to stdout on success.
type batchOutput struct {
	Batch    string `json:"batch"`
	Product  string `json:"product"`
	Quantity int64  `json:"quantity"`
	Unit     string `json:"unit"`
	Status   string `json:"status"`
}

// runBatchRegister parses the flags, validates the input and performs the
// registration. It returns the process exit code: 0 on success, 2 on usage
// errors, 1 on registration failures. No registry file is created or
// modified when validation fails.
func runBatchRegister(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("batch-register", flag.ContinueOnError)
	// Capture flag output so help goes to stdout while parse errors go to
	// stderr.
	var flagOutput strings.Builder
	fs.SetOutput(&flagOutput)
	fs.Usage = func() { fmt.Fprint(&flagOutput, batchUsage) }

	registry := fs.String("registry", "", "path to the registry file")
	batch := fs.String("batch", "", "batch number (unique within the file)")
	product := fs.String("product", "", "product number")
	quantity := fs.String("quantity", "", "positive decimal integer, e.g. 12 or 0012")
	unit := fs.String("unit", "", "unit of measure")

	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			fmt.Fprint(stdout, flagOutput.String())
			return 0
		}
		fmt.Fprint(stderr, flagOutput.String())
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "batch-register: unexpected positional arguments")
		return 2
	}

	var missing []string
	for _, spec := range []struct {
		name  string
		value string
	}{
		{"--registry", *registry},
		{"--batch", *batch},
		{"--product", *product},
		{"--quantity", *quantity},
		{"--unit", *unit},
	} {
		if spec.value == "" {
			missing = append(missing, spec.name)
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(stderr, "batch-register: missing required flag(s): %s\n", strings.Join(missing, ", "))
		return 2
	}

	result, err := govflow.RegisterBatch(*registry, *batch, *product, *quantity, *unit)
	if err != nil {
		fmt.Fprintln(stderr, "batch-register:", err)
		return 1
	}

	status := "new"
	if result.Duplicate {
		status = "duplicate"
	}
	out, err := json.Marshal(batchOutput{
		Batch:    result.Record.Batch,
		Product:  result.Record.Product,
		Quantity: result.Record.Quantity,
		Unit:     result.Record.Unit,
		Status:   status,
	})
	if err != nil {
		fmt.Fprintln(stderr, "batch-register: cannot encode result:", err)
		return 1
	}
	fmt.Fprintln(stdout, string(out))
	return 0
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
