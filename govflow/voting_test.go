package govflow

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
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

// TestIncompleteBallotRejectedAsCorrupt：尚未计票的提案中，一张票缺字段、
// 字段为 null 或 JSON 类型不符时，整份状态文件判为损坏：
// 打开、查询与计票全部拒绝，不给出基于其余票据的部分结果，也不把缺损补成
// 反对票或时间 0 后写回。错误原因必须能看出提案编号、票据下标与字段，
// 并区分缺失、空值与类型不符。
func TestIncompleteBallotRejectedAsCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "treasury.json")
	store, err := InitTreasury(path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	in := baseVoteInput("gip-incomplete")
	in.StartAt, in.Deadline, in.TimelockEnd = 0, 200, 300
	mustCreateVote(t, store, in)
	// 两张票：alice 600 赞成（0 时刻）、dave 400 反对（150 时刻），尚未计票。
	if _, err := store.CastVote("gip-incomplete", "alice", true, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CastVote("gip-incomplete", "dave", false, 150); err != nil {
		t.Fatal(err)
	}
	// 另有一笔已执行支出，便于断言失败后资金余额与文件内容原样保留。
	mustRegister(t, store, "gip-spent", 0, "transfer:audits:100")
	if _, err := store.Execute("gip-spent", 0); err != nil {
		t.Fatal(err)
	}
	store.Close()

	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// mutate 以结构化改写构造损坏变体；index 标明出问题的票据下标（dave 是第 1 张）。
	mutate := func(fn func(p map[string]any)) []byte {
		var clone map[string]any
		if err := json.Unmarshal(good, &clone); err != nil {
			t.Fatal(err)
		}
		p := clone["vote_proposals"].(map[string]any)["gip-incomplete"].(map[string]any)
		fn(p)
		raw, err := json.MarshalIndent(clone, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		return append(raw, '\n')
	}
	// 第一张票（下标 0）与第二张票（下标 1）各覆盖若干字段，确保下标被正确报告。
	cases := map[string]struct {
		raw       []byte
		index     string
		field     string
		causeWord string
	}{
		"support missing on ballot 0": {
			mutate(func(p map[string]any) {
				delete(p["ballots"].([]any)[0].(map[string]any), "support")
			}), "ballot 0", "support", "missing",
		},
		"support null on ballot 0": {
			mutate(func(p map[string]any) {
				p["ballots"].([]any)[0].(map[string]any)["support"] = nil
			}), "ballot 0", "support", "null",
		},
		"support string not boolean": {
			mutate(func(p map[string]any) {
				p["ballots"].([]any)[0].(map[string]any)["support"] = "true"
			}), "ballot 0", "support", "wrong type",
		},
		"support number not boolean": {
			mutate(func(p map[string]any) {
				p["ballots"].([]any)[0].(map[string]any)["support"] = 1
			}), "ballot 0", "support", "wrong type",
		},
		"voted_at missing with zero window": {
			mutate(func(p map[string]any) {
				delete(p["ballots"].([]any)[0].(map[string]any), "voted_at")
			}), "ballot 0", "voted_at", "missing",
		},
		"voted_at null not timestamp zero": {
			mutate(func(p map[string]any) {
				p["ballots"].([]any)[0].(map[string]any)["voted_at"] = nil
			}), "ballot 0", "voted_at", "null",
		},
		"voted_at string not integer": {
			mutate(func(p map[string]any) {
				p["ballots"].([]any)[0].(map[string]any)["voted_at"] = "0"
			}), "ballot 0", "voted_at", "wrong type",
		},
		"voted_at float not integer": {
			mutate(func(p map[string]any) {
				p["ballots"].([]any)[0].(map[string]any)["voted_at"] = 150.5
			}), "ballot 0", "voted_at", "wrong type",
		},
		"weight null": {
			mutate(func(p map[string]any) {
				p["ballots"].([]any)[1].(map[string]any)["weight"] = nil
			}), "ballot 1", "weight", "null",
		},
		"weight missing": {
			mutate(func(p map[string]any) {
				delete(p["ballots"].([]any)[1].(map[string]any), "weight")
			}), "ballot 1", "weight", "missing",
		},
		"weight float not integer": {
			mutate(func(p map[string]any) {
				p["ballots"].([]any)[1].(map[string]any)["weight"] = 400.5
			}), "ballot 1", "weight", "wrong type",
		},
		"weight over int64": {
			mutate(func(p map[string]any) {
				p["ballots"].([]any)[1].(map[string]any)["weight"] = 9223372036854775808.0
			}), "ballot 1", "weight", "wrong type",
		},
		"representative null": {
			mutate(func(p map[string]any) {
				p["ballots"].([]any)[1].(map[string]any)["representative"] = nil
			}), "ballot 1", "representative", "null",
		},
		"representative missing": {
			mutate(func(p map[string]any) {
				delete(p["ballots"].([]any)[1].(map[string]any), "representative")
			}), "ballot 1", "representative", "missing",
		},
		"representative number not string": {
			mutate(func(p map[string]any) {
				p["ballots"].([]any)[1].(map[string]any)["representative"] = 7
			}), "ballot 1", "representative", "wrong type",
		},
		"support array not boolean": {
			mutate(func(p map[string]any) {
				p["ballots"].([]any)[0].(map[string]any)["support"] = []any{}
			}), "ballot 0", "support", "wrong type",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			bad := filepath.Join(dir, "bad-"+strings.ReplaceAll(name, " ", "-")+".json")
			if err := os.WriteFile(bad, tc.raw, 0o600); err != nil {
				t.Fatal(err)
			}

			// 打开即拒绝，错误带提案编号、票据下标、字段与原因分类。
			s, err := Open(bad)
			if !errors.Is(err, ErrStateCorrupt) {
				if s != nil {
					s.Close()
				}
				t.Fatalf("Open err=%v, want ErrStateCorrupt", err)
			}
			msg := err.Error()
			for _, want := range []string{`"gip-incomplete"`, tc.index, `"` + tc.field + `"`, tc.causeWord} {
				if !strings.Contains(msg, want) {
					t.Fatalf("error %q missing %q", msg, want)
				}
			}

			// 原文件内容必须原样保留（失败不回写、不补默认值）。
			if got, rerr := os.ReadFile(bad); rerr != nil || string(got) != string(tc.raw) {
				t.Fatalf("corrupt file was modified on rejected Open")
			}

			// 重复打开仍以同一损坏分类拒绝，绝不返回可用句柄给出部分结果。
			s2, e := Open(bad)
			if !errors.Is(e, ErrStateCorrupt) {
				if s2 != nil {
					s2.Close()
				}
				t.Fatalf("reopen err=%v, want ErrStateCorrupt", e)
			}
		})
	}
}

// TestIncompleteBallotOpsRejectAndKeepFile：损坏文件上的查询与计票/投票操作
// 一律返回 ErrStateCorrupt，且不产生部分结果、不改写文件、不动提案状态与余额。
func TestIncompleteBallotOpsRejectAndKeepFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "treasury.json")
	store, err := InitTreasury(path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	in := baseVoteInput("gip-ops")
	in.StartAt, in.Deadline, in.TimelockEnd = 0, 200, 300
	mustCreateVote(t, store, in)
	if _, err := store.CastVote("gip-ops", "alice", true, 0); err != nil {
		t.Fatal(err)
	}
	mustRegister(t, store, "gip-spent", 0, "transfer:audits:100")
	if _, err := store.Execute("gip-spent", 0); err != nil {
		t.Fatal(err)
	}
	store.Close()

	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var clone map[string]any
	if err := json.Unmarshal(good, &clone); err != nil {
		t.Fatal(err)
	}
	// 删除赞成票的 support 字段：若被默认成 false，600 赞成会被误算成 600 反对。
	delete(clone["vote_proposals"].(map[string]any)["gip-ops"].(map[string]any)["ballots"].([]any)[0].(map[string]any), "support")
	bad, err := json.MarshalIndent(clone, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	bad = append(bad, '\n')
	if err := os.WriteFile(path, bad, 0o600); err != nil {
		t.Fatal(err)
	}

	// 打开后，任何读取或操作都拒绝。
	s, err := Open(path)
	if !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("Open err=%v, want ErrStateCorrupt", err)
	}
	if s != nil {
		t.Fatal("corrupt open must not return a usable store")
	}

	// 直接验证文件未被任何尝试改写。
	if got, rerr := os.ReadFile(path); rerr != nil || string(got) != string(bad) {
		t.Fatalf("corrupt file was normalized or rewritten")
	}

	// 用一个真实打开的句柄，随后在句柄之外把文件改坏，下一次读取/操作必须拒绝。
	recoverPath := filepath.Join(dir, "recover.json")
	if err := os.WriteFile(recoverPath, good, 0o600); err != nil {
		t.Fatal(err)
	}
	live, err := Open(recoverPath)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if bal, err := live.TreasuryBalance(); err != nil || bal != 900 {
		t.Fatalf("balance before corruption=%d err=%v", bal, err)
	}
	if err := os.WriteFile(recoverPath, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := live.TreasuryBalance(); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("TreasuryBalance after corruption err=%v, want ErrStateCorrupt", err)
	}
	if _, _, err := live.VoteProposal("gip-ops"); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("VoteProposal after corruption err=%v", err)
	}
	if _, err := live.VoteProposals(); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("VoteProposals after corruption err=%v", err)
	}
	if _, err := live.ProposalsSnapshot(); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("ProposalsSnapshot after corruption err=%v", err)
	}
	if _, err := live.TallyVote("gip-ops", 200); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("TallyVote after corruption err=%v", err)
	}
	if _, err := live.CastVote("gip-ops", "dave", false, 150); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("CastVote after corruption err=%v", err)
	}
	if _, err := live.BalanceSnapshot(); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("BalanceSnapshot after corruption err=%v", err)
	}
	// 失败后文件字节、提案状态与余额保持损坏被发现时的原样（未回写默认值）。
	if got, rerr := os.ReadFile(recoverPath); rerr != nil || string(got) != string(bad) {
		t.Fatalf("failed ops rewrote or normalized the state file")
	}
}

