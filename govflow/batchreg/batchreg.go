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
type DuplicateIDError struct {
	Batch string
}

func (e *DuplicateIDError) Error() string {
	return fmt.Sprintf("registry contains multiple records for batch %q", e.Batch)
}

// NormalizeField trims leading and trailing whitespace; the result must stay
// non-empty. Interior characters, including interior whitespace, are kept,
// and casing is preserved so "B1" and "b1" are different values.
func NormalizeField(value string) (string, error) {
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
// the public format, of an unsupported version, or holding duplicate batch
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

func decode(data []byte) (*Registry, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	reg := &Registry{}
	if err := dec.Decode(reg); err != nil {
		return nil, err
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("unexpected data after registry object")
		}
		return nil, err
	}
	if reg.Version != FormatVersion {
		return nil, fmt.Errorf("unsupported registry version: got %d, want %d", reg.Version, FormatVersion)
	}
	return reg, nil
}

func validate(reg *Registry) error {
	seen := make(map[string]struct{}, len(reg.Batches))
	for i, b := range reg.Batches {
		if b.Batch == "" || b.Product == "" || b.Unit == "" {
			return fmt.Errorf("batches[%d] (batch %q): batch, product and unit must all be non-empty", i, b.Batch)
		}
		if b.Quantity <= 0 {
			return fmt.Errorf("batches[%d] (batch %q): quantity must be a positive integer", i, b.Batch)
		}
		if _, dup := seen[b.Batch]; dup {
			return &DuplicateIDError{Batch: b.Batch}
		}
		seen[b.Batch] = struct{}{}
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

// requiredManifestFields are the only members a manifest record may carry.
var requiredManifestFields = map[string]struct{}{
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
		if _, ok := requiredManifestFields[name]; !ok {
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
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil { // opening brace
		return nil, fmt.Errorf("record is not a JSON object: %w", err)
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := tok.(string)
		if !ok {
			return nil, errors.New("record object keys must be JSON strings")
		}
		keys = append(keys, key)
		// Skip over the value token(s) so the next token is the next key.
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return nil, err
		}
	}
	if _, err := dec.Token(); err != nil { // closing brace
		return nil, err
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
