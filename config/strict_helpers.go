package config

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// isTOMLTable reports whether path names a table (or array of tables) in the
// generically-decoded document, as opposed to a single key holding a scalar
// or array value.
func isTOMLTable(tree map[string]any, path []string) bool {
	var cur any = tree
	for _, seg := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		cur, ok = m[seg]
		if !ok {
			return false
		}
	}
	switch v := cur.(type) {
	case map[string]any:
		return true
	case []any: // [[array.of.tables]]
		return len(v) > 0 && isMapSlice(v)
	}
	return false
}

// isMapSlice reports whether every element of v is a table, which is what
// distinguishes an array of tables from a plain array value.
func isMapSlice(v []any) bool {
	for _, e := range v {
		if _, ok := e.(map[string]any); !ok {
			return false
		}
	}
	return true
}

// unknownConfigKeyHint guesses what the operator meant, in the order the
// guesses are trustworthy:
//
//  1. Mis-nesting — the key IS a real setting, just in the wrong table.
//     `addr` at the top level resolves to "[runtime.server] addr", which is
//     the single most useful thing this check can say: the value looks
//     right, reads right, and does nothing.
//  2. Typo — a sibling key of the (owned) table the operator wrote into is
//     within a couple of edits, so `enabeld` resolves to `enabled`.
//  3. Neither — name what the table DOES accept. Spelling distance gives up
//     on the common case of a plausible-but-wrong word (`adress` is four
//     edits from `addr`, and `pool_size` is near nothing at all), where the
//     accepted-key list still answers the question immediately.
//
// Empty only at the document root, where the answer is (1) or nothing: the
// root's own "keys" are tables, and listing them would suggest writing a
// setting there — the very mistake being reported.
func unknownConfigKeyHint(schema *configSchemaNode, leaves map[string][]string, path []string) string {
	leaf := path[len(path)-1]
	parent := strings.Join(path[:len(path)-1], ".")

	if table, ok := bestLeafTable(leaves[leaf], parent); ok {
		return fmt.Sprintf("did you mean [%s] %s?", table, leaf)
	}
	n := schema.lookup(path[:len(path)-1])
	if n == nil || parent == "" {
		return ""
	}
	if near := nearestName(leaf, n.names()); near != "" {
		return fmt.Sprintf("did you mean [%s] %s?", parent, near)
	}
	if names := n.names(); len(names) > 0 {
		return fmt.Sprintf("[%s] accepts: %s", parent, previewNames(names, 12))
	}
	return ""
}

// previewNames joins key names for a hint, truncating past max so one
// oversized table can't swallow the report.
func previewNames(names []string, max int) string {
	if len(names) <= max {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:max], ", ") + ", …"
}

// bestLeafTable picks the table to name in a mis-nesting hint from the set of
// tables that declare a key of this name, excluding the table the operator
// already wrote into. Concrete paths beat wildcard ones (`addr` belongs to
// both [runtime.server] and [runtime.server.listeners.*]; the former is what
// an operator means nine times in ten), then shallower beats deeper.
func bestLeafTable(candidates []string, parent string) (string, bool) {
	best, found := "", false
	for _, c := range candidates {
		if c == parent || c == "" {
			continue
		}
		if !found || betterLeafTable(c, best) {
			best, found = c, true
		}
	}
	return best, found
}

// betterLeafTable orders mis-nesting candidates: [runtime] tables first, then
// concrete before wildcard,
// then shallow before deep, then lexical for a stable message.
func betterLeafTable(a, b string) bool {
	if ra, rb := isRuntimePath(a), isRuntimePath(b); ra != rb {
		return ra
	}
	if wa, wb := strings.Contains(a, "*"), strings.Contains(b, "*"); wa != wb {
		return wb
	}
	if da, db := strings.Count(a, "."), strings.Count(b, "."); da != db {
		return da < db
	}
	return a < b
}

// configSchemaNode is the shape of the TOML schema runtimeConfigDoc declares,
// derived once by reflection over the struct tags. It exists so the hints
// can't drift from the structs: adding a field to ServerConfigBlock makes it
// hintable with no second list to maintain.
//
// A node with no fields and no wildcard is a leaf (a scalar or array value).
type configSchemaNode struct {
	fields map[string]*configSchemaNode
	// wild is the element schema of a map-typed table
	// (map[string]ListenerConfigBlock → any [runtime.server.listeners.X]).
	wild *configSchemaNode
}

