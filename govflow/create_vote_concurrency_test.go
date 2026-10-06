package govflow

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// createVoteCLIResponse 是 create-vote --json 的成功响应：完整提案视图加
// “是否已存在”标志。首次创建 already_exists=false，相同内容重试为 true。
type createVoteCLIResponse struct {
	Proposal      *VoteProposalView `json:"proposal"`
	AlreadyExists bool              `json:"already_exists"`
}

// createVoteCLIArgs 按给定成员排列、委托排列与有序动作拼出 create-vote 参数。
// 成员与委托的排列先后不影响提案相等性；动作原文及顺序参与相等性比较。
func createVoteCLIArgs(id string, members, delegates []string, quorum, start, deadline, timelock string, actions []string) []string {
	args := []string{"create-vote", "--id", id}
	for _, m := range members {
		args = append(args, "--member", m)
	}
	for _, d := range delegates {
		args = append(args, "--delegate", d)
	}
	args = append(args, "--quorum", quorum, "--start", start, "--deadline", deadline, "--timelock", timelock)
	for _, a := range actions {
		args = append(args, "--action", a)
	}
	return append(args, "--json")
}

// rotatedStrings 返回 base 向左轮换 k 位后的副本，用于生成不同的成员排列。
func rotatedStrings(base []string, k int) []string {
	out := make([]string, len(base))
	for i := range base {
		out[i] = base[(i+k)%len(base)]
	}
	return out
}

// argMemberIDs 从 ID:WEIGHT 形参数中取出成员编号（按最后一个冒号切分）。
func argMemberIDs(members []string) []string {
	ids := make([]string, len(members))
	for i, m := range members {
		ids[i] = m[:strings.LastIndex(m, ":")]
	}
	return ids
}

