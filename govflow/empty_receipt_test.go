package govflow

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件回归“成功执行必须包含实际动作”在读取路径上的判定：无动作提案被
// 写成 executed、再配一份动作列表为空（或缺失/为 null）的执行凭据时，
// 即使凭据编号、执行时间、成功序号合法且余额能够核对一致，也必须判整份
// 状态文件损坏，而不是被当成一次成功执行记录接受。

// writeStateDoc 把结构化状态文档写入临时状态文件，供手工构造“应用自身
// 永远不会写出”的损坏内容（如 executed 无动作提案 + 空动作凭据）。
func writeStateDoc(t *testing.T, path string, doc map[string]any) {
	t.Helper()
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// emptyReceiptTaskDoc 构造任务描述的典型损坏文件：初始/当前余额均为 100、
// 尚无收款账户记录；登记提案 gip-empty 没有任何动作却被写成 executed，
// 关联凭据编号、执行时间与成功序号都合法，但动作列表为 []。
func emptyReceiptTaskDoc() map[string]any {
	return map[string]any{
		"magic":            stateMagic,
		"version":          stateVersion,
		"initial_treasury": float64(100),
		"treasury":         float64(100),
		"balances":         map[string]any{},
		"proposals": map[string]any{
			"gip-empty": map[string]any{
				"id":           "gip-empty",
				"state":        "executed",
				"timelock_end": float64(0),
				"actions":      []any{},
			},
		},
		"receipts": []any{
			map[string]any{
				"proposal_id": "gip-empty",
				"executed_at": float64(0),
				"order":       float64(0),
				"actions":     []any{},
			},
		},
		"vote_proposals": map[string]any{},
	}
}

// TestEmptyActionReceiptCorruptsWholeState：任务主例。空动作凭据让整份
// 状态文件打开失败——错误指出提案编号并说明凭据没有动作。打开失败即不
// 返回可用句柄，余额、提案与凭据查询都不可能得到部分结果，对该提案执行
// 也无从当作已完成重试；init 不覆盖损坏文件；失败后原文件逐字节保留
// （查询与执行在损坏内容上的逐操作拒绝见
// TestEmptyReceiptSpottedOnAlreadyOpenStore）。
func TestEmptyActionReceiptCorruptsWholeState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "treasury.json")
	doc := emptyReceiptTaskDoc()
	writeStateDoc(t, path, doc)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if s != nil {
		s.Close()
	}
	if !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("Open err=%v, want ErrStateCorrupt", err)
	}
	for _, want := range []string{"receipt 0", `"gip-empty"`, "has no actions"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must contain %q", err.Error(), want)
		}
	}
	// 反复打开结论一致，不会第二次“修复”或返回可用句柄。
	if again, aerr := Open(path); again != nil || !errors.Is(aerr, ErrStateCorrupt) {
		again.Close()
		t.Fatalf("second Open store=%v err=%v, want nil + ErrStateCorrupt", again, aerr)
	}

	// init 不得重建或覆盖损坏文件。
	if _, err := InitTreasury(path, 0); !errors.Is(err, ErrTreasuryAlreadyInit) {
		t.Fatalf("InitTreasury over corrupt file err=%v, want ErrTreasuryAlreadyInit", err)
	}

	// 失败保留原文件：不删除空凭据、不补造转账、不把提案改回 passed。
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("corrupt file was modified")
	}
}

