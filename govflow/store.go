package govflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// 资金库状态文件相关错误。
var (
	// ErrTreasuryAlreadyInit：初始化目标路径已存在文件，新文件才能初始化。
	ErrTreasuryAlreadyInit = errors.New("treasury init refused: state file already exists")
	// ErrStateCorrupt：状态文件损坏或不是受支持的资金库状态文件。
	ErrStateCorrupt = errors.New("treasury state file is corrupt")
	// ErrProposalNotFound：按编号查询或执行时提案不存在。
	ErrProposalNotFound = errors.New("proposal not found")
	// ErrProposalConflict：同编号提案再次登记但时间锁或动作原文与首次不同。
	ErrProposalConflict = errors.New("proposal already registered with different timelock or actions")
	// ErrInvalidRegistration：登记参数非法（编号为空、初始余额越界等）。
	ErrInvalidRegistration = errors.New("invalid registration")
)

const (
	stateMagic   = "govflow-treasury-state"
	stateVersion = 1
	lockSuffix   = ".lock"
)

// BalanceUpdate 记录一个账户在一次动作前后的余额。
//
// before 与 after 必须各自明确写出且为 int64 整数：字段缺失、为 null 或
// 类型不符都不得被折叠成 0 再当作合法凭据读出——某个余额本来应为 0 时，
// 缺失字段与明确写出的 0 在解码后必须仍可区分。因此解码时保留字段的原始
// JSON，由 validateReceiptActionBalances 区分“缺失/空值/类型不符”。
type BalanceUpdate struct {
	Account string `json:"account"`
	Before  int64  `json:"before"`
	After   int64  `json:"after"`

	beforeRaw json.RawMessage
	afterRaw  json.RawMessage
}

// balanceUpdateJSONShape 只用于解码，按保存格式的字段名逐字接住每个字段的原始 JSON。
type balanceUpdateJSONShape struct {
	Account string          `json:"account"`
	Before  json.RawMessage `json:"before"`
	After   json.RawMessage `json:"after"`
}

// UnmarshalJSON 保留 before/after 是否出现及其原始写法（缺失为 nil、null 为
// "null"），使“字段缺失”与“显式写出 0”在解码后仍可区分：encoding/json 直接
// 解进 int64 会把缺失/null/类型不符都折叠成 0。判定统一交给
// validateReceiptActionBalances，此处只在字段确实是 int64 整数时填充值。
func (u *BalanceUpdate) UnmarshalJSON(data []byte) error {
	var shape balanceUpdateJSONShape
	if err := json.Unmarshal(data, &shape); err != nil {
		return err
	}
	u.Account = shape.Account
	u.beforeRaw, u.afterRaw = shape.Before, shape.After
	fillInt64(shape.Before, &u.Before)
	fillInt64(shape.After, &u.After)
	return nil
}

// mustBalanceUpdate 构造一份内存中的合法余额记录（首次执行构造凭据使用）。
// 同步填上字段原始片段，使“提交前校验”与“打开重放校验”走同一份
// validateReceiptActionBalances 判定时不会把本进程新建的凭据误判为字段缺失。
func mustBalanceUpdate(account string, before, after int64) BalanceUpdate {
	beforeRaw, _ := json.Marshal(before)
	afterRaw, _ := json.Marshal(after)
	return BalanceUpdate{
		Account: account, Before: before, After: after,
		beforeRaw: beforeRaw, afterRaw: afterRaw,
	}
}

// ActionReceipt 是凭据中对单项动作的留痕：动作原文、在提案中的顺序编号（0 起），
// 以及该动作引发的资金库与收款账户余额变动。两侧的 before/after 四个余额字段
// 都必须明确写出；任何一个缺失、为 null 或类型不符，整份状态文件即判损坏，
// 不能根据转账金额、账户余额或其它凭据补出（见 validateReceiptActionBalances）。
//
// index 同样必须明确写出：它是执行留痕的一部分，记录恰好排在 actions 第一项
// 并不能补出一个合法的 0。字段缺失、为 null、不是 int64 范围内的非负整数
// （字符串/布尔/小数/指数/超界/负数）或与该动作在凭据中的实际位置不符，都判
// 整份状态损坏（见 validateReceiptActionIndex）。因此解码时保留 index 的原始
// JSON，与余额字段走同一套“缺失/空值/类型不符”判定。
type ActionReceipt struct {
	Index     int64         `json:"index"`
	Action    string        `json:"action"`
	Treasury  BalanceUpdate `json:"treasury"`
	Recipient BalanceUpdate `json:"recipient"`

	indexRaw json.RawMessage
}

// actionReceiptJSONShape 只用于解码，按保存格式的字段名逐字接住每个字段的原始 JSON。
type actionReceiptJSONShape struct {
	Index     json.RawMessage `json:"index"`
	Action    string          `json:"action"`
	Treasury  BalanceUpdate   `json:"treasury"`
	Recipient BalanceUpdate   `json:"recipient"`
}

// UnmarshalJSON 保留 index 是否出现及其原始写法（缺失为 nil、null 为 "null"），
// 使“字段缺失”与“显式写出 0”在解码后仍可区分：encoding/json 直接解进 int64
// 会把缺失/null/类型不符都折叠成 0，第一项动作缺失 index 时就会被误当成合法
// 编号 0。判定统一交给 validateReceiptActionIndex，此处只在字段确实是 int64
// 整数时填充值。
func (a *ActionReceipt) UnmarshalJSON(data []byte) error {
	var shape actionReceiptJSONShape
	if err := json.Unmarshal(data, &shape); err != nil {
		return err
	}
	a.Action = shape.Action
	a.Treasury = shape.Treasury
	a.Recipient = shape.Recipient
	a.indexRaw = shape.Index
	fillInt64(shape.Index, &a.Index)
	return nil
}

// Receipt 是一项提案首次成功执行后生成的执行凭据。
// 一项提案只允许成功一次；重试返回同一份凭据。
//
// executed_at 必须明确写出且不得早于对应提案的 timelock_end：首次执行只在
// now 达到时间锁后发生，保存的凭据必须满足同一条执行资格规则。字段缺失、
// 为 null 或类型不符都不得被折叠成 0 再当作合法凭据读出，因此解码时保留
// 字段的原始 JSON，由 validateState 区分“缺失/空值/类型不符/早于时间锁”。
type Receipt struct {
	ProposalID string          `json:"proposal_id"`
	ExecutedAt int64           `json:"executed_at"` // 首次成功执行时调用方提供的时间
	Order      int64           `json:"order"`       // 成功提交的先后顺序，0 起
	Actions    []ActionReceipt `json:"actions"`

	executedAtRaw json.RawMessage
	orderRaw      json.RawMessage
}

// receiptJSONShape 只用于解码，按保存格式的字段名逐字接住每个字段的原始 JSON。
type receiptJSONShape struct {
	ProposalID string          `json:"proposal_id"`
	ExecutedAt json.RawMessage `json:"executed_at"`
	Order      json.RawMessage `json:"order"`
	Actions    []ActionReceipt `json:"actions"`
}

