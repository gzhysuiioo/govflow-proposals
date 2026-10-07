package govflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"unicode/utf8"
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

// storedBallot 是一张已保存的票。四个字段都必须明确写出、非空且类型正确：
// representative 是 JSON 字符串，weight/voted_at 是 JSON 整数（int64 范围），
// support 必须是 JSON 布尔值。绝不能靠零值/默认值补出治理记录：
// 缺失或 null 的 support 不得被当作 false（反对票），缺失或 null 的
// voted_at 不得被当作 0（首次投票时间）。
//
// 标量字段先用 RawMessage 接住，使“字段缺失”与“显式 null/写错类型”在解码后
// 仍可区分：encoding/json 直接解进 bool/int64/string 时会把这三种情况都
// 折叠成零值。每个字段的“缺失/空值/类型不符”由 validateStoredBallot 判定。
type storedBallot struct {
	Representative string `json:"representative"`
	Weight         int64  `json:"weight"`
	Support        bool   `json:"support"`
	VotedAt        int64  `json:"voted_at"`

	representativeRaw json.RawMessage
	weightRaw         json.RawMessage
	supportRaw        json.RawMessage
	votedAtRaw        json.RawMessage
}

// ballotJSONShape 只用于解码，按保存格式的字段名逐字接住每个字段的原始 JSON。
type ballotJSONShape struct {
	Representative json.RawMessage `json:"representative"`
	Weight         json.RawMessage `json:"weight"`
	Support        json.RawMessage `json:"support"`
	VotedAt        json.RawMessage `json:"voted_at"`
}

// UnmarshalJSON 保留字段是否出现及其原始写法（缺失为 nil、null 为 "null"），
// 供 validateStoredBallot 区分缺失、空值与类型不符。单个字段类型不符时这里
// 不返回错误（对应治理字段保持零值），以免解码器在不含提案编号/票据下标的
// 通用错误处提前失败；定位与判定统一交给 validateStoredBallot。
func (b *storedBallot) UnmarshalJSON(data []byte) error {
	var shape ballotJSONShape
	if err := json.Unmarshal(data, &shape); err != nil {
		return err
	}
	b.representativeRaw, b.weightRaw = shape.Representative, shape.Weight
	b.supportRaw, b.votedAtRaw = shape.Support, shape.VotedAt
	// 仅当字段确实是目标类型时才填充治理字段；类型不符留待校验拒绝，
	// 绝不能让错误类型悄悄落成零值并参与查询或计票。
	fillString(shape.Representative, &b.Representative)
	fillInt64(shape.Weight, &b.Weight)
	fillBool(shape.Support, &b.Support)
	fillInt64(shape.VotedAt, &b.VotedAt)
	return nil
}

// MarshalJSON 只输出四个治理字段，RawMessage 状态（仅用于入站判定）不落盘。
func (b storedBallot) MarshalJSON() ([]byte, error) {
	return json.Marshal(storedBallotOut{
		Representative: b.Representative,
		Weight:         b.Weight,
		Support:        b.Support,
		VotedAt:        b.VotedAt,
	})
}

// storedBallotOut 是票据的保存形状；字段名与保存格式逐字一致。
type storedBallotOut struct {
	Representative string `json:"representative"`
	Weight         int64  `json:"weight"`
	Support        bool   `json:"support"`
	VotedAt        int64  `json:"voted_at"`
}

// mustStoreBallot 构造一张内存中的合法票据（投票成功路径使用）。
// 同步填上字段原始片段，使“提交前校验”与“打开重放校验”走同一份
// validateStoredBallot 判定时不会把本进程新建的票据误判为字段缺失。
func mustStoreBallot(representative string, weight int64, support bool, votedAt int64) storedBallot {
	repRaw, _ := json.Marshal(representative)
	wRaw, _ := json.Marshal(weight)
	supRaw, _ := json.Marshal(support)
	atRaw, _ := json.Marshal(votedAt)
	return storedBallot{
		Representative:    representative,
		Weight:            weight,
		Support:           support,
		VotedAt:           votedAt,
		representativeRaw: repRaw,
		weightRaw:         wRaw,
		supportRaw:        supRaw,
		votedAtRaw:        atRaw,
	}
}

