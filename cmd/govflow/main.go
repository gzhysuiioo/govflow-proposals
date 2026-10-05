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
	// 以这次登记确认的同一条记录为准：编号、时间锁、动作原文与动作顺序都取自
	// 状态文件中的已确认记录，state 是确认时刻的实际状态——首次登记为 passed，
	// 相同内容重试保留已有状态（passed 或 executed），不能固定写成 passed。
	record, existed, err := store.RegisterRecord(*id, timelock, []string(actions))
	if err != nil {
		fail(err)
	}
	if cf.asJSON {
		emitJSON(map[string]any{"id": record.ID, "state": record.State, "timelock_end": record.TimelockEnd,
			"actions": record.Actions, "already_registered": existed})
		return
	}
	if existed {
		fmt.Printf("proposal %s already registered with identical content (state=%s)\n", record.ID, record.State)
	} else {
		fmt.Printf("registered %s proposal %s (%d action(s), timelock_end=%d)\n",
			record.State, record.ID, len(record.Actions), record.TimelockEnd)
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

// delegationList 收集重复的 --delegate FROM:TO 参数原文，保持提交顺序。
// 成员编号本身可含冒号（如 team:alice），FROM 与 TO 两端都可能含冒号，
// 因此不能在解析单个参数时切分：必须等完整成员名单齐备后，
// 由 resolveDelegations 结合名单确定唯一的切分点。
type delegationList []string

func (d *delegationList) String() string { return fmt.Sprint([]string(*d)) }
func (d *delegationList) Set(v string) error {
	*d = append(*d, v)
	return nil
}

// delegationUsageError 是 --delegate 的参数错误（格式错误或切分有歧义），
// 以退出码 2 结束；格式正确但无法匹配名单的错误不属于此类，按域错误退出码 1 处理。
type delegationUsageError struct{ reason string }

func (e *delegationUsageError) Error() string { return e.reason }

// resolveDelegations 结合本次提交的完整成员名单理解每条 FROM:TO 委托原文。
// 成员编号保持原文并区分大小写；编号可含冒号，且冒号可以出现在编号开头、
// 末尾或连续出现（如 ":alice"、"a:"、"a::b"），所以一条委托原文在每个冒号
// 位置都有一种候选切分，切分点两侧的编号仍须各自非空。
//
// 判定只依据“两端非空”这一纯语法条件与成员名单，不拿自委托/重复/循环等
// 业务规则替用户筛选候选：
//   - 原文不含冒号，或每个冒号位置的切分都会让某一端为空（如 ":alice"、
//     "alice:"、":"、"::"：冒号只出现在首尾、中间再无切分点）：格式错误，
//     参数错误（退出码 2）；
//   - 存在两端非空的切分，但没有任何一对都在名单里：域错误（退出码 1）；
//   - 两对或更多成员同时命中（即使其中某种解释随后会触发自委托、重复或循环）：
//     委托有歧义，参数错误（退出码 2），错误列出有歧义的原文与全部成员对；
//   - 恰好一对命中：按该对成员创建委托。
//
// 成员与委托参数在命令行上的排列先后不影响结果：解析前名单已收集完毕。
func resolveDelegations(members []govflow.VoteMember, raws []string) ([]govflow.Delegation, error) {
	roster := make(map[string]bool, len(members))
	for _, m := range members {
		roster[m.ID] = true
	}
	out := make([]govflow.Delegation, 0, len(raws))
	for _, raw := range raws {
		var matches []govflow.Delegation
		hasNonEmptySplit := false
		for i := 0; i < len(raw); i++ {
			if raw[i] != ':' {
				continue
			}
			from, to := raw[:i], raw[i+1:]
			// 纯语法候选：切分点两侧都非空才算“存在 FROM:TO 写法”。
			// 编号本身允许以冒号开头、结尾或包含连续冒号，因此位于位置 0
			// 的冒号产生的空 from、以及结尾冒号产生的空 to 都只是不合法的
			// 切分点之一，绝不据此提前拒绝整条原文——其他冒号位置仍可能合法。
			if from == "" || to == "" {
				continue
			}
			hasNonEmptySplit = true
			if roster[from] && roster[to] {
				matches = append(matches, govflow.Delegation{From: from, To: to})
			}
		}
		if !hasNonEmptySplit {
			return nil, &delegationUsageError{fmt.Sprintf("malformed delegation %q, expected FROM:TO with non-empty member ids on both sides", raw)}
		}
		switch len(matches) {
		case 0:
			return nil, fmt.Errorf("delegation %q does not match any pair of members on the roster", raw)
		case 1:
			out = append(out, matches[0])
		default:
			pairs := make([]string, len(matches))
			for i, m := range matches {
				pairs[i] = fmt.Sprintf("%q delegating to %q", m.From, m.To)
			}
			return nil, &delegationUsageError{fmt.Sprintf("ambiguous delegation %q: could mean %s",
				raw, strings.Join(pairs, " or "))}
		}
	}
	return out, nil
}

func runCreateVote(args []string) {
	cf := newCmdFlags("create-vote")
	id := cf.fs.String("id", "", "proposal id (non-empty)")
	var members memberList
	cf.fs.Var(&members, "member", "member ID:WEIGHT with positive int64 weight (repeatable, order irrelevant to equality)")
	var delegationRaws delegationList
	cf.fs.Var(&delegationRaws, "delegate", "delegation FROM:TO (repeatable, order irrelevant to equality; either side may itself contain colons and is matched against the full member roster)")
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
	// 委托切分依赖完整成员名单，必须在打开状态文件之前完成：
	// 格式错误与歧义是参数错误（退出码 2），无法匹配名单是域错误（退出码 1），
	// 两种失败都不触碰已有状态文件。
	delegations, derr := resolveDelegations([]govflow.VoteMember(members), []string(delegationRaws))
	if derr != nil {
		var usageErr *delegationUsageError
		if errors.As(derr, &usageErr) {
			fmt.Fprintf(os.Stderr, "usage error: %v\n", derr)
			os.Exit(2)
		}
		fail(derr)
	}

	store, err := govflow.Open(cf.state)
	if err != nil {
		fail(err)
	}
	defer store.Close()
	in := &govflow.CreateVoteInput{
		ID:          *id,
		Members:     []govflow.VoteMember(members),
		Delegations: delegations,
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
