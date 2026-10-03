package auth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/extension/auth"
	"github.com/paulmanoni/nexus/v2/registry"
)

func TestPermsWildcards(t *testing.T) {
	id := &auth.Identity{Perms: []string{"orders.*", "users.view"}, Roles: []string{"legacy"}}
	for perm, want := range map[string]bool{
		"orders.view":           true,
		"orders.refunds.create": true,
		"users.view":            true,
		"users.edit":            false,
		"legacy":                true,
		"ordersx":               false,
	} {
		if got := id.Has(perm); got != want {
			t.Errorf("Has(%q) = %v, want %v", perm, got, want)
		}
	}
	if !(&auth.Identity{Perms: []string{"*"}}).Has("anything") {
		t.Fatal(`"*" grants everything`)
	}
	ctx := auth.WithIdentity(context.Background(), id)
	if !auth.Can(ctx, "orders.refund") || auth.Can(ctx, "users.edit") {
		t.Fatal("Can follows Perms")
	}
	if !auth.AnyOf("users.edit", "orders.view")(id, nil) {
		t.Fatal("AnyOf follows Perms")
	}
}

func TestCurrentAndID(t *testing.T) {
	if auth.Current(context.Background()) != nil {
		t.Fatal("anonymous: Current is nil")
	}
	ctx := auth.WithIdentity(context.Background(), &auth.Identity{ID: "42", Kind: "staff"})
	if me := auth.Current(ctx); me == nil || me.Kind != "staff" {
		t.Fatalf("Current = %+v", me)
	}
	if id, ok := auth.ID[int64](ctx); !ok || id != 42 {
		t.Fatalf("ID = %d, %v", id, ok)
	}
}

type gateOK struct {
	OK bool `json:"ok"`
}

func refund(ctx context.Context) (*gateOK, error)  { return &gateOK{OK: true}, nil }
func archive(ctx context.Context) (*gateOK, error) { return &gateOK{OK: true}, nil }
func staffQ(ctx context.Context) (*gateOK, error)  { return &gateOK{OK: true}, nil }

var people = map[string]*auth.Identity{
	"clerk":    {ID: "1", Kind: "staff", Perms: []string{"orders.view"}},
	"manager":  {ID: "2", Kind: "staff", Perms: []string{"orders.*"}},
	"customer": {ID: "3", Kind: "customer", Perms: []string{"orders.*"}},
}

func gateApp(t *testing.T) (*nexus.App, *httptest.Server) {
	t.Helper()
	app, stop, err := nexus.InProcess(config.Runtime{},
		auth.Module(auth.Config{Authentication: auth.Authentication{Schemes: []auth.Scheme{{
			Extract: auth.Bearer(),
			Resolve: func(ctx context.Context, tok string) (*auth.Identity, error) {
				if id, ok := people[tok]; ok {
					return id, nil
				}
				return nil, auth.ErrUnauthenticated
			},
		}}}}),
		nexus.AsRest("POST", "/refund", refund, auth.Kind("staff"), auth.RequiresAny("orders.refund", "orders.admin")),
		nexus.AsRest("POST", "/archive", archive, auth.RequiresAny("orders.archive"), auth.RequiresAny("orders.view")),
		nexus.AsQuery(staffQ, auth.Kind("staff")),
	)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app)
	t.Cleanup(func() { srv.Close(); _ = stop(context.Background()) })
	return app, srv
}

func TestKindAndRequiresAnyGates(t *testing.T) {
	app, srv := gateApp(t)
	call := func(path, who string) int {
		req, _ := http.NewRequest("POST", srv.URL+path, nil)
		if who != "" {
			req.Header.Set("Authorization", "Bearer "+who)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	for _, c := range []struct {
		path, who string
		want      int
	}{
		{"/refund", "", 401},
		{"/refund", "clerk", 403},    // staff, but neither permission
		{"/refund", "manager", 201},  // orders.* grants orders.refund
		{"/refund", "customer", 403}, // the permission, not the kind
		{"/archive", "manager", 201}, // both RequiresAny groups pass
		{"/archive", "clerk", 403},   // orders.view but not orders.archive
	} {
		if got := call(c.path, c.who); got != c.want {
			t.Errorf("%s as %q = %d, want %d", c.path, c.who, got, c.want)
		}
	}

	tags := map[string]map[string]string{}
	for _, e := range app.Registry().Endpoints() {
		tags[e.Name] = e.Tags
	}
	if got := tags["staffQ"][registry.AuthKindTag]; got != "staff" {
		t.Fatalf("staffQ kind tag = %q", got)
	}
	if got := tags["POST /archive"][registry.AuthRequiresAnyTag]; got != "orders.archive;orders.view" {
		t.Fatalf("archive requires_any tag = %q (tags %v)", got, tags)
	}

	for who, want := range map[string]map[string]bool{
		"clerk":    {"staffQ": true, "POST /refund": false, "POST /archive": false},
		"manager":  {"staffQ": true, "POST /refund": true, "POST /archive": true},
		"customer": {"staffQ": false, "POST /refund": false, "POST /archive": true},
	} {
		g := auth.OpGates(auth.WithIdentity(context.Background(), people[who]), app)
		for op, w := range want {
			if g[op] != w {
				t.Errorf("OpGates as %s: %s = %v, want %v", who, op, g[op], w)
			}
		}
	}
	if g := auth.OpGates(context.Background(), app); g["staffQ"] {
		t.Fatal("anonymous fails a Kind gate")
	}
}
