package govflow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// prepareCLIVoteTableState 通过 CLI 构造一份含多种记录的状态文件：
// gip-reg 登记提案已执行（资金库 100 -> 60，audits 40），gip-vote 投票提案
// 处于投票中且已有一张赞成票。返回状态文件路径。
func prepareCLIVoteTableState(t *testing.T, binary, state string) {
	t.Helper()
	if _, se, code := runCLI(t, binary, state, "init", "--balance", "100"); code != 0 {
		t.Fatalf("init: %s", se)
	}
	if _, se, code := runCLI(t, binary, state, "register", "--id", "gip-reg",
		"--timelock", "0", "--action", "transfer:audits:40"); code != 0 {
		t.Fatalf("register: %s", se)
	}
	if _, se, code := runCLI(t, binary, state, "execute", "--id", "gip-reg", "--now", "0"); code != 0 {
		t.Fatalf("execute: %s", se)
	}
	args := []string{"create-vote", "--id", "gip-vote",
		"--member", "alice:600", "--member", "bob:400",
		"--quorum", "600", "--start", "0", "--deadline", "100", "--timelock", "200",
		"--action", "transfer:audits:10"}
	if _, se, code := runCLI(t, binary, state, args...); code != 0 {
		t.Fatalf("create-vote: %s", se)
	}
	if _, se, code := runCLI(t, binary, state, "vote", "--id", "gip-vote",
		"--voter", "alice", "--choice", "for", "--now", "50"); code != 0 {
		t.Fatalf("vote: %s", se)
	}
}

// TestCLIVoteProposalsTableCorrupt：vote_proposals 整表缺损时，普通文本与
// --json 查询都以退出码 1 结束、stdout 没有任何查询结果、stderr 指出
// vote_proposals 字段以及空值或类型不符的具体原因；创建提案同样被拒绝且不
// 覆盖原文件。Go 调用方按 ErrStateCorrupt 识别由包内测试保证，这里验证 CLI
// 边界行为。
func TestCLIVoteProposalsTableCorrupt(t *testing.T) {
	binary := buildCLI(t)
	state := filepath.Join(t.TempDir(), "treasury.json")
	prepareCLIVoteTableState(t, binary, state)

	cases := map[string]struct {
		mutate func(doc map[string]any)
		want   string
	}{
		"null": {
			mutate: func(doc map[string]any) { doc["vote_proposals"] = nil },
			want:   `field "vote_proposals" is null`,
		},
		"array": {
			mutate: func(doc map[string]any) { doc["vote_proposals"] = []any{} },
			want:   `field "vote_proposals" has wrong type: want object, got array`,
		},
		"string": {
			mutate: func(doc map[string]any) { doc["vote_proposals"] = "gip-vote" },
			want:   `field "vote_proposals" has wrong type: want object, got string`,
		},
		"number": {
			mutate: func(doc map[string]any) { doc["vote_proposals"] = 0 },
			want:   `field "vote_proposals" has wrong type: want object, got number`,
		},
		"boolean": {
			mutate: func(doc map[string]any) { doc["vote_proposals"] = false },
			want:   `field "vote_proposals" has wrong type: want object, got boolean`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "treasury.json")
			if err := os.WriteFile(path, mustReadFile(t, state), 0o600); err != nil {
				t.Fatal(err)
			}
			rewriteTopLevelField(t, path, tc.mutate)
			corrupt := mustReadFile(t, path)

			// 普通文本查询：退出码 1、stdout 空、stderr 指出字段与原因。
			so, se, code := runCLI(t, binary, path, "balances")
			if code != 1 {
				t.Fatalf("text balances code=%d, want 1", code)
			}
			if so != "" {
				t.Fatalf("text balances stdout must be empty, got %q", so)
			}
			if !strings.Contains(se, tc.want) {
				t.Fatalf("text balances stderr=%q, want substring %q", se, tc.want)
			}

			// --json 查询同样退出码 1、stdout 没有查询结果。
			so, se, code = runCLI(t, binary, path, "proposal", "--id", "gip-vote", "--json")
			if code != 1 {
				t.Fatalf("json proposal code=%d, want 1", code)
			}
			if so != "" {
				t.Fatalf("json proposal stdout must be empty, got %q", so)
			}
			if !strings.Contains(se, tc.want) {
				t.Fatalf("json proposal stderr=%q, want substring %q", se, tc.want)
			}

			// 查询某编号不能报成“未找到”：stderr 必须是状态损坏而不是 not found。
			if strings.Contains(se, "proposal not found") {
				t.Fatalf("corrupt table must not be reported as proposal not found: %q", se)
			}

			// 其余查询命令同样拒绝，stdout 必须为空。
			for _, args := range [][]string{
				{"proposals", "--json"},
				{"receipts", "--json"},
				{"receipt", "--id", "gip-reg", "--json"},
				{"balances", "--account", "audits", "--json"},
			} {
				so, se, code = runCLI(t, binary, path, args...)
				if code != 1 || so != "" || !strings.Contains(se, tc.want) {
					t.Fatalf("query %v code=%d stdout=%q stderr=%q, want code 1 / empty stdout / %q",
						args, code, so, se, tc.want)
				}
			}

			// 创建投票提案被拒绝，且不能用新记录覆盖缺损文件。
			so, se, code = runCLI(t, binary, path, "create-vote", "--id", "gip-new",
				"--member", "zoe:1", "--quorum", "1",
				"--start", "0", "--deadline", "10", "--timelock", "10")
			if code != 1 {
				t.Fatalf("create-vote on corrupt table code=%d, want 1", code)
			}
			if so != "" {
				t.Fatalf("create-vote stdout must be empty, got %q", so)
			}
			if !strings.Contains(se, tc.want) {
				t.Fatalf("create-vote stderr=%q, want substring %q", se, tc.want)
			}
			if got := mustReadFile(t, path); string(got) != string(corrupt) {
				t.Fatalf("corrupt state file was overwritten by rejected create-vote")
			}
		})
	}
}

