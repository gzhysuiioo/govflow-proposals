package govflow

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func baseVoteInput(id string) *CreateVoteInput {
	return &CreateVoteInput{
		ID: id,
		Members: []VoteMember{
			{ID: "alice", Weight: 300},
			{ID: "bob", Weight: 200},
			{ID: "carol", Weight: 100},
			{ID: "dave", Weight: 400},
		},
		Delegations: []Delegation{{From: "bob", To: "alice"}, {From: "carol", To: "alice"}},
		Quorum:      600,
		StartAt:     100,
		Deadline:    200,
		TimelockEnd: 300,
		Actions:     []string{"transfer:audits:100"},
	}
}

func mustCreateVote(t *testing.T, store *Store, in *CreateVoteInput) *VoteProposalView {
	t.Helper()
	view, existed, err := store.CreateVoteProposal(in)
	if err != nil || existed {
		t.Fatalf("CreateVoteProposal(%s) existed=%v err=%v", in.ID, existed, err)
	}
	return view
}

func TestCreateVoteValidation(t *testing.T) {
	store, _ := openTempStore(t, 1000)
	defer store.Close()

	cases := []struct {
		name   string
		mutate func(in *CreateVoteInput)
		want   error
	}{
		{"empty id", func(in *CreateVoteInput) { in.ID = "" }, ErrInvalidProposal},
		{"no members", func(in *CreateVoteInput) { in.Members = nil }, ErrInvalidProposal},
		{"empty member id", func(in *CreateVoteInput) { in.Members[0].ID = "" }, ErrInvalidProposal},
		{"duplicate member", func(in *CreateVoteInput) { in.Members[1].ID = "alice" }, ErrInvalidProposal},
		{"zero weight", func(in *CreateVoteInput) { in.Members[2].Weight = 0 }, ErrInvalidProposal},
		{"negative weight", func(in *CreateVoteInput) { in.Members[2].Weight = -5 }, ErrInvalidProposal},
		{"quorum zero", func(in *CreateVoteInput) { in.Quorum = 0 }, ErrInvalidProposal},
		{"quorum over total", func(in *CreateVoteInput) { in.Quorum = 1001 }, ErrInvalidProposal},
		{"negative start", func(in *CreateVoteInput) { in.StartAt = -1 }, ErrInvalidProposal},
		{"start equals deadline", func(in *CreateVoteInput) { in.StartAt = 200 }, ErrInvalidProposal},
		{"deadline after timelock", func(in *CreateVoteInput) { in.TimelockEnd = 199 }, ErrInvalidProposal},
		{"start zero ok boundary", func(in *CreateVoteInput) { in.StartAt = 0; in.Deadline = 1; in.TimelockEnd = 1 }, nil},
		{"self delegation", func(in *CreateVoteInput) {
			in.Delegations = append(in.Delegations, Delegation{From: "alice", To: "alice"})
		}, ErrInvalidProposal},
		{"unknown delegator", func(in *CreateVoteInput) { in.Delegations[0].From = "ghost" }, ErrInvalidProposal},
		{"unknown delegatee", func(in *CreateVoteInput) { in.Delegations[0].To = "ghost" }, ErrInvalidProposal},
		{"double delegation", func(in *CreateVoteInput) {
			in.Delegations = append(in.Delegations, Delegation{From: "bob", To: "dave"})
		}, ErrInvalidProposal},
		{"delegation cycle", func(in *CreateVoteInput) {
			in.Delegations = []Delegation{{From: "alice", To: "bob"}, {From: "bob", To: "alice"}}
		}, ErrInvalidProposal},
		{"delegation cycle length three", func(in *CreateVoteInput) {
			in.Delegations = []Delegation{{From: "alice", To: "bob"}, {From: "bob", To: "carol"}, {From: "carol", To: "alice"}}
		}, ErrInvalidProposal},
		{"total weight overflow", func(in *CreateVoteInput) {
			in.Members = []VoteMember{{ID: "a", Weight: math.MaxInt64}, {ID: "b", Weight: 1}}
		}, ErrInvalidProposal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := baseVoteInput("vc-" + strings.ReplaceAll(tc.name, " ", "-"))
			tc.mutate(in)
			_, existed, err := store.CreateVoteProposal(in)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || existed {
				t.Fatalf("expected rejection, existed=%v err=%v", existed, err)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err=%v, want %v", err, tc.want)
			}
		})
	}

	// 非法创建不得留下任何记录。
	if all, _ := store.VoteProposals(); len(all) != 1 {
		t.Fatalf("failed creates left records: %d", len(all))
	}
}

func TestCreateVoteIdempotentAndConflict(t *testing.T) {
	store, _ := openTempStore(t, 1000)
	defer store.Close()
	in := baseVoteInput("gip-v")
	mustCreateVote(t, store, in)

	// 相同编号、成员与委托换序、动作同序：视为完全相同。
	retry := baseVoteInput("gip-v")
	retry.Members = []VoteMember{
		{ID: "dave", Weight: 400}, {ID: "carol", Weight: 100},
		{ID: "bob", Weight: 200}, {ID: "alice", Weight: 300},
	}
	retry.Delegations = []Delegation{{From: "carol", To: "alice"}, {From: "bob", To: "alice"}}
	view, existed, err := store.CreateVoteProposal(retry)
	if err != nil || !existed {
		t.Fatalf("reordered retry existed=%v err=%v", existed, err)
	}
	if view.State != "voting" {
		t.Fatalf("retry changed state: %s", view.State)
	}

	diffs := map[string]func(*CreateVoteInput){
		"weight changed":    func(x *CreateVoteInput) { x.Members[0].Weight = 301 },
		"member changed":    func(x *CreateVoteInput) { x.Members[3].ID = "davina" },
		"quorum changed":    func(x *CreateVoteInput) { x.Quorum = 599 },
		"start changed":     func(x *CreateVoteInput) { x.StartAt = 99 },
		"deadline changed":  func(x *CreateVoteInput) { x.Deadline = 201 },
		"timelock changed":  func(x *CreateVoteInput) { x.TimelockEnd = 301 },
		"action text":       func(x *CreateVoteInput) { x.Actions = []string{"transfer:audits:99"} },
		"action order":      func(x *CreateVoteInput) { x.Actions = []string{"transfer:legal:1", "transfer:audits:100"} },
		"action removed":    func(x *CreateVoteInput) { x.Actions = nil },
		"delegation target": func(x *CreateVoteInput) { x.Delegations[0].To = "dave" },
		"delegation added":  func(x *CreateVoteInput) { x.Delegations = append(x.Delegations, Delegation{From: "dave", To: "alice"}) },
	}
	for name, mut := range diffs {
		t.Run(name, func(t *testing.T) {
			x := baseVoteInput("gip-v")
			mut(x)
			if _, _, err := store.CreateVoteProposal(x); !errors.Is(err, ErrProposalConflict) {
				t.Fatalf("err=%v, want ErrProposalConflict", err)
			}
		})
	}

	// register 与投票提案共用编号，互不覆盖。
	if _, err := store.Register("gip-v", 300, []string{"transfer:audits:100"}); !errors.Is(err, ErrProposalConflict) {
		t.Fatalf("register over voting proposal err=%v", err)
	}
	mustRegister(t, store, "gip-r", 300, "transfer:a:1")
	in2 := baseVoteInput("gip-r")
	if _, _, err := store.CreateVoteProposal(in2); !errors.Is(err, ErrProposalConflict) {
		t.Fatalf("create-vote over registered id err=%v", err)
	}
}

// memberPaths 抽取视图中每个成员的路径/直接委托对象/最终代表，便于按编号断言。
func memberPaths(v *VoteProposalView) map[string]MemberView {
	got := make(map[string]MemberView, len(v.Members))
	for _, m := range v.Members {
		got[m.ID] = MemberView{
			ID:       m.ID,
			Weight:   m.Weight,
			Path:     append([]string(nil), m.Path...),
			Delegate: m.Delegate,
			Direct:   m.Direct,
		}
	}
	return got
}

