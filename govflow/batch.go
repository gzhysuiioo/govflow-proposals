package govflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// BatchRecord is one registered batch with its four saved fields.
type BatchRecord struct {
	Batch    string `json:"batch"`
	Product  string `json:"product"`
	Quantity int64  `json:"quantity"`
	Unit     string `json:"unit"`
}

// Registration is the outcome of a batch registration attempt.
type Registration struct {
	// Record is the saved batch (the existing one for a duplicate retry).
	Record BatchRecord
	// Duplicate reports whether this submission matched an existing record.
	Duplicate bool
}

// RegisterBatch validates the inputs and registers one batch in the registry
// file at registryPath.
//
// The batch, product and unit values have leading/trailing whitespace stripped
// and must be non-empty afterwards. Quantity must be a positive decimal
// integer (digits 0-9, optional leading zeros) within the signed 64-bit range;
// it is stored and compared by integer value.
//
// A new registry file is created only when the file does not exist. An
// existing file that is empty, unreadable, malformed, or contains duplicate
// batch numbers is rejected and never overwritten. A submission whose batch
// number already exists with identical product, quantity and unit succeeds as
// a duplicate without modifying the file; any differing field is rejected and
// the existing record is left unchanged.
func RegisterBatch(registryPath, batch, product, quantityText, unit string) (*Registration, error) {
	b := strings.TrimSpace(batch)
	if b == "" {
		return nil, errors.New("--batch must not be empty (leading/trailing whitespace is stripped)")
	}
	p := strings.TrimSpace(product)
	if p == "" {
		return nil, errors.New("--product must not be empty (leading/trailing whitespace is stripped)")
	}
	u := strings.TrimSpace(unit)
	if u == "" {
		return nil, errors.New("--unit must not be empty (leading/trailing whitespace is stripped)")
	}
	qty, err := parseQuantity(quantityText)
	if err != nil {
		return nil, err
	}

	records, err := loadRegistry(registryPath)
	if err != nil {
		return nil, err
	}

	for _, existing := range records {
		if existing.Batch != b {
			continue
		}
		if existing.Product == p && existing.Quantity == qty && existing.Unit == u {
			return &Registration{Record: existing, Duplicate: true}, nil
		}
		var fields []string
		if existing.Product != p {
			fields = append(fields, "product")
		}
		if existing.Quantity != qty {
			fields = append(fields, "quantity")
		}
		if existing.Unit != u {
			fields = append(fields, "unit")
		}
		return nil, fmt.Errorf("batch %q already registered with a different %s; the existing record is unchanged",
			b, strings.Join(fields, ", "))
	}

	record := BatchRecord{Batch: b, Product: p, Quantity: qty, Unit: u}
	if err := saveRegistry(registryPath, append(records, record)); err != nil {
		return nil, err
	}
	return &Registration{Record: record, Duplicate: false}, nil
}

// parseQuantity parses a positive decimal integer made of ASCII digits only.
// Leading zeros are accepted; comparison happens by integer value.
func parseQuantity(text string) (int64, error) {
	if text == "" {
		return 0, errors.New("--quantity must be a positive decimal integer (digits 0-9)")
	}
	for i := 0; i < len(text); i++ {
		if text[i] < '0' || text[i] > '9' {
			return 0, errors.New("--quantity must be a positive decimal integer (digits 0-9 only, no sign, decimal point or whitespace)")
		}
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("--quantity is out of range (maximum %d)", math.MaxInt64)
	}
	if value <= 0 {
		return 0, errors.New("--quantity must be a positive decimal integer (zero is not allowed)")
	}
	return value, nil
}

