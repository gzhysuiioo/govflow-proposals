package batchreg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"unicode/utf8"
)

// This file holds the single-record rules shared by the two on-disk sources:
// the registry file (decode, where stored text is already authoritative) and
// an import manifest (ParseManifest, where text is normalized on the way in).
// Both sources accept exactly the same four lowercase fields with the same
// strict JSON decoding — case-sensitive names, escaped spellings counted,
// duplicates visible, strict UTF-8 and strict JSON-integer quantities. The
// source-specific differences live in the callers:
//
//   - text policy: registry records are read verbatim (an empty string is a
//     format-level problem handled later by validate), while manifest text is
//     trimmed and must stay non-empty;
//   - which problem is reported first when one object has several: registry
//     records reject any duplicated field before an unknown one, manifest
//     records reject whichever anomaly appears first in source order;
//   - error types and reason wording, which remain owned by each caller.
type textPolicy int

const (
	textVerbatim textPolicy = iota // registry record: decode, keep as written
	textTrimmed                    // manifest record: trim ends, reject blank
)

// unambiguousBatchID returns the record's batch id only when it can be
// determined uniquely and reliably: exactly one "batch" member (counted after
// JSON key decoding, so an escaped respelling is the same member) holding one
// JSON string whose decoded text is valid UTF-8 and, under textTrimmed,
// non-blank after trimming. A missing, duplicated, mistyped, malformed or
// blank member yields "" — never a U+FFFD substitution and never one of two
// repeated values picked arbitrarily. The trimmed policy returns the
// normalized id; the verbatim policy returns the decoded text untouched.
func unambiguousBatchID(fields []objectField, policy textPolicy) string {
	if countField(fields, "batch") != 1 {
		return ""
	}
	raw, ok := findField(fields, "batch")
	if !ok {
		return ""
	}
	id, err := unmarshalStringStrict(raw)
	if err != nil {
		return ""
	}
	if policy == textTrimmed {
		normalized, err := NormalizeField(id)
		if err != nil {
			return ""
		}
		id = normalized
	}
	return id
}

// memberFault identifies one structural problem with an object's field names.
type memberFaultKind int

const (
	faultNone memberFaultKind = iota
	faultDuplicate
	faultUnknown
)

type memberFault struct {
	field string
	kind  memberFaultKind
}

// checkRecordMembers validates the member names of one batch record against
// batchRecordFields. With duplicatesFirst it reports the first member name
// that repeats anywhere in the object, and only reports an unknown member
// when nothing is repeated — the registry file rule. Otherwise it reports
// the first anomaly encountered in source order, be it an unknown member or
// a repeat — the manifest rule. In source order, an unknown name outranks a
// repeat on the same member, matching the original per-member scan. Repeated
// keys are detected on the decoded name, so a field hidden behind a JSON
// escape is the same member.
func checkRecordMembers(fields []objectField, duplicatesFirst bool) memberFault {
	seen := make(map[string]struct{}, len(fields))
	var firstDuplicate, firstUnknown memberFault
	for _, f := range fields {
		_, known := batchRecordFields[f.key]
		_, repeated := seen[f.key]
		seen[f.key] = struct{}{}
		if !duplicatesFirst {
			if !known {
				return memberFault{field: f.key, kind: faultUnknown}
			}
			if repeated {
				return memberFault{field: f.key, kind: faultDuplicate}
			}
			continue
		}
		if repeated && firstDuplicate.kind == faultNone {
			firstDuplicate = memberFault{field: f.key, kind: faultDuplicate}
		}
		if !known && firstUnknown.kind == faultNone {
			firstUnknown = memberFault{field: f.key, kind: faultUnknown}
		}
	}
	if duplicatesFirst {
		if firstDuplicate.kind != faultNone {
			return firstDuplicate
		}
		return firstUnknown
	}
	return memberFault{}
}

// fieldProblem classifies what is wrong with one text member's value,
// independently of the error wording each source presents.
type fieldProblem int

const (
	fieldOK fieldProblem = iota
	fieldMissing
	fieldNotString
	fieldEncoding
	fieldBlank
)

// decodeRecordText reads one text member. found says whether the member
// exists; raw is its JSON value. Under textVerbatim every syntactically and
// encoding-valid string is returned as written, including "". Under
// textTrimmed the value must additionally be non-empty after trimming
// (NormalizeField); in that case cause carries NormalizeField's error so the
// caller can keep the established reason wording.
func decodeRecordText(raw json.RawMessage, found bool, policy textPolicy) (value string, problem fieldProblem, cause error) {
	if !found {
		return "", fieldMissing, nil
	}
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '"' {
		return "", fieldNotString, nil
	}
	value, err := unmarshalStringStrict(raw)
	if errors.Is(err, errStringEncoding) {
		return "", fieldEncoding, nil
	}
	if err != nil {
		return "", fieldNotString, nil
	}
	if policy == textTrimmed {
		normalized, normErr := NormalizeField(value)
		if normErr != nil {
			// After strict decoding the only remaining failure is a blank
			// result, but keep the cause rather than re-stating its text.
			return "", fieldBlank, normErr
		}
		value = normalized
	}
	return value, fieldOK, nil
}

