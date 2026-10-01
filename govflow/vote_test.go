package govflow

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func votingSpec(id string) VotingSpec {
	return VotingSpec{
		ID: id,
		Members: []VotingMemberSpec{
			{ID: "alice", Weight: 60},
			{ID: "bob", Weight: 40},
			{ID: "carol", Weight: 20},
		},
		Quorum:   80,
		Start:    100,
		End:      200,
		Timelock: 500,
		Actions:  []string{"transfer:audits:10"},
	}
}

func mustCreateVoting(t *testing.T, store *Store, spec VotingSpec) {
	t.Helper()
	existed, err := store.CreateVoting(spec)
	if err != nil {
		t.Fatalf("CreateVoting(%s): %v", spec.ID, err)
	}
	if existed {
		t.Fatalf("CreateVoting(%s) unexpectedly reported existed", spec.ID)
	}
}

func TestCreateVotingHappyPathAndIdempotent(t *testing.T) {
	store, _ := openTempStore(t, 1000)
	defer store.Close()

	spec := votingSpec("gip-1")
	mustCreateVoting(t, store, spec)

	// 完全相同的重试：existed=true，不新增、不改状态。
	existed, err := store.CreateVoting(spec)
	if err != nil || !existed {
		t.Fatalf("identical retry existed=%v err=%v", existed, err)
	}

	// 成员与委托的输入次序不影响比较。
	reordered := spec
	reordered.Members = []VotingMemberSpec{
		{ID: "carol", Weight: 20},
		{ID: "alice", Weight: 60},
		{ID: "bob", Weight: 40},
	}
	existed, err = store.CreateVoting(reordered)
	if err != nil || !existed {
		t.Fatalf("reordered retry existed=%v err=%v", existed, err)
	}
	// 带委托的提案同样按集合比较。
	withDel := votingSpec("gip-del-set")
	withDel.Delegations = []VotingDelegationSpec{
		{From: "carol", To: "alice"},
		{From: "bob", To: "alice"},
	}
	mustCreateVoting(t, store, withDel)
	reorderedDel := withDel
	reorderedDel.Delegations = []VotingDelegationSpec{
		{From: "bob", To: "alice"},
		{From: "carol", To: "alice"},
	}
	existed, err = store.CreateVoting(reorderedDel)
	if err != nil || !existed {
		t.Fatalf("reordered delegation retry existed=%v err=%v", existed, err)
	}

	// 内容不同报冲突。
	diff := spec
	diff.Quorum = 81
	if _, err := store.CreateVoting(diff); !errors.Is(err, ErrVotingConflict) {
		t.Fatalf("quorum conflict err=%v", err)
	}
	diff = spec
	diff.Members = []VotingMemberSpec{
		{ID: "alice", Weight: 60},
		{ID: "bob", Weight: 41},
		{ID: "carol", Weight: 20},
	}
	if _, err := store.CreateVoting(diff); !errors.Is(err, ErrVotingConflict) {
		t.Fatalf("weight conflict err=%v", err)
	}
	diff = spec
	diff.Actions = []string{"transfer:audits:11"}
	if _, err := store.CreateVoting(diff); !errors.Is(err, ErrVotingConflict) {
		t.Fatalf("action conflict err=%v", err)
	}
	// 动作顺序不同也是冲突。
	ordered := votingSpec("gip-order")
	ordered.Actions = []string{"transfer:audits:10", "transfer:legal:1"}
	mustCreateVoting(t, store, ordered)
	reversed := ordered
	reversed.Actions = []string{"transfer:legal:1", "transfer:audits:10"}
	if _, err := store.CreateVoting(reversed); !errors.Is(err, ErrVotingConflict) {
		t.Fatalf("action order conflict err=%v", err)
	}

	record, ok, err := store.Voting("gip-1")
	if err != nil || !ok {
		t.Fatalf("Voting: ok=%v err=%v", ok, err)
	}
	if record.State != "voting" || record.Quorum != 80 || record.TotalWeight != 120 {
		t.Fatalf("unexpected record: %+v", record)
	}
	if len(record.Members) != 3 {
		t.Fatalf("members=%d", len(record.Members))
	}
	// 无委托时每人代表自己，票重等于原始权重。
	for _, m := range record.Members {
		if m.Representative != m.ID || m.RepresentativeWeight != m.Weight {
			t.Fatalf("member %s: rep=%s repWeight=%d", m.ID, m.Representative, m.RepresentativeWeight)
		}
		if len(m.DelegationPath) != 1 || m.DelegationPath[0] != m.ID {
			t.Fatalf("member %s path=%v", m.ID, m.DelegationPath)
		}
	}
}

