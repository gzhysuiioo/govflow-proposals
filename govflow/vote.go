package govflow

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// 投票与计票相关错误。
var (
	// ErrInvalidVoting：创建投票提案时参数非法（编号为空、权重越界、时间不合法等）。
	ErrInvalidVoting = errors.New("invalid voting proposal")
	// ErrVotingConflict：同编号投票提案再次创建，但成员、委托、法定人数、时间或动作与首次不同。
	ErrVotingConflict = errors.New("voting proposal already exists with different content")
	// ErrNotVotingProposal：对一个登记提案执行投票相关操作。
	ErrNotVotingProposal = errors.New("proposal is not a voting proposal")
	// ErrVoteConflict：同一代表已投过相同提案的票，但选择不同。
	ErrVoteConflict = errors.New("vote already recorded with a different choice")
	// ErrVotingClosed：投票窗口未开始、已结束或已计票，新票被拒绝。
	ErrVotingClosed = errors.New("voting is closed")
	// ErrTallyTooEarly：计票时间早于投票截止时刻。
	ErrTallyTooEarly = errors.New("tally time is before voting end")
)

// VotingMemberSpec 是创建投票提案时提交的成员及其原始权重。
type VotingMemberSpec struct {
	ID     string
	Weight int64
}

// VotingDelegationSpec 是创建投票提案时提交的委托关系：From 委托 To 代为投票。
type VotingDelegationSpec struct {
	From string
	To   string
}

// VotingSpec 是创建投票提案的全部输入。
type VotingSpec struct {
	ID          string
	Members     []VotingMemberSpec
	Delegations []VotingDelegationSpec
	Quorum      int64
	Start       int64
	End         int64
	Timelock    int64
	Actions     []string
}

// VoteRecord 是一张已记录的票：最终代表、票重（归到其名下的全部原始权重）、
// 选择以及首次投票时间。
type VoteRecord struct {
	ProposalID string `json:"proposal_id"`
	Voter      string `json:"voter"`
	Weight     int64  `json:"weight"`
	Support    bool   `json:"support"`
	VotedAt    int64  `json:"voted_at"`
}

// TallyRecord 是首次计票的结论。
type TallyRecord struct {
	ProposalID   string `json:"proposal_id"`
	Result       string `json:"result"` // passed 或 rejected
	ForVotes     int64  `json:"for_votes"`
	AgainstVotes int64  `json:"against_votes"`
	Turnout      int64  `json:"turnout"`
	Quorum       int64  `json:"quorum"`
	TalliedAt    int64  `json:"tallied_at"`
}

// VotingMemberRecord 是查询投票提案时的成员视图：原始权重、委托路径与最终代表。
type VotingMemberRecord struct {
	ID                   string   `json:"id"`
	Weight               int64    `json:"weight"`
	DelegationPath       []string `json:"delegation_path"`
	Representative       string   `json:"representative"`
	RepresentativeWeight int64    `json:"representative_weight"`
}

// VotingRecord 是投票提案的完整查询视图。
type VotingRecord struct {
	ID           string               `json:"id"`
	State        string               `json:"state"`            // voting、passed、rejected、executed
	Result       string               `json:"result,omitempty"` // 首次计票结论 passed/rejected，未计票为空
	Members      []VotingMemberRecord `json:"members"`
	Quorum       int64                `json:"quorum"`
	TotalWeight  int64                `json:"total_weight"`
	Start        int64                `json:"start"`
	End          int64                `json:"end"`
	Timelock     int64                `json:"timelock"`
	Actions      []string             `json:"actions"`
	Votes        []VoteRecord         `json:"votes"`
	ForVotes     int64                `json:"for_votes"`
	AgainstVotes int64                `json:"against_votes"`
	Turnout      int64                `json:"turnout"`
	TalliedAt    *int64               `json:"tallied_at,omitempty"`
}

// ---- 磁盘结构（JSON） ----

