// Package govflow treasury.go: local treasury execution backed by a state file.
//
// The state file holds treasury balance, payee accounts, registered proposals
// and execution receipts together. Every mutation is performed as one
// read-modify-write transaction under an exclusive flock on a sidecar lock
// file, and committed atomically (temp file + fsync + rename + directory
// fsync). A process that dies mid-transaction therefore leaves either the
// pre-execution or the post-execution state on disk, never a partial one.
package govflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// State file format version.
const stateVersion int64 = 1

// Errors returned by the store. Use errors.Is to classify them.
var (
	// ErrCorruptState means the state file is missing, unreadable or failed
	// validation. The file is never rebuilt or overwritten in this case.
	ErrCorruptState = errors.New("corrupt state file")
	// ErrNotFound means the requested proposal or receipt does not exist.
	ErrNotFound = errors.New("not found")
	// ErrConflict means a proposal id was reused with different content.
	ErrConflict = errors.New("conflict")
	// ErrInvalidArg means a caller argument is invalid.
	ErrInvalidArg = errors.New("invalid argument")
	// ErrInvalidAction means an action does not match transfer:<account>:<amount>.
	ErrInvalidAction = errors.New("invalid action")
	// ErrInvalidState means a proposal is not in the required state.
	ErrInvalidState = errors.New("invalid proposal state")
	// ErrTimelock means the timelock has not been reached yet.
	ErrTimelock = errors.New("timelock not reached")
	// ErrOverflow means an int64 balance or amount computation overflowed.
	ErrOverflow = errors.New("integer overflow")
	// ErrInsufficient means the treasury cannot cover the transfer total.
	ErrInsufficient = errors.New("insufficient treasury balance")
	// ErrFileExists means create was attempted on an existing state file.
	ErrFileExists = errors.New("state file already exists")
	// ErrFileNotFound means the state file path does not exist.
	ErrFileNotFound = errors.New("state file not found")
)

// ReceiptAction is one leg of an executed proposal: the payee, the amount and
// the payee account balance immediately before and after the leg.
type ReceiptAction struct {
	Account       string `json:"account"`
	Amount        int64  `json:"amount"`
	BalanceBefore int64  `json:"balance_before"`
	BalanceAfter  int64  `json:"balance_after"`
}

// Receipt is the immutable proof of a successful proposal execution. It is
// stored once, on the first successful execution, and returned unchanged by
// every later retry.
type Receipt struct {
	ProposalID     string          `json:"proposal_id"`
	ExecutedAt     int64           `json:"executed_at"`
	TreasuryBefore int64           `json:"treasury_before"`
	TreasuryAfter  int64           `json:"treasury_after"`
	Actions        []ReceiptAction `json:"actions"`
}

// persistedState is the on-disk JSON document.
type persistedState struct {
	Version   int64             `json:"version"`
	Initial   int64             `json:"initial_balance"`
	Treasury  int64             `json:"treasury"`
	Accounts  map[string]int64  `json:"accounts"`
	Proposals map[string]Proposal `json:"proposals"`
	Order     []string          `json:"order"`
	Receipts  []Receipt         `json:"receipts"`
}

// Store is a handle to one local state file. All methods are safe for
// concurrent use from multiple goroutines and multiple processes.
type Store struct {
	path string
}

// CreateStore creates a new state file with the given initial treasury
// balance. It fails with ErrFileExists if the file already exists and never
// resets an existing file's balance.
func CreateStore(path string, initialBalance int64) (*Store, error) {
	if initialBalance < 0 {
		return nil, fmt.Errorf("%w: initial balance must not be negative", ErrInvalidArg)
	}
	st := &Store{path: path}
	unlock, err := st.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("%w: %s", ErrFileExists, path)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat state file: %w", err)
	}
	state := &persistedState{
		Version:   stateVersion,
		Initial:   initialBalance,
		Treasury:  initialBalance,
		Accounts:  map[string]int64{},
		Proposals: map[string]Proposal{},
		Order:     []string{},
		Receipts:  []Receipt{},
	}
	if err := st.writeState(state); err != nil {
		return nil, err
	}
	return st, nil
}

