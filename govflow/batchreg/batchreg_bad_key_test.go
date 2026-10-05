package batchreg

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A member name that cannot be decoded is its own failure: the registry
// parser must call it an invalid field name instead of "root/record must be
// a JSON object", must never quote the substituted spelling, and a root
// member carries no record position while a record member carries the
// 1-based position plus the unambiguous batch id.
func TestLoadRejectsInvalidFieldNames(t *testing.T) {
	const BYTE = "\x00BYTE\x00"
	cases := map[string]struct {
		content string
		pos     int
		batch   string
	}{
		"root raw bad byte": {
			`{"version":1,"batches":[],"x` + BYTE + `":1}`, 0, "",
		},
		"root lone surrogate escape": {
			`{"version":1,"batches":[],"x\ud800":1}`, 0, "",
		},
		"record raw bad byte with batch first": {
			`{"version":1,"batches":[{"batch":"B-002","product":"P","quantity":1,"unit":"kg","x` + BYTE + `":1}]}`,
			1, "B-002",
		},
		"record lone surrogate with batch after": {
			`{"version":1,"batches":[{"x\udc01":1,"batch":"B-002","product":"P","quantity":1,"unit":"kg"}]}`,
			1, "B-002",
		},
		"bad key on second record": {
			`{"version":1,"batches":[
			 {"batch":"B1","product":"P","quantity":1,"unit":"kg"},
			 {"batch":"B2","product":"P","quantity":2,"unit":"kg","x` + BYTE + `":1}]}`,
			2, "B2",
		},
		"batch itself missing leaves no id": {
			`{"version":1,"batches":[{"product":"P","quantity":1,"unit":"kg","x` + BYTE + `":1}]}`,
			1, "",
		},
		"batch duplicated leaves no id": {
			`{"version":1,"batches":[{"batch":"B1","batch":"B2","product":"P","quantity":1,"unit":"kg","x` + BYTE + `":1}]}`,
			1, "",
		},
		"batch non-string leaves no id": {
			`{"version":1,"batches":[{"batch":1,"product":"P","quantity":1,"unit":"kg","x` + BYTE + `":1}]}`,
			1, "",
		},
		"batch value itself bad-encoded leaves no id": {
			`{"version":1,"batches":[{"batch":"B\ud800","product":"P","quantity":1,"unit":"kg","x` + BYTE + `":1}]}`,
			1, "",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "r.json")
			content := []byte(strings.ReplaceAll(tc.content, BYTE, string([]byte{0xff})))
			if err := os.WriteFile(path, content, 0o644); err != nil {
				t.Fatal(err)
			}
			_, _, err := Load(path)
			var fe *FormatError
			if !errors.As(err, &fe) {
				t.Fatalf("expected FormatError, got %v", err)
			}
			if fe.Position != tc.pos || fe.Batch != tc.batch {
				t.Fatalf("FormatError = %+v, want position=%d batch=%q", fe, tc.pos, tc.batch)
			}
			if fe.Field != "" {
				t.Fatalf("the bad field name must not be quoted back, got Field=%q", fe.Field)
			}
			msg := err.Error()
			if !strings.Contains(msg, path) {
				t.Fatalf("error must name the file path: %v", msg)
			}
			if !strings.Contains(msg, "field name") || !strings.Contains(msg, "UTF-8") {
				t.Fatalf("error must say the field name encoding is invalid: %v", msg)
			}
			if strings.Contains(msg, "must be a JSON object") {
				t.Fatalf("a bad field name must not be reported as a non-object: %v", msg)
			}
			if strings.Contains(msg, "unknown field") {
				t.Fatalf("a bad field name must not be reported as an unknown field: %v", msg)
			}
			if tc.pos == 0 && strings.Contains(msg, "record") {
				t.Fatalf("a root problem must not carry a record position: %v", msg)
			}
		})
	}
}

// A record that genuinely is not an object keeps the old message, distinct
// from a bad field name; a bad field *value* is likewise a different fault.
func TestRegistryFailuresStayDistinct(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.json")

	t.Run("not an object", func(t *testing.T) {
		writeRegistry(t, path, `{"version":1,"batches":[null]}`)
		_, _, err := Load(path)
		var fe *FormatError
		if !errors.As(err, &fe) || !strings.Contains(fe.Reason, "must be a JSON object") {
			t.Fatalf("want non-object message, got %v", err)
		}
		if strings.Contains(err.Error(), "field name") {
			t.Fatalf("a non-object is not a field-name fault: %v", err)
		}
	})

	t.Run("bad value encoding", func(t *testing.T) {
		writeRegistry(t, path, "{\"version\":1,\"batches\":[{\"batch\":\"B\\ud800\",\"product\":\"P\",\"quantity\":1,\"unit\":\"kg\"}]}")
		_, _, err := Load(path)
		var fe *FormatError
		if !errors.As(err, &fe) || fe.Position != 1 || fe.Field != "batch" {
			t.Fatalf("want field-value encoding error on batch, got %v", err)
		}
		if strings.Contains(err.Error(), "field name") {
			t.Fatalf("a bad field value must not be called a bad field name: %v", err)
		}
	})
}

