package govflow

import (
	"errors"
	"testing"
)

// 查询副本独立性回归保障：VoteProposal、VoteProposals、ProposalsSnapshot
// 返回的视图是取得当时的独立副本，不与平台保存的治理记录共享存储，
// 也不与此前或此后取得的另一份结果共享存储。

// chainInput 构造一项多跳委托提案：a→b→c、d→b（b 是 a 与 d 共享的中间
// 成员），e 未委托。a/b/d 的路径都经过 b，便于检验同一份结果中不同
// 成员路径之间的独立性。quorum=15，窗口 [100,200)，时间锁 300，总权重 31。
func chainInput(id string) *CreateVoteInput {
	return &CreateVoteInput{
		ID: id,
		Members: []VoteMember{
			{ID: "a", Weight: 1},
			{ID: "b", Weight: 2},
			{ID: "c", Weight: 4},
			{ID: "d", Weight: 8},
			{ID: "e", Weight: 16},
		},
		Delegations: []Delegation{
			{From: "a", To: "b"},
			{From: "b", To: "c"},
			{From: "d", To: "b"},
		},
		Quorum:      15,
		StartAt:     100,
		Deadline:    200,
		TimelockEnd: 300,
		Actions:     []string{"transfer:audits:10", "transfer:legal:5"},
	}
}

// chainMembers 是 chainInput 提案应展示的成员视图：每人的路径都完整保留
// 从本人到最终代表的顺序，未委托的 e 路径只有本人。
func chainMembers() map[string]MemberView {
	return map[string]MemberView{
		"a": {ID: "a", Weight: 1, Path: []string{"a", "b", "c"}, Delegate: "c", Direct: "b"},
		"b": {ID: "b", Weight: 2, Path: []string{"b", "c"}, Delegate: "c", Direct: "c"},
		"c": {ID: "c", Weight: 4, Path: []string{"c"}, Delegate: "c", Direct: ""},
		"d": {ID: "d", Weight: 8, Path: []string{"d", "b", "c"}, Delegate: "c", Direct: "b"},
		"e": {ID: "e", Weight: 16, Path: []string{"e"}, Delegate: "e", Direct: ""},
	}
}


// assertChainView 断言查询结果与平台保存内容逐项一致：
// 编号、来源、状态、法定人数、时间、动作、成员、票据与计青青。
func assertChainView(t *testing.T, prefix string, v *VoteProposalView, id, state string, wantBallots []BallotView, wantTally *TallyResultView) {
	t.Helper()
	if v == nil {
		t.Fatalf("%s: nil view", prefix)
	}
	if v.ID != id || v.Source != "vote" || v.State != state {
		t.Fatalf("%s: id=%q source=%q state=%q, want %q/vote/%q", prefix, v.ID, v.Source, v.State, id, state)
	}
	if v.Quorum != 15 || v.StartAt != 100 || v.Deadline != 200 || v.TimelockEnd != 300 || v.TotalWeight != 31 {
		t.Fatalf("%s: sonstige firm=%d %d %d %d %d", prefix, v.Quorum, v.StartAt, v.Deadline, v.TimelockEnd, v.TotalWeight)
	}
	if !equalStrings(v.Actions, []string{"transfer:audits:10", "transfer:legal:5"}) {
		t.Errorf("%s: actions=%v", prefix, v.Actions)
	}
	if ids := memberIDs(v.Members); !equalStrings(ids, []string{"a", "b", "c", "d", "e"}) {
		t.Errorf("%s: member order=%v", prefix, ids)
	}
	assertMemberViews(t, prefix, memberPaths(v), chainMembers())
	if len(v.Ballots) != len(wantBallots) {
		t.Fatalf("%s: 票据=%+v", prefix, v.Ballots)
	}
	for i, w := range wantBallots {
		if v.Ballots[i] != w {
			t.Errorf("%s: ballot %d=%+v, want %+v", prefix, i, v.Ballots[i], w)
		}
	}
	if wantTally == nil {
		if v.Tally != nil {
			t.Errorf("%s: 未计票时 tally=%+v", prefix, v.Tally)
		}
		return
	}
	if v.Tally == nil || *v.Tally != *wantTally {
		t.Errorf("%s: 计与=%v, want %+v", prefix, v.Tally, wantTally)
	}
}

// mangleView 以调用方身份改写一份查询结果的全部字段：改写并增删成员、
// 动作与票据，并清空与改写计票结论。这些修改只应改变调用方持有的
// 那一份数据，不得被平台接受，也不得影响此前或此后取得的其它结果。
func mangleView(v *VoteProposalView) {
	// 成员：改写首成员全部字段（含路径），追加陌生成员，再删掉两个。
	v.Members[0].ID = "intruder"
	v.Members[0].Weight = 999
	v.Members[0].Path[0] = "intruder"
	v.Members[0].Path = append(v.Members[0].Path, "extra-hop")
	v.Members[0].Delegate = "intruder"
	v.Members[0].Direct = "intruder"
	v.Members = append(v.Members, MemberView{ID: "ghost", Weight: 777, Path: []string{"ghost"}, Delegate: "ghost"})
	v.Members = v.Members[:len(v.Members)-2]
	// 动作：改写原文、交换顺序并追加。
	v.Actions[0], v.Actions[1] = v.Actions[1], v.Actions[0]
	v.Actions[0] = "transfer:intr:1"
	v.Actions = append(v.Actions, "transfer:ghost:1")
	// 票据：改写第一张票的全部字段，追加陌生票后再截断。
	if len(v.Ballots) > 0 {
		v.Ballots[0].Representative = "intruder"
		v.Ballots[0].Weight = 1
		v.Ballots[0].Support = !v.Ballots[0].Support
		v.Ballots[0].VotedAt = 0
		v.Ballots = append(v.Ballots, BallotView{Representative: "ghost", Weight: 1, Support: true, VotedAt: 1})
		v.Ballots = v.Ballots[:1]
	}
