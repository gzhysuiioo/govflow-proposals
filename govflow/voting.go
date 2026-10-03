package govflow

import (
	"errors"
	"fmt"
	"sort"
)

// 投票提案相关错误。
var (
	// ErrInvalidProposal：创建投票提案的参数非法（编号/成员/权重/时间/委托等）。
	ErrInvalidProposal = errors.New("invalid voting proposal")
	// ErrProposalNotFound 见 store.go。
	// ErrProposalConflict：同编号提案已存在但内容或来源不同，或重复投票选择不同。
	// ErrVoteRejected：投票被拒绝（时间窗口外、投票人无资格等）。
	ErrVoteRejected = errors.New("vote rejected")
	// ErrTallyRejected：计票被拒绝（截止时刻未到）。
	ErrTallyRejected = errors.New("tally rejected")
)

// ---- 输入与对外视图 ----

// VoteMember 是创建投票提案时提交的一名成员及其原始权重。
type VoteMember struct {
	ID     string `json:"id"`
	Weight int64  `json:"weight"`
}

// Delegation 表示 From 把投票权委托给 To。委托可以继续转交，
// 最终未再委托的人代表沿途所有成员投票。
type Delegation struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// CreateVoteInput 是创建一项投票提案的完整输入。
type CreateVoteInput struct {
	ID          string
	Members     []VoteMember
	Delegations []Delegation
	Quorum      int64
	StartAt     int64
	Deadline    int64
	TimelockEnd int64
	Actions     []string
}

// MemberView 是查询时展示的一名成员：委托路径、最终代表与原始权重。
type MemberView struct {
	ID       string   `json:"id"`
	Weight   int64    `json:"weight"`
	Path     []string `json:"path"`      // 从本人到最终代表的完整编号链（含首尾）
	Delegate string   `json:"delegate"`  // 最终代表
	Direct   string   `json:"direct_to"` // 直接委托对象；未委托为空串
}

// BallotView 是查询时展示的一张已投出的票。
type BallotView struct {
	Representative string `json:"representative"`
	Weight         int64  `json:"weight"`
	Support        bool   `json:"support"`
	VotedAt        int64  `json:"voted_at"`
}

// TallyResultView 是首次计票结论的查询视图；尚未计票时为 nil。
type TallyResultView struct {
	ForWeight     int64 `json:"for_weight"`
	AgainstWeight int64 `json:"against_weight"`
	Turnout       int64 `json:"turnout"`
	Quorum        int64 `json:"quorum"`
	Passed        bool  `json:"passed"`
	TalliedAt     int64 `json:"tallied_at"`
}

// VoteProposalView 是一项投票提案的完整对外视图。
type VoteProposalView struct {
	ID          string           `json:"id"`
	Source      string           `json:"source"` // vote
	State       string           `json:"state"`  // voting / passed / rejected / executed
	Quorum      int64            `json:"quorum"`
	StartAt     int64            `json:"start_at"`
	Deadline    int64            `json:"deadline"`
	TimelockEnd int64            `json:"timelock_end"`
	Actions     []string         `json:"actions"`
	TotalWeight int64            `json:"total_weight"`
	Members     []MemberView     `json:"members"`
	Ballots     []BallotView     `json:"ballots"`
	Tally       *TallyResultView `json:"tally,omitempty"`
}

// ---- 磁盘结构（JSON，版本仍为 1） ----

type storedMember struct {
	ID     string `json:"id"`
	Weight int64  `json:"weight"`
}

type storedDelegation struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type storedBallot struct {
	Representative string `json:"representative"`
	Weight         int64  `json:"weight"`
	Support        bool   `json:"support"`
	VotedAt        int64  `json:"voted_at"`
}

type storedTally struct {
	ForWeight     int64 `json:"for_weight"`
	AgainstWeight int64 `json:"against_weight"`
	TalliedAt     int64 `json:"tallied_at"`
}

// storedVoteProposal 按成员提交顺序保存成员与委托（顺序不影响相等性），
// 动作文本与顺序原样保存（顺序影响相等性）。
type storedVoteProposal struct {
	ID          string             `json:"id"`
	State       string             `json:"state"`
	Members     []storedMember     `json:"members"`
	Delegations []storedDelegation `json:"delegations"`
	Quorum      int64              `json:"quorum"`
	StartAt     int64              `json:"start_at"`
	Deadline    int64              `json:"deadline"`
	TimelockEnd int64              `json:"timelock_end"`
	Actions     []string           `json:"actions"`
	Ballots     []storedBallot     `json:"ballots"`
	Tally       *storedTally       `json:"tally,omitempty"`
}

