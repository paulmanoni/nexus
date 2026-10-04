// Package viewtest drives view pages end to end in a test, without a
// browser: the page is fetched over HTTP from the app, the view runtime
// (/_view/runtime.js and the compiled expressions) runs in goja against a
// small DOM, a live page connects its WebSocket to the app, and every
// event, patch and form rule is the real runtime's.
//
//	func TestBoard(t *testing.T) {
//	    app := nexustest.New(t, config.Runtime{}, appOptions()...)
//	    p := viewtest.Mount[*pets.Board](t, app)
//	    p.Fill("name", "Rex").Fill("kind", "dog").Click("#add-submit")
//	    p.Expect("#pet-Rex").Exists()
//	    p.Expect("name").Value("") // the form reset
//	}
//
// Locators are a form field's name, else a CSS selector (tag, #id, .class,
// [attr], [attr=v] and its ^= $= *= ~= forms, :checked, :disabled, :not(…),
// descendant, >, + and ~).
//
// Actions (Fill, Select, Check, Click, Submit, Press) first let the page
// settle — replies to earlier events arrive, debounced work runs — so a test
// acts on a quiet page, as a person does. Expect assertions retry until they
// hold or the timeout passes, so they also see server pushes. Wait settles
// the page explicitly.
//
// It is not a browser: there is no layout or CSS (Visible means no hidden
// ancestor), only the view runtime's scripts run, and islands don't mount
// (there is no Vite build to load them from). viewtest checks behaviour;
// layout needs a real browser.
package viewtest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dop251/goja"
	"github.com/gorilla/websocket"

	"github.com/paulmanoni/nexus/v2/extension/auth"
	"github.com/paulmanoni/nexus/v2/extension/auth/authtest"
	"github.com/paulmanoni/nexus/v2/registry"
	"github.com/paulmanoni/nexus/v2/view"
	"github.com/paulmanoni/nexus/v2/view/internal/browser"
)

// Option configures a page.
type Option func(*settings)

type settings struct {
	header  http.Header
	cookies []*http.Cookie
	path    string
	timeout time.Duration
}

// As makes every request of the page — page loads, fetches and the live
// socket — act as id, through extension/auth's test credential
// (authtest.As): every gate, area and policy runs as for a real sign-in.
func As(id *auth.Identity) Option {
	return func(s *settings) {
		for k, vs := range authtest.As(id) {
			for _, v := range vs {
				s.header.Add(k, v)
			}
		}
	}
}

// Header adds a header to every request of the page.
func Header(key, value string) Option {
	return func(s *settings) { s.header.Add(key, value) }
}

// Cookie starts the page's cookie jar with c (a session, say).
func Cookie(c *http.Cookie) Option {
	return func(s *settings) { s.cookies = append(s.cookies, c) }
}

// At is the path Mount opens, for a live page whose prefix has parameters
// ("/orders/:id") or that is served at more than one path.
func At(path string) Option { return func(s *settings) { s.path = path } }

// Timeout bounds how long the page waits for the server and how long an
// Expect retries (default 5s).
func Timeout(d time.Duration) Option { return func(s *settings) { s.timeout = d } }

// Page is a page loaded in the test browser.
type Page struct {
	t      testing.TB
	s      settings
	srv    *httptest.Server
	client *http.Client
	b      *browser.Browser
	status int
	nav    *navigation

	mu      sync.Mutex
	pending map[*wsConn]map[int]bool // events sent and not yet answered
	console []string
}

type navigation struct{ method, url, body, contentType string }

// Mount opens the live page T (view.Live[T]) served by app — an
// *nexus.App, a nexustest.App, anything with the app's Registry — and waits
// for its socket to connect.
func Mount[T any](t testing.TB, app http.Handler, opts ...Option) *Page {
	t.Helper()
	s := newSettings(opts)
	path := s.path
	if path == "" {
		path = livePath(t, app, reflect.TypeFor[T]())
	}
	p := open(t, app, s, path)
	if p.status != http.StatusOK {
		t.Fatalf("viewtest: GET %s = %d\n%s", path, p.status, clip(p.HTML(), 2000))
	}
	if !p.Exists("[data-nx-live]") {
		t.Fatalf("viewtest: %s is not a live page", path)
	}
	return p
}

