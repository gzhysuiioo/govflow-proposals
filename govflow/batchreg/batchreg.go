// Package batchreg registers supply-chain batches in a local JSON file.
//
// The registry file format is public and stable:
//
//	{
//	  "version": 1,
//	  "batches": [
//	    {"batch": "B-001", "product": "P-7", "quantity": 120, "unit": "kg"}
//	  ]
//	}
//
// Batch ids are unique per file (compared byte for byte, so casing and
// interior characters matter). Re-registering an existing id with the same
// product, quantity and unit is an idempotent duplicate confirmation; any
// differing field is rejected without touching stored records.
package batchreg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// FormatVersion is the only registry file format understood by this build.
const FormatVersion = 1

// MaxQuantity is the inclusive upper bound for a batch quantity.
const MaxQuantity = int64(1<<63 - 1)

var quantityPattern = regexp.MustCompile(`^[0-9]+$`)

// ErrInvalidUTF8 reports that a batch, product or unit value carries bytes
// that are not valid UTF-8 (or a JSON string escape that does not encode a
// Unicode scalar, such as a lone surrogate). Go's JSON decoder otherwise
// rewrites such input to the replacement character U+FFFD, which would merge
// distinct inputs into one value; the value is rejected instead. A genuine,
// deliberately typed U+FFFD is valid and never triggers this error. Call
// sites wrap it so the field name and record position stay attached.
var ErrInvalidUTF8 = errors.New("value is not valid UTF-8 text")

// invalidUTF8Field names the field whose encoding is invalid.
func invalidUTF8Field(field string) error {
	return fmt.Errorf("field %q is not valid UTF-8 text and must not be replaced with %q: %w", field, "�", ErrInvalidUTF8)
}

// jsonStringTokenValid reports whether raw is a syntactically complete JSON
// string literal whose decoded text is a valid Unicode string. It inspects
// the raw token only: malformed bytes fail as JSON, and a \u escape denoting
// a lone surrogate fails as invalid encoding, while a surrogate pair and the
// escape "�" stay valid. Raw invalid UTF-8 bytes fail as well.
func jsonStringTokenValid(raw []byte) bool {
	return jsonStringTokenError(raw) == nil
}

// jsonStringTokenError validates one JSON string token, returning
// errMalformedJSONString for malformed JSON and ErrInvalidUTF8 for a decoded
// string that is not valid Unicode. Callers treat anything other than
// ErrInvalidUTF8 as an ordinary "must be a JSON string" type error.
func jsonStringTokenError(raw []byte) error {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return jsonSyntaxError()
	}
	body := raw[1 : len(raw)-1]
	for i := 0; i < len(body); {
		b := body[i]
		if b == '"' {
			return jsonSyntaxError() // unescaped quote ends the token early
		}
		if b < 0x20 {
			return jsonSyntaxError() // control characters must be escaped
		}
		if b != '\\' {
			// A literal byte >= 0x80 starts a UTF-8 sequence; verify it
			// decodes to a rune (RuneError with width 1 means an invalid byte).
			if b >= 0x80 {
				r, size := utf8.DecodeRune(body[i:])
				if r == utf8.RuneError && size == 1 {
					return ErrInvalidUTF8
				}
				i += size
				continue
			}
			i++
			continue
		}
		if i+1 >= len(body) {
			return jsonSyntaxError()
		}
		switch body[i+1] {
		case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			i += 2
			continue
		case 'u':
			lo, end, ok := readHex4(body, i+2)
			if !ok {
				return jsonSyntaxError()
			}
			// A lone surrogate never denotes a Unicode scalar. A paired
			// surrogate (\uD800-\uDBFF immediately followed by a
			// \uDC00-\uDFFF) is consumed together and is valid.
			if lo >= 0xD800 && lo <= 0xDBFF {
				if lo2, _, ok2 := readFollowingUEScape(body, end); ok2 && lo2 >= 0xDC00 && lo2 <= 0xDFFF {
					i = end + 6 // skip both "\uXXXX" escapes
					continue
				}
				return ErrInvalidUTF8
			}
			if lo >= 0xDC00 && lo <= 0xDFFF {
				return ErrInvalidUTF8 // low surrogate with no leading high surrogate
			}
			i = end
			continue
		default:
			return jsonSyntaxError()
		}
	}
	return nil
}