// validateStoredBallot 严格判定一张已保存票据的四个必填字段：
// 任一字段未写出、显式为 null 或 JSON 类型不符（support 不接受字符串/数字，
// weight/voted_at 不接受字符串/小数/指数等非整数）都返回带原因的错误。
// 明确写出的 false 是有效反对票；窗口允许时明确写出的 0 是有效投票时间，
// 这两种合法值不得当成缺失。index 是票据在提案中的下标（0 起），用于定位。
// “缺失/空值/类型不符”的判定与计票结果、执行凭据共用同一份规则
// （validateRawFields），此处只补充票据的业务位置：提案编号 + 票据下标。
func validateStoredBallot(id string, index int, b *storedBallot) error {
	return validateRawFields([]rawField{
		{name: "representative", kind: scalarString, raw: b.representativeRaw},
		{name: "weight", kind: scalarInteger, raw: b.weightRaw},
		{name: "support", kind: scalarBoolean, raw: b.supportRaw},
		{name: "voted_at", kind: scalarInteger, raw: b.votedAtRaw},
	}, func(field, problem string) error {
		return fmt.Errorf("voting proposal %q ballot %d field %q %s", id, index, field, problem)
	})
}

// storedTally 是已保存的首次计票结果。只要提案有计票结果，for_weight 与
// against_weight 就必须各自明确写出：即使某一侧（或两侧）的实际票重为零，
// 也不得靠零值/默认值补出治理记录——缺失或 null 的 for_weight 不得被当作 0
// 赞成，缺失或 null 的 against_weight 不得被当作 0 反对。
//
// 两个权重字段先用 RawMessage 接住，使“字段缺失”与“显式 null/写错类型”在解码后
// 仍可区分：encoding/json 直接解进 int64 时会把这三种情况都折叠成 0，恰好与
// “该侧没有票”的合法零权重无法区分。每个字段的“缺失/空值/类型不符”由
// validateStoredTally 判定。
type storedTally struct {
	ForWeight     int64 `json:"for_weight"`
	AgainstWeight int64 `json:"against_weight"`
	TalliedAt     int64 `json:"tallied_at"`

	forWeightRaw     json.RawMessage
	againstWeightRaw json.RawMessage
}

// tallyJSONShape 只用于解码，按保存格式的字段名逐字接住每个字段的原始 JSON。
type tallyJSONShape struct {
	ForWeight     json.RawMessage `json:"for_weight"`
	AgainstWeight json.RawMessage `json:"against_weight"`
	TalliedAt     int64           `json:"tallied_at"`
}

// UnmarshalJSON 保留两个权重字段是否出现及其原始写法（缺失为 nil、null 为
// "null"），供 validateStoredTally 区分缺失、空值与类型不符。单个字段类型不符时
// 这里不返回错误（对应治理字段保持零值），以免解码器在不含提案编号的通用错误处
// 提前失败；定位与判定统一交给 validateStoredTally。
func (t *storedTally) UnmarshalJSON(data []byte) error {
	var shape tallyJSONShape
	if err := json.Unmarshal(data, &shape); err != nil {
		return err
	}
	t.forWeightRaw, t.againstWeightRaw = shape.ForWeight, shape.AgainstWeight
	t.TalliedAt = shape.TalliedAt
	// 仅当字段确实是 int64 整数时才填充治理字段；类型不符留待校验拒绝，
	// 绝不能让错误类型悄悄落成 0 并参与查询或再次计票。
	fillInt64(shape.ForWeight, &t.ForWeight)
	fillInt64(shape.AgainstWeight, &t.AgainstWeight)
	return nil
}

