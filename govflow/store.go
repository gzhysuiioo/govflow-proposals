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
type BalanceUpdate struct {
	Account string `json:"account"`
	Before  int64  `json:"before"`
	After   int64  `json:"after"`
}

// ActionReceipt 是凭据中对单项动作的留痕：动作原文、在提案中的顺序编号（0 起），
// 以及该动作引发的资金库与收款账户余额变动。
type ActionReceipt struct {
	Index     int           `json:"index"`
	Action    string        `json:"action"`
	Treasury  BalanceUpdate `json:"treasury"`
	Recipient BalanceUpdate `json:"recipient"`
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
	Order      int             `json:"order"`       // 成功提交的先后顺序，0 起
	Actions    []ActionReceipt `json:"actions"`

	executedAtSF strictField
}

// receiptJSONShape 只用于解码，按保存格式的字段名逐字接住每个字段的原始 JSON。
type receiptJSONShape struct {
	ProposalID string          `json:"proposal_id"`
	ExecutedAt json.RawMessage `json:"executed_at"`
	Order      int             `json:"order"`
	Actions    []ActionReceipt `json:"actions"`
}

// UnmarshalJSON 保留 executed_at 是否出现及其原始写法（缺失为 nil、null 为
// "null"），使“字段缺失”与“显式写出 0”在解码后仍可区分：encoding/json 直接
// 解进 int64 会把缺失/null/类型不符都折叠成 0。判定统一交给 validateState，
// 此处只在字段确实是 int64 整数时填充 ExecutedAt。
func (r *Receipt) UnmarshalJSON(data []byte) error {
	var shape receiptJSONShape
	if err := json.Unmarshal(data, &shape); err != nil {
		return err
	}
	r.ProposalID = shape.ProposalID
	r.Order = shape.Order
	r.Actions = shape.Actions
	r.executedAtSF = captureStrictField(strictInteger, shape.ExecutedAt)
	r.ExecutedAt = r.executedAtSF.int64Value()
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

type storedProposal struct {
	ID          string   `json:"id"`
	State       string   `json:"state"`
	TimelockEnd int64    `json:"timelock_end"`
	Actions     []string `json:"actions"`
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
func (s *Store) Register(id string, timelockEnd int64, actions []string) (existed bool, err error) {
	if id == "" {
		return false, fmt.Errorf("%w: proposal id must not be empty", ErrInvalidRegistration)
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
	stored := copyActions(actions)
	if existing, ok := state.Proposals[id]; ok {
		if existing.TimelockEnd == timelockEnd && sameActions(existing.Actions, stored) {
			return true, nil
		}
		return false, fmt.Errorf("%w: proposal %s", ErrProposalConflict, id)
	}
	// 两种来源共用编号：register 不能覆盖投票提案或绕过其投票结论。
	if vp, ok := state.VoteProposals[id]; ok {
		return false, fmt.Errorf("%w: proposal id %s belongs to a voting proposal (state=%s)", ErrProposalConflict, id, vp.State)
	}
	state.Proposals[id] = &storedProposal{
		ID:          id,
		State:       "passed",
		TimelockEnd: timelockEnd,
		Actions:     stored,
	}
	if err := s.commitLocked(state); err != nil {
		return false, err
	}
	return false, nil
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
		Order:      len(state.Receipts),
		Actions:    make([]ActionReceipt, 0, len(steps)),
	}
	// 同步填上字段原始片段，使“提交前校验”与“打开重放校验”走同一份
	// executed_at 判定时不会把本进程新建的凭据误判为字段缺失。
	receipt.executedAtSF = presentStrictField(strictInteger, now)
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
		if rcpt.Order != order {
			return fmt.Errorf("receipt %q order field is %d, expected %d", rcpt.ProposalID, rcpt.Order, order)
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
		// 保存的凭据与首次执行受同一条执行资格规则约束：executed_at 必须明确
		// 写出且不得早于提案时间锁。登记提案与投票提案共用这一判断。
		if err := validateReceiptExecutedAt(order, rcpt, ownerTimelock); err != nil {
			return err
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
	if len(simBalances) != len(state.Balances) {
		return fmt.Errorf("balance table has %d accounts, receipt replay yields %d", len(state.Balances), len(simBalances))
	}
	for account, balance := range state.Balances {
		if simBalances[account] != balance {
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

// validateReceiptExecutedAt 严格判定一份已保存凭据的 executed_at：
// 字段未写出、显式为 null 或不是 int64 整数都返回带定位的错误（完整性与
// 类型判定复用 validateStrictFields，与票据字段、计票权重同一份规则）；
// 明确写出的值必须不早于所属提案的 timelock_end（恰好等于时间锁有效，
// 时间锁允许 0 时明确写出的 0 同样有效）。order 是凭据在凭据表中的
// 位置（0 起），与提案编号一起用于定位。
func validateReceiptExecutedAt(order int, rcpt *Receipt, timelockEnd int64) error {
	location := fmt.Sprintf("receipt %d for proposal %q", order, rcpt.ProposalID)
	if err := validateStrictFields(location, []strictFieldSpec{
		{name: "executed_at", field: &rcpt.executedAtSF},
	}); err != nil {
		return err
	}
	if rcpt.ExecutedAt < timelockEnd {
		return fmt.Errorf("receipt %d for proposal %q executed_at %d is before timelock_end %d",
			order, rcpt.ProposalID, rcpt.ExecutedAt, timelockEnd)
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
	// 凭据的 executed_at 同理：缺失/空值/类型不符/早于时间锁的判定需要带上
	// 提案编号与凭据位置，统一由 validateReceiptExecutedAt 报错，结构扫描在此
	// 叶子位置保持宽松。
	storedStateSchema.fields["receipts"].elem.fields["executed_at"].kind = kindAny
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