// readHex4 reads four hex digits from body starting at start, returning the
// code unit value and the index just past the fourth digit.
func readHex4(body []byte, start int) (value, end int, ok bool) {
	if start+4 > len(body) {
		return 0, 0, false
	}
	var v int
	for k := 0; k < 4; k++ {
		c := body[start+k]
		var d int
		switch {
		case c >= '0' && c <= '9':
			d = int(c - '0')
		case c >= 'a' && c <= 'f':
			d = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = int(c-'A') + 10
		default:
			return 0, 0, false
		}
		v = v<<4 | d
	}
	return v, start + 4, true
}

// readFollowingUEScape checks that body at index at begins with a "\uXXXX"
// escape and returns its code unit along with the index just past the four
// hex digits.
func readFollowingUEScape(body []byte, at int) (value, end int, ok bool) {
	if at+6 > len(body) || body[at] != '\\' || body[at+1] != 'u' {
		return 0, 0, false
	}
	return readHex4(body, at+2)
}

// errMalformedJSONString marks a string token that is not well-formed JSON;
// callers report it the same way as a wrong-typed field.
var errMalformedJSONString = errors.New("invalid JSON string literal")

// jsonSyntaxError builds the malformed-token error.
func jsonSyntaxError() error {
	return errMalformedJSONString
}

// Batch is the four-piece record of one supply-chain batch.
type Batch struct {
	Batch    string `json:"batch"`
	Product  string `json:"product"`
	Quantity int64  `json:"quantity"`
	Unit     string `json:"unit"`
}

// Registry is the on-disk registry file.
type Registry struct {
	Version int     `json:"version"`
	Batches []Batch `json:"batches"`
}

// Input is a normalized registration request.
type Input struct {
	Batch    string
	Product  string
	Quantity int64
	Unit     string
}

// Outcome reports what Register did.
type Outcome struct {
	Batch   Batch
	Created bool // true when a record was added, false for an identical repeat
}

// ConflictError reports that a batch id is already held by a different record.
// Fields lists every differing member among "product", "quantity" and "unit".
type ConflictError struct {
	Batch  string
	Fields []string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("batch %q is already registered with conflicting field(s): %s; the existing record cannot be overwritten",
		e.Batch, strings.Join(e.Fields, ", "))
}

// DuplicateIDError reports two stored records sharing one batch id.
// First and Second are the 1-based positions of the two records in the
// batches array when known.
type DuplicateIDError struct {
	Batch  string
	First  int
	Second int
}

func (e *DuplicateIDError) Error() string {
	if e.First > 0 && e.Second > 0 {
		return fmt.Sprintf("registry contains multiple records for batch %q (records %d and %d)", e.Batch, e.First, e.Second)
	}
	return fmt.Sprintf("registry contains multiple records for batch %q", e.Batch)
}

// FormatError reports a structural violation of the public registry file
// format: a missing, duplicated, misspelled, null or mistyped field, either
// in the root object (Position == 0) or in one batch record (Position is its
// 1-based index in "batches"). Batch carries the record's batch id only when
// it is uniquely determined; it stays empty when the "batch" member itself
// is missing, duplicated or not a string.
type FormatError struct {
	Position int
	Batch    string
	Field    string
	Reason   string
}

func (e *FormatError) Error() string {
	where := "root object"
	if e.Position > 0 {
		where = fmt.Sprintf("batches record %d", e.Position)
		if e.Batch != "" {
			where += fmt.Sprintf(" (batch %q)", e.Batch)
		}
	}
	if e.Field != "" {
		return fmt.Sprintf("%s: field %q: %s", where, e.Field, e.Reason)
	}
	return fmt.Sprintf("%s: %s", where, e.Reason)
}

// NormalizeField trims leading and trailing whitespace; the result must stay
// non-empty. Interior characters, including interior whitespace, are kept,
// and casing is preserved so "B1" and "b1" are different values. The value
// must be valid UTF-8 before trimming: invalid bytes are rejected rather than
// replaced, including when they sit next to leading or trailing whitespace.
// File parsers additionally check the raw token with jsonStringTokenError so
// they can reject lone surrogate escapes before decoding.
func NormalizeField(value string) (string, error) {
	if !utf8.ValidString(value) {
		return "", fmt.Errorf("value is not valid UTF-8 text: %w", ErrInvalidUTF8)
	}
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", errors.New("value must not be empty after trimming whitespace")
	}
	return trimmed, nil
}