// UnmarshalJSON 保留 executed_at/order 是否出现及其原始写法（缺失为 nil、null 为
// "null"），使“字段缺失”与“显式写出 0”在解码后仍可区分：encoding/json 直接
// 解进 int64 会把缺失/null/类型不符都折叠成 0，第一份凭据缺失 order 时就会被
// 误当成合法编号 0。判定统一交给 validateState，此处只在字段确实是 int64
// 整数时填充 ExecutedAt/Order。
func (r *Receipt) UnmarshalJSON(data []byte) error {
	var shape receiptJSONShape
	if err := json.Unmarshal(data, &shape); err != nil {
		return err
	}
	r.ProposalID = shape.ProposalID
	r.Actions = shape.Actions
	r.executedAtRaw = shape.ExecutedAt
	r.orderRaw = shape.Order
	fillInt64(shape.ExecutedAt, &r.ExecutedAt)
	fillInt64(shape.Order, &r.Order)
	return nil
}

// ProposalRecord 是已登记提案的对外视图。State 只可能是 passed 或 executed。
type ProposalRecord struct {
	ID          string   `json:"id"`
	State       string   `json:"state"`
	TimelockEnd int64    `json:"timelock_end"`
	Actions     []string `json:"actions"`
}

// ---- 磁盘结构（JSON） ----

// storedProposal 是 register 来源提案的保存形状。
//
// timelock_end 必须明确写出且为 int64 整数：时间锁决定资金何时可以支出，
// 字段缺失、为 null 或类型不符都不得被折叠成 0 再当作“时间锁已到期”读出——
// 某个提案的时间锁本来应为 0 时，缺失字段与明确写出的 0 在解码后必须仍可
// 区分。即使记录已是 executed 状态、执行凭据与余额能一致核对，也不能从
// 执行时间或其它记录推测补齐。因此解码时保留字段的原始 JSON，由
// validateState 区分“缺失/空值/类型不符”。
type storedProposal struct {
	ID          string   `json:"id"`
	State       string   `json:"state"`
	TimelockEnd int64    `json:"timelock_end"`
	Actions     []string `json:"actions"`

	timelockEndRaw json.RawMessage
}

// storedProposalJSONShape 只用于解码，按保存格式的字段名逐字接住每个字段的原始 JSON。
type storedProposalJSONShape struct {
	ID          string          `json:"id"`
	State       string          `json:"state"`
	TimelockEnd json.RawMessage `json:"timelock_end"`
	Actions     []string        `json:"actions"`
}

// UnmarshalJSON 保留 timelock_end 是否出现及其原始写法（缺失为 nil、null 为
// "null"），使“字段缺失”与“显式写出 0”在解码后仍可区分：encoding/json 直接
// 解进 int64 会把缺失/null/类型不符都折叠成 0，缺失时间锁的提案就会被误当成
// 时间锁为 0、任何 now 都已到期。判定统一交给 validateState，此处只在字段
// 确实是 int64 整数时填充值。
func (p *storedProposal) UnmarshalJSON(data []byte) error {
	var shape storedProposalJSONShape
	if err := json.Unmarshal(data, &shape); err != nil {
		return err
	}
	p.ID = shape.ID
	p.State = shape.State
	p.Actions = shape.Actions
	p.timelockEndRaw = shape.TimelockEnd
	fillInt64(shape.TimelockEnd, &p.TimelockEnd)
	return nil
}

type storedState struct {
	Magic           string                         `json:"magic"`
	Version         int                            `json:"version"`
	InitialTreasury int64                          `json:"initial_treasury"`
	Treasury        int64                          `json:"treasury"`
	Balances        map[string]int64               `json:"balances"`
	Proposals       map[string]*storedProposal     `json:"proposals"`
	Receipts        []*Receipt                     `json:"receipts"`
	VoteProposals   map[string]*storedVoteProposal `json:"vote_proposals"`
}

// Store 是绑定到单个本地状态文件的资金库句柄。
// 多个 goroutine 可并发使用同一个 Store；不同 Store/进程操作同一状态文件时，
// 通过进程内互斥锁与文件锁（flock）串行化。
type Store struct {
	path   string
	mu     sync.Mutex // 保护本句柄上的全部操作
	lockFD *os.File   // 跨进程文件锁
	closed bool
}

// 进程内按规范路径共享的互斥锁，保证同进程内多个 Store 句柄也互相串行。
var (
	pathLocksMu sync.Mutex
	pathLocks   = map[string]*sync.Mutex{}
)

// canonicalPath 将路径规范化为绝对路径，保证不同拼写（如 t.json 与 ./t.json）
// 命中同一把进程内锁与同一个锁文件。有意不解析符号链接：文件在初始化前后必须
// 解析到同一个键，硬链接/绑定挂载等更深层别名不在本地单文件场景的处理范围内。
func canonicalPath(path string) (string, error) {
	return filepath.Abs(path)
}

func inProcessLock(path string) *sync.Mutex {
	pathLocksMu.Lock()
	defer pathLocksMu.Unlock()
	m, ok := pathLocks[path]
	if !ok {
		m = &sync.Mutex{}
		pathLocks[path] = m
	}
	return m
}

// InitTreasury 创建一个全新的状态文件并写入初始资金库余额。
// 只有新文件才能初始化：路径已存在（含损坏文件）一律拒绝，不会重置或覆盖。
// treasury 必须落在有符号 64 位整数范围内且不得为负。
func InitTreasury(path string, treasury int64) (*Store, error) {
	if treasury < 0 {
		return nil, fmt.Errorf("%w: initial treasury balance must not be negative", ErrInvalidRegistration)
	}
	path, err := canonicalPath(path)
	if err != nil {
		return nil, err
	}
	pathMu := inProcessLock(path)
	pathMu.Lock()
	defer pathMu.Unlock()

	// 先打开/创建锁文件并取跨进程锁，再创建状态文件，消除“空文件已出现、
	// 尚未写入”期间被其它进程 Open 判为损坏的窗口。
	lockFD, err := os.OpenFile(path+lockSuffix, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lockFD.Fd()), syscall.LOCK_EX); err != nil {
		_ = lockFD.Close()
		return nil, err
	}
	// O_EXCL 保证与并发初始化竞争时只有一方成功。
	fd, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		_ = syscall.Flock(int(lockFD.Fd()), syscall.LOCK_UN)
		_ = lockFD.Close()
		if errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("%w: %s", ErrTreasuryAlreadyInit, path)
		}
		return nil, err
	}
	_ = fd.Close()

	store := &Store{path: path, lockFD: lockFD}
	if err := store.commitLocked(newStoredState(treasury)); err != nil {
		_ = store.funlock()
		_ = lockFD.Close()
		_ = os.Remove(path)
		return nil, err
	}
	// 初始化完成后立即释放跨进程锁；后续每次操作各自短持锁。
	if err := store.funlock(); err != nil {
		_ = lockFD.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return store, nil
}

// Open 打开已有的资金库状态文件。文件损坏时明确拒绝，不会重建或覆盖原文件。
func Open(path string) (*Store, error) {
	path, err := canonicalPath(path)
	if err != nil {
		return nil, err
	}
	pathMu := inProcessLock(path)
	pathMu.Lock()
	defer pathMu.Unlock()
	return openStoreHoldingPathLock(path)
}

// openStoreHoldingPathLock 的调用方必须持有该路径的进程内互斥锁。
func openStoreHoldingPathLock(path string) (*Store, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	lockFD, err := os.OpenFile(path+lockSuffix, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lockFD.Fd()), syscall.LOCK_EX); err != nil {
		_ = lockFD.Close()
		return nil, err
	}
	store := &Store{path: path, lockFD: lockFD}
	if _, err := store.loadLocked(); err != nil {
		_ = store.funlock()
		_ = lockFD.Close()
		return nil, err
	}
	// 打开时已做严格校验；锁随即释放，每次操作再各自短持锁。
	if err := store.funlock(); err != nil {
		_ = lockFD.Close()
		return nil, err
	}
	return store, nil
}