// TestExplicitFalseAndZeroBallotLegal：明确写出的 false 是有效反对票，
// 投票窗口从 0 开始时明确写出的 0 是有效首次投票时间，二者都不得当成缺失。
// 空票据列表的未投票提案同样继续可读、可计票。
func TestExplicitFalseAndZeroBallotLegal(t *testing.T) {
	dir := t.TempDir()
	handwritten := filepath.Join(dir, "handwritten.json")
	// 手工构造：start=0、票据 support=false、voted_at=0，quorum=1。
	raw := `{
  "magic": "govflow-treasury-state",
  "version": 1,
  "initial_treasury": 1000,
  "treasury": 1000,
  "balances": {},
  "proposals": {},
  "receipts": [],
  "vote_proposals": {
    "gip-fz": {
      "id": "gip-fz",
      "state": "voting",
      "members": [{"id": "a", "weight": 5}, {"id": "b", "weight": 5}],
      "delegations": [],
      "quorum": 1,
      "start_at": 0,
      "deadline": 10,
      "timelock_end": 10,
      "actions": [],
      "ballots": [{"representative": "b", "weight": 5, "support": false, "voted_at": 0}]
    }
  }
}
`
	if err := os.WriteFile(handwritten, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(handwritten)
	if err != nil {
		t.Fatalf("explicit false/0 ballot rejected: %v", err)
	}
	defer s.Close()
	v, ok, err := s.VoteProposal("gip-fz")
	if err != nil || !ok {
		t.Fatalf("query: %v ok=%v", err, ok)
	}
	if len(v.Ballots) != 1 || v.Ballots[0].Support || v.Ballots[0].VotedAt != 0 ||
		v.Ballots[0].Representative != "b" || v.Ballots[0].Weight != 5 {
		t.Fatalf("false/0 ballot misread as missing/default: %+v", v.Ballots)
	}
	// 只有 5 权重反对：for=0 < against=5 => 拒绝；0 不得被当成“缺失时间”。
	res, err := s.TallyVote("gip-fz", 10)
	if err != nil {
		t.Fatalf("tally legal against-only ballot: %v", err)
	}
	if res.Passed || res.ForWeight != 0 || res.AgainstWeight != 5 || res.Turnout != 5 {
		t.Fatalf("unexpected tally for explicit-false ballot: %+v", res)
	}

	// 尚未投票的提案允许空票据列表（null 与 [] 两种旧/新写法都合法）。
	empty, _ := openTempStore(t, 1000)
	defer empty.Close()
	mustCreateVote(t, empty, &CreateVoteInput{
		ID: "gip-empty-ballots", Members: []VoteMember{{ID: "a", Weight: 1}},
		Quorum: 1, StartAt: 0, Deadline: 10, TimelockEnd: 10,
	})
	empty.Close()
	es, err := Open(empty.Path())
	if err != nil {
		t.Fatalf("reopen empty-ballot proposal: %v", err)
	}
	defer es.Close()
	ev, ok, err := es.VoteProposal("gip-empty-ballots")
	if err != nil || !ok || len(ev.Ballots) != 0 {
		t.Fatalf("empty ballots query: %+v ok=%v err=%v", ev.Ballots, ok, err)
	}
	res2, err := es.TallyVote("gip-empty-ballots", 10)
	if err != nil || res2.Passed || res2.Turnout != 0 {
		t.Fatalf("empty-ballot tally should reject with zero turnout: %+v err=%v", res2, err)
	}
}

// TestNullTopLevelLeafRejected：非票据位置的标量叶子为 null 同样整文件拒绝，
// 不会被解码器静默读成零值（例如 null 余额）。
func TestNullTopLevelLeafRejected(t *testing.T) {
	store, _ := openTempStore(t, 1000)
	defer store.Close()
	goodPath := store.Path()
	store.Close()
	good, err := os.ReadFile(goodPath)
	if err != nil {
		t.Fatal(err)
	}
	raw := strings.Replace(string(good), `"treasury": 1000`, `"treasury": null`, 1)
	bad := filepath.Join(t.TempDir(), "null-leaf.json")
	if err := os.WriteFile(bad, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(bad)
	if !errors.Is(err, ErrStateCorrupt) {
		if s != nil {
			s.Close()
		}
		t.Fatalf("null treasury leaf err=%v, want ErrStateCorrupt", err)
	}
	if !strings.Contains(err.Error(), "treasury") {
		t.Fatalf("error should name the field: %v", err)
	}
}

// TestIncompleteTallyRejectedAsCorrupt：已计票提案的计票结果缺少 for_weight 或
// against_weight（或为 null、JSON 类型不符）时，整份状态文件判为损坏——即使缺失
// 一侧的真实票重恰好为零、逐票明细能重放出相同结论，也不得默认补零后接受。
// 已通过、已拒绝、已执行的提案遵守同一条完整性规则。错误必须能看出提案编号、
// 缺失字段与原因分类，且原文件保留。
func TestIncompleteTallyRejectedAsCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "treasury.json")
	store, err := InitTreasury(path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	// 三项提案共用同一份名单（quorum=600，alice 归集 600，dave 400）：
	// gip-passed：alice 600 赞成、反对 0，恰好达到法定人数而通过；
	// gip-rejected：dave 400 反对、赞成 0，参与不足而拒绝；
	// gip-executed：与 gip-passed 相同并已在时间锁后执行。
	passed := baseVoteInput("gip-passed")
	mustCreateVote(t, store, passed)
	rejected := baseVoteInput("gip-rejected")
	mustCreateVote(t, store, rejected)
	executed := baseVoteInput("gip-executed")
	mustCreateVote(t, store, executed)
	for _, id := range []string{"gip-passed", "gip-rejected", "gip-executed"} {
		voter, support := "alice", true
		if id == "gip-rejected" {
			voter, support = "dave", false
		}
		if _, err := store.CastVote(id, voter, support, 150); err != nil {
			t.Fatal(err)
		}
		if _, err := store.TallyVote(id, 200); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Execute("gip-executed", 300); err != nil {
		t.Fatal(err)
	}
	store.Close()

	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// mutate 以结构化改写构造损坏变体；id 标明出问题的提案。
	mutate := func(id string, fn func(tally map[string]any)) []byte {
		var clone map[string]any
		if err := json.Unmarshal(good, &clone); err != nil {
			t.Fatal(err)
		}
		p := clone["vote_proposals"].(map[string]any)[id].(map[string]any)
		fn(p["tally"].(map[string]any))
		raw, err := json.MarshalIndent(clone, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		return append(raw, '\n')
	}
	cases := map[string]struct {
		raw       []byte
		id        string
		field     string
		causeWord string
	}{
		// 标题场景：赞成 600/反对 0/法定人数 600 已通过，反对权重被删掉。
		"against weight missing on passed zero side": {
			mutate("gip-passed", func(tl map[string]any) { delete(tl, "against_weight") }),
			"gip-passed", "against_weight", "missing",
		},
		"for weight missing on passed": {
			mutate("gip-passed", func(tl map[string]any) { delete(tl, "for_weight") }),
			"gip-passed", "for_weight", "missing",
		},
		// 已拒绝提案的赞成侧恰好为零，同样不得补零接受。
		"for weight missing on rejected zero side": {
			mutate("gip-rejected", func(tl map[string]any) { delete(tl, "for_weight") }),
			"gip-rejected", "for_weight", "missing",
		},
		// 已执行提案的首次计票记录同样必须完整。
		"against weight missing on executed": {
			mutate("gip-executed", func(tl map[string]any) { delete(tl, "against_weight") }),
			"gip-executed", "against_weight", "missing",
		},
		"for weight null": {
			mutate("gip-passed", func(tl map[string]any) { tl["for_weight"] = nil }),
			"gip-passed", "for_weight", "null",
		},
		"against weight null": {
			mutate("gip-rejected", func(tl map[string]any) { tl["against_weight"] = nil }),
			"gip-rejected", "against_weight", "null",
		},
		"for weight string not integer": {
			mutate("gip-passed", func(tl map[string]any) { tl["for_weight"] = "600" }),
			"gip-passed", "for_weight", "wrong type",
		},
		"against weight float not integer": {
			mutate("gip-passed", func(tl map[string]any) { tl["against_weight"] = 0.5 }),
			"gip-passed", "against_weight", "wrong type",
		},
		"for weight boolean not integer": {
			mutate("gip-passed", func(tl map[string]any) { tl["for_weight"] = true }),
			"gip-passed", "for_weight", "wrong type",
		},
		"against weight over int64": {
			mutate("gip-passed", func(tl map[string]any) { tl["against_weight"] = 9223372036854775808.0 }),
			"gip-passed", "against_weight", "wrong type",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			bad := filepath.Join(dir, "bad-"+strings.ReplaceAll(name, " ", "-")+".json")
			if err := os.WriteFile(bad, tc.raw, 0o600); err != nil {
				t.Fatal(err)
			}

			// 打开即拒绝，错误带提案编号、计票字段与原因分类。
			s, err := Open(bad)
			if !errors.Is(err, ErrStateCorrupt) {
				if s != nil {
					s.Close()
				}
				t.Fatalf("Open err=%v, want ErrStateCorrupt", err)
			}
			msg := err.Error()
			for _, want := range []string{`"` + tc.id + `"`, "tally", `"` + tc.field + `"`, tc.causeWord} {
				if !strings.Contains(msg, want) {
					t.Fatalf("error %q missing %q", msg, want)
				}
			}

			// 原文件内容必须原样保留（失败不回写、不把缺失权重补成 0）。
			if got, rerr := os.ReadFile(bad); rerr != nil || string(got) != string(tc.raw) {
				t.Fatalf("corrupt file was modified on rejected Open")
			}

			// 重复打开仍以同一损坏分类拒绝，绝不返回可用句柄给出部分结果。
			s2, e := Open(bad)
			if !errors.Is(e, ErrStateCorrupt) {
				if s2 != nil {
					s2.Close()
				}
				t.Fatalf("reopen err=%v, want ErrStateCorrupt", e)
			}
		})
	}
}

// TestIncompleteTallyOpsRejectAndKeepFile：计票结果缺损的文件上，查询与
// 计票/投票操作一律返回 ErrStateCorrupt，不给出部分结果、不因请求同库内另一项
// 正常提案而忽略损坏、不改写文件，写入操作也不能借机把缺失权重补成零后保存。
func TestIncompleteTallyOpsRejectAndKeepFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "treasury.json")
	store, err := InitTreasury(path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	// gip-tally-ops：已计票通过（for=600/against=0）；gip-still-voting：仍在投票。
	mustCreateVote(t, store, baseVoteInput("gip-tally-ops"))
	if _, err := store.CastVote("gip-tally-ops", "alice", true, 150); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TallyVote("gip-tally-ops", 200); err != nil {
		t.Fatal(err)
	}
	mustCreateVote(t, store, baseVoteInput("gip-still-voting"))
	mustRegister(t, store, "gip-spent", 0, "transfer:audits:100")
	if _, err := store.Execute("gip-spent", 0); err != nil {
		t.Fatal(err)
	}
	store.Close()

	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var clone map[string]any
	if err := json.Unmarshal(good, &clone); err != nil {
		t.Fatal(err)
	}
	// 删除已计票结果的 against_weight：真实反对票重恰好为 0，
	// 若被默认补零，这份缺损记录会被误当成有效的首次计票结果。
	delete(clone["vote_proposals"].(map[string]any)["gip-tally-ops"].(map[string]any)["tally"].(map[string]any), "against_weight")
	bad, err := json.MarshalIndent(clone, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	bad = append(bad, '\n')
	if err := os.WriteFile(path, bad, 0o600); err != nil {
		t.Fatal(err)
	}

	// 打开即拒绝，不返回可用句柄。
	s, err := Open(path)
	if !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("Open err=%v, want ErrStateCorrupt", err)
	}
	if s != nil {
		t.Fatal("corrupt open must not return a usable store")
	}
	if got, rerr := os.ReadFile(path); rerr != nil || string(got) != string(bad) {
		t.Fatalf("corrupt file was normalized or rewritten")
	}

	// 用一个真实打开的句柄，随后在句柄之外把文件改坏，下一次读取/操作必须拒绝。
	recoverPath := filepath.Join(dir, "recover.json")
	if err := os.WriteFile(recoverPath, good, 0o600); err != nil {
		t.Fatal(err)
	}
	live, err := Open(recoverPath)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if bal, err := live.TreasuryBalance(); err != nil || bal != 900 {
		t.Fatalf("balance before corruption=%d err=%v", bal, err)
	}
	if err := os.WriteFile(recoverPath, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := live.TreasuryBalance(); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("TreasuryBalance after corruption err=%v, want ErrStateCorrupt", err)
	}
	if _, _, err := live.VoteProposal("gip-tally-ops"); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("VoteProposal(corrupt) after corruption err=%v", err)
	}
	// 请求同库内另一项正常提案同样必须拒绝，不得忽略损坏。
	if _, _, err := live.VoteProposal("gip-still-voting"); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("VoteProposal(healthy) after corruption err=%v", err)
	}
	if _, err := live.VoteProposals(); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("VoteProposals after corruption err=%v", err)
	}
	if _, err := live.ProposalsSnapshot(); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("ProposalsSnapshot after corruption err=%v", err)
	}
	// 再次计票不得返回由其余字段组成的部分结果。
	if _, err := live.TallyVote("gip-tally-ops", 200); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("TallyVote(corrupt) after corruption err=%v", err)
	}
	// 对另一项正常提案首次计票同样拒绝，且不得借机改写状态文件。
	if _, err := live.TallyVote("gip-still-voting", 200); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("TallyVote(healthy) after corruption err=%v", err)
	}
	if _, err := live.CastVote("gip-still-voting", "dave", false, 150); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("CastVote after corruption err=%v", err)
	}
	if _, err := live.BalanceSnapshot(); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("BalanceSnapshot after corruption err=%v", err)
	}
	// 失败后文件字节保持损坏被发现时的原样（未回写、未把缺失权重补成 0）。
	if got, rerr := os.ReadFile(recoverPath); rerr != nil || string(got) != string(bad) {
		t.Fatalf("failed ops rewrote or normalized the state file")
	}
}

