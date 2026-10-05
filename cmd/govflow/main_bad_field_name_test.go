package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A member name with invalid encoding rejects the whole import: record 1 is
// legal but is not registered, record 2 names its position and batch id,
// stdout stays empty, the manifest is read-only and a missing registry is
// never created.
func TestBatchImportCLIRejectsInvalidFieldName(t *testing.T) {
	cases := map[string]struct {
		content []byte
		pos     string
		batch   string
	}{
		"raw bad byte in second record's field name": {
			[]byte("[{\"batch\":\"B-001\",\"product\":\"P\",\"quantity\":1,\"unit\":\"kg\"}," +
				"{\"batch\":\"B-002\",\"product\":\"P\",\"quantity\":2,\"unit\":\"kg\",\"x\xff\":1}]"),
			"record 2", "B-002",
		},
		"lone surrogate key after a valid first record": {
			[]byte(`[{"batch":"B-001","product":"P","quantity":1,"unit":"kg"},` +
				`{"batch":"B-002","product":"P","quantity":2,"unit":"kg","x\ud800":1}]`),
			"record 2", "B-002",
		},
		"bad key before the batch member": {
			[]byte("[{\"x\xff\":1,\"batch\":\" B-002 \",\"product\":\"P\",\"quantity\":2,\"unit\":\"kg\"}]"),
			"record 1", "B-002",
		},
		"bad key with no batch id to name": {
			[]byte("[{\"product\":\"P\",\"quantity\":1,\"unit\":\"kg\",\"x\xff\":1}]"),
			"record 1", "",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			registry := filepath.Join(dir, "reg.json")
			manifest := filepath.Join(dir, "in.json")
			if err := os.WriteFile(manifest, tc.content, 0o644); err != nil {
				t.Fatal(err)
			}

			var stdout bytes.Buffer
			err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout)
			if err == nil {
				t.Fatal("an invalid field name must reject the import")
			}
			msg := err.Error()
			for _, want := range []string{manifest, tc.pos, "field name"} {
				if !strings.Contains(msg, want) {
					t.Fatalf("error must contain %q: %v", want, msg)
				}
			}
			if !strings.Contains(msg, "UTF-8") {
				t.Fatalf("error must explain the field-name encoding fault: %v", msg)
			}
			if strings.Contains(msg, "must be a JSON object") {
				t.Fatalf("a bad field name must not look like a non-object: %v", msg)
			}
			if strings.Contains(msg, "unknown field") {
				t.Fatalf("a substituted name must not be called an unknown field: %v", msg)
			}
			if tc.batch != "" {
				if !strings.Contains(msg, "batch "+strconv.Quote(tc.batch)) {
					t.Fatalf("error must name batch %q: %v", tc.batch, msg)
				}
			} else if strings.Contains(msg, "batch \"") {
				t.Fatalf("error must not invent a batch id: %v", msg)
			}
			if strings.ContainsRune(msg, '�') {
				t.Fatalf("the invalid field name must not be substituted into the message: %v", msg)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout must stay empty on failure, got %q", stdout.String())
			}
			// First record must not have landed; registry must not exist.
			if _, statErr := os.Stat(registry); !os.IsNotExist(statErr) {
				t.Fatal("a rejected import must not create the registry file")
			}
			after, rerr := os.ReadFile(manifest)
			if rerr != nil || !bytes.Equal(after, tc.content) {
				t.Fatal("the read-only manifest was modified")
			}
		})
	}
}

