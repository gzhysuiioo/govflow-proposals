// Command govflow is the DAO 治理提案与执行平台 entry point.
package main

import (
	"encoding/json"
	"errors"
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

// rawDelegationList 收集重复的 --delegate FROM:TO 参数原文，保持提交顺序。
// FROM 与 TO 都可能含冒号，单条参数自身无法确定切分点，必须等全部成员
// 解析完后结合完整名单一起理解，因此这里只保留原文，不在逐个参数处切分。
type rawDelegationList []string

func (d *rawDelegationList) String() string { return fmt.Sprint([]string(*d)) }
func (d *rawDelegationList) Set(v string) error {
	*d = append(*d, v)
	return nil
}

// delegationUsageError 是 --delegate 的参数错误（格式错误或有歧义），退出码 2。
type delegationUsageError struct{ msg string }

func (e *delegationUsageError) Error() string { return e.msg }

// resolveDelegations 结合本次提交的完整成员名单，把每条 FROM:TO 原文解析成
// 确定的一对成员。解析只依赖完整名单，与 --member/--delegate 的排列先后无关。
func resolveDelegations(raw []string, members []govflow.VoteMember) ([]govflow.Delegation, error) {
	roster := make(map[string]bool, len(members))
	for _, m := range members {
		roster[m.ID] = true
	}
	delegations := make([]govflow.Delegation, 0, len(raw))
	for _, arg := range raw {
		from, to, err := resolveDelegation(arg, roster)
		if err != nil {
			return nil, err
		}
		delegations = append(delegations, govflow.Delegation{From: from, To: to})
	}
	return delegations, nil
}

// resolveDelegation 沿参数原文的每个冒号尝试切分，两端都逐字（区分大小写）
// 等于名单中完整编号的切分才成立。恰好一种切分成立时按该对成员创建委托；
// 多种切分都成立时该委托有歧义，返回参数错误，绝不默认选择任意一对；
// 任何切分都无法同时命中名单双方时返回域错误并指出无法匹配的参数。
// 无冒号或首末为冒号（FROM/TO 为空）仍按原有格式错误处理。
func resolveDelegation(arg string, roster map[string]bool) (string, string, error) {
	if !strings.Contains(arg, ":") || strings.HasPrefix(arg, ":") || strings.HasSuffix(arg, ":") {
		return "", "", &delegationUsageError{fmt.Sprintf("malformed delegation %q, expected FROM:TO", arg)}
	}
	type split struct{ from, to string }
	var matches []split
	for i := 0; i < len(arg); i++ {
		if arg[i] != ':' {
			continue
		}
		from, to := arg[:i], arg[i+1:]
		if roster[from] && roster[to] {
			matches = append(matches, split{from, to})
		}
	}
	switch len(matches) {
	case 1:
		return matches[0].from, matches[0].to, nil
	case 0:
		return "", "", fmt.Errorf("%w: delegation %q does not match any pair of roster members",
			govflow.ErrInvalidProposal, arg)
	default:
		pairs := make([]string, len(matches))
		for i, m := range matches {
			pairs[i] = fmt.Sprintf("%s -> %s", m.from, m.to)
		}
		return "", "", &delegationUsageError{fmt.Sprintf("delegation %q is ambiguous: matches more than one pair of roster members (%s)",
			arg, strings.Join(pairs, "; "))}
	}
}

func runCreateVote(args []string) {
	cf := newCmdFlags("create-vote")
	id := cf.fs.String("id", "", "proposal id (non-empty)")
	var members memberList
	cf.fs.Var(&members, "member", "member ID:WEIGHT with positive int64 weight (repeatable, order irrelevant to equality)")
	var delegations rawDelegationList
	cf.fs.Var(&delegations, "delegate", "delegation FROM:TO (repeatable, order irrelevant to equality; FROM/TO may contain colons and are resolved against the full member roster)")
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
	// 委托必须结合完整成员名单解析，且失败时不触碰状态文件：
	// 格式错误或有歧义是参数错误（退出码 2），找不到名单内双方是域错误（退出码 1）。
	resolved, err := resolveDelegations(delegations, members)
	if err != nil {
		var usageErr *delegationUsageError
		if errors.As(err, &usageErr) {
			fmt.Fprintf(os.Stderr, "usage error: %v\n", err)
			os.Exit(2)
		}
		fail(err)
	}

	store, err := govflow.Open(cf.state)
	if err != nil {
		fail(err)
	}
	defer store.Close()
	in := &govflow.CreateVoteInput{
		ID:          *id,
		Members:     []govflow.VoteMember(members),
		Delegations: resolved,
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
	// 一次快照读取保证资金库与账户余额来自同一份已提交状态，
	// 不会把并发执行前后的数字拼在一起。
	snapshot, err := store.BalanceSnapshot()
	if err != nil {
		fail(err)
	}
	if *account != "" {
		// 未出现过的账户余额为 0，且不向状态文件新增账户记录。
		balance := snapshot.Balances[*account]
		if cf.asJSON {
			emitJSON(map[string]any{"treasury": snapshot.Treasury, "account": *account, "balance": balance})
		} else {
			fmt.Printf("treasury %d\n%s %d\n", snapshot.Treasury, *account, balance)
		}
		return
	}
	if cf.asJSON {
		emitJSON(map[string]any{"treasury": snapshot.Treasury, "balances": snapshot.Balances})
		return
	}
	fmt.Printf("treasury %d\n", snapshot.Treasury)
	accounts := make([]string, 0, len(snapshot.Balances))
	for acct := range snapshot.Balances {
		accounts = append(accounts, acct)
	}
	sort.Strings(accounts)
	for _, acct := range accounts {
		fmt.Printf("%s %d\n", acct, snapshot.Balances[acct])
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
	// 一次快照读取保证登记提案与投票提案来自同一份已提交状态，
	// 不会把并发执行前后的提案状态拼在一起。
	snapshot, err := store.ProposalsSnapshot()
	if err != nil {
		fail(err)
	}
	registered := snapshot.Registered
	voting := snapshot.Voting
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
