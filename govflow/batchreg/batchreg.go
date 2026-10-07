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

// DirectoryPathError reports a registry path that the user spelled as a
// DIRECTORY location rather than a file: it ends in one or more path
// separators ("store/new.json/"), or its final path component is "." or ".."
// ("store/new/.", "store/new/.."). Such a spelling can never name the
// registry file — no matter whether the location does not exist yet, already
// is a directory, or happens to have a regular file directly beneath the
// spelled directory name (a plain file named "new.json" beside a
// "store/new.json/" request, or a plain file named "new" beside a
// "store/new/." request). Saving there would either fail against the
// directory or silently create that sibling regular file, so the success
// would name a path the user cannot read the registry back through.
//
// Load and Save therefore refuse the path on its spelling alone, without
// touching the filesystem: no missing parent directory is created, no
// registry file or temporary file is written at the name left after stripping
// the trailing directory marker, an existing file keeps its bytes,
// permissions and modification time, and an existing directory or symbolic
// link is never replaced. Path carries the registry path exactly as the
// caller supplied it; the reason states that the registry target must be a
// file while this path denotes a directory. A "." or ".." in a MIDDLE path
// component is not a directory marker and keeps resolving by the ordinary
// path rules.
type DirectoryPathError struct {
	Path string
}

func (e *DirectoryPathError) Error() string {
	return fmt.Sprintf(
		"registry path %s denotes a directory (the path ends with a separator or its last component is \".\" or \"..\"); "+
			"the registry target must be a file, so this path cannot be used to save a registry",
		strconv.Quote(e.Path))
}

