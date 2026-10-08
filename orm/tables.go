package orm

import (
	"cmp"
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/paulmanoni/nexus/orm/internal/schema"
)

// Model is a model's manager, as CreateTables and the migrations take it.
type Model interface {
	tables(d Dialect) ([]schema.Table, error)
	conn(ctx context.Context) (conn, error)
}

// CreateTables creates the models' tables, and the tables between their
// many-to-many relations, where they are not already: for tests and
// tools. Foreign keys are made between the tables it makes only. A
// deployed schema changes through migrations (orm.Migrate).
func CreateTables(ctx context.Context, models ...Model) error {
	if len(models) == 0 {
		return nil
	}
	c, err := models[0].conn(ctx)
	if err != nil {
		return err
	}
	if c.d.Name() == "postgres" {
		for _, e := range declaredExtensions() {
			if _, err := c.exec(ctx, createExtension(c.d, e.name), nil); err != nil {
				return err
			}
		}
	}
	var all []schema.Table
	seen := map[string]bool{}
	for _, m := range models {
		ts, err := m.tables(c.d)
		if err != nil {
			return err
		}
		for _, t := range ts {
			if !seen[t.Name] {
				seen[t.Name] = true
				all = append(all, t)
			}
		}
	}
	// A foreign key to a table the call doesn't make is left out: it may
	// not exist, and these tables are for tests and tools.
	for i := range all {
		fks := all[i].FKs[:0]
		for _, fk := range all[i].FKs {
			if seen[fk.Table] {
				fks = append(fks, fk)
			}
		}
		all[i].FKs = fks
	}
	d := schema.Dialect{Name: c.d.Name(), Quote: c.d.Quote}
	for _, t := range schema.Sort(all) {
		for _, s := range d.Create(t, true) {
			if _, err := c.exec(ctx, s, nil); err != nil {
				return fmt.Errorf("orm: creating %s: %w", t.Name, err)
			}
		}
	}
	return nil
}

func (m *Manager[T]) tables(d Dialect) ([]schema.Table, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.meta.tables(d)
}

// tables is the model's table on dialect d, its indexes and generated
// columns written for it, and the tables between its many-to-many
// relations.
func (m *model) tables(d Dialect) ([]schema.Table, error) {
	t := schema.Table{Name: m.Table}
	b := newBuilder(d, m)
	b.ddl = true
	for _, f := range m.Fields {
		c := columnOf(f)
		if f != m.PK {
			c.Auto = false
		}
		switch {
		case f.Type == vectorType && d.Name() == "mysql":
			return nil, fmt.Errorf("orm: %s.%s: MySQL has no vector type nexus can search (its VECTOR distances are HeatWave's only)", m.Name, f.Name)
		case f.Type == vectorType && d.Name() == "postgres":
			c.Type = "vector"
			if f.Dims > 0 {
				c.Type += "(" + strconv.Itoa(f.Dims) + ")"
			}
		case f.Type == tsvectorType && d.Name() == "postgres":
			c.Type = "tsvector"
		}
		if f.Gen != nil {
			s, err := f.Gen.exprSQL(b)
			if err != nil {
				return nil, fmt.Errorf("orm: %s.%s: %w", m.Name, f.Name, err)
			}
			c.Generated, c.Nullable = s, true
		}
		t.Columns = append(t.Columns, c)
	}
	if len(*b.joins) > 0 || len(b.args()) > 0 {
		return nil, fmt.Errorf("orm: %s: a generated column reads the row's own columns, and takes no arguments", m.Name)
	}
	for n, ix := range m.indexes {
		si, err := m.index(d, ix, n)
		if err != nil {
			return nil, err
		}
		if si.SQL != "" {
			t.Indexes = append(t.Indexes, si)
		}
	}
	out := []schema.Table{t}
	for _, r := range m.relList {
		if r.Kind != relFK && r.Kind != relM2M {
			continue
		}
		target, rf, err := r.ends()
		if err != nil {
			return nil, err
		}
		switch r.Kind {
		case relFK:
			out[0].FKs = append(out[0].FKs, schema.ForeignKey{Column: r.local.Column, Table: target.Table, Ref: rf.Column, OnDelete: r.OnDelete})
		case relM2M:
			local, remote := columnOf(r.local), columnOf(rf)
			local.Name, remote.Name = r.ThroughLocal, r.ThroughRemote
			local.Auto, remote.Auto = false, false
			local.Unique, remote.Unique = false, false
			out = append(out, schema.Table{
				Name:    r.Through,
				Columns: []schema.Column{local, remote},
				FKs: []schema.ForeignKey{
					{Column: r.ThroughLocal, Table: m.Table, Ref: r.local.Column, OnDelete: "cascade"},
					{Column: r.ThroughRemote, Table: target.Table, Ref: rf.Column, OnDelete: "cascade"},
				},
			})
		}
	}
	return out, nil
}

