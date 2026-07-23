// Package inertiatest is the in-process testing harness for Inertia pages — the
// Inertia-aware layer over nexustest. nexustest drives REST/GraphQL/WS through a
// listener-less App; inertiatest adds the Inertia visit protocol on top: it sets
// the X-Inertia headers, decodes the page object (from the XHR JSON or the
// initial load's data-page attribute), and hands back a *Page with prop, merge,
// defer, redirect, and validation assertions. A cookie jar persists across
// visits, so flash-error and session flows work like a real browser.
//
//	func TestUsersPage(t *testing.T) {
//	    c := inertiatest.New(t, nexus.Config{},
//	        inertia.Module(inertia.Config{Frontend: dist, Root: "dist"}),
//	        inertia.Page("GET", "/users", "Users/Index", NewListUsers),
//	    )
//
//	    c.Get("/users").Page().
//	        AssertComponent("Users/Index").
//	        AssertProp("title", "Users").
//	        AssertPropAbsent("stats") // Optional prop, skipped on a full visit
//
//	    // A failed submit flashes errors and redirects back; Follow chases the
//	    // 303 with the jar cookie so the errors surface on the re-render.
//	    c.Post("/users", nil).AssertRedirect("/users").Follow().Page().
//	        AssertError("email", "Email is required")
//	}
//
// Use New to boot an app, or Wrap to layer Inertia visits over a nexustest.App
// you already built (so REST/GraphQL and Inertia assertions share one app).
package inertiatest

import (
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/nexustest"
)

// Inertia protocol headers (mirrored from the extension so the harness needs no
// access to its unexported constants).
const (
	headerInertia          = "X-Inertia"
	headerVersion          = "X-Inertia-Version"
	headerLocation         = "X-Inertia-Location"
	headerPartialData      = "X-Inertia-Partial-Data"
	headerPartialExcept    = "X-Inertia-Partial-Except"
	headerPartialComponent = "X-Inertia-Partial-Component"
	headerReset            = "X-Inertia-Reset"
	headerErrorBag         = "X-Inertia-Error-Bag"
)

// Client issues Inertia visits against a listener-less nexustest.App. It keeps a
// cookie jar so multi-step flows (a validation redirect-back, a login session)
// carry state across visits, exactly as a browser would.
type Client struct {
	app     *nexustest.App
	tb      testing.TB
	jar     map[string]string // name → value
	version string            // default X-Inertia-Version sent on every visit
}

// New boots cfg+opts through nexustest and returns an Inertia client for it. The
// underlying app is stopped via t.Cleanup by nexustest.New.
func New(tb testing.TB, cfg nexus.Config, opts ...nexus.Option) *Client {
	tb.Helper()
	return Wrap(tb, nexustest.New(tb, cfg, opts...))
}

// Wrap layers an Inertia client over an app you already booted with nexustest,
// so the same app serves both REST/GraphQL assertions (via app) and Inertia
// visits (via the returned client).
func Wrap(tb testing.TB, app *nexustest.App) *Client {
	return &Client{app: app, tb: tb, jar: map[string]string{}}
}

// App returns the underlying nexustest.App for REST/GraphQL/WS assertions.
func (c *Client) App() *nexustest.App { return c.app }

// WithVersion sets the X-Inertia-Version sent on every subsequent visit, so a
// version-mismatch (409 + X-Inertia-Location) can be exercised. Chainable.
func (c *Client) WithVersion(v string) *Client { c.version = v; return c }

// ReqOption customizes a visit's request before it is sent.
type ReqOption func(*http.Request)

// Header sets an arbitrary request header.
func Header(k, v string) ReqOption { return func(r *http.Request) { r.Header.Set(k, v) } }

// Version sets X-Inertia-Version for this visit only (overriding WithVersion).
func Version(v string) ReqOption { return Header(headerVersion, v) }

// ErrorBag sets X-Inertia-Error-Bag so flashed validation errors nest under the
// named bag (for a page with multiple forms).
func ErrorBag(name string) ReqOption { return Header(headerErrorBag, name) }

// Except sets X-Inertia-Partial-Except with a matching partial component, so a
// reload sends every plain prop but the excepted ones.
func Except(component string, keys ...string) ReqOption {
	return func(r *http.Request) {
		r.Header.Set(headerPartialComponent, component)
		r.Header.Set(headerPartialExcept, strings.Join(keys, ","))
	}
}

// Reset sets X-Inertia-Reset so the named Merge props are replaced rather than
// merged on this visit.
func Reset(keys ...string) ReqOption {
	return Header(headerReset, strings.Join(keys, ","))
}

