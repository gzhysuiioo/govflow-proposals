package govflow

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// worker mode: re-executes the test binary as an independent process
// ---------------------------------------------------------------------------

func TestMain(m *testing.M) {
	switch os.Getenv("GOVFLOW_TEST_WORKER") {
	case "register":
		os.Exit(workerRegister())
	case "execute":
		os.Exit(workerExecute())
	}
	os.Exit(m.Run())
}

func workerRegister() int {
	path := os.Getenv("GOVFLOW_TEST_FILE")
	balance, err := strconv.ParseInt(os.Getenv("GOVFLOW_TEST_BALANCE"), 10, 64)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bad balance:", err)
		return 2
	}
	st, err := CreateStore(path, balance)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	timelock, err := strconv.ParseInt(os.Getenv("GOVFLOW_TEST_TIMELOCK"), 10, 64)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bad timelock:", err)
		return 2
	}
	var actions []string
	for _, a := range strings.Split(os.Getenv("GOVFLOW_TEST_ACTIONS"), "\n") {
		if a != "" {
			actions = append(actions, a)
		}
	}
	p := Proposal{
		ID:          os.Getenv("GOVFLOW_TEST_ID"),
		Title:       os.Getenv("GOVFLOW_TEST_TITLE"),
		State:       "passed",
		TimelockEnd: timelock,
		Actions:     actions,
	}
	if _, err := st.Register(p); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func workerExecute() int {
	path := os.Getenv("GOVFLOW_TEST_FILE")
	id := os.Getenv("GOVFLOW_TEST_ID")
	now, err := strconv.ParseInt(os.Getenv("GOVFLOW_TEST_NOW"), 10, 64)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bad now:", err)
		return 2
	}
	st, err := OpenStore(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	r, err := st.Execute(id, now)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if r.ExecutedAt == now {
		fmt.Println("EXECUTED")
	} else {
		fmt.Println("ALREADY")
	}
	return 0
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func newTestStore(t *testing.T, balance int64) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	st, err := CreateStore(path, balance)
	if err != nil {
		t.Fatalf("CreateStore: %v", err)
	}
	return st, path
}

func passedProposal(id string, timelock int64, actions ...string) Proposal {
	return Proposal{
		ID:          id,
		Title:       "proposal " + id,
		State:       "passed",
		ForVotes:    100,
		AgainstVotes: 10,
		Quorum:      50,
		TimelockEnd: timelock,
		Actions:     actions,
	}
}

// ---------------------------------------------------------------------------
// basic lifecycle
// ---------------------------------------------------------------------------

func TestCreateAndOpen(t *testing.T) {
	st, path := newTestStore(t, 1000)
	treasury, accounts, err := st.Balance()
	if err != nil {
		t.Fatal(err)
	}
	if treasury != 1000 {
		t.Fatalf("treasury = %d, want 1000", treasury)
	}
	if len(accounts) != 0 {
		t.Fatalf("accounts = %v, want empty", accounts)
	}
	st2, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	treasury2, _, _ := st2.Balance()
	if treasury2 != 1000 {
		t.Fatalf("reopened treasury = %d, want 1000", treasury2)
	}
}

func TestCreateExistingFails(t *testing.T) {
	_, path := newTestStore(t, 1000)
	_, err := CreateStore(path, 9999)
	if !errors.Is(err, ErrFileExists) {
		t.Fatalf("err = %v, want ErrFileExists", err)
	}
	st, _ := OpenStore(path)
	treasury, _, _ := st.Balance()
	if treasury != 1000 {
		t.Fatalf("existing file balance changed to %d", treasury)
	}
}

func TestCreateNegativeFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	_, err := CreateStore(path, -1)
	if !errors.Is(err, ErrInvalidArg) {
		t.Fatalf("err = %v, want ErrInvalidArg", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("state file should not be created, stat err = %v", err)
	}
}

