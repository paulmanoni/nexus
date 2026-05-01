package users

import (
	"sync"
	"time"

	"github.com/paulmanoni/nexus/resource"
)

// Cache is a tiny in-memory cache fronting the user store. It exists as
// its own type so the dashboard can render it as a Resource (cache
// kind) and draw an edge from the User service constructor — which
// takes *Cache as a dep — onto the cache node. Real apps would back
// this with Redis; the demo keeps it simple.
//
// nexus.Provide(NewCache) is enough to register: the framework's
// auto-mount picks up the NexusResources() method below at boot.
type Cache struct {
	mu      sync.RWMutex
	items   map[string]cacheEntry
	healthy bool
}

type cacheEntry struct {
	user User
	seen time.Time
}

func NewCache() *Cache { return &Cache{items: map[string]cacheEntry{}, healthy: true} }

// NexusResources advertises this cache to the dashboard. The Service
// constructor takes *Cache as a parameter; the resource auto-attach
// pass walks that signature and draws the edge.
func (c *Cache) NexusResources() []resource.Resource {
	return []resource.Resource{
		resource.NewCache(
			"users-cache",
			"Hot user lookup cache",
			map[string]any{"backend": "memory", "ttl": "30s"},
			c.IsConnected,
			resource.AsDefault(),
		),
	}
}

func (c *Cache) IsConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.healthy
}

func (c *Cache) Put(u User) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[u.ID] = cacheEntry{user: u, seen: time.Now()}
}

func (c *Cache) Get(id string) (User, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.items[id]
	if !ok {
		return User{}, false
	}
	return e.user, true
}

func (c *Cache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}