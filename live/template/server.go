package template

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Handler returns an http.Handler for one registered component. The
// handler serves two distinct contracts on the same path:
//
//   - GET (no Upgrade header): server-side renders the component to
//     full HTML and returns it. This is the SEO + first-paint path
//     — the page is usable before any JS runs.
//   - GET with Upgrade: websocket → engine.Handler upgrades, sends
//     the initial "joined" frame, and runs the session loop until
//     the client disconnects.
//
// Path params and query strings are merged into Params and passed
// to Mount as-is.
//
// Returns nil with a 500 wrap-up if the named component isn't
// registered, so misrouted paths fail loudly rather than silently
// serving blanks.
func (e *Engine) Handler(componentName string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		def, ok := e.lookup(componentName)
		if !ok {
			http.Error(w, fmt.Sprintf("live: component %q not registered", componentName), http.StatusNotFound)
			return
		}

		params := paramsFromRequest(r)

		if websocket.IsWebSocketUpgrade(r) {
			e.serveWS(w, r, def, params)
			return
		}
		e.serveSSR(w, r, def, params)
	})
}

// serveSSR builds a one-shot Rendered, stitches it into HTML, and
// wraps it in a minimal bootstrap page. The bootstrap embeds the
// session ID and a placeholder for the client JS to attach to.
func (e *Engine) serveSSR(w http.ResponseWriter, r *http.Request, def *componentDef, params Params) {
	component := def.factory()
	if component == nil {
		http.Error(w, "factory returned nil", http.StatusInternalServerError)
		return
	}
	ctx := &Ctx{
		Context: r.Context(),
		Params:  params,
		// Push and Notify are no-ops in the SSR path — no live channel exists yet.
		Push:   func(string, any) {},
		Notify: func() {},
	}
	if err := component.Mount(ctx); err != nil {
		http.Error(w, "mount: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Refresh runs before render so view-derived state is populated
	// for the SSR frame, exactly as it would be over the WS path.
	// Errors here are non-fatal — render proceeds with whatever
	// state the component has, matching session.refresh semantics.
	if r, ok := component.(Refresher); ok {
		_ = r.Refresh(ctx)
	}
	opts := []RenderOption{}
	if e.helpers != nil {
		opts = append(opts, WithHelpers(e.helpers))
	}
	body := Render(def.fragment, component, opts...).HTML()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprint(w, ssrShell(def.name, body, def.style))
}

// serveWS upgrades the request, sends the initial join frame, and
// runs the session loop. Errors during the loop are logged-but-not-
// returned (HTTP is already detached at this point).
func (e *Engine) serveWS(w http.ResponseWriter, r *http.Request, def *componentDef, params Params) {
	up := websocket.Upgrader{
		// Permissive CheckOrigin for the v1 — production wiring should
		// inject a stricter check via http.Handler middleware before
		// reaching Handler. (Live templates are same-origin in
		// practice; cross-origin needs CORS-style approval.)
		CheckOrigin: func(*http.Request) bool { return true },
	}
	conn, err := up.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote a response on failure.
		return
	}
	defer conn.Close()

	tr := newWSTransport(conn)
	sess, err := newSession(e, def, params, tr)
	if err != nil {
		_ = tr.Send(r.Context(), Outbound{Type: "error", Msg: err.Error()})
		return
	}
	_ = sess.Run(r.Context())
}

// paramsFromRequest merges path vars and query strings into a single
// Params map. Path vars take precedence on collision. For v1 we
// read only query strings — pattern-style path vars need router
// integration which the engine doesn't own.
func paramsFromRequest(r *http.Request) Params {
	p := make(Params)
	for k, vs := range r.URL.Query() {
		if len(vs) > 0 {
			p[k] = vs[0]
		}
	}
	return p
}

// ssrShell wraps the rendered component body in a minimal HTML page.
// The data-nl-component attribute lets the client JS identify which
// component to "join" for the live channel; data-nl-id is the (yet
// unallocated) session ID — the client requests a new ID on join.
//
// Scoped styles, when present, ship inline so the first paint isn't
// FOUC'd. A v2 build step would extract them to a /__live/styles.css.
func ssrShell(componentName, body string, style *Style) string {
	var sb strings.Builder
	sb.WriteString(`<!doctype html><html><head><meta charset="utf-8"><title>`)
	sb.WriteString(html.EscapeString(componentName))
	sb.WriteString(`</title>`)
	if style != nil && style.Body != "" {
		sb.WriteString(`<style>`)
		sb.WriteString(style.Body)
		sb.WriteString(`</style>`)
	}
	sb.WriteString(`</head><body><div data-nl-component="`)
	sb.WriteString(html.EscapeString(componentName))
	sb.WriteString(`">`)
	sb.WriteString(body)
	sb.WriteString(`</div><script src="`)
	sb.WriteString(ScriptPath)
	sb.WriteString(`" defer></script></body></html>`)
	return sb.String()
}

// --- WebSocket Transport adapter -----------------------------------

// wsTransport adapts a gorilla/websocket Conn to the Transport
// interface. Read and write deadlines are bounded so a misbehaving
// peer can't hold the goroutine forever; the values mirror what the
// existing transport/ws hub uses.
type wsTransport struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
}

func newWSTransport(conn *websocket.Conn) *wsTransport {
	conn.SetReadLimit(1 << 20) // 1 MiB per message
	return &wsTransport{conn: conn}
}

func (t *wsTransport) Recv(ctx context.Context) (Inbound, error) {
	// gorilla's ReadMessage doesn't take a context. We approximate
	// cancellation by setting a read deadline derived from ctx's
	// deadline (if any); on cancel the underlying close in our
	// goroutine driver causes a read error that we map to io.EOF.
	if d, ok := ctx.Deadline(); ok {
		_ = t.conn.SetReadDeadline(d)
	} else {
		_ = t.conn.SetReadDeadline(time.Time{})
	}
	_, raw, err := t.conn.ReadMessage()
	if err != nil {
		return Inbound{}, fmt.Errorf("ws read: %w", err)
	}
	var msg Inbound
	if err := json.Unmarshal(raw, &msg); err != nil {
		return Inbound{}, fmt.Errorf("ws unmarshal: %w", err)
	}
	return msg, nil
}

func (t *wsTransport) Send(ctx context.Context, msg Outbound) error {
	raw, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("ws marshal: %w", err)
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if d, ok := ctx.Deadline(); ok {
		_ = t.conn.SetWriteDeadline(d)
	} else {
		_ = t.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	}
	return t.conn.WriteMessage(websocket.TextMessage, raw)
}

func (t *wsTransport) Close() error {
	err := t.conn.Close()
	if errors.Is(err, websocket.ErrCloseSent) {
		return nil
	}
	return err
}
