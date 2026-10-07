package govflow

import (
	"errors"
	"testing"
)

// 本文件为投票提案的三个现有查询入口——按编号查询（VoteProposal）、
// 列出全部（VoteProposals）、综合提案快照（ProposalsSnapshot 中的 Voting
// 列表）——补充一组回归保障，保护“查询结果是取得当时的独立副本”这条约定：
//
//  1. 同一份结果内的嵌套数据彼此独立：多跳委托、不同成员的路径经过同一个
//     中间成员时，每人的路径仍各自完整保留“从本人到最终代表”的顺序；
//  2. 调用方改写、增删自己拿到的成员、动作、票据或计票结论，只影响自己
//     持有的数据：同批取得的另一份结果、随后通过任一入口重新读到的内容、
//     以及列表中的另一项提案都保持平台实际保存的内容；
//  3. 这种编辑是调用方处理查询返回值，不是业务操作：平台保存的委托关系、
//     原始权重、已投票据与首次计票结论不被覆盖，也不被解释成接受了新的
//     成员、委托或动作；
//  4. 旧结果不会被后续正常业务反向更新：无人投票时取得的结果保持空票据
//     列表与“尚未计票”的含义；随后窗口内投票、到期计票只出现在新查询中。
//
// 这些保障只覆盖公开查询行为，保留现有返回类型、字段含义与排序，不增加
// 新的查询功能；既有的投票窗口、代表资格、法定人数与多数规则继续适用。

// copyChainInput 构造本文件共用的委托拓扑：
//   - 多跳链 甲→乙→丙，且 丁 的路径也经过同一个中间成员 乙（丁→乙→丙），
//     用于证明每名成员各自保留完整路径，互不共用底层数组；
//   - 丙是最终代表，归集 10+20+40+80=150 的原始权重；戊未委托，路径只有本人；
//   - 法定人数 100，窗口 [100,200)，时间锁 300；
//   - 两个动作按固定原文与顺序保存，用于保护动作文本与顺序不被其他结果波及。
func copyChainInput(id string) *CreateVoteInput {
	return &CreateVoteInput{
		ID: id,
		Members: []VoteMember{
			{ID: "甲", Weight: 10},
			{ID: "乙", Weight: 20},
			{ID: "丙", Weight: 40},
			{ID: "丁", Weight: 80},
			{ID: "戊", Weight: 5},
		},
		Delegations: []Delegation{
			{From: "甲", To: "乙"},
			{From: "乙", To: "丙"},
			{From: "丁", To: "乙"},
		},
		Quorum:      100,
		StartAt:     100,
		Deadline:    200,
		TimelockEnd: 300,
		Actions:     []string{"transfer:audits:100", "transfer:legal:50"},
	}
}

// copyChainMemberViews 是 copyChainInput 对应提案保存状态下应展示的成员视图：
// 甲 [甲 乙 丙]、乙 [乙 丙]、丁 [丁 乙 丙] 三条路径都保留到最终代表丙的完整
// 顺序，丙与戊各自只代表自己；原始权重不被委托改变。
func copyChainMemberViews() map[string]MemberView {
	return map[string]MemberView{
		"甲": {ID: "甲", Weight: 10, Path: []string{"甲", "乙", "丙"}, Delegate: "丙", Direct: "乙"},
		"乙": {ID: "乙", Weight: 20, Path: []string{"乙", "丙"}, Delegate: "丙", Direct: "丙"},
		"丙": {ID: "丙", Weight: 40, Path: []string{"丙"}, Delegate: "丙", Direct: ""},
		"丁": {ID: "丁", Weight: 80, Path: []string{"丁", "乙", "丙"}, Delegate: "丙", Direct: "乙"},
		"戊": {ID: "戊", Weight: 5, Path: []string{"戊"}, Delegate: "戊", Direct: ""},
	}
}

func copyChainActions() []string {
	return []string{"transfer:audits:100", "transfer:legal:50"}
}

