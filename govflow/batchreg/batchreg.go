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
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// FormatVersion is the only registry file format understood by this build.
const FormatVersion = 1

// MaxQuantity is the inclusive upper bound for a batch quantity.
const MaxQuantity = int64(1<<63 - 1)

var quantityPattern = regexp.MustCompile(`^[0-9]+$`)

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
// format: a missing, duplicated, misspelled, null or mistyped field, or a
// field NAME whose bytes are not valid UTF-8 (or hold a lone surrogate
// escape) — either in the root object (Position == 0) or in one batch
// record (Position is its 1-based index in "batches"). Batch carries the
// record's batch id only when it is uniquely determined; it stays empty when
// the "batch" member itself is missing, duplicated or not a string. A bad
// field name is reported with an empty Field: the undecodable name is never
// quoted back, since decoding it would substitute U+FFFD.
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

// EncodingError reports that a text field is not valid UTF-8: either the
// bytes of a CLI argument, or a JSON string value whose source bytes hold
// malformed UTF-8 or a lone surrogate escape. The offending value is never
// quoted back, since decoding it would substitute U+FFFD and make distinct
// inputs collide. Field is one of "batch", "product" and "unit". Inside a
// manifest, ParseManifest wraps this in a ManifestRecordError carrying the
// 1-based record position.
type EncodingError struct {
	Field string
}

func (e *EncodingError) Error() string {
	return fmt.Sprintf("field %q contains bytes that are not valid UTF-8; the value is rejected instead of being replaced with U+FFFD", e.Field)
}

