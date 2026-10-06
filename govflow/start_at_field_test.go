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

// corruptStartAtState 构造一份含投票提案 gip-start 的合法状态文件（开始 100、
// 截止 200、时间锁 300），再按 mutate 改写该提案的 start_at，返回路径与改写
// 前后的字节。phase 决定提案停留的生命周期：
//   - "voting"：alice 已在 100 投赞成（300 权重），尚未计票；
//   - "passed"：alice 与 bob 均在 100 投赞成（合计 500）后已计票通过；
//   - "rejected"：无人投票直接计票，零参与权重拒绝；
//   - "executed"：通过提案已执行（资金库向 audits 转账 100）。
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
		if _, err := store.CastVote("gip-start", "alice", true, 100); err != nil {
			t.Fatal(err)
		}
	case "passed":
		if _, err := store.CastVote("gip-start", "alice", true, 100); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CastVote("gip-start", "bob", true, 100); err != nil {
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
		if _, err := store.CastVote("gip-start", "alice", true, 100); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CastVote("gip-start", "bob", true, 100); err != nil {
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
	// UseNumber：指数形式与超界整数以词面逐字落盘，不被 float64 折叠。
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

// TestCorruptStartAtRejectedAsCorrupt：每项已保存提案的 start_at 都必须明确
// 写出且为 int64 范围内的 JSON 整数。缺失、为 null 或写成字符串/布尔/对象/
// 数组/小数/指数/越界数字都判整份状态文件损坏，原因指出提案编号、start_at
// 以及缺失、空值或类型不符。尚在投票、已通过、被拒绝（零参与权重）与已执行
// 的提案适用同一条要求：即使票据、计票结论、执行凭据与余额都能与其余内容
// 核对一致，也不能把缺损的开始时间补成 0 后认可。
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
			mutate: func(p map[string]any) { p["start_at"] = []any{json.Number("100")} },
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

// TestMissingStartAtDoesNotExpandWindow：历史漏洞——开始 100、截止 200 的提案
// 遗漏 start_at 时被读成开始 0，查询显示开始时间 0，代表在 50 的首次投票也会
// 被接受，投票窗口被向前扩大。缺治理规则必须判整份状态文件损坏：即使把票据
// 首次投票时间改到 50（窗口被错误扩大后恰好落“在窗口内”），打开仍要失败，
// 原因落在 start_at 缺失，查询绝不能展示开始时间 0。
func TestMissingStartAtDoesNotExpandWindow(t *testing.T) {
	path, good, bad := corruptStartAtState(t, "voting", func(p map[string]any) {
		delete(p, "start_at")
		// alice 的首次投票改到 50：start_at 被误读成 0 时它恰好落在 [0,200)。
		ballot := p["ballots"].([]any)[0].(map[string]any)
		ballot["voted_at"] = json.Number("50")
	})

	// 缺损文件直接打开：拒绝且不返回可用句柄（查询绝不会看到开始时间 0）。
	s, err := Open(path)
	if !errors.Is(err, ErrStateCorrupt) || s != nil {
		if s != nil {
			s.Close()
		}
		t.Fatalf("Open corrupt: store=%v err=%v", s, err)
	}
	if !strings.Contains(err.Error(), `voting proposal "gip-start"`) ||
		!strings.Contains(err.Error(), `field "start_at" is missing`) {
		t.Fatalf("error must name proposal and missing start_at, got: %v", err)
	}

	// 完好文件先正常打开，随后在句柄之外把文件改成缺损版本：开始之前（50）
	// 的投票不得借扩大的窗口被接受，查询与计票都以同一份损坏判定拒绝。
	livePath := filepath.Join(t.TempDir(), "live.json")
	if err := os.WriteFile(livePath, good, 0o600); err != nil {
		t.Fatal(err)
	}
	live, err := Open(livePath)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if err := os.WriteFile(livePath, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	if v, ok, qerr := live.VoteProposal("gip-start"); !errors.Is(qerr, ErrStateCorrupt) {
		t.Fatalf("query must not show start_at 0 from corrupt file: view=%+v ok=%v err=%v", v, ok, qerr)
	}
	// bob 在 50 的首次投票：缺损文件上必须报损坏，不能追加票据。
	if b, verr := live.CastVote("gip-start", "bob", true, 50); !errors.Is(verr, ErrStateCorrupt) {
		t.Fatalf("pre-start vote on corrupt file: ballot=%+v err=%v, want ErrStateCorrupt", b, verr)
	}
	// alice 的相同选择重试同样先读状态：不得返回“窗口内”的首次记录。
	if b, verr := live.CastVote("gip-start", "alice", true, 50); !errors.Is(verr, ErrStateCorrupt) {
		t.Fatalf("retry on corrupt file: ballot=%+v err=%v, want ErrStateCorrupt", b, verr)
	}
	if _, terr := live.TallyVote("gip-start", 200); !errors.Is(terr, ErrStateCorrupt) {
		t.Fatalf("tally on corrupt file err=%v, want ErrStateCorrupt", terr)
	}
	if got, rerr := os.ReadFile(livePath); rerr != nil || string(got) != string(bad) {
		t.Fatalf("failed ops rewrote or normalized the state file")
	}
}

// TestCorruptStartAtOpsRejectAndKeepFile：start_at 缺损后，查询不得返回其他
// 完整提案或余额的部分结果，投票不得追加票据，计票不得产生新结论，执行不得
// 转账；所有操作都返回状态损坏错误，原文件保持原样。文件里另有一项完整提案
// 也不能只略过坏提案。
func TestCorruptStartAtOpsRejectAndKeepFile(t *testing.T) {
	path, good, bad := corruptStartAtState(t, "voting", func(p map[string]any) {
		p["start_at"] = nil
	})

	// 先验证损坏前的正常语义：查询保留开始时间 100，窗口前投票被拒、窗口内接受。
	livePath := filepath.Join(t.TempDir(), "live.json")
	if err := os.WriteFile(livePath, good, 0o600); err != nil {
		t.Fatal(err)
	}
	live, err := Open(livePath)
	if err != nil {
		t.Fatal(err)
	}
	v, ok, err := live.VoteProposal("gip-start")
	if err != nil || !ok || v.StartAt != 100 {
		t.Fatalf("view before corruption: ok=%v view=%+v err=%v", ok, v, err)
	}
	if _, verr := live.CastVote("gip-start", "bob", true, 99); !errors.Is(verr, ErrVoteRejected) {
		t.Fatalf("vote at 99 before start err=%v, want ErrVoteRejected", verr)
	}
	live.Close()
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
	if s, oerr := Open(path); !errors.Is(oerr, ErrStateCorrupt) || s != nil {
		if s != nil {
			s.Close()
		}
		t.Fatalf("Open with sibling good proposal: store=%v err=%v", s, oerr)
	}

	// 句柄打开在完好文件上，随后替换成含坏提案的版本：即使查询/投票目标是
	// 完好的 gip-ok，也不得返回部分列表、余额或写入新票。
	h, err := Open(livePath)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if err := os.WriteFile(livePath, withSibling, 0o600); err != nil {
		t.Fatal(err)
	}
	checks := map[string]func() error{
		"VoteProposal bad":   func() error { _, _, e := h.VoteProposal("gip-start"); return e },
		"VoteProposal ok":    func() error { _, _, e := h.VoteProposal("gip-ok"); return e },
		"VoteProposals":      func() error { _, e := h.VoteProposals(); return e },
		"ProposalsSnapshot":  func() error { _, e := h.ProposalsSnapshot(); return e },
		"Proposal":           func() error { _, _, e := h.Proposal("gip-start"); return e },
		"Proposals":          func() error { _, e := h.Proposals(); return e },
		"Receipt":            func() error { _, _, e := h.Receipt("gip-start"); return e },
		"Receipts":           func() error { _, e := h.Receipts(); return e },
		"Balance":            func() error { _, e := h.Balance("audits"); return e },
		"Balances":           func() error { _, e := h.Balances(); return e },
		"BalanceSnapshot":    func() error { _, e := h.BalanceSnapshot(); return e },
		"TreasuryBalance":    func() error { _, e := h.TreasuryBalance(); return e },
		"CastVote bad":       func() error { _, e := h.CastVote("gip-start", "bob", true, 150); return e },
		"CastVote sibling":   func() error { _, e := h.CastVote("gip-ok", "zoe", true, 100); return e },
		"TallyVote":          func() error { _, e := h.TallyVote("gip-start", 200); return e },
		"Execute bad state":  func() error { _, e := h.Execute("gip-start", 300); return e },
		"Execute registered": func() error { _, e := h.Execute("gip-other", 0); return e },
	}
	for name, op := range checks {
		if e := op(); !errors.Is(e, ErrStateCorrupt) {
			t.Fatalf("%s after corruption err=%v, want ErrStateCorrupt", name, e)
		}
	}
	if got, rerr := os.ReadFile(livePath); rerr != nil || string(got) != string(withSibling) {
		t.Fatalf("failed ops rewrote or normalized the state file")
	}
}

// TestCorruptStartAtCLIUsesSameJudgement：命令行普通输出与 --json 使用同一
// 份判定：失败时不向标准输出给出成功记录或部分结果，原因写入标准错误（能
// 看出提案编号与 start_at），退出码为 1。
func TestCorruptStartAtCLIUsesSameJudgement(t *testing.T) {
	binary := buildCLI(t)
	path, _, _ := corruptStartAtState(t, "voting", func(p map[string]any) {
		delete(p, "start_at")
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

// TestExplicitZeroStartAtLegal：明确写出的开始时间 0 仍然合法。正常创建
// start=0、deadline=1、timelock=1 的提案后，查询保留 0；有资格的最终代表
// 在 0 首次投票被接受（voted_at 保留 0），在 1（截止时刻）首次投票仍被拒绝。
// 相同选择重试返回首次记录的既有规则不变。
func TestExplicitZeroStartAtLegal(t *testing.T) {
	store, path := openTempStore(t, 1000)
	in := &CreateVoteInput{
		ID:          "gip-zero-start",
		Members:     []VoteMember{{ID: "alice", Weight: 1}, {ID: "bob", Weight: 1}},
		Quorum:      1,
		StartAt:     0,
		Deadline:    1,
		TimelockEnd: 1,
		Actions:     []string{},
	}
	view := mustCreateVote(t, store, in)
	if view.StartAt != 0 {
		t.Fatalf("created view start_at=%d, want 0", view.StartAt)
	}
	store.Close()

	// 落盘记录必须明确写出 0，而不是省略 start_at。
	persisted, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(persisted), `"start_at": 0`) {
		t.Fatalf("persisted state must write explicit start_at 0:\n%s", persisted)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen explicit zero start: %v", err)
	}
	defer reopened.Close()
	v, ok, err := reopened.VoteProposal("gip-zero-start")
	if err != nil || !ok || v.StartAt != 0 || v.Deadline != 1 || v.TimelockEnd != 1 {
		t.Fatalf("query must preserve start_at 0: ok=%v view=%+v err=%v", ok, v, err)
	}
	// 最终代表在 0 的首次投票被接受，首次投票时间保留 0。
	b, err := reopened.CastVote("gip-zero-start", "alice", true, 0)
	if err != nil || b.VotedAt != 0 || b.Representative != "alice" {
		t.Fatalf("first vote at 0 must be accepted: %+v err=%v", b, err)
	}
	// 另一代表在截止时刻 1 的首次投票仍被拒绝（窗口为 [0,1)）。
	if b2, verr := reopened.CastVote("gip-zero-start", "bob", true, 1); !errors.Is(verr, ErrVoteRejected) {
		t.Fatalf("first vote at deadline 1 ballot=%+v err=%v, want ErrVoteRejected", b2, verr)
	}
	// 相同选择的重试不受窗口影响，始终返回首次记录（voted_at 仍为 0）。
	retry, rerr := reopened.CastVote("gip-zero-start", "alice", true, 1)
	if rerr != nil || retry.VotedAt != 0 {
		t.Fatalf("same-choice retry must return first record: %+v err=%v", retry, rerr)
	}
	// 改投报冲突，既有规则不变。
	if _, rerr := reopened.CastVote("gip-zero-start", "alice", false, 0); !errors.Is(rerr, ErrProposalConflict) {
		t.Fatalf("changing choice must conflict, got %v", rerr)
	}

	// 重开文件：开始时间 0 与 0 时刻票据原样保留。
	again, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after votes: %v", err)
	}
	defer again.Close()
	v, ok, err = again.VoteProposal("gip-zero-start")
	if err != nil || !ok || v.StartAt != 0 || len(v.Ballots) != 1 || v.Ballots[0].VotedAt != 0 {
		t.Fatalf("reopen query: ok=%v view=%+v err=%v", ok, v, err)
	}
}

// TestPositiveStartAtPreservedAndWindowEnforced：正数开始时间必须原值保留，
// 开始之前不能接受首次投票，窗口边界（start 可投、deadline 不可投）不变。
func TestPositiveStartAtPreservedAndWindowEnforced(t *testing.T) {
	store, path := openTempStore(t, 1000)
	in := &CreateVoteInput{
		ID:          "gip-pos-start",
		Members:     []VoteMember{{ID: "alice", Weight: 300}, {ID: "bob", Weight: 200}},
		Quorum:      250,
		StartAt:     100,
		Deadline:    200,
		TimelockEnd: 300,
	}
	mustCreateVote(t, store, in)

	// 开始之前（99）的首次投票被拒。
	if _, err := store.CastVote("gip-pos-start", "alice", true, 99); !errors.Is(err, ErrVoteRejected) {
		t.Fatalf("vote before start err=%v, want ErrVoteRejected", err)
	}
	// 开始时刻（100）的首次投票被接受，时间原值保留。
	b, err := store.CastVote("gip-pos-start", "alice", true, 100)
	if err != nil || b.VotedAt != 100 {
		t.Fatalf("first vote at start 100: %+v err=%v", b, err)
	}
	store.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	v, ok, err := reopened.VoteProposal("gip-pos-start")
	if err != nil || !ok || v.StartAt != 100 {
		t.Fatalf("positive start_at must be preserved as 100: ok=%v view=%+v err=%v", ok, v, err)
	}
	// 相同选择重试返回首次记录（即使传入不同时间）；改投仍冲突。
	retry, rerr := reopened.CastVote("gip-pos-start", "alice", true, 150)
	if rerr != nil || retry.VotedAt != 100 {
		t.Fatalf("same-choice retry returns first record: %+v err=%v", retry, rerr)
	}
	if _, rerr := reopened.CastVote("gip-pos-start", "alice", false, 150); !errors.Is(rerr, ErrProposalConflict) {
		t.Fatalf("changing choice must conflict, got %v", rerr)
	}
	// 未投票代表在截止时刻仍被拒。
	if _, rerr := reopened.CastVote("gip-pos-start", "bob", true, 200); !errors.Is(rerr, ErrVoteRejected) {
		t.Fatalf("first vote at deadline 200 err=%v, want ErrVoteRejected", rerr)
	}
}

// TestStartAtLegacyTableAbsenceStillReadable：旧版状态文件完全没有投票提案
// 表时继续可读；但只要文件里保存了投票提案，其缺少 start_at 就不能被解释成
// 旧版兼容，必须判整份损坏。类型合法的整数仍须满足 0 <= start < deadline <=
// timelock，违反时序同样拒绝。
func TestStartAtLegacyTableAbsenceStillReadable(t *testing.T) {
	dir := t.TempDir()

	// 旧版文件：没有 vote_proposals 表，继续可读。
	oldPath := filepath.Join(dir, "old.json")
	old := `{
  "magic": "govflow-treasury-state",
  "version": 1,
  "initial_treasury": 500,
  "treasury": 500,
  "balances": {},
  "proposals": {},
  "receipts": []
}
`
	if err := os.WriteFile(oldPath, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	oldStore, err := Open(oldPath)
	if err != nil {
		t.Fatalf("legacy file without vote_proposals table rejected: %v", err)
	}
	if bal, err := oldStore.TreasuryBalance(); err != nil || bal != 500 {
		t.Fatalf("legacy treasury=%d err=%v", bal, err)
	}
	vs, err := oldStore.VoteProposals()
	if err != nil || len(vs) != 0 {
		t.Fatalf("legacy vote proposals=%v err=%v", vs, err)
	}
	oldStore.Close()

	// 保存了投票提案却缺 start_at：不是旧版兼容，整份损坏。
	missingPath := filepath.Join(dir, "missing-start.json")
	missingStart := `{
  "magic": "govflow-treasury-state",
  "version": 1,
  "initial_treasury": 1000,
  "treasury": 1000,
  "balances": {},
  "proposals": {},
  "receipts": [],
  "vote_proposals": {
    "gip-legacy?": {
      "id": "gip-legacy?",
      "state": "voting",
      "members": [{"id": "alice", "weight": 300}],
      "delegations": [],
      "quorum": 1,
      "deadline": 200,
      "timelock_end": 300,
      "actions": [],
      "ballots": []
    }
  }
}
`
	if err := os.WriteFile(missingPath, []byte(missingStart), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(missingPath); !errors.Is(err, ErrStateCorrupt) {
		if s != nil {
			s.Close()
		}
		t.Fatalf("saved proposal without start_at err=%v, want ErrStateCorrupt", err)
	} else if !strings.Contains(err.Error(), `field "start_at" is missing`) {
		t.Fatalf("error must name missing start_at, got: %v", err)
	}

	// 类型合法但违反 0 <= start < deadline <= timelock：整数合法不放宽时序规则。
	badOrderPath := filepath.Join(dir, "bad-order.json")
	badOrder := strings.Replace(missingStart, `"deadline": 200`,
		`"start_at": 200,
      "deadline": 200`, 1)
	if err := os.WriteFile(badOrderPath, []byte(badOrder), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(badOrderPath); !errors.Is(err, ErrStateCorrupt) {
		if s != nil {
			s.Close()
		}
		t.Fatalf("start == deadline err=%v, want ErrStateCorrupt", err)
	} else if !strings.Contains(err.Error(), "times must satisfy 0 <= start < deadline <= timelock") {
		t.Fatalf("error must name the time ordering rule, got: %v", err)
	}
}
