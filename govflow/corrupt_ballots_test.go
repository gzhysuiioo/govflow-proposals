package govflow

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件回归“票据列表是必填治理留痕”在读取路径上的判定：已保存的投票提案
// 必须明确写出 ballots 且它是 JSON 数组。字段缺失、为 null 或写成对象、
// 字符串、数字、布尔值，都判整份状态文件损坏——缺损列表不能被当成“无人
// 投票”，否则尚在投票中的提案会被展示成空票据、已投过票的代表还能再次
// 投票，到期计票则按零参与权重给出拒绝结论，原有选择与首次投票时间全部
// 丢失。明确写出的空数组仍表示确实无人投票。

// corruptBallotsState 构造一份含“bob 委托给 alice”的合法状态文件（alice
// 一旦投票即携带归集权重 500），再按 mutate 改写该提案的 ballots 字段，
// 返回改写前后的字节与路径。phase 决定提案停留在哪个生命周期：
//   - "voting"：alice 已在窗口内投出一张赞成票，但尚未计票（删除 ballots
//     会丢失这张票的选择与首次投票时间）；
//   - "passed"：alice（归集 500）投赞成后已计票通过；
//   - "rejected"：无人投票直接计票，两侧权重为零、未达法定人数；
//   - "executed"：通过提案已执行，资金余额与执行凭据完全对应。
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
		Delegations: []Delegation{{From: "bob", To: "alice"}},
		Quorum:      250,
		StartAt:     0,
		Deadline:    200,
		TimelockEnd: 300,
		Actions:     []string{"transfer:audits:100"},
	}
	mustCreateVote(t, store, in)
	switch phase {
	case "voting":
		// 已投出一张票：缺损列表会丢失该代表的选择与首次投票时间，且让该
		// 代表看起来还能“再次投首次票”。
		if _, err := store.CastVote("gip-bal", "alice", true, 100); err != nil {
			t.Fatal(err)
		}
	case "passed":
		if _, err := store.CastVote("gip-bal", "alice", true, 100); err != nil {
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

// TestCorruptBallotsRejectedAsCorrupt：ballots 缺失、为 null 或写成对象/
// 字符串/数字/布尔/元素类型不对的数组，都必须判整份状态文件损坏，且原因
// 指出提案编号与 ballots，并区分缺失、空值与类型不符。尚在投票、已通过、
// 已拒绝（计票汇总恰好为零）与已执行（余额与凭据对应）的提案适用同一条
// 要求。
func TestCorruptBallotsRejectedAsCorrupt(t *testing.T) {
	mutations := map[string]struct {
		mutate func(p map[string]any)
		want   string // 错误原因中必须包含的片段
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
			mutate: func(p map[string]any) { p["ballots"] = map[string]any{"alice": true} },
			want:   `field "ballots" has wrong type: want array, got object`,
		},
		"string": {
			mutate: func(p map[string]any) { p["ballots"] = "[]" },
			want:   `field "ballots" has wrong type: want array, got string`,
		},
		"number": {
			mutate: func(p map[string]any) { p["ballots"] = 0 },
			want:   `field "ballots" has wrong type: want array, got number`,
		},
		"boolean": {
			mutate: func(p map[string]any) { p["ballots"] = false },
			want:   `field "ballots" has wrong type: want array, got boolean`,
		},
		"wrong element": {
			mutate: func(p map[string]any) { p["ballots"] = []any{5} },
			want:   `field "ballots" has wrong type`,
		},
		"string element": {
			mutate: func(p map[string]any) { p["ballots"] = []any{"alice"} },
			want:   `field "ballots" has wrong type`,
		},
	}
	// null 元素时 ballots 字段本身存在且是数组，逐票校验已以带提案编号与
	// 票据下标的原因拒绝（"representative" is missing）；这里只要求同样判
	// 整份文件损坏且能定位到提案，与空动作凭据用例对 null 元素的断言一致。
	nullElement := func(p map[string]any) { p["ballots"] = []any{nil} }
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
				// 失败的打开不得改写原文件：不补空数组、不重计票、不回退状态。
				if got, rerr := os.ReadFile(path); rerr != nil || string(got) != string(bad) {
					t.Fatalf("failed open rewrote or normalized the state file")
				}
				// 反复打开结论一致，不会第二次“修复”或返回可用句柄。
				if again, aerr := Open(path); again != nil || !errors.Is(aerr, ErrStateCorrupt) {
					again.Close()
					t.Fatalf("second Open store=%v err=%v, want nil + ErrStateCorrupt", again, aerr)
				}
			})
		}
		// null 元素：字段在、数组在，但元素不是票据对象；逐票校验同样判
		// 整份文件损坏，错误仍能看出提案编号与票据下标。
		t.Run(phase+"/null element", func(t *testing.T) {
			path, _, bad := corruptBallotsState(t, phase, nullElement)
			s, err := Open(path)
			if !errors.Is(err, ErrStateCorrupt) || s != nil {
				if s != nil {
					s.Close()
				}
				t.Fatalf("Open err=%v, want nil + ErrStateCorrupt", err)
			}
			if !strings.Contains(err.Error(), `voting proposal "gip-bal"`) ||
				!strings.Contains(err.Error(), "ballot 0") {
				t.Fatalf("error must name proposal and ballot index, got: %v", err)
			}
			if got, rerr := os.ReadFile(path); rerr != nil || string(got) != string(bad) {
				t.Fatalf("failed open rewrote or normalized the state file")
			}
		})
	}
}

