package template

import "context"

// Component is the live-template authoring contract. A user-defined
// component is a Go struct that:
//
//   - implements Mount(ctx *Ctx) error to seed state on connect
//   - declares its state as exported fields (the template reaches
//     them by name via reflection)
//   - declares event handlers as methods whose name is the TitleCased
//     event name (template @click="like" → method Like)
//
// Components are paired with a parsed Fragment at registration time;
// the engine holds the Fragment and instantiates the component via
// the factory passed to Register.
type Component interface {
	Mount(ctx *Ctx) error
}

// TemplateNamer is the optional contract a component implements to
// declare its own .nlt source path, replacing the
// template.WithTemplate option on AsComponent. The adapter calls
// TemplateName(nil) once at registration time — implementations
// MUST treat a nil ctx gracefully and return a stable path, since
// no session or request context exists yet. Use template.Ctx fields
// only when you've already returned the static branch:
//
//	func (c *PostRow) TemplateName(ctx *template.Ctx) string {
//	    return "templates/PostRow"
//	}
//
// When neither WithTemplate nor TemplateName is provided, the
// adapter falls back to the convention "templates/<ComponentName>"
// — covers the 90% case where a component named "PostRow" has its
// source at templates/PostRow.nlt.
//
// Override precedence (first wins):
//  1. template.WithTemplate("path") on AsComponent
//  2. (Component).TemplateName(nil)
//  3. "templates/" + spec.Name
type TemplateNamer interface {
	TemplateName(ctx *Ctx) string
}

// EventDispatcher lets a component opt out of reflection-based
// event routing and handle dispatch itself. Useful for generic
// components (e.g. a form that routes every "field-change" through
// one handler) or for tests. Components that don't implement it get
// reflection-based routing as the default.
type EventDispatcher interface {
	HandleEvent(ctx *Ctx, event string, payload Payload) error
}

// Topicer is an optional hook a component implements to scope its
// notifier subscription to specific topics. When implemented, the
// session subscribes to each returned topic on join (via
// notifier.SubscribeTopic) and re-renders only when those topics
// fire — instead of waking on every global Notify across the app.
//
// Critical for apps with many concurrent sessions: without topics,
// a single mutation in one corner of the system re-renders every
// connected live page. With topics, only the sessions actually
// interested in "post:42" wake when post 42 changes.
//
// Components that don't implement Topicer fall back to broadcast
// subscription (every Notify wakes them), preserving the v0
// behavior. Return an empty slice to opt out entirely (the
// session won't subscribe to anything — useful for client-only
// state components driven solely by events).
//
// Topics() is called once at session start; the result is cached
// for the session's lifetime. Topics can't change mid-session.
type Topicer interface {
	Topics(ctx *Ctx) []string
}

// Refresher is an optional hook the session calls before every
// render — both the initial render after Mount and every
// subsequent re-render (event-, notifier-, or self-Notify-
// triggered). Use it to recompute view-derived state from external
// sources (a repo, a dataloader cache) so that a notifier-
// triggered re-render sees fresh data even though no handler ran.
//
// Without Refresh, derived state stored on the component drifts
// from upstream sources between handler invocations. The workaround
// is to expose a method like Posts() and call it as {{ Posts() }};
// Refresh lets you keep a plain Posts field and {{ Posts }}
// instead.
//
// Errors returned from Refresh emit an "error" frame to the client
// but do not abort the render — the previous component state is
// what gets sent. Refresh is expected to be idempotent and cheap;
// expensive work belongs in event handlers or background goroutines
// that signal completion via Ctx.Notify.
type Refresher interface {
	Refresh(ctx *Ctx) error
}

// Ctx is what handler methods receive. It carries the request
// context, route params, and the session-level helpers: Push for
// server-initiated client events, Notify to nudge a re-render after
// async work completes.
//
// Each event handler invocation gets a freshly constructed Ctx with
// Context derived from the connection-level cancel.
type Ctx struct {
	Context context.Context
	Params  Params

	// User is whatever the engine's WithUserExtractor returned for
	// the HTTP request that started this session — typically a
	// *User struct from the auth middleware, a claims map, or nil
	// for anonymous. Components that gate behavior on auth read
	// it once per handler invocation; the value is identity-stable
	// across every Ctx of the same session.
	User any

	// Push sends a one-off client event to the connected browser.
	// Useful for toasts, scroll-to, "session-expired" prompts that
	// aren't state changes. The client JS dispatches by event name.
	Push func(event string, payload any)

	// Notify schedules a re-render. Call after async work finishes
	// (e.g. a goroutine'd request completes) to surface the new
	// state. Re-renders triggered this way coalesce with mutator-
	// triggered re-renders; the user never sees duplicate frames.
	Notify func()

	// Stream returns the handle for one nl-stream container by
	// name. Use it to push incremental updates (append/prepend/
	// delete/update/reset) into the matching DOM container
	// without re-rendering the surrounding template. See
	// StreamRef for the trade-offs vs nl-for.
	Stream func(name string) *StreamRef

	// PushIsland dispatches an event to every <element
	// nl-island="<name>"> on the page. The island's mount(el,
	// props, channel) receives a channel handle whose
	// on(event, fn) listener fires when this is called. Use
	// it to push live signals (a new snapshot, a "reset"
	// command, etc.) into a client-side widget — VueFlow,
	// chart, anything wrapped as an island — without
	// re-rendering the surrounding live template.
	//
	// payload is JSON-marshaled and arrives on the client as
	// the second argument to the listener.
	PushIsland func(name, event string, payload any)
}

// Params is the merged set of route + query parameters available to
// Mount. Filled in by the HTTP layer when the WS join arrives.
type Params map[string]string

// Payload is the JSON object the client ships alongside an event.
// Typical contents: {"value": "...", "id": "..."} pulled from the
// element's :data-* attributes or the input's value. Handlers
// destructure via type assertion or a helper like ev.String("id").
type Payload map[string]any

// String pulls a string-typed value out of a payload. Missing key
// or wrong type returns the zero value — handlers that need
// stricter validation can check (v, ok := p[k].(string)) directly.
func (p Payload) String(k string) string {
	if s, ok := p[k].(string); ok {
		return s
	}
	return ""
}

// Int pulls an int-typed value. JSON numbers arrive as float64 from
// encoding/json; we handle that case in addition to the integer
// types so handlers don't need to second-guess the wire format.
func (p Payload) Int(k string) int {
	switch v := p[k].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case string:
		// String numeric values are common when payloads come from
		// HTML data-attributes (which are always strings on the
		// wire). Best-effort parse, zero on failure.
		var n int
		for _, c := range v {
			if c < '0' || c > '9' {
				return 0
			}
			n = n*10 + int(c-'0')
		}
		return n
	}
	return 0
}

// Bool reads a boolean. "true" / "false" string forms are honored
// for HTML data-attribute round-tripping; everything else returns
// false.
func (p Payload) Bool(k string) bool {
	switch v := p[k].(type) {
	case bool:
		return v
	case string:
		return v == "true"
	}
	return false
}

// BaseComponent is an optional embed for components that don't have
// their own Mount logic. Embedding it satisfies the Component
// interface with a zero-effort default; users can still define
// methods (events) freely.
type BaseComponent struct{}

func (BaseComponent) Mount(ctx *Ctx) error { return nil }