// Path 返回状态文件路径。
func (s *Store) Path() string { return s.path }

// Close 释放文件锁。关闭后其它操作返回错误。
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if err := s.funlock(); err != nil {
		_ = s.lockFD.Close()
		return err
	}
	return s.lockFD.Close()
}

func (s *Store) flock(how int) error {
	if s.lockFD == nil {
		return errors.New("treasury store is closed")
	}
	return syscall.Flock(int(s.lockFD.Fd()), how)
}

func (s *Store) funlock() error {
	if s.lockFD == nil {
		return nil
	}
	return syscall.Flock(int(s.lockFD.Fd()), syscall.LOCK_UN)
}

// begin 串行化一次读改写操作：先取进程内路径锁，再取跨进程排他文件锁。
// 返回的 commit 在操作结束时释放锁。
func (s *Store) begin() (func(), error) {
	pathMu := inProcessLock(s.path)
	pathMu.Lock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		pathMu.Unlock()
		return nil, errors.New("treasury store is closed")
	}
	if err := s.flock(syscall.LOCK_EX); err != nil {
		s.mu.Unlock()
		pathMu.Unlock()
		return nil, err
	}
	return func() {
		_ = s.funlock()
		s.mu.Unlock()
		pathMu.Unlock()
	}, nil
}

func newStoredState(treasury int64) *storedState {
	return &storedState{
		Magic:           stateMagic,
		Version:         stateVersion,
		InitialTreasury: treasury,
		Treasury:        treasury,
		Balances:        map[string]int64{},
		Proposals:       map[string]*storedProposal{},
		Receipts:        []*Receipt{},
		VoteProposals:   map[string]*storedVoteProposal{},
	}
}

// ---- 登记 ----

// Register 登记一项已通过（passed）的提案，保留时间锁与动作原文。
// 编号必须非空；动作列表可为空（执行时会因动作为空被拒绝），动作原文不做改写。
// 同编号且时间锁、动作原文与首次登记完全相同的重试返回 existed=true；
// 内容不同则返回 ErrProposalConflict。
// 需要这次登记实际认可的完整记录（含已执行提案的实际状态）时使用
// RegisterProposal；两者走同一条登记路径，重试标志含义一致。
//
// 提案编号与每一项动作原文还必须是合法 UTF-8：状态文件以 JSON 保存，
// encoding/json 会把字符串中的非法 UTF-8 字节悄悄改写成替换字符 U+FFFD
// 且不报错。登记入口若放行，落盘的编号/动作原文就不再是用户提交的原文——
// 原编号无法查询，收款账户名称与原文不同。因此这两类文本在一切落盘之前
// 逐字校验，非法输入整项登记失败（ErrInvalidRegistration），不新增提案、
// 不保存部分动作；错误说明是编号还是动作原文，动作出错时指出它在提交
// 列表中的位置（0 起）。即使非法输入改写后恰好等于状态中已存在的合法
// 编号（如含真实 U+FFFD 的编号），也不被当作该编号的成功重试或内容冲突。
// 合法文本（含中文、表情、用户明确写出的 U+FFFD，以及 "\uD800" 这类
// 由反斜杠与普通字母组成的字面文本——登记参数不是 JSON 字符串，不做
// 转义重解释）原样保留。编码合法但转账格式错误的动作照常登记，
// 由执行操作按现有规则拒绝。
func (s *Store) Register(id string, timelockEnd int64, actions []string) (existed bool, err error) {
	_, existed, err = s.RegisterProposal(id, timelockEnd, actions)
	return existed, err
}

// RegisterProposal 与 Register 执行同一登记流程，并额外返回这次登记实际认可的
// 完整记录，使调用方能区分三种成功结果：
//   - 首次登记：existed=false，记录状态为 passed；
//   - 相同内容重试且提案尚未执行：existed=true，记录状态为 passed；
//   - 相同内容重试且提案已执行：existed=true，记录状态为 executed。
//
// 返回记录的编号、时间锁、动作原文及动作顺序就是这次登记认可的那一条记录：
// 相同内容重试只确认已有提案，不改写状态、余额或首次执行凭据。若登记重试与
// 首次执行同时发生，两者由文件锁串行化，响应按实际确认的先后反映状态——
// 登记先确认时返回 passed，执行先完成时返回 executed；记录是本次确认时的
// 副本，随后执行完成不会改写已返回的结果。
func (s *Store) RegisterProposal(id string, timelockEnd int64, actions []string) (ProposalRecord, bool, error) {
	if id == "" {
		return ProposalRecord{}, false, fmt.Errorf("%w: proposal id must not be empty", ErrInvalidRegistration)
	}
	if problem := invalidUTF8(id); problem != "" {
		return ProposalRecord{}, false, fmt.Errorf("%w: proposal id is not valid UTF-8: %s", ErrInvalidRegistration, problem)
	}
	for i, action := range actions {
		if problem := invalidUTF8(action); problem != "" {
			return ProposalRecord{}, false, fmt.Errorf("%w: action %d text is not valid UTF-8: %s", ErrInvalidRegistration, i, problem)
		}
	}
	commit, err := s.begin()
	if err != nil {
		return ProposalRecord{}, false, err
	}
	defer commit()

	state, err := s.loadLocked()
	if err != nil {
		return ProposalRecord{}, false, err
	}
	stored := copyActions(actions)
	if existing, ok := state.Proposals[id]; ok {
		if existing.TimelockEnd == timelockEnd && sameActions(existing.Actions, stored) {
			// 相同内容重试：只确认已有记录，按本次持锁读取时的实际状态
			// （passed 或 executed）返回副本；不提交、不改写任何状态。
			return toProposalRecord(existing), true, nil
		}
		return ProposalRecord{}, false, fmt.Errorf("%w: proposal %s", ErrProposalConflict, id)
	}
	// 两种来源共用编号：register 不能覆盖投票提案或绕过其投票结论。
	if vp, ok := state.VoteProposals[id]; ok {
		return ProposalRecord{}, false, fmt.Errorf("%w: proposal id %s belongs to a voting proposal (state=%s)", ErrProposalConflict, id, vp.State)
	}
	fresh := &storedProposal{
		ID:          id,
		State:       "passed",
		TimelockEnd: timelockEnd,
		Actions:     stored,
	}
	// 同步填上字段原始片段，使“提交前校验”与“打开重放校验”走同一份
	// timelock_end 判定时不会把本进程新建的提案误判为字段缺失。
	fresh.timelockEndRaw, _ = json.Marshal(timelockEnd)
	state.Proposals[id] = fresh
	if err := s.commitLocked(state); err != nil {
		return ProposalRecord{}, false, err
	}
	return toProposalRecord(fresh), false, nil
}

func copyActions(actions []string) []string {
	if actions == nil {
		return nil
	}
	copied := make([]string, len(actions))
	copy(copied, actions)
	return copied
}

