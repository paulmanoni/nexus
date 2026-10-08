package orm

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iter"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/paulmanoni/nexus/v2"
)

// QuerySet is a lazy, immutable query of a model: each method returns a
// new QuerySet, and nothing runs until a terminal (All, First, Get,
// Count, Exists, Iter, Delete, Update, Aggregate). Keep one in a package
// variable and share it freely.
type QuerySet[T any] struct {
	m *Manager[T]
	q query
}

func (qs QuerySet[T]) with(change func(*query)) QuerySet[T] {
	qs.q = qs.q.clone()
	change(&qs.q)
	return qs
}

func (qs QuerySet[T]) prefetchQuery() query { return qs.q }

// Annotate adds a computed column, named: Filter, OrderBy and Values use
// it by name, and a field of the model tagged orm:"computed" with the
// same name receives it.
//
//	Users.Annotate("joined", orm.Year.Of(orm.F("created_at"))).Filter(orm.Q{"joined__gte": 2025})
func (qs QuerySet[T]) Annotate(name string, e Expr) QuerySet[T] {
	return qs.with(func(q *query) { q.ann = append(q.ann, annotation{name, e}) })
}

// Filter keeps the rows matching every condition. Keys may follow
// relations: author__name (a join), posts__title__icontains (a related
// row exists).
func (qs QuerySet[T]) Filter(conds ...Cond) QuerySet[T] {
	return qs.with(func(q *query) { q.where = append(q.where, conds...) })
}

// Exclude drops the rows matching all the conditions.
func (qs QuerySet[T]) Exclude(conds ...Cond) QuerySet[T] {
	return qs.with(func(q *query) { q.where = append(q.where, Not(And(conds...))) })
}

// OrderBy sorts by fields, a leading - for descending: OrderBy("-age",
// "author__name").
func (qs QuerySet[T]) OrderBy(fields ...string) QuerySet[T] {
	return qs.with(func(q *query) { q.order = slices.Clone(fields) })
}

// Limit keeps at most n rows.
func (qs QuerySet[T]) Limit(n int) QuerySet[T] { return qs.with(func(q *query) { q.limit = n }) }

// Offset skips the first n rows.
func (qs QuerySet[T]) Offset(n int) QuerySet[T] { return qs.with(func(q *query) { q.offset = n }) }

// Distinct drops duplicate rows.
func (qs QuerySet[T]) Distinct() QuerySet[T] { return qs.with(func(q *query) { q.distinct = true }) }

// SelectRelated reads foreign keys in the same query, one LEFT JOIN each:
// SelectRelated("author", "author__profile"). A missing related row
// leaves the field nil (or zero).
func (qs QuerySet[T]) SelectRelated(paths ...string) QuerySet[T] {
	return qs.with(func(q *query) { q.related = append(q.related, paths...) })
}

// PrefetchRelated loads relations of any kind after the rows, one query
// each (per thousand keys): names, paths (posts__comments) or Prefetch
// with a query of your own. Only All, First and Get load them; Iter
// streams rows without.
func (qs QuerySet[T]) PrefetchRelated(specs ...any) QuerySet[T] {
	return qs.with(func(q *query) { q.prefetch = appendSpecs(q.prefetch, specs) })
}

func appendSpecs(to []PrefetchSpec, specs []any) []PrefetchSpec {
	for _, s := range specs {
		switch v := s.(type) {
		case string:
			to = append(to, PrefetchSpec{path: v})
		case PrefetchSpec:
			to = append(to, v)
		default:
			to = append(to, PrefetchSpec{path: fmt.Sprintf("%v", s), qs: badPrefetch{s}})
		}
	}
	return to
}

type badPrefetch struct{ v any }

func (b badPrefetch) prefetchQuery() query { return query{} }