// TestReceiptActionsFieldPresenceAndShape：凭据的 actions 缺失、为 null 或
// 写成非数组类型时，与空数组一样判整份文件损坏，错误带凭据位置、提案
// 编号、字段名与缺失/空值/类型不符原因。
func TestReceiptActionsFieldPresenceAndShape(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name    string
		actions any // nil 表示删除字段
		delete  bool
		want    []string
	}{
		{"empty array", []any{}, false, []string{`"gip-empty"`, "has no actions"}},
		{"missing field", nil, true, []string{`"gip-empty"`, `"actions"`, "is missing"}},
		{"null", nil, false, []string{`"gip-empty"`, `"actions"`, "is null"}},
		{"object", map[string]any{}, false, []string{`"gip-empty"`, `"actions"`, "has wrong type", "object"}},
		{"string", "[]", false, []string{`"gip-empty"`, `"actions"`, "has wrong type", "string"}},
		{"number", float64(0), false, []string{`"gip-empty"`, `"actions"`, "has wrong type", "number"}},
		{"boolean", false, false, []string{`"gip-empty"`, `"actions"`, "has wrong type", "boolean"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, "doc-"+strings.ReplaceAll(tc.name, " ", "-")+".json")
			doc := emptyReceiptTaskDoc()
			rcpt := doc["receipts"].([]any)[0].(map[string]any)
			switch {
			case tc.delete:
				delete(rcpt, "actions")
			case tc.name == "null":
				rcpt["actions"] = nil
			default:
				rcpt["actions"] = tc.actions
			}
			writeStateDoc(t, path, doc)
			raw, _ := os.ReadFile(path)

			s, err := Open(path)
			if !errors.Is(err, ErrStateCorrupt) {
				if s != nil {
					s.Close()
				}
				t.Fatalf("Open err=%v, want ErrStateCorrupt", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q must contain %q", err.Error(), want)
				}
			}
			if got, _ := os.ReadFile(path); string(got) != string(raw) {
				t.Fatalf("corrupt file was modified")
			}
		})
	}

	// 数组写出但元素不是动作留痕对象（数字、字符串）同样拒绝整份文件，
	// 错误仍能定位到提案与 actions 字段。
	for _, tc := range []struct {
		name  string
		elems []any
	}{
		{"null element", []any{nil}},
		{"number element", []any{float64(1)}},
		{"string element", []any{"transfer:a:1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, "elem-"+strings.ReplaceAll(tc.name, " ", "-")+".json")
			doc := emptyReceiptTaskDoc()
			rcpt := doc["receipts"].([]any)[0].(map[string]any)
			rcpt["actions"] = tc.elems
			writeStateDoc(t, path, doc)

			s, err := Open(path)
			if !errors.Is(err, ErrStateCorrupt) {
				if s != nil {
					s.Close()
				}
				t.Fatalf("Open err=%v, want ErrStateCorrupt", err)
			}
			if !strings.Contains(err.Error(), `"gip-empty"`) {
				t.Fatalf("error %q must name the proposal", err.Error())
			}
		})
	}
}

// TestEmptyActionReceiptFromVotingProposalCorrupt：投票通过来源适用同一要求。
// 提案无动作，投票明细与计票通过结论完全合法，但状态被写成 executed 并配
// 空动作凭据时，合法投票内容不能使空凭据成为有效执行凭据。
func TestEmptyActionReceiptFromVotingProposalCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "treasury.json")
	store, err := InitTreasury(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	in := baseVoteInput("gip-vote")
	in.Actions = nil // 无动作投票提案
	mustCreateVote(t, store, in)
	if _, err := store.CastVote("gip-vote", "alice", true, 150); err != nil {
		t.Fatal(err)
	}
	tally, err := store.TallyVote("gip-vote", 200)
	if err != nil || !tally.Passed {
		t.Fatalf("tally=%+v err=%v, want proposal passed with fully legal votes", tally, err)
	}
	// 通过后尝试执行：继续以没有动作为由拒绝，状态保持 passed、无凭据。
	if rcpt, eerr := store.Execute("gip-vote", 300); !errors.Is(eerr, ErrExecutionRejected) ||
		!strings.Contains(eerr.Error(), "no actions") || rcpt != nil {
		t.Fatalf("execute empty-action vote rcpt=%+v err=%v, want no-actions rejection", rcpt, eerr)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// 手工把通过提案改成 executed 并补一份空动作凭据（应用自身不会写出）。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	vp := doc["vote_proposals"].(map[string]any)["gip-vote"].(map[string]any)
	vp["state"] = "executed"
	doc["receipts"] = []any{
		map[string]any{
			"proposal_id": "gip-vote",
			"executed_at": float64(300),
			"order":       float64(0),
			"actions":     []any{},
		},
	}
	corrupt, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	corrupt = append(corrupt, '\n')
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}

	s, oerr := Open(path)
	if !errors.Is(oerr, ErrStateCorrupt) {
		if s != nil {
			s.Close()
		}
		t.Fatalf("Open vote-source corrupt err=%v, want ErrStateCorrupt", oerr)
	}
	for _, want := range []string{`"gip-vote"`, "has no actions"} {
		if !strings.Contains(oerr.Error(), want) {
			t.Fatalf("error %q must contain %q", oerr.Error(), want)
		}
	}
	if got, _ := os.ReadFile(path); string(got) != string(corrupt) {
		t.Fatalf("corrupt file was modified")
	}
}

