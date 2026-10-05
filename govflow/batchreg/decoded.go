package batchreg

import "unicode/utf8"

// This file holds the validity rules for one record that has already been
// decoded into Go values — the shape Register, Import and Save all receive
// directly from Go callers. The three entry points enforce the same rules:
//
//   - batch, product and unit are taken verbatim (no trimming and no case
//     folding, so whitespace-only text is an ordinary non-empty value), must
//     be valid UTF-8 and must not be empty;
//   - quantity must lie in 1..MaxQuantity.
//
// They differ only in which fault they report first and in the error type and
// wording that wraps it. checkDecodedRecord therefore collects every fault of
// a record in one pass, and each entry point selects the first fault in its
// own established order instead of re-implementing the checks.
type textFieldFault int

const (
	textFieldOK textFieldFault = iota
	textFieldEmpty
	textFieldInvalidUTF8
)

// quantityFault classifies a decoded quantity against the 1..MaxQuantity
// window. The two sides are kept distinct even though Register and Save phrase
// them alike, because a direct Import reports them with different sentences.
type quantityFault int

const (
	quantityOK quantityFault = iota
	quantityNotPositive
	quantityTooLarge
)

// decodedRecordFaults lists every fault of one decoded record: one entry per
// text field in batch, product, unit order, plus the quantity.
type decodedRecordFaults struct {
	text     [3]textFieldFault
	quantity quantityFault
}

// checkDecodedRecord inspects one already-decoded record without modifying it
// and collects all of its faults. Text is read verbatim, exactly as the Go
// caller submitted it: no whitespace is trimmed and no case is folded, so
// " B1 " and "B1" are different values and whitespace-only text is non-empty.
// An empty string is itself valid UTF-8, so a text field carries at most one
// of the two text faults.
func checkDecodedRecord(batch, product, unit string, quantity int64) decodedRecordFaults {
	var faults decodedRecordFaults
	values := [3]string{batch, product, unit}
	for i := range textFieldNames {
		switch {
		case !utf8.ValidString(values[i]):
			faults.text[i] = textFieldInvalidUTF8
		case values[i] == "":
			faults.text[i] = textFieldEmpty
		}
	}
	switch {
	case quantity < 1:
		faults.quantity = quantityNotPositive
	case quantity > MaxQuantity:
		faults.quantity = quantityTooLarge
	}
	return faults
}

// firstTextFault returns the name of the first text field carrying want,
// scanning in batch, product, unit order.
func (f decodedRecordFaults) firstTextFault(want textFieldFault) (string, bool) {
	for i, fault := range f.text {
		if fault == want {
			return textFieldNames[i], true
		}
	}
	return "", false
}
