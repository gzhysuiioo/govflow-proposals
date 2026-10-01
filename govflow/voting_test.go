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
