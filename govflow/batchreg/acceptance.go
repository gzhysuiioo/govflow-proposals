package batchreg

// This file is the single definition of how ONE record that has already
// passed its entry point's input validation is accepted into a batch table:
//
//   - a batch id the table does not hold yet creates a record, appended after
//     the records already there;
//   - an id the table already holds, with product, quantity and unit all equal
//     to the submitted values, confirms that EXISTING record as a duplicate —
//     the stored values are echoed back and the submitted values can never
//     replace them;
//   - an id held by a record differing in any of product, quantity or unit is
//     rejected, and every differing member is listed in that fixed order.
//
// Both public entry points (Register for one record and Import for a whole
// manifest) run this exact rule through batchTable, so the create / duplicate
// / conflict decision exists in one place only. What stays entry-point
// specific, by design, is everything around the decision: input validation
// and its error precedence (Register and Import deliberately order text and
// quantity problems differently), the trimming boundary (the command line and
// ParseManifest trim text before constructing an Input; records a Go caller
// submits directly keep their text verbatim), the refusal of a registry that
// already stores two records under one id, the all-or-nothing bookkeeping for
// a manifest, and the wording and concrete error type each entry point
// returns. Ids always compare byte for byte: casing and interior whitespace
// matter, and neither entry point folds them here.

// differingFields lists the members among product, quantity and unit in which
// in differs from existing, in that fixed order. It returns nil when the two
// describe the same batch content. Comparison is byte for byte on the text
// fields, so no trimming, case folding or whitespace collapsing happens here;
// any normalization is the caller's job before it builds the Input.
func differingFields(existing Batch, in Input) []string {
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

// batchTable is the append-only working set a record is accepted into. It is
// built from the stored records (copied, so a rejected acceptance can never
// touch the caller's registry) and grows while a submission is processed.
// index locates the record currently held under every id — stored records and
// records introduced during this run alike — and firstPos additionally
// remembers, for ids introduced during this run, the 1-based position of
// their first occurrence in the submitted sequence. firstPos is what keeps a
// later conflict attributable to the id's FIRST introduction: an intermediate
// identical repeat confirms the record but never becomes its own source. Ids
// present in the stored table from the start are deliberately absent from
// firstPos, so a conflict with them keeps reporting the registry as the
// source no matter how many identical confirmations follow.
type batchTable struct {
	records  []Batch
	index    map[string]int
	firstPos map[string]int
}

// newBatchTable copies stored into a fresh working table. The caller must have
// established that stored ids are unique (Register and Import reject an
// ambiguous registry before accepting anything); the map would otherwise hide
// one of two records under a shared id.
func newBatchTable(stored []Batch) *batchTable {
	table := &batchTable{
		records:  append([]Batch(nil), stored...),
		index:    make(map[string]int, len(stored)),
		firstPos: make(map[string]int),
	}
	for i, b := range table.records {
		table.index[b.Batch] = i
	}
	return table
}

// accept applies the shared acceptance rule to one validated record.
// sourcePos is the record's 1-based position in the submitted sequence and is
// remembered for an id newly introduced here; an entry point accepting a
// single record with no sequence positions passes 0.
//
// A new id appends the submitted record and returns it with created == true.
// A known id with identical content returns the record ALREADY HELD (never
// the submitted copy) with created == false, so a duplicate cannot overwrite
// stored text. A known id with different content returns an empty record,
// created == false and every differing member in product, quantity, unit
// order; the table is left exactly as it was before the call — the rejected
// record is neither appended nor confirmed.
func (t *batchTable) accept(in Input, sourcePos int) (record Batch, created bool, diffs []string) {
	if idx, known := t.index[in.Batch]; known {
		existing := t.records[idx]
		if diffs := differingFields(existing, in); len(diffs) > 0 {
			return Batch{}, false, diffs
		}
		return existing, false, nil
	}
	b := Batch{Batch: in.Batch, Product: in.Product, Quantity: in.Quantity, Unit: in.Unit}
	t.records = append(t.records, b)
	t.index[in.Batch] = len(t.records) - 1
	t.firstPos[in.Batch] = sourcePos
	return b, true, nil
}

// conflictSource classifies where the table got a known conflicting id:
// inManifest == true with firstPos set when the id was first introduced by
// this submission at that 1-based position; inManifest == false when the id
// belongs to the stored table the run started with.
func (t *batchTable) conflictSource(id string) (inManifest bool, firstPos int) {
	pos, introduced := t.firstPos[id]
	return introduced, pos
}
