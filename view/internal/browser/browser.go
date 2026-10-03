// Package browser runs the view runtime against a small DOM in goja: a page
// realm with documents, events, form fields, selectors, a virtual clock,
// fetch and WebSocket, whose network is supplied by the host. It is what
// viewtest drives and what the runtime's own tests run on. It is not a
// browser: no layout, no CSS, and no scripts but the ones the host runs.
package browser

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

//go:embed dom.js
var domJS string

// Response is a fetch's answer.
type Response struct {
	Status     int
	URL        string // the final URL, after redirects
	Header     http.Header
	Body       string
	Redirected bool
}

// Conn is an open WebSocket, as the host dialled it.
type Conn interface {
	Send(msg string) error
	Close() error
}

// Host is the page's network. Dial's recv and closed may be called from any
// goroutine; the browser runs them on its own loop.
type Host struct {
	Fetch func(method, url string, header http.Header, body string) (*Response, error)
	Dial  func(url string, recv func(msg string), closed func()) (Conn, error)
	// Navigate is a full page load the page asked for: a plain link, a form
	// nothing handled, location.assign.
	Navigate func(method, url, body, contentType string)
	// Console receives console output ("log", "warn", "error", …).
	Console func(level, msg string)
}

// Browser is one page realm.
type Browser struct {
	VM   *goja.Runtime
	host Host
	dom  *goja.Object

	mu     sync.Mutex
	queue  []func()
	notify chan struct{}

	sockets map[int]*socket
	nextID  int
	closed  bool
}

type socket struct {
	js   *goja.Object
	conn Conn
}

// New makes a page realm with the DOM installed and an empty document.
func New(host Host) (*Browser, error) {
	b := &Browser{VM: goja.New(), host: host, notify: make(chan struct{}, 1), sockets: map[int]*socket{}}
	g := b.VM.NewObject()
	set := func(name string, fn any) { _ = g.Set(name, fn) }
	set("parseDocument", func(s string) string { return mustJSON(parseDocument(s)) })
	set("parseFragment", func(s, context string) string { return mustJSON(parseFragment(s, context)) })
	set("resolveURL", resolveURL)
	set("console", func(level, msg string) {
		if b.host.Console != nil {
			b.host.Console(level, msg)
		}
	})
	set("navigate", func(method, u, body, contentType string) {
		if b.host.Navigate != nil {
			b.host.Navigate(method, u, body, contentType)
		}
	})
	set("fetch", b.fetch)
	set("wsOpen", b.wsOpen)
	set("wsSend", b.wsSend)
	set("wsClose", b.wsClose)
	if err := b.VM.Set("__go", g); err != nil {
		return nil, err
	}
	if _, err := b.VM.RunScript("dom.js", domJS); err != nil {
		return nil, fmt.Errorf("browser: dom.js: %w", err)
	}
	b.dom = b.VM.Get("__dom").ToObject(b.VM)
	return b, nil
}

// Load makes html, served from url, the page's document.
func (b *Browser) Load(htmlDoc, pageURL string) error {
	_, err := b.Call("load", htmlDoc, pageURL)
	return err
}

// Run runs a script in the page.
func (b *Browser) Run(name, src string) error {
	_, err := b.VM.RunScript(name, src)
	return err
}

// Call calls a function of the DOM's host handle (__dom in dom.js).
func (b *Browser) Call(name string, args ...any) (goja.Value, error) {
	fn, ok := goja.AssertFunction(b.dom.Get(name))
	if !ok {
		return nil, fmt.Errorf("browser: no __dom.%s", name)
	}
	vals := make([]goja.Value, len(args))
	for i, a := range args {
		vals[i] = b.VM.ToValue(a)
	}
	return fn(goja.Undefined(), vals...)
}

// Close closes the page's sockets; nothing the network sends reaches it
// afterwards.
func (b *Browser) Close() {
	b.mu.Lock()
	b.closed = true
	b.queue = nil
	b.mu.Unlock()
	for _, s := range b.sockets {
		_ = s.conn.Close()
	}
	b.sockets = map[int]*socket{}
}

