package batchreg

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Regression net for undecodable field NAMES (object keys) in the two
// on-disk JSON sources. A key holding malformed UTF-8 bytes or a lone
// surrogate escape must reject the whole operation with its own cause —
// "field name encoding invalid" — never the "root/record must be an object"
// misdiagnosis, never an "unknown field" verdict against the U+FFFD-
// substituted text, and never a silently dropped batch id.
//
// The BYTE placeholder stands in for a real invalid UTF-8 byte inside the
// raw strings; SUR stays a literal lone-surrogate JSON escape.
const (
	keyBYTE = "\x00BYTE\x00"
	keySUR  = "\x00SUR\x00"
)

func keyFaultContent(s string) []byte {
	r := strings.NewReplacer(keyBYTE, string([]byte{0xff}), keySUR, `\ud800`)
	return []byte(r.Replace(s))
}

// assertKeyEncodingReason pins the wording that distinguishes the three
// rejection causes: the message must say the field NAME encoding is invalid,
// and must not read like the not-an-object cause or the unknown-field cause.
// It must also never echo a substituted name.
func assertKeyEncodingReason(t *testing.T, msg string) {
	t.Helper()
	if !strings.Contains(msg, "field name") || !strings.Contains(msg, "UTF-8") {
		t.Errorf("error must report an invalid field NAME encoding: %v", msg)
	}
	if strings.Contains(msg, "must be a JSON object") {
		t.Errorf("bad field name misreported as not-an-object: %v", msg)
	}
	if strings.Contains(msg, "unknown field") {
		t.Errorf("bad field name misreported as an unknown field: %v", msg)
	}
	if strings.Contains(msg, "�") {
		t.Errorf("error must not echo a U+FFFD-substituted name: %v", msg)
	}
}

func TestLoadRejectsUndecodableFieldNames(t *testing.T) {
	cases := map[string]struct {
		content string
		root    bool   // problem is in the root object, not a record
		pos     int    // 1-based record position when root is false
		batch   string // batch id the error must cite; "" for none
	}{
		"root bad key bytes": {
			`{"version":1,"batches":[],"su` + keyBYTE + `plier":"S"}`,
			true, 0, "",
		},
		"root lone surrogate escape key": {
			`{"version":1,"batches":[],"sup` + keySUR + `lier":"S"}`,
			true, 0, "",
		},
		"record bad key after valid batch": {
			`{"version":1,"batches":[{"batch":"B-002","product":"P","quantity":1,"unit":"kg","su` + keyBYTE + `plier":"S"}]}`,
			false, 1, "B-002",
		},
		"record bad key before valid batch": {
			`{"version":1,"batches":[{"su` + keyBYTE + `plier":"S","batch":"B-002","product":"P","quantity":1,"unit":"kg"}]}`,
			false, 1, "B-002",
		},
		"second record bad key keeps first record blameless": {
			`{"version":1,"batches":[
			 {"batch":"B-001","product":"P","quantity":1,"unit":"kg"},
			 {"batch":"B-002","product":"P","quantity":2,"unit":"kg","x` + keySUR + `y":1}]}`,
			false, 2, "B-002",
		},
		"registry keeps padded batch id verbatim": {
			`{"version":1,"batches":[{"batch":" B2 ","su` + keyBYTE + `plier":"S","product":"P","quantity":1,"unit":"kg"}]}`,
			false, 1, " B2 ",
		},
		"duplicated batch cites no id": {
			`{"version":1,"batches":[{"batch":"B2","batch":"B2","su` + keyBYTE + `plier":"S","product":"P","quantity":1,"unit":"kg"}]}`,
			false, 1, "",
		},
		"non-string batch cites no id": {
			`{"version":1,"batches":[{"batch":7,"su` + keyBYTE + `plier":"S","product":"P","quantity":1,"unit":"kg"}]}`,
			false, 1, "",
		},
		"badly encoded batch value cites no id": {
			`{"version":1,"batches":[{"batch":"B` + keyBYTE + `","su` + keySUR + `plier":"S","product":"P","quantity":1,"unit":"kg"}]}`,
			false, 1, "",
		},
		"missing batch cites no id": {
			`{"version":1,"batches":[{"su` + keyBYTE + `plier":"S","product":"P","quantity":1,"unit":"kg"}]}`,
			false, 1, "",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "registry.json")
			content := keyFaultContent(tc.content)
			if err := os.WriteFile(path, content, 0o644); err != nil {
				t.Fatal(err)
			}
			_, _, err := Load(path)
			var fe *FormatError
			if !errors.As(err, &fe) {
				t.Fatalf("expected FormatError, got %v", err)
			}
			if tc.root && fe.Position != 0 {
				t.Fatalf("root problem must carry no record position, got %d", fe.Position)
			}
			if !tc.root && fe.Position != tc.pos {
				t.Fatalf("position=%d, want %d", fe.Position, tc.pos)
			}
			if fe.Field != "" {
				t.Fatalf("an undecodable name must not be quoted back, got field %q", fe.Field)
			}
			if fe.Batch != tc.batch {
				t.Fatalf("batch=%q, want %q (error: %v)", fe.Batch, tc.batch, err)
			}
			msg := err.Error()
			assertKeyEncodingReason(t, msg)
			if !strings.Contains(msg, path) {
				t.Fatalf("error must name the file path: %v", msg)
			}
			if tc.root && !strings.Contains(msg, "root object") {
				t.Fatalf("root problem must be located at the root object: %v", msg)
			}
			// The rejected file must stay exactly as it was.
			after, rerr := os.ReadFile(path)
			if rerr != nil || string(after) != string(content) {
				t.Fatal("rejected registry was modified")
			}
		})
	}
}

