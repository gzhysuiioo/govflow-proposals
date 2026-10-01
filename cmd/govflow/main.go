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
	"strings"

	"github.com/gzhysuiioo/govflow-proposals/govflow"
)

const versionText = "govflow 0.2.0"

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
	case "create-vote":
		runCreateVote(args)
	case "vote":
		runVote(args)
	case "tally":
		runTally(args)
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
  create-vote --state FILE --id ID --member ID:W [--member ID:W ...]
             [--delegate FROM:TO ...] --quorum N --start N --deadline N
             --timelock N [--action A ...]
             create a voting proposal with roster, weights, delegations and a
             voting window (idempotent on identical content; conflict otherwise)
  vote       --state FILE --id ID --voter M --choice for|against --now N
             cast one ballot as a final representative (repeat same choice is
             idempotent; changing choice errors)
  tally      --state FILE --id ID --now N
             tally a proposal at/after its deadline; repeat calls return the
             first result
  execute    --state FILE --id ID --now N
             execute a passed proposal whose timelock has elapsed
             (works for both registered and voted proposals)
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

// memberList 收集重复的 --member ID:WEIGHT 参数，保持提交顺序。
type memberList []govflow.VoteMember

func (m *memberList) String() string { return fmt.Sprint([]govflow.VoteMember(*m)) }
func (m *memberList) Set(v string) error {
	id, weight, err := splitMember(v)
	if err != nil {
		return err
	}
	*m = append(*m, govflow.VoteMember{ID: id, Weight: weight})
	return nil
}

func splitMember(raw string) (string, int64, error) {
	idx := strings.LastIndex(raw, ":")
	if idx <= 0 || idx == len(raw)-1 {
		return "", 0, fmt.Errorf("malformed member %q, expected ID:WEIGHT", raw)
	}
	id := raw[:idx]
	weight, err := strconv.ParseInt(raw[idx+1:], 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("malformed member %q: weight must be int64: %v", raw, err)
	}
	return id, weight, nil
}

// delegationList 收集重复的 --delegate FROM:TO 参数，保持提交顺序。
type delegationList []govflow.Delegation

func (d *delegationList) String() string { return fmt.Sprint([]govflow.Delegation(*d)) }
func (d *delegationList) Set(v string) error {
	from, to, ok := strings.Cut(v, ":")
	if !ok || from == "" || to == "" {
		return fmt.Errorf("malformed delegation %q, expected FROM:TO", v)
	}
	*d = append(*d, govflow.Delegation{From: from, To: to})
	return nil
}

func runCreateVote(args []string) {
	cf := newCmdFlags("create-vote")
	id := cf.fs.String("id", "", "proposal id (non-empty)")
	var members memberList
	cf.fs.Var(&members, "member", "member ID:WEIGHT with positive int64 weight (repeatable, order irrelevant to equality)")
	var delegations delegationList
	cf.fs.Var(&delegations, "delegate", "delegation FROM:TO (repeatable, order irrelevant to equality)")
	quorumRaw := cf.fs.String("quorum", "", "required turnout weight between 1 and total member weight")
	startRaw := cf.fs.String("start", "", "voting start timestamp (int64, >= 0)")
	deadlineRaw := cf.fs.String("deadline", "", "voting deadline timestamp (int64, > start)")
	timelockRaw := cf.fs.String("timelock", "", "timelock end timestamp (int64, >= deadline)")
	var actions actionList
	cf.fs.Var(&actions, "action", "action text transfer:<account>:<amount> (repeatable, order preserved)")
	cf.parse(args)
	if *id == "" {
		fmt.Fprintf(os.Stderr, "usage error: --id is required\n")
		os.Exit(2)
	}
	quorum := parseIntFlag(cf.fs, "quorum", *quorumRaw)
	start := parseIntFlag(cf.fs, "start", *startRaw)
	deadline := parseIntFlag(cf.fs, "deadline", *deadlineRaw)
	timelock := parseIntFlag(cf.fs, "timelock", *timelockRaw)
	if len(members) == 0 {
		fmt.Fprintf(os.Stderr, "usage error: at least one --member ID:WEIGHT is required\n")
		os.Exit(2)
	}

	store, err := govflow.Open(cf.state)
	if err != nil {
		fail(err)
	}
	defer store.Close()
	in := &govflow.CreateVoteInput{
		ID:          *id,
		Members:     []govflow.VoteMember(members),
		Delegations: []govflow.Delegation(delegations),
		Quorum:      quorum,
		StartAt:     start,
		Deadline:    deadline,
		TimelockEnd: timelock,
		Actions:     []string(actions),
	}
	view, existed, err := store.CreateVoteProposal(in)
	if err != nil {
		fail(err)
	}
	if cf.asJSON {
		emitJSON(map[string]any{"proposal": view, "already_exists": existed})
		return
	}
	if existed {
		fmt.Printf("voting proposal %s already exists with identical content (state=%s)\n", view.ID, view.State)
	} else {
		fmt.Printf("created voting proposal %s: %d member(s), total_weight=%d, quorum=%d, voting=[%d,%d), timelock_end=%d, %d action(s)\n",
			view.ID, len(view.Members), view.TotalWeight, view.Quorum, view.StartAt, view.Deadline, view.TimelockEnd, len(view.Actions))
	}
}

