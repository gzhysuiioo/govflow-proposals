package govflow

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// 本文件为“同一位最终代表在多个独立命令进程中同时提交赞成和反对”补充
// 回归保障，保护两条既有规则：
//  1. 首次保存的选择不能被后来的相反选择覆盖（改投冲突，绝不静默覆盖）；
//  2. 委托归集的权重只能记入一张票——bob(200) 委托给 carol，carol 再委托
//     给 alice，因此只有 alice 是最终代表，归集权重 300+200+100=600。
//
// 串行化由进程内互斥锁与 flock 文件锁共同保证，因此这里直接启动多个 CLI
// 进程压跨进程路径（与 proc_test.go 中的执行/创建并发用例做法一致）。
//
// 场景固定：一项已经创建、尚未投票的提案；成员 alice/bob/carol 原始权重
// 300/200/100；委托 bob -> carol -> alice；quorum=600；投票窗口 [100,200)；
// timelock=300。多个进程对同一状态文件、同一提案、同一位代表 alice 提交
// 两种选择，每种选择各多个请求；全部提交使用互不相同且都落在窗口内的时间。
//
// 赞成或反对都有可能成为首次成功选择，因此断言绝不预设哪一边胜出，也不把
// 传入的最小时间当成首次投票时间——首次记录只以实际首次成功进程返回的
// 票据为准。并发竞争使用窗口中段的时间（120..165），竞争结束后再用窗口内
// 更早（100/101）与更晚（190/199）的时间确定性补压：成功请求即使传入更早
// 或更晚的窗口内时间，也只能返回首次记录，不能更新它。

// voteRaceProposalID 是并发投票用的提案编号。
const voteRaceProposalID = "gip-conc-vote-race"

// voteRaceWindowTimes 是并发竞争阶段使用的时间池：互不相同且都在窗口
// [100,200) 中段。两种选择按下标交错取时间（for 取偶数位、against 取
// 奇数位），使两排进程的时间与启动顺序都不系统性偏向任何一种选择。
// 池的两端留出 100/101 与 190/199，供竞争结束后的确定性早/晚时间补压。
var voteRaceWindowTimes = []int64{120, 125, 130, 135, 140, 145, 150, 155, 160, 165}

// 竞争结束后的补压时间：两种选择各两个，全部互不相同、也不与竞争池重复；
// 100/101 严格早于任何可能的首次时间，190/199 严格晚于任何可能的首次时间。
var voteRaceEarlierTimes = map[string]int64{"for": 100, "against": 101}
var voteRaceLaterTimes = map[string]int64{"for": 190, "against": 199}

// voteRaceResponse 是 vote --json 成功时 stdout 上的票据对象。四个字段
// 与已保存票据逐字对应：代表、归集票重、选择、首次投票时间。
type voteRaceResponse struct {
	Representative string `json:"representative"`
	Weight         int64  `json:"weight"`
	Support        bool   `json:"support"`
	VotedAt        int64  `json:"voted_at"`
}

// voteRaceResult 记录一个并发投票进程的全部可观察结果。
type voteRaceResult struct {
	choice string // "for" / "against"
	now    int64
	code   int
	stdout string
	stderr string
	resp   voteRaceResponse
}

// voteRaceSetup 创建状态文件、初始化资金库并创建好那项尚未投票的提案。
// 名单、委托与治理规则在全部断言中固定。
func voteRaceSetup(t *testing.T, binary, state string) {
	t.Helper()
	if _, se, code := runCLI(t, binary, state, "init", "--balance", "1000"); code != 0 {
		t.Fatalf("init failed: %s", se)
	}
	args := []string{"create-vote", "--id", voteRaceProposalID,
		"--member", "alice:300", "--member", "bob:200", "--member", "carol:100",
		"--delegate", "bob:carol", "--delegate", "carol:alice",
		"--quorum", "600", "--start", "100", "--deadline", "200", "--timelock", "300",
		"--action", "transfer:audits:100"}
	if _, se, code := runCLI(t, binary, state, args...); code != 0 {
		t.Fatalf("create-vote failed: %s", se)
	}
}

