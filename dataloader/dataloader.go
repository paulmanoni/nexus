// Package dataloader coalesces N individual key lookups into one batched
// fetch, eliminating the N+1 query pattern that GraphQL's nested
// resolvers otherwise produce.
//
// Two pieces:
//
//  1. Loader[K, V] — holds a fetch function and per-request state.
//     Load(key) enqueues the key and returns a thunk; the first thunk
//     to run dispatches one batched fetch for every enqueued key.
//
//  2. Registry — per-request map of loaders, attached to context via
//     WithRegistry. Get[K, V] looks up (or lazily creates) a named
//     loader on the registry so multiple resolvers in one request
//     share one Loader instance.
//
// Wired into the GraphQL transport via a request middleware that
// drops a fresh Registry on every POST /graphql. graphql-go's
// thunk-aware executor calls deferred resolvers breadth-first after
// the synchronous pass, which is exactly the batching window the
// Loader exploits — no goroutines, no timeouts, no surprises.
//
// Example, inside a GraphQL resolver:
//
//	loader := dataloader.Get[int64, *BankDetail](p.Context, "bankByUserID",
//	    func(ctx context.Context, userIDs []int64) (map[int64]*BankDetail, error) {
//	        return db.BankDetailsByUserIDs(ctx, userIDs)
//	    })
//	thunk := loader.Load(user.ID)
//	return thunk, nil
//
// 50 users in a list query → 1 BankDetailsByUserIDs call.
package dataloader

import (
	"context"
	"sync"
)

// Fetch is the batched lookup the Loader memoizes. It receives every
// distinct key seen during one Loader's lifetime and must return a
// map keyed by those same keys. Missing keys in the returned map are
// surfaced as the zero V (the resolver decides whether that means
// "null" or "not found").
//
// Errors propagate to every thunk attached to this loader instance —
// one fetch failure fails every caller in that request, which matches
// the dataloader-spec semantics and prevents partial-render confusion.
type Fetch[K comparable, V any] func(ctx context.Context, keys []K) (map[K]V, error)

// Loader is the per-(request, name) batcher. Construct via Get; do
// not create directly — the registry handles lifecycle so siblings
// share a single instance.
type Loader[K comparable, V any] struct {
	fetch Fetch[K, V]

	mu       sync.Mutex
	queue    []K
	enqueued map[K]struct{}
	batchCtx context.Context

	// once gates the first thunk to run; subsequent thunks read the
	// cached result map. The fetch fires exactly once per Loader.
	once   sync.Once
	result map[K]V
	err    error
}

// New constructs a standalone Loader. Most callers should use
// dataloader.Get(ctx, name, fetch) instead — it shares one Loader
// across every resolver in the same request, which is the whole
// point of the pattern. New is here for tests + advanced callers
// that manage lifecycle by hand.
func New[K comparable, V any](fetch Fetch[K, V]) *Loader[K, V] {
	return &Loader[K, V]{
		fetch:    fetch,
		enqueued: make(map[K]struct{}),
	}
}

// Load enqueues key for the next batch and returns a thunk that, when
// called, returns the value for that key. The thunk is intended to
// be returned directly from a graphql-go resolver — the executor
// dethunks breadth-first after all sibling resolvers have run, which
// is exactly when every key for this batch is in the queue.
//
// Duplicate keys are deduped: 50 thunks for the same user.ID enqueue
// the key once and share the result. The caller doesn't have to
// pre-uniqueify upstream.
func (l *Loader[K, V]) Load(key K) func() (interface{}, error) {
	l.mu.Lock()
	if _, dup := l.enqueued[key]; !dup {
		l.queue = append(l.queue, key)
		l.enqueued[key] = struct{}{}
	}
	l.mu.Unlock()

	return func() (interface{}, error) {
		l.once.Do(l.dispatch)
		if l.err != nil {
			return nil, l.err
		}
		// Missing key → zero V. Callers wanting "explicit null"
		// behavior can wrap with a pointer type so a missing key
		// surfaces as a typed-nil pointer.
		return l.result[key], nil
	}
}

// LoadCtx is Load with an explicit context for the batch call. Most
// resolvers pass the GraphQL request context; this overload exists
// so the batch call can use a different one (e.g. a longer-lived
// background ctx during cleanup). First call wins — siblings calling
// LoadCtx with different contexts get the first one for the batch.
func (l *Loader[K, V]) LoadCtx(ctx context.Context, key K) func() (interface{}, error) {
	l.mu.Lock()
	if l.batchCtx == nil {
		l.batchCtx = ctx
	}
	l.mu.Unlock()
	return l.Load(key)
}

// dispatch is the once-per-Loader fetch call. The mutex covers the
// queue read; graphql-go's executor is synchronous so contention is
// theoretical, but correctness wants the lock.
func (l *Loader[K, V]) dispatch() {
	l.mu.Lock()
	keys := l.queue
	l.queue = nil
	ctx := l.batchCtx
	l.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	if len(keys) == 0 {
		return
	}
	l.result, l.err = l.fetch(ctx, keys)
}

// Prime seeds the loader's cache with a key/value pair. Useful when
// a previous query already fetched the data and you want subsequent
// Load calls in the same request to skip the fetch.
func (l *Loader[K, V]) Prime(key K, value V) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.result == nil {
		l.result = make(map[K]V)
	}
	l.result[key] = value
	l.enqueued[key] = struct{}{}
}
