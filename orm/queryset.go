package orm

import (
	"context"
	"fmt"
	"iter"
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
	m        *Manager[T]
	where    []Cond
	order    []string
	limit    int
	offset   int
	distinct bool
	ann      []annotation
}

type annotation struct {
	name string
	expr Expr
}

// Annotate adds a computed column, named: Filter, OrderBy and Values use
// it by name, and a field of the model tagged orm:"computed" with the
// same name receives it.
//
//	Users.Annotate("joined", orm.Year.Of(orm.F("created_at"))).Filter(orm.Q{"joined__gte": 2025})
func (q QuerySet[T]) Annotate(name string, e Expr) QuerySet[T] {
	q.ann = append(slices.Clone(q.ann), annotation{name, e})
	return q
}

// builder is a statement of the query on dialect d.
func (q QuerySet[T]) builder(d Dialect) *builder {
	b := &builder{d: d, m: q.m.meta}
	if len(q.ann) > 0 {
		b.ann = make(map[string]Expr, len(q.ann))
		for _, a := range q.ann {
			b.ann[a.name] = a.expr
		}
	}
	return b
}

// Filter keeps the rows matching every condition.
func (q QuerySet[T]) Filter(conds ...Cond) QuerySet[T] {
	q.where = append(slices.Clone(q.where), conds...)
	return q
}

// Exclude drops the rows matching all the conditions.
func (q QuerySet[T]) Exclude(conds ...Cond) QuerySet[T] {
	q.where = append(slices.Clone(q.where), Not(And(conds...)))
	return q
}

// OrderBy sorts by fields, a leading - for descending: OrderBy("-age", "name").
func (q QuerySet[T]) OrderBy(fields ...string) QuerySet[T] {
	q.order = slices.Clone(fields)
	return q
}

// Limit keeps at most n rows.
func (q QuerySet[T]) Limit(n int) QuerySet[T] {
	q.limit = n
	return q
}

// Offset skips the first n rows.
func (q QuerySet[T]) Offset(n int) QuerySet[T] {
	q.offset = n
	return q
}

// Distinct drops duplicate rows.
func (q QuerySet[T]) Distinct() QuerySet[T] {
	q.distinct = true
	return q
}

// where is the WHERE clause, empty when there is none.
func (q QuerySet[T]) whereSQL(b *builder) (string, error) {
	if len(q.where) == 0 {
		return "", nil
	}
	s, err := And(q.where...).sql(b)
	if err != nil || s == "" {
		return "", err
	}
	return " WHERE " + s, nil
}

func (q QuerySet[T]) orderSQL(b *builder) (string, error) {
	if len(q.order) == 0 {
		return "", nil
	}
	parts := make([]string, len(q.order))
	for i, o := range q.order {
		dir := " ASC"
		if strings.HasPrefix(o, "-") {
			o, dir = o[1:], " DESC"
		}
		col, err := b.ref(o)
		if err != nil {
			return "", fmt.Errorf("orm: %s has no field %q to order by", q.m.meta.Name, o)
		}
		parts[i] = col + dir
	}
	return " ORDER BY " + strings.Join(parts, ", "), nil
}

func (q QuerySet[T]) pageSQL(b *builder) string {
	s := ""
	switch {
	case q.limit > 0:
		s = " LIMIT " + strconv.Itoa(q.limit)
	case q.offset > 0 && b.d.NoLimit() != "":
		s = " LIMIT " + b.d.NoLimit()
	}
	if q.offset > 0 {
		s += " OFFSET " + strconv.Itoa(q.offset)
	}
	return s
}