// assertVoteRaceFreshProposal 核对 setup 产出的提案确为任务要求的那一项：
// 仍在投票、票据为空、只有 alice 是最终代表、alice 名下归集权重恰好 600。
func assertVoteRaceFreshProposal(t *testing.T, binary, state string) {
	t.Helper()
	so, se, code := runCLI(t, binary, state, "proposal", "--id", voteRaceProposalID, "--json")
	if code != 0 {
		t.Fatalf("proposal query failed: %s", se)
	}
	var p VoteProposalView
	if err := json.Unmarshal([]byte(so), &p); err != nil {
		t.Fatalf("proposal query is not JSON: %v\n%s", err, so)
	}
	if p.State != "voting" || len(p.Ballots) != 0 || p.Tally != nil {
		t.Fatalf("fresh proposal must be voting with no ballot/tally, got state=%s ballots=%d tally=%v",
			p.State, len(p.Ballots), p.Tally)
	}
	if p.Quorum != 600 || p.StartAt != 100 || p.Deadline != 200 || p.TimelockEnd != 300 || p.TotalWeight != 600 {
		t.Fatalf("governance params differ: quorum=%d window=[%d,%d) timelock=%d total=%d",
			p.Quorum, p.StartAt, p.Deadline, p.TimelockEnd, p.TotalWeight)
	}
	// 从成员视图重算最终代表分组与委托路径，不依赖产品自报归集权重。
	groups := map[string]int64{}
	for _, m := range p.Members {
		groups[m.Delegate] += m.Weight
		switch m.ID {
		case "alice":
			if m.Weight != 300 || m.Delegate != "alice" {
				t.Fatalf("alice must keep her own 300 as final representative, got weight=%d delegate=%q", m.Weight, m.Delegate)
			}
		case "bob":
			if m.Weight != 200 || m.Delegate != "alice" {
				t.Fatalf("bob(200) must roll up to alice, got delegate=%q", m.Delegate)
			}
			wantPath := []string{"bob", "carol", "alice"}
			if !reflect.DeepEqual(m.Path, wantPath) {
				t.Fatalf("bob delegation path=%v, want %v (intermediate carol must not be lost)", m.Path, wantPath)
			}
		case "carol":
			if m.Weight != 100 || m.Delegate != "alice" {
				t.Fatalf("carol(100) must roll up to alice, got delegate=%q", m.Delegate)
			}
		default:
			t.Fatalf("unexpected member %q", m.ID)
		}
	}
	if len(groups) != 1 || groups["alice"] != 600 {
		t.Fatalf("alice must be the only final representative carrying exactly 600, got %v", groups)
	}
}

// runVoteRace 用同一个启动栅栏同时放出 2*perChoice 个独立进程：赞成、反对
// 各 perChoice 个，时间互不相同且都在窗口中段，两种选择的时间交错排列，
// 避免任何一种选择在调度上系统性占先。
func runVoteRace(t *testing.T, binary, state string, perChoice int) []voteRaceResult {
	t.Helper()
	n := perChoice * 2
	if n > len(voteRaceWindowTimes) {
		t.Fatalf("test harness: %d requests exceed %d distinct in-window times", n, len(voteRaceWindowTimes))
	}
	results := make([]voteRaceResult, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		choice := "for"
		if i%2 == 1 {
			choice = "against"
		}
		now := voteRaceWindowTimes[i]
		wg.Add(1)
		go func(i int, choice string, now int64) {
			defer wg.Done()
			<-start
			so, se, code := runCLI(t, binary, state, "vote",
				"--id", voteRaceProposalID, "--voter", "alice",
				"--choice", choice, "--now", strconv.FormatInt(now, 10), "--json")
			results[i] = voteRaceResult{choice: choice, now: now, code: code, stdout: so, stderr: se}
		}(i, choice, now)
	}
	// 交错（for@120, against@125, for@130, ...）保证时间在两选择间交替；
	// 哪一边首次成功完全由文件锁竞争决定。
	close(start)
	wg.Wait()
	return results
}

