package govflow

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件为 tally 首次计票“保存失败”补充命令行回归保障，覆盖两类必须可区分
// 的结果（普通文本与 --json 两种输出模式一致）：
//
//   - 失败发生在状态文件替换之前：结论没有写入。命令退出码 1、stdout 没有任何
//     计票结果、stderr 保留具体保存原因（既不能说成投票尚未截止，也不能说成
//     结论已写入）；随后查询仍是 voting、没有计票结论；恢复保存条件后在 210
//     再次计票才形成首次通过结论，首次计票时间为 210，不沿用失败尝试的 200。
//   - 状态文件已替换成功、只在随后的目录同步失败：结论已写入但持久性未确认。
//     命令同样退出码 1、stdout 仍没有计票结果、stderr 说明“已写入但持久性未
//     确认”并保留具体原因；恢复正常读取条件后查询应看到 passed 与 tallied_at=200
//     的完整结论；210 再次计票只返回已有的 200 结论，不改成 210，也不退回 voting。
//
// 业务场景复用 cliVoteCreateArgs：资金库 1000；带委托的有效转账提案
// alice/bob/carol/dave 原始权重 300/200/100/400，bob、carol 委托给 alice，
// 法定人数 600、投票窗口 [100,200)、时间锁 300、动作 transfer:audits:100；
// dave 在 130 投反对（归集 400），alice 在 170 投赞成（归集 600）。截止时刻
// 200 的正常首次计票结论应为赞成 600、反对 400、参与 1000 的通过结论。

// cliTallyInit 跨进程准备上述场景：初始化资金库、创建提案、保存两张窗口内票据
// （dave 反对@130 在前，alice 赞成@170 在后）。
func cliTallyInit(t *testing.T, binary, state, id string) {
	t.Helper()
	if _, se, code := runCLI(t, binary, state, "init", "--balance", "1000"); code != 0 {
		t.Fatalf("init exit=%d: %s", code, se)
	}
	if _, se, code := runCLI(t, binary, state, cliVoteCreateArgs(id)...); code != 0 {
		t.Fatalf("create-vote exit=%d: %s", code, se)
	}
	if _, se, code := runCLI(t, binary, state, "vote", "--id", id,
		"--voter", "dave", "--choice", "against", "--now", "130"); code != 0 {
		t.Fatalf("dave against ballot exit=%d: %s", code, se)
	}
	if _, se, code := runCLI(t, binary, state, "vote", "--id", id,
		"--voter", "alice", "--choice", "for", "--now", "170"); code != 0 {
		t.Fatalf("alice for ballot exit=%d: %s", code, se)
	}
}

// cliTallyQueryJSON 以全新进程用 JSON 查询提案并解析成视图。
func cliTallyQueryJSON(t *testing.T, binary, state, id string) *VoteProposalView {
	t.Helper()
	so, se, code := runCLI(t, binary, state, "proposal", "--id", id, "--json")
	if code != 0 {
		t.Fatalf("json proposal query exit=%d: %s", code, se)
	}
	var v VoteProposalView
	if err := json.Unmarshal([]byte(so), &v); err != nil {
		t.Fatalf("proposal json is not valid: %v\n%s", err, so)
	}
	return &v
}