// Get opens the page at path — a view page, live or not, or any HTML. A
// status other than 200 does not fail the test: check Status.
func Get(t testing.TB, app http.Handler, path string, opts ...Option) *Page {
	t.Helper()
	return open(t, app, newSettings(opts), path)
}

func newSettings(opts []Option) settings {
	s := settings{header: http.Header{}, timeout: 5 * time.Second}
	for _, o := range opts {
		o(&s)
	}
	return s
}

func livePath(t testing.TB, app http.Handler, typ reflect.Type) string {
	t.Helper()
	ra, ok := app.(interface{ Registry() *registry.Registry })
	if !ok {
		t.Fatalf("viewtest.Mount[%s]: %T has no Registry: pass the *nexus.App (or nexustest.App), or open the page with viewtest.Get", typ, app)
	}
	key := view.LiveKey(typ)
	var paths []string
	for _, e := range ra.Registry().Endpoints() {
		if e.Tags[view.LiveTag] == key {
			paths = append(paths, e.Path)
		}
	}
	switch {
	case len(paths) == 0:
		t.Fatalf("viewtest.Mount[%s]: the app serves no view.Live[%s]", typ, typ)
	case len(paths) > 1:
		t.Fatalf("viewtest.Mount[%s]: served at %v: pick one with viewtest.At", typ, paths)
	case strings.ContainsAny(paths[0], ":*"):
		t.Fatalf("viewtest.Mount[%s]: %s has parameters: give the path with viewtest.At(\"…\")", typ, paths[0])
	}
	return paths[0]
}

func open(t testing.TB, app http.Handler, s settings, path string) *Page {
	t.Helper()
	srv := httptest.NewServer(app)
	jar, _ := cookiejar.New(nil)
	base, _ := url.Parse(srv.URL)
	if len(s.cookies) > 0 {
		jar.SetCookies(base, s.cookies)
	}
	p := &Page{
		t:       t,
		s:       s,
		srv:     srv,
		client:  &http.Client{Jar: jar, Transport: headerTransport{s.header, http.DefaultTransport}},
		pending: map[*wsConn]map[int]bool{},
	}
	t.Cleanup(p.Close)
	ref, err := url.Parse(path)
	if err != nil {
		t.Fatalf("viewtest: path %q: %v", path, err)
	}
	p.load(navigation{method: http.MethodGet, url: base.ResolveReference(ref).String()})
	p.settle()
	return p
}

type headerTransport struct {
	h    http.Header
	next http.RoundTripper
}

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if len(h.h) > 0 {
		r = r.Clone(r.Context())
		for k, vs := range h.h {
			for _, v := range vs {
				r.Header.Add(k, v)
			}
		}
	}
	return h.next.RoundTrip(r)
}

// Close closes the page's sockets and server. It runs on test cleanup.
func (p *Page) Close() {
	if p.b != nil {
		p.b.Close()
	}
	p.srv.Close()
}

// ---- loading ---------------------------------------------------------------

// load replaces the page with the document at n, as a full page load does:
// a new realm, the view runtime's scripts run.
func (p *Page) load(n navigation) {
	p.t.Helper()
	if p.b != nil {
		p.b.Close()
	}
	res, err := p.request(n.method, n.url, http.Header{"Content-Type": {n.contentType}, "Accept": {"text/html"}}, n.body)
	if err != nil {
		p.t.Fatalf("viewtest: %s %s: %v", n.method, n.url, err)
	}
	p.status = res.Status
	b, err := browser.New(browser.Host{
		Fetch: p.request,
		Dial:  p.dial,
		Navigate: func(method, u, body, contentType string) {
			p.nav = &navigation{method, u, body, contentType}
		},
		Console: p.log,
		Cookie:  p.cookie,
	})
	if err != nil {
		p.t.Fatal(err)
	}
	p.b = b
	if err := b.Load(res.Body, res.URL); err != nil {
		p.t.Fatalf("viewtest: loading %s: %v", res.URL, err)
	}
	srcs, err := b.VM.RunString(`Array.from(document.querySelectorAll("script[src]")).map(function (s) { return s.getAttribute("src"); })`)
	if err != nil {
		p.t.Fatal(err)
	}
	for _, src := range srcs.Export().([]any) {
		u, err := url.Parse(src.(string))
		if err != nil || (u.Path != "/_view/twins.js" && u.Path != "/_view/runtime.js") {
			continue // only the view runtime runs
		}
		abs, _ := url.Parse(res.URL)
		js, err := p.request(http.MethodGet, abs.ResolveReference(u).String(), nil, "")
		if err != nil || js.Status != http.StatusOK {
			p.t.Fatalf("viewtest: loading %s: %v (status %d)", src, err, js.Status)
		}
		if err := b.Run(u.Path, js.Body); err != nil {
			p.t.Fatalf("viewtest: running %s: %v", u.Path, err)
		}
	}
}

