// Command govflow is the DAO 治理提案与执行平台 entry point.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
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
	case "init":
		runInit(os.Args[2:])
	case "register":
		runRegister(os.Args[2:])
	case "execute":
		runExecute(os.Args[2:])
	case "balance":
		runBalance(os.Args[2:])
	case "proposal":
		runProposal(os.Args[2:])
	case "receipts":
		runReceipts(os.Args[2:])
	case "receipt":
		runReceipt(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", command)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Println("usage: govflow <command> [flags]")
	fmt.Println()
	fmt.Println("proposal lifecycle:")
	fmt.Println("  demo                                   run the built-in demo")
	fmt.Println("  version                                print the version")
	fmt.Println()
	fmt.Println("local treasury execution:")
	fmt.Println("  init     -f FILE -balance N            create a new state file")
	fmt.Println("  register -f FILE -id ID [flags]         register a passed proposal")
	fmt.Println("  execute  -f FILE -id ID -now N          execute a proposal")
	fmt.Println("  balance  -f FILE                        show treasury and account balances")
	fmt.Println("  proposal -f FILE -id ID                 show one proposal")
	fmt.Println("  receipts -f FILE                        list all receipts in commit order")
	fmt.Println("  receipt  -f FILE -id ID                 show one receipt")
	fmt.Println()
	fmt.Println("run 'govflow <command> -h' for flag details.")
}

// ---------------------------------------------------------------------------
// init
// ---------------------------------------------------------------------------

func runInit(args []string) {
	fs := newFlagSet("init")
	path := fs.String("f", "", "state file path (required)")
	balance := fs.Int64("balance", 0, "initial treasury balance (required, >= 0)")
	parseFlags(fs, args)
	if *path == "" {
		fatal("init: -f is required")
	}
	if fs.NArg() != 0 {
		fatal("init: unexpected positional arguments")
	}
	st, err := govflow.CreateStore(*path, *balance)
	if err != nil {
		fatal("init: %v", err)
	}
	treasury, _, err := st.Balance()
	if err != nil {
		fatal("init: %v", err)
	}
	fmt.Printf("initialized %s with treasury balance %d\n", *path, treasury)
}

// ---------------------------------------------------------------------------
// register
// ---------------------------------------------------------------------------

func runRegister(args []string) {
	fs := newFlagSet("register")
	path := fs.String("f", "", "state file path (required)")
	id := fs.String("id", "", "proposal id (required, non-empty)")
	title := fs.String("title", "", "proposal title")
	forVotes := fs.Int64("for", 0, "for votes")
	againstVotes := fs.Int64("against", 0, "against votes")
	quorum := fs.Int64("quorum", 0, "quorum")
	timelock := fs.Int64("timelock", 0, "timelock end (unix seconds)")
	var actions stringList
	fs.Var(&actions, "action", "action text, repeatable (e.g. transfer:audits:25000)")
	parseFlags(fs, args)
	if *path == "" {
		fatal("register: -f is required")
	}
	if *id == "" {
		fatal("register: -id is required")
	}
	if fs.NArg() != 0 {
		fatal("register: unexpected positional arguments")
	}
	p := govflow.Proposal{
		ID:           *id,
		Title:        *title,
		State:        "passed",
		ForVotes:     *forVotes,
		AgainstVotes: *againstVotes,
		Quorum:       *quorum,
		TimelockEnd:  *timelock,
		Actions:      []string(actions),
	}
	st, err := govflow.OpenStore(*path)
	if err != nil {
		fatal("register: %v", err)
	}
	existing, err := st.Register(p)
	if err != nil {
		fatal("register: %v", err)
	}
	if existing.State == "executed" {
		fmt.Printf("proposal %s already registered (state=executed); returning stored record\n", *id)
	} else {
		fmt.Printf("registered %s (state=%s, timelock=%d, actions=%d)\n", *id, existing.State, existing.TimelockEnd, len(existing.Actions))
	}
}

// ---------------------------------------------------------------------------
// execute
// ---------------------------------------------------------------------------

func runExecute(args []string) {
	fs := newFlagSet("execute")
	path := fs.String("f", "", "state file path (required)")
	id := fs.String("id", "", "proposal id (required)")
	now := fs.Int64("now", 0, "current time, unix seconds (required)")
	parseFlags(fs, args)
	if *path == "" {
		fatal("execute: -f is required")
	}
	if *id == "" {
		fatal("execute: -id is required")
	}
	if fs.NArg() != 0 {
		fatal("execute: unexpected positional arguments")
	}
	st, err := govflow.OpenStore(*path)
	if err != nil {
		fatal("execute: %v", err)
	}
	receipt, err := st.Execute(*id, *now)
	if err != nil {
		fatal("execute: %v", err)
	}
	printReceipt(receipt)
}

