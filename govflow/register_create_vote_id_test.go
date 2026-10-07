package govflow

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// 本文件为“register 登记提案与 create-vote 投票提案共用编号”这条规则补充
// 自动化回归保障：两种来源之间不存在“内容相同就算同一次创建”的关系——
// 针对同一个资金库、同一个非空编号，即使两边提交的时间锁与动作原文完全相同，
// 也只能保存其中一种来源，另一种必须得到明确的编号冲突。
//
// 覆盖三类场景：
//  1. 编号尚未占用时两种创建请求同时到达（不预先假定登记方或投票方获胜）；
//  2. 投票提案到期计票被拒绝后，同编号登记仍报冲突，编号仍归原提案所有；
//  3. 不同编号的合法提案仍可由两种来源分别正常创建。
//
// 冲突沿用现有错误含义（ErrProposalConflict），命令行以退出码 1 结束，
// 只在标准错误说明编号冲突，标准输出不出现成功创建的记录。

// sharedIDConflictMeaning 是 ErrProposalConflict 的既有错误文案：两种来源的
// 编号冲突都包装它，测试据此识别“明确的编号冲突”，而不是新的错误种类。
const sharedIDConflictMeaning = "proposal already registered with different timelock or actions"

// registerCLIResponse 是 register --json 的成功结果：既有字段
// （id/state/timelock_end/actions/already_registered）必须原样保留。
type registerCLIResponse struct {
	ID                string   `json:"id"`
	State             string   `json:"state"`
	TimelockEnd       int64    `json:"timelock_end"`
	Actions           []string `json:"actions"`
	AlreadyRegistered bool     `json:"already_registered"`
}

// sharedIDRaceResult 记录一个并发创建进程（任一来源）的全部可观察结果。
type sharedIDRaceResult struct {
	source      string // "register" / "vote"
	code        int
	stdout      string
	stderr      string
	memberOrder []string // 投票来源：该请求自己提交的成员编号顺序
	regResp     *registerCLIResponse
	voteResp    *createVoteCLIResponse
}

