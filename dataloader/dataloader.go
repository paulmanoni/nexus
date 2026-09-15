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
// Errors propagate to every thunk attached to the same batch — one
// fetch failure fails every caller whose key rode in that batch,
// which matches the dataloader-spec semantics and prevents
// partial-render confusion.
type Fetch[K comparable, V any] func(ctx context.Context, keys []K) (map[K]V, error)

// batch is one dispatch unit: the keys enqueued between the previous
// dispatch and this one, fetched together exactly once.
type batch[K comparable, V any] struct {
	once sync.Once
	keys []K
	err  error
}

// Loader is the per-(request, name) batcher. Construct via Get; do
// not create directly — the registry handles lifecycle so siblings
// share a single instance.
//
// Dispatch is per BATCH, not per Loader: keys enqueued after a
// dispatch open a new batch that fetches when its first thunk runs.
// That is what makes nested queries work — level-2 resolvers Load
// into the same named loader after level-1's batch has fired, and
// their keys get a real fetch instead of a silent zero value.
type Loader[K comparable, V any] struct {
	fetch Fetch[K, V]

	mu       sync.Mutex
	cur      *batch[K, V]       // open batch collecting keys; nil until the next Load
	keyBatch map[K]*batch[K, V] // every enqueued key → the batch that fetches it
	batchCtx context.Context
	result   map[K]V // merged results across all dispatched batches
}

// New constructs a standalone Loader. Most callers should use
// dataloader.Get(ctx, name, fetch) instead — it shares one Loader
// across every resolver in the same request, which is the whole
// point of the pattern. New is here for tests + advanced callers
// that manage lifecycle by hand.
func New[K comparable, V any](fetch Fetch[K, V]) *Loader[K, V] {
	return &Loader[K, V]{
		fetch:    fetch,
		keyBatch: make(map[K]*batch[K, V]),
		result:   make(map[K]V),
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
	b, seen := l.keyBatch[key]
	if !seen {
		if _, primed := l.result[key]; !primed {
			if l.cur == nil {
				l.cur = &batch[K, V]{}
			}
			l.cur.keys = append(l.cur.keys, key)
			l.keyBatch[key] = l.cur
			b = l.cur
		}
	}
	l.mu.Unlock()

	return func() (interface{}, error) {
		if b != nil {
			b.once.Do(func() { l.dispatch(b) })
			if b.err != nil {
				return nil, b.err
			}
		}
		// Missing key → zero V. Callers wanting "explicit null"
		// behavior can wrap with a pointer type so a missing key
		// surfaces as a typed-nil pointer.
		l.mu.Lock()
		v := l.result[key]
		l.mu.Unlock()
		return v, nil
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

// dispatch is the once-per-batch fetch call, gated by the batch's
// own sync.Once so every thunk from that batch shares one fetch.
// Closing the open batch (cur = nil) BEFORE fetching means keys
// loaded during or after the fetch — nested resolvers — collect into
// a fresh batch that dispatches on its own first dethunk.
func (l *Loader[K, V]) dispatch(b *batch[K, V]) {
	l.mu.Lock()
	if l.cur == b {
		l.cur = nil
	}
	ctx := l.batchCtx
	l.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	if len(b.keys) == 0 {
		return
	}
	res, err := l.fetch(ctx, b.keys)
	if err != nil {
		b.err = err
		return
	}
	l.mu.Lock()
	for k, v := range res {
		l.result[k] = v
	}
	l.mu.Unlock()
}

// Prime seeds the loader's cache with a key/value pair. Useful when
// a previous query already fetched the data and you want subsequent
// Load calls in the same request to skip the fetch.
func (l *Loader[K, V]) Prime(key K, value V) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.result[key] = value
}