func TestRegisterAcceptance(t *testing.T) {
	st, _ := newTestStore(t, 1000)

	// non-passed state is rejected
	p := passedProposal("gip-1", 100, "transfer:audits:100")
	p.State = "voting"
	if _, err := st.Register(p); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("voting register err = %v, want ErrInvalidState", err)
	}

	// empty id is rejected
	p2 := passedProposal("", 100, "transfer:audits:100")
	if _, err := st.Register(p2); !errors.Is(err, ErrInvalidArg) {
		t.Fatalf("empty id err = %v, want ErrInvalidArg", err)
	}

	// passed proposal is stored verbatim
	p3 := passedProposal("gip-3", 100, "transfer:audits:100")
	got, err := st.Register(p3)
	if err != nil {
		t.Fatal(err)
	}
	if got.TimelockEnd != 100 || len(got.Actions) != 1 || got.Actions[0] != "transfer:audits:100" {
		t.Fatalf("stored proposal changed: %+v", got)
	}
	loaded, err := st.GetProposal("gip-3")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != "passed" {
		t.Fatalf("state = %q, want passed", loaded.State)
	}
}

func TestRegisterIdempotentAndConflict(t *testing.T) {
	st, _ := newTestStore(t, 1000)
	p := passedProposal("gip-7", 400, "transfer:audits:25000")
	if _, err := st.Register(p); err != nil {
		t.Fatal(err)
	}

	// identical retry returns the stored record
	again, err := st.Register(p)
	if err != nil {
		t.Fatalf("identical retry: %v", err)
	}
	if again.ID != "gip-7" || again.TimelockEnd != 400 {
		t.Fatalf("retry returned %+v", again)
	}

	// different content conflicts
	pDiff := passedProposal("gip-7", 400, "transfer:audits:25001")
	if _, err := st.Register(pDiff); !errors.Is(err, ErrConflict) {
		t.Fatalf("different content err = %v, want ErrConflict", err)
	}
	pDiff2 := passedProposal("gip-7", 401, "transfer:audits:25000")
	if _, err := st.Register(pDiff2); !errors.Is(err, ErrConflict) {
		t.Fatalf("different timelock err = %v, want ErrConflict", err)
	}

	// the stored record is untouched by the conflicts
	loaded, _ := st.GetProposal("gip-7")
	if loaded.Actions[0] != "transfer:audits:25000" || loaded.TimelockEnd != 400 {
		t.Fatalf("stored record changed after conflict: %+v", loaded)
	}
}

// ---------------------------------------------------------------------------
// execution
// ---------------------------------------------------------------------------

