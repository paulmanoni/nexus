package template

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
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

	// token is the session-resumption identifier. Assigned by
	// newSession and shipped to the client in the "joined" frame.
	// Empty when resumption is disabled (engine.parkTTL == 0).
	token string

	// resumed is set true when the session was constructed from a
	// parked entry — skips Mount on Run start to preserve the
	// per-tab state the parked component holds.
	resumed bool

	component Component
	prev      Rendered

	selfNotify chan struct{} // buffered(1); coalesces Notify calls

	// outQ is the bounded outgoing queue. Renders + event
	// dispatch + ping responses all enqueue here; the writer
	// goroutine drains and writes to the transport. Capacity is
	// engine.sendBuffer (default 64).
	//
	// done closes on shutdown so producers stop enqueuing and the
	// writer goroutine returns. closeOnce guards both against
	// double-close and against a writer error racing with a
	// shutdown initiated elsewhere.
	outQ      chan Outbound
	done      chan struct{}
	closeOnce sync.Once

	// staticIsland caches the first-render evaluation of
	// IslandPropsSlot{Static: true} so subsequent renders return
	// the cached value — the diff layer naturally treats it as
	// unchanged and never re-ships the (potentially large) props
	// blob. One cache per session; passed into every render call
	// via WithStaticIslandCache.
	staticIsland *staticIslandCache
}

// defaultSendBuffer is the queue depth used when engine.sendBuffer
// is unset or non-positive. Sized to absorb a few seconds of
// chatty diffs (e.g., a typing input) without blocking the render
// goroutine, while still being shallow enough that a truly stuck
// client triggers a close within ~100 ms even under heavy load.
const defaultSendBuffer = 64

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
		token:      newSessionToken(engine),
		engine:     engine,
		def:        def,
		params:     params,
		tr:         tr,
		component:  component,
		selfNotify:   make(chan struct{}, 1),
		outQ:         make(chan Outbound, engine.sendBufferOrDefault()),
		done:         make(chan struct{}),
		staticIsland: newStaticIslandCache(),
	}
	return s, nil
}

// newResumedSession reconstructs a session from a parked entry:
// the component and last-rendered tree are adopted, the resumed
// flag is set so Run skips Mount, and a fresh token is issued so
// the client gets a rotated value (limits replay if a stale token
// leaks). Caller obtains parked via Engine.claimParked.
func newResumedSession(engine *Engine, def *componentDef, params Params, tr Transport, parked *parkedSession) *Session {
	return &Session{
		id:         randID(),
		token:      newSessionToken(engine),
		resumed:    true,
		engine:     engine,
		def:        def,
		params:     params,
		tr:         tr,
		user:       parked.user,
		component:  parked.component,
		prev:       parked.prev,
		selfNotify:   make(chan struct{}, 1),
		outQ:         make(chan Outbound, engine.sendBufferOrDefault()),
		done:         make(chan struct{}),
		staticIsland: newStaticIslandCache(),
	}
}