func printReceipt(r *govflow.Receipt) {
	fmt.Printf("receipt for %s (executed at %d)\n", r.ProposalID, r.ExecutedAt)
	fmt.Printf("  treasury: %d -> %d\n", r.TreasuryBefore, r.TreasuryAfter)
	for _, a := range r.Actions {
		fmt.Printf("  %s  amount %d  balance %d -> %d\n", a.Account, a.Amount, a.BalanceBefore, a.BalanceAfter)
	}
}

// ---------------------------------------------------------------------------
// balance
// ---------------------------------------------------------------------------

func runBalance(args []string) {
	fs := newFlagSet("balance")
	path := fs.String("f", "", "state file path (required)")
	parseFlags(fs, args)
	if *path == "" {
		fatal("balance: -f is required")
	}
	if fs.NArg() != 0 {
		fatal("balance: unexpected positional arguments")
	}
	st, err := govflow.OpenStore(*path)
	if err != nil {
		fatal("balance: %v", err)
	}
	treasury, accounts, err := st.Balance()
	if err != nil {
		fatal("balance: %v", err)
	}
	fmt.Printf("treasury: %d\n", treasury)
	names := make([]string, 0, len(accounts))
	for name := range accounts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Printf("  %s: %d\n", name, accounts[name])
	}
}

// ---------------------------------------------------------------------------
// proposal
// ---------------------------------------------------------------------------

func runProposal(args []string) {
	fs := newFlagSet("proposal")
	path := fs.String("f", "", "state file path (required)")
	id := fs.String("id", "", "proposal id (required)")
	parseFlags(fs, args)
	if *path == "" {
		fatal("proposal: -f is required")
	}
	if *id == "" {
		fatal("proposal: -id is required")
	}
	if fs.NArg() != 0 {
		fatal("proposal: unexpected positional arguments")
	}
	st, err := govflow.OpenStore(*path)
	if err != nil {
		fatal("proposal: %v", err)
	}
	p, err := st.GetProposal(*id)
	if err != nil {
		fatal("proposal: %v", err)
	}
	fmt.Printf("id=%s\n", p.ID)
	fmt.Printf("state=%s\n", p.State)
	fmt.Printf("title=%q\n", p.Title)
	fmt.Printf("for=%d against=%d quorum=%d\n", p.ForVotes, p.AgainstVotes, p.Quorum)
	fmt.Printf("timelock_end=%d\n", p.TimelockEnd)
	fmt.Printf("actions:\n")
	for _, a := range p.Actions {
		fmt.Printf("  %s\n", a)
	}
}

// ---------------------------------------------------------------------------
// receipts / receipt
// ---------------------------------------------------------------------------

func runReceipts(args []string) {
	fs := newFlagSet("receipts")
	path := fs.String("f", "", "state file path (required)")
	parseFlags(fs, args)
	if *path == "" {
		fatal("receipts: -f is required")
	}
	if fs.NArg() != 0 {
		fatal("receipts: unexpected positional arguments")
	}
	st, err := govflow.OpenStore(*path)
	if err != nil {
		fatal("receipts: %v", err)
	}
	receipts, err := st.Receipts()
	if err != nil {
		fatal("receipts: %v", err)
	}
	if len(receipts) == 0 {
		fmt.Println("no receipts")
		return
	}
	for i, r := range receipts {
		fmt.Printf("%d. ", i+1)
		printReceipt(&r)
	}
}

func runReceipt(args []string) {
	fs := newFlagSet("receipt")
	path := fs.String("f", "", "state file path (required)")
	id := fs.String("id", "", "proposal id (required)")
	parseFlags(fs, args)
	if *path == "" {
		fatal("receipt: -f is required")
	}
	if *id == "" {
		fatal("receipt: -id is required")
	}
	if fs.NArg() != 0 {
		fatal("receipt: unexpected positional arguments")
	}
	st, err := govflow.OpenStore(*path)
	if err != nil {
		fatal("receipt: %v", err)
	}
	r, err := st.GetReceipt(*id)
	if err != nil {
		fatal("receipt: %v", err)
	}
	printReceipt(r)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// stringList is a repeatable flag.Value.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error {
	*l = append(*l, v)
	return nil
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

func parseFlags(fs *flag.FlagSet, args []string) {
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			os.Exit(0)
		}
		os.Exit(2)
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
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
