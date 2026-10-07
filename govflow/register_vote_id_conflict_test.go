package govflow

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// 本文件为“登记提案与投票提案共用编号”这条规则补充回归保障：
// 同一个本地资金库、同一个非空编号，register 与 create-vote 两种来源各自
// 合法的内容也只能保存其中一种，另一种必须得到明确的编号冲突
// （ErrProposalConflict；命令行退出码 1、原因只在标准错误）。两种来源之间
// 不存在“内容相同就算同一次创建”的关系：即使时间锁与动作原文完全相同，
// 来源不同仍是冲突。创建请求之间没有来源优先级——并发同时到达时哪一方
// 先完成都可以，但只能有一方首次创建成功，失败方不得也返回创建成功、
// 不得留下第二份同编号记录、不得改写已保存提案的来源或状态。
//
// 覆盖三类场景：
//  1. 编号尚未占用时两种创建请求同时到达（库内并发与跨进程 CLI 并发）；
//  2. 投票提案到期计票被拒绝后，同编号登记仍必须冲突，拒绝结论、成员与
//     委托明细、实际票据与首次计票结论全部保留，原提案仍不能执行；
//  3. 不同编号的合法提案仍可由两种来源分别正常创建。

// crossSourceActions 是并发竞争双方共同提交的有序动作：时间锁与动作原文
// 在两种来源间完全相同，冲突只能来自“来源不同”，不能来自内容差异。
var crossSourceActions = []string{"transfer:audits:100", "transfer:legal:50"}

// registerCLIResponse 是 register --json 的成功结果：既有字段与
// already_registered 标志都必须保留，冲突不得被包装成一次成功重试。
type registerCLIResponse struct {
	ID                string   `json:"id"`
	State             string   `json:"state"`
	TimelockEnd       int64    `json:"timelock_end"`
	Actions           []string `json:"actions"`
	AlreadyRegistered bool     `json:"already_registered"`
}

// crossSourceResult 记录一个并发创建进程（任一来源）的全部可观察结果。
type crossSourceResult struct {
	source     string // "register" / "vote"
	code       int
	stdout     string
	stderr     string
	register   *registerCLIResponse
	createVote *createVoteCLIResponse
}

// crossSourceRegisterArgs 构造一次 register --json 调用的完整参数。
func crossSourceRegisterArgs(id string, timelock int64, actions []string) []string {
	args := []string{"register", "--id", id, "--timelock", itoa(int(timelock))}
	for _, a := range actions {
		args = append(args, "--action", a)
	}
	return append(args, "--json")
}

// assertConflictCLI 核对一次编号冲突的命令行结果：域错误退出码 1，
// 标准错误说明编号冲突（沿用现有错误含义并给出提案编号），标准输出
// 不出现任何成功创建记录（含成功重试标志）。
func assertConflictCLI(t *testing.T, r crossSourceResult, id string) {
	t.Helper()
	if r.code != 1 {
		t.Fatalf("%s request exit=%d, want 1 (stdout=%s stderr=%s)",
			r.source, r.code, r.stdout, r.stderr)
	}
	if strings.TrimSpace(r.stdout) != "" {
		t.Fatalf("%s conflict must not print a success record on stdout, got %q", r.source, r.stdout)
	}
	if strings.Contains(r.stdout, "already_registered") || strings.Contains(r.stdout, "already_exists") {
		t.Fatalf("%s conflict must not be wrapped as a successful retry: %q", r.source, r.stdout)
	}
	if !strings.Contains(r.stderr, "different timelock or actions") || !strings.Contains(r.stderr, id) {
		t.Fatalf("%s conflict stderr must name the id conflict and the proposal, got %q", r.source, r.stderr)
	}
}

