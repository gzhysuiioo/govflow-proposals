package govflow

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// 本文件为“多个独立命令进程同时创建同编号投票提案”补充回归保障，保护
// “一个编号只保存一份完整定义”的既有规则：无论哪个请求先完成，结果都必须
// 与按某种先后顺序串行完成一致——不能多个请求都声称首次创建成功，也不能
// 由后提交者覆盖已有定义。串行化由进程内互斥锁与 flock 共同保证，
// 因此这些用例直接启动多个 CLI 进程压跨进程路径。

// createVoteCLIResponse 是 create-vote --json 的成功结果：既有提案对象与
// already_exists 标志都必须保留，冲突不得被包装成一次成功重试。
type createVoteCLIResponse struct {
	Proposal *VoteProposalView `json:"proposal"`
	Existed  bool              `json:"already_exists"`
}

// createConcResult 记录一个并发创建进程的全部可观察结果。
type createConcResult struct {
	group       string // 仅动作次序冲突用例使用："A" / "B"
	code        int
	stdout      string
	stderr      string
	memberOrder []string // 该请求自己提交的成员编号顺序
	resp        *createVoteCLIResponse
}

// concCreateRoster 与 concMemberExpectations 描述两个用例共用的名单：
// alice、bob、carol、dave，原始权重 300/200/100/400，总权重 1000；
// 委托 bob -> carol -> alice 是一条经过两次转交的链，查询时必须展示完整
// 路径（含中间成员 carol）与正确的最终代表，不能因输入排列不同而漏掉 carol。
var concCreateRoster = []string{"alice:300", "bob:200", "carol:100", "dave:400"}

type concMemberExpectation struct {
	weight   int64
	path     []string
	delegate string // 最终代表
	direct   string // 直接委托对象；未委托为空串
}

var concMemberExpectations = map[string]concMemberExpectation{
	"alice": {weight: 300, path: []string{"alice"}, delegate: "alice"},
	"bob":   {weight: 200, path: []string{"bob", "carol", "alice"}, delegate: "alice", direct: "carol"},
	"carol": {weight: 100, path: []string{"carol", "alice"}, delegate: "alice", direct: "alice"},
	"dave":  {weight: 400, path: []string{"dave"}, delegate: "dave"},
}

// concMemberPerm 生成确定性的成员排列：旋转与反向旋转交替，使并发请求采用
// 互不相同的成员排列；排列只影响展示顺序，不影响内容相等性。
func concMemberPerm(i int) []string {
	n := len(concCreateRoster)
	out := make([]string, n)
	for j := range concCreateRoster {
		out[j] = concCreateRoster[(j+i)%n]
	}
	if i%2 == 1 {
		for j, k := 0, n-1; j < k; j, k = j+1, k-1 {
			out[j], out[k] = out[k], out[j]
		}
	}
	return out
}

// concDelegationPerm 让委托参数也采用不同排列先后：两条委托的输入次序
// 不影响同一项委托关系的含义。
func concDelegationPerm(i int) []string {
	d := []string{"bob:carol", "carol:alice"}
	if i%2 == 0 {
		return []string{d[1], d[0]}
	}
	return d
}

func concMemberIDs(members []string) []string {
	ids := make([]string, len(members))
	for i, m := range members {
		ids[i] = m[:strings.LastIndex(m, ":")]
	}
	return ids
}

// concCreateVoteArgs 构造一次 create-vote --json 调用的完整参数。
// 治理规则在两个用例间固定：quorum=600、voting=[100,200)、timelock=300。
func concCreateVoteArgs(id string, members, delegates, actions []string) []string {
	args := []string{"create-vote", "--id", id}
	for _, m := range members {
		args = append(args, "--member", m)
	}
	for _, d := range delegates {
		args = append(args, "--delegate", d)
	}
	args = append(args, "--quorum", "600", "--start", "100", "--deadline", "200", "--timelock", "300")
	for _, a := range actions {
		args = append(args, "--action", a)
	}
	return append(args, "--json")
}

// runConcurrentCreates 用同一个启动栅栏同时放出 n 个独立创建进程，
// 每个进程采用不同的成员排列与委托排列，动作由 actionsFor(i) 决定。
func runConcurrentCreates(t *testing.T, binary, state, id string, n int, actionsFor func(i int) []string) []createConcResult {
	t.Helper()
	results := make([]createConcResult, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			members := concMemberPerm(i)
			args := concCreateVoteArgs(id, members, concDelegationPerm(i), actionsFor(i))
			<-start
			so, se, code := runCLI(t, binary, state, args...)
			results[i] = createConcResult{
				code:        code,
				stdout:      so,
				stderr:      se,
				memberOrder: concMemberIDs(members),
			}
		}(i)
	}
	close(start)
	wg.Wait()
	return results
}