// viewMemberIDs 返回查询视图中成员的展示次序。
func viewMemberIDs(v *VoteProposalView) []string {
	ids := make([]string, len(v.Members))
	for i, m := range v.Members {
		ids[i] = m.ID
	}
	return ids
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// parseCreateVoteResponse 解析一次成功的 create-vote --json 输出。
func parseCreateVoteResponse(t *testing.T, stdout string) *createVoteCLIResponse {
	t.Helper()
	var resp createVoteCLIResponse
	if err := json.Unmarshal([]byte(stdout), &resp); err != nil {
		t.Fatalf("create-vote output is not JSON: %v\n%s", err, stdout)
	}
	if resp.Proposal == nil {
		t.Fatalf("create-vote success output missing proposal object:\n%s", stdout)
	}
	return &resp
}

// queryVoteProposal 以 --json 查询单项投票提案并解析视图。
func queryVoteProposal(t *testing.T, binary, state, id string) *VoteProposalView {
	t.Helper()
	so, se, code := runCLI(t, binary, state, "proposal", "--id", id, "--json")
	if code != 0 {
		t.Fatalf("proposal query exit=%d: %s", code, se)
	}
	var view VoteProposalView
	if err := json.Unmarshal([]byte(so), &view); err != nil {
		t.Fatalf("proposal query output is not JSON: %v\n%s", err, so)
	}
	return &view
}

// checkPristineTreasury 验证全部创建请求结束后资金未被触碰：
// 资金库余额保持初始值、没有任何账户余额、没有任何执行凭据。
func checkPristineTreasury(t *testing.T, binary, state string, balance string) {
	t.Helper()
	bo, _, code := runCLI(t, binary, state, "balances", "--json")
	if code != 0 || !strings.Contains(bo, `"treasury": `+balance) || !strings.Contains(bo, `"balances": {}`) {
		t.Fatalf("treasury changed by create-vote requests, exit=%d:\n%s", code, bo)
	}
	ro, _, code := runCLI(t, binary, state, "receipts", "--json")
	if code != 0 || strings.TrimSpace(ro) != "[]" {
		t.Fatalf("no receipts may exist after create-vote, exit=%d:\n%s", code, ro)
	}
}

// TestCrossProcessConcurrentCreateVoteIdentical：多个独立进程同时对同一编号、
// 同一内容（仅成员排列与委托排列不同）发起 create-vote。全部请求都必须成功，
// 其中恰好一个表示首次创建（already_exists=false），其余表示已经存在；所有成功
// 响应都对应同一份保存的提案，成员展示次序保留首次成功请求的排列。提案含一条
// 两次转交的委托链（carol -> bob -> alice），查询必须展示完整路径、最终代表、
// 成员原始权重与总权重，不因并发输入排列不同而丢失中间成员。
func TestCrossProcessConcurrentCreateVoteIdentical(t *testing.T) {
	binary := buildCLI(t)
	state := filepath.Join(t.TempDir(), "treasury.json")
	if _, _, code := runCLI(t, binary, state, "init", "--balance", "1000"); code != 0 {
		t.Fatal("init failed")
	}

	baseMembers := []string{"alice:300", "bob:200", "carol:100", "dave:400"}
	// 两次转交：carol -> bob -> alice；dave 不委托。
	baseDelegates := []string{"carol:bob", "bob:alice"}
	actions := []string{"transfer:audits:100"}

	const n = 12
	memberOrders := make([][]string, n)
	delegateOrders := make([][]string, n)
	for i := 0; i < n; i++ {
		memberOrders[i] = rotatedStrings(baseMembers, i)
		delegateOrders[i] = baseDelegates
		if i%2 == 1 {
			delegateOrders[i] = []string{baseDelegates[1], baseDelegates[0]}
		}
	}

	stdouts := make([]string, n)
	stderrs := make([]string, n)
	codes := make([]int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			args := createVoteCLIArgs("gip-conc", memberOrders[i], delegateOrders[i],
				"600", "100", "200", "300", actions)
			stdouts[i], stderrs[i], codes[i] = runCLI(t, binary, state, args...)
		}(i)
	}
	close(start)
	wg.Wait()

	// 内容完全相同的并发请求全部成功；恰好一个表示首次创建。
	responses := make([]*createVoteCLIResponse, n)
	winner := -1
	for i := 0; i < n; i++ {
		if codes[i] != 0 {
			t.Fatalf("identical create %d exit=%d: %s", i, codes[i], stderrs[i])
		}
		responses[i] = parseCreateVoteResponse(t, stdouts[i])
		if !responses[i].AlreadyExists {
			if winner != -1 {
				t.Fatalf("both process %d and %d claim first creation", winner, i)
			}
			winner = i
		}
	}
	if winner == -1 {
		t.Fatal("no process reported first creation")
	}

	// 所有成功响应都对应同一份保存的提案：成员展示次序必须与首次成功请求
	// 提交的排列一致，不能被其它排列的并发重试改写。
	wantOrder := argMemberIDs(memberOrders[winner])
	for i, resp := range responses {
		if got := viewMemberIDs(resp.Proposal); !sameStrings(got, wantOrder) {
			t.Fatalf("response %d member order %v, want first-winner order %v", i, got, wantOrder)
		}
		if !sameStrings(resp.Proposal.Actions, actions) {
			t.Fatalf("response %d actions %v, want %v", i, resp.Proposal.Actions, actions)
		}
		if resp.Proposal.ID != "gip-conc" || resp.Proposal.State != "voting" ||
			resp.Proposal.Quorum != 600 || resp.Proposal.StartAt != 100 ||
			resp.Proposal.Deadline != 200 || resp.Proposal.TimelockEnd != 300 {
			t.Fatalf("response %d proposal header diverged: %+v", i, resp.Proposal)
		}
	}

	// 单项查询：仍是首次成功请求保存的那一份，两次转交链完整——
	// carol 的路径必须包含中间成员 bob 与最终代表 alice，不能因并发到达的
	// 不同输入排列而漏掉中间成员。
	view := queryVoteProposal(t, binary, state, "gip-conc")
	if got := viewMemberIDs(view); !sameStrings(got, wantOrder) {
		t.Fatalf("stored member order %v, want first-winner order %v", got, wantOrder)
	}
	byID := map[string]MemberView{}
	for _, m := range view.Members {
		byID[m.ID] = m
	}
	if len(byID) != 4 {
		t.Fatalf("stored proposal has %d members, want 4", len(byID))
	}
	for id, wantWeight := range map[string]int64{"alice": 300, "bob": 200, "carol": 100, "dave": 400} {
		if byID[id].Weight != wantWeight {
			t.Fatalf("member %s weight=%d, want %d", id, byID[id].Weight, wantWeight)
		}
	}
	if !sameStrings(byID["carol"].Path, []string{"carol", "bob", "alice"}) ||
		byID["carol"].Delegate != "alice" || byID["carol"].Direct != "bob" {
		t.Fatalf("carol delegation chain broken: %+v", byID["carol"])
	}
	if !sameStrings(byID["bob"].Path, []string{"bob", "alice"}) ||
		byID["bob"].Delegate != "alice" || byID["bob"].Direct != "alice" {
		t.Fatalf("bob delegation chain broken: %+v", byID["bob"])
	}
	if !sameStrings(byID["alice"].Path, []string{"alice"}) || byID["alice"].Delegate != "alice" {
		t.Fatalf("alice must be final representative: %+v", byID["alice"])
	}
	if !sameStrings(byID["dave"].Path, []string{"dave"}) || byID["dave"].Delegate != "dave" {
		t.Fatalf("dave must represent itself: %+v", byID["dave"])
	}
	if view.TotalWeight != 1000 {
		t.Fatalf("total_weight=%d, want 1000", view.TotalWeight)
	}

	// 全部创建结束后：提案仍处于投票状态，票据为空且没有计票结论。
	if view.State != "voting" || len(view.Ballots) != 0 || view.Tally != nil {
		t.Fatalf("proposal must still be open voting with no ballots/tally: %+v", view)
	}

	// 列表查询只展示这一个完整版本，动作原文与次序一致。
	lo, _, code := runCLI(t, binary, state, "proposals", "--json")
	if code != 0 {
		t.Fatal("proposals query failed")
	}
	var listed []VoteProposalView
	if err := json.Unmarshal([]byte(lo), &listed); err != nil {
		t.Fatalf("proposals output is not JSON: %v\n%s", err, lo)
	}
	if len(listed) != 1 || listed[0].ID != "gip-conc" || !sameStrings(listed[0].Actions, actions) {
		t.Fatalf("proposals list must show exactly the winning version: %+v", listed)
	}

	// 并发落定后的串行换序重试仍是幂等确认：成功、already_exists=true，
	// 成员展示次序不被改写。
	retryArgs := createVoteCLIArgs("gip-conc", rotatedStrings(baseMembers, 2),
		[]string{baseDelegates[1], baseDelegates[0]}, "600", "100", "200", "300", actions)
	so, se, code := runCLI(t, binary, state, retryArgs...)
	if code != 0 {
		t.Fatalf("serial reordered retry exit=%d: %s", code, se)
	}
	retry := parseCreateVoteResponse(t, so)
	if !retry.AlreadyExists {
		t.Fatalf("serial retry after concurrency must report already_exists, got:\n%s", so)
	}
	if got := viewMemberIDs(retry.Proposal); !sameStrings(got, wantOrder) {
		t.Fatalf("serial reordered retry rewrote member order: %v, want %v", got, wantOrder)
	}

	// 创建不动资金：余额、账户与执行凭据保持创建前的值。
	checkPristineTreasury(t, binary, state, "1000")
}