// TestRegisterVoteSharedIDSequential 以确定性的先后顺序核对共用编号规则：
// 任一来源先占用编号后，另一来源即使提交相同时间锁与动作原文也必须得到
// ErrProposalConflict；冲突不留下第二份记录；先占方的相同内容重试仍幂等；
// 不同编号的合法提案仍可由两种来源分别正常创建。
func TestRegisterVoteSharedIDSequential(t *testing.T) {
	store, _ := openTempStore(t, 1000)
	defer store.Close()

	// register 先占用编号：create-vote 同编号（相同时间锁与动作）必须冲突。
	mustRegister(t, store, "gip-reg-first", 300, "transfer:audits:100")
	in := baseVoteInput("gip-reg-first") // timelock=300、actions=[transfer:audits:100]，与登记完全相同
	if _, _, err := store.CreateVoteProposal(in); !errors.Is(err, ErrProposalConflict) {
		t.Fatalf("create-vote over registered id err=%v, want ErrProposalConflict", err)
	}
	// 冲突不得留下第二份同编号记录：投票提案表中仍查无此编号。
	if _, ok, err := store.VoteProposal("gip-reg-first"); err != nil || ok {
		t.Fatalf("failed create-vote left a voting record: ok=%v err=%v", ok, err)
	}
	// 已保存提案的来源与状态不被改写：仍是登记来源的 passed。
	if rec, ok, err := store.Proposal("gip-reg-first"); err != nil || !ok || rec.State != "passed" {
		t.Fatalf("registered proposal changed by conflict: %+v ok=%v err=%v", rec, ok, err)
	}
	// 先占方的相同内容重试仍然幂等成功。
	if existed, err := store.Register("gip-reg-first", 300, []string{"transfer:audits:100"}); err != nil || !existed {
		t.Fatalf("identical register retry existed=%v err=%v", existed, err)
	}

	// create-vote 先占用编号：register 同编号（相同时间锁与动作）必须冲突。
	voteIn := baseVoteInput("gip-vote-first")
	mustCreateVote(t, store, voteIn)
	if _, _, err := store.RegisterProposal("gip-vote-first", 300, []string{"transfer:audits:100"}); !errors.Is(err, ErrProposalConflict) {
		t.Fatalf("register over voting id err=%v, want ErrProposalConflict", err)
	}
	// 冲突不得留下第二份同编号记录：登记表中仍查无此编号。
	if _, ok, err := store.Proposal("gip-vote-first"); err != nil || ok {
		t.Fatalf("failed register left a registered record: ok=%v err=%v", ok, err)
	}
	// 已保存提案仍是投票来源的 voting，定义不被改写。
	if v, ok, err := store.VoteProposal("gip-vote-first"); err != nil || !ok ||
		v.Source != "vote" || v.State != "voting" || v.TotalWeight != 1000 {
		t.Fatalf("voting proposal changed by conflict: %+v ok=%v err=%v", v, ok, err)
	}
	// 先占方的相同内容重试仍然幂等成功。
	if _, existed, err := store.CreateVoteProposal(voteIn); err != nil || !existed {
		t.Fatalf("identical create-vote retry existed=%v err=%v", existed, err)
	}

	// 不同编号的合法提案仍可由两种来源分别正常创建。
	mustRegister(t, store, "gip-reg-other", 0, "transfer:x:1")
	mustCreateVote(t, store, baseVoteInput("gip-vote-other"))
	snap, err := store.ProposalsSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Registered) != 2 || snap.Registered[0].ID != "gip-reg-first" || snap.Registered[1].ID != "gip-reg-other" {
		t.Fatalf("registered proposals = %+v", snap.Registered)
	}
	if len(snap.Voting) != 2 || snap.Voting[0].ID != "gip-vote-first" || snap.Voting[1].ID != "gip-vote-other" {
		t.Fatalf("voting proposals = %+v", snap.Voting)
	}

	// 整个冲突与创建过程不改变任何余额，也不产生执行凭据。
	if bal, err := store.TreasuryBalance(); err != nil || bal != 1000 {
		t.Fatalf("treasury balance=%d err=%v, want 1000", bal, err)
	}
	if receipts, err := store.Receipts(); err != nil || len(receipts) != 0 {
		t.Fatalf("receipts=%d err=%v, want none", len(receipts), err)
	}
}

