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
	mu    sync.RWMutex
	users map[string]User
}

func NewService(app *nexus.App) *Service {
	return &Service{
		Service: app.Service("users").Describe("User catalog"),
		users: map[string]User{
			"u1": {ID: "u1", Name: "Alice"},
			"u2": {ID: "u2", Name: "Bob"},
		},
	}
}

// Plain methods on Service form the public cross-module surface. The
// shadow generator reads these signatures to produce matching HTTP-
// stub methods in binaries that consume users remotely.

// Get returns a typed user by id.
func (s *Service) Get(ctx context.Context, args GetArgs) (*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.users[args.ID]
	if !ok {
		return nil, fmt.Errorf("user %q not found", args.ID)
	}
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