// ---- 校验与派生 ----

type proposalSpec struct {
	members  []VoteMember
	weights  map[string]int64
	delegate map[string]string // 直接委托对象
	pathOf   map[string][]string
	headOf   map[string]string // 最终代表
	groupW   map[string]int64  // 最终代表名下归集的全部原始权重
	total    int64
}

// validateProposalInput 校验全部创建规则并派生委托路径与代表权重。
// 任何非法条件都返回错误，调用方必须整项拒绝。
func validateProposalInput(in *CreateVoteInput) (*proposalSpec, error) {
	if in.ID == "" {
		return nil, invalidProposal("proposal id must not be empty")
	}
	if len(in.Members) == 0 {
		return nil, invalidProposal("proposal %s: member list must not be empty", in.ID)
	}
	weights := make(map[string]int64, len(in.Members))
	order := make([]string, 0, len(in.Members))
	for _, m := range in.Members {
		if m.ID == "" {
			return nil, invalidProposal("proposal %s: member id must not be empty", in.ID)
		}
		if _, dup := weights[m.ID]; dup {
			return nil, invalidProposal("proposal %s: duplicate member %q", in.ID, m.ID)
		}
		if m.Weight <= 0 {
			return nil, invalidProposal("proposal %s: member %q weight must be a positive integer, got %d", in.ID, m.ID, m.Weight)
		}
		weights[m.ID] = m.Weight
		order = append(order, m.ID)
	}

	// 总权重必须在 int64 范围内（各权重已为正）。
	var total int64
	for _, m := range in.Members {
		sum, ok := addInt64(total, m.Weight)
		if !ok {
			return nil, invalidProposal("proposal %s: total member weight overflows int64", in.ID)
		}
		total = sum
	}

	if in.Quorum < 1 || in.Quorum > total {
		return nil, invalidProposal("proposal %s: quorum must be between 1 and total weight %d, got %d", in.ID, total, in.Quorum)
	}
	if !(0 <= in.StartAt && in.StartAt < in.Deadline && in.Deadline <= in.TimelockEnd) {
		return nil, invalidProposal("proposal %s: times must satisfy 0 <= start < deadline <= timelock, got start=%d deadline=%d timelock=%d",
			in.ID, in.StartAt, in.Deadline, in.TimelockEnd)
	}

	delegate := make(map[string]string, len(in.Delegations))
	for _, d := range in.Delegations {
		if d.From == "" || d.To == "" {
			return nil, invalidProposal("proposal %s: delegation members must not be empty", in.ID)
		}
		if _, ok := weights[d.From]; !ok {
			return nil, invalidProposal("proposal %s: delegation from unknown member %q", in.ID, d.From)
		}
		if _, ok := weights[d.To]; !ok {
			return nil, invalidProposal("proposal %s: delegation to unknown member %q", in.ID, d.To)
		}
		if d.From == d.To {
			return nil, invalidProposal("proposal %s: member %q must not delegate to itself", in.ID, d.From)
		}
		if _, dup := delegate[d.From]; dup {
			return nil, invalidProposal("proposal %s: member %q delegates more than once", in.ID, d.From)
		}
		delegate[d.From] = d.To
	}

	// 解析每名成员的委托路径：从本人出发，沿直接委托对象逐次前行，
	// 直到未再委托的最终代表。每个成员出度至多 1，且自委托/循环已被拦截，
	// 所以该遍历必然终止。
	// 命中已解析成员时必须拼接其“完整路径”（去掉与之重复的衔接点），
	// 不能直接跳到它的最终代表，否则多跳链上的中间委托对象会丢失。
	// 结果只依赖委托关系本身，与成员及委托参数的提交顺序无关。
	pathOf := make(map[string][]string, len(weights))
	headOf := make(map[string]string, len(weights))
	for _, id := range order {
		if _, done := headOf[id]; done {
			continue
		}
		chain := []string{id}
		onChain := map[string]int{id: 0}
		cur := id
		for {
			next, has := delegate[cur]
			if !has {
				break // cur 即未再委托的最终代表
			}
			if pos, cyc := onChain[next]; cyc {
				return nil, invalidProposal("proposal %s: delegation cycle involving %q", in.ID, chain[pos])
			}
			if resolved, ok := pathOf[next]; ok {
				// 拼接已解析的完整后缀；resolved[0] == next，逐跳衔接且无重复成员。
				chain = append(chain, resolved...)
				cur = resolved[len(resolved)-1]
				break
			}
			chain = append(chain, next)
			onChain[next] = len(chain) - 1
			cur = next
		}
		head := cur
		// 链上每个成员的路径都是自身到 head 的后缀。命中已解析后缀时，
		// 后缀成员均已登记，这里只补齐尚未解析的成员。
		for i := 0; i < len(chain); i++ {
			member := chain[i]
			if _, ok := headOf[member]; !ok {
				pathOf[member] = append([]string(nil), chain[i:]...)
				headOf[member] = head
			}
		}
	}

	// 归集每个最终代表名下的全部原始权重；子集和必然不超过总权重。
	groupW := make(map[string]int64)
	for _, id := range order {
		head := headOf[id]
		sum, ok := addInt64(groupW[head], weights[id])
		if !ok {
			return nil, invalidProposal("proposal %s: delegated weight for %q overflows int64", in.ID, head)
		}
		groupW[head] = sum
	}

	return &proposalSpec{
		members:  append([]VoteMember(nil), in.Members...),
		weights:  weights,
		delegate: delegate,
		pathOf:   pathOf,
		headOf:   headOf,
		groupW:   groupW,
		total:    total,
	}, nil
}

