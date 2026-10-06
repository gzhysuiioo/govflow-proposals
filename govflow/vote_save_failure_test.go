package govflow

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readStateFile 读取状态文件当前字节，用于逐字节断言“失败前内容原样保留”。
func readStateFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	return raw
}

// assertNoTempStateFiles 断言目录中没有提交失败遗留的临时状态文件：
// 保存失败不得留下部分写出的记录文件。
func assertNoTempStateFiles(t *testing.T, statePath string) {
	t.Helper()
	leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(statePath), ".govflow-state-*"))
	if err != nil {
		t.Fatalf("glob temp state files: %v", err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("failed save left partial temp file(s): %v", leftovers)
	}
}

// assertOnlyDaveBallot 断言视图中只剩 dave 在 120 投出的 400 权重反对票，
// 提案仍在投票中且没有计票结论：alice 归集的 600 权重不得出现在任何已投记录中。
func assertOnlyDaveBallot(t *testing.T, v *VoteProposalView, id string) {
	t.Helper()
	if v.State != "voting" {
		t.Fatalf("state = %s, want voting (no tally conclusion may exist)", v.State)
	}
	if v.Tally != nil {
		t.Fatalf("tally = %+v, want nil while voting is still open", v.Tally)
	}
	if len(v.Ballots) != 1 {
		t.Fatalf("ballots = %+v, want only dave's original ballot", v.Ballots)
	}
	b := v.Ballots[0]
	if b.Representative != "dave" || b.Weight != 400 || b.Support || b.VotedAt != 120 {
		t.Fatalf("ballot = %+v, want dave/400/against/at 120 unchanged", b)
	}
	for _, vb := range v.Ballots {
		if vb.Representative == "alice" || vb.Weight == 600 {
			t.Fatalf("alice's delegated 600 weight must not appear in any recorded ballot: %+v", v.Ballots)
		}
	}
	// 成员权重、委托路径与提案动作原文不受失败影响。
	wantMembers := map[string]MemberView{
		"alice": {ID: "alice", Weight: 300, Path: []string{"alice"}, Delegate: "alice", Direct: ""},
		"bob":   {ID: "bob", Weight: 200, Path: []string{"bob", "alice"}, Delegate: "alice", Direct: "alice"},
		"carol": {ID: "carol", Weight: 100, Path: []string{"carol", "alice"}, Delegate: "alice", Direct: "alice"},
		"dave":  {ID: "dave", Weight: 400, Path: []string{"dave"}, Delegate: "dave", Direct: ""},
	}
	assertMemberViews(t, "after failed save", memberPaths(v), wantMembers)
	if !equalStrings(v.Actions, []string{"transfer:audits:100"}) {
		t.Fatalf("actions changed after failed save: %v", v.Actions)
	}
	if v.ID != id || v.Quorum != 600 || v.StartAt != 100 || v.Deadline != 200 || v.TimelockEnd != 300 {
		t.Fatalf("proposal header changed after failed save: %+v", v)
	}
}

// mustJSON 序列化查询视图，用于逐字节比较两次查询结果。
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal view: %v", err)
	}
	return string(raw)
}

