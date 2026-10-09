package dev

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/a-h/templ"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/httpx"
	"github.com/paulmanoni/nexus/v2/trace"
)

// ToolbarEnabled reports whether the debug toolbar is on: under nexus dev
// only, unless nexus.toml turns it off:
//
//	[runtime.toolbar]
//	enabled = false
func ToolbarEnabled() bool {
	return Enabled() && config.Get("runtime.toolbar.enabled", true)
}

// Request is one HTTP request as the debug toolbar shows it: what the
// browser asked, what the app answered, the spans its traces recorded
// (SQL statements among them), the log records written under it, and what
// panels noted while it ran.
type Request struct {
	ID       string
	Method   string
	Path     string
	Query    url.Values
	Header   http.Header
	Status   int
	Start    time.Time
	Duration time.Duration
	// Spans are the request's trace spans, root first, filled when the
	// request ends.
	Spans []*trace.SpanNode

	mu       sync.Mutex
	traceIDs []string
	logs     []LogRecord
	notes    map[string][]any
}

// LogRecord is a log record written while a request ran.
type LogRecord struct {
	Time    time.Time
	Level   slog.Level
	Message string
	Attrs   map[string]any
}

// Logs is what the app logged under the request: records written with a
// context (InfoContext, …) that carries it.
func (r *Request) Logs() []LogRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.logs)
}

// Notes is what Note recorded for panel during the request.
func (r *Request) Notes(panel string) []any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.notes[panel])
}

// TraceIDs are the traces the request opened (one per traced route).
func (r *Request) TraceIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.traceIDs)
}

type requestKey struct{}

// RequestFrom is the toolbar's record of the request ctx belongs to; nil
// outside nexus dev, or when the toolbar is off.
func RequestFrom(ctx context.Context) *Request {
	r, _ := ctx.Value(requestKey{}).(*Request)
	return r
}

// Note records v for panel on the request ctx belongs to — what a panel's
// Render reads back with Request.Notes. A no-op outside nexus dev, so it
// may stay in production code:
//
//	dev.Note(ctx, "Cache", CacheHit{Key: key, Hit: ok})
func Note(ctx context.Context, panel string, v any) {
	r := RequestFrom(ctx)
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.notes == nil {
		r.notes = map[string][]any{}
	}
	r.notes[panel] = append(r.notes[panel], v)
	r.mu.Unlock()
}

// Panel is a tab of the debug toolbar. Render runs when the toolbar shows
// a request, and returns what the tab displays; Name is its title.
//
//	dev.AddPanel(dev.Panel{Name: "Cache", Render: func(r *dev.Request) dev.Section {
//		hits := r.Notes("Cache")
//		return dev.Section{Summary: fmt.Sprintf("%d lookups", len(hits)), …}
//	}})
type Panel struct {
	Name   string
	Render func(r *Request) Section
}

// Section is what a panel shows: Summary beside its title in the toolbar's
// list, Stats as figures at the top, then Text (preformatted), a templ View,
// HTML (the app's own markup, shown as is) and a Table — whichever are set.
type Section struct {
	Summary string `json:"summary,omitempty"`
	// Tone colours the summary: "", "warn" or "error".
	Tone  string `json:"tone,omitempty"`
	Stats []Stat `json:"stats,omitempty"`
	Table *Table `json:"table,omitempty"`
	Text  string `json:"text,omitempty"`
	// View is a templ component the panel renders, styled by the
	// toolbar's stylesheet (the classes of toolbar.css) or its own.
	View templ.Component `json:"-"`
	HTML string          `json:"html,omitempty"`
}

// Stat is a figure at the top of a panel.
type Stat struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Tone  string `json:"tone,omitempty"`
}

// Table is a panel's rows. Tones, when set, colours each row ("", "warn"
// or "error").
type Table struct {
	Columns []Column `json:"columns"`
	Rows    [][]any  `json:"rows"`
	Tones   []string `json:"tones,omitempty"`
}