// ParseQuantity accepts a decimal positive integer made only of ASCII digits
// 0-9. Leading zeros are allowed; the numeric value is what gets stored and
// compared. Zero, signs, fractions, non-ASCII digits and values above the
// signed 64-bit maximum are rejected.
func ParseQuantity(text string) (int64, error) {
	if !quantityPattern.MatchString(text) {
		return 0, fmt.Errorf("quantity %q must be a positive decimal integer made only of digits 0-9", text)
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("quantity %q must be a positive integer no greater than %d", text, MaxQuantity)
	}
	if value == 0 {
		return 0, fmt.Errorf("quantity %q must be greater than zero", text)
	}
	return value, nil
}

// Load reads the registry at path. A missing file yields an empty registry
// with existed == false. An existing file that is empty, not parseable as
// the public format — including missing, null, duplicated, misspelled or
// mistyped fields — of an unsupported version, or holding duplicate batch
// ids is an error: callers must never overwrite such a file as if it were a
// fresh registry.
func Load(path string) (reg *Registry, existed bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &Registry{Version: FormatVersion}, false, nil
		}
		return nil, false, fmt.Errorf("cannot read registry %q: %w", path, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, true, fmt.Errorf("registry file %q is empty; refusing to replace it with a new registry", path)
	}
	reg, err = decode(data)
	if err != nil {
		return nil, true, fmt.Errorf("cannot parse registry %q as version %d JSON: %w", path, FormatVersion, err)
	}
	if err := validate(reg); err != nil {
		return nil, true, fmt.Errorf("registry %q is not usable: %w", path, err)
	}
	return reg, true, nil
}

// decode parses data as the public registry format. The root must be a JSON
// object holding exactly "version" (the integer 1) and "batches" (an array,
// possibly empty); every record must be an object holding exactly "batch",
// "product", "quantity" and "unit". Field names are matched case-sensitively
// after JSON string decoding, so "Version" or "Batch" are unknown fields and
// an escaped respelling such as "bat\u0063h" counts as the same field. A
// field appearing twice in one object is rejected even when both values are
// identical; missing fields, nulls, wrong types and unknown fields are
// rejected just the same. Nothing is patched up by taking the later value,
// merging or defaulting.
func decode(data []byte) (*Registry, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	var root json.RawMessage
	if err := dec.Decode(&root); err != nil {
		return nil, err
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("unexpected data after registry object")
		}
		return nil, err
	}

	fields, err := parseObjectFields(root)
	if err != nil {
		return nil, &FormatError{Reason: "root must be a JSON object holding exactly \"version\" and \"batches\""}
	}
	if dup, ok := duplicateField(fields); ok {
		return nil, &FormatError{Field: dup, Reason: "field appears more than once; duplicate fields are not allowed"}
	}
	for _, f := range fields {
		if f.key != "version" && f.key != "batches" {
			return nil, &FormatError{Field: f.key, Reason: "unknown field; only \"version\" and \"batches\" are allowed"}
		}
	}
	versionRaw, ok := findField(fields, "version")
	if !ok {
		return nil, &FormatError{Field: "version", Reason: "required field is missing"}
	}
	batchesRaw, ok := findField(fields, "batches")
	if !ok {
		return nil, &FormatError{Field: "batches", Reason: "required field is missing"}
	}
	version, err := parseRegistryVersion(versionRaw)
	if err != nil {
		return nil, err
	}
	if trimmed := bytes.TrimSpace(batchesRaw); len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, &FormatError{Field: "batches", Reason: "must be an array of batch records"}
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(batchesRaw, &elems); err != nil {
		return nil, &FormatError{Field: "batches", Reason: "must be an array of batch records"}
	}
	reg := &Registry{Version: version, Batches: make([]Batch, 0, len(elems))}
	for i, raw := range elems {
		b, err := parseRegistryRecord(raw, i+1)
		if err != nil {
			return nil, err
		}
		reg.Batches = append(reg.Batches, b)
	}
	return reg, nil
}