// post queues fn for the browser's loop; safe from any goroutine.
func (b *Browser) post(fn func()) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.queue = append(b.queue, fn)
	b.mu.Unlock()
	select {
	case b.notify <- struct{}{}:
	default:
	}
}

// RunTasks runs the tasks waiting (network events), and reports how many ran.
func (b *Browser) RunTasks() int {
	n := 0
	for {
		b.mu.Lock()
		if len(b.queue) == 0 {
			b.mu.Unlock()
			return n
		}
		fn := b.queue[0]
		b.queue = b.queue[1:]
		b.mu.Unlock()
		fn()
		n++
	}
}

// WaitTask blocks until a task is queued or d passes; it reports whether
// one is.
func (b *Browser) WaitTask(d time.Duration) bool {
	b.mu.Lock()
	n := len(b.queue)
	b.mu.Unlock()
	if n > 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-b.notify:
		return true
	case <-t.C:
		return false
	}
}

// NextTimer is the virtual time the next timer is due at, or -1.
func (b *Browser) NextTimer() float64 {
	v, err := b.Call("nextTimer")
	if err != nil {
		return -1
	}
	return v.ToFloat()
}

// Now is the page's virtual clock, in milliseconds.
func (b *Browser) Now() float64 {
	v, _ := b.Call("now")
	return v.ToFloat()
}

// RunTimer advances the clock to the next timer and runs it.
func (b *Browser) RunTimer() bool {
	v, err := b.Call("runTimer")
	return err == nil && v.ToBoolean()
}

func (b *Browser) fetch(method, u, headersJSON, body string) string {
	if b.host.Fetch == nil {
		return mustJSON(map[string]string{"error": "no network"})
	}
	var hm map[string]string
	_ = json.Unmarshal([]byte(headersJSON), &hm)
	h := http.Header{}
	for k, v := range hm {
		h.Set(k, v)
	}
	res, err := b.host.Fetch(method, u, h, body)
	if err != nil {
		return mustJSON(map[string]string{"error": err.Error()})
	}
	headers := map[string]string{}
	for k := range res.Header {
		headers[strings.ToLower(k)] = res.Header.Get(k)
	}
	return mustJSON(map[string]any{"status": res.Status, "url": res.URL, "headers": headers, "body": res.Body, "redirected": res.Redirected})
}

func (b *Browser) wsOpen(u string, js *goja.Object) int {
	b.nextID++
	id := b.nextID
	s := &socket{js: js}
	b.sockets[id] = s
	closeJS := func(code int) {
		b.post(func() {
			if b.sockets[id] == s {
				delete(b.sockets, id)
			}
			b.callMethod(js, "_close", code)
		})
	}
	if b.host.Dial == nil {
		closeJS(1006)
		return id
	}
	conn, err := b.host.Dial(u,
		func(msg string) { b.post(func() { b.callMethod(js, "_message", msg) }) },
		func() { closeJS(1006) },
	)
	if err != nil {
		b.Console("error", fmt.Sprintf("WebSocket connection to %s failed: %v", u, err))
		closeJS(1006)
		return id
	}
	s.conn = conn
	b.post(func() { b.callMethod(js, "_open") })
	return id
}

// Console reports msg as page console output.
func (b *Browser) Console(level, msg string) {
	if b.host.Console != nil {
		b.host.Console(level, msg)
	}
}

func (b *Browser) wsSend(id int, msg string) {
	s := b.sockets[id]
	if s == nil || s.conn == nil {
		return
	}
	if err := s.conn.Send(msg); err != nil {
		b.Console("error", "WebSocket send: "+err.Error())
	}
}

func (b *Browser) wsClose(id int) {
	s := b.sockets[id]
	if s == nil {
		return
	}
	delete(b.sockets, id)
	if s.conn != nil {
		_ = s.conn.Close()
	}
	b.post(func() { b.callMethod(s.js, "_close", 1000) })
}