// mustStoreTally 构造一份内存中的合法计票结果（首次计票路径使用）。
// 同步填上字段原始片段，使“提交前校验”与“打开重放校验”走同一份
// validateStoredTally 判定时不会把本进程新建的计票结果误判为字段缺失。
func mustStoreTally(forWeight, againstWeight, talliedAt int64) *storedTally {
	forRaw, _ := json.Marshal(forWeight)
	againstRaw, _ := json.Marshal(againstWeight)
	return &storedTally{
		ForWeight:        forWeight,
		AgainstWeight:    againstWeight,
		TalliedAt:        talliedAt,
		forWeightRaw:     forRaw,
		againstWeightRaw: againstRaw,
	}
}

// validateStoredTally 严格判定一份已保存计票结果的两个权重字段：
// for_weight 与 against_weight 任一未写出、显式为 null 或不是 int64 整数
// （字符串/小数/指数/超界）都返回带提案编号、字段名与原因的错误。
// 明确写出的 0 是合法权重（该侧没有票），不得当成缺失。
// “缺失/空值/类型不符”的判定与逐票明细、执行凭据共用同一份规则
// （validateRawFields），此处只补充计票记录的业务位置：提案编号 + tally 标识。
func validateStoredTally(id string, t *storedTally) error {
	return validateRawFields([]rawField{
		{name: "for_weight", kind: scalarInteger, raw: t.forWeightRaw},
		{name: "against_weight", kind: scalarInteger, raw: t.againstWeightRaw},
	}, func(field, problem string) error {
		return fmt.Errorf("voting proposal %q tally field %q %s", id, field, problem)
	})
}

// storedVoteProposal 按成员提交顺序保存成员与委托（顺序不影响相等性），
// 动作文本与顺序原样保存（顺序影响相等性）。
//
// delegations 必须明确写出且为 JSON 数组：委托记录决定成员的完整委托路径、
// 最终代表与归集票重，谁能直接投票完全由它派生。字段缺失或为 null 绝不能
// 被解释成“撤回全部委托”——那样已委托成员会被显示成各自代表、还能直接
// 投票，悄悄改变投票资格；写成对象、字符串等其它类型同样不合法。缺少
// 记录不能按零值补出，因此解码时保留 delegations 的原始 JSON，由
// validateVoteProposalDelegations 区分“缺失/空值/类型不符”。明确写出的
// 空数组仍表示无人委托，成员各自代表自己、按原始权重投票。
//
// ballots 同理必须明确写出且为 JSON 数组：它是代表选择与首次投票时间的
// 唯一留痕。字段缺失或为 null 绝不能折叠成空列表——尚在投票的提案会因此
// 被展示成无人投票、已投代表还能再次投票，到期计票则按零参与权重给出
// 拒绝结论，原有选择与首次投票时间全部丢失；写成对象、字符串等其它类型
// 同样不合法。此要求不取决于提案状态：voting/passed/rejected/executed
// 都适用，即使计票汇总恰好为零、资金余额仍能与执行凭据对应，也不能把缺损
// 列表补成空数组。解码时保留 ballots 的原始 JSON，由
// validateVoteProposalBallots 区分“缺失/空值/类型不符”。明确写出的空数组
// 仍表示确实无人投票。
//
// start_at 同样必须明确写出且为 int64 范围内的 JSON 整数：它决定投票窗口
// 何时开启，字段缺失、为 null 或类型不符绝不能被折叠成 0——原本在 100
// 开始、200 截止的提案，缺失开始时间若被读成 0，窗口会被悄悄向前扩大，
// 开始之前投出的票就可能被当成窗口内的首次投票接受。取值不得从票据时间、
// 截止时间、时间锁或其它记录推算补齐，也不能补零后继续使用；即使票据、
// 计票结论与执行凭据都能与其余内容核对一致也不宽免。解码时保留 start_at
// 的原始 JSON，由 validateVoteProposalStartAt 区分“缺失/空值/类型不符”。
// 明确写出的 0 是合法开始时间，不与缺失混同；合法整数仍须满足既有的
// 0 <= start < deadline <= timelock 规则（specOf 复核）。
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

	startAtRaw json.RawMessage

	delegationsRaw json.RawMessage
	// delegationsShapeErr 记录数组元素形状与 []storedDelegation 不符时的解码
	// 错误（如元素写成数字、字符串、from/to 不是字符串）。外层 UnmarshalJSON
	// 不直接返回它，以免解码器抢在 validateVoteProposalDelegations 之前给出
	// 不含提案编号的通用错误；判定与错误文案统一由该校验函数负责。
	delegationsShapeErr error

	ballotsRaw json.RawMessage
	// ballotsShapeErr 记录数组元素形状与 []storedBallot 不符时的解码错误
	// （如元素写成数字、字符串）。外层 UnmarshalJSON 不直接返回它，以免
	// 解码器抢在 validateVoteProposalBallots 之前给出不含提案编号的通用
	// 错误；判定与错误文案统一由该校验函数负责。
	ballotsShapeErr error
}