// TestNormalReceiptsDoNotMaskEmptyReceipt：同一份文件里即使另有完全合法的
// 转账凭据（无论排在空凭据之前还是之后），即使本次只想查询正常提案，
// 空凭据都让整份文件打开失败。
func TestNormalReceiptsDoNotMaskEmptyReceipt(t *testing.T) {
	dir := t.TempDir()

	// 先产生一份含合法凭据的状态：gip-good 转给 acct 10，余额 1000->990。
	goodPath := filepath.Join(dir, "good.json")
	store, err := InitTreasury(goodPath, 1000)
	if err != nil {
		t.Fatal(err)
	}
	mustRegister(t, store, "gip-good", 0, "transfer:acct:10")
	if _, err := store.Execute("gip-good", 0); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	goodRaw, err := os.ReadFile(goodPath)
	if err != nil {
		t.Fatal(err)
	}

	emptyProposal := map[string]any{
		"id": "gip-empty", "state": "executed",
		"timelock_end": float64(0), "actions": []any{},
	}
	emptyReceipt := func(order float64) map[string]any {
		return map[string]any{
			"proposal_id": "gip-empty",
			"executed_at": float64(0),
			"order":       order,
			"actions":     []any{},
		}
	}

	// 正常凭据在前、空凭据在后。
	t.Run("empty receipt after normal receipt", func(t *testing.T) {
		path := filepath.Join(dir, "after.json")
		var doc map[string]any
		if err := json.Unmarshal(goodRaw, &doc); err != nil {
			t.Fatal(err)
		}
		doc["proposals"].(map[string]any)["gip-empty"] = emptyProposal
		receipts := append(doc["receipts"].([]any), emptyReceipt(1))
		doc["receipts"] = receipts
		writeStateDoc(t, path, doc)
		assertWholeFileCorrupt(t, path, "gip-empty", "gip-good")
	})

	// 空凭据在前、正常凭据在后（合法凭据 order 改成 1）。
	t.Run("empty receipt before normal receipt", func(t *testing.T) {
		path := filepath.Join(dir, "before.json")
		var doc map[string]any
		if err := json.Unmarshal(goodRaw, &doc); err != nil {
			t.Fatal(err)
		}
		doc["proposals"].(map[string]any)["gip-empty"] = emptyProposal
		existing := doc["receipts"].([]any)[0].(map[string]any)
		existing["order"] = float64(1)
		doc["receipts"] = []any{emptyReceipt(0), existing}
		writeStateDoc(t, path, doc)
		assertWholeFileCorrupt(t, path, "gip-empty", "gip-good")
	})
}

// assertWholeFileCorrupt 断言含空凭据的文件整份打不开：即使查询正常提案
// 或正常凭据也没有任何部分成功结果，且原文件不被改写。
func assertWholeFileCorrupt(t *testing.T, path, emptyID, goodID string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if !errors.Is(err, ErrStateCorrupt) {
		if s != nil {
			s.Close()
		}
		t.Fatalf("Open err=%v, want ErrStateCorrupt", err)
	}
	if !strings.Contains(err.Error(), `"`+emptyID+`"`) ||
		!strings.Contains(err.Error(), "has no actions") {
		t.Fatalf("error must name %s and explain empty actions: %v", emptyID, err)
	}
	// 打开失败意味着连正常提案/凭据也查不到：不存在能返回部分结果的句柄。
	if _, oerr := Open(path); !errors.Is(oerr, ErrStateCorrupt) {
		t.Fatalf("querying %s must not succeed on a file that also contains %s: %v",
			goodID, emptyID, oerr)
	}
	if got, _ := os.ReadFile(path); string(got) != string(raw) {
		t.Fatalf("corrupt file was modified")
	}
}