// isLeaf reports whether the node holds a value rather than a table.
func (n *configSchemaNode) isLeaf() bool { return len(n.fields) == 0 && n.wild == nil }

// names returns the node's declared key names, sorted for deterministic hints.
func (n *configSchemaNode) names() []string {
	out := make([]string, 0, len(n.fields))
	for name := range n.fields {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// lookup walks path from this node, following the wildcard for a table key
// that isn't a declared field (a listener name). Returns nil when the path
// leaves the schema.
func (n *configSchemaNode) lookup(path []string) *configSchemaNode {
	cur := n
	for _, seg := range path {
		switch {
		case cur == nil:
			return nil
		case cur.fields[seg] != nil:
			cur = cur.fields[seg]
		case cur.wild != nil:
			cur = cur.wild
		default:
			return nil
		}
	}
	return cur
}

// schemaLeaves indexes a schema: leaf key name → the dotted paths of every
// table declaring a key of that name. It is what makes the mis-nesting hint
// possible: "addr" → ["runtime.server", "runtime.server.listeners.*"].
func schemaLeaves(n *configSchemaNode) map[string][]string {
	out := map[string][]string{}
	indexConfigLeaves(n, "", out)
	for _, paths := range out {
		sort.Strings(paths)
	}
	return out
}

// buildConfigSchema reflects a TOML-tagged type into a configSchemaNode.
// Structs become tables, maps become wildcard tables, everything else a leaf.
func buildConfigSchema(t reflect.Type) *configSchemaNode {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch {
	case t == nil:
		return &configSchemaNode{}
	case t.Kind() == reflect.Struct:
		n := &configSchemaNode{fields: make(map[string]*configSchemaNode, t.NumField())}
		for i := range t.NumField() {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			if name := tomlFieldName(f); name != "" {
				n.fields[name] = buildConfigSchema(f.Type)
			}
		}
		return n
	case t.Kind() == reflect.Map:
		return &configSchemaNode{wild: buildConfigSchema(t.Elem())}
	}
	return &configSchemaNode{}
}

// tomlFieldName is the key a struct field decodes from: its `toml` tag name,
// or the lowercased field name when untagged (go-toml's own default).
// Returns "" for a field that never appears in a document (`toml:"-"`).
func tomlFieldName(f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("toml"), ",")
	switch name {
	case "-":
		return ""
	case "":
		return strings.ToLower(f.Name)
	}
	return name
}

// indexConfigLeaves walks the schema recording, for each leaf key name, the
// dotted path of the table that declares it. Wildcard tables contribute a
// "*" segment so a hint can say [runtime.server.listeners.*].
func indexConfigLeaves(n *configSchemaNode, prefix string, out map[string][]string) {
	for name, child := range n.fields {
		if child.isLeaf() {
			out[name] = append(out[name], prefix)
			continue
		}
		indexConfigLeaves(child, joinConfigPath(prefix, name), out)
	}
	if n.wild != nil {
		indexConfigLeaves(n.wild, joinConfigPath(prefix, "*"), out)
	}
}

// joinConfigPath appends one segment to a dotted TOML path, handling the
// empty (document root) prefix.
func joinConfigPath(prefix, seg string) string {
	if prefix == "" {
		return seg
	}
	return prefix + "." + seg
}

// nearestName returns the candidate within a small edit distance of name, or
// "" when nothing is close enough to claim. The budget scales with length so
// short keys ("csp", "rpm") don't collect wrong suggestions: one edit up to 5
// characters, two beyond. Ties go to the lexically first candidate so the
// message is deterministic.
func nearestName(name string, candidates []string) string {
	budget := 1
	if len(name) > 5 {
		budget = 2
	}
	best, bestDist := "", budget+1
	for _, c := range candidates {
		if d := editDistance(name, c); d < bestDist {
			best, bestDist = c, d
		}
	}
	if bestDist > budget {
		return ""
	}
	return best
}

// editDistance is the Levenshtein distance between a and b, computed with a
// single rolling row. Inputs here are TOML key names (a few dozen bytes at
// most) compared a handful of times per boot, so the naive algorithm is
// well inside the noise.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, min(cur[j-1]+1, prev[j-1]+cost))
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

func isRuntimePath(p string) bool { return p == "runtime" || strings.HasPrefix(p, "runtime.") }
