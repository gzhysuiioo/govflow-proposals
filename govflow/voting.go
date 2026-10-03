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

// ---- 计票规则（唯一实现，首次计票、打开校验、查询结论共用） ----

// ballotCounts 是按逐票明细汇总出的两侧权重。参与量只统计最终代表
// 实际投出的票，未投成员的权重既不在 For 也不在 Against 中，
// 因而不会被计入 Turnout，更不能直接拿成员总权重充当参与量。
type ballotCounts struct {
	forWeight     int64
	againstWeight int64
}

// turnout 返回参与权重（赞成与反对之和），ok 表示合计未溢出 int64。
// 两侧各自不超过合法成员总权重时合计必不溢出；此处仍显式检查，
// 使“总权重恰好为 math.MaxInt64”的合法提案也不依赖该前提。
func (c ballotCounts) turnout() (int64, bool) {
	return addInt64(c.forWeight, c.againstWeight)
}

// votePasses 是唯一的通过条件：参与权重达到法定人数（恰好达到也算）
// 且赞成严格多于反对。平票、无人投票或参与不足一律不通过。
// 通过比较法定人数剩余容量判断参与量，避免 for+against 上溢时回绕为负，
// 把已达标（乃至总权重恰好为 int64 上限）的提案误判为参与不足。
func votePasses(forWeight, againstWeight, quorum int64) bool {
	if forWeight <= againstWeight {
		return false // 平票或赞成更少都不通过；两侧非负，无票时同样落在这里
	}
	return forWeight >= quorum-againstWeight // 等价于 for+against >= quorum，且不做上溢加法
}

// tallyVerdict 把通过条件映射为提案首次计票后的状态。
func tallyVerdict(passed bool) string {
	if passed {
		return "passed"
	}
	return "rejected"
}

// countBallots 只按票据选择累加两侧权重，不做代表资格与票重核对
// （资格核对由 verifyBallots 负责）。任何一侧溢出 int64 都返回损坏错误。
func countBallots(ballots []storedBallot) (ballotCounts, error) {
	var c ballotCounts
	for _, b := range ballots {
		if b.Support {
			sum, ok := addInt64(c.forWeight, b.Weight)
			if !ok {
				return ballotCounts{}, errors.New("for-side ballot weights overflow int64")
			}
			c.forWeight = sum
		} else {
			sum, ok := addInt64(c.againstWeight, b.Weight)
			if !ok {
				return ballotCounts{}, errors.New("against-side ballot weights overflow int64")
			}
			c.againstWeight = sum
		}
	}
	return c, nil
}

// verifyBallots 依据成员名单与委托关系逐票核对并汇总：
// 代表必须是最终代表、不得重复、票重必须等于委托后归集到其名下的权重。
// 这些条件全部由打开状态文件时的严格校验保证，首次计票路径再核对一次，
// 使两条路径对“票重/代表是否可信”的判定使用同一份规则。
// 汇总只含实际投出的票；任一条件不符返回带具体原因的错误，
// 错误分类（ErrStateCorrupt）由调用方按自身路径统一包装。
func verifyBallots(id string, ballots []storedBallot, spec *proposalSpec) (ballotCounts, error) {
	seen := map[string]bool{}
	for i, b := range ballots {
		if b.Representative == "" {
			return ballotCounts{}, fmt.Errorf("proposal %s ballot %d has empty representative", id, i)
		}
		if spec.headOf[b.Representative] != b.Representative {
			return ballotCounts{}, fmt.Errorf("proposal %s ballot %d: %q is not a final representative",
				id, i, b.Representative)
		}
		if seen[b.Representative] {
			return ballotCounts{}, fmt.Errorf("proposal %s has duplicate stored ballot for %q", id, b.Representative)
		}
		seen[b.Representative] = true
		if b.Weight != spec.groupW[b.Representative] {
			return ballotCounts{}, fmt.Errorf("proposal %s ballot weight for %q (%d) does not match roster delegation (%d)",
				id, b.Representative, b.Weight, spec.groupW[b.Representative])
		}
	}
	counts, err := countBallots(ballots)
	if err != nil {
		return ballotCounts{}, fmt.Errorf("proposal %s %v", id, err)
	}
	return counts, nil
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

// TallyVote 在截止时刻或之后进行首次计票：赞成与反对权重之和达到法定人数，
// 且赞成严格多于反对才通过。未投权重不计入参与量。
// 计票只确定状态，不转账；再次计票始终返回首次结论。
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
	// 计票规则只有一份：同一套“代表资格 + 票重 + 两侧汇总 + 法定人数/多数”
	// 同时用于首次计票、打开文件时的重放校验与查询结论。
	counts, verr := verifyBallots(id, p.Ballots, spec)
	if verr != nil {
		return nil, fmt.Errorf("%w: %v", ErrStateCorrupt, verr)
	}
	passed := votePasses(counts.forWeight, counts.againstWeight, p.Quorum)
	p.Tally = &storedTally{ForWeight: counts.forWeight, AgainstWeight: counts.againstWeight, TalliedAt: now}
	p.State = tallyVerdict(passed)
	if err := s.commitLocked(state); err != nil {
		return nil, err
	}
	return storedTallyResult(p), nil
}

