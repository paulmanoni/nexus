package auth_test

import (
	"context"
	"testing"

	"github.com/paulmanoni/nexus/extension/auth"
)

// meWrap is a wrapper Extra (e.g. a "me" payload that also carries roles)
// that exposes the underlying domain user through Principal — so
// auth.User[testUser] can reach the user inside it. inner lets a test build
// nested wrappers to exercise repeated unwrapping.
type meWrap struct {
	user  *testUser
	inner *meWrap
}

func (m *meWrap) Principal() any {
	if m.inner != nil {
		return m.inner
	}
	if m.user != nil {
		return m.user
	}
	return nil
}

// cyclic is a self-referential Principal — Principal() returns itself. The
// unwrap loop must terminate on it rather than spin forever.
type cyclic struct{ self *cyclic }

func (c *cyclic) Principal() any { return c.self }

// TestUser_PrincipalUnwrap covers the Principal unwrapping added to
// auth.User[T]: a wrapper Extra can expose the typed user via Principal(),
// nested wrappers unwrap repeatedly, value-typed Extras still resolve, and
// cyclic / nil Principals miss instead of looping or panicking.
func TestUser_PrincipalUnwrap(t *testing.T) {
	u := &testUser{Name: "ada"}

	// Wrapper Extra → unwraps to the underlying *testUser.
	ctx := auth.WithIdentity(context.Background(), &auth.Identity{ID: "1", Extra: &meWrap{user: u}})
	if got, ok := auth.User[testUser](ctx); !ok || got.Name != "ada" {
		t.Fatalf("Principal unwrap: got (%+v, %v); want ada", got, ok)
	}

	// Nested wrappers unwrap repeatedly.
	ctx = auth.WithIdentity(context.Background(), &auth.Identity{ID: "2", Extra: &meWrap{inner: &meWrap{user: u}}})
	if got, ok := auth.User[testUser](ctx); !ok || got.Name != "ada" {
		t.Fatalf("nested unwrap: got (%+v, %v); want ada", got, ok)
	}

	// Value-typed Extra still resolves (returned as a pointer).
	ctx = auth.WithIdentity(context.Background(), &auth.Identity{ID: "3", Extra: testUser{Name: "grace"}})
	if got, ok := auth.User[testUser](ctx); !ok || got.Name != "grace" {
		t.Fatalf("value Extra: got (%+v, %v); want grace", got, ok)
	}

	// Self-referential Principal must terminate → (nil, false), no hang.
	c := &cyclic{}
	c.self = c
	ctx = auth.WithIdentity(context.Background(), &auth.Identity{ID: "4", Extra: c})
	if _, ok := auth.User[testUser](ctx); ok {
		t.Fatal("cyclic Principal should miss, not loop")
	}

	// Principal() returning nil → miss.
	ctx = auth.WithIdentity(context.Background(), &auth.Identity{ID: "5", Extra: &meWrap{}})
	if _, ok := auth.User[testUser](ctx); ok {
		t.Fatal("nil Principal payload should miss")
	}

	// A wrapper whose Principal payload is the wrong type still misses.
	type other struct{ X int }
	ctx = auth.WithIdentity(context.Background(), &auth.Identity{ID: "6", Extra: &meWrap{user: u}})
	if _, ok := auth.User[other](ctx); ok {
		t.Fatal("wrong target type should miss")
	}
}
