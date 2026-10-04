package viewtest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// BrowserPage is a page of the app opened in a real, headless Chrome — for
// what the test DOM can't check: layout and CSS (Box, Visible), islands,
// third-party scripts, a screenshot. Locators work as in Page: a field
// name, else a CSS selector.
type BrowserPage struct {
	t       testing.TB
	s       settings
	srv     *httptest.Server
	c       *chrome
	session string
	loaded  chan struct{}
}

// Rect is an element's box in CSS pixels, relative to the viewport.
type Rect struct {
	X, Y, Width, Height float64
}

// Browser opens path of app in headless Chrome. It skips the test when no
// Chrome is found — set NEXUS_CHROME to its binary when it isn't installed
// where Chrome usually is. The options are Page's: As and Header become
// request headers (the live socket included), Cookie a cookie, Timeout how
// long it waits.
//
//	p := viewtest.Browser(t, app, "/", viewtest.As(staff))
//	p.Click("#run").Expect("#ran").Text("1")
//	if !p.Visible("#save") { t.Fatal("the save button is off screen") }
func Browser(t testing.TB, app http.Handler, path string, opts ...Option) *BrowserPage {
	t.Helper()
	bin := findChrome()
	if bin == "" {
		t.Skip("viewtest.Browser: no Chrome found (set NEXUS_CHROME)")
	}
	s := newSettings(opts)
	c, err := launchChrome(bin, 1280, 800)
	if err != nil {
		t.Fatalf("viewtest.Browser: %v", err)
	}
	p := &BrowserPage{t: t, s: s, srv: httptest.NewServer(app), c: c, loaded: make(chan struct{}, 8)}
	t.Cleanup(p.Close)

	ctx, cancel := p.ctx()
	defer cancel()
	var target struct {
		TargetID string `json:"targetId"`
	}
	if err := c.call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}, &target); err != nil {
		t.Fatalf("viewtest.Browser: %v", err)
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := c.call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": target.TargetID, "flatten": true}, &attached); err != nil {
		t.Fatalf("viewtest.Browser: %v", err)
	}
	p.session = attached.SessionID
	c.on(func(m cdpMessage) {
		if m.SessionID == p.session && m.Method == "Page.loadEventFired" {
			select {
			case p.loaded <- struct{}{}:
			default:
			}
		}
	})
	for _, m := range []string{"Page.enable", "Runtime.enable", "Network.enable"} {
		p.must(m, nil, nil)
	}
	if len(s.header) > 0 {
		h := map[string]string{}
		for k, vs := range s.header {
			h[k] = strings.Join(vs, ", ")
		}
		p.must("Network.setExtraHTTPHeaders", map[string]any{"headers": h}, nil)
	}
	for _, ck := range s.cookies {
		p.must("Network.setCookie", map[string]any{"name": ck.Name, "value": ck.Value, "url": p.srv.URL, "path": "/"}, nil)
	}
	return p.Visit(path)
}

func (p *BrowserPage) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), p.s.timeout)
}

func (p *BrowserPage) must(method string, params, out any) {
	p.t.Helper()
	ctx, cancel := p.ctx()
	defer cancel()
	if err := p.c.call(ctx, p.session, method, params, out); err != nil {
		p.t.Fatalf("viewtest.Browser: %v", err)
	}
}

// Close closes Chrome and the server. It runs on test cleanup.
func (p *BrowserPage) Close() {
	if p.c != nil {
		p.c.close()
		p.c = nil
	}
	if p.srv != nil {
		p.srv.Close()
		p.srv = nil
	}
}

// Visit loads path and waits for the page's load event.
func (p *BrowserPage) Visit(path string) *BrowserPage {
	p.t.Helper()
	for len(p.loaded) > 0 {
		<-p.loaded
	}
	u := path
	if !strings.HasPrefix(path, "http") {
		u = p.srv.URL + path
	}
	var nav struct {
		ErrorText string `json:"errorText"`
	}
	p.must("Page.navigate", map[string]any{"url": u}, &nav)
	if nav.ErrorText != "" {
		p.t.Fatalf("viewtest.Browser: loading %s: %s", u, nav.ErrorText)
	}
	select {
	case <-p.loaded:
	case <-time.After(p.s.timeout):
		p.t.Fatalf("viewtest.Browser: %s did not finish loading", u)
	}
	return p
}

