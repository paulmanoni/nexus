// Package schema is the ORM's tables as the database needs them, and the
// SQL that creates and changes them. The runtime builds it from reflect
// (orm.CreateTables, ormtest) and the generator from go/types
// (nexus makemigrations): one description, one SQL, both ways.
package schema

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Kind is what a column holds, as the database types it.
type Kind string

const (
	Bool    Kind = "bool"
	Int     Kind = "int"   // 64-bit
	Int32   Kind = "int32" // and smaller
	Float   Kind = "float"
	Float32 Kind = "float32"
	String  Kind = "string"
	Time    Kind = "time"
	Bytes   Kind = "bytes"
	Custom  Kind = "custom" // a Scanner/Valuer: TEXT unless typed
)

// Column is a table's column.
type Column struct {
	Name     string `json:"name"`
	Kind     Kind   `json:"kind"`
	Nullable bool   `json:"nullable,omitempty"`
	PK       bool   `json:"pk,omitempty"`
	Auto     bool   `json:"auto,omitempty"` // an integer key the database counts
	Unique   bool   `json:"unique,omitempty"`
	Index    bool   `json:"index,omitempty"`
	Size     int    `json:"size,omitempty"`
	Type     string `json:"type,omitempty"` // a database type of the model's own
}

// ForeignKey is a column referring to another table's key.
type ForeignKey struct {
	Column   string `json:"column"`
	Table    string `json:"table"`
	Ref      string `json:"ref"`
	OnDelete string `json:"onDelete,omitempty"` // cascade, set_null, restrict
}

// Table is a table: its columns in order, its foreign keys.
type Table struct {
	Name    string       `json:"name"`
	Columns []Column     `json:"columns"`
	FKs     []ForeignKey `json:"fks,omitempty"`
}

// Dialect is how a database writes the schema.
type Dialect struct {
	Name  string // postgres, mysql, sqlite
	Quote func(string) string
}

// Type is a column's SQL type, its key clause included.
func (d Dialect) Type(c Column) string {
	if c.Type != "" {
		return c.Type
	}
	switch c.Kind {
	case Bool:
		return "BOOLEAN"
	case Int:
		if c.Auto {
			switch d.Name {
			case "postgres":
				return "BIGSERIAL"
			case "sqlite":
				return "INTEGER"
			}
		}
		return "BIGINT"
	case Int32:
		if c.Auto && d.Name == "postgres" {
			return "SERIAL"
		}
		return "INTEGER"
	case Float:
		switch d.Name {
		case "postgres":
			return "DOUBLE PRECISION"
		case "mysql":
			return "DOUBLE"
		}
		return "REAL"
	case Float32:
		return "REAL"
	case String:
		if c.Size > 0 {
			return "VARCHAR(" + strconv.Itoa(c.Size) + ")"
		}
		if d.Name == "mysql" && (c.PK || c.Unique || c.Index) {
			return "VARCHAR(255)" // MySQL can't key TEXT
		}
		return "TEXT"
	case Time:
		switch d.Name {
		case "postgres":
			return "TIMESTAMP"
		case "mysql":
			return "DATETIME(6)"
		}
		return "DATETIME"
	case Bytes:
		switch d.Name {
		case "postgres":
			return "BYTEA"
		case "mysql":
			return "LONGBLOB"
		}
		return "BLOB"
	}
	return "TEXT"
}

// columnSQL is a column's definition in CREATE TABLE or ADD COLUMN.
func (d Dialect) columnSQL(c Column, singlePK bool) string {
	s := d.Quote(c.Name) + " " + d.Type(c)
	if c.PK && singlePK {
		s += " PRIMARY KEY"
		if c.Auto {
			switch d.Name {
			case "sqlite":
				s += " AUTOINCREMENT"
			case "mysql":
				s += " AUTO_INCREMENT"
			}
		}
		return s
	}
	if !c.Nullable {
		s += " NOT NULL"
	}
	if c.Unique {
		s += " UNIQUE"
	}
	return s
}

func (t Table) pks() []string {
	var out []string
	for _, c := range t.Columns {
		if c.PK {
			out = append(out, c.Name)
		}
	}
	return out
}