// assertSavedConcProposal 核对胜出的保存版本：仍在投票状态、票据为空且无计票
// 结论；成员编号与原始权重、委托关系（含两次转交的完整路径与最终代表）、
// 法定人数、投票窗口、时间锁、总权重与有序动作全部符合本次共同提交的定义。
// wantMemberOrder 非空时还要求成员展示次序就是该顺序（首次创建者的排列）。
func assertSavedConcProposal(t *testing.T, v *VoteProposalView, id string, wantActions, wantMemberOrder []string) {
	t.Helper()
	if v == nil {
		t.Fatal("saved proposal view is nil")
	}
	if v.ID != id || v.Source != "vote" || v.State != "voting" {
		t.Fatalf("saved proposal id=%q source=%q state=%q, want %q/vote/voting", v.ID, v.Source, v.State, id)
	}
	if v.Quorum != 600 || v.StartAt != 100 || v.Deadline != 200 || v.TimelockEnd != 300 {
		t.Fatalf("governance rules changed: quorum=%d window=[%d,%d) timelock=%d",
			v.Quorum, v.StartAt, v.Deadline, v.TimelockEnd)
	}
	if v.TotalWeight != 1000 {
		t.Fatalf("total_weight=%d, want 1000", v.TotalWeight)
	}
	if !reflect.DeepEqual(v.Actions, wantActions) {
		t.Fatalf("actions=%v, want %v in order", v.Actions, wantActions)
	}
	if len(v.Ballots) != 0 {
		t.Fatalf("fresh voting proposal must have empty ballots, got %d: %+v", len(v.Ballots), v.Ballots)
	}
	if v.Tally != nil {
		t.Fatalf("fresh voting proposal must have no tally conclusion, got %+v", v.Tally)
	}
	if len(v.Members) != len(concMemberExpectations) {
		t.Fatalf("member count=%d, want %d", len(v.Members), len(concMemberExpectations))
	}
	gotOrder := make([]string, 0, len(v.Members))
	for _, m := range v.Members {
		gotOrder = append(gotOrder, m.ID)
		want, ok := concMemberExpectations[m.ID]
		if !ok {
			t.Fatalf("unexpected member %q in saved proposal", m.ID)
		}
		if m.Weight != want.weight {
			t.Fatalf("member %s weight=%d, want %d", m.ID, m.Weight, want.weight)
		}
		if m.Delegate != want.delegate {
			t.Fatalf("member %s final representative=%q, want %q", m.ID, m.Delegate, want.delegate)
		}
		if m.Direct != want.direct {
			t.Fatalf("member %s direct_to=%q, want %q", m.ID, m.Direct, want.direct)
		}
		if !reflect.DeepEqual(m.Path, want.path) {
			t.Fatalf("member %s delegation path=%v, want %v (intermediate hop must not be lost)",
				m.ID, m.Path, want.path)
		}
	}
	if wantMemberOrder != nil && !reflect.DeepEqual(gotOrder, wantMemberOrder) {
		t.Fatalf("member display order=%v, must preserve first creator's order %v; reordered retries must not rewrite it",
			gotOrder, wantMemberOrder)
	}
}

// assertNoTreasuryChange 核对创建并发结束后资金库余额、账户余额与执行凭据
// 全部保持创建前的值：创建只登记提案，绝不转账、不产生凭据。
func assertNoTreasuryChange(t *testing.T, binary, state string, initial int64) {
	t.Helper()
	bo, be, code := runCLI(t, binary, state, "balances", "--json")
	if code != 0 {
		t.Fatalf("balances query failed: %s", be)
	}
	var bal struct {
		Treasury int64            `json:"treasury"`
		Balances map[string]int64 `json:"balances"`
	}
	if err := json.Unmarshal([]byte(bo), &bal); err != nil {
		t.Fatalf("balances output is not JSON: %v\n%s", err, bo)
	}
	if bal.Treasury != initial {
		t.Fatalf("treasury balance=%d, must stay %d", bal.Treasury, initial)
	}
	if len(bal.Balances) != 0 {
		t.Fatalf("no transfers may happen at create time, got balances %v", bal.Balances)
	}
	ro, re, code := runCLI(t, binary, state, "receipts", "--json")
	if code != 0 {
		t.Fatalf("receipts query failed: %s", re)
	}
	var receipts []json.RawMessage
	if err := json.Unmarshal([]byte(ro), &receipts); err != nil {
		t.Fatalf("receipts output is not JSON: %v\n%s", err, ro)
	}
	if len(receipts) != 0 {
		t.Fatalf("create must produce no execution receipts, got %d", len(receipts))
	}
}