func invalidProposal(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidProposal, fmt.Sprintf(format, args...))
}

// proposalsEqual 判断重试内容是否与已存提案逐项相同。
// 成员与委托的输入次序不影响比较；动作文本与顺序必须一致。
func proposalsEqual(stored *storedVoteProposal, in *CreateVoteInput, spec *proposalSpec) bool {
	if stored.Quorum != in.Quorum || stored.StartAt != in.StartAt ||
		stored.Deadline != in.Deadline || stored.TimelockEnd != in.TimelockEnd {
		return false
	}
	if !sameActions(stored.Actions, in.Actions) {
		return false
	}
	if len(stored.Members) != len(in.Members) {
		return false
	}
	for _, m := range stored.Members {
		if w, ok := spec.weights[m.ID]; !ok || w != m.Weight {
			return false
		}
	}
	if len(stored.Delegations) != len(in.Delegations) {
		return false
	}
	for _, d := range stored.Delegations {
		to, ok := spec.delegate[d.From]
		if !ok || to != d.To {
			return false
		}
	}
	return true
}

// ---- 存储操作 ----

// CreateVoteProposal 创建一项投票提案。
// 同编号且全部内容相同的重试返回已存在提案（existed=true）且不改变任何状态；
// 同编号内容不同（或编号已被 register 来源占用）返回 ErrProposalConflict。
// 非法条件整项拒绝，不落盘任何部分变化。
func (s *Store) CreateVoteProposal(in *CreateVoteInput) (view *VoteProposalView, existed bool, err error) {
	spec, err := validateProposalInput(in)
	if err != nil {
		return nil, false, err
	}
	commit, err := s.begin()
	if err != nil {
		return nil, false, err
	}
	defer commit()

	state, err := s.loadLocked()
	if err != nil {
		return nil, false, err
	}
	if existing, ok := state.VoteProposals[in.ID]; ok {
		if !proposalsEqual(existing, in, spec) {
			return nil, false, fmt.Errorf("%w: voting proposal %s", ErrProposalConflict, in.ID)
		}
		v := voteProposalView(existing)
		return v, true, nil
	}
	if _, ok := state.Proposals[in.ID]; ok {
		// register 不能覆盖投票提案；反向同样拒绝，两种来源共用编号。
		return nil, false, fmt.Errorf("%w: proposal id %s already registered", ErrProposalConflict, in.ID)
	}

	stored := &storedVoteProposal{
		ID:          in.ID,
		State:       "voting",
		Members:     make([]storedMember, 0, len(in.Members)),
		Delegations: make([]storedDelegation, 0, len(in.Delegations)),
		Quorum:      in.Quorum,
		StartAt:     in.StartAt,
		Deadline:    in.Deadline,
		TimelockEnd: in.TimelockEnd,
		Actions:     copyActions(in.Actions),
		Ballots:     []storedBallot{},
	}
	for _, m := range in.Members {
		stored.Members = append(stored.Members, storedMember{ID: m.ID, Weight: m.Weight})
	}
	for _, d := range in.Delegations {
		stored.Delegations = append(stored.Delegations, storedDelegation{From: d.From, To: d.To})
	}
	state.VoteProposals[in.ID] = stored
	if err := s.commitLocked(state); err != nil {
		return nil, false, err
	}
	return voteProposalView(stored), false, nil
}