func TestCreateVotingValidation(t *testing.T) {
	store, _ := openTempStore(t, 1000)
	defer store.Close()

	cases := []struct {
		name string
		mod  func(*VotingSpec)
	}{
		{"empty id", func(s *VotingSpec) { s.ID = "" }},
		{"empty members", func(s *VotingSpec) { s.Members = nil }},
		{"empty member id", func(s *VotingSpec) { s.Members[0].ID = "" }},
		{"duplicate member", func(s *VotingSpec) { s.Members[1].ID = "alice" }},
		{"zero weight", func(s *VotingSpec) { s.Members[0].Weight = 0 }},
		{"negative weight", func(s *VotingSpec) { s.Members[0].Weight = -1 }},
		{"quorum zero", func(s *VotingSpec) { s.Quorum = 0 }},
		{"quorum too high", func(s *VotingSpec) { s.Quorum = 121 }},
		{"start negative", func(s *VotingSpec) { s.Start = -1 }},
		{"start equals end", func(s *VotingSpec) { s.Start = 200 }},
		{"start after end", func(s *VotingSpec) { s.Start = 201 }},
		{"end after timelock", func(s *VotingSpec) { s.Timelock = 199 }},
		{"self delegation", func(s *VotingSpec) { s.Delegations = []VotingDelegationSpec{{From: "alice", To: "alice"}} }},
		{"delegate unknown from", func(s *VotingSpec) { s.Delegations = []VotingDelegationSpec{{From: "nobody", To: "alice"}} }},
		{"delegate unknown to", func(s *VotingSpec) { s.Delegations = []VotingDelegationSpec{{From: "bob", To: "nobody"}} }},
		{"duplicate delegation", func(s *VotingSpec) {
			s.Delegations = []VotingDelegationSpec{{From: "bob", To: "alice"}, {From: "bob", To: "carol"}}
		}},
		{"delegation cycle", func(s *VotingSpec) {
			s.Delegations = []VotingDelegationSpec{
				{From: "alice", To: "bob"},
				{From: "bob", To: "carol"},
				{From: "carol", To: "alice"},
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := votingSpec("bad-" + tc.name)
			tc.mod(&spec)
			if _, err := store.CreateVoting(spec); !errors.Is(err, ErrInvalidVoting) {
				t.Fatalf("err=%v, want ErrInvalidVoting", err)
			}
		})
	}

	// 总权重溢出：MaxInt64 与 1 相加溢出。
	spec := votingSpec("overflow")
	spec.Members = []VotingMemberSpec{
		{ID: "alice", Weight: math.MaxInt64},
		{ID: "bob", Weight: 1},
	}
	if _, err := store.CreateVoting(spec); !errors.Is(err, ErrInvalidVoting) {
		t.Fatalf("overflow err=%v", err)
	}
}

func TestDelegationChainAndAttributedWeight(t *testing.T) {
	store, _ := openTempStore(t, 1000)
	defer store.Close()

	spec := votingSpec("gip-del")
	spec.Delegations = []VotingDelegationSpec{
		{From: "carol", To: "bob"}, // carol -> bob -> alice
		{From: "bob", To: "alice"},
	}
	mustCreateVoting(t, store, spec)

	record, ok, err := store.Voting("gip-del")
	if err != nil || !ok {
		t.Fatalf("Voting: ok=%v err=%v", ok, err)
	}
	byID := map[string]VotingMemberRecord{}
	for _, m := range record.Members {
		byID[m.ID] = m
	}
	if byID["alice"].Representative != "alice" || byID["alice"].RepresentativeWeight != 120 {
		t.Fatalf("alice: rep=%s weight=%d", byID["alice"].Representative, byID["alice"].RepresentativeWeight)
	}
	if byID["bob"].Representative != "alice" || byID["bob"].RepresentativeWeight != 120 {
		t.Fatalf("bob: rep=%s weight=%d", byID["bob"].Representative, byID["bob"].RepresentativeWeight)
	}
	if byID["carol"].Representative != "alice" {
		t.Fatalf("carol: rep=%s", byID["carol"].Representative)
	}
	if got := strings.Join(byID["carol"].DelegationPath, "->"); got != "carol->bob->alice" {
		t.Fatalf("carol path=%s", got)
	}

	// 只有最终代表 alice 可投票，票重为全部权重 120。
	vote, err := store.Vote("gip-del", "alice", true, 150)
	if err != nil {
		t.Fatalf("Vote: %v", err)
	}
	if vote.Weight != 120 || vote.Voter != "alice" || !vote.Support || vote.VotedAt != 150 {
		t.Fatalf("unexpected vote: %+v", vote)
	}

	// 已委托出去的人投票被拒绝。
	if _, err := store.Vote("gip-del", "bob", true, 150); !errors.Is(err, ErrNotVotingProposal) {
		t.Fatalf("delegated member vote err=%v", err)
	}
	if _, err := store.Vote("gip-del", "carol", false, 150); !errors.Is(err, ErrNotVotingProposal) {
		t.Fatalf("delegated member vote err=%v", err)
	}
	// 不在名单的人投票被拒绝。
	if _, err := store.Vote("gip-del", "nobody", true, 150); !errors.Is(err, ErrNotVotingProposal) {
		t.Fatalf("non-member vote err=%v", err)
	}
}

func TestVoteWindowAndIdempotency(t *testing.T) {
	store, _ := openTempStore(t, 1000)
	defer store.Close()

	mustCreateVoting(t, store, votingSpec("gip-v"))

	// 窗口外拒绝。
	if _, err := store.Vote("gip-v", "alice", true, 99); !errors.Is(err, ErrVotingClosed) {
		t.Fatalf("before start err=%v", err)
	}
	if _, err := store.Vote("gip-v", "alice", true, 200); !errors.Is(err, ErrVotingClosed) {
		t.Fatalf("at end err=%v", err)
	}
	if _, err := store.Vote("gip-v", "alice", true, 201); !errors.Is(err, ErrVotingClosed) {
		t.Fatalf("after end err=%v", err)
	}

	// 窗口内投票成功。
	first, err := store.Vote("gip-v", "alice", true, 150)
	if err != nil {
		t.Fatalf("Vote: %v", err)
	}
	// 相同选择的重试始终返回首次记录（时间不同也不改变）。
	retry, err := store.Vote("gip-v", "alice", true, 151)
	if err != nil {
		t.Fatalf("Vote retry: %v", err)
	}
	if retry.VotedAt != 150 || retry.Weight != first.Weight || retry.Support != first.Support {
		t.Fatalf("retry changed record: %+v vs %+v", retry, first)
	}
	// 改投另一选择报冲突。
	if _, err := store.Vote("gip-v", "alice", false, 152); !errors.Is(err, ErrVoteConflict) {
		t.Fatalf("different choice err=%v", err)
	}

	record, _, err := store.Voting("gip-v")
	if err != nil {
		t.Fatalf("Voting: %v", err)
	}
	if len(record.Votes) != 1 {
		t.Fatalf("votes=%d, want 1 (retries must not append)", len(record.Votes))
	}
	if record.ForVotes != 60 || record.AgainstVotes != 0 || record.Turnout != 60 {
		t.Fatalf("for=%d against=%d turnout=%d", record.ForVotes, record.AgainstVotes, record.Turnout)
	}
}

func TestTallyRules(t *testing.T) {
	t.Run("pass with quorum and majority", func(t *testing.T) {
		store, _ := openTempStore(t, 1000)
		defer store.Close()
		mustCreateVoting(t, store, votingSpec("gip-pass"))
		// alice 60 for, bob 40 against, carol 20 未投。
		if _, err := store.Vote("gip-pass", "alice", true, 150); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Vote("gip-pass", "bob", false, 151); err != nil {
			t.Fatal(err)
		}
		// 截止前计票被拒绝，状态不变。
		if _, err := store.Tally("gip-pass", 199); !errors.Is(err, ErrTallyTooEarly) {
			t.Fatalf("early tally err=%v", err)
		}
		record, _, _ := store.Voting("gip-pass")
		if record.State != "voting" || record.TalliedAt != nil {
			t.Fatalf("state changed after early tally: %+v", record)
		}
		// 截止时刻计票：turnout=100 >= quorum 80，for 60 > against 40，通过。
		tally, err := store.Tally("gip-pass", 200)
		if err != nil {
			t.Fatalf("Tally: %v", err)
		}
		if tally.Result != "passed" || tally.ForVotes != 60 || tally.AgainstVotes != 40 || tally.Turnout != 100 || tally.TalliedAt != 200 {
			t.Fatalf("unexpected tally: %+v", tally)
		}
		// 再次计票返回首次结论。
		tally2, err := store.Tally("gip-pass", 9999)
		if err != nil || tally2.Result != "passed" || tally2.TalliedAt != 200 {
			t.Fatalf("re-tally: %+v err=%v", tally2, err)
		}
		record, _, _ = store.Voting("gip-pass")
		if record.State != "passed" {
			t.Fatalf("state=%s, want passed", record.State)
		}
	})

	t.Run("tie rejected", func(t *testing.T) {
		store, _ := openTempStore(t, 1000)
		defer store.Close()
		mustCreateVoting(t, store, votingSpec("gip-tie"))
		if _, err := store.Vote("gip-tie", "alice", true, 150); err != nil {
			t.Fatal(err)
		}
		// bob 40 against, carol 20 未投：turnout=100 过法定人数，但 for 60 < against...
		// 用 60:60 平票：alice 60 for, bob+carol 60 against。
		if _, err := store.Vote("gip-tie", "bob", false, 151); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Vote("gip-tie", "carol", false, 152); err != nil {
			t.Fatal(err)
		}
		tally, err := store.Tally("gip-tie", 200)
		if err != nil {
			t.Fatalf("Tally: %v", err)
		}
		if tally.Result != "rejected" || tally.ForVotes != 60 || tally.AgainstVotes != 60 {
			t.Fatalf("tie tally: %+v", tally)
		}
	})

	t.Run("quorum not met rejected", func(t *testing.T) {
		store, _ := openTempStore(t, 1000)
		defer store.Close()
		mustCreateVoting(t, store, votingSpec("gip-noq"))
		// 只有 alice 60 票：turnout=60 < quorum 80，未投权重不计入参与量。
		if _, err := store.Vote("gip-noq", "alice", true, 150); err != nil {
			t.Fatal(err)
		}
		tally, err := store.Tally("gip-noq", 200)
		if err != nil {
			t.Fatalf("Tally: %v", err)
		}
		if tally.Result != "rejected" || tally.Turnout != 60 {
			t.Fatalf("tally: %+v", tally)
		}
	})

	t.Run("unanimous for passes", func(t *testing.T) {
		store, _ := openTempStore(t, 1000)
		defer store.Close()
		mustCreateVoting(t, store, votingSpec("gip-all"))
		for _, m := range []string{"alice", "bob", "carol"} {
			if _, err := store.Vote("gip-all", m, true, 150); err != nil {
				t.Fatal(err)
			}
		}
		tally, err := store.Tally("gip-all", 200)
		if err != nil {
			t.Fatalf("Tally: %v", err)
		}
		if tally.Result != "passed" || tally.Turnout != 120 {
			t.Fatalf("tally: %+v", tally)
		}
	})
}

func TestVoteAfterTallyRejected(t *testing.T) {
	store, _ := openTempStore(t, 1000)
	defer store.Close()
	mustCreateVoting(t, store, votingSpec("gip-done"))
	if _, err := store.Vote("gip-done", "alice", true, 150); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Tally("gip-done", 200); err != nil {
		t.Fatal(err)
	}
	// 计票后即使传入窗口内时间也拒绝新票。
	if _, err := store.Vote("gip-done", "bob", false, 150); !errors.Is(err, ErrVotingClosed) {
		t.Fatalf("vote after tally err=%v", err)
	}
	if _, err := store.Vote("gip-done", "alice", false, 150); !errors.Is(err, ErrVotingClosed) {
		t.Fatalf("changed vote after tally err=%v", err)
	}
}

func TestPassedVotingProposalExecutesWithoutRegister(t *testing.T) {
	store, _ := openTempStore(t, 1000)
	defer store.Close()

	// 资金库初始 1000。
	spec := votingSpec("gip-exec")
	mustCreateVoting(t, store, spec)
	if _, err := store.Vote("gip-exec", "alice", true, 150); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Vote("gip-exec", "bob", true, 151); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Tally("gip-exec", 200); err != nil {
		t.Fatal(err)
	}

	// 通过后直接执行，无须登记。
	receipt, err := store.Execute("gip-exec", 500)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if receipt.ProposalID != "gip-exec" || receipt.ExecutedAt != 500 || len(receipt.Actions) != 1 {
		t.Fatalf("unexpected receipt: %+v", receipt)
	}
	treasury, _ := store.TreasuryBalance()
	if treasury != 990 {
		t.Fatalf("treasury=%d, want 990", treasury)
	}
	bal, _ := store.Balance("audits")
	if bal != 10 {
		t.Fatalf("audits balance=%d, want 10", bal)
	}

	// 执行后再计票不退回通过结论。
	tally, err := store.Tally("gip-exec", 9999)
	if err != nil || tally.Result != "passed" || tally.TalliedAt != 200 {
		t.Fatalf("re-tally after execute: %+v err=%v", tally, err)
	}
	record, _, _ := store.Voting("gip-exec")
	if record.State != "executed" {
		t.Fatalf("state=%s, want executed", record.State)
	}

	// register 不能覆盖投票提案。
	if _, err := store.Register("gip-exec", 500, []string{"transfer:audits:10"}); !errors.Is(err, ErrProposalConflict) {
		t.Fatalf("register over voting proposal err=%v", err)
	}
}

func TestRegisterCannotCreateOverVoting(t *testing.T) {
	store, _ := openTempStore(t, 1000)
	defer store.Close()
	mustCreateVoting(t, store, votingSpec("gip-shared"))
	if _, err := store.Register("gip-shared", 500, []string{"transfer:audits:10"}); !errors.Is(err, ErrProposalConflict) {
		t.Fatalf("register err=%v", err)
	}
	// 登记提案也不能被投票提案占用编号。
	mustRegister(t, store, "gip-reg", 100, "transfer:a:1")
	spec := votingSpec("gip-reg")
	if _, err := store.CreateVoting(spec); !errors.Is(err, ErrProposalConflict) {
		t.Fatalf("voting over registered err=%v", err)
	}
}

func TestVotingPersistenceAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "treasury.json")
	store, err := InitTreasury(path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	spec := votingSpec("gip-persist")
	spec.Delegations = []VotingDelegationSpec{{From: "bob", To: "alice"}}
	mustCreateVoting(t, store, spec)
	if _, err := store.Vote("gip-persist", "alice", true, 150); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Vote("gip-persist", "carol", false, 151); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Tally("gip-persist", 200); err != nil {
		t.Fatal(err)
	}
	store.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer reopened.Close()
	record, ok, err := reopened.Voting("gip-persist")
	if err != nil || !ok {
		t.Fatalf("Voting: ok=%v err=%v", ok, err)
	}
	if record.State != "passed" || record.ForVotes != 100 || record.AgainstVotes != 20 || record.Turnout != 120 {
		t.Fatalf("persisted record: %+v", record)
	}
	if record.TalliedAt == nil || *record.TalliedAt != 200 {
		t.Fatalf("tallied_at=%v", record.TalliedAt)
	}
	// 重开后计票幂等。
	tally, err := reopened.Tally("gip-persist", 9999)
	if err != nil || tally.Result != "passed" {
		t.Fatalf("re-tally: %+v err=%v", tally, err)
	}
}

func TestVotingCorruptionRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "treasury.json")
	store, err := InitTreasury(path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	mustCreateVoting(t, store, votingSpec("gip-corrupt"))
	if _, err := store.Vote("gip-corrupt", "alice", true, 150); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Vote("gip-corrupt", "bob", true, 151); err != nil {
		t.Fatal(err)
	}
	store.Close()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// 篡改票重：票重与名单归属不一致，拒绝打开。
	tampered := strings.Replace(string(raw), `"weight": 60`, `"weight": 61`, 1)
	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("tampered vote weight open err=%v, want ErrStateCorrupt", err)
	}

	// 恢复后计票通过，再篡改计票结论：结论与明细不一致。
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	store2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store2.Tally("gip-corrupt", 200); err != nil {
		t.Fatal(err)
	}
	store2.Close()
	raw2, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw2), `"result": "passed"`) {
		t.Fatalf("expected passed result in file")
	}
	tampered2 := strings.Replace(string(raw2), `"result": "passed"`, `"result": "rejected"`, 1)
	if err := os.WriteFile(path, []byte(tampered2), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("tampered tally result open err=%v, want ErrStateCorrupt", err)
	}
}

func TestConcurrentVotesOneRecordPerRepresentative(t *testing.T) {
	store, _ := openTempStore(t, 1000)
	defer store.Close()
	mustCreateVoting(t, store, votingSpec("gip-conc"))

	var wg sync.WaitGroup
	errs := make(chan error, 30)
	// 同一代表、相同选择并发投 10 次：只应有一条记录。
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.Vote("gip-conc", "alice", true, 150)
			errs <- err
		}()
	}
	// 同一代表改投反对：与赞成竞争，最终要么首条赞成要么冲突报错，
	// 但不得出现两条记录或静默覆盖。
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.Vote("gip-conc", "alice", false, 150)
			errs <- err
		}()
	}
	// 其他代表正常投票。
	for _, m := range []string{"bob", "carol"} {
		wg.Add(1)
		go func(member string) {
			defer wg.Done()
			_, err := store.Vote("gip-conc", member, true, 150)
			errs <- err
		}(m)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil && !errors.Is(err, ErrVoteConflict) && !errors.Is(err, ErrVotingClosed) {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	record, _, err := store.Voting("gip-conc")
	if err != nil {
		t.Fatal(err)
	}
	// 每名最终代表至多一条票记录。
	seen := map[string]bool{}
	for _, v := range record.Votes {
		if seen[v.Voter] {
			t.Fatalf("duplicate vote from %s", v.Voter)
		}
		seen[v.Voter] = true
	}
	if len(record.Votes) > 3 {
		t.Fatalf("votes=%d, want <= 3", len(record.Votes))
	}
	// 票重必须与名单归属一致。
	if record.ForVotes+record.AgainstVotes != record.Turnout {
		t.Fatalf("turnout mismatch")
	}
}

func TestConcurrentTallyOneConclusion(t *testing.T) {
	store, _ := openTempStore(t, 1000)
	defer store.Close()
	mustCreateVoting(t, store, votingSpec("gip-tally-conc"))
	if _, err := store.Vote("gip-tally-conc", "alice", true, 150); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Vote("gip-tally-conc", "bob", true, 151); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	results := make(chan *TallyRecord, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := store.Tally("gip-tally-conc", 200)
			if err == nil {
				results <- r
			}
		}()
	}
	wg.Wait()
	close(results)
	var first *TallyRecord
	for r := range results {
		if first == nil {
			first = r
		} else if r.TalliedAt != first.TalliedAt || r.Result != first.Result {
			t.Fatalf("tally conclusions differ: %+v vs %+v", r, first)
		}
	}
	if first == nil || first.Result != "passed" || first.TalliedAt != 200 || first.Turnout != 100 {
		t.Fatalf("first tally: %+v", first)
	}
}

func TestVotingQueries(t *testing.T) {
	store, _ := openTempStore(t, 1000)
	defer store.Close()

	if _, ok, err := store.Voting("missing"); err != nil || ok {
		t.Fatalf("missing voting: ok=%v err=%v", ok, err)
	}
	mustCreateVoting(t, store, votingSpec("gip-b"))
	mustCreateVoting(t, store, votingSpec("gip-a"))

	records, err := store.Votings()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].ID != "gip-a" || records[1].ID != "gip-b" {
		t.Fatalf("Votings order: %+v", records)
	}
}

func TestOldStateFileWithoutVotingsOpens(t *testing.T) {
	// 旧状态文件没有 votings 字段，必须能正常打开并使用登记/执行。
	store, path := openTempStore(t, 1000)
	mustRegister(t, store, "gip-old", 100, "transfer:a:5")
	store.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("Open old-style state: %v", err)
	}
	defer reopened.Close()
	if _, ok, err := reopened.Voting("gip-old"); err != nil || ok {
		t.Fatalf("Voting on registered proposal: ok=%v err=%v", ok, err)
	}
	receipt, err := reopened.Execute("gip-old", 100)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if receipt.ProposalID != "gip-old" {
		t.Fatalf("receipt: %+v", receipt)
	}
}