// Iter runs the query and yields its rows one at a time, without holding
// them all; relations PrefetchRelated names are not loaded.
func (qs QuerySet[T]) Iter(ctx context.Context) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var zero T
		c, err := qs.m.conn(ctx)
		if err != nil {
			yield(zero, err)
			return
		}
		q := qs.q
		if mk, ok := rowScanner[T](q.m); ok && len(q.computedFields()) == 0 && len(q.related) == 0 {
			b := q.builder(c.d)
			cols, err := q.columns(b, nil)
			if err != nil {
				yield(zero, err)
				return
			}
			s, err := q.selectSQL(b, cols)
			if err != nil {
				yield(zero, err)
				return
			}
			rows, err := c.query(ctx, s, b.args())
			if err != nil {
				yield(zero, err)
				return
			}
			defer rows.Close()
			sc := mk()
			dest := sc.Dest()
			for rows.Next() {
				var row T
				sc.Bind(&row)
				if err := rows.Scan(dest...); err != nil {
					yield(zero, err)
					return
				}
				if !yield(row, nil) {
					return
				}
			}
			if err := rows.Err(); err != nil {
				yield(zero, err)
			}
			return
		}
		stopped := false
		err = q.scan(ctx, c, func() reflect.Value { return reflect.New(q.m.Type).Elem() }, func(v reflect.Value) bool {
			if !yield(v.Interface().(T), nil) {
				stopped = true
				return false
			}
			return true
		})
		if err != nil && !stopped {
			yield(zero, err)
		}
	}
}