// TestCorruptBallotsOpsRejectAndKeepFile：票据列表缺损后，查询不得返回空
// 票据或其他提案的部分结果，投票不得追加新票（已投过票的代表不能把它当
// 成首次投票重投），计票不得按零参与权重给出新的拒绝结论，执行不得继续
// 转账；所有操作都返回状态损坏错误，原文件保持原样。
func TestCorruptBallotsOpsRejectAndKeepFile(t *testing.T) {
	path, good, bad := corruptBallotsState(t, "voting", func(p map[string]any) {
		delete(p, "ballots")
	})

	// 先验证损坏前的正常语义：alice 的首次赞成票可查、同选择重试返回首次
	// 记录、改投冲突。
	if err := os.WriteFile(path, good, 0o600); err != nil {
		t.Fatal(err)
	}
	live, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	v, ok, err := live.VoteProposal("gip-bal")
	if err != nil || !ok || len(v.Ballots) != 1 {
		t.Fatalf("view before corruption: ok=%v err=%v ballots=%+v", ok, err, v)
	}
	if b := v.Ballots[0]; b.Representative != "alice" || b.Weight != 500 || !b.Support || b.VotedAt != 100 {
		t.Fatalf("first ballot before corruption = %+v", b)
	}
	if rb, err := live.CastVote("gip-bal", "alice", true, 101); err != nil ||
		rb.VotedAt != 100 || rb.Weight != 500 || !rb.Support {
		t.Fatalf("same-choice retry must return first ballot: %+v err=%v", rb, err)
	}
	live.Close()

	// 把票据列表整段删成缺损后直接打开：拒绝且不返回可用句柄。
	if err := os.WriteFile(path, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(path); !errors.Is(err, ErrStateCorrupt) || s != nil {
		if s != nil {
			s.Close()
		}
		t.Fatalf("Open corrupt: store=%v err=%v", s, err)
	}

	// 用一个真实打开的句柄，随后在句柄之外把文件改坏，验证每条操作路径。
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
		// alice 已投过票：缺损时绝不能把这次追加当成新的首次票。
		"CastVote": func() error { _, e := h.CastVote("gip-bal", "alice", true, 101); return e },
		// 截止计票绝不能按零参与权重给出新的拒绝结论。
		"TallyVote": func() error { _, e := h.TallyVote("gip-bal", 200); return e },
		"Execute":   func() error { _, e := h.Execute("gip-bal", 300); return e },
	}
	for name, op := range checks {
		if e := op(); !errors.Is(e, ErrStateCorrupt) {
			t.Fatalf("%s after corruption err=%v, want ErrStateCorrupt", name, e)
		}
	}
	// 全部失败后文件字节保持缺损被发现时的原样：没有追加票据、没有计票、
	// 没有执行、没有正常化重写。
	if got, rerr := os.ReadFile(recoverPath); rerr != nil || string(got) != string(bad) {
		t.Fatalf("failed ops rewrote or normalized the state file")
	}
}