// TestEmptyReceiptSpottedOnAlreadyOpenStore：已经打开的资金库随后读到
// 空凭据内容时，每次操作都重新读取并按状态损坏失败，而不是沿用打开时
// 的内存视图返回旧余额/旧凭据。
func TestEmptyReceiptSpottedOnAlreadyOpenStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "treasury.json")
	store, err := InitTreasury(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	// 无动作提案尚未执行：登记与查询正常。
	if existed, err := store.Register("gip-empty", 0, nil); err != nil || existed {
		t.Fatalf("register no-action proposal: existed=%v err=%v", existed, err)
	}
	record, ok, err := store.Proposal("gip-empty")
	if err != nil || !ok || record.State != "passed" {
		t.Fatalf("query before swap: %+v ok=%v err=%v", record, ok, err)
	}

	// 文件在句柄存活期间被换成“executed + 空凭据”的损坏内容。
	writeStateDoc(t, path, emptyReceiptTaskDoc())
	corrupt, _ := os.ReadFile(path)

	for _, op := range []struct {
		name string
		run  func() error
	}{
		{"BalanceSnapshot", func() error { _, e := store.BalanceSnapshot(); return e }},
		{"TreasuryBalance", func() error { _, e := store.TreasuryBalance(); return e }},
		{"Proposal", func() error { _, _, e := store.Proposal("gip-empty"); return e }},
		{"Proposals", func() error { _, e := store.Proposals(); return e }},
		{"Receipt", func() error { _, _, e := store.Receipt("gip-empty"); return e }},
		{"Receipts", func() error { _, e := store.Receipts(); return e }},
	} {
		if err := op.run(); !errors.Is(err, ErrStateCorrupt) {
			t.Fatalf("%s after swap err=%v, want ErrStateCorrupt", op.name, err)
		}
	}
	if rcpt, err := store.Execute("gip-empty", 0); !errors.Is(err, ErrStateCorrupt) || rcpt != nil {
		t.Fatalf("Execute after swap rcpt=%+v err=%v, want nil + ErrStateCorrupt", rcpt, err)
	}
	if got, _ := os.ReadFile(path); string(got) != string(corrupt) {
		t.Fatalf("corrupt file was modified by rejected operations")
	}
	store.Close()
}