// selectSQL is the SELECT of cols (all the model's when none).
func (q QuerySet[T]) selectSQL(b *builder, cols string) (string, error) {
	if cols == "" {
		quoted := make([]string, len(q.m.meta.Fields))
		for i, f := range q.m.meta.Fields {
			quoted[i] = b.col(f)
		}
		for _, f := range q.computedFields() {
			col, err := b.ref(f.ann)
			if err != nil {
				return "", err
			}
			quoted = append(quoted, col+" AS "+b.d.Quote(f.ann))
		}
		cols = strings.Join(quoted, ", ")
	}
	w, err := q.whereSQL(b)
	if err != nil {
		return "", err
	}
	o, err := q.orderSQL(b)
	if err != nil {
		return "", err
	}
	d := ""
	if q.distinct {
		d = "DISTINCT "
	}
	return "SELECT " + d + cols + " FROM " + b.d.Quote(q.m.meta.Table) + w + o + q.pageSQL(b), nil
}

type computedField struct {
	ann   string
	index []int
}

// computedFields is the model's computed fields the query annotates, in
// the order of the annotations.
func (q QuerySet[T]) computedFields() []computedField {
	var out []computedField
	for _, a := range q.ann {
		if f, ok := q.m.meta.computed[strings.ToLower(a.name)]; ok {
			out = append(out, computedField{a.name, f.Index})
		}
	}
	return out
}

// Iter runs the query and yields its rows one at a time, without holding
// them all.
func (q QuerySet[T]) Iter(ctx context.Context) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var zero T
		c, err := q.m.conn(ctx)
		if err != nil {
			yield(zero, err)
			return
		}
		b := q.builder(c.d)
		s, err := q.selectSQL(b, "")
		if err != nil {
			yield(zero, err)
			return
		}
		rows, err := c.query(ctx, s, b.args)
		if err != nil {
			yield(zero, err)
			return
		}
		defer rows.Close()
		computed := q.computedFields()
		if mk, ok := rowScanner[T](q.m.meta); ok && len(computed) == 0 {
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
		n := len(q.m.meta.Fields)
		cells := make([]cell, n+len(computed))
		dest := make([]any, len(cells))
		for i := range cells {
			dest[i] = &cells[i]
		}
		for rows.Next() {
			var row T
			v := reflect.ValueOf(&row).Elem()
			for i, f := range q.m.meta.Fields {
				cells[i].dst = fieldOf(v, f.Index)
			}
			for i, f := range computed {
				cells[n+i].dst = fieldOf(v, f.index)
			}
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
	}
}

