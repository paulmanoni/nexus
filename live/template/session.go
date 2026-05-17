package template

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

// Transport abstracts the wire so the Session loop is testable
// without a real WebSocket. A channel-based Transport flips between
// "test driver" (push Inbound, assert Outbound) and "WS" (a tiny
// adapter in server.go) with no Session-side changes.
type Transport interface {
	// Recv blocks until the next client message arrives, returns
	// io.EOF (or similar) on graceful close, or a non-nil error on
	// transport failure.
	Recv(ctx context.Context) (Inbound, error)

	// Send delivers one server message. Implementations should
	// serialize concurrent calls so a goroutine that fires Notify
	// during an event handler doesn't interleave with the diff
	// being sent for the same handler.
	Send(ctx context.Context, msg Outbound) error

	// Close releases the underlying transport (close the socket,
	// close channels, etc.). Idempotent.
	Close() error
}

// Session is one connected client. It owns the component instance,
// the previous Rendered (basis for the next diff), an event queue,
// and a goroutine that runs the main loop. Sessions never share
// state — the only cross-session signal is via [live.Notifier].
type Session struct {
	id     string
	engine *Engine
	def    *componentDef
	params Params
	tr     Transport

	// user is captured from the upgrade request by serveWS via the
	// engine's configured WithUserExtractor. Stable for the
	// lifetime of the session.
	user any

	component Component
	prev      Rendered

	selfNotify chan struct{} // buffered(1); coalesces Notify calls

	sendMu sync.Mutex
}

// newSession creates and Mount-initializes a session for the given
// component. It does NOT start the loop; call Run for that. Splitting
// construction from the loop lets tests inspect the post-Mount state
// before any frames ship.
func newSession(engine *Engine, def *componentDef, params Params, tr Transport) (*Session, error) {
	component := def.factory()
	if component == nil {
		return nil, fmt.Errorf("factory for %q returned nil", def.name)
	}
	s := &Session{
		id:         randID(),
		engine:     engine,
		def:        def,
		params:     params,
		tr:         tr,
		component:  component,
		selfNotify: make(chan struct{}, 1),
	}
	return s, nil
}

// Run executes the session's main loop. It blocks until the
// transport closes, ctx is cancelled, or an unrecoverable error
// occurs. The loop owns the component — no external goroutine
// touches it once Run is invoked.
//
// First step is to call Mount, render the initial tree, and ship a
// "joined" frame. Subsequent iterations multiplex three sources:
//
//  1. Client events (transport.Recv)
//  2. Engine-level external mutations (live.Notifier subscription)
//  3. Self-notifies from handler-spawned goroutines (Ctx.Notify)
func (s *Session) Run(ctx context.Context) error {
	// Register with the engine so hot-reload can broadcast frames
	// to us, and so test/debug tooling can enumerate live sessions.
	s.engine.trackSession(s)
	defer s.engine.untrackSession(s)

	mountCtx := s.newCtx(ctx)
	if err := s.safeMount(mountCtx); err != nil {
		_ = s.send(ctx, Outbound{Type: "error", Msg: "mount: " + err.Error()})
		return fmt.Errorf("mount: %w", err)
	}

	// Initial render — establishes baseline statics on the client.
	// Refresh runs first so view-derived state (computed in Refresh)
	// is fresh before the very first frame.
	s.refresh(ctx)
	s.prev = s.render()
	if err := s.send(ctx, Outbound{Type: "joined", Rendered: &s.prev}); err != nil {
		return fmt.Errorf("send joined: %w", err)
	}

	// External-mutation subscription. The notifier is shared across
	// sessions; coalescing is its job. Skip if no notifier configured.
	var changeCh <-chan struct{}
	var cancelChange func()
	if s.engine.notifier != nil {
		changeCh, cancelChange = s.engine.notifier.Subscribe()
		defer cancelChange()
	}

	// recvCh carries one Inbound per Recv call. We can't read from
	// Recv inside the select because Recv blocks; instead a single
	// goroutine pumps it. Buffer is 1 because we always drain before
	// re-reading.
	recvCh := make(chan recvResult, 1)
	go s.pumpRecv(ctx, recvCh)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case res := <-recvCh:
			if res.err != nil {
				if errors.Is(res.err, io.EOF) || errors.Is(res.err, context.Canceled) {
					return nil
				}
				return fmt.Errorf("recv: %w", res.err)
			}
			s.handleInbound(ctx, res.msg)
			// Refill the pump for the next inbound message.
			go s.pumpRecv(ctx, recvCh)

		case <-changeCh:
			s.diffAndSend(ctx)

		case <-s.selfNotify:
			s.diffAndSend(ctx)
		}
	}
}