type storedMember struct {
	ID     string `json:"id"`
	Weight int64  `json:"weight"`
}

type storedDelegation struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type storedVote struct {
	Voter   string `json:"voter"`
	Weight  int64  `json:"weight"`
	Support bool   `json:"support"`
	VotedAt int64  `json:"voted_at"`
}

type storedVoting struct {
	ID          string             `json:"id"`
	Members     []storedMember     `json:"members"`
	Delegations []storedDelegation `json:"delegations"`
	Quorum      int64              `json:"quorum"`
	Start       int64              `json:"start"`
	End         int64              `json:"end"`
	Timelock    int64              `json:"timelock"`
	Actions     []string           `json:"actions"`
	Votes       []*storedVote      `json:"votes"`
	TalliedAt   *int64             `json:"tallied_at,omitempty"`
	Result      string             `json:"result,omitempty"`
}

// ---- 校验与委托解析 ----

// validateVotingSpec 校验创建参数并返回规范化的磁盘结构。
// 成员编号非空且不重复，权重为正整数，总权重不溢出；法定人数在 1 至总权重之间；
// 时间满足 0 ≤ start < end ≤ timelock；委托关系合法（名单内、非自委托、不重复、不成环）。
func validateVotingSpec(spec VotingSpec) ([]storedMember, []storedDelegation, error) {
	if spec.ID == "" {
		return nil, nil, fmt.Errorf("%w: proposal id must not be empty", ErrInvalidVoting)
	}
	if len(spec.Members) == 0 {
		return nil, nil, fmt.Errorf("%w: member list must not be empty", ErrInvalidVoting)
	}
	members := make([]storedMember, 0, len(spec.Members))
	seen := map[string]bool{}
	var total int64
	for _, m := range spec.Members {
		if m.ID == "" {
			return nil, nil, fmt.Errorf("%w: member id must not be empty", ErrInvalidVoting)
		}
		if seen[m.ID] {
			return nil, nil, fmt.Errorf("%w: duplicate member %q", ErrInvalidVoting, m.ID)
		}
		if m.Weight <= 0 {
			return nil, nil, fmt.Errorf("%w: member %q weight must be a positive int64, got %d", ErrInvalidVoting, m.ID, m.Weight)
		}
		next, ok := addInt64(total, m.Weight)
		if !ok {
			return nil, nil, fmt.Errorf("%w: total member weight overflows int64 at member %q", ErrInvalidVoting, m.ID)
		}
		total = next
		seen[m.ID] = true
		members = append(members, storedMember{ID: m.ID, Weight: m.Weight})
	}
	if spec.Quorum < 1 || spec.Quorum > total {
		return nil, nil, fmt.Errorf("%w: quorum must be between 1 and total weight %d, got %d", ErrInvalidVoting, total, spec.Quorum)
	}
	if spec.Start < 0 || spec.Start >= spec.End || spec.End > spec.Timelock {
		return nil, nil, fmt.Errorf("%w: times must satisfy 0 <= start < end <= timelock, got start=%d end=%d timelock=%d",
			ErrInvalidVoting, spec.Start, spec.End, spec.Timelock)
	}
	delegations := make([]storedDelegation, 0, len(spec.Delegations))
	seenFrom := map[string]bool{}
	for _, d := range spec.Delegations {
		if d.From == "" || d.To == "" {
			return nil, nil, fmt.Errorf("%w: delegation member ids must not be empty", ErrInvalidVoting)
		}
		if !seen[d.From] {
			return nil, nil, fmt.Errorf("%w: delegating member %q is not in the member list", ErrInvalidVoting, d.From)
		}
		if !seen[d.To] {
			return nil, nil, fmt.Errorf("%w: delegation target %q is not in the member list", ErrInvalidVoting, d.To)
		}
		if d.From == d.To {
			return nil, nil, fmt.Errorf("%w: member %q must not delegate to themselves", ErrInvalidVoting, d.From)
		}
		if seenFrom[d.From] {
			return nil, nil, fmt.Errorf("%w: member %q has already delegated", ErrInvalidVoting, d.From)
		}
		seenFrom[d.From] = true
		delegations = append(delegations, storedDelegation{From: d.From, To: d.To})
	}
	if _, _, err := resolveDelegations(members, delegations); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrInvalidVoting, err)
	}
	return members, delegations, nil
}

