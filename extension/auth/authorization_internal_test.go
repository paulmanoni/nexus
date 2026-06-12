package auth

import "testing"

func TestWildcardAuthority(t *testing.T) {
	w := Wildcard()
	cases := []struct {
		granted, required string
		want              bool
	}{
		{"admin:*", "admin:read", true},
		{"admin:*", "admin:users:delete", true},
		{"admin:*", "billing:read", false},
		{"*", "anything", true},
		{"admin", "admin", true},
		{"admin", "admin:read", false}, // no trailing * → exact only
		{"admin:read", "admin:read", true},
	}
	for _, c := range cases {
		if got := w(c.granted, c.required); got != c.want {
			t.Errorf("Wildcard()(%q, %q) = %v, want %v", c.granted, c.required, got, c.want)
		}
	}
}

func TestExactAuthorityIsDefault(t *testing.T) {
	if !ExactAuthority("a", "a") || ExactAuthority("a", "b") {
		t.Fatal("ExactAuthority must be plain string equality")
	}
}

func TestAuthorization_PermissionFn(t *testing.T) {
	id := &Identity{Roles: []string{"admin:*"}, Scopes: []string{"read"}}

	// Zero value → exact match: the literal role "admin:*" does NOT
	// satisfy "admin:read"; the scope "read" does.
	exact := Authorization{}.permissionFn()
	if exact(id, []string{"admin:read"}) {
		t.Error("exact: \"admin:*\" should not match \"admin:read\"")
	}
	if !exact(id, []string{"read"}) {
		t.Error("exact: \"read\" should match")
	}

	// Wildcard authority → "admin:*" now satisfies "admin:read", and the
	// all-required semantics still hold.
	wild := Authorization{Authority: Wildcard()}.permissionFn()
	if !wild(id, []string{"admin:read"}) {
		t.Error("wildcard: \"admin:*\" should match \"admin:read\"")
	}
	if !wild(id, []string{"admin:read", "read"}) {
		t.Error("wildcard: all of {admin:read, read} should pass")
	}
	if wild(id, []string{"admin:read", "write"}) {
		t.Error("wildcard: missing \"write\" should fail the all-required check")
	}

	// An explicit Permissions override wins over Authority.
	called := false
	override := Authorization{
		Authority:   Wildcard(),
		Permissions: func(*Identity, []string) bool { called = true; return false },
	}.permissionFn()
	if override(id, []string{"admin:read"}) || !called {
		t.Error("Permissions override should win over Authority and be invoked")
	}

	// nil identity always fails.
	if wild(nil, []string{"read"}) {
		t.Error("nil identity must never satisfy a requirement")
	}
}