// TestNoActionUnexecutedProposalUsageUnchanged：无动作提案尚未执行时的
// 现有用法保持不变——登记/创建投票、正常查询、按既有规则投票计票都照常；
// 通过后执行继续以没有动作为由拒绝，状态、余额与凭据记录保持原样。
func TestNoActionUnexecutedProposalUsageUnchanged(t *testing.T) {
	// 登记来源：可登记（含空切片与 nil 等价重试）、可查询，执行拒绝且不留痕。
	store, path := openTempStore(t, 100)
	defer store.Close()
	if existed, err := store.Register("gip-nil", 0, nil); err != nil || existed {
		t.Fatalf("register nil actions: existed=%v err=%v", existed, err)
	}
	if existed, err := store.Register("gip-nil", 0, []string{}); err != nil || !existed {
		t.Fatalf("empty-slice retry: existed=%v err=%v", existed, err)
	}
	rec, ok, err := store.Proposal("gip-nil")
	if err != nil || !ok || rec.State != "passed" || len(rec.Actions) != 0 {
		t.Fatalf("query no-action proposal: %+v ok=%v err=%v", rec, ok, err)
	}
	if rcpt, eerr := store.Execute("gip-nil", 0); !errors.Is(eerr, ErrExecutionRejected) ||
		!strings.Contains(eerr.Error(), "no actions") || rcpt != nil {
		t.Fatalf("execute register no-action rcpt=%+v err=%v", rcpt, eerr)
	}
	if bal, _ := store.TreasuryBalance(); bal != 100 {
		t.Fatalf("treasury changed: %d", bal)
	}
	if rs, _ := store.Receipts(); len(rs) != 0 {
		t.Fatalf("receipt created for no-action rejection: %+v", rs)
	}
	rec, _, _ = store.Proposal("gip-nil")
	if rec.State != "passed" {
		t.Fatalf("state changed after rejected execute: %s", rec.State)
	}

	// 投票来源：无动作也可创建、投票与计票（按既有规则通过），执行拒绝。
	in := baseVoteInput("gip-vote-empty")
	in.Actions = []string{}
	mustCreateVote(t, store, in)
	if _, err := store.CastVote("gip-vote-empty", "alice", true, 150); err != nil {
		t.Fatal(err)
	}
	tally, err := store.TallyVote("gip-vote-empty", 200)
	if err != nil || !tally.Passed {
		t.Fatalf("tally=%+v err=%v", tally, err)
	}
	if rcpt, eerr := store.Execute("gip-vote-empty", 300); !errors.Is(eerr, ErrExecutionRejected) ||
		!strings.Contains(eerr.Error(), "no actions") || rcpt != nil {
		t.Fatalf("execute vote no-action rcpt=%+v err=%v", rcpt, eerr)
	}
	vp, ok, err := store.VoteProposal("gip-vote-empty")
	if err != nil || !ok || vp.State != "passed" {
		t.Fatalf("vote proposal state after rejected execute: %+v ok=%v err=%v", vp, ok, err)
	}
	// 再次计票返回首次结论，不转账。
	again, err := store.TallyVote("gip-vote-empty", 201)
	if err != nil || !again.Passed {
		t.Fatalf("retally=%+v err=%v", again, err)
	}
	if bal, _ := store.TreasuryBalance(); bal != 100 {
		t.Fatalf("treasury changed after no-action vote flow: %d", bal)
	}

	// 重开：未执行的无动作提案（两种来源）仍是合法状态，查询不受影响。
	store.Close()
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if rec, ok, _ := reopened.Proposal("gip-nil"); !ok || rec.State != "passed" {
		t.Fatalf("register proposal after reopen: %+v ok=%v", rec, ok)
	}
	if vp, ok, _ := reopened.VoteProposal("gip-vote-empty"); !ok || vp.State != "passed" {
		t.Fatalf("vote proposal after reopen: %+v ok=%v", vp, ok)
	}
	if bal, _ := reopened.TreasuryBalance(); bal != 100 {
		t.Fatalf("treasury after reopen: %d", bal)
	}
	if rs, _ := reopened.Receipts(); len(rs) != 0 {
		t.Fatalf("receipts after reopen: %+v", rs)
	}
}

// TestRealTransferReceiptStillReadsIncludingZeroTreasury：含真实转账动作的
// 合法凭据继续按已有规则读取；转账后资金库余额恰好为零也是有效结果，
// 重试仍返回首次凭据。
func TestRealTransferReceiptStillReadsIncludingZeroTreasury(t *testing.T) {
	store, path := openTempStore(t, 100)
	defer store.Close()
	mustRegister(t, store, "gip-zero", 0, "transfer:acct:40", "transfer:acct:60")
	first, err := store.Execute("gip-zero", 0)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(first.Actions) != 2 || first.Actions[1].Treasury.After != 0 {
		t.Fatalf("unexpected receipt: %+v", first)
	}
	if bal, _ := store.TreasuryBalance(); bal != 0 {
		t.Fatalf("treasury=%d, want 0", bal)
	}
	store.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("zero-treasury receipt must still read: %v", err)
	}
	defer reopened.Close()
	if bal, _ := reopened.TreasuryBalance(); bal != 0 {
		t.Fatalf("treasury after reopen=%d, want 0", bal)
	}
	if snap, err := reopened.BalanceSnapshot(); err != nil || snap.Treasury != 0 || snap.Balances["acct"] != 100 {
		t.Fatalf("snapshot=%+v err=%v", snap, err)
	}
	again, err := reopened.Execute("gip-zero", 999)
	if err != nil || again.Order != first.Order || again.ExecutedAt != first.ExecutedAt ||
		len(again.Actions) != 2 {
		t.Fatalf("retry receipt=%+v err=%v", again, err)
	}
}