// Column is a table column: Code shows its cells in a monospace block
// (JSON, stacks), Lang "sql" also highlights SQL, Num aligns them right,
// and Bar draws each number as a bar against the column's largest.
type Column struct {
	Title string `json:"title"`
	Code  bool   `json:"code,omitempty"`
	Lang  string `json:"lang,omitempty"`
	Num   bool   `json:"num,omitempty"`
	Bar   bool   `json:"bar,omitempty"`
}

var panels = struct {
	sync.Mutex
	list []Panel
}{}

// AddPanel adds a tab to the debug toolbar, after the built-in ones; a
// panel of the same name replaces it. Call it from init or main: it costs
// nothing outside nexus dev.
func AddPanel(p Panel) {
	panels.Lock()
	defer panels.Unlock()
	if i := slices.IndexFunc(panels.list, func(q Panel) bool { return q.Name == p.Name }); i >= 0 {
		panels.list[i] = p
		return
	}
	panels.list = append(panels.list, p)
}

func allPanels() []Panel {
	panels.Lock()
	defer panels.Unlock()
	out := builtinPanels()
	for _, p := range panels.list {
		if i := slices.IndexFunc(out, func(q Panel) bool { return q.Name == p.Name }); i >= 0 {
			out[i] = p
		} else {
			out = append(out, p)
		}
	}
	return out
}

// SpanSource is where the toolbar reads a trace's spans: the app's trace bus.
type SpanSource interface {
	Spans(traceID string) []*trace.SpanNode
}

// history keeps the latest requests for the toolbar.
type history struct {
	mu    sync.Mutex
	byID  map[string]*Request
	order []string
}

const historySize = 200

func (h *history) add(r *Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.byID[r.ID] = r
	h.order = append(h.order, r.ID)
	if len(h.order) > historySize {
		delete(h.byID, h.order[0])
		h.order = h.order[1:]
	}
}

func (h *history) get(id string) *Request {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.byID[id]
}

var requests = &history{byID: map[string]*Request{}}

// HeaderName is the response header carrying a request's toolbar ID, which
// the toolbar reads off fetch and XHR responses to list them.
const HeaderName = "X-Nexus-Toolbar"

// Toolbar is the whole-mux middleware of the debug toolbar: it records each
// request, and puts the toolbar on every HTML page it answers.
func Toolbar(spans SpanSource) httpx.HandlerFunc {
	return func(c *httpx.Ctx) {
		path := c.Path()
		if path == "/__nexus" || strings.HasPrefix(path, "/__nexus/") || c.Request.Header.Get("Upgrade") != "" {
			c.Next()
			return
		}
		r := &Request{
			ID:     newToolbarID(),
			Method: c.Request.Method,
			Path:   path,
			Query:  c.Request.URL.Query(),
			Header: c.Request.Header.Clone(),
			Start:  time.Now(),
		}
		ctx := context.WithValue(c.Request.Context(), requestKey{}, r)
		ctx = trace.OnRoot(ctx, func(s *trace.Span) {
			r.mu.Lock()
			r.traceIDs = append(r.traceIDs, s.TraceID)
			r.mu.Unlock()
		})
		page := isPageRequest(c.Request)
		if page && c.Request.Header.Get("Accept-Encoding") != "" {
			// The app's own compression may run inside this middleware; a
			// page it can't compress is one the toolbar can be added to.
			c.Request.Header = c.Request.Header.Clone()
			c.Request.Header.Del("Accept-Encoding")
		}
		c.Request = c.Request.WithContext(ctx)
		c.Writer.Header().Set(HeaderName, r.ID)
		w := &injector{ResponseWriter: c.Writer.ResponseWriter, page: page, id: r.ID}
		c.Writer.ResponseWriter = w
		defer func() {
			r.Duration = time.Since(r.Start)
			r.Status = c.Writer.Status()
			if spans != nil {
				for _, id := range r.TraceIDs() {
					r.Spans = append(r.Spans, spans.Spans(id)...)
				}
			}
			requests.add(r)
			w.finish()
		}()
		c.Next()
	}
}