// TestCastVotePreReplaceFailureKeepsPriorState：alice 首次投赞成时，保存失败
// 发生在状态文件替换之前（临时文件创建失败），则：
//   - 操作返回具体保存原因，不返回成功票据，也不是投票资格类拒绝；
//   - 查询仍只有 dave 的原记录，代表、400 权重、反对选择与首次时间都不变，
//     alice 归集的 600 权重不被计入任何已投记录，提案继续投票中、无计票结论；
//   - 原状态文件保留失败前的完整内容，重新打开后读到相同结果，
//     成员权重、委托路径与提案动作原文不受影响；
//   - 保存条件恢复后，alice 在 160 再投赞成被当作首次有效投票接受，
//     记录 600 权重与时间 160（不沿用失败尝试的 150），票据顺序 dave 在前；
//   - 截止时计票得到赞成 600、反对 400、参与 1000 并通过，失败尝试不留下
//     另一张票或额外权重；同选择重试返回首次记录，改投反对按冲突规则拒绝；
//   - 投票与计票不改变资金余额，也不生成执行凭据。
func TestCastVotePreReplaceFailureKeepsPriorState(t *testing.T) {
	store, path := openTempStore(t, 1000)
	in := baseVoteInput("gip-vote-save")
	mustCreateVote(t, store, in)
	// dave 已在 120 投出反对票（400 权重）。
	daveBallot, err := store.CastVote("gip-vote-save", "dave", false, 120)
	if err != nil {
		t.Fatalf("dave vote: %v", err)
	}
	if daveBallot.Weight != 400 || daveBallot.Support || daveBallot.VotedAt != 120 {
		t.Fatalf("dave ballot = %+v, want 400/against/at 120", daveBallot)
	}
	// 失败前的完整状态：文件字节与查询视图各留一份基准。
	beforeRaw := readStateFile(t, path)
	beforeView, ok, err := store.VoteProposal("gip-vote-save")
	if err != nil || !ok {
		t.Fatalf("query before failure: ok=%v err=%v", ok, err)
	}

	// 保存条件破坏：提交在状态文件替换之前就失败。
	makeDirUnwritable(t, path)
	ballot, err := store.CastVote("gip-vote-save", "alice", true, 150)
	if err == nil {
		t.Fatal("expected save failure, got nil error")
	}
	// 返回具体保存原因：不是“已写入但持久性未确认”，也不是投票资格/冲突类拒绝。
	if errors.Is(err, ErrDurabilityUnconfirmed) {
		t.Fatalf("pre-replacement failure must not be reported as durability warning: %v", err)
	}
	if errors.Is(err, ErrVoteRejected) || errors.Is(err, ErrProposalConflict) {
		t.Fatalf("save failure must not be reported as a voting-rule rejection: %v", err)
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("error must carry the concrete save cause, got: %v", err)
	}
	// 不返回成功票据。
	if ballot != nil {
		t.Fatalf("failed save must not return a success ballot: %+v", ballot)
	}
	// 不留下部分写出的临时记录文件。
	assertNoTempStateFiles(t, path)

	// 查询仍只有 dave 的原记录；alice 的 600 权重未被计入任何已投记录。
	view, ok, err := store.VoteProposal("gip-vote-save")
	if err != nil || !ok {
		t.Fatalf("query after failed save: ok=%v err=%v", ok, err)
	}
	assertOnlyDaveBallot(t, view, "gip-vote-save")
	if mustJSON(t, view) != mustJSON(t, beforeView) {
		t.Fatalf("view changed after failed save:\n got: %s\nwant: %s", mustJSON(t, view), mustJSON(t, beforeView))
	}
	// 原状态文件保留失败前的完整内容（逐字节一致）。
	if got := readStateFile(t, path); string(got) != string(beforeRaw) {
		t.Fatalf("state file modified by failed save")
	}

	// 重新打开（目录仍不可写、只读打开即可）：读到与失败前相同的结果。
	store.Close()
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after failed save: %v", err)
	}
	reView, ok, err := reopened.VoteProposal("gip-vote-save")
	if err != nil || !ok {
		t.Fatalf("query after reopen: ok=%v err=%v", ok, err)
	}
	assertOnlyDaveBallot(t, reView, "gip-vote-save")
	if mustJSON(t, reView) != mustJSON(t, beforeView) {
		t.Fatalf("reopened view differs from pre-failure view:\n got: %s\nwant: %s", mustJSON(t, reView), mustJSON(t, beforeView))
	}
	reopened.Close()

	// 保存条件恢复：alice 在 160 再投赞成，被当作首次有效投票接受。
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("restore dir: %v", err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatalf("reopen for retry: %v", err)
	}
	defer store.Close()
	first, err := store.CastVote("gip-vote-save", "alice", true, 160)
	if err != nil {
		t.Fatalf("alice vote after recovery: %v", err)
	}
	// 记录归集后的 600 权重与时间 160，不沿用失败尝试的 150。
	if first.Representative != "alice" || first.Weight != 600 || !first.Support || first.VotedAt != 160 {
		t.Fatalf("first effective ballot = %+v, want alice/600/for/at 160", first)
	}

	// 票据顺序仍是 dave 在前、alice 在后；失败尝试没有留下另一张票。
	view, ok, err = store.VoteProposal("gip-vote-save")
	if err != nil || !ok {
		t.Fatalf("query after retry: ok=%v err=%v", ok, err)
	}
	if len(view.Ballots) != 2 ||
		view.Ballots[0].Representative != "dave" || view.Ballots[0].VotedAt != 120 ||
		view.Ballots[1].Representative != "alice" || view.Ballots[1].VotedAt != 160 {
		t.Fatalf("ballot order/content = %+v, want dave@120 then alice@160, no extra ballot", view.Ballots)
	}

	// 同选择重试（即使时间不同）仍返回这张首次记录，不报已经投过票之外的错误。
	again, err := store.CastVote("gip-vote-save", "alice", true, 170)
	if err != nil {
		t.Fatalf("identical retry: %v", err)
	}
	if again.Weight != 600 || again.VotedAt != 160 {
		t.Fatalf("identical retry = %+v, want the first record at 160", again)
	}
	// 改投反对仍按既有冲突规则拒绝。
	if _, err := store.CastVote("gip-vote-save", "alice", false, 170); !errors.Is(err, ErrProposalConflict) {
		t.Fatalf("changing vote err=%v, want ErrProposalConflict", err)
	}

	// 截止时计票：赞成 600、反对 400、参与 1000 并通过；
	// 失败尝试不留下额外权重（参与恰好 1000，不是 1600）。
	result, err := store.TallyVote("gip-vote-save", 200)
	if err != nil {
		t.Fatalf("tally: %v", err)
	}
	if !result.Passed || result.ForWeight != 600 || result.AgainstWeight != 400 ||
		result.Turnout != 1000 || result.Quorum != 600 || result.TalliedAt != 200 {
		t.Fatalf("tally = %+v, want passed for=600 against=400 turnout=1000", result)
	}
	view, _, _ = store.VoteProposal("gip-vote-save")
	if view.State != "passed" || len(view.Ballots) != 2 {
		t.Fatalf("final view state=%s ballots=%+v, want passed with exactly two ballots", view.State, view.Ballots)
	}

	// 投票与计票本身不改变资金余额，也不生成执行凭据。
	if bal, err := store.TreasuryBalance(); err != nil || bal != 1000 {
		t.Fatalf("treasury = %d err=%v, want untouched 1000", bal, err)
	}
	if receipts, err := store.Receipts(); err != nil || len(receipts) != 0 {
		t.Fatalf("receipts = %+v err=%v, want none from voting/tally", receipts, err)
	}
}

