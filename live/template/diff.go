package template

import "strconv"

// Diff is the wire-format patch transforming a previously-sent
// Rendered into a new one. It is a sparse representation keyed by
// dynamic slot index (as a string, for JSON-object compatibility).
// Values are one of:
//
//   - string: the new value of a leaf dynamic
//   - Rendered: a full Rendered replacing whatever was in the slot
//     before (used when the prior dynamic was nil/string/Comprehension,
//     i.e. statics don't line up to sparse-diff)
//   - Diff: a nested sparse patch for a Rendered slot whose statics
//     match the prior render
//   - CompDiff: a sparse patch for a Comprehension slot
//   - Comprehension: a full Comprehension replacing the prior slot
//
// A nil Diff (or empty map) means the two Rendereds are identical
// and the caller should not ship anything.
//
// Wire-format discrimination on the client: a value with an "s" key
// is a full Rendered/Comprehension (replace); a value with an "o"
// key is a CompDiff (sparse comp update); a value whose keys are all
// numeric strings is a Diff (sparse merge); a bare string is a leaf.
type Diff map[string]any

// CompDiff is the patch for a Comprehension slot. Order is the
// authoritative new sequence of row keys — the client mirrors it,
// which implicitly handles removals (any key in prev but absent from
// Order is dropped) and reorders. Updates carries sparse per-key
// patches for rows that survived; Inserts carries full dynamics for
// rows that didn't exist before.
type CompDiff struct {
	Order   []string         `json:"o"`
	Updates map[string]Diff  `json:"u,omitempty"`
	Inserts map[string][]any `json:"i,omitempty"`
}

// DiffRendered computes the minimal patch transforming prev into next.
// Returns nil if the two are equal. Panics if their statics differ:
// that indicates the caller is diffing two structurally different
// renders, which is a bug — the diff protocol assumes the client
// already holds the statics from the prior frame.
func DiffRendered(prev, next Rendered) Diff {
	if !staticsEqual(prev.S, next.S) {
		panic("template: DiffRendered called with mismatched statics; comparing different components")
	}
	if len(prev.D) != len(next.D) {
		panic("template: DiffRendered called with mismatched dynamic counts; this is a codegen bug")
	}
	var d Diff
	for i := range prev.D {
		patch := diffDynamic(prev.D[i], next.D[i])
		if patch == nil {
			continue
		}
		if d == nil {
			d = make(Diff)
		}
		d[strconv.Itoa(i)] = patch
	}
	return d
}

// diffDynamic returns the patch for a single dynamic slot, or nil if
// the slot is unchanged. The shape of the returned value depends on
// the prev/next combination:
//
//   - equal → nil
//   - prev and next both Rendered with matching statics → nested Diff
//   - prev and next both Comprehension with matching statics → CompDiff
//   - otherwise → the next value verbatim (full replacement)
func diffDynamic(prev, next any) any {
	if dynamicEqual(prev, next) {
		return nil
	}
	switch n := next.(type) {
	case nil:
		// Slot went from something to nothing. Ship empty string so
		// the client clears the slot deterministically. (The
		// alternative — ship JSON null — works too, but empty-string
		// keeps the client merge logic uniform with leaf updates.)
		return ""
	case string:
		return n
	case Rendered:
		if p, ok := prev.(Rendered); ok && staticsEqual(p.S, n.S) {
			// Same template fragment, just new dynamics — sparse diff.
			return DiffRendered(p, n)
		}
		// Different fragment (or prev was nil/string/Comp) — ship
		// the full Rendered so the client has statics for it.
		return n
	case Comprehension:
		if p, ok := prev.(Comprehension); ok && staticsEqual(p.S, n.S) {
			if cd := diffComprehension(p, n); cd != nil {
				return *cd
			}
			return nil
		}
		return n
	}
	// Codegen invariant: dynamics are always one of the four cases
	// above. If something else slipped in, defer to the equality
	// check above (treats as different) and ship verbatim.
	return next
}

// diffComprehension produces the row-keyed patch for a Comprehension
// pair. Returns nil if the two are identical in order, key set, and
// per-row dynamics (saves emitting an empty CompDiff).
func diffComprehension(prev, next Comprehension) *CompDiff {
	prevByKey := make(map[string]Row, len(prev.Rows))
	for _, r := range prev.Rows {
		prevByKey[r.Key] = r
	}

	order := make([]string, len(next.Rows))
	var updates map[string]Diff
	var inserts map[string][]any

	for i, n := range next.Rows {
		order[i] = n.Key
		p, existed := prevByKey[n.Key]
		if !existed {
			if inserts == nil {
				inserts = make(map[string][]any)
			}
			inserts[n.Key] = n.D
			continue
		}
		if rowDiff := diffRowDynamics(p.D, n.D); rowDiff != nil {
			if updates == nil {
				updates = make(map[string]Diff)
			}
			updates[n.Key] = rowDiff
		}
	}

	// Detect no-op: same length, same keys in same positions, no
	// updates, no inserts. Caller can skip emitting the slot.
	if updates == nil && inserts == nil && len(prev.Rows) == len(next.Rows) {
		same := true
		for i := range order {
			if prev.Rows[i].Key != order[i] {
				same = false
				break
			}
		}
		if same {
			return nil
		}
	}

	return &CompDiff{Order: order, Updates: updates, Inserts: inserts}
}

// diffRowDynamics diffs the dynamics of a single Comprehension row.
// Mirrors DiffRendered's body but works on bare []any slices because
// rows share their parent Comprehension's statics. Returns nil when
// nothing changed.
func diffRowDynamics(prev, next []any) Diff {
	if len(prev) != len(next) {
		// Shape mismatch shouldn't happen — the loop body has a
		// fixed dynamic-slot count at compile time. Emit a full
		// replacement so the client at least gets correct output.
		d := make(Diff, len(next))
		for i, v := range next {
			d[strconv.Itoa(i)] = v
		}
		return d
	}
	var d Diff
	for i := range prev {
		patch := diffDynamic(prev[i], next[i])
		if patch == nil {
			continue
		}
		if d == nil {
			d = make(Diff)
		}
		d[strconv.Itoa(i)] = patch
	}
	return d
}
