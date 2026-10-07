package govflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// buildRichVoteTableState 构造一份记录丰富的合法状态：
//   - gip-reg：登记来源提案，已执行，凭据与余额已核对（资金库 100 -> 60，
//     audits 40）；
//   - gip-voting：尚在投票中的提案，carol 已在窗口内投出一张赞成票；
//   - gip-pass：已计票通过但尚未执行的提案；
//   - gip-rej：已计票被拒绝的提案。
//
// 把整张 vote_proposals 表替换为 null 后，余额与执行凭据仍能核对一致，却会
// 藏起后面三种状态的提案——这正是不能把缺损表当成空表的原因。
func buildRichVoteTableState(t *testing.T) (*Store, string) {
	t.Helper()
	store, path := openTempStore(t, 100)
	mustRegister(t, store, "gip-reg", 0, "transfer:audits:40")
	if _, err := store.Execute("gip-reg", 0); err != nil {
		t.Fatalf("execute gip-reg: %v", err)
	}
	voting := &CreateVoteInput{
		ID: "gip-voting",
		Members: []VoteMember{
			{ID: "carol", Weight: 1},
		},
		Quorum:      1,
		StartAt:     0,
		Deadline:    100,
		TimelockEnd: 200,
		Actions:     []string{"transfer:audits:1"},
	}
	mustCreateVote(t, store, voting)
	if _, err := store.CastVote("gip-voting", "carol", true, 50); err != nil {
		t.Fatalf("carol vote: %v", err)
	}
	pass := &CreateVoteInput{
		ID:          "gip-pass",
		Members:     []VoteMember{{ID: "alice", Weight: 10}},
		Quorum:      5,
		StartAt:     0,
		Deadline:    100,
		TimelockEnd: 200,
		Actions:     []string{"transfer:audits:10"},
	}
	mustCreateVote(t, store, pass)
	if _, err := store.CastVote("gip-pass", "alice", true, 50); err != nil {
		t.Fatalf("alice vote: %v", err)
	}
	if r, err := store.TallyVote("gip-pass", 100); err != nil || !r.Passed {
		t.Fatalf("tally gip-pass: %+v err=%v", r, err)
	}
	rejected := &CreateVoteInput{
		ID:          "gip-rej",
		Members:     []VoteMember{{ID: "bob", Weight: 10}},
		Quorum:      5,
		StartAt:     0,
		Deadline:    100,
		TimelockEnd: 200,
		Actions:     nil,
	}
	mustCreateVote(t, store, rejected)
	if r, err := store.TallyVote("gip-rej", 100); err != nil || r.Passed {
		t.Fatalf("tally gip-rej: %+v err=%v", r, err)
	}
	return store, path
}