// storedVoteProposalJSONShape 只用于解码，按保存格式的字段名逐字接住
// delegations、ballots 与 start_at 的原始 JSON；其余字段直接按目标类型解码。
type storedVoteProposalJSONShape struct {
	ID          string          `json:"id"`
	State       string          `json:"state"`
	Members     []storedMember  `json:"members"`
	Delegations json.RawMessage `json:"delegations"`
	Quorum      int64           `json:"quorum"`
	StartAt     json.RawMessage `json:"start_at"`
	Deadline    int64           `json:"deadline"`
	TimelockEnd int64           `json:"timelock_end"`
	Actions     []string        `json:"actions"`
	Ballots     json.RawMessage `json:"ballots"`
	Tally       *storedTally    `json:"tally,omitempty"`
}

// UnmarshalJSON 保留 delegations/ballots 是否出现及其原始写法（缺失为 nil、
// null 为 "null"），使“字段缺失/显式 null/写错类型”与“明确写出空数组”在
// 解码后仍可区分：encoding/json 直接解进切片会把前三种情况都折叠成 nil
// 切片，委托缺失会把已委托成员当成各自代表，票据缺失则把已投票提案当成
// 无人投票。判定统一交给 validateVoteProposalDelegations 与
// validateVoteProposalBallots，此处只在字段确实是 JSON 数组时填充列表
// （元素形状由解码器核对，类型不符直接记录、延后报告）。
func (p *storedVoteProposal) UnmarshalJSON(data []byte) error {
	var shape storedVoteProposalJSONShape
	if err := json.Unmarshal(data, &shape); err != nil {
		return err
	}
	p.ID = shape.ID
	p.State = shape.State
	p.Members = shape.Members
	p.Quorum = shape.Quorum
	p.startAtRaw = shape.StartAt
	p.Deadline = shape.Deadline
	p.TimelockEnd = shape.TimelockEnd
	p.Actions = shape.Actions
	p.Tally = shape.Tally
	p.delegationsRaw = shape.Delegations
	p.ballotsRaw = shape.Ballots
	// 仅当 start_at 确实是 int64 整数时才填充开始时间；缺失/null/类型不符
	// 留待 validateVoteProposalStartAt 拒绝，绝不能让缺失字段悄悄落成 0 并
	// 参与窗口判断或查询展示。
	fillInt64(shape.StartAt, &p.StartAt)
	if jsonValueType(shape.Delegations) == "array" {
		// 元素形状非法时不在此返回：让整份文档解码通过，由
		// validateVoteProposalDelegations 统一给出带提案编号与字段名的
		// “类型不符”原因，而不是退化成不含提案定位的解码器通用错误。
		p.delegationsShapeErr = json.Unmarshal(shape.Delegations, &p.Delegations)
	}
	if jsonValueType(shape.Ballots) == "array" {
		// 同理：票据元素形状非法时延后由 validateVoteProposalBallots 统一
		// 报出带提案编号与字段名的原因。
		p.ballotsShapeErr = json.Unmarshal(shape.Ballots, &p.Ballots)
	}
	return nil
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
//
// 提案编号、成员编号与动作原文还必须是合法 UTF-8：状态文件以 JSON 保存，
// encoding/json 会把字符串中的非法 UTF-8 字节悄悄改写成替换字符 U+FFFD
// 且不报错。创建入口若放行，落盘的编号就不再是用户提交的原文——原编号
// 无法查询，不同成员可能被改写成同一个名字，整份状态文件随之无法读取。
// 因此这三类文本在一切落盘之前逐字校验，非法输入整项创建失败；
// 合法文本（含中文、表情、用户明确写出的 U+FFFD，以及 "\uD800" 这类
// 由反斜杠与普通字母组成的字面文本——创建参数不是 JSON 字符串，不做
// 转义重解释）原样保留。
func validateProposalInput(in *CreateVoteInput) (*proposalSpec, error) {
	if in.ID == "" {
		return nil, invalidProposal("proposal id must not be empty")
	}
	if problem := invalidUTF8(in.ID); problem != "" {
		return nil, invalidProposal("proposal id is not valid UTF-8: %s", problem)
	}
	if len(in.Members) == 0 {
		return nil, invalidProposal("proposal %s: member list must not be empty", in.ID)
	}
	weights := make(map[string]int64, len(in.Members))
	order := make([]string, 0, len(in.Members))
	for i, m := range in.Members {
		if m.ID == "" {
			return nil, invalidProposal("proposal %s: member id must not be empty", in.ID)
		}
		if problem := invalidUTF8(m.ID); problem != "" {
			return nil, invalidProposal("proposal %s: member %d id is not valid UTF-8: %s", in.ID, i, problem)
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

	// 动作原文逐字校验编码，但不校验动作格式：编码合法而格式错误的动作
	// 照常创建，由执行操作按现有规则拒绝，创建入口不提前改变动作资格。
	for i, action := range in.Actions {
		if problem := invalidUTF8(action); problem != "" {
			return nil, invalidProposal("proposal %s: action %d text is not valid UTF-8: %s", in.ID, i, problem)
		}
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

// invalidUTF8 定位文本中第一个非法 UTF-8 字节并给出可读描述；文本合法时
// 返回空串。残缺的多字节序列、续字节缺失、越界编码以及 WTF-8 形式写出的
// 代理码位（如 ED A0 80）都会被 DecodeRuneInString 判为单字节 RuneError；
// 用户明确写出的合法 U+FFFD（EF BF BD）解码长度为 3，照常放行。
func invalidUTF8(s string) string {
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			return fmt.Sprintf("illegal byte 0x%02X at byte offset %d", c, i)
		}
		i += size
	}
	return ""
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
// 非法条件整项拒绝，不落盘任何部分变化。提案编号、成员编号与动作原文
// 含有非法 UTF-8 字节时同样整项拒绝（ErrInvalidProposal）：保存会把这些
// 字节改写成 U+FFFD，使落盘文本不再是用户提交的原文；因此即使非法输入
// 改写后恰好等于状态中已存在的合法编号（如含真实 U+FFFD 的编号），
// 也不被当作该编号的重试或覆盖。合法文本逐字保留，不做任何转义重解释。
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
	// 同步填上字段原始片段，使“提交前校验”与“打开重放校验”走同一份
	// start_at/delegations/ballots 判定时不会把本进程新建的提案误判为字段
	// 缺失。开始时间为 0 时写出的也是明确的 0；创建时不提供委托、尚无投票
	// 都是合法的：空数组明确写出即表示无人委托、无人投票。
	stored.startAtRaw, _ = json.Marshal(stored.StartAt)
	stored.delegationsRaw, _ = json.Marshal(stored.Delegations)
	stored.ballotsRaw, _ = json.Marshal(stored.Ballots)
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

	p.Ballots = append(p.Ballots, mustStoreBallot(voter, spec.groupW[voter], support, now))
	// 同步刷新 ballots 原始片段：提交前 validateState 与读取路径走同一份
	// “必须明确写出数组”判定，追加新票后该片段仍应是实际票据列表。
	p.ballotsRaw, _ = json.Marshal(p.Ballots)
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
	p.Tally = mustStoreTally(counts.forWeight, counts.againstWeight, now)
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

// requiredArrayProblem 统一判定一个“必须明确写出的 JSON 数组”字段的原始
// JSON：字段未写出返回 is missing、显式为 null 返回 is null、写成对象/字符串/
// 数字/布尔等其它类型返回带期望与实际类型的类型不符原因；合法数组（含空
// 数组）返回空串。与 requiredScalarProblem 同理，缺失绝不补成空数组。
func requiredArrayProblem(raw json.RawMessage) string {
	switch {
	case raw == nil:
		return "is missing"
	case jsonValueType(raw) == "null":
		return "is null"
	case jsonValueType(raw) != "array":
		return fmt.Sprintf("has wrong type: want array, got %s", jsonValueType(raw))
	}
	return ""
}

// requiredObjectProblem 统一判定一个“出现时必须为 JSON 对象”的字段的原始
// JSON：字段未写出返回 is missing、显式为 null 返回 is null、写成数组/字符串/
// 数字/布尔等其它类型返回带期望与实际类型的类型不符原因；合法对象（含空
// 对象 {}）返回空串。与 requiredArrayProblem 同理，显式 null 不能折叠成空
// 对象；但与必填字段不同，缺失是否兼容由调用方按自己的旧版本约定决定
// （见 validateVoteProposalsTable）。
func requiredObjectProblem(raw json.RawMessage) string {
	switch {
	case raw == nil:
		return "is missing"
	case jsonValueType(raw) == "null":
		return "is null"
	case jsonValueType(raw) != "object":
		return fmt.Sprintf("has wrong type: want object, got %s", jsonValueType(raw))
	}
	return ""
}

// validateVoteProposalDelegations 严格判定一项已保存投票提案的 delegations：
// 字段必须明确写出且为 JSON 数组。字段缺失、显式为 null 或写成对象/字符串等
// 其它类型都返回带提案编号与字段名的错误，由 validateState 判整份状态文件
// 损坏——委托记录决定完整路径、最终代表与归集票重，缺少记录不能被解释成
// “撤回委托”，即使提案尚未投票、票据与计票结论仍能复核也不例外；未投票、
// 已计票与已执行的提案适用同一条要求。明确写出的空数组是合法的无人委托
// 列表，成员各自代表自己、按原始权重投票，不与缺失混同。
func validateVoteProposalDelegations(p *storedVoteProposal) error {
	if problem := requiredArrayProblem(p.delegationsRaw); problem != "" {
		return fmt.Errorf("voting proposal %q field %q %s", p.ID, "delegations", problem)
	}
	// 数组写出但元素形状与委托记录不符（元素不是对象、或 from/to 类型不对等）：
	// 解码错误被延后到这里统一报告，原因同样指出提案编号与 delegations，并
	// 归为类型不符；这类数组不能被当成“无人委托”的空列表接受。
	if p.delegationsShapeErr != nil {
		return fmt.Errorf("voting proposal %q field %q has wrong type: %v", p.ID, "delegations", p.delegationsShapeErr)
	}
	return nil
}

// validateVoteProposalBallots 严格判定一项已保存投票提案的 ballots：
// 字段必须明确写出且为 JSON 数组。字段缺失、显式为 null 或写成对象/字符串/
// 数字/布尔等其它类型都返回带提案编号与字段名的错误，由 validateState 判
// 整份状态文件损坏——ballots 是代表选择与首次投票时间的唯一留痕，缺损列表
// 不能被折叠成空切片后按“无人投票”处理：尚在投票的提案会展示空票据、已投
// 代表还能再次投票，到期计票则按零参与权重给出拒绝结论，原有选择与首次
// 投票时间就此丢失。此要求不取决于提案状态：voting/passed/rejected/
// executed 适用同一条要求，不能因为计票汇总恰好为零或资金余额仍能与执行
// 凭据对应就把缺损列表补成空数组。数组写出但元素形状与票据记录不符（元素
// 不是对象等）同样拒绝。明确写出的空数组是合法的“确实无人投票”列表，
// 不与缺失混同；空数组合法只说明列表没有缺损，不放宽状态与计票一致性。
func validateVoteProposalBallots(p *storedVoteProposal) error {
	if problem := requiredArrayProblem(p.ballotsRaw); problem != "" {
		return fmt.Errorf("voting proposal %q field %q %s", p.ID, "ballots", problem)
	}
	if p.ballotsShapeErr != nil {
		return fmt.Errorf("voting proposal %q field %q has wrong type: %v", p.ID, "ballots", p.ballotsShapeErr)
	}
	return nil
}

// validateVoteProposalStartAt 严格判定一项已保存投票提案的 start_at：
// 字段必须明确写出、非 null 且为 int64 范围内的 JSON 整数字面量。字段未写出、
// 显式为 null 或写成字符串/布尔/对象/数组/小数/指数形式/超出范围都返回带
// 提案编号与字段名的错误，由 validateState 判整份状态文件损坏——开始时间
// 决定投票窗口何时开启，缺失字段被折叠成 0 会把窗口向前扩大，让开始之前
// 投出的票被当成窗口内的首次投票接受；取值不得从票据时间、截止时间、时间
// 锁或其它记录推算补齐，也不能补零后继续使用。即使票据、计票结论与执行
// 凭据都能与其余内容核对一致也不宽免：voting/passed/rejected/executed 适用
// 同一条要求。明确写出的 0 是合法开始时间，不与缺失混同；合法整数仍须
// 满足既有的 0 <= start < deadline <= timelock 规则（由 specOf 复核）。
// “缺失/空值/类型不符”的判定与逐票明细、计票结果、执行凭据共用同一份规则
// （requiredScalarProblem），此处只补充投票提案的业务位置：提案编号。
func validateVoteProposalStartAt(p *storedVoteProposal) error {
	if problem := requiredScalarProblem(p.startAtRaw, scalarInteger); problem != "" {
		return fmt.Errorf("voting proposal %q field %q %s", p.ID, "start_at", problem)
	}
	return nil
}

// validateVoteProposalsTable 严格判定顶层 vote_proposals 整张表的形状，必须先
// 于表内任何提案（validateVoteProposals）执行：
//   - 字段未写出：只对真正没有 vote_proposals 字段的旧版本状态文件兼容，按
//     空表处理。旧文件中其余内容（资金余额、登记提案、执行凭据）仍必须各自
//     合法，且也能在空表下正常创建第一项投票提案；
//   - 明确写出空对象 {}：表示确实没有投票提案，同样合法，不与缺失混同；
//   - 显式 null：状态损坏。null 不能折叠成空表——原来尚在投票、已通过未执行
//     或被拒绝的提案一旦随整表被替换成 null，折叠成空表会让查询成功却报告
//     提案不存在，随后创建还可能重新占用原编号，而其余余额与执行凭据仍能
//     核对一致；
//   - 写成数组、字符串、数字、布尔：同样判状态损坏，不能当成空表。
//
// 这条规则不因资金库是否为零、有没有执行凭据或登记提案是否完整而放宽，也
// 不从其它记录推测原来有哪些投票提案。兼容旧文件只允许“没有写出整张表”，
// 不能借此放过表内提案已要求明确保存的字段：对象一旦给出，其中任何损坏仍
// 由 validateVoteProposals 沿用整份拒绝的行为处理。
func validateVoteProposalsTable(state *storedState) error {
	const field = "vote_proposals"
	switch problem := requiredObjectProblem(state.voteProposalsRaw); problem {
	case "":
		// 字段是 JSON 对象（含 {}）：映射已在解码时填充，表内校验随后执行。
		if state.VoteProposals == nil {
			state.VoteProposals = map[string]*storedVoteProposal{}
		}
	case "is missing":
		// 仅旧版本状态文件可缺整张表：按空表兼容，登记提案、余额与执行凭据
		// 照常读出，也能在其上创建第一项投票提案（提交时会明确写出该字段）。
		// 同步把原始片段规范化为明确写出的空对象 {}：本进程随后提交时，
		// commitLocked 的提交前校验与“明确写出空表”的读取校验走同一判定，
		// 序列化本身始终输出 VoteProposals 映射，缺字段只在读取旧文件时出现。
		state.VoteProposals = map[string]*storedVoteProposal{}
		state.voteProposalsRaw = json.RawMessage("{}")
	default:
		return fmt.Errorf("field %q %s", field, problem)
	}
	return nil
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
		// delegations 决定委托路径、最终代表与归集票重，必须明确写出且为 JSON
		// 数组。先于 specOf 的派生核对执行：字段缺损时直接判整份文件损坏，
		// 绝不能把缺失记录折叠成空委托列表后再按“无人委托”继续查询、投票或
		// 复核计票——未投票、已计票与已执行的提案适用同一条要求。
		if err := validateVoteProposalDelegations(p); err != nil {
			return err
		}
		// ballots 是代表选择与首次投票时间的唯一留痕，必须明确写出且为 JSON
		// 数组。先于逐票字段核对、代表资格与票重重放执行：字段缺损时直接判
		// 整份文件损坏，绝不能把缺失列表折叠成空票据后再按“无人投票”继续
		// 查询、接受重复投票或复核出零参与权重的拒绝结论。voting/passed/
		// rejected/executed 适用同一条要求。
		if err := validateVoteProposalBallots(p); err != nil {
			return err
		}
		// start_at 决定投票窗口何时开启，必须明确写出且为 int64 整数。先于
		// specOf 的时间规则派生核对执行：字段缺损时直接判整份文件损坏，绝不
		// 能把缺失的开始时间折叠成 0 后扩大投票窗口（开始之前投出的票会被
		// 当成窗口内的首次投票），也不能从票据时间、截止时间、时间锁或其它
		// 记录推算补齐。voting/passed/rejected/executed 适用同一条要求；
		// 明确写出的 0 是合法开始时间。
		if err := validateVoteProposalStartAt(p); err != nil {
			return err
		}
		spec, err := specOf(p)
		if err != nil {
			return err
		}

		// 每张已保存票据都必须明确写出 representative/weight/support/voted_at
		// 且非 null、JSON 类型正确。先于代表资格与票重核对执行：字段缺损时直接判
		// 整份文件损坏，绝不能把缺失的 support 默认成反对票、把缺失的 voted_at
		// 默认成 0 后再继续给出查询或计票的部分结果。
		for i := range p.Ballots {
			if err := validateStoredBallot(p.ID, i, &p.Ballots[i]); err != nil {
				return err
			}
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
			// 只要提案有计票结果，for_weight 与 against_weight 就必须各自明确
			// 写出且为 int64 整数。先于权重重放核对执行：字段缺损时直接判整份
			// 文件损坏，绝不能把缺失的一侧默认补成 0（恰好等于该侧真实零票重）
			// 后再继续给出查询或再次计票的结果。明确写出的 0 是合法权重。
			if err := validateStoredTally(p.ID, p.Tally); err != nil {
				return err
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
