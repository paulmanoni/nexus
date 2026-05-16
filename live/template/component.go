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

// EventDispatcher lets a component opt out of reflection-based
// event routing and handle dispatch itself. Useful for generic
// components (e.g. a form that routes every "field-change" through
// one handler) or for tests. Components that don't implement it get
// reflection-based routing as the default.
type EventDispatcher interface {
	HandleEvent(ctx *Ctx, event string, payload Payload) error
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

	// Push sends a one-off client event to the connected browser.
	// Useful for toasts, scroll-to, "session-expired" prompts that
	// aren't state changes. The client JS dispatches by event name.
	Push func(event string, payload any)

	// Notify schedules a re-render. Call after async work finishes
	// (e.g. a goroutine'd request completes) to surface the new
	// state. Re-renders triggered this way coalesce with mutator-
	// triggered re-renders; the user never sees duplicate frames.
	Notify func()
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
