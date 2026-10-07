package govflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件是“首次计票保存失败”的命令行回归保障：用带委托且已保存有效票据的
// 提案（alice/bob/carol/dave 权重 300/200/100/400，bob、carol 委托给 alice，
// alice 投赞成、dave 投反对；法定人数 600，窗口 [100,200)，时间锁 300，一项
// 有效转账动作），跨进程区分“结论没有写入”与“结论已写入，但持久性未确认”
// 两种保存失败，覆盖普通文本与 --json 下的退出码、输出及随后查询的实际状态。

// cliTallyPrepare 通过 CLI 建立计票场景：创建提案（复用 cliVoteCreateArgs 的
// 规格名单与委托），dave 在 120 投出归集 400 权重的反对票，alice 在 150 投出
// 归集 600 权重的赞成票。正常在 200 首次计票应得到赞成 600、反对 400、
// 参与 1000 的通过结论。
func cliTallyPrepare(t *testing.T, binary, state, id string) {
	t.Helper()
	if _, se, code := runCLI(t, binary, state, cliVoteCreateArgs(id)...); code != 0 {
		t.Fatalf("create-vote failed: %s", se)
	}
	if _, se, code := runCLI(t, binary, state, "vote", "--id", id,
		"--voter", "dave", "--choice", "against", "--now", "120"); code != 0 {
		t.Fatalf("dave vote failed: %s", se)
	}
	if _, se, code := runCLI(t, binary, state, "vote", "--id", id,
		"--voter", "alice", "--choice", "for", "--now", "150"); code != 0 {
		t.Fatalf("alice vote failed: %s", se)
	}
}

// assertCLITallyPassedJSON 断言输出是赞成 600、反对 400、参与 1000、法定人数
// 600 的通过结论，首次计票时间为 talliedAt。
func assertCLITallyPassedJSON(t *testing.T, so, talliedAt string) {
	t.Helper()
	for _, want := range []string{
		`"for_weight": 600`, `"against_weight": 400`, `"turnout": 1000`,
		`"quorum": 600`, `"passed": true`, `"tallied_at": ` + talliedAt,
	} {
		if !strings.Contains(so, want) {
			t.Fatalf("tally output missing %s:\n%s", want, so)
		}
	}
}

// assertCLITallyBallotsJSON 断言提案查询 JSON 中两张原票据的代表、归集权重、
// 选择、首次投票时间与顺序全部保留：dave 反对 400@120 在前，alice 赞成
// 600@150 在后；委托关系与动作原文也保持原样。
func assertCLITallyBallotsJSON(t *testing.T, so string) {
	t.Helper()
	if strings.Count(so, `"representative"`) != 2 {
		t.Fatalf("query must show exactly the two saved ballots:\n%s", so)
	}
	davePos := strings.Index(so, `"representative": "dave"`)
	alicePos := strings.Index(so, `"representative": "alice"`)
	if davePos < 0 || alicePos < 0 || davePos > alicePos {
		t.Fatalf("ballot order must be dave then alice:\n%s", so)
	}
	for _, want := range []string{
		`"weight": 400`, `"support": false`, `"voted_at": 120`, // dave 反对票
		`"weight": 600`, `"support": true`, `"voted_at": 150`, // alice 赞成票
		`"transfer:audits:100"`,
	} {
		if !strings.Contains(so, want) {
			t.Fatalf("query missing %s:\n%s", want, so)
		}
	}
	// 委托关系逐字保留：alice 本人与 bob、carol 的最终代表都是 alice，
	// bob、carol 的直接委托对象也是 alice。
	if strings.Count(so, `"delegate": "alice"`) != 3 ||
		strings.Count(so, `"direct_to": "alice"`) != 2 {
		t.Fatalf("delegation relations changed:\n%s", so)
	}
}

// assertCLITallyShapeText 断言文本查询保留创建时的成员、权重、委托路径与
// 动作原文，不被任何计票保存失败改写。
func assertCLITallyShapeText(t *testing.T, so string) {
	t.Helper()
	for _, want := range []string{
		"member alice weight=300 representative=self",
		"member bob weight=200 path=bob->alice",
		"member carol weight=100 path=carol->alice",
		"member dave weight=400 representative=self",
		"action 0: transfer:audits:100",
		"ballot representative=dave choice=against weight=400 voted_at=120",
		"ballot representative=alice choice=for weight=600 voted_at=150",
	} {
		if !strings.Contains(so, want) {
			t.Fatalf("text query missing %q:\n%s", want, so)
		}
	}
}