// parseRegistryVersion accepts exactly the integer literal of FormatVersion.
// Other integers report an unsupported version; anything else (strings,
// fractions, exponents, booleans, null, arrays, objects) is a type error.
func parseRegistryVersion(raw json.RawMessage) (int, error) {
	token := string(bytes.TrimSpace(raw))
	if isJSONInteger(token) {
		if token == strconv.Itoa(FormatVersion) {
			return FormatVersion, nil
		}
		return 0, fmt.Errorf("unsupported registry version: got %s, want %d", token, FormatVersion)
	}
	return 0, &FormatError{Field: "version", Reason: fmt.Sprintf("must be the integer %d, not %s", FormatVersion, token)}
}

// parseRegistryRecord validates one batches element: a JSON object with
// exactly the four required lowercase fields, text fields as JSON strings
// and quantity as a strict JSON integer in 1..MaxQuantity. pos is the
// record's 1-based position in the batches array.
func parseRegistryRecord(raw json.RawMessage, pos int) (Batch, error) {
	var b Batch
	fields, err := parseObjectFields(raw)
	if err != nil {
		return b, &FormatError{Position: pos, Reason: "record must be a JSON object holding exactly \"batch\", \"product\", \"quantity\" and \"unit\""}
	}
	// The batch id is attached to errors only when it is unambiguous:
	// exactly one "batch" member carrying a JSON string whose decoded text is
	// valid UTF-8. When the "batch" member is duplicated or its encoding is
	// invalid, no id is picked or reconstructed from replacement characters.
	batchID := ""
	if countField(fields, "batch") == 1 {
		if rawID, _ := findField(fields, "batch"); rawID != nil {
			trimmedID := bytes.TrimSpace(rawID)
			if len(trimmedID) > 0 && trimmedID[0] == '"' && jsonStringTokenValid(trimmedID) {
				var id string
				if json.Unmarshal(rawID, &id) == nil {
					batchID = id
				}
			}
		}
	}
	fail := func(field, reason string) (Batch, error) {
		return Batch{}, &FormatError{Position: pos, Batch: batchID, Field: field, Reason: reason}
	}

	if dup, ok := duplicateField(fields); ok {
		return fail(dup, "field appears more than once; duplicate fields are not allowed")
	}
	for _, f := range fields {
		if _, ok := batchRecordFields[f.key]; !ok {
			return fail(f.key, "unknown field; only \"batch\", \"product\", \"quantity\" and \"unit\" are allowed")
		}
	}

	text := func(name string) (string, error) {
		rawValue, ok := findField(fields, name)
		if !ok {
			return "", &FormatError{Position: pos, Batch: batchID, Field: name, Reason: "required field is missing"}
		}
		trimmed := bytes.TrimSpace(rawValue)
		if len(trimmed) == 0 || trimmed[0] != '"' {
			return "", &FormatError{Position: pos, Batch: batchID, Field: name, Reason: "must be a JSON string"}
		}
		// Validate the raw token before decoding so invalid bytes are never
		// silently turned into U+FFFD.
		if err := jsonStringTokenError(trimmed); err != nil {
			if errors.Is(err, ErrInvalidUTF8) {
				return "", &FormatError{Position: pos, Batch: batchID, Field: name,
					Reason: "value is not valid UTF-8 text; the invalid bytes must not be replaced with \"�\""}
			}
			return "", &FormatError{Position: pos, Batch: batchID, Field: name, Reason: "must be a JSON string"}
		}
		var s string
		if err := json.Unmarshal(rawValue, &s); err != nil {
			return "", &FormatError{Position: pos, Batch: batchID, Field: name, Reason: "must be a JSON string"}
		}
		return s, nil
	}
	if b.Batch, err = text("batch"); err != nil {
		return Batch{}, err
	}
	if b.Product, err = text("product"); err != nil {
		return Batch{}, err
	}
	if b.Unit, err = text("unit"); err != nil {
		return Batch{}, err
	}
	quantityRaw, ok := findField(fields, "quantity")
	if !ok {
		return fail("quantity", "required field is missing")
	}
	b.Quantity, err = parseRegistryQuantity(bytes.TrimSpace(quantityRaw))
	if err != nil {
		return fail("quantity", err.Error())
	}
	return b, nil
}

// parseRegistryQuantity accepts exactly a strict JSON integer token in the
// range 1..MaxQuantity. Strings, signs, fractions, exponents, null and
// out-of-range values are rejected.
func parseRegistryQuantity(token []byte) (int64, error) {
	if !isJSONInteger(string(token)) || len(token) == 0 || token[0] == '-' {
		return 0, fmt.Errorf("must be a JSON integer between 1 and %d", MaxQuantity)
	}
	value, err := strconv.ParseInt(string(token), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("must be an integer no greater than %d", MaxQuantity)
	}
	if value == 0 {
		return 0, errors.New("must be greater than zero")
	}
	return value, nil
}