// assertCopyChainView 断言一份查询视图保持平台保存的全部内容：提案字段、
// 动作原文与顺序、每名成员的完整委托路径/最终代表/直接委托对象/原始权重。
func assertCopyChainView(t *testing.T, prefix string, v *VoteProposalView) {
	t.Helper()
	if v.ID == "" || v.Source != "vote" {
		t.Fatalf("%s: id/source = %q/%q", prefix, v.ID, v.Source)
	}
	if v.Quorum != 100 || v.StartAt != 100 || v.Deadline != 200 ||
		v.TimelockEnd != 300 || v.TotalWeight != 155 {
		t.Fatalf("%s: proposal parameters changed: %+v", prefix, v)
	}
	if !sameActions(v.Actions, copyChainActions()) {
		t.Fatalf("%s: actions = %v, want original text and order %v", prefix, v.Actions, copyChainActions())
	}
	assertMemberViews(t, prefix, memberPaths(v), copyChainMemberViews())
	// 成员仍按提交顺序展示，查询副本不得重排。
	if ids := memberIDs(v.Members); !equalStrings(ids, []string{"甲", "乙", "丙", "丁", "戊"}) {
		t.Fatalf("%s: member order changed: %v", prefix, ids)
	}
}

// assertCopyChainBallotAndTally 断言到期投票计票后的保存内容：
// 丙在 150 投出归集权重 150 的赞成票，200 首次计票为通过，
// 赞成 150、反对 0、参与 150（未投的戊的 5 不计入参与量）。
func assertCopyChainBallotAndTally(t *testing.T, prefix string, v *VoteProposalView) {
	t.Helper()
	if len(v.Ballots) != 1 {
		t.Fatalf("%s: ballots = %+v, want exactly 丙's ballot", prefix, v.Ballots)
	}
	b := v.Ballots[0]
	if b.Representative != "丙" || b.Weight != 150 || !b.Support || b.VotedAt != 150 {
		t.Fatalf("%s: ballot = %+v, want 丙 weight=150 for voted_at=150", prefix, b)
	}
	if v.Tally == nil {
		t.Fatalf("%s: tally is nil, want the saved first tally", prefix)
	}
	if !v.Tally.Passed || v.Tally.ForWeight != 150 || v.Tally.AgainstWeight != 0 ||
		v.Tally.Turnout != 150 || v.Tally.Quorum != 100 || v.Tally.TalliedAt != 200 {
		t.Fatalf("%s: tally = %+v, want passed for=150 against=0 turnout=150 at 200", prefix, v.Tally)
	}
	if v.State != "passed" {
		t.Fatalf("%s: state = %q, want passed", prefix, v.State)
	}
}

// viewFetcher 是三个现有查询入口的统一取数方式：都按编号返回同一项提案的
// 完整投票视图。每个入口都必须满足同一份副本独立性约定。
type viewFetcher struct {
	name  string
	fetch func(t *testing.T, store *Store, id string) *VoteProposalView
}

func voteViewFetchers() []viewFetcher {
	return []viewFetcher{
		{
			name: "VoteProposal",
			fetch: func(t *testing.T, store *Store, id string) *VoteProposalView {
				t.Helper()
				v, ok, err := store.VoteProposal(id)
				if err != nil || !ok {
					t.Fatalf("VoteProposal(%s): ok=%v err=%v", id, ok, err)
				}
				return v
			},
		},
		{
			name: "VoteProposals",
			fetch: func(t *testing.T, store *Store, id string) *VoteProposalView {
				t.Helper()
				all, err := store.VoteProposals()
				if err != nil {
					t.Fatalf("VoteProposals: %v", err)
				}
				return mustFindView(t, id, all)
			},
		},
		{
			name: "ProposalsSnapshot",
			fetch: func(t *testing.T, store *Store, id string) *VoteProposalView {
				t.Helper()
				snap, err := store.ProposalsSnapshot()
				if err != nil {
					t.Fatalf("ProposalsSnapshot: %v", err)
				}
				return mustFindView(t, id, snap.Voting)
			},
		},
	}
}

func mustFindView(t *testing.T, id string, views []*VoteProposalView) *VoteProposalView {
	t.Helper()
	for _, v := range views {
		if v.ID == id {
			return v
		}
	}
	t.Fatalf("proposal %q missing from %d returned views", id, len(views))
	return nil
}

// corruptOneView 以调用方身份对一份查询结果做各种就地编辑：改写成员编号/权重
// 与委托路径、改写并增删动作、改写并追加票据、改写计票结论。它只处理返回值，
// 不调用任何业务操作，因此任何一份平台保存内容都不应随之改变。
func corruptOneView(v *VoteProposalView) {
	for i := range v.Members {
		m := &v.Members[i]
		m.ID = "HACKED-" + m.ID
		m.Weight = -m.Weight - 1
		m.Path = append(m.Path, "HACK")
		m.Delegate = "HACKED"
		m.Direct = "HACKED"
	}
	v.Members = append(v.Members, MemberView{
		ID: "闯入者", Weight: 999, Path: []string{"闯入者"}, Delegate: "闯入者",
	})
	v.Actions[0] = "transfer:attacker:999"
	v.Actions = append(v.Actions, "transfer:intruder:1")
	if len(v.Ballots) > 0 {
		v.Ballots[0].Representative = "闯入者"
		v.Ballots[0].Weight = 1
		v.Ballots[0].Support = false
		v.Ballots[0].VotedAt = -1
	}
	v.Ballots = append(v.Ballots, BallotView{
		Representative: "闯入者", Weight: 999, Support: true, VotedAt: 101,
	})
	if v.Tally != nil {
		v.Tally.ForWeight, v.Tally.AgainstWeight = 0, 999
		v.Tally.Turnout = 999
		v.Tally.Passed = false
		v.Tally.TalliedAt = -1
	}
	v.State = "executed"
	v.TotalWeight = -1
}

