package nexus

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/httpx"
	"github.com/paulmanoni/nexus/v2/middleware"
)

// markMW returns a middleware that records its name, so tests can assert
// which shared options ran and in what order.
func markMW(name string, log *[]string) MiddlewareOption {
	return Use(middleware.FromHandler(middleware.NewFunc(name, middleware.AllTransports,
		func(rc *middleware.RequestCtx, next middleware.Next) error {
			*log = append(*log, name)
			return next(rc)
		})))
}

func routerGet(t *testing.T, app *App, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}

// TestRouterNesting: prefixes stack under Include, shared options inherit
// downward and run in declaration order ahead of the op's own.
func TestRouterNesting(t *testing.T) {
	var log []string
	handler := func() httpx.HandlerFunc {
		return func(c *httpx.Ctx) { c.String(200, "invoices") }
	}

	billing := NewRouter("billing", "/billing", markMW("billing-shared", &log))
	billing.Rest("GET", "/invoices", handler, markMW("per-op", &log))

	v1 := NewRouter("v1", "/api/v1", markMW("v1-shared", &log))
	v1.Include(billing)

	app, stop, err := InProcess(config.Runtime{}, v1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })

	if w := routerGet(t, app, "/api/v1/billing/invoices"); w.Code != 200 {
		t.Fatalf("stacked prefix route: status %d", w.Code)
	}
	want := []string{"v1-shared", "billing-shared", "per-op"}
	if strings.Join(log, ",") != strings.Join(want, ",") {
		t.Errorf("middleware order = %v, want %v", log, want)
	}
	// The unprefixed path must not exist.
	if w := routerGet(t, app, "/invoices"); w.Code == 200 {
		t.Error("route leaked outside the router prefix")
	}
}

// TestRouterIncludedGuard: passing an included router (not the root) to the
// app is a boot error, not a silent double mount.
func TestRouterIncludedGuard(t *testing.T) {
	child := NewRouter("child", "/c")
	NewRouter("root", "/r").Include(child)
	_, _, err := InProcess(config.Runtime{}, child)
	if err == nil || !strings.Contains(err.Error(), "pass only the root") {
		t.Fatalf("expected included-router error, got: %v", err)
	}
}

// TestRouterDeclAssembly: the decorator-form runtime — RouterDecl/OnRouter
// recorded at init time assemble into the same nested, shared-option
// behaviour, with the shared middleware prepended ahead of the op's own.
func TestRouterDeclAssembly(t *testing.T) {
	var log []string
	handler := func() httpx.HandlerFunc {
		return func(c *httpx.Ctx) { c.String(200, "ok") }
	}
	RouterDecl("v1", "/api/v1", "")
	RouterDecl("billing", "/billing", "v1", markMW("decl-shared", &log))
	OnRouter("billing", AsRestHandler("GET", "/invoices", handler, markMW("per-op", &log)))

	app, stop, err := InProcess(config.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })

	if w := routerGet(t, app, "/api/v1/billing/invoices"); w.Code != 200 {
		t.Fatalf("assembled route: status %d", w.Code)
	}
	want := []string{"decl-shared", "per-op"}
	if strings.Join(log, ",") != strings.Join(want, ",") {
		t.Errorf("middleware order = %v, want %v", log, want)
	}
}

// TestRouterDeclErrors: unknown parents, unknown //nexus:on names, and parent
// cycles fail the boot with clear messages.
func TestRouterDeclErrors(t *testing.T) {
	boot := func() error {
		_, stop, err := InProcess(config.Runtime{})
		if err == nil {
			_ = stop(context.Background())
		}
		return err
	}

	RouterDecl("a", "/a", "missing")
	if err := boot(); err == nil || !strings.Contains(err.Error(), `unknown parent "missing"`) {
		t.Fatalf("unknown parent: %v", err)
	}

	OnRouter("ghost", Options())
	if err := boot(); err == nil || !strings.Contains(err.Error(), `unknown router "ghost"`) {
		t.Fatalf("unknown member router: %v", err)
	}

	RouterDecl("a", "/a", "b")
	RouterDecl("b", "/b", "a")
	if err := boot(); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle: %v", err)
	}
}