func (p *Page) request(method, u string, header http.Header, body string) (*browser.Response, error) {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, u, r)
	if err != nil {
		return nil, err
	}
	for k, vs := range header {
		for _, v := range vs {
			if v != "" {
				req.Header.Add(k, v)
			}
		}
	}
	res, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	final := res.Request.URL.String()
	return &browser.Response{Status: res.StatusCode, URL: final, Header: res.Header, Body: string(b), Redirected: final != u}, nil
}

// cookie is document.cookie: the jar's cookies for u. The jar doesn't keep
// HttpOnly, so the page sees those too.
func (p *Page) cookie(u string) string {
	ref, err := url.Parse(u)
	if err != nil {
		return ""
	}
	var parts []string
	for _, c := range p.client.Jar.Cookies(ref) {
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

func (p *Page) log(level, msg string) {
	p.mu.Lock()
	p.console = append(p.console, level+": "+msg)
	p.mu.Unlock()
	if level == "error" || level == "warn" {
		p.t.Logf("viewtest: console.%s: %s", level, msg)
	}
}

// ---- the live socket -------------------------------------------------------

type wsConn struct {
	p       *Page
	c       *websocket.Conn
	wmu     sync.Mutex
	closing bool
}

// resync is what the test browser sends first on every live socket: the
// server answers it after Mount with the page's render, so the page is known
// to be joined once that reply is in — whether or not the join sent anything.
const resyncRef = -1

func (p *Page) dial(u string, recv func(string), closed func()) (browser.Conn, error) {
	h := http.Header{"Origin": {p.srv.URL}}
	for k, vs := range p.s.header {
		h[k] = append(h[k], vs...)
	}
	d := websocket.Dialer{Jar: p.client.Jar, HandshakeTimeout: p.s.timeout}
	c, res, err := d.Dial(u, h)
	if err != nil {
		if res != nil {
			return nil, fmt.Errorf("%v (HTTP %d)", err, res.StatusCode)
		}
		return nil, err
	}
	w := &wsConn{p: p, c: c}
	if err := w.Send(fmt.Sprintf(`{"event":"__resync","ref":%d}`, resyncRef)); err != nil {
		c.Close()
		return nil, err
	}
	go func() {
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				p.mu.Lock()
				delete(p.pending, w)
				closing := w.closing
				p.mu.Unlock()
				if !closing {
					closed()
				}
				return
			}
			recv(string(data))
			var m struct {
				Ref int `json:"ref"`
			}
			_ = json.Unmarshal(data, &m)
			p.mu.Lock()
			delete(p.pending[w], m.Ref)
			p.mu.Unlock()
		}
	}()
	return w, nil
}

func (w *wsConn) Send(msg string) error {
	var m struct {
		Ref int `json:"ref"`
	}
	_ = json.Unmarshal([]byte(msg), &m)
	if m.Ref != 0 {
		w.p.mu.Lock()
		if w.p.pending[w] == nil {
			w.p.pending[w] = map[int]bool{}
		}
		w.p.pending[w][m.Ref] = true
		w.p.mu.Unlock()
	}
	w.wmu.Lock()
	defer w.wmu.Unlock()
	_ = w.c.SetWriteDeadline(time.Now().Add(w.p.s.timeout))
	return w.c.WriteMessage(websocket.TextMessage, []byte(msg))
}

func (w *wsConn) Close() error {
	w.p.mu.Lock()
	w.closing = true
	delete(w.p.pending, w)
	w.p.mu.Unlock()
	return w.c.Close()
}

