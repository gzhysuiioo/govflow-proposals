package govflow

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// corruptBallotsState 构造一份含投票提案 gip-bal 的合法状态文件，
// 再按 mutate 改写该提案的 ballots 字段，返回改写前后的字节与路径。
// phase 决定提案停留在哪个生命周期：
//   - "voting"：alice 已投赞成（300 权重，100 时刻），尚未计票；
//   - "passed"：alice 与 bob 均投赞成（合计 500）后已计票通过；
//   - "rejected"：无人投票直接计票，零参与权重拒绝；
//   - "executed"：通过提案已执行（资金库向 audits 转账 100）。
func corruptBallotsState(t *testing.T, phase string, mutate func(p map[string]any)) (path string, good, bad []byte) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "treasury.json")
	store, err := InitTreasury(path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	in := &CreateVoteInput{
		ID: "gip-bal",
		Members: []VoteMember{
			{ID: "alice", Weight: 300},
			{ID: "bob", Weight: 200},
		},
		Quorum:      250,
		StartAt:     0,
		Deadline:    200,
		TimelockEnd: 300,
		Actions:     []string{"transfer:audits:100"},
	}
	mustCreateVote(t, store, in)
	switch phase {
	case "voting":
		if _, err := store.CastVote("gip-bal", "alice", true, 100); err != nil {
			t.Fatal(err)
		}
	case "passed":
		if _, err := store.CastVote("gip-bal", "alice", true, 100); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CastVote("gip-bal", "bob", true, 100); err != nil {
			t.Fatal(err)
		}
		if r, err := store.TallyVote("gip-bal", 200); err != nil || !r.Passed {
			t.Fatalf("tally passed: %+v err=%v", r, err)
		}
	case "rejected":
		if r, err := store.TallyVote("gip-bal", 200); err != nil || r.Passed {
			t.Fatalf("tally rejected: %+v err=%v", r, err)
		}
	case "executed":
		if _, err := store.CastVote("gip-bal", "alice", true, 100); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CastVote("gip-bal", "bob", true, 100); err != nil {
			t.Fatal(err)
		}
		if _, err := store.TallyVote("gip-bal", 200); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Execute("gip-bal", 300); err != nil {
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
	var clone map[string]any
	if err := json.Unmarshal(good, &clone); err != nil {
		t.Fatal(err)
	}
	p := clone["vote_proposals"].(map[string]any)["gip-bal"].(map[string]any)
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

// TestCorruptBallotsRejectedAsCorrupt：ballots 缺失、为 null 或写成
// 对象/字符串/数字/布尔/元素类型不对的数组，都必须判整份状态文件损坏，
// 且原因指出提案编号与 ballots，并区分缺失、空值与类型不符。
// 尚在投票、已通过、被拒绝（零参与权重）与已执行的提案适用同一条要求：
// 不能因为计票汇总恰好为零或资金余额仍能与执行凭据对应，就把缺损列表
// 补成空数组。
func TestCorruptBallotsRejectedAsCorrupt(t *testing.T) {
	mutations := map[string]struct {
		mutate func(p map[string]any)
		want   string
	}{
		"missing": {
			mutate: func(p map[string]any) { delete(p, "ballots") },
			want:   `field "ballots" is missing`,
		},
		"null": {
			mutate: func(p map[string]any) { p["ballots"] = nil },
			want:   `field "ballots" is null`,
		},
		"object": {
			mutate: func(p map[string]any) { p["ballots"] = map[string]any{"alice": "for"} },
			want:   `field "ballots" has wrong type: want array, got object`,
		},
		"string": {
			mutate: func(p map[string]any) { p["ballots"] = "alice:for" },
			want:   `field "ballots" has wrong type: want array, got string`,
		},
		"number": {
			mutate: func(p map[string]any) { p["ballots"] = 7 },
			want:   `field "ballots" has wrong type: want array, got number`,
		},
		"boolean": {
			mutate: func(p map[string]any) { p["ballots"] = true },
			want:   `field "ballots" has wrong type: want array, got boolean`,
		},
		"wrong element": {
			mutate: func(p map[string]any) { p["ballots"] = []any{5} },
			want:   `field "ballots" has wrong type`,
		},
	}
	for _, phase := range []string{"voting", "passed", "rejected", "executed"} {
		for name, tc := range mutations {
			t.Run(phase+"/"+name, func(t *testing.T) {
				path, _, bad := corruptBallotsState(t, phase, tc.mutate)
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
				if err == nil || !strings.Contains(err.Error(), `voting proposal "gip-bal"`) {
					t.Fatalf("error must name proposal gip-bal, got: %v", err)
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

// TestCorruptBallotsOpsRejectAndKeepFile：票据列表缺损后，查询不得返回空
// 票据或其它提案的部分结果，投票不得追加新票（已投代表不能借缺损列表
// 再次投票），计票不得产生新结论，执行不得继续转账；所有操作都返回状态
// 损坏错误，原文件保持原样。
func TestCorruptBallotsOpsRejectAndKeepFile(t *testing.T) {
	path, good, bad := corruptBallotsState(t, "voting", func(p map[string]any) {
		delete(p, "ballots")
	})

	// 先恢复成完好文件，验证损坏前的正常语义：查询能看到 alice 的首次票据。
	if err := os.WriteFile(path, good, 0o600); err != nil {
		t.Fatal(err)
	}
	live, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	v, ok, err := live.VoteProposal("gip-bal")
	if err != nil || !ok || len(v.Ballots) != 1 {
		t.Fatalf("view before corruption: ok=%v ballots=%+v err=%v", ok, v, err)
	}
	if v.Ballots[0].Representative != "alice" || v.Ballots[0].Weight != 300 ||
		!v.Ballots[0].Support || v.Ballots[0].VotedAt != 100 {
		t.Fatalf("original ballot before corruption = %+v", v.Ballots[0])
	}
	live.Close()

	// 缺损文件直接打开：拒绝且不返回可用句柄（查询绝不会看到空票据）。
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
		"VoteProposal":      func() error { _, _, e := h.VoteProposal("gip-bal"); return e },
		"VoteProposals":     func() error { _, e := h.VoteProposals(); return e },
		"ProposalsSnapshot": func() error { _, e := h.ProposalsSnapshot(); return e },
		"Proposal":          func() error { _, _, e := h.Proposal("gip-bal"); return e },
		"Proposals":         func() error { _, e := h.Proposals(); return e },
		"Receipt":           func() error { _, _, e := h.Receipt("gip-bal"); return e },
		"Receipts":          func() error { _, e := h.Receipts(); return e },
		"Balance":           func() error { _, e := h.Balance("audits"); return e },
		"Balances":          func() error { _, e := h.Balances(); return e },
		"BalanceSnapshot":   func() error { _, e := h.BalanceSnapshot(); return e },
		"TreasuryBalance":   func() error { _, e := h.TreasuryBalance(); return e },
		// 已投过票的 alice 在缺损文件上绝不能被当成“尚未投票”而再次投出。
		"CastVote": func() error { _, e := h.CastVote("gip-bal", "alice", true, 100); return e },
		// 到期计票绝不能按零参与权重给出拒绝结论。
		"TallyVote": func() error { _, e := h.TallyVote("gip-bal", 200); return e },
		// 即使文件被改成 passed 类缺损，执行也不能继续转账（本变体为 voting，
		// 这里同样断言损坏判定先于状态判断）。
		"Execute": func() error { _, e := h.Execute("gip-bal", 300); return e },
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

// TestCorruptBallotsCLIUsesSameJudgement：命令行普通输出与 --json 使用同
// 一份判定：失败时不向标准输出给出成功记录或部分结果，原因写入标准错误，
// 退出码为 1。
func TestCorruptBallotsCLIUsesSameJudgement(t *testing.T) {
	binary := buildCLI(t)
	path, _, _ := corruptBallotsState(t, "voting", func(p map[string]any) {
		p["ballots"] = nil
	})
	commands := [][]string{
		{"proposal", "--id", "gip-bal"},
		{"proposals"},
		{"vote", "--id", "gip-bal", "--voter", "alice", "--choice", "for", "--now", "100"},
		{"tally", "--id", "gip-bal", "--now", "200"},
		{"execute", "--id", "gip-bal", "--now", "300"},
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
			if !strings.Contains(se, "corrupt") || !strings.Contains(se, "gip-bal") ||
				!strings.Contains(se, "ballots") {
				t.Fatalf("%s: stderr must name corrupt state, proposal and ballots, got: %s", name, se)
			}
		}
	}
}

// TestCorruptBallotsSiblingProposalRejectsWholeFile：文件里即使还有另一项
// 内容完整的提案，也不能只略过坏提案继续查询或投票——整份文件判损坏。
func TestCorruptBallotsSiblingProposalRejectsWholeFile(t *testing.T) {
	path, good, bad := corruptBallotsState(t, "voting", func(p map[string]any) {
		delete(p, "ballots")
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
	if !strings.Contains(err.Error(), `"gip-bal"`) || !strings.Contains(err.Error(), "ballots") {
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

// TestExplicitEmptyBallotsLegal：明确写出的空数组仍表示确实无人投票。
// 正常创建提案后应可直接查询并在窗口内接受首次投票；无人投票时到期计票
// 得到赞成、反对与参与权重均为零的拒绝结论，该记录之后仍可正常查询。
func TestExplicitEmptyBallotsLegal(t *testing.T) {
	t.Run("handwritten empty array accepts first vote", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "treasury.json")
		raw := `{
  "magic": "govflow-treasury-state",
  "version": 1,
  "initial_treasury": 1000,
  "treasury": 1000,
  "balances": {},
  "proposals": {},
  "receipts": [],
  "vote_proposals": {
    "gip-empty": {
      "id": "gip-empty",
      "state": "voting",
      "members": [{"id": "alice", "weight": 300}, {"id": "bob", "weight": 200}],
      "delegations": [],
      "quorum": 250,
      "start_at": 0,
      "deadline": 200,
      "timelock_end": 300,
      "actions": [],
      "ballots": []
    }
  }
}
`
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		store, err := Open(path)
		if err != nil {
			t.Fatalf("empty ballots array rejected: %v", err)
		}
		defer store.Close()
		v, ok, err := store.VoteProposal("gip-empty")
		if err != nil || !ok || len(v.Ballots) != 0 {
			t.Fatalf("query empty ballots: ok=%v ballots=%+v err=%v", ok, v, err)
		}
		// 窗口内的首次投票必须被接受。
		b, err := store.CastVote("gip-empty", "bob", false, 100)
		if err != nil || b.Representative != "bob" || b.Weight != 200 || b.Support || b.VotedAt != 100 {
			t.Fatalf("first vote into empty list: %+v err=%v", b, err)
		}
		store.Close()
		reopened, err := Open(path)
		if err != nil {
			t.Fatalf("reopen after first vote: %v", err)
		}
		defer reopened.Close()
		rv, ok, err := reopened.VoteProposal("gip-empty")
		if err != nil || !ok || len(rv.Ballots) != 1 {
			t.Fatalf("reopen query: %+v ok=%v err=%v", rv, ok, err)
		}
		got := rv.Ballots[0]
		if got.Representative != "bob" || got.Weight != 200 || got.Support || got.VotedAt != 100 {
			t.Fatalf("first ballot must keep order, representative, weight, choice and first time: %+v", got)
		}
		// 相同选择重试返回首次记录；改投报冲突，既有规则不因空数组来源放宽。
		if retry, rerr := reopened.CastVote("gip-empty", "bob", false, 150); rerr != nil ||
			retry.VotedAt != 100 {
			t.Fatalf("same-choice retry must return first record: %+v err=%v", retry, rerr)
		}
		if _, rerr := reopened.CastVote("gip-empty", "bob", true, 100); !errors.Is(rerr, ErrProposalConflict) {
			t.Fatalf("changing choice must conflict, got %v", rerr)
		}
	})

	t.Run("create then zero-turnout tally still queryable", func(t *testing.T) {
		// 从不含整张投票提案表的旧版状态文件起步：旧文件仍可读。
		dir := t.TempDir()
		path := filepath.Join(dir, "treasury.json")
		old := `{
  "magic": "govflow-treasury-state",
  "version": 1,
  "initial_treasury": 1000,
  "treasury": 1000,
  "balances": {},
  "proposals": {},
  "receipts": []
}
`
		if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
			t.Fatal(err)
		}
		store, err := Open(path)
		if err != nil {
			t.Fatalf("old state file rejected: %v", err)
		}
		in := &CreateVoteInput{
			ID:       "gip-novote",
			Members:  []VoteMember{{ID: "alice", Weight: 300}, {ID: "bob", Weight: 200}},
			Quorum:   250,
			StartAt:  0,
			Deadline: 200, TimelockEnd: 300,
			Actions: []string{},
		}
		if v, existed, err := store.CreateVoteProposal(in); err != nil || existed || (v != nil && len(v.Ballots) != 0) {
			t.Fatalf("create: existed=%v err=%v view=%+v", existed, err, v)
		}
		store.Close()

		// 落盘记录必须明确写出空数组，而不是省略 ballots。
		persisted, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(persisted), `"ballots": []`) {
			t.Fatalf("persisted state must write explicit empty ballots array:\n%s", persisted)
		}
		reopened, err := Open(path)
		if err != nil {
			t.Fatalf("reopen after create: %v", err)
		}
		defer reopened.Close()
		// 无人投票到期计票：赞成、反对、参与权重均为零的拒绝结论。
		res, err := reopened.TallyVote("gip-novote", 200)
		if err != nil {
			t.Fatalf("zero-turnout tally: %v", err)
		}
		if res.Passed || res.ForWeight != 0 || res.AgainstWeight != 0 || res.Turnout != 0 {
			t.Fatalf("zero-turnout tally = %+v, want rejected with all-zero weights", res)
		}
		// 拒绝后记录仍可正常查询，结论与首次计票一致。
		v, ok, err := reopened.VoteProposal("gip-novote")
		if err != nil || !ok || v.State != "rejected" || len(v.Ballots) != 0 {
			t.Fatalf("query after zero tally: %+v ok=%v err=%v", v, ok, err)
		}
		if v.Tally == nil || v.Tally.ForWeight != 0 || v.Tally.AgainstWeight != 0 ||
			v.Tally.Turnout != 0 || v.Tally.Passed {
			t.Fatalf("tally view after zero-turnout rejection: %+v", v.Tally)
		}
		reopened.Close()
		if s, err := Open(path); err != nil || s == nil {
			if s != nil {
				s.Close()
			}
			t.Fatalf("rejected zero-turnout record must stay readable: %v", err)
		} else {
			s.Close()
		}
	})
}