func (b *Browser) callMethod(o *goja.Object, name string, args ...any) {
	fn, ok := goja.AssertFunction(o.Get(name))
	if !ok {
		return
	}
	vals := make([]goja.Value, len(args))
	for i, a := range args {
		vals[i] = b.VM.ToValue(a)
	}
	_, _ = fn(o, vals...)
}

// ---- HTML ----------------------------------------------------------------

// A parsed node travels to dom.js as nested arrays: [1, tag, ns, attrs,
// children] for an element, [3, text] for text and [8, text] for a comment.

func parseDocument(s string) []any {
	doc, err := html.Parse(strings.NewReader(s))
	if err != nil {
		return nil
	}
	return convertKids(doc)
}

func parseFragment(s, context string) []any {
	ctx := &html.Node{Type: html.ElementNode, Data: context, DataAtom: atom.Lookup([]byte(context))}
	nodes, err := html.ParseFragment(strings.NewReader(s), ctx)
	if err != nil {
		return nil
	}
	out := make([]any, 0, len(nodes))
	for _, n := range nodes {
		if j := convert(n); j != nil {
			out = append(out, j)
		}
	}
	return out
}

func convertKids(n *html.Node) []any {
	out := []any{}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if j := convert(c); j != nil {
			out = append(out, j)
		}
	}
	return out
}

func convert(n *html.Node) []any {
	switch n.Type {
	case html.TextNode:
		return []any{3, n.Data}
	case html.CommentNode:
		return []any{8, n.Data}
	case html.ElementNode:
		ns := n.Namespace
		if ns == "" {
			ns = "html"
		}
		attrs := make([][2]string, 0, len(n.Attr))
		for _, a := range n.Attr {
			name := a.Key
			if a.Namespace != "" {
				name = a.Namespace + ":" + a.Key
			}
			attrs = append(attrs, [2]string{name, a.Val})
		}
		return []any{1, n.Data, ns, attrs, convertKids(n)}
	}
	return nil
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func resolveURL(ref, base string) string {
	fail := mustJSON(map[string]string{"error": "invalid URL"})
	r, err := url.Parse(ref)
	if err != nil {
		return fail
	}
	if base != "" {
		bu, err := url.Parse(base)
		if err != nil {
			return fail
		}
		r = bu.ResolveReference(r)
	}
	if !r.IsAbs() {
		return fail
	}
	if r.Path == "" && (r.Scheme == "http" || r.Scheme == "https") {
		r.Path = "/"
	}
	search := ""
	if r.RawQuery != "" {
		search = "?" + r.RawQuery
	}
	hash := ""
	if r.Fragment != "" {
		hash = "#" + r.EscapedFragment()
	}
	return mustJSON(map[string]string{
		"href":     r.String(),
		"origin":   r.Scheme + "://" + r.Host,
		"protocol": r.Scheme + ":",
		"host":     r.Host,
		"hostname": r.Hostname(),
		"port":     r.Port(),
		"pathname": r.EscapedPath(),
		"search":   search,
		"hash":     hash,
	})
}

// ErrTimeout is Settle's error when the page is still busy at its deadline.
var ErrTimeout = errors.New("timed out")

// Settle runs the page until it is quiet: tasks run as they arrive, busy
// (the host's view of outstanding work) is waited out until timeout, and
// timers due within horizon of virtual time run. stop, when it reports
// true, ends it early (a navigation the host must perform).
func (b *Browser) Settle(horizon float64, timeout time.Duration, busy, stop func() bool) error {
	limit := b.Now() + horizon
	deadline := time.Now().Add(timeout)
	for {
		b.RunTasks()
		if stop != nil && stop() {
			return nil
		}
		if busy != nil && busy() {
			left := time.Until(deadline)
			if left <= 0 {
				return ErrTimeout
			}
			b.WaitTask(left)
			continue
		}
		if next := b.NextTimer(); next >= 0 && next <= limit {
			b.RunTimer()
			continue
		}
		return nil
	}
}