func runVote(args []string) {
	cf := newCmdFlags("vote")
	id := cf.fs.String("id", "", "proposal id to vote on")
	voter := cf.fs.String("voter", "", "voting member (must be a final representative)")
	choice := cf.fs.String("choice", "", "vote choice: for or against")
	nowRaw := cf.fs.String("now", "", "current timestamp provided by the caller (int64)")
	cf.parse(args)
	if *id == "" || *voter == "" {
		fmt.Fprintf(os.Stderr, "usage error: --id and --voter are required\n")
		os.Exit(2)
	}
	var support bool
	switch *choice {
	case "for":
		support = true
	case "against":
		support = false
	default:
		fmt.Fprintf(os.Stderr, "usage error: --choice must be %q or %q\n", "for", "against")
		os.Exit(2)
	}
	now := parseIntFlag(cf.fs, "now", *nowRaw)

	store, err := govflow.Open(cf.state)
	if err != nil {
		fail(err)
	}
	defer store.Close()
	ballot, err := store.CastVote(*id, *voter, support, now)
	if err != nil {
		fail(err)
	}
	if cf.asJSON {
		emitJSON(ballot)
		return
	}
	fmt.Printf("recorded vote proposal=%s representative=%s choice=%s weight=%d voted_at=%d\n",
		*id, ballot.Representative, choiceWord(ballot.Support), ballot.Weight, ballot.VotedAt)
}

func choiceWord(support bool) string {
	if support {
		return "for"
	}
	return "against"
}

func runTally(args []string) {
	cf := newCmdFlags("tally")
	id := cf.fs.String("id", "", "proposal id to tally")
	nowRaw := cf.fs.String("now", "", "current timestamp provided by the caller (int64, >= deadline)")
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
	result, err := store.TallyVote(*id, now)
	if err != nil {
		fail(err)
	}
	if cf.asJSON {
		emitJSON(map[string]any{"id": *id, "result": result})
		return
	}
	verdict := "rejected"
	if result.Passed {
		verdict = "passed"
	}
	fmt.Printf("tally proposal=%s for=%d against=%d turnout=%d quorum=%d => %s (tallied_at=%d)\n",
		*id, result.ForWeight, result.AgainstWeight, result.Turnout, result.Quorum, verdict, result.TalliedAt)
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
	if ok {
		// 保持既有 JSON 形状：直接输出登记提案记录。
		if cf.asJSON {
			emitJSON(record)
			return
		}
		printProposalText(record)
		return
	}
	voteView, vok, verr := store.VoteProposal(*id)
	if verr != nil {
		fail(verr)
	}
	if !vok {
		fail(fmt.Errorf("%w: %s", govflow.ErrProposalNotFound, *id))
	}
	if cf.asJSON {
		emitJSON(voteView)
		return
	}
	printVoteProposalText(voteView)
}

func runProposals(args []string) {
	cf := newCmdFlags("proposals")
	cf.parse(args)
	store, err := govflow.Open(cf.state)
	if err != nil {
		fail(err)
	}
	defer store.Close()
	registered, err := store.Proposals()
	if err != nil {
		fail(err)
	}
	voting, err := store.VoteProposals()
	if err != nil {
		fail(err)
	}
	if cf.asJSON {
		// 保持既有 JSON 形状：一个数组；登记记录字段不变，投票提案带额外字段。
		merged := make([]any, 0, len(registered)+len(voting))
		for _, r := range registered {
			merged = append(merged, r)
		}
		for _, v := range voting {
			merged = append(merged, v)
		}
		emitJSON(merged)
		return
	}
	for _, record := range registered {
		printProposalText(record)
	}
	for _, view := range voting {
		printVoteProposalText(view)
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

func printVoteProposalText(v *govflow.VoteProposalView) {
	fmt.Printf("proposal %s source=vote state=%s quorum=%d voting=[%d,%d) timelock_end=%d actions=%d total_weight=%d\n",
		v.ID, v.State, v.Quorum, v.StartAt, v.Deadline, v.TimelockEnd, len(v.Actions), v.TotalWeight)
	for _, m := range v.Members {
		chain := strings.Join(m.Path, "->")
		if len(m.Path) == 1 {
			fmt.Printf("  member %s weight=%d representative=self\n", m.ID, m.Weight)
		} else {
			fmt.Printf("  member %s weight=%d path=%s\n", m.ID, m.Weight, chain)
		}
	}
	for i, action := range v.Actions {
		fmt.Printf("  action %d: %s\n", i, action)
	}
	for _, b := range v.Ballots {
		fmt.Printf("  ballot representative=%s choice=%s weight=%d voted_at=%d\n",
			b.Representative, choiceWord(b.Support), b.Weight, b.VotedAt)
	}
	if v.Tally != nil {
		verdict := "rejected"
		if v.Tally.Passed {
			verdict = "passed"
		}
		fmt.Printf("  tally for=%d against=%d turnout=%d => %s tallied_at=%d\n",
			v.Tally.ForWeight, v.Tally.AgainstWeight, v.Tally.Turnout, verdict, v.Tally.TalliedAt)
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