type recvResult struct {
	msg Inbound
	err error
}

func (s *Session) pumpRecv(ctx context.Context, out chan<- recvResult) {
	msg, err := s.tr.Recv(ctx)
	select {
	case out <- recvResult{msg: msg, err: err}:
	case <-ctx.Done():
	}
}

// handleInbound dispatches one client message. Events trigger event
// dispatch; ping echoes a pong; join after the initial join is
// rejected (the engine assumes one session per Run call). The
// reserved event name "__model" is special-cased to handle the
// nl-model two-way binding sugar — see handleModelEvent.
func (s *Session) handleInbound(ctx context.Context, msg Inbound) {
	switch msg.Type {
	case "event":
		if msg.Name == modelEventName {
			s.handleModelEvent(ctx, msg.Payload)
		} else {
			s.dispatchEvent(ctx, msg.Name, msg.Payload)
		}
		s.diffAndSend(ctx)
	case "ping":
		_ = s.send(ctx, Outbound{Type: "pong"})
	case "join":
		// We accept the initial join out-of-band in server.go before
		// invoking Run; an inbound join here is a protocol error.
		_ = s.send(ctx, Outbound{Type: "error", Msg: "duplicate join"})
	default:
		_ = s.send(ctx, Outbound{Type: "error", Msg: "unknown message type: " + msg.Type})
	}
}

// modelEventName is the reserved synthetic event the lowering emits
// for nl-model. Clients should never use this name directly; the
// lowering reserves the leading "__" by convention.
const modelEventName = "__model"

// handleModelEvent processes a v-model-style update: read the LHS
// expression (data-model-expr in the payload), apply any value-
// coercion modifiers (data-model-mods), then assign via reflection
// to the named component field.
//
// Assignment supports dotted-chain expressions (Filter, State.Filter)
// but not indexing or method calls — the lowering already validates
// the expression is a bare identifier chain, so anything that gets
// here is well-formed.
//
// Errors are surfaced as "error" frames but the session continues;
// a malformed model binding shouldn't take down the connection.
func (s *Session) handleModelEvent(ctx context.Context, payload Payload) {
	expr := payload.String("model-expr")
	if expr == "" {
		_ = s.send(ctx, Outbound{Type: "error", Msg: "__model: missing model-expr"})
		return
	}
	value := applyModelMods(payload["value"], payload.String("model-mods"))
	if err := assignField(s.component, expr, value); err != nil {
		_ = s.send(ctx, Outbound{Type: "error", Msg: "__model: " + err.Error()})
	}
}

// applyModelMods runs the value-coercion modifiers shipped in
// data-model-mods. Currently supported:
//
//	trim   — strings.TrimSpace, only when v is a string
//	number — parse v as float64 (or int if no decimal)
//
// Modifiers are applied left-to-right so .trim.number gives the
// expected "strip whitespace, then parse" behavior.
func applyModelMods(v any, mods string) any {
	if mods == "" {
		return v
	}
	for _, m := range strings.Split(mods, ".") {
		switch m {
		case "trim":
			if s, ok := v.(string); ok {
				v = strings.TrimSpace(s)
			}
		case "number":
			switch s := v.(type) {
			case string:
				if n, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
					v = n
				}
			}
		}
	}
	return v
}