// TestConcurrentRegisterVsCreateVoteSameID 在库内压“编号尚未占用时两种
// 创建请求同时到达”的竞争：两个 Store 句柄（进程内路径锁串行化）分别
// 并发提交 register 与 create-vote，双方内容各自合法且时间锁、动作原文
// 完全相同。不预设哪一来源获胜：必须恰好一个请求首次创建成功，胜出来源
// 的其余请求确认已有提案，另一来源的全部请求得到 ErrProposalConflict；
// 竞争结束后编号只保存一份记录，余额与凭据不变。
func TestConcurrentRegisterVsCreateVoteSameID(t *testing.T) {
	initStore, path := openTempStore(t, 1000)
	if err := initStore.Close(); err != nil {
		t.Fatal(err)
	}
	regStore, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer regStore.Close()
	voteStore, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer voteStore.Close()

	const id = "gip-cross-race"
	const perSource = 6
	type outcome struct {
		source  string
		existed bool
		err     error
	}
	results := make([]outcome, perSource*2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < perSource; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			<-start
			_, existed, err := regStore.RegisterProposal(id, 300, crossSourceActions)
			results[i] = outcome{source: "register", existed: existed, err: err}
		}(i)
		go func(i int) {
			defer wg.Done()
			<-start
			in := baseVoteInput(id)
			in.Actions = append([]string(nil), crossSourceActions...)
			_, existed, err := voteStore.CreateVoteProposal(in)
			results[perSource+i] = outcome{source: "vote", existed: existed, err: err}
		}(i)
	}
	close(start)
	wg.Wait()

	// 恰好一个首次创建；其来源即胜出来源，另一来源必须全部冲突。
	firstCreators := 0
	winnerSource := ""
	for _, r := range results {
		if r.err == nil && !r.existed {
			firstCreators++
			winnerSource = r.source
		}
	}
	if firstCreators != 1 {
		t.Fatalf("exactly one request may report first creation, got %d (results=%+v)", firstCreators, results)
	}
	for _, r := range results {
		if r.source == winnerSource {
			if r.err != nil {
				t.Fatalf("%s request in winning source must succeed, got %v", r.source, r.err)
			}
		} else if !errors.Is(r.err, ErrProposalConflict) {
			t.Fatalf("%s request in losing source err=%v, want ErrProposalConflict", r.source, r.err)
		}
	}

	// 编号只保存一份记录，来源与状态属于胜方；另一来源查无此编号。
	snap, err := regStore.ProposalsSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	switch winnerSource {
	case "register":
		if len(snap.Registered) != 1 || snap.Registered[0].ID != id || snap.Registered[0].State != "passed" ||
			!reflect.DeepEqual(snap.Registered[0].Actions, crossSourceActions) {
			t.Fatalf("registered side must hold exactly the winning passed proposal, got %+v", snap.Registered)
		}
		if len(snap.Voting) != 0 {
			t.Fatalf("losing create-vote must leave no voting record, got %+v", snap.Voting)
		}
	case "vote":
		if len(snap.Voting) != 1 || snap.Voting[0].ID != id || snap.Voting[0].State != "voting" ||
			snap.Voting[0].Source != "vote" || !reflect.DeepEqual(snap.Voting[0].Actions, crossSourceActions) {
			t.Fatalf("voting side must hold exactly the winning voting proposal, got %+v", snap.Voting)
		}
		if len(snap.Registered) != 0 {
			t.Fatalf("losing register must leave no registered record, got %+v", snap.Registered)
		}
	}

	// 竞争结束后失败方再试仍是冲突：已保存记录不被后来的请求覆盖。
	if _, _, err := regStore.RegisterProposal(id, 300, crossSourceActions); winnerSource == "vote" {
		if !errors.Is(err, ErrProposalConflict) {
			t.Fatalf("register retry after losing the race err=%v, want ErrProposalConflict", err)
		}
	} else if err != nil {
		t.Fatalf("register retry after winning the race must succeed, got %v", err)
	}
	in := baseVoteInput(id)
	in.Actions = append([]string(nil), crossSourceActions...)
	if _, _, err := voteStore.CreateVoteProposal(in); winnerSource == "register" {
		if !errors.Is(err, ErrProposalConflict) {
			t.Fatalf("create-vote retry after losing the race err=%v, want ErrProposalConflict", err)
		}
	} else if err != nil {
		t.Fatalf("create-vote retry after winning the race must succeed, got %v", err)
	}

	// 整个创建竞争不改变任何余额，也不产生执行凭据。
	if bal, err := regStore.TreasuryBalance(); err != nil || bal != 1000 {
		t.Fatalf("treasury balance=%d err=%v, want 1000", bal, err)
	}
	if receipts, err := regStore.Receipts(); err != nil || len(receipts) != 0 {
		t.Fatalf("receipts=%d err=%v, want none", len(receipts), err)
	}
}