func TestDelegationPathNotShortenedByRosterOrder(t *testing.T) {
	// 复现缺陷场景：成员按 b、c、a、d 的顺序提交，委托 a→b、b→c、d→b。
	// a 与 d 的路径必须保留中间的 b，不得显示为直接到 c。
	store, _ := openTempStore(t, 1000)
	defer store.Close()
	in := &CreateVoteInput{
		ID:      "gip-ordered-chain",
		Members: []VoteMember{{ID: "b", Weight: 2}, {ID: "c", Weight: 4}, {ID: "a", Weight: 1}, {ID: "d", Weight: 8}},
		Delegations: []Delegation{
			{From: "a", To: "b"},
			{From: "b", To: "c"},
			{From: "d", To: "b"},
		},
		Quorum: 1, StartAt: 0, Deadline: 10, TimelockEnd: 10,
	}
	view := mustCreateVote(t, store, in)

	want := map[string]MemberView{
		"a": {ID: "a", Weight: 1, Path: []string{"a", "b", "c"}, Delegate: "c", Direct: "b"},
		"b": {ID: "b", Weight: 2, Path: []string{"b", "c"}, Delegate: "c", Direct: "c"},
		"c": {ID: "c", Weight: 4, Path: []string{"c"}, Delegate: "c", Direct: ""},
		"d": {ID: "d", Weight: 8, Path: []string{"d", "b", "c"}, Delegate: "c", Direct: "b"},
	}
	got := memberPaths(view)
	assertMemberViews(t, "create", got, want)

	// 成员列表仍按首次提交顺序展示，不被路径修正重排。
	if ids := memberIDs(view.Members); !equalStrings(ids, []string{"b", "c", "a", "d"}) {
		t.Fatalf("member order changed: %v", ids)
	}

	// 票重归集不变：c 代表全部 1+2+4+8=15；路径修正不改变最终代表与归集权重。
	ballot, err := store.CastVote("gip-ordered-chain", "c", true, 5)
	if err != nil {
		t.Fatalf("final representative vote: %v", err)
	}
	if ballot.Weight != 15 {
		t.Fatalf("group weight=%d, want 15", ballot.Weight)
	}
}

func TestDelegationPathsOrderIndependent(t *testing.T) {
	// 同一组权重与委托关系，无论成员及委托参数怎样排列，
	// 每名成员的完整路径、直接委托对象和最终代表都必须一致。
	spec := func(id string, members []VoteMember, delegations []Delegation) *CreateVoteInput {
		return &CreateVoteInput{
			ID: id, Members: members, Delegations: delegations,
			Quorum: 1, StartAt: 0, Deadline: 10, TimelockEnd: 10,
		}
	}
	// 两层链 a→b→c 与 d→b 汇入 c；另有互不相连的独立链 f→g，以及自代的 e。
	membersA := []VoteMember{
		{ID: "b", Weight: 2}, {ID: "c", Weight: 4}, {ID: "a", Weight: 1},
		{ID: "d", Weight: 8}, {ID: "e", Weight: 16}, {ID: "f", Weight: 32}, {ID: "g", Weight: 64},
	}
	delegationsA := []Delegation{
		{From: "d", To: "b"}, {From: "a", To: "b"}, {From: "f", To: "g"}, {From: "b", To: "c"},
	}
	membersB := []VoteMember{
		{ID: "g", Weight: 64}, {ID: "f", Weight: 32}, {ID: "e", Weight: 16},
		{ID: "d", Weight: 8}, {ID: "c", Weight: 4}, {ID: "a", Weight: 1}, {ID: "b", Weight: 2},
	}
	delegationsB := []Delegation{
		{From: "b", To: "c"}, {From: "f", To: "g"}, {From: "a", To: "b"}, {From: "d", To: "b"},
	}
	want := map[string]MemberView{
		"a": {ID: "a", Weight: 1, Path: []string{"a", "b", "c"}, Delegate: "c", Direct: "b"},
		"b": {ID: "b", Weight: 2, Path: []string{"b", "c"}, Delegate: "c", Direct: "c"},
		"c": {ID: "c", Weight: 4, Path: []string{"c"}, Delegate: "c", Direct: ""},
		"d": {ID: "d", Weight: 8, Path: []string{"d", "b", "c"}, Delegate: "c", Direct: "b"},
		"e": {ID: "e", Weight: 16, Path: []string{"e"}, Delegate: "e", Direct: ""},
		"f": {ID: "f", Weight: 32, Path: []string{"f", "g"}, Delegate: "g", Direct: "g"},
		"g": {ID: "g", Weight: 64, Path: []string{"g"}, Delegate: "g", Direct: ""},
	}

	store1, _ := openTempStore(t, 1000)
	defer store1.Close()
	v1 := mustCreateVote(t, store1, spec("gip-perm-1", membersA, delegationsA))
	assertMemberViews(t, "order A", memberPaths(v1), want)

	store2, _ := openTempStore(t, 1000)
	defer store2.Close()
	v2 := mustCreateVote(t, store2, spec("gip-perm-2", membersB, delegationsB))
	assertMemberViews(t, "order B", memberPaths(v2), want)

	// 同一提案换序重试：返回已有提案、不重排已保存成员、不产生新记录。
	retry := spec("gip-perm-1", membersB, delegationsB)
	again, existed, err := store1.CreateVoteProposal(retry)
	if err != nil || !existed {
		t.Fatalf("reordered retry existed=%v err=%v", existed, err)
	}
	if ids := memberIDs(again.Members); !equalStrings(ids, memberIDs(v1.Members)) {
		t.Fatalf("retry reordered stored members: %v want %v", ids, memberIDs(v1.Members))
	}
	all, _ := store1.VoteProposals()
	if len(all) != 1 {
		t.Fatalf("retry created an extra proposal record: %d", len(all))
	}
	assertMemberViews(t, "retry", memberPaths(again), want)

	// 更深的转交链：a→b→c→d→e(停)，每个中间成员都必须出现在上游路径中。
	deep := &CreateVoteInput{
		ID: "gip-deep",
		Members: []VoteMember{
			{ID: "e", Weight: 1}, {ID: "d", Weight: 1}, {ID: "c", Weight: 1},
			{ID: "b", Weight: 1}, {ID: "a", Weight: 1},
		},
		Delegations: []Delegation{
			{From: "a", To: "b"}, {From: "b", To: "c"}, {From: "c", To: "d"}, {From: "d", To: "e"},
		},
		Quorum: 1, StartAt: 0, Deadline: 10, TimelockEnd: 10,
	}
	store3, _ := openTempStore(t, 1000)
	defer store3.Close()
	dv := mustCreateVote(t, store3, deep)
	dgot := memberPaths(dv)
	for id, p := range map[string][]string{
		"a": {"a", "b", "c", "d", "e"},
		"b": {"b", "c", "d", "e"},
		"c": {"c", "d", "e"},
		"d": {"d", "e"},
		"e": {"e"},
	} {
		if !equalStrings(dgot[id].Path, p) {
			t.Errorf("deep member %s path=%v want %v", id, dgot[id].Path, p)
		}
		if dgot[id].Delegate != "e" {
			t.Errorf("deep member %s head=%s want e", id, dgot[id].Delegate)
		}
	}
}

func TestDelegationPathsSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "treasury.json")
	store, err := InitTreasury(path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	in := &CreateVoteInput{
		ID:      "gip-reopen-chain",
		Members: []VoteMember{{ID: "b", Weight: 2}, {ID: "c", Weight: 4}, {ID: "a", Weight: 1}, {ID: "d", Weight: 8}},
		Delegations: []Delegation{
			{From: "a", To: "b"}, {From: "b", To: "c"}, {From: "d", To: "b"},
		},
		Quorum: 15, StartAt: 0, Deadline: 10, TimelockEnd: 10,
		Actions: []string{"transfer:audits:15"},
	}
	mustCreateVote(t, store, in)
	if _, err := store.CastVote("gip-reopen-chain", "c", true, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TallyVote("gip-reopen-chain", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Execute("gip-reopen-chain", 10); err != nil {
		t.Fatal(err)
	}
	store.Close()

	// 关闭后重新打开：无须重新创建，多级委托路径直接完整显示，状态与记录保留。
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	v, ok, err := reopened.VoteProposal("gip-reopen-chain")
	if err != nil || !ok {
		t.Fatalf("query after reopen: %v ok=%v", err, ok)
	}
	want := map[string]MemberView{
		"a": {ID: "a", Weight: 1, Path: []string{"a", "b", "c"}, Delegate: "c", Direct: "b"},
		"b": {ID: "b", Weight: 2, Path: []string{"b", "c"}, Delegate: "c", Direct: "c"},
		"c": {ID: "c", Weight: 4, Path: []string{"c"}, Delegate: "c", Direct: ""},
		"d": {ID: "d", Weight: 8, Path: []string{"d", "b", "c"}, Delegate: "c", Direct: "b"},
	}
	assertMemberViews(t, "reopen", memberPaths(v), want)
	if v.State != "executed" || v.Tally == nil || !v.Tally.Passed {
		t.Fatalf("state/tally after reopen: state=%s tally=%+v", v.State, v.Tally)
	}
	if len(v.Ballots) != 1 || v.Ballots[0].Representative != "c" || v.Ballots[0].Weight != 15 {
		t.Fatalf("ballot after reopen: %+v", v.Ballots)
	}
	if r, ok, _ := reopened.Receipt("gip-reopen-chain"); !ok || r.ExecutedAt != 10 {
		t.Fatalf("receipt after reopen: %+v ok=%v", r, ok)
	}
}

func memberIDs(ms []MemberView) []string {
	ids := make([]string, len(ms))
	for i, m := range ms {
		ids[i] = m.ID
	}
	return ids
}

func assertMemberViews(t *testing.T, prefix string, got, want map[string]MemberView) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: member count=%d want %d", prefix, len(got), len(want))
	}
	for id, w := range want {
		g, ok := got[id]
		if !ok {
			t.Errorf("%s: member %s missing from view", prefix, id)
			continue
		}
		if !equalStrings(g.Path, w.Path) {
			t.Errorf("%s: member %s path=%v want %v", prefix, id, g.Path, w.Path)
		}
		// 路径的相邻成员必须对应一次真实委托；无重复成员；首尾为本人和最终代表。
		if len(g.Path) == 0 || g.Path[0] != id || g.Path[len(g.Path)-1] != g.Delegate {
			t.Errorf("%s: member %s path %v inconsistent with self/final representative %q", prefix, id, g.Path, g.Delegate)
		}
		if g.Delegate != w.Delegate {
			t.Errorf("%s: member %s delegate=%q want %q", prefix, id, g.Delegate, w.Delegate)
		}
		if g.Direct != w.Direct {
			t.Errorf("%s: member %s direct_to=%q want %q", prefix, id, g.Direct, w.Direct)
		}
		if g.Weight != w.Weight {
			t.Errorf("%s: member %s weight=%d want %d", prefix, id, g.Weight, w.Weight)
		}
	}
}

func TestDelegationChains(t *testing.T) {
	store, _ := openTempStore(t, 1000)
	defer store.Close()
	in := &CreateVoteInput{
		ID:      "gip-chain",
		Members: []VoteMember{{ID: "a", Weight: 1}, {ID: "b", Weight: 2}, {ID: "c", Weight: 4}, {ID: "d", Weight: 8}, {ID: "e", Weight: 16}},
		// a -> b -> c(停止)；d -> c；e 自代。
		Delegations: []Delegation{{From: "a", To: "b"}, {From: "b", To: "c"}, {From: "d", To: "c"}},
		Quorum:      15, StartAt: 0, Deadline: 10, TimelockEnd: 10,
	}
	view := mustCreateVote(t, store, in)
	wantPath := map[string][]string{
		"a": {"a", "b", "c"},
		"b": {"b", "c"},
		"c": {"c"},
		"d": {"d", "c"},
		"e": {"e"},
	}
	wantHead := map[string]string{"a": "c", "b": "c", "c": "c", "d": "c", "e": "e"}
	for _, m := range view.Members {
		if !equalStrings(m.Path, wantPath[m.ID]) {
			t.Errorf("member %s path=%v want %v", m.ID, m.Path, wantPath[m.ID])
		}
		if m.Delegate != wantHead[m.ID] {
			t.Errorf("member %s head=%s want %s", m.ID, m.Delegate, wantHead[m.ID])
		}
	}

	// 已委托出去的成员不能直接投票；最终代表 c 归集 1+2+4+8=15。
	if _, err := store.CastVote("gip-chain", "a", true, 5); !errors.Is(err, ErrVoteRejected) {
		t.Fatalf("delegated member voted: %v", err)
	}
	if _, err := store.CastVote("gip-chain", "ghost", true, 5); !errors.Is(err, ErrVoteRejected) {
		t.Fatalf("non-member voted: %v", err)
	}
	b, err := store.CastVote("gip-chain", "c", true, 5)
	if err != nil {
		t.Fatalf("representative vote: %v", err)
	}
	if b.Weight != 15 {
		t.Fatalf("group weight=%d, want 15", b.Weight)
	}
	b2, err := store.CastVote("gip-chain", "c", true, 9) // 窗口外重试
	if err != nil || b2.VotedAt != 5 || b2.Weight != 15 {
		t.Fatalf("identical retry: %+v err=%v", b2, err)
	}
	if _, err := store.CastVote("gip-chain", "c", false, 5); !errors.Is(err, ErrProposalConflict) {
		t.Fatalf("changing vote err=%v", err)
	}
}