// TestVoteProposalsTableShape：顶层 vote_proposals 字段一旦出现就必须是 JSON
// 对象。显式 null 以及数组、字符串、数字、布尔都判整份状态文件损坏，错误指出
// vote_proposals 字段与“空值/类型不符”的具体原因；字段缺失（旧文件）与显式
// 空对象 {} 合法，其余记录（余额、登记提案、执行凭据）照常读出。
func TestVoteProposalsTableShape(t *testing.T) {
	cases := map[string]struct {
		mutate func(doc map[string]any)
		want   string
	}{
		"explicit null": {
			mutate: func(doc map[string]any) { doc["vote_proposals"] = nil },
			want:   `field "vote_proposals" is null`,
		},
		"array": {
			mutate: func(doc map[string]any) { doc["vote_proposals"] = []any{} },
			want:   `field "vote_proposals" has wrong type: want object, got array`,
		},
		"non-empty array": {
			mutate: func(doc map[string]any) { doc["vote_proposals"] = []any{"gip-voting"} },
			want:   `field "vote_proposals" has wrong type: want object, got array`,
		},
		"string": {
			mutate: func(doc map[string]any) { doc["vote_proposals"] = "gip-voting" },
			want:   `field "vote_proposals" has wrong type: want object, got string`,
		},
		"number": {
			mutate: func(doc map[string]any) { doc["vote_proposals"] = json.Number("1") },
			want:   `field "vote_proposals" has wrong type: want object, got number`,
		},
		"boolean": {
			mutate: func(doc map[string]any) { doc["vote_proposals"] = true },
			want:   `field "vote_proposals" has wrong type: want object, got boolean`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store, path := buildRichVoteTableState(t)
			store.Close()
			rewriteTopLevelField(t, path, tc.mutate)
			// 改坏后的磁盘内容：Open 失败不得重建或覆盖它。
			corrupt, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			s, err := Open(path)
			if !errors.Is(err, ErrStateCorrupt) {
				if s != nil {
					s.Close()
				}
				t.Fatalf("Open err=%v, want ErrStateCorrupt", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Open err=%q, want substring %q", err, tc.want)
			}
			// 损坏文件保持原样，不被重建或覆盖。
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, corrupt) {
				t.Fatalf("corrupt file was overwritten")
			}
		})
	}

	t.Run("explicit empty object is legal", func(t *testing.T) {
		store, path := buildRichVoteTableState(t)
		store.Close()
		rewriteTopLevelField(t, path, func(doc map[string]any) {
			doc["vote_proposals"] = map[string]any{}
		})
		s, err := Open(path)
		if err != nil {
			t.Fatalf("{} must be a legal vote table: %v", err)
		}
		defer s.Close()
		// 空表表示确实没有投票提案；其余余额、登记提案与执行凭据照常读出。
		if vps, err := s.VoteProposals(); err != nil || len(vps) != 0 {
			t.Fatalf("VoteProposals=%v err=%v, want empty", vps, err)
		}
		if bal, err := s.TreasuryBalance(); err != nil || bal != 60 {
			t.Fatalf("TreasuryBalance=%d err=%v, want 60", bal, err)
		}
		if p, ok, err := s.Proposal("gip-reg"); err != nil || !ok || p.State != "executed" {
			t.Fatalf("registered proposal = %+v ok=%v err=%v", p, ok, err)
		}
		if r, ok, err := s.Receipt("gip-reg"); err != nil || !ok || r.Order != 0 {
			t.Fatalf("receipt = %+v ok=%v err=%v", r, ok, err)
		}
		// 空对象不是“未找到”的错误：显式查询得到 ok=false 而非损坏错误。
		if _, ok, err := s.VoteProposal("gip-voting"); err != nil || ok {
			t.Fatalf("VoteProposal on explicit {} : ok=%v err=%v, want not found without error", ok, err)
		}
	})

	t.Run("missing field is legacy and legal", func(t *testing.T) {
		store, path := buildRichVoteTableState(t)
		store.Close()
		rewriteTopLevelField(t, path, func(doc map[string]any) { delete(doc, "vote_proposals") })
		s, err := Open(path)
		if err != nil {
			t.Fatalf("legacy file without the table must stay readable: %v", err)
		}
		defer s.Close()
		if vps, err := s.VoteProposals(); err != nil || len(vps) != 0 {
			t.Fatalf("VoteProposals=%v err=%v, want empty for legacy file", vps, err)
		}
		if bal, err := s.TreasuryBalance(); err != nil || bal != 60 {
			t.Fatalf("TreasuryBalance=%d err=%v, want 60", bal, err)
		}
		if p, ok, err := s.Proposal("gip-reg"); err != nil || !ok || p.State != "executed" {
			t.Fatalf("registered proposal = %+v ok=%v err=%v", p, ok, err)
		}
		if r, ok, err := s.Receipt("gip-reg"); err != nil || !ok || len(r.Actions) != 1 {
			t.Fatalf("receipt = %+v ok=%v err=%v", r, ok, err)
		}
	})
}

// TestVoteProposalsTableFreshFileWritesExplicitObject：新建资金库落盘时必须
// 明确写出空的投票提案对象 {}，与旧文件“字段缺失”的兼容情形逐字区分。
func TestVoteProposalsTableFreshFileWritesExplicitObject(t *testing.T) {
	store, path := openTempStore(t, 0)
	store.Close()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	got, ok := doc["vote_proposals"]
	if !ok {
		t.Fatalf("fresh state must explicitly contain vote_proposals, got:\n%s", raw)
	}
	if string(bytes.TrimSpace(got)) != "{}" {
		t.Fatalf("vote_proposals=%s, want explicit {}", got)
	}
}