// TestCrossProcessConcurrentRegisterVsCreateVoteSharedID：编号尚未占用时，
// 8 个 register 进程与 8 个 create-vote 进程同时到达，两边提交的内容各自合法，
// 且时间锁（300）与有序动作原文完全相同。两种来源之间不存在“内容相同就算
// 同一次创建”的关系：恰好一种来源整体获胜（一个首次创建、其余为相同内容确认），
// 另一种来源全部得到明确的编号冲突。测试不预先假定哪一方获胜：
//   - 登记方获胜时，查询只显示登记来源的 passed 提案；
//   - 投票方获胜时，查询只显示投票来源的 voting 提案，保留原始成员权重、
//     委托关系、投票窗口及有序动作。
//
// 成功返回的内容必须与实际保存的一致；失败方不得返回创建成功、不得留下第二份
// 同编号记录、不得改写已保存提案的来源或状态；单项查询与列表查询给出同一结果；
// 整个竞争不改变任何余额，也不产生执行凭据。
func TestCrossProcessConcurrentRegisterVsCreateVoteSharedID(t *testing.T) {
	binary := buildCLI(t)
	state := filepath.Join(t.TempDir(), "treasury.json")
	if _, _, code := runCLI(t, binary, state, "init", "--balance", "1000"); code != 0 {
		t.Fatal("init failed")
	}

	const id = "gip-shared-id-race"
	wantActions := []string{"transfer:audits:100", "transfer:legal:50"}
	const perSource = 8
	results := make([]sharedIDRaceResult, 2*perSource)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < perSource; i++ {
		wg.Add(2)
		go func(i int) { // 登记来源：时间锁与动作原文和投票方完全相同
			defer wg.Done()
			args := []string{"register", "--id", id, "--timelock", "300"}
			for _, a := range wantActions {
				args = append(args, "--action", a)
			}
			args = append(args, "--json")
			<-start
			so, se, code := runCLI(t, binary, state, args...)
			results[i] = sharedIDRaceResult{source: "register", code: code, stdout: so, stderr: se}
		}(i)
		go func(i int) { // 投票来源：同一编号、同一时间锁、同一动作列表
			defer wg.Done()
			members := concMemberPerm(i)
			args := concCreateVoteArgs(id, members, concDelegationPerm(i), wantActions)
			<-start
			so, se, code := runCLI(t, binary, state, args...)
			results[perSource+i] = sharedIDRaceResult{
				source: "vote", code: code, stdout: so, stderr: se,
				memberOrder: concMemberIDs(members),
			}
		}(i)
	}
	close(start)
	wg.Wait()

	// 解析全部成功响应，并统计两种来源的“首次创建”声明：全场景恰好一个，
	// 且它所属的来源即胜出来源。
	winner := ""
	firstClaims := 0
	for i := range results {
		r := &results[i]
		if r.code != 0 {
			continue
		}
		switch r.source {
		case "register":
			r.regResp = &registerCLIResponse{}
			if err := json.Unmarshal([]byte(r.stdout), r.regResp); err != nil {
				t.Fatalf("register process %d success output is not JSON: %v\n%s", i, err, r.stdout)
			}
			if !r.regResp.AlreadyRegistered {
				firstClaims++
				winner = "register"
			}
		case "vote":
			r.voteResp = &createVoteCLIResponse{}
			if err := json.Unmarshal([]byte(r.stdout), r.voteResp); err != nil {
				t.Fatalf("create-vote process %d success output is not JSON: %v\n%s", i, err, r.stdout)
			}
			if r.voteResp.Proposal == nil {
				t.Fatalf("create-vote process %d success output missing proposal object: %s", i, r.stdout)
			}
			if !r.voteResp.Existed {
				firstClaims++
				winner = "vote"
			}
		}
	}
	if firstClaims != 1 {
		t.Fatalf("exactly one request across both sources may claim first creation, got %d", firstClaims)
	}

	// 逐进程核对：胜出来源全部成功（一个首次创建、其余确认已有提案），
	// 失败来源全部以退出码 1 报编号冲突，stdout 不出现成功创建记录。
	var winnerMemberOrder []string
	for i := range results {
		r := &results[i]
		if r.source != winner {
			if r.code != 1 {
				t.Fatalf("%s process %d lost the race and must exit 1, got %d (stdout=%q stderr=%q)",
					r.source, i, r.code, r.stdout, r.stderr)
			}
			if strings.TrimSpace(r.stdout) != "" {
				t.Fatalf("%s process %d conflict must not print a success record on stdout, got %q",
					r.source, i, r.stdout)
			}
			if !strings.Contains(r.stderr, sharedIDConflictMeaning) || !strings.Contains(r.stderr, id) {
				t.Fatalf("%s process %d stderr must state the id conflict for %q, got %q",
					r.source, i, id, r.stderr)
			}
			continue
		}
		if r.code != 0 {
			t.Fatalf("%s process %d won the race and must succeed, got exit=%d stderr=%s",
				r.source, i, r.code, r.stderr)
		}
		switch r.source {
		case "register":
			resp := r.regResp
			if resp.ID != id || resp.State != "passed" || resp.TimelockEnd != 300 ||
				!reflect.DeepEqual(resp.Actions, wantActions) {
				t.Fatalf("register process %d response = %+v, want passed record id=%s timelock=300 actions=%v",
					i, resp, id, wantActions)
			}
		case "vote":
			if !r.voteResp.Existed {
				winnerMemberOrder = r.memberOrder
			}
			assertSavedConcProposal(t, r.voteResp.Proposal, id, wantActions, nil)
		}
	}

	// 全部创建结束后：单项查询与列表查询对该编号给出同一个结果，且只有一份记录。
	so, se, code := runCLI(t, binary, state, "proposal", "--id", id, "--json")
	if code != 0 {
		t.Fatalf("proposal query failed: %s", se)
	}
	lo, le, code := runCLI(t, binary, state, "proposals", "--json")
	if code != 0 {
		t.Fatalf("proposals query failed: %s", le)
	}
	var listed []map[string]any
	if err := json.Unmarshal([]byte(lo), &listed); err != nil {
		t.Fatalf("proposals output is not JSON: %v\n%s", err, lo)
	}
	if len(listed) != 1 || listed[0]["id"] != id {
		t.Fatalf("proposal list must contain exactly one record for %q (no second same-id record), got %s", id, lo)
	}

	switch winner {
	case "register":
		// 登记方获胜：查询只显示登记来源的 passed 提案，不带来源字段；
		// 每个成功响应都与实际保存的记录一致。
		var saved ProposalRecord
		if err := json.Unmarshal([]byte(so), &saved); err != nil {
			t.Fatalf("proposal query output is not a registered record: %v\n%s", err, so)
		}
		if saved.ID != id || saved.State != "passed" || saved.TimelockEnd != 300 ||
			!reflect.DeepEqual(saved.Actions, wantActions) {
			t.Fatalf("saved record = %+v, want passed id=%s timelock=300 actions=%v", saved, id, wantActions)
		}
		if _, hasSource := listed[0]["source"]; hasSource {
			t.Fatalf("registered record must not carry a vote source marker: %s", lo)
		}
		if listed[0]["state"] != "passed" {
			t.Fatalf("list entry state=%v, want passed (single query and list must agree)", listed[0]["state"])
		}
		for i := range results {
			r := &results[i]
			if r.source == "register" {
				resp := r.regResp
				if resp.ID != saved.ID || resp.State != saved.State || resp.TimelockEnd != saved.TimelockEnd ||
					!reflect.DeepEqual(resp.Actions, saved.Actions) {
					t.Fatalf("register process %d response %+v does not match the saved record %+v", i, resp, saved)
				}
			}
		}
	case "vote":
		// 投票方获胜：查询只显示投票来源的 voting 提案，保留原始成员权重、
		// 委托关系（含两次转交的完整路径）、投票窗口及有序动作；成员展示次序
		// 为首次创建者的排列；每个成功响应都与实际保存的提案一致。
		var saved VoteProposalView
		if err := json.Unmarshal([]byte(so), &saved); err != nil {
			t.Fatalf("proposal query output is not a vote view: %v\n%s", err, so)
		}
		assertSavedConcProposal(t, &saved, id, wantActions, winnerMemberOrder)
		if listed[0]["source"] != "vote" || listed[0]["state"] != "voting" {
			t.Fatalf("list entry source=%v state=%v, want vote/voting (single query and list must agree)",
				listed[0]["source"], listed[0]["state"])
		}
		var listedView VoteProposalView
		raw, _ := json.Marshal(listed[0])
		if err := json.Unmarshal(raw, &listedView); err != nil {
			t.Fatalf("list entry is not a vote view: %v\n%s", err, lo)
		}
		if !reflect.DeepEqual(&listedView, &saved) {
			t.Fatalf("single query and list query disagree:\nsingle: %+v\nlist:   %+v", &saved, &listedView)
		}
		for i := range results {
			r := &results[i]
			if r.source == "vote" && !reflect.DeepEqual(r.voteResp.Proposal, &saved) {
				t.Fatalf("create-vote process %d returned a proposal that does not match the saved one:\n%+v\nwant:\n%+v",
					i, r.voteResp.Proposal, &saved)
			}
		}
	}

	// 整个创建竞争不改变任何余额，也不产生执行凭据。
	assertNoTreasuryChange(t, binary, state, 1000)
}

