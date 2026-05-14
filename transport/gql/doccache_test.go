package gql

import (
	"testing"

	"github.com/graphql-go/graphql/language/ast"
)

// TestDocumentCache_HitMiss exercises the basic get/put/eviction
// contract. We don't put real AST nodes through it here — those
// are covered by the end-to-end benchmark — just the bookkeeping.
func TestDocumentCache_HitMiss(t *testing.T) {
	c := NewDocumentCache(2)
	if c == nil {
		t.Fatal("expected non-nil cache for capacity 2")
	}

	a := &documentEntry{doc: &ast.Document{}, valid: true}
	b := &documentEntry{doc: &ast.Document{}, valid: true}

	// Miss on empty.
	if _, ok := c.Get("a"); ok {
		t.Fatal("expected miss on empty cache")
	}

	c.Put("a", a)
	got, ok := c.Get("a")
	if !ok || got != a {
		t.Fatalf("get after put: ok=%v got=%v", ok, got)
	}

	// Second key fits.
	c.Put("b", b)
	if got, ok := c.Get("b"); !ok || got != b {
		t.Fatalf("get b: ok=%v got=%v", ok, got)
	}

	stats := c.Stats()
	if stats.Hits != 2 || stats.Misses != 1 {
		t.Fatalf("counters: %+v", stats)
	}
	if stats.Size != 2 {
		t.Fatalf("size: %d", stats.Size)
	}
}

func TestDocumentCache_LRUEviction(t *testing.T) {
	c := NewDocumentCache(2)
	a := &documentEntry{}
	b := &documentEntry{}
	cEntry := &documentEntry{}

	c.Put("a", a)
	c.Put("b", b)
	// Touch "a" → "b" becomes LRU.
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a should hit")
	}
	c.Put("c", cEntry) // evicts "b"

	if _, ok := c.Get("b"); ok {
		t.Fatal("b should have been evicted (LRU)")
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a should still be cached (was touched after b)")
	}
	if _, ok := c.Get("c"); !ok {
		t.Fatal("c should be cached (just inserted)")
	}

	stats := c.Stats()
	if stats.Evictions != 1 {
		t.Fatalf("evictions: %d", stats.Evictions)
	}
	if stats.Size != 2 {
		t.Fatalf("size should stay at cap: %d", stats.Size)
	}
}

func TestDocumentCache_NilSafe(t *testing.T) {
	// NewDocumentCache(0) returns nil — calling Get/Put/Stats on
	// nil must not panic. Lets callers pass a single field with
	// "disable" semantics without branching.
	var c *DocumentCache
	if c2 := NewDocumentCache(0); c2 != nil {
		t.Fatal("NewDocumentCache(0) should return nil")
	}
	if _, ok := c.Get("anything"); ok {
		t.Fatal("nil cache should always miss")
	}
	c.Put("k", &documentEntry{}) // must not panic
	if s := c.Stats(); s != (DocumentCacheStats{}) {
		t.Fatalf("nil stats should be zero-value: %+v", s)
	}
}

func TestDocumentCache_OverwriteSameKey(t *testing.T) {
	// Put on an existing key replaces the entry and promotes to MRU,
	// rather than growing the cache or evicting another entry.
	c := NewDocumentCache(2)
	first := &documentEntry{}
	second := &documentEntry{}

	c.Put("a", first)
	c.Put("a", second)
	got, _ := c.Get("a")
	if got != second {
		t.Fatal("overwrite did not replace entry")
	}
	if c.Stats().Size != 1 {
		t.Fatalf("size should be 1, got %d", c.Stats().Size)
	}
	if c.Stats().Evictions != 0 {
		t.Fatalf("overwrite should not evict, got %d", c.Stats().Evictions)
	}
}