// assertVoteRaceOutcome 在不知道哪边会赢的前提下，核对全部并发结果：
//   - 恰好一边选择能成功：该边全部请求成功，且彼此返回与已保存票据完全
//     一致的代表、归集权重、选择和首次时间；
//   - 另一边全部以现有的改投冲突失败：退出码 1、stdout 为空、stderr 明确
//     说已记录的选择不能改投，并点出提案编号；
//   - 首次时间必须是某个实际请求传入的窗口内时间，但绝不预设为最小时间。
func assertVoteRaceOutcome(t *testing.T, results []voteRaceResult) voteRaceResponse {
	t.Helper()
	if len(results) < 4 {
		t.Fatalf("test harness: each choice needs multiple requests, got %d", len(results))
	}
	total := map[string]int{}
	for _, r := range results {
		total[r.choice]++
	}
	if total["for"] < 2 || total["against"] < 2 {
		t.Fatalf("each choice must have multiple requests, got %v", total)
	}

	var saved voteRaceResponse
	savedChoice := ""
	successes, conflicts := map[string]int{}, map[string]int{}
	for i := range results {
		r := &results[i]
		if r.code == 0 {
			if err := json.Unmarshal([]byte(r.stdout), &r.resp); err != nil {
				t.Fatalf("process %d success stdout is not JSON: %v\n%s", i, err, r.stdout)
			}
			successes[r.choice]++
			if savedChoice == "" {
				savedChoice = r.choice
				saved = r.resp
			}
			continue
		}
		conflicts[r.choice]++
	}
	if savedChoice == "" {
		t.Fatal("no vote succeeded: one choice must be saved first")
	}
	if successes["for"] != 0 && successes["against"] != 0 {
		t.Fatalf("both choices report success (for=%d against=%d): first choice was overwritten",
			successes["for"], successes["against"])
	}
	loser := "against"
	if savedChoice == "against" {
		loser = "for"
	}
	if successes[savedChoice] != total[savedChoice] {
		t.Fatalf("all %d %s requests must succeed, got %d successes and %d failures",
			total[savedChoice], savedChoice, successes[savedChoice], conflicts[savedChoice])
	}
	if conflicts[loser] != total[loser] || successes[loser] != 0 {
		t.Fatalf("all %d %s requests must conflict, got %d conflicts and %d successes",
			total[loser], loser, conflicts[loser], successes[loser])
	}

	// 票据四字段的固定部分：代表只能是 alice，归集权重恰好 600，只记一张票。
	if saved.Representative != "alice" || saved.Weight != 600 {
		t.Fatalf("saved ticket representative=%q weight=%d, want alice/600", saved.Representative, saved.Weight)
	}
	if (savedChoice == "for") != saved.Support {
		t.Fatalf("saved support=%v but first-success choice=%s", saved.Support, savedChoice)
	}
	// 首次时间必须是实际胜出请求传入的时间，不能被预设成任何固定值。
	found := false
	for _, tm := range voteRaceWindowTimes {
		if tm == saved.VotedAt {
			found = true
		}
	}
	if !found || saved.VotedAt < 100 || saved.VotedAt >= 200 {
		t.Fatalf("first voted_at=%d must be the actual in-window time of the winning request", saved.VotedAt)
	}

	for i := range results {
		r := &results[i]
		if r.choice == savedChoice {
			if r.code != 0 {
				t.Fatalf("%s request at now=%d must succeed like the saved choice: exit=%d stderr=%s",
					r.choice, r.now, r.code, r.stderr)
			}
			if !reflect.DeepEqual(r.resp, saved) {
				t.Fatalf("success at now=%d returned %+v, must equal saved first ticket %+v (first record must not move)",
					r.now, r.resp, saved)
			}
		} else {
			if r.code != 1 {
				t.Fatalf("%s request at now=%d exit=%d, want 1 (stdout=%q stderr=%q)",
					r.choice, r.now, r.code, r.stdout, r.stderr)
			}
			if strings.TrimSpace(r.stdout) != "" {
				t.Fatalf("%s conflict at now=%d must leave stdout empty, got %q", r.choice, r.now, r.stdout)
			}
			// 现有错误文案：... already voted <for|against> for <id>; changing the vote is not allowed
			for _, want := range []string{"already voted", savedChoice, voteRaceProposalID, "changing the vote is not allowed"} {
				if !strings.Contains(r.stderr, want) {
					t.Fatalf("%s conflict at now=%d stderr %q must contain %q", r.choice, r.now, r.stderr, want)
				}
			}
		}
	}
	return saved
}

