package template

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
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
	mountCtx := s.newCtx(ctx)
	if err := s.component.Mount(mountCtx); err != nil {
		_ = s.send(ctx, Outbound{Type: "error", Msg: "mount: " + err.Error()})
		return fmt.Errorf("mount: %w", err)
	}

	// Initial render — establishes baseline statics on the client.
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
// rejected (the engine assumes one session per Run call).
func (s *Session) handleInbound(ctx context.Context, msg Inbound) {
	switch msg.Type {
	case "event":
		s.dispatchEvent(ctx, msg.Name, msg.Payload)
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
func (s *Session) dispatchEvent(ctx context.Context, name string, payload Payload) {
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
// state. Helpers from the engine flow through; per-session helpers
// could be added later (currently no use case).
func (s *Session) render() Rendered {
	opts := []RenderOption{}
	if s.engine.helpers != nil {
		opts = append(opts, WithHelpers(s.engine.helpers))
	}
	return Render(s.def.fragment, s.component, opts...)
}

// diffAndSend renders, compares against prev, ships a sparse diff
// if anything changed, and updates prev. Skipping the send when
// diff is nil keeps the wire quiet during no-op renders (common
// when notifier-triggered re-renders touch state that doesn't
// affect the visible tree).
func (s *Session) diffAndSend(ctx context.Context) {
	next := s.render()
	if diff := DiffRendered(s.prev, next); diff != nil {
		_ = s.send(ctx, Outbound{Type: "diff", Diff: diff})
	}
	s.prev = next
}

// newCtx builds the per-handler Ctx. Push and Notify capture the
// session via closures; calling Notify after the handler returns
// is the supported pattern for async background work.
func (s *Session) newCtx(parent context.Context) *Ctx {
	return &Ctx{
		Context: parent,
		Params:  s.params,
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
