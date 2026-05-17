package template

import (
	"context"
	"fmt"
	"net/http"
	"sync"

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

	mu       sync.RWMutex
	registry map[string]*componentDef

	// Session tracking is used by hot reload to broadcast a reload
	// frame to every connected client when a template source
	// changes on disk. Sessions self-register on Run start and
	// deregister on Run exit.
	sessionsMu sync.Mutex
	sessions   map[*Session]struct{}
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
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
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
	defer e.sessionsMu.Unlock()
	e.sessions[s] = struct{}{}
}

func (e *Engine) untrackSession(s *Session) {
	e.sessionsMu.Lock()
	defer e.sessionsMu.Unlock()
	delete(e.sessions, s)
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
	style    *Style  // retained for future scoped-style emission
}