// isJSONInteger reports whether token is a JSON number literal without
// fraction or exponent. The token comes from validated JSON, so leading
// zeros cannot hide extra digits.
func isJSONInteger(token string) bool {
	if token == "" {
		return false
	}
	if token[0] == '-' {
		token = token[1:]
	}
	if token == "" {
		return false
	}
	for i := 0; i < len(token); i++ {
		if token[i] < '0' || token[i] > '9' {
			return false
		}
	}
	return true
}

// objectField is one member of a JSON object in source order; the key is
// the decoded JSON string, so escaped spellings compare by meaning.
type objectField struct {
	key   string
	value json.RawMessage
}

// parseObjectFields decodes a single JSON object value into its members in
// source order, keeping duplicates visible for the caller to reject.
func parseObjectFields(raw []byte) ([]objectField, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, errors.New("value is not a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	if _, err := dec.Token(); err != nil { // opening brace
		return nil, err
	}
	var fields []objectField
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := tok.(string)
		if !ok {
			return nil, errors.New("object keys must be JSON strings")
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		fields = append(fields, objectField{key: key, value: value})
	}
	if _, err := dec.Token(); err != nil { // closing brace
		return nil, err
	}
	return fields, nil
}

// duplicateField returns the first field name appearing more than once.
func duplicateField(fields []objectField) (string, bool) {
	seen := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		if _, dup := seen[f.key]; dup {
			return f.key, true
		}
		seen[f.key] = struct{}{}
	}
	return "", false
}

// findField returns the raw value of the first member named name.
func findField(fields []objectField, name string) (json.RawMessage, bool) {
	for _, f := range fields {
		if f.key == name {
			return f.value, true
		}
	}
	return nil, false
}

// countField counts the members named name.
func countField(fields []objectField, name string) int {
	n := 0
	for _, f := range fields {
		if f.key == name {
			n++
		}
	}
	return n
}

func validate(reg *Registry) error {
	seen := make(map[string]int, len(reg.Batches))
	for i, b := range reg.Batches {
		pos := i + 1
		if b.Batch == "" || b.Product == "" || b.Unit == "" {
			return fmt.Errorf("batches record %d (batch %q): batch, product and unit must all be non-empty", pos, b.Batch)
		}
		if b.Quantity <= 0 {
			return fmt.Errorf("batches record %d (batch %q): quantity must be a positive integer", pos, b.Batch)
		}
		if first, dup := seen[b.Batch]; dup {
			return &DuplicateIDError{Batch: b.Batch, First: first, Second: pos}
		}
		seen[b.Batch] = pos
	}
	return nil
}

// ManifestRecordError reports a manifest record that is structurally invalid
// or carries a quantity outside the accepted range. Position is the record's
// 1-based position in the manifest.
type ManifestRecordError struct {
	Position int
	Batch    string // normalized batch id when available, otherwise ""
	Reason   string
}

func (e *ManifestRecordError) Error() string {
	if e.Batch != "" {
		return fmt.Sprintf("manifest record %d (batch %q): %s", e.Position, e.Batch, e.Reason)
	}
	return fmt.Sprintf("manifest record %d: %s", e.Position, e.Reason)
}

// ManifestConflictError reports that a manifest record disagrees with the
// record already registered, or introduced earlier in the same manifest,
// under the same batch id. Position is the record's 1-based position in the
// manifest and Fields lists every differing member among "product",
// "quantity" and "unit" in that order.
type ManifestConflictError struct {
	Position int
	Batch    string
	Fields   []string
	Source   string // "registry" or "manifest"
	PrevPos  int    // 1-based position of the earlier manifest record, if Source == "manifest"
}

func (e *ManifestConflictError) Error() string {
	if e.Source == "manifest" {
		return fmt.Sprintf("manifest record %d (batch %q) conflicts with manifest record %d on field(s): %s; the whole manifest is rejected",
			e.Position, e.Batch, e.PrevPos, strings.Join(e.Fields, ", "))
	}
	return fmt.Sprintf("manifest record %d (batch %q) conflicts with the registered record on field(s): %s; the whole manifest is rejected",
		e.Position, e.Batch, strings.Join(e.Fields, ", "))
}

