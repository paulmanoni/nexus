package inertiatest_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension/inertia"
	"github.com/paulmanoni/nexus/extension/inertia/inertiatest"
	"github.com/paulmanoni/nexus/httpx"
)

const manifestJSON = `{"src/main.ts":{"file":"assets/main-abc123.js","isEntry":true,"css":["assets/main-xyz.css"]}}`

// widgetProps exercises plain, Optional, Always, Merge and Defer prop kinds.
type widgetProps struct {
	Title string       `json:"title"`
	Stats inertia.Prop `json:"stats"`
	Menu  inertia.Prop `json:"menu"`
	Items inertia.Prop `json:"items"`
	More  inertia.Prop `json:"more"`
}

func NewWidgets(ctx context.Context) (widgetProps, error) {
	return widgetProps{
		Title: "Widgets",
		Stats: inertia.Optional(func() (int, error) { return 42, nil }),
		Menu:  inertia.Always([]string{"home", "about"}),
		Items: inertia.Merge(func() ([]int, error) { return []int{1, 2}, nil }),
		More:  inertia.Defer(func() (string, error) { return "later", nil }),
	}, nil
}

// NewRegister renders on GET and flashes a validation error on POST.
func NewRegister(p nexus.Params[struct{}]) (any, error) {
	if p.Method == http.MethodGet {
		return widgetProps{Title: "Register"}, nil
	}
	return nil, inertia.Invalid(map[string]string{"email": "Email is required"})
}

func NewSave(ctx context.Context) (any, error) { return nil, inertia.Redirect("/widgets") }
func NewLogout(c *httpx.Ctx) (any, error)      { inertia.ClearHistory(c); return nil, inertia.Redirect("/") }
func NewSecure(c *httpx.Ctx) (widgetProps, error) {
	inertia.EncryptHistory(c)
	return widgetProps{Title: "Secure"}, nil
}

func newClient(t *testing.T, extra ...nexus.Option) *inertiatest.Client {
	t.Helper()
	fsys := fstest.MapFS{
		"dist/.vite/manifest.json": {Data: []byte(manifestJSON)},
		"dist/index.html":          {Data: []byte("<!doctype html><div id=app></div>")},
	}
	opts := append([]nexus.Option{
		nexus.ServeFrontend(fsys, "dist"),
		inertia.Module(inertia.Config{Head: inertia.Head{Title: "NXHEAD"}}),
		inertia.Share(func(ctx context.Context) (string, any) { return "csrf", "tok-123" }),
		inertia.Page("GET", "/widgets", "Widgets/Index", NewWidgets),
	}, extra...)
	return inertiatest.New(t, nexus.Config{TraceCapacity: 10}, opts...)
}

// TestXHRVisit covers the core page-object assertions on an XHR visit: component,
// url, plain/shared/Always props present, Optional/Defer props absent.
func TestXHRVisit(t *testing.T) {
	c := newClient(t)
	c.Get("/widgets?tab=1").AssertOK().Page().
		AssertComponent("Widgets/Index").
		AssertURL("/widgets?tab=1").
		AssertProp("title", "Widgets").
		AssertProp("csrf", "tok-123").
		AssertPropPresent("menu").
		AssertPropAbsent("stats"). // Optional, skipped on a full visit
		AssertPropAbsent("more")   // Defer, excluded and advertised instead
}

// TestBindAndVersion covers Bind (typed decode) and AssertVersion (manifest hash).
func TestBindAndVersion(t *testing.T) {
	p := newClient(t).Get("/widgets").Page()
	var menu []string
	p.Bind("menu", &menu)
	if len(menu) != 2 || menu[0] != "home" {
		t.Fatalf("bound menu = %v", menu)
	}
	if p.Version == "" {
		t.Fatal("expected a manifest-derived version")
	}
}

// TestMergeAndDefer covers the merge/defer metadata assertions.
func TestMergeAndDefer(t *testing.T) {
	p := newClient(t).Get("/widgets").Page()
	p.AssertMerge("items")
	p.AssertDeferred("default", "more")

	// The follow-up partial delivers the deferred prop.
	newClient(t).Partial("/widgets", "Widgets/Index", "more").Page().
		AssertProp("more", "later")
}

// TestPartialReload covers Partial: only the named prop plus Always props.
func TestPartialReload(t *testing.T) {
	newClient(t).Partial("/widgets", "Widgets/Index", "stats").Page().
		AssertProp("stats", 42).   // int want matches JSON float64
		AssertPropPresent("menu"). // Always survives a partial
		AssertPropAbsent("title")  // unrequested plain prop
}

// TestExceptAndReset covers the Except and Reset request options.
func TestExceptAndReset(t *testing.T) {
	c := newClient(t)
	c.Get("/widgets", inertiatest.Except("Widgets/Index", "title")).Page().
		AssertPropAbsent("title").
		AssertPropPresent("csrf")

	// Reset a merge prop: still sent, no longer flagged.
	c.Get("/widgets", inertiatest.Reset("items")).Page().
		AssertMerge().
		AssertPropPresent("items")
}

// TestFullLoad covers Load: an HTML shell whose data-page still decodes to a page.
func TestFullLoad(t *testing.T) {
	res := newClient(t).Load("/widgets")
	res.AssertOK()
	if body := res.String(); !strings.Contains(body, "<title>NXHEAD</title>") {
		t.Fatalf("Config.Head not injected: %s", body)
	}
	res.Page().AssertComponent("Widgets/Index").AssertProp("title", "Widgets")
}

// TestRedirect covers AssertRedirect on an internal Inertia redirect.
func TestRedirect(t *testing.T) {
	c := newClient(t, inertia.Page("POST", "/save", "Save", NewSave))
	c.Post("/save", nil).AssertRedirect("/widgets")
}

// TestValidationFlow covers the flash-cookie round trip: a failed submit 303s
// back, and Follow (carrying the jar cookie) surfaces the error on the re-render.
func TestValidationFlow(t *testing.T) {
	c := newClient(t, inertia.Page("GET,POST", "/register", "Register", NewRegister, nexus.Public()))
	c.Post("/register", nil).
		AssertRedirect("/register").
		Follow().Page().
		AssertError("email", "Email is required")
}

// TestValidationBag covers ErrorBag: the flashed error nests under the bag name.
func TestValidationBag(t *testing.T) {
	c := newClient(t, inertia.Page("GET,POST", "/register", "Register", NewRegister, nexus.Public()))
	c.Post("/register", nil, inertiatest.ErrorBag("signup")).
		AssertRedirect("/register").
		Follow().Page().
		AssertError("signup.email", "Email is required")
}

// TestVersionMismatch covers WithVersion driving the 409 + X-Inertia-Location guard.
func TestVersionMismatch(t *testing.T) {
	c := newClient(t).WithVersion("stale-hash")
	c.Get("/widgets").AssertLocation("/widgets")
}

// TestHistoryControls covers the per-response history flag assertions.
func TestHistoryControls(t *testing.T) {
	c := newClient(t,
		inertia.Page("GET", "/secure", "Secure", NewSecure),
		inertia.Page("POST", "/logout", "Logout", NewLogout),
	)
	c.Get("/secure").Page().AssertEncryptHistory()
	c.Post("/logout", nil).AssertRedirect("/")
}

// TestWrapSharesApp covers Wrap: REST assertions and Inertia visits over one app.
func TestWrapSharesApp(t *testing.T) {
	c := newClient(t)
	// The same app answers a raw REST probe and an Inertia visit.
	c.App().GET("/__nexus/config").AssertOK()
	c.Get("/widgets").AssertOK().Page().AssertComponent("Widgets/Index")
}
