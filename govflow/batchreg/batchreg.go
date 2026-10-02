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

// ImportRecord is one normalized record read from a batch-import input list.
type ImportRecord struct {
	Batch    string
	Product  string
	Quantity int64
	Unit     string
}

// ImportResult is one entry of the batch-import outcome: the normalized
// record and its status, in the list's original order.
type ImportResult struct {
	Batch    string `json:"batch"`
	Product  string `json:"product"`
	Quantity int64  `json:"quantity"`
	Unit     string `json:"unit"`
	Status   string `json:"status"`
}

// ImportConflictError reports that a record in an import list reuses a batch
// id already held by the registry or by an earlier record of the same list,
// with product, quantity or unit differing. Position is the 1-based record
// number in the input list.
type ImportConflictError struct {
	Position int
	Batch    string
	Fields   []string
}

func (e *ImportConflictError) Error() string {
	return fmt.Sprintf("record %d: batch %q is already registered with conflicting field(s): %s; the existing record cannot be overwritten",
		e.Position, e.Batch, strings.Join(e.Fields, ", "))
}

// ParseImportInput strictly parses the batch-import input document. The
// document must be a non-empty JSON array of objects, each containing exactly
// the four fields batch, product, quantity and unit. The three text fields
// are trimmed and must stay non-empty afterwards; quantity must be a JSON
// integer in [1, MaxQuantity] — strings, fractions, exponents and other
// non-integer forms are rejected. Every failure is reported with the
// offending record's 1-based position.
func ParseImportInput(data []byte) ([]ImportRecord, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errors.New("input file is empty")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var wire []struct {
		Batch    *string         `json:"batch"`
		Product  *string         `json:"product"`
		Quantity json.RawMessage `json:"quantity"`
		Unit     *string         `json:"unit"`
	}
	if err := dec.Decode(&wire); err != nil {
		return nil, fmt.Errorf("input must be a non-empty JSON array of records: %w", err)
	}
	if len(wire) == 0 {
		return nil, errors.New("input array is empty")
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("unexpected data after the JSON array")
		}
		return nil, err
	}
	records := make([]ImportRecord, 0, len(wire))
	for i, w := range wire {
		pos := i + 1
		if w.Batch == nil {
			return nil, fmt.Errorf("record %d: missing required field \"batch\"", pos)
		}
		if w.Product == nil {
			return nil, fmt.Errorf("record %d: missing required field \"product\"", pos)
		}
		if w.Unit == nil {
			return nil, fmt.Errorf("record %d: missing required field \"unit\"", pos)
		}
		if len(w.Quantity) == 0 {
			return nil, fmt.Errorf("record %d: missing required field \"quantity\"", pos)
		}
		quantity, err := ParseQuantity(string(w.Quantity))
		if err != nil {
			return nil, fmt.Errorf("record %d: invalid quantity: %w", pos, err)
		}
		batch, err := NormalizeField(*w.Batch)
		if err != nil {
			return nil, fmt.Errorf("record %d: invalid batch: %w", pos, err)
		}
		product, err := NormalizeField(*w.Product)
		if err != nil {
			return nil, fmt.Errorf("record %d: invalid product: %w", pos, err)
		}
		unit, err := NormalizeField(*w.Unit)
		if err != nil {
			return nil, fmt.Errorf("record %d: invalid unit: %w", pos, err)
		}
		records = append(records, ImportRecord{
			Batch: batch, Product: product, Quantity: quantity, Unit: unit,
		})
	}
	return records, nil
}

// Import validates records against reg and produces the per-record status
// list in list order. A record whose batch id matches an existing registry
// record or an earlier record of this list is a duplicate when product,
// quantity and unit all match; otherwise the whole import fails with
// *ImportConflictError and reg is left untouched. New records are appended
// to reg in first-occurrence order only after every record has been accepted,
// so a rejected list never leaves partial new batches behind.
func Import(reg *Registry, records []ImportRecord) ([]ImportResult, error) {
	if reg.Version != FormatVersion {
		return nil, fmt.Errorf("unsupported registry version %d", reg.Version)
	}
	added := make([]Batch, 0)
	results := make([]ImportResult, 0, len(records))
	for i, rec := range records {
		pos := i + 1
		if rec.Batch == "" || rec.Product == "" || rec.Unit == "" {
			return nil, fmt.Errorf("record %d: batch, product and unit must be non-empty", pos)
		}
		if rec.Quantity <= 0 || rec.Quantity > MaxQuantity {
			return nil, fmt.Errorf("record %d: quantity must be a positive integer no greater than %d", pos, MaxQuantity)
		}
		existing := findBatch(reg.Batches, rec.Batch)
		if existing == nil {
			existing = findBatch(added, rec.Batch)
		}
		if existing != nil {
			var diffs []string
			if existing.Product != rec.Product {
				diffs = append(diffs, "product")
			}
			if existing.Quantity != rec.Quantity {
				diffs = append(diffs, "quantity")
			}
			if existing.Unit != rec.Unit {
				diffs = append(diffs, "unit")
			}
			if len(diffs) > 0 {
				return nil, &ImportConflictError{Position: pos, Batch: rec.Batch, Fields: diffs}
			}
			results = append(results, ImportResult{
				Batch: rec.Batch, Product: rec.Product, Quantity: rec.Quantity, Unit: rec.Unit,
				Status: "duplicate",
			})
			continue
		}
		added = append(added, Batch{Batch: rec.Batch, Product: rec.Product, Quantity: rec.Quantity, Unit: rec.Unit})
		results = append(results, ImportResult{
			Batch: rec.Batch, Product: rec.Product, Quantity: rec.Quantity, Unit: rec.Unit,
			Status: "created",
		})
	}
	reg.Batches = append(reg.Batches, added...)
	return results, nil
}

func findBatch(batches []Batch, id string) *Batch {
	for i := range batches {
		if batches[i].Batch == id {
			return &batches[i]
		}
	}
	return nil
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