// assertCLITallyFundsUntouched 断言计票（无论成功还是保存失败）都不执行转账：
// 资金库 1000、收款账户 audits 没有余额、没有任何执行凭据。
func assertCLITallyFundsUntouched(t *testing.T, binary, state string) {
	t.Helper()
	so, _, code := runCLI(t, binary, state, "balances", "--json")
	if code != 0 || !strings.Contains(so, `"treasury": 1000`) || strings.Contains(so, "audits") {
		t.Fatalf("tally must not move funds, code=%d:\n%s", code, so)
	}
	so, _, code = runCLI(t, binary, state, "receipts", "--json")
	if code != 0 || strings.TrimSpace(so) != "[]" {
		t.Fatalf("tally must not produce execution receipts, code=%d:\n%s", code, so)
	}
}

// TestCLITallyFirstCountBaseline：正常在截止时刻 200 首次计票的命令行基线：
// 普通文本与 --json 都以退出码 0 给出赞成 600、反对 400、参与 1000 的通过
// 结论，首次计票时间为 200；再次计票（即使更晚）返回同一结论；计票不执行
// 转账、不产生执行凭据。这是两类保存失败场景共同的对照。
func TestCLITallyFirstCountBaseline(t *testing.T) {
	binary := buildCLI(t)
	state := filepath.Join(t.TempDir(), "treasury.json")
	if _, _, code := runCLI(t, binary, state, "init", "--balance", "1000"); code != 0 {
		t.Fatal("init failed")
	}
	const id = "gip-cli-tally-ok"
	cliTallyPrepare(t, binary, state, id)

	// 普通文本：单行给出完整通过结论与首次计票时间。
	so, se, code := runCLI(t, binary, state, "tally", "--id", id, "--now", "200")
	if code != 0 {
		t.Fatalf("first tally exit=%d: %s", code, se)
	}
	if !strings.Contains(so, "tally proposal="+id+
		" for=600 against=400 turnout=1000 quorum=600 => passed (tallied_at=200)") {
		t.Fatalf("text tally output wrong:\n%s", so)
	}

	// 再次计票（--json、更晚的 210）返回首次 200 结论，不重新计票。
	so, se, code = runCLI(t, binary, state, "tally", "--id", id, "--now", "210", "--json")
	if code != 0 {
		t.Fatalf("repeat tally exit=%d: %s", code, se)
	}
	assertCLITallyPassedJSON(t, so, "200")

	// 查询：passed 状态、200 的首次结论、两张原票据与委托关系全部保留。
	so, se, code = runCLI(t, binary, state, "proposal", "--id", id, "--json")
	if code != 0 {
		t.Fatalf("proposal query exit=%d: %s", code, se)
	}
	if !strings.Contains(so, `"state": "passed"`) {
		t.Fatalf("proposal must be passed:\n%s", so)
	}
	assertCLITallyPassedJSON(t, so, "200")
	assertCLITallyBallotsJSON(t, so)

	so, se, code = runCLI(t, binary, state, "proposal", "--id", id)
	if code != 0 {
		t.Fatalf("text proposal query exit=%d: %s", code, se)
	}
	if !strings.Contains(so, "state=passed") ||
		!strings.Contains(so, "tally for=600 against=400 turnout=1000 => passed tallied_at=200") {
		t.Fatalf("text query must show the passed conclusion:\n%s", so)
	}
	assertCLITallyShapeText(t, so)
	assertCLITallyFundsUntouched(t, binary, state)
}