func equalStrings(a, b []string) bool {
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

func TestVotingWindowAndTallyRules(t *testing.T) {
	store, _ := openTempStore(t, 1000)
	defer store.Close()
	in := baseVoteInput("gip-t")
	mustCreateVote(t, store, in) // alice 代表 600（含 bob、carol），dave 400；quorum 600

	// 窗口外投票。
	if _, err := store.CastVote("gip-t", "alice", true, 99); !errors.Is(err, ErrVoteRejected) {
		t.Fatalf("vote before start: %v", err)
	}
	if _, err := store.CastVote("gip-t", "dave", false, 200); !errors.Is(err, ErrVoteRejected) {
		t.Fatalf("vote at deadline (exclusive): %v", err)
	}
	if _, err := store.CastVote("gip-t", "alice", true, 100); err != nil {
		t.Fatalf("vote at start (inclusive): %v", err)
	}

	// 截止前计票拒绝，状态不变。
	if _, err := store.TallyVote("gip-t", 199); !errors.Is(err, ErrTallyRejected) {
		t.Fatalf("early tally: %v", err)
	}
	v, _, _ := store.VoteProposal("gip-t")
	if v.State != "voting" || v.Tally != nil {
		t.Fatalf("state changed after rejected tally: %s %+v", v.State, v.Tally)
	}

	// 只有 alice 的 600：达到法定人数且赞成严格大于反对 => 通过。
	res, err := store.TallyVote("gip-t", 200)
	if err != nil {
		t.Fatalf("tally: %v", err)
	}
	if !res.Passed || res.ForWeight != 600 || res.AgainstWeight != 0 || res.Turnout != 600 {
		t.Fatalf("unexpected tally: %+v", res)
	}
	// 计票后拒绝新票，即使传入窗口内时间。
	if _, err := store.CastVote("gip-t", "dave", false, 150); !errors.Is(err, ErrVoteRejected) {
		t.Fatalf("vote after tally: %v", err)
	}
	// 再次计票返回首次结论与首次时间。
	res2, err := store.TallyVote("gip-t", 999)
	if err != nil || res2.TalliedAt != 200 || res2.ForWeight != 600 {
		t.Fatalf("repeat tally: %+v err=%v", res2, err)
	}
}

func TestTallyQuorumAndTie(t *testing.T) {
	cases := []struct {
		name    string
		quorum  int64
		ballots []struct {
			voter   string
			support bool
		}
		passed bool
	}{
		{"quorum met strict majority", 600, []struct {
			voter   string
			support bool
		}{{"alice", true}, {"dave", false}}, true}, // 600 vs 400
		{"full turnout still majority", 1000, []struct {
			voter   string
			support bool
		}{{"alice", true}, {"dave", false}}, true},
		{"turnout below quorum rejects", 1000, []struct {
			voter   string
			support bool
		}{{"alice", true}}, false}, // 参与 600 < 1000
		{"only against votes", 1, []struct {
			voter   string
			support bool
		}{{"dave", false}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, _ := openTempStore(t, 1000)
			defer store.Close()
			in := baseVoteInput("gip-q-" + strings.ReplaceAll(tc.name, " ", "-"))
			in.Quorum = tc.quorum
			mustCreateVote(t, store, in)
			for _, b := range tc.ballots {
				if _, err := store.CastVote(in.ID, b.voter, b.support, 150); err != nil {
					t.Fatalf("vote: %v", err)
				}
			}
			res, err := store.TallyVote(in.ID, 200)
			if err != nil {
				t.Fatalf("tally: %v", err)
			}
			if res.Passed != tc.passed {
				t.Fatalf("passed=%v want %v (for=%d against=%d turnout=%d quorum=%d)",
					res.Passed, tc.passed, res.ForWeight, res.AgainstWeight, res.Turnout, res.Quorum)
			}
		})
	}

	// 平票：对称成员各 500。
	store, _ := openTempStore(t, 1000)
	defer store.Close()
	in := &CreateVoteInput{
		ID: "gip-tie", Members: []VoteMember{{ID: "x", Weight: 500}, {ID: "y", Weight: 500}},
		Quorum: 1000, StartAt: 0, Deadline: 10, TimelockEnd: 10,
	}
	mustCreateVote(t, store, in)
	if _, err := store.CastVote("gip-tie", "x", true, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CastVote("gip-tie", "y", false, 5); err != nil {
		t.Fatal(err)
	}
	res, err := store.TallyVote("gip-tie", 10)
	if err != nil || res.Passed {
		t.Fatalf("tie must reject: %+v err=%v", res, err)
	}
	v, _, _ := store.VoteProposal("gip-tie")
	if v.State != "rejected" {
		t.Fatalf("state=%s, want rejected", v.State)
	}

	// 未投权重不计入参与量：1000 总权、quorum 700，仅 dave 400 反对 => 参与不足拒绝。
	store2, _ := openTempStore(t, 1000)
	defer store2.Close()
	in2 := baseVoteInput("gip-low")
	in2.Quorum = 700
	mustCreateVote(t, store2, in2)
	if _, err := store2.CastVote("gip-low", "dave", false, 150); err != nil {
		t.Fatal(err)
	}
	res2, err := store2.TallyVote("gip-low", 200)
	if err != nil || res2.Passed || res2.Turnout != 400 {
		t.Fatalf("low turnout: %+v err=%v", res2, err)
	}
}

func TestVotedProposalExecutesThroughExecute(t *testing.T) {
	store, path := openTempStore(t, 1000)
	in := baseVoteInput("gip-exec")
	mustCreateVote(t, store, in)
	if _, err := store.CastVote("gip-exec", "alice", true, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TallyVote("gip-exec", 200); err != nil {
		t.Fatal(err)
	}
	// rejected/passed 之外：passed 但时间锁未到。
	if _, err := store.Execute("gip-exec", 299); !errors.Is(err, ErrExecutionRejected) {
		t.Fatalf("execute before timelock: %v", err)
	}
	rcpt, err := store.Execute("gip-exec", 300)
	if err != nil {
		t.Fatalf("execute passed vote proposal: %v", err)
	}
	if rcpt.ExecutedAt != 300 || len(rcpt.Actions) != 1 {
		t.Fatalf("bad receipt: %+v", rcpt)
	}
	if bal, _ := store.TreasuryBalance(); bal != 900 {
		t.Fatalf("treasury=%d", bal)
	}
	// 已执行提案不退回通过；重试返回首次凭据。
	again, err := store.Execute("gip-exec", 99999)
	if err != nil || again.ExecutedAt != 300 {
		t.Fatalf("retry execute: %+v err=%v", again, err)
	}
	v, _, _ := store.VoteProposal("gip-exec")
	if v.State != "executed" {
		t.Fatalf("state=%s", v.State)
	}
	store.Close()

	// 重开后状态完整、结论保留。
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	v2, ok, err := reopened.VoteProposal("gip-exec")
	if err != nil || !ok || v2.State != "executed" || v2.Tally == nil || !v2.Tally.Passed {
		t.Fatalf("reopened view: %+v ok=%v err=%v", v2, ok, err)
	}
	if r, ok, _ := reopened.Receipt("gip-exec"); !ok || r.ExecutedAt != 300 {
		t.Fatalf("receipt after reopen: %+v %v", r, ok)
	}

	// rejected 的投票提案不能执行。
	bad, _ := openTempStore(t, 100)
	defer bad.Close()
	in2 := baseVoteInput("gip-rej")
	mustCreateVote(t, bad, in2)
	if _, err := bad.CastVote("gip-rej", "dave", false, 150); err != nil {
		t.Fatal(err)
	}
	if _, err := bad.TallyVote("gip-rej", 200); err != nil {
		t.Fatal(err)
	}
	if _, err := bad.Execute("gip-rej", 300); !errors.Is(err, ErrExecutionRejected) {
		t.Fatalf("execute rejected proposal: %v", err)
	}
}

func TestConcurrentVotingAndTally(t *testing.T) {
	store, _ := openTempStore(t, 1000)
	defer store.Close()
	in := baseVoteInput("gip-conc")
	mustCreateVote(t, store, in)

	// 两名代表在同一时刻反复投同一选择；只能各成功一次且票据不重复。
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	start := make(chan struct{})
	vote := func(voter string, support bool, now int64) {
		defer wg.Done()
		<-start
		if _, err := store.CastVote("gip-conc", voter, support, int64(now)); err != nil {
			errs <- err
		}
	}
	for i := 0; i < 16; i++ {
		wg.Add(2)
		go vote("alice", true, 150)
		go vote("dave", false, 150)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent identical vote: %v", err)
	}
	v, _, _ := store.VoteProposal("gip-conc")
	if len(v.Ballots) != 2 {
		t.Fatalf("ballot count=%d, want 2", len(v.Ballots))
	}

	// 计票与投票并发：只有先于计票成功的票进入结论；二者由同一把锁串行。
	in2 := baseVoteInput("gip-conc2")
	in2.Quorum = 1000
	mustCreateVote(t, store, in2)
	var wg2 sync.WaitGroup
	start2 := make(chan struct{})
	wg2.Add(1)
	go func() { // 迟到的票：可能在计票前或后
		defer wg2.Done()
		<-start2
		_, _ = store.CastVote("gip-conc2", "dave", false, 150)
	}()
	wg2.Add(1)
	go func() {
		defer wg2.Done()
		<-start2
		_, _ = store.TallyVote("gip-conc2", 200)
	}()
	close(start2)
	wg2.Wait()
	v2, _, _ := store.VoteProposal("gip-conc2")
	if v2.Tally == nil {
		t.Fatal("proposal not tallied")
	}
	// 结论必须能由已保存票据重放（打开/提交时的严格校验兜底；这里再显式断言）。
	var forW, againstW int64
	for _, b := range v2.Ballots {
		if b.Support {
			forW += b.Weight
		} else {
			againstW += b.Weight
		}
	}
	if forW != v2.Tally.ForWeight || againstW != v2.Tally.AgainstWeight {
		t.Fatalf("tally %+v inconsistent with ballots for=%d against=%d", v2.Tally, forW, againstW)
	}
}

func TestVoteProposalNotFound(t *testing.T) {
	store, _ := openTempStore(t, 1000)
	defer store.Close()
	if _, _, err := store.VoteProposal("nope"); err != nil {
		t.Fatalf("query missing: %v", err)
	}
	if _, err := store.CastVote("nope", "alice", true, 0); !errors.Is(err, ErrProposalNotFound) {
		t.Fatalf("vote missing: %v", err)
	}
	if _, err := store.TallyVote("nope", 0); !errors.Is(err, ErrProposalNotFound) {
		t.Fatalf("tally missing: %v", err)
	}
}

func TestVoteOrderPreservedAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "treasury.json")
	store, err := InitTreasury(path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	in := baseVoteInput("gip-order")
	in.Quorum = 400
	mustCreateVote(t, store, in)
	if _, err := store.CastVote("gip-order", "dave", false, 120); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CastVote("gip-order", "alice", true, 130); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TallyVote("gip-order", 200); err != nil {
		t.Fatal(err)
	}
	store.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	v, ok, err := reopened.VoteProposal("gip-order")
	if err != nil || !ok {
		t.Fatalf("query: %v %v", err, ok)
	}
	// 票据按首次投票先后，而非编号或权重。
	if len(v.Ballots) != 2 || v.Ballots[0].Representative != "dave" || v.Ballots[1].Representative != "alice" {
		t.Fatalf("ballot order: %+v", v.Ballots)
	}
	if v.Ballots[0].VotedAt != 120 || v.Ballots[1].VotedAt != 130 {
		t.Fatalf("vote timestamps: %+v", v.Ballots)
	}
	if v.Tally == nil || v.Tally.TalliedAt != 200 || !v.Tally.Passed {
		t.Fatalf("tally after reopen: %+v", v.Tally)
	}
}

func TestOldStateFileStillOpens(t *testing.T) {
	// 手工构造一个不含 vote_proposals 字段的 v1 旧状态文件，必须原样可读。
	dir := t.TempDir()
	path := filepath.Join(dir, "treasury.json")
	old := `{
  "magic": "govflow-treasury-state",
  "version": 1,
  "initial_treasury": 100,
  "treasury": 100,
  "balances": {},
  "proposals": {"gip-old": {"id": "gip-old", "state": "passed", "timelock_end": 0, "actions": ["transfer:audits:10"]}},
  "receipts": []
}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatalf("old file rejected: %v", err)
	}
	defer store.Close()
	p, ok, err := store.Proposal("gip-old")
	if err != nil || !ok || p.State != "passed" {
		t.Fatalf("old proposal: %+v ok=%v err=%v", p, ok, err)
	}
	// 在旧文件上可以新增投票提案（缺省表在首次提交时被正常化）。
	in := baseVoteInput("gip-new")
	mustCreateVote(t, store, in)
	if _, err := store.Execute("gip-old", 0); err != nil {
		t.Fatalf("execute old proposal: %v", err)
	}
}

func TestTamperedVoteStateRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "treasury.json")
	store, err := InitTreasury(path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	in := baseVoteInput("gip-tamper")
	mustCreateVote(t, store, in)
	if _, err := store.CastVote("gip-tamper", "alice", true, 150); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TallyVote("gip-tamper", 200); err != nil {
		t.Fatal(err)
	}
	store.Close()

	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// 用结构化改写构造损坏变体，避免依赖具体缩进。
	mutate := func(fn func(p map[string]any)) []byte {
		var clone map[string]any
		if err := json.Unmarshal(good, &clone); err != nil {
			t.Fatal(err)
		}
		p := clone["vote_proposals"].(map[string]any)["gip-tamper"].(map[string]any)
		fn(p)
		raw, err := json.MarshalIndent(clone, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		return append(raw, '\n')
	}
	cases := map[string][]byte{
		"ballot weight mismatch": mutate(func(p map[string]any) {
			p["ballots"].([]any)[0].(map[string]any)["weight"] = 601
		}),
		"ballot from non-head": mutate(func(p map[string]any) {
			p["ballots"].([]any)[0].(map[string]any)["representative"] = "bob"
		}),
		"duplicate ballot rep": mutate(func(p map[string]any) {
			extra := map[string]any{"representative": "alice", "weight": 600, "support": true, "voted_at": 151}
			p["ballots"] = append(p["ballots"].([]any), extra)
		}),
		"tally before deadline": mutate(func(p map[string]any) {
			p["tally"].(map[string]any)["tallied_at"] = 199
		}),
		"tally disagrees with ballots": mutate(func(p map[string]any) {
			p["tally"].(map[string]any)["for_weight"] = 601
		}),
		"unknown vote state": mutate(func(p map[string]any) {
			p["state"] = "approved"
		}),
		"quorum out of range": mutate(func(p map[string]any) {
			p["quorum"] = 1001
		}),
	}
	// id collision：在 register 来源表里注入同一编号。
	collision := mutate(func(p map[string]any) {})
	var cd map[string]any
	if err := json.Unmarshal(collision, &cd); err != nil {
		t.Fatal(err)
	}
	cd["proposals"] = map[string]any{"gip-tamper": map[string]any{
		"id": "gip-tamper", "state": "passed", "timelock_end": 300,
		"actions": []any{"transfer:audits:100"},
	}}
	if raw, err := json.MarshalIndent(cd, "", "  "); err != nil {
		t.Fatal(err)
	} else {
		cases["id collision"] = append(raw, '\n')
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			bad := filepath.Join(dir, "bad-"+strings.ReplaceAll(name, " ", "-")+".json")
			if err := os.WriteFile(bad, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			s, err := Open(bad)
			if !errors.Is(err, ErrStateCorrupt) {
				if s != nil {
					s.Close()
				}
				t.Fatalf("Open err=%v, want ErrStateCorrupt", err)
			}
			if got, rerr := os.ReadFile(bad); rerr != nil || string(got) != string(raw) {
				t.Fatalf("corrupt file was modified")
			}
		})
	}
}

func TestCreateVoteBoundaryTimesAndQuorum(t *testing.T) {
	store, _ := openTempStore(t, 1000)
	defer store.Close()

	// 合法边界：start=0，deadline==timelock。
	ok := &CreateVoteInput{
		ID: "gip-bound-ok", Members: []VoteMember{{ID: "a", Weight: math.MaxInt64}},
		Quorum: math.MaxInt64, StartAt: 0, Deadline: 1, TimelockEnd: 1,
	}
	if _, existed, err := store.CreateVoteProposal(ok); err != nil || existed {
		t.Fatalf("boundary-legal create: err=%v existed=%v", err, existed)
	}

	// start==deadline 必须拒绝。
	bad := *ok
	bad.ID = "gip-bound-bad1"
	bad.StartAt, bad.Deadline, bad.TimelockEnd = 5, 5, 5
	if _, _, err := store.CreateVoteProposal(&bad); !errors.Is(err, ErrInvalidProposal) {
		t.Fatalf("start==deadline err=%v", err)
	}
	// deadline > timelock 必须拒绝。
	bad.ID = "gip-bound-bad2"
	bad.StartAt, bad.Deadline, bad.TimelockEnd = 0, 2, 1
	if _, _, err := store.CreateVoteProposal(&bad); !errors.Is(err, ErrInvalidProposal) {
		t.Fatalf("deadline>timelock err=%v", err)
	}
	// 单成员持有 MaxInt64：quorum 可等于总权重；quorum 超过总权重拒绝。
	bad.ID = "gip-bound-bad3"
	bad.StartAt, bad.Deadline, bad.TimelockEnd = 0, 1, 1
	bad.Quorum = math.MinInt64
	if _, _, err := store.CreateVoteProposal(&bad); !errors.Is(err, ErrInvalidProposal) {
		t.Fatalf("negative quorum err=%v", err)
	}
}

func TestProposalsSnapshotContentsAndCopies(t *testing.T) {
	store, _ := openTempStore(t, 1000)
	defer store.Close()

	// 空状态：两个列表都为空且非 nil。
	snap, err := store.ProposalsSnapshot()
	if err != nil {
		t.Fatalf("ProposalsSnapshot: %v", err)
	}
	if snap.Registered == nil || snap.Voting == nil {
		t.Fatalf("empty snapshot lists must be non-nil: %+v", snap)
	}
	if len(snap.Registered) != 0 || len(snap.Voting) != 0 {
		t.Fatalf("empty snapshot = %+v", snap)
	}

	mustRegister(t, store, "gip-reg-b", 0, "transfer:audits:100")
	mustRegister(t, store, "gip-reg-a", 0, "transfer:legal:50")
	in := baseVoteInput("gip-vote-b")
	mustCreateVote(t, store, in)
	in2 := baseVoteInput("gip-vote-a")
	mustCreateVote(t, store, in2)
	if _, err := store.CastVote("gip-vote-a", "alice", true, 100); err != nil {
		t.Fatalf("CastVote: %v", err)
	}
	if _, err := store.TallyVote("gip-vote-a", 200); err != nil {
		t.Fatalf("TallyVote: %v", err)
	}

	snap, err = store.ProposalsSnapshot()
	if err != nil {
		t.Fatalf("ProposalsSnapshot: %v", err)
	}
	// 各来源内部按编号排序。
	if len(snap.Registered) != 2 || snap.Registered[0].ID != "gip-reg-a" || snap.Registered[1].ID != "gip-reg-b" {
		t.Fatalf("registered order = %+v", snap.Registered)
	}
	if len(snap.Voting) != 2 || snap.Voting[0].ID != "gip-vote-a" || snap.Voting[1].ID != "gip-vote-b" {
		t.Fatalf("voting order = %+v", snap.Voting)
	}
	// 投票提案保留成员、委托路径、逐票与计票结论。
	va := snap.Voting[0]
	if va.State != "passed" || va.Tally == nil || !va.Tally.Passed || va.Tally.ForWeight != 600 {
		t.Fatalf("tallied view = %+v", va)
	}
	if len(va.Members) != 4 || va.Members[1].ID != "bob" || va.Members[1].Delegate != "alice" ||
		len(va.Members[1].Path) != 2 || va.Members[1].Path[0] != "bob" || va.Members[1].Path[1] != "alice" {
		t.Fatalf("member delegation view = %+v", va.Members)
	}
	if len(va.Ballots) != 1 || va.Ballots[0].Representative != "alice" || va.Ballots[0].Weight != 600 {
		t.Fatalf("ballots = %+v", va.Ballots)
	}

	// 返回的是副本：改写快照不影响后续查询。
	snap.Registered[0].State = "executed"
	snap.Voting[0].State = "executed"
	again, err := store.ProposalsSnapshot()
	if err != nil {
		t.Fatalf("ProposalsSnapshot: %v", err)
	}
	if again.Registered[0].State != "passed" || again.Voting[0].State != "passed" {
		t.Fatalf("snapshot shares state with store: %+v", again)
	}
}

func TestProposalsSnapshotConsistentUnderConcurrentExecute(t *testing.T) {
	store, _ := openTempStore(t, 1000)
	defer store.Close()
	// 登记提案与投票提案最初都为 passed 且时间锁已到期；
	// 执行方先执行登记提案，再执行投票提案。快照只允许出现
	// (passed,passed)、(executed,passed)、(executed,executed) 三种完整组合，
	// 不得出现登记提案仍为 passed 而投票提案已 executed 的混杂状态。
	mustRegister(t, store, "gip-reg", 0, "transfer:audits:100")
	mustCreateVote(t, store, baseVoteInput("gip-vote"))
	if _, err := store.CastVote("gip-vote", "alice", true, 100); err != nil {
		t.Fatalf("CastVote: %v", err)
	}
	if _, err := store.TallyVote("gip-vote", 200); err != nil {
		t.Fatalf("TallyVote: %v", err)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		_, _ = store.Execute("gip-reg", 300)
		_, _ = store.Execute("gip-vote", 300)
		close(stop)
	}()
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for {
				select {
				case <-stop:
					return
				default:
				}
				snap, err := store.ProposalsSnapshot()
				if err != nil {
					t.Errorf("ProposalsSnapshot: %v", err)
					return
				}
				if len(snap.Registered) != 1 || len(snap.Voting) != 1 {
					t.Errorf("snapshot sizes = %d/%d, want 1/1", len(snap.Registered), len(snap.Voting))
					return
				}
				reg := snap.Registered[0].State
				vote := snap.Voting[0].State
				if reg == "passed" && vote == "executed" {
					t.Errorf("mixed committed states: registered=%s voting=%s", reg, vote)
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
}

// TestTallyAtInt64WeightBoundary：合法成员总权重恰好为有符号 64 位整数上限时，
// 首次计票、查询结论与重开后的汇总/结论都不能因数值边界改变：
// 参与量恰好等于法定人数（MaxInt64）且赞成严格多于反对即通过；
// 无票时参与量为 0，绝不能把成员总权重当成参与量。
func TestTallyAtInt64WeightBoundary(t *testing.T) {
	max := int64(math.MaxInt64)

	// 单成员持有全部权重且赞成：for=MaxInt64、against=0、turnout=MaxInt64
	// 恰好达到法定人数 MaxInt64 => 通过。
	t.Run("single member exactly quorum", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "treasury.json")
		store, err := InitTreasury(path, 0)
		if err != nil {
			t.Fatal(err)
		}
		in := &CreateVoteInput{
			ID: "gip-max-one", Members: []VoteMember{{ID: "a", Weight: max}},
			Quorum: max, StartAt: 0, Deadline: 10, TimelockEnd: 10,
		}
		mustCreateVote(t, store, in)
		if _, err := store.CastVote("gip-max-one", "a", true, 5); err != nil {
			t.Fatal(err)
		}
		res, err := store.TallyVote("gip-max-one", 10)
		if err != nil {
			t.Fatalf("tally: %v", err)
		}
		if !res.Passed || res.ForWeight != max || res.AgainstWeight != 0 || res.Turnout != max || res.Quorum != max {
			t.Fatalf("boundary tally = %+v, want passed for=turnout=quorum=MaxInt64", res)
		}
		store.Close()

		// 重开后查询结论与首次计票完全一致。
		reopened, err := Open(path)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer reopened.Close()
		v, ok, err := reopened.VoteProposal("gip-max-one")
		if err != nil || !ok {
			t.Fatalf("query: %v ok=%v", err, ok)
		}
		if v.State != "passed" || v.Tally == nil || !v.Tally.Passed ||
			v.Tally.ForWeight != max || v.Tally.Turnout != max || v.Tally.AgainstWeight != 0 ||
			v.Tally.TalliedAt != 10 || v.TotalWeight != max {
			t.Fatalf("reopened boundary view = state=%s tally=%+v total=%d", v.State, v.Tally, v.TotalWeight)
		}
	})

	// 总权重 MaxInt64 拆为 MaxInt64-1（赞成）与 1（反对）：参与量恰好到上限、
	// 赞成严格多于反对 => 通过；此场景覆盖两侧相加恰好等于 MaxInt64 的边界。
	t.Run("split weights turnout exactly max", func(t *testing.T) {
		store, _ := openTempStore(t, 0)
		defer store.Close()
		in := &CreateVoteInput{
			ID: "gip-max-split",
			Members: []VoteMember{
				{ID: "a", Weight: max - 1},
				{ID: "b", Weight: 1},
			},
			Quorum: max, StartAt: 0, Deadline: 10, TimelockEnd: 10,
		}
		mustCreateVote(t, store, in)
		if _, err := store.CastVote("gip-max-split", "a", true, 5); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CastVote("gip-max-split", "b", false, 6); err != nil {
			t.Fatal(err)
		}
		res, err := store.TallyVote("gip-max-split", 10)
		if err != nil {
			t.Fatalf("tally: %v", err)
		}
		if !res.Passed || res.ForWeight != max-1 || res.AgainstWeight != 1 || res.Turnout != max {
			t.Fatalf("split boundary tally = %+v, want passed turnout=MaxInt64", res)
		}
	})

	// 没有任何人投票：参与量必须是 0，而不是成员总权重 MaxInt64；即使法定人数为 1 也拒绝。
	t.Run("no ballots turnout zero not total", func(t *testing.T) {
		store, _ := openTempStore(t, 0)
		defer store.Close()
		in := &CreateVoteInput{
			ID: "gip-max-novote", Members: []VoteMember{{ID: "a", Weight: max}},
			Quorum: 1, StartAt: 0, Deadline: 10, TimelockEnd: 10,
		}
		mustCreateVote(t, store, in)
		res, err := store.TallyVote("gip-max-novote", 10)
		if err != nil {
			t.Fatalf("tally: %v", err)
		}
		if res.Passed || res.ForWeight != 0 || res.AgainstWeight != 0 || res.Turnout != 0 {
			t.Fatalf("no-ballot tally = %+v, want rejected with zero turnout", res)
		}
		v, _, _ := store.VoteProposal("gip-max-novote")
		if v.State != "rejected" || v.Tally == nil || v.Tally.Passed {
			t.Fatalf("no-ballot query state=%s tally=%+v, want rejected/not-passed", v.State, v.Tally)
		}
	})
}

// TestRepeatTallyAfterExecutionIgnoresEarlyNow：已有结论后，即使再次传入截止前
// 时间，也返回首次结论与首次计票时间；提案后来已执行时，状态不得退回 passed。
func TestRepeatTallyAfterExecutionIgnoresEarlyNow(t *testing.T) {
	store, _ := openTempStore(t, 1000)
	defer store.Close()
	mustCreateVote(t, store, baseVoteInput("gip-again"))
	if _, err := store.CastVote("gip-again", "alice", true, 100); err != nil {
		t.Fatal(err)
	}
	first, err := store.TallyVote("gip-again", 200)
	if err != nil || !first.Passed || first.TalliedAt != 200 {
		t.Fatalf("first tally: %+v err=%v", first, err)
	}
	// 执行后状态变为 executed。
	if _, err := store.Execute("gip-again", 300); err != nil {
		t.Fatalf("execute: %v", err)
	}

	// 传入截止前时间再次计票：不是“时间未到”，而是返回首次结论与首次时间。
	early, err := store.TallyVote("gip-again", 150)
	if err != nil {
		t.Fatalf("repeat tally with early now must not error: %v", err)
	}
	if !early.Passed || early.TalliedAt != 200 || early.ForWeight != 600 || early.Turnout != 600 {
		t.Fatalf("repeat tally = %+v, want first result tallied_at=200", early)
	}
	v, _, _ := store.VoteProposal("gip-again")
	if v.State != "executed" {
		t.Fatalf("state regressed to %s, want executed", v.State)
	}
	if v.Tally == nil || !v.Tally.Passed || v.Tally.TalliedAt != 200 {
		t.Fatalf("query tally after execution = %+v", v.Tally)
	}

	// 截止前计票若发生在首次之前仍应明确报时间未到（对照另一项未计票提案）。
	mustCreateVote(t, store, baseVoteInput("gip-fresh"))
	if _, err := store.TallyVote("gip-fresh", 199); !errors.Is(err, ErrTallyRejected) {
		t.Fatalf("first early tally err=%v, want ErrTallyRejected", err)
	}
	fv, _, _ := store.VoteProposal("gip-fresh")
	if fv.State != "voting" || fv.Tally != nil {
		t.Fatalf("rejected early tally changed state: %s %+v", fv.State, fv.Tally)
	}
}

// TestTamperedTallyConclusionKeepsFile：保存的赞成/反对权重或提案状态与票据
// 重放不一致时，必须作为状态损坏拒绝打开，错误文本能区分原因，且原文件保留。
func TestTamperedTallyConclusionKeepsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "treasury.json")
	store, err := InitTreasury(path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	mustCreateVote(t, store, baseVoteInput("gip-ctamper"))
	if _, err := store.CastVote("gip-ctamper", "alice", true, 150); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TallyVote("gip-ctamper", 200); err != nil {
		t.Fatal(err)
	}
	store.Close()

	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	mutate := func(fn func(p map[string]any)) []byte {
		var clone map[string]any
		if err := json.Unmarshal(good, &clone); err != nil {
			t.Fatal(err)
		}
		p := clone["vote_proposals"].(map[string]any)["gip-ctamper"].(map[string]any)
		fn(p)
		raw, err := json.MarshalIndent(clone, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		return append(raw, '\n')
	}

	cases := map[string]struct {
		raw     []byte
		wantSub string
	}{
		// 票据是 600 赞成、法定人数 600，本应 passed；状态被改成 rejected。
		"state contradicts ballots": {
			mutate(func(p map[string]any) { p["state"] = "rejected" }),
			"does not match quorum/majority replay",
		},
		// 保存的反对权重与票据重放不符（票据里没有反对票）。
		"tally against weight disagrees": {
			mutate(func(p map[string]any) {
				p["tally"].(map[string]any)["against_weight"] = 1
			}),
			"do not replay ballots",
		},
		// 代表票重与委托后归集权重不符，走同一份票重核对规则。
		"delegated ballot weight disagrees": {
			mutate(func(p map[string]any) {
				p["ballots"].([]any)[0].(map[string]any)["weight"] = 599
			}),
			"does not match roster delegation",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			bad := filepath.Join(dir, "bad-"+strings.ReplaceAll(name, " ", "-")+".json")
			if err := os.WriteFile(bad, tc.raw, 0o600); err != nil {
				t.Fatal(err)
			}
			s, err := Open(bad)
			if !errors.Is(err, ErrStateCorrupt) {
				if s != nil {
					s.Close()
				}
				t.Fatalf("Open err=%v, want ErrStateCorrupt", err)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not distinguish cause; want substring %q", err.Error(), tc.wantSub)
			}
			if got, rerr := os.ReadFile(bad); rerr != nil || string(got) != string(tc.raw) {
				t.Fatalf("corrupt file was modified on rejected open")
			}
		})
	}
}

// TestIncompleteBallotRejectedAsCorrupt：已保存票据的 representative、weight、
// support、voted_at 四个字段中任何一个缺失、为 null 或类型不符，整份状态文件
// 都判为损坏；错误必须指出提案编号、票据下标、字段与缺陷类别，且原文件不被改写。
// 特别地：删掉一张赞成票的 support 绝不能把它当成反对票，而是拒绝打开。
func TestIncompleteBallotRejectedAsCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "treasury.json")
	store, err := InitTreasury(path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	// 窗口从 0 开始：alice 在 0 时刻投赞成（600 权重），dave 在窗口内反对（400）。
	in := baseVoteInput("gip-incomplete")
	in.StartAt, in.Deadline, in.TimelockEnd = 0, 200, 300
	mustCreateVote(t, store, in)
	if _, err := store.CastVote("gip-incomplete", "alice", true, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CastVote("gip-incomplete", "dave", false, 100); err != nil {
		t.Fatal(err)
	}
	store.Close()

	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// mutateBallot 结构化改写第 ballotIdx 张票据，避免依赖缩进与字段次序。
	mutateBallot := func(ballotIdx int, fn func(b map[string]any)) []byte {
		var clone map[string]any
		if err := json.Unmarshal(good, &clone); err != nil {
			t.Fatal(err)
		}
		fn(clone["vote_proposals"].(map[string]any)["gip-incomplete"].(map[string]any)["ballots"].([]any)[ballotIdx].(map[string]any))
		raw, err := json.MarshalIndent(clone, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		return append(raw, '\n')
	}
	mutateBallots := func(fn func(p map[string]any)) []byte {
		var clone map[string]any
		if err := json.Unmarshal(good, &clone); err != nil {
			t.Fatal(err)
		}
		fn(clone["vote_proposals"].(map[string]any)["gip-incomplete"].(map[string]any))
		raw, err := json.MarshalIndent(clone, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		return append(raw, '\n')
	}
	del := func(key string) func(b map[string]any) {
		return func(b map[string]any) { delete(b, key) }
	}
	set := func(key string, v any) func(b map[string]any) {
		return func(b map[string]any) { b[key] = v }
	}

	// 每个变体都必须被拒绝，并在原因中给出提案、票据下标、字段与 missing/null/wrong type。
	cases := map[string]struct {
		raw    []byte
		ballot string
		field  string
		defect string
	}{
		"support deleted":     {mutateBallot(0, del("support")), "ballot 0", `"support"`, "missing"},
		"support null":        {mutateBallot(0, set("support", nil)), "ballot 0", `"support"`, "null"},
		"support as string":   {mutateBallot(0, set("support", "true")), "ballot 0", `"support"`, "wrong type"},
		"support as number":   {mutateBallot(0, set("support", 1)), "ballot 0", `"support"`, "wrong type"},
		"voted_at deleted":    {mutateBallot(0, del("voted_at")), "ballot 0", `"voted_at"`, "missing"},
		"voted_at null":       {mutateBallot(0, set("voted_at", nil)), "ballot 0", `"voted_at"`, "null"},
		"voted_at as bool":    {mutateBallot(0, set("voted_at", true)), "ballot 0", `"voted_at"`, "wrong type"},
		"voted_at as string":  {mutateBallot(0, set("voted_at", "0")), "ballot 0", `"voted_at"`, "wrong type"},
		"weight deleted":      {mutateBallot(0, del("weight")), "ballot 0", `"weight"`, "missing"},
		"weight null":         {mutateBallot(0, set("weight", nil)), "ballot 0", `"weight"`, "null"},
		"weight as string":    {mutateBallot(0, set("weight", "600")), "ballot 0", `"weight"`, "wrong type"},
		"weight as fraction":  {mutateBallot(0, set("weight", 600.5)), "ballot 0", `"weight"`, "wrong type"},
		"representative del":  {mutateBallot(0, del("representative")), "ballot 0", `"representative"`, "missing"},
		"representative null": {mutateBallot(0, set("representative", nil)), "ballot 0", `"representative"`, "null"},
		"representative num":  {mutateBallot(0, set("representative", 7)), "ballot 0", `"representative"`, "wrong type"},
		// 缺损发生在第二张票据时，下标必须是 1，且不能只凭第一张正常票据给出部分结果。
		"second ballot support null": {mutateBallot(1, set("support", nil)), "ballot 1", `"support"`, "null"},
		// 整个票据元素是 null：四个字段同时缺失，按代表字段的空值缺陷报告并定位到该票据。
		"null ballot element": {
			mutateBallots(func(p map[string]any) { p["ballots"] = []any{nil} }),
			"ballot 0", `"representative"`, "null",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			bad := filepath.Join(dir, "bad-"+strings.ReplaceAll(name, " ", "-")+".json")
			if err := os.WriteFile(bad, tc.raw, 0o600); err != nil {
				t.Fatal(err)
			}
			s, err := Open(bad)
			if !errors.Is(err, ErrStateCorrupt) {
				if s != nil {
					s.Close()
				}
				t.Fatalf("Open err=%v, want ErrStateCorrupt", err)
			}
			msg := err.Error()
			for _, want := range []string{"gip-incomplete", tc.ballot, tc.field, tc.defect} {
				if !strings.Contains(msg, want) {
					t.Fatalf("error %q must identify %s", msg, want)
				}
			}
			// 缺损文件原样保留：不补默认值、不整理、不覆盖。
			if got, rerr := os.ReadFile(bad); rerr != nil || string(got) != string(tc.raw) {
				t.Fatalf("corrupt file was modified on rejected open")
			}
			// 损坏文件同样禁止 init 覆盖（沿用既有规则）。
			if _, err := InitTreasury(bad, 0); !errors.Is(err, ErrTreasuryAlreadyInit) {
				t.Fatalf("InitTreasury over corrupt file err=%v", err)
			}
		})
	}
}

// TestExplicitFalseAndZeroBallotStillValid：显式写出的 false 是合法反对票，
// 窗口从 0 开始时显式写出的 voted_at=0 是合法投票时间；二者都不得被当成缺失。
// 空票据列表的未投票提案继续可读，相同选择重试保留首次（0 时刻）记录。
func TestExplicitFalseAndZeroBallotStillValid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "treasury.json")
	store, err := InitTreasury(path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	in := baseVoteInput("gip-explicit")
	in.Quorum = 1
	in.StartAt, in.Deadline, in.TimelockEnd = 0, 200, 300
	mustCreateVote(t, store, in)

	// 尚未投票：空票据列表合法，可查询。
	if v, _, err := store.VoteProposal("gip-explicit"); err != nil || len(v.Ballots) != 0 {
		t.Fatalf("empty ballots view: %+v err=%v", v, err)
	}
	// alice 在起始时刻 0 投反对票：false 与 0 都是显式合法值。
	b, err := store.CastVote("gip-explicit", "alice", false, 0)
	if err != nil {
		t.Fatalf("explicit false/0 vote: %v", err)
	}
	if b.Support || b.VotedAt != 0 || b.Weight != 600 {
		t.Fatalf("ballot=%+v, want support=false voted_at=0 weight=600", b)
	}
	// 相同选择在更晚时间重试：返回首次记录（voted_at 仍为 0），不新增票据。
	again, err := store.CastVote("gip-explicit", "alice", false, 150)
	if err != nil || again.VotedAt != 0 {
		t.Fatalf("same-choice retry changed record: %+v err=%v", again, err)
	}
	// 截止计票：600 反对、0 赞成，参与达到法定人数但赞成不占多数 => rejected，
	// 证明 false 被当作真实反对票而非缺失。
	res, err := store.TallyVote("gip-explicit", 200)
	if err != nil || res.Passed || res.AgainstWeight != 600 || res.ForWeight != 0 {
		t.Fatalf("tally explicit-against: %+v err=%v", res, err)
	}
	store.Close()

	// 重开：显式 false/0 票据原样可读，结论保留为 rejected。
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen explicit false/0 ballot: %v", err)
	}
	defer reopened.Close()
	v, ok, err := reopened.VoteProposal("gip-explicit")
	if err != nil || !ok {
		t.Fatalf("query after reopen: %v ok=%v", err, ok)
	}
	if len(v.Ballots) != 1 || v.Ballots[0].Support || v.Ballots[0].VotedAt != 0 || v.Ballots[0].Weight != 600 {
		t.Fatalf("ballot after reopen = %+v", v.Ballots)
	}
	if v.State != "rejected" || v.Tally == nil || v.Tally.Passed {
		t.Fatalf("state/tally after reopen = %s %+v", v.State, v.Tally)
	}
}

// TestCorruptionAfterOpenRejectsNextRead：句柄已成功打开后，状态文件才被改成
// 缺损票据，下一次读取或操作必须返回 ErrStateCorrupt：不返回任何部分查询结果，
// 不把缺损字段补成反对票或时间 0 后写回，提案状态与资金余额保持原样。
func TestCorruptionAfterOpenRejectsNextRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "treasury.json")
	store, err := InitTreasury(path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	in := baseVoteInput("gip-late-corrupt")
	in.StartAt, in.Deadline, in.TimelockEnd = 0, 200, 300
	mustCreateVote(t, store, in)
	// 一张赞成票：若缺损被补成 false，计票会被翻成 rejected；正确行为是直接报错。
	if _, err := store.CastVote("gip-late-corrupt", "alice", true, 100); err != nil {
		t.Fatal(err)
	}

	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var clone map[string]any
	if err := json.Unmarshal(good, &clone); err != nil {
		t.Fatal(err)
	}
	b := clone["vote_proposals"].(map[string]any)["gip-late-corrupt"].(map[string]any)["ballots"].([]any)[0].(map[string]any)
	b["voted_at"] = nil // 首次投票时间被改成 null
	raw, err := json.MarshalIndent(clone, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	corrupt := append(raw, '\n')
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}

	// 句柄仍开着：下一次任何读取或操作都必须因状态损坏失败，且不产生结果。
	if _, _, err := store.VoteProposal("gip-late-corrupt"); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("VoteProposal after corruption err=%v", err)
	}
	if _, err := store.VoteProposals(); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("VoteProposals after corruption err=%v", err)
	}
	if _, err := store.ProposalsSnapshot(); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("ProposalsSnapshot after corruption err=%v", err)
	}
	if _, err := store.TreasuryBalance(); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("TreasuryBalance after corruption err=%v", err)
	}
	if _, err := store.BalanceSnapshot(); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("BalanceSnapshot after corruption err=%v", err)
	}
	if _, err := store.TallyVote("gip-late-corrupt", 200); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("TallyVote after corruption err=%v", err)
	}
	if _, err := store.CastVote("gip-late-corrupt", "dave", false, 150); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("CastVote after corruption err=%v", err)
	}

	// 失败不得写回：缺损文件内容、提案状态、资金余额全部保持失败前的样子。
	if got, rerr := os.ReadFile(path); rerr != nil || string(got) != string(corrupt) {
		t.Fatalf("state file was rewritten after failed read/operation")
	}
	store.Close()

	// 另一个文件里的正常提案不受影响（不同状态文件互不影响，对照项）。
	goodPath := filepath.Join(t.TempDir(), "treasury.json")
	goodStore, err := InitTreasury(goodPath, 50)
	if err != nil {
		t.Fatal(err)
	}
	defer goodStore.Close()
	if bal, err := goodStore.TreasuryBalance(); err != nil || bal != 50 {
		t.Fatalf("unrelated store balance=%d err=%v, want 50", bal, err)
	}
}
