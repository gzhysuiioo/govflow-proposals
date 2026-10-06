package govflow

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// 本文件为“多个独立命令进程同时向同一提案的同一位最终代表提交赞成与反对”
// 补充回归保障，保护既有规则：首次保存的选择不能被覆盖，归集到代表名下的
// 委托权重只能记入一张票。无论哪个请求先完成，结果都必须与按某种先后顺序
// 串行完成一致——不能两种选择各记一张票，也不能由后到者改写首次记录。
// 串行化由进程内互斥锁与 flock 共同保证，因此这些用例直接启动多个 CLI
// 进程压跨进程路径，全部在本机离线完成，沿用现有 vote/tally/proposal 命令
// 及其输出，不改变任何投票规则。

// voteConcRequest 描述一次并发投票请求：选择与支持该选择的提交时间。
type voteConcRequest struct {
	choice string // "for" / "against"
	now    int64  // 窗口 [100,200) 内、各请求互不相同的时间
}

// voteConcResult 记录一个并发投票进程的全部可观察结果。
type voteConcResult struct {
	req    voteConcRequest
	code   int
	stdout string
	stderr string
	ballot *BallotView // 成功时从 --json 输出解析出的票据
}

// setupConcVoteProposal 创建一项已存在、尚未投票的提案：
// alice/bob/carol 原始权重 300/200/100，bob -> carol -> alice 两次转交后
// 只有 alice 是最终代表，归集权重 600；法定人数 600，窗口 [100,200)，
// 时间锁 300。
func setupConcVoteProposal(t *testing.T, binary, state, id string) {
	t.Helper()
	if _, se, code := runCLI(t, binary, state, "create-vote", "--id", id,
		"--member", "alice:300", "--member", "bob:200", "--member", "carol:100",
		"--delegate", "bob:carol", "--delegate", "carol:alice",
		"--quorum", "600", "--start", "100", "--deadline", "200", "--timelock", "300",
		"--action", "transfer:audits:100"); code != 0 {
		t.Fatalf("create-vote failed: %s", se)
	}
}

// runConcurrentVotes 用同一个启动栅栏同时放出 len(requests) 个独立投票进程，
// 全部针对同一状态文件、同一提案、同一最终代表 alice。
func runConcurrentVotes(t *testing.T, binary, state, id string, requests []voteConcRequest) []voteConcResult {
	t.Helper()
	results := make([]voteConcResult, len(requests))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range requests {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := requests[i]
			<-start
			so, se, code := runCLI(t, binary, state, "vote", "--id", id,
				"--voter", "alice", "--choice", req.choice,
				"--now", strconv.FormatInt(req.now, 10), "--json")
			results[i] = voteConcResult{req: req, code: code, stdout: so, stderr: se}
		}(i)
	}
	close(start)
	wg.Wait()
	return results
}

// queryConcProposal 查询提案的完整 JSON 视图。
func queryConcProposal(t *testing.T, binary, state, id string) *VoteProposalView {
	t.Helper()
	so, se, code := runCLI(t, binary, state, "proposal", "--id", id, "--json")
	if code != 0 {
		t.Fatalf("proposal query failed: %s", se)
	}
	var view VoteProposalView
	if err := json.Unmarshal([]byte(so), &view); err != nil {
		t.Fatalf("proposal query output is not JSON: %v\n%s", err, so)
	}
	return &view
}

// assertConcRoster 核对提案的名单与委托派生：只有 alice 是最终代表，
// 归集权重 600；bob、carol 的完整委托路径（含中间成员）不得丢失。
func assertConcRoster(t *testing.T, v *VoteProposalView, id string) {
	t.Helper()
	if v.ID != id || v.Source != "vote" {
		t.Fatalf("proposal id=%q source=%q, want %q/vote", v.ID, v.Source, id)
	}
	if v.Quorum != 600 || v.StartAt != 100 || v.Deadline != 200 || v.TimelockEnd != 300 {
		t.Fatalf("governance rules changed: quorum=%d window=[%d,%d) timelock=%d",
			v.Quorum, v.StartAt, v.Deadline, v.TimelockEnd)
	}
	if v.TotalWeight != 600 {
		t.Fatalf("total_weight=%d, want 600", v.TotalWeight)
	}
	type memberWant struct {
		weight   int64
		delegate string
		direct   string
	}
	wants := map[string]memberWant{
		"alice": {weight: 300, delegate: "alice"},
		"bob":   {weight: 200, delegate: "alice", direct: "carol"},
		"carol": {weight: 100, delegate: "alice", direct: "alice"},
	}
	if len(v.Members) != len(wants) {
		t.Fatalf("member count=%d, want %d", len(v.Members), len(wants))
	}
	for _, m := range v.Members {
		want, ok := wants[m.ID]
		if !ok {
			t.Fatalf("unexpected member %q", m.ID)
		}
		if m.Weight != want.weight || m.Delegate != want.delegate || m.Direct != want.direct {
			t.Fatalf("member %s weight=%d delegate=%q direct_to=%q, want %d/%q/%q",
				m.ID, m.Weight, m.Delegate, m.Direct, want.weight, want.delegate, want.direct)
		}
		if m.Path[len(m.Path)-1] != "alice" {
			t.Fatalf("member %s path=%v must end at final representative alice", m.ID, m.Path)
		}
	}
}