// Against an existing registry, an invalid field name in a registry record
// (or in the root object) blocks both commands, names the registry path and
// the exact location, and leaves bytes and mtime untouched.
func TestInvalidFieldNameRegistryBlocksBothCommands(t *testing.T) {
	registries := map[string]struct {
		content  []byte
		locator  string // "root object" for root faults, else "record N"
		batch    string
		isRecord bool
	}{
		"root bad key": {
			[]byte("{\"version\":1,\"batches\":[],\"x\xff\":1}"),
			"root object", "", false,
		},
		"root lone surrogate key": {
			[]byte(`{"version":1,"batches":[],"x\ud800":1}`),
			"root object", "", false,
		},
		"record bad key names batch": {
			[]byte("{\"version\":1,\"batches\":[{\"batch\":\"B2\",\"product\":\"P\",\"quantity\":2,\"unit\":\"kg\",\"x\xff\":1}]}"),
			"record 1", "B2", true,
		},
	}
	manifestContent := []byte(`[{"batch":"NEW","product":"P","quantity":1,"unit":"kg"}]`)
	for name, tc := range registries {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			registry := filepath.Join(dir, "reg.json")
			manifest := filepath.Join(dir, "in.json")
			if err := os.WriteFile(registry, tc.content, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(manifest, manifestContent, 0o644); err != nil {
				t.Fatal(err)
			}
			pinned := time.Date(2010, time.January, 2, 3, 4, 5, 0, time.UTC)
			if err := os.Chtimes(registry, pinned, pinned); err != nil {
				t.Fatal(err)
			}

			assertBlocked := func(err error, stdout *bytes.Buffer) {
				t.Helper()
				if err == nil {
					t.Fatal("the registry must be rejected")
				}
				msg := err.Error()
				for _, want := range []string{registry, tc.locator, "field name", "UTF-8"} {
					if !strings.Contains(msg, want) {
						t.Fatalf("error must contain %q: %v", want, msg)
					}
				}
				if !tc.isRecord && strings.Contains(msg, "record") {
					t.Fatalf("a root-object fault must not carry a record position: %v", msg)
				}
				if tc.batch != "" && !strings.Contains(msg, tc.batch) {
					t.Fatalf("error must name batch %q: %v", tc.batch, msg)
				}
				if strings.Contains(msg, "must be a JSON object") {
					t.Fatalf("a bad field name must not be called a non-object: %v", msg)
				}
				if stdout.Len() != 0 {
					t.Fatalf("stdout must stay empty, got %q", stdout.String())
				}
			}

			var stdout bytes.Buffer
			err := runBatchRegister([]string{
				"--registry", registry, "--batch", "NEW", "--product", "P",
				"--quantity", "1", "--unit", "kg",
			}, &stdout)
			assertBlocked(err, &stdout)

			stdout.Reset()
			err = runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout)
			assertBlocked(err, &stdout)

			after, rerr := os.ReadFile(registry)
			if rerr != nil || !bytes.Equal(after, tc.content) {
				t.Fatalf("registry bytes changed: %q", after)
			}
			if info, serr := os.Stat(registry); serr != nil || !info.ModTime().Equal(pinned) {
				t.Fatalf("registry mtime changed: %v", serr)
			}
			if mAfter, merr := os.ReadFile(manifest); merr != nil || !bytes.Equal(mAfter, manifestContent) {
				t.Fatal("manifest was modified")
			}
		})
	}
}

// A genuinely valid Unicode field name keeps using the ordinary name rules
// end to end: it surfaces as an unknown field, not an encoding fault.
func TestBatchImportCLIValidUnicodeFieldNameIsOrdinaryUnknown(t *testing.T) {
	dir := t.TempDir()
	registry := filepath.Join(dir, "reg.json")
	manifest := filepath.Join(dir, "in.json")
	writeFile(t, manifest, `[{"batch":"B","product":"P","quantity":1,"unit":"kg","名称":1}]`)

	var stdout bytes.Buffer
	err := runBatchImport([]string{"--registry", registry, "--input", manifest}, &stdout)
	if err == nil {
		t.Fatal("the unknown field must reject")
	}
	msg := err.Error()
	if !strings.Contains(msg, strconv.Quote("名称")) {
		t.Fatalf("error must name the legal Unicode field: %v", msg)
	}
	if strings.Contains(msg, "field name is not valid UTF-8") {
		t.Fatalf("a legal Unicode name is not an encoding fault: %v", msg)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout must stay empty, got %q", stdout.String())
	}
}