// TestRegisterAfterRejectedTallyKeepsIDConflict：投票提案到期计票被拒绝后，
// 编号仍归原提案所有——再以同编号登记已通过提案，即使提交相同的时间锁与动作
// 原文，也必须继续报编号冲突（退出码 1、原因在 stderr、stdout 无成功记录）。
// 查询仍保留原来的 rejected 状态、成员及委托明细、实际票据与首次计票结论，
// 拒绝结论不得变成通过，原提案仍不能执行；同库另一项完整提案不受这次冲突影响。
func TestRegisterAfterRejectedTallyKeepsIDConflict(t *testing.T) {
	binary := buildCLI(t)
	state := filepath.Join(t.TempDir(), "treasury.json")
	if _, _, code := runCLI(t, binary, state, "init", "--balance", "1000"); code != 0 {
		t.Fatal("init failed")
	}

	// 投票提案：alice 经 bob->carol->alice 两次转交归集 600 权重。
	const id = "gip-rejected-keeps-id"
	createArgs := concCreateVoteArgs(id,
		[]string{"alice:300", "bob:200", "carol:100", "dave:400"},
		[]string{"bob:carol", "carol:alice"},
		[]string{"transfer:a:1"})
	if _, se, code := runCLI(t, binary, state, createArgs...); code != 0 {
		t.Fatalf("create-vote failed: %s", se)
	}
	// 同库另一项完整提案（登记来源），用于验证编号冲突不影响它的内容。
	if _, se, code := runCLI(t, binary, state, "register", "--id", "gip-other",
		"--timelock", "0", "--action", "transfer:o:1"); code != 0 {
		t.Fatalf("register gip-other failed: %s", se)
	}
	// alice 作为最终代表投反对票（归集权重 600），到期计票被拒绝。
	if _, se, code := runCLI(t, binary, state, "vote", "--id", id,
		"--voter", "alice", "--choice", "against", "--now", "150"); code != 0 {
		t.Fatalf("vote failed: %s", se)
	}
	so, se, code := runCLI(t, binary, state, "tally", "--id", id, "--now", "200", "--json")
	if code != 0 {
		t.Fatalf("tally failed: %s", se)
	}
	var tallyOut struct {
		Result *TallyResultView `json:"result"`
	}
	if err := json.Unmarshal([]byte(so), &tallyOut); err != nil || tallyOut.Result == nil {
		t.Fatalf("tally output is not JSON: %v\n%s", err, so)
	}
	if tallyOut.Result.Passed || tallyOut.Result.ForWeight != 0 || tallyOut.Result.AgainstWeight != 600 {
		t.Fatalf("tally must reject (for=0 against=600), got %+v", tallyOut.Result)
	}

	// 同编号登记已通过提案：相同时间锁（300）与动作原文，仍必须报编号冲突。
	ro, re, code := runCLI(t, binary, state, "register", "--id", id,
		"--timelock", "300", "--action", "transfer:a:1", "--json")
	if code != 1 {
		t.Fatalf("register over rejected voting id exit=%d, want 1 (stdout=%q stderr=%q)", code, ro, re)
	}
	if strings.TrimSpace(ro) != "" {
		t.Fatalf("conflict must not print a success record on stdout, got %q", ro)
	}
	for _, want := range []string{sharedIDConflictMeaning, id, "belongs to a voting proposal", "rejected"} {
		if !strings.Contains(re, want) {
			t.Fatalf("conflict stderr %q missing %q", re, want)
		}
	}

	// 投票来源的相同内容重试仍确认已有提案：编号继续归投票来源所有，
	// 状态保持 rejected，没有被登记请求换成 passed。
	co, ce, code := runCLI(t, binary, state, createArgs...)
	if code != 0 {
		t.Fatalf("identical create-vote retry should still confirm the voting proposal: %s", ce)
	}
	var retryResp createVoteCLIResponse
	if err := json.Unmarshal([]byte(co), &retryResp); err != nil || retryResp.Proposal == nil {
		t.Fatalf("create-vote retry output is not JSON: %v\n%s", err, co)
	}
	if !retryResp.Existed || retryResp.Proposal.State != "rejected" || retryResp.Proposal.Source != "vote" {
		t.Fatalf("create-vote retry = %+v, want already_exists confirmed rejected vote proposal", retryResp)
	}

	// 再次计票仍返回首次结论：拒绝不得变成通过，计票时间保持首次值。
	to, te, code := runCLI(t, binary, state, "tally", "--id", id, "--now", "999", "--json")
	if code != 0 {
		t.Fatalf("repeat tally failed: %s", te)
	}
	if !strings.Contains(to, `"passed": false`) || !strings.Contains(to, `"tallied_at": 200`) {
		t.Fatalf("repeat tally must return the first rejection, got %s", to)
	}

	// 单项查询：保留 rejected 状态、成员及委托明细、实际票据与首次计票结论。
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
	if saved.Quorum != 600 || saved.StartAt != 100 || saved.Deadline != 200 || saved.TimelockEnd != 300 {
		t.Fatalf("governance rules changed: quorum=%d window=[%d,%d) timelock=%d",
			saved.Quorum, saved.StartAt, saved.Deadline, saved.TimelockEnd)
	}
	if !reflect.DeepEqual(saved.Actions, []string{"transfer:a:1"}) {
		t.Fatalf("actions=%v, want [transfer:a:1]", saved.Actions)
	}
	if len(saved.Members) != len(concMemberExpectations) {
		t.Fatalf("member count=%d, want %d", len(saved.Members), len(concMemberExpectations))
	}
	for _, m := range saved.Members {
		want := concMemberExpectations[m.ID]
		if m.Weight != want.weight || m.Delegate != want.delegate || m.Direct != want.direct ||
			!reflect.DeepEqual(m.Path, want.path) {
			t.Fatalf("member %s = %+v, delegation details must be preserved as %+v", m.ID, m, want)
		}
	}
	if len(saved.Ballots) != 1 || saved.Ballots[0] != (BallotView{Representative: "alice", Weight: 600, Support: false, VotedAt: 150}) {
		t.Fatalf("actual ballot must be preserved (alice against, weight 600, voted_at 150), got %+v", saved.Ballots)
	}
	if saved.Tally == nil || saved.Tally.ForWeight != 0 || saved.Tally.AgainstWeight != 600 ||
		saved.Tally.Turnout != 600 || saved.Tally.Quorum != 600 || saved.Tally.Passed || saved.Tally.TalliedAt != 200 {
		t.Fatalf("first tally conclusion must stay rejected for=0 against=600 tallied_at=200, got %+v", saved.Tally)
	}

	// 列表查询与单项查询一致；另一项完整提案的内容不受这次编号冲突影响。
	lo, le, code := runCLI(t, binary, state, "proposals", "--json")
	if code != 0 {
		t.Fatalf("proposals query failed: %s", le)
	}
	var listed []map[string]any
	if err := json.Unmarshal([]byte(lo), &listed); err != nil {
		t.Fatalf("proposals output is not JSON: %v\n%s", err, lo)
	}
	if len(listed) != 2 {
		t.Fatalf("proposal list must contain exactly the rejected voting proposal and gip-other, got %s", lo)
	}
	seen := map[string]map[string]any{}
	for _, entry := range listed {
		idKey, _ := entry["id"].(string)
		seen[idKey] = entry
	}
	entry, ok := seen[id]
	if !ok || entry["source"] != "vote" || entry["state"] != "rejected" {
		t.Fatalf("list entry for %q must be the rejected vote proposal, got %v", id, entry)
	}
	other, ok := seen["gip-other"]
	if !ok || other["state"] != "passed" {
		t.Fatalf("gip-other must stay a passed registered proposal, got %v", other)
	}
	if _, hasSource := other["source"]; hasSource {
		t.Fatalf("gip-other must remain a registered record without a vote source marker: %v", other)
	}
	if tm, _ := other["timelock_end"].(float64); tm != 0 {
		t.Fatalf("gip-other timelock changed by the conflict: %v", other)
	}
	if acts, _ := other["actions"].([]any); len(acts) != 1 || acts[0] != "transfer:o:1" {
		t.Fatalf("gip-other actions changed by the conflict: %v", other)
	}

	// 原提案仍不能执行：状态是 rejected 而不是 passed，也不产生执行凭据。
	if eo, ee, code := runCLI(t, binary, state, "execute", "--id", id, "--now", "300"); code != 1 ||
		!strings.Contains(ee, "not passed") || strings.TrimSpace(eo) != "" {
		t.Fatalf("rejected proposal must not execute: exit=%d stdout=%q stderr=%q", code, eo, ee)
	}
	if _, _, code := runCLI(t, binary, state, "receipt", "--id", id); code != 1 {
		t.Fatalf("rejected proposal must have no execution receipt")
	}

	// 整个流程不改变任何余额，也不产生执行凭据。
	assertNoTreasuryChange(t, binary, state, 1000)
}