func voteReject(reason string) error {
	return fmt.Errorf("%w: %s", ErrVoteRejected, reason)
}

func tallyReject(reason string) error {
	return fmt.Errorf("%w: %s", ErrTallyRejected, reason)
}

// CastVote 在一项投票提案上投票。调用方通过 now 提供当前时间。
// 只有最终代表本人可投赞成/反对票，每位代表一张票，票重为归集到其名下的
// 全部原始权重。相同选择的重试始终返回首次记录；改投报冲突。
// 首次投票只接受 start <= now < deadline；计票之后拒绝一切新票。
func (s *Store) CastVote(id, voter string, support bool, now int64) (*BallotView, error) {
	if voter == "" {
		return nil, voteReject("voter id must not be empty")
	}
	commit, err := s.begin()
	if err != nil {
		return nil, err
	}
	defer commit()

	state, err := s.loadLocked()
	if err != nil {
		return nil, err
	}
	p, ok := state.VoteProposals[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrProposalNotFound, id)
	}
	spec, verr := specOf(p)
	if verr != nil {
		return nil, verr // 正常路径不会发生：打开与提交前均已严格校验
	}
	if _, member := spec.weights[voter]; !member {
		return nil, voteReject(fmt.Sprintf("voter %q is not on the member roster of %s", voter, id))
	}
	if spec.headOf[voter] != voter {
		return nil, voteReject(fmt.Sprintf("voter %q has delegated the vote to %q and may not vote directly", voter, spec.headOf[voter]))
	}
	// 已投过票：相同选择的重试始终返回首次记录（与时间窗口、是否已计票无关）；
	// 改投另一选择报冲突，绝不静默覆盖。
	for i := range p.Ballots {
		if p.Ballots[i].Representative == voter {
			if p.Ballots[i].Support != support {
				return nil, fmt.Errorf("%w: representative %q already voted %s for %s; changing the vote is not allowed",
					ErrProposalConflict, voter, voteChoice(p.Ballots[i].Support), id)
			}
			b := p.Ballots[i]
			return &BallotView{Representative: b.Representative, Weight: b.Weight, Support: b.Support, VotedAt: b.VotedAt}, nil
		}
	}
	if p.Tally != nil {
		return nil, voteReject(fmt.Sprintf("proposal %s has already been tallied; voting is closed", id))
	}
	if now < p.StartAt || now >= p.Deadline {
		return nil, voteReject(fmt.Sprintf("proposal %s is not open for voting at now=%d (start=%d deadline=%d)", id, now, p.StartAt, p.Deadline))
	}

	p.Ballots = append(p.Ballots, storedBallot{
		Representative: voter,
		Weight:         spec.groupW[voter],
		Support:        support,
		VotedAt:        now,
	})
	if err := s.commitLocked(state); err != nil {
		return nil, err
	}
	return &BallotView{Representative: voter, Weight: spec.groupW[voter], Support: support, VotedAt: now}, nil
}

func voteChoice(support bool) string {
	if support {
		return "for"
	}
	return "against"
}

// ---- 计票规则（唯一实现）----
//
// 首次计票、打开状态文件时的记录校验、以及查询时展示的结论，都必须经过同一份
// 票据重放与同一套通过条件，避免在多处分别维护汇总方式与判定规则而产生分歧。

// ballotTally 是逐票明细按名单与委托关系重放后的汇总：赞成、反对两侧权重，
// 以及二者之和的参与量。未投成员的权重不计入任何一侧，因此参与量只来自
// 实际投出的票，绝不等于成员总权重。
type ballotTally struct {
	forWeight     int64
	againstWeight int64
	turnout       int64
}