// cliTallyAssertShape 断言提案的窗口、时间锁、法定人数、总权重、成员原始权重、
// 委托关系与动作原文仍是创建时的样子，不被任何保存失败改写。
func cliTallyAssertShape(t *testing.T, v *VoteProposalView) {
	t.Helper()
	if v.Quorum != 600 || v.StartAt != 100 || v.Deadline != 200 ||
		v.TimelockEnd != 300 || v.TotalWeight != 1000 {
		t.Fatalf("proposal parameters changed: %+v", v)
	}
	if !sameActions(v.Actions, []string{"transfer:audits:100"}) {
		t.Fatalf("actions = %v, want the original transfer action text", v.Actions)
	}
	wantMembers := map[string]MemberView{
		"alice": {ID: "alice", Weight: 300, Path: []string{"alice"}, Delegate: "alice", Direct: ""},
		"bob":   {ID: "bob", Weight: 200, Path: []string{"bob", "alice"}, Delegate: "alice", Direct: "alice"},
		"carol": {ID: "carol", Weight: 100, Path: []string{"carol", "alice"}, Delegate: "alice", Direct: "alice"},
		"dave":  {ID: "dave", Weight: 400, Path: []string{"dave"}, Delegate: "dave", Direct: ""},
	}
	if len(v.Members) != len(wantMembers) {
		t.Fatalf("members = %+v, want the original four members", v.Members)
	}
	for _, m := range v.Members {
		want, ok := wantMembers[m.ID]
		if !ok {
			t.Fatalf("unexpected member %+v", m)
		}
		if m.Weight != want.Weight || m.Delegate != want.Delegate ||
			m.Direct != want.Direct || !sameActions(m.Path, want.Path) {
			t.Fatalf("member %s changed: got %+v want %+v", m.ID, m, want)
		}
	}
}

// cliTallyAssertBallots 断言两张已保存票据的代表、归集权重、选择、首次时间与
// 顺序全部保留：dave 反对 400@130 在前，alice 赞成 600@170 在后。
func cliTallyAssertBallots(t *testing.T, v *VoteProposalView) {
	t.Helper()
	if len(v.Ballots) != 2 {
		t.Fatalf("ballots = %+v, want exactly the two saved ballots", v.Ballots)
	}
	first, second := v.Ballots[0], v.Ballots[1]
	if first.Representative != "dave" || first.Weight != 400 || first.Support || first.VotedAt != 130 {
		t.Fatalf("first ballot = %+v, want dave against weight=400 voted_at=130", first)
	}
	if second.Representative != "alice" || second.Weight != 600 || !second.Support || second.VotedAt != 170 {
		t.Fatalf("second ballot = %+v, want alice for weight=600 voted_at=170", second)
	}
}

// cliTallyAssertText 断言文本查询的关键字与查询状态一致：state、提案参数、
// 成员/委托路径、票据、动作原文以及（有结论时）计票行都按文本格式呈现。
func cliTallyAssertText(t *testing.T, binary, state, id, wantState string, tallyLine string) {
	t.Helper()
	to, te, code := runCLI(t, binary, state, "proposal", "--id", id)
	if code != 0 {
		t.Fatalf("text proposal query exit=%d: %s", code, te)
	}
	if !strings.Contains(to, "state="+wantState) {
		t.Fatalf("text query must show state=%s:\n%s", wantState, to)
	}
	for _, want := range []string{
		"quorum=600 voting=[100,200) timelock_end=300 actions=1 total_weight=1000",
		"member alice weight=300 representative=self",
		"member bob weight=200 path=bob->alice",
		"member carol weight=100 path=carol->alice",
		"member dave weight=400 representative=self",
		"action 0: transfer:audits:100",
		"ballot representative=dave choice=against weight=400 voted_at=130",
		"ballot representative=alice choice=for weight=600 voted_at=170",
	} {
		if !strings.Contains(to, want) {
			t.Fatalf("text proposal view missing %q:\n%s", want, to)
		}
	}
	// dave 票据必须排在 alice 之前。
	davePos := strings.Index(to, "ballot representative=dave")
	alicePos := strings.Index(to, "ballot representative=alice")
	if davePos < 0 || alicePos < 0 || davePos > alicePos {
		t.Fatalf("text ballots must preserve dave-before-alice order:\n%s", to)
	}
	if tallyLine == "" {
		if strings.Contains(to, "tally for=") {
			t.Fatalf("text query must contain no tally line:\n%s", to)
		}
	} else if !strings.Contains(to, tallyLine) {
		t.Fatalf("text query missing tally line %q:\n%s", tallyLine, to)
	}
}