// sameActions 将 nil 与空切片视为等价，其余按顺序逐字比较。
func sameActions(a, b []string) bool {
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

// ---- 执行 ----

// executionTarget 是 register 与投票两种来源提案在执行路径上的统一视图。
type executionTarget struct {
	state        string
	timelockEnd  int64
	actions      []string
	markExecuted func()
}

// resolveExecutable 在两种提案表中按编号定位执行目标；不存在返回 nil。
func resolveExecutable(state *storedState, id string) *executionTarget {
	if p, ok := state.Proposals[id]; ok {
		return &executionTarget{
			state:        p.State,
			timelockEnd:  p.TimelockEnd,
			actions:      p.Actions,
			markExecuted: func() { p.State = "executed" },
		}
	}
	if vp, ok := state.VoteProposals[id]; ok {
		return &executionTarget{
			state:        vp.State,
			timelockEnd:  vp.TimelockEnd,
			actions:      vp.Actions,
			markExecuted: func() { vp.State = "executed" },
		}
	}
	return nil
}

// proposalOwners 将两个来源的提案编号映射到状态、时间锁与动作，供凭据重放交叉校验。
func proposalOwnerState(state *storedState, id string) (string, int64, []string, bool) {
	if p, ok := state.Proposals[id]; ok {
		return p.State, p.TimelockEnd, p.Actions, true
	}
	if vp, ok := state.VoteProposals[id]; ok {
		return vp.State, vp.TimelockEnd, vp.Actions, true
	}
	return "", 0, nil, false
}

// Execute 按编号执行一项提案。调用方通过 now 提供当前时间。
// 只有 passed 状态、now 已达到时间锁且动作非空的提案可以首次执行；
// 成功后状态变为 executed 并返回凭据。资格不符、动作格式错误、余额不足、
// 金额计算溢出都返回明确原因，整项提案不产生部分转账，也不消耗执行机会。
// 已完成提案再次执行（即使 now 不同）直接返回首次成功凭据。
func (s *Store) Execute(id string, now int64) (*Receipt, error) {
	commit, err := s.begin()
	if err != nil {
		return nil, err
	}
	defer commit()

	state, err := s.loadLocked()
	if err != nil {
		return nil, err
	}
	target := resolveExecutable(state, id)
	if target == nil {
		return nil, fmt.Errorf("%w: %s", ErrProposalNotFound, id)
	}
	if target.state == "executed" {
		if rcpt := findReceipt(state.Receipts, id); rcpt != nil {
			return cloneReceipt(rcpt), nil
		}
		return nil, fmt.Errorf("%w: executed proposal %s has no receipt", ErrStateCorrupt, id)
	}
	if target.state != "passed" {
		return nil, execReject(fmt.Sprintf("proposal %s is not passed (state=%s)", id, target.state))
	}
	if now < target.timelockEnd {
		return nil, execReject(fmt.Sprintf("timelock not reached for %s: now=%d timelock_end=%d", id, now, target.timelockEnd))
	}
	if len(target.actions) == 0 {
		return nil, execReject(fmt.Sprintf("proposal %s has no actions", id))
	}

	// 第一阶段：在副本上按统一转账规则（planTransfers）校验并预演全部
	// 动作。任何一步失败都直接返回，副本之外的状态不落盘。
	steps, simTreasury, simBalances, perr := planTransfers(target.actions, state.Treasury, state.Balances)
	if perr != nil {
		return nil, perr
	}

	// 第二阶段：全部预演成功后构造凭据并一次性原子提交。
	receipt := &Receipt{
		ProposalID: id,
		ExecutedAt: now,
		Order:      int64(len(state.Receipts)),
		Actions:    make([]ActionReceipt, 0, len(steps)),
	}
	// 同步填上字段原始片段，使“提交前校验”与“打开重放校验”走同一份
	// executed_at/order 判定时不会把本进程新建的凭据误判为字段缺失。
	receipt.executedAtRaw, _ = json.Marshal(now)
	receipt.orderRaw, _ = json.Marshal(receipt.Order)
	for i, st := range steps {
		receipt.Actions = append(receipt.Actions, st.actionReceipt(i, target.actions[i]))
	}
	state.Treasury = simTreasury
	state.Balances = simBalances
	target.markExecuted()
	state.Receipts = append(state.Receipts, receipt)

	if err := s.commitLocked(state); err != nil {
		return nil, err
	}
	return cloneReceipt(receipt), nil
}

func findReceipt(receipts []*Receipt, id string) *Receipt {
	for _, r := range receipts {
		if r.ProposalID == id {
			return r
		}
	}
	return nil
}

func cloneReceipt(r *Receipt) *Receipt {
	if r == nil {
		return nil
	}
	copied := *r
	if r.Actions != nil {
		copied.Actions = make([]ActionReceipt, len(r.Actions))
		copy(copied.Actions, r.Actions)
	}
	return &copied
}

// ---- 查询 ----

// BalanceSnapshot 是同一份完整已提交状态下的资金库余额与全部收款账户余额。
// 一次 BalanceSnapshot 调用只读取一次状态文件，保证返回的字段彼此一致：
// 不会把不同提交时刻的余额拼在一起。
type BalanceSnapshot struct {
	Treasury int64            `json:"treasury"`
	Balances map[string]int64 `json:"balances"`
}

// BalanceSnapshot 在一次持锁读取中取得资金库与全部收款账户余额。
// 返回的 Balances 是副本（永不为 nil），调用方可自由使用；
// 后续执行不会改变已返回的快照，下一次调用重新读取当前已提交状态。
func (s *Store) BalanceSnapshot() (*BalanceSnapshot, error) {
	state, err := s.readState()
	if err != nil {
		return nil, err
	}
	out := &BalanceSnapshot{
		Treasury: state.Treasury,
		Balances: make(map[string]int64, len(state.Balances)),
	}
	for k, v := range state.Balances {
		out.Balances[k] = v
	}
	return out, nil
}

// TreasuryBalance 返回资金库当前余额。
func (s *Store) TreasuryBalance() (int64, error) {
	state, err := s.readState()
	if err != nil {
		return 0, err
	}
	return state.Treasury, nil
}

// Balance 返回收款账户余额；从未出现过的账户余额为零。
func (s *Store) Balance(account string) (int64, error) {
	state, err := s.readState()
	if err != nil {
		return 0, err
	}
	return state.Balances[account], nil
}

// Balances 返回全部出现过的收款账户余额的副本，按账户名排序。
func (s *Store) Balances() (map[string]int64, error) {
	state, err := s.readState()
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(state.Balances))
	for k, v := range state.Balances {
		out[k] = v
	}
	return out, nil
}

// Proposal 按编号返回提案登记信息；不存在时 ok 为 false。
func (s *Store) Proposal(id string) (ProposalRecord, bool, error) {
	state, err := s.readState()
	if err != nil {
		return ProposalRecord{}, false, err
	}
	p, ok := state.Proposals[id]
	if !ok {
		return ProposalRecord{}, false, nil
	}
	return toProposalRecord(p), true, nil
}

// Proposals 返回全部已登记提案，按编号排序。
func (s *Store) Proposals() ([]ProposalRecord, error) {
	state, err := s.readState()
	if err != nil {
		return nil, err
	}
	return proposalRecords(state), nil
}