func TestParseManifestRejectsUndecodableFieldNames(t *testing.T) {
	cases := map[string]struct {
		content string
		pos     int
		batch   string
	}{
		"bad key after valid batch": {
			`[{"batch":"B-002","product":"P","quantity":1,"unit":"kg","su` + keyBYTE + `plier":"S"}]`,
			1, "B-002",
		},
		"bad key before valid batch": {
			`[{"su` + keySUR + `plier":"S","batch":"B-002","product":"P","quantity":1,"unit":"kg"}]`,
			1, "B-002",
		},
		"second record bad key rejects the whole manifest": {
			`[{"batch":"OK1","product":"P","quantity":1,"unit":"kg"},
			  {"batch":"B-002","product":"P","quantity":2,"unit":"kg","x` + keyBYTE + `y":1}]`,
			2, "B-002",
		},
		"manifest trims the padded batch id": {
			`[{"batch":"  B-002  ","su` + keyBYTE + `plier":"S","product":"P","quantity":1,"unit":"kg"}]`,
			1, "B-002",
		},
		"duplicated batch cites no id": {
			`[{"batch":"B2","batch":"B2","su` + keyBYTE + `plier":"S","product":"P","quantity":1,"unit":"kg"}]`,
			1, "",
		},
		"non-string batch cites no id": {
			`[{"batch":7,"su` + keyBYTE + `plier":"S","product":"P","quantity":1,"unit":"kg"}]`,
			1, "",
		},
		"badly encoded batch value cites no id": {
			`[{"batch":"B` + keySUR + `","su` + keyBYTE + `plier":"S","product":"P","quantity":1,"unit":"kg"}]`,
			1, "",
		},
		"missing batch cites no id": {
			`[{"su` + keyBYTE + `plier":"S","product":"P","quantity":1,"unit":"kg"}]`,
			1, "",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			inputs, err := ParseManifest(keyFaultContent(tc.content))
			var re *ManifestRecordError
			if !errors.As(err, &re) {
				t.Fatalf("expected ManifestRecordError, got %v", err)
			}
			if inputs != nil {
				t.Fatalf("a rejected manifest must yield no inputs, got %+v", inputs)
			}
			if re.Position != tc.pos {
				t.Fatalf("position=%d, want %d", re.Position, tc.pos)
			}
			if re.Batch != tc.batch {
				t.Fatalf("batch=%q, want %q (error: %v)", re.Batch, tc.batch, err)
			}
			assertKeyEncodingReason(t, err.Error())
		})
	}
}

// TestDecodableUnusualFieldNamesKeepNameRules is the acceptance mirror: a
// genuine U+FFFD, Chinese text or a correctly paired surrogate escape is a
// legal Unicode field NAME. Such names are judged by the ordinary name
// rules — here: rejected as unknown fields under their real decoded text —
// and must never be confused with the name-encoding cause.
func TestDecodableUnusualFieldNamesKeepNameRules(t *testing.T) {
	fffd := string(rune(0xfffd))
	cases := map[string]struct {
		key  string // raw JSON key spelling inside the record
		name string // decoded name the error must quote
	}{
		"genuine replacement character": {`"sup` + fffd + `plier"`, "sup" + fffd + "plier"},
		"chinese name":                  {`"供应商"`, "供应商"},
		"paired surrogate escape":       {`"😀"`, "😀"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			record := `{"batch":"B1","product":"P","quantity":1,"unit":"kg",` + tc.key + `:1}`

			regPath := filepath.Join(t.TempDir(), "registry.json")
			if err := os.WriteFile(regPath, []byte(`{"version":1,"batches":[`+record+`]}`), 0o644); err != nil {
				t.Fatal(err)
			}
			_, _, regErr := Load(regPath)
			var fe *FormatError
			if !errors.As(regErr, &fe) || fe.Field != tc.name {
				t.Fatalf("registry: expected unknown-field FormatError naming %q, got %v", tc.name, regErr)
			}
			if !strings.Contains(regErr.Error(), "unknown field") {
				t.Fatalf("registry: legal name must follow the unknown-field rule: %v", regErr)
			}

			_, manErr := ParseManifest([]byte(`[` + record + `]`))
			var re *ManifestRecordError
			if !errors.As(manErr, &re) || re.Batch != "B1" {
				t.Fatalf("manifest: expected ManifestRecordError citing B1, got %v", manErr)
			}
			if !strings.Contains(manErr.Error(), "unknown field") ||
				!strings.Contains(manErr.Error(), `"`+tc.name+`"`) {
				t.Fatalf("manifest: legal name must be reported decoded as unknown: %v", manErr)
			}
		})
	}
}

// TestNotAnObjectStaysDistinct pins the neighboring cause: a record that is
// not an object at all keeps its own wording and must not borrow the
// field-name-encoding phrasing.
func TestNotAnObjectStaysDistinct(t *testing.T) {
	regPath := filepath.Join(t.TempDir(), "registry.json")
	if err := os.WriteFile(regPath, []byte(`{"version":1,"batches":[42]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, regErr := Load(regPath)
	if regErr == nil || !strings.Contains(regErr.Error(), "must be a JSON object") ||
		strings.Contains(regErr.Error(), "field name") {
		t.Fatalf("not-an-object registry record keeps its own cause: %v", regErr)
	}

	_, manErr := ParseManifest([]byte(`[42]`))
	if manErr == nil || !strings.Contains(manErr.Error(), "must be a JSON object") ||
		strings.Contains(manErr.Error(), "field name") {
		t.Fatalf("not-an-object manifest record keeps its own cause: %v", manErr)
	}
}