// assignField walks expr (a dot-separated identifier chain) on root
// via reflection and stores value at the leaf. Each intermediate
// segment is dereferenced through any pointers automatically;
// the leaf field must be both addressable and settable.
//
// Compatible types assign directly; convertible types (int → int64,
// string → []byte, etc.) go through reflect.Value.Convert. Mismatched
// types return a typed error so the calling __model handler can
// surface it on the wire.
func assignField(root any, expr string, value any) error {
	parts := strings.Split(expr, ".")
	rv := reflect.ValueOf(root)
	for rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return fmt.Errorf("nil receiver while resolving %q", expr)
		}
		rv = rv.Elem()
	}

	// Walk parent segments to reach the field that holds the target.
	for i := 0; i < len(parts)-1; i++ {
		f := rv.FieldByName(parts[i])
		if !f.IsValid() {
			return fmt.Errorf("no field %q in %q", parts[i], expr)
		}
		for f.Kind() == reflect.Ptr {
			if f.IsNil() {
				return fmt.Errorf("nil pointer at %q", parts[i])
			}
			f = f.Elem()
		}
		rv = f
	}

	leaf := parts[len(parts)-1]
	dst := rv.FieldByName(leaf)
	if !dst.IsValid() {
		return fmt.Errorf("no field %q", leaf)
	}
	if !dst.CanSet() {
		return fmt.Errorf("field %q is not settable (unexported?)", leaf)
	}

	if value == nil {
		dst.Set(reflect.Zero(dst.Type()))
		return nil
	}
	src := reflect.ValueOf(value)
	if src.Type() == dst.Type() {
		dst.Set(src)
		return nil
	}
	if src.Type().ConvertibleTo(dst.Type()) {
		dst.Set(src.Convert(dst.Type()))
		return nil
	}
	return fmt.Errorf("cannot assign %T to field %q (%s)", value, leaf, dst.Type())
}

// dispatchEvent routes a named event to a handler on the component.
// Priority: EventDispatcher interface first; then reflection on a
// method whose name is the TitleCased event. Two method shapes are
// supported (only the first matching signature is used):
//
//	func (c *T) Like(ctx *Ctx) error
//	func (c *T) Like(ctx *Ctx, payload Payload) error
//
// Handlers returning a non-nil error get a server-side log marker
// in the "error" frame; the session continues (errors are surfaced
// to the user, not fatal to the connection).
//
// Panics in handlers are caught and surfaced as error frames; the
// session goroutine survives so one buggy handler doesn't dump the
// page for the user.
func (s *Session) dispatchEvent(ctx context.Context, name string, payload Payload) {
	defer func() {
		if r := recover(); r != nil {
			_ = s.send(ctx, Outbound{Type: "error", Msg: fmt.Sprintf("handler %q panic: %v", name, r)})
		}
	}()

	hctx := s.newCtx(ctx)

	if d, ok := s.component.(EventDispatcher); ok {
		if err := d.HandleEvent(hctx, name, payload); err != nil {
			_ = s.send(ctx, Outbound{Type: "error", Msg: err.Error()})
		}
		return
	}

	method := titleCaseEvent(name)
	cv := reflect.ValueOf(s.component)
	m := cv.MethodByName(method)
	if !m.IsValid() {
		_ = s.send(ctx, Outbound{Type: "error", Msg: fmt.Sprintf("no handler for event %q (looked for method %q)", name, method)})
		return
	}

	args, err := buildHandlerArgs(m, hctx, payload)
	if err != nil {
		_ = s.send(ctx, Outbound{Type: "error", Msg: err.Error()})
		return
	}
	out := m.Call(args)
	if len(out) > 0 {
		if e, ok := out[len(out)-1].Interface().(error); ok && e != nil {
			_ = s.send(ctx, Outbound{Type: "error", Msg: e.Error()})
		}
	}
}

// safeMount wraps Mount with panic recovery so a misbehaving
// constructor doesn't blow up the session goroutine before Run's
// main loop even starts. Returns the original error or a synthetic
// one carrying the panic value.
func (s *Session) safeMount(ctx *Ctx) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return s.component.Mount(ctx)
}

func buildHandlerArgs(m reflect.Value, ctx *Ctx, payload Payload) ([]reflect.Value, error) {
	t := m.Type()
	switch t.NumIn() {
	case 1:
		// (ctx)
		return []reflect.Value{reflect.ValueOf(ctx)}, nil
	case 2:
		// (ctx, payload)
		return []reflect.Value{reflect.ValueOf(ctx), reflect.ValueOf(payload)}, nil
	}
	return nil, fmt.Errorf("handler must take (ctx) or (ctx, payload); got %d args", t.NumIn())
}

// titleCaseEvent converts a wire event name to a Go-conventional
// method name. Handles two casing conventions clients might send:
//
//	"like"          → "Like"
//	"add-comment"   → "AddComment"   (kebab-case)
//	"addComment"    → "AddComment"   (camelCase)
//
// The exact-match path is intentional: if a method named
// "addComment" exists on the component (unusual in Go), the client
// could call it directly via the bare name — but we don't recommend
// this and don't test it.
func titleCaseEvent(name string) string {
	if name == "" {
		return ""
	}
	out := make([]rune, 0, len(name))
	upNext := true
	for _, r := range name {
		if r == '-' || r == '_' {
			upNext = true
			continue
		}
		if upNext {
			out = append(out, unicode.ToUpper(r))
			upNext = false
		} else {
			out = append(out, r)
		}
	}
	return string(out)
}

