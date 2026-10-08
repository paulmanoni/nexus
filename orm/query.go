package orm

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// query is a QuerySet without its Go type: what prefetching runs for a
// related model, and what QuerySet[T] runs for its own.
type query struct {
	m        *model
	where    []Cond
	order    []string
	limit    int
	offset   int
	distinct bool
	ann      []annotation
	related  []string
	prefetch []PrefetchSpec
	every    bool   // Unfiltered: an Update or Delete may touch every row
	group    string // Values' GROUP BY
}

type annotation struct {
	name string
	expr Expr
}

func (q query) clone() query {
	q.where = slices.Clone(q.where)
	q.order = slices.Clone(q.order)
	q.ann = slices.Clone(q.ann)
	q.related = slices.Clone(q.related)
	q.prefetch = slices.Clone(q.prefetch)
	return q
}

// builder is a statement of the query on dialect d.
func (q query) builder(d Dialect) *builder {
	b := newBuilder(d, q.m)
	if len(q.ann) > 0 {
		b.ann = make(map[string]Expr, len(q.ann))
		for _, a := range q.ann {
			b.ann[a.name] = a.expr
		}
	}
	return b
}

// whereSQL is the WHERE clause, empty when there is none.
func (q query) whereSQL(b *builder) (string, error) {
	if len(q.where) == 0 {
		return "", nil
	}
	s, err := And(q.where...).sql(b)
	if err != nil || s == "" {
		return "", err
	}
	return " WHERE " + s, nil
}

func (q query) orderSQL(b *builder) (string, error) {
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
			return "", fmt.Errorf("orm: %s can't be ordered by %q: %w", q.m.Name, o, err)
		}
		parts[i] = col + dir
	}
	return " ORDER BY " + strings.Join(parts, ", "), nil
}