func proposalRecords(state *storedState) []ProposalRecord {
	out := make([]ProposalRecord, 0, len(state.Proposals))
	for _, p := range state.Proposals {
		out = append(out, toProposalRecord(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ProposalsSnapshot 是同一份完整已提交状态下的登记提案与投票提案列表。
// 一次 ProposalsSnapshot 调用只读取一次状态文件，保证两个来源的提案状态、
// 动作与投票明细彼此一致：不会把不同提交时刻的提案状态拼在一起。
type ProposalsSnapshot struct {
	Registered []ProposalRecord    `json:"registered"`
	Voting     []*VoteProposalView `json:"voting"`
}

// ProposalsSnapshot 在一次持锁读取中取得全部登记提案与全部投票提案。
// Registered 按编号排序，Voting 按编号排序；返回的切片与记录均为副本，
// 后续执行不会改变已返回的快照，下一次调用重新读取当前已提交状态。
func (s *Store) ProposalsSnapshot() (*ProposalsSnapshot, error) {
	state, err := s.readState()
	if err != nil {
		return nil, err
	}
	return &ProposalsSnapshot{
		Registered: proposalRecords(state),
		Voting:     voteProposalViews(state),
	}, nil
}

func toProposalRecord(p *storedProposal) ProposalRecord {
	return ProposalRecord{
		ID:          p.ID,
		State:       p.State,
		TimelockEnd: p.TimelockEnd,
		Actions:     copyActions(p.Actions),
	}
}

// Receipt 按编号返回执行凭据；尚未成功执行或不存在时 ok 为 false。
func (s *Store) Receipt(id string) (*Receipt, bool, error) {
	state, err := s.readState()
	if err != nil {
		return nil, false, err
	}
	r := findReceipt(state.Receipts, id)
	if r == nil {
		return nil, false, nil
	}
	return cloneReceipt(r), true, nil
}

// Receipts 按成功提交的先后顺序返回全部执行凭据的副本。
func (s *Store) Receipts() ([]*Receipt, error) {
	state, err := s.readState()
	if err != nil {
		return nil, err
	}
	out := make([]*Receipt, 0, len(state.Receipts))
	for _, r := range state.Receipts {
		out = append(out, cloneReceipt(r))
	}
	return out, nil
}

func (s *Store) readState() (*storedState, error) {
	commit, err := s.begin()
	if err != nil {
		return nil, err
	}
	defer commit()
	return s.loadLocked()
}

// ---- 持久化 ----

// loadLocked 读取、严格解析并重放校验状态文件。调用方必须持锁。
func (s *Store) loadLocked() (*storedState, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("%w: %s: file is empty", ErrStateCorrupt, s.path)
	}
	// 先于结构扫描与解码逐字节校验字符串：非法 UTF-8 与未配对代理项转义
	// 会被 encoding/json 悄悄改写成 U+FFFD 且不报错，必须在此明确拒绝，
	// 使被改写的编号/账户名/动作原文不可能参与后续任何匹配与重放。
	if err := checkStrictStrings(raw); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrStateCorrupt, s.path, err)
	}
	if err := checkStateStructure(raw); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrStateCorrupt, s.path, err)
	}
	var state storedState
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&state); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrStateCorrupt, s.path, err)
	}
	// 拒绝单个合法 JSON 之后再拼接的额外内容（如 "{} {}"）。
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("unexpected trailing content after state document: %s", strings.TrimSpace(string(extra)))
		}
		return nil, fmt.Errorf("%w: %s: %v", ErrStateCorrupt, s.path, err)
	}
	if err := validateState(&state); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrStateCorrupt, s.path, err)
	}
	return &state, nil
}