// TestCorruptBallotsCLIUsesSameJudgement：命令行普通输出与 --json 使用同一
// 份判定：失败时不向标准输出给出成功记录或部分结果，原因写入标准错误，
// 退出码为 1。
func TestCorruptBallotsCLIUsesSameJudgement(t *testing.T) {
	binary := buildCLI(t)
	path, _, _ := corruptBallotsState(t, "passed", func(p map[string]any) {
		p["ballots"] = nil
	})
	commands := [][]string{
		{"proposal", "--id", "gip-bal"},
		{"proposals"},
		// alice 已投过票且提案已计票：null 列表不能让它再次投票或被当作闭票处理。
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
	// 全部拒绝后资金未被划转：文件仍损坏打不开；直接核对余额字段未被执行改写。
	raw, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if tb, _ := doc["treasury"].(float64); tb != 1000 {
		t.Fatalf("treasury must stay 1000 after rejected execute, got %v", doc["treasury"])
	}
	if rs, _ := doc["receipts"].([]any); len(rs) != 0 {
		t.Fatalf("no receipt may be written on the rejected execute, got %v", rs)
	}
}

// TestCorruptBallotsSiblingProposalRejectsWholeFile：文件里即使还有另一项
// 内容完整的提案，也不能只略过坏提案继续查询或投票——整份文件判损坏，
// 即使本次操作的对象正是那项完整提案。
func TestCorruptBallotsSiblingProposalRejectsWholeFile(t *testing.T) {
	path, _, bad := corruptBallotsState(t, "voting", func(p map[string]any) {
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
		t.Fatalf("error must name gip-bal and ballots, got: %v", err)
	}

	// CLI 上即使查询的是完整提案 gip-ok，文本与 JSON 也都必须拒绝整份文件。
	binary := buildCLI(t)
	for _, asJSON := range []bool{false, true} {
		args := []string{"proposal", "--id", "gip-ok"}
		name := "proposal gip-ok"
		if asJSON {
			args = append(args, "--json")
			name += " --json"
		}
		so, se, code := runCLI(t, binary, path, args...)
		if code != 1 || so != "" {
			t.Fatalf("%s: code=%d stdout=%q stderr=%q, want 1 with empty stdout", name, code, so, se)
		}
		if !strings.Contains(se, "gip-bal") || !strings.Contains(se, "ballots") {
			t.Fatalf("%s: stderr must name the corrupt sibling, got: %s", name, se)
		}
	}
}

// TestExplicitEmptyBallotsLegal：明确写出的空数组只说明列表没有缺损，仍
// 表示确实无人投票。正常创建后落盘必须明确写出 "ballots": []，可直接查询，
// 窗口内可以投出首次票；无人投票到期计票得到两侧权重与参与量均为零的
// 拒绝结论，该记录之后仍可正常查询与重开。空数组合法不放宽状态与计票的
// 一致性要求：已有 500/0 计票结论的提案不能借空数组抹掉票据。
func TestExplicitEmptyBallotsLegal(t *testing.T) {
	t.Run("handwritten empty array", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "handwritten.json")
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
		if err != nil || !ok || v.State != "voting" || len(v.Ballots) != 0 {
			t.Fatalf("query empty ballots: %+v ok=%v err=%v", v, ok, err)
		}
		// 窗口内接受首次投票，票重为该代表的原始/归集权重。
		b, err := store.CastVote("gip-empty", "bob", false, 100)
		if err != nil || b.Representative != "bob" || b.Weight != 200 || b.Support || b.VotedAt != 100 {
			t.Fatalf("first ballot after empty list: %+v err=%v", b, err)
		}
	})

	t.Run("created proposal persists and tallies empty", func(t *testing.T) {
		store, path := openTempStore(t, 1000)
		in := &CreateVoteInput{
			ID:       "gip-zero-turnout",
			Members:  []VoteMember{{ID: "alice", Weight: 300}, {ID: "bob", Weight: 200}},
			Quorum:   250,
			StartAt:  0,
			Deadline: 200, TimelockEnd: 300,
		}
		mustCreateVote(t, store, in)
		store.Close()

		// 落盘记录必须明确写出空数组，而不是省略 ballots。
		persisted, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(persisted), `"ballots": []`) {
			t.Fatalf("persisted state must write explicit empty ballots array:\n%s", persisted)
		}

		s, err := Open(path)
		if err != nil {
			t.Fatalf("reopen created empty-ballot proposal: %v", err)
		}
		defer s.Close()
		v, ok, err := s.VoteProposal("gip-zero-turnout")
		if err != nil || !ok || len(v.Ballots) != 0 || v.Tally != nil {
			t.Fatalf("query before tally: %+v ok=%v err=%v", v, ok, err)
		}
		// 无人投票到期计票：赞成、反对与参与权重均为零的拒绝结论。
		res, err := s.TallyVote("gip-zero-turnout", 200)
		if err != nil {
			t.Fatalf("tally no-vote proposal: %v", err)
		}
		if res.Passed || res.ForWeight != 0 || res.AgainstWeight != 0 || res.Turnout != 0 ||
			res.Quorum != 250 || res.TalliedAt != 200 {
			t.Fatalf("unexpected zero-turnout tally: %+v", res)
		}
		// 拒绝记录之后仍可正常查询，再次计票返回首次结论。
		v, ok, err = s.VoteProposal("gip-zero-turnout")
		if err != nil || !ok || v.State != "rejected" || v.Tally == nil || len(v.Ballots) != 0 {
			t.Fatalf("query rejected no-vote proposal: %+v ok=%v err=%v", v, ok, err)
		}
		again, err := s.TallyVote("gip-zero-turnout", 250)
		if err != nil || again.Passed || again.TalliedAt != 200 {
			t.Fatalf("retally must return first result: %+v err=%v", again, err)
		}
		s.Close()
		reopened, err := Open(path)
		if err != nil {
			t.Fatalf("reopen after zero-turnout tally: %v", err)
		}
		defer reopened.Close()
		v, ok, err = reopened.VoteProposal("gip-zero-turnout")
		if err != nil || !ok || v.State != "rejected" {
			t.Fatalf("query after reopen: %+v ok=%v err=%v", v, ok, err)
		}
	})

	t.Run("empty array does not launder tally mismatch", func(t *testing.T) {
		// 空数组只表示无人投票：已保存 500/0 通过结论的提案把票据抹成 []，
		// 状态与计票一致性仍须判整份文件损坏，不能借“空列表合法”放过。
		path, _, _ := corruptBallotsState(t, "passed", func(p map[string]any) {
			p["ballots"] = []any{}
		})
		s, err := Open(path)
		if !errors.Is(err, ErrStateCorrupt) || s != nil {
			if s != nil {
				s.Close()
			}
			t.Fatalf("passed proposal with emptied ballots: store=%v err=%v", s, err)
		}
		if !strings.Contains(err.Error(), `"gip-bal"`) {
			t.Fatalf("error must name the proposal: %v", err)
		}
	})
}