// tallyBallots 依据成员名单与委托关系（spec）重放一项提案的全部票据，
// 逐张确认代表资格、票重、（可选）投票时间窗口，并做去重与权重累加。
// 这是票重汇总的唯一实现：首次计票与打开校验共用，二者不再各自维护一份。
//
// checkWindow 为 true 时额外要求每张票落在 [start, deadline) 内；首次计票时
// 票据均在投票阶段落盘、调用方持锁，无需再查时间，传 false 即可。
// 任何与名单/委托/明细不符的情况都返回原因可区分的普通错误（不自带错误分类），
// 由边界统一归类为状态损坏；调用方不得用重算出的结果修正后继续使用。
func tallyBallots(p *storedVoteProposal, spec *proposalSpec, checkWindow bool) (ballotTally, error) {
	var t ballotTally
	seenReps := map[string]bool{}
	for i, b := range p.Ballots {
		if b.Representative == "" {
			return t, fmt.Errorf("proposal %s ballot %d has empty representative", p.ID, i)
		}
		if spec.headOf[b.Representative] != b.Representative {
			return t, fmt.Errorf("proposal %s ballot %d: %q is not a final representative", p.ID, i, b.Representative)
		}
		if seenReps[b.Representative] {
			return t, fmt.Errorf("proposal %s has duplicate stored ballot for %q", p.ID, b.Representative)
		}
		seenReps[b.Representative] = true
		if b.Weight != spec.groupW[b.Representative] {
			return t, fmt.Errorf("proposal %s ballot weight for %q (%d) does not match roster delegation (%d)",
				p.ID, b.Representative, b.Weight, spec.groupW[b.Representative])
		}
		if checkWindow && (b.VotedAt < p.StartAt || b.VotedAt >= p.Deadline) {
			return t, fmt.Errorf("proposal %s ballot by %q cast at %d outside window [%d,%d)",
				p.ID, b.Representative, b.VotedAt, p.StartAt, p.Deadline)
		}
		// 票重为委托归集的正权重且每张代表票只计一次；子集和不超过已校验的总权重，
		// 这里仍以 addInt64 兜底，任何溢出都按状态损坏处理而非静默回绕。
		if b.Support {
			sum, ok := addInt64(t.forWeight, b.Weight)
			if !ok {
				return t, fmt.Errorf("proposal %s vote weights overflow int64", p.ID)
			}
			t.forWeight = sum
		} else {
			sum, ok := addInt64(t.againstWeight, b.Weight)
			if !ok {
				return t, fmt.Errorf("proposal %s vote weights overflow int64", p.ID)
			}
			t.againstWeight = sum
		}
	}
	// 参与量是赞成与反对两侧之和；两侧各自合法时其和仍可能触及 int64 上限，
	// 不能直接相加重算（合法总权重恰好为 MaxInt64 时结论不得因此改变）。
	sum, ok := addInt64(t.forWeight, t.againstWeight)
	if !ok {
		return t, fmt.Errorf("proposal %s turnout weight overflows int64", p.ID)
	}
	t.turnout = sum
	return t, nil
}

// decideTally 是通过条件的唯一判定：参与量达到法定人数（恰好达到也算），
// 且赞成严格多于反对。平票、无人投票或参与不足一律不通过。
// turnout 由 tallyBallots 以 addInt64 安全汇总（合法提案中 <= 成员总权重 <= MaxInt64），
// 这里只做比较、不做可能上溢的加法，因此参与量恰好为 int64 上限时结论仍正确。
func decideTally(t ballotTally, quorum int64) bool {
	return t.turnout >= quorum && t.forWeight > t.againstWeight
}