// TestCLITallyPreReplaceFailureLeavesNoConclusion：首次计票的保存失败发生在
// 状态文件替换之前（目录不可写，临时文件无法创建）时，普通文本与 --json 的
// tally 都以退出码 1 结束，标准输出没有计票结果，标准错误保留具体保存原因
// ——不能说成投票尚未截止，也不能说成结论已写入。随后查询仍显示 voting、
// 没有计票结论；恢复保存条件后在 210 再次计票才产生首次通过结论，首次计票
// 时间为 210，不沿用失败尝试的 200。
func TestCLITallyPreReplaceFailureLeavesNoConclusion(t *testing.T) {
	binary := buildCLI(t)
	state := filepath.Join(t.TempDir(), "treasury.json")
	if _, _, code := runCLI(t, binary, state, "init", "--balance", "1000"); code != 0 {
		t.Fatal("init failed")
	}
	const id = "gip-cli-tally-pre"
	cliTallyPrepare(t, binary, state, id)

	// 记录失败前的完整状态文件字节：保存失败后它必须原样保留。
	before, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}

	// 目录不可写：提交在状态文件替换之前（创建临时文件）失败。
	makeDirUnwritable(t, state)
	for _, args := range [][]string{
		{"tally", "--id", id, "--now", "200"},
		{"tally", "--id", id, "--now", "200", "--json"},
	} {
		so, se, code := runCLI(t, binary, state, args...)
		if code != 1 {
			t.Fatalf("%v exit=%d, want 1 (stdout=%q stderr=%q)", args, code, so, se)
		}
		if so != "" {
			t.Fatalf("%v stdout must carry no tally result, got %q", args, so)
		}
		if !strings.Contains(se, "permission denied") {
			t.Fatalf("%v stderr must keep the concrete save cause, got %q", args, se)
		}
		if strings.Contains(se, "still open") {
			t.Fatalf("%v a deadline-reached tally must not be reported as not yet closed: %q", args, se)
		}
		if strings.Contains(se, "durability not confirmed") {
			t.Fatalf("%v nothing was written; must not claim a written conclusion: %q", args, se)
		}
	}

	// 恢复保存条件：原文件字节未变，没有遗留临时文件。
	if err := os.Chmod(filepath.Dir(state), 0o700); err != nil {
		t.Fatalf("restore dir: %v", err)
	}
	if got, rerr := os.ReadFile(state); rerr != nil || string(got) != string(before) {
		t.Fatalf("state file changed despite pre-replacement save failure")
	}
	if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(state), ".govflow-state-*")); len(leftovers) != 0 {
		t.Fatalf("failed tally left temporary file(s): %v", leftovers)
	}

	// 随后查询仍显示 voting、没有计票结论；两张原票据与委托关系保持原样。
	so, se, code := runCLI(t, binary, state, "proposal", "--id", id, "--json")
	if code != 0 {
		t.Fatalf("proposal query exit=%d: %s", code, se)
	}
	if !strings.Contains(so, `"state": "voting"`) {
		t.Fatalf("proposal must stay voting:\n%s", so)
	}
	if strings.Contains(so, `"tally"`) {
		t.Fatalf("no tally conclusion may exist:\n%s", so)
	}
	assertCLITallyBallotsJSON(t, so)

	so, se, code = runCLI(t, binary, state, "proposal", "--id", id)
	if code != 0 {
		t.Fatalf("text proposal query exit=%d: %s", code, se)
	}
	if !strings.Contains(so, "state=voting") || strings.Contains(so, "tally for=") {
		t.Fatalf("text query must show voting with no tally line:\n%s", so)
	}
	assertCLITallyShapeText(t, so)
	assertCLITallyFundsUntouched(t, binary, state)

	// 恢复保存条件后在 210 再次计票：才产生首次通过结论，首次计票时间是 210，
	// 绝不沿用失败尝试的 200。
	so, se, code = runCLI(t, binary, state, "tally", "--id", id, "--now", "210", "--json")
	if code != 0 {
		t.Fatalf("first successful tally at 210 exit=%d: %s", code, se)
	}
	assertCLITallyPassedJSON(t, so, "210")
	if strings.Contains(so, `"tallied_at": 200`) {
		t.Fatalf("must not reuse the failed attempt's 200:\n%s", so)
	}

	// 再次计票返回 210 的首次结论，不随新时间改变；资金仍未被触动。
	so, se, code = runCLI(t, binary, state, "tally", "--id", id, "--now", "300")
	if code != 0 {
		t.Fatalf("repeat tally exit=%d: %s", code, se)
	}
	if !strings.Contains(so, "tallied_at=210") {
		t.Fatalf("repeat tally must return the first 210 conclusion:\n%s", so)
	}
	assertCLITallyFundsUntouched(t, binary, state)
}

