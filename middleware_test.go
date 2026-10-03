package nexus

import (
	"context"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/graph"
	"github.com/paulmanoni/nexus/v2/httpx"
	"github.com/paulmanoni/nexus/v2/middleware"
)

func ginOnlyBundle(name string) middleware.Middleware {
	return middleware.Middleware{Name: name, HTTP: func(*httpx.Ctx) {}}
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
		HTTP:  func(*httpx.Ctx) {},
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
	app, stop, err := InProcess(config.Runtime{Server: config.Server{StripTrailingSlash: true}},
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
	app2, stop2, err := InProcess(config.Runtime{},
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

// App-wide middleware runs in stage order (Edge, Session, Auth, App), then
// declaration order, and a constructor gets its parameters from DI.
func TestAppMiddlewareStagesAndConstructors(t *testing.T) {
	type greeting struct{ text string }
	var order []string
	mark := func(name string, stage middleware.Stage) middleware.Middleware {
		return middleware.Middleware{Name: name, Stage: stage, HTTP: func(c *httpx.Ctx) {
			order = append(order, name)
			c.Next()
		}}
	}
	app, stop, err := InProcess(config.Runtime{},
		Supply(&greeting{text: "hello"}),
		Middleware(mark("app-1", middleware.App), mark("auth", middleware.Auth)),
		Middleware(func(g *greeting) middleware.Middleware {
			return middleware.Middleware{Name: "ctor", HTTP: func(c *httpx.Ctx) {
				order = append(order, "ctor:"+g.text)
				c.Next()
			}}
		}),
		Middleware(mark("edge", middleware.Edge), mark("session", middleware.Session)),
		AsRest("GET", "/ping", func() (string, error) { return "pong", nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(context.Background())

	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest("GET", "/ping", nil))
	want := []string{"edge", "session", "auth", "app-1", "ctor:hello"}
	if !slices.Equal(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
}

func TestAppMiddlewareRejectsOtherValues(t *testing.T) {
	_, _, err := InProcess(config.Runtime{}, Middleware(42))
	if err == nil || !strings.Contains(err.Error(), "neither a middleware.Middleware") {
		t.Fatalf("err = %v", err)
	}
}

// Every request carries the caller's address, read with nexus.ClientIP.
func TestClientIPOnREST(t *testing.T) {
	var got string
	app, stop, err := InProcess(config.Runtime{},
		AsRest("GET", "/ip", func(ctx context.Context) (string, error) {
			got = ClientIP(ctx)
			return got, nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(context.Background())
	req := httptest.NewRequest("GET", "/ip", nil)
	req.RemoteAddr = "203.0.113.7:51000"
	app.ServeHTTP(httptest.NewRecorder(), req)
	if got != "203.0.113.7" {
		t.Fatalf("ClientIP = %q", got)
	}
}

// Bodies are capped at 32MB by default; MaxBody moves one endpoint's cap,
// and max_body_bytes = -1 turns the default off.
func TestBodyCapDefaultsAndMaxBody(t *testing.T) {
	type in struct {
		Data string `json:"data"`
	}
	echo := func(ctx context.Context, a in) (int, error) { return len(a.Data), nil }
	post := func(app *App, path string, n int) int {
		body := `{"data":"` + strings.Repeat("x", n) + `"}`
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		app.ServeHTTP(rec, req)
		return rec.Code
	}
	app, stop, err := InProcess(config.Runtime{},
		AsRest("POST", "/default", echo),
		AsRest("POST", "/small", echo, MaxBody(64)),
		AsRest("POST", "/large", echo, MaxBody(64<<20)),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(context.Background())
	if c := post(app, "/default", 1<<20); c >= 300 {
		t.Errorf("1MB on the default cap: %d", c)
	}
	if c := post(app, "/default", 33<<20); c != 413 {
		t.Errorf("33MB on the default cap: %d, want 413", c)
	}
	if c := post(app, "/small", 100); c != 413 {
		t.Errorf("100B on MaxBody(64): %d, want 413", c)
	}
	if c := post(app, "/large", 33<<20); c >= 300 {
		t.Errorf("33MB on MaxBody(64MB): %d", c)
	}

	off := config.Runtime{}
	off.Server.MaxBodyBytes = -1
	app2, stop2, err := InProcess(off, AsRest("POST", "/any", echo))
	if err != nil {
		t.Fatal(err)
	}
	defer stop2(context.Background())
	if c := post(app2, "/any", 33<<20); c >= 300 {
		t.Errorf("33MB with the cap off: %d", c)
	}
}

// Timeout gives the handler a context deadline.
func TestEndpointTimeout(t *testing.T) {
	var deadline time.Time
	app, stop, err := InProcess(config.Runtime{},
		AsRest("GET", "/t", func(ctx context.Context) (string, error) {
			deadline, _ = ctx.Deadline()
			return "ok", nil
		}, Timeout(2*time.Second)),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(context.Background())
	app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/t", nil))
	if d := time.Until(deadline); d <= 0 || d > 2*time.Second {
		t.Fatalf("deadline in %v, want within 2s", d)
	}
}
