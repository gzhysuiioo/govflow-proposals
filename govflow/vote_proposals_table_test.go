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

// setupStateWithVoteHistory 构造一份内容丰富的合法状态：一项已执行的投票提案
// （带执行凭据与账户余额）、一项 passed 登记提案，以及一项尚在投票且已有票据
// 的投票提案。vote_proposals 表被改坏时，其余余额与执行凭据仍能核对一致——
// 正用于验证“记录缺损必须明确拒绝，不得当成正常空表”。
func setupStateWithVoteHistory(t *testing.T) (*Store, string) {
	t.Helper()
	store, path := openTempStore(t, 1000)

	done := &CreateVoteInput{
		ID:          "gip-done",
		Members:     []VoteMember{{ID: "alice", Weight: 100}},
		Quorum:      100,
		StartAt:     0,
		Deadline:    100,
		TimelockEnd: 200,
		Actions:     []string{"transfer:audits:100"},
	}
	mustCreateVote(t, store, done)
	if _, err := store.CastVote("gip-done", "alice", true, 50); err != nil {
		t.Fatalf("CastVote gip-done: %v", err)
	}
	if _, err := store.TallyVote("gip-done", 100); err != nil {
		t.Fatalf("TallyVote gip-done: %v", err)
	}
	if _, err := store.Execute("gip-done", 200); err != nil {
		t.Fatalf("Execute gip-done: %v", err)
	}

	mustRegister(t, store, "gip-reg", 300, "transfer:legal:5")

	voting := &CreateVoteInput{
		ID:          "gip-v",
		Members:     []VoteMember{{ID: "bob", Weight: 200}},
		Quorum:      100,
		StartAt:     0,
		Deadline:    100,
		TimelockEnd: 200,
		Actions:     []string{"transfer:bob:1"},
	}
	mustCreateVote(t, store, voting)
	if _, err := store.CastVote("gip-v", "bob", true, 40); err != nil {
		t.Fatalf("CastVote gip-v: %v", err)
	}
	return store, path
}

// TestVoteProposalsTableBadShapeRejected：字段一旦出现就必须是 JSON 对象。
// 显式 null、数组（空或非空）、字符串（即使内容是 "{}"）、数字与布尔都判
// 整份状态损坏，错误指出 vote_proposals 字段与空值/类型不符的具体原因；
// 拒绝后文件保持原样。其余余额与执行凭据恰好能核对一致也不放宽——用零资金
// 库、无凭据的新文件重复 null 情形同样拒绝。
func TestVoteProposalsTableBadShapeRejected(t *testing.T) {
	cases := map[string]struct {
		mutate func(doc map[string]any)
		want   string
	}{
		"null": {
			mutate: func(doc map[string]any) { doc["vote_proposals"] = nil },
			want:   `field "vote_proposals" is null`,
		},
		"empty array": {
			mutate: func(doc map[string]any) { doc["vote_proposals"] = []any{} },
			want:   `field "vote_proposals" has wrong type: want object, got array`,
		},
		"non-empty array": {
			mutate: func(doc map[string]any) {
				doc["vote_proposals"] = []any{map[string]any{"id": "gip-v"}}
			},
			want: `field "vote_proposals" has wrong type: want object, got array`,
		},
		"string looking like object": {
			mutate: func(doc map[string]any) { doc["vote_proposals"] = "{}" },
			want:   `field "vote_proposals" has wrong type: want object, got string`,
		},
		"empty string": {
			mutate: func(doc map[string]any) { doc["vote_proposals"] = "" },
			want:   `field "vote_proposals" has wrong type: want object, got string`,
		},
		"integer": {
			mutate: func(doc map[string]any) { doc["vote_proposals"] = json.Number("0") },
			want:   `field "vote_proposals" has wrong type: want object, got number`,
		},
		"fraction": {
			mutate: func(doc map[string]any) { doc["vote_proposals"] = json.Number("1.5") },
			want:   `field "vote_proposals" has wrong type: want object, got number`,
		},
		"boolean": {
			mutate: func(doc map[string]any) { doc["vote_proposals"] = true },
			want:   `field "vote_proposals" has wrong type: want object, got boolean`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// 富状态：有执行凭据、余额与登记提案可核对，坏表仍必须整份拒绝。
			store, path := setupStateWithVoteHistory(t)
			store.Close()
			rewriteTopLevelField(t, path, tc.mutate)
			corrupt, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			s, err := Open(path)
			if !errors.Is(err, ErrStateCorrupt) || s != nil {
				if s != nil {
					s.Close()
				}
				t.Fatalf("Open store=%v err=%v, want ErrStateCorrupt and no store", s, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Open err=%q, want substring %q", err, tc.want)
			}
			if after, rerr := os.ReadFile(path); rerr != nil || !bytes.Equal(after, corrupt) {
				t.Fatalf("corrupt state file must stay unchanged")
			}
		})
	}

	// 零资金库、无执行凭据、无登记提案：不能因“其余内容恰好都是零/空”就把
	// null 表认可成正常空表。
	t.Run("null on empty zero treasury", func(t *testing.T) {
		store, path := openTempStore(t, 0)
		store.Close()
		rewriteTopLevelField(t, path, func(doc map[string]any) { doc["vote_proposals"] = nil })
		s, err := Open(path)
		if !errors.Is(err, ErrStateCorrupt) || s != nil {
			if s != nil {
				s.Close()
			}
			t.Fatalf("Open store=%v err=%v, want ErrStateCorrupt", s, err)
		}
		if !strings.Contains(err.Error(), `field "vote_proposals" is null`) {
			t.Fatalf("Open err=%q, want vote_proposals null reason", err)
		}
	})
}