// TestExplicitZeroTallyWeightsLegal：计票结果中明确写出的整数 0 是合法权重
// （该侧或两侧都没有票），不得当成缺失；保存的结论仍按逐票明细重放核对，
// 再次计票返回首次结论。
func TestExplicitZeroTallyWeightsLegal(t *testing.T) {
	dir := t.TempDir()
	handwritten := filepath.Join(dir, "handwritten.json")
	raw := `{
  "magic": "govflow-treasury-state",
  "version": 1,
  "initial_treasury": 1000,
  "treasury": 1000,
  "balances": {},
  "proposals": {},
  "receipts": [],
  "vote_proposals": {
    "gip-zero": {
      "id": "gip-zero",
      "state": "rejected",
      "members": [{"id": "a", "weight": 5}],
      "delegations": [],
      "quorum": 1,
      "start_at": 0,
      "deadline": 10,
      "timelock_end": 10,
      "actions": [],
      "ballots": [],
      "tally": {"for_weight": 0, "against_weight": 0, "tallied_at": 10}
    },
    "gip-zerowin": {
      "id": "gip-zerowin",
      "state": "passed",
      "members": [{"id": "a", "weight": 5}],
      "delegations": [],
      "quorum": 1,
      "start_at": 0,
      "deadline": 10,
      "timelock_end": 10,
      "actions": [],
      "ballots": [{"representative": "a", "weight": 5, "support": true, "voted_at": 3}],
      "tally": {"for_weight": 5, "against_weight": 0, "tallied_at": 10}
    }
  }
}
`
	if err := os.WriteFile(handwritten, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(handwritten)
	if err != nil {
		t.Fatalf("explicit zero tally weights rejected: %v", err)
	}
	defer s.Close()

	// 双方都明确写出 0：无票提案的合法拒绝结论。
	v, ok, err := s.VoteProposal("gip-zero")
	if err != nil || !ok {
		t.Fatalf("query gip-zero: %v ok=%v", err, ok)
	}
	if v.Tally == nil || v.Tally.ForWeight != 0 || v.Tally.AgainstWeight != 0 ||
		v.Tally.Turnout != 0 || v.Tally.Passed || v.Tally.TalliedAt != 10 {
		t.Fatalf("zero tally misread: %+v", v.Tally)
	}
	// 再次计票返回首次结论（tallied_at 保持 10），不重新计票。
	res, err := s.TallyVote("gip-zero", 20)
	if err != nil {
		t.Fatalf("re-tally gip-zero: %v", err)
	}
	if res.Passed || res.TalliedAt != 10 || res.ForWeight != 0 || res.AgainstWeight != 0 {
		t.Fatalf("re-tally must return first result: %+v", res)
	}

	// 反对侧明确写出 0：合法通过结论，重放核对一致。
	v2, ok, err := s.VoteProposal("gip-zerowin")
	if err != nil || !ok {
		t.Fatalf("query gip-zerowin: %v ok=%v", err, ok)
	}
	if v2.Tally == nil || v2.Tally.ForWeight != 5 || v2.Tally.AgainstWeight != 0 || !v2.Tally.Passed {
		t.Fatalf("zero against weight misread: %+v", v2.Tally)
	}
}

// TestCreateVoteRejectsInvalidUTF8：提案编号、成员编号或动作原文含有非法
// UTF-8 字节（非法首字节、残缺多字节序列、UTF-8 形式直接编码的代理码位）
// 时整项创建失败，错误分类为 ErrInvalidProposal，且说明是哪类文本、
// 成员/动作在提交列表中的位置；状态文件逐字节保持原样。
func TestCreateVoteRejectsInvalidUTF8(t *testing.T) {
	store, path := openTempStore(t, 1000)
	defer store.Close()

	// 状态中先存在一项编号含真实 U+FFFD 的合法提案与一笔登记提案，
	// 验证失败不重试/覆盖已有内容，已有提案、票据与余额不受影响。
	fffdIn := baseVoteInput("gip-�")
	mustCreateVote(t, store, fffdIn)
	mustRegister(t, store, "gip-reg", 300, "transfer:a:1")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}

	cases := []struct {
		name      string
		mutate    func(in *CreateVoteInput)
		wantInErr []string
	}{
		{"proposal id illegal byte", func(in *CreateVoteInput) {
			in.ID = "gip-\xff"
		}, []string{"proposal id", "UTF-8"}},
		{"proposal id truncated rune", func(in *CreateVoteInput) {
			in.ID = "gip-\xe4\xb8" // “中”缺少最后一个字节
		}, []string{"proposal id", "UTF-8"}},
		{"proposal id encoded surrogate", func(in *CreateVoteInput) {
			in.ID = "gip-\xed\xa0\x80" // UTF-8 形式直接编码的 U+D800
		}, []string{"proposal id", "UTF-8"}},
		{"member id illegal byte", func(in *CreateVoteInput) {
			in.Members[1].ID = "bo\xffb"
		}, []string{"member 1", "UTF-8"}},
		{"member id truncated rune", func(in *CreateVoteInput) {
			in.Members[3].ID = "\xe4\xb8"
		}, []string{"member 3", "UTF-8"}},
		{"member id encoded surrogate", func(in *CreateVoteInput) {
			in.Members[0].ID = "\xed\xb0\x80" // UTF-8 形式直接编码的 U+DC00
		}, []string{"member 0", "UTF-8"}},
		{"action text illegal byte", func(in *CreateVoteInput) {
			in.Actions[0] = "transfer:audits:\xff"
		}, []string{"action 0", "UTF-8"}},
		{"action text truncated rune", func(in *CreateVoteInput) {
			in.Actions = append(in.Actions, "\xf0\x9f\x98") // 表情缺少最后一个字节
		}, []string{"action 1", "UTF-8"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := baseVoteInput("bad-" + strings.ReplaceAll(tc.name, " ", "-"))
			tc.mutate(in)
			_, existed, err := store.CreateVoteProposal(in)
			if err == nil || existed {
				t.Fatalf("expected rejection, existed=%v err=%v", existed, err)
			}
			if !errors.Is(err, ErrInvalidProposal) {
				t.Fatalf("err=%v, want ErrInvalidProposal", err)
			}
			for _, want := range tc.wantInErr {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("err=%q, want substring %q", err.Error(), want)
				}
			}
		})
	}

	// 非法字节输入落盘时会被改写成真实 U+FFFD，恰好等于已存在的合法编号：
	// 仍必须整项失败（ErrInvalidProposal），不得当作该编号的相同内容重试，
	// 更不得覆盖它。
	retry := baseVoteInput("gip-\xef\xbf") // “�”缺少最后一个字节
	if _, existed, err := store.CreateVoteProposal(retry); err == nil || existed ||
		!errors.Is(err, ErrInvalidProposal) {
		t.Fatalf("invalid-UTF-8 collision with real U+FFFD id: existed=%v err=%v", existed, err)
	}

	// 状态文件逐字节保持原样；已有合法提案仍可按原编号查询。
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("state file changed after rejected creates")
	}
	v, ok, err := store.VoteProposal("gip-�")
	if err != nil || !ok || v.ID != "gip-�" {
		t.Fatalf("existing U+FFFD proposal: ok=%v err=%v view=%+v", ok, err, v)
	}
	if bal, err := store.TreasuryBalance(); err != nil || bal != 1000 {
		t.Fatalf("treasury changed: bal=%d err=%v", bal, err)
	}
}