// newSessionToken returns an opaque resumption token, or "" when
// session resumption is disabled on the engine. 64 bits of
// crypto/rand are enough — the token rotates on every join and
// pairs with origin/IP checks for actual auth.
func newSessionToken(e *Engine) string {
	if e.parkTTL <= 0 {
		return ""
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%x", b)
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
	// Park the component on exit so a quick reconnect with the
	// same token can resume. No-op when resumption is disabled or
	// no token was assigned.
	defer s.parkOnExit()
	// Always signal shutdown on exit so the writer goroutine
	// returns and any in-flight callers of send() short-circuit.
	defer s.shutdown("run exit")

	// Start the writer that drains outQ into the transport. One
	// per session; lives until shutdown.
	go s.writerLoop()

	// Skip Mount on a resumed session — the parked component
	// already has the user's state. Mount would clobber it.
	if !s.resumed {
		mountCtx := s.newCtx(ctx)
		if err := s.safeMount(mountCtx); err != nil {
			_ = s.send(ctx, Outbound{Type: "error", Msg: "mount: " + err.Error()})
			return fmt.Errorf("mount: %w", err)
		}
	}

	// Initial render — establishes baseline statics on the client.
	// Refresh runs first so view-derived state (computed in Refresh)
	// is fresh before the very first frame.
	s.refresh(ctx)
	s.prev = s.render()
	if err := s.send(ctx, Outbound{Type: "joined", Rendered: &s.prev, Token: s.token}); err != nil {
		return fmt.Errorf("send joined: %w", err)
	}

	// External-mutation subscription. The notifier is shared across
	// sessions; coalescing is its job. Skip if no notifier configured.
	//
	// Two paths: a Topicer component subscribes per-topic so it only
	// wakes on relevant changes; everything else falls back to the
	// broadcast subscription. Multiple topic channels are merged into
	// a single changeCh via a fan-in goroutine — the main loop's
	// select stays compact regardless of topic count.
	var changeCh <-chan struct{}
	var cancelChange func()
	if s.engine.notifier != nil {
		if t, ok := s.component.(Topicer); ok {
			topics := t.Topics(s.newCtx(ctx))
			changeCh, cancelChange = s.subscribeTopics(ctx, topics)
		} else {
			changeCh, cancelChange = s.engine.notifier.Subscribe()
		}
		defer cancelChange()
	}

	// recvCh carries one Inbound per Recv call. We can't read from
	// Recv inside the select because Recv blocks; instead a single
	// goroutine pumps it. Buffer is 1 because we always drain before
	// re-reading.
	recvCh := make(chan recvResult, 1)
	go s.pumpRecv(ctx, recvCh)

	// Idle-timeout channel: reset on every loop iteration that
	// observed activity. Nil channel when no timeout is configured
	// — select never picks a nil case, so the timeout path is a
	// no-op for engines without WithIdleTimeout.
	var idleC <-chan time.Time
	var idleTimer *time.Timer
	if s.engine.idleTimeout > 0 {
		idleTimer = time.NewTimer(s.engine.idleTimeout)
		idleC = idleTimer.C
		defer idleTimer.Stop()
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-idleC:
			// No client / notifier / self-notify activity for the
			// configured window. Exit cleanly; the deferred
			// untrack + transport close take it from here.
			_ = s.send(ctx, Outbound{Type: "error", Msg: "session idle timeout"})
			return nil

		case res := <-recvCh:
			if res.err != nil {
				if errors.Is(res.err, io.EOF) || errors.Is(res.err, context.Canceled) {
					return nil
				}
				return fmt.Errorf("recv: %w", res.err)
			}
			s.handleInbound(ctx, res.msg)
			resetIdle(idleTimer, s.engine.idleTimeout)
			// Refill the pump for the next inbound message.
			go s.pumpRecv(ctx, recvCh)

		case <-changeCh:
			s.diffAndSend(ctx)
			resetIdle(idleTimer, s.engine.idleTimeout)

		case <-s.selfNotify:
			s.diffAndSend(ctx)
			resetIdle(idleTimer, s.engine.idleTimeout)
		}
	}
}

