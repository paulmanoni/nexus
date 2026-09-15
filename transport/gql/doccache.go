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
// Eviction is LRU per shard. The cache is sharded by hash(key) so the
// per-mount mutex — previously the single serialization point of the
// whole GraphQL path — divides across independent locks; LRU order is
// therefore per shard, which for eviction quality is indistinguishable
// at realistic capacities (an app has dozens, not millions, of
// distinct queries).
//
// A nil *DocumentCache is a valid no-op — callers can pass nil
// to bypass caching without a separate code path.
type DocumentCache struct {
	shards   []docShard
	mask     uint32 // len(shards)-1; len is a power of two
	capTotal int

	hits      atomic.Uint64
	misses    atomic.Uint64
	evictions atomic.Uint64
}

// docCacheShards is the shard count for full-size caches — a power of
// two so the shard pick is a mask. Small caches (capacity below
// docCacheShards²) stay single-shard: they keep exact global LRU
// order, and a cache that small implies traffic that can't contend.
const docCacheShards = 16

type docShard struct {
	mu    sync.Mutex
	cap   int
	items map[string]*list.Element
	order *list.List // front = MRU
}

// shardFor picks a shard by FNV-1a over the query string.
func (c *DocumentCache) shardFor(key string) *docShard {
	if c.mask == 0 {
		return &c.shards[0]
	}
	h := uint32(2166136261)
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= 16777619
	}
	return &c.shards[h&c.mask]
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
	n := 1
	if capacity >= docCacheShards*docCacheShards {
		n = docCacheShards
	}
	c := &DocumentCache{
		shards:   make([]docShard, n),
		mask:     uint32(n - 1),
		capTotal: capacity,
	}
	perShard, extra := capacity/n, capacity%n
	for i := range c.shards {
		sc := perShard
		if i < extra {
			sc++
		}
		c.shards[i] = docShard{
			cap:   sc,
			items: make(map[string]*list.Element, sc),
			order: list.New(),
		}
	}
	return c
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
	sh := c.shardFor(key)
	sh.mu.Lock()
	el, ok := sh.items[key]
	if !ok {
		sh.mu.Unlock()
		c.misses.Add(1)
		return nil, false
	}
	sh.order.MoveToFront(el)
	entry := el.Value.(*lruRecord).entry
	sh.mu.Unlock()
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
	sh := c.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if el, ok := sh.items[key]; ok {
		el.Value.(*lruRecord).entry = entry
		sh.order.MoveToFront(el)
		return
	}
	el := sh.order.PushFront(&lruRecord{key: key, entry: entry})
	sh.items[key] = el
	if sh.order.Len() > sh.cap {
		oldest := sh.order.Back()
		if oldest != nil {
			rec := oldest.Value.(*lruRecord)
			delete(sh.items, rec.key)
			sh.order.Remove(oldest)
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
	size := 0
	for i := range c.shards {
		sh := &c.shards[i]
		sh.mu.Lock()
		size += sh.order.Len()
		sh.mu.Unlock()
	}
	return DocumentCacheStats{
		Size:      size,
		Capacity:  c.capTotal,
		Hits:      c.hits.Load(),
		Misses:    c.misses.Load(),
		Evictions: c.evictions.Load(),
	}
}