func TestExecuteSuccess(t *testing.T) {
	st, _ := newTestStore(t, 100000)
	p := passedProposal("gip-7", 400, "transfer:audits:25000", "transfer:dev:10000")
	if _, err := st.Register(p); err != nil {
		t.Fatal(err)
	}
	r, err := st.Execute("gip-7", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if r.ProposalID != "gip-7" || r.ExecutedAt != 1000 {
		t.Fatalf("receipt header = %+v", r)
	}
	if r.TreasuryBefore != 100000 || r.TreasuryAfter != 65000 {
		t.Fatalf("treasury = %d -> %d", r.TreasuryBefore, r.TreasuryAfter)
	}
	if len(r.Actions) != 2 {
		t.Fatalf("actions = %d, want 2", len(r.Actions))
	}
	a0 := r.Actions[0]
	if a0.Account != "audits" || a0.Amount != 25000 || a0.BalanceBefore != 0 || a0.BalanceAfter != 25000 {
		t.Fatalf("leg 0 = %+v", a0)
	}
	a1 := r.Actions[1]
	if a1.Account != "dev" || a1.Amount != 10000 || a1.BalanceBefore != 0 || a1.BalanceAfter != 10000 {
		t.Fatalf("leg 1 = %+v", a1)
	}
	treasury, accounts, _ := st.Balance()
	if treasury != 65000 {
		t.Fatalf("treasury = %d", treasury)
	}
	if accounts["audits"] != 25000 || accounts["dev"] != 10000 {
		t.Fatalf("accounts = %v", accounts)
	}
	loaded, _ := st.GetProposal("gip-7")
	if loaded.State != "executed" {
		t.Fatalf("state = %q, want executed", loaded.State)
	}
}

func TestExecuteSameAccountConsecutive(t *testing.T) {
	st, _ := newTestStore(t, 100)
	p := passedProposal("gip-1", 0, "transfer:alice:30", "transfer:alice:20", "transfer:alice:10")
	if _, err := st.Register(p); err != nil {
		t.Fatal(err)
	}
	r, err := st.Execute("gip-1", 1)
	if err != nil {
		t.Fatal(err)
	}
	wantBefore := []int64{0, 30, 50}
	wantAfter := []int64{30, 50, 60}
	for i, a := range r.Actions {
		if a.BalanceBefore != wantBefore[i] || a.BalanceAfter != wantAfter[i] {
			t.Fatalf("leg %d = %+v, want before %d after %d", i, a, wantBefore[i], wantAfter[i])
		}
	}
	_, accounts, _ := st.Balance()
	if accounts["alice"] != 60 {
		t.Fatalf("alice = %d", accounts["alice"])
	}
}

func TestExecuteErrors(t *testing.T) {
	t.Run("not found", func(t *testing.T) {
		st, _ := newTestStore(t, 1000)
		_, err := st.Execute("gip-x", 1000)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("timelock not reached", func(t *testing.T) {
		st, _ := newTestStore(t, 1000)
		st.Register(passedProposal("gip-1", 500, "transfer:audits:100"))
		_, err := st.Execute("gip-1", 499)
		if !errors.Is(err, ErrTimelock) {
			t.Fatalf("err = %v, want ErrTimelock", err)
		}
		// state and balance untouched
		p, _ := st.GetProposal("gip-1")
		if p.State != "passed" {
			t.Fatalf("state = %q", p.State)
		}
		treasury, _, _ := st.Balance()
		if treasury != 1000 {
			t.Fatalf("treasury = %d", treasury)
		}
	})

	t.Run("no actions", func(t *testing.T) {
		st, _ := newTestStore(t, 1000)
		st.Register(passedProposal("gip-1", 0))
		_, err := st.Execute("gip-1", 1)
		if !errors.Is(err, ErrInvalidAction) {
			t.Fatalf("err = %v, want ErrInvalidAction", err)
		}
	})

	t.Run("malformed action", func(t *testing.T) {
		for _, action := range []string{
			"transfer::100",          // empty account
			"transfer:a:b:100",       // colon in amount
			"transfer:a:",            // empty amount
			"transfer:a:abc",         // non-digit
			"transfer:a:-5",          // negative sign
			"transfer:a:0",           // zero
			"transfer:a:00",          // zero
			"transfer:a:9223372036854775808", // int64 overflow
			"bogus:a:100",            // unknown verb
			"transfer",               // malformed
		} {
			st, _ := newTestStore(t, 1000)
			st.Register(passedProposal("gip-1", 0, action))
			_, err := st.Execute("gip-1", 1)
			if !errors.Is(err, ErrInvalidAction) {
				t.Fatalf("action %q: err = %v, want ErrInvalidAction", action, err)
			}
			// no partial state change
			treasury, accounts, _ := st.Balance()
			if treasury != 1000 {
				t.Fatalf("action %q: treasury = %d", action, treasury)
			}
			if len(accounts) != 0 {
				t.Fatalf("action %q: accounts = %v", action, accounts)
			}
			p, _ := st.GetProposal("gip-1")
			if p.State != "passed" {
				t.Fatalf("action %q: state = %q", action, p.State)
			}
		}
	})

	t.Run("insufficient balance", func(t *testing.T) {
		st, _ := newTestStore(t, 100)
		st.Register(passedProposal("gip-1", 0, "transfer:audits:101"))
		_, err := st.Execute("gip-1", 1)
		if !errors.Is(err, ErrInsufficient) {
			t.Fatalf("err = %v, want ErrInsufficient", err)
		}
		treasury, _, _ := st.Balance()
		if treasury != 100 {
			t.Fatalf("treasury = %d, want 100 (unchanged)", treasury)
		}
		p, _ := st.GetProposal("gip-1")
		if p.State != "passed" {
			t.Fatalf("state = %q, want passed", p.State)
		}
	})

	t.Run("sum overflow", func(t *testing.T) {
		st, _ := newTestStore(t, 100)
		st.Register(passedProposal("gip-1", 0,
			"transfer:a:9223372036854775807",
			"transfer:b:1"))
		_, err := st.Execute("gip-1", 1)
		if !errors.Is(err, ErrOverflow) {
			t.Fatalf("err = %v, want ErrOverflow", err)
		}
		treasury, _, _ := st.Balance()
		if treasury != 100 {
			t.Fatalf("treasury = %d", treasury)
		}
	})
}

func TestExecuteIdempotent(t *testing.T) {
	st, _ := newTestStore(t, 100000)
	st.Register(passedProposal("gip-7", 400, "transfer:audits:25000"))
	first, err := st.Execute("gip-7", 1000)
	if err != nil {
		t.Fatal(err)
	}
	// retry with a different time: returns the first receipt, no second debit
	second, err := st.Execute("gip-7", 999999)
	if err != nil {
		t.Fatal(err)
	}
	if second.ExecutedAt != 1000 {
		t.Fatalf("retry ExecutedAt = %d, want 1000", second.ExecutedAt)
	}
	if second.TreasuryAfter != first.TreasuryAfter {
		t.Fatalf("retry TreasuryAfter = %d, want %d", second.TreasuryAfter, first.TreasuryAfter)
	}
	treasury, _, _ := st.Balance()
	if treasury != 75000 {
		t.Fatalf("treasury = %d, want 75000 (single debit)", treasury)
	}
	receipts, err := st.Receipts()
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 1 {
		t.Fatalf("receipts = %d, want 1", len(receipts))
	}
}

func TestReceiptsOrder(t *testing.T) {
	st, _ := newTestStore(t, 100000)
	for _, id := range []string{"gip-1", "gip-2", "gip-3"} {
		st.Register(passedProposal(id, 0, "transfer:audits:1000"))
	}
	for _, id := range []string{"gip-2", "gip-1", "gip-3"} {
		if _, err := st.Execute(id, 100); err != nil {
			t.Fatal(err)
		}
	}
	receipts, err := st.Receipts()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"gip-2", "gip-1", "gip-3"}
	for i, r := range receipts {
		if r.ProposalID != want[i] {
			t.Fatalf("receipt %d = %s, want %s", i, r.ProposalID, want[i])
		}
	}
	got, err := st.GetReceipt("gip-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ExecutedAt != 100 {
		t.Fatalf("gip-1 ExecutedAt = %d", got.ExecutedAt)
	}
}

// ---------------------------------------------------------------------------
// persistence and corruption
// ---------------------------------------------------------------------------

func TestPersistenceAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	st, err := CreateStore(path, 50000)
	if err != nil {
		t.Fatal(err)
	}
	st.Register(passedProposal("gip-7", 400, "transfer:audits:25000"))
	if _, err := st.Execute("gip-7", 1000); err != nil {
		t.Fatal(err)
	}

	st2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	treasury, accounts, err := st2.Balance()
	if err != nil {
		t.Fatal(err)
	}
	if treasury != 25000 || accounts["audits"] != 25000 {
		t.Fatalf("after reopen: treasury=%d accounts=%v", treasury, accounts)
	}
	p, _ := st2.GetProposal("gip-7")
	if p.State != "executed" {
		t.Fatalf("state = %q", p.State)
	}
	r, err := st2.GetReceipt("gip-7")
	if err != nil {
		t.Fatal(err)
	}
	if r.ExecutedAt != 1000 || r.TreasuryAfter != 25000 {
		t.Fatalf("receipt after reopen = %+v", r)
	}
	// retry after reopen still returns the original receipt
	r2, err := st2.Execute("gip-7", 2000)
	if err != nil {
		t.Fatal(err)
	}
	if r2.ExecutedAt != 1000 {
		t.Fatalf("retry after reopen ExecutedAt = %d", r2.ExecutedAt)
	}
}

func TestCorruptFileRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := OpenStore(path)
	if !errors.Is(err, ErrCorruptState) {
		t.Fatalf("err = %v, want ErrCorruptState", err)
	}
	// file untouched
	data, _ := os.ReadFile(path)
	if string(data) != "{not json" {
		t.Fatalf("corrupt file was modified: %q", data)
	}
}