// commitLocked 序列化状态并以“临时文件 + fsync + 原子改名 + 目录 fsync”提交。
// 任一步失败都保留上次完整状态。调用方必须持锁。
func (s *Store) commitLocked(state *storedState) error {
	if err := validateState(state); err != nil {
		return fmt.Errorf("refusing to persist invalid state: %w", err)
	}
	payload, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')

	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".govflow-state-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		cleanup()
		return err
	}
	if err := syncDir(dir); err != nil {
		return err
	}
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// validateState 做结构校验并重放全部凭据，确认余额与凭据完全一致。
func validateState(state *storedState) error {
	if state.Magic != stateMagic {
		return fmt.Errorf("bad magic %q", state.Magic)
	}
	if state.Version != stateVersion {
		return fmt.Errorf("unsupported state version %d", state.Version)
	}
	if state.InitialTreasury < 0 {
		return errors.New("initial treasury balance is negative")
	}
	if state.Treasury < 0 {
		return errors.New("treasury balance is negative")
	}
	if state.Balances == nil {
		return errors.New("balances table is missing")
	}
	if state.Proposals == nil {
		return errors.New("proposals table is missing")
	}
	if state.Receipts == nil {
		return errors.New("receipts table is missing")
	}
	// VoteProposals 缺省视为空表：旧版本写出的状态文件不含投票提案，继续可读。
	if state.VoteProposals == nil {
		state.VoteProposals = map[string]*storedVoteProposal{}
	}
	for account, balance := range state.Balances {
		if account == "" {
			return errors.New("balance entry with empty account")
		}
		if balance < 0 {
			return fmt.Errorf("balance of %q is negative", account)
		}
	}
	for key, p := range state.Proposals {
		if p == nil {
			return fmt.Errorf("proposal %q is empty", key)
		}
		if p.ID != key || p.ID == "" {
			return fmt.Errorf("proposal key %q does not match id %q", key, p.ID)
		}
		if p.State != "passed" && p.State != "executed" {
			return fmt.Errorf("proposal %q has unknown state %q", p.ID, p.State)
		}
		// timelock_end 决定资金何时可以支出，必须明确写出且为 int64 整数：
		// 字段缺失、为 null 或类型不符（字符串/小数/指数/超界）都判整份状态
		// 损坏，不能把缺失的时间锁折叠成 0 再当作已到期，也不能从执行时间或
		// 其它记录推测补齐——即使记录已是 executed 且凭据与余额核对一致。
		// 明确写出的 0 与负时间取值仍是合法时间锁，不与缺失混同。
		if err := validateProposalTimelock(p); err != nil {
			return err
		}
		// 动作原文登记时原样保存；空串或非法原文留待执行时拒绝。
	}
	if err := validateVoteProposals(state); err != nil {
		return err
	}

	// 重放凭据：从初始资金库余额出发，按成功顺序重放每笔转账，
	// 校验凭据内的前后余额、最终资金库与各账户余额，并检查状态一致性。
	simTreasury := state.InitialTreasury
	simBalances := map[string]int64{}
	seenReceipts := map[string]int{}
	for order, rcpt := range state.Receipts {
		if rcpt == nil {
			return fmt.Errorf("receipt %d is empty", order)
		}
		// order 是执行留痕中明确保存的成功提交序号（0 起），必须显式写出、
		// 为 int64 范围内的非负整数且恰好等于凭据在记录表中的位置：第一份
		// 凭据缺 order 也不能补成 0。缺失/空值/类型不符/位置不符都带提案
		// 编号与字段报损坏，不按数组位置重新编号。
		if err := validateReceiptOrder(order, rcpt); err != nil {
			return err
		}
		if prev, dup := seenReceipts[rcpt.ProposalID]; dup {
			return fmt.Errorf("duplicate receipt for %q at positions %d and %d", rcpt.ProposalID, prev, order)
		}
		seenReceipts[rcpt.ProposalID] = order
		ownerState, ownerTimelock, ownerActions, found := proposalOwnerState(state, rcpt.ProposalID)
		if !found {
			return fmt.Errorf("receipt %q references an unregistered proposal", rcpt.ProposalID)
		}
		if ownerState != "executed" {
			return fmt.Errorf("proposal %q has receipt but state is %q", rcpt.ProposalID, ownerState)
		}
		// 成功执行必须包含实际动作：执行路径以“没有动作”拒绝空动作提案，
		// 保存的凭据同样不得凭空动作列表构成成功执行记录。actions 字段缺失、
		// 为 null 或解出后为空都判整份状态损坏——不能因提案与凭据的动作数量
		// 相等（同为 0）、余额又能核对一致就认可这份成功记录。登记提案与投票
		// 提案共用这一判断，投票明细与通过结论合法也不能使空凭据有效。
		if len(rcpt.Actions) == 0 {
			return fmt.Errorf("receipt %d for proposal %q has no actions: an empty action list cannot record a successful execution",
				order, rcpt.ProposalID)
		}
		// 保存的凭据与首次执行受同一条执行资格规则约束：executed_at 必须明确
		// 写出且不得早于提案时间锁。登记提案与投票提案共用这一判断。
		if err := validateReceiptExecutedAt(order, rcpt, ownerTimelock); err != nil {
			return err
		}
		// 每项动作的 index 与四个余额字段（资金库/收款账户 × before/after）
		// 都必须明确写出：任何一个缺失、为 null 或类型不符都判整份状态损坏，
		// 不能按数组位置、转账金额、账户余额或其它凭据补出；明确写出的 0 是
		// 合法记录（第一项动作的 index 0 必须显式保存）。
		for i := range rcpt.Actions {
			if err := validateReceiptActionIndex(rcpt.ProposalID, i, &rcpt.Actions[i]); err != nil {
				return err
			}
			if err := validateReceiptActionBalances(rcpt.ProposalID, i, &rcpt.Actions[i]); err != nil {
				return err
			}
		}
		// 凭据声明的逐笔转账由统一转账规则（verifyReceiptTransfers）重放
		// 核对：动作条数/编号/原文、资金库与收款账户前后余额任一不符都带
		// 动作位置报损坏原因；本函数随即返回错误，整份状态被拒绝，重放结果
		// 不会被查询或落盘采纳。
		var verr error
		simTreasury, verr = verifyReceiptTransfers(rcpt, ownerActions, simTreasury, simBalances)
		if verr != nil {
			return verr
		}
	}
	if simTreasury != state.Treasury {
		return fmt.Errorf("treasury balance %d does not match receipt replay %d", state.Treasury, simTreasury)
	}
	// 余额表的账户集合必须与成功凭据涉及的收款账户完全一致：缺少收款账户、
	// 出现无凭据支持的账户（即使写成 0）或已记录余额不符都判整份状态损坏。
	// 不能只按账户数量或仅遍历文件自身的键：把一个已收款账户换成数量相同、
	// 余额写成 0 的陌生账户时，数量相等且缺失键在 map 中读到零值，会被误判
	// 为合法。账户名排序后报告，使错误信息确定且可读；余额表的排列顺序本身
	// 不影响判定。
	savedAccounts := make([]string, 0, len(state.Balances))
	for account := range state.Balances {
		savedAccounts = append(savedAccounts, account)
	}
	replayAccounts := make([]string, 0, len(simBalances))
	for account := range simBalances {
		replayAccounts = append(replayAccounts, account)
	}
	sort.Strings(savedAccounts)
	sort.Strings(replayAccounts)
	var missing, unexpected []string
	si, ri := 0, 0
	for si < len(savedAccounts) || ri < len(replayAccounts) {
		switch {
		case ri == len(replayAccounts) || (si < len(savedAccounts) && savedAccounts[si] < replayAccounts[ri]):
			unexpected = append(unexpected, savedAccounts[si])
			si++
		case si == len(savedAccounts) || savedAccounts[si] > replayAccounts[ri]:
			missing = append(missing, replayAccounts[ri])
			ri++
		default:
			si++
			ri++
		}
	}
	if len(missing) > 0 || len(unexpected) > 0 {
		problems := ""
		if len(missing) > 0 {
			problems = fmt.Sprintf("missing recipient account(s) %s with no balance record for funds actually received",
				strings.Join(quoteAccounts(missing), ", "))
		}
		if len(unexpected) > 0 {
			if problems != "" {
				problems += "; "
			}
			problems += fmt.Sprintf("account(s) %s have balance records but no successful receipt supports them (a zero balance does not make an unsupported account legal)",
				strings.Join(quoteAccounts(unexpected), ", "))
		}
		return fmt.Errorf("balance table is inconsistent with execution receipts: %s", problems)
	}
	for _, account := range replayAccounts {
		if balance := state.Balances[account]; balance != simBalances[account] {
			return fmt.Errorf("balance of %q is %d but receipt replay yields %d", account, balance, simBalances[account])
		}
	}
	// 已执行的提案必须有凭据；passed 提案不得有凭据（上面已部分保证）。
	for _, p := range state.Proposals {
		_, hasReceipt := seenReceipts[p.ID]
		if p.State == "executed" && !hasReceipt {
			return fmt.Errorf("proposal %q is executed without receipt", p.ID)
		}
	}
	for _, p := range state.VoteProposals {
		_, hasReceipt := seenReceipts[p.ID]
		if p.State == "executed" && !hasReceipt {
			return fmt.Errorf("voting proposal %q is executed without receipt", p.ID)
		}
	}
	return nil
}

// quoteAccounts 给账户名列表逐个加上 Go 风格引号，便于在损坏原因中逐字定位。
func quoteAccounts(accounts []string) []string {
	quoted := make([]string, len(accounts))
	for i, account := range accounts {
		quoted[i] = strconv.Quote(account)
	}
	return quoted
}

// validateProposalTimelock 严格判定一条已登记提案的 timelock_end：
// 字段未写出、显式为 null 或不是 int64 整数（字符串、布尔、小数、指数写法、
// 超界）都返回带提案编号与字段的错误。明确写出的 0 与负时间取值是合法时间锁，
// 不得当成缺失；登记入口不新增非负限制，这里同样不做。
// “缺失/空值/类型不符”的判定与逐票明细、计票结果、执行凭据共用同一份规则
// （requiredScalarProblem），此处只补充登记提案的业务位置：提案编号。
func validateProposalTimelock(p *storedProposal) error {
	if problem := requiredScalarProblem(p.timelockEndRaw, scalarInteger); problem != "" {
		return fmt.Errorf("proposal %q field %q %s", p.ID, "timelock_end", problem)
	}
	return nil
}

// validateReceiptExecutedAt 严格判定一份已保存凭据的 executed_at：
// 字段未写出、显式为 null 或不是 int64 整数都返回带定位的错误；
// 明确写出的值必须不早于所属提案的 timelock_end（恰好等于时间锁有效，
// 时间锁允许 0 时明确写出的 0 同样有效）。order 是凭据在凭据表中的
// 位置（0 起），与提案编号一起用于定位。
// “缺失/空值/类型不符”的判定与逐票明细、计票结果共用同一份规则
// （requiredScalarProblem），此处只补充凭据的业务位置与执行资格检查。
func validateReceiptExecutedAt(order int, rcpt *Receipt, timelockEnd int64) error {
	if problem := requiredScalarProblem(rcpt.executedAtRaw, scalarInteger); problem != "" {
		return fmt.Errorf("receipt %d for proposal %q field %q %s", order, rcpt.ProposalID, "executed_at", problem)
	}
	if rcpt.ExecutedAt < timelockEnd {
		return fmt.Errorf("receipt %d for proposal %q executed_at %d is before timelock_end %d",
			order, rcpt.ProposalID, rcpt.ExecutedAt, timelockEnd)
	}
	return nil
}