// TestDistinctIDsAcrossSourcesCreateNormally：不同编号的合法提案仍可由两种
// 来源分别正常创建并共存，共用编号规则不影响不同编号的创建、查询与输出形状。
func TestDistinctIDsAcrossSourcesCreateNormally(t *testing.T) {
	binary := buildCLI(t)
	state := filepath.Join(t.TempDir(), "treasury.json")
	if _, _, code := runCLI(t, binary, state, "init", "--balance", "500"); code != 0 {
		t.Fatal("init failed")
	}

	ro, re, code := runCLI(t, binary, state, "register", "--id", "gip-reg",
		"--timelock", "40", "--action", "transfer:x:1", "--json")
	if code != 0 {
		t.Fatalf("register on a free id failed: exit=%d stderr=%s", code, re)
	}
	var regResp registerCLIResponse
	if err := json.Unmarshal([]byte(ro), &regResp); err != nil {
		t.Fatalf("register output is not JSON: %v\n%s", err, ro)
	}
	if regResp.AlreadyRegistered || regResp.State != "passed" || regResp.TimelockEnd != 40 {
		t.Fatalf("register response = %+v, want first registration of a passed proposal", regResp)
	}

	co, ce, code := runCLI(t, binary, state, "create-vote", "--id", "gip-vot",
		"--member", "a:5", "--member", "b:5",
		"--quorum", "5", "--start", "0", "--deadline", "10", "--timelock", "20",
		"--action", "transfer:y:2", "--json")
	if code != 0 {
		t.Fatalf("create-vote on a free id failed: exit=%d stderr=%s", code, ce)
	}
	var voteResp createVoteCLIResponse
	if err := json.Unmarshal([]byte(co), &voteResp); err != nil || voteResp.Proposal == nil {
		t.Fatalf("create-vote output is not JSON: %v\n%s", err, co)
	}
	if voteResp.Existed || voteResp.Proposal.State != "voting" || voteResp.Proposal.Source != "vote" {
		t.Fatalf("create-vote response = %+v, want first creation of a voting proposal", voteResp)
	}

	// 单项查询：两种来源各自的编号各归各的记录。
	po, pe, code := runCLI(t, binary, state, "proposal", "--id", "gip-reg", "--json")
	if code != 0 {
		t.Fatalf("proposal gip-reg query failed: %s", pe)
	}
	var regRecord ProposalRecord
	if err := json.Unmarshal([]byte(po), &regRecord); err != nil {
		t.Fatalf("proposal gip-reg output is not JSON: %v\n%s", err, po)
	}
	if regRecord.State != "passed" || regRecord.TimelockEnd != 40 ||
		!reflect.DeepEqual(regRecord.Actions, []string{"transfer:x:1"}) {
		t.Fatalf("gip-reg record = %+v, want passed timelock=40 actions=[transfer:x:1]", regRecord)
	}
	vo, ve, code := runCLI(t, binary, state, "proposal", "--id", "gip-vot", "--json")
	if code != 0 {
		t.Fatalf("proposal gip-vot query failed: %s", ve)
	}
	var voteView VoteProposalView
	if err := json.Unmarshal([]byte(vo), &voteView); err != nil {
		t.Fatalf("proposal gip-vot output is not JSON: %v\n%s", err, vo)
	}
	if voteView.Source != "vote" || voteView.State != "voting" || voteView.Quorum != 5 ||
		voteView.StartAt != 0 || voteView.Deadline != 10 || voteView.TimelockEnd != 20 ||
		!reflect.DeepEqual(voteView.Actions, []string{"transfer:y:2"}) {
		t.Fatalf("gip-vot view = %+v, want voting vote-source proposal with its own rules", voteView)
	}

	// 列表查询同时包含两条记录，来源与状态各自正确。
	lo, le, code := runCLI(t, binary, state, "proposals", "--json")
	if code != 0 {
		t.Fatalf("proposals query failed: %s", le)
	}
	var listed []map[string]any
	if err := json.Unmarshal([]byte(lo), &listed); err != nil {
		t.Fatalf("proposals output is not JSON: %v\n%s", err, lo)
	}
	if len(listed) != 2 {
		t.Fatalf("proposal list must contain both proposals, got %s", lo)
	}
	seen := map[string]map[string]any{}
	for _, entry := range listed {
		idKey, _ := entry["id"].(string)
		seen[idKey] = entry
	}
	if regEntry, ok := seen["gip-reg"]; !ok || regEntry["state"] != "passed" {
		t.Fatalf("gip-reg must be listed as a passed registered proposal, got %v", regEntry)
	} else if _, hasSource := regEntry["source"]; hasSource {
		t.Fatalf("gip-reg must remain a registered record without a vote source marker: %v", regEntry)
	}
	if voteEntry, ok := seen["gip-vot"]; !ok || voteEntry["source"] != "vote" || voteEntry["state"] != "voting" {
		t.Fatalf("gip-vot must be listed as a voting vote-source proposal, got %v", voteEntry)
	}

	// 创建只登记提案：余额不变，不产生执行凭据。
	assertNoTreasuryChange(t, binary, state, 500)
}