// Get issues an XHR GET visit (X-Inertia: true).
func (c *Client) Get(path string, opts ...ReqOption) *Visit {
	c.tb.Helper()
	return c.Visit(http.MethodGet, path, nil, opts...)
}

// Post issues an XHR POST visit with an optional JSON/verbatim body.
func (c *Client) Post(path string, body any, opts ...ReqOption) *Visit {
	c.tb.Helper()
	return c.Visit(http.MethodPost, path, body, opts...)
}

// Visit issues an XHR visit for any method: it sets X-Inertia, replays the jar,
// applies opts, sends the request, and captures any Set-Cookie.
func (c *Client) Visit(method, path string, body any, opts ...ReqOption) *Visit {
	c.tb.Helper()
	req := c.build(method, path, body, opts...)
	req.Header.Set(headerInertia, "true")
	return c.send(req)
}

// Partial issues a partial reload: an XHR GET with X-Inertia-Partial-Component
// set to component and X-Inertia-Partial-Data listing only. With no keys it
// behaves like a full XHR visit for that component.
func (c *Client) Partial(path, component string, only ...string) *Visit {
	c.tb.Helper()
	return c.Visit(http.MethodGet, path, nil, func(r *http.Request) {
		r.Header.Set(headerPartialComponent, component)
		if len(only) > 0 {
			r.Header.Set(headerPartialData, strings.Join(only, ","))
		}
	})
}

// Load issues an initial (non-XHR) browser navigation and returns the full HTML
// document shell. The embedded page object is still available via Page().
func (c *Client) Load(path string, opts ...ReqOption) *Visit {
	c.tb.Helper()
	return c.send(c.build(http.MethodGet, path, nil, opts...))
}

func (c *Client) build(method, path string, body any, opts ...ReqOption) *http.Request {
	req := httptest.NewRequest(method, path, encodeBody(c.tb, body))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.version != "" {
		req.Header.Set(headerVersion, c.version)
	}
	c.applyJar(req)
	for _, o := range opts {
		o(req)
	}
	return req
}

func (c *Client) send(req *http.Request) *Visit {
	c.tb.Helper()
	res := c.app.Do(req)
	c.captureJar(res.Header())
	return &Visit{Response: res, c: c, tb: c.tb}
}

// applyJar attaches every stored cookie to the outgoing request.
func (c *Client) applyJar(req *http.Request) {
	for name, val := range c.jar {
		req.AddCookie(&http.Cookie{Name: name, Value: val})
	}
}

// captureJar folds Set-Cookie into the jar, dropping cookies the response
// expires (MaxAge < 0) so one-shot flash cookies clear after consumption.
func (c *Client) captureJar(h http.Header) {
	for _, ck := range (&http.Response{Header: h}).Cookies() {
		if ck.MaxAge < 0 {
			delete(c.jar, ck.Name)
			continue
		}
		c.jar[ck.Name] = ck.Value
	}
}

// Visit is a recorded Inertia response. It embeds nexustest.Response, so status,
// header, and body helpers pass through; the Inertia-specific methods decode the
// page object and assert redirects.
type Visit struct {
	*nexustest.Response
	c  *Client
	tb testing.TB
}

// AssertStatus asserts the status code and keeps the Inertia chain (it shadows
// the embedded nexustest.Response method, which returns *nexustest.Response).
func (v *Visit) AssertStatus(want int) *Visit {
	v.tb.Helper()
	v.Response.AssertStatus(want)
	return v
}

// AssertOK is AssertStatus(200).
func (v *Visit) AssertOK() *Visit { return v.AssertStatus(http.StatusOK) }

// AssertRedirect asserts an internal Inertia redirect: 303 See Other + Location.
func (v *Visit) AssertRedirect(location string) *Visit {
	v.tb.Helper()
	v.AssertStatus(http.StatusSeeOther)
	if got := v.Header().Get("Location"); got != location {
		v.tb.Fatalf("inertiatest: Location = %q, want %q", got, location)
	}
	return v
}

// AssertLocation asserts an external redirect: 409 Conflict + X-Inertia-Location
// (the client follows it with a full page load).
func (v *Visit) AssertLocation(external string) *Visit {
	v.tb.Helper()
	v.AssertStatus(http.StatusConflict)
	if got := v.Header().Get(headerLocation); got != external {
		v.tb.Fatalf("inertiatest: X-Inertia-Location = %q, want %q", got, external)
	}
	return v
}