// assertVoteRaceEdgeTimeRetries 在并发竞争结束后，用窗口内严格更早与更晚的
// 时间确定性补压：保存选择的两个请求仍成功，但返回的必须是首次票据；
// 相反选择的两个请求仍以改投冲突失败。所有四个时间互不相同、也不与竞争
// 阶段重复。失败的相反选择不能改动首次时间。
func assertVoteRaceEdgeTimeRetries(t *testing.T, binary, state string, saved voteRaceResponse) {
	t.Helper()
	savedChoice := "for"
	loser := "against"
	if !saved.Support {
		savedChoice, loser = "against", "for"
	}
	for _, label := range []string{"earlier", "later"} {
		var times map[string]int64
		if label == "earlier" {
			times = voteRaceEarlierTimes
		} else {
			times = voteRaceLaterTimes
		}
		so, se, code := runCLI(t, binary, state, "vote", "--id", voteRaceProposalID,
			"--voter", "alice", "--choice", savedChoice,
			"--now", strconv.FormatInt(times[savedChoice], 10), "--json")
		if code != 0 {
			t.Fatalf("%s %s retry at now=%d must succeed: exit=%d stderr=%s",
				label, savedChoice, times[savedChoice], code, se)
		}
		var got voteRaceResponse
		if err := json.Unmarshal([]byte(so), &got); err != nil {
			t.Fatalf("%s success stdout is not JSON: %v\n%s", label, err, so)
		}
		if !reflect.DeepEqual(got, saved) {
			t.Fatalf("%s retry at now=%d returned %+v, must still return first ticket %+v",
				label, times[savedChoice], got, saved)
		}

		// 相反选择在同一早/晚时间仍冲突：退出码 1、stdout 空、原因不能改投。
		co, ce, ccode := runCLI(t, binary, state, "vote", "--id", voteRaceProposalID,
			"--voter", "alice", "--choice", loser,
			"--now", strconv.FormatInt(times[loser], 10), "--json")
		if ccode != 1 {
			t.Fatalf("%s %s attempt exit=%d, want 1 (stdout=%q stderr=%q)",
				label, loser, ccode, co, ce)
		}
		if strings.TrimSpace(co) != "" {
			t.Fatalf("%s %s conflict must leave stdout empty, got %q", label, loser, co)
		}
		if !strings.Contains(ce, "changing the vote is not allowed") || !strings.Contains(ce, voteRaceProposalID) {
			t.Fatalf("%s %s conflict stderr=%q must name the recorded choice and proposal", label, loser, ce)
		}
	}
}

// assertVoteRaceSingleBallot 查询提案并核对：只能看到 alice 的一张票，权重
// 恰好 600，内容与成功响应完全一致；不存在两种选择各一张、原始权重和委托
// 权重分别记票，或响应成功却没有对应记录。wantState 为该阶段应有的提案
// 状态（计票前 voting，计票后 passed/rejected）。
func assertVoteRaceSingleBallot(t *testing.T, binary, state, wantState string, saved voteRaceResponse) VoteProposalView {
	t.Helper()
	so, se, code := runCLI(t, binary, state, "proposal", "--id", voteRaceProposalID, "--json")
	if code != 0 {
		t.Fatalf("proposal query failed: %s", se)
	}
	var p VoteProposalView
	if err := json.Unmarshal([]byte(so), &p); err != nil {
		t.Fatalf("proposal query is not JSON: %v\n%s", err, so)
	}
	if p.State != wantState {
		t.Fatalf("proposal state=%q, want %q", p.State, wantState)
	}
	if len(p.Ballots) != 1 {
		t.Fatalf("delegated weight must land on exactly one ballot, got %d: %+v", len(p.Ballots), p.Ballots)
	}
	b := p.Ballots[0]
	if b.Representative != saved.Representative || b.Weight != saved.Weight ||
		b.Support != saved.Support || b.VotedAt != saved.VotedAt {
		t.Fatalf("queried ballot %+v must match successful response %+v", b, saved)
	}
	if b.Representative != "alice" || b.Weight != 600 {
		t.Fatalf("queried ballot must be alice carrying exactly 600, got %q/%d", b.Representative, b.Weight)
	}
	if p.TotalWeight != 600 {
		t.Fatalf("total roster weight=%d, want 600", p.TotalWeight)
	}
	return p
}

