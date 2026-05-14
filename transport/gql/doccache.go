package gql

import (
	"container/list"
	"sync"
	"sync/atomic"

	"github.com/graphql-go/graphql/gqlerrors"
	"github.com/graphql-go/graphql/language/ast"
)

// DocumentCache memoizes the parse + validate work that graphql.Do
// otherwise repeats on every request. Profiling under the GraphQL
// hot path showed ~89% of allocations come from those two phases —
// the validator alone walks the AST through ~15 named rules per
// request. Since both phases are pure functions of (query string,
// schema), they're trivially cacheable, and the schema is built
// once at app boot.
//
// The cache keys on the raw query string. Two identical queries
// reuse the same parsed AST and validation verdict; the variables
// and operation name are still applied per-request inside Execute.
//
// Eviction is LRU under a single mutex. That's good enough for
// realistic workloads (an app typically has dozens, not millions,
// of distinct queries) and keeps the implementation small. If
// contention ever shows up in profiles, sharding by hash(key)%N
// is a small change.
//
// A nil *DocumentCache is a valid no-op — callers can pass nil
// to bypass caching without a separate code path.
type DocumentCache struct {
	mu    sync.Mutex
	cap   int
	items map[string]*list.Element
	order *list.List // front = MRU

	hits      atomic.Uint64
	misses    atomic.Uint64
	evictions atomic.Uint64
}

// documentEntry is what we cache: the parsed AST plus the validator's
// verdict. Storing the (FormattedError) slice lets us skip even the
// error-formatting cost on a repeat-bad query.
type documentEntry struct {
	doc     *ast.Document
	valErrs []gqlerrors.FormattedError
	valid   bool
}

type lruRecord struct {
	key   string
	entry *documentEntry
}

// NewDocumentCache returns a cache holding up to capacity distinct
// queries. capacity <= 0 returns nil — useful for "cache disabled"
// configs that still want to call cache methods unconditionally.
func NewDocumentCache(capacity int) *DocumentCache {
	if capacity <= 0 {
		return nil
	}
	return &DocumentCache{
		cap:   capacity,
		items: make(map[string]*list.Element, capacity),
		order: list.New(),
	}
}

// Get returns the cached entry for key, promoting it to most-recently-
// used. The second return is false on miss; on miss the caller is
// expected to parse + validate and then call Put.
//
// Safe to call on a nil receiver — always reports a miss.
func (c *DocumentCache) Get(key string) (*documentEntry, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	el, ok := c.items[key]
	if !ok {
		c.mu.Unlock()
		c.misses.Add(1)
		return nil, false
	}
	c.order.MoveToFront(el)
	entry := el.Value.(*lruRecord).entry
	c.mu.Unlock()
	c.hits.Add(1)
	return entry, true
}

// Put inserts entry under key, evicting the least-recently-used record
// when the cache is at capacity. If key already exists, its entry is
// replaced and the position promoted to MRU.
//
// Safe to call on a nil receiver — drops silently.
func (c *DocumentCache) Put(key string, entry *documentEntry) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		el.Value.(*lruRecord).entry = entry
		c.order.MoveToFront(el)
		return
	}
	el := c.order.PushFront(&lruRecord{key: key, entry: entry})
	c.items[key] = el
	if c.order.Len() > c.cap {
		oldest := c.order.Back()
		if oldest != nil {
			rec := oldest.Value.(*lruRecord)
			delete(c.items, rec.key)
			c.order.Remove(oldest)
			c.evictions.Add(1)
		}
	}
}

// DocumentCacheStats is a snapshot of cache counters. The dashboard
// surfaces these so operators can verify the cache is hitting; a low
// hit ratio in production usually means clients are sending queries
// with embedded variable values (use $vars instead).
type DocumentCacheStats struct {
	Size      int
	Capacity  int
	Hits      uint64
	Misses    uint64
	Evictions uint64
}

// Stats returns a snapshot of counters. Safe on nil (returns zero
// value).
func (c *DocumentCache) Stats() DocumentCacheStats {
	if c == nil {
		return DocumentCacheStats{}
	}
	c.mu.Lock()
	size := c.order.Len()
	c.mu.Unlock()
	return DocumentCacheStats{
		Size:      size,
		Capacity:  c.cap,
		Hits:      c.hits.Load(),
		Misses:    c.misses.Load(),
		Evictions: c.evictions.Load(),
	}
}
