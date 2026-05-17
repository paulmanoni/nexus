package template

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/paulmanoni/nexus/live"
)

// Engine is the central coordinator: it owns the component registry,
// shared helpers, and the optional notifier used to push external
// state mutations to every connected session.
//
// One Engine is typically created at app boot and mounted under
// multiple HTTP paths via Handler. It's safe for concurrent use; the
// registry is RWMutex-guarded and reads dominate (registration is a
// boot-time activity).
type Engine struct {
	notifier      *live.Notifier
	helpers       map[string]any
	checkOrigin   func(*http.Request) bool
	userExtractor func(*http.Request) any
	idleTimeout   time.Duration // 0 = no idle timeout
	parkTTL       time.Duration // 0 = no session resumption
	sendBuffer    int           // outgoing-queue depth per session (default 64)

	// staticMounts collects WithStatic options so the adapter
	// can register a gin static handler for each. Subdirs are
	// resolved against the same fs.FS the engine loads
	// templates from — typical use is the //go:embed of
	// nl-island ES modules sitting next to the .nlt files.
	staticMounts []staticMount

	stats engineStats

	mu       sync.RWMutex
	registry map[string]*componentDef
	// routes maps URL path → component name so the session can
	// resolve a client "navigate" message to the right component.
	// Populated by the adapter when AsComponent has nexus.Path
	// configured; missing entries fall through as 404-style navs.
	routes map[string]string

	// Session tracking is used by hot reload to broadcast a reload
	// frame to every connected client when a template source
	// changes on disk. Sessions self-register on Run start and
	// deregister on Run exit.
	sessionsMu sync.Mutex
	sessions   map[*Session]struct{}

	// parked holds component instances from sessions whose WS
	// connection dropped, keyed by resumption token. The reaper
	// goroutine sweeps expired entries periodically; on reconnect
	// with a matching token the engine claims the entry and
	// resumes the session with preserved state.
	parkedMu sync.Mutex
	parked   map[string]*parkedSession
}

// parkedSession is the state needed to resume a session after a
// transient disconnect: the live component instance (carries
// per-tab fields like Filter / NewTitle), the last Rendered tree
// (so the first frame after resume can be sent as-is), and the
// captured user value. Deadline bounds how long the entry stays
// claimable.
type parkedSession struct {
	component Component
	prev      Rendered
	user      any
	deadline  time.Time
}

// Option configures an Engine at construction time.
type Option func(*Engine)

// WithNotifier wires the engine to a [live.Notifier]. Every session
// subscribes on join; an external Notify() triggers a re-render on
// each connected component. Pass nil (or omit) for components that
// only update in response to client events.
func WithNotifier(n *live.Notifier) Option {
	return func(e *Engine) { e.notifier = n }
}

// WithEngineHelpers registers shared helper functions available to
// every template the engine renders. Per-render WithHelpers is also
// supported and merges on top.
func WithEngineHelpers(h map[string]any) Option {
	return func(e *Engine) {
		if e.helpers == nil {
			e.helpers = make(map[string]any, len(h))
		}
		for k, v := range h {
			e.helpers[k] = v
		}
	}
}

// WithCheckOrigin overrides the WebSocket-upgrade origin check.
// Default is same-origin: the request's Origin header host must
// match the Host header. Override to allow specific cross-origin
// pairings (e.g., dashboard mounted on a different host), or to
// pass a permissive func(*http.Request) bool { return true } during
// local development.
//
// A nil func is treated as "use the default same-origin check".
func WithCheckOrigin(fn func(*http.Request) bool) Option {
	return func(e *Engine) { e.checkOrigin = fn }
}

// WithSessionResumption enables short-window resumption of a
// session after a WS disconnect. When set to a non-zero duration,
// every joined session is assigned an opaque token (sent in the
// "joined" frame); on transport close the session's component is
// parked under that token. A reconnect that presents the same
// token within ttl gets back the same component instance — Filter,
// NewTitle, and other per-tab state survive across the gap.
//
// Zero (the default) disables resumption: every reconnect is a
// fresh Mount with default state. Most apps want at least 30s for
// graceful handling of transient network blips and laptop sleep.
//
// Memory cost: one parked entry per disconnected session, bounded
// by ttl. Tabs closed cleanly still park for ttl before the reaper
// frees them — keep ttl modest unless you've sized the heap for it.
func WithSessionResumption(ttl time.Duration) Option {
	return func(e *Engine) { e.parkTTL = ttl }
}

