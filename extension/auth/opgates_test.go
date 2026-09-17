package auth

import (
	"context"
	"testing"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/registry"
)

type gateOut struct {
	OK bool `json:"ok"`
}

func listGated(ctx context.Context) (*gateOut, error)   { return &gateOut{OK: true}, nil }
func showGated(ctx context.Context) (*gateOut, error)   { return &gateOut{OK: true}, nil }
func addGated(ctx context.Context) (*gateOut, error)    { return &gateOut{OK: true}, nil }
func openHandler(ctx context.Context) (*gateOut, error) { return &gateOut{OK: true}, nil }

func gatesApp(t *testing.T) (*nexus.App, func()) {
	t.Helper()
	app, stop, err := nexus.InProcess(nexus.Config{},
		nexus.AsQuery(listGated, Requires("view_user")),
		nexus.AsQuery(showGated, Requires("view_user")),
		nexus.AsMutation(addGated, Requires("add_user", "change_user")),
		nexus.AsQuery(openHandler),
	)
	if err != nil {
		t.Fatal(err)
	}
	return app, func() { _ = stop(context.Background()) }
}

// Requires must stamp its permission list onto the endpoint's registry
// tags — the declaration OpGates evaluates is the one the gate enforces.
func TestRequiresStampsRegistryTag(t *testing.T) {
	app, done := gatesApp(t)
	defer done()

	tags := map[string]string{}
	for _, e := range app.Registry().Endpoints() {
		tags[e.Name] = e.Tags[registry.AuthRequiresTag]
	}
	if tags["listGated"] != "view_user" {
		t.Fatalf("listGated tag = %q", tags["listGated"])
	}
	if tags["addGated"] != "add_user,change_user" {
		t.Fatalf("addGated tag = %q", tags["addGated"])
	}
	if tags["openHandler"] != "" {
		t.Fatalf("openHandler must carry no requires tag, got %q", tags["openHandler"])
	}
}

func TestOpGates(t *testing.T) {
	app, done := gatesApp(t)
	defer done()

	viewer := WithIdentity(context.Background(), &Identity{ID: "u1", Roles: []string{"view_user"}})
	g := OpGates(viewer, app)
	if !g["listGated"] || !g["showGated"] {
		t.Fatalf("viewer must pass view_user ops: %v", g)
	}
	if g["addGated"] {
		t.Fatalf("viewer must fail add_user+change_user: %v", g)
	}
	if !g["openHandler"] {
		t.Fatalf("ungated op must be true: %v", g)
	}

	anon := OpGates(context.Background(), app)
	if anon["listGated"] || anon["addGated"] {
		t.Fatalf("anonymous must fail every gated op: %v", anon)
	}
	if !anon["openHandler"] {
		t.Fatalf("ungated op stays true for anonymous: %v", anon)
	}

	admin := WithIdentity(context.Background(), &Identity{ID: "u2", Roles: []string{"view_user", "add_user", "change_user"}})
	g = OpGates(admin, app)
	if !g["listGated"] || !g["addGated"] {
		t.Fatalf("admin must pass everything: %v", g)
	}
}

// The compiled gate table is cached per registry version: same version →
// same pointer, and ops sharing a permission set share one group so a
// call costs one check per UNIQUE set.
func TestOpGatesTableCachedAndGrouped(t *testing.T) {
	app, done := gatesApp(t)
	defer done()

	t1 := gateTableFor(app)
	t2 := gateTableFor(app)
	if t1 != t2 {
		t.Fatal("gate table must be reused while the registry is unchanged")
	}
	var viewGroup *gateGroup
	for i := range t1.groups {
		if len(t1.groups[i].perms) == 1 && t1.groups[i].perms[0] == "view_user" {
			viewGroup = &t1.groups[i]
		}
	}
	if viewGroup == nil || len(viewGroup.ops) != 2 {
		t.Fatalf("listGated+showGated must share one view_user group: %+v", t1.groups)
	}

	app.Registry().RegisterEndpoint(registry.Endpoint{Name: "lateOp"})
	t3 := gateTableFor(app)
	if t3 == t1 {
		t.Fatal("registry mutation must invalidate the cached table")
	}
}

func BenchmarkOpGates(b *testing.B) {
	app, stop, err := nexus.InProcess(nexus.Config{},
		nexus.AsQuery(listGated, Requires("view_user")),
		nexus.AsQuery(showGated, Requires("view_user")),
		nexus.AsMutation(addGated, Requires("add_user", "change_user")),
		nexus.AsQuery(openHandler),
	)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()
	// Pad the registry to ~200 ops across 20 unique permission sets.
	for i := 0; i < 196; i++ {
		app.Registry().RegisterEndpoint(registry.Endpoint{
			Name: "op" + string(rune('A'+i%26)) + string(rune('a'+i/26)),
			Tags: map[string]string{registry.AuthRequiresTag: "perm_" + string(rune('a'+i%20))},
		})
	}
	ctx := WithIdentity(context.Background(), &Identity{ID: "u1", Roles: []string{"view_user", "perm_a", "perm_b"}})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		OpGates(ctx, app)
	}
}
