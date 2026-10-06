package orm

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
)

// Prefetch loads a relation's rows with a query of your own (filtered,
// ordered, selecting or prefetching further), as Django's Prefetch does:
//
//	Users.PrefetchRelated(orm.Prefetch("posts", Posts.Filter(orm.Q{"draft": false}).OrderBy("-id")))
//
// PrefetchRelated("posts") is Prefetch("posts", nil): every related row.
func Prefetch(path string, qs prefetcher) PrefetchSpec {
	return PrefetchSpec{path: path, qs: qs}
}

// PrefetchSpec is a relation PrefetchRelated loads; see Prefetch.
type PrefetchSpec struct {
	path string
	qs   prefetcher
}

// prefetcher is a QuerySet of any model, as Prefetch takes it.
type prefetcher interface{ prefetchQuery() query }

// prefetchChunk is how many keys one IN (…) of a prefetch holds.
const prefetchChunk = 1000

// prefetch loads specs' relations into parents (addressable structs of
// m), each relation in one query per prefetchChunk keys, nested paths
// (posts__comments) on the rows loaded.
func prefetch(ctx context.Context, c conn, m *model, parents []reflect.Value, specs []PrefetchSpec) error {
	if len(parents) == 0 {
		return nil
	}
	type step struct {
		custom *query
		nested []PrefetchSpec
	}
	var order []string
	steps := map[string]*step{}
	for _, sp := range specs {
		first, rest, _ := strings.Cut(sp.path, "__")
		s := steps[first]
		if s == nil {
			s = &step{}
			steps[first] = s
			order = append(order, first)
		}
		switch {
		case rest != "":
			s.nested = append(s.nested, PrefetchSpec{path: rest, qs: sp.qs})
		case sp.qs != nil:
			q := sp.qs.prefetchQuery()
			if q.m == nil {
				return fmt.Errorf("orm: PrefetchRelated takes relation names and orm.Prefetch, not %T", sp.qs.(badPrefetch).v)
			}
			s.custom = &q
		}
	}
	for _, name := range order {
		r, ok := m.relation(name)
		if !ok {
			return fmt.Errorf("orm: PrefetchRelated: %s has no relation %q", m.Name, name)
		}
		s := steps[name]
		if err := fetchRelated(ctx, c, m, r, parents, s.custom, s.nested); err != nil {
			return err
		}
	}
	return nil
}

func fetchRelated(ctx context.Context, c conn, m *model, r *relation, parents []reflect.Value, custom *query, nested []PrefetchSpec) error {
	t, err := r.target()
	if err != nil {
		return err
	}
	base := query{m: t}
	if custom != nil {
		if custom.m != t {
			return fmt.Errorf("orm: Prefetch(%q) is a query of %s, not %s", r.Name, custom.m.Name, t.Name)
		}
		base = custom.clone()
		base.limit, base.offset = 0, 0
	}
	base.prefetch = append(base.prefetch, nested...)
	load := func(field string, keys []any) ([]reflect.Value, error) {
		var out []reflect.Value
		for chunk := range slices.Chunk(keys, prefetchChunk) {
			q := base.clone()
			q.where = append(q.where, Q{field + "__in": chunk})
			rows, err := q.all(ctx, c)
			if err != nil {
				return nil, err
			}
			out = append(out, rows...)
		}
		return out, nil
	}
	switch r.Kind {
	case relFK:
		fk, _ := m.field(r.Column)
		keys := distinctKeys(parents, fk)
		rows, err := load(t.PK.Name, keys)
		if err != nil {
			return err
		}
		byKey := map[any]reflect.Value{}
		for _, row := range rows {
			byKey[key(row.Elem(), t.PK)] = row
		}
		for _, p := range parents {
			if row, ok := byKey[key(p, fk)]; ok {
				setOne(fieldOf(p, r.Index), r, row)
			}
		}
	case relRev:
		col, err := r.childColumn(m, t)
		if err != nil {
			return err
		}
		rows, err := load(col.Name, distinctKeys(parents, m.PK))
		if err != nil {
			return err
		}
		byKey := map[any][]reflect.Value{}
		for _, row := range rows {
			k := key(row.Elem(), col)
			byKey[k] = append(byKey[k], row)
		}
		for _, p := range parents {
			setMany(fieldOf(p, r.Index), r, byKey[key(p, m.PK)])
		}
	case relM2M:
		pairs, err := throughPairs(ctx, c, r, distinctKeys(parents, m.PK))
		if err != nil {
			return err
		}
		var remotes []any
		seen := map[any]bool{}
		for _, pr := range pairs {
			if !seen[pr[1]] {
				seen[pr[1]] = true
				remotes = append(remotes, pr[1])
			}
		}
		rows, err := load(t.PK.Name, remotes)
		if err != nil {
			return err
		}
		byKey := map[any]reflect.Value{}
		for _, row := range rows {
			byKey[key(row.Elem(), t.PK)] = row
		}
		byLocal := map[any][]reflect.Value{}
		for _, pr := range pairs {
			if row, ok := byKey[pr[1]]; ok {
				byLocal[pr[0]] = append(byLocal[pr[0]], row)
			}
		}
		for _, p := range parents {
			setMany(fieldOf(p, r.Index), r, byLocal[key(p, m.PK)])
		}
	}
	return nil
}

