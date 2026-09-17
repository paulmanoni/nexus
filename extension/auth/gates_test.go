package auth

import (
	"context"
	"testing"
)

func TestCanAndGates(t *testing.T) {
	id := &Identity{ID: "u1", Roles: []string{"add_user", "view_user"}}
	ctx := WithIdentity(context.Background(), id)

	if !Can(ctx, "add_user") {
		t.Fatal("add_user should be allowed")
	}
	if Can(ctx, "delete_user") {
		t.Fatal("delete_user should be denied")
	}
	if Can(context.Background(), "add_user") {
		t.Fatal("anonymous must always be false")
	}

	g := Gates(ctx, "add_user", "view_user", "delete_user")
	if !g["add_user"] || !g["view_user"] || g["delete_user"] {
		t.Fatalf("gates = %v", g)
	}
	if len(g) != 3 {
		t.Fatalf("every requested permission must appear: %v", g)
	}

	anon := Gates(context.Background(), "add_user")
	if anon["add_user"] {
		t.Fatalf("anonymous gates must be false: %v", anon)
	}
}

// Gates must consult the same PermissionFn Requires() uses — a custom
// authorizer changes both, so UI toggles cannot drift from endpoint gates.
func TestGatesUseConfiguredPermissionFn(t *testing.T) {
	st := &moduleState{permissions: func(id *Identity, perms []string) bool {
		return perms[0] == "granted_by_custom_fn"
	}}
	ctx := WithIdentity(withState(context.Background(), st), &Identity{ID: "u2"})

	g := Gates(ctx, "granted_by_custom_fn", "add_user")
	if !g["granted_by_custom_fn"] || g["add_user"] {
		t.Fatalf("gates must ride the configured PermissionFn: %v", g)
	}
}