// TestCrossProcessConcurrentCreateVoteIdentical：多个进程同时创建同一编号、
// 语义完全相同（成员编号与原始权重、委托关系、法定人数、投票窗口、时间锁、
// 有序动作一致）但成员排列与委托排列不同的投票提案。所有请求都必须成功，
// 其中恰好一个表示首次创建，其余表示已经存在；所有成功返回对应同一份保存
// 提案，成员展示次序保留首次成功请求的排列，换序重试不得改写。
func TestCrossProcessConcurrentCreateVoteIdentical(t *testing.T) {
	binary := buildCLI(t)
	state := filepath.Join(t.TempDir(), "treasury.json")
	if _, _, code := runCLI(t, binary, state, "init", "--balance", "1000"); code != 0 {
		t.Fatal("init failed")
	}

	const id = "gip-conc-create"
	wantActions := []string{"transfer:audits:100", "transfer:legal:50"}
	const n = 12
	results := runConcurrentCreates(t, binary, state, id, n, func(int) []string { return wantActions })

	firstCreators := 0
	var winnerView *VoteProposalView
	var winnerMemberOrder []string
	for i := range results {
		r := &results[i]
		if r.code != 0 {
			t.Fatalf("process %d failed although content is identical: exit=%d stderr=%s", i, r.code, r.stderr)
		}
		r.resp = &createVoteCLIResponse{}
		if err := json.Unmarshal([]byte(r.stdout), r.resp); err != nil {
			t.Fatalf("process %d success output is not JSON: %v\n%s", i, err, r.stdout)
		}
		if r.resp.Proposal == nil {
			t.Fatalf("process %d success output missing proposal object: %s", i, r.stdout)
		}
		if !r.resp.Existed {
			firstCreators++
			winnerView = r.resp.Proposal
			winnerMemberOrder = r.memberOrder
		}
	}
	if firstCreators != 1 {
		t.Fatalf("exactly one request may report first creation, got %d", firstCreators)
	}

	// 所有成功返回必须对应同一份保存提案，且成员次序为首次创建者的排列。
	for i := range results {
		r := &results[i]
		if !reflect.DeepEqual(r.resp.Proposal, winnerView) {
			t.Fatalf("process %d returned a different proposal view:\n%+v\nwant:\n%+v",
				i, r.resp.Proposal, winnerView)
		}
		assertSavedConcProposal(t, r.resp.Proposal, id, wantActions, winnerMemberOrder)
	}

	// 全部创建结束后，单项查询只能展示这一个完整的胜出版本。
	so, se, code := runCLI(t, binary, state, "proposal", "--id", id, "--json")
	if code != 0 {
		t.Fatalf("proposal query failed: %s", se)
	}
	var saved VoteProposalView
	if err := json.Unmarshal([]byte(so), &saved); err != nil {
		t.Fatalf("proposal query output is not JSON: %v\n%s", err, so)
	}
	assertSavedConcProposal(t, &saved, id, wantActions, winnerMemberOrder)

	// 提案列表同样只有一个完整版本。
	lo, le, code := runCLI(t, binary, state, "proposals", "--json")
	if code != 0 {
		t.Fatalf("proposals query failed: %s", le)
	}
	var listed []VoteProposalView
	if err := json.Unmarshal([]byte(lo), &listed); err != nil {
		t.Fatalf("proposals output is not JSON: %v\n%s", err, lo)
	}
	if len(listed) != 1 || listed[0].ID != id ||
		!reflect.DeepEqual(listed[0].Actions, wantActions) || listed[0].State != "voting" {
		t.Fatalf("proposal list must contain exactly one voting winner, got %+v", listed)
	}

	// 仍在投票、票据为空、无计票结论；资金库余额、账户余额与执行凭据不变。
	assertNoTreasuryChange(t, binary, state, 1000)
}