// All runs the query and returns its rows.
func (q QuerySet[T]) All(ctx context.Context) ([]T, error) {
	out := []T{}
	for row, err := range q.Iter(ctx) {
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

// First is the first row, by the order given or else by primary key;
// nexus.NotFound when there is none.
func (q QuerySet[T]) First(ctx context.Context) (T, error) {
	if len(q.order) == 0 && q.m.meta != nil && q.m.meta.PK != nil {
		q = q.OrderBy(q.m.meta.PK.Name)
	}
	rows, err := q.Limit(1).All(ctx)
	if err != nil {
		var zero T
		return zero, err
	}
	if len(rows) == 0 {
		var zero T
		return zero, q.notFound()
	}
	return rows[0], nil
}

// Get is the one row matching conds: nexus.NotFound when there is none,
// nexus.Conflict when there are several.
func (q QuerySet[T]) Get(ctx context.Context, conds ...Cond) (T, error) {
	var zero T
	rows, err := q.Filter(conds...).Limit(2).All(ctx)
	switch {
	case err != nil:
		return zero, err
	case len(rows) == 0:
		return zero, q.notFound()
	case len(rows) > 1:
		return zero, nexus.Errf(nexus.Conflict, "get() returned more than one %s", q.name())
	}
	return rows[0], nil
}

func (q QuerySet[T]) name() string {
	if q.m.meta == nil {
		return reflect.TypeFor[T]().Name()
	}
	return q.m.meta.Name
}

func (q QuerySet[T]) notFound() error {
	return nexus.Errf(nexus.NotFound, "%s matching query does not exist", q.name())
}

// Count is how many rows match.
func (q QuerySet[T]) Count(ctx context.Context) (int64, error) {
	c, err := q.m.conn(ctx)
	if err != nil {
		return 0, err
	}
	b := q.builder(c.d)
	inner := q
	inner.order = nil
	var s string
	if q.limit > 0 || q.offset > 0 || q.distinct {
		sub, err := inner.selectSQL(b, "")
		if err != nil {
			return 0, err
		}
		s = "SELECT COUNT(*) FROM (" + sub + ") AS nexus_count"
	} else {
		w, err := inner.whereSQL(b)
		if err != nil {
			return 0, err
		}
		s = "SELECT COUNT(*) FROM " + b.d.Quote(q.m.meta.Table) + w
	}
	var n int64
	err = scanOne(ctx, c, s, b.args, &n)
	return n, err
}

// Exists is whether any row matches.
func (q QuerySet[T]) Exists(ctx context.Context) (bool, error) {
	c, err := q.m.conn(ctx)
	if err != nil {
		return false, err
	}
	b := q.builder(c.d)
	w, err := q.whereSQL(b)
	if err != nil {
		return false, err
	}
	s := "SELECT 1 FROM " + b.d.Quote(q.m.meta.Table) + w + " LIMIT 1"
	rows, err := c.query(ctx, s, b.args)
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
		if err := rows.Err(); err != nil {
			return err
		}
		return nil
	}
	if err := rows.Scan(&cell{reflect.ValueOf(dst).Elem()}); err != nil {
		return err
	}
	return rows.Err()
}

// Delete removes the matching rows and says how many.
func (q QuerySet[T]) Delete(ctx context.Context) (int64, error) {
	if q.limit > 0 || q.offset > 0 {
		return 0, fmt.Errorf("orm: can't delete a sliced %s query", q.name())
	}
	c, err := q.m.conn(ctx)
	if err != nil {
		return 0, err
	}
	b := q.builder(c.d)
	w, err := q.whereSQL(b)
	if err != nil {
		return 0, err
	}
	res, err := c.exec(ctx, "DELETE FROM "+b.d.Quote(q.m.meta.Table)+w, b.args)
	if err != nil {
		return 0, q.m.mapErr(c, err)
	}
	return res.RowsAffected()
}

// Set is the fields an Update writes, by field name.
type Set map[string]any

// Update writes values to the matching rows and says how many; fields
// marked auto_now are set to now too.
func (q QuerySet[T]) Update(ctx context.Context, values Set) (int64, error) {
	if q.limit > 0 || q.offset > 0 {
		return 0, fmt.Errorf("orm: can't update a sliced %s query", q.name())
	}
	c, err := q.m.conn(ctx)
	if err != nil {
		return 0, err
	}
	b := q.builder(c.d)
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var sets []string
	seen := map[*field]bool{}
	for _, k := range keys {
		f, ok := q.m.meta.field(k)
		if !ok {
			return 0, fmt.Errorf("orm: %s has no field %q", q.name(), k)
		}
		seen[f] = true
		val, err := b.operand(values[k])
		if err != nil {
			return 0, err
		}
		sets = append(sets, b.col(f)+" = "+val)
	}
	for _, f := range q.m.meta.Fields {
		if f.AutoNow && !seen[f] {
			sets = append(sets, b.col(f)+" = "+b.arg(stamp(f)))
		}
	}
	if len(sets) == 0 {
		return 0, nil
	}
	w, err := q.whereSQL(b)
	if err != nil {
		return 0, err
	}
	res, err := c.exec(ctx, "UPDATE "+b.d.Quote(q.m.meta.Table)+" SET "+strings.Join(sets, ", ")+w, b.args)
	if err != nil {
		return 0, q.m.mapErr(c, err)
	}
	return res.RowsAffected()
}

// Values is the query reading only some columns into R: a scalar for one
// column (Values[string]("name")), else a struct whose fields match the
// columns by name or tag.
func (q QuerySet[T]) Values[R any](fields ...string) Values[T, R] {
	return Values[T, R]{q: q, fields: fields}
}

// Values is a query of some columns of T, read into R.
type Values[T, R any] struct {
	q      QuerySet[T]
	fields []string
}

// Iter runs the query and yields its rows one at a time.
func (vq Values[T, R]) Iter(ctx context.Context) iter.Seq2[R, error] {
	return func(yield func(R, error) bool) {
		var zero R
		q := vq.q
		c, err := q.m.conn(ctx)
		if err != nil {
			yield(zero, err)
			return
		}
		if len(vq.fields) == 0 {
			yield(zero, fmt.Errorf("orm: Values needs at least one field"))
			return
		}
		b := q.builder(c.d)
		cols := make([]*field, len(vq.fields))
		quoted := make([]string, len(vq.fields))
		for i, name := range vq.fields {
			if _, ok := b.ann[name]; ok {
				s, err := b.ref(name)
				if err != nil {
					yield(zero, err)
					return
				}
				cols[i], quoted[i] = &field{Name: name, Column: name}, s+" AS "+b.d.Quote(name)
				continue
			}
			f, ok := q.m.meta.field(name)
			if !ok {
				yield(zero, fmt.Errorf("orm: %s has no field %q", q.name(), name))
				return
			}
			cols[i], quoted[i] = f, b.col(f)
		}
		var targets []func(reflect.Value) reflect.Value
		rt := reflect.TypeFor[R]()
		if len(cols) == 1 && !(rt.Kind() == reflect.Struct && rt != timeType) {
			targets = []func(reflect.Value) reflect.Value{func(v reflect.Value) reflect.Value { return v }}
		} else {
			rm, err := modelOf(rt, "-")
			if err != nil {
				yield(zero, err)
				return
			}
			for _, f := range cols {
				rf, ok := rm.field(f.Column)
				if !ok {
					rf, ok = rm.field(f.Name)
				}
				if !ok {
					yield(zero, fmt.Errorf("orm: %v has no field for %q", rt, f.Column))
					return
				}
				idx := rf.Index
				targets = append(targets, func(v reflect.Value) reflect.Value { return fieldOf(v, idx) })
			}
		}
		s, err := q.selectSQL(b, strings.Join(quoted, ", "))
		if err != nil {
			yield(zero, err)
			return
		}
		rows, err := c.query(ctx, s, b.args)
		if err != nil {
			yield(zero, err)
			return
		}
		defer rows.Close()
		cells := make([]cell, len(cols))
		dest := make([]any, len(cells))
		for i := range cells {
			dest[i] = &cells[i]
		}
		for rows.Next() {
			var row R
			v := reflect.ValueOf(&row).Elem()
			for i, t := range targets {
				cells[i].dst = t(v)
			}
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
	}
}

// All runs the query and returns its rows.
func (vq Values[T, R]) All(ctx context.Context) ([]R, error) {
	out := []R{}
	for row, err := range vq.Iter(ctx) {
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

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
//	var StringAgg = orm.Function("string_agg", orm.Template("mysql", "GROUP_CONCAT({0} SEPARATOR {1})"))
//	r, _ := Users.Aggregate(ctx, orm.AggOf("names", StringAgg.Of(orm.F("name"), ", ")))
func AggOf(key string, e Expr) Agg { return Agg{key: key, expr: e} }

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
func (q QuerySet[T]) Aggregate(ctx context.Context, aggs ...Agg) (Result, error) {
	c, err := q.m.conn(ctx)
	if err != nil {
		return nil, err
	}
	b := q.builder(c.d)
	exprs := make([]string, len(aggs))
	for i, a := range aggs {
		if a.expr != nil {
			s, err := a.expr.exprSQL(b)
			if err != nil {
				return nil, err
			}
			exprs[i] = s
			continue
		}
		col, err := b.ref(a.field)
		if err != nil {
			return nil, err
		}
		exprs[i] = a.fn + "(" + col + ")"
	}
	w, err := q.whereSQL(b)
	if err != nil {
		return nil, err
	}
	rows, err := c.query(ctx, "SELECT "+strings.Join(exprs, ", ")+" FROM "+b.d.Quote(q.m.meta.Table)+w, b.args)
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