func TestTamperedFileRejected(t *testing.T) {
	st, path := newTestStore(t, 1000)
	st.Register(passedProposal("gip-1", 0, "transfer:audits:100"))
	if _, err := st.Execute("gip-1", 1); err != nil {
		t.Fatal(err)
	}
	// tamper: inflate the stored treasury balance
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Replace(data, []byte(`"treasury": 900`), []byte(`"treasury": 99999`), 1)
	if bytes.Equal(tampered, data) {
		t.Fatal("could not tamper with treasury value")
	}
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = OpenStore(path)
	if !errors.Is(err, ErrCorruptState) {
		t.Fatalf("err = %v, want ErrCorruptState", err)
	}
	// the tampered file is left untouched, not rebuilt
	data2, _ := os.ReadFile(path)
	if !bytes.Equal(data2, tampered) {
		t.Fatal("tampered file was modified")
	}
}

func TestMissingFileRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	_, err := OpenStore(path)
	if !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("err = %v, want ErrFileNotFound", err)
	}
}

func TestSaveFailureKeepsOldState(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, permission test would not fail")
	}
	st, path := newTestStore(t, 1000)
	st.Register(passedProposal("gip-1", 0, "transfer:audits:100"))

	// make the directory unwritable so the commit cannot create a temp file
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o755)
	_, err := st.Execute("gip-1", 1)
	if err == nil {
		t.Fatal("execute should fail when the state file cannot be committed")
	}
	// restore and verify the last complete state is intact
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	st2, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen after failed save: %v", err)
	}
	treasury, _, _ := st2.Balance()
	if treasury != 1000 {
		t.Fatalf("treasury = %d, want 1000 (last complete state)", treasury)
	}
	p, _ := st2.GetProposal("gip-1")
	if p.State != "passed" {
		t.Fatalf("state = %q, want passed", p.State)
	}
}