// loadRegistry reads and validates the registry file. A non-existent file
// yields no records so the caller can start a new registry; any other read
// error or malformed content is reported.
func loadRegistry(path string) ([]BatchRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("cannot read registry file %q: %w", path, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, fmt.Errorf("registry file %q is empty; refusing to overwrite an existing file", path)
	}

	lines := strings.Split(string(data), "\n")
	// A single trailing newline from the last record is allowed.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	records := make([]BatchRecord, 0, len(lines))
	firstLine := make(map[string]int)
	for i, line := range lines {
		lineNo := i + 1
		if line == "" {
			return nil, fmt.Errorf("registry file %q: line %d: blank line is not allowed", path, lineNo)
		}
		record, err := parseRecord(line)
		if err != nil {
			return nil, fmt.Errorf("registry file %q: line %d: %w", path, lineNo, err)
		}
		if prev, ok := firstLine[record.Batch]; ok {
			return nil, fmt.Errorf("registry file %q: batch %q is registered more than once (lines %d and %d)",
				path, record.Batch, prev, lineNo)
		}
		firstLine[record.Batch] = lineNo
		records = append(records, record)
	}
	return records, nil
}

// parseRecord parses one JSON-line record of the public registry format.
func parseRecord(line string) (BatchRecord, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &fields); err != nil {
		return BatchRecord{}, fmt.Errorf("record is not valid JSON: %w", err)
	}
	if len(fields) != 4 {
		return BatchRecord{}, fmt.Errorf("record must have exactly the fields batch, product, quantity, unit")
	}
	for _, name := range []string{"batch", "product", "quantity", "unit"} {
		if _, ok := fields[name]; !ok {
			return BatchRecord{}, fmt.Errorf("record is missing field %q", name)
		}
	}
	var record BatchRecord
	if err := json.Unmarshal(fields["batch"], &record.Batch); err != nil {
		return BatchRecord{}, fmt.Errorf("batch must be a JSON string")
	}
	if err := json.Unmarshal(fields["product"], &record.Product); err != nil {
		return BatchRecord{}, fmt.Errorf("product must be a JSON string")
	}
	if err := json.Unmarshal(fields["unit"], &record.Unit); err != nil {
		return BatchRecord{}, fmt.Errorf("unit must be a JSON string")
	}
	for _, value := range []string{record.Batch, record.Product, record.Unit} {
		if strings.TrimSpace(value) == "" {
			return BatchRecord{}, fmt.Errorf("batch, product and unit must not be empty")
		}
	}
	quantity, err := parseStoredQuantity(fields["quantity"])
	if err != nil {
		return BatchRecord{}, err
	}
	record.Quantity = quantity
	return record, nil
}

// parseStoredQuantity validates a quantity value found in a registry file:
// a JSON integer (no fraction or exponent) in the positive int64 range.
func parseStoredQuantity(raw json.RawMessage) (int64, error) {
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return 0, errors.New("quantity must be a positive integer")
	}
	for i := 0; i < len(text); i++ {
		if text[i] < '0' || text[i] > '9' {
			return 0, errors.New("quantity must be a positive integer (digits only, no fraction or exponent)")
		}
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("quantity is out of range (maximum %d)", math.MaxInt64)
	}
	if value <= 0 {
		return 0, errors.New("quantity must be a positive integer")
	}
	return value, nil
}

// saveRegistry writes all records atomically: a temp file in the same
// directory is synced and renamed over the target, so a failed save never
// leaves a half-written registry behind.
func saveRegistry(path string, records []BatchRecord) error {
	var buf bytes.Buffer
	for _, record := range records {
		line, err := json.Marshal(record)
		if err != nil {
			return fmt.Errorf("cannot encode registry record: %w", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".govflow-registry-*")
	if err != nil {
		return fmt.Errorf("cannot create temporary file next to registry %q: %w", path, err)
	}
	tmpName := tmp.Name()

	failed := func(closeErr error) error {
		tmp.Close()
		_ = os.Remove(tmpName)
		return closeErr
	}
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		return failed(fmt.Errorf("cannot write registry file %q: %w", path, err))
	}
	if err := tmp.Sync(); err != nil {
		return failed(fmt.Errorf("cannot sync registry file %q: %w", path, err))
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("cannot close registry file %q: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("cannot save registry file %q: %w", path, err)
	}
	return nil
}
