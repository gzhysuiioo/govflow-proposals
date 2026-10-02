package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runBatchRegister(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestCLISuccessOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reg.jsonl")
	code, stdout, stderr := runCLI(t, "--registry", path, "--batch", "B001", "--product", "P100", "--quantity", "100", "--unit", "箱")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("stdout must be one JSON object, got %q: %v", stdout, err)
	}
	if out["batch"] != "B001" || out["product"] != "P100" || out["unit"] != "箱" {
		t.Fatalf("unexpected fields: %v", out)
	}
	if out["quantity"].(float64) != 100 {
		t.Fatalf("quantity = %v", out["quantity"])
	}
	if out["status"] != "new" {
		t.Fatalf("status = %v, want new", out["status"])
	}
	if strings.TrimSpace(stdout) != strings.TrimSpace(stdout) || strings.Count(stdout, "\n") != 1 {
		t.Fatalf("stdout must contain exactly one line, got %q", stdout)
	}
}

func TestCLIDuplicateOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reg.jsonl")
	runCLI(t, "--registry", path, "--batch", "B001", "--product", "P100", "--quantity", "100", "--unit", "箱")

	code, stdout, stderr := runCLI(t, "--registry", path, "--batch", "B001", "--product", "P100", "--quantity", "0100", "--unit", "箱")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	var out map[string]any
	json.Unmarshal([]byte(stdout), &out)
	if out["status"] != "duplicate" {
		t.Fatalf("status = %v, want duplicate", out["status"])
	}
}

func TestCLIConflictExitCodeAndMessage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reg.jsonl")
	runCLI(t, "--registry", path, "--batch", "B001", "--product", "P100", "--quantity", "100", "--unit", "箱")

	code, stdout, stderr := runCLI(t, "--registry", path, "--batch", "B001", "--product", "P100", "--quantity", "200", "--unit", "箱")
	if code == 0 {
		t.Fatal("conflict must exit non-zero")
	}
	if stdout != "" {
		t.Fatalf("stdout must be empty on failure, got %q", stdout)
	}
	if !strings.Contains(stderr, "B001") || !strings.Contains(stderr, "quantity") {
		t.Fatalf("stderr must name batch and differing field, got %q", stderr)
	}
}

func TestCLIMissingFlags(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reg.jsonl")
	code, stdout, stderr := runCLI(t, "--registry", path, "--batch", "B1")
	if code == 0 {
		t.Fatal("missing flags must fail")
	}
	if stdout != "" {
		t.Fatalf("stdout must be empty on failure, got %q", stdout)
	}
	for _, flag := range []string{"--product", "--quantity", "--unit"} {
		if !strings.Contains(stderr, flag) {
			t.Fatalf("stderr should name missing flag %s, got %q", flag, stderr)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("no registry file must be created on missing flags")
	}
}

func TestCLIInvalidQuantity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reg.jsonl")
	for _, bad := range []string{"0", "-1", "1.5", "1e3", "abc", "9223372036854775808"} {
		code, stdout, _ := runCLI(t, "--registry", path, "--batch", "B1", "--product", "P1", "--quantity", bad, "--unit", "箱")
		if code == 0 {
			t.Fatalf("quantity %q must be rejected", bad)
		}
		if stdout != "" {
			t.Fatalf("stdout must be empty on failure for %q, got %q", bad, stdout)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("no registry file must be created on invalid input")
	}
}

func TestCLIEmptyExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reg.jsonl")
	os.WriteFile(path, []byte(""), 0o644)
	code, _, stderr := runCLI(t, "--registry", path, "--batch", "B1", "--product", "P1", "--quantity", "1", "--unit", "箱")
	if code == 0 {
		t.Fatal("empty existing file must be rejected")
	}
	if !strings.Contains(stderr, "empty") {
		t.Fatalf("stderr should explain the empty file, got %q", stderr)
	}
}

func TestCLIHelp(t *testing.T) {
	code, stdout, stderr := runCLI(t, "-h")
	if code != 0 {
		t.Fatalf("-h exit = %d, stderr = %s", code, stderr)
	}
	for _, want := range []string{"--registry", "--batch", "--product", "--quantity", "--unit", "duplicate", "status", "json"} {
		if !strings.Contains(strings.ToLower(stdout), strings.ToLower(want)) {
			t.Fatalf("help should mention %q, got:\n%s", want, stdout)
		}
	}
}

func TestCLINoArgs(t *testing.T) {
	code, _, stderr := runCLI(t)
	if code == 0 {
		t.Fatal("no args must fail with usage error")
	}
	if !strings.Contains(stderr, "--registry") {
		t.Fatalf("stderr should guide the user, got %q", stderr)
	}
}

func TestCLIPositionalArgsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reg.jsonl")
	code, _, _ := runCLI(t, "--registry", path, "--batch", "B1", "--product", "P1", "--quantity", "1", "--unit", "箱", "extra")
	if code == 0 {
		t.Fatal("positional arguments must be rejected")
	}
}