// Follow chases a redirect (303 Location or 409 X-Inertia-Location) with an XHR
// GET, carrying the jar cookies — so a validation redirect-back re-renders the
// form with its flashed errors. Fails if the response carried no redirect.
func (v *Visit) Follow(opts ...ReqOption) *Visit {
	v.tb.Helper()
	loc := v.Header().Get("Location")
	if loc == "" {
		loc = v.Header().Get(headerLocation)
	}
	if loc == "" {
		v.tb.Fatalf("inertiatest: Follow called on a non-redirect response (status %d)", v.Code)
	}
	return v.c.Visit(http.MethodGet, loc, nil, opts...)
}

// Page decodes the Inertia page object — from the JSON body of an XHR visit, or
// from the data-page attribute of an initial HTML load — and returns it for
// assertions. Fails the test if no page object is present.
func (v *Visit) Page() *Page {
	v.tb.Helper()
	raw := extractPage(v.Body())
	if raw == nil {
		v.tb.Fatalf("inertiatest: no Inertia page object in response (status %d)\nbody: %s", v.Code, v.String())
	}
	var p Page
	if err := json.Unmarshal(raw, &p); err != nil {
		v.tb.Fatalf("inertiatest: decode page object: %v\nraw: %s", err, raw)
	}
	p.tb = v.tb
	p.raw = raw
	return &p
}

// extractPage returns the page-object JSON from a response body: the body itself
// for an XHR JSON page, or the (HTML-unescaped) data-page attribute for a full
// document load. Returns nil when neither is present.
func extractPage(body []byte) []byte {
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "{") {
		return []byte(trimmed)
	}
	_, rest, ok := strings.Cut(trimmed, `data-page="`)
	if !ok {
		return nil
	}
	attr, _, ok := strings.Cut(rest, `"`)
	if !ok {
		return nil
	}
	return []byte(html.UnescapeString(attr))
}

// Page is a decoded Inertia page object with prop and metadata assertions.
type Page struct {
	Component      string              `json:"component"`
	Props          map[string]any      `json:"props"`
	URL            string              `json:"url"`
	Version        string              `json:"version"`
	DeferredProps  map[string][]string `json:"deferredProps"`
	MergeProps     []string            `json:"mergeProps"`
	DeepMergeProps []string            `json:"deepMergeProps"`
	MatchPropsOn   []string            `json:"matchPropsOn"`
	EncryptHistory bool                `json:"encryptHistory"`
	ClearHistory   bool                `json:"clearHistory"`

	tb  testing.TB
	raw []byte
}

// Raw returns the page object's JSON.
func (p *Page) Raw() []byte { return p.raw }

// AssertComponent fails unless the page renders the named component.
func (p *Page) AssertComponent(name string) *Page {
	p.tb.Helper()
	if p.Component != name {
		p.tb.Fatalf("inertiatest: component = %q, want %q", p.Component, name)
	}
	return p
}

// AssertURL fails unless the page's canonical URL equals want.
func (p *Page) AssertURL(want string) *Page {
	p.tb.Helper()
	if p.URL != want {
		p.tb.Fatalf("inertiatest: url = %q, want %q", p.URL, want)
	}
	return p
}

// AssertVersion fails unless the asset version equals want.
func (p *Page) AssertVersion(want string) *Page {
	p.tb.Helper()
	if p.Version != want {
		p.tb.Fatalf("inertiatest: version = %q, want %q", p.Version, want)
	}
	return p
}

// Prop returns a prop value (nil if absent). Numbers are float64 per JSON.
func (p *Page) Prop(key string) any { return p.Props[key] }

// AssertProp fails unless the prop equals want. Comparison is JSON-normalized,
// so an int want matches a JSON float64, and structs/maps compare by shape.
func (p *Page) AssertProp(key string, want any) *Page {
	p.tb.Helper()
	got, ok := p.Props[key]
	if !ok {
		p.tb.Fatalf("inertiatest: prop %q absent; props: %v", key, p.Props)
	}
	if !jsonEqual(got, want) {
		p.tb.Fatalf("inertiatest: prop %q = %#v, want %#v", key, got, want)
	}
	return p
}

// AssertPropPresent fails unless the prop is present (any value, including null).
func (p *Page) AssertPropPresent(key string) *Page {
	p.tb.Helper()
	if _, ok := p.Props[key]; !ok {
		p.tb.Fatalf("inertiatest: prop %q must be present; props: %v", key, p.Props)
	}
	return p
}

// AssertPropAbsent fails if the prop is present (e.g. an Optional/Defer prop on a
// full visit).
func (p *Page) AssertPropAbsent(key string) *Page {
	p.tb.Helper()
	if _, ok := p.Props[key]; ok {
		p.tb.Fatalf("inertiatest: prop %q must be absent; props: %v", key, p.Props)
	}
	return p
}

