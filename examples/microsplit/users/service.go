package users

import (
	"context"
	"fmt"
	"sync"

	"github.com/paulmanoni/nexus"
)

// Service holds the in-memory store. Real implementations would back
// it with a DB; this stays simple to keep the example focused on the
// build-time transport switch. In binaries that don't own users, the
// shadow generator replaces this struct with an HTTP-stub variant
// whose methods route over PeerCaller — same public method set, same
// type identifier, different body.
type Service struct {
	*nexus.Service
	cache *Cache
	mu    sync.RWMutex
	users map[string]User
}

// NewService takes *Cache as a dep so the dashboard's auto-attach pass
// records "User service uses users-cache" — that draws an edge from
// the service card to the cache resource node.
func NewService(app *nexus.App, cache *Cache) *Service {
	return &Service{
		Service: app.Service("User service").Describe("User catalog"),
		cache:   cache,
		users: map[string]User{
			"u1": {ID: "u1", Name: "Alice"},
			"u2": {ID: "u2", Name: "Bob"},
		},
	}
}

// Plain methods on Service form the public cross-module surface. The
// shadow generator reads these signatures to produce matching HTTP-
// stub methods in binaries that consume users remotely.

// Get returns a typed user by id. Reads the hot cache first so the
// dashboard's edge from the service to the users-cache resource sees
// activity on every successful lookup.
func (s *Service) Get(ctx context.Context, args GetArgs) (*User, error) {
	if u, ok := s.cache.Get(args.ID); ok {
		return &u, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.users[args.ID]
	if !ok {
		return nil, fmt.Errorf("user %q not found", args.ID)
	}
	s.cache.Put(u)
	return &u, nil
}

// Create inserts a new user under a generated id and warms the cache.
// Backs the createUser GraphQL mutation registered in module.go.
func (s *Service) Create(ctx context.Context, args CreateArgs) (*User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := fmt.Sprintf("u%d", len(s.users)+1)
	u := User{ID: id, Name: args.Name}
	s.users[id] = u
	s.cache.Put(u)
	return &u, nil
}

// List streams every user.
func (s *Service) List(ctx context.Context, args ListArgs) ([]*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*User, 0, len(s.users))
	for _, u := range s.users {
		uu := u
		out = append(out, &uu)
	}
	return out, nil
}

// Search returns users whose names begin with the supplied prefix.
func (s *Service) Search(ctx context.Context, args SearchArgs) ([]*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*User, 0, len(s.users))
	for _, u := range s.users {
		if args.Prefix == "" || startsWith(u.Name, args.Prefix) {
			uu := u
			out = append(out, &uu)
		}
	}
	return out, nil
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