// cliTallyAssertVoting 以全新进程分别用 JSON 与文本查询，断言提案仍是 voting、
// 没有任何计票结论，且票据、成员、委托关系与动作原文全部保留。
func cliTallyAssertVoting(t *testing.T, binary, state, id string) {
	t.Helper()
	v := cliTallyQueryJSON(t, binary, state, id)
	if v.State != "voting" || v.Tally != nil {
		t.Fatalf("state=%s tally=%+v, want voting with no tally conclusion", v.State, v.Tally)
	}
	cliTallyAssertBallots(t, v)
	cliTallyAssertShape(t, v)
	cliTallyAssertText(t, binary, state, id, "voting", "")
}

// cliTallyAssertPassed 以全新进程分别用 JSON 与文本查询，断言提案为 passed，
// 计票结论为赞成 600、反对 400、参与 1000、法定人数 600、首次计票时间恰为
// wantTalliedAt；票据、成员、委托关系与动作原文仍完整保留。
func cliTallyAssertPassed(t *testing.T, binary, state, id string, wantTalliedAt int64) {
	t.Helper()
	v := cliTallyQueryJSON(t, binary, state, id)
	if v.State != "passed" || v.Tally == nil {
		t.Fatalf("state=%s tally=%+v, want passed with the written conclusion", v.State, v.Tally)
	}
	if !v.Tally.Passed || v.Tally.ForWeight != 600 || v.Tally.AgainstWeight != 400 ||
		v.Tally.Turnout != 1000 || v.Tally.Quorum != 600 || v.Tally.TalliedAt != wantTalliedAt {
		t.Fatalf("tally = %+v, want passed for=600 against=400 turnout=1000 quorum=600 tallied_at=%d",
			v.Tally, wantTalliedAt)
	}
	cliTallyAssertBallots(t, v)
	cliTallyAssertShape(t, v)
	line := fmt.Sprintf("tally for=600 against=400 turnout=1000 => passed tallied_at=%d", wantTalliedAt)
	cliTallyAssertText(t, binary, state, id, "passed", line)
}

// cliTallyAssertFundsUntouched 断言计票（无论成功还是保存失败）从不执行转账：
// 资金库仍是 1000、没有任何收款账户余额、没有执行凭据。
func cliTallyAssertFundsUntouched(t *testing.T, binary, state string) {
	t.Helper()
	bo, be, bc := runCLI(t, binary, state, "balances", "--json")
	if bc != 0 || !strings.Contains(bo, `"treasury": 1000`) || strings.Contains(bo, "audits") {
		t.Fatalf("tally must not move funds, exit=%d stderr=%s:\n%s", bc, be, bo)
	}
	ro, re, rc := runCLI(t, binary, state, "receipts", "--json")
	if rc != 0 || strings.TrimSpace(ro) != "[]" {
		t.Fatalf("tally must not produce execution receipts, exit=%d stderr=%s:\n%s", rc, re, ro)
	}
}

