package nexus

import (
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"
)

// listenerBoundAddr returns "127.0.0.1:<port>" for the listener whose
// scope matches want. The scope table is keyed by port (so dual-stack
// IPv6/IPv4 binds resolve correctly at request time); tests rebuild
// the dial-able address by gluing the loopback host to that port.
func listenerBoundAddr(app *App, want ListenerScope) string {
	app.listenerScopes.mu.RLock()
	defer app.listenerScopes.mu.RUnlock()
	for port, s := range app.listenerScopes.m {
		if s == want {
			return "127.0.0.1:" + port
		}
	}
	return ""
}

func httpGetStatus(t *testing.T, addr, path string) int {
	t.Helper()
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://" + addr + path)
	if err != nil {
		t.Fatalf("GET %s%s: %v", addr, path, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// TestListeners_ScopeFilter verifies that an explicit Listeners map
// binds N ports and the scope filter routes correctly: dashboard hidden
// on public, dashboard visible on admin, user routes hidden on admin.
func TestListeners_ScopeFilter(t *testing.T) {
	var app *App
	fxApp := fxtest.New(t,
		fxBootOptions(Config{
			Dashboard:     DashboardConfig{Enabled: true},
			TraceCapacity: 100,
			Server: ServerConfig{
				Listeners: map[string]Listener{
					"public":   {Addr: "127.0.0.1:0", Scope: ScopePublic},
					"internal": {Addr: "127.0.0.1:0", Scope: ScopeInternal},
					"admin":    {Addr: "127.0.0.1:0", Scope: ScopeAdmin},
				},
			},
		}),
		fx.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	publicAddr := listenerBoundAddr(app, ScopePublic)
	adminAddr := listenerBoundAddr(app, ScopeAdmin)
	if publicAddr == "" || adminAddr == "" {
		t.Fatalf("listeners not bound: public=%q admin=%q", publicAddr, adminAddr)
	}

	// Dashboard is admin-scoped: hidden on public, served on admin.
	if got := httpGetStatus(t, publicAddr, "/__nexus/config"); got != http.StatusNotFound {
		t.Errorf("public /__nexus/config: want 404, got %d", got)
	}
	if got := httpGetStatus(t, adminAddr, "/__nexus/config"); got != http.StatusOK {
		t.Errorf("admin /__nexus/config: want 200, got %d", got)
	}

	// Register a user route and confirm it's reachable on admin —
	// admin scope serves both /__nexus/* and user routes (operator
	// ergonomics; lets the dashboard's RestTester fire relative
	// fetch() calls without 404ing on the listener it loaded from).
	app.Engine().GET("/ping", func(c *gin.Context) { c.String(http.StatusOK, "pong") })
	if got := httpGetStatus(t, adminAddr, "/ping"); got != http.StatusOK {
		t.Errorf("admin /ping: want 200, got %d (admin scope should serve user routes)", got)
	}
	// Same route on the public listener still works — public allows
	// everything except /__nexus/*.
	if got := httpGetStatus(t, publicAddr, "/ping"); got != http.StatusOK {
		t.Errorf("public /ping: want 200, got %d", got)
	}
}

// TestListeners_DualStackBindResolves regression-tests the case that
// motivated keying scopes by port: a listener configured with bare
// ":<port>" binds dual-stack on `[::]:<port>`, but inbound requests
// land with a LocalAddr of `127.0.0.1:<port>`. Looking up the scope by
// full bound address misses; lookup by port hits. Without this, the
// dashboard leaked onto the public listener under any unbracketed
// bind (the common case).
func TestListeners_DualStackBindResolves(t *testing.T) {
	var app *App
	fxApp := fxtest.New(t,
		fxBootOptions(Config{
			Dashboard:     DashboardConfig{Enabled: true},
			TraceCapacity: 100,
			Server: ServerConfig{
				Listeners: map[string]Listener{
					// Bare host elides → dual-stack. ln.Addr()
					// comes back as "[::]:<port>"; request
					// LocalAddr arrives as "127.0.0.1:<port>".
					"public": {Addr: ":0", Scope: ScopePublic},
				},
			},
		}),
		fx.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	publicAddr := listenerBoundAddr(app, ScopePublic)
	if publicAddr == "" {
		t.Fatal("public listener not registered")
	}
	if got := httpGetStatus(t, publicAddr, "/__nexus/config"); got != http.StatusNotFound {
		t.Errorf("public /__nexus/config on dual-stack bind: want 404, got %d", got)
	}
}

// TestFillListenerAddrs verifies the auto-fill: empty Addrs derive
// from publicAddr per scope; explicit Addrs pass through. This is
// the load-bearing helper that makes split deployments work without
// per-binary main.go — the manifest's per-deployment port flows
// into the public listener and admin = public + offset.
func TestFillListenerAddrs(t *testing.T) {
	in := map[string]Listener{
		"public":   {},
		"admin":    {Scope: ScopeAdmin},
		"internal": {Scope: ScopeInternal},
		"explicit": {Addr: "127.0.0.1:5555", Scope: ScopeAdmin},
	}
	out := fillListenerAddrs(in, ":8081")

	if out["public"].Addr != ":8081" {
		t.Errorf("public: want :8081, got %q", out["public"].Addr)
	}
	if out["admin"].Addr != ":9081" {
		t.Errorf("admin: want :9081, got %q", out["admin"].Addr)
	}
	if out["internal"].Addr != ":10081" {
		t.Errorf("internal: want :10081, got %q", out["internal"].Addr)
	}
	if out["explicit"].Addr != "127.0.0.1:5555" {
		t.Errorf("explicit: want pass-through, got %q", out["explicit"].Addr)
	}
}

// TestFillListenerAddrs_DefaultsWhenEmpty verifies the framework's
// :8080 fallback kicks in for plain `go run` (no manifest defaults).
func TestFillListenerAddrs_DefaultsWhenEmpty(t *testing.T) {
	in := map[string]Listener{
		"public": {},
		"admin":  {Scope: ScopeAdmin},
	}
	out := fillListenerAddrs(in, "")
	if out["public"].Addr != ":8080" {
		t.Errorf("public default: want :8080, got %q", out["public"].Addr)
	}
	if out["admin"].Addr != ":9080" {
		t.Errorf("admin default: want :9080, got %q", out["admin"].Addr)
	}
}

// TestListeners_BackCompat_NoConfig verifies the default single-listener
// path keeps working: /__nexus/* stays reachable when Listeners is empty.
func TestListeners_BackCompat_NoConfig(t *testing.T) {
	var app *App
	fxApp := fxtest.New(t,
		fxBootOptions(Config{
			Server:        ServerConfig{Addr: "127.0.0.1:0"},
			Dashboard:     DashboardConfig{Enabled: true},
			TraceCapacity: 100,
		}),
		fx.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	// No filtering when Listeners is empty — scope table stays empty.
	if !app.listenerScopes.empty() {
		t.Fatal("scope table should be empty in back-compat mode")
	}
}
