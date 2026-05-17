package template

import (
	"context"
	"fmt"
	"io"
	"time"
)

// TestTransport is a channel-backed Transport for driving a
// session from a test without a real WebSocket. Tests push
// Inbound frames via Send and pop server Outbound frames via
// NextOut.
//
// Designed for the in-process test loop: buffered channels
// large enough to absorb a few frames without coordination,
// idempotent Close, deterministic Recv that picks up done /
// ctx cancellation so the session goroutine exits cleanly.
type TestTransport struct {
	in   chan Inbound
	out  chan Outbound
	done chan struct{}
}

// NewTestTransport builds a fresh transport. Capacity 8 inbound
// / 32 outbound covers nearly every test pattern without
// forcing the caller to think about backpressure.
func NewTestTransport() *TestTransport {
	return &TestTransport{
		in:   make(chan Inbound, 8),
		out:  make(chan Outbound, 32),
		done: make(chan struct{}),
	}
}

// Recv satisfies Transport: blocks until an inbound message is
// available, ctx is cancelled, or the transport is closed.
func (t *TestTransport) Recv(ctx context.Context) (Inbound, error) {
	select {
	case m := <-t.in:
		return m, nil
	case <-t.done:
		return Inbound{}, io.EOF
	case <-ctx.Done():
		return Inbound{}, ctx.Err()
	}
}

// Send satisfies Transport: drops the outbound onto the test-
// accessible queue. Blocks only if the test isn't draining; in
// practice the buffer is generous.
func (t *TestTransport) Send(ctx context.Context, msg Outbound) error {
	select {
	case t.out <- msg:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close releases the transport. Idempotent so tests' defer
// patterns don't double-close.
func (t *TestTransport) Close() error {
	select {
	case <-t.done:
	default:
		close(t.done)
	}
	return nil
}

// Push injects an Inbound from the test side — the session
// goroutine sees it via Recv. Convenience wrapper so test code
// reads as "transport.Push(...)" instead of "transport.In <- ...".
func (t *TestTransport) Push(msg Inbound) {
	t.in <- msg
}

// NextOut returns the next Outbound the session emitted, or
// fails after timeout. The two-second default absorbs scheduler
// jitter on busy CI without making truly stuck tests slow to
// detect.
func (t *TestTransport) NextOut(timeout time.Duration) (Outbound, bool) {
	if timeout == 0 {
		timeout = 2 * time.Second
	}
	select {
	case m := <-t.out:
		return m, true
	case <-time.After(timeout):
		return Outbound{}, false
	}
}

// NewTestSession creates and runs a session against a fresh
// TestTransport for the named registered component. Returns
// the transport for driving I/O and a cancel func that stops
// the session goroutine; both work even after the session has
// exited so test cleanup doesn't have to guard ordering.
//
// The session's Run starts in its own goroutine — the caller's
// first NextOut returns the "joined" frame (with full Rendered
// tree). After that, drive events via Push and observe diffs
// via NextOut.
//
// Tests should NOT push a "join" frame themselves. The
// production serveWS consumes the join out-of-band before Run
// starts; bypassing serveWS here means Run never wants a join
// (handleInbound would treat one as a duplicate).
func (e *Engine) NewTestSession(componentName string, params Params) (*TestTransport, func(), error) {
	def, ok := e.lookup(componentName)
	if !ok {
		return nil, nil, fmt.Errorf("NewTestSession: component %q not registered", componentName)
	}
	tr := NewTestTransport()
	sess, err := newSession(e, def, params, tr)
	if err != nil {
		return nil, nil, fmt.Errorf("NewTestSession: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_ = sess.Run(ctx)
	}()

	stop := func() {
		cancel()
		_ = tr.Close()
	}
	return tr, stop, nil
}

// RenderOnce is the simplest test entry point: SSR the named
// component once with the given params, return the resulting
// HTML body (without the shell). For tests that only care about
// the initial output and not the live channel.
//
// The component is constructed via the registered factory;
// Mount and (optional) Refresh run before render. Errors from
// Mount surface here so the test sees them; Refresh errors are
// silenced — they mirror the server's "render with what we
// have" behavior, which the test usually wants too.
func (e *Engine) RenderOnce(componentName string, params Params) (string, error) {
	def, ok := e.lookup(componentName)
	if !ok {
		return "", fmt.Errorf("RenderOnce: component %q not registered", componentName)
	}
	component := def.factory()
	if component == nil {
		return "", fmt.Errorf("RenderOnce: factory for %q returned nil", componentName)
	}
	ctx := &Ctx{
		Context: context.Background(),
		Params:  params,
		Push:    func(string, any) {},
		Notify:  func() {},
	}
	if err := component.Mount(ctx); err != nil {
		return "", fmt.Errorf("RenderOnce: mount: %w", err)
	}
	if r, ok := component.(Refresher); ok {
		_ = r.Refresh(ctx)
	}
	opts := []RenderOption{WithComponents(e)}
	if e.helpers != nil {
		opts = append(opts, WithHelpers(e.helpers))
	}
	return Render(def.fragment, component, opts...).HTML(), nil
}
