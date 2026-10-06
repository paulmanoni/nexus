package orm

import (
	"context"
	"slices"
	"sync"
)

// ChangeKind is what a write did.
type ChangeKind int

const (
	Created     ChangeKind = iota + 1 // a row, by Create or BulkCreate
	Updated                           // a row, by Save
	Deleted                           // a row, by Remove
	BulkUpdated                       // rows, by a QuerySet's Update
	BulkDeleted                       // rows, by a QuerySet's Delete
)

// Change is a write OnChange hears: the row for Created, Updated and
// Deleted; how many rows for the bulk kinds.
type Change[T any] struct {
	Kind  ChangeKind
	Row   *T
	Count int64
}

type listeners[T any] struct {
	mu   sync.Mutex
	next int
	fns  map[int]func(context.Context, Change[T])
}

// OnChange calls fn after each write of T's rows, once its transaction
// commits (at once outside one; never for a rolled-back one): the place
// to tell live pages, with view.Broadcast. It returns what stops it.
//
//	Users.OnChange(func(ctx context.Context, c orm.Change[User]) {
//		view.Broadcast(ctx, "users", "changed")
//	})
func (m *Manager[T]) OnChange(fn func(ctx context.Context, c Change[T])) (stop func()) {
	m.ls.mu.Lock()
	defer m.ls.mu.Unlock()
	if m.ls.fns == nil {
		m.ls.fns = map[int]func(context.Context, Change[T]){}
	}
	id := m.ls.next
	m.ls.next++
	m.ls.fns[id] = fn
	return func() {
		m.ls.mu.Lock()
		defer m.ls.mu.Unlock()
		delete(m.ls.fns, id)
	}
}

// changed tells the listeners of a write on c's database, after commit.
func (m *Manager[T]) changed(ctx context.Context, c conn, ch Change[T]) {
	m.ls.mu.Lock()
	ids := make([]int, 0, len(m.ls.fns))
	for id := range m.ls.fns {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	fns := make([]func(context.Context, Change[T]), len(ids))
	for i, id := range ids {
		fns[i] = m.ls.fns[id]
	}
	m.ls.mu.Unlock()
	if len(fns) == 0 {
		return
	}
	if ch.Row != nil {
		row := *ch.Row
		ch.Row = &row
	}
	afterCommit(ctx, c.db, func(ctx context.Context) {
		for _, fn := range fns {
			fn(ctx, ch)
		}
	})
}

type quietKey struct{}

// hush has a QuerySet's Update or Delete tell no listener: Save and
// Remove tell their own, row-level change.
func hush(ctx context.Context) context.Context { return context.WithValue(ctx, quietKey{}, true) }

func quiet(ctx context.Context) bool { v, _ := ctx.Value(quietKey{}).(bool); return v }