// registryPathDenotesDirectory reports whether path is spelled as a directory
// location rather than a file: it ends in one or more path separators, or its
// final path component — the part after the last separator, with no trailing
// separator present — is "." or "..". The check is purely lexical on the
// caller's exact spelling: it never cleans the path (filepath cleaning would
// erase exactly the trailing directory marker the caller typed) and never
// inspects the filesystem, so a location that does not exist yet is refused
// just like an existing directory, and a regular file sitting directly
// beneath the spelled name never tempts a save into creating it. A "." or
// ".." in a MIDDLE component is not a marker: "store/./b.json" and
// "store/sub/../b.json" name files and keep their ordinary resolution. An
// empty path is left to the caller's own missing-argument validation.
func registryPathDenotesDirectory(path string) bool {
	if path == "" {
		return false
	}
	if strings.HasSuffix(path, string(os.PathSeparator)) {
		return true
	}
	base := path[strings.LastIndexByte(path, os.PathSeparator)+1:]
	return base == "." || base == ".."
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
// create the target the user never asked for. This holds for a link on the
// final component AND for a link on a DIRECTORY component: when work/alias
// points at a directory that does not exist, neither work/alias/batches.json
// nor work/alias/../batches.json reaches any file the way the kernel
// resolves names (the ".." cannot leave a directory the link never reached),
// so both fail even if a same-named work/batches.json exists beside the
// link — an os.ReadFile on such a path reports only fs.ErrNotExist,
// indistinguishable from a registry file that has never been created, and
// the reachability check below tells the two cases apart. The check only
// inspects the path (Lstat/EvalSymlinks): it creates no directory, file or
// temporary file and changes no link. A valid directory link into a real
// directory where the registry file is merely absent stays an ordinary
// first registration, and a valid link mixed with ".." resolves by real
// directory relations.
//
// A path the caller spells as a DIRECTORY location — ending in one or more
// path separators, or whose final component is "." or ".." — is a
// *DirectoryPathError naming the exact supplied path, before any filesystem
// inspection and for every underlying state alike: the location absent, an
// existing directory there, or a regular file sitting directly beneath the
// spelled directory name (a file "new.json" beside a "new.json/" request).
// Such a path never names a registry file, so Load neither reads it nor
// treats it as a fresh empty registry.
func Load(path string) (reg *Registry, existed bool, err error) {
	// Refuse a path spelled as a directory (trailing separator, or a final
	// "."/".." component) before any filesystem call: it can never name a
	// registry file, and the later Save must not be allowed to create a
	// sibling regular file that this path cannot read back. This also stops a
	// path over an existing directory being reported with the kernel's
	// "is a directory" read error rather than the file-vs-directory rule.
	if registryPathDenotesDirectory(path) {
		return nil, true, &DirectoryPathError{Path: path}
	}
	if err := checkRegistryPathReachable(path); err != nil {
		return nil, true, fmt.Errorf("cannot read registry %q: %w; refusing to treat the unreachable path as a new registry", path, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// A dangling symbolic link also reports "not exist", but the path
			// is occupied: the caller must not treat it as a first
			// registration. checkRegistryPathReachable above already refuses
			// a link that is dangling now (on the final component or on a
			// leading directory); this Lstat is the backstop for a link that
			// became dangling between the check and the read.
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

// storedDuplicateID reports the first batch id held by two records of the
// registry as handed in, scanning in stored order: Batch is that id, First
// the 1-based position of its first record and Second the position of its
// earliest repeat. It returns nil when every stored id is unique. Ids compare
// byte for byte, so "B1", "b1" and " B1 " are different ids.
//
// Register and Import run this check because a Go caller may hand them a
// Registry assembled directly — without Load — whose records already share an
// id. Neither entry point may then treat one of those records as the id's
// record (confirming a repeat against it, conflicting with it or appending
// next to it), and neither may repair the registry by dropping or merging a
// record: the only honest answer is the DuplicateIDError Load would have
// raised.
func storedDuplicateID(reg *Registry) *DuplicateIDError {
	seen := make(map[string]int, len(reg.Batches))
	for i, b := range reg.Batches {
		pos := i + 1
		if first, dup := seen[b.Batch]; dup {
			return &DuplicateIDError{Batch: b.Batch, First: first, Second: pos}
		}
		seen[b.Batch] = pos
	}
	return nil
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
// is left untouched on error. A reg that already holds two records under one
// batch id (only possible when it was assembled directly, since Load refuses
// such files) likewise fails the whole submission with a *DuplicateIDError
// naming the id and the two 1-based stored positions — checked after the
// version and record validations above, before any create, duplicate
// confirmation or conflict, and leaving reg exactly as handed in. Directly
// submitted text is kept verbatim. New
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

	// A registry that already holds two records under one id — possible only
	// when the caller assembled it directly, since Load refuses such files —
	// rejects the whole submission before any create, duplicate confirmation
	// or conflict is weighed: the existing records are ambiguous and Import
	// must neither pick one of them as the id's record nor repair the
	// registry on its own.
	if dup := storedDuplicateID(reg); dup != nil {
		return nil, dup
	}

	// Work on a copy: a rejected manifest must never partially land in reg.
	// The create / duplicate / conflict decision itself lives in batchTable —
	// the same one Register uses; only the source attribution and the
	// all-or-nothing rollback are manifest specific.
	table := newBatchTable(reg.Batches)

	out := make([]ImportResult, len(inputs))
	for i, in := range inputs {
		pos := i + 1
		b, created, diffs := table.accept(in, pos)
		if len(diffs) > 0 {
			// reg was never touched: the accepted records live only in the
			// table's copy, so abandoning it leaves the registry as handed in.
			if inManifest, prev := table.conflictSource(in.Batch); inManifest {
				return nil, &ManifestConflictError{Position: pos, Batch: in.Batch, Fields: diffs, Source: "manifest", PrevPos: prev}
			}
			return nil, &ManifestConflictError{Position: pos, Batch: in.Batch, Fields: diffs, Source: "registry"}
		}
		out[i] = ImportResult{Batch: b, Created: created}
	}

	reg.Batches = table.records
	return out, nil
}

// Register adds in to reg, or confirms the identical record already stored
// under the same batch id. A batch id held by a record differing in product,
// quantity or unit produces a *ConflictError and leaves reg untouched. A reg
// that already holds two records under one batch id (only possible when it
// was assembled directly, since Load refuses such files) produces a
// *DuplicateIDError naming the id and the two 1-based stored positions —
// reported after the version and input validations above, before any stored
// record is consulted, and leaving reg exactly as handed in. The zero Outcome
// is returned together with the error.
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
	// A registry already holding two records under one id — possible only
	// when the caller assembled it directly, since Load refuses such files —
	// rejects the registration before the stored records are consulted: no
	// record may be picked as the id's record for a confirmation or a
	// conflict, and the ambiguous records are never merged or pruned.
	if dup := storedDuplicateID(reg); dup != nil {
		return Outcome{}, dup
	}
	// Accept the record through the same create / duplicate / conflict rule
	// Import uses; Register has no sequence positions, so a newly introduced id
	// carries none. Only the concrete error type is Register-specific.
	table := newBatchTable(reg.Batches)
	b, created, diffs := table.accept(in, 0)
	if len(diffs) > 0 {
		return Outcome{}, &ConflictError{Batch: in.Batch, Fields: diffs}
	}
	if created {
		reg.Batches = table.records
	}
	return Outcome{Batch: b, Created: created}, nil
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

// errUnreachablePathLink is the refusal reason for a registry path that runs
// into a symbolic link whose target cannot be resolved — a dangling link or a
// link loop, on a directory component as well as on the final component. It
// is deliberately worded as an unusable link, not as a registry file that has
// merely not been created: the two are indistinguishable from ReadFile's
// fs.ErrNotExist alone, and only the former is a first registration.
var errUnreachablePathLink = errors.New("a symbolic link in the registry path has a target that does not exist or cannot be resolved")

// checkRegistryPathReachable decides, before the registry bytes are read,
// whether path can reach anything the way the kernel resolves names. It
// exists because os.ReadFile reports fs.ErrNotExist both for a registry file
// that has never been created (a legitimate first registration) and for a
// path that descends through a dangling directory symbolic link (not one):
// with work/alias pointing at a missing directory, ReadFile on
// work/alias/batches.json — and, because the kernel cannot step ".." out of
// a directory the link never reaches, on work/alias/../batches.json too —
// returns exactly the not-exist error even when work/batches.json exists.
//
// The check climbs through the raw components missing from the filesystem
// (never by lexical cleaning, which would collapse "alias/.." before the
// kernel resolves alias) to the longest existing prefix. A ".." among the
// missing components is harmless here: without a symbolic link in the path
// the prefix genuinely exists, so such a path is either a real file (read
// normally) or a path through missing ordinary directories, and reading is
// allowed either way — the read creates nothing and Save runs its own
// stricter refusal before writing. What must fail is an existing prefix that
// is itself a symbolic link which cannot be chased to a real directory:
// EvalSymlinks on the prefix names exactly that failure for a dangling
// link or a link loop, including one reached through several links. A
// relative link target is resolved by EvalSymlinks against the link's own
// directory, matching the kernel. The check only calls Lstat and
// EvalSymlinks, so it never creates a directory, registry file or temporary
// file and never modifies a link.
func checkRegistryPathReachable(path string) error {
	cur := path
	for {
		info, err := os.Lstat(cur)
		if err == nil {
			// cur is the longest existing prefix. A prefix that is itself a
			// symbolic link which cannot be chased to a real directory is the
			// one failure that looks like a never-created registry file: the
			// final read reports fs.ErrNotExist either way. EvalSymlinks
			// resolves an absolute link target, or a relative one against the
			// link's own directory (never the process working directory),
			// through every hop of a chain; a dangling link or a loop fails
			// here. Any other failure at a deeper component (for example
			// permission denied) is left for the read itself to report.
			if info.Mode()&os.ModeSymlink != 0 {
				if _, eerr := filepath.EvalSymlinks(cur); eerr != nil {
					return errUnreachablePathLink
				}
			}
			return nil
		}
		parent, _ := rawParent(cur)
		if parent == cur {
			// No existing prefix at all: a bare relative name resolved
			// against the process working directory, the same way an
			// ordinary open would. No symbolic link could have been
			// encountered, so a not-exist is an ordinary first registration
			// and any other failure is the read's own to report.
			return nil
		}
		cur = parent
	}
}

// resolveRegistryTarget returns the real file a save to path must replace,
// resolved the same way the kernel resolves path for reads: every symbolic
// link is chased — links on DIRECTORY components included — and every ".."
// steps out of the directory it really leaves, not out of the link's own
// parent name. The result is a cleaned path with no link or ".." component
// naming exactly the file Load read, so the temporary file and the rename
// land in the target's real directory (a relative path stays relative,
// resolved against the process working directory as the input was).
//
// Physical resolution is what keeps read and save locations identical when a
// directory link is followed by "..": with work/alias -> store/child, the
// path work/alias/../batches.json means store/batches.json. Lexical cleaning
// (filepath.Clean, filepath.Dir) collapses alias/.. to work and would save
// next to the link — into an unrelated, possibly read-only directory.
// Components missing from the filesystem (a first registration) are appended
// verbatim onto the longest existing prefix, so creating a new file needs
// write permission only in the directory that really holds it.
//
// A path that descends into a directory that does not exist and only then
// climbs back with ".." — store/missing/../batches.json with no store/missing
// — cannot reach any file the way the kernel resolves names, so it is an
// error rather than a first registration: the save neither creates the
// missing directory (which would turn an unresolvable path into a live one)
// nor writes the lexically cleaned name (store/batches.json), which may be an
// unrelated existing registry. A path that only creates new directories
// without climbing back out (store/new/batches.json), or climbs ".." after a
// prefix that really exists, is resolved and saved normally.
//
// A dangling symbolic link or a link loop — on the final component or on a
// directory leading to it — is an error: the save must not create the target
// or overwrite the link as if this were a first registration.
func resolveRegistryTarget(path string) (string, error) {
	// Climb through components missing from the filesystem by stripping raw
	// path segments, never by lexical cleaning: filepath.Clean would collapse
	// "alias/.." against the link's own parent name before the kernel ever
	// resolves alias, sending the save at work/x.json instead of store/x.json.
	// os.Lstat resolves every component but the last exactly as the kernel
	// does, so each surviving prefix is checked with its real meaning.
	cur := path
	var tail []string
	for {
		info, err := os.Lstat(cur)
		if err == nil {
			// Refuse a ".." among the not-yet-existing components that climbs
			// back out of a directory which would first have to be created:
			// the user's path itself cannot name anything there, and creating
			// the directory would make a previously unresolvable path valid.
			if missing, escape := missingDirDotDotEscape(tail); escape {
				return "", errEscapeThroughMissingDir(missing)
			}
			// cur is the longest existing prefix. Resolve it physically: an
			// absolute link target, or a relative one resolved against the
			// link's own directory (never the process working directory), is
			// followed; a dangling link or a loop fails here and is reported
			// as an unusable symbolic link rather than a fresh-registry path.
			base, eerr := filepath.EvalSymlinks(cur)
			if eerr != nil {
				if info.Mode()&os.ModeSymlink != 0 {
					return "", fmt.Errorf("registry path is a symbolic link whose target cannot be resolved: %w", eerr)
				}
				return "", fmt.Errorf("cannot inspect registry path: %w", eerr)
			}
			// Append the not-yet-existing components verbatim: a first
			// registration needs write permission only in the real directory
			// that will hold the file, never in directories merely traversed.
			target := base
			for i := len(tail) - 1; i >= 0; i-- {
				target = filepath.Join(target, tail[i])
			}
			return filepath.Clean(target), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("cannot inspect registry path: %w", err)
		}
		parent, base := rawParent(cur)
		tail = append(tail, base)
		if parent == cur {
			// A bare relative name with no existing prefix: it names a file
			// in the process working directory, just as a plain open would —
			// unless it climbs ".." out of a directory that does not exist.
			if missing, escape := missingDirDotDotEscape(tail); escape {
				return "", errEscapeThroughMissingDir(missing)
			}
			return filepath.Clean(path), nil
		}
		cur = parent
	}
}

// missingDirDotDotEscape inspects the raw components stripped while climbing
// to the longest existing prefix. tail holds them from the registry file end
// (tail[0] is the final, registry-file component) toward the existing prefix
// (tail[len-1] sits next to it). A ".." that climbs out of a directory
// descended into only within the not-yet-existing tail marks exactly the
// unresolvable shape — store/missing/../batches.json — that must be refused:
// the kernel cannot resolve the ".." until "missing" exists, and it never
// legitimately exists on this path.
//
// The final component is the registry file name, never a directory descent,
// so tail[0] is not counted. ".." steps matched by an equally non-existent
// descent keep the lexical depth positive and do not cancel the violation:
// the first ".." that leaves a missing directory is already fatal. The
// returned name is the missing directory that ".." immediately tries to
// leave. Symlinked or genuinely existing components never reach tail, so
// this never trips over a physical ".." (as after work/alias -> store/child).
func missingDirDotDotEscape(tail []string) (missing string, escape bool) {
	depth := 0
	lastMissing := ""
	// Walk from the existing-prefix side toward the file.
	for i := len(tail) - 1; i >= 1; i-- {
		switch tail[i] {
		case ".", "":
			// A "." (or the empty segment of a doubled separator) is a
			// lexical no-op even in the not-yet-existing tail.
		case "..":
			if depth > 0 {
				return lastMissing, true
			}
			depth--
		default:
			depth++
			lastMissing = tail[i]
		}
	}
	return "", false
}

// errEscapeThroughMissingDir is the refusal reason for a registry path that
// descends into a non-existent directory and then climbs back with "..". The
// Save wrapper adds the user-supplied path; the missing directory is named so
// the user can see which component made the path unresolvable.
func errEscapeThroughMissingDir(missing string) error {
	return fmt.Errorf(
		"the path descends through directory %s, which does not exist, and then climbs back with \"..\"; "+
			"such a path cannot reach an existing file and the missing directory is not created to make it valid, so the registry is not written",
		strconv.Quote(missing))
}

// rawParent splits path at its final separator without resolving "..", so the
// caller can climb one raw component at a time and keep physical resolution of
// any directory link in the surviving prefix.
func rawParent(path string) (parent, base string) {
	i := strings.LastIndexByte(path, os.PathSeparator)
	if i < 0 {
		return path, path
	}
	return path[:i], path[i+1:]
}

// Save atomically writes reg to path, replacing the file only after the new
// content is fully on disk so an existing registry stays usable on failure.
// Records are serialized in their current order; the file is created with
// 0644 permissions or, when replacing an existing file, with its permissions.
//
// When path reaches the registry through a symbolic link, the link's target
// is the file replaced: the new content is prepared next to the target and
// moved over it, so the link stays a link to the same target and the target
// keeps its permissions, while reads through either the link or the real
// path see the saved records. This holds for a link on the final component
// and for a link on a DIRECTORY component, including a ".." that steps back
// after it: with work/alias -> store/child, work/alias/../batches.json is
// store/batches.json, and the save writes there even though the directory
// containing the link (here work) is not writable — write access is needed
// only in the target's real directory. A link whose target does not exist,
// or a link loop, on the file or on a leading directory, rejects the save
// with an error naming path — the target is never created and the link is
// never replaced.
//
// A path that descends through a directory that does not exist and only then
// climbs back with ".." (store/missing/../batches.json with store/missing
// absent) is likewise rejected before anything is written: such a path
// reaches no file the way the kernel resolves names, so the save neither
// creates the missing directory — which would turn the unresolvable path into
// a live one — nor writes the lexically cleaned name, where an unrelated
// registry could be overwritten. A ".." after a directory that genuinely
// exists, including the real target of a directory symlink, still resolves by
// real directory relations, and a not-yet-existing parent that is never
// climbed out of (store/new/batches.json) is created as on any first save.
//
// A path spelled as a DIRECTORY location — one ending in one or more path
// separators ("store/new.json/"), or whose final component is "." or ".."
// ("store/new/.", "store/new/..") — is rejected with a *DirectoryPathError
// purely on the spelling, before validation or any filesystem change. The
// rule is the same whether the location does not exist yet, already is a
// directory, or has a regular file directly beneath the spelled name: the
// save never creates a missing parent, never writes a registry file or
// temporary file at the name left after stripping the trailing separator or
// the final "."/"..", and never replaces an existing directory or symbolic
// link. Stripping the marker would otherwise create a file the supplied path
// cannot read back ("store/new.json/" would create plain file
// "store/new.json"; "store/new/." would create plain file "store/new"). A
// "." or ".." in a MIDDLE component is not a directory marker and keeps its
// ordinary resolution.
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
	// The path's file-or-directory meaning is decided before anything else: a
	// trailing separator or a final "."/".." component spells a directory
	// location and can never hold a registry file. Refusing here — purely on
	// the spelling, before version and record validation, directory creation,
	// the temporary file or the rename — guarantees the table the caller
	// handed in is untouched and nothing appears at the name left after
	// stripping the trailing directory marker.
	if registryPathDenotesDirectory(path) {
		return &DirectoryPathError{Path: path}
	}
	if reg.Version != FormatVersion {
		return fmt.Errorf("cannot write registry format version %d", reg.Version)
	}
	if err := validateForSave(reg); err != nil {
		return fmt.Errorf("cannot save registry %q: %w", path, err)
	}
	target, err := resolveRegistryTarget(path)
	if err != nil {
		return fmt.Errorf("cannot save registry %q: %w", path, err)
	}
	mode := os.FileMode(0o644)
	if info, err := os.Stat(target); err == nil {
		mode = info.Mode().Perm()
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("cannot inspect registry %q: %w", path, err)
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
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

	tmp, err := os.CreateTemp(filepath.Dir(target), ".govflow-registry-*.tmp")
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