// ParseManifest must report a bad field name with the 1-based position and
// unambiguous batch id, keep it distinct from "not an object" and from a
// bad field value, and reject the whole manifest even when earlier records
// are perfectly legal.
func TestParseManifestRejectsInvalidFieldNames(t *testing.T) {
	const BYTE = "\x00BYTE\x00"
	replace := func(s string) []byte {
		return []byte(strings.ReplaceAll(s, BYTE, string([]byte{0xff})))
	}
	cases := map[string]struct {
		content string
		pos     int
		batch   string
	}{
		"bad key on record 2": {
			`[{"batch":"B-001","product":"P","quantity":1,"unit":"kg"},
			  {"batch":"B-002","product":"P","quantity":2,"unit":"kg","x` + BYTE + `":1}]`,
			2, "B-002",
		},
		"bad key before a valid batch": {
			`[{"x` + BYTE + `":1,"batch":" B-002 ","product":"P","quantity":2,"unit":"kg"}]`,
			1, "B-002", // manifest ids are trimmed
		},
		"lone surrogate key": {
			`[{"batch":"B-002","product":"P","quantity":2,"unit":"kg","x\ud800":1}]`,
			1, "B-002",
		},
		"batch missing": {
			`[{"product":"P","quantity":1,"unit":"kg","x` + BYTE + `":1}]`,
			1, "",
		},
		"batch duplicated": {
			`[{"batch":"B1","batch":"B2","product":"P","quantity":1,"unit":"kg","x` + BYTE + `":1}]`,
			1, "",
		},
		"batch not a string": {
			`[{"batch":9,"product":"P","quantity":1,"unit":"kg","x` + BYTE + `":1}]`,
			1, "",
		},
		"batch value itself bad": {
			`[{"batch":"B` + BYTE + `","product":"P","quantity":1,"unit":"kg","x\ud800":1}]`,
			1, "",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseManifest(replace(tc.content))
			var re *ManifestRecordError
			if !errors.As(err, &re) {
				t.Fatalf("expected ManifestRecordError, got %v", err)
			}
			if re.Position != tc.pos || re.Batch != tc.batch {
				t.Fatalf("position=%d batch=%q, want %d/%q (err: %v)",
					re.Position, re.Batch, tc.pos, tc.batch, err)
			}
			var ee *EncodingError
			if errors.As(err, &ee) {
				t.Fatalf("a bad field name must not arrive as an EncodingError (field value): %v", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, "field name") || !strings.Contains(msg, "UTF-8") {
				t.Fatalf("reason must name invalid field-name encoding: %v", msg)
			}
			if strings.Contains(msg, "must be a JSON object") {
				t.Fatalf("a bad field name must not be reported as a non-object: %v", msg)
			}
			if strings.Contains(msg, "unknown field") {
				t.Fatalf("a substituted name must not be reported as an unknown field: %v", msg)
			}
			if strings.ContainsRune(msg, '�') {
				t.Fatalf("the invalid field name must never be echoed back substituted: %v", msg)
			}
		})
	}

	// Not-an-object and bad-field-value keep their own messages.
	for _, tc := range []struct {
		name    string
		content string
		want    string
	}{
		{"not object", `[null]`, "must be a JSON object"},
		{"bad value", `[{"batch":"B\ud800","product":"P","quantity":1,"unit":"kg"}]`, "contains bytes that are not valid UTF-8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseManifest([]byte(tc.content))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want message containing %q, got %v", tc.want, err)
			}
			if strings.Contains(err.Error(), "field name") {
				t.Fatalf("this failure must not be a field-name fault: %v", err)
			}
		})
	}
}

// A genuinely valid Unicode field name — a real U+FFFD rune, Chinese text,
// or a correctly paired surrogate escape — is ordinary text and still goes
// through the normal name rules (i.e. rejected as unknown), never as an
// encoding fault.
func TestValidUnicodeFieldNamesStayNormalNames(t *testing.T) {
	registryContents := map[string]string{
		"genuine replacement char name": `{"version":1,"batches":[{"batch":"B","product":"P","quantity":1,"unit":"kg","x�":1}]}`,
		"chinese name":                  `{"version":1,"batches":[{"batch":"B","product":"P","quantity":1,"unit":"kg","名称":1}]}`,
		"matched surrogate pair name":   `{"version":1,"batches":[{"batch":"B","product":"P","quantity":1,"unit":"kg","x😀":1}]}`,
	}
	for name, content := range registryContents {
		t.Run("registry/"+name, func(t *testing.T) {
			_, err := decode([]byte(content))
			var fe *FormatError
			if !errors.As(err, &fe) {
				t.Fatalf("expected FormatError, got %v", err)
			}
			if strings.Contains(fe.Reason, "field name is not valid UTF-8") {
				t.Fatalf("a legal Unicode name must not be an encoding fault: %v", err)
			}
		})
	}

	manifestContents := map[string]string{
		"genuine replacement char name": `[{"batch":"B","product":"P","quantity":1,"unit":"kg","x�":1}]`,
		"chinese name":                  `[{"batch":"B","product":"P","quantity":1,"unit":"kg","名称":1}]`,
		"matched surrogate pair name":   `[{"batch":"B","product":"P","quantity":1,"unit":"kg","x😀":1}]`,
	}
	for name, content := range manifestContents {
		t.Run("manifest/"+name, func(t *testing.T) {
			_, err := ParseManifest([]byte(content))
			if err == nil {
				t.Fatal("unknown field must still reject")
			}
			if strings.Contains(err.Error(), "field name is not valid UTF-8") {
				t.Fatalf("a legal Unicode name must not be an encoding fault: %v", err)
			}
			var ee *EncodingError
			if errors.As(err, &ee) {
				t.Fatalf("a bad name must not arrive as a field-value EncodingError: %v", err)
			}
		})
	}
}
