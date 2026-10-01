package view

import (
	"encoding/json"
	"errors"
	"reflect"

	"github.com/a-h/templ"
)

// Signal is a piece of browser state, created in a component with State:
//
//	{{ count := view.State(ctx, 0) }}
//	<button onclick={ count.Set(count.Get() + 1) }>+1</button>
//	<p>Clicked { count.Get() } times</p>
//
// Markup that reads it with Get stays current in the browser. Set is its
// one action: in an on* attribute it describes what the event does, runs in
// the browser, and changes nothing on the server. Anything that needs the
// event goes through Do.
//
// A value that comes back from the browser (a shard re-render) is user
// input: validate it before acting on it.
type Signal[T any] struct {
	id string
	v  T
}

// Get returns the value: on the server, the one the page renders with; in
// the browser, the current one.
func (s *Signal[T]) Get() T {
	if s == nil {
		var zero T
		return zero
	}
	return s.v
}

// ID is the signal's identity on the page.
func (s *Signal[T]) ID() string {
	if s == nil {
		return ""
	}
	return s.id
}

// Set is the action that replaces the value.
func (s *Signal[T]) Set(v T) templ.ComponentScript { return uncompiled }

// Event is the DOM event an action written with Do receives.
type Event struct {
	Target EventTarget
	Key    string
}

// EventTarget is the element the event fired on.
type EventTarget struct {
	Value   string
	Checked bool
}

// Do is an action written as a Go function — for one that reads the event
// or takes more than one step:
//
//	oninput={ view.Do(func(e view.Event) { q.Set(e.Target.Value) }) }
//
// The Go compiler type-checks fn; the generator compiles it to JavaScript.
// It never runs on the server.
func Do(fn func(e Event)) templ.ComponentScript { return uncompiled }

// uncompiled is what an action renders as when the templates were generated
// with plain templ instead of nexus (nexus dev, nexus build, nexus generate views).
var uncompiled = templ.ComponentScript{
	Call: `console.warn('nexus view: this action was not compiled - generate the templates with nexus (nexus dev, nexus build, nexus generate views)')`,
}

// NexusSchemaAs types a signal as its value in the client SDK: an island
// receives the value.
func (*Signal[T]) NexusSchemaAs() reflect.Type { return reflect.TypeFor[T]() }

type signalWire[T any] struct {
	ID string `json:"$sig"`
	V  T      `json:"v"`
}

// MarshalJSON writes the reference the browser restores as a live signal.
func (s *Signal[T]) MarshalJSON() ([]byte, error) {
	if s == nil {
		return []byte("null"), nil
	}
	return json.Marshal(signalWire[T]{ID: s.id, V: s.v})
}

// UnmarshalJSON reads a reference the browser sent back, keeping its id.
func (s *Signal[T]) UnmarshalJSON(b []byte) error {
	var w signalWire[T]
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	if w.ID == "" {
		return errors.New(`view: a signal needs {"$sig": id, "v": value}`)
	}
	s.id, s.v = w.ID, w.V
	return nil
}
