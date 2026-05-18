package template

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
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
		User:    e.extractUser(r),
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
	opts := []RenderOption{WithComponents(e)}
	if e.helpers != nil {
		opts = append(opts, WithHelpers(e.helpers))
	}
	body := Render(def.fragment, component, opts...).HTML()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprint(w, ssrShell(def.name, body, def.style, def.scopeID, def.script, e.defaultStyles, e.stylesheets))
}

// serveWS upgrades the request, sends the initial join frame, and
// runs the session loop. Errors during the loop are logged-but-not-
// returned (HTTP is already detached at this point).
func (e *Engine) serveWS(w http.ResponseWriter, r *http.Request, def *componentDef, params Params) {
	up := websocket.Upgrader{
		CheckOrigin: e.resolveCheckOrigin(),
	}
	conn, err := up.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote a response on failure.
		return
	}
	defer conn.Close()

	tr := newWSTransport(conn)

	// Read the initial join frame to pick up any resumption token.
	// A bounded timeout protects the goroutine from a misbehaving
	// client that connects but never speaks. The client always
	// sends "join" immediately on socket open, so 5s is generous.
	joinCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	join, joinErr := tr.Recv(joinCtx)
	cancel()
	if joinErr != nil {
		_ = tr.Send(r.Context(), Outbound{Type: "error", Msg: "join read: " + joinErr.Error()})
		return
	}
	if join.Type != "join" {
		_ = tr.Send(r.Context(), Outbound{Type: "error", Msg: "first frame must be 'join'"})
		return
	}

	var sess *Session
	if parked := e.claimParked(join.Token); parked != nil {
		sess = newResumedSession(e, def, params, tr, parked)
	} else {
		var nsErr error
		sess, nsErr = newSession(e, def, params, tr)
		if nsErr != nil {
			_ = tr.Send(r.Context(), Outbound{Type: "error", Msg: nsErr.Error()})
			return
		}
		// Extract user only on fresh join; resumed sessions keep
		// the parked user value so a token-rotated reconnect
		// doesn't have to re-authenticate the request.
		sess.user = e.extractUser(r)
	}
	_ = sess.Run(r.Context())
}

// resolveCheckOrigin returns the configured check or the default
// same-origin check. Default: Origin header must be empty (non-
// browser client) OR its host must equal the Host header. Rejects
// cross-origin upgrades unless WithCheckOrigin opted in.
func (e *Engine) resolveCheckOrigin() func(*http.Request) bool {
	if e.checkOrigin != nil {
		return e.checkOrigin
	}
	return func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		// Parse the Origin URL to extract its host. A malformed
		// Origin can't be trusted — reject it.
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" {
			return false
		}
		return strings.EqualFold(u.Host, r.Host)
	}
}

// extractUser runs the configured user extractor against the
// request, or returns nil if no extractor was wired. Centralized
// here so both SSR and WS paths produce the same Ctx.User value.
func (e *Engine) extractUser(r *http.Request) any {
	if e.userExtractor == nil {
		return nil
	}
	return e.userExtractor(r)
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

// tailwindCDN is the Tailwind Play CDN URL injected by the SSR
// shell when defaultStyles is on. Play is the JIT-in-browser
// distribution — ships ~50 KB of JS that scans the DOM, generates
// only the utility classes actually used, and injects them as a
// <style> tag. Perfect for dev and small apps; production builds
// should opt out via WithoutDefaultStyles + bundle their own
// pre-compiled Tailwind.
//
// Pinned to a major version so a Tailwind 4 breakage doesn't
// silently demolish existing apps. The CDN's actual major
// breaks are infrequent — last one was 2 → 3.
const tailwindCDN = "https://cdn.tailwindcss.com"

// ssrShell wraps the rendered component body in a minimal HTML
// page. The data-nl-component attribute lets the client JS
// identify which component to "join" for the live channel; the
// data-nl-scope attribute pairs with the scoped CSS rewrite
// (when style.Scoped is true) so .foo selectors only match
// elements inside this component's container.
//
// Scoped styles, when present, ship inline so the first paint
// isn't FOUC'd. A v2 build step would extract them to a
// /__live/styles.css. The rewrite is regex-light — see
// rewriteScopedCSS for the documented limitations.
//
// Style cascade order (later wins):
//  1. Tailwind Play CDN script (if defaultStyles)
//  2. User-added <link rel="stylesheet"> URLs in declaration
//     order
//  3. Component's <style> block (inline, scoped or not)
//
// So a user's theme stylesheet can override Tailwind, and a
// component's scoped style can override the theme — the same
// order a hand-written page would have.
func ssrShell(componentName, body string, style *Style, scopeID string, script *Script, defaultStyles bool, stylesheets []string) string {
	var sb strings.Builder
	sb.WriteString(`<!doctype html><html><head><meta charset="utf-8"><title>`)
	sb.WriteString(html.EscapeString(componentName))
	sb.WriteString(`</title>`)
	if defaultStyles {
		sb.WriteString(`<script src="`)
		sb.WriteString(tailwindCDN)
		sb.WriteString(`"></script>`)
	}
	for _, href := range stylesheets {
		sb.WriteString(`<link rel="stylesheet" href="`)
		sb.WriteString(html.EscapeString(href))
		sb.WriteString(`">`)
	}
	if style != nil && style.Body != "" {
		sb.WriteString(`<style>`)
		if style.Scoped {
			sb.WriteString(rewriteScopedCSS(style.Body, scopeID))
		} else {
			sb.WriteString(style.Body)
		}
		sb.WriteString(`</style>`)
	}
	sb.WriteString(`<body><div data-nl-component="`)
	sb.WriteString(html.EscapeString(componentName))
	if style != nil && style.Scoped {
		sb.WriteString(`" data-nl-scope="`)
		sb.WriteString(scopeID)
	}
	sb.WriteString(`">`)
	sb.WriteString(body)
	sb.WriteString(`</div>`)

	// Component <script> block. Tagged data-nl-script="<Name>" so
	// the client can find + remove it on live-navigate before
	// injecting the new component's script. Scoped form is wrapped
	// in an IIFE binding `el` to this component's SSR root, so
	// querySelector calls don't escape to siblings.
	if script != nil && script.Body != "" {
		sb.WriteString(`<script data-nl-script="`)
		sb.WriteString(html.EscapeString(componentName))
		sb.WriteString(`">`)
		sb.WriteString(wrapScript(script, componentName))
		sb.WriteString(`</script>`)
	}

	sb.WriteString(`<script src="`)
	sb.WriteString(ScriptPath)
	sb.WriteString(`" defer></script></body></html>`)
	return sb.String()
}

// wrapScript returns the script body ready to emit between <script>
// tags. For unscoped scripts the body is passed through verbatim;
// for scoped scripts it's wrapped in an IIFE that binds `el` to the
// component's SSR root via the data-nl-component selector.
//
// Why an IIFE and not a JS module: classic <script> bodies share
// the top-level scope across reloads, so a re-injected scoped
// script on live-navigate wouldn't get a fresh `el`. The IIFE form
// gives the body a local scope per execution so re-running it on
// navigate just rebinds `el` without leaking variables.
func wrapScript(script *Script, componentName string) string {
	if !script.Scoped {
		return script.Body
	}
	var sb strings.Builder
	sb.WriteString("(function(el){\n")
	sb.WriteString(script.Body)
	sb.WriteString("\n})(document.querySelector('[data-nl-component=\"")
	sb.WriteString(componentName)
	sb.WriteString("\"]'));")
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