// TestCrossProcessConcurrentCreateVoteActionOrderConflict：两组请求使用相同
// 成员与治理规则，仅动作次序不同（先 100 后 50 vs 先 50 后 100；合计金额与
// 最终收款账户相同，仍是不同的提案定义）。任意一组都可能成为首次成功方：
// 与保存版本相同的请求全部成功（恰好一个首次创建，其余确认已有提案），
// 另一组全部以现有的内容冲突失败——退出码 1、原因写入 stderr、stdout 不输出
// 成功创建结果，冲突不得被包装成成功重试。
func TestCrossProcessConcurrentCreateVoteActionOrderConflict(t *testing.T) {
	binary := buildCLI(t)
	state := filepath.Join(t.TempDir(), "treasury.json")
	if _, _, code := runCLI(t, binary, state, "init", "--balance", "1000"); code != 0 {
		t.Fatal("init failed")
	}

	const id = "gip-conc-conflict"
	actionsA := []string{"transfer:audits:100", "transfer:audits:50"}
	actionsB := []string{"transfer:audits:50", "transfer:audits:100"}
	const perGroup = 8
	n := perGroup * 2
	actionsFor := func(i int) []string {
		if i < perGroup {
			return actionsA
		}
		return actionsB
	}
	results := runConcurrentCreates(t, binary, state, id, n, actionsFor)
	for i := range results {
		if i < perGroup {
			results[i].group = "A"
		} else {
			results[i].group = "B"
		}
	}

	creatorIdx := -1
	for i := range results {
		r := &results[i]
		if r.code != 0 {
			continue // 冲突结果稍后逐类核对
		}
		r.resp = &createVoteCLIResponse{}
		if err := json.Unmarshal([]byte(r.stdout), r.resp); err != nil {
			t.Fatalf("process %d success output is not JSON: %v\n%s", i, err, r.stdout)
		}
		if r.resp.Proposal == nil {
			t.Fatalf("process %d success output missing proposal object: %s", i, r.stdout)
		}
		if !r.resp.Existed {
			if creatorIdx != -1 {
				t.Fatalf("multiple requests (%d and %d) both claim first creation", creatorIdx, i)
			}
			creatorIdx = i
		}
	}
	if creatorIdx == -1 {
		t.Fatal("exactly one request must create the proposal first, none succeeded")
	}
	winnerGroup := results[creatorIdx].group
	wantActions := actionsFor(creatorIdx)

	successes, conflicts := 0, 0
	for i := range results {
		r := &results[i]
		if r.group == winnerGroup {
			// 与保存版本相同：全部成功，除首次创建者外都必须是 already_exists。
			if r.code != 0 {
				t.Fatalf("process %d in winning group %s must succeed: exit=%d stderr=%s",
					i, r.group, r.code, r.stderr)
			}
			if i == creatorIdx {
				if r.resp.Existed {
					t.Fatalf("creator process %d must report first creation, got already_exists", i)
				}
			} else if !r.resp.Existed {
				t.Fatalf("process %d must confirm the existing proposal (already_exists=true)", i)
			}
			if !reflect.DeepEqual(r.resp.Proposal, results[creatorIdx].resp.Proposal) {
				t.Fatalf("process %d returned a different proposal than the saved winner", i)
			}
			assertSavedConcProposal(t, r.resp.Proposal, id, wantActions, nil)
			successes++
		} else {
			// 内容冲突：域错误退出码 1，原因写 stderr，stdout 不输出成功结果。
			if r.code != 1 {
				t.Fatalf("conflicting process %d exit=%d, want 1 (stdout=%s stderr=%s)",
					i, r.code, r.stdout, r.stderr)
			}
			if strings.TrimSpace(r.stdout) != "" {
				t.Fatalf("conflict must not print a success result on stdout, got %q", r.stdout)
			}
			if strings.Contains(r.stdout, "already_exists") {
				t.Fatalf("conflict must not be wrapped as a successful retry: %q", r.stdout)
			}
			if !strings.Contains(r.stderr, "different timelock or actions") ||
				!strings.Contains(r.stderr, id) {
				t.Fatalf("conflict stderr must name the content conflict and proposal, got %q", r.stderr)
			}
			conflicts++
		}
	}
	if successes != perGroup {
		t.Fatalf("winning group successes=%d, want all %d", successes, perGroup)
	}
	if conflicts != perGroup {
		t.Fatalf("losing group conflicts=%d, want all %d", conflicts, perGroup)
	}

	// 全部创建结束后：单项查询与列表都只展示一个完整胜出版本，动作原文与
	// 次序一致；仍在投票、票据为空、无计票结论，资金与凭据保持创建前的值。
	so, se, code := runCLI(t, binary, state, "proposal", "--id", id, "--json")
	if code != 0 {
		t.Fatalf("proposal query failed: %s", se)
	}
	var saved VoteProposalView
	if err := json.Unmarshal([]byte(so), &saved); err != nil {
		t.Fatalf("proposal query output is not JSON: %v\n%s", err, so)
	}
	assertSavedConcProposal(t, &saved, id, wantActions, nil)
	if saved.Actions[0] != wantActions[0] || saved.Actions[1] != wantActions[1] {
		t.Fatalf("saved action text/order=%v, want %v", saved.Actions, wantActions)
	}

	lo, le, code := runCLI(t, binary, state, "proposals", "--json")
	if code != 0 {
		t.Fatalf("proposals query failed: %s", le)
	}
	var listed []VoteProposalView
	if err := json.Unmarshal([]byte(lo), &listed); err != nil {
		t.Fatalf("proposals output is not JSON: %v\n%s", err, lo)
	}
	if len(listed) != 1 || listed[0].ID != id ||
		!reflect.DeepEqual(listed[0].Actions, wantActions) || listed[0].State != "voting" {
		t.Fatalf("proposal list must contain exactly one winner with its original action order, got %+v", listed)
	}

	assertNoTreasuryChange(t, binary, state, 1000)
}
