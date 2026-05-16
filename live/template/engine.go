package template

import (
	"fmt"
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
	notifier *live.Notifier
	helpers  map[string]any

	mu       sync.RWMutex
	registry map[string]*componentDef
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

// New builds a fresh Engine. Most apps create exactly one.
func New(opts ...Option) *Engine {
	e := &Engine{registry: make(map[string]*componentDef)}
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
