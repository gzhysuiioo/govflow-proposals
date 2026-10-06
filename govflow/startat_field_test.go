package govflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// corruptStartAtState 构造一份含投票提案 gip-start 的合法状态文件
// （开始 100、截止 200、时间锁 300），再按 mutate 改写该提案的
// start_at 字段，返回改写前后的字节与路径。phase 决定提案停留的生命周期：
//   - "voting"：alice 已投赞成（300 权重，150 时刻），尚未计票；
//   - "passed"：alice 与 bob 均投赞成（合计 500）后已计票通过；
//   - "rejected"：无人投票直接计票，零参与权重拒绝；
//   - "executed"：通过提案已执行（资金库向 audits 转账 100）。
//
// voting 变体中票据时间 150 落在真实窗口 [100,200) 内；一旦 start_at 被
// 删除，按“缺失补零”的旧行为该票据反而会落在 [0,200) 内显得合法——
// 正是新校验必须在窗口判断之前拦截的情形。
func corruptStartAtState(t *testing.T, phase string, mutate func(p map[string]any)) (path string, good, bad []byte) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "treasury.json")
	store, err := InitTreasury(path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	in := &CreateVoteInput{
		ID: "gip-start",
		Members: []VoteMember{
			{ID: "alice", Weight: 300},
			{ID: "bob", Weight: 200},
		},
		Quorum:      250,
		StartAt:     100,
		Deadline:    200,
		TimelockEnd: 300,
		Actions:     []string{"transfer:audits:100"},
	}
	mustCreateVote(t, store, in)
	switch phase {
	case "voting":
		if _, err := store.CastVote("gip-start", "alice", true, 150); err != nil {
			t.Fatal(err)
		}
	case "passed":
		if _, err := store.CastVote("gip-start", "alice", true, 150); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CastVote("gip-start", "bob", true, 150); err != nil {
			t.Fatal(err)
		}
		if r, err := store.TallyVote("gip-start", 200); err != nil || !r.Passed {
			t.Fatalf("tally passed: %+v err=%v", r, err)
		}
	case "rejected":
		if r, err := store.TallyVote("gip-start", 200); err != nil || r.Passed {
			t.Fatalf("tally rejected: %+v err=%v", r, err)
		}
	case "executed":
		if _, err := store.CastVote("gip-start", "alice", true, 150); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CastVote("gip-start", "bob", true, 150); err != nil {
			t.Fatal(err)
		}
		if _, err := store.TallyVote("gip-start", 200); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Execute("gip-start", 300); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown phase %q", phase)
	}
	store.Close()

	good, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// UseNumber 保留数字词面：指数、小数、超界整数等非法写法要逐字落盘，
	// 不被 float64 折叠，合法字段也保持原有整数写法。
	dec := json.NewDecoder(bytes.NewReader(good))
	dec.UseNumber()
	var clone map[string]any
	if err := dec.Decode(&clone); err != nil {
		t.Fatal(err)
	}
	p := clone["vote_proposals"].(map[string]any)["gip-start"].(map[string]any)
	mutate(p)
	bad, err = json.MarshalIndent(clone, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	bad = append(bad, '\n')
	if err := os.WriteFile(path, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, good, bad
}

// TestCorruptStartAtRejectedAsCorrupt：每项已保存投票提案的 start_at 都必须
// 明确写出且为 int64 范围内的 JSON 整数。字段缺失、为 null 或写成字符串/
// 布尔/对象/数组/小数/指数形式/超出范围，都判整份状态文件损坏，错误指出
// 提案编号、start_at 字段及缺失/空值/类型不符。尚在投票、已通过、被拒绝
// 与已执行的提案适用同一条要求：即使票据、计票结论与执行凭据都能与其余
// 内容核对一致，也不能宽免开始时间缺失，更不能补零后扩大投票窗口。
func TestCorruptStartAtRejectedAsCorrupt(t *testing.T) {
	mutations := map[string]struct {
		mutate func(p map[string]any)
		want   string
	}{
		"missing": {
			mutate: func(p map[string]any) { delete(p, "start_at") },
			want:   `field "start_at" is missing`,
		},
		"null": {
			mutate: func(p map[string]any) { p["start_at"] = nil },
			want:   `field "start_at" is null`,
		},
		"string": {
			mutate: func(p map[string]any) { p["start_at"] = "100" },
			want:   `field "start_at" has wrong type: want integer, got string`,
		},
		"boolean": {
			mutate: func(p map[string]any) { p["start_at"] = true },
			want:   `field "start_at" has wrong type: want integer, got boolean`,
		},
		"object": {
			mutate: func(p map[string]any) { p["start_at"] = map[string]any{"value": 100} },
			want:   `field "start_at" has wrong type: want integer, got object`,
		},
		"array": {
			mutate: func(p map[string]any) { p["start_at"] = []any{100} },
			want:   `field "start_at" has wrong type: want integer, got array`,
		},
		"fraction": {
			mutate: func(p map[string]any) { p["start_at"] = json.Number("100.5") },
			want:   `field "start_at" has wrong type: want integer, got number`,
		},
		"exponent": {
			mutate: func(p map[string]any) { p["start_at"] = json.Number("1e2") },
			want:   `field "start_at" has wrong type: want integer, got number`,
		},
		"out of range positive": {
			mutate: func(p map[string]any) { p["start_at"] = json.Number("9223372036854775808") },
			want:   `field "start_at" has wrong type: want integer, got number`,
		},
		"out of range negative": {
			mutate: func(p map[string]any) { p["start_at"] = json.Number("-9223372036854775809") },
			want:   `field "start_at" has wrong type: want integer, got number`,
		},
	}
	for _, phase := range []string{"voting", "passed", "rejected", "executed"} {
		for name, tc := range mutations {
			t.Run(phase+"/"+name, func(t *testing.T) {
				path, _, bad := corruptStartAtState(t, phase, tc.mutate)
				s, err := Open(path)
				if !errors.Is(err, ErrStateCorrupt) {
					if s != nil {
						s.Close()
					}
					t.Fatalf("Open err=%v, want ErrStateCorrupt", err)
				}
				if s != nil {
					t.Fatal("corrupt open must not return a usable store")
				}
				if err == nil || !strings.Contains(err.Error(), `voting proposal "gip-start"`) {
					t.Fatalf("error must name proposal gip-start, got: %v", err)
				}
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("error must distinguish %s (%q), got: %v", name, tc.want, err)
				}
				// 失败的打开不得改写原文件。
				if got, rerr := os.ReadFile(path); rerr != nil || string(got) != string(bad) {
					t.Fatalf("failed open rewrote or normalized the state file")
				}
			})
		}
	}
}

// TestMissingStartAtRejectsEarlyBallot：原本 100 开始、200 截止的提案缺失
// start_at，且票据中存在 50 时刻的首次投票时，绝不能把开始时间补成 0 后
// 认为该票据落在 [0,200) 窗口内而接受整份文件——必须以 start_at 缺失判
// 整份状态损坏，错误先于票据窗口核对报出。
func TestMissingStartAtRejectsEarlyBallot(t *testing.T) {
	path, _, _ := corruptStartAtState(t, "voting", func(p map[string]any) {
		delete(p, "start_at")
		// 再把 alice 的首次投票时间改到开始之前的 50：补零旧行为下它会落在
		// [0,200) 内显得完全合法。
		ballots := p["ballots"].([]any)
		ballots[0].(map[string]any)["voted_at"] = json.Number("50")
	})
	_, err := Open(path)
	if !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("Open err=%v, want ErrStateCorrupt", err)
	}
	if !strings.Contains(err.Error(), `voting proposal "gip-start"`) ||
		!strings.Contains(err.Error(), `field "start_at" is missing`) {
		t.Fatalf("error must name proposal and missing start_at before window checks, got: %v", err)
	}
}

