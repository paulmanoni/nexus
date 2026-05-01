package nexus

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// Store is the persistence contract AsCRUD operates against. A
// resolver function (passed to AsCRUD as its first argument) yields
// a Store on each request, so request-scoped behaviour — multi-
// tenancy, transactions, read-replica routing — is just "build the
// right Store in the resolver body."
//
// Custom backends (GORM, sqlc, Redis, …) implement this interface
// directly. The framework ships MemoryStore[T] for prototyping and
// tests; the GORM adapter ships in nexus/storage/gorm (subpackage)
// for production use.
//
// Method contracts:
//   - Find: returns ErrCRUDNotFound when id is unknown.
//   - Search: returns the page slice + total count for pagination.
//   - Save: upsert by id (Find then Save round-trips a create).
//   - Remove: returns ErrCRUDNotFound when id is unknown.
type Store[T any] interface {
	Find(ctx context.Context, id string) (*T, error)
	Search(ctx context.Context, opts ListOptions) (items []T, total int, err error)
	Save(ctx context.Context, item *T) error
	Remove(ctx context.Context, id string) error
}

// CRUDResolver is the per-request Store resolver passed to AsCRUD.
// Returns the Store the framework should use for this request — for
// multi-tenant apps, scope the Store to the tenant pulled from ctx.
//
// The resolver runs once per request before the action handler. An
// error short-circuits the request as a 500 (or as the mapped sentinel
// status when the error wraps one of the ErrCRUD* constants).
type CRUDResolver[T any] func(ctx context.Context) (Store[T], error)

// MemoryStore is a thread-safe in-memory Store[T]. Useful for
// prototyping and tests; not durable, not suitable for production.
//
// Construction requires id accessors so the store knows where the
// "primary key" lives on T — auto-detection happens at AsCRUD's
// boot when MemoryStore is built behind the scenes via
// MemoryResolver, so most users never call NewMemoryStore directly.
type MemoryStore[T any] struct {
	mu    sync.RWMutex
	items map[string]T
	getID func(*T) string
	setID func(*T, string)
	newID func() string
}

// NewMemoryStore constructs an empty in-memory store. getID/setID
// are required; newID defaults to uuid.NewString.
func NewMemoryStore[T any](getID func(*T) string, setID func(*T, string), newID func() string) *MemoryStore[T] {
	if newID == nil {
		newID = uuid.NewString
	}
	return &MemoryStore[T]{
		items: map[string]T{},
		getID: getID,
		setID: setID,
		newID: newID,
	}
}

// Find returns a copy of the stored value to avoid handing the
// caller a reference to the live map entry — saves us from
// "I mutated the returned object and the store changed" surprises.
func (s *MemoryStore[T]) Find(_ context.Context, id string) (*T, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.items[id]
	if !ok {
		return nil, ErrCRUDNotFound
	}
	cp := v
	return &cp, nil
}

// Search applies sort + pagination over a snapshot of the items
// map. For very large maps this allocates the whole slice; that's
// acceptable for an in-memory store whose purpose is prototyping.
func (s *MemoryStore[T]) Search(_ context.Context, opts ListOptions) ([]T, int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	all := make([]T, 0, len(s.items))
	for _, v := range s.items {
		all = append(all, v)
	}
	// Stable sort by id for reproducible output when no Sort given.
	sort.SliceStable(all, func(i, j int) bool {
		ii, jj := all[i], all[j]
		return s.getID(&ii) < s.getID(&jj)
	})
	total := len(all)

	limit := opts.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	offset := opts.Offset
	if offset < 0 {
		offset = 0
	}
	if offset >= total {
		return []T{}, total, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	page := make([]T, end-offset)
	copy(page, all[offset:end])
	return page, total, nil
}

// Save assigns a new id when getID returns "" (treat as create);
// otherwise upserts at the existing id. Mutates *item via setID
// so the caller sees the assigned id on a fresh create.
func (s *MemoryStore[T]) Save(_ context.Context, item *T) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.getID(item)
	if id == "" {
		id = s.newID()
		s.setID(item, id)
	}
	s.items[id] = *item
	return nil
}

func (s *MemoryStore[T]) Remove(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.items[id]; !ok {
		return ErrCRUDNotFound
	}
	delete(s.items, id)
	return nil
}

// MemoryResolver returns a CRUDResolver that always yields the same
// process-wide MemoryStore[T]. Construction inspects T via reflection
// to locate ID accessors:
//
//   - explicit get/set if both non-nil
//   - struct field named "ID" otherwise (case-sensitive, kind == String)
//
// Boot fails with a clear error if neither path resolves.
//
// The most common AsCRUD call:
//
//	nexus.AsCRUD[User](nexus.MemoryResolver[User](nil, nil))
func MemoryResolver[T any](getID func(*T) string, setID func(*T, string)) CRUDResolver[T] {
	if getID == nil || setID == nil {
		gid, sid, err := reflectIDAccessors[T]()
		if err != nil {
			// Defer the panic until the resolver actually runs so
			// pathological types don't take down init().
			return func(ctx context.Context) (Store[T], error) { return nil, err }
		}
		getID, setID = gid, sid
	}
	store := NewMemoryStore[T](getID, setID, nil)
	return func(_ context.Context) (Store[T], error) { return store, nil }
}

// trimmed-up name for a single helper used both here and by AsCRUD's
// boot validation; lives in this file so the memory store can fall
// back to it when called without explicit accessors.
func defaultPlural(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.HasSuffix(n, "s"), strings.HasSuffix(n, "x"), strings.HasSuffix(n, "z"):
		return n + "es"
	case strings.HasSuffix(n, "y") && len(n) > 1 && !isVowel(n[len(n)-2]):
		return n[:len(n)-1] + "ies"
	default:
		return n + "s"
	}
}

func isVowel(b byte) bool {
	switch b {
	case 'a', 'e', 'i', 'o', 'u':
		return true
	}
	return false
}