// validateReceiptOrder 严格判定一份已保存凭据的 order：它是执行留痕中明确
// 保存的成功提交序号（0 起），字段未写出、显式为 null 或不是 int64 整数都
// 返回带提案编号与字段的错误；类型合法但值与凭据在记录表中的实际位置不符
// （含负数）则返回位置不符的错误。第一份凭据恰好排在位置 0 也不能因此把
// 缺失字段补成 0——明确写出的 0 才是合法序号，缺失/null/写错类型一律拒绝。
// position 是凭据在凭据表中的位置（0 起）。
func validateReceiptOrder(position int, rcpt *Receipt) error {
	if problem := requiredScalarProblem(rcpt.orderRaw, scalarInteger); problem != "" {
		return fmt.Errorf("receipt %d for proposal %q field %q %s",
			position, rcpt.ProposalID, "order", problem)
	}
	if rcpt.Order != int64(position) {
		return fmt.Errorf("receipt %d for proposal %q field %q is %d, expected %d",
			position, rcpt.ProposalID, "order", rcpt.Order, position)
	}
	return nil
}

// validateReceiptActionIndex 严格判定一条动作留痕的 index：它是该动作在所属
// 凭据中明确保存的位置编号（0 起）。字段未写出、显式为 null 或不是 int64
// 整数（字符串、布尔、小数、指数写法、超界）返回带提案编号、动作位置与
// 字段的错误；类型合法但值与动作的实际位置不符（含负数）返回位置不符错误。
// 第一项动作缺失 index 不得被补成 0：只有明确写出的 0 才合法。position 是
// 动作在凭据 actions 中的位置（0 起）。
func validateReceiptActionIndex(id string, position int, ar *ActionReceipt) error {
	if problem := requiredScalarProblem(ar.indexRaw, scalarInteger); problem != "" {
		return fmt.Errorf("receipt for proposal %q action %d field %q %s",
			id, position, "index", problem)
	}
	if ar.Index != int64(position) {
		return fmt.Errorf("receipt for proposal %q action %d field %q is %d, expected %d",
			id, position, "index", ar.Index, position)
	}
	return nil
}

