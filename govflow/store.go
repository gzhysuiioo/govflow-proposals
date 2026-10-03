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
	"sort"
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
type Receipt struct {
	ProposalID string          `json:"proposal_id"`
	ExecutedAt int64           `json:"executed_at"` // 首次成功执行时调用方提供的时间
	Order      int             `json:"order"`       // 成功提交的先后顺序，0 起
	Actions    []ActionReceipt `json:"actions"`
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

// proposalOwners 将两个来源的提案编号映射到状态，供凭据重放交叉校验。
func proposalOwnerState(state *storedState, id string) (string, []string, bool) {
	if p, ok := state.Proposals[id]; ok {
		return p.State, p.Actions, true
	}
	if vp, ok := state.VoteProposals[id]; ok {
		return vp.State, vp.Actions, true
	}
	return "", nil, false
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

	// 第一阶段：在副本上校验并预演全部动作。任何一步失败都不得落盘。
	type step struct {
		account        string
		amount         int64
		treasuryBefore int64
		treasuryAfter  int64
		accountBefore  int64
		accountAfter   int64
	}
	simTreasury := state.Treasury
	simBalances := map[string]int64{}
	for k, v := range state.Balances {
		simBalances[k] = v
	}
	steps := make([]step, 0, len(target.actions))
	for i, raw := range target.actions {
		account, amount, perr := ParseTransfer(raw)
		if perr != nil {
			// 双 %w：调用方既可按 ErrExecutionRejected 判定执行被拒，
			// 也可按 ErrInvalidAction 区分“动作格式错误”这一具体原因。
			return nil, fmt.Errorf("%w: action %d (%q): %w", ErrExecutionRejected, i, raw, perr)
		}
		balanceBefore := simBalances[account]
		balanceAfter, ok := addInt64(balanceBefore, amount)
		if !ok {
			return nil, execReject(fmt.Sprintf("recipient balance overflow at action %d: account=%s balance=%d amount=%d", i, account, balanceBefore, amount))
		}
		if simTreasury < amount {
			return nil, execReject(fmt.Sprintf("insufficient treasury balance at action %d: treasury=%d amount=%d", i, simTreasury, amount))
		}
		treasuryBefore := simTreasury
		simTreasury -= amount
		simBalances[account] = balanceAfter
		steps = append(steps, step{
			account:        account,
			amount:         amount,
			treasuryBefore: treasuryBefore,
			treasuryAfter:  simTreasury,
			accountBefore:  balanceBefore,
			accountAfter:   balanceAfter,
		})
	}

	// 第二阶段：全部预演成功后构造凭据并一次性原子提交。
	receipt := &Receipt{
		ProposalID: id,
		ExecutedAt: now,
		Order:      len(state.Receipts),
		Actions:    make([]ActionReceipt, 0, len(steps)),
	}
	for i, st := range steps {
		receipt.Actions = append(receipt.Actions, ActionReceipt{
			Index:  i,
			Action: target.actions[i],
			Treasury: BalanceUpdate{
				Account: "treasury",
				Before:  st.treasuryBefore,
				After:   st.treasuryAfter,
			},
			Recipient: BalanceUpdate{
				Account: st.account,
				Before:  st.accountBefore,
				After:   st.accountAfter,
			},
		})
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
	out := make([]ProposalRecord, 0, len(state.Proposals))
	for _, p := range state.Proposals {
		out = append(out, toProposalRecord(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ProposalsSnapshot 是同一份完整已提交状态下的全部提案列表：
// 登记提案（按编号排序）在前，投票提案（按编号排序）在后。
// 一次 ProposalsSnapshot 调用只读取一次状态文件，保证两个来源的提案状态、
// 动作与投票明细彼此一致：不会把不同提交时刻的提案拼在一起。
type ProposalsSnapshot struct {
	Registered []ProposalRecord
	Voting     []*VoteProposalView
}

// ProposalsSnapshot 在一次持锁读取中取得登记提案与投票提案的完整列表。
// 返回的记录与视图均为副本，后续执行不会改变已返回的快照；
// 下一次调用重新读取当前已提交状态。
func (s *Store) ProposalsSnapshot() (*ProposalsSnapshot, error) {
	state, err := s.readState()
	if err != nil {
		return nil, err
	}
	out := &ProposalsSnapshot{
		Registered: make([]ProposalRecord, 0, len(state.Proposals)),
		Voting:     make([]*VoteProposalView, 0, len(state.VoteProposals)),
	}
	for _, p := range state.Proposals {
		out.Registered = append(out.Registered, toProposalRecord(p))
	}
	sort.Slice(out.Registered, func(i, j int) bool { return out.Registered[i].ID < out.Registered[j].ID })
	for _, vp := range state.VoteProposals {
		out.Voting = append(out.Voting, voteProposalView(vp))
	}
	sort.Slice(out.Voting, func(i, j int) bool { return out.Voting[i].ID < out.Voting[j].ID })
	return out, nil
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
	if err := rejectDuplicateKeys(raw); err != nil {
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
		ownerState, ownerActions, found := proposalOwnerState(state, rcpt.ProposalID)
		if !found {
			return fmt.Errorf("receipt %q references an unregistered proposal", rcpt.ProposalID)
		}
		if ownerState != "executed" {
			return fmt.Errorf("proposal %q has receipt but state is %q", rcpt.ProposalID, ownerState)
		}
		if len(rcpt.Actions) != len(ownerActions) {
			return fmt.Errorf("receipt %q action count %d does not match proposal %d", rcpt.ProposalID, len(rcpt.Actions), len(ownerActions))
		}
		for i, ar := range rcpt.Actions {
			if ar.Index != i {
				return fmt.Errorf("receipt %q action %d has index %d", rcpt.ProposalID, i, ar.Index)
			}
			if ar.Action != ownerActions[i] {
				return fmt.Errorf("receipt %q action %d text does not match registered action", rcpt.ProposalID, i)
			}
			account, amount, err := ParseTransfer(ar.Action)
			if err != nil {
				return fmt.Errorf("receipt %q action %d is not executable: %v", rcpt.ProposalID, i, err)
			}
			if ar.Treasury.Account != "treasury" || ar.Treasury.Before != simTreasury {
				return fmt.Errorf("receipt %q action %d treasury before-balance mismatch", rcpt.ProposalID, i)
			}
			if simTreasury < amount || ar.Treasury.After != simTreasury-amount {
				return fmt.Errorf("receipt %q action %d treasury after-balance mismatch", rcpt.ProposalID, i)
			}
			before := simBalances[account]
			if ar.Recipient.Account != account || ar.Recipient.Before != before {
				return fmt.Errorf("receipt %q action %d recipient before-balance mismatch", rcpt.ProposalID, i)
			}
			after, ok := addInt64(before, amount)
			if !ok || ar.Recipient.After != after {
				return fmt.Errorf("receipt %q action %d recipient after-balance mismatch", rcpt.ProposalID, i)
			}
			simTreasury -= amount
			simBalances[account] = after
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

// scanFrame 是重复键扫描的栈帧。
type scanFrame struct {
	isArray bool
	seen    map[string]bool
	atValue bool // 对象帧：下一个待消费 token 是值而非键
}

func rejectDuplicateKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	var stack []*scanFrame
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
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{':
				stack = append(stack, &scanFrame{seen: map[string]bool{}})
				continue
			case '[':
				stack = append(stack, &scanFrame{isArray: true})
				continue
			case '}', ']':
				if len(stack) == 0 {
					return errors.New("unexpected closing delimiter")
				}
				stack = stack[:len(stack)-1]
				// 关闭的容器本身是外层对象的某个“值”，消费完毕后外层等待下一个键。
				if len(stack) > 0 && !stack[len(stack)-1].isArray {
					stack[len(stack)-1].atValue = false
				}
				continue
			}
		default:
			// 标量 token。
		}
		if len(stack) > 0 && !stack[len(stack)-1].isArray {
			frame := stack[len(stack)-1]
			if !frame.atValue {
				key, ok := tok.(string)
				if !ok {
					return errors.New("object key must be a string")
				}
				if frame.seen[key] {
					return fmt.Errorf("duplicate key %q", key)
				}
				frame.seen[key] = true
				frame.atValue = true
			} else {
				frame.atValue = false
			}
		}
	}
}
