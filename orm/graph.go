package orm

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/paulmanoni/nexus/v2"
)

// GraphRelation makes a relation of T a GraphQL field of T's type, loaded
// in batches: every parent a query returns asks for its related rows,
// and one IN (…) query a level answers them all — no query per parent.
// C is the field's type: *User for a foreign key, []Post (or []*Post)
// for rows held by many. Pass the option to nexus.Boot or a module.
//
//	Books.GraphRelation[*Author]("author"), Authors.GraphRelation[[]Book]("books")
func (m *Manager[T]) GraphRelation[C any](name string) nexus.Option {
	if m.err != nil {
		return nexus.FailBoot(m.err)
	}
	r, ok := m.meta.relation(name)
	if !ok {
		return nexus.FailBoot(fmt.Errorf("orm: GraphRelation: %s has no relation %q", m.meta.Name, name))
	}
	ft := m.meta.Type.FieldByIndex(r.Index).Type
	if ft != reflect.TypeFor[C]() {
		return nexus.FailBoot(fmt.Errorf("orm: GraphRelation[%v](%q): the field is %v", reflect.TypeFor[C](), name, ft))
	}
	parentKey := m.meta.PK
	if r.one() {
		parentKey, _ = m.meta.field(r.Column)
	}
	keyFn := func(p T) any { return key(reflect.ValueOf(&p).Elem(), parentKey) }
	fetch := func(ctx context.Context, keys []any) (map[any]C, error) {
		c, err := m.conn(ctx)
		if err != nil {
			return nil, err
		}
		related, err := relatedByKey(ctx, c, m.meta, r, keys)
		if err != nil {
			return nil, err
		}
		out := make(map[any]C, len(related))
		for k, rows := range related {
			v := reflect.New(reflect.TypeFor[C]()).Elem()
			if r.one() {
				setOne(v, r, rows[0])
			} else {
				setMany(v, r, rows)
			}
			out[k] = v.Interface().(C)
		}
		return out, nil
	}
	return nexus.LoadField[T, any, C](lowerFirst(r.Name), keyFn, fetch)
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// relatedByKey is a relation's rows (pointers) for the keys of the
// parents asking: their foreign keys, or their primary keys for rows
// held by many.
func relatedByKey(ctx context.Context, c conn, m *model, r *relation, keys []any) (map[any][]reflect.Value, error) {
	t, err := r.target()
	if err != nil {
		return nil, err
	}
	var clean []any
	for _, k := range keys {
		if k != nil {
			clean = append(clean, k)
		}
	}
	out := map[any][]reflect.Value{}
	if len(clean) == 0 {
		return out, nil
	}
	load := func(field string, ks []any) ([]reflect.Value, error) {
		var rows []reflect.Value
		for start := 0; start < len(ks); start += prefetchChunk {
			q := query{m: t, where: []Cond{Q{field + "__in": ks[start:min(start+prefetchChunk, len(ks))]}}}
			got, err := q.all(ctx, c)
			if err != nil {
				return nil, err
			}
			rows = append(rows, got...)
		}
		return rows, nil
	}
	switch r.Kind {
	case relFK:
		rows, err := load(t.PK.Name, clean)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			k := key(row.Elem(), t.PK)
			out[k] = append(out[k], row)
		}
	case relRev:
		col, err := r.childColumn(m, t)
		if err != nil {
			return nil, err
		}
		rows, err := load(col.Name, clean)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			k := key(row.Elem(), col)
			out[k] = append(out[k], row)
		}
	case relM2M:
		pairs, err := throughPairs(ctx, c, r, clean)
		if err != nil {
			return nil, err
		}
		var remotes []any
		seen := map[any]bool{}
		for _, p := range pairs {
			if !seen[p[1]] {
				seen[p[1]] = true
				remotes = append(remotes, p[1])
			}
		}
		rows, err := load(t.PK.Name, remotes)
		if err != nil {
			return nil, err
		}
		byKey := map[any]reflect.Value{}
		for _, row := range rows {
			byKey[key(row.Elem(), t.PK)] = row
		}
		for _, p := range pairs {
			if row, ok := byKey[p[1]]; ok {
				out[p[0]] = append(out[p[0]], row)
			}
		}
	}
	// Parents with none still get an empty list, not null.
	if !r.one() {
		for _, k := range clean {
			if _, ok := out[k]; !ok {
				out[k] = nil
			}
		}
	}
	return out, nil
}