// TestCrossProcessConcurrentRegisterVsCreateVote：多个独立命令进程同时
// 对同一空闲编号提交 register 与 create-vote，双方内容各自合法，时间锁
// （300）与有序动作原文完全相同。不预设登记方或投票方获胜：
//   - 恰好一个进程首次创建成功；胜出来源的其余进程确认已有提案，
//     且所有成功返回的内容与实际保存的提案对应；
//   - 另一来源的全部进程以退出码 1 失败，只在标准错误说明编号冲突，
//     标准输出不出现成功创建记录；
//   - 单项查询与全部提案查询对该编号给出同一个结果：登记方胜出时只有
//     登记来源的 passed 提案；投票方胜出时只有投票来源的 voting 提案，
//     保留原始成员权重、委托关系、投票窗口与有序动作；
//   - 整个竞争不改变任何余额，也不产生执行凭据。
func TestCrossProcessConcurrentRegisterVsCreateVote(t *testing.T) {
	binary := buildCLI(t)
	state := filepath.Join(t.TempDir(), "treasury.json")
	if _, _, code := runCLI(t, binary, state, "init", "--balance", "1000"); code != 0 {
		t.Fatal("init failed")
	}

	const id = "gip-cross-source"
	const perSource = 6
	results := make([]crossSourceResult, perSource*2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < perSource; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			<-start
			so, se, code := runCLI(t, binary, state, crossSourceRegisterArgs(id, 300, crossSourceActions)...)
			results[i] = crossSourceResult{source: "register", code: code, stdout: so, stderr: se}
		}(i)
		go func(i int) {
			defer wg.Done()
			<-start
			// 成员与委托排列在全部投票进程间固定：竞争只发生在来源之间。
			args := concCreateVoteArgs(id, concCreateRoster, []string{"bob:carol", "carol:alice"}, crossSourceActions)
			so, se, code := runCLI(t, binary, state, args...)
			results[perSource+i] = crossSourceResult{source: "vote", code: code, stdout: so, stderr: se}
		}(i)
	}
	close(start)
	wg.Wait()

	// 解析全部成功结果并找出唯一的首次创建者；失败结果逐条按冲突核对。
	firstCreators := 0
	winnerSource := ""
	for i := range results {
		r := &results[i]
		if r.code != 0 {
			continue // 冲突结果在确定胜方后统一核对
		}
		switch r.source {
		case "register":
			r.register = &registerCLIResponse{}
			if err := json.Unmarshal([]byte(r.stdout), r.register); err != nil {
				t.Fatalf("register success output is not JSON: %v\n%s", err, r.stdout)
			}
			if !r.register.AlreadyRegistered {
				firstCreators++
				winnerSource = "register"
			}
		case "vote":
			r.createVote = &createVoteCLIResponse{}
			if err := json.Unmarshal([]byte(r.stdout), r.createVote); err != nil {
				t.Fatalf("create-vote success output is not JSON: %v\n%s", err, r.stdout)
			}
			if r.createVote.Proposal == nil {
				t.Fatalf("create-vote success output missing proposal object: %s", r.stdout)
			}
			if !r.createVote.Existed {
				firstCreators++
				winnerSource = "vote"
			}
		}
	}
	if firstCreators != 1 {
		t.Fatalf("exactly one request may report first creation, got %d (results=%+v)", firstCreators, results)
	}

	// 胜出来源的全部进程成功且内容一致；失败来源的全部进程是明确的编号冲突。
	var winnerRegister *registerCLIResponse
	var winnerVote *VoteProposalView
	for i := range results {
		r := &results[i]
		if r.source != winnerSource {
			assertConflictCLI(t, *r, id)
			continue
		}
		if r.code != 0 {
			t.Fatalf("%s request in winning source must succeed: exit=%d stderr=%s", r.source, r.code, r.stderr)
		}
		switch r.source {
		case "register":
			if winnerRegister == nil {
				winnerRegister = r.register
			}
			// 内容字段必须一致；already_registered 标志在首次创建者与
			// 确认已有提案的重试之间本来就不同，不参与比较。
			if r.register.ID != winnerRegister.ID || r.register.State != winnerRegister.State ||
				r.register.TimelockEnd != winnerRegister.TimelockEnd ||
				!reflect.DeepEqual(r.register.Actions, winnerRegister.Actions) {
				t.Fatalf("register successes disagree on content: %+v vs %+v", r.register, winnerRegister)
			}
		case "vote":
			if winnerVote == nil {
				winnerVote = r.createVote.Proposal
			}
			if !reflect.DeepEqual(r.createVote.Proposal, winnerVote) {
				t.Fatalf("create-vote successes disagree:\n%+v\nvs\n%+v", r.createVote.Proposal, winnerVote)
			}
		}
	}

	// 单项查询与全部提案查询对该编号给出同一个结果，且就是胜方保存的内容。
	so, se, code := runCLI(t, binary, state, "proposal", "--id", id, "--json")
	if code != 0 {
		t.Fatalf("proposal query failed: %s", se)
	}
	lo, le, code := runCLI(t, binary, state, "proposals", "--json")
	if code != 0 {
		t.Fatalf("proposals query failed: %s", le)
	}
	var listed []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(lo), &listed); err != nil {
		t.Fatalf("proposals output is not JSON: %v\n%s", err, lo)
	}
	if len(listed) != 1 {
		t.Fatalf("proposal list must contain exactly one record for the raced id, got %d:\n%s", len(listed), lo)
	}
	var listedID string
	if err := json.Unmarshal(listed[0]["id"], &listedID); err != nil || listedID != id {
		t.Fatalf("listed proposal id=%q err=%v, want %q", listedID, err, id)
	}
	_, listedHasSource := listed[0]["source"]

	switch winnerSource {
	case "register":
		// 成功返回的内容必须与实际保存的记录对应：passed、时间锁与有序动作。
		if winnerRegister.ID != id || winnerRegister.State != "passed" || winnerRegister.TimelockEnd != 300 ||
			!reflect.DeepEqual(winnerRegister.Actions, crossSourceActions) {
			t.Fatalf("winning register response = %+v, want passed timelock=300 actions=%v",
				winnerRegister, crossSourceActions)
		}
		var saved registerCLIResponse
		if err := json.Unmarshal([]byte(so), &saved); err != nil {
			t.Fatalf("proposal query output is not a registered record: %v\n%s", err, so)
		}
		if saved.ID != winnerRegister.ID || saved.State != winnerRegister.State ||
			saved.TimelockEnd != winnerRegister.TimelockEnd ||
			!reflect.DeepEqual(saved.Actions, winnerRegister.Actions) {
			t.Fatalf("single query %+v does not match the winning register response %+v", saved, winnerRegister)
		}
		if strings.Contains(so, `"source"`) || strings.Contains(so, `"members"`) {
			t.Fatalf("registered winner must not show voting-source fields:\n%s", so)
		}
		if listedHasSource {
			t.Fatalf("listed record must be the registered passed proposal (no source field):\n%s", lo)
		}
	case "vote":
		// 投票方胜出：保留原始成员权重、委托关系、投票窗口与有序动作；
		// 单项查询与成功返回的是同一份提案。
		assertSavedConcProposal(t, winnerVote, id, crossSourceActions, concMemberIDs(concCreateRoster))
		var saved VoteProposalView
		if err := json.Unmarshal([]byte(so), &saved); err != nil {
			t.Fatalf("proposal query output is not a voting view: %v\n%s", err, so)
		}
		if !reflect.DeepEqual(&saved, winnerVote) {
			t.Fatalf("single query does not match the winning create-vote response:\n%+v\nvs\n%+v", &saved, winnerVote)
		}
		assertSavedConcProposal(t, &saved, id, crossSourceActions, concMemberIDs(concCreateRoster))
		if !listedHasSource {
			t.Fatalf("listed record must be the voting proposal (source=vote):\n%s", lo)
		}
		var listedSource string
		if err := json.Unmarshal(listed[0]["source"], &listedSource); err != nil || listedSource != "vote" {
			t.Fatalf("listed source=%q err=%v, want vote", listedSource, err)
		}
	}

	// 整个创建竞争不改变任何余额，也不产生执行凭据。
	assertNoTreasuryChange(t, binary, state, 1000)
}