// validateReceiptActionBalances 严格判定一条动作留痕的四个余额字段：
// 资金库与收款账户两侧的 before/after 都必须明确写出、非 null 且为 int64
// 整数。任何一个字段缺失都判整份状态损坏，不能根据转账金额、账户余额或
// 其它凭据补出它——即使按其余记录推算该字段本来应为 0。明确写出的整数 0
// 是合法记录（如资金库恰好扣到 0、收款账户首次收款前为 0），不得当成缺失。
// index 是动作在凭据中的下标（0 起），与提案编号、所在侧一起用于定位。
// “缺失/空值/类型不符”的判定与逐票明细、计票结果、executed_at 共用同一份
// 规则（validateRawFields），此处只补充业务位置：提案编号 + 动作下标 + 侧。
func validateReceiptActionBalances(id string, index int, ar *ActionReceipt) error {
	sides := []struct {
		name   string
		update *BalanceUpdate
	}{
		{"treasury", &ar.Treasury},
		{"recipient", &ar.Recipient},
	}
	for _, side := range sides {
		err := validateRawFields([]rawField{
			{name: "before", kind: scalarInteger, raw: side.update.beforeRaw},
			{name: "after", kind: scalarInteger, raw: side.update.afterRaw},
		}, func(field, problem string) error {
			return fmt.Errorf("receipt for proposal %q action %d %s field %q %s",
				id, index, side.name, field, problem)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// ---- 结构检查：固定字段名逐字匹配 + 重复键拒绝 ----

// schemaKind 是状态文件 JSON 在某一层的形状类别。
type schemaKind int

const (
	kindAny     schemaKind = iota // 叶子值：结构检查不深入，类型由解码器核对
	kindObject                    // 固定字段对象：键必须与保存格式的字段名逐字一致
	kindMap                       // 业务编号表：键是自由文本（账户名、提案编号）
	kindArray                     // 数组
	kindString                    // 叶子：必须是 JSON 字符串
	kindBoolean                   // 叶子：必须是 JSON 布尔值（不接受字符串/数字/null）
	kindInteger                   // 叶子：必须是 int64 范围内的 JSON 整数（不接受小数/指数/字符串/null）
)

// schemaNode 描述状态文件某一层允许的形状。整棵树由 storedState 的 json tag
// 反射生成，字段名集合因此永远与保存格式一致，不会随结构演进而漂移。
type schemaNode struct {
	kind   schemaKind
	fields map[string]*schemaNode // kindObject：字段名 → 值的形状
	elem   *schemaNode            // kindMap/kindArray：值/元素的形状
}

// schemaOf 按 encoding/json 的字段名规则（json tag 优先，缺省用字段名）推导形状。
func schemaOf(t reflect.Type) *schemaNode {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		node := &schemaNode{kind: kindObject, fields: map[string]*schemaNode{}}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.PkgPath != "" { // 未导出字段不参与 JSON
				continue
			}
			name := f.Name
			if tag, ok := f.Tag.Lookup("json"); ok {
				tagName, _, _ := strings.Cut(tag, ",")
				if tagName == "-" {
					continue
				}
				if tagName != "" {
					name = tagName
				}
			}
			node.fields[name] = schemaOf(f.Type)
		}
		return node
	case reflect.Map:
		return &schemaNode{kind: kindMap, elem: schemaOf(t.Elem())}
	case reflect.Slice, reflect.Array:
		return &schemaNode{kind: kindArray, elem: schemaOf(t.Elem())}
	case reflect.String:
		return &schemaNode{kind: kindString}
	case reflect.Bool:
		return &schemaNode{kind: kindBoolean}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return &schemaNode{kind: kindInteger}
	default:
		return &schemaNode{kind: kindAny}
	}
}

var storedStateSchema = schemaOf(reflect.TypeOf(storedState{}))

func init() {
	// 票据的四个标量字段（representative/weight/support/voted_at）由
	// validateStoredBallot 逐票判定“缺失/空值/类型不符”，其错误原因需要带上
	// 提案编号、票据下标与字段名；因此结构扫描在这些叶子位置保持宽松（kindAny），
	// 不抢在解码器之前用不含票据定位的通用信息报错。
	ballotFields := storedStateSchema.fields["vote_proposals"].elem.fields["ballots"].elem.fields
	for _, name := range []string{"representative", "weight", "support", "voted_at"} {
		ballotFields[name].kind = kindAny
	}
	// 计票结果的两个权重字段（for_weight/against_weight）同理：缺失/空值/
	// 类型不符的判定需要带上提案编号，统一由 validateStoredTally 报错，
	// 结构扫描在这两个叶子位置保持宽松。tallied_at 保持严格整数叶子，
	// 其“不早于截止”的既有校验不变。
	tallyFields := storedStateSchema.fields["vote_proposals"].elem.fields["tally"].fields
	for _, name := range []string{"for_weight", "against_weight"} {
		tallyFields[name].kind = kindAny
	}
	// 登记提案的 timelock_end 同理：缺失/空值/类型不符的判定需要带上提案
	// 编号，统一由 validateProposalTimelock 报错，结构扫描在此叶子位置保持宽松。
	storedStateSchema.fields["proposals"].elem.fields["timelock_end"].kind = kindAny
	// 凭据的 executed_at 同理：缺失/空值/类型不符/早于时间锁的判定需要带上
	// 提案编号与凭据位置，统一由 validateReceiptExecutedAt 报错，结构扫描在此
	// 叶子位置保持宽松。
	receiptFields := storedStateSchema.fields["receipts"].elem.fields
	receiptFields["executed_at"].kind = kindAny
	// 凭据的 order 同理：缺失/空值/类型不符/位置不符的判定需要带上提案编号与
	// 凭据位置，统一由 validateReceiptOrder 报错，结构扫描在此叶子位置保持宽松。
	receiptFields["order"].kind = kindAny
	// 凭据逐笔留痕的 index 同理：缺失/空值/类型不符/位置不符的判定需要带上
	// 提案编号与动作下标，统一由 validateReceiptActionIndex 报错，结构扫描在
	// 此叶子位置保持宽松。
	receiptFields["actions"].elem.fields["index"].kind = kindAny
	// 凭据逐笔留痕的四个余额字段（treasury/recipient 的 before/after）同理：
	// 缺失/空值/类型不符的判定需要带上提案编号、动作下标与所在侧，统一由
	// validateReceiptActionBalances 报错，结构扫描在这些叶子位置保持宽松。
	actionFields := receiptFields["actions"].elem.fields
	for _, side := range []string{"treasury", "recipient"} {
		balanceFields := actionFields[side].fields
		balanceFields["before"].kind = kindAny
		balanceFields["after"].kind = kindAny
	}
}

// scanFrame 是结构扫描的栈帧。
type scanFrame struct {
	node    *schemaNode
	isArray bool
	opaque  bool // 形状不符或叶子位置的容器：只配对括号，类型错误交给解码器报告
	seen    map[string]bool
	atValue bool // 对象帧：下一个待消费 token 是值而非键
	index   int  // 数组帧：下一个元素下标
	path    string
}

func peekFrame(stack []*scanFrame) *scanFrame {
	if len(stack) == 0 {
		return nil
	}
	return stack[len(stack)-1]
}

// checkStateStructure 在解码前逐 token 扫描状态文件：
//   - 同一对象内键（按 JSON 解读后的字符串，转义拼写与普通拼写视为同一键）不得重复；
//   - 固定字段对象的键必须与保存格式的字段名逐字一致：大小写变体（如 Treasury、
//     Support）与未知字段一样拒绝，避免解码器的大小写折叠把两份值并入同一字段；
//   - balances/proposals/vote_proposals 等业务编号表的键是自由文本，
//     Audit 与 audit、GIP-1 与 gip-1 是不同账户/编号，不做字段名检查。
//
// 报错带有出问题的字段与所在记录的路径（如 vote_proposals["GIP-1"].ballots[0]），
// 便于定位。字段顺序、缩进与合法转义不影响判定。
func checkStateStructure(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	// UseNumber：数字 token 保留词面写法，整数叶子可据此精确判定 int64 范围，
	// 不被 float64 在 2^63 附近的舍入混淆（MaxInt64 与 MaxInt64+1 会折成同一浮点）。
	dec.UseNumber()
	var stack []*scanFrame
	// 对象帧读到键时，把该键对应值的形状与路径暂存于此，供紧随其后的值使用。
	pendingNode := storedStateSchema
	pendingPath := "state"

	// 一个值消费完毕：推进父帧的键/值位置或数组下标。
	finishValue := func() {
		top := peekFrame(stack)
		if top == nil || top.opaque {
			return
		}
		if top.isArray {
			top.index++
		} else {
			top.atValue = false
		}
	}

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			if len(stack) != 0 {
				return errors.New("unterminated object or array")
			}
			return nil
		}
		if err != nil {
			return err
		}

		if top := peekFrame(stack); top != nil && top.opaque {
			// 不透明容器内不做字段检查，只配对括号。
			if d, ok := tok.(json.Delim); ok {
				if d == '{' || d == '[' {
					stack = append(stack, &scanFrame{opaque: true})
				} else {
					stack = stack[:len(stack)-1]
					finishValue()
				}
			}
			continue
		}

		if top := peekFrame(stack); top != nil && !top.isArray && !top.atValue {
			// 对象帧的键位置：闭括号或下一个键。
			if d, ok := tok.(json.Delim); ok {
				if d != '}' {
					return fmt.Errorf("unexpected %q inside object at %s", string(d), top.path)
				}
				stack = stack[:len(stack)-1]
				finishValue()
				continue
			}
			key, ok := tok.(string)
			if !ok {
				return errors.New("object key must be a string")
			}
			if top.seen[key] {
				return fmt.Errorf("duplicate key %q at %s", key, top.path)
			}
			top.seen[key] = true
			top.atValue = true
			switch top.node.kind {
			case kindObject:
				child, known := top.node.fields[key]
				if !known {
					return fmt.Errorf("unknown field %q at %s", key, top.path)
				}
				pendingNode = child
				pendingPath = top.path + "." + key
			case kindMap:
				// 业务编号是自由文本，大小写变体属于不同编号，不得拒绝或合并。
				pendingNode = top.node.elem
				pendingPath = top.path + "[" + strconv.Quote(key) + "]"
			}
			continue
		}

		// 值位置：对象键之后的值或数组元素。
		node := pendingNode
		path := pendingPath
		if top := peekFrame(stack); top != nil && top.isArray {
			node = top.node.elem
			path = top.path + "[" + strconv.Itoa(top.index) + "]"
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				frame := &scanFrame{isArray: d == '[', path: path}
				shapeOK := (d == '{' && (node.kind == kindObject || node.kind == kindMap)) ||
					(d == '[' && node.kind == kindArray)
				if !shapeOK {
					// 叶子位置出现容器或形状不符：跳过内容，类型错误由解码器报告。
					frame.opaque = true
				} else {
					frame.node = node
					if d == '{' {
						frame.seen = map[string]bool{}
					}
				}
				stack = append(stack, frame)
			default: // '}' 或 ']'：数组结束（对象的闭括号已在键位置处理）
				if len(stack) == 0 {
					return errors.New("unexpected closing delimiter")
				}
				stack = stack[:len(stack)-1]
				finishValue()
			}
			continue
		}
		// 标量值（含 null）：严格叶子必须与字段声明的 JSON 类型逐字一致，
		// null 不允许用来顶替字符串/布尔/整数。
		if err := checkLeafScalar(node, path, tok); err != nil {
			return err
		}
		finishValue()
	}
}

// leafKindName 是严格叶子类型在错误信息中的人类可读名称。
func leafKindName(k schemaKind) string {
	switch k {
	case kindString:
		return "string"
	case kindBoolean:
		return "boolean"
	case kindInteger:
		return "integer"
	default:
		return "value"
	}
}

// tokenKindName 归类一个标量 token 实际写出的 JSON 类型。
func tokenKindName(tok json.Token) string {
	switch v := tok.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case json.Number:
		if _, err := strconv.ParseInt(string(v), 10, 64); err == nil {
			return "integer"
		}
		return "number"
	default:
		return "value"
	}
}

// checkLeafScalar 校验严格标量叶子：字符串/布尔/int64 整数位置出现 null 或
// 其它 JSON 类型即判结构损坏。非整数（小数、指数）或超出 int64 的整数同样拒绝。
func checkLeafScalar(node *schemaNode, path string, tok json.Token) error {
	switch node.kind {
	case kindString, kindBoolean, kindInteger:
	default:
		return nil
	}
	if node.kind == kindInteger {
		if n, ok := tok.(json.Number); ok {
			if _, err := strconv.ParseInt(string(n), 10, 64); err != nil {
				return fmt.Errorf("field at %s must be an int64 integer, got %s", path, string(n))
			}
			return nil
		}
	}
	want := leafKindName(node.kind)
	if got := tokenKindName(tok); got != want {
		return fmt.Errorf("field at %s must be %s, got %s", path, want, got)
	}
	return nil
}
