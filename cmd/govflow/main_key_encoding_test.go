package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// End-to-end regression for undecodable field NAMES (object keys holding
// malformed UTF-8 bytes or a lone surrogate escape) in the files both batch
// commands read. The whole operation must be rejected with a non-zero exit
// and no stdout result; the standard error must say the field NAME encoding
// is invalid (never "must be an object", never "unknown field") and must
// name the file at fault — the root object without a record position, a
// record with its 1-based position, plus the batch id when exactly one
// valid batch member determines it. The manifest stays read-only, an
// existing registry keeps bytes and mtime, and a missing registry is not
// created.

// badKeyByte is a raw invalid UTF-8 byte placed inside a JSON object key.
const badKeyByte = "\xff"

func TestBatchImportCLIRejectsBadFieldNameInManifest(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "reg.json")
	manifest := filepath.Join(dir, "in.json")
	// Record 1 is legal; record 2 carries a bad key after its valid batch.
	content := []byte(`[{"batch":"B-001","product":"P","quantity":1,"unit":"kg"},` +
		`{"batch":"B-002","product":"P","quantity":2,"unit":"kg","su` + badKeyByte + `plier":"S"}]`)
	if err := os.WriteFile(manifest, content, 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout)
	if err == nil {
		t.Fatal("a manifest with an undecodable field name must be rejected")
	}
	msg := err.Error()
	for _, want := range []string{manifest, "record 2", `"B-002"`, "field name"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error must mention %s: %v", want, msg)
		}
	}
	for _, banned := range []string{"must be a JSON object", "unknown field", "�"} {
		if strings.Contains(msg, banned) {
			t.Errorf("error must not contain %q: %v", banned, msg)
		}
	}
	if stdout.Len() != 0 {
		t.Fatalf("a rejected import must print no success result, got %q", stdout.String())
	}
	if _, statErr := os.Stat(registry); !os.IsNotExist(statErr) {
		t.Fatal("a rejected first import must not create the registry")
	}
	after, rerr := os.ReadFile(manifest)
	if rerr != nil || !bytes.Equal(after, content) {
		t.Fatal("the read-only manifest was modified")
	}
}

func TestBatchImportCLIRejectsBadFieldNameInRegistry(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "reg.json")
	manifest := filepath.Join(dir, "in.json")
	// The bad key sits in record 2, before its batch member: the id must
	// still be found and cited, and record 1 stays blameless.
	writeFile(t, registry, `{"version":1,"batches":[
		{"batch":"B-001","product":"P","quantity":1,"unit":"kg"},
		{"sup`+badKeyByte+`plier":"S","batch":"B-002","product":"P","quantity":2,"unit":"kg"}]}`)
	writeFile(t, manifest, `[{"batch":"B-003","product":"P","quantity":3,"unit":"kg"}]`)
	content, pinned := pinRegistry(t, registry)

	var stdout bytes.Buffer
	err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout)
	if err == nil {
		t.Fatal("a registry with an undecodable field name must be rejected")
	}
	msg := err.Error()
	for _, want := range []string{registry, "record 2", `"B-002"`, "field name"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error must mention %s: %v", want, msg)
		}
	}
	if strings.Contains(msg, "must be a JSON object") || strings.Contains(msg, "unknown field") {
		t.Errorf("error must not misstate the cause: %v", msg)
	}
	if stdout.Len() != 0 {
		t.Fatalf("a rejected import must print no success result, got %q", stdout.String())
	}
	assertRegistryUntouched(t, registry, content, pinned)
}

func TestBatchRegisterCLIRejectsBadFieldNameInRoot(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "reg.json")
	// A bad key in the ROOT object: no record position may be attached.
	writeFile(t, registry, `{"version":1,"batches":[],"su`+badKeyByte+`plier":"S"}`)
	content, pinned := pinRegistry(t, registry)

	var stdout bytes.Buffer
	err := runBatchRegister([]string{
		"--registry", registry, "--batch", "B-1", "--product", "P",
		"--quantity", "1", "--unit", "kg",
	}, &stdout)
	if err == nil {
		t.Fatal("a registry whose root object has an undecodable field name must be rejected")
	}
	msg := err.Error()
	for _, want := range []string{registry, "root object", "field name"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error must mention %s: %v", want, msg)
		}
	}
	if strings.Contains(msg, "record ") {
		t.Errorf("a root-object problem must not cite a record position: %v", msg)
	}
	if stdout.Len() != 0 {
		t.Fatalf("a rejected registration must print no success result, got %q", stdout.String())
	}
	assertRegistryUntouched(t, registry, content, pinned)
}

func TestBatchImportCLIRejectsLoneSurrogateEscapeFieldName(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "reg.json")
	manifest := filepath.Join(dir, "in.json")
	// The key hides a lone high-surrogate escape; the batch id sits after it.
	writeFile(t, manifest, `[{"sup\ud800lier":"S","batch":" B-002 ","product":"P","quantity":2,"unit":"kg"}]`)

	var stdout bytes.Buffer
	err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout)
	if err == nil {
		t.Fatal("a lone-surrogate field name must be rejected")
	}
	msg := err.Error()
	// The manifest id rule trims the padded batch before citing it.
	for _, want := range []string{manifest, "record 1", `"B-002"`, "field name"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error must mention %s: %v", want, msg)
		}
	}
	if _, statErr := os.Stat(registry); !os.IsNotExist(statErr) {
		t.Fatal("a rejected first import must not create the registry")
	}
}

// TestBadFieldNameFailsThroughRun proves the rejection surfaces through the
// command dispatcher as a non-nil error — i.e. a non-zero process exit.
func TestBadFieldNameFailsThroughRun(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "reg.json")
	manifest := filepath.Join(dir, "in.json")
	writeFile(t, manifest, `[{"batch":"B-002","x`+badKeyByte+`y":1,"product":"P","quantity":2,"unit":"kg"}]`)

	if err := run([]string{"batch-import", "--registry", registry, "--input", manifest}); err == nil {
		t.Fatal("run must return an error so the process exits non-zero")
	}
	if _, statErr := os.Stat(registry); !os.IsNotExist(statErr) {
		t.Fatal("a rejected first import must not create the registry")
	}
}