// TallyVote 在截止时刻或之后进行首次计票：按最终代表实际投出的票汇总
// 赞成与反对权重（委托到其名下的成员权重计入代表票重，未投成员不计入参与量），
// 参与量达到法定人数且赞成严格多于反对才通过。
// 计票只保存结论与首次计票时间、把提案置为通过或拒绝，不改票据明细、不产生资金变动；
// 已有结论时再次调用（即使传入截止前的时间）始终返回首次结论。
func (s *Store) TallyVote(id string, now int64) (*TallyResultView, error) {
	commit, err := s.begin()
	if err != nil {
		return nil, err
	}
	defer commit()

	state, err := s.loadLocked()
	if err != nil {
		return nil, err
	}
	p, ok := state.VoteProposals[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrProposalNotFound, id)
	}
	if p.Tally != nil {
		return storedTallyResult(p), nil
	}
	if now < p.Deadline {
		return nil, tallyReject(fmt.Sprintf("voting for %s is still open: now=%d deadline=%d", id, now, p.Deadline))
	}

	spec, verr := specOf(p)
	if verr != nil {
		return nil, verr
	}
	// 汇总与判定都走唯一实现，与打开校验、查询共用同一份规则。
	t, terr := tallyBallots(p, spec, false)
	if terr != nil {
		return nil, fmt.Errorf("%w: %v", ErrStateCorrupt, terr)
	}
	passed := decideTally(t, p.Quorum)
	// 首次计票只落结论与首次计票时间，并迁移提案状态；
	// 不改动票据明细，也不产生任何资金变动。
	p.Tally = &storedTally{ForWeight: t.forWeight, AgainstWeight: t.againstWeight, TalliedAt: now}
	if passed {
		p.State = "passed"
	} else {
		p.State = "rejected"
	}
	if err := s.commitLocked(state); err != nil {
		return nil, err
	}
	return storedTallyResult(p), nil
}

// storedTallyResult 从已存储提案投影首次计票结论；尚未计票时返回 nil。
// 参与量与是否通过都由唯一的计票规则从已保存权重算出，与首次计票时完全一致；
// 提案后续执行（executed）只改变生命周期状态，不改变这里投影出的首次结论。
func storedTallyResult(p *storedVoteProposal) *TallyResultView {
	if p.Tally == nil {
		return nil
	}
	t := ballotTally{forWeight: p.Tally.ForWeight, againstWeight: p.Tally.AgainstWeight}
	// 已保存状态均通过打开/提交校验：两侧之和不超过合法总权重（<= MaxInt64），
	// 这里不会溢出；仍以 addInt64 表达参与量，避免裸加。
	t.turnout, _ = addInt64(t.forWeight, t.againstWeight)
	return &TallyResultView{
		ForWeight:     t.forWeight,
		AgainstWeight: t.againstWeight,
		Turnout:       t.turnout,
		Quorum:        p.Quorum,
		Passed:        decideTally(t, p.Quorum),
		TalliedAt:     p.Tally.TalliedAt,
	}
}

// VoteProposal 按编号查询一项投票提案；不存在时 ok 为 false。
func (s *Store) VoteProposal(id string) (*VoteProposalView, bool, error) {
	state, err := s.readState()
	if err != nil {
		return nil, false, err
	}
	p, ok := state.VoteProposals[id]
	if !ok {
		return nil, false, nil
	}
	return voteProposalView(p), true, nil
}

// VoteProposals 返回全部投票提案，按编号排序。
func (s *Store) VoteProposals() ([]*VoteProposalView, error) {
	state, err := s.readState()
	if err != nil {
		return nil, err
	}
	return voteProposalViews(state), nil
}