// render re-evaluates the component's fragment against its current
// state. Helpers from the engine flow through; the engine itself
// is the component resolver, so <Foo /> tags expand against the
// same registry the session was started against.
//
// Panic recovery keeps a buggy expression (nil-deref, out-of-range
// index) from killing the session goroutine. The recovered frame
// is empty (no statics, no dynamics), which DiffRendered handles
// as a no-op against prev — the client keeps its last good tree
// while the user sees an error frame describing what blew up.
func (s *Session) render() (out Rendered) {
	defer func() {
		if r := recover(); r != nil {
			// Background ctx is fine — the error frame is
			// best-effort and shouldn't block on a slow client.
			_ = s.send(context.Background(), Outbound{Type: "error", Msg: fmt.Sprintf("render panic: %v", r)})
			out = Rendered{}
		}
	}()
	opts := []RenderOption{WithComponents(s.engine)}
	if s.engine.helpers != nil {
		opts = append(opts, WithHelpers(s.engine.helpers))
	}
	return Render(s.def.fragment, s.component, opts...)
}

// diffAndSend renders, compares against prev, ships a sparse diff
// if anything changed, and updates prev. Refresh fires first so a
// notifier- or event-triggered re-render sees fresh derived state.
// Skipping the send when diff is nil keeps the wire quiet during
// no-op renders (common when notifier-triggered re-renders touch
// state that doesn't affect the visible tree).
func (s *Session) diffAndSend(ctx context.Context) {
	s.refresh(ctx)
	next := s.render()
	if diff := DiffRendered(s.prev, next); diff != nil {
		_ = s.send(ctx, Outbound{Type: "diff", Diff: diff})
	}
	s.prev = next
}

// refresh invokes the optional Refresher hook on the component.
// Components without it short-circuit immediately. Errors are
// surfaced as "error" frames but the render proceeds with the
// component state as-is — failure to refresh shouldn't blank the
// page, and a partial state is usually more useful than nothing.
// Panics are caught for the same reason: the next render still
// runs with whatever state was set before the panic.
func (s *Session) refresh(ctx context.Context) {
	r, ok := s.component.(Refresher)
	if !ok {
		return
	}
	defer func() {
		if rec := recover(); rec != nil {
			_ = s.send(ctx, Outbound{Type: "error", Msg: fmt.Sprintf("refresh panic: %v", rec)})
		}
	}()
	if err := r.Refresh(s.newCtx(ctx)); err != nil {
		_ = s.send(ctx, Outbound{Type: "error", Msg: "refresh: " + err.Error()})
	}
}

// newCtx builds the per-handler Ctx. Push and Notify capture the
// session via closures; calling Notify after the handler returns
// is the supported pattern for async background work. User is
// captured once at session start (see serveWS); same value lands
// on every Ctx for the session's lifetime.
func (s *Session) newCtx(parent context.Context) *Ctx {
	return &Ctx{
		Context: parent,
		Params:  s.params,
		User:    s.user,
		Push: func(event string, payload any) {
			_ = s.send(parent, Outbound{Type: "push", Event: event, EventPayload: payload})
		},
		Notify: func() {
			select {
			case s.selfNotify <- struct{}{}:
			default:
				// already pending — coalesce
			}
		},
	}
}

// send serializes Outbound emission. Multiple concurrent senders
// (a handler explicitly Push'ing, the loop sending a diff, the
// notifier path firing) can otherwise race on the underlying socket.
func (s *Session) send(ctx context.Context, msg Outbound) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.tr.Send(ctx, msg)
}

// randID returns a short opaque session identifier. Not cryptographically
// secure on its own — the WS handshake is the authority for trust;
// this ID is just for log correlation and diff routing on the client.
func randID() string {
	// Lightweight: timestamp-millis xor'd with a per-process counter.
	// Avoids pulling in crypto/rand for what is essentially a
	// debug correlator.
	n := nextSessionSeq()
	return fmt.Sprintf("s%x", n)
}

var (
	sessionSeqMu sync.Mutex
	sessionSeq   uint64
)

func nextSessionSeq() uint64 {
	sessionSeqMu.Lock()
	defer sessionSeqMu.Unlock()
	sessionSeq++
	return sessionSeq
}