// ParseManifest decodes data as a non-empty JSON array holding exactly the
// four required fields batch, product, quantity and unit per element. The
// three text fields must be JSON strings that are not blank after trimming;
// quantity must be a JSON integer (no strings, fractions or exponents) in
// the range 1..MaxQuantity. An empty file, empty array, non-array document
// or any malformed record is rejected. The manifest bytes are never written.
func ParseManifest(data []byte) ([]Input, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, errors.New("manifest file is empty")
	}
	if trimmed[0] != '[' {
		return nil, errors.New("manifest must be a non-empty JSON array of records")
	}

	var elems []json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&elems); err != nil {
		return nil, fmt.Errorf("manifest is not valid JSON: %w", err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("manifest has unexpected data after the array")
		}
		return nil, fmt.Errorf("manifest must be a single JSON array: %w", err)
	}
	if len(elems) == 0 {
		return nil, errors.New("manifest must contain at least one record")
	}

	inputs := make([]Input, 0, len(elems))
	for i, raw := range elems {
		pos := i + 1
		in, err := parseManifestRecord(raw)
		if err != nil {
			return nil, &ManifestRecordError{Position: pos, Batch: in.Batch, Reason: err.Error()}
		}
		inputs = append(inputs, in)
	}
	return inputs, nil
}

// batchRecordFields are the only members a batch record may carry, in the
// registry file and in an import manifest alike.
var batchRecordFields = map[string]struct{}{
	"batch": {}, "product": {}, "quantity": {}, "unit": {},
}

// parseManifestRecord validates one manifest element: it must be a JSON
// object with exactly the four required keys, text fields as non-blank
// strings and quantity as a strict JSON integer.
func parseManifestRecord(raw json.RawMessage) (Input, error) {
	var in Input
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return in, errors.New("record must be a JSON object with batch, product, quantity and unit")
	}
	fields := make(map[string]json.RawMessage)
	if err := json.Unmarshal(raw, &fields); err != nil {
		return in, fmt.Errorf("record is not valid JSON: %w", err)
	}
	for name := range fields {
		if _, ok := batchRecordFields[name]; !ok {
			return in, fmt.Errorf("record has unknown field %q; only batch, product, quantity and unit are allowed", name)
		}
	}
	keys, err := objectKeys(raw)
	if err != nil {
		return in, err
	}
	counts := make(map[string]int, len(keys))
	for _, k := range keys {
		counts[k]++
		if counts[k] > 1 {
			return in, fmt.Errorf("record lists field %q more than once", k)
		}
	}

	textField := func(name string) (string, error) {
		value, ok := fields[name]
		if !ok {
			return "", fmt.Errorf("missing required field %q", name)
		}
		trimmed := bytes.TrimSpace(value)
		if len(trimmed) == 0 || trimmed[0] != '"' {
			return "", fmt.Errorf("field %q must be a JSON string", name)
		}
		// Validate the raw token before decoding so invalid bytes are never
		// silently turned into U+FFFD. Returning early on an invalid "batch"
		// also keeps the replacement value out of the error's batch id.
		if err := jsonStringTokenError(trimmed); err != nil {
			if errors.Is(err, ErrInvalidUTF8) {
				return "", fmt.Errorf("field %q is not valid UTF-8 text; the invalid bytes must not be replaced with %q", name, "�")
			}
			return "", fmt.Errorf("field %q must be a JSON string", name)
		}
		var text string
		if err := json.Unmarshal(value, &text); err != nil {
			return "", fmt.Errorf("field %q must be a JSON string", name)
		}
		normalized, err := NormalizeField(text)
		if err != nil {
			return "", fmt.Errorf("field %q must not be blank: %w", name, err)
		}
		return normalized, nil
	}

	in.Batch, err = textField("batch")
	if err != nil {
		return in, err
	}
	in.Product, err = textField("product")
	if err != nil {
		return in, err
	}
	in.Unit, err = textField("unit")
	if err != nil {
		return in, err
	}
	quantityRaw, ok := fields["quantity"]
	if !ok {
		return in, errors.New("missing required field \"quantity\"")
	}
	in.Quantity, err = parseManifestQuantity(bytes.TrimSpace(quantityRaw))
	if err != nil {
		return in, err
	}
	return in, nil
}