// isPageRequest is whether the browser loads the response as a page
// (navigation or frame), not a fetch: only those get the toolbar.
func isPageRequest(r *http.Request) bool {
	if r.Method != http.MethodGet || r.Header.Get("X-Inertia") != "" {
		return false
	}
	switch r.Header.Get("Sec-Fetch-Dest") {
	case "", "document", "iframe":
		return true
	}
	return false
}

func newToolbarID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// injector holds back an HTML page to add the toolbar's script before its
// </body>; anything else passes straight through.
type injector struct {
	http.ResponseWriter
	page    bool
	id      string
	decided bool
	hold    bool
	status  int
	buf     bytes.Buffer
}

func (w *injector) decide(first []byte) {
	if w.decided {
		return
	}
	w.decided = true
	if !w.page || w.Header().Get("Content-Encoding") != "" {
		return
	}
	ct := w.Header().Get("Content-Type")
	if ct == "" && first != nil {
		ct = http.DetectContentType(first)
	}
	w.hold = strings.HasPrefix(ct, "text/html")
}

func (w *injector) WriteHeader(code int) {
	w.decide(nil)
	if w.hold {
		w.status = code
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *injector) Write(b []byte) (int, error) {
	if !w.decided {
		w.decide(b)
		if w.hold && w.status == 0 {
			w.status = http.StatusOK
		}
	}
	if w.hold {
		return w.buf.Write(b)
	}
	return w.ResponseWriter.Write(b)
}

func (w *injector) Flush() {
	if w.hold {
		return
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *injector) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, errors.New("dev: the response writer can't be hijacked")
}

// finish sends a held page, with the toolbar when it is a whole document.
func (w *injector) finish() {
	if !w.hold {
		return
	}
	body := w.buf.Bytes()
	if at := bytes.LastIndex(bytes.ToLower(body), []byte("</body>")); at >= 0 {
		tag := `<script src="/__nexus/toolbar/toolbar.js" data-nexus-toolbar="` + w.id + `" defer></script>`
		body = append(body[:at:at], append([]byte(tag), w.buf.Bytes()[at:]...)...)
	}
	w.Header().Del("Content-Length")
	w.ResponseWriter.WriteHeader(w.status)
	_, _ = w.ResponseWriter.Write(body)
}

// ToolbarLogs wraps h so the records written under a request also reach
// the toolbar's Logs panel.
func ToolbarLogs(h slog.Handler) slog.Handler { return &logTee{h: h} }

type logTee struct {
	h     slog.Handler
	attrs []slog.Attr
}

func (t *logTee) Enabled(ctx context.Context, l slog.Level) bool {
	return RequestFrom(ctx) != nil || t.h.Enabled(ctx, l)
}

func (t *logTee) Handle(ctx context.Context, rec slog.Record) error {
	if r := RequestFrom(ctx); r != nil {
		attrs := map[string]any{}
		for _, a := range t.attrs {
			attrs[a.Key] = a.Value.Any()
		}
		rec.Attrs(func(a slog.Attr) bool {
			attrs[a.Key] = a.Value.Resolve().Any()
			return true
		})
		r.mu.Lock()
		r.logs = append(r.logs, LogRecord{Time: rec.Time, Level: rec.Level, Message: rec.Message, Attrs: attrs})
		r.mu.Unlock()
	}
	if !t.h.Enabled(ctx, rec.Level) {
		return nil
	}
	return t.h.Handle(ctx, rec)
}

func (t *logTee) WithAttrs(as []slog.Attr) slog.Handler {
	return &logTee{h: t.h.WithAttrs(as), attrs: append(slices.Clip(t.attrs), as...)}
}

func (t *logTee) WithGroup(name string) slog.Handler {
	return &logTee{h: t.h.WithGroup(name), attrs: t.attrs}
}