// TestCLITallyPreReplaceSaveFailure：首次计票的保存失败发生在状态文件替换之前
// （目录不可写，临时文件都无法创建）。普通文本与 --json 两种模式都必须：
//   - 退出码 1，stdout 没有任何计票结果；
//   - stderr 保留具体保存原因（permission denied），不能说成投票尚未截止
//     （still open），也不能说成结论已写入（durability not confirmed）；
//   - 原状态文件字节不变、不遗留临时文件；
//   - 随后查询仍是 voting、没有计票结论，票据/成员/委托/动作完整保留；
//   - 恢复保存条件后在 210 再次计票才形成首次通过结论，首次计票时间为 210，
//     不沿用失败尝试的 200；再次计票（300）仍返回 210 结论。
//
// 全过程计票不执行转账，资金库与收款账户余额不变、不产生执行凭据。
func TestCLITallyPreReplaceSaveFailure(t *testing.T) {
	binary := buildCLI(t)
	state := filepath.Join(t.TempDir(), "treasury.json")
	const id = "gip-tally-cli-pre"
	cliTallyInit(t, binary, state, id)

	// 记录失败前的完整状态文件字节：两次失败尝试后它必须原样保留。
	good, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}

	// 目录不可写：提交在状态文件替换之前（创建临时文件）失败。该失败不写入任何
	// 内容，因此文本与 JSON 两次独立调用都应得到同样的“尚未写入”失败，且提案
	// 始终保持 voting。
	makeDirUnwritable(t, state)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"text", []string{"tally", "--id", id, "--now", "200"}},
		{"json", []string{"tally", "--id", id, "--now", "200", "--json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			so, se, code := runCLI(t, binary, state, tc.args...)
			if code != 1 {
				t.Fatalf("exit=%d want 1 (stdout=%q stderr=%q)", code, so, se)
			}
			// 结论没有写入：stdout 不得出现任何计票结果（JSON 模式也不得吐对象）。
			if so != "" {
				t.Fatalf("stdout must carry no tally result when nothing was written, got %q", so)
			}
			// stderr 必须保留具体保存原因。
			if !strings.Contains(se, "permission denied") {
				t.Fatalf("stderr must carry the concrete save cause, got %q", se)
			}
			// 不能误报“投票尚未截止”（截止时刻 200 本就允许计票）。
			if strings.Contains(se, "still open") {
				t.Fatalf("a deadline-reached tally must not be reported as still open: %q", se)
			}
			// 不能说成结论已写入/持久性未确认——这是“尚未写入”的失败。
			if strings.Contains(se, "written but durability not confirmed") {
				t.Fatalf("pre-replacement failure must not claim the conclusion was written: %q", se)
			}
		})
	}

	// 恢复保存条件：原文件字节未变、没有遗留临时文件。
	if err := os.Chmod(filepath.Dir(state), 0o700); err != nil {
		t.Fatalf("restore dir: %v", err)
	}
	if got, rerr := os.ReadFile(state); rerr != nil || string(got) != string(good) {
		t.Fatalf("state file changed despite the pre-replacement save failure")
	}
	if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(state), ".govflow-state-*")); len(leftovers) != 0 {
		t.Fatalf("failed tally left temporary file(s): %v", leftovers)
	}

	// 恢复后、重计前的查询：仍是 voting、无结论，票据与提案形状完整保留。
	cliTallyAssertVoting(t, binary, state, id)

	// 210 再次计票才形成首次成功结论：600/400/1000 通过，首次计票时间为 210，
	// 绝不沿用失败尝试的 200。
	so, se, code := runCLI(t, binary, state, "tally", "--id", id, "--now", "210")
	if code != 0 {
		t.Fatalf("first successful tally after recovery exit=%d: %s", code, se)
	}
	if !strings.Contains(so, "for=600 against=400 turnout=1000 quorum=600 => passed (tallied_at=210)") {
		t.Fatalf("first successful tally must be the 210 passed conclusion:\n%s", so)
	}
	cliTallyAssertPassed(t, binary, state, id, 210)

	// 再次计票（传入更晚的 300）始终返回首次 210 结论，不重新计票、不改时间。
	ro, re, rc := runCLI(t, binary, state, "tally", "--id", id, "--now", "300", "--json")
	if rc != 0 || !strings.Contains(ro, `"passed": true`) || !strings.Contains(ro, `"tallied_at": 210`) {
		t.Fatalf("repeat tally must return the first 210 conclusion, exit=%d stderr=%s:\n%s", rc, re, ro)
	}
	cliTallyAssertPassed(t, binary, state, id, 210)
	cliTallyAssertFundsUntouched(t, binary, state)
}