// TestVoteQueryNestedPathsAreIndependentCopies：同一提案含多跳委托且不同成员的
// 路径经过同一个中间成员时，每名成员的路径都必须是各自独立的完整副本——
// 调用方改写其中一人的路径或成员信息，不能连带改动同一结果中的其他成员，
// 也不能改动此前取得的另一份查询结果。动作原文与顺序、票据代表及归集权重、
// 投票选择与首次时间、计票汇总与首次计票时间同样不受另一份结果上的编辑影响。
// 三个查询入口逐一覆盖。
func TestVoteQueryNestedPathsAreIndependentCopies(t *testing.T) {
	for _, f := range voteViewFetchers() {
		t.Run(f.name, func(t *testing.T) {
			store, _ := openTempStore(t, 1000)
			defer store.Close()
			const id = "gip-query-copy-paths"
			mustCreateVote(t, store, copyChainInput(id))
			if _, err := store.CastVote(id, "丙", true, 150); err != nil {
				t.Fatalf("丙 vote: %v", err)
			}
			if _, err := store.TallyVote(id, 200); err != nil {
				t.Fatalf("tally: %v", err)
			}

			first := f.fetch(t, store, id)
			assertCopyChainView(t, "first result", first)
			assertCopyChainBallotAndTally(t, "first result", first)
			// 取得第二份结果后，所有后续编辑只作用在 first 上。
			second := f.fetch(t, store, id)

			// 先验证同一结果内部嵌套路径的独立性：改写甲路径中下标 1 的
			// 共享中间成员“乙”，并追加节点。乙的路径 [乙 丙] 与丁的
			// [丁 乙 丙] 都经过同一个乙，若路径退化成后缀切片共享，这两处
			// 会被连带改写；它们必须原样保留各自“从本人到最终代表”的顺序。
			jia := &first.Members[0]
			if jia.ID != "甲" || !equalStrings(jia.Path, []string{"甲", "乙", "丙"}) {
				t.Fatalf("member order/path assumption broken: %+v", jia)
			}
			jia.Path[1] = "被改写"
			jia.Path = append(jia.Path, "多余节点")
			jia.Weight = 1
			jia.Delegate, jia.Direct = "被改写", "被改写"
			if yi := first.Members[1]; !equalStrings(yi.Path, []string{"乙", "丙"}) ||
				yi.Delegate != "丙" || yi.Direct != "丙" || yi.Weight != 20 {
				t.Fatalf("editing 甲's shared hop bled into 乙 within the same result: %+v", yi)
			}
			if ding := first.Members[3]; !equalStrings(ding.Path, []string{"丁", "乙", "丙"}) ||
				ding.Delegate != "丙" || ding.Direct != "乙" || ding.Weight != 80 {
				t.Fatalf("editing 甲's shared hop bled into 丁 within the same result: %+v", ding)
			}
			// 另一份结果中的丁同样不受影响。
			ding := second.Members[3]
			if ding.ID != "丁" || !equalStrings(ding.Path, []string{"丁", "乙", "丙"}) ||
				ding.Delegate != "丙" || ding.Direct != "乙" || ding.Weight != 80 {
				t.Fatalf("editing 甲's path bled into another fetched result: %+v", ding)
			}

			// 再对整份 first 做成员/动作/票据/计票结论的改写与增删。
			corruptOneView(first)

			// 此前取得的 second 保持取得时的保存内容。
			assertCopyChainView(t, "earlier result after edit", second)
			assertCopyChainBallotAndTally(t, "earlier result after edit", second)

			// 重新读取得到的仍是平台实际保存的提案，而不是被编辑后的副本。
			fresh := f.fetch(t, store, id)
			assertCopyChainView(t, "fresh result after edit", fresh)
			assertCopyChainBallotAndTally(t, "fresh result after edit", fresh)
		})
	}
}