// All runs the query and returns its rows, relations loaded.
func (qs QuerySet[T]) All(ctx context.Context) ([]T, error) {
	out := []T{}
	for row, err := range qs.Iter(ctx) {
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if err := qs.m.prefetchRows(ctx, out, qs.q.prefetch); err != nil {
		return nil, err
	}
	return out, nil
}

// prefetchRows loads specs' relations into rows.
func (m *Manager[T]) prefetchRows(ctx context.Context, rows []T, specs []PrefetchSpec) error {
	if len(specs) == 0 || len(rows) == 0 {
		return nil
	}
	c, err := m.conn(ctx)
	if err != nil {
		return err
	}
	parents := make([]reflect.Value, len(rows))
	for i := range rows {
		parents[i] = reflect.ValueOf(&rows[i]).Elem()
	}
	return prefetch(ctx, c, m.meta, parents, specs)
}

// First is the first row, by the order given or else by primary key;
// nexus.NotFound when there is none.
func (qs QuerySet[T]) First(ctx context.Context) (T, error) {
	if len(qs.q.order) == 0 && qs.q.m != nil && qs.q.m.PK != nil {
		qs = qs.OrderBy(qs.q.m.PK.Name)
	}
	rows, err := qs.Limit(1).All(ctx)
	if err != nil {
		var zero T
		return zero, err
	}
	if len(rows) == 0 {
		var zero T
		return zero, qs.notFound()
	}
	return rows[0], nil
}

// Get is the one row matching conds: nexus.NotFound when there is none,
// nexus.Conflict when there are several.
func (qs QuerySet[T]) Get(ctx context.Context, conds ...Cond) (T, error) {
	var zero T
	rows, err := qs.Filter(conds...).Limit(2).All(ctx)
	switch {
	case err != nil:
		return zero, err
	case len(rows) == 0:
		return zero, qs.notFound()
	case len(rows) > 1:
		return zero, nexus.Errf(nexus.Conflict, "get() returned more than one %s", qs.name())
	}
	return rows[0], nil
}

func (qs QuerySet[T]) name() string {
	if qs.q.m == nil {
		return reflect.TypeFor[T]().Name()
	}
	return qs.q.m.Name
}

func (qs QuerySet[T]) notFound() error {
	return nexus.Errf(nexus.NotFound, "%s matching query does not exist", qs.name())
}

// Count is how many rows match.
func (qs QuerySet[T]) Count(ctx context.Context) (int64, error) {
	c, err := qs.m.conn(ctx)
	if err != nil {
		return 0, err
	}
	q := qs.q.clone()
	q.order = nil
	b := q.builder(c.d)
	var s string
	if q.limit > 0 || q.offset > 0 || q.distinct {
		sub, err := q.selectSQL(b, b.col(q.m.Fields[0]))
		if q.m.PK != nil {
			sub, err = q.selectSQL(b, b.col(q.m.PK))
		}
		if err != nil {
			return 0, err
		}
		s = "SELECT COUNT(*) FROM (" + sub + ") AS nexus_count"
	} else {
		w, err := q.whereSQL(b)
		if err != nil {
			return 0, err
		}
		s = "SELECT COUNT(*) FROM " + b.from() + w
	}
	var n int64
	err = scanOne(ctx, c, s, b.args(), &n)
	return n, err
}

// Exists is whether any row matches.
func (qs QuerySet[T]) Exists(ctx context.Context) (bool, error) {
	c, err := qs.m.conn(ctx)
	if err != nil {
		return false, err
	}
	b := qs.q.builder(c.d)
	w, err := qs.q.whereSQL(b)
	if err != nil {
		return false, err
	}
	rows, err := c.query(ctx, "SELECT 1 FROM "+b.from()+w+" LIMIT 1", b.args())
	if err != nil {
		return false, err
	}
	defer rows.Close()
	return rows.Next(), rows.Err()
}

func scanOne(ctx context.Context, c conn, s string, args []any, dst any) error {
	rows, err := c.query(ctx, s, args)
	if err != nil {
		return err
	}
	defer rows.Close()
	if !rows.Next() {
		return rows.Err()
	}
	if err := rows.Scan(&cell{reflect.ValueOf(dst).Elem()}); err != nil {
		return err
	}
	return rows.Err()
}

// ErrUnfiltered is the error of an Update or Delete with no condition:
// one that would change every row of the table. Unfiltered says that is
// what is meant.
var ErrUnfiltered = errors.New("orm: no condition")

// Unfiltered lets Update and Delete reach every row the query has: without
// it, an Update or Delete whose conditions are empty (no Filter, or one of
// nothing, such as an empty Q) fails with ErrUnfiltered and runs nothing.
//
//	Sessions.Unfiltered().Delete(ctx) // every row, on purpose
func (qs QuerySet[T]) Unfiltered() QuerySet[T] {
	return qs.with(func(q *query) { q.every = true })
}

// keys is the primary keys of the rows qs matches.
func (qs QuerySet[T]) keys(ctx context.Context) ([]any, error) {
	rows, err := qs.All(ctx)
	if err != nil {
		return nil, err
	}
	pk := qs.q.m.PK
	out := make([]any, len(rows))
	for i := range rows {
		out[i] = value(peek(reflect.ValueOf(&rows[i]).Elem(), pk.Index, pk.Type))
	}
	return out, nil
}

// guardEvery refuses a write that would reach every row by accident.
func (qs QuerySet[T]) guardEvery(op, where string) error {
	if where != "" || qs.q.every {
		return nil
	}
	return fmt.Errorf("%w: %s of %s would reach every row; filter it, or call Unfiltered() to mean every row", ErrUnfiltered, op, qs.name())
}

// Delete removes the matching rows and says how many. With no condition it
// fails with ErrUnfiltered unless the query is Unfiltered.
func (qs QuerySet[T]) Delete(ctx context.Context) (int64, error) {
	if qs.q.limit > 0 || qs.q.offset > 0 {
		return 0, fmt.Errorf("orm: can't delete a sliced %s query", qs.name())
	}
	c, err := qs.m.conn(ctx)
	if err != nil {
		return 0, err
	}
	b := qs.q.builder(c.d)
	w, err := qs.q.writeWhere(b)
	if err != nil {
		return 0, err
	}
	if err := qs.guardEvery("Delete", w); err != nil {
		return 0, err
	}
	var gone []T
	if !quiet(ctx) && qs.m.listening() {
		if gone, err = qs.All(ctx); err != nil {
			return 0, err
		}
	}
	res, err := c.exec(ctx, "DELETE FROM "+b.d.Quote(qs.q.m.Table)+w, b.args())
	if err != nil {
		return 0, qs.m.mapErr(c, err)
	}
	qs.m.mirrored(ctx, c, "delete", func(ctx context.Context) error {
		_, err := qs.Delete(ctx)
		return err
	})
	n, err := res.RowsAffected()
	if err == nil && n > 0 && !quiet(ctx) {
		qs.m.changed(ctx, c, Change[T]{Kind: BulkDeleted, Count: n, Rows: gone})
	}
	return n, err
}

// Set is the fields an Update writes, by field name.
type Set map[string]any

// Update writes values to the matching rows and says how many; fields
// marked auto_now are set to now too. With no condition it fails with
// ErrUnfiltered unless the query is Unfiltered.
func (qs QuerySet[T]) Update(ctx context.Context, values Set) (int64, error) {
	if qs.q.limit > 0 || qs.q.offset > 0 {
		return 0, fmt.Errorf("orm: can't update a sliced %s query", qs.name())
	}
	c, err := qs.m.conn(ctx)
	if err != nil {
		return 0, err
	}
	m := qs.q.m
	b := qs.q.builder(c.d)
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var sets []string
	seen := map[*field]bool{}
	for _, k := range keys {
		f, ok := m.field(k)
		if !ok {
			return 0, fmt.Errorf("orm: %s has no field %q", qs.name(), k)
		}
		if f.Via != "" {
			parts := strings.Split(f.Via, "__")
			owner := m
			for _, p := range parts[:len(parts)-1] {
				if r, ok := owner.relation(p); ok {
					if t, _, err := r.ends(); err == nil {
						owner = t
					}
				}
			}
			return 0, fmt.Errorf("orm: %s is read through %s: write %s", f.Name, parts[0], owner.Name)
		}
		if f.Gen != nil {
			return 0, fmt.Errorf("orm: %s.%s is generated by the database: it can't be written", m.Name, f.Name)
		}
		seen[f] = true
		val, err := b.operand(values[k])
		if err != nil {
			return 0, err
		}
		sets = append(sets, b.bare(f)+" = "+val)
	}
	stamped := values
	for _, f := range m.Fields {
		if f.AutoNow && !seen[f] {
			now := stamp(f)
			sets = append(sets, b.bare(f)+" = "+b.arg(now))
			if len(stamped) == len(values) {
				stamped = maps.Clone(values)
			}
			stamped[f.Name] = now
		}
	}
	if len(sets) == 0 {
		return 0, nil
	}
	if len(*b.joins) > 0 {
		return 0, fmt.Errorf("orm: Update of %s can't set values read through a relation", qs.name())
	}
	w, err := qs.q.writeWhere(b)
	if err != nil {
		return 0, err
	}
	if err := qs.guardEvery("Update", w); err != nil {
		return 0, err
	}
	var touched []any
	if !quiet(ctx) && qs.m.listening() && m.PK != nil {
		if touched, err = qs.keys(ctx); err != nil {
			return 0, err
		}
	}
	res, err := c.exec(ctx, "UPDATE "+b.d.Quote(m.Table)+" SET "+strings.Join(sets, ", ")+w, b.args())
	if err != nil {
		return 0, qs.m.mapErr(c, err)
	}
	qs.m.mirrored(ctx, c, "update", func(ctx context.Context) error {
		_, err := qs.Update(ctx, stamped)
		return err
	})
	n, err := res.RowsAffected()
	if err == nil && n > 0 && !quiet(ctx) {
		qs.m.changed(ctx, c, Change[T]{Kind: BulkUpdated, Count: n, Keys: touched})
	}
	return n, err
}

// Values is the query reading some fields into R, Django's values and
// values_list. A name is a field, an annotation, or a path through
// relations (team__name), joined in the same query; through rows held by
// many, a row per related row. R is a scalar for one name
// (Values[string]("email")); a struct, its fields matched by orm path tag
// (orm:"team__name"), name or column, and with no names its own fields
// say what is read; map[string]any keyed by the names as written; or
// []any in their order. An aggregate annotation among the names groups
// the rows by the others:
//
//	Users.Annotate("n", orm.Count("id")).Values[map[string]any]("team__name", "n")
func (qs QuerySet[T]) Values[R any](fields ...string) Values[T, R] {
	return Values[T, R]{q: qs, fields: fields}
}

// Values is a query of some fields of T, read into R.
type Values[T, R any] struct {
	q      QuerySet[T]
	fields []string
}

// Iter runs the query and yields its rows one at a time.
func (vq Values[T, R]) Iter(ctx context.Context) iter.Seq2[R, error] {
	return func(yield func(R, error) bool) {
		var zero R
		c, err := vq.q.m.conn(ctx)
		if err != nil {
			yield(zero, err)
			return
		}
		q := vq.q.q
		rt := reflect.TypeFor[R]()
		names := vq.fields
		if len(names) == 0 && rt.Kind() == reflect.Struct && rt != timeType {
			rm, err := modelOf(rt, "", "-")
			if err != nil {
				yield(zero, err)
				return
			}
			for _, f := range rm.readFields() {
				names = append(names, cmp.Or(f.Via, f.Column))
			}
		}
		if len(names) == 0 {
			yield(zero, fmt.Errorf("orm: Values needs at least one field"))
			return
		}
		b := q.builder(c.d)
		b.many = true
		cols := make([]string, len(names))
		fields := make([]*field, len(names))
		var group []string
		grouped := false
		for i, name := range names {
			if cols[i], fields[i], err = b.refField(name); err != nil {
				yield(zero, err)
				return
			}
			e, ok := b.ann[name]
			if ok {
				cols[i] += " AS " + b.d.Quote(name)
			}
			if _, agg := e.(Agg); agg {
				grouped = true
			} else {
				group = append(group, strconv.Itoa(i+1))
			}
		}
		if grouped && len(group) > 0 {
			q.group = " GROUP BY " + strings.Join(group, ", ")
		}
		rd, err := newReader(rt, names, fields)
		if err != nil {
			yield(zero, err)
			return
		}
		s, err := q.selectSQL(b, strings.Join(cols, ", "))
		if err != nil {
			yield(zero, err)
			return
		}
		rows, err := c.query(ctx, s, b.args())
		if err != nil {
			yield(zero, err)
			return
		}
		readRows(rows, rd, yield)
	}
}

// All runs the query and returns its rows.
func (vq Values[T, R]) All(ctx context.Context) ([]R, error) { return collectRows(vq.Iter(ctx)) }

func collectRows[R any](rows iter.Seq2[R, error]) ([]R, error) {
	out := []R{}
	for row, err := range rows {
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

// readRows yields rows as rd reads them, then closes them.
func readRows[R any](rows *sql.Rows, rd *reader, yield func(R, error) bool) {
	defer rows.Close()
	for rows.Next() {
		var row R
		if err := rd.scan(rows, reflect.ValueOf(&row).Elem()); err != nil {
			yield(row, err)
			return
		}
		if !yield(row, nil) {
			return
		}
	}
	if err := rows.Err(); err != nil {
		var zero R
		yield(zero, err)
	}
}

// reader puts a row's columns, named names, into an R: a scalar, a struct
// (a field per name, by orm path tag, name or column), map[string]any
// keyed by the names, or []any in their order.
type reader struct {
	names []string
	types []reflect.Type // a map's or list's value types, nil as the driver gives them
	index [][]int        // a struct's field per name
	shape int
}

const (
	scalarShape = iota
	structShape
	mapShape
	listShape
)

// newReader is the reader of R for names; fields, where known, are the
// model fields the names read: their Go types for maps and lists, and
// more names for a struct's fields to match.
func newReader(rt reflect.Type, names []string, fields []*field) (*reader, error) {
	rd := &reader{names: names, types: make([]reflect.Type, len(names))}
	for i, f := range fields {
		if f != nil {
			rd.types[i] = f.Type
			if f.Type.Kind() != reflect.Pointer {
				rd.types[i] = reflect.PointerTo(f.Type) // NULL as nil
			}
		}
	}
	switch {
	case rt == reflect.TypeFor[map[string]any]():
		rd.shape = mapShape
	case rt == reflect.TypeFor[[]any]():
		rd.shape = listShape
	case rt.Kind() == reflect.Struct && rt != timeType:
		rd.shape = structShape
	case len(names) != 1:
		return nil, fmt.Errorf("orm: %d columns can't be read into %v: read them into a struct, map[string]any or []any", len(names), rt)
	}
	if rd.shape != structShape {
		return rd, nil
	}
	rm, err := modelOf(rt, "", "-")
	if err != nil {
		return nil, err
	}
	for i, name := range names {
		alts := []string{name}
		if i < len(fields) && fields[i] != nil {
			alts = append(alts, fields[i].Column, fields[i].Name)
		}
		parts := strings.Split(name, "__")
		alts = append(alts, parts[len(parts)-1])
		var rf *field
		for _, f := range rm.via {
			if f.Via == name {
				rf = f
			}
		}
		for _, alt := range alts {
			if rf == nil {
				rf, _ = rm.field(alt)
			}
		}
		if rf == nil {
			return nil, fmt.Errorf("orm: %v has no field for %q", rt, name)
		}
		rd.index = append(rd.index, rf.Index)
	}
	return rd, nil
}

// scan reads the current row into v.
func (rd *reader) scan(rows *sql.Rows, v reflect.Value) error {
	if rd.shape == scalarShape {
		return rows.Scan(&cell{v})
	}
	cells := make([]cell, len(rd.names))
	dest := make([]any, len(cells))
	for i := range cells {
		if rd.shape == structShape {
			cells[i].dst = fieldOf(v, rd.index[i])
		} else {
			cells[i].dst = reflect.New(cmp.Or(rd.types[i], anyType)).Elem()
		}
		dest[i] = &cells[i]
	}
	if err := rows.Scan(dest...); err != nil {
		return err
	}
	plain := func(i int) any {
		x := value(cells[i].dst)
		if b, ok := x.([]byte); ok && rd.types[i] == nil {
			return string(b)
		}
		return x
	}
	switch rd.shape {
	case mapShape:
		out := make(map[string]any, len(cells))
		for i, name := range rd.names {
			out[name] = plain(i)
		}
		v.Set(reflect.ValueOf(out))
	case listShape:
		out := make([]any, len(cells))
		for i := range out {
			out[i] = plain(i)
		}
		v.Set(reflect.ValueOf(out))
	}
	return nil
}

var anyType = reflect.TypeFor[any]()

// Agg is an aggregate: of a field (Count, Sum, Avg, Min, Max), or any
// aggregate expression (AggOf).
type Agg struct {
	fn, field string
	key       string
	expr      Expr
}

// AggOf is an aggregate expression under a key of your own, for the
// aggregates the database has beyond the five:
//
//	var Names = orm.Function("names",
//		orm.Template("postgres", "string_agg({0}, ', ')"),
//		orm.Template("", "GROUP_CONCAT({0}, ', ')"),               // SQLite
//		orm.Template("mysql", "GROUP_CONCAT({0} SEPARATOR ', ')")) // a literal: MySQL takes no argument here
//	r, _ := Users.Aggregate(ctx, orm.AggOf("names", Names.Of(orm.F("name"))))
func AggOf(key string, e Expr) Agg { return Agg{key: key, expr: e} }

// exprSQL makes an aggregate an Expr: Annotate takes one, and Values
// listing it groups by the other names.
func (a Agg) exprSQL(b *builder) (string, error) {
	if a.expr != nil {
		return a.expr.exprSQL(b)
	}
	col, err := b.ref(a.field)
	if err != nil {
		return "", err
	}
	return a.fn + "(" + col + ")", nil
}

func Count(field string) Agg { return Agg{fn: "COUNT", field: field} }
func Sum(field string) Agg   { return Agg{fn: "SUM", field: field} }
func Avg(field string) Agg   { return Agg{fn: "AVG", field: field} }
func Min(field string) Agg   { return Agg{fn: "MIN", field: field} }
func Max(field string) Agg   { return Agg{fn: "MAX", field: field} }

// Key is the aggregate's name in a Result, Django's: age__avg.
func (a Agg) Key() string {
	if a.key != "" {
		return a.key
	}
	return a.field + "__" + strings.ToLower(a.fn)
}

// Result is aggregates by Key: COUNT an int64, AVG a float64, the others
// what the database gives (nil over no rows).
type Result map[string]any

// Int is an aggregate as an int64.
func (r Result) Int(key string) int64 {
	var n int64
	_ = assign(reflect.ValueOf(&n).Elem(), r[key])
	return n
}

// Float is an aggregate as a float64.
func (r Result) Float(key string) float64 {
	var f float64
	_ = assign(reflect.ValueOf(&f).Elem(), r[key])
	return f
}

// Aggregate computes aggregates over the matching rows.
func (qs QuerySet[T]) Aggregate(ctx context.Context, aggs ...Agg) (Result, error) {
	c, err := qs.m.conn(ctx)
	if err != nil {
		return nil, err
	}
	q := qs.q
	b := q.builder(c.d)
	exprs := make([]string, len(aggs))
	for i, a := range aggs {
		s, err := a.exprSQL(b)
		if err != nil {
			return nil, err
		}
		exprs[i] = s
	}
	w, err := q.whereSQL(b)
	if err != nil {
		return nil, err
	}
	rows, err := c.query(ctx, "SELECT "+strings.Join(exprs, ", ")+" FROM "+b.from()+w, b.args())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := Result{}
	if rows.Next() {
		vals := make([]any, len(aggs))
		dest := make([]any, len(aggs))
		for i := range vals {
			dest[i] = &vals[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		for i, a := range aggs {
			v := vals[i]
			switch a.fn {
			case "COUNT":
				var n int64
				_ = assign(reflect.ValueOf(&n).Elem(), v)
				v = n
			case "AVG":
				if v != nil {
					var f float64
					_ = assign(reflect.ValueOf(&f).Elem(), v)
					v = f
				}
			}
			if bs, ok := v.([]byte); ok {
				v = string(bs)
			}
			out[a.Key()] = v
		}
	}
	return out, rows.Err()
}