func (d Dialect) fkSQL(fk ForeignKey) string {
	s := "FOREIGN KEY (" + d.Quote(fk.Column) + ") REFERENCES " + d.Quote(fk.Table) + " (" + d.Quote(fk.Ref) + ")"
	switch fk.OnDelete {
	case "cascade":
		s += " ON DELETE CASCADE"
	case "set_null":
		s += " ON DELETE SET NULL"
	case "restrict":
		s += " ON DELETE RESTRICT"
	}
	return s
}

func (d Dialect) indexName(table, col string) string { return "idx_" + table + "_" + col }

// Create is the statements creating t: the table, then its indexes.
// ifNotExists leaves a table that is there alone.
func (d Dialect) Create(t Table, ifNotExists bool) []string {
	pks := t.pks()
	var defs []string
	for _, c := range t.Columns {
		defs = append(defs, d.columnSQL(c, len(pks) == 1))
	}
	if len(pks) > 1 {
		quoted := make([]string, len(pks))
		for i, p := range pks {
			quoted[i] = d.Quote(p)
		}
		defs = append(defs, "PRIMARY KEY ("+strings.Join(quoted, ", ")+")")
	}
	for _, fk := range t.FKs {
		defs = append(defs, d.fkSQL(fk))
	}
	head := "CREATE TABLE "
	if ifNotExists {
		head += "IF NOT EXISTS "
	}
	out := []string{head + d.Quote(t.Name) + " (\n  " + strings.Join(defs, ",\n  ") + "\n)"}
	for _, c := range t.Columns {
		if c.Index && !c.Unique && !c.PK {
			out = append(out, d.createIndex(t.Name, c.Name, ifNotExists))
		}
	}
	return out
}

func (d Dialect) createIndex(table, col string, ifNotExists bool) string {
	s := "CREATE INDEX "
	if ifNotExists && d.Name != "mysql" {
		s += "IF NOT EXISTS "
	}
	return s + d.Quote(d.indexName(table, col)) + " ON " + d.Quote(table) + " (" + d.Quote(col) + ")"
}

// Sort orders tables so each comes after the tables its foreign keys
// refer to (a cycle keeps its order).
func Sort(tables []Table) []Table {
	byName := map[string]Table{}
	for _, t := range tables {
		byName[t.Name] = t
	}
	names := make([]string, 0, len(tables))
	for _, t := range tables {
		names = append(names, t.Name)
	}
	sort.Strings(names)
	var out []Table
	state := map[string]int{} // 1 visiting, 2 done
	var visit func(string)
	visit = func(n string) {
		if state[n] != 0 {
			return
		}
		state[n] = 1
		for _, fk := range byName[n].FKs {
			if _, ok := byName[fk.Table]; ok && fk.Table != n {
				visit(fk.Table)
			}
		}
		state[n] = 2
		out = append(out, byName[n])
	}
	for _, n := range names {
		visit(n)
	}
	return out
}

// Step is one statement of a migration, and a note when it needs a
// person (a drop that loses data, a change SQLite can't make).
type Step struct {
	SQL  string
	Note string
}

// Diff is the steps from the tables old to the tables new: tables created
// and dropped, columns added, dropped and changed, indexes, foreign keys.
// A rename shows as a drop and an add: the note says so.
func (d Dialect) Diff(old, new []Table) []Step {
	var steps []Step
	oldBy := map[string]Table{}
	for _, t := range old {
		oldBy[t.Name] = t
	}
	newBy := map[string]Table{}
	for _, t := range new {
		newBy[t.Name] = t
	}
	for _, t := range Sort(new) {
		o, ok := oldBy[t.Name]
		if !ok {
			for _, s := range d.Create(t, false) {
				steps = append(steps, Step{SQL: s})
			}
			continue
		}
		steps = append(steps, d.alter(o, t)...)
	}
	sorted := Sort(old)
	slices.Reverse(sorted)
	for _, t := range sorted {
		if _, ok := newBy[t.Name]; !ok {
			steps = append(steps, Step{SQL: "DROP TABLE " + d.Quote(t.Name), Note: "drops table " + t.Name + " and its rows; if it was renamed, write the rename instead"})
		}
	}
	return steps
}