// TestVoteQueryEditsDoNotRewriteStoredProposal：对已取得结果中的成员、动作、
// 票据与计票结论做改写或增删后，通过任一查询入口重新读取仍得到平台实际保存
// 的提案：委托关系与原始权重不变，已保存票据与首次计票结论不被覆盖；
// 列表中的另一项提案也不受影响。这种编辑是调用方处理返回值，不是修改提案的
// 业务操作——已委托成员仍不能直接投票，平台没有接受任何新成员、委托或动作。
func TestVoteQueryEditsDoNotRewriteStoredProposal(t *testing.T) {
	store, _ := openTempStore(t, 1000)
	defer store.Close()
	const id = "gip-query-copy-store"
	const otherID = "gip-query-copy-other"
	mustCreateVote(t, store, copyChainInput(id))
	if _, err := store.CastVote(id, "丙", true, 150); err != nil {
		t.Fatalf("丙 vote: %v", err)
	}
	if _, err := store.TallyVote(id, 200); err != nil {
		t.Fatalf("tally: %v", err)
	}
	// 列表中另一项仍在投票、尚无票据的提案，用于证明编辑不会跨提案波及。
	other := copyChainInput(otherID)
	other.Actions = []string{"transfer:other:7"}
	mustCreateVote(t, store, other)

	owned, ok, err := store.VoteProposal(id)
	if err != nil || !ok {
		t.Fatalf("VoteProposal: ok=%v err=%v", ok, err)
	}
	corruptOneView(owned)

	// 三个入口重新读取，主提案仍为保存内容；综合快照与全量列表同时证明
	// 排序（按编号）与另一项提案不受影响。
	for _, f := range voteViewFetchers() {
		fresh := f.fetch(t, store, id)
		assertCopyChainView(t, f.name+" main proposal", fresh)
		assertCopyChainBallotAndTally(t, f.name+" main proposal", fresh)
	}
	all, err := store.VoteProposals()
	if err != nil {
		t.Fatalf("VoteProposals: %v", err)
	}
	if len(all) != 2 || all[0].ID != otherID || all[1].ID != id {
		t.Fatalf("list order/content changed after caller edit: %+v", all)
	}
	got := all[0]
	if got.State != "voting" || got.Tally != nil {
		t.Fatalf("other proposal state/tally changed: %s %+v", got.State, got.Tally)
	}
	if !sameActions(got.Actions, []string{"transfer:other:7"}) {
		t.Fatalf("other proposal actions changed: %v", got.Actions)
	}
	// 另一项提案的委托拓扑、原始权重与成员顺序同样保持保存内容。
	assertMemberViews(t, "other proposal", memberPaths(got), copyChainMemberViews())
	if ids := memberIDs(got.Members); !equalStrings(ids, []string{"甲", "乙", "丙", "丁", "戊"}) {
		t.Fatalf("other proposal member order changed: %v", ids)
	}
	snap, err := store.ProposalsSnapshot()
	if err != nil {
		t.Fatalf("ProposalsSnapshot: %v", err)
	}
	if len(snap.Voting) != 2 || snap.Voting[0].ID != otherID || snap.Voting[1].ID != id {
		t.Fatalf("snapshot list changed after caller edit: %+v", snap.Voting)
	}

	// 编辑不是业务操作：委托关系与归集权重仍是保存的样子——甲已把票委托
	// 给丙，直接投票仍按代表资格拒绝；丙的重复/改投规则与票重也未改变。
	if _, err := store.CastVote(id, "甲", true, 160); !errors.Is(err, ErrVoteRejected) {
		t.Fatalf("delegated member direct vote err=%v, want ErrVoteRejected", err)
	}
	if _, err := store.CastVote(id, "丙", false, 160); !errors.Is(err, ErrProposalConflict) {
		t.Fatalf("changing saved vote err=%v, want ErrProposalConflict", err)
	}
	repeat, err := store.CastVote(id, "丙", true, 160)
	if err != nil {
		t.Fatalf("same-choice retry: %v", err)
	}
	if repeat.Representative != "丙" || repeat.Weight != 150 || !repeat.Support || repeat.VotedAt != 150 {
		t.Fatalf("first ballot changed after caller edit: %+v", repeat)
	}
	// 再次计票返回的仍是首次结论（含首次计票时间 200），没有被编辑覆盖。
	again, err := store.TallyVote(id, 250)
	if err != nil {
		t.Fatalf("repeat tally: %v", err)
	}
	if !again.Passed || again.ForWeight != 150 || again.AgainstWeight != 0 ||
		again.Turnout != 150 || again.Quorum != 100 || again.TalliedAt != 200 {
		t.Fatalf("first tally overwritten by caller edit: %+v", again)
	}
}