// TestCreateVotePreservesValidText：中文、表情、用户明确写出的 U+FFFD 以及
// 由反斜杠与普通字母组成的字面文本（如 "\uD800"：创建参数不是 JSON 字符串，
// 不得把其中看似 Unicode 转义的文字重解释成字符）都按原文保存与展示。
// 编码合法但动作格式错误的文本照常创建，由执行操作按现有规则拒绝。
func TestCreateVotePreservesValidText(t *testing.T) {
	store, path := openTempStore(t, 1000)
	in := &CreateVoteInput{
		ID: "提案-�-😀",
		Members: []VoteMember{
			{ID: "成员甲", Weight: 100},
			{ID: "smi�le-😀", Weight: 200},
			{ID: `back\uD800slash`, Weight: 300}, // 反斜杠 + 普通字母的字面文本
		},
		Quorum:      400,
		StartAt:     10,
		Deadline:    20,
		TimelockEnd: 30,
		Actions: []string{
			"transfer:账户:100",
			`literal FFFD stays text`,
			"not-a-transfer-action", // 编码合法但格式错误：创建仍允许
		},
	}
	view := mustCreateVote(t, store, in)
	if view.ID != in.ID {
		t.Fatalf("view id = %q, want %q", view.ID, in.ID)
	}
	for i, m := range view.Members {
		if m.ID != in.Members[i].ID {
			t.Fatalf("member %d id = %q, want %q", i, m.ID, in.Members[i].ID)
		}
	}
	if !sameActions(view.Actions, in.Actions) {
		t.Fatalf("view actions = %q, want %q", view.Actions, in.Actions)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// 重新打开后查询仍逐字展示原文。
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	got, ok, err := reopened.VoteProposal(in.ID)
	if err != nil || !ok {
		t.Fatalf("query after reopen: ok=%v err=%v", ok, err)
	}
	if got.ID != in.ID {
		t.Fatalf("reopened id = %q, want %q", got.ID, in.ID)
	}
	for i, m := range got.Members {
		if m.ID != in.Members[i].ID {
			t.Fatalf("reopened member %d id = %q, want %q", i, m.ID, in.Members[i].ID)
		}
	}
	if !sameActions(got.Actions, in.Actions) {
		t.Fatalf("reopened actions = %q, want %q", got.Actions, in.Actions)
	}

	// 格式错误的动作在执行时才被拒绝，创建入口不提前改变动作资格。
	if _, err := reopened.Execute(in.ID, 30); err == nil {
		t.Fatalf("execute with malformed action must fail")
	}
}

// executedRetryInput 构造一项已执行提案的完整创建内容：多人连续委托
// （erin->carol->bob，bob 为最终代表）加上两笔发给同一收款账户、金额不同、
// 顺序敏感的转账动作。成员与委托的提交顺序刻意与路径推演顺序不同。
func executedRetryInput(id string) *CreateVoteInput {
	return &CreateVoteInput{
		ID: id,
		Members: []VoteMember{
			{ID: "alice", Weight: 300},
			{ID: "bob", Weight: 100},
			{ID: "carol", Weight: 50},
			{ID: "dave", Weight: 400},
			{ID: "erin", Weight: 25},
		},
		Delegations: []Delegation{
			{From: "erin", To: "carol"},
			{From: "carol", To: "bob"},
		},
		Quorum:      600,
		StartAt:     100,
		Deadline:    200,
		TimelockEnd: 300,
		Actions: []string{
			"transfer:audits:100",
			"transfer:audits:50",
		},
	}
}

// assertExecutedProposalUntouched 断言提案查询视图仍为首次创建、投票、计票、
// 执行后的原样：已执行状态、投票明细、首次计票结论、成员顺序与完整委托路径、
// 动作原文与顺序都不得被创建重试改动。
func assertExecutedProposalUntouched(t *testing.T, v *VoteProposalView, in *CreateVoteInput) {
	t.Helper()
	if v.State != "executed" {
		t.Fatalf("state=%s, want executed", v.State)
	}
	if v.Source != "vote" {
		t.Fatalf("source=%s, want vote", v.Source)
	}
	if !sameActions(v.Actions, in.Actions) {
		t.Fatalf("actions=%v, want %v", v.Actions, in.Actions)
	}
	// 成员展示次序来自原提案的提交顺序，不被换序重试重排。
	if ids := memberIDs(v.Members); !equalStrings(ids, memberIDsFromInput(in.Members)) {
		t.Fatalf("member order=%v, want original %v", ids, memberIDsFromInput(in.Members))
	}
	// 经多人连续委托给最终代表时必须保留完整路径，不得省略中间成员 carol。
	want := map[string]MemberView{
		"alice": {ID: "alice", Weight: 300, Path: []string{"alice"}, Delegate: "alice"},
		"bob":   {ID: "bob", Weight: 100, Path: []string{"bob"}, Delegate: "bob"},
		"carol": {ID: "carol", Weight: 50, Path: []string{"carol", "bob"}, Delegate: "bob", Direct: "bob"},
		"dave":  {ID: "dave", Weight: 400, Path: []string{"dave"}, Delegate: "dave"},
		"erin":  {ID: "erin", Weight: 25, Path: []string{"erin", "carol", "bob"}, Delegate: "bob", Direct: "carol"},
	}
	assertMemberViews(t, "post-retry", memberPaths(v), want)

	// 投票明细：原有代表、归集票重、赞成/反对选择、首次投票时间全部保留，
	// 创建重试不得清空或追加票据。
	if len(v.Ballots) != 3 {
		t.Fatalf("ballot count=%d, want 3: %+v", len(v.Ballots), v.Ballots)
	}
	ballots := map[string]BallotView{}
	for _, b := range v.Ballots {
		ballots[b.Representative] = b
	}
	// bob 归集 bob+carol+erin = 100+50+25 = 175；alice 代表自己 300。
	if b := ballots["bob"]; b.Weight != 175 || !b.Support || b.VotedAt != 150 {
		t.Fatalf("bob ballot=%+v, want weight=175 support=true voted_at=150", b)
	}
	if b := ballots["alice"]; b.Weight != 300 || !b.Support || b.VotedAt != 150 {
		t.Fatalf("alice ballot=%+v, want weight=300 support=true voted_at=150", b)
	}
	if b := ballots["dave"]; b.Weight != 400 || b.Support || b.VotedAt != 160 {
		t.Fatalf("dave ballot=%+v, want weight=400 support=false voted_at=160", b)
	}

	// 首次计票结论与首次计票时间保留：赞成 300+175=475，反对 400，
	// 参与 875 >= 600 且赞成严格多于反对。
	if v.Tally == nil {
		t.Fatal("tally conclusion missing after create retry")
	}
	if v.Tally.ForWeight != 475 || v.Tally.AgainstWeight != 400 ||
		v.Tally.Turnout != 875 || v.Tally.Quorum != 600 || !v.Tally.Passed ||
		v.Tally.TalliedAt != 200 {
		t.Fatalf("tally=%+v, want for=475 against=400 turnout=875 quorum=600 passed=true tallied_at=200", v.Tally)
	}
}

func memberIDsFromInput(ms []VoteMember) []string {
	ids := make([]string, len(ms))
	for i, m := range ms {
		ids[i] = m.ID
	}
	return ids
}

// assertFirstReceiptUntouched 断言成功执行记录仍只有原来的一份，且凭据的
// 首次执行时间、提交序号、各笔动作原文、动作位置与前后余额均保持原样；
// 同一账户的多笔收款继续分别留痕。
func assertFirstReceiptUntouched(t *testing.T, store *Store, id string, treasuryAfter int64) *Receipt {
	t.Helper()
	all, err := store.Receipts()
	if err != nil {
		t.Fatalf("Receipts: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("receipt count=%d, want 1 (retry must not append a successful execution)", len(all))
	}
	r, ok, err := store.Receipt(id)
	if err != nil || !ok {
		t.Fatalf("Receipt(%s): ok=%v err=%v", id, ok, err)
	}
	if r.ProposalID != id || r.ExecutedAt != 300 || r.Order != 0 {
		t.Fatalf("receipt header=%+v, want id=%s executed_at=300 order=0", r, id)
	}
	if len(r.Actions) != 2 {
		t.Fatalf("receipt actions=%d, want 2", len(r.Actions))
	}
	// 初始资金库 1000：第一笔 audits 1000->900、0->100；第二笔 900->850、100->150。
	wantActions := []ActionReceipt{
		{Index: 0, Action: "transfer:audits:100"},
		{Index: 1, Action: "transfer:audits:50"},
	}
	treasury := []struct{ before, after int64 }{
		{1000, 900},
		{900, 850},
	}
	recipient := []struct{ before, after int64 }{
		{0, 100},
		{100, 150},
	}
	for i := range wantActions {
		got := r.Actions[i]
		if got.Index != wantActions[i].Index || got.Action != wantActions[i].Action {
			t.Fatalf("receipt action %d = (index=%d text=%q), want index=%d text=%q",
				i, got.Index, got.Action, wantActions[i].Index, wantActions[i].Action)
		}
		if got.Treasury.Account != "treasury" ||
			got.Treasury.Before != treasury[i].before || got.Treasury.After != treasury[i].after {
			t.Fatalf("receipt action %d treasury=%+v, want account=treasury %d->%d",
				i, got.Treasury, treasury[i].before, treasury[i].after)
		}
		if got.Recipient.Account != "audits" ||
			got.Recipient.Before != recipient[i].before || got.Recipient.After != recipient[i].after {
			t.Fatalf("receipt action %d recipient=%+v, want audits %d->%d",
				i, got.Recipient, recipient[i].before, recipient[i].after)
		}
	}
	if bal, err := store.TreasuryBalance(); err != nil || bal != treasuryAfter {
		t.Fatalf("treasury=%d err=%v, want %d", bal, err, treasuryAfter)
	}
	if bal, err := store.Balance("audits"); err != nil || bal != 150 {
		t.Fatalf("audits=%d err=%v, want 150", bal, err)
	}
	return r
}

// TestCreateVoteRetryAfterExecutionPreservesExecutedProposal 回归保护：
// 提案已有投票明细、首次计票结论与成功执行凭据（资金库已完成转账）后，
// 再次提交原创建内容必须按“已存在”成功返回，返回的仍是已执行提案，
// 而不是新建的 voting 提案；投票、计票、执行留痕与资金余额一律保留。
func TestCreateVoteRetryAfterExecutionPreservesExecutedProposal(t *testing.T) {
	store, path := openTempStore(t, 1000)
	in := executedRetryInput("gip-executed-retry")
	mustCreateVote(t, store, in)

	if b, err := store.CastVote(in.ID, "bob", true, 150); err != nil {
		t.Fatalf("bob vote: %v", err)
	} else if b.Weight != 175 {
		t.Fatalf("bob delegated weight=%d, want 175", b.Weight)
	}
	if _, err := store.CastVote(in.ID, "alice", true, 150); err != nil {
		t.Fatalf("alice vote: %v", err)
	}
	if _, err := store.CastVote(in.ID, "dave", false, 160); err != nil {
		t.Fatalf("dave vote: %v", err)
	}
	if tally, err := store.TallyVote(in.ID, 200); err != nil || !tally.Passed {
		t.Fatalf("tally=%+v err=%v", tally, err)
	}
	if _, err := store.Execute(in.ID, 300); err != nil {
		t.Fatalf("execute: %v", err)
	}

	// 首次执行后的余额基线：资金库 850，audits 150；成功记录只有一份。
	if bal, _ := store.TreasuryBalance(); bal != 850 {
		t.Fatalf("treasury after execute=%d, want 850", bal)
	}
	first := assertFirstReceiptUntouched(t, store, in.ID, 850)

	// 成员名单与委托条目换序提交，编号、权重、委托关系与其它创建内容相同：
	// 仍属同一次创建，必须 existed=true 成功返回已执行提案。
	retry := executedRetryInput(in.ID)
	retry.Members = []VoteMember{
		{ID: "erin", Weight: 25},
		{ID: "dave", Weight: 400},
		{ID: "carol", Weight: 50},
		{ID: "bob", Weight: 100},
		{ID: "alice", Weight: 300},
	}
	retry.Delegations = []Delegation{{From: "carol", To: "bob"}, {From: "erin", To: "carol"}}
	view, existed, err := store.CreateVoteProposal(retry)
	if err != nil || !existed {
		t.Fatalf("same-content retry after execution: existed=%v err=%v", existed, err)
	}
	assertExecutedProposalUntouched(t, view, in)

	// 查询展示与创建重试返回一致，且成员次序仍来自原提案。
	queried, ok, err := store.VoteProposal(in.ID)
	if err != nil || !ok {
		t.Fatalf("VoteProposal: ok=%v err=%v", ok, err)
	}
	assertExecutedProposalUntouched(t, queried, in)
	if !reflect.DeepEqual(view, queried) {
		t.Fatalf("retry view and query view differ:\nretry=%+v\nquery=%+v", view, queried)
	}

	// 重试不重新赋予一次转账机会：余额与成功执行记录保持首次执行后的原样。
	assertFirstReceiptUntouched(t, store, in.ID, 850)
	// 再次执行同样只返回首次凭据，不产生第二份成功记录或新转账。
	if again, err := store.Execute(in.ID, 99999); err != nil ||
		again.ExecutedAt != 300 || again.Order != 0 || len(again.Actions) != 2 {
		t.Fatalf("execute after create retry: %+v err=%v", again, err)
	}
	assertFirstReceiptUntouched(t, store, in.ID, 850)
	store.Close()

	// 重开状态文件：创建重试没有替换凭据或追加记录，提案查询与凭据查询
	// 仍对应同一次执行。
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	v2, ok, err := reopened.VoteProposal(in.ID)
	if err != nil || !ok {
		t.Fatalf("query after reopen: ok=%v err=%v", ok, err)
	}
	assertExecutedProposalUntouched(t, v2, in)
	r2, ok, err := reopened.Receipt(in.ID)
	if err != nil || !ok {
		t.Fatalf("receipt after reopen: ok=%v err=%v", ok, err)
	}
	if !reflect.DeepEqual(r2, first) {
		t.Fatalf("receipt changed after create retry/reopen:\nbefore=%+v\nafter =%+v", first, r2)
	}
	assertFirstReceiptUntouched(t, reopened, in.ID, 850)
}

// TestCreateVoteRetryActionSwapAfterExecutionConflicts 回归保护动作次序的含义：
// 提案已通过并完成转账后，仅交换两笔发给同一收款账户、金额不同的动作顺序，
// 即使合计金额不变、最终余额相同，也属于不同创建内容，必须返回现有的
// 提案内容冲突错误；冲突不得改变已执行状态、投票/计票记录、动作列表或余额。
func TestCreateVoteRetryActionSwapAfterExecutionConflicts(t *testing.T) {
	store, path := openTempStore(t, 1000)
	in := executedRetryInput("gip-executed-swap")
	mustCreateVote(t, store, in)
	if _, err := store.CastVote(in.ID, "bob", true, 150); err != nil {
		t.Fatalf("bob vote: %v", err)
	}
	if _, err := store.CastVote(in.ID, "alice", true, 150); err != nil {
		t.Fatalf("alice vote: %v", err)
	}
	if _, err := store.CastVote(in.ID, "dave", false, 160); err != nil {
		t.Fatalf("dave vote: %v", err)
	}
	if tally, err := store.TallyVote(in.ID, 200); err != nil || !tally.Passed {
		t.Fatalf("tally=%+v err=%v", tally, err)
	}
	original, err := store.Execute(in.ID, 300)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if bal, _ := store.TreasuryBalance(); bal != 850 {
		t.Fatalf("treasury after execute=%d, want 850", bal)
	}

	// 仅交换两笔同收款账户、不同金额动作的顺序：合计仍是 150，
	// 但动作原文的次序是创建内容的一部分，必须按冲突拒绝，
	// 不能因为最终余额相同就判为成功重试。
	swapped := executedRetryInput(in.ID)
	swapped.Actions = []string{"transfer:audits:50", "transfer:audits:100"}
	if view, existed, err := store.CreateVoteProposal(swapped); !errors.Is(err, ErrProposalConflict) || existed || view != nil {
		t.Fatalf("swapped-action retry: view=%+v existed=%v err=%v, want ErrProposalConflict", view, existed, err)
	}

	// 冲突不得改变提案：仍是 executed，投票/计票明细、动作原文与顺序保留。
	v, ok, err := store.VoteProposal(in.ID)
	if err != nil || !ok {
		t.Fatalf("VoteProposal: ok=%v err=%v", ok, err)
	}
	assertExecutedProposalUntouched(t, v, in)

	// 冲突不得改变资金库与收款账户余额，也不得追加或替换成功执行记录。
	after := assertFirstReceiptUntouched(t, store, in.ID, 850)
	if !reflect.DeepEqual(after, original) {
		t.Fatalf("receipt changed after swapped-action conflict:\nbefore=%+v\nafter =%+v", original, after)
	}
	store.Close()

	// 重开后冲突尝试同样没有留下任何痕迹。
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	v2, ok, err := reopened.VoteProposal(in.ID)
	if err != nil || !ok {
		t.Fatalf("query after reopen: ok=%v err=%v", ok, err)
	}
	assertExecutedProposalUntouched(t, v2, in)
	assertFirstReceiptUntouched(t, reopened, in.ID, 850)
}