// resolveDelegations 沿委托链解析每名成员的最终代表与委托路径。
// 委托可继续转交：A→B、B→C，则 A 的路径为 [A B C]，最终代表为 C；
// 未委托者代表自己。存在环或目标不在名单内时返回错误。
func resolveDelegations(members []storedMember, delegations []storedDelegation) (representative map[string]string, paths map[string][]string, err error) {
	memberSet := map[string]bool{}
	for _, m := range members {
		memberSet[m.ID] = true
	}
	out := map[string]string{}
	for _, d := range delegations {
		if !memberSet[d.From] {
			return nil, nil, fmt.Errorf("delegating member %q is not in the member list", d.From)
		}
		if !memberSet[d.To] {
			return nil, nil, fmt.Errorf("delegation target %q is not in the member list", d.To)
		}
		out[d.From] = d.To
	}
	representative = map[string]string{}
	paths = map[string][]string{}
	for _, m := range members {
		seen := map[string]bool{m.ID: true}
		path := []string{m.ID}
		cur := m.ID
		for {
			to, ok := out[cur]
			if !ok {
				break
			}
			if seen[to] {
				return nil, nil, fmt.Errorf("delegation cycle involving %q", to)
			}
			seen[to] = true
			path = append(path, to)
			cur = to
		}
		representative[m.ID] = cur
		paths[m.ID] = path
	}
	return representative, paths, nil
}

// attributedWeights 计算归到每名最终代表名下的全部原始权重。
func attributedWeights(members []storedMember, representative map[string]string) map[string]int64 {
	weights := map[string]int64{}
	for _, m := range members {
		weights[representative[m.ID]] += m.Weight
	}
	return weights
}

// sameVoting 比较两项投票提案的全部内容：成员（权重按编号映射比较，与输入次序无关）、
// 委托（按 from 映射比较，与输入次序无关）、法定人数、时间与动作（按顺序逐字比较）。
func sameVoting(a, b *storedVoting) bool {
	if a.Quorum != b.Quorum || a.Start != b.Start || a.End != b.End || a.Timelock != b.Timelock {
		return false
	}
	if !sameMembers(a.Members, b.Members) || !sameDelegations(a.Delegations, b.Delegations) {
		return false
	}
	return sameActions(a.Actions, b.Actions)
}

func sameMembers(a, b []storedMember) bool {
	if len(a) != len(b) {
		return false
	}
	weights := map[string]int64{}
	for _, m := range a {
		weights[m.ID] += m.Weight
	}
	for _, m := range b {
		w, ok := weights[m.ID]
		if !ok || w != m.Weight {
			return false
		}
	}
	return true
}

func sameDelegations(a, b []storedDelegation) bool {
	if len(a) != len(b) {
		return false
	}
	targets := map[string]string{}
	for _, d := range a {
		targets[d.From] = d.To
	}
	for _, d := range b {
		if targets[d.From] != d.To {
			return false
		}
	}
	return true
}

// ---- 创建 ----