func (q query) pageSQL(b *builder) string {
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

// selectSQL is the SELECT of cols, then the query's conditions, order and
// page: written in that order, so the arguments are; the FROM, with the
// joins they followed, last.
func (q query) selectSQL(b *builder, cols string) (string, error) {
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
	return "SELECT " + d + cols + " FROM " + b.from() + w + q.group + o + q.pageSQL(b), nil
}

type computedField struct {
	ann   string
	index []int
}

// computedFields is the model's computed fields the query annotates, in
// the order of the annotations.
func (q query) computedFields() []computedField {
	var out []computedField
	for _, a := range q.ann {
		if f, ok := q.m.computed[strings.ToLower(a.name)]; ok {
			out = append(out, computedField{a.name, f.Index})
		}
	}
	return out
}

// relPlan is a foreign key SelectRelated reads in the same row: the
// builder of its model, the plan of the model holding it (-1 for the
// queried one), and its relation.
type relPlan struct {
	b      *builder
	parent int
	rel    *relation
}

// relatedPlans is the plans of SelectRelated's paths and of every path
// on their way, parents before children.
func (q query) relatedPlans(b *builder) ([]relPlan, error) {
	paths := slices.Clone(q.related)
	sort.Strings(paths)
	var plans []relPlan
	index := map[string]int{}
	for _, path := range paths {
		cur, parent, prefix := b, -1, ""
		for part := range strings.SplitSeq(path, "__") {
			r, ok := cur.m.relation(part)
			if !ok || !r.one() {
				return nil, fmt.Errorf("orm: SelectRelated(%q): %s has no foreign key or one-to-one %q (PrefetchRelated loads rows held by many)", path, cur.m.Name, part)
			}
			if err := r.held(cur.m); err != nil {
				return nil, err
			}
			prefix = strings.TrimPrefix(prefix+"__"+part, "__")
			if i, ok := index[prefix]; ok {
				cur, parent = plans[i].b, i
				continue
			}
			next, err := cur.follow(r)
			if err != nil {
				return nil, err
			}
			plans = append(plans, relPlan{b: next, parent: parent, rel: r})
			index[prefix] = len(plans) - 1
			cur, parent = next, len(plans)-1
		}
	}
	return plans, nil
}

// columns is the SELECT list reading q's rows: the model's fields, its
// computed annotations, and the fields of the foreign keys it selects.
func (q query) columns(b *builder, plans []relPlan) (string, error) {
	var cols []string
	read := func(b *builder) error {
		for _, f := range b.m.readFields() {
			col := b.col(f)
			if f.Via != "" {
				var err error
				if col, err = b.ref(f.Via); err != nil {
					return err
				}
			}
			cols = append(cols, col)
		}
		return nil
	}
	if err := read(b); err != nil {
		return "", err
	}
	for _, f := range q.computedFields() {
		col, err := b.ref(f.ann)
		if err != nil {
			return "", err
		}
		cols = append(cols, col+" AS "+b.d.Quote(f.ann))
	}
	for _, p := range plans {
		if err := read(p.b); err != nil {
			return "", err
		}
	}
	return strings.Join(cols, ", "), nil
}

// scan runs the query and reads each row into a value newRow makes (an
// addressable struct), by reflection, related rows included; each gets
// the rows until it returns false.
func (q query) scan(ctx context.Context, c conn, newRow func() reflect.Value, each func(reflect.Value) bool) error {
	b := q.builder(c.d)
	plans, err := q.relatedPlans(b)
	if err != nil {
		return err
	}
	cols, err := q.columns(b, plans)
	if err != nil {
		return err
	}
	s, err := q.selectSQL(b, cols)
	if err != nil {
		return err
	}
	rows, err := c.query(ctx, s, b.args())
	if err != nil {
		return err
	}
	defer rows.Close()
	computed := q.computedFields()
	n := len(q.m.readFields()) + len(computed)
	for _, p := range plans {
		n += len(p.b.m.readFields())
	}
	cells := make([]cell, n)
	dest := make([]any, n)
	for i := range cells {
		dest[i] = &cells[i]
	}
	tmps := make([]reflect.Value, len(plans))
	for rows.Next() {
		row := newRow()
		i := 0
		for _, f := range q.m.readFields() {
			cells[i].dst = fieldOf(row, f.Index)
			i++
		}
		for _, f := range computed {
			cells[i].dst = fieldOf(row, f.index)
			i++
		}
		for j, p := range plans {
			tmps[j] = reflect.New(p.b.m.Type)
			for _, f := range p.b.m.readFields() {
				cells[i].dst = fieldOf(tmps[j].Elem(), f.Index)
				i++
			}
		}
		if err := rows.Scan(dest...); err != nil {
			return err
		}
		// Children into their parents first, so a related row held by
		// value carries its own related rows.
		for j := len(plans) - 1; j >= 0; j-- {
			p := plans[j]
			if peek(tmps[j].Elem(), p.b.m.PK.Index, p.b.m.PK.Type).IsZero() {
				continue // no related row: the LEFT JOIN found none
			}
			parent := row
			if p.parent >= 0 {
				parent = tmps[p.parent].Elem()
			}
			dst := fieldOf(parent, p.rel.Index)
			if p.rel.Ptr {
				dst.Set(tmps[j])
			} else {
				dst.Set(tmps[j].Elem())
			}
		}
		if !each(row) {
			return nil
		}
	}
	return rows.Err()
}

// all is the query's rows of its model, as pointers, prefetches done.
func (q query) all(ctx context.Context, c conn) ([]reflect.Value, error) {
	var out []reflect.Value
	err := q.scan(ctx, c, func() reflect.Value { return reflect.New(q.m.Type).Elem() }, func(v reflect.Value) bool {
		out = append(out, v.Addr())
		return true
	})
	if err != nil {
		return nil, err
	}
	if len(q.prefetch) > 0 {
		parents := make([]reflect.Value, len(out))
		for i, p := range out {
			parents[i] = p.Elem()
		}
		if err := prefetch(ctx, c, q.m, parents, q.prefetch); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// writeWhere is the WHERE of an UPDATE or DELETE: the query's conditions,
// or, when they follow foreign keys, its rows' keys picked by a subquery
// (UPDATE and DELETE can't join portably; MySQL also needs the subquery
// wrapped to read the table it writes).
func (q query) writeWhere(b *builder) (string, error) {
	w, err := q.whereSQL(b)
	if err != nil || len(*b.joins) == 0 {
		return w, err
	}
	pk := q.m.PK
	if pk == nil {
		return "", fmt.Errorf("orm: %s has no primary key to write rows found through a relation", q.m.Name)
	}
	inner := "SELECT " + b.col(pk) + " AS nexus_pk FROM " + b.from() + w
	return " WHERE " + b.col(pk) + " IN (SELECT nexus_pk FROM (" + inner + ") AS nexus_rows)", nil
}