// Eval runs a JavaScript expression in the page (a promise is awaited) and
// returns its JSON value.
func (p *BrowserPage) Eval(expr string) any {
	p.t.Helper()
	v, err := p.eval(expr)
	if err != nil {
		p.t.Fatalf("viewtest.Browser: %v", err)
	}
	return v
}

func (p *BrowserPage) eval(expr string) (any, error) {
	ctx, cancel := p.ctx()
	defer cancel()
	var out struct {
		Result struct {
			Value any `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	err := p.c.call(ctx, p.session, "Runtime.evaluate", map[string]any{
		"expression": expr, "awaitPromise": true, "returnByValue": true,
	}, &out)
	if err != nil {
		return nil, err
	}
	if e := out.ExceptionDetails; e != nil {
		msg := e.Exception.Description
		if msg == "" {
			msg = e.Text
		}
		return nil, fmt.Errorf("evaluating %s: %s", expr, msg)
	}
	return out.Result.Value, nil
}

// find is the page-side locator: a field by name, else a CSS selector.
func find(loc string) string {
	q, _ := json.Marshal(loc)
	return fmt.Sprintf(`(function(l){var e=null;try{e=document.querySelector('[name="'+l.replace(/"/g,'\\"')+'"]')}catch(_){}if(!e){try{e=document.querySelector(l)}catch(_){}}return e})(%s)`, q)
}

// Box is the element's border box, in viewport coordinates.
func (p *BrowserPage) Box(loc string) Rect {
	p.t.Helper()
	v := p.Eval(`(function(){var e=` + find(loc) + `;if(!e)return null;var r=e.getBoundingClientRect();return {x:r.x,y:r.y,w:r.width,h:r.height}})()`)
	m, ok := v.(map[string]any)
	if !ok {
		p.t.Fatalf("viewtest.Browser: no element %q", loc)
	}
	f := func(k string) float64 { n, _ := m[k].(float64); return n }
	return Rect{f("x"), f("y"), f("w"), f("h")}
}

// visibleJS reports whether an element is rendered with a size, shown by
// CSS, and at least partly inside the viewport.
func visibleJS(loc string) string {
	return `(function(){var e=` + find(loc) + `;if(!e)return false;var r=e.getBoundingClientRect();var s=getComputedStyle(e);` +
		`return r.width>0&&r.height>0&&s.visibility!=="hidden"&&s.display!=="none"&&parseFloat(s.opacity)>0&&` +
		`r.bottom>0&&r.right>0&&r.top<innerHeight&&r.left<innerWidth})()`
}

// Visible reports whether the element is laid out with a size, not hidden
// by CSS, and at least partly in the viewport.
func (p *BrowserPage) Visible(loc string) bool {
	p.t.Helper()
	v, _ := p.Eval(visibleJS(loc)).(bool)
	return v
}

// Text is the element's rendered text (innerText).
func (p *BrowserPage) Text(loc string) string {
	p.t.Helper()
	v, _ := p.Eval(`(function(){var e=` + find(loc) + `;return e?e.innerText.trim():null})()`).(string)
	return v
}

// URL is the page's current address.
func (p *BrowserPage) URL() string {
	v, _ := p.Eval("location.href").(string)
	return v
}

// Click clicks the middle of the element with the mouse — so it hits what
// is really there: an element covered by another isn't clicked.
func (p *BrowserPage) Click(loc string) *BrowserPage {
	p.t.Helper()
	p.Eval(`(function(){var e=` + find(loc) + `;if(e)e.scrollIntoView({block:"center",inline:"center"})})()`)
	b := p.Box(loc)
	if b.Width == 0 || b.Height == 0 {
		p.t.Fatalf("viewtest.Browser: %q has no size to click", loc)
	}
	x, y := b.X+b.Width/2, b.Y+b.Height/2
	for _, typ := range []string{"mouseMoved", "mousePressed", "mouseReleased"} {
		p.must("Input.dispatchMouseEvent", map[string]any{"type": typ, "x": x, "y": y, "button": "left", "clickCount": 1}, nil)
	}
	return p
}

// Fill replaces the field's text by typing value, as a user would.
func (p *BrowserPage) Fill(field, value string) *BrowserPage {
	p.t.Helper()
	ok, _ := p.Eval(`(function(){var e=` + find(field) + `;if(!e)return false;e.focus();if(e.select)e.select();return true})()`).(bool)
	if !ok {
		p.t.Fatalf("viewtest.Browser: no field %q", field)
	}
	if value == "" {
		p.must("Input.dispatchKeyEvent", map[string]any{"type": "keyDown", "key": "Backspace", "code": "Backspace", "windowsVirtualKeyCode": 8}, nil)
		p.must("Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": "Backspace", "code": "Backspace", "windowsVirtualKeyCode": 8}, nil)
		return p
	}
	p.must("Input.insertText", map[string]any{"text": value}, nil)
	return p
}

// Viewport resizes the page's viewport to width × height CSS pixels — a
// phone-sized one, say. Media queries follow it.
func (p *BrowserPage) Viewport(width, height int) *BrowserPage {
	p.t.Helper()
	p.must("Emulation.setDeviceMetricsOverride", map[string]any{
		"width": width, "height": height, "deviceScaleFactor": 1, "mobile": false,
	}, nil)
	return p
}

// Screenshot writes a PNG of the viewport to path.
func (p *BrowserPage) Screenshot(path string) *BrowserPage {
	p.t.Helper()
	var shot struct {
		Data string `json:"data"`
	}
	p.must("Page.captureScreenshot", map[string]any{"format": "png"}, &shot)
	b, err := base64.StdEncoding.DecodeString(shot.Data)
	if err != nil {
		p.t.Fatalf("viewtest.Browser: screenshot: %v", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		p.t.Fatalf("viewtest.Browser: %v", err)
	}
	return p
}

// Expect checks the element, retrying until it holds or Timeout passes —
// the live page re-renders after events, so a check right after a Click
// waits for it.
func (p *BrowserPage) Expect(loc string) *BrowserExpectation {
	return &BrowserExpectation{p: p, loc: loc}
}

// BrowserExpectation is a retrying check on one element of a BrowserPage.
type BrowserExpectation struct {
	p   *BrowserPage
	loc string
}

func (e *BrowserExpectation) holds(what string, test func() (bool, string)) *BrowserExpectation {
	e.p.t.Helper()
	deadline := time.Now().Add(e.p.s.timeout)
	var got string
	for {
		ok, g := test()
		if ok {
			return e
		}
		got = g
		if time.Now().After(deadline) {
			e.p.t.Fatalf("viewtest.Browser: %s: want %s, got %s", e.loc, what, got)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// Text waits for the element's rendered text to equal want.
func (e *BrowserExpectation) Text(want string) *BrowserExpectation {
	return e.holds(fmt.Sprintf("text %q", want), func() (bool, string) {
		got := e.p.Text(e.loc)
		return got == want, fmt.Sprintf("%q", got)
	})
}

// ContainsText waits for the element's text to contain want.
func (e *BrowserExpectation) ContainsText(want string) *BrowserExpectation {
	return e.holds(fmt.Sprintf("text containing %q", want), func() (bool, string) {
		got := e.p.Text(e.loc)
		return strings.Contains(got, want), fmt.Sprintf("%q", got)
	})
}

// Visible waits for the element to be laid out, shown and in the viewport.
func (e *BrowserExpectation) Visible() *BrowserExpectation {
	return e.holds("visible", func() (bool, string) { return e.p.Visible(e.loc), "not visible" })
}

// Hidden waits for the element to be absent or not visible.
func (e *BrowserExpectation) Hidden() *BrowserExpectation {
	return e.holds("hidden", func() (bool, string) { return !e.p.Visible(e.loc), "visible" })
}

// ExpectURL waits for the page's address to end with suffix (a path).
func (p *BrowserPage) ExpectURL(suffix string) *BrowserPage {
	p.t.Helper()
	deadline := time.Now().Add(p.s.timeout)
	for !strings.HasSuffix(strings.TrimSuffix(p.URL(), "/"), strings.TrimSuffix(suffix, "/")) {
		if time.Now().After(deadline) {
			u, _ := url.Parse(p.URL())
			p.t.Fatalf("viewtest.Browser: URL %s, want a path ending %s", u.Path, suffix)
		}
		time.Sleep(25 * time.Millisecond)
	}
	return p
}