// TestCLITallyDirSyncWrittenButUnconfirmed：首次计票时状态文件已替换成功、只在
// 随后的目录同步失败（目录可写可进入但不可读，0o300）。普通文本与 --json
// 两种模式都必须：
//   - 退出码 1，stdout 仍没有计票结果（JSON 模式也不得吐结论对象）；
//   - stderr 明确“已写入但持久性未确认”，并保留具体失败原因（permission denied），
//     不能说成投票尚未截止；
//   - 恢复正常读取条件后，查询应看到 passed 与 tallied_at=200 的完整结论；
//   - 210 再次计票正常返回已有的 200 结论，不改成 210，也不把已通过提案退回 voting。
//
// 每种输出模式使用各自独立的状态文件：第一个进程的原子改名已经把结论写入，
// 之后的进程会直接读到已保存结论并成功返回，因此“失败当次”的输出判定必须在
// 一份全新状态上、只触发一次。全过程计票不执行转账，资金不变、无执行凭据。
func TestCLITallyDirSyncWrittenButUnconfirmed(t *testing.T) {
	binary := buildCLI(t)

	for _, tc := range []struct {
		name      string
		failArgs  func(id string) []string
		retryArgs func(id string) []string
		wantOut   string
	}{
		{
			name:      "text",
			failArgs:  func(id string) []string { return []string{"tally", "--id", id, "--now", "200"} },
			retryArgs: func(id string) []string { return []string{"tally", "--id", id, "--now", "210"} },
			wantOut:   "for=600 against=400 turnout=1000 quorum=600 => passed (tallied_at=200)",
		},
		{
			name: "json",
			failArgs: func(id string) []string {
				return []string{"tally", "--id", id, "--now", "200", "--json"}
			},
			retryArgs: func(id string) []string {
				return []string{"tally", "--id", id, "--now", "210", "--json"}
			},
			wantOut: `"tallied_at": 200`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "treasury.json")
			const id = "gip-tally-cli-sync"
			cliTallyInit(t, binary, state, id)

			// 0300：临时文件写入/改名成功，只有随后的目录 fsync 失败。
			makeDirSyncFail(t, state)
			so, se, code := runCLI(t, binary, state, tc.failArgs(id)...)
			if code != 1 {
				t.Fatalf("exit=%d want 1 (stdout=%q stderr=%q)", code, so, se)
			}
			// 这次调用不报告成功计票结果：stdout 为空（JSON 模式同样不吐对象）。
			if so != "" {
				t.Fatalf("stdout must carry no tally result on the durability-uncertain call, got %q", so)
			}
			// stderr 必须明确“已写入但持久性未确认”并保留具体原因。
			if !strings.Contains(se, "state written but durability not confirmed") {
				t.Fatalf("stderr must state the conclusion was written but durability unconfirmed, got %q", se)
			}
			if !strings.Contains(se, "permission denied") {
				t.Fatalf("stderr must preserve the concrete save cause, got %q", se)
			}
			if strings.Contains(se, "still open") {
				t.Fatalf("a deadline-reached tally must not be reported as still open: %q", se)
			}

			// 恢复正常读取条件：结论其实已随原子改名写入，无遗留临时文件。
			if err := os.Chmod(filepath.Dir(state), 0o700); err != nil {
				t.Fatalf("restore dir: %v", err)
			}
			if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(state), ".govflow-state-*")); len(leftovers) != 0 {
				t.Fatalf("dir-sync failure left temporary file(s): %v", leftovers)
			}

			// 全新进程查询：passed 与 tallied_at=200 的完整结论，票据/成员/委托/动作保留。
			cliTallyAssertPassed(t, binary, state, id, 200)

			// 210 再次计票：正常（退出码 0）返回已写入的 200 结论，不改成 210。
			ro, re, rc := runCLI(t, binary, state, tc.retryArgs(id)...)
			if rc != 0 {
				t.Fatalf("repeat tally after recovery exit=%d: %s", rc, re)
			}
			if !strings.Contains(ro, tc.wantOut) {
				t.Fatalf("repeat tally must return the already-written 200 conclusion:\n%s", ro)
			}
			if strings.Contains(ro, "tallied_at=210") || strings.Contains(ro, `"tallied_at": 210`) {
				t.Fatalf("repeat tally must not move the first tally time to 210:\n%s", ro)
			}
			// 已通过提案不退回 voting，仍是 passed/200；票据、资金记录一致。
			cliTallyAssertPassed(t, binary, state, id, 200)
			cliTallyAssertFundsUntouched(t, binary, state)
		})
	}
}