// TestCorruptStartAtOpsRejectAndKeepFile：开始时间缺损后，查询不能给出该
// 提案或其它提案、余额的部分结果，投票不能追加票据，计票不能产生新结论，
// 执行不能转账；所有操作都返回状态损坏错误，原文件保持原样。
func TestCorruptStartAtOpsRejectAndKeepFile(t *testing.T) {
	path, good, bad := corruptStartAtState(t, "voting", func(p map[string]any) {
		delete(p, "start_at")
	})

	// 先恢复成完好文件，验证损坏前的正常语义：查询保留开始时间 100 与
	// alice 在 150 的首次票据。
	if err := os.WriteFile(path, good, 0o600); err != nil {
		t.Fatal(err)
	}
	live, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	v, ok, err := live.VoteProposal("gip-start")
	if err != nil || !ok || v.StartAt != 100 || len(v.Ballots) != 1 {
		t.Fatalf("view before corruption: ok=%v view=%+v err=%v", ok, v, err)
	}
	if v.Ballots[0].Representative != "alice" || v.Ballots[0].VotedAt != 150 {
		t.Fatalf("original ballot before corruption = %+v", v.Ballots[0])
	}
	live.Close()

	// 缺损文件直接打开：拒绝且不返回可用句柄。
	if err := os.WriteFile(path, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(path); !errors.Is(err, ErrStateCorrupt) || s != nil {
		if s != nil {
			s.Close()
		}
		t.Fatalf("Open corrupt: store=%v err=%v", s, err)
	}

	// 用一个真实打开在完好文件上的句柄，随后在句柄之外把文件改坏，
	// 验证每条操作路径都以同一份损坏判定拒绝。
	recoverPath := filepath.Join(t.TempDir(), "recover.json")
	if err := os.WriteFile(recoverPath, good, 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := Open(recoverPath)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if err := os.WriteFile(recoverPath, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	checks := map[string]func() error{
		"VoteProposal":      func() error { _, _, e := h.VoteProposal("gip-start"); return e },
		"VoteProposals":     func() error { _, e := h.VoteProposals(); return e },
		"ProposalsSnapshot": func() error { _, e := h.ProposalsSnapshot(); return e },
		"Proposal":          func() error { _, _, e := h.Proposal("gip-start"); return e },
		"Proposals":         func() error { _, e := h.Proposals(); return e },
		"Receipt":           func() error { _, _, e := h.Receipt("gip-start"); return e },
		"Receipts":          func() error { _, e := h.Receipts(); return e },
		"Balance":           func() error { _, e := h.Balance("audits"); return e },
		"Balances":          func() error { _, e := h.Balances(); return e },
		"BalanceSnapshot":   func() error { _, e := h.BalanceSnapshot(); return e },
		"TreasuryBalance":   func() error { _, e := h.TreasuryBalance(); return e },
		// 缺损文件上不得追加票据（alice 的重试也必须先撞上损坏判定）。
		"CastVote": func() error { _, e := h.CastVote("gip-start", "bob", true, 150); return e },
		// 到期计票不得产生新结论。
		"TallyVote": func() error { _, e := h.TallyVote("gip-start", 200); return e },
		// 执行不得转账。
		"Execute": func() error { _, e := h.Execute("gip-start", 300); return e },
	}
	for name, op := range checks {
		if e := op(); !errors.Is(e, ErrStateCorrupt) {
			t.Fatalf("%s after corruption err=%v, want ErrStateCorrupt", name, e)
		}
	}
	if got, rerr := os.ReadFile(recoverPath); rerr != nil || string(got) != string(bad) {
		t.Fatalf("failed ops rewrote or normalized the state file")
	}
}

// TestCorruptStartAtCLIUsesSameJudgement：普通文本与 --json 使用同一份判定：
// 失败时不向标准输出给出成功记录或部分结果，原因写入标准错误，退出码为 1。
func TestCorruptStartAtCLIUsesSameJudgement(t *testing.T) {
	binary := buildCLI(t)
	path, _, _ := corruptStartAtState(t, "voting", func(p map[string]any) {
		p["start_at"] = nil
	})
	commands := [][]string{
		{"proposal", "--id", "gip-start"},
		{"proposals"},
		{"vote", "--id", "gip-start", "--voter", "bob", "--choice", "for", "--now", "150"},
		{"tally", "--id", "gip-start", "--now", "200"},
		{"execute", "--id", "gip-start", "--now", "300"},
		{"balances"},
		{"receipts"},
	}
	for _, args := range commands {
		for _, asJSON := range []bool{false, true} {
			name := strings.Join(args, " ")
			full := args
			if asJSON {
				full = append(append([]string{}, full...), "--json")
				name += " --json"
			}
			so, se, code := runCLI(t, binary, path, full...)
			if code != 1 {
				t.Fatalf("%s: exit code=%d, want 1; stdout=%s stderr=%s", name, code, so, se)
			}
			if so != "" {
				t.Fatalf("%s: stdout must stay empty, got: %s", name, so)
			}
			if !strings.Contains(se, "corrupt") || !strings.Contains(se, "gip-start") ||
				!strings.Contains(se, "start_at") {
				t.Fatalf("%s: stderr must name corrupt state, proposal and start_at, got: %s", name, se)
			}
		}
	}
}

// TestCorruptStartAtSiblingProposalRejectsWholeFile：文件里即使还有另一项
// 内容完整（含明确 start_at）的提案，也不能只略过坏提案继续查询或投票——
// 整份文件判损坏。
func TestCorruptStartAtSiblingProposalRejectsWholeFile(t *testing.T) {
	path, good, bad := corruptStartAtState(t, "voting", func(p map[string]any) {
		delete(p, "start_at")
	})
	var doc map[string]any
	if err := json.Unmarshal(bad, &doc); err != nil {
		t.Fatal(err)
	}
	props := doc["vote_proposals"].(map[string]any)
	props["gip-ok"] = map[string]any{
		"id": "gip-ok", "state": "voting",
		"members":     []any{map[string]any{"id": "zoe", "weight": 1}},
		"delegations": []any{},
		"quorum":      1, "start_at": 0, "deadline": 200, "timelock_end": 300,
		"actions": []any{}, "ballots": []any{},
	}
	withSibling, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	withSibling = append(withSibling, '\n')
	if err := os.WriteFile(path, withSibling, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if !errors.Is(err, ErrStateCorrupt) || s != nil {
		if s != nil {
			s.Close()
		}
		t.Fatalf("Open with sibling good proposal: store=%v err=%v", s, err)
	}
	if !strings.Contains(err.Error(), `"gip-start"`) || !strings.Contains(err.Error(), "start_at") {
		t.Fatalf("error must still name the corrupt proposal, got: %v", err)
	}

	// 句柄打开在完好文件上，随后在文件内加入坏提案：即使查询/投票目标是
	// 完好的 gip-ok，也不得返回部分列表或写入新票。
	livePath := filepath.Join(t.TempDir(), "live.json")
	if err := os.WriteFile(livePath, good, 0o600); err != nil {
		t.Fatal(err)
	}
	live, err := Open(livePath)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if err := os.WriteFile(livePath, withSibling, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, qerr := live.VoteProposal("gip-ok"); !errors.Is(qerr, ErrStateCorrupt) {
		t.Fatalf("querying the intact sibling must still fail: ok=%v err=%v", ok, qerr)
	}
	if _, qerr := live.VoteProposals(); !errors.Is(qerr, ErrStateCorrupt) {
		t.Fatalf("listing proposals must not skip the bad one: err=%v", qerr)
	}
	if _, verr := live.CastVote("gip-ok", "zoe", true, 100); !errors.Is(verr, ErrStateCorrupt) {
		t.Fatalf("voting on the intact sibling must still fail: err=%v", verr)
	}
	if got, rerr := os.ReadFile(livePath); rerr != nil || string(got) != string(withSibling) {
		t.Fatalf("failed ops rewrote or normalized the state file")
	}
}

// TestExplicitZeroStartAtLegal：明确写出的开始时间 0 仍然合法。正常创建
// 开始 0、截止 1、时间锁 1 的提案后，查询必须保留 0；有资格的最终代表
// 在 0 首次投票被接受（票据时间保留 0），在 1 首次投票仍被拒绝；到期
// 计票得出首次结论，相同选择重试与改投冲突规则保持不变，重开后仍是 0。
func TestExplicitZeroStartAtLegal(t *testing.T) {
	store, path := openTempStore(t, 1000)
	in := &CreateVoteInput{
		ID:          "gip-zero",
		Members:     []VoteMember{{ID: "alice", Weight: 300}, {ID: "bob", Weight: 200}},
		Quorum:      250,
		StartAt:     0,
		Deadline:    1,
		TimelockEnd: 1,
		Actions:     []string{},
	}
	view, existed, err := store.CreateVoteProposal(in)
	if err != nil || existed || view == nil || view.StartAt != 0 {
		t.Fatalf("create start=0: existed=%v view=%+v err=%v", existed, view, err)
	}

	// 有资格的最终代表在 0 首次投票：必须接受，票据时间保留 0。
	b0, err := store.CastVote("gip-zero", "alice", true, 0)
	if err != nil || b0.Representative != "alice" || b0.VotedAt != 0 || !b0.Support || b0.Weight != 300 {
		t.Fatalf("first vote at now=0 must be accepted: %+v err=%v", b0, err)
	}
	// 另一名最终代表在 1 首次投票：截止时刻窗口已闭，必须拒绝。
	if _, err := store.CastVote("gip-zero", "bob", false, 1); !errors.Is(err, ErrVoteRejected) {
		t.Fatalf("first vote at now=1 must be rejected, got %v", err)
	}
	// 相同选择的重试始终返回首次记录（含 0 时刻），与时间无关。
	retry, err := store.CastVote("gip-zero", "alice", true, 1)
	if err != nil || retry.VotedAt != 0 {
		t.Fatalf("same-choice retry must return first record at 0: %+v err=%v", retry, err)
	}
	// 改投另一选择仍报冲突，不静默覆盖。
	if _, err := store.CastVote("gip-zero", "alice", false, 0); !errors.Is(err, ErrProposalConflict) {
		t.Fatalf("changing choice must conflict, got %v", err)
	}

	// 落盘记录必须明确写出 "start_at": 0 与 "voted_at": 0。
	store.Close()
	persisted, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(persisted), `"start_at": 0`) {
		t.Fatalf("persisted state must write explicit start_at 0:\n%s", persisted)
	}
	if !strings.Contains(string(persisted), `"voted_at": 0`) {
		t.Fatalf("persisted state must keep ballot voted_at 0:\n%s", persisted)
	}

	// 重开查询：开始时间与票据时间都保留 0。
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen start=0 proposal: %v", err)
	}
	defer reopened.Close()
	rv, ok, err := reopened.VoteProposal("gip-zero")
	if err != nil || !ok || rv.StartAt != 0 || rv.Deadline != 1 || rv.TimelockEnd != 1 {
		t.Fatalf("query after reopen: %+v ok=%v err=%v", rv, ok, err)
	}
	if len(rv.Ballots) != 1 || rv.Ballots[0].Representative != "alice" ||
		rv.Ballots[0].VotedAt != 0 || !rv.Ballots[0].Support {
		t.Fatalf("ballot at 0 must be preserved: %+v", rv.Ballots)
	}
	// 截止时刻计票得出首次结论（赞成 300 达到法定人数，通过），开始时间仍为 0。
	res, err := reopened.TallyVote("gip-zero", 1)
	if err != nil || !res.Passed || res.ForWeight != 300 {
		t.Fatalf("tally at deadline: %+v err=%v", res, err)
	}
	rv2, _, err := reopened.VoteProposal("gip-zero")
	if err != nil || rv2.StartAt != 0 || rv2.State != "passed" {
		t.Fatalf("query after tally must keep start_at 0: %+v err=%v", rv2, err)
	}
}

// TestPositiveStartAtPreservedAndGuardsWindow：正数开始时间必须原值保留，
// 开始之前不得接受首次投票；窗口边界（恰好等于开始）接受，相同选择重试与
// 改投冲突规则保持不变。
func TestPositiveStartAtPreservedAndGuardsWindow(t *testing.T) {
	store, path := openTempStore(t, 1000)
	in := &CreateVoteInput{
		ID:          "gip-pos",
		Members:     []VoteMember{{ID: "alice", Weight: 100}},
		Quorum:      1,
		StartAt:     100,
		Deadline:    200,
		TimelockEnd: 300,
		Actions:     []string{},
	}
	view, _, err := store.CreateVoteProposal(in)
	if err != nil || view.StartAt != 100 {
		t.Fatalf("create start=100: view=%+v err=%v", view, err)
	}
	// 开始之前的首次投票必须拒绝，不能落票。
	if _, err := store.CastVote("gip-pos", "alice", true, 99); !errors.Is(err, ErrVoteRejected) {
		t.Fatalf("vote before start must be rejected, got %v", err)
	}
	// 恰好等于开始时间的首次投票必须接受。
	b, err := store.CastVote("gip-pos", "alice", true, 100)
	if err != nil || b.VotedAt != 100 {
		t.Fatalf("first vote exactly at start: %+v err=%v", b, err)
	}
	// 相同选择的窗口内重试返回首次记录；改投报冲突。
	retry, err := store.CastVote("gip-pos", "alice", true, 150)
	if err != nil || retry.VotedAt != 100 {
		t.Fatalf("same-choice retry returns first record: %+v err=%v", retry, err)
	}
	if _, err := store.CastVote("gip-pos", "alice", false, 150); !errors.Is(err, ErrProposalConflict) {
		t.Fatalf("changing choice must conflict, got %v", err)
	}
	store.Close()

	persisted, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(persisted), `"start_at": 100`) {
		t.Fatalf("positive start_at must be persisted verbatim:\n%s", persisted)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	rv, ok, err := reopened.VoteProposal("gip-pos")
	if err != nil || !ok || rv.StartAt != 100 || len(rv.Ballots) != 1 || rv.Ballots[0].VotedAt != 100 {
		t.Fatalf("positive start and ballot preserved: %+v ok=%v err=%v", rv, ok, err)
	}
}

// TestStartAtLegacyFileWithoutVoteTable：旧版状态文件完全没有投票提案表时
// 继续可读；但只要保存了投票提案，缺少 start_at 就不能解释为旧版兼容。
func TestStartAtLegacyFileWithoutVoteTable(t *testing.T) {
	store, path := openTempStore(t, 500)
	store.Close()

	// 删掉整张 vote_proposals 表：旧格式文件照常可读。
	rewriteTopLevelField(t, path, func(doc map[string]any) { delete(doc, "vote_proposals") })
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("legacy file without vote table rejected: %v", err)
	}
	if bal, err := reopened.TreasuryBalance(); err != nil || bal != 500 {
		t.Fatalf("TreasuryBalance=%d err=%v", bal, err)
	}

	// 在旧文件上正常创建一项提案（明确写出 start_at），仍可读。
	in := &CreateVoteInput{
		ID:          "gip-legacy",
		Members:     []VoteMember{{ID: "alice", Weight: 1}},
		Quorum:      1,
		StartAt:     0,
		Deadline:    10,
		TimelockEnd: 10,
		Actions:     []string{},
	}
	if _, e, err := reopened.CreateVoteProposal(in); err != nil || e {
		t.Fatalf("create on legacy file: existed=%v err=%v", e, err)
	}
	reopened.Close()

	// 保存了投票提案却删去 start_at：不是旧版兼容，必须判损坏。
	rewriteTopLevelField(t, path, func(doc map[string]any) {
		p := doc["vote_proposals"].(map[string]any)["gip-legacy"].(map[string]any)
		delete(p, "start_at")
	})
	s, err := Open(path)
	if !errors.Is(err, ErrStateCorrupt) || s != nil {
		if s != nil {
			s.Close()
		}
		t.Fatalf("saved proposal without start_at must not be treated as legacy: store=%v err=%v", s, err)
	}
	if !strings.Contains(err.Error(), `"gip-legacy"`) ||
		!strings.Contains(err.Error(), `field "start_at" is missing`) {
		t.Fatalf("error must name proposal and missing start_at, got: %v", err)
	}
}