func (p *Page) busy() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, refs := range p.pending {
		if len(refs) > 0 {
			return true
		}
	}
	return false
}

// ---- running ---------------------------------------------------------------

// settle runs the page until it is quiet: every event answered, debounced
// work (up to a second of page time) done, navigations followed.
func (p *Page) settle() {
	p.t.Helper()
	for {
		err := p.b.Settle(1000, p.s.timeout, p.busy, func() bool { return p.nav != nil })
		if err != nil {
			p.t.Fatalf("viewtest: the server did not answer within %s", p.s.timeout)
		}
		if p.nav == nil {
			return
		}
		n := *p.nav
		p.nav = nil
		p.load(n)
	}
}

// Wait lets the page settle: replies to the events sent so far arrive and
// are applied, and debounced work (view.Change) runs.
func (p *Page) Wait() *Page {
	p.t.Helper()
	p.settle()
	return p
}

// pump waits a moment for the network (a server push) and settles.
func (p *Page) pump() {
	p.t.Helper()
	p.b.WaitTask(20 * time.Millisecond)
	p.settle()
}

// locate finds the element loc names, waiting for it to appear.
func (p *Page) locate(loc string) goja.Value {
	p.t.Helper()
	deadline := time.Now().Add(p.s.timeout)
	for {
		el, err := p.b.Call("find", loc)
		if err != nil {
			p.t.Fatalf("viewtest: %q: %v", loc, err)
		}
		if !goja.IsNull(el) && !goja.IsUndefined(el) {
			return el
		}
		if time.Now().After(deadline) {
			p.t.Fatalf("viewtest: no element %q on %s\n%s", loc, p.URL(), clip(p.HTML(), 4000))
		}
		p.pump()
	}
}

func (p *Page) call(name string, args ...any) goja.Value {
	p.t.Helper()
	v, err := p.b.Call(name, args...)
	if err != nil {
		p.t.Fatalf("viewtest: %s: %v", name, err)
	}
	return v
}

// act settles the page, runs a user action on loc's element and fails the
// test with the action's complaint, if any.
func (p *Page) act(what, loc string, name string, args ...any) *Page {
	p.t.Helper()
	p.settle()
	el := p.locate(loc)
	if msg := p.call(name, append([]any{el}, args...)...).String(); msg != "" {
		p.t.Fatalf("viewtest: %s %q: %s", what, loc, msg)
	}
	return p
}

// ---- actions ---------------------------------------------------------------

// Fill types value into a text field or textarea: it takes focus, gets the
// value, and fires input and change.
func (p *Page) Fill(field, value string) *Page {
	p.t.Helper()
	return p.act("Fill", field, "fill", value)
}

// Select chooses the options of a select by value — one for a single
// select, any number for a multiple one — and fires input and change.
func (p *Page) Select(field string, values ...string) *Page {
	p.t.Helper()
	return p.act("Select", field, "choose", values)
}

// Check checks a checkbox or radio (a click, if it isn't checked).
func (p *Page) Check(field string) *Page {
	p.t.Helper()
	return p.act("Check", field, "check", true)
}

// Uncheck unchecks a checkbox.
func (p *Page) Uncheck(field string) *Page {
	p.t.Helper()
	return p.act("Uncheck", field, "check", false)
}

// Click clicks an element: it takes focus (a field, button or link), the
// click event bubbles, and the default action follows unless prevented — a
// submit button submits its form, a link navigates, a checkbox toggles.
func (p *Page) Click(loc string) *Page {
	p.t.Helper()
	return p.act("Click", loc, "click")
}

// Submit submits a form — the form itself, a field in it or its submit
// button — as pressing Enter does.
func (p *Page) Submit(loc string) *Page {
	p.t.Helper()
	return p.act("Submit", loc, "submit")
}

// Press sends a key (keydown and keyup) to an element: "Enter", "Escape", "a".
func (p *Page) Press(loc, key string) *Page {
	p.t.Helper()
	return p.act("Press", loc, "press", key)
}

// Focus gives an element focus; Blur takes it away.
func (p *Page) Focus(loc string) *Page {
	p.t.Helper()
	p.settle()
	p.call("focus", p.locate(loc))
	return p
}

