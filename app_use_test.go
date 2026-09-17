package nexus

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/httpx"

	"github.com/paulmanoni/nexus/graph"
	"github.com/paulmanoni/nexus/middleware"
)

func ginOnlyBundle(name string) middleware.Middleware {
	return middleware.Middleware{Name: name, Gin: func(*httpx.Ctx) {}}
}

func graphOnlyBundle(name string) middleware.Middleware {
	return middleware.Middleware{
		Name:  name,
		Graph: func(next graph.FieldResolveFn) graph.FieldResolveFn { return next },
	}
}

func bothBundle(name string) middleware.Middleware {
	return middleware.Middleware{
		Name:  name,
		Gin:   func(*httpx.Ctx) {},
		Graph: func(next graph.FieldResolveFn) graph.FieldResolveFn { return next },
	}
}

func TestCheckBundleTransports(t *testing.T) {
	tests := []struct {
		name    string
		bundles []middleware.Middleware
		on      middleware.Transport
		wantErr bool
	}{
		{"gin bundle on REST passes", []middleware.Middleware{ginOnlyBundle("a")}, middleware.TransportREST, false},
		{"gin bundle on WS passes", []middleware.Middleware{ginOnlyBundle("a")}, middleware.TransportWebSocket, false},
		{"gin bundle on GraphQL fails", []middleware.Middleware{ginOnlyBundle("a")}, middleware.TransportGraphQL, true},
		{"graph bundle on GraphQL passes", []middleware.Middleware{graphOnlyBundle("b")}, middleware.TransportGraphQL, false},
		{"graph bundle on REST fails", []middleware.Middleware{graphOnlyBundle("b")}, middleware.TransportREST, true},
		{"both-transport bundle on any passes", []middleware.Middleware{bothBundle("c")}, middleware.TransportGraphQL, false},
		{"empty/metadata bundle is allowed anywhere", []middleware.Middleware{{Name: "label"}}, middleware.TransportGraphQL, false},
		{"no bundles passes", nil, middleware.TransportREST, false},
		{"first bad bundle reported among many", []middleware.Middleware{bothBundle("ok"), ginOnlyBundle("bad")}, middleware.TransportGraphQL, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkBundleTransports(tc.bundles, tc.on, "someOp")
			if tc.wantErr != (err != nil) {
				t.Fatalf("wantErr=%v, got err=%v", tc.wantErr, err)
			}
		})
	}
}

func TestCheckBundleTransportsMessage(t *testing.T) {
	err := checkBundleTransports([]middleware.Middleware{ginOnlyBundle("auth:custom")}, middleware.TransportGraphQL, "createAdvert")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"auth:custom", "GraphQL", "createAdvert", "UseOnGraph"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message missing %q: %s", want, err.Error())
		}
	}
}

// [runtime.server] strip_trailing_slash: "/users/" serves the "/users"
// route via an internal rewrite at the App boundary — no redirect, bodies
// intact, identical on every router backend.
func TestStripTrailingSlash(t *testing.T) {
	app, stop, err := InProcess(Config{Server: ServerConfig{StripTrailingSlash: true}},
		AsRestHandler("GET", "/users", func() httpx.HandlerFunc {
			return func(c *httpx.Ctx) { c.String(200, "list") }
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()

	for _, path := range []string{"/users", "/users/", "/users//"} {
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || w.Body.String() != "list" {
			t.Fatalf("GET %s = %d %q", path, w.Code, w.Body.String())
		}
	}

	// Off by default: the trailing-slash spelling stays a 404.
	app2, stop2, err := InProcess(Config{},
		AsRestHandler("GET", "/users", func() httpx.HandlerFunc {
			return func(c *httpx.Ctx) { c.String(200, "list") }
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop2(context.Background()) }()
	w := httptest.NewRecorder()
	app2.ServeHTTP(w, httptest.NewRequest("GET", "/users/", nil))
	if w.Code == 200 {
		t.Fatal("strip must be opt-in")
	}
}