// assertVoteRaceTally 在截止时刻 200 计票并核对汇总：参与权重始终 600；
// 首次选择为赞成就 for=600/against=0/passed，为反对就 for=0/against=600/
// rejected。失败的相反选择既不能进入汇总，也不能改变保存的首次时间；
// 查询内容必须与成功响应及计票响应一致。
func assertVoteRaceTally(t *testing.T, binary, state string, saved voteRaceResponse) {
	t.Helper()
	so, se, code := runCLI(t, binary, state, "tally", "--id", voteRaceProposalID, "--now", "200", "--json")
	if code != 0 {
		t.Fatalf("tally at deadline failed: %s", se)
	}
	var out struct {
		ID     string          `json:"id"`
		Result TallyResultView `json:"result"`
	}
	if err := json.Unmarshal([]byte(so), &out); err != nil {
		t.Fatalf("tally output is not JSON: %v\n%s", err, so)
	}
	r := out.Result
	if r.Quorum != 600 || r.Turnout != 600 {
		t.Fatalf("quorum=%d turnout=%d, want 600/600", r.Quorum, r.Turnout)
	}
	switch saved.Support {
	case true:
		if r.ForWeight != 600 || r.AgainstWeight != 0 || !r.Passed {
			t.Fatalf("first choice for => for=600 against=0 passed, got for=%d against=%d passed=%v",
				r.ForWeight, r.AgainstWeight, r.Passed)
		}
	case false:
		if r.ForWeight != 0 || r.AgainstWeight != 600 || r.Passed {
			t.Fatalf("first choice against => for=0 against=600 rejected, got for=%d against=%d passed=%v",
				r.ForWeight, r.AgainstWeight, r.Passed)
		}
	}
	if r.TalliedAt != 200 {
		t.Fatalf("first tally time=%d, want 200", r.TalliedAt)
	}

	// 计票后查询：票据仍是同一张、首次时间未被失败的相反选择改动；
	// 状态与首次结论对应，汇总与计票响应逐项一致。
	wantState := "passed"
	if !saved.Support {
		wantState = "rejected"
	}
	p := assertVoteRaceSingleBallot(t, binary, state, wantState, saved)
	if p.Tally == nil {
		t.Fatal("queried proposal missing tally conclusion")
	}
	if p.Tally.ForWeight != r.ForWeight || p.Tally.AgainstWeight != r.AgainstWeight ||
		p.Tally.Turnout != r.Turnout || p.Tally.Quorum != r.Quorum ||
		p.Tally.Passed != r.Passed || p.Tally.TalliedAt != r.TalliedAt {
		t.Fatalf("query tally %+v must match tally response %+v", p.Tally, r)
	}
}

// TestCrossProcessConcurrentVoteFirstChoiceWins：多个独立命令进程同时对同一
// 状态文件、同一提案、同一位最终代表 alice 提交赞成与反对（各多个请求，
// 时间互不相同且都在窗口内）。无论哪一边先抢到文件锁完成首次保存：
//   - 首次选择不能被覆盖：同选择请求全部成功并返回同一张首次票据，相反
//     选择全部以改投冲突失败（退出码 1、stdout 空、stderr 说明不能改投）；
//   - 更早/更晚的窗口内时间不改变首次记录（竞争后确定性补压）；
//   - 委托权重只记一张票：查询只有 alice/600 一张票，与成功响应一致；
//   - 200 时刻首次计票：参与始终 600，结论完全由首次选择决定（赞成通过、
//     反对拒绝），失败的相反选择不进汇总、不改首次时间。
//
// 重复多轮执行，每轮重新建库、重新竞争：若实现存在跨进程竞态，多轮压力下
// 更易暴露。离线运行，只使用现有命令与其既有输出，不改变投票规则。
func TestCrossProcessConcurrentVoteFirstChoiceWins(t *testing.T) {
	binary := buildCLI(t)
	const rounds = 5
	const perChoice = 5 // 每种选择 5 个请求 => 每轮 10 个并发进程，10 个互不相同的时间
	for round := 0; round < rounds; round++ {
		t.Run("round"+strconv.Itoa(round), func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "treasury.json")
			voteRaceSetup(t, binary, state)
			assertVoteRaceFreshProposal(t, binary, state)

			results := runVoteRace(t, binary, state, perChoice)
			saved := assertVoteRaceOutcome(t, results)
			firstChoice := "against"
			if saved.Support {
				firstChoice = "for"
			}
			t.Logf("round %d: first saved choice=%s voted_at=%d", round, firstChoice, saved.VotedAt)

			assertVoteRaceSingleBallot(t, binary, state, "voting", saved)
			assertVoteRaceEdgeTimeRetries(t, binary, state, saved)
			assertVoteRaceSingleBallot(t, binary, state, "voting", saved)
			assertVoteRaceTally(t, binary, state, saved)
		})
	}
}