// TestCrossProcessConcurrentCreateVoteConflict：两组进程同时对同一编号发起
// 内容冲突的 create-vote——成员与治理规则相同，仅动作次序不同（先转 100 再转 50
// 与先转 50 再转 100；合计金额与最终收款账户相同，仍是不同的提案定义）。
// 任意一组都可以成为首次成功的一方：与保存版本相同的请求全部成功（恰好一个
// 首次创建，其余确认已存在），另一组全部以内容冲突失败（退出码 1、原因在
// 标准错误、不输出成功创建结果）。最终只保存一个完整的胜出版本。
func TestCrossProcessConcurrentCreateVoteConflict(t *testing.T) {
	binary := buildCLI(t)
	state := filepath.Join(t.TempDir(), "treasury.json")
	if _, _, code := runCLI(t, binary, state, "init", "--balance", "1000"); code != 0 {
		t.Fatal("init failed")
	}

	members := []string{"alice:600", "dave:400"}
	actionsA := []string{"transfer:acct:100", "transfer:acct:50"}
	actionsB := []string{"transfer:acct:50", "transfer:acct:100"}

	const perGroup = 8
	const total = 2 * perGroup
	stdouts := make([]string, total)
	stderrs := make([]string, total)
	codes := make([]int, total)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			actions := actionsA
			if i%2 == 1 {
				actions = actionsB
			}
			args := createVoteCLIArgs("gip-conflict", members, nil,
				"600", "0", "100", "200", actions)
			stdouts[i], stderrs[i], codes[i] = runCLI(t, binary, state, args...)
		}(i)
	}
	close(start)
	wg.Wait()

	// 按进程归属分组统计：i 偶数为 A 组（100 先 50 后），奇数为 B 组。
	groupOf := func(i int) int { return i % 2 }
	successes := [2]int{}
	failures := [2]int{}
	for i := 0; i < total; i++ {
		if codes[i] == 0 {
			successes[groupOf(i)]++
		} else {
			failures[groupOf(i)]++
		}
	}
	var winnerGroup, loserGroup int
	switch {
	case successes[0] == perGroup && failures[1] == perGroup:
		winnerGroup, loserGroup = 0, 1
	case successes[1] == perGroup && failures[0] == perGroup:
		winnerGroup, loserGroup = 1, 0
	default:
		t.Fatalf("groups diverged: successes=%v failures=%v (one group must fully win)",
			successes, failures)
	}
	wantActions := actionsA
	if winnerGroup == 1 {
		wantActions = actionsB
	}

	// 胜出组：全部成功，恰好一个首次创建，成功 JSON 保留提案对象与
	// 已存在标志，提案动作与保存版本一致。
	firstCreates := 0
	for i := 0; i < total; i++ {
		if groupOf(i) != winnerGroup {
			continue
		}
		resp := parseCreateVoteResponse(t, stdouts[i])
		if !resp.AlreadyExists {
			firstCreates++
		}
		if !sameStrings(resp.Proposal.Actions, wantActions) {
			t.Fatalf("winner response %d actions %v, want saved version %v",
				i, resp.Proposal.Actions, wantActions)
		}
		if resp.Proposal.ID != "gip-conflict" || resp.Proposal.State != "voting" {
			t.Fatalf("winner response %d proposal diverged: %+v", i, resp.Proposal)
		}
	}
	if firstCreates != 1 {
		t.Fatalf("exactly one process may claim first creation, got %d", firstCreates)
	}

	// 失败组：全部以内容冲突失败——退出码 1，原因写到标准错误，
	// 标准输出不出现成功创建结果，不能把冲突包装成一次成功重试。
	for i := 0; i < total; i++ {
		if groupOf(i) != loserGroup {
			continue
		}
		if codes[i] != 1 {
			t.Fatalf("conflicting create %d exit=%d, want 1 (stdout=%q)", i, codes[i], stdouts[i])
		}
		if !strings.Contains(stderrs[i], "different timelock or actions") ||
			!strings.Contains(stderrs[i], "gip-conflict") {
			t.Fatalf("conflicting create %d stderr missing reason: %q", i, stderrs[i])
		}
		if strings.Contains(stdouts[i], "already_exists") || strings.Contains(stdouts[i], "proposal") {
			t.Fatalf("conflicting create %d must not emit a success result, stdout=%q", i, stdouts[i])
		}
	}

	// 全部请求结束后：单项查询与列表只展示一个完整的胜出版本，
	// 动作原文与次序一致；提案仍处于投票状态，票据为空且没有计票结论。
	view := queryVoteProposal(t, binary, state, "gip-conflict")
	if !sameStrings(view.Actions, wantActions) {
		t.Fatalf("stored actions %v, want winning version %v", view.Actions, wantActions)
	}
	if view.State != "voting" || len(view.Ballots) != 0 || view.Tally != nil {
		t.Fatalf("proposal must still be open voting with no ballots/tally: %+v", view)
	}
	to, _, code := runCLI(t, binary, state, "proposal", "--id", "gip-conflict")
	if code != 0 || !strings.Contains(to, "action 0: "+wantActions[0]) ||
		!strings.Contains(to, "action 1: "+wantActions[1]) {
		t.Fatalf("text query must show winning action order, exit=%d:\n%s", code, to)
	}

	lo, _, code := runCLI(t, binary, state, "proposals", "--json")
	if code != 0 {
		t.Fatal("proposals query failed")
	}
	var listed []VoteProposalView
	if err := json.Unmarshal([]byte(lo), &listed); err != nil {
		t.Fatalf("proposals output is not JSON: %v\n%s", err, lo)
	}
	if len(listed) != 1 || listed[0].ID != "gip-conflict" || !sameStrings(listed[0].Actions, wantActions) {
		t.Fatalf("proposals list must show exactly the winning version: %+v", listed)
	}

	// 冲突与创建都不动资金：余额、账户与执行凭据保持创建前的值。
	checkPristineTreasury(t, binary, state, "1000")
}