// resetIdle drains and resets the timer when configured. Idle
// timer is nil when no timeout is set, in which case this is a
// no-op. The drain handles the corner case where Reset is called
// after the timer fired but before its tick was selected.
func resetIdle(t *time.Timer, d time.Duration) {
	if t == nil || d <= 0 {
		return
	}
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

// subscribeTopics turns the Topicer's slice into a single merged
// changeCh. Each per-topic subscription's nudges are forwarded
// into one fan-in channel; the returned cancel tears down every
// underlying subscription plus the fan-in goroutine.
//
// Empty topic list returns a nil channel — the select case for it
// is never selectable, effectively opting the session out of
// notifier wakes entirely (handler/self-notify still work). That's
// the documented Topicer escape hatch for client-only components.
func (s *Session) subscribeTopics(ctx context.Context, topics []string) (<-chan struct{}, func()) {
	if len(topics) == 0 {
		return nil, func() {}
	}
	merged := make(chan struct{}, 1)
	cancels := make([]func(), 0, len(topics))
	stopFan := make(chan struct{})
	for _, t := range topics {
		ch, cancel := s.engine.notifier.SubscribeTopic(t)
		cancels = append(cancels, cancel)
		go func() {
			for {
				select {
				case <-stopFan:
					return
				case _, ok := <-ch:
					if !ok {
						return
					}
					// Coalesce into merged with the same
					// non-blocking-send pattern the notifier
					// itself uses.
					select {
					case merged <- struct{}{}:
					default:
					}
				}
			}
		}()
	}
	cancel := func() {
		close(stopFan)
		for _, c := range cancels {
			c()
		}
	}
	_ = ctx // reserved for future per-topic cancellation semantics
	return merged, cancel
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
		s.engine.stats.eventsTotal.Add(1)
		if msg.Name == modelEventName {
			s.handleModelEvent(ctx, msg.Payload)
		} else {
			s.dispatchEvent(ctx, msg.Name, msg.Payload)
		}
		s.diffAndSend(ctx)
	case "navigate":
		s.handleNavigate(ctx, msg.Path, msg.Params)
	case "ping":
		_ = s.send(ctx, Outbound{Type: "pong"})
	case "join":
		// serveWS consumes the initial join before starting Run,
		// so a join arriving here is a client error — surface it
		// but don't disconnect; the session is already running.
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

// parkOnExit hands the session's component to the engine's
// parked-session pool so a fast reconnect with the same token
// resumes state. No-op when resumption is disabled or this
// session has no token (newSession only assigns one when the
// engine has parkTTL > 0).
func (s *Session) parkOnExit() {
	s.engine.parkSession(s.token, s)
}

// handleNavigate swaps the session's component to the one
// registered at path, re-mounts, and ships a fresh "joined"
// frame including the new Path so the client can update the
// address bar via history.pushState. State of the old component
// is discarded — for SPA-style "stay on the same page, just
// change a query param" use a patch-style flow instead (planned
// follow-up).
//
// Unknown path emits an error frame and leaves the current
// session untouched. The client logs the error; users see no
// visible change, which is the right default — silently
// "navigating to nothing" is worse than a no-op.
func (s *Session) handleNavigate(ctx context.Context, path string, params Params) {
	name := s.engine.lookupRoute(path)
	if name == "" {
		_ = s.send(ctx, Outbound{Type: "error", Msg: fmt.Sprintf("navigate: no live component at %q", path)})
		return
	}
	def, ok := s.engine.lookup(name)
	if !ok {
		_ = s.send(ctx, Outbound{Type: "error", Msg: fmt.Sprintf("navigate: component %q not registered", name)})
		return
	}
	component := def.factory()
	if component == nil {
		_ = s.send(ctx, Outbound{Type: "error", Msg: fmt.Sprintf("navigate: factory for %q returned nil", name)})
		return
	}

	// Swap the session's component bindings in place; the loop
	// continues with the new state.
	s.def = def
	s.component = component
	if params == nil {
		params = Params{}
	}
	s.params = params
	s.prev = Rendered{} // force the next diff to be a full re-render

	if err := s.safeMount(s.newCtx(ctx)); err != nil {
		_ = s.send(ctx, Outbound{Type: "error", Msg: "navigate mount: " + err.Error()})
		return
	}
	s.refresh(ctx)
	s.prev = s.render()

	// Ship style + scope so the client can swap the head's
	// <style> tag together with the body content. Skip when
	// the new component has no <style> block — clients with no
	// style tag yet will just create one.
	var styleBody, scope string
	if def.style != nil && def.style.Body != "" {
		if def.style.Scoped {
			styleBody = rewriteScopedCSS(def.style.Body, def.scopeID)
			scope = def.scopeID
		} else {
			styleBody = def.style.Body
		}
	}

	// Ship the component's <script> body (already wrapped if
	// scoped) and a Component label so the client can find +
	// replace the prior data-nl-script="<Name>" tag in the DOM.
	// Empty when the new component has no <script> block.
	var scriptBody string
	if def.script != nil && def.script.Body != "" {
		scriptBody = wrapScript(def.script, def.name)
	}
	_ = s.send(ctx, Outbound{
		Type:      "joined",
		Rendered:  &s.prev,
		Path:      path,
		Token:     s.token,
		Style:     styleBody,
		Scope:     scope,
		Script:    scriptBody,
		Component: def.name,
	})
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

// buildHandlerArgs matches the handler's reflect.Type to the
// arguments the engine can supply. Three patterns are honored:
//
//	func (c *T) Like(ctx *Ctx)                          // no args
//	func (c *T) Like(ctx *Ctx, p Payload)               // raw payload (v0)
//	func (c *T) Like(ctx *Ctx, id int)                  // positional (v1+, from data-nl-args)
//	func (c *T) Like(ctx *Ctx, id int, body string)     // many positional
//
// The positional path consumes payload["args"] — populated by
// the client from data-nl-args, which the lowering attaches to
// elements that use the @click="like(arg, arg)" call form.
// Each arg is reflect-converted to the handler's parameter type;
// JSON numbers become int / float / etc. via reflect.Convert,
// which handles the common cases without ceremony.
func buildHandlerArgs(m reflect.Value, ctx *Ctx, payload Payload) ([]reflect.Value, error) {
	t := m.Type()
	n := t.NumIn()
	if n == 0 {
		return nil, fmt.Errorf("handler must take at least (ctx)")
	}
	ctxV := reflect.ValueOf(ctx)
	if n == 1 {
		return []reflect.Value{ctxV}, nil
	}
	// Legacy single-payload form: (ctx, Payload). Preserved so
	// existing handlers continue to compile without churn.
	payloadType := reflect.TypeOf(Payload(nil))
	if n == 2 && t.In(1) == payloadType {
		return []reflect.Value{ctxV, reflect.ValueOf(payload)}, nil
	}
	// Positional args path. Anything beyond ctx maps to
	// payload.args by index.
	rawArgs, _ := payload["args"].([]any)
	if len(rawArgs) != n-1 {
		return nil, fmt.Errorf("handler expects %d positional args; got %d", n-1, len(rawArgs))
	}
	out := make([]reflect.Value, n)
	out[0] = ctxV
	for i := 0; i < n-1; i++ {
		target := t.In(i + 1)
		v, err := convertArg(rawArgs[i], target)
		if err != nil {
			return nil, fmt.Errorf("arg %d: %w", i, err)
		}
		out[i+1] = v
	}
	return out, nil
}

// convertArg coerces a value freshly decoded from JSON (so
// numeric inputs arrive as float64, strings as string, etc.) to
// the reflect.Type the handler expects. Falls back to
// reflect.Convert for the common numeric widening cases.
func convertArg(v any, target reflect.Type) (reflect.Value, error) {
	if v == nil {
		return reflect.Zero(target), nil
	}
	rv := reflect.ValueOf(v)
	if rv.Type() == target {
		return rv, nil
	}
	if rv.Type().ConvertibleTo(target) {
		return rv.Convert(target), nil
	}
	return reflect.Value{}, fmt.Errorf("cannot convert %T to %s", v, target)
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
	s.engine.stats.rendersTotal.Add(1)
	opts := []RenderOption{
		WithComponents(s.engine),
		WithStaticIslandCache(s.staticIsland),
	}
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
		s.engine.stats.diffsTotal.Add(1)
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
		Stream: func(name string) *StreamRef {
			return &StreamRef{
				name: name,
				send: func(o Outbound) { _ = s.send(parent, o) },
				// captured for symmetry with the other Ctx
				// helpers; current StreamRef ops don't read it
				// but a future timeout/cancel could.
				context: parent,
			}
		},
		PushIsland: func(island, event string, payload any) {
			// Routed as a push frame with a prefixed event
			// name; the client splits "island/<name>/<event>"
			// and forwards payload to the matching island's
			// channel listener. Re-uses the existing push
			// machinery — no new wire type, no new
			// per-session state on the server.
			_ = s.send(parent, Outbound{
				Type:         "push",
				Event:        "island/" + island + "/" + event,
				EventPayload: payload,
			})
		},
	}
}

// send enqueues a frame onto the bounded outgoing queue. The
// writer goroutine pops from the queue and writes to the
// transport in order; concurrent senders are serialized by the
// channel itself.
//
// On a full queue (slow consumer), shutdown is signalled — the
// writer exits, the transport closes, the Recv loop unblocks,
// and Run returns cleanly. The closed-session case returns an
// error so callers that care can stop pushing; most call sites
// don't because the next loop iteration sees the same shutdown.
//
// The ctx parameter is kept for API stability and may carry a
// deadline future implementations honor; today the bounded
// channel + shutdown signal supersede it.
func (s *Session) send(_ context.Context, msg Outbound) error {
	select {
	case <-s.done:
		return errSessionClosed
	default:
	}
	select {
	case <-s.done:
		return errSessionClosed
	case s.outQ <- msg:
		return nil
	default:
		// Backpressure: queue full. Slow consumer or stuck
		// client. Close so resources free; future sends short-
		// circuit on the done check above.
		s.engine.stats.diffsDropped.Add(1)
		s.shutdown("send backpressure: outgoing queue full")
		return errSendBackpressure
	}
}

// writerLoop drains outQ and writes to the transport. Exits on
// shutdown signal or transport error. Transport errors trigger
// shutdown so the Recv loop unblocks; the session is gone
// either way.
func (s *Session) writerLoop() {
	for {
		select {
		case <-s.done:
			return
		case msg := <-s.outQ:
			if err := s.tr.Send(context.Background(), msg); err != nil {
				s.shutdown("transport write: " + err.Error())
				return
			}
		}
	}
}

// shutdown signals session termination. Idempotent. Closes the
// done channel (so producers and the writer loop see it) and
// the underlying transport (so the Recv loop in Run errors out
// and returns cleanly).
func (s *Session) shutdown(reason string) {
	s.closeOnce.Do(func() {
		close(s.done)
		_ = s.tr.Close()
		// Reason is captured for future logging/metrics — keep
		// the parameter even though we don't currently emit it
		// (avoiding stdout noise from normal disconnects).
		_ = reason
	})
}

// Sentinel errors so callers can discriminate without string-
// matching. Most callers ignore them today (the next loop tick
// sees the same shutdown), but tests and future observability
// hooks may inspect them.
var (
	errSessionClosed    = errors.New("session: closed")
	errSendBackpressure = errors.New("session: send queue full")
)

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