// assertSingleConcBallot 核对查询中恰好有 alice 的一张票，且与首次成功
// 记录完全一致：不能两种选择各一张，也不能把原始权重与委托权重分别记票。
func assertSingleConcBallot(t *testing.T, v *VoteProposalView, first *BallotView) {
	t.Helper()
	if len(v.Ballots) != 1 {
		t.Fatalf("proposal must hold exactly one ballot, got %d: %+v", len(v.Ballots), v.Ballots)
	}
	got := v.Ballots[0]
	if got != *first {
		t.Fatalf("saved ballot %+v does not match the first successful record %+v", got, *first)
	}
	if got.Representative != "alice" || got.Weight != 600 {
		t.Fatalf("ballot representative=%q weight=%d, want alice carrying the aggregated 600",
			got.Representative, got.Weight)
	}
}

// TestCrossProcessConcurrentVoteChoiceConflict：多个进程同时向同一提案的
// 最终代表 alice 提交赞成与反对两种选择，各次提交使用不同但都在窗口内的
// 时间。赞成或反对都可能成为首次保存的选择（由实际提交先后决定，不预设
// 哪边胜出，也不把最小时间当成首次时间）：与保存选择相同的请求全部成功，
// 返回与已保存票据完全一致的代表、归集权重、选择与首次时间；另一选择的
// 请求全部以改投冲突失败（退出码 1、原因写 stderr、stdout 为空）。结束后
// 查询只能看到 alice 的一张 600 权重票；在 200 计票时参与权重为 600，
// 首次选择为赞成则通过、为反对则拒绝，失败的相反选择不进入汇总。
func TestCrossProcessConcurrentVoteChoiceConflict(t *testing.T) {
	binary := buildCLI(t)
	state := filepath.Join(t.TempDir(), "treasury.json")
	if _, _, code := runCLI(t, binary, state, "init", "--balance", "1000"); code != 0 {
		t.Fatal("init failed")
	}
	const id = "gip-conc-vote"
	setupConcVoteProposal(t, binary, state, id)

	// 两种选择各 8 个进程交错放出；每次提交的时间互不相同且都在 [100,200) 内。
	const perChoice = 8
	const n = perChoice * 2
	requests := make([]voteConcRequest, n)
	for i := 0; i < n; i++ {
		choice := "against"
		if i%2 == 0 {
			choice = "for"
		}
		requests[i] = voteConcRequest{choice: choice, now: int64(110 + i)}
	}
	results := runConcurrentVotes(t, binary, state, id, requests)

	// 从成功响应中确定首次保存的选择与首次记录：哪个请求先完成不定，
	// 但成功响应的选择必须全部一致——两种选择不能都被记下。
	var winnerChoice string
	var firstBallot *BallotView
	for i := range results {
		r := &results[i]
		if r.code != 0 {
			continue // 冲突结果稍后逐类核对
		}
		b := &BallotView{}
		if err := json.Unmarshal([]byte(r.stdout), b); err != nil {
			t.Fatalf("process %d success output is not JSON: %v\n%s", i, err, r.stdout)
		}
		r.ballot = b
		choice := "against"
		if b.Support {
			choice = "for"
		}
		if firstBallot == nil {
			winnerChoice, firstBallot = choice, b
			continue
		}
		if choice != winnerChoice {
			t.Fatalf("process %d succeeded with choice %s after %s was already saved: both choices must not win",
				i, choice, winnerChoice)
		}
		if *b != *firstBallot {
			t.Fatalf("process %d returned ballot %+v, want the identical first record %+v",
				i, *b, *firstBallot)
		}
	}
	if firstBallot == nil {
		t.Fatal("no vote succeeded: exactly one choice must win the first save")
	}

	// 首次记录必须是 alice 名下归集的全部 600 权重，首次时间在窗口内，
	// 且就是某个胜出方请求实际提交的时间——不是推算出的最小时间。
	if firstBallot.Representative != "alice" || firstBallot.Weight != 600 {
		t.Fatalf("first ballot representative=%q weight=%d, want alice with aggregated 600",
			firstBallot.Representative, firstBallot.Weight)
	}
	if firstBallot.VotedAt < 100 || firstBallot.VotedAt >= 200 {
		t.Fatalf("first voted_at=%d outside window [100,200)", firstBallot.VotedAt)
	}
	submitted := false
	for _, req := range requests {
		if req.choice == winnerChoice && req.now == firstBallot.VotedAt {
			submitted = true
		}
	}
	if !submitted {
		t.Fatalf("first voted_at=%d is not any submitted %s time; it must come from the winning request",
			firstBallot.VotedAt, winnerChoice)
	}

	// 逐请求核对：同选择全部成功且返回同一份首次记录（即使自己提交的时间
	// 更早或更晚也不更新它）；相反选择全部以改投冲突失败。
	successes, conflicts, retimed := 0, 0, 0
	for i := range results {
		r := &results[i]
		if r.req.choice == winnerChoice {
			if r.code != 0 {
				t.Fatalf("process %d shares the saved choice %s and must succeed: exit=%d stderr=%s",
					i, winnerChoice, r.code, r.stderr)
			}
			if r.ballot == nil || *r.ballot != *firstBallot {
				t.Fatalf("process %d must return the identical first record %+v, got %+v",
					i, *firstBallot, r.ballot)
			}
			if r.req.now != firstBallot.VotedAt {
				retimed++ // 提交时间不同仍只返回首次记录
			}
			successes++
			continue
		}
		if r.code != 1 {
			t.Fatalf("conflicting process %d exit=%d, want 1 (stdout=%q stderr=%q)",
				i, r.code, r.stdout, r.stderr)
		}
		if strings.TrimSpace(r.stdout) != "" {
			t.Fatalf("conflict must not print a success result on stdout, got %q", r.stdout)
		}
		for _, want := range []string{"changing the vote is not allowed", "alice", winnerChoice, id} {
			if !strings.Contains(r.stderr, want) {
				t.Fatalf("conflict stderr %q missing %q (recorded choice must be named)", r.stderr, want)
			}
		}
		conflicts++
	}
	if successes != perChoice {
		t.Fatalf("successes=%d, want all %d same-choice requests", successes, perChoice)
	}
	if conflicts != perChoice {
		t.Fatalf("conflicts=%d, want all %d opposite-choice requests", conflicts, perChoice)
	}
	// 各请求时间互不相同，首次时间只属于一个请求：其余同选择成功请求必然
	// 提交了不同时间却仍拿到首次记录。
	if retimed == 0 {
		t.Fatal("no same-choice request submitted a different time; cannot prove the first record is never updated")
	}

	// 全部提交结束后：查询只能看到 alice 的一张票，与成功响应完全一致；
	// 提案仍在投票、无计票结论，名单与委托派生不变。
	view := queryConcProposal(t, binary, state, id)
	if view.State != "voting" || view.Tally != nil {
		t.Fatalf("proposal state=%q tally=%+v, want voting without tally", view.State, view.Tally)
	}
	assertConcRoster(t, view, id)
	assertSingleConcBallot(t, view, firstBallot)

	// 在 200 计票：参与权重恒为 600；首次选择为赞成则 600/0 通过，
	// 为反对则 0/600 拒绝。失败的相反选择不得进入汇总。
	to, te, code := runCLI(t, binary, state, "tally", "--id", id, "--now", "200", "--json")
	if code != 0 {
		t.Fatalf("tally failed: %s", te)
	}
	var tallyResp struct {
		Result *TallyResultView `json:"result"`
	}
	if err := json.Unmarshal([]byte(to), &tallyResp); err != nil || tallyResp.Result == nil {
		t.Fatalf("tally output is not JSON: %v\n%s", err, to)
	}
	tr := tallyResp.Result
	wantFor, wantAgainst, wantPassed := int64(0), int64(600), false
	if winnerChoice == "for" {
		wantFor, wantAgainst, wantPassed = 600, 0, true
	}
	if tr.ForWeight != wantFor || tr.AgainstWeight != wantAgainst {
		t.Fatalf("tally for=%d against=%d, want %d/%d (losing choice must not enter the summary)",
			tr.ForWeight, tr.AgainstWeight, wantFor, wantAgainst)
	}
	if tr.Turnout != 600 || tr.Quorum != 600 {
		t.Fatalf("tally turnout=%d quorum=%d, want 600/600", tr.Turnout, tr.Quorum)
	}
	if tr.Passed != wantPassed {
		t.Fatalf("tally passed=%v, want %v for winning choice %s", tr.Passed, wantPassed, winnerChoice)
	}
	if tr.TalliedAt != 200 {
		t.Fatalf("tallied_at=%d, want 200", tr.TalliedAt)
	}

	// 计票后再查询：状态与结论落定，保存的票据与首次时间不被计票改变。
	final := queryConcProposal(t, binary, state, id)
	wantState := "rejected"
	if wantPassed {
		wantState = "passed"
	}
	if final.State != wantState {
		t.Fatalf("final state=%q, want %q", final.State, wantState)
	}
	assertSingleConcBallot(t, final, firstBallot)
	if final.Tally == nil || final.Tally.ForWeight != wantFor || final.Tally.AgainstWeight != wantAgainst ||
		final.Tally.Turnout != 600 || final.Tally.Passed != wantPassed || final.Tally.TalliedAt != 200 {
		t.Fatalf("final tally view %+v does not match the first tally result", final.Tally)
	}
}