// TestVoteQueryOldResultKeepsEmptyBallotsAcrossLifecycle：尚无人投票时取得的
// 查询结果保留空（但非 nil）的票据列表和 nil 计票结论；未委托成员的路径只
// 包含本人，合法空票据列表不被误当作缺损。随后有资格的最终代表在窗口内投票、
// 到期完成计票后，新查询能看到新增票据和确定结论，旧结果仍保持取得时的状态；
// 后续正常投票与计票不会反过来更新旧结果。三个入口各自取得的旧结果逐一验证。
func TestVoteQueryOldResultKeepsEmptyBallotsAcrossLifecycle(t *testing.T) {
	store, _ := openTempStore(t, 1000)
	defer store.Close()
	const id = "gip-query-copy-lifecycle"
	mustCreateVote(t, store, copyChainInput(id))

	olds := make(map[string]*VoteProposalView, len(voteViewFetchers()))
	for _, f := range voteViewFetchers() {
		v := f.fetch(t, store, id)
		if v.State != "voting" || v.Tally != nil {
			t.Fatalf("%s before voting: state=%s tally=%+v, want voting/nil", f.name, v.State, v.Tally)
		}
		// 空票据列表必须是“合法的空”，不是缺损：非 nil、长度为零。
		if v.Ballots == nil || len(v.Ballots) != 0 {
			t.Fatalf("%s before voting: ballots=%v, want non-nil empty list", f.name, v.Ballots)
		}
		assertCopyChainView(t, f.name+" before voting", v)
		// 未委托成员的路径仍只包含本人。
		wu := v.Members[4]
		if wu.ID != "戊" || len(wu.Path) != 1 || wu.Path[0] != "戊" ||
			wu.Delegate != "戊" || wu.Direct != "" {
			t.Fatalf("%s: self-representing member view = %+v", f.name, wu)
		}
		olds[f.name] = v
	}

	// 后续正常业务：合格代表窗口内投票、到期首次计票。既有规则继续适用——
	// 窗口外的首次票与已委托成员直接投票仍被拒绝。
	if _, err := store.CastVote(id, "丙", true, 99); !errors.Is(err, ErrVoteRejected) {
		t.Fatalf("vote before window err=%v, want ErrVoteRejected", err)
	}
	if _, err := store.CastVote(id, "丁", true, 150); !errors.Is(err, ErrVoteRejected) {
		t.Fatalf("delegated 丁 direct vote err=%v, want ErrVoteRejected", err)
	}
	if _, err := store.CastVote(id, "丙", true, 150); err != nil {
		t.Fatalf("丙 vote in window: %v", err)
	}
	res, err := store.TallyVote(id, 200)
	if err != nil {
		t.Fatalf("tally at deadline: %v", err)
	}
	if !res.Passed || res.ForWeight != 150 || res.AgainstWeight != 0 || res.Turnout != 150 {
		t.Fatalf("tally = %+v, want passed for=150 against=0 turnout=150", res)
	}

	// 新查询反映新票据与确定结论；旧结果保持取得时（无人投票、未计票）的状态。
	for _, f := range voteViewFetchers() {
		fresh := f.fetch(t, store, id)
		assertCopyChainView(t, f.name+" after tally", fresh)
		assertCopyChainBallotAndTally(t, f.name+" after tally", fresh)

		old := olds[f.name]
		if old.State != "voting" || old.Tally != nil {
			t.Fatalf("%s: old result retroactively updated: state=%s tally=%+v",
				f.name, old.State, old.Tally)
		}
		if old.Ballots == nil || len(old.Ballots) != 0 {
			t.Fatalf("%s: old empty ballots retroactively changed: %+v", f.name, old.Ballots)
		}
		assertCopyChainView(t, f.name+" old result after lifecycle", old)
	}

	// 计票后新票被拒绝，但旧结果同样不被这一业务动作反向更新。
	if _, err := store.CastVote(id, "戊", true, 200); !errors.Is(err, ErrVoteRejected) {
		t.Fatalf("post-tally vote err=%v, want ErrVoteRejected", err)
	}
	for name, old := range olds {
		if old.State != "voting" || old.Tally != nil || len(old.Ballots) != 0 {
			t.Fatalf("%s: old result changed after rejected post-tally vote: %+v", name, old)
		}
	}
}