// staticMount records one subdir-of-FS → URL-prefix pairing
// declared via WithStatic. The adapter walks the slice on first
// AsComponent and registers each via gin.StaticFS.
type staticMount struct {
	sub       string
	mountPath string
}

// WithStatic serves an embed.FS subdirectory at an HTTP path.
// sub is the directory inside the fs.FS supplied to Module();
// the optional mountPath is the URL prefix. With no mountPath
// the URL defaults to "/" + the cleaned sub — so
// WithStatic("islands") serves the islands/ subdir at
// /islands/. Pass an explicit second arg when the URL needs to
// differ from the directory name (e.g. mounting under
// /__nexus/islands).
//
// Typical use is nl-island ES modules embedded alongside .nlt
// templates in the same go:embed — saves wiring a separate
// nexus.Invoke + StaticFS for every app.
//
//	//go:embed templates/*.nlt islands/*.js
//	var assets embed.FS
//
//	template.Module(assets,
//	    template.WithStatic("islands"),                    // → /islands/
//	    template.WithStatic("images", "/__nexus/assets"),  // → /__nexus/assets/
//	)
//
// Trailing slashes on sub are tolerated; leading slashes on
// mountPath are added if missing. Variadic for ergonomics —
// passing more than one mountPath uses the first; extras are
// ignored rather than rejected so existing callers writing
// WithStatic("islands", "") keep compiling.
func WithStatic(sub string, mountPath ...string) Option {
	sub = strings.Trim(sub, "/")
	mp := ""
	if len(mountPath) > 0 {
		mp = mountPath[0]
	}
	if mp == "" {
		mp = "/" + sub
	} else if !strings.HasPrefix(mp, "/") {
		mp = "/" + mp
	}
	mp = strings.TrimRight(mp, "/")
	return func(e *Engine) {
		e.staticMounts = append(e.staticMounts, staticMount{sub: sub, mountPath: mp})
	}
}

// WithSendBuffer sets the per-session outgoing-queue depth.
// Each session has a bounded channel between the render/event
// goroutine and the writer goroutine that pushes frames onto
// the transport; this knob is the queue's depth.
//
// When a client is too slow to drain the queue (slow network,
// frozen tab), the queue fills and the session closes its
// transport to release backpressure on the server side. A
// shallower queue triggers this sooner (cheaper memory, more
// false positives on bursty servers); a deeper queue absorbs
// transient spikes (more memory, slower detection of truly
// stuck clients).
//
// Default 64 — typically a few seconds of diffs for chatty
// pages. Zero or negative restores the default.
func WithSendBuffer(n int) Option {
	return func(e *Engine) { e.sendBuffer = n }
}

// WithIdleTimeout closes sessions that see no client message, no
// notifier wake, and no self-notify for the given duration. The
// session goroutine exits cleanly and any parked-session
// resumption state expires with it. Zero (the default) disables
// the timeout — sessions live as long as the WS connection.
//
// Set this in production. Long-lived idle sessions are how a
// long-tail of forgotten browser tabs accumulates into memory
// pressure that's only obvious during an incident.
func WithIdleTimeout(d time.Duration) Option {
	return func(e *Engine) { e.idleTimeout = d }
}

// WithUserExtractor wires the engine to whatever auth middleware
// is upstream of it. The provided func runs once per session join
// (SSR and WS) with the inbound *http.Request and returns whatever
// the app considers "the authenticated user" — a struct pointer,
// a map of claims, or nil for anonymous. The value lands on
// Ctx.User and stays attached to every handler invocation for the
// duration of the session.
//
// Typical wiring with nexus's auth extension:
//
//	template.WithUserExtractor(func(r *http.Request) any {
//	    return r.Context().Value(authpkg.UserContextKey)
//	})
//
// Without this option, Ctx.User is always nil — components that
// gate behavior on the user can fall back to refusing the action.
func WithUserExtractor(fn func(*http.Request) any) Option {
	return func(e *Engine) { e.userExtractor = fn }
}