// TestCLIRejectedVoteKeepsID：已有投票提案到期计票并被拒绝后，编号仍归
// 原提案所有——再以同编号登记已通过提案，即使提交相同时间锁和动作原文，
// 也必须继续报编号冲突（退出码 1、原因只在标准错误、标准输出无成功记录）。
// 查询仍保留原来的 rejected 状态、成员及委托明细、实际票据与首次计票结论，
// 不能把拒绝结论变成通过；原提案仍不能执行；同库另一项完整提案的内容不受
// 这次冲突影响；余额与凭据不变。
func TestCLIRejectedVoteKeepsID(t *testing.T) {
	binary := buildCLI(t)
	state := filepath.Join(t.TempDir(), "treasury.json")
	if _, _, code := runCLI(t, binary, state, "init", "--balance", "1000"); code != 0 {
		t.Fatal("init failed")
	}

	const id = "gip-rejected"
	// alice(300) + bob(200 委托给 alice) 归集 500 在 alice 名下；carol(100) 自代表。
	// quorum=500；alice 投反对 => for=0 against=500，参与达标但赞成不多于反对 => rejected。
	create := []string{"create-vote", "--id", id,
		"--member", "alice:300", "--member", "bob:200", "--member", "carol:100",
		"--delegate", "bob:alice",
		"--quorum", "500", "--start", "100", "--deadline", "200", "--timelock", "300",
		"--action", "transfer:audits:100"}
	if _, se, code := runCLI(t, binary, state, create...); code != 0 {
		t.Fatalf("create-vote failed: %s", se)
	}
	// 同库另一项完整提案（登记来源），用于验证编号冲突不影响它的内容。
	if _, se, code := runCLI(t, binary, state, "register", "--id", "gip-keeper",
		"--timelock", "50", "--action", "transfer:legal:5"); code != 0 {
		t.Fatalf("register gip-keeper failed: %s", se)
	}
	if _, se, code := runCLI(t, binary, state, "vote", "--id", id,
		"--voter", "alice", "--choice", "against", "--now", "150"); code != 0 {
		t.Fatalf("vote failed: %s", se)
	}
	if so, se, code := runCLI(t, binary, state, "tally", "--id", id, "--now", "200"); code != 0 ||
		!strings.Contains(so, "=> rejected") {
		t.Fatalf("tally exit=%d so=%s se=%s, want rejected", code, so, se)
	}

	// 同编号登记已通过提案：相同时间锁（300）与相同动作原文，仍必须冲突。
	so, se, code := runCLI(t, binary, state,
		crossSourceRegisterArgs(id, 300, []string{"transfer:audits:100"})...)
	assertConflictCLI(t, crossSourceResult{source: "register", code: code, stdout: so, stderr: se}, id)
	if !strings.Contains(se, "belongs to a voting proposal") || !strings.Contains(se, "rejected") {
		t.Fatalf("conflict stderr must name the existing voting proposal and its rejected state, got %q", se)
	}

	// 单项查询：仍是投票来源的 rejected 提案，成员及委托明细、实际票据与
	// 首次计票结论全部保留，拒绝结论没有变成通过。
	po, pe, code := runCLI(t, binary, state, "proposal", "--id", id, "--json")
	if code != 0 {
		t.Fatalf("proposal query failed: %s", pe)
	}
	var saved VoteProposalView
	if err := json.Unmarshal([]byte(po), &saved); err != nil {
		t.Fatalf("proposal query output is not JSON: %v\n%s", err, po)
	}
	if saved.ID != id || saved.Source != "vote" || saved.State != "rejected" {
		t.Fatalf("saved proposal id=%q source=%q state=%q, want %q/vote/rejected",
			saved.ID, saved.Source, saved.State, id)
	}
	if saved.Quorum != 500 || saved.StartAt != 100 || saved.Deadline != 200 || saved.TimelockEnd != 300 {
		t.Fatalf("governance rules changed: quorum=%d window=[%d,%d) timelock=%d",
			saved.Quorum, saved.StartAt, saved.Deadline, saved.TimelockEnd)
	}
	if !reflect.DeepEqual(saved.Actions, []string{"transfer:audits:100"}) {
		t.Fatalf("actions=%v, want the original ordered actions", saved.Actions)
	}
	if len(saved.Members) != 3 {
		t.Fatalf("member count=%d, want 3", len(saved.Members))
	}
	for _, m := range saved.Members {
		switch m.ID {
		case "alice":
			if m.Weight != 300 || m.Delegate != "alice" || m.Direct != "" {
				t.Fatalf("alice view = %+v, want own 300 as final representative", m)
			}
		case "bob":
			if m.Weight != 200 || m.Delegate != "alice" || m.Direct != "alice" ||
				!reflect.DeepEqual(m.Path, []string{"bob", "alice"}) {
				t.Fatalf("bob view = %+v, want delegation to alice with full path", m)
			}
		case "carol":
			if m.Weight != 100 || m.Delegate != "carol" || m.Direct != "" {
				t.Fatalf("carol view = %+v, want own 100 as final representative", m)
			}
		default:
			t.Fatalf("unexpected member %q", m.ID)
		}
	}
	if len(saved.Ballots) != 1 || saved.Ballots[0].Representative != "alice" ||
		saved.Ballots[0].Weight != 500 || saved.Ballots[0].Support || saved.Ballots[0].VotedAt != 150 {
		t.Fatalf("actual ballot record changed: %+v", saved.Ballots)
	}
	if saved.Tally == nil || saved.Tally.ForWeight != 0 || saved.Tally.AgainstWeight != 500 ||
		saved.Tally.Turnout != 500 || saved.Tally.Quorum != 500 || saved.Tally.Passed ||
		saved.Tally.TalliedAt != 200 {
		t.Fatalf("first tally conclusion changed: %+v", saved.Tally)
	}

	// 再次计票仍返回首次拒绝结论；被拒绝的提案不能执行。
	if to, te, code := runCLI(t, binary, state, "tally", "--id", id, "--now", "999", "--json"); code != 0 ||
		!strings.Contains(to, `"passed": false`) || !strings.Contains(to, `"tallied_at": 200`) {
		t.Fatalf("repeat tally must keep the first rejection: exit=%d so=%s se=%s", code, to, te)
	}
	if _, ee, code := runCLI(t, binary, state, "execute", "--id", id, "--now", "500"); code != 1 ||
		!strings.Contains(ee, "not passed") {
		t.Fatalf("rejected proposal must not execute: exit=%d stderr=%s", code, ee)
	}

	// 全部提案查询与单项查询给出同一个结果：该编号仍是投票来源的 rejected，
	// 另一项完整提案的内容不受这次编号冲突影响。
	lo, le, code := runCLI(t, binary, state, "proposals", "--json")
	if code != 0 {
		t.Fatalf("proposals query failed: %s", le)
	}
	var listed []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(lo), &listed); err != nil {
		t.Fatalf("proposals output is not JSON: %v\n%s", err, lo)
	}
	if len(listed) != 2 {
		t.Fatalf("proposal list must contain exactly the rejected voting proposal and gip-keeper, got %d:\n%s",
			len(listed), lo)
	}
	byID := map[string]map[string]json.RawMessage{}
	for _, entry := range listed {
		var entryID string
		if err := json.Unmarshal(entry["id"], &entryID); err != nil {
			t.Fatalf("listed entry has no id: %v", entry)
		}
		byID[entryID] = entry
	}
	rejectedEntry, ok := byID[id]
	if !ok {
		t.Fatalf("rejected proposal missing from list:\n%s", lo)
	}
	var entryState, entrySource string
	if err := json.Unmarshal(rejectedEntry["state"], &entryState); err != nil || entryState != "rejected" {
		t.Fatalf("listed state=%q err=%v, want rejected", entryState, err)
	}
	if err := json.Unmarshal(rejectedEntry["source"], &entrySource); err != nil || entrySource != "vote" {
		t.Fatalf("listed source=%q err=%v, want vote", entrySource, err)
	}
	keeper, ok := byID["gip-keeper"]
	if !ok {
		t.Fatalf("gip-keeper missing from list:\n%s", lo)
	}
	var keeperRec ProposalRecord
	keeperJSON, err := json.Marshal(keeper)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(keeperJSON, &keeperRec); err != nil {
		t.Fatalf("gip-keeper entry is not a registered record: %v", err)
	}
	if keeperRec.State != "passed" || keeperRec.TimelockEnd != 50 ||
		!reflect.DeepEqual(keeperRec.Actions, []string{"transfer:legal:5"}) {
		t.Fatalf("unrelated proposal changed by the id conflict: %+v", keeperRec)
	}

	// 冲突、计票与执行尝试都不改变任何余额，也不产生执行凭据。
	assertNoTreasuryChange(t, binary, state, 1000)
}