func TestMultipleStoresIndependent(t *testing.T) {
	dir := t.TempDir()
	pathA := filepath.Join(dir, "a.json")
	pathB := filepath.Join(dir, "b.json")
	stA, err := CreateStore(pathA, 1000)
	if err != nil {
		t.Fatal(err)
	}
	stB, err := CreateStore(pathB, 1000)
	if err != nil {
		t.Fatal(err)
	}
	stA.Register(passedProposal("gip-1", 0, "transfer:audits:1000"))
	if _, err := stA.Execute("gip-1", 1); err != nil {
		t.Fatal(err)
	}
	treasuryA, _, _ := stA.Balance()
	treasuryB, _, _ := stB.Balance()
	if treasuryA != 0 {
		t.Fatalf("A treasury = %d", treasuryA)
	}
	if treasuryB != 1000 {
		t.Fatalf("B treasury = %d, want 1000 (independent)", treasuryB)
	}
}

// ---------------------------------------------------------------------------
// concurrency
// ---------------------------------------------------------------------------

func TestConcurrentGoroutines(t *testing.T) {
	st, _ := newTestStore(t, 100000)
	for i := 0; i < 10; i++ {
		st.Register(passedProposal(fmt.Sprintf("gip-%d", i), 0, fmt.Sprintf("transfer:acct%d:1000", i)))
	}
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := st.Execute(fmt.Sprintf("gip-%d", i), 100)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
	}
	treasury, accounts, _ := st.Balance()
	if treasury != 90000 {
		t.Fatalf("treasury = %d, want 90000", treasury)
	}
	if len(accounts) != 10 {
		t.Fatalf("accounts = %d, want 10", len(accounts))
	}
	receipts, _ := st.Receipts()
	if len(receipts) != 10 {
		t.Fatalf("receipts = %d, want 10", len(receipts))
	}
}

func TestConcurrentGoroutinesSameProposal(t *testing.T) {
	st, _ := newTestStore(t, 100000)
	st.Register(passedProposal("gip-1", 0, "transfer:audits:1000"))
	var wg sync.WaitGroup
	results := make(chan *Receipt, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := st.Execute("gip-1", int64(100+i))
			if err != nil {
				t.Errorf("execute: %v", err)
				return
			}
			results <- r
		}(i)
	}
	wg.Wait()
	close(results)
	var first *Receipt
	for r := range results {
		if first == nil {
			first = r
		} else if r.ExecutedAt != first.ExecutedAt || r.TreasuryAfter != first.TreasuryAfter {
			t.Fatalf("got different receipts: %+v vs %+v", first, r)
		}
	}
	treasury, _, _ := st.Balance()
	if treasury != 99000 {
		t.Fatalf("treasury = %d, want 99000 (single debit)", treasury)
	}
	receipts, _ := st.Receipts()
	if len(receipts) != 1 {
		t.Fatalf("receipts = %d, want 1", len(receipts))
	}
}