// TestCastVotePreReplaceFailureWindowStillApplies：保存失败后投票窗口的限制
// 仍然保留——若恢复后一直到截止时刻 200 才再次提交，alice 的首次票按截止
// 边界拒绝（[start, deadline) 不含 200），旧反对票照常保留；到期计票只能
// 得到参与权重 400 的拒绝结论。投票与计票不改变资金余额，也不生成执行凭据。
func TestCastVotePreReplaceFailureWindowStillApplies(t *testing.T) {
	store, path := openTempStore(t, 1000)
	in := baseVoteInput("gip-vote-window")
	mustCreateVote(t, store, in)
	if _, err := store.CastVote("gip-vote-window", "dave", false, 120); err != nil {
		t.Fatalf("dave vote: %v", err)
	}
	beforeRaw := readStateFile(t, path)

	// alice 在 150 的首次赞成因保存失败（替换前）未生效。
	makeDirUnwritable(t, path)
	ballot, err := store.CastVote("gip-vote-window", "alice", true, 150)
	if err == nil || ballot != nil {
		t.Fatalf("expected save failure without ballot, got ballot=%+v err=%v", ballot, err)
	}
	assertNoTempStateFiles(t, path)
	if got := readStateFile(t, path); string(got) != string(beforeRaw) {
		t.Fatalf("state file modified by failed save")
	}

	// 保存条件恢复，但再次提交时已是截止时刻 200：按截止边界拒绝。
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("restore dir: %v", err)
	}
	if _, err := store.CastVote("gip-vote-window", "alice", true, 200); !errors.Is(err, ErrVoteRejected) {
		t.Fatalf("vote at deadline after recovery err=%v, want ErrVoteRejected", err)
	}
	defer store.Close()

	// 旧反对票照常保留，alice 的 600 权重仍未计入任何已投记录。
	view, ok, err := store.VoteProposal("gip-vote-window")
	if err != nil || !ok {
		t.Fatalf("query after rejected retry: ok=%v err=%v", ok, err)
	}
	assertOnlyDaveBallot(t, view, "gip-vote-window")

	// 到期计票只能得到参与权重 400 的拒绝结论（参与不足法定人数 600）。
	result, err := store.TallyVote("gip-vote-window", 200)
	if err != nil {
		t.Fatalf("tally: %v", err)
	}
	if result.Passed || result.ForWeight != 0 || result.AgainstWeight != 400 ||
		result.Turnout != 400 || result.Quorum != 600 {
		t.Fatalf("tally = %+v, want rejected with turnout 400", result)
	}
	view, _, _ = store.VoteProposal("gip-vote-window")
	if view.State != "rejected" || len(view.Ballots) != 1 {
		t.Fatalf("final view state=%s ballots=%+v, want rejected with only dave's ballot", view.State, view.Ballots)
	}

	// 投票与计票本身不改变资金余额，也不生成执行凭据。
	if bal, err := store.TreasuryBalance(); err != nil || bal != 1000 {
		t.Fatalf("treasury = %d err=%v, want untouched 1000", bal, err)
	}
	if receipts, err := store.Receipts(); err != nil || len(receipts) != 0 {
		t.Fatalf("receipts = %+v err=%v, want none from voting/tally", receipts, err)
	}
}