func voteProposalViews(state *storedState) []*VoteProposalView {
	out := make([]*VoteProposalView, 0, len(state.VoteProposals))
	for _, p := range state.VoteProposals {
		out = append(out, voteProposalView(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func voteProposalView(p *storedVoteProposal) *VoteProposalView {
	spec, err := specOf(p)
	if err != nil {
		// 调用方持有的状态均经过 validateState；损坏文件根本无法打开。
		panic("govflow: building view of corrupt proposal: " + err.Error())
	}
	members := make([]MemberView, 0, len(p.Members))
	for _, m := range p.Members { // 保持成员提交顺序
		mv := MemberView{
			ID:       m.ID,
			Weight:   m.Weight,
			Path:     append([]string(nil), spec.pathOf[m.ID]...),
			Delegate: spec.headOf[m.ID],
		}
		if to, ok := spec.delegate[m.ID]; ok {
			mv.Direct = to
		}
		members = append(members, mv)
	}
	ballots := make([]BallotView, 0, len(p.Ballots))
	for _, b := range p.Ballots { // 保持首次投票先后顺序
		ballots = append(ballots, BallotView{
			Representative: b.Representative,
			Weight:         b.Weight,
			Support:        b.Support,
			VotedAt:        b.VotedAt,
		})
	}
	v := &VoteProposalView{
		ID:          p.ID,
		Source:      "vote",
		State:       p.State,
		Quorum:      p.Quorum,
		StartAt:     p.StartAt,
		Deadline:    p.Deadline,
		TimelockEnd: p.TimelockEnd,
		Actions:     copyActions(p.Actions),
		TotalWeight: spec.total,
		Members:     members,
		Ballots:     ballots,
	}
	if p.Tally != nil {
		v.Tally = storedTallyResult(p)
	}
	return v
}

// specOf 从已存储提案重建校验派生物（委托路径、代表归集权重）。
func specOf(p *storedVoteProposal) (*proposalSpec, error) {
	in := &CreateVoteInput{
		ID:          p.ID,
		Quorum:      p.Quorum,
		StartAt:     p.StartAt,
		Deadline:    p.Deadline,
		TimelockEnd: p.TimelockEnd,
		Actions:     p.Actions,
	}
	in.Members = make([]VoteMember, len(p.Members))
	for i, m := range p.Members {
		in.Members[i] = VoteMember{ID: m.ID, Weight: m.Weight}
	}
	in.Delegations = make([]Delegation, len(p.Delegations))
	for i, d := range p.Delegations {
		in.Delegations[i] = Delegation{From: d.From, To: d.To}
	}
	return validateProposalInput(in)
}

// validateVoteProposals 在打开状态文件时严格校验全部投票提案：
// 编号与 register 来源不冲突；状态机与计票结论一致；票重/代表与名单及委托明细一致；
// 首次投票时间落在窗口内；首次计票不早于截止且结论可由明细重放。
func validateVoteProposals(state *storedState) error {
	for key, p := range state.VoteProposals {
		if p == nil {
			return fmt.Errorf("voting proposal %q is empty", key)
		}
		if p.ID != key || p.ID == "" {
			return fmt.Errorf("voting proposal key %q does not match id %q", key, p.ID)
		}
		if _, collide := state.Proposals[p.ID]; collide {
			return fmt.Errorf("proposal id %q exists in both proposal sources", p.ID)
		}
		switch p.State {
		case "voting", "passed", "rejected", "executed":
		default:
			return fmt.Errorf("voting proposal %q has unknown state %q", p.ID, p.State)
		}
		spec, err := specOf(p)
		if err != nil {
			return err
		}

		// 票据重放走与首次计票完全相同的唯一实现：代表资格、票重是否与名单/
		// 委托归集一致、投票时间窗口、去重与两侧权重汇总都在这里核对。
		// 任何不符都作为状态损坏拒绝读取，不用重算结果修正后继续。
		replayed, terr := tallyBallots(p, spec, true)
		if terr != nil {
			return terr
		}

		switch {
		case p.Tally == nil:
			if p.State != "voting" {
				return fmt.Errorf("voting proposal %q is %s but has no tally", p.ID, p.State)
			}
		default:
			if p.State == "voting" {
				return fmt.Errorf("voting proposal %q has a tally but is still in voting state", p.ID)
			}
			if p.Tally.TalliedAt < p.Deadline {
				return fmt.Errorf("voting proposal %q tallied at %d before deadline %d", p.ID, p.Tally.TalliedAt, p.Deadline)
			}
			if p.Tally.ForWeight != replayed.forWeight || p.Tally.AgainstWeight != replayed.againstWeight {
				return fmt.Errorf("voting proposal %q tally weights for=%d/against=%d do not replay ballots for=%d/against=%d",
					p.ID, p.Tally.ForWeight, p.Tally.AgainstWeight, replayed.forWeight, replayed.againstWeight)
			}
			// 通过/拒绝结论由同一套规则重放得出，并与保存的状态一致；
			// executed 是 passed 提案执行后的合法后续状态，不回退首次结论。
			if decideTally(replayed, p.Quorum) != (p.State == "passed" || p.State == "executed") {
				return fmt.Errorf("voting proposal %q state %s does not match quorum/majority replay", p.ID, p.State)
			}
		}
	}
	return nil
}