func (p *Page) Blur() *Page {
	p.t.Helper()
	p.call("blur")
	return p
}

// Visit loads path in the same page (cookies kept), as typing it into the
// address bar does.
func (p *Page) Visit(path string) *Page {
	p.t.Helper()
	base, _ := url.Parse(p.URL())
	ref, err := url.Parse(path)
	if err != nil {
		p.t.Fatalf("viewtest: path %q: %v", path, err)
	}
	p.load(navigation{method: http.MethodGet, url: base.ResolveReference(ref).String()})
	p.settle()
	return p
}

// Back goes back in the page's history (an in-app navigation).
func (p *Page) Back() *Page {
	p.t.Helper()
	p.call("back")
	p.settle()
	return p
}

// ---- reading ---------------------------------------------------------------

// Status is the HTTP status of the page's last full load.
func (p *Page) Status() int { return p.status }

// URL is the page's address.
func (p *Page) URL() string { return p.call("url").String() }

// HTML is the page's document as it stands, serialised.
func (p *Page) HTML() string { return p.call("html").String() }

// Console is what the page logged ("level: message").
func (p *Page) Console() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.console...)
}

// Exists reports whether an element matches loc now.
func (p *Page) Exists(loc string) bool {
	p.t.Helper()
	return p.call("exists", loc).ToBoolean()
}

// Count is how many elements match a CSS selector now.
func (p *Page) Count(selector string) int {
	p.t.Helper()
	return int(p.call("count", selector).ToInteger())
}

// Text is an element's text, whitespace collapsed and trimmed.
func (p *Page) Text(loc string) string {
	p.t.Helper()
	return p.call("text", p.locate(loc)).String()
}

// Attr is an element's attribute and whether it has it.
func (p *Page) Attr(loc, name string) (string, bool) {
	p.t.Helper()
	v := p.call("attr", p.locate(loc), name)
	if goja.IsNull(v) {
		return "", false
	}
	return v.String(), true
}

// Value is a field's current value — what the user sees, not its value
// attribute. A multiple select's values are joined with commas.
func (p *Page) Value(field string) string {
	p.t.Helper()
	return p.call("value", p.locate(field)).String()
}

// Checked reports whether a checkbox, radio or option is checked.
func (p *Page) Checked(field string) bool {
	p.t.Helper()
	return p.call("checked", p.locate(field)).ToBoolean()
}

// ---- expectations ----------------------------------------------------------

// Expect starts an assertion on loc's element. Each assertion retries —
// settling the page and waiting for the server — until it holds or the
// timeout passes, then fails the test.
func (p *Page) Expect(loc string) *Expectation { return &Expectation{p: p, loc: loc} }

// Expectation is an assertion about one element.
type Expectation struct {
	p   *Page
	loc string
}

// check retries test until it holds; test reports whether it does and what
// it found.
func (e *Expectation) check(want string, test func(el goja.Value) (bool, string)) *Expectation {
	e.p.t.Helper()
	p := e.p
	deadline := time.Now().Add(p.s.timeout)
	for {
		p.settle()
		el, err := p.b.Call("find", e.loc)
		if err != nil {
			p.t.Fatalf("viewtest: %q: %v", e.loc, err)
		}
		if goja.IsNull(el) {
			el = nil
		}
		ok, got := test(el)
		if ok {
			return e
		}
		if time.Now().After(deadline) {
			ctx := clip(p.HTML(), 4000)
			if el != nil {
				ctx = clip(p.call("html", el).String(), 2000)
			}
			p.t.Fatalf("viewtest: expected %q %s, got %s\n%s", e.loc, want, got, ctx)
		}
		p.b.WaitTask(20 * time.Millisecond)
	}
}

// element wraps a test that needs the element to exist.
func (e *Expectation) element(want string, test func(el goja.Value) (bool, string)) *Expectation {
	e.p.t.Helper()
	return e.check(want, func(el goja.Value) (bool, string) {
		if el == nil {
			return false, "no such element"
		}
		return test(el)
	})
}

// Exists expects an element to match.
func (e *Expectation) Exists() *Expectation {
	e.p.t.Helper()
	return e.element("to exist", func(goja.Value) (bool, string) { return true, "" })
}