// New builds a fresh Engine. Most apps create exactly one.
func New(opts ...Option) *Engine {
	e := &Engine{
		registry: make(map[string]*componentDef),
		sessions: make(map[*Session]struct{}),
		parked:   make(map[string]*parkedSession),
		routes:   make(map[string]string),
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// RegisterRoute records the URL path a component is mounted at so
// live-navigate messages from the client can resolve to the right
// component. Called by the adapter when AsComponent has a
// nexus.Path option; never called for child-only components.
//
// Re-registering the same path overwrites — useful in dev /
// hot-reload and consistent with Register's idempotence.
func (e *Engine) RegisterRoute(path, componentName string) {
	if path == "" || componentName == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.routes[path] = componentName
}

// lookupRoute returns the component name registered for path, or
// "" if no live component handles it.
func (e *Engine) lookupRoute(path string) string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.routes[path]
}

// sendBufferOrDefault resolves the configured outgoing-queue
// depth to a positive value. Negative or zero falls back to
// defaultSendBuffer so tests and unset-option callers get
// reasonable behavior.
func (e *Engine) sendBufferOrDefault() int {
	if e.sendBuffer > 0 {
		return e.sendBuffer
	}
	return defaultSendBuffer
}

// parkSession stashes a disconnected session's component under
// its token with a TTL-bounded deadline. No-op when resumption is
// disabled or the session has no token (defensive — every joined
// session should have one when parkTTL>0).
func (e *Engine) parkSession(token string, s *Session) {
	if e.parkTTL <= 0 || token == "" {
		return
	}
	e.parkedMu.Lock()
	defer e.parkedMu.Unlock()
	e.parked[token] = &parkedSession{
		component: s.component,
		prev:      s.prev,
		user:      s.user,
		deadline:  time.Now().Add(e.parkTTL),
	}
	e.stats.sessionsParked.Add(1)
}

// claimParked removes and returns the parked entry for token if
// it exists and hasn't expired. Returns nil otherwise — caller
// falls through to a fresh Mount.
func (e *Engine) claimParked(token string) *parkedSession {
	if token == "" {
		return nil
	}
	e.parkedMu.Lock()
	defer e.parkedMu.Unlock()
	p, ok := e.parked[token]
	if !ok {
		return nil
	}
	delete(e.parked, token)
	if time.Now().After(p.deadline) {
		return nil
	}
	return p
}

// reaperLoop periodically removes expired parked entries. Started
// by template.Module's lifecycle hook and runs until the supplied
// ctx is cancelled. Sweep period is parkTTL/2 so an entry lives at
// most ~1.5 * parkTTL before cleanup.
func (e *Engine) reaperLoop(ctx context.Context) {
	if e.parkTTL <= 0 {
		return
	}
	period := e.parkTTL / 2
	if period < time.Second {
		period = time.Second
	}
	t := time.NewTicker(period)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := time.Now()
			e.parkedMu.Lock()
			for k, p := range e.parked {
				if now.After(p.deadline) {
					delete(e.parked, k)
				}
			}
			e.parkedMu.Unlock()
		}
	}
}

// StartReaper spawns the parked-session reaper goroutine. Safe to
// call when parkTTL is zero (the goroutine exits immediately).
// Typically invoked from template.Module's lifecycle hook.
func (e *Engine) StartReaper(ctx context.Context) {
	go e.reaperLoop(ctx)
}