// CreateVoting 创建一项投票提案：提交成员名单与权重、可选委托、法定人数、
// 投票起止时间、执行时间锁及有序动作。同编号且全部内容相同的重试返回已有提案
// （existed=true）且不改变当前状态；内容不同返回 ErrVotingConflict。
// 已被登记提案占用的编号同样返回冲突。
func (s *Store) CreateVoting(spec VotingSpec) (existed bool, err error) {
	members, delegations, err := validateVotingSpec(spec)
	if err != nil {
		return false, err
	}
	commit, err := s.begin()
	if err != nil {
		return false, err
	}
	defer commit()

	state, err := s.loadLocked()
	if err != nil {
		return false, err
	}
	stored := &storedVoting{
		ID:          spec.ID,
		Members:     members,
		Delegations: delegations,
		Quorum:      spec.Quorum,
		Start:       spec.Start,
		End:         spec.End,
		Timelock:    spec.Timelock,
		Actions:     copyActions(spec.Actions),
	}
	if existing, ok := state.Votings[spec.ID]; ok {
		if sameVoting(existing, stored) {
			return true, nil
		}
		return false, fmt.Errorf("%w: voting proposal %s", ErrVotingConflict, spec.ID)
	}
	if _, ok := state.Proposals[spec.ID]; ok {
		return false, fmt.Errorf("%w: proposal %s is already registered", ErrProposalConflict, spec.ID)
	}
	state.Votings[spec.ID] = stored
	state.Proposals[spec.ID] = &storedProposal{
		ID:          spec.ID,
		State:       "voting",
		TimelockEnd: spec.Timelock,
		Actions:     copyActions(spec.Actions),
	}
	if err := s.commitLocked(state); err != nil {
		return false, err
	}
	return false, nil
}

// ---- 投票 ----

// Vote 为提案投赞成或反对票。只有最终代表（未委托出去的名单成员）可投票，
// 票重等于归到其名下的全部原始权重。首次投票仅在 start ≤ now < end 时接受；
// 已计票后拒绝新票。同一代表相同选择的重试返回首次记录，改投另一选择返回冲突。
func (s *Store) Vote(id, member string, support bool, now int64) (*VoteRecord, error) {
	commit, err := s.begin()
	if err != nil {
		return nil, err
	}
	defer commit()

	state, err := s.loadLocked()
	if err != nil {
		return nil, err
	}
	voting, ok := state.Votings[id]
	if !ok {
		if _, exists := state.Proposals[id]; exists {
			return nil, fmt.Errorf("%w: %s", ErrNotVotingProposal, id)
		}
		return nil, fmt.Errorf("%w: %s", ErrProposalNotFound, id)
	}
	if voting.TalliedAt != nil {
		return nil, fmt.Errorf("%w: voting for proposal %s has already concluded", ErrVotingClosed, id)
	}
	memberSet := map[string]bool{}
	for _, m := range voting.Members {
		memberSet[m.ID] = true
	}
	if !memberSet[member] {
		return nil, fmt.Errorf("%w: member %q is not in the roster of proposal %s", ErrNotVotingProposal, member, id)
	}
	delegated := map[string]bool{}
	for _, d := range voting.Delegations {
		delegated[d.From] = true
	}
	if delegated[member] {
		return nil, fmt.Errorf("%w: member %q has delegated and cannot vote; only the final representative can vote", ErrNotVotingProposal, member)
	}
	if now < voting.Start {
		return nil, fmt.Errorf("%w: voting for proposal %s starts at %d (now=%d)", ErrVotingClosed, id, voting.Start, now)
	}
	if now >= voting.End {
		return nil, fmt.Errorf("%w: voting for proposal %s ended at %d (now=%d)", ErrVotingClosed, id, voting.End, now)
	}
	representative, _, err := resolveDelegations(voting.Members, voting.Delegations)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	weight := attributedWeights(voting.Members, representative)[member]
	for _, v := range voting.Votes {
		if v.Voter == member {
			if v.Support == support {
				return &VoteRecord{
					ProposalID: id,
					Voter:      v.Voter,
					Weight:     v.Weight,
					Support:    v.Support,
					VotedAt:    v.VotedAt,
				}, nil
			}
			return nil, fmt.Errorf("%w: representative %q already voted %s on proposal %s",
				ErrVoteConflict, member, choiceText(v.Support), id)
		}
	}
	vote := &storedVote{
		Voter:   member,
		Weight:  weight,
		Support: support,
		VotedAt: now,
	}
	voting.Votes = append(voting.Votes, vote)
	if err := s.commitLocked(state); err != nil {
		return nil, err
	}
	return &VoteRecord{
		ProposalID: id,
		Voter:      member,
		Weight:     weight,
		Support:    support,
		VotedAt:    now,
	}, nil
}

