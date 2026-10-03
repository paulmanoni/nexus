package main

import (
	"fmt"
	"path"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/paulmanoni/nexus/v2/config"
)

// The "nexus.toml" migrate rule. v1 ignored keys nothing read; v2 fails boot
// on them. Most such keys are real settings written in the wrong table — a
// top-level `environment` that belongs under [runtime], an `addr` under
// [runtime] that belongs under [runtime.server] — which v1 silently dropped.
// The rule moves every key the strict check pins to one concrete table
// (config.Problem.Fix) into that table, and leaves the rest (typos,
// undeclared sections) to `nexus config check`.
//
// Only single-line `key = value` entries move; the entry keeps its text and
// trailing comment, and lands after the last entry of its target table (a
// table that doesn't exist yet is appended). A key whose target already sets
// it is left where it is — moving it would make a duplicate.

func isNexusTOML(rel string) bool { return path.Base(rel) == "nexus.toml" }

type tomlKeyMove struct {
	line   int    // 0-based line in the original source
	table  string // dotted target table
	entry  string // the entry text, trimmed
	leaf   string
	before string // the original line, for the report
}

func migrateNexusTOMLKeys(rel string, src []byte) ([]byte, []migrateChange, error) {
	problems, err := config.Check(src, rel)
	if err != nil {
		return nil, nil, err
	}
	var tree map[string]any
	if err := toml.Unmarshal(src, &tree); err != nil {
		return nil, nil, err
	}
	lines := strings.Split(string(src), "\n")
	var moves []tomlKeyMove
	targetSets := map[string]bool{} // table.leaf already claimed by a move
	for _, p := range problems {
		if len(p.Fix) < 2 || (p.Kind != "misplaced" && p.Kind != "key") || p.Line < 1 || p.Line > len(lines) {
			continue
		}
		leaf := p.Fix[len(p.Fix)-1]
		table := strings.Join(p.Fix[:len(p.Fix)-1], ".")
		raw := lines[p.Line-1]
		entry := strings.TrimSpace(raw)
		if !singleLineEntry(entry, leaf) || hasPath(tree, p.Fix) || targetSets[table+"."+leaf] {
			continue
		}
		targetSets[table+"."+leaf] = true
		moves = append(moves, tomlKeyMove{line: p.Line - 1, table: table, entry: entry, leaf: leaf, before: raw})
	}
	if len(moves) == 0 {
		return src, nil, nil
	}

	removed := map[int]bool{}
	byTable := map[string][]string{}
	var tables []string
	var changes []migrateChange
	for _, m := range moves {
		removed[m.line] = true
		if _, ok := byTable[m.table]; !ok {
			tables = append(tables, m.table)
		}
		byTable[m.table] = append(byTable[m.table], m.entry)
		changes = append(changes, migrateChange{
			Line: m.line + 1,
			Old:  strings.TrimSpace(m.before),
			New:  fmt.Sprintf("[%s] %s", m.table, m.entry),
		})
	}
	var kept []string
	for i, l := range lines {
		if !removed[i] {
			kept = append(kept, l)
		}
	}
	for _, table := range tables {
		kept = insertIntoTable(kept, table, byTable[table])
	}
	return []byte(strings.Join(kept, "\n")), changes, nil
}

// singleLineEntry reports whether entry is `leaf = value` complete on its
// own line (a multi-line array or string can't be moved line by line).
func singleLineEntry(entry, leaf string) bool {
	k, _, ok := strings.Cut(entry, "=")
	if !ok || strings.Trim(strings.TrimSpace(k), `"'`) != leaf {
		return false
	}
	var m map[string]any
	return toml.Unmarshal([]byte(entry), &m) == nil
}

func hasPath(tree map[string]any, p []string) bool {
	var cur any = tree
	for _, seg := range p {
		m, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		if cur, ok = m[seg]; !ok {
			return false
		}
	}
	return true
}

// insertIntoTable adds entries to the [table] block of lines: after its last
// entry, or as a new table at the end when the file has none.
func insertIntoTable(lines []string, table string, entries []string) []string {
	header := -1
	for i, l := range lines {
		if tableHeader(l) == table {
			header = i
			break
		}
	}
	if header < 0 {
		for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
			lines = lines[:len(lines)-1]
		}
		lines = append(lines, "", "["+table+"]")
		lines = append(lines, entries...)
		return append(lines, "")
	}
	at := header + 1
	for i := header + 1; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if strings.HasPrefix(t, "[") {
			break
		}
		if t != "" && !strings.HasPrefix(t, "#") {
			at = i + 1
		}
	}
	out := append([]string(nil), lines[:at]...)
	out = append(out, entries...)
	return append(out, lines[at:]...)
}

// tableHeader returns the dotted name of a `[a.b]` header line, "" otherwise.
func tableHeader(line string) string {
	t := strings.TrimSpace(line)
	if i := strings.Index(t, "#"); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	if !strings.HasPrefix(t, "[") || strings.HasPrefix(t, "[[") || !strings.HasSuffix(t, "]") {
		return ""
	}
	parts := strings.Split(strings.Trim(t, "[]"), ".")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return strings.Join(parts, ".")
}
