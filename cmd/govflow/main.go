// Command govflow is the DAO 治理提案与执行平台 entry point.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"

	"github.com/gzhysuiioo/govflow-proposals/govflow"
)

const versionText = "govflow 0.1.0"

func main() {
	command := "demo"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	args := os.Args[2:]
	switch command {
	case "demo":
		runDemo()
	case "version":
		fmt.Println(versionText)
	case "help", "-h", "--help":
		usage()
	case "init":
		runInit(args)
	case "register":
		runRegister(args)
	case "execute":
		runExecute(args)
	case "balances":
		runBalances(args)
	case "proposal":
		runProposal(args)
	case "proposals":
		runProposals(args)
	case "receipt":
		runReceipt(args)
	case "receipts":
		runReceipts(args)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", command)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Println(`usage: govflow <command> [flags]

legacy commands:
  demo                         run the built-in voting demo
  version                      print the version

local treasury commands:
  init       --state FILE --balance N
             create a new state file with the initial treasury balance
  register   --state FILE --id ID --timelock N --action A [--action A ...]
             register a passed proposal (idempotent; conflicting content errors)
  execute    --state FILE --id ID --now N
             execute a passed proposal whose timelock has elapsed
  balances   --state FILE [--account ACCT]
             show treasury and recipient account balances
  proposal   --state FILE --id ID
             show one registered proposal
  proposals  --state FILE
             list all registered proposals by id
  receipt    --state FILE --id ID
             show the execution receipt of one proposal
  receipts   --state FILE
             list all receipts in the order they were successfully committed

all treasury commands accept --json for machine-readable output.`)
}

// fail prints a domain error to stderr and exits 1; usage errors exit 2.
func fail(err error) {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	os.Exit(1)
}

type commonFlags struct {
	state  string
	asJSON bool
	fs     *flag.FlagSet
}

func newCmdFlags(name string) *commonFlags {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cf := &commonFlags{fs: fs}
	fs.StringVar(&cf.state, "state", "", "path to the local treasury state file")
	fs.BoolVar(&cf.asJSON, "json", false, "emit machine-readable JSON")
	return cf
}

func (c *commonFlags) parse(args []string) {
	if err := c.fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "usage error: %v\n", err)
		c.fs.SetOutput(os.Stderr)
		c.fs.Usage()
		os.Exit(2)
	}
	if c.state == "" {
		fmt.Fprintf(os.Stderr, "usage error: --state is required\n")
		os.Exit(2)
	}
}

func emitJSON(v any) {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fail(err)
	}
	fmt.Println(string(raw))
}

func parseIntFlag(fs *flag.FlagSet, name, raw string) int64 {
	if raw == "" {
		fmt.Fprintf(os.Stderr, "usage error: --%s is required\n", name)
		os.Exit(2)
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "usage error: --%s must be an int64: %v\n", name, err)
		os.Exit(2)
	}
	return value
}

// actionList 收集重复出现的 --action 参数，保持登记顺序。
type actionList []string

func (a *actionList) String() string { return fmt.Sprint([]string(*a)) }
func (a *actionList) Set(v string) error {
	*a = append(*a, v)
	return nil
}

func runInit(args []string) {
	cf := newCmdFlags("init")
	balanceRaw := cf.fs.String("balance", "", "initial treasury balance (non-negative int64)")
	cf.parse(args)
	balance := parseIntFlag(cf.fs, "balance", *balanceRaw)

	store, err := govflow.InitTreasury(cf.state, balance)
	if err != nil {
		fail(err)
	}
	defer store.Close()
	if cf.asJSON {
		emitJSON(map[string]any{"state": cf.state, "treasury": balance, "initialized": true})
		return
	}
	fmt.Printf("initialized %s with treasury balance %d\n", cf.state, balance)
}

func runRegister(args []string) {
	cf := newCmdFlags("register")
	id := cf.fs.String("id", "", "proposal id (non-empty)")
	timelockRaw := cf.fs.String("timelock", "", "timelock end timestamp (int64)")
	var actions actionList
	cf.fs.Var(&actions, "action", "action text transfer:<account>:<amount> (repeatable, order preserved)")
	cf.parse(args)
	if *id == "" {
		fmt.Fprintf(os.Stderr, "usage error: --id is required\n")
		os.Exit(2)
	}
	timelock := parseIntFlag(cf.fs, "timelock", *timelockRaw)

	store, err := govflow.Open(cf.state)
	if err != nil {
		fail(err)
	}
	defer store.Close()
	existed, err := store.Register(*id, timelock, []string(actions))
	if err != nil {
		fail(err)
	}
	if cf.asJSON {
		emitJSON(map[string]any{"id": *id, "state": "passed", "timelock_end": timelock,
			"actions": []string(actions), "already_registered": existed})
		return
	}
	if existed {
		fmt.Printf("proposal %s already registered with identical content\n", *id)
	} else {
		fmt.Printf("registered passed proposal %s (%d action(s), timelock_end=%d)\n", *id, len(actions), timelock)
	}
}

func runExecute(args []string) {
	cf := newCmdFlags("execute")
	id := cf.fs.String("id", "", "proposal id to execute")
	nowRaw := cf.fs.String("now", "", "current timestamp provided by the caller (int64)")
	cf.parse(args)
	if *id == "" {
		fmt.Fprintf(os.Stderr, "usage error: --id is required\n")
		os.Exit(2)
	}
	now := parseIntFlag(cf.fs, "now", *nowRaw)

	store, err := govflow.Open(cf.state)
	if err != nil {
		fail(err)
	}
	defer store.Close()
	receipt, err := store.Execute(*id, now)
	if err != nil {
		fail(err)
	}
	if cf.asJSON {
		emitJSON(receipt)
		return
	}
	printReceiptText(receipt, false)
}

func runBalances(args []string) {
	cf := newCmdFlags("balances")
	account := cf.fs.String("account", "", "show only this recipient account balance (unknown accounts are 0)")
	cf.parse(args)
	store, err := govflow.Open(cf.state)
	if err != nil {
		fail(err)
	}
	defer store.Close()
	treasury, err := store.TreasuryBalance()
	if err != nil {
		fail(err)
	}
	if *account != "" {
		balance, err := store.Balance(*account)
		if err != nil {
			fail(err)
		}
		if cf.asJSON {
			emitJSON(map[string]any{"treasury": treasury, "account": *account, "balance": balance})
		} else {
			fmt.Printf("treasury %d\n%s %d\n", treasury, *account, balance)
		}
		return
	}
	balances, err := store.Balances()
	if err != nil {
		fail(err)
	}
	if cf.asJSON {
		emitJSON(map[string]any{"treasury": treasury, "balances": balances})
		return
	}
	fmt.Printf("treasury %d\n", treasury)
	accounts := make([]string, 0, len(balances))
	for acct := range balances {
		accounts = append(accounts, acct)
	}
	sort.Strings(accounts)
	for _, acct := range accounts {
		fmt.Printf("%s %d\n", acct, balances[acct])
	}
}

func runProposal(args []string) {
	cf := newCmdFlags("proposal")
	id := cf.fs.String("id", "", "proposal id")
	cf.parse(args)
	if *id == "" {
		fmt.Fprintf(os.Stderr, "usage error: --id is required\n")
		os.Exit(2)
	}
	store, err := govflow.Open(cf.state)
	if err != nil {
		fail(err)
	}
	defer store.Close()
	record, ok, err := store.Proposal(*id)
	if err != nil {
		fail(err)
	}
	if !ok {
		fail(fmt.Errorf("%w: %s", govflow.ErrProposalNotFound, *id))
	}
	if cf.asJSON {
		emitJSON(record)
		return
	}
	printProposalText(record)
}

func runProposals(args []string) {
	cf := newCmdFlags("proposals")
	cf.parse(args)
	store, err := govflow.Open(cf.state)
	if err != nil {
		fail(err)
	}
	defer store.Close()
	records, err := store.Proposals()
	if err != nil {
		fail(err)
	}
	if cf.asJSON {
		emitJSON(records)
		return
	}
	for _, record := range records {
		printProposalText(record)
	}
}

func runReceipt(args []string) {
	cf := newCmdFlags("receipt")
	id := cf.fs.String("id", "", "proposal id")
	cf.parse(args)
	if *id == "" {
		fmt.Fprintf(os.Stderr, "usage error: --id is required\n")
		os.Exit(2)
	}
	store, err := govflow.Open(cf.state)
	if err != nil {
		fail(err)
	}
	defer store.Close()
	receipt, ok, err := store.Receipt(*id)
	if err != nil {
		fail(err)
	}
	if !ok {
		fail(fmt.Errorf("no execution receipt for proposal %s", *id))
	}
	if cf.asJSON {
		emitJSON(receipt)
		return
	}
	printReceiptText(receipt, true)
}

func runReceipts(args []string) {
	cf := newCmdFlags("receipts")
	cf.parse(args)
	store, err := govflow.Open(cf.state)
	if err != nil {
		fail(err)
	}
	defer store.Close()
	receipts, err := store.Receipts()
	if err != nil {
		fail(err)
	}
	if cf.asJSON {
		emitJSON(receipts)
		return
	}
	for _, receipt := range receipts {
		printReceiptText(receipt, true)
	}
}

func printProposalText(p govflow.ProposalRecord) {
	fmt.Printf("proposal %s state=%s timelock_end=%d actions=%d\n", p.ID, p.State, p.TimelockEnd, len(p.Actions))
	for i, action := range p.Actions {
		fmt.Printf("  action %d: %s\n", i, action)
	}
}

func printReceiptText(r *govflow.Receipt, header bool) {
	if header {
		fmt.Printf("receipt proposal=%s order=%d executed_at=%d actions=%d\n", r.ProposalID, r.Order, r.ExecutedAt, len(r.Actions))
	} else {
		fmt.Printf("executed proposal %s at %d (%d action(s)); receipt order %d\n", r.ProposalID, r.ExecutedAt, len(r.Actions), r.Order)
	}
	for _, ar := range r.Actions {
		fmt.Printf("  action %d: %s\n", ar.Index, ar.Action)
		fmt.Printf("    treasury:  %d -> %d\n", ar.Treasury.Before, ar.Treasury.After)
		fmt.Printf("    %s: %d -> %d\n", ar.Recipient.Account, ar.Recipient.Before, ar.Recipient.After)
	}
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
