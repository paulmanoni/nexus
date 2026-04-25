// Package users is the canonical "owns its data, exposes typed REST"
// module: a minimal user catalog tagged DeployAs("users-svc") so it
// can be peeled out into its own binary later.
//
// The Module declaration here is what `nexus gen clients` reads to
// emit zz_users_client_gen.go alongside this file. Other modules
// (e.g. checkout) consume `users.UsersClient` instead of touching
// the in-memory store directly — the codegen makes the local-vs-
// remote choice transparent at construction time.
package users

import (
	"fmt"
	"sync"

	"github.com/paulmanoni/nexus"
)

// User is the public shape returned over the wire. Generated client
// code lives in this package, so it sees this type without an import
// and consumers in other packages get it via `users.User`.
type User struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Service holds the in-memory store. Real implementations would back
// it with a DB; this stays simple to keep the example focused on the
// codegen + cross-module wiring.
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

// GetArgs binds a path parameter named `id`. The framework's gin
// binding reads the `uri:"id"` tag; the generated client's path
// expansion reads the same tag — guaranteed to round-trip.
type GetArgs struct {
	ID string `json:"id" uri:"id"`
}

// NewGet returns a typed user by id. 404 surfaces as a *RemoteError
// on the client side (see the generated stub).
func NewGet(svc *Service, p nexus.Params[GetArgs]) (*User, error) {
	svc.mu.RLock()
	defer svc.mu.RUnlock()
	u, ok := svc.users[p.Args.ID]
	if !ok {
		return nil, fmt.Errorf("user %q not found", p.Args.ID)
	}
	return &u, nil
}

// ListArgs is empty — kept as a real type (rather than struct{}) so the
// generated client method has a stable shape if filters are added.
type ListArgs struct{}

// NewList streams every user. In a real service this would paginate;
// for the demo, just return the full slice.
func NewList(svc *Service, p nexus.Params[ListArgs]) ([]*User, error) {
	svc.mu.RLock()
	defer svc.mu.RUnlock()
	out := make([]*User, 0, len(svc.users))
	for _, u := range svc.users {
		uu := u
		out = append(out, &uu)
	}
	return out, nil
}

// SearchArgs is the typed input for a GraphQL query — `nexus gen
// clients` uses the same Params[T] pattern AsRest does.
type SearchArgs struct {
	Prefix string `graphql:"prefix" json:"prefix"`
}

// NewSearch is a GraphQL Query handler returning users whose names
// begin with prefix. Demonstrates the codegen emitting a typed
// GraphQL method on the generated client.
func NewSearch(svc *Service, p nexus.Params[SearchArgs]) ([]*User, error) {
	svc.mu.RLock()
	defer svc.mu.RUnlock()
	out := make([]*User, 0, len(svc.users))
	for _, u := range svc.users {
		if p.Args.Prefix == "" || startsWith(u.Name, p.Args.Prefix) {
			uu := u
			out = append(out, &uu)
		}
	}
	return out, nil
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// Module is the wired declaration. DeployAs("users-svc") is the
// hint that drives codegen — without it, this module is "always
// local" and no client stub is emitted.
var Module = nexus.Module("users",
	nexus.DeployAs("users-svc"),
	nexus.Provide(NewService),
	nexus.AsRest("GET", "/users/:id", NewGet),
	nexus.AsRest("GET", "/users", NewList),
	nexus.AsQuery(NewSearch),
)