// TestBallotFidelityAndVoteRulesUnchanged：完整票据在重开后仍保留原顺序、
// 代表、权重、选择与首次投票时间；同选择重试返回首次记录，改投冲突规则
// 不因必填列表校验而改变。
func TestBallotFidelityAndVoteRulesUnchanged(t *testing.T) {
	store, path := openTempStore(t, 1000)
	in := &CreateVoteInput{
		ID: "gip-keep",
		Members: []VoteMember{
			{ID: "alice", Weight: 300},
			{ID: "bob", Weight: 200},
		},
		Delegations: []Delegation{{From: "bob", To: "alice"}},
		Quorum:      250,
		StartAt:     0,
		Deadline:    200,
		TimelockEnd: 300,
	}
	mustCreateVote(t, store, in)
	if _, err := store.CastVote("gip-keep", "alice", true, 100); err != nil {
		t.Fatal(err)
	}
	store.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	v, ok, err := reopened.VoteProposal("gip-keep")
	if err != nil || !ok || len(v.Ballots) != 1 {
		t.Fatalf("query: %+v ok=%v err=%v", v, ok, err)
	}
	b := v.Ballots[0]
	if b.Representative != "alice" || b.Weight != 500 || !b.Support || b.VotedAt != 100 {
		t.Fatalf("ballot fidelity lost across reopen: %+v", b)
	}
	// 同选择重试始终返回首次记录（含首次投票时间，不随 now 改变）。
	retry, err := reopened.CastVote("gip-keep", "alice", true, 150)
	if err != nil || retry.VotedAt != 100 || !retry.Support || retry.Weight != 500 {
		t.Fatalf("same-choice retry must return first ballot: %+v err=%v", retry, err)
	}
	// 改投仍报冲突，不静默覆盖。
	if _, err := reopened.CastVote("gip-keep", "alice", false, 150); !errors.Is(err, ErrProposalConflict) {
		t.Fatalf("changing choice err=%v, want ErrProposalConflict", err)
	}
	// 已委托成员仍不得直接投票。
	if _, err := reopened.CastVote("gip-keep", "bob", true, 150); !errors.Is(err, ErrVoteRejected) {
		t.Fatalf("delegated member direct vote err=%v, want ErrVoteRejected", err)
	}
}