// NormalizeField trims leading and trailing whitespace; the result must stay
// non-empty and be valid UTF-8. Interior characters, including interior
// whitespace, are kept, and casing is preserved so "B1" and "b1" are
// different values. A genuine replacement character (U+FFFD) is ordinary
// text; only malformed UTF-8 bytes are refused.
func NormalizeField(value string) (string, error) {
	if !utf8.ValidString(value) {
		return "", errors.New("value is not valid UTF-8 text")
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

var (
	errStringNotJSON  = errors.New("value is not a JSON string")
	errStringEncoding = errors.New("JSON string contains bytes that are not valid UTF-8")
)

// unmarshalStringStrict decodes one JSON string literal into a Go string
// without encoding/json's silent U+FFFD substitution. The literal must be
// syntactically valid JSON (only the single token, no trailing data) and the
// decoded text must be valid Unicode: malformed UTF-8 source bytes and lone
// surrogate escapes are refused, while a genuine U+FFFD — written directly
// or as the escape "�" — is ordinary text. Syntax problems return
// errStringNotJSON; encoding problems return errStringEncoding.
func unmarshalStringStrict(raw json.RawMessage) (string, error) {
	token := trimJSONSpace(raw)
	if len(token) < 2 || token[0] != '"' || token[len(token)-1] != '"' {
		return "", errStringNotJSON
	}
	var b strings.Builder
	b.Grow(len(token))
	i := 1
	for i < len(token)-1 {
		c := token[i]
		switch {
		case c == '"':
			// An unescaped quote before the final byte ends the literal
			// early, so anything following it is trailing data.
			return "", errStringNotJSON
		case c == '\\':
			if i+1 >= len(token)-1 {
				return "", errStringNotJSON
			}
			switch token[i+1] {
			case '"', '\\', '/':
				b.WriteByte(token[i+1])
				i += 2
			case 'b':
				b.WriteByte('\b')
				i += 2
			case 'f':
				b.WriteByte('\f')
				i += 2
			case 'n':
				b.WriteByte('\n')
				i += 2
			case 'r':
				b.WriteByte('\r')
				i += 2
			case 't':
				b.WriteByte('\t')
				i += 2
			case 'u':
				r, size, err := decodeUnicodeEscape(token[i:])
				if err != nil {
					return "", err
				}
				if _, err := b.WriteRune(r); err != nil {
					return "", errStringEncoding
				}
				i += size
			default:
				return "", errStringNotJSON
			}
		case c < 0x20:
			// Raw control characters must be escaped in JSON.
			return "", errStringNotJSON
		default:
			r, size := utf8.DecodeRune(token[i : len(token)-1])
			if r == utf8.RuneError && size == 1 {
				return "", errStringEncoding
			}
			b.WriteRune(r)
			i += size
		}
	}
	if i != len(token)-1 {
		return "", errStringNotJSON
	}
	return b.String(), nil
}

// decodeUnicodeEscape reads a "\uXXXX" escape (or a UTF-16 surrogate pair
// "\uHHHH\uLLLL") at the start of p, returning the rune and the number of
// bytes consumed. A high surrogate not followed by a low surrogate, a low
// surrogate with no high one, or a non-hex code unit are encoding errors.
func decodeUnicodeEscape(p []byte) (rune, int, error) {
	hi, ok := hex4(p)
	if !ok {
		return 0, 0, errStringNotJSON
	}
	switch {
	case hi >= 0xD800 && hi <= 0xDBFF:
		if len(p) < 12 || p[6] != '\\' || p[7] != 'u' {
			return 0, 0, errStringEncoding
		}
		lo, ok := hex4(p[6:])
		if !ok {
			return 0, 0, errStringNotJSON
		}
		if lo < 0xDC00 || lo > 0xDFFF {
			return 0, 0, errStringEncoding
		}
		return 0x10000 + (rune(hi)-0xD800)<<10 + (rune(lo) - 0xDC00), 12, nil
	case hi >= 0xDC00 && hi <= 0xDFFF:
		return 0, 0, errStringEncoding
	default:
		return rune(hi), 6, nil
	}
}

// hex4 reads the four hex digits following a leading "\u" (p[:2] == `\u`).
func hex4(p []byte) (int, bool) {
	if len(p) < 6 || p[0] != '\\' || p[1] != 'u' {
		return 0, false
	}
	v := 0
	for _, c := range p[2:6] {
		var d byte
		switch {
		case c >= '0' && c <= '9':
			d = c - '0'
		case c >= 'a' && c <= 'f':
			d = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			d = c - 'A' + 10
		default:
			return 0, false
		}
		v = v<<4 | int(d)
	}
	return v, true
}

// Load reads the registry at path. A missing file yields an empty registry
// with existed == false. An existing file that is empty, not parseable as
// the public format — including missing, null, duplicated, misspelled or
// mistyped fields — of an unsupported version, or holding duplicate batch
// ids is an error: callers must never overwrite such a file as if it were a
// fresh registry.
//
// A symbolic link is followed to its target, so reading through a link sees
// exactly the records Save writes to the linked file. A link whose target
// does not exist, or a chain of links forming a loop, is an error naming
// path and the reason — never a fresh-registry signal: treating it as a
// first registration would either replace the link with a regular file or
// create the target the user never asked for.
func Load(path string) (reg *Registry, existed bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// A dangling symbolic link also reports "not exist", but the path
			// is occupied: the caller must not treat it as a first
			// registration.
			if info, lerr := os.Lstat(path); lerr == nil && info.Mode()&os.ModeSymlink != 0 {
				return nil, true, fmt.Errorf("cannot read registry %q: symbolic link target does not exist; refusing to treat the link as a new registry", path)
			}
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
// field NAME whose source bytes are malformed UTF-8, or that hides a lone
// surrogate escape, is its own rejection cause — reported as an invalid
// field name, never as "must be an object" and never as an unknown field
// under the U+FFFD-substituted text. A field appearing twice in one object
// is rejected even when both values are
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

	fields, badKey, err := parseObjectFields(root)
	if err != nil {
		return nil, &FormatError{Reason: "root must be a JSON object holding exactly \"version\" and \"batches\""}
	}
	if badKey {
		return nil, &FormatError{Reason: keyEncodingReason}
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
//
// The structural rules (member names, strict string decoding, quantity
// range) are shared with parseManifestRecord; only this source's reporting
// differs: problems are surfaced as *FormatError with registry wording, a
// duplicated member anywhere in the object outranks an unknown one, and
// text values are read verbatim — validate handles the empty-string case.
// A member name that cannot be decoded is reported as an invalid field name
// (keyEncodingReason), ahead of the duplicate/unknown scan, with the batch
// id attached under the same unambiguity rule as every other record error.
func parseRegistryRecord(raw json.RawMessage, pos int) (Batch, error) {
	var b Batch
	fields, badKey, err := parseObjectFields(raw)
	if err != nil {
		return b, &FormatError{Position: pos, Reason: "record must be a JSON object holding exactly \"batch\", \"product\", \"quantity\" and \"unit\""}
	}
	// The batch id is attached to errors only when it is unambiguous:
	// exactly one "batch" member carrying a JSON string whose decoded text
	// is valid UTF-8. When the "batch" member itself is duplicated, or its
	// bytes are not valid UTF-8, no id (and never a U+FFFD replacement) is
	// picked arbitrarily.
	batchID := unambiguousBatchID(fields, textVerbatim)
	if badKey {
		return Batch{}, &FormatError{Position: pos, Batch: batchID, Reason: keyEncodingReason}
	}
	fail := func(field, reason string) (Batch, error) {
		return Batch{}, &FormatError{Position: pos, Batch: batchID, Field: field, Reason: reason}
	}

	switch fault := checkRecordMembers(fields, true); fault.kind {
	case faultDuplicate:
		return fail(fault.field, "field appears more than once; duplicate fields are not allowed")
	case faultUnknown:
		return fail(fault.field, "unknown field; only \"batch\", \"product\", \"quantity\" and \"unit\" are allowed")
	}

	text := func(name string) (string, error) {
		rawValue, ok := findField(fields, name)
		value, problem, _ := decodeRecordText(rawValue, ok, textVerbatim)
		switch problem {
		case fieldMissing:
			return "", &FormatError{Position: pos, Batch: batchID, Field: name, Reason: "required field is missing"}
		case fieldEncoding:
			return "", &FormatError{Position: pos, Batch: batchID, Field: name,
				Reason: "must be valid UTF-8 text: malformed bytes or lone surrogate escapes are rejected instead of being replaced with U+FFFD"}
		case fieldNotString:
			return "", &FormatError{Position: pos, Batch: batchID, Field: name, Reason: "must be a JSON string"}
		}
		return value, nil
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
	b.Quantity, err = registryQuantityError(bytes.TrimSpace(quantityRaw))
	if err != nil {
		return fail("quantity", err.Error())
	}
	return b, nil
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
// source order, keeping duplicates visible for the caller to reject. Keys
// are re-scanned strictly: encoding/json would otherwise hand back U+FFFD
// substitutions for malformed key bytes.
//
// A member whose name cannot be decoded — malformed UTF-8 bytes or a lone
// surrogate escape — is reported through the badKey flag and dropped from
// the returned list: the substituted text encoding/json produced must never
// be treated as a real name, where it could collide with a genuine field or
// be misreported as an unknown field. Scanning still continues past such a
// member (its value is consumed normally) so every other member, a valid
// "batch" among them, stays visible to the caller's batch-id rule. The
// returned error is reserved for structural problems: the value not being an
// object, or malformed JSON inside it.
func parseObjectFields(raw []byte) (fields []objectField, badKey bool, err error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, false, errors.New("value is not a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	if _, err := dec.Token(); err != nil { // opening brace
		return nil, false, err
	}
	for dec.More() {
		keyStart := dec.InputOffset()
		tok, err := dec.Token()
		keyEnd := dec.InputOffset()
		if err != nil {
			return nil, false, err
		}
		key, ok := tok.(string)
		if !ok {
			return nil, false, errors.New("object keys must be JSON strings")
		}
		_, keyErr := unmarshalStringStrict(keyTokenBytes(trimmed, int(keyStart), int(keyEnd)))
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, false, err
		}
		if keyErr != nil {
			if errors.Is(keyErr, errStringEncoding) {
				badKey = true
				continue
			}
			return nil, false, keyErr
		}
		fields = append(fields, objectField{key: key, value: value})
	}
	if _, err := dec.Token(); err != nil { // closing brace
		return nil, false, err
	}
	return fields, badKey, nil
}

// trimJSONSpace strips only the four JSON whitespace bytes (space, tab,
// carriage return, newline). bytes.TrimSpace would also remove U+000B and
// other Unicode whitespace, which is illegal around a JSON token and must
// therefore be rejected rather than ignored.
func trimJSONSpace(b []byte) []byte {
	isSpace := func(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }
	for len(b) > 0 && isSpace(b[0]) {
		b = b[1:]
	}
	for len(b) > 0 && isSpace(b[len(b)-1]) {
		b = b[:len(b)-1]
	}
	return b
}

// keyTokenBytes isolates one key token inside trimmed[start:end]. The
// decoder reports the offset just after the preceding token (so the span
// may still contain a separating comma and whitespace), hence the left trim.
func keyTokenBytes(trimmed []byte, start, end int) []byte {
	return bytes.TrimLeft(trimmed[start:end], " \t\r\n,")
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
// object with exactly the four required keys, text fields as non-blank,
// valid UTF-8 strings and quantity as a strict JSON integer. The members
// are scanned with parseObjectFields (strict key decoding, duplicates kept
// visible) rather than a lenient map unmarshal, so malformed key or value
// bytes cannot be silently replaced with U+FFFD. Whatever makes the record
// fail — an unknown, duplicated or case-mismatched field included — the
// returned Input carries the normalized batch id whenever exactly one
// "batch" member holds valid, non-blank text, so the caller can name the
// batch in the error; otherwise it stays empty.
//
// The structural rules are shared with parseRegistryRecord; this source's
// own choices are: member-name anomalies are reported in source order
// (unknown and duplicate carry equal weight), text is trimmed and must stay
// non-empty, and reasons use manifest wording (including *EncodingError for
// malformed text). A member name that cannot be decoded fails the record
// with keyEncodingReason before the source-order anomaly scan — the
// U+FFFD-substituted text is never judged as an unknown or duplicate name.
func parseManifestRecord(raw json.RawMessage) (Input, error) {
	var in Input
	members, badKey, err := parseObjectFields(raw)
	if err != nil {
		return in, errors.New("record must be a JSON object with batch, product, quantity and unit")
	}
	// Attach the batch id to whatever error this record raises, but only when
	// it is unambiguous: exactly one "batch" member (compared after JSON key
	// decoding, so an escaped respelling counts as the same field) carrying a
	// JSON string whose decoded text is valid UTF-8 and non-blank once
	// trimmed. A missing, duplicated, mistyped, blank or malformed "batch"
	// member leaves the id out — never a U+FFFD substitution, and never one
	// of two duplicate values picked arbitrarily, even when both are equal.
	// Where the "batch" member sits relative to the offending field does not
	// matter.
	in.Batch = unambiguousBatchID(members, textTrimmed)

	// A member name that cannot be decoded is a name-encoding problem, never
	// an unknown field: the substituted text is not a real name.
	if badKey {
		return in, errors.New(keyEncodingReason)
	}

	// Manifest member-name problems are reported in source order: the first
	// unknown name or first repeat fails the record.
	switch fault := checkRecordMembers(members, false); fault.kind {
	case faultUnknown:
		return in, fmt.Errorf("record has unknown field %q; only batch, product, quantity and unit are allowed", fault.field)
	case faultDuplicate:
		return in, fmt.Errorf("record lists field %q more than once", fault.field)
	}

	textField := func(name string) (string, error) {
		rawValue, found := findField(members, name)
		value, problem, cause := decodeRecordText(rawValue, found, textTrimmed)
		switch problem {
		case fieldMissing:
			return "", fmt.Errorf("missing required field %q", name)
		case fieldNotString:
			return "", fmt.Errorf("field %q must be a JSON string", name)
		case fieldEncoding:
			return "", &EncodingError{Field: name}
		case fieldBlank:
			return "", fmt.Errorf("field %q must not be blank: %w", name, cause)
		}
		return value, nil
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
	quantityRaw, ok := findField(members, "quantity")
	if !ok {
		return in, errors.New("missing required field \"quantity\"")
	}
	in.Quantity, err = manifestQuantityError(bytes.TrimSpace(quantityRaw))
	if err != nil {
		return in, err
	}
	return in, nil
}

// ImportResult is one entry of an Import outcome: the normalized record in
// manifest order plus whether it was newly created or already present.
type ImportResult struct {
	Batch   Batch
	Created bool
}

// validateManifestInput applies the same effective-value rules to one
// directly submitted record that Register enforces for single entries and
// parseManifestRecord enforces for file records: the three text fields must
// be valid UTF-8 and must not be empty strings, and quantity must lie in
// 1..MaxQuantity. Text is checked verbatim — direct submissions get no
// whitespace trimming or case folding (the command-line entry normalizes
// before constructing the Input), so whitespace-only text is accepted as
// ordinary non-empty text. pos is the record's 1-based position in this
// submission. The returned *ManifestRecordError names the offending field
// in its reason and cites the batch id only when that id is itself
// non-empty, valid UTF-8 text; it is never invented or substituted when the
// batch field is empty or malformed.
func validateManifestInput(in Input, pos int) error {
	// batchID is "" whenever the batch field itself is empty or malformed, so
	// it is safe to attach it to whichever error follows.
	batchID := safeBatchIDForError(in.Batch)
	// Import reports an encoding problem anywhere in the three text fields
	// before any empty-text problem; quantity problems come after both.
	issues := inspectRecordText(in.Batch, in.Product, in.Unit)
	if issue, bad := firstTextIssue(issues, textEncoding); bad {
		return &ManifestRecordError{Position: pos, Batch: batchID, Reason: (&EncodingError{Field: issue.field}).Error()}
	}
	if issue, bad := firstTextIssue(issues, textEmpty); bad {
		return &ManifestRecordError{Position: pos, Batch: batchID, Reason: fmt.Sprintf("field %q must not be empty", issue.field)}
	}
	switch classifyDecodedQuantity(in.Quantity) {
	case decodedQtyNotPositive:
		return &ManifestRecordError{Position: pos, Batch: batchID, Reason: `field "quantity" must be greater than zero`}
	case decodedQtyAboveMax:
		return &ManifestRecordError{Position: pos, Batch: batchID,
			Reason: fmt.Sprintf("field %q must be an integer no greater than %d", "quantity", MaxQuantity)}
	}
	return nil
}

// Import validates every record of inputs before appending anything: the
// whole manifest fails if any record carries invalid UTF-8, an empty text
// field or a quantity outside 1..MaxQuantity — whether its batch id is new,
// already stored or seen earlier in this submission — or if any record
// conflicts with a stored record or with an earlier manifest record, and reg
// is left untouched on error. Directly submitted text is kept verbatim. New
// batch ids are appended in first-occurrence order; an id already stored or
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
	// Reject malformed text and non-effective values before touching
	// anything; this covers records handed in directly by Go callers the
	// same way ParseManifest covers file records, so a blank text field or a
	// non-positive quantity can never reach the new/duplicate/conflict logic
	// below and end up in a file the reader would refuse. Text is checked
	// verbatim: direct submissions get no trimming or case folding (only the
	// command-line entry normalizes, before building the Input).
	for i, in := range inputs {
		if err := validateManifestInput(in, i+1); err != nil {
			return nil, err
		}
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

// Register adds in to reg, or confirms the identical record already stored
// under the same batch id. A batch id held by a record differing in product,
// quantity or unit produces a *ConflictError and leaves reg untouched. The
// zero Outcome is returned together with the error.
func Register(reg *Registry, in Input) (Outcome, error) {
	if reg.Version != FormatVersion {
		return Outcome{}, fmt.Errorf("unsupported registry version %d", reg.Version)
	}
	// Register reports any empty text field before any encoding problem;
	// quantity problems come after both.
	issues := inspectRecordText(in.Batch, in.Product, in.Unit)
	if _, bad := firstTextIssue(issues, textEmpty); bad {
		return Outcome{}, errors.New("batch, product and unit must be non-empty")
	}
	if issue, bad := firstTextIssue(issues, textEncoding); bad {
		return Outcome{}, &EncodingError{Field: issue.field}
	}
	if !quantityInRange(in.Quantity) {
		return Outcome{}, fmt.Errorf("quantity must be a positive integer no greater than %d", MaxQuantity)
	}
	for _, b := range reg.Batches {
		if b.Batch != in.Batch {
			continue
		}
		if diffs := diffFields(b, in); len(diffs) > 0 {
			return Outcome{}, &ConflictError{Batch: in.Batch, Fields: diffs}
		}
		return Outcome{Batch: b, Created: false}, nil
	}
	b := Batch{Batch: in.Batch, Product: in.Product, Quantity: in.Quantity, Unit: in.Unit}
	reg.Batches = append(reg.Batches, b)
	return Outcome{Batch: b, Created: true}, nil
}

// validateForSave checks every record of reg against the rules Load enforces,
// so a hand-assembled registry cannot be persisted in a shape the reader
// would refuse. Records are checked in order and the first violation is
// reported as a *FormatError naming the 1-based record position, the field
// and the reason; the batch id is attached only when it is itself non-empty,
// valid UTF-8 text — never a U+FFFD substitution for a malformed id. A batch
// id seen twice is reported as a *DuplicateIDError carrying both 1-based
// positions, even when the two records are identical: Save persists records,
// it never confirms duplicates or merges them. Text is checked verbatim —
// no trimming, no case folding — and reg is never modified.
func validateForSave(reg *Registry) error {
	seen := make(map[string]int, len(reg.Batches))
	for i, b := range reg.Batches {
		pos := i + 1
		batchID := safeBatchIDForError(b.Batch)
		// Save reports the first problem in batch, product, unit field
		// order, with a field's encoding problem ahead of its empty-string
		// problem — exactly the order inspectRecordText lists issues in.
		if issues := inspectRecordText(b.Batch, b.Product, b.Unit); len(issues) > 0 {
			issue := issues[0]
			if issue.kind == textEncoding {
				return &FormatError{Position: pos, Batch: batchID, Field: issue.field,
					Reason: "must be valid UTF-8 text: malformed bytes are rejected instead of being replaced with U+FFFD"}
			}
			return &FormatError{Position: pos, Batch: batchID, Field: issue.field, Reason: "must not be empty"}
		}
		if !quantityInRange(b.Quantity) {
			return &FormatError{Position: pos, Batch: batchID, Field: "quantity",
				Reason: fmt.Sprintf("must be between 1 and %d", MaxQuantity)}
		}
		if first, dup := seen[b.Batch]; dup {
			return &DuplicateIDError{Batch: b.Batch, First: first, Second: pos}
		}
		seen[b.Batch] = pos
	}
	return nil
}

// WriteTempContent and RenameTempFile are the two failure-prone steps of the
// atomic save — writing the new content into the temporary file, and replacing
// the target with it — kept behind package-level variables so the regression
// tests (including the batch-import entry-point tests in the cmd/govflow
// package) can make a chosen step of a real save fail deterministically, on the
// save's actual target rather than a neighboring unusable path. Production
// behavior is exactly the wrapped os operations; production code never swaps
// them.
var (
	WriteTempContent = func(f *os.File, data []byte) (int, error) { return f.Write(data) }
	RenameTempFile   = os.Rename
)

// resolveRegistryTarget returns the file a save to path must actually
// replace, together with the directory the atomic save must prepare its
// temporary file in — the physical directory holding target, with every
// symbolic link in path's ancestry and every ".." resolved by the kernel's
// rules rather than folded as plain text.
//
// The link-aware resolution matters whenever an ancestor directory is a
// symbolic link. filepath.Dir and os.CreateTemp treat ".." lexically: with
// work/alias -> store/child, "work/alias/../batches.json" folds to
// "work/batches.json" even though the file the user reads is
// store/batches.json. Resolving the existing part of the directory chain
// through the kernel makes a save land in the exact file Load read and lets
// it succeed with write permission on that one directory alone — a
// directory named earlier in the user's path (e.g. work) never needs to be
// writable, and a sibling file that merely shares the lexical spelling
// (work/batches.json) is never created or replaced.
//
// The split keeps ".." un-cleaned on purpose: only the longest existing
// ancestor is resolved, and missing tail directories are reattached verbatim
// so a first registration can still create them next to the real file. When
// the file itself is a symbolic link, the link is followed to its target —
// an absolute target, or a relative one resolved against the link's own
// directory, never against the process working directory — so the save
// updates the file seen through the link instead of replacing the link with
// a regular file. A dangling link, a chain of links forming a loop, or any
// other unresolvable component is an error: the save must not create the
// target, overwrite the link, or write into the lexical ".." sibling
// directory as if this were a first registration. When path needs no
// resolution its spelling (relative or absolute) is returned unchanged.
func resolveRegistryTarget(path string) (target, dir string, err error) {
	// A symbolic link in the final component keeps its long-standing rule:
	// follow it to the linked file, reject a dangling or looping link.
	if info, lerr := os.Lstat(path); lerr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			resolved, ferr := filepath.EvalSymlinks(path)
			if ferr != nil {
				return "", "", fmt.Errorf("registry path is a symbolic link whose target cannot be resolved: %w", ferr)
			}
			return resolved, filepath.Dir(resolved), nil
		}
	} else if !errors.Is(lerr, fs.ErrNotExist) {
		return "", "", fmt.Errorf("cannot inspect registry path: %w", lerr)
	}

	// Split on the final separator without cleaning: a ".." immediately after
	// a symlinked directory must be resolved by the kernel, not folded
	// lexically into the link's sibling.
	parent, base := filepath.Split(path)
	parent = strings.TrimSuffix(parent, string(filepath.Separator))
	if parent == "" {
		parent = "."
	}

	// Walk every parent segment from the root, concatenating prefixes
	// verbatim (never filepath.Clean: that would fold "link/.." away). The
	// walk records whether any symbolic link is crossed, where the crossed
	// path physically ends, and where a not-yet-created tail begins.
	cur := "."
	if filepath.IsAbs(parent) {
		cur = string(filepath.Separator)
	}
	joinRaw := func(dir, seg string) string {
		if dir == string(filepath.Separator) {
			return dir + seg
		}
		return dir + string(filepath.Separator) + seg
	}
	crossedLink := false
	segs := splitPathSegments(parent)
	i := 0
walk:
	for ; i < len(segs); i++ {
		seg := segs[i]
		if seg == "." {
			continue
		}
		next := joinRaw(cur, seg)
		if seg == ".." {
			// Every component before a ".." must already resolve.
			if _, e := os.Stat(next); e != nil {
				return "", "", fmt.Errorf("registry path is a symbolic link whose target cannot be resolved: %w", e)
			}
			cur = next
			continue
		}
		li, e := os.Lstat(next)
		switch {
		case e == nil:
			if li.Mode()&os.ModeSymlink != 0 {
				crossedLink = true
				if _, f := os.Stat(next); f != nil {
					// A dangling or looping link in the ancestry.
					return "", "", fmt.Errorf("registry path is a symbolic link whose target cannot be resolved: %w", f)
				}
			}
			cur = next
		case errors.Is(e, fs.ErrNotExist):
			// First missing component: the rest of the walk is the
			// not-yet-created tail.
			break walk
		default:
			return "", "", fmt.Errorf("cannot inspect registry path: %w", e)
		}
	}
	// tail holds the not-yet-existing creation segments, if any.
	tail := segs[i:]

	// Past a crossed link the kernel can only resolve the tail after the
	// save creates every missing directory itself; a ".." in such a tail
	// cannot be located ahead of time, so refuse rather than guessing. With
	// no link crossed the path keeps its exact legacy spelling — MkdirAll and
	// CreateTemp resolve an ordinary ".." against directories they create.
	if crossedLink && slices.Contains(tail, "..") {
		return "", "", errors.New("registry path passes a symbolic link but cannot be resolved to an existing directory: a component before \"..\" does not exist")
	}

	// cur now names an existing directory using the kernel-meaningful
	// crossing (with work/alias -> store/child, cur for "work/alias/.." maps
	// onto store). Follow it textually and reattach a genuinely missing
	// creation tail verbatim.
	physAncestor, rerr := filepath.EvalSymlinks(cur)
	if rerr != nil {
		return "", "", fmt.Errorf("registry path is a symbolic link whose target cannot be resolved: %w", rerr)
	}
	physDir := physAncestor
	for _, seg := range tail {
		physDir = filepath.Join(physDir, seg)
	}

	// Diverge from the caller's spelling only when the lexical directory and
	// the physical directory are different directories. ".." that crosses no
	// link, or a symlinked /tmp-style prefix that lands back in the same
	// directory, keeps the exact established target, temporary-file location
	// and error wording. Identity compares the deepest existing ancestor
	// followed to its real directory plus the remaining tail text.
	lexParent := filepath.Dir(filepath.Clean(path))
	lexAnchor, lexTail := longestExistingAncestor(lexParent)
	same := false
	if resolvedLexAnchor, e2 := filepath.EvalSymlinks(lexAnchor); e2 == nil {
		if a1, e1 := os.Stat(physAncestor); e1 == nil {
			if a2, e3 := os.Stat(resolvedLexAnchor); e3 == nil && os.SameFile(a1, a2) {
				same = equalSegments(tail, lexTail)
			}
		}
	}
	if same {
		return path, filepath.Dir(path), nil
	}

	target = filepath.Join(physDir, base)
	// The computed file may itself be a dangling or looping link reached
	// through the resolved ancestry; refuse it like a direct dangling link
	// rather than replacing it with a regular file on rename.
	if info, lerr := os.Lstat(target); lerr == nil && info.Mode()&os.ModeSymlink != 0 {
		if _, ferr := filepath.EvalSymlinks(target); ferr != nil {
			return "", "", fmt.Errorf("registry path is a symbolic link whose target cannot be resolved: %w", ferr)
		}
	} else if lerr != nil && !errors.Is(lerr, fs.ErrNotExist) {
		return "", "", fmt.Errorf("cannot inspect registry path: %w", lerr)
	}
	return target, physDir, nil
}

// equalSegments reports whether two path tail lists are textually equal.
func equalSegments(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// splitPathSegments breaks dir into its slash-separated segments, dropping
// empty pieces from leading, trailing and repeated separators. "." and ".."
// are kept as real segments for the caller to resolve.
func splitPathSegments(dir string) []string {
	var segs []string
	for _, seg := range strings.Split(dir, string(filepath.Separator)) {
		if seg != "" {
			segs = append(segs, seg)
		}
	}
	return segs
}

// longestExistingAncestor walks dir up to the longest ancestor that exists,
// returning it together with the tail segments that were stripped, in
// top-to-bottom order. Lstat resolves ".." the way the kernel does, so a
// missing component past a symlinked directory is removed without ever
// collapsing the link; a first registration reattaches the tail verbatim
// once the real ancestor is known.
func longestExistingAncestor(dir string) (existing string, missing []string) {
	existing = dir
	if existing == "" {
		existing = "."
	}
	for {
		if _, err := os.Lstat(existing); err == nil {
			return existing, missing
		} else if !errors.Is(err, fs.ErrNotExist) {
			return existing, missing
		}
		upper, seg := filepath.Split(existing)
		if seg == "" {
			return existing, missing // reached a filesystem root
		}
		missing = append([]string{seg}, missing...)
		if upper == "" {
			return ".", missing // top of a relative chain: the working dir
		}
		existing = strings.TrimSuffix(upper, string(filepath.Separator))
		if existing == "" {
			existing = string(filepath.Separator)
		}
	}
}

// Save atomically writes reg to path, replacing the file only after the new
// content is fully on disk so an existing registry stays usable on failure.
// Records are serialized in their current order; the file is created with
// 0644 permissions or, when replacing an existing file, with its permissions.
//
// When path is a symbolic link, the link's target is the file replaced: the
// new content is prepared next to the target and moved over it, so the link
// stays a link to the same target and the target keeps its permissions,
// while reads through either the link or the real path see the saved
// records. The same holds when only an ancestor DIRECTORY is a link — even
// with a ".." after it, as in work/alias/../batches.json where alias points
// at store/child: lexical path cleaning would fold that to
// work/batches.json, but the kernel resolves "alias/.." to store, and so
// does the save. The temporary file is prepared in the real target
// directory (store), so write permission on that directory alone is enough;
// the link-side directory (work) is never written to and its own
// batches.json, if any, is never touched. A link whose target does not
// exist, or a link loop — whether the link is the registry file itself or
// sits in a parent directory — rejects the save with an error naming path;
// the target is never created and the link is never replaced.
//
// Every record must satisfy the same effective-value rules Load enforces, so
// a registry saved successfully always reads back: batch, product and unit
// must be non-empty valid UTF-8 text (kept verbatim — no trimming or case
// folding, so whitespace-only text is ordinary non-empty text), quantity
// must lie in 1..MaxQuantity, and batch ids must be unique under exact
// byte-for-byte comparison, two identical records included. Any violation
// rejects the whole save before anything is written: an existing target
// keeps its bytes and modification time, a missing target stays missing and
// no temporary file is left behind. A nil or empty Batches is a legal empty
// registry and is written as "batches": [] without touching reg itself.
func Save(path string, reg *Registry) error {
	if reg.Version != FormatVersion {
		return fmt.Errorf("cannot write registry format version %d", reg.Version)
	}
	if err := validateForSave(reg); err != nil {
		return fmt.Errorf("cannot save registry %q: %w", path, err)
	}
	target, dir, err := resolveRegistryTarget(path)
	if err != nil {
		return fmt.Errorf("cannot save registry %q: %w", path, err)
	}
	mode := os.FileMode(0o644)
	if info, err := os.Stat(target); err == nil {
		mode = info.Mode().Perm()
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("cannot inspect registry %q: %w", path, err)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("cannot save registry %q: %w", path, err)
	}

	// Encode a shallow copy so a nil Batches marshals as [] (null would not
	// read back) without mutating the caller's registry.
	out := *reg
	if out.Batches == nil {
		out.Batches = []Batch{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(&out); err != nil {
		return fmt.Errorf("cannot encode registry %q: %w", path, err)
	}

	tmp, err := os.CreateTemp(dir, ".govflow-registry-*.tmp")
	if err != nil {
		return fmt.Errorf("cannot save registry %q: %w", path, err)
	}
	tmpName := tmp.Name()
	abort := func(cause error) error {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("cannot save registry %q: %w", path, cause)
	}
	if _, err := WriteTempContent(tmp, buf.Bytes()); err != nil {
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
	if err := RenameTempFile(tmpName, target); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("cannot save registry %q: %w", path, err)
	}
	return nil
}