// Absent expects no element to match.
func (e *Expectation) Absent() *Expectation {
	e.p.t.Helper()
	return e.check("to be absent", func(el goja.Value) (bool, string) { return el == nil, "an element" })
}

// Count expects the locator, as a CSS selector, to match n elements.
func (e *Expectation) Count(n int) *Expectation {
	e.p.t.Helper()
	return e.check(fmt.Sprintf("to match %d elements", n), func(goja.Value) (bool, string) {
		got := e.p.Count(e.loc)
		return got == n, fmt.Sprint(got)
	})
}

// Text expects the element's text (whitespace collapsed and trimmed).
func (e *Expectation) Text(want string) *Expectation {
	e.p.t.Helper()
	return e.element(fmt.Sprintf("to have text %q", want), func(el goja.Value) (bool, string) {
		got := e.p.call("text", el).String()
		return got == want, fmt.Sprintf("%q", got)
	})
}

// ContainsText expects the element's text to contain sub.
func (e *Expectation) ContainsText(sub string) *Expectation {
	e.p.t.Helper()
	return e.element(fmt.Sprintf("to contain text %q", sub), func(el goja.Value) (bool, string) {
		got := e.p.call("text", el).String()
		return strings.Contains(got, sub), fmt.Sprintf("%q", got)
	})
}

// Attr expects the element to have attribute name with value want.
func (e *Expectation) Attr(name, want string) *Expectation {
	e.p.t.Helper()
	return e.element(fmt.Sprintf("to have %s=%q", name, want), func(el goja.Value) (bool, string) {
		v := e.p.call("attr", el, name)
		if goja.IsNull(v) {
			return false, "no " + name
		}
		return v.String() == want, fmt.Sprintf("%s=%q", name, v.String())
	})
}

// NoAttr expects the element not to have attribute name.
func (e *Expectation) NoAttr(name string) *Expectation {
	e.p.t.Helper()
	return e.element("not to have "+name, func(el goja.Value) (bool, string) {
		v := e.p.call("attr", el, name)
		return goja.IsNull(v), fmt.Sprintf("%s=%q", name, v.String())
	})
}

// Value expects a field's current value (see Page.Value).
func (e *Expectation) Value(want string) *Expectation {
	e.p.t.Helper()
	return e.element(fmt.Sprintf("to have value %q", want), func(el goja.Value) (bool, string) {
		got := e.p.call("value", el).String()
		return got == want, fmt.Sprintf("%q", got)
	})
}

func (e *Expectation) is(what, call string, want bool) *Expectation {
	e.p.t.Helper()
	desc := "to be " + what
	return e.element(desc, func(el goja.Value) (bool, string) {
		got := e.p.call(call, el).ToBoolean()
		if got == want {
			return true, ""
		}
		return false, "not so"
	})
}

// Enabled and Disabled expect the element (a field or button, or one in a
// disabled fieldset) to be enabled or disabled.
func (e *Expectation) Enabled() *Expectation {
	e.p.t.Helper()
	return e.is("enabled", "disabled", false)
}

func (e *Expectation) Disabled() *Expectation {
	e.p.t.Helper()
	return e.is("disabled", "disabled", true)
}

// Checked and Unchecked expect a checkbox, radio or option's state.
func (e *Expectation) Checked() *Expectation {
	e.p.t.Helper()
	return e.is("checked", "checked", true)
}

func (e *Expectation) Unchecked() *Expectation {
	e.p.t.Helper()
	return e.is("unchecked", "checked", false)
}

// Visible and Hidden expect the element to be shown or not: hidden means
// it, or an ancestor, has the hidden attribute (a view if-branch that does
// not hold, say), or sits in a template, script or the head.
func (e *Expectation) Visible() *Expectation {
	e.p.t.Helper()
	return e.is("visible", "visible", true)
}

func (e *Expectation) Hidden() *Expectation {
	e.p.t.Helper()
	return e.is("hidden", "visible", false)
}

// Focused expects the element to have focus.
func (e *Expectation) Focused() *Expectation {
	e.p.t.Helper()
	return e.is("focused", "focused", true)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