// quantityShape classifies a JSON quantity token without assigning source-
// specific wording. The token is the trimmed raw JSON value; the callers
// decide how each class is phrased for registry files versus manifests.
type quantityShape int

const (
	qtyValid       quantityShape = iota
	qtyNotNumeric                // value does not even start as a JSON number digit
	qtyFractional                // digits followed by '.', 'e' or another non-digit
	qtyLeadingZero               // "01": no conforming JSON document can reach this
	qtyOutOfRange                // all digits, but outside signed 64-bit range
	qtyZero
)

// classifyJSONQuantity is the shared numeric rule: a strict JSON integer
// token (no sign, fraction or exponent) in 1..MaxQuantity. It only inspects
// token shape and range — never the surrounding object — so both on-disk
// sources and every future caller maintain one quantity definition.
func classifyJSONQuantity(token []byte) (int64, quantityShape) {
	if len(token) == 0 || token[0] < '0' || token[0] > '9' {
		return 0, qtyNotNumeric
	}
	for _, c := range token {
		if c < '0' || c > '9' {
			return 0, qtyFractional
		}
	}
	if len(token) > 1 && token[0] == '0' {
		return 0, qtyLeadingZero
	}
	value, err := strconv.ParseInt(string(token), 10, 64)
	if err != nil {
		return 0, qtyOutOfRange
	}
	if value == 0 {
		return 0, qtyZero
	}
	return value, qtyValid
}

// textFieldNames lists the three textual members every record carries, in the
// fixed order in which per-field problems are reported: batch, product, unit.
// It is the single source of field names and field order for the decoded-record
// checker (decoded.go); the strict JSON readers in this file address the same
// members by name, which keeps those names stable.
var textFieldNames = [3]string{"batch", "product", "unit"}

// safeBatchIDForError returns id exactly when it may be cited in an error:
// non-empty and valid UTF-8. An empty or malformed id yields "" so no value
// and no U+FFFD substitution is ever invented to name a batch.
func safeBatchIDForError(id string) string {
	if id != "" && utf8.ValidString(id) {
		return id
	}
	return ""
}

// These wrappers keep the registry-specific reason strings next to the
// registry parser while sharing classifyJSONQuantity; the wording predates
// the shared classifier and must not change.
func registryQuantityError(token []byte) (int64, error) {
	value, shape := classifyJSONQuantity(token)
	switch shape {
	case qtyValid:
		return value, nil
	case qtyZero:
		return 0, errors.New("must be greater than zero")
	case qtyOutOfRange:
		return 0, fmt.Errorf("must be an integer no greater than %d", MaxQuantity)
	case qtyLeadingZero:
		// No conforming JSON token reaches this branch (leading zeros fail the
		// document scan); historically such a digit run was parsed on its
		// numeric value, so keep that exact behavior.
		parsed, err := strconv.ParseInt(string(token), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("must be an integer no greater than %d", MaxQuantity)
		}
		if parsed == 0 {
			return 0, errors.New("must be greater than zero")
		}
		return parsed, nil
	default: // qtyNotNumeric, qtyFractional
		return 0, fmt.Errorf("must be a JSON integer between 1 and %d", MaxQuantity)
	}
}

// manifestQuantityError mirrors the manifest wording: non-numeric values and
// signed numbers point at the accepted range, while fractions, exponents and
// (defensively) leading zeros each keep their own sentence.
func manifestQuantityError(token []byte) (int64, error) {
	value, shape := classifyJSONQuantity(token)
	switch shape {
	case qtyValid:
		return value, nil
	case qtyZero:
		return 0, errors.New(`field "quantity" must be greater than zero`)
	case qtyLeadingZero:
		return 0, errors.New(`field "quantity" must be a JSON integer without leading zeros`)
	case qtyFractional:
		return 0, errors.New(`field "quantity" must be a JSON integer without sign, fraction or exponent`)
	case qtyOutOfRange:
		return 0, fmt.Errorf(`field %q must be an integer no greater than %d`, "quantity", MaxQuantity)
	default: // qtyNotNumeric
		return 0, fmt.Errorf(`field %q must be a JSON integer between 1 and %d`, "quantity", MaxQuantity)
	}
}