// Bind re-decodes a prop into v (a pointer) so a typed struct/slice can be
// asserted with ordinary Go comparisons. Fails if the prop is absent.
func (p *Page) Bind(key string, v any) *Page {
	p.tb.Helper()
	raw, ok := p.Props[key]
	if !ok {
		p.tb.Fatalf("inertiatest: prop %q absent; props: %v", key, p.Props)
	}
	blob, err := json.Marshal(raw)
	if err != nil {
		p.tb.Fatalf("inertiatest: marshal prop %q: %v", key, err)
	}
	if err := json.Unmarshal(blob, v); err != nil {
		p.tb.Fatalf("inertiatest: bind prop %q: %v", key, err)
	}
	return p
}

// AssertDeferred fails unless the named deferred group advertises exactly keys
// (order-independent). A page's Defer props are excluded from the initial
// payload and announced here for the client to fetch.
func (p *Page) AssertDeferred(group string, keys ...string) *Page {
	p.tb.Helper()
	if !sameSet(p.DeferredProps[group], keys) {
		p.tb.Fatalf("inertiatest: deferredProps[%q] = %v, want %v", group, p.DeferredProps[group], keys)
	}
	return p
}

// AssertMerge fails unless mergeProps equals keys (order-independent).
func (p *Page) AssertMerge(keys ...string) *Page {
	p.tb.Helper()
	if !sameSet(p.MergeProps, keys) {
		p.tb.Fatalf("inertiatest: mergeProps = %v, want %v", p.MergeProps, keys)
	}
	return p
}

// AssertDeepMerge fails unless deepMergeProps equals keys (order-independent).
func (p *Page) AssertDeepMerge(keys ...string) *Page {
	p.tb.Helper()
	if !sameSet(p.DeepMergeProps, keys) {
		p.tb.Fatalf("inertiatest: deepMergeProps = %v, want %v", p.DeepMergeProps, keys)
	}
	return p
}

// AssertEncryptHistory fails unless the page requests history encryption.
func (p *Page) AssertEncryptHistory() *Page {
	p.tb.Helper()
	if !p.EncryptHistory {
		p.tb.Fatal("inertiatest: encryptHistory = false, want true")
	}
	return p
}

// AssertClearHistory fails unless the page requests history clearing.
func (p *Page) AssertClearHistory() *Page {
	p.tb.Helper()
	if !p.ClearHistory {
		p.tb.Fatal("inertiatest: clearHistory = false, want true")
	}
	return p
}

// Errors returns the page's validation errors map (props.errors), which Inertia
// always exposes ({} when there are none). With an error bag, the messages nest
// one level under the bag name.
func (p *Page) Errors() map[string]any {
	if e, ok := p.Props["errors"].(map[string]any); ok {
		return e
	}
	return map[string]any{}
}

// AssertError fails unless props.errors[field] equals msg. For a bagged form,
// pass "bag.field" as field.
func (p *Page) AssertError(field, msg string) *Page {
	p.tb.Helper()
	errs := p.Errors()
	if bag, rest, nested := strings.Cut(field, "."); nested {
		if sub, ok := errs[bag].(map[string]any); ok {
			errs = sub
			field = rest
		}
	}
	if got := errs[field]; got != msg {
		p.tb.Fatalf("inertiatest: errors[%q] = %v, want %q", field, got, msg)
	}
	return p
}

// jsonEqual compares two values by their JSON encodings, so int/float and
// struct/map shapes compare equal regardless of Go type. Marshal sorts map keys,
// making the byte comparison order-independent.
func jsonEqual(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return reflect.DeepEqual(a, b)
	}
	return string(ab) == string(bb)
}

// sameSet reports whether got and want hold the same elements, ignoring order
// and duplicates.
func sameSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]int, len(got))
	for _, g := range got {
		seen[g]++
	}
	for _, w := range want {
		if seen[w] == 0 {
			return false
		}
		seen[w]--
	}
	return true
}

// encodeBody mirrors nexustest's body encoding: nil → no body (a true nil
// io.Reader, never a typed nil), []byte/string/io.Reader sent verbatim, anything
// else JSON-encoded.
func encodeBody(tb testing.TB, body any) io.Reader {
	tb.Helper()
	switch b := body.(type) {
	case nil:
		return nil
	case string:
		return strings.NewReader(b)
	case []byte:
		return strings.NewReader(string(b))
	case io.Reader:
		return b
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			tb.Fatalf("inertiatest: marshal body: %v", err)
		}
		return strings.NewReader(string(raw))
	}
}