// objectKeys returns the member keys of a JSON object in source order,
// flagging repeated names for the caller.
func objectKeys(raw json.RawMessage) ([]string, error) {
	fields, err := parseObjectFields(raw)
	if err != nil {
		return nil, err
	}
	keys := make([]string, len(fields))
	for i, f := range fields {
		keys[i] = f.key
	}
	return keys, nil
}

// parseManifestQuantity accepts exactly a strict JSON integer token in the
// range 1..MaxQuantity. Strings, signs, fractions, exponents, leading zeros
// and out-of-range values are rejected instead of silently rounded.
func parseManifestQuantity(token []byte) (int64, error) {
	if len(token) == 0 || token[0] < '0' || token[0] > '9' {
		return 0, fmt.Errorf("field %q must be a JSON integer between 1 and %d", "quantity", MaxQuantity)
	}
	for _, c := range token {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("field %q must be a JSON integer without sign, fraction or exponent", "quantity")
		}
	}
	if len(token) > 1 && token[0] == '0' {
		return 0, errors.New("field \"quantity\" must be a JSON integer without leading zeros")
	}
	value, err := strconv.ParseInt(string(token), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("field %q must be an integer no greater than %d", "quantity", MaxQuantity)
	}
	if value == 0 {
		return 0, errors.New("field \"quantity\" must be greater than zero")
	}
	return value, nil
}

// ImportResult is one entry of an Import outcome: the normalized record in
// manifest order plus whether it was newly created or already present.
type ImportResult struct {
	Batch   Batch
	Created bool
}

// Import validates every record of inputs before appending anything: the
// whole manifest fails if any record conflicts with a stored record or with
// an earlier manifest record, and reg is left untouched on error. New batch
// ids are appended in first-occurrence order; an id already stored or
// introduced earlier in the same manifest is confirmed as a duplicate only
// when product, quantity and unit all match. Results come back in manifest
// order; Created is false for duplicates.
func Import(reg *Registry, inputs []Input) (results []ImportResult, err error) {
	if reg.Version != FormatVersion {
		return nil, fmt.Errorf("unsupported registry version %d", reg.Version)
	}
	if len(inputs) == 0 {
		return nil, errors.New("manifest must contain at least one record")
	}

	// Work on a copy: a rejected manifest must never partially land in reg.
	working := append([]Batch(nil), reg.Batches...)
	indexByID := make(map[string]int, len(working))
	for i, b := range working {
		indexByID[b.Batch] = i
	}
	firstPos := make(map[string]int, len(inputs)) // manifest 1-based position of a newly introduced id

	out := make([]ImportResult, len(inputs))
	for i, in := range inputs {
		pos := i + 1
		// Defense in depth for inputs built directly rather than via
		// ParseManifest: invalid text is rejected before anything is touched.
		for _, f := range []struct{ name, value string }{
			{"batch", in.Batch}, {"product", in.Product}, {"unit", in.Unit},
		} {
			if !utf8.ValidString(f.value) {
				batch := in.Batch
				if f.name == "batch" || !utf8.ValidString(batch) {
					batch = ""
				}
				return nil, &ManifestRecordError{Position: pos, Batch: batch,
					Reason: fmt.Sprintf("field %q is not valid UTF-8 text; the invalid bytes must not be replaced with %q", f.name, "�")}
			}
		}
		if idx, known := indexByID[in.Batch]; known {
			existing := working[idx]
			diffs := diffFields(existing, in)
			if len(diffs) > 0 {
				if prev, inManifest := firstPos[in.Batch]; inManifest {
					return nil, &ManifestConflictError{Position: pos, Batch: in.Batch, Fields: diffs, Source: "manifest", PrevPos: prev}
				}
				return nil, &ManifestConflictError{Position: pos, Batch: in.Batch, Fields: diffs, Source: "registry"}
			}
			out[i] = ImportResult{Batch: existing, Created: false}
			continue
		}
		b := Batch{Batch: in.Batch, Product: in.Product, Quantity: in.Quantity, Unit: in.Unit}
		working = append(working, b)
		indexByID[in.Batch] = len(working) - 1
		firstPos[in.Batch] = pos
		out[i] = ImportResult{Batch: b, Created: true}
	}

	reg.Batches = working
	return out, nil
}