// storedTallyResult 从已存储提案重建首次计票结论；尚未计票时返回 nil。
// Passed 由唯一的通过规则（votePasses）按保存权重重算：提案后续执行
// （executed）不改变首次结论，查询结论与首次计票结论因此始终一致。
// 调用方持有的状态均经过 validateState，两侧合计必然不超过合法总权重。
func storedTallyResult(p *storedVoteProposal) *TallyResultView {
	if p.Tally == nil {
		return nil
	}
	c := ballotCounts{forWeight: p.Tally.ForWeight, againstWeight: p.Tally.AgainstWeight}
	turnout, _ := c.turnout()
	return &TallyResultView{
		ForWeight:     c.forWeight,
		AgainstWeight: c.againstWeight,
		Turnout:       turnout,
		Quorum:        p.Quorum,
		Passed:        votePasses(c.forWeight, c.againstWeight, p.Quorum),
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

		// 票据明细的代表资格、重复票据、票重与两侧汇总，全部复用首次计票的
		// verifyBallots：打开重放与首次计票对“票重/代表是否可信”只有一份判定，
		// 汇总只含最终代表实际投出的票，未投成员权重不进入参与量。
		counts, err := verifyBallots(p.ID, p.Ballots, spec)
		if err != nil {
			return err
		}
		// 首次投票时间必须落在窗口 [start, deadline) 内；此约束与票重汇总无关，
		// 仍只属于打开时的明细校验。
		for _, b := range p.Ballots {
			if b.VotedAt < p.StartAt || b.VotedAt >= p.Deadline {
				return fmt.Errorf("voting proposal %q ballot by %q cast at %d outside window [%d,%d)",
					p.ID, b.Representative, b.VotedAt, p.StartAt, p.Deadline)
			}
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
			if p.Tally.ForWeight != counts.forWeight || p.Tally.AgainstWeight != counts.againstWeight {
				return fmt.Errorf("voting proposal %q tally weights for=%d/against=%d do not replay ballots for=%d/against=%d",
					p.ID, p.Tally.ForWeight, p.Tally.AgainstWeight, counts.forWeight, counts.againstWeight)
			}
			// 通过条件同样只有一份：保存状态必须与票据重放出的结论一致，
			// executed 是 passed 提案执行后的后继状态，仍对应“首次通过”。
			passedNow := votePasses(counts.forWeight, counts.againstWeight, p.Quorum)
			if passedNow != (p.State == "passed" || p.State == "executed") {
				return fmt.Errorf("voting proposal %q state %s does not match quorum/majority replay", p.ID, p.State)
			}
		}
	}
	return nil
}