// TestCLITallyDirSyncFailureWrittenButUnconfirmed：首次计票时状态文件已替换
// 成功、只在随后的目录同步失败（目录可写可进入但不可读，0o300）时，普通文本
// 与 --json 的 tally 同样以退出码 1 结束，标准输出仍没有计票结果，标准错误
// 说明“已写入但持久性未确认”并保留具体原因。恢复正常读取条件后，查询应显示
// passed 和首次计票时间为 200 的完整结论；在 210 再次计票应正常返回这份已有
// 结论，不改成 210，也不把已通过提案退回 voting。
func TestCLITallyDirSyncFailureWrittenButUnconfirmed(t *testing.T) {
	binary := buildCLI(t)
	state := filepath.Join(t.TempDir(), "treasury.json")
	if _, _, code := runCLI(t, binary, state, "init", "--balance", "1000"); code != 0 {
		t.Fatal("init failed")
	}
	// 同一状态文件中的两项相同提案分别覆盖普通文本与 --json：
	// 各自的首次计票都遇到目录同步失败。
	const idText = "gip-cli-tally-sync-text"
	const idJSON = "gip-cli-tally-sync-json"
	cliTallyPrepare(t, binary, state, idText)
	cliTallyPrepare(t, binary, state, idJSON)

	makeDirSyncFail(t, state)

	// 普通文本：退出码 1，stdout 没有计票结果，stderr 说明已写入但持久性未确认。
	so, se, code := runCLI(t, binary, state, "tally", "--id", idText, "--now", "200")
	if code != 1 {
		t.Fatalf("text tally exit=%d, want 1 (stdout=%q stderr=%q)", code, so, se)
	}
	if so != "" {
		t.Fatalf("stdout must carry no tally result on durability warning, got %q", so)
	}
	if !strings.Contains(se, "durability not confirmed") ||
		!strings.Contains(se, "permission denied") {
		t.Fatalf("stderr must state written-but-unconfirmed with the concrete cause, got %q", se)
	}
	if strings.Contains(se, "still open") {
		t.Fatalf("a deadline-reached tally must not be reported as not yet closed: %q", se)
	}

	// --json：同样退出码 1、stdout 没有计票结果对象，stderr 保留同一告警。
	so, se, code = runCLI(t, binary, state, "tally", "--id", idJSON, "--now", "200", "--json")
	if code != 1 {
		t.Fatalf("json tally exit=%d, want 1 (stdout=%q stderr=%q)", code, so, se)
	}
	if so != "" {
		t.Fatalf("json stdout must carry no tally result on durability warning, got %q", so)
	}
	if !strings.Contains(se, "durability not confirmed") {
		t.Fatalf("json stderr must state the durability warning, got %q", se)
	}

	// 改名已成功：没有遗留临时文件。恢复正常读取条件。
	if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(state), ".govflow-state-*")); len(leftovers) != 0 {
		t.Fatalf("failed dir sync left temporary file(s): %v", leftovers)
	}
	if err := os.Chmod(filepath.Dir(state), 0o700); err != nil {
		t.Fatalf("restore dir: %v", err)
	}

	// 查询：两项提案都显示 passed 和首次计票时间 200 的完整结论，
	// 原票据、委托关系与动作原文保持原样。
	for _, id := range []string{idText, idJSON} {
		so, se, code = runCLI(t, binary, state, "proposal", "--id", id, "--json")
		if code != 0 {
			t.Fatalf("proposal %s query exit=%d: %s", id, code, se)
		}
		if !strings.Contains(so, `"state": "passed"`) {
			t.Fatalf("proposal %s must show the written passed state:\n%s", id, so)
		}
		assertCLITallyPassedJSON(t, so, "200")
		assertCLITallyBallotsJSON(t, so)

		so, se, code = runCLI(t, binary, state, "proposal", "--id", id)
		if code != 0 {
			t.Fatalf("text proposal %s query exit=%d: %s", id, code, se)
		}
		if !strings.Contains(so, "state=passed") ||
			!strings.Contains(so, "tally for=600 against=400 turnout=1000 => passed tallied_at=200") {
			t.Fatalf("text query for %s must show the written 200 conclusion:\n%s", id, so)
		}
		assertCLITallyShapeText(t, so)
	}

	// 在 210 再次计票：正常返回已经写入的 200 结论（退出码 0），不改成 210，
	// 也不把已通过提案退回 voting。
	so, se, code = runCLI(t, binary, state, "tally", "--id", idText, "--now", "210")
	if code != 0 {
		t.Fatalf("repeat tally exit=%d: %s", code, se)
	}
	if !strings.Contains(so, "=> passed (tallied_at=200)") {
		t.Fatalf("repeat tally must return the already-written 200 conclusion:\n%s", so)
	}
	so, se, code = runCLI(t, binary, state, "tally", "--id", idJSON, "--now", "210", "--json")
	if code != 0 {
		t.Fatalf("repeat json tally exit=%d: %s", code, se)
	}
	assertCLITallyPassedJSON(t, so, "200")

	so, se, code = runCLI(t, binary, state, "proposal", "--id", idText, "--json")
	if code != 0 {
		t.Fatalf("proposal query exit=%d: %s", code, se)
	}
	if !strings.Contains(so, `"state": "passed"`) || !strings.Contains(so, `"tallied_at": 200`) {
		t.Fatalf("passed proposal must not fall back to voting or move to 210:\n%s", so)
	}
	assertCLITallyFundsUntouched(t, binary, state)
}
