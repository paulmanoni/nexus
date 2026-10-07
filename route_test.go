package nexus

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/httpx"
	"github.com/paulmanoni/nexus/v2/internal/maskhook"
)

type routeUsers struct{}

func newRouteUsers() *routeUsers { return &routeUsers{} }

func (*routeUsers) Index(ctx context.Context) ([]string, error)        { return nil, nil }
func (*routeUsers) Show(ctx context.Context, id int) (string, error)   { return strconv.Itoa(id), nil }
func (*routeUsers) Update(ctx context.Context, id int) (string, error) { return "", nil }

type routeFiles struct {
	Path string `path:"path"`
}

type routeSearch struct {
	ID   int    `path:"id"`
	Tab  string `query:"tab"`
	Page int    `query:"page"`
}

func listFiles(p Params[routeFiles]) (string, error) { return p.Args.Path, nil }

var (
	routeShowUser = AsRest("GET", "/users/:id", (*routeUsers).Show, Arg("id"))
	routeHome     = AsRest("GET", "/", func(c *httpx.Ctx) { c.String(200, URL(c.Request.Context(), "users:show", 7)) })
)

func bootRoutes(t *testing.T, cfg config.Runtime, opts ...Option) *App {
	t.Helper()
	app, stop, err := InProcess(cfg, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
	return app
}

func TestRouteURLs(t *testing.T) {
	app := bootRoutes(t, config.Runtime{Server: config.Server{RoutePrefix: "/api"}},
		Module("users", Path("/admin"), Provide(newRouteUsers),
			AsRest("GET", "/users", (*routeUsers).Index),
			routeShowUser,
			AsRest("GET", "/users/:id/", (*routeUsers).Show, Arg("id")),
			AsRest("PUT", "/users/:id", (*routeUsers).Update, Arg("id")),
			AsRest("PATCH", "/users/:id", (*routeUsers).Update, Arg("id")),
			AsRest("GET", "/files/*path", listFiles),
			AsRest("GET", "/search/:id", (*routeUsers).Show, Arg("id"), Name("search")),
		),
		routeHome,
	)
	ctx := WithApp(context.Background(), app)

	for _, c := range []struct {
		got, want string
	}{
		{routeShowUser.URL(ctx, 7), "/api/admin/users/7"},
		{URL(ctx, "users:show", 7), "/api/admin/users/7"},
		{URL(ctx, "users:show", P{"id": "a b"}), "/api/admin/users/a%20b"},
		{URL(ctx, "users:index", Query{"q": "x y", "tag": []string{"a", "b"}}), "/api/admin/users?q=x+y&tag=a&tag=b"},
		{URL(ctx, "users:update", 3), "/api/admin/users/3"},
		{URL(ctx, "users:listFiles", "docs/a b.txt"), "/api/admin/files/docs/a%20b.txt"},
		{URL(ctx, "users:search", routeSearch{ID: 4, Tab: "roles"}), "/api/admin/search/4?tab=roles"},
		{URL(ctx, "users:search", &routeSearch{ID: 4}), "/api/admin/search/4"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
	if routeShowUser.Method() != "GET" {
		t.Errorf("method %q", routeShowUser.Method())
	}

	if w := routerGet(t, app, "/api/"); w.Body.String() != "/api/admin/users/7" {
		t.Errorf("URL from a request's context: %q", w.Body.String())
	}

	for name, params := range map[string][]any{
		"users:shw":  {7},
		"users:show": {},
	} {
		if _, err := Reverse(ctx, name, params...); err == nil {
			t.Errorf("%s %v built", name, params)
		}
	}
	if _, err := Reverse(ctx, "users:shw", 7); err == nil || !strings.Contains(err.Error(), `did you mean "users:show"`) {
		t.Errorf("unknown name: %v", err)
	}
	if _, err := Reverse(ctx, "users:show", 1, 2); err == nil {
		t.Error("an extra positional parameter built")
	}
	if _, err := Reverse(ctx, "users:show", P{"pk": 1}); err == nil {
		t.Error("a parameter the path doesn't have built")
	}
	defer func() {
		if recover() == nil {
			t.Error("URL of an unknown name didn't panic in a test")
		}
	}()
	URL(ctx, "nope")
}

func TestRouteNames(t *testing.T) {
	app := bootRoutes(t, config.Runtime{},
		Module("a", Provide(newRouteUsers),
			AsRest("GET", "/a/:id", (*routeUsers).Show, Arg("id")),
			AsRest("GET", "/b/:id", (*routeUsers).Show, Arg("id")),
			AsRest("GET", "/c", (*routeUsers).Index),
			AsRest("GET", "/d", (*routeUsers).Index, Name("index")),
		),
	)
	ctx := WithApp(context.Background(), app)
	if _, err := Reverse(ctx, "a:show", 1); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("default names clashing: %v", err)
	}
	if got, _ := Reverse(ctx, "a:index"); got != "/d" {
		t.Errorf("an explicit name over a default one: %q", got)
	}
	routes := map[string]string{}
	for _, r := range app.Routes() {
		routes[r.Name] = r.Method + " " + r.Path
	}
	if routes["a:index"] != "GET /d" || routes["a:show"] != "" {
		t.Errorf("routes: %v", routes)
	}
	named := map[string]string{}
	for _, r := range app.Registry().Routes() {
		named[r.Path] = r.Route
	}
	if named["/d"] != "a:index" || named["/c"] != "" || named["/a/:id"] != "" {
		t.Errorf("registry route names: %v", named)
	}

	_, _, err := InProcess(config.Runtime{},
		Module("b", Provide(newRouteUsers),
			AsRest("GET", "/x", (*routeUsers).Index, Name("same")),
			AsRest("GET", "/y", (*routeUsers).Index, Name("same")),
		),
	)
	if err == nil || !strings.Contains(err.Error(), `two routes are named "b:same"`) {
		t.Errorf("explicit duplicate: %v", err)
	}
}

type routeOrders struct{}

func (*routeOrders) Show(ctx context.Context, id int) (string, error) { return "", nil }

func TestRouterNamespaces(t *testing.T) {
	orders := Controller[*routeOrders]("/orders").Provide(func() *routeOrders { return &routeOrders{} }).
		Get("/:id", (*routeOrders).Show)
	billing := NewRouter("billing", "/billing").Include(orders.Router)
	billing.Rest("GET", "/invoices", (*routeUsers).Index)
	v1 := NewRouter("v1", "/v1").Include(billing)
	app := bootRoutes(t, config.Runtime{}, Provide(newRouteUsers), v1)
	ctx := WithApp(context.Background(), app)

	if got := URL(ctx, "v1:billing:routeOrders:show", 5); got != "/v1/billing/orders/5" {
		t.Errorf("stacked namespace: %q", got)
	}
	if got := orders.URL(ctx, (*routeOrders).Show, 5); got != "/v1/billing/orders/5" {
		t.Errorf("controller action: %q", got)
	}
	if got := billing.URL(ctx, "index"); got != "/v1/billing/invoices" {
		t.Errorf("router-relative name: %q", got)
	}
	if got := URL(context.Background(), "v1:billing:index"); got != "/v1/billing/invoices" {
		t.Errorf("the one running app, without a request: %q", got)
	}
}

func TestRouteMasksIDs(t *testing.T) {
	maskhook.Install(maskhook.Hooks{
		IsID: func(k string) bool { return k == "id" },
		Mask: func(_ string, n int64) (string, bool) { return "m" + strconv.FormatInt(n, 10), true },
		Unmask: func(_, s string) (int64, bool) {
			n, err := strconv.ParseInt(strings.TrimPrefix(s, "m"), 10, 64)
			return n, err == nil
		},
	})
	t.Cleanup(maskhook.Uninstall)
	show := AsRest("GET", "/users/:id", (*routeUsers).Show, Arg("id"))
	app := bootRoutes(t, config.Runtime{}, Module("u", Provide(newRouteUsers), show))
	ctx := WithApp(context.Background(), app)
	if got := show.URL(ctx, 7, Query{"id": 8, "n": 9}); got != "/users/m7?id=m8&n=9" {
		t.Errorf("masked: %q", got)
	}
}