func choiceText(support bool) string {
	if support {
		return "for"
	}
	return "against"
}

// ---- 计票 ----

// Tally 在投票截止时刻或之后计票：赞成与反对权重之和达到法定人数且赞成严格多于反对
// 才通过，否则拒绝。计票只确定状态，不转账。首次计票须在 end 或之后，否则报
// ErrTallyTooEarly 且状态不变；再次计票返回首次结论。
func (s *Store) Tally(id string, now int64) (*TallyRecord, error) {
	commit, err := s.begin()
	if err != nil {
		return nil, err
	}
	defer commit()

	state, err := s.loadLocked()
	if err != nil {
		return nil, err
	}
	voting, ok := state.Votings[id]
	if !ok {
		if _, exists := state.Proposals[id]; exists {
			return nil, fmt.Errorf("%w: %s", ErrNotVotingProposal, id)
		}
		return nil, fmt.Errorf("%w: %s", ErrProposalNotFound, id)
	}
	if voting.TalliedAt != nil {
		return buildTallyRecord(voting, id), nil
	}
	if now < voting.End {
		return nil, fmt.Errorf("%w: voting for proposal %s ends at %d (now=%d)", ErrTallyTooEarly, id, voting.End, now)
	}
	var forVotes, againstVotes int64
	for _, v := range voting.Votes {
		if v.Support {
			forVotes += v.Weight
		} else {
			againstVotes += v.Weight
		}
	}
	turnout := forVotes + againstVotes
	result := "rejected"
	if turnout >= voting.Quorum && forVotes > againstVotes {
		result = "passed"
	}
	voting.TalliedAt = &now
	voting.Result = result
	if proposal, ok := state.Proposals[id]; ok {
		proposal.State = result
	}
	if err := s.commitLocked(state); err != nil {
		return nil, err
	}
	return buildTallyRecord(voting, id), nil
}

func buildTallyRecord(voting *storedVoting, id string) *TallyRecord {
	var forVotes, againstVotes int64
	for _, v := range voting.Votes {
		if v.Support {
			forVotes += v.Weight
		} else {
			againstVotes += v.Weight
		}
	}
	return &TallyRecord{
		ProposalID:   id,
		Result:       voting.Result,
		ForVotes:     forVotes,
		AgainstVotes: againstVotes,
		Turnout:      forVotes + againstVotes,
		Quorum:       voting.Quorum,
		TalliedAt:    *voting.TalliedAt,
	}
}

// ---- 查询 ----

// Voting 按编号返回投票提案的完整查询视图；不存在时 ok 为 false。
func (s *Store) Voting(id string) (*VotingRecord, bool, error) {
	state, err := s.readState()
	if err != nil {
		return nil, false, err
	}
	voting, ok := state.Votings[id]
	if !ok {
		return nil, false, nil
	}
	return buildVotingRecord(voting, id, state.Proposals[id]), true, nil
}