// Index is an index a model declares in its Indexes() method, Django's
// Meta.indexes, made and dropped by migrations. It is over fields, named
// as queries name them (their columns under the schema's names), or
// expressions:
//
//	func (Post) Indexes() []orm.Index {
//		return []orm.Index{
//			orm.GinIndex(orm.SearchVector("title", "body").Config("english")).Name("posts_search"),
//			orm.GinIndex("title").Trigram(),
//			orm.HnswIndex("embedding").Ops(orm.Cosine).M(16).EfConstruction(64),
//			orm.FullTextIndex("title", "body"),
//		}
//	}
//
// MySQL makes a FullTextIndex only, and a model declaring another kind
// can't map onto it; SQLite makes none of them, its searches unindexed.
type Index struct {
	kind         string
	on           []any // field names and Exprs
	name, config string
	ops          Metric
	trigram      bool
	m, ef, lists int
}

// GinIndex is a Postgres GIN index of fields or expressions: a
// SearchVector, or text fields with Trigram.
func GinIndex(on ...any) Index { return Index{kind: "gin", on: on} }

// GistIndex is a Postgres GiST index of fields or expressions.
func GistIndex(on ...any) Index { return Index{kind: "gist", on: on} }

// FullTextIndex is the fields' full-text index: MySQL's FULLTEXT, which
// MATCH needs, and on Postgres a GIN index of their tsvector (by Config,
// simple when not given), which the __search lookup of a field indexed
// alone uses.
func FullTextIndex(fields ...string) Index {
	ix := Index{kind: "fulltext"}
	for _, f := range fields {
		ix.on = append(ix.on, f)
	}
	return ix
}

// HnswIndex is pgvector's HNSW index of a vector field.
func HnswIndex(field string) Index { return Index{kind: "hnsw", on: []any{field}} }

// IvfflatIndex is pgvector's IVFFlat index of a vector field.
func IvfflatIndex(field string) Index { return Index{kind: "ivfflat", on: []any{field}} }

// Name names the index; <table>_<fields>_<kind> otherwise.
func (i Index) Name(s string) Index { i.name = s; return i }

// Ops is the metric a vector index serves (L2 when not given): only
// queries by that metric's distance use it.
func (i Index) Ops(m Metric) Index { i.ops = m; return i }

// M is an HNSW index's connections per layer; EfConstruction its
// candidate list while building; Lists an IVFFlat index's lists.
func (i Index) M(n int) Index              { i.m = n; return i }
func (i Index) EfConstruction(n int) Index { i.ef = n; return i }
func (i Index) Lists(n int) Index          { i.lists = n; return i }

// Trigram indexes text fields by pg_trgm's trigrams (gin_trgm_ops,
// gist_trgm_ops), for trigram_similar, Similarity and LIKE.
func (i Index) Trigram() Index { i.trigram = true; return i }

// Config is a FullTextIndex's text search configuration on Postgres.
func (i Index) Config(c string) Index { i.config = c; return i }

