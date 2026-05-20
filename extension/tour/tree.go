package tour

import "sort"

// buildTree turns a flat slice of Step rows (as the DB returns
// them) into the nested tree the runner walks. Roots (ParentStepID
// nil) become the top-level slice; every other row attaches under
// its parent's Children. Stable on Order within a parent.
//
// O(n) — one pass to index by ID, one pass to attach. Tolerates a
// dangling ParentStepID (orphan row) by skipping it; the
// alternative — error out — would block reads on a row that's
// recoverable via a cleanup script.
func buildTree(flat []*Step) []*Step {
	if len(flat) == 0 {
		return nil
	}
	byID := make(map[string]*Step, len(flat))
	for _, s := range flat {
		// Reset Children so a re-call on the same slice doesn't
		// double-attach (defensive — happens when callers re-use
		// the result of a previous buildTree).
		s.Children = nil
		byID[s.ID] = s
	}

	var roots []*Step
	for _, s := range flat {
		if s.ParentStepID == nil || *s.ParentStepID == "" {
			roots = append(roots, s)
			continue
		}
		parent, ok := byID[*s.ParentStepID]
		if !ok {
			// Orphan — parent missing. Skip rather than crash;
			// the row is fixable via the dashboard or a SQL
			// cleanup. Logging is the caller's job (we don't
			// own a logger here).
			continue
		}
		parent.Children = append(parent.Children, s)
	}

	// Stable sort each parent's children by Order so the runner
	// walks them deterministically. Roots first, then recurse.
	sort.SliceStable(roots, func(i, j int) bool { return roots[i].Order < roots[j].Order })
	for _, r := range roots {
		sortChildren(r)
	}
	return roots
}

// sortChildren recursively orders a subtree. Separate from
// buildTree so the recursion is obvious; callers reading the
// tree can rely on every level being Order-sorted.
func sortChildren(s *Step) {
	if len(s.Children) == 0 {
		return
	}
	sort.SliceStable(s.Children, func(i, j int) bool {
		return s.Children[i].Order < s.Children[j].Order
	})
	for _, c := range s.Children {
		sortChildren(c)
	}
}

// flattenTree is the inverse — given a hydrated tree (one or many
// roots), produce the flat slice the store can persist. ParentStepID
// is rewritten on each row so a tree that's been re-rooted in the
// editor still saves correctly. Order is preserved from the input.
//
// The function does NOT generate IDs; callers must populate
// Step.ID before calling (the handler layer mints UUIDs for new
// steps).
func flattenTree(tourID string, roots []*Step) []*Step {
	if len(roots) == 0 {
		return nil
	}
	out := make([]*Step, 0, len(roots))
	var walk func(parent *string, nodes []*Step)
	walk = func(parent *string, nodes []*Step) {
		for i, n := range nodes {
			n.TourID = tourID
			// Copy the parent pointer so the caller can't
			// mutate our internal value by holding the slice.
			if parent != nil {
				p := *parent
				n.ParentStepID = &p
			} else {
				n.ParentStepID = nil
			}
			// Re-number Order from the input position. The
			// editor reorders by sending a new array; we don't
			// trust the .Order field across an edit because
			// drag-drop UIs typically don't update it.
			n.Order = i
			out = append(out, n)
			if len(n.Children) > 0 {
				walk(&n.ID, n.Children)
			}
		}
	}
	walk(nil, roots)
	return out
}