// Votings 返回全部投票提案的查询视图，按编号排序。
func (s *Store) Votings() ([]*VotingRecord, error) {
	state, err := s.readState()
	if err != nil {
		return nil, err
	}
	out := make([]*VotingRecord, 0, len(state.Votings))
	for id, voting := range state.Votings {
		out = append(out, buildVotingRecord(voting, id, state.Proposals[id]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func buildVotingRecord(voting *storedVoting, id string, proposal *storedProposal) *VotingRecord {
	representative, paths, _ := resolveDelegations(voting.Members, voting.Delegations)
	attributed := attributedWeights(voting.Members, representative)
	total := int64(0)
	members := make([]VotingMemberRecord, 0, len(voting.Members))
	for _, m := range voting.Members {
		total += m.Weight
		members = append(members, VotingMemberRecord{
			ID:                   m.ID,
			Weight:               m.Weight,
			DelegationPath:       append([]string{}, paths[m.ID]...),
			Representative:       representative[m.ID],
			RepresentativeWeight: attributed[representative[m.ID]],
		})
	}
	sort.Slice(members, func(i, j int) bool { return members[i].ID < members[j].ID })

	votes := make([]VoteRecord, 0, len(voting.Votes))
	var forVotes, againstVotes int64
	for _, v := range voting.Votes {
		votes = append(votes, VoteRecord{
			ProposalID: id,
			Voter:      v.Voter,
			Weight:     v.Weight,
			Support:    v.Support,
			VotedAt:    v.VotedAt,
		})
		if v.Support {
			forVotes += v.Weight
		} else {
			againstVotes += v.Weight
		}
	}
	// 按首次投票时间排序，时间相同按代表编号，保证确定性。
	sort.Slice(votes, func(i, j int) bool {
		if votes[i].VotedAt != votes[j].VotedAt {
			return votes[i].VotedAt < votes[j].VotedAt
		}
		return votes[i].Voter < votes[j].Voter
	})

	state := "voting"
	if proposal != nil {
		state = proposal.State
	}
	record := &VotingRecord{
		ID:           id,
		State:        state,
		Result:       voting.Result,
		Members:      members,
		Quorum:       voting.Quorum,
		TotalWeight:  total,
		Start:        voting.Start,
		End:          voting.End,
		Timelock:     voting.Timelock,
		Actions:      copyActions(voting.Actions),
		Votes:        votes,
		ForVotes:     forVotes,
		AgainstVotes: againstVotes,
		Turnout:      forVotes + againstVotes,
	}
	if voting.TalliedAt != nil {
		t := *voting.TalliedAt
		record.TalliedAt = &t
	}
	return record
}

// ---- 打开状态文件时的一致性校验 ----

// validateVotingRecord 校验落盘的投票记录：名单、权重、法定人数、时间、委托链、
// 票重与代表归属、计票结论必须与明细一致。任何不一致都拒绝打开。
func validateVotingRecord(v *storedVoting, key string) error {
	if v == nil {
		return fmt.Errorf("voting proposal %q is empty", key)
	}
	if v.ID != key || v.ID == "" {
		return fmt.Errorf("voting proposal key %q does not match id %q", key, v.ID)
	}
	memberSet := map[string]bool{}
	var total int64
	for _, m := range v.Members {
		if m.ID == "" {
			return errors.New("member id must not be empty")
		}
		if memberSet[m.ID] {
			return fmt.Errorf("duplicate member %q", m.ID)
		}
		if m.Weight <= 0 {
			return fmt.Errorf("member %q weight %d is not a positive integer", m.ID, m.Weight)
		}
		next, ok := addInt64(total, m.Weight)
		if !ok {
			return fmt.Errorf("total member weight overflows int64 at member %q", m.ID)
		}
		total = next
		memberSet[m.ID] = true
	}
	if len(v.Members) == 0 {
		return errors.New("member list must not be empty")
	}
	if v.Quorum < 1 || v.Quorum > total {
		return fmt.Errorf("quorum %d is not between 1 and total weight %d", v.Quorum, total)
	}
	if v.Start < 0 || v.Start >= v.End || v.End > v.Timelock {
		return fmt.Errorf("times must satisfy 0 <= start < end <= timelock, got start=%d end=%d timelock=%d", v.Start, v.End, v.Timelock)
	}
	out := map[string]string{}
	for _, d := range v.Delegations {
		if d.From == "" || d.To == "" {
			return errors.New("delegation member ids must not be empty")
		}
		if !memberSet[d.From] {
			return fmt.Errorf("delegating member %q is not in the member list", d.From)
		}
		if !memberSet[d.To] {
			return fmt.Errorf("delegation target %q is not in the member list", d.To)
		}
		if d.From == d.To {
			return fmt.Errorf("member %q delegates to themselves", d.From)
		}
		if _, dup := out[d.From]; dup {
			return fmt.Errorf("member %q has delegated more than once", d.From)
		}
		out[d.From] = d.To
	}
	representative, _, err := resolveDelegations(v.Members, v.Delegations)
	if err != nil {
		return err
	}
	attributed := attributedWeights(v.Members, representative)

	seenVoters := map[string]bool{}
	var forVotes, againstVotes int64
	for _, vote := range v.Votes {
		if vote == nil {
			return errors.New("empty vote record")
		}
		if !memberSet[vote.Voter] {
			return fmt.Errorf("vote from %q who is not a member", vote.Voter)
		}
		if _, delegated := out[vote.Voter]; delegated {
			return fmt.Errorf("vote from %q who has delegated away", vote.Voter)
		}
		if vote.Weight != attributed[vote.Voter] {
			return fmt.Errorf("vote weight %d for %q does not match attributed weight %d", vote.Weight, vote.Voter, attributed[vote.Voter])
		}
		if seenVoters[vote.Voter] {
			return fmt.Errorf("duplicate vote from representative %q", vote.Voter)
		}
		if vote.VotedAt < v.Start || vote.VotedAt >= v.End {
			return fmt.Errorf("vote from %q at %d is outside voting window [%d, %d)", vote.Voter, vote.VotedAt, v.Start, v.End)
		}
		seenVoters[vote.Voter] = true
		if vote.Support {
			forVotes += vote.Weight
		} else {
			againstVotes += vote.Weight
		}
	}
	if v.TalliedAt != nil {
		if *v.TalliedAt < v.End {
			return fmt.Errorf("tallied_at %d is before voting end %d", *v.TalliedAt, v.End)
		}
		if v.Result != "passed" && v.Result != "rejected" {
			return fmt.Errorf("unknown tally result %q", v.Result)
		}
		turnout := forVotes + againstVotes
		want := "rejected"
		if turnout >= v.Quorum && forVotes > againstVotes {
			want = "passed"
		}
		if v.Result != want {
			return fmt.Errorf("stored result %q does not match tally of votes (for=%d against=%d quorum=%d)", v.Result, forVotes, againstVotes, v.Quorum)
		}
	} else if v.Result != "" {
		return fmt.Errorf("result %q is set without a tally", v.Result)
	}
	return nil
}

// memberFlag 与 delegationFlag 供 CLI 解析使用，放在这里便于测试。

// ParseMemberFlag 解析 "id:weight" 形式的成员参数。
func ParseMemberFlag(raw string) (VotingMemberSpec, error) {
	id, weightRaw, ok := strings.Cut(raw, ":")
	if !ok || id == "" || weightRaw == "" {
		return VotingMemberSpec{}, fmt.Errorf("malformed member %q: expected id:weight", raw)
	}
	weight, err := parsePositiveInt64(weightRaw)
	if err != nil {
		return VotingMemberSpec{}, fmt.Errorf("malformed member %q: %v", raw, err)
	}
	return VotingMemberSpec{ID: id, Weight: weight}, nil
}

// ParseDelegationFlag 解析 "from:to" 形式的委托参数。
func ParseDelegationFlag(raw string) (VotingDelegationSpec, error) {
	from, to, ok := strings.Cut(raw, ":")
	if !ok || from == "" || to == "" || strings.Contains(to, ":") {
		return VotingDelegationSpec{}, fmt.Errorf("malformed delegation %q: expected from:to", raw)
	}
	return VotingDelegationSpec{From: from, To: to}, nil
}
