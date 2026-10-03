package govflow

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildRichStateFile 通过公开 API 构造一份覆盖全部记录类型的状态文件：
// 已执行的登记提案、含委托的投票中提案、已投赞成票并计票通过且执行的
// 投票提案（带余额变动凭据）。返回路径与文件字节，供按文本注入损坏变体。
func buildRichStateFile(t *testing.T) (string, []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "treasury.json")
	store, err := InitTreasury(path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	mustRegister(t, store, "p1", 0, "transfer:acct:100")
	if _, err := store.Execute("p1", 0); err != nil {
		t.Fatal(err)
	}
	// 含委托的投票中提案（不投票、不计票），覆盖 delegations 记录。
	del := &CreateVoteInput{
		ID: "v2",
		Members: []VoteMember{
			{ID: "alice", Weight: 300}, {ID: "bob", Weight: 200},
		},
		Delegations: []Delegation{{From: "bob", To: "alice"}},
		Quorum:      500, StartAt: 0, Deadline: 10, TimelockEnd: 10,
	}
	if _, existed, err := store.CreateVoteProposal(del); err != nil || existed {
		t.Fatalf("create v2: existed=%v err=%v", existed, err)
	}
	// 已计票通过并执行的投票提案：ballots/tally/执行凭据都落盘。
	v1 := &CreateVoteInput{
		ID: "v1", Members: []VoteMember{{ID: "alice", Weight: 600}},
		Quorum: 600, StartAt: 0, Deadline: 10, TimelockEnd: 10,
		Actions: []string{"transfer:vc:50"},
	}
	if _, existed, err := store.CreateVoteProposal(v1); err != nil || existed {
		t.Fatalf("create v1: existed=%v err=%v", existed, err)
	}
	if _, err := store.CastVote("v1", "alice", true, 5); err != nil {
		t.Fatal(err)
	}
	if r, err := store.TallyVote("v1", 10); err != nil || !r.Passed {
		t.Fatalf("tally v1: %+v err=%v", r, err)
	}
	if _, err := store.Execute("v1", 10); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, raw
}

// replaceOnce 在 s 中替换一处锚点；锚点不存在即测试失败，避免“没改到”
// 却因为别的原因被拒绝而让用例失去意义。
func replaceOnce(t *testing.T, s, old, new string) string {
	t.Helper()
	if !strings.Contains(s, old) {
		t.Fatalf("anchor not found in state file:\n%s", old)
	}
	return strings.Replace(s, old, new, 1)
}