// Register pairs a component name with its parsed template source
// and a factory that produces fresh instances. The .nlt source is
// parsed and lowered once at registration; each session gets a new
// component via the factory but shares the cached Fragment.
//
// Re-registering the same name overwrites — intentional for tests
// and for the planned dev-mode reloader.
func (e *Engine) Register(name string, src []byte, factory func() Component) error {
	if name == "" {
		return fmt.Errorf("Register: name is empty")
	}
	if factory == nil {
		return fmt.Errorf("Register(%q): factory is nil", name)
	}
	file, err := Parse(name+".nlt", src)
	if err != nil {
		return fmt.Errorf("Register(%q): parse: %w", name, err)
	}
	frag, err := Lower(file.Template)
	if err != nil {
		return fmt.Errorf("Register(%q): lower: %w", name, err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.registry[name] = &componentDef{
		name:     name,
		fragment: frag,
		factory:  factory,
		script:   file.Script,
		style:    file.Style,
		scopeID:  computeScopeID(name),
	}
	return nil
}

// lookup returns the registered definition for a component name, or
// nil + false if unknown. Public consumers use this via the session
// indirectly — there's no need to expose registry internals.
func (e *Engine) lookup(name string) (*componentDef, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	d, ok := e.registry[name]
	return d, ok
}

// Resolve satisfies ComponentResolver so the interpreter can expand
// <Foo /> tags inside a template into rendered sub-trees. Returns
// a fresh component instance per call — child components in v1 are
// pure renders, so reusing instances across renders would just leak
// per-parent-render state into a shared cell.
func (e *Engine) Resolve(name string) (Component, *Fragment, error) {
	def, ok := e.lookup(name)
	if !ok {
		return nil, nil, fmt.Errorf("component %q not registered", name)
	}
	component := def.factory()
	if component == nil {
		return nil, nil, fmt.Errorf("component %q: factory returned nil", name)
	}
	return component, def.fragment, nil
}

// Reload re-parses src for an already-registered component and
// broadcasts a "reload" frame to every connected session so they
// hard-refresh and pick up the new template. Used by hot-reload
// in dev (see WithHotReload); safe to call at runtime — Register
// is idempotent and reload-frame broadcasting is best-effort.
//
// Returns an error if the new source fails to parse/lower; the
// existing registration is left untouched in that case so a
// malformed save doesn't bring the live page down.
func (e *Engine) Reload(name string, src []byte) error {
	if _, ok := e.lookup(name); !ok {
		return fmt.Errorf("Reload(%q): not registered", name)
	}
	// Snapshot existing factory so we preserve it across reload —
	// only the parsed Fragment (the static markup) changes; the
	// component constructor doesn't.
	old, _ := e.lookup(name)
	if err := e.Register(name, src, old.factory); err != nil {
		return err
	}
	e.broadcastReload()
	return nil
}

func (e *Engine) trackSession(s *Session) {
	e.sessionsMu.Lock()
	e.sessions[s] = struct{}{}
	e.sessionsMu.Unlock()
	e.stats.sessionsOpen.Add(1)
	e.stats.sessionsTotal.Add(1)
	if s.resumed {
		e.stats.sessionsResumed.Add(1)
	}
}

func (e *Engine) untrackSession(s *Session) {
	e.sessionsMu.Lock()
	delete(e.sessions, s)
	e.sessionsMu.Unlock()
	e.stats.sessionsOpen.Add(-1)
}

// Stats returns a point-in-time snapshot of the engine's
// observability counters. Lock-free; safe to call concurrently
// from any goroutine. Individual counters may be slightly out of
// sync relative to each other on a busy engine.
func (e *Engine) Stats() Stats {
	return e.stats.snapshot()
}

// broadcastReload sends Outbound{Type:"reload"} to every connected
// session. Send errors are swallowed — a session that's already
// closing has nothing to learn from a reload nudge. Background
// context is fine here: the WS transport applies its own write
// deadline (10s default), and a stuck send shouldn't block the
// hot-reload watcher goroutine.
func (e *Engine) broadcastReload() {
	e.sessionsMu.Lock()
	snapshot := make([]*Session, 0, len(e.sessions))
	for s := range e.sessions {
		snapshot = append(snapshot, s)
	}
	e.sessionsMu.Unlock()
	ctx := context.Background()
	for _, s := range snapshot {
		_ = s.send(ctx, Outbound{Type: "reload"})
	}
}

// componentDef is the engine-side bundle for one registered template.
// fragment is shared across sessions (immutable after lowering);
// factory makes a fresh, mutable Component per session.
type componentDef struct {
	name     string
	fragment *Fragment
	factory  func() Component
	script   *Script // retained for future dev-mode introspection
	style    *Style  // emitted as <style> in the SSR shell; scoped via scopeID
	// scopeID is the stable per-component attribute key used for
	// CSS scoping. Stamped on the SSR container as
	// data-nl-scope="<id>"; every selector in style.Body is
	// rewritten to require [data-nl-scope="<id>"] when
	// style.Scoped is true.
	scopeID string
}