func TestConcurrentProcessesSameProposal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	st, err := CreateStore(path, 100000)
	if err != nil {
		t.Fatal(err)
	}
	st.Register(passedProposal("gip-1", 0, "transfer:audits:1000"))

	workers := 8
	var wg sync.WaitGroup
	out := make(chan string, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=TestWorkerExecute")
			cmd.Env = append(os.Environ(),
				"GOVFLOW_TEST_WORKER=execute",
				"GOVFLOW_TEST_FILE="+path,
				"GOVFLOW_TEST_ID=gip-1",
				fmt.Sprintf("GOVFLOW_TEST_NOW=%d", 100+i),
			)
			var stdout bytes.Buffer
			cmd.Stdout = &stdout
			if err := cmd.Run(); err != nil {
				t.Errorf("worker: %v", err)
				return
			}
			out <- strings.TrimSpace(stdout.String())
		}(i)
	}
	wg.Wait()
	close(out)
	executed := 0
	for line := range out {
		if line == "EXECUTED" {
			executed++
		}
	}
	if executed != 1 {
		t.Fatalf("EXECUTED count = %d, want exactly 1", executed)
	}
	treasury, _, _ := st.Balance()
	if treasury != 99000 {
		t.Fatalf("treasury = %d, want 99000", treasury)
	}
	receipts, _ := st.Receipts()
	if len(receipts) != 1 {
		t.Fatalf("receipts = %d, want 1", len(receipts))
	}
}

func TestConcurrentProcessesDifferentProposals(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	st, err := CreateStore(path, 100000)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		st.Register(passedProposal(fmt.Sprintf("gip-%d", i), 0, fmt.Sprintf("transfer:acct%d:25000", i)))
	}
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=TestWorkerExecute")
			cmd.Env = append(os.Environ(),
				"GOVFLOW_TEST_WORKER=execute",
				"GOVFLOW_TEST_FILE="+path,
				fmt.Sprintf("GOVFLOW_TEST_ID=gip-%d", i),
				"GOVFLOW_TEST_NOW=1000",
			)
			if err := cmd.Run(); err != nil {
				errs <- fmt.Errorf("worker %d: %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	treasury, accounts, _ := st.Balance()
	if treasury != 0 {
		t.Fatalf("treasury = %d, want 0", treasury)
	}
	for i := 0; i < 4; i++ {
		if accounts[fmt.Sprintf("acct%d", i)] != 25000 {
			t.Fatalf("acct%d = %d, want 25000", i, accounts[fmt.Sprintf("acct%d", i)])
		}
	}
	receipts, _ := st.Receipts()
	if len(receipts) != 4 {
		t.Fatalf("receipts = %d, want 4", len(receipts))
	}
}

func TestConcurrentProcessesInsufficient(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	st, err := CreateStore(path, 50000)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		st.Register(passedProposal(fmt.Sprintf("gip-%d", i), 0, fmt.Sprintf("transfer:acct%d:30000", i)))
	}
	var wg sync.WaitGroup
	type result struct {
		worker int
		err    error
	}
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=TestWorkerExecute")
			cmd.Env = append(os.Environ(),
				"GOVFLOW_TEST_WORKER=execute",
				"GOVFLOW_TEST_FILE="+path,
				fmt.Sprintf("GOVFLOW_TEST_ID=gip-%d", i),
				"GOVFLOW_TEST_NOW=1000",
			)
			err := cmd.Run()
			results <- result{i, err}
		}(i)
	}
	wg.Wait()
	close(results)
	successes := 0
	for r := range results {
		if r.err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successes = %d, want exactly 1", successes)
	}
	treasury, _, _ := st.Balance()
	if treasury != 20000 {
		t.Fatalf("treasury = %d, want 20000", treasury)
	}
	receipts, _ := st.Receipts()
	if len(receipts) != 1 {
		t.Fatalf("receipts = %d, want 1", len(receipts))
	}
}

// TestWorkerExecute is not a real test: it is the worker entry point re-executed
// by the concurrency tests via GOVFLOW_TEST_WORKER=execute.
func TestWorkerExecute(t *testing.T) {}