// TestNullVoteTableHidesLiveProposalsRegression：精确复现标题场景——投票提案
// 分别处于“尚在投票（已有票据）”“已通过但未执行”“已被拒绝”三种状态，它们
// 都没有执行凭据。整张 vote_proposals 表被替换成 null 时，其余余额与记录仍能
// 核对：旧行为会把坏表折叠成正常空表，导致打开与查询成功却报告提案不存在，
// 随后还能用原编号重新创建并覆盖文件。修正后整份状态必须明确拒绝：打不开、
// 查询不返回“未找到”、不能用原编号或新编号创建，文件保持原样。
func TestNullVoteTableHidesLiveProposalsRegression(t *testing.T) {
	store, path := openTempStore(t, 1000)

	mk := func(id string) *CreateVoteInput {
		return &CreateVoteInput{
			ID:          id,
			Members:     []VoteMember{{ID: "m-" + id, Weight: 100}},
			Quorum:      1,
			StartAt:     0,
			Deadline:    100,
			TimelockEnd: 200,
			Actions:     []string{"transfer:acct:1"},
		}
	}
	// voting：尚在投票且已有一张赞成票。
	mustCreateVote(t, store, mk("gip-voting"))
	if _, err := store.CastVote("gip-voting", "m-gip-voting", true, 50); err != nil {
		t.Fatalf("CastVote: %v", err)
	}
	// passed：已计票通过但尚未执行（无执行凭据）。
	mustCreateVote(t, store, mk("gip-passed"))
	if _, err := store.CastVote("gip-passed", "m-gip-passed", true, 50); err != nil {
		t.Fatalf("CastVote passed: %v", err)
	}
	if r, err := store.TallyVote("gip-passed", 100); err != nil || !r.Passed {
		t.Fatalf("TallyVote passed=%v err=%v", r, err)
	}
	// rejected：无人投票，计票拒绝（同样无执行凭据）。
	mustCreateVote(t, store, mk("gip-rejected"))
	if r, err := store.TallyVote("gip-rejected", 100); err != nil || r.Passed {
		t.Fatalf("TallyVote rejected=%v err=%v", r, err)
	}

	// 句柄保持打开，仅把磁盘文件的整张表改坏：三张提案都在、资金库仍为
	// 1000、没有任何执行凭据，其余记录完全自洽，正是“被当成正常空表”最
	// 危险的情形。后续每个操作各自重读文件，都必须在坏表上失败。
	rewriteTopLevelField(t, path, func(doc map[string]any) { doc["vote_proposals"] = nil })
	corrupt, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// 重新打开必须直接失败，错误指出 vote_proposals 为空值。
	s, oerr := Open(path)
	if !errors.Is(oerr, ErrStateCorrupt) || s != nil {
		if s != nil {
			s.Close()
		}
		t.Fatalf("Open with null table: store=%v err=%v, want corrupt", s, oerr)
	}
	if !strings.Contains(oerr.Error(), `field "vote_proposals" is null`) {
		t.Fatalf("Open err=%q, want vote_proposals null reason", oerr)
	}

	// 既有句柄上的每个查询都报损坏：不返回余额/快照等部分结果，也不把原
	// 提案报告成“未找到”（ok=false 且无错误正是旧行为的错误表现）。
	for _, id := range []string{"gip-voting", "gip-passed", "gip-rejected"} {
		if v, ok, qerr := store.VoteProposal(id); !errors.Is(qerr, ErrStateCorrupt) {
			t.Fatalf("VoteProposal(%s) v=%+v ok=%v err=%v, want corrupt", id, v, ok, qerr)
		}
		if _, qerr := store.Execute(id, 200); !errors.Is(qerr, ErrStateCorrupt) {
			t.Fatalf("Execute(%s) err=%v, want corrupt", id, qerr)
		}
	}
	for name, fn := range map[string]func() error{
		"VoteProposals":           func() error { _, err := store.VoteProposals(); return err },
		"ProposalsSnapshot":       func() error { _, err := store.ProposalsSnapshot(); return err },
		"Proposals":               func() error { _, err := store.Proposals(); return err },
		"TreasuryBalance":         func() error { _, err := store.TreasuryBalance(); return err },
		"BalanceSnapshot":         func() error { _, err := store.BalanceSnapshot(); return err },
		"Receipts":                func() error { _, err := store.Receipts(); return err },
		"CastVote":                func() error { _, err := store.CastVote("gip-voting", "m-gip-voting", false, 60); return err },
		"TallyVote":               func() error { _, err := store.TallyVote("gip-voting", 100); return err },
		"Register":                func() error { _, err := store.Register("gip-x", 0, []string{"transfer:a:1"}); return err },
		"CreateVote(existing id)": func() error { _, _, err := store.CreateVoteProposal(mk("gip-voting")); return err },
	} {
		if err := fn(); !errors.Is(err, ErrStateCorrupt) {
			t.Fatalf("%s err=%v, want ErrStateCorrupt", name, err)
		}
	}
	brandNew := mk("gip-brand-new")
	brandNew.Members = []VoteMember{{ID: "zoe", Weight: 1}}
	if _, _, cerr := store.CreateVoteProposal(brandNew); !errors.Is(cerr, ErrStateCorrupt) {
		t.Fatalf("create new proposal err=%v, want corrupt", cerr)
	}

	if after, rerr := os.ReadFile(path); rerr != nil || !bytes.Equal(after, corrupt) {
		t.Fatalf("corrupt state file must stay unchanged")
	}
}