func (d Dialect) alter(o, t Table) []Step {
	var steps []Step
	tq := d.Quote(t.Name)
	oldCols := map[string]Column{}
	for _, c := range o.Columns {
		oldCols[c.Name] = c
	}
	newCols := map[string]bool{}
	for _, c := range t.Columns {
		newCols[c.Name] = true
		oc, ok := oldCols[c.Name]
		if !ok {
			def := c
			note := ""
			if !c.Nullable && !c.PK {
				// A NOT NULL column added to rows that exist needs a value.
				note = "adds NOT NULL column " + t.Name + "." + c.Name + ": give existing rows a value (a DEFAULT) if the table has any"
			}
			steps = append(steps, Step{SQL: "ALTER TABLE " + tq + " ADD COLUMN " + d.columnSQL(def, false), Note: note})
			if c.Index && !c.Unique {
				steps = append(steps, Step{SQL: d.createIndex(t.Name, c.Name, false)})
			}
			continue
		}
		if d.Type(oc) != d.Type(c) || oc.Nullable != c.Nullable {
			steps = append(steps, d.changeColumn(t.Name, oc, c)...)
		}
		if oc.Unique != c.Unique {
			if c.Unique {
				steps = append(steps, Step{SQL: "CREATE UNIQUE INDEX " + d.Quote("uniq_"+t.Name+"_"+c.Name) + " ON " + tq + " (" + d.Quote(c.Name) + ")"})
			} else {
				steps = append(steps, Step{Note: "drop the unique constraint on " + t.Name + "." + c.Name + " (its name is the database's)"})
			}
		}
		if oc.Index != c.Index && !c.Unique {
			if c.Index {
				steps = append(steps, Step{SQL: d.createIndex(t.Name, c.Name, false)})
			} else {
				steps = append(steps, Step{SQL: d.dropIndex(t.Name, c.Name)})
			}
		}
	}
	for _, c := range o.Columns {
		if !newCols[c.Name] {
			steps = append(steps, Step{SQL: "ALTER TABLE " + tq + " DROP COLUMN " + d.Quote(c.Name), Note: "drops column " + t.Name + "." + c.Name + " and its data; if it was renamed, write the rename instead"})
		}
	}
	oldFK := map[ForeignKey]bool{}
	for _, fk := range o.FKs {
		oldFK[fk] = true
	}
	for _, fk := range t.FKs {
		if oldFK[fk] {
			continue
		}
		if d.Name == "sqlite" {
			steps = append(steps, Step{Note: "SQLite can't add a foreign key to " + t.Name + "." + fk.Column + " to an existing table: rebuild the table to enforce it"})
			continue
		}
		steps = append(steps, Step{SQL: "ALTER TABLE " + tq + " ADD " + d.fkSQL(fk)})
	}
	return steps
}

func (d Dialect) dropIndex(table, col string) string {
	if d.Name == "mysql" {
		return "DROP INDEX " + d.Quote(d.indexName(table, col)) + " ON " + d.Quote(table)
	}
	return "DROP INDEX " + d.Quote(d.indexName(table, col))
}

func (d Dialect) changeColumn(table string, o, c Column) []Step {
	tq, cq := d.Quote(table), d.Quote(c.Name)
	switch d.Name {
	case "postgres":
		var steps []Step
		if d.Type(o) != d.Type(c) {
			steps = append(steps, Step{SQL: "ALTER TABLE " + tq + " ALTER COLUMN " + cq + " TYPE " + d.Type(c) + " USING " + cq + "::" + d.Type(c)})
		}
		if o.Nullable != c.Nullable {
			if c.Nullable {
				steps = append(steps, Step{SQL: "ALTER TABLE " + tq + " ALTER COLUMN " + cq + " DROP NOT NULL"})
			} else {
				steps = append(steps, Step{SQL: "ALTER TABLE " + tq + " ALTER COLUMN " + cq + " SET NOT NULL", Note: "fails while " + table + "." + c.Name + " holds NULLs"})
			}
		}
		return steps
	case "mysql":
		return []Step{{SQL: "ALTER TABLE " + tq + " MODIFY COLUMN " + d.columnSQL(c, false)}}
	}
	return []Step{{Note: fmt.Sprintf("SQLite can't change %s.%s to %s: rebuild the table", table, c.Name, d.Type(c))}}
}