// TestCorruptVoteProposalsTableBlocksEverything：vote_proposals 被替换成 null
// 后，整份状态读取必须失败：任何查询都不能返回余额或登记提案的部分结果，
// VoteProposal 不能把原提案报成“未找到”，登记、创建提案、投票、计票与执行
// 也都不能继续进行；一切操作报 ErrStateCorrupt，文件字节保持原样，不会用新
// 记录覆盖缺损文件。
func TestCorruptVoteProposalsTableBlocksEverything(t *testing.T) {
	store, path := buildRichVoteTableState(t)
	defer store.Close()

	rewriteTopLevelField(t, path, func(doc map[string]any) { doc["vote_proposals"] = nil })
	corrupt, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	firstVote := &CreateVoteInput{
		ID:          "gip-new",
		Members:     []VoteMember{{ID: "zoe", Weight: 1}},
		Quorum:      1,
		StartAt:     0,
		Deadline:    100,
		TimelockEnd: 200,
	}
	checks := map[string]func() error{
		"BalanceSnapshot":   func() error { _, err := store.BalanceSnapshot(); return err },
		"TreasuryBalance":   func() error { _, err := store.TreasuryBalance(); return err },
		"Balance":           func() error { _, err := store.Balance("audits"); return err },
		"Balances":          func() error { _, err := store.Balances(); return err },
		"Proposal":          func() error { _, _, err := store.Proposal("gip-reg"); return err },
		"Proposals":         func() error { _, err := store.Proposals(); return err },
		"ProposalsSnapshot": func() error { _, err := store.ProposalsSnapshot(); return err },
		"Receipt":           func() error { _, _, err := store.Receipt("gip-reg"); return err },
		"Receipts":          func() error { _, err := store.Receipts(); return err },
		"VoteProposals":     func() error { _, err := store.VoteProposals(); return err },
		"VoteProposal": func() error {
			_, ok, err := store.VoteProposal("gip-voting")
			if err == nil && !ok {
				return errors.New("VoteProposal reported not found instead of state corruption")
			}
			return err
		},
		"Register": func() error {
			_, err := store.Register("gip-reg-2", 0, []string{"transfer:x:1"})
			return err
		},
		"RegisterProposal": func() error {
			_, _, err := store.RegisterProposal("gip-reg-2", 0, []string{"transfer:x:1"})
			return err
		},
		"CreateVoteProposal": func() error { _, _, err := store.CreateVoteProposal(firstVote); return err },
		"CastVote":           func() error { _, err := store.CastVote("gip-voting", "carol", false, 60); return err },
		"TallyVote":          func() error { _, err := store.TallyVote("gip-voting", 100); return err },
		"Execute":            func() error { _, err := store.Execute("gip-reg", 0); return err },
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

// TestLegacyFileWithoutVoteTableCreatesFirstProposal：旧文件没有整张
// vote_proposals 表时仍可打开与查询；在其上创建第一项投票提案后提交成功，
// 重开文件提案仍在（加载时的空表规范化不能在提交前重校验时把新映射清空），
// 且落盘形状已变成明确写出的对象。在旧文件上登记提案同样能提交并保留投票
// 表字段。
func TestLegacyFileWithoutVoteTableCreatesFirstProposal(t *testing.T) {
	store, path := buildRichVoteTableState(t)
	store.Close()
	// 删除整张表，得到一份含登记提案与执行凭据、但没有投票表的旧文件。
	rewriteTopLevelField(t, path, func(doc map[string]any) { delete(doc, "vote_proposals") })

	store, err := Open(path)
	if err != nil {
		t.Fatalf("legacy file must open: %v", err)
	}
	in := &CreateVoteInput{
		ID:          "gip-first",
		Members:     []VoteMember{{ID: "zoe", Weight: 1}},
		Quorum:      1,
		StartAt:     0,
		Deadline:    100,
		TimelockEnd: 200,
		Actions:     []string{"transfer:audits:5"},
	}
	view, existed, err := store.CreateVoteProposal(in)
	if err != nil || existed || view == nil || view.ID != "gip-first" {
		t.Fatalf("create first vote on legacy file: view=%+v existed=%v err=%v", view, existed, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// 落盘文件现在明确写出 vote_proposals 对象，且新提案确实保留在文件里。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	table, ok := doc["vote_proposals"]
	if !ok {
		t.Fatalf("after creating the first vote, the table must be explicitly written")
	}
	if jsonValueType(table) != "object" {
		t.Fatalf("vote_proposals = %s, want JSON object", table)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after create on legacy file: %v", err)
	}
	defer reopened.Close()
	if vp, ok, err := reopened.VoteProposal("gip-first"); err != nil || !ok || vp.State != "voting" {
		t.Fatalf("first vote proposal lost after commit: %+v ok=%v err=%v", vp, ok, err)
	}
	// 原有的登记提案与执行凭据仍在。
	if p, ok, err := reopened.Proposal("gip-reg"); err != nil || !ok || p.State != "executed" {
		t.Fatalf("registered proposal = %+v ok=%v err=%v", p, ok, err)
	}
}

// TestLegacyFileRegisterCommitWritesExplicitTable：旧文件上的任意一次提交
// （此处是登记新提案）都会把缺省的投票表规范化为明确写出的 {}，重开照常
// 读取，且原有余额与凭据不变。
func TestLegacyFileRegisterCommitWritesExplicitTable(t *testing.T) {
	_, path := buildRichVoteTableState(t)
	rewriteTopLevelField(t, path, func(doc map[string]any) { delete(doc, "vote_proposals") })

	store, err := Open(path)
	if err != nil {
		t.Fatalf("legacy file must open: %v", err)
	}
	if existed, err := store.Register("gip-reg-2", 10, []string{"transfer:audits:1"}); err != nil || existed {
		t.Fatalf("register on legacy file: existed=%v err=%v", existed, err)
	}
	store.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after register: %v", err)
	}
	defer reopened.Close()
	if bal, err := reopened.TreasuryBalance(); err != nil || bal != 60 {
		t.Fatalf("TreasuryBalance=%d err=%v, want 60", bal, err)
	}
	if vps, err := reopened.VoteProposals(); err != nil || len(vps) != 0 {
		t.Fatalf("VoteProposals=%v err=%v, want still empty", vps, err)
	}
}