// throughPairs is the (local, remote) keys of a many-to-many table for
// the local keys.
func throughPairs(ctx context.Context, c conn, r *relation, locals []any) ([][2]any, error) {
	var out [][2]any
	for chunk := range slices.Chunk(locals, prefetchChunk) {
		b := &builder{d: c.d, st: &stmt{}, joins: &[]joinClause{}}
		marks := make([]string, len(chunk))
		for i, k := range chunk {
			marks[i] = b.arg(k)
		}
		local, remote := c.d.Quote(r.ThroughLocal), c.d.Quote(r.ThroughRemote)
		s := "SELECT " + local + ", " + remote + " FROM " + c.d.Quote(r.Through) + " WHERE " + local + " IN (" + strings.Join(marks, ", ") + ")"
		rows, err := c.query(ctx, s, b.args())
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var l, rm any
			if err := rows.Scan(&l, &rm); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, [2]any{normKey(l), normKey(rm)})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// distinctKeys is the non-null values of f over rows, once each.
func distinctKeys(rows []reflect.Value, f *field) []any {
	var out []any
	seen := map[any]bool{}
	for _, row := range rows {
		k := key(row, f)
		if k == nil || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}

// key is f of row as a map key both sides of a relation agree on.
func key(row reflect.Value, f *field) any { return normKey(value(peek(row, f.Index, f.Type))) }

// normKey makes keys of different Go types compare: integers as int64,
// bytes as strings, pointers by what they point to.
func normKey(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case int:
		return int64(x)
	case int8:
		return int64(x)
	case int16:
		return int64(x)
	case int32:
		return int64(x)
	case uint:
		return int64(x)
	case uint8:
		return int64(x)
	case uint16:
		return int64(x)
	case uint32:
		return int64(x)
	case uint64:
		return int64(x)
	case []byte:
		return string(x)
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil
		}
		return normKey(rv.Elem().Interface())
	}
	if rv.Kind() >= reflect.Int && rv.Kind() <= reflect.Int64 {
		return rv.Int()
	}
	if rv.Kind() == reflect.String {
		return rv.String()
	}
	return v
}

// setOne puts a related row (a pointer) into a field holding one.
func setOne(dst reflect.Value, r *relation, row reflect.Value) {
	if r.Ptr {
		dst.Set(row)
	} else {
		dst.Set(row.Elem())
	}
}

// setMany puts related rows (pointers) into a slice field, empty rather
// than nil when there are none.
func setMany(dst reflect.Value, r *relation, rows []reflect.Value) {
	s := reflect.MakeSlice(dst.Type(), 0, len(rows))
	for _, row := range rows {
		if r.Ptr {
			s = reflect.Append(s, row)
		} else {
			s = reflect.Append(s, row.Elem())
		}
	}
	dst.Set(s)
}