// TestWrongCaseFixedFieldsRejected：固定字段名的大小写变体必须拒绝整个文件，
// 覆盖顶层、登记提案、投票提案、成员/委托/票据/计票、执行凭据与余额变动。
func TestWrongCaseFixedFieldsRejected(t *testing.T) {
	path, good := buildRichStateFile(t)

	cases := map[string]struct {
		mutate func(string) string
		// want 是错误信息中必须出现的片段：被拒字段名及所在记录定位。
		want []string
	}{
		"top treasury renamed": {
			func(s string) string {
				return replaceOnce(t, s, `  "treasury": 850,`, `  "Treasury": 850,`)
			},
			[]string{`"Treasury"`, "treasury"},
		},
		"top treasury duplicate same value": {
			// 即使大小写变体与正确字段同值、且与凭据重放完全一致也拒绝。
			func(s string) string {
				return replaceOnce(t, s, "  \"treasury\": 850,\n",
					"  \"treasury\": 850,\n  \"Treasury\": 850,\n")
			},
			[]string{`"Treasury"`},
		},
		"top magic renamed": {
			func(s string) string {
				return replaceOnce(t, s, `  "magic": "govflow-treasury-state",`,
					`  "Magic": "govflow-treasury-state",`)
			},
			[]string{`"Magic"`, "magic"},
		},
		"top vote_proposals renamed": {
			func(s string) string {
				return replaceOnce(t, s, `  "vote_proposals": {`, `  "Vote_proposals": {`)
			},
			[]string{`"Vote_proposals"`},
		},
		"top exact duplicate key": {
			func(s string) string {
				return replaceOnce(t, s, "  \"version\": 1,\n",
					"  \"version\": 1,\n  \"version\": 1,\n")
			},
			[]string{`duplicate key "version"`},
		},
		"top unknown field": {
			func(s string) string {
				return replaceOnce(t, s, "  \"magic\":",
					"  \"bogus\": 1,\n  \"magic\":")
			},
			[]string{`unknown field "bogus"`},
		},
		"registered proposal state renamed": {
			func(s string) string {
				return replaceOnce(t, s,
					"    \"p1\": {\n      \"id\": \"p1\",\n      \"state\": \"executed\",",
					"    \"p1\": {\n      \"id\": \"p1\",\n      \"State\": \"executed\",")
			},
			[]string{`"State"`, `id="p1"`},
		},
		"registered proposal state plus State same value": {
			// 同一条记录里 state 与 State 同值：仍是冲突写法，拒绝。
			func(s string) string {
				return replaceOnce(t, s,
					"      \"id\": \"p1\",\n      \"state\": \"executed\",\n",
					"      \"id\": \"p1\",\n      \"state\": \"executed\",\n      \"State\": \"executed\",\n")
			},
			[]string{`"State"`, `id="p1"`},
		},
		"registered proposal timelock variant": {
			func(s string) string {
				return replaceOnce(t, s,
					"      \"id\": \"p1\",\n      \"state\": \"executed\",\n      \"timelock_end\": 0,",
					"      \"id\": \"p1\",\n      \"state\": \"executed\",\n      \"Timelock_end\": 0,")
			},
			[]string{`"Timelock_end"`, `id="p1"`},
		},
		"receipt proposal_id variant": {
			func(s string) string {
				return replaceOnce(t, s, `      "proposal_id": "p1",`, `      "Proposal_id": "p1",`)
			},
			[]string{`"Proposal_id"`, "/receipts[0]"},
		},
		"receipt order variant": {
			func(s string) string {
				return replaceOnce(t, s, `      "order": 0,`, `      "Order": 0,`)
			},
			[]string{`"Order"`, "/receipts[0]"},
		},
		"action receipt index variant": {
			func(s string) string {
				return replaceOnce(t, s,
					"          \"index\": 0,\n          \"action\": \"transfer:acct:100\",",
					"          \"Index\": 0,\n          \"action\": \"transfer:acct:100\",")
			},
			[]string{`"Index"`, "/receipts[0]/actions[0]"},
		},
		"action receipt treasury object variant": {
			func(s string) string {
				return replaceOnce(t, s,
					"          \"treasury\": {\n            \"account\": \"treasury\",",
					"          \"Treasury\": {\n            \"account\": \"treasury\",")
			},
			[]string{`"Treasury"`, "/receipts[0]/actions[0]"},
		},
		"balance update account variant": {
			func(s string) string {
				return replaceOnce(t, s,
					"          \"treasury\": {\n            \"account\": \"treasury\",\n            \"before\": 1000,",
					"          \"treasury\": {\n            \"Account\": \"treasury\",\n            \"before\": 1000,")
			},
			[]string{`"Account"`, "/actions[0]/treasury"},
		},
		"balance update before variant": {
			func(s string) string {
				return replaceOnce(t, s, `            "before": 1000,`, `            "Before": 1000,`)
			},
			[]string{`"Before"`, "/actions[0]/treasury"},
		},
		"vote proposal state variant": {
			func(s string) string {
				return replaceOnce(t, s,
					"    \"v1\": {\n      \"id\": \"v1\",\n      \"state\": \"executed\",",
					"    \"v1\": {\n      \"id\": \"v1\",\n      \"State\": \"executed\",")
			},
			[]string{`"State"`, `id="v1"`},
		},
		"vote proposal quorum variant": {
			func(s string) string {
				return replaceOnce(t, s, `      "quorum": 600,`, `      "Quorum": 600,`)
			},
			[]string{`"Quorum"`, `id="v1"`},
		},
		"member weight variant": {
			func(s string) string {
				return replaceOnce(t, s,
					"          \"id\": \"alice\",\n          \"weight\": 600",
					"          \"id\": \"alice\",\n          \"Weight\": 600")
			},
			// alice 同时是 v1 唯一成员与 v2 成员；只要定位到某个投票提案记录即可。
			[]string{`"Weight"`, "/members[0]"},
		},
		"delegation from variant": {
			func(s string) string {
				return replaceOnce(t, s,
					"          \"from\": \"bob\",\n          \"to\": \"alice\"",
					"          \"From\": \"bob\",\n          \"to\": \"alice\"")
			},
			[]string{`"From"`, "/delegations[0]"},
		},
		"delegation to variant": {
			func(s string) string {
				return replaceOnce(t, s,
					"          \"from\": \"bob\",\n          \"to\": \"alice\"",
					"          \"from\": \"bob\",\n          \"To\": \"alice\"")
			},
			[]string{`"To"`, "/delegations[0]"},
		},
		"ballot support renamed": {
			func(s string) string {
				return replaceOnce(t, s, `          "support": true,`, `          "Support": true,`)
			},
			[]string{`"Support"`, `representative="alice"`, "/ballots[0]"},
		},
		"ballot support plus Support": {
			// 后一个值故意与票据汇总和计票结论冲突，仍然必须在读取阶段拒绝。
			func(s string) string {
				return replaceOnce(t, s,
					"          \"support\": true,\n",
					"          \"support\": true,\n          \"Support\": false,\n")
			},
			[]string{`"Support"`, `representative="alice"`},
		},
		"ballot representative variant": {
			func(s string) string {
				return replaceOnce(t, s, `          "representative": "alice",`,
					`          "Representative": "alice",`)
			},
			[]string{`"Representative"`, "/ballots[0]"},
		},
		"tally for_weight variant": {
			func(s string) string {
				return replaceOnce(t, s, `        "for_weight": 600,`, `        "For_weight": 600,`)
			},
			[]string{`"For_weight"`, "/tally"},
		},
		"tally tallied_at variant": {
			func(s string) string {
				return replaceOnce(t, s, `        "tallied_at": 10`, `        "Tallied_at": 10`)
			},
			[]string{`"Tallied_at"`, "/tally"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			badPath := filepath.Join(filepath.Dir(path), "bad-"+strings.ReplaceAll(name, " ", "-")+".json")
			raw := tc.mutate(string(good))
			if err := os.WriteFile(badPath, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			s, err := Open(badPath)
			if !errors.Is(err, ErrStateCorrupt) {
				if s != nil {
					s.Close()
				}
				t.Fatalf("Open err=%v, want ErrStateCorrupt", err)
			}
			for _, frag := range tc.want {
				if !strings.Contains(err.Error(), frag) {
					t.Fatalf("error %q missing %q (must name the field and record)", err.Error(), frag)
				}
			}
			// 不自动整理或写回原文件。
			if got, rerr := os.ReadFile(badPath); rerr != nil || string(got) != raw {
				t.Fatalf("corrupt file was rewritten on rejected open")
			}
			// 再次打开仍以同样方式拒绝。
			if s2, err2 := Open(badPath); !errors.Is(err2, ErrStateCorrupt) {
				if s2 != nil {
					s2.Close()
				}
				t.Fatalf("second Open err=%v, want ErrStateCorrupt", err2)
			}
		})
	}
}

// TestBusinessKeysRemainCaseSensitive：账户名与提案编号是业务数据，
// Audit/audit、GIP-1/gip-1 是两个不同账户/编号，必须共存且不被改写。
func TestBusinessKeysRemainCaseSensitive(t *testing.T) {
	store, _ := openTempStore(t, 10000)
	path := store.Path()

	mustRegister(t, store, "GIP-1", 0, "transfer:Audit:10")
	mustRegister(t, store, "gip-1", 0, "transfer:audit:20")
	if _, err := store.Execute("GIP-1", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Execute("gip-1", 0); err != nil {
		t.Fatal(err)
	}
	upper := &CreateVoteInput{
		ID: "VC-1", Members: []VoteMember{{ID: "a", Weight: 1}},
		Quorum: 1, StartAt: 0, Deadline: 10, TimelockEnd: 10,
	}
	lower := &CreateVoteInput{
		ID: "vc-1", Members: []VoteMember{{ID: "b", Weight: 1}},
		Quorum: 1, StartAt: 0, Deadline: 10, TimelockEnd: 10,
	}
	mustCreateVote(t, store, upper)
	mustCreateVote(t, store, lower)
	store.Close()

	// 重开：仅大小写不同的账户、提案编号、投票编号必须各自独立存在。
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	bal, err := reopened.Balance("Audit")
	if err != nil || bal != 10 {
		t.Fatalf("Audit balance=%d err=%v", bal, err)
	}
	bal, err = reopened.Balance("audit")
	if err != nil || bal != 20 {
		t.Fatalf("audit balance=%d err=%v", bal, err)
	}
	proposals, err := reopened.Proposals()
	if err != nil || len(proposals) != 2 {
		t.Fatalf("proposals=%d err=%v", len(proposals), err)
	}
	gotIDs := map[string]bool{}
	for _, p := range proposals {
		gotIDs[p.ID] = true
	}
	if !gotIDs["GIP-1"] || !gotIDs["gip-1"] {
		t.Fatalf("case-distinct proposal ids collapsed: %v", gotIDs)
	}
	votes, err := reopened.VoteProposals()
	if err != nil || len(votes) != 2 {
		t.Fatalf("vote proposals=%d err=%v", len(votes), err)
	}
	gotVoteIDs := map[string]bool{}
	for _, v := range votes {
		gotVoteIDs[v.ID] = true
	}
	if !gotVoteIDs["VC-1"] || !gotVoteIDs["vc-1"] {
		t.Fatalf("case-distinct vote ids collapsed: %v", gotVoteIDs)
	}

	// 文件中两个账户与两个编号都原样保留。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, frag := range []string{`"Audit": 10`, `"audit": 20`, `"GIP-1": {`, `"gip-1": {`, `"VC-1": {`, `"vc-1": {`} {
		if !strings.Contains(string(raw), frag) {
			t.Fatalf("state file lost case-distinct key fragment %q", frag)
		}
	}
}

// TestEscapedKeySpellings：字段名按 JSON 解读后的字符串判断。
// 合法转义拼写与普通拼写表示同一字段：单独出现正常识别，
// 同一对象出现两次（无论转义与否）仍属重复键。
func TestEscapedKeySpellings(t *testing.T) {
	path, good := buildRichStateFile(t)

	// 顶层 treasury：普通拼写改成等价转义拼写，单独出现必须可读。
	escaped := replaceOnce(t, string(good), `  "treasury": 850,`,
		"  \"\\u0074\\u0072easury\": 850,")
	if err := scanStrictKeys([]byte(escaped)); err != nil {
		t.Fatalf("escaped single fixed key rejected: %v", err)
	}
	escPath := filepath.Join(filepath.Dir(path), "escaped-ok.json")
	if err := os.WriteFile(escPath, []byte(escaped), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(escPath)
	if err != nil {
		t.Fatalf("file with escaped treasury spelling rejected: %v", err)
	}
	if bal, _ := s.TreasuryBalance(); bal != 850 {
		t.Fatalf("escaped treasury key read balance=%d, want 850", bal)
	}
	s.Close()

	// 提案记录里的 state 用等价转义拼写，单独出现正常。
	escState := replaceOnce(t, string(good),
		"      \"id\": \"p1\",\n      \"state\": \"executed\",",
		"      \"id\": \"p1\",\n      \"\\u0073tate\": \"executed\",")
	if err := scanStrictKeys([]byte(escState)); err != nil {
		t.Fatalf("escaped single state key rejected: %v", err)
	}

	// 同一对象内普通拼写 + 等价转义拼写 = 重复键。
	dupEscaped := replaceOnce(t, string(good), "  \"treasury\": 850,\n",
		"  \"treasury\": 850,\n  \"\\u0074\\u0072easury\": 850,\n")
	err = scanStrictKeys([]byte(dupEscaped))
	if err == nil || !strings.Contains(err.Error(), `duplicate key "treasury"`) {
		t.Fatalf("escaped duplicate treasury: err=%v, want duplicate key", err)
	}

	dupState := replaceOnce(t, string(good),
		"      \"id\": \"p1\",\n      \"state\": \"executed\",\n",
		"      \"id\": \"p1\",\n      \"state\": \"executed\",\n      \"\\u0073tate\": \"executed\",\n")
	err = scanStrictKeys([]byte(dupState))
	if err == nil || !strings.Contains(err.Error(), `duplicate key "state"`) ||
		!strings.Contains(err.Error(), `id="p1"`) {
		t.Fatalf("escaped duplicate state: err=%v, want duplicate key at p1", err)
	}

	// 业务编号表同理：转义只影响拼写，解读后相同即为同一个账户。
	store2, path2 := openTempStore(t, 9999)
	mustRegister(t, store2, "p", 0, "transfer:Acct:5")
	if _, err := store2.Execute("p", 0); err != nil {
		t.Fatal(err)
	}
	store2.Close()
	raw2, err := os.ReadFile(path2)
	if err != nil {
		t.Fatal(err)
	}
	// 单独出现的等价转义账户名：与普通拼写等价，余额与凭据重放仍然一致。
	escAcct := strings.Replace(string(raw2), `"Acct": 5`, `"\u0041cct": 5`, 1)
	if err := os.WriteFile(path2, []byte(escAcct), 0o600); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path2)
	if err != nil {
		t.Fatalf("escaped business key rejected: %v", err)
	}
	if bal, _ := s2.Balance("Acct"); bal != 5 {
		t.Fatalf("escaped Acct balance=%d, want 5", bal)
	}
	s2.Close()

	// 同一对象里普通拼写与等价转义拼写并存：解读后重复，拒绝整个文件。
	dupAcct := strings.Replace(escAcct,
		`  "balances": {
    "\u0041cct": 5
  },`,
		`  "balances": {
    "Acct": 5,
    "\u0041cct": 5
  },`, 1)
	if dupAcct == escAcct {
		t.Fatalf("test setup failed to inject escaped duplicate account")
	}
	if err := scanStrictKeys([]byte(dupAcct)); err == nil ||
		!strings.Contains(err.Error(), `duplicate key "Acct"`) ||
		!strings.Contains(err.Error(), "/balances") {
		t.Fatalf("escaped duplicate business key: err=%v", err)
	}
}

// TestKeyOrderAndIndentationIrrelevant：字段顺序与缩进不影响读取结果。
func TestKeyOrderAndIndentationIrrelevant(t *testing.T) {
	path, good := buildRichStateFile(t)

	var doc map[string]any
	if err := json.Unmarshal(good, &doc); err != nil {
		t.Fatal(err)
	}
	// map 经 json.Marshal 输出按键排序的紧凑 JSON：顺序与缩进都与保存格式不同。
	reordered, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if equalBytes(reordered, good) {
		t.Fatal("test setup did not change serialization")
	}
	altPath := filepath.Join(filepath.Dir(path), "reordered.json")
	if err := os.WriteFile(altPath, reordered, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(altPath)
	if err != nil {
		t.Fatalf("reordered/indented file rejected: %v", err)
	}
	defer s.Close()
	snap, err := s.ProposalsSnapshot()
	if err != nil || len(snap.Registered) != 1 || snap.Registered[0].ID != "p1" ||
		snap.Registered[0].State != "executed" || len(snap.Voting) != 2 {
		t.Fatalf("snapshot from reordered file: %+v err=%v", snap, err)
	}
	bal, err := s.BalanceSnapshot()
	if err != nil || bal.Treasury != 850 || bal.Balances["acct"] != 100 || bal.Balances["vc"] != 50 {
		t.Fatalf("balances from reordered file: %+v err=%v", bal, err)
	}
	// 查询内容与原文件一致：凭据顺序与票决提案状态不变。
	rcpts, err := s.Receipts()
	if err != nil || len(rcpts) != 2 || rcpts[0].ProposalID != "p1" || rcpts[1].ProposalID != "v1" {
		t.Fatalf("receipts from reordered file: %+v err=%v", rcpts, err)
	}
	v1, ok, err := s.VoteProposal("v1")
	if err != nil || !ok || v1.State != "executed" || v1.Tally == nil || !v1.Tally.Passed {
		t.Fatalf("v1 from reordered file: %+v ok=%v err=%v", v1, ok, err)
	}
}

// TestReadsAfterExternalCorruption：成功打开后文件被改成含非法字段，
// 后续每一次读取/执行都必须返回损坏错误且不返回部分数据；恢复后读取复原。
func TestReadsAfterExternalCorruption(t *testing.T) {
	path, good := buildRichStateFile(t)
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// 打开时正常。
	if bal, _ := store.TreasuryBalance(); bal != 850 {
		t.Fatalf("initial balance=%d", bal)
	}

	// 模拟文件在两次操作之间被改成大小写变体冲突写法（同值，核对本可通过）。
	corrupt := replaceOnce(t, string(good),
		"      \"id\": \"p1\",\n      \"state\": \"executed\",\n",
		"      \"id\": \"p1\",\n      \"state\": \"executed\",\n      \"State\": \"executed\",\n")
	if err := os.WriteFile(path, []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func() error{
		"balances":  func() error { _, e := store.BalanceSnapshot(); return e },
		"proposals": func() error { _, e := store.ProposalsSnapshot(); return e },
		"proposal":  func() error { _, _, e := store.Proposal("p1"); return e },
		"receipts":  func() error { _, e := store.Receipts(); return e },
		"execute":   func() error { _, e := store.Execute("v2", 10); return e },
	} {
		if err := call(); !errors.Is(err, ErrStateCorrupt) {
			t.Fatalf("%s after corruption err=%v, want ErrStateCorrupt", name, err)
		}
	}
	if got, rerr := os.ReadFile(path); rerr != nil || string(got) != corrupt {
		t.Fatalf("failed read rewrote the state file")
	}

	// 恢复为完好内容后读取立刻复原，证明拒绝的是文件内容而非句柄状态。
	if err := os.WriteFile(path, good, 0o600); err != nil {
		t.Fatal(err)
	}
	if bal, err := store.TreasuryBalance(); err != nil || bal != 850 {
		t.Fatalf("read after restore bal=%d err=%v", bal, err)
	}
}

// TestCorruptFieldNamesCLIExitsOne：命令行沿用现有失败方式：原因写 stderr、
// 退出码 1，stdout 不输出任何余额或提案信息。
func TestCorruptFieldNamesCLIExitsOne(t *testing.T) {
	statePath, good := buildRichStateFile(t)
	bad := filepath.Join(filepath.Dir(statePath), "cli-corrupt.json")
	raw := replaceOnce(t, string(good), `  "treasury": 850,`,
		"  \"treasury\": 850,\n  \"Treasury\": 850,")
	if err := os.WriteFile(bad, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := buildCLI(t)

	for _, cmd := range [][]string{
		{"balances"},
		{"proposals"},
		{"proposal", "--id", "p1"},
		{"receipts"},
		{"execute", "--id", "p1", "--now", "1"},
	} {
		so, se, code := runCLI(t, binary, bad, append(cmd, "--json")...)
		if code != 1 {
			t.Fatalf("%v exit code=%d, want 1; stderr=%s", cmd, code, se)
		}
		if so != "" {
			t.Fatalf("%v printed partial data on stdout: %s", cmd, so)
		}
		if !strings.Contains(se, `"Treasury"`) || !strings.Contains(se, "corrupt") {
			t.Fatalf("%v stderr=%q must name the bad field and corrupt-state error", cmd, se)
		}
	}
	if got, rerr := os.ReadFile(bad); rerr != nil || string(got) != raw {
		t.Fatalf("corrupt file was rewritten by CLI")
	}

	// 记录内字段变体：stderr 还必须能定位到具体提案记录。
	bad2 := filepath.Join(filepath.Dir(statePath), "cli-corrupt-2.json")
	raw2 := replaceOnce(t, string(good),
		"      \"id\": \"v1\",\n      \"state\": \"executed\",",
		"      \"id\": \"v1\",\n      \"State\": \"executed\",")
	if err := os.WriteFile(bad2, []byte(raw2), 0o600); err != nil {
		t.Fatal(err)
	}
	_, se, code := runCLI(t, binary, bad2, "proposal", "--id", "v1")
	if code != 1 || !strings.Contains(se, `"State"`) || !strings.Contains(se, `id="v1"`) {
		t.Fatalf("stderr=%q code=%d must name field State and record v1", se, code)
	}
}