// TestVoteProposalsTableMissingAndEmptyObjectLegal：兼容范围仅限真正没有
// vote_proposals 字段的旧文件，以及明确写出的空对象 {}。两种情况下余额、登记
// 提案与执行凭据照常读出，查询返回“没有投票提案”，并能创建第一项投票提案。
func TestVoteProposalsTableMissingAndEmptyObjectLegal(t *testing.T) {
	// 带登记提案与已执行凭据的文件，保证旧表缺省时其余记录仍完整可用。
	store, path := openTempStore(t, 500)
	mustRegister(t, store, "gip-old", 0, "transfer:acct:25")
	if _, err := store.Execute("gip-old", 0); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	store.Close()

	for _, tc := range []struct {
		name   string
		mutate func(doc map[string]any)
	}{
		{"legacy missing", func(doc map[string]any) { delete(doc, "vote_proposals") }},
		{"explicit empty object", func(doc map[string]any) { doc["vote_proposals"] = map[string]any{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rewriteTopLevelField(t, path, tc.mutate)
			reopened, err := Open(path)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if bal, err := reopened.TreasuryBalance(); err != nil || bal != 475 {
				t.Fatalf("TreasuryBalance=%d err=%v, want 475", bal, err)
			}
			if p, ok, err := reopened.Proposal("gip-old"); err != nil || !ok || p.State != "executed" {
				t.Fatalf("registered proposal ok=%v err=%v: %+v", ok, err, p)
			}
			if r, ok, err := reopened.Receipt("gip-old"); err != nil || !ok || r.Order != 0 {
				t.Fatalf("receipt ok=%v err=%v: %+v", ok, err, r)
			}
			if vps, err := reopened.VoteProposals(); err != nil || len(vps) != 0 {
				t.Fatalf("VoteProposals=%v err=%v, want no voting proposals", vps, err)
			}
			if v, ok, err := reopened.VoteProposal("gip-v"); err != nil || ok || v != nil {
				t.Fatalf("VoteProposal(gip-v) v=%v ok=%v err=%v, want not found without error", v, ok, err)
			}
			snap, err := reopened.ProposalsSnapshot()
			if err != nil || len(snap.Voting) != 0 || len(snap.Registered) != 1 {
				t.Fatalf("snapshot=%+v err=%v", snap, err)
			}

			// 在旧文件/空表文件上创建第一项投票提案：正常成功并重开可读。
			in := &CreateVoteInput{
				ID:          "gip-first",
				Members:     []VoteMember{{ID: "alice", Weight: 1}},
				Quorum:      1,
				StartAt:     0,
				Deadline:    10,
				TimelockEnd: 10,
			}
			if view, existed, err := reopened.CreateVoteProposal(in); err != nil || existed || view.ID != "gip-first" {
				t.Fatalf("create first vote proposal: view=%+v existed=%v err=%v", view, existed, err)
			}
			reopened.Close()

			reopened2, err := Open(path)
			if err != nil {
				t.Fatalf("reopen after create: %v", err)
			}
			defer reopened2.Close()
			if v, ok, err := reopened2.VoteProposal("gip-first"); err != nil || !ok || v.State != "voting" {
				t.Fatalf("first voting proposal v=%+v ok=%v err=%v", v, ok, err)
			}
		})
	}
}

// TestCorruptVoteProposalsTableBlocksAllOperations：坏表必须让整份状态读取
// 失败——不能返回余额或登记提案/凭据的部分结果，不能把某编号查询报告成
// “未找到”，更不能继续创建（包括用原编号）并用新记录覆盖文件。所有操作都
// 返回 ErrStateCorrupt，文件字节保持原样。
func TestCorruptVoteProposalsTableBlocksAllOperations(t *testing.T) {
	store, path := setupStateWithVoteHistory(t)
	defer store.Close()

	rewriteTopLevelField(t, path, func(doc map[string]any) { doc["vote_proposals"] = nil })
	corrupt, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 与已存在提案同编号的合法输入：必须报损坏，而不是冲突或成功占用编号。
	sameAsExisting := &CreateVoteInput{
		ID:          "gip-v",
		Members:     []VoteMember{{ID: "bob", Weight: 200}},
		Quorum:      100,
		StartAt:     0,
		Deadline:    100,
		TimelockEnd: 200,
		Actions:     []string{"transfer:bob:1"},
	}
	brandNew := &CreateVoteInput{
		ID:          "gip-new",
		Members:     []VoteMember{{ID: "zoe", Weight: 1}},
		Quorum:      1,
		StartAt:     0,
		Deadline:    10,
		TimelockEnd: 10,
	}

	checks := map[string]func() error{
		"BalanceSnapshot":   func() error { _, err := store.BalanceSnapshot(); return err },
		"TreasuryBalance":   func() error { _, err := store.TreasuryBalance(); return err },
		"Balance":           func() error { _, err := store.Balance("audits"); return err },
		"Balances":          func() error { _, err := store.Balances(); return err },
		"Proposal":          func() error { _, _, err := store.Proposal("gip-reg"); return err },
		"Proposals":         func() error { _, err := store.Proposals(); return err },
		"ProposalsSnapshot": func() error { _, err := store.ProposalsSnapshot(); return err },
		"Receipt":           func() error { _, _, err := store.Receipt("gip-done"); return err },
		"Receipts":          func() error { _, err := store.Receipts(); return err },
		// 关键：原提案既不能报告成“未找到”，也不能被重新创建占用编号。
		"VoteProposal(gip-v)": func() error {
			v, ok, err := store.VoteProposal("gip-v")
			if err == nil {
				t.Fatalf("VoteProposal returned v=%+v ok=%v with no error, want corrupt error", v, ok)
			}
			return err
		},
		"VoteProposal(missing id)": func() error { _, _, err := store.VoteProposal("gip-nope"); return err },
		"VoteProposals":            func() error { _, err := store.VoteProposals(); return err },
		"Register":                 func() error { _, err := store.Register("gip-2", 0, []string{"transfer:x:1"}); return err },
		"RegisterProposal": func() error {
			_, _, err := store.RegisterProposal("gip-2", 0, []string{"transfer:x:1"})
			return err
		},
		"CreateVoteProposal(existing id)": func() error {
			_, _, err := store.CreateVoteProposal(sameAsExisting)
			return err
		},
		"CreateVoteProposal(brand new id)": func() error {
			_, _, err := store.CreateVoteProposal(brandNew)
			return err
		},
		"CastVote":  func() error { _, err := store.CastVote("gip-v", "bob", false, 50); return err },
		"TallyVote": func() error { _, err := store.TallyVote("gip-v", 100); return err },
		// 已执行投票提案正常会返回首次凭据；坏表上必须连凭据也读不出来。
		"Execute executed vote": func() error { _, err := store.Execute("gip-done", 200); return err },
		"Execute registered":    func() error { _, err := store.Execute("gip-reg", 300); return err },
		"Execute missing id":    func() error { _, err := store.Execute("gip-ghost", 300); return err },
	}
	for name, fn := range checks {
		if err := fn(); !errors.Is(err, ErrStateCorrupt) {
			t.Fatalf("%s err=%v, want ErrStateCorrupt", name, err)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, corrupt) {
		t.Fatalf("corrupt state file was modified by rejected operations")
	}
}

// TestCLIVoteProposalsTableCorrupt：文本与 --json 查询/写命令在坏表上都以
// 退出码 1 结束，stdout 没有任何结果，stderr 指出 vote_proposals 字段与具体
// 空值原因；拒绝后原文件保持原样，不被新记录覆盖。
func TestCLIVoteProposalsTableCorrupt(t *testing.T) {
	binary := buildCLI(t)
	state := filepath.Join(t.TempDir(), "treasury.json")
	if _, _, code := runCLI(t, binary, state, "init", "--balance", "1000"); code != 0 {
		t.Fatal("init failed")
	}
	createArgs := []string{"create-vote", "--id", "gip-v",
		"--member", "bob:200", "--quorum", "100",
		"--start", "0", "--deadline", "100", "--timelock", "200",
		"--action", "transfer:bob:1"}
	if _, se, code := runCLI(t, binary, state, createArgs...); code != 0 {
		t.Fatalf("create-vote failed: %s", se)
	}
	if _, se, code := runCLI(t, binary, state, "vote", "--id", "gip-v",
		"--voter", "bob", "--choice", "for", "--now", "40"); code != 0 {
		t.Fatalf("vote failed: %s", se)
	}

	good, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	rewriteTopLevelField(t, state, func(doc map[string]any) { doc["vote_proposals"] = nil })
	corrupt, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(good, corrupt) {
		t.Fatal("test setup did not corrupt the state file")
	}

	assertRejected := func(name string, args ...string) {
		t.Helper()
		so, se, code := runCLI(t, binary, state, args...)
		if code != 1 {
			t.Fatalf("%s exit=%d, want 1 (stdout=%q stderr=%q)", name, code, so, se)
		}
		if so != "" {
			t.Fatalf("%s stdout must be empty, got %q", name, so)
		}
		if !strings.Contains(se, "vote_proposals") || !strings.Contains(se, "is null") {
			t.Fatalf("%s stderr=%q, want vote_proposals field and null reason", name, se)
		}
	}

	// 普通文本与 --json 查询同样拒绝，stdout 都不能有查询结果。
	assertRejected("proposal text", "proposal", "--id", "gip-v")
	assertRejected("proposal json", "proposal", "--id", "gip-v", "--json")
	assertRejected("proposal missing id text", "proposal", "--id", "gip-ghost")
	assertRejected("proposals text", "proposals")
	assertRejected("proposals json", "proposals", "--json")
	assertRejected("balances text", "balances")
	assertRejected("balances json", "balances", "--json")
	assertRejected("receipt text", "receipt", "--id", "gip-v")
	assertRejected("receipts json", "receipts", "--json")
	// 写命令也必须在读状态处失败，不能创建/占用编号或覆盖文件。
	assertRejected("create-vote json", append(append([]string{}, createArgs...), "--json")...)
	assertRejected("create another vote", "create-vote", "--id", "gip-new",
		"--member", "zoe:1", "--quorum", "1",
		"--start", "0", "--deadline", "10", "--timelock", "10")
	assertRejected("vote", "vote", "--id", "gip-v", "--voter", "bob",
		"--choice", "against", "--now", "50")
	assertRejected("tally json", "tally", "--id", "gip-v", "--now", "100", "--json")
	assertRejected("execute", "execute", "--id", "gip-v", "--now", "200")

	after, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, corrupt) {
		t.Fatalf("corrupt state file was overwritten by rejected CLI commands")
	}
}
