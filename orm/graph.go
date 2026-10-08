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
	if err := r.held(m.meta); err != nil {
		return nexus.FailBoot(err)
	}
	ft := m.meta.Type.FieldByIndex(r.Index).Type
	if ft != reflect.TypeFor[C]() {
		return nexus.FailBoot(fmt.Errorf("orm: GraphRelation[%v](%q): the field is %v", reflect.TypeFor[C](), name, ft))
	}
	keyFn := func(p T) any { return key(reflect.ValueOf(&p).Elem(), r.local) }
	fetch := func(ctx context.Context, keys []any) (map[any]C, error) {
		c, err := m.conn(ctx)
		if err != nil {
			return nil, err
		}
		related, err := relatedByKey(ctx, c, r, keys)
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
// parents asking: their values of the relation's local field.
func relatedByKey(ctx context.Context, c conn, r *relation, keys []any) (map[any][]reflect.Value, error) {
	t, _, err := r.ends()
	if err != nil {
		return nil, err
	}
	var clean []any
	for _, k := range keys {
		if k != nil {
			clean = append(clean, k)
		}
	}
	out, err := related(ctx, c, r, query{m: t}, clean)
	if err != nil {
		return nil, err
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