// index is the model's n-th declared index on dialect d: none on SQLite.
func (m *model) index(d Dialect, ix Index, n int) (schema.Index, error) {
	b := newBuilder(d, m)
	b.ddl = true
	var fields, names []string
	for _, o := range ix.on {
		if f, ok := o.(string); ok {
			fields, names = append(fields, f), append(names, strings.ReplaceAll(f, "__", "_"))
		}
	}
	on := ix.on
	if ix.kind == "fulltext" && d.Name() == "postgres" {
		on = []any{SearchVector(fields...).Config(cmp.Or(ix.config, "simple"))}
	}
	var parts []string
	for _, o := range on {
		var s string
		var err error
		switch v := o.(type) {
		case string:
			s, err = b.ref(v)
		case Expr:
			s, err = v.exprSQL(b)
			s = "(" + s + ")"
		default:
			err = fmt.Errorf("an index is of field names and Exprs, not %T", o)
		}
		if err != nil {
			return schema.Index{}, fmt.Errorf("orm: %s's index: %w", m.Name, err)
		}
		switch {
		case ix.trigram:
			s += " " + ix.kind + "_trgm_ops"
		case ix.kind == "hnsw" || ix.kind == "ivfflat":
			s += " vector_" + [...]string{L2: "l2", Cosine: "cosine", IP: "ip"}[max(ix.ops, L2)] + "_ops"
		}
		parts = append(parts, s)
	}
	if len(*b.joins) > 0 || len(b.args()) > 0 {
		return schema.Index{}, fmt.Errorf("orm: %s's index: an index is of the table's own columns, and takes no arguments", m.Name)
	}
	if len(names) == 0 {
		names = []string{strconv.Itoa(n)}
	}
	si := schema.Index{Name: cmp.Or(ix.name, m.Table+"_"+strings.Join(names, "_")+"_"+ix.kind)}
	q := d.Quote
	switch {
	case d.Name() == "sqlite":
		return schema.Index{}, nil
	case d.Name() == "mysql" && ix.kind != "fulltext":
		return schema.Index{}, fmt.Errorf("orm: %s: MySQL has no %s index", m.Name, ix.kind)
	case d.Name() == "mysql":
		si.SQL = "CREATE FULLTEXT INDEX " + q(si.Name) + " ON " + q(m.Table) + " (" + strings.Join(parts, ", ") + ")"
		return si, nil
	}
	using := cmp.Or(map[string]string{"fulltext": "gin"}[ix.kind], ix.kind)
	si.SQL = "CREATE INDEX " + q(si.Name) + " ON " + q(m.Table) + " USING " + using + " (" + strings.Join(parts, ", ") + ")"
	var with []string
	for k, v := range map[string]int{"m": ix.m, "ef_construction": ix.ef, "lists": ix.lists} {
		if v > 0 {
			with = append(with, k+" = "+strconv.Itoa(v))
		}
	}
	if len(with) > 0 {
		slices.Sort(with)
		si.SQL += " WITH (" + strings.Join(with, ", ") + ")"
	}
	return si, nil
}

// extensions is the Postgres extensions the model's columns and indexes
// need.
func (m *model) extensions() []string {
	var out []string
	need := func(e string) {
		if !slices.Contains(out, e) {
			out = append(out, e)
		}
	}
	for _, f := range m.Fields {
		if f.Type == vectorType {
			need("vector")
		}
	}
	for _, ix := range m.indexes {
		switch {
		case ix.kind == "hnsw" || ix.kind == "ivfflat":
			need("vector")
		case ix.trigram:
			need("pg_trgm")
		}
	}
	return out
}

// columnOf is a field as a column: its kind from its Go type, NULL when
// it is a pointer.
func columnOf(f *field) schema.Column {
	t := f.Type
	c := schema.Column{Name: f.Column, PK: f.PK, Unique: f.Unique, Index: f.Indexed, Size: f.Size, Type: f.SQLType}
	if t.Kind() == reflect.Pointer {
		c.Nullable, t = true, t.Elem()
	}
	c.Kind = kindOf(t)
	// A nil []byte or Vector is NULL, as the driver sends it.
	if c.Kind == schema.Bytes || t == vectorType {
		c.Nullable = true
	}
	if c.PK {
		c.Nullable = false
		c.Auto = c.Kind == schema.Int || c.Kind == schema.Int32
	}
	return c
}

var (
	scannerT = reflect.TypeFor[sql.Scanner]()
	valuerT  = reflect.TypeFor[driver.Valuer]()
)

func kindOf(t reflect.Type) schema.Kind {
	switch {
	case t == timeType:
		return schema.Time
	case reflect.PointerTo(t).Implements(scannerT) || t.Implements(valuerT):
		return schema.Custom
	}
	switch t.Kind() {
	case reflect.Bool:
		return schema.Bool
	case reflect.Int, reflect.Int64, reflect.Uint, reflect.Uint64, reflect.Uint32:
		return schema.Int
	case reflect.Int8, reflect.Int16, reflect.Int32, reflect.Uint8, reflect.Uint16:
		return schema.Int32
	case reflect.Float64:
		return schema.Float
	case reflect.Float32:
		return schema.Float32
	case reflect.String:
		return schema.String
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return schema.Bytes
		}
	}
	return schema.Custom
}