// diffFields lists the members of product, quantity and unit in which in
// differs from existing, in that fixed order.
func diffFields(existing Batch, in Input) []string {
	var diffs []string
	if existing.Product != in.Product {
		diffs = append(diffs, "product")
	}
	if existing.Quantity != in.Quantity {
		diffs = append(diffs, "quantity")
	}
	if existing.Unit != in.Unit {
		diffs = append(diffs, "unit")
	}
	return diffs
}

// validateTextInputs rejects a request whose batch, product or unit carries
// invalid UTF-8. The CLI reaches Register and Import only through
// NormalizeField, but a direct library caller must be stopped too: such a
// string would otherwise be re-encoded as U+FFFD on save and merge with an
// unrelated value.
func validateTextInputs(in Input) error {
	for _, f := range []struct{ name, value string }{
		{"batch", in.Batch}, {"product", in.Product}, {"unit", in.Unit},
	} {
		if !utf8.ValidString(f.value) {
			return invalidUTF8Field(f.name)
		}
	}
	return nil
}

// Register adds in to reg, or confirms the identical record already stored
// under the same batch id. A batch id held by a record differing in product,
// quantity or unit produces a *ConflictError and leaves reg untouched. The
// zero Outcome is returned together with the error.
func Register(reg *Registry, in Input) (Outcome, error) {
	if reg.Version != FormatVersion {
		return Outcome{}, fmt.Errorf("unsupported registry version %d", reg.Version)
	}
	if err := validateTextInputs(in); err != nil {
		return Outcome{}, err
	}
	if in.Batch == "" || in.Product == "" || in.Unit == "" {
		return Outcome{}, errors.New("batch, product and unit must be non-empty")
	}
	if in.Quantity <= 0 || in.Quantity > MaxQuantity {
		return Outcome{}, fmt.Errorf("quantity must be a positive integer no greater than %d", MaxQuantity)
	}
	for _, b := range reg.Batches {
		if b.Batch != in.Batch {
			continue
		}
		var diffs []string
		if b.Product != in.Product {
			diffs = append(diffs, "product")
		}
		if b.Quantity != in.Quantity {
			diffs = append(diffs, "quantity")
		}
		if b.Unit != in.Unit {
			diffs = append(diffs, "unit")
		}
		if len(diffs) > 0 {
			return Outcome{}, &ConflictError{Batch: in.Batch, Fields: diffs}
		}
		return Outcome{Batch: b, Created: false}, nil
	}
	b := Batch{Batch: in.Batch, Product: in.Product, Quantity: in.Quantity, Unit: in.Unit}
	reg.Batches = append(reg.Batches, b)
	return Outcome{Batch: b, Created: true}, nil
}

// Save atomically writes reg to path, replacing the file only after the new
// content is fully on disk so an existing registry stays usable on failure.
// Records are serialized in their current order; the file is created with
// 0644 permissions or, when replacing an existing file, with its permissions.
func Save(path string, reg *Registry) error {
	if reg.Version != FormatVersion {
		return fmt.Errorf("cannot write registry format version %d", reg.Version)
	}
	// Final backstop: a record with invalid text must never be serialized,
	// since encoding/json would rewrite the bytes to U+FFFD on disk.
	for i, b := range reg.Batches {
		for _, f := range []struct{ name, value string }{
			{"batch", b.Batch}, {"product", b.Product}, {"unit", b.Unit},
		} {
			if !utf8.ValidString(f.value) {
				return fmt.Errorf("cannot save registry %q: batches record %d field %q is not valid UTF-8 text", path, i+1, f.name)
			}
		}
	}
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("cannot inspect registry %q: %w", path, err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("cannot save registry %q: %w", path, err)
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(reg); err != nil {
		return fmt.Errorf("cannot encode registry %q: %w", path, err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".govflow-registry-*.tmp")
	if err != nil {
		return fmt.Errorf("cannot save registry %q: %w", path, err)
	}
	tmpName := tmp.Name()
	abort := func(cause error) error {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("cannot save registry %q: %w", path, cause)
	}
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		return abort(err)
	}
	if err := tmp.Chmod(mode); err != nil {
		return abort(err)
	}
	if err := tmp.Sync(); err != nil {
		return abort(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("cannot save registry %q: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("cannot save registry %q: %w", path, err)
	}
	return nil
}