// OpenStore opens an existing state file and validates it. A missing or
// corrupt file is reported with ErrFileNotFound or ErrCorruptState; it is
// never rebuilt or overwritten.
func OpenStore(path string) (*Store, error) {
	st := &Store{path: path}
	state, err := st.readState()
	if err != nil {
		return nil, err
	}
	_ = state
	return st, nil
}

// Register records a passed proposal. Only proposals in state "passed" with a
// non-empty id are accepted; the timelock and action texts are preserved
// verbatim. Registering the same id with identical content returns the stored
// record (idempotent retry); different content is an ErrConflict.
func (s *Store) Register(p Proposal) (*Proposal, error) {
	if p.State != "passed" {
		return nil, fmt.Errorf("%w: only passed proposals can be registered (got %q)", ErrInvalidState, p.State)
	}
	if strings.TrimSpace(p.ID) == "" {
		return nil, fmt.Errorf("%w: proposal id must not be empty", ErrInvalidArg)
	}
	var result *Proposal
	err := s.transact(func(state *persistedState) (bool, error) {
		if existing, ok := state.Proposals[p.ID]; ok {
			if sameProposalContent(existing, p) {
				cp := existing
				result = &cp
				return false, nil
			}
			return false, fmt.Errorf("%w: proposal %q already registered with different content", ErrConflict, p.ID)
		}
		cp := p
		state.Proposals[p.ID] = cp
		state.Order = append(state.Order, p.ID)
		result = &cp
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// Execute performs the transfers of a registered proposal in action order.
// now is supplied by the caller. A proposal is executed at most once: the
// first successful execution stores a receipt and flips the state to
// "executed"; every later call returns that receipt unchanged without
// re-checking eligibility or moving funds. A failed execution moves no funds
// and consumes no execution chance.
func (s *Store) Execute(proposalID string, now int64) (*Receipt, error) {
	if strings.TrimSpace(proposalID) == "" {
		return nil, fmt.Errorf("%w: proposal id must not be empty", ErrInvalidArg)
	}
	var receipt *Receipt
	err := s.transact(func(state *persistedState) (bool, error) {
		prop, ok := state.Proposals[proposalID]
		if !ok {
			return false, fmt.Errorf("%w: proposal %q is not registered", ErrNotFound, proposalID)
		}
		if prop.State == "executed" {
			r := findReceipt(state, proposalID)
			if r == nil {
				return false, fmt.Errorf("%w: receipt for executed proposal %q is missing", ErrCorruptState, proposalID)
			}
			receipt = r
			return false, nil
		}
		if prop.State != "passed" {
			return false, fmt.Errorf("%w: proposal %q is %q, not passed", ErrInvalidState, proposalID, prop.State)
		}
		if now < prop.TimelockEnd {
			return false, fmt.Errorf("%w: proposal %q timelock not reached (now=%d, timelock=%d)", ErrTimelock, proposalID, now, prop.TimelockEnd)
		}
		if len(prop.Actions) == 0 {
			return false, fmt.Errorf("%w: proposal %q has no actions", ErrInvalidAction, proposalID)
		}
		type leg struct {
			account string
			amount  int64
		}
		legs := make([]leg, 0, len(prop.Actions))
		var total int64
		for _, action := range prop.Actions {
			account, amount, perr := parseAction(action)
			if perr != nil {
				return false, fmt.Errorf("proposal %q: %w", proposalID, perr)
			}
			if amount > math.MaxInt64-total {
				return false, fmt.Errorf("%w: transfer total overflows int64", ErrOverflow)
			}
			total += amount
			legs = append(legs, leg{account, amount})
		}
		if total > state.Treasury {
			return false, fmt.Errorf("%w: proposal %q needs %d, treasury has %d", ErrInsufficient, proposalID, total, state.Treasury)
		}
		receiptActions := make([]ReceiptAction, 0, len(legs))
		treasuryAfter := state.Treasury - total
		accounts := cloneAccounts(state.Accounts)
		for _, l := range legs {
			before := accounts[l.account]
			if before > math.MaxInt64-l.amount {
				return false, fmt.Errorf("%w: balance for account %q overflows int64", ErrOverflow, l.account)
			}
			after := before + l.amount
			receiptActions = append(receiptActions, ReceiptAction{
				Account:       l.account,
				Amount:        l.amount,
				BalanceBefore: before,
				BalanceAfter:  after,
			})
			accounts[l.account] = after
		}
		r := Receipt{
			ProposalID:     proposalID,
			ExecutedAt:     now,
			TreasuryBefore: state.Treasury,
			TreasuryAfter:  treasuryAfter,
			Actions:        receiptActions,
		}
		state.Treasury = treasuryAfter
		state.Accounts = accounts
		prop.State = "executed"
		state.Proposals[proposalID] = prop
		state.Receipts = append(state.Receipts, r)
		receipt = &r
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return receipt, nil
}

// Balance returns the treasury balance and a copy of all payee account
// balances. Accounts that never appeared in a transfer are simply absent;
// their balance is zero.
func (s *Store) Balance() (int64, map[string]int64, error) {
	var treasury int64
	var accounts map[string]int64
	err := s.readLocked(func(state *persistedState) error {
		treasury = state.Treasury
		accounts = cloneAccounts(state.Accounts)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return treasury, accounts, nil
}

// GetProposal returns a copy of the stored proposal.
func (s *Store) GetProposal(id string) (*Proposal, error) {
	var prop *Proposal
	err := s.readLocked(func(state *persistedState) error {
		p, ok := state.Proposals[id]
		if !ok {
			return fmt.Errorf("%w: proposal %q is not registered", ErrNotFound, id)
		}
		cp := p
		prop = &cp
		return nil
	})
	if err != nil {
		return nil, err
	}
	return prop, nil
}

// Receipts returns all receipts in the order they were successfully committed.
func (s *Store) Receipts() ([]Receipt, error) {
	var out []Receipt
	err := s.readLocked(func(state *persistedState) error {
		out = append([]Receipt(nil), state.Receipts...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetReceipt returns the receipt for one proposal.
func (s *Store) GetReceipt(proposalID string) (*Receipt, error) {
	var r *Receipt
	err := s.readLocked(func(state *persistedState) error {
		rec := findReceipt(state, proposalID)
		if rec == nil {
			return fmt.Errorf("%w: no receipt for proposal %q", ErrNotFound, proposalID)
		}
		cp := *rec
		r = &cp
		return nil
	})
	if err != nil {
		return nil, err
	}
	return r, nil
}

// ---------------------------------------------------------------------------
// internals
// ---------------------------------------------------------------------------

// transact runs fn under the exclusive lock. fn returns dirty=true when the
// state must be committed; a failed commit leaves the previous file intact.
func (s *Store) transact(fn func(*persistedState) (bool, error)) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	state, err := s.readState()
	if err != nil {
		return err
	}
	dirty, err := fn(state)
	if err != nil {
		return err
	}
	if dirty {
		return s.writeState(state)
	}
	return nil
}

func (s *Store) readLocked(fn func(*persistedState) error) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	state, err := s.readState()
	if err != nil {
		return err
	}
	return fn(state)
}

// lock acquires an exclusive flock on the sidecar lock file for the duration
// of the transaction. Separate processes and separate goroutines each hold a
// distinct open file description, so the lock serializes all of them.
func (s *Store) lock() (func(), error) {
	lockPath := s.path + ".lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("acquire lock: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// readState loads and validates the state file.
func (s *Store) readState() (*persistedState, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrFileNotFound, s.path)
		}
		return nil, fmt.Errorf("read state file: %w", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, fmt.Errorf("%w: state file is empty", ErrCorruptState)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var state persistedState
	if err := dec.Decode(&state); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorruptState, err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("%w: trailing data after JSON document", ErrCorruptState)
		}
		return nil, fmt.Errorf("%w: %v", ErrCorruptState, err)
	}
	if err := validateState(&state); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorruptState, err)
	}
	return &state, nil
}

// writeState commits the state atomically: temp file in the same directory,
// fsync, rename, then fsync the directory. The previous file stays untouched
// unless the rename succeeds.
func (s *Store) writeState(state *persistedState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(s.path)
	base := filepath.Base(s.path)
	tmp, err := os.CreateTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	abort := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if _, err := tmp.Write(data); err != nil {
		abort()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		abort()
		return fmt.Errorf("fsync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("rename state file: %w", err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// validateState checks the document structure and replays all receipts from
// the initial balance to verify the final treasury and account balances.
func validateState(state *persistedState) error {
	if state == nil {
		return errors.New("empty state")
	}
	if state.Version != stateVersion {
		return fmt.Errorf("unsupported state version %d", state.Version)
	}
	if state.Initial < 0 {
		return errors.New("negative initial balance")
	}
	if state.Treasury < 0 {
		return errors.New("negative treasury balance")
	}
	if state.Accounts == nil {
		return errors.New("missing accounts map")
	}
	if state.Proposals == nil {
		return errors.New("missing proposals map")
	}
	if state.Order == nil {
		return errors.New("missing order list")
	}
	if state.Receipts == nil {
		return errors.New("missing receipts list")
	}
	for acct, bal := range state.Accounts {
		if bal < 0 {
			return fmt.Errorf("account %q has negative balance %d", acct, bal)
		}
	}
	seen := make(map[string]bool, len(state.Order))
	for _, id := range state.Order {
		if seen[id] {
			return fmt.Errorf("duplicate proposal id %q in order", id)
		}
		seen[id] = true
		p, ok := state.Proposals[id]
		if !ok {
			return fmt.Errorf("proposal %q in order but missing from proposals map", id)
		}
		if p.ID != id {
			return fmt.Errorf("proposal %q has internal id %q", id, p.ID)
		}
		if p.State != "passed" && p.State != "executed" {
			return fmt.Errorf("proposal %q has invalid state %q", id, p.State)
		}
	}
	if len(seen) != len(state.Proposals) {
		return errors.New("proposals map contains ids not in order")
	}
	receiptByID := make(map[string]*Receipt, len(state.Receipts))
	for i := range state.Receipts {
		r := &state.Receipts[i]
		if r.ProposalID == "" {
			return errors.New("receipt with empty proposal id")
		}
		if _, dup := receiptByID[r.ProposalID]; dup {
			return fmt.Errorf("duplicate receipt for proposal %q", r.ProposalID)
		}
		receiptByID[r.ProposalID] = r
		p, ok := state.Proposals[r.ProposalID]
		if !ok {
			return fmt.Errorf("receipt references unknown proposal %q", r.ProposalID)
		}
		if p.State != "executed" {
			return fmt.Errorf("receipt exists for proposal %q in state %q", r.ProposalID, p.State)
		}
		if r.ExecutedAt < p.TimelockEnd {
			return fmt.Errorf("receipt for proposal %q executed before timelock", r.ProposalID)
		}
		if r.TreasuryBefore < 0 || r.TreasuryAfter < 0 {
			return fmt.Errorf("receipt for proposal %q has negative treasury", r.ProposalID)
		}
		if len(r.Actions) == 0 {
			return fmt.Errorf("receipt for proposal %q has no actions", r.ProposalID)
		}
		var sum int64
		for _, a := range r.Actions {
			if a.Amount <= 0 {
				return fmt.Errorf("receipt for proposal %q has non-positive action amount", r.ProposalID)
			}
			if a.BalanceBefore < 0 || a.BalanceAfter < 0 {
				return fmt.Errorf("receipt for proposal %q has negative account balance", r.ProposalID)
			}
			if a.BalanceAfter != a.BalanceBefore+a.Amount {
				return fmt.Errorf("receipt for proposal %q has inconsistent account balance", r.ProposalID)
			}
			if a.Amount > math.MaxInt64-sum {
				return fmt.Errorf("receipt for proposal %q action sum overflows", r.ProposalID)
			}
			sum += a.Amount
		}
		if r.TreasuryAfter != r.TreasuryBefore-sum {
			return fmt.Errorf("receipt for proposal %q has inconsistent treasury balance", r.ProposalID)
		}
	}
	for id, p := range state.Proposals {
		if p.State == "executed" {
			if _, ok := receiptByID[id]; !ok {
				return fmt.Errorf("executed proposal %q has no receipt", id)
			}
		}
	}
	// Replay receipts from the initial balance to verify the final state.
	treasury := state.Initial
	accounts := map[string]int64{}
	for _, r := range state.Receipts {
		if r.TreasuryBefore != treasury {
			return fmt.Errorf("receipt for proposal %q treasury_before %d does not match replayed %d", r.ProposalID, r.TreasuryBefore, treasury)
		}
		for _, a := range r.Actions {
			if accounts[a.Account] != a.BalanceBefore {
				return fmt.Errorf("receipt for proposal %q account %q balance_before mismatch", r.ProposalID, a.Account)
			}
			accounts[a.Account] = a.BalanceAfter
		}
		treasury = r.TreasuryAfter
	}
	if treasury != state.Treasury {
		return fmt.Errorf("replayed treasury %d does not match stored %d", treasury, state.Treasury)
	}
	if len(accounts) != len(state.Accounts) {
		return errors.New("replayed account set does not match stored accounts")
	}
	for acct, bal := range state.Accounts {
		if accounts[acct] != bal {
			return fmt.Errorf("replayed balance for account %q does not match", acct)
		}
	}
	return nil
}

// parseAction parses transfer:<account>:<positive-decimal-amount>.
func parseAction(action string) (string, int64, error) {
	parts := strings.Split(action, ":")
	if len(parts) != 3 || parts[0] != "transfer" {
		return "", 0, fmt.Errorf("%w: action %q must look like transfer:<account>:<positive-decimal-amount>", ErrInvalidAction, action)
	}
	account := parts[1]
	if account == "" {
		return "", 0, fmt.Errorf("%w: payee account must not be empty (action %q)", ErrInvalidAction, action)
	}
	digits := parts[2]
	if digits == "" {
		return "", 0, fmt.Errorf("%w: amount must not be empty (action %q)", ErrInvalidAction, action)
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return "", 0, fmt.Errorf("%w: amount %q must be decimal digits only", ErrInvalidAction, digits)
		}
	}
	amount, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("%w: amount %q out of int64 range", ErrInvalidAction, digits)
	}
	if amount <= 0 {
		return "", 0, fmt.Errorf("%w: amount %q must be a positive integer", ErrInvalidAction, digits)
	}
	return account, amount, nil
}

func sameProposalContent(a, b Proposal) bool {
	if a.Title != b.Title || a.ForVotes != b.ForVotes || a.AgainstVotes != b.AgainstVotes ||
		a.Quorum != b.Quorum || a.TimelockEnd != b.TimelockEnd {
		return false
	}
	if len(a.Actions) != len(b.Actions) {
		return false
	}
	for i := range a.Actions {
		if a.Actions[i] != b.Actions[i] {
			return false
		}
	}
	return true
}

func findReceipt(state *persistedState, proposalID string) *Receipt {
	for i := range state.Receipts {
		if state.Receipts[i].ProposalID == proposalID {
			r := state.Receipts[i]
			return &r
		}
	}
	return nil
}

func cloneAccounts(in map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// sortedAccounts returns account names in a stable order for display.
func sortedAccounts(accounts map[string]int64) []string {
	names := make([]string, 0, len(accounts))
	for name := range accounts {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