// TestCLILegacyFileWithoutVoteTable：旧文件没有整张 vote_proposals 表时仍可
// 查询余额、登记提案与执行凭据，并能创建第一项投票提案；提交后文件包含
// 明确写出的投票表对象。纯查询不改动旧文件。
func TestCLILegacyFileWithoutVoteTable(t *testing.T) {
	binary := buildCLI(t)
	state := filepath.Join(t.TempDir(), "treasury.json")
	prepareCLIVoteTableState(t, binary, state)

	// 删除整张表得到旧文件（登记提案与执行凭据保留）。
	rewriteTopLevelField(t, state, func(doc map[string]any) { delete(doc, "vote_proposals") })
	legacy := mustReadFile(t, state)

	// 纯查询照常，且不重写旧文件。
	so, se, code := runCLI(t, binary, state, "balances", "--json")
	if code != 0 {
		t.Fatalf("legacy balances failed: %s", se)
	}
	if !strings.Contains(so, `"treasury": 60`) || !strings.Contains(so, `"audits": 40`) {
		t.Fatalf("legacy balances must preserve funds:\n%s", so)
	}
	so, se, code = runCLI(t, binary, state, "receipt", "--id", "gip-reg", "--json")
	if code != 0 || !strings.Contains(so, `"proposal_id": "gip-reg"`) {
		t.Fatalf("legacy receipt code=%d stdout=%s stderr=%s", code, so, se)
	}
	if got := mustReadFile(t, state); string(got) != string(legacy) {
		t.Fatalf("read-only queries must not rewrite the legacy file")
	}

	// 在旧文件上创建第一项投票提案：成功落盘。
	so, se, code = runCLI(t, binary, state, "create-vote", "--id", "gip-first",
		"--member", "zoe:1", "--quorum", "1",
		"--start", "0", "--deadline", "10", "--timelock", "10")
	if code != 0 {
		t.Fatalf("create first vote on legacy file failed: %s", se)
	}

	// 重开查询：第一项提案存在且处于投票中。
	so, se, code = runCLI(t, binary, state, "proposal", "--id", "gip-first", "--json")
	if code != 0 {
		t.Fatalf("query first vote failed: %s", se)
	}
	if !strings.Contains(so, `"id": "gip-first"`) || !strings.Contains(so, `"state": "voting"`) {
		t.Fatalf("first vote proposal must be persisted:\n%s", so)
	}

	// 落盘文件现在明确写出对象形状的 vote_proposals 表。
	rewritten := mustReadFile(t, state)
	var doc map[string]any
	if err := json.Unmarshal(rewritten, &doc); err != nil {
		t.Fatal(err)
	}
	table, ok := doc["vote_proposals"].(map[string]any)
	if !ok {
		t.Fatalf("vote_proposals must be an explicitly written object after first create")
	}
	if _, ok := table["gip-first"]; !ok {
		t.Fatalf("created proposal missing from persisted table: %v", table)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
