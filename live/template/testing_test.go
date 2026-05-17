package template_test

import (
	"testing"
	"time"

	"github.com/paulmanoni/nexus/live"
	"github.com/paulmanoni/nexus/live/template"
)

// External-package test ensures the test harness compiles and
// works from outside the template package — i.e., that we're
// not accidentally leaning on unexported symbols.

type counter struct {
	template.BaseComponent
	Count int
}

func (c *counter) Inc(_ *template.Ctx)                      { c.Count++ }
func (c *counter) AddBy(_ *template.Ctx, p template.Payload) { c.Count += p.Int("by") }

const counterSrc = `<template><span id="count">{{ Count }}</span></template>`

func TestRenderOnce_ProducesInitialHTML(t *testing.T) {
	e := template.New(template.WithNotifier(live.New()))
	if err := e.Register("Counter", []byte(counterSrc), func() template.Component {
		return &counter{}
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	got, err := e.RenderOnce("Counter", nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	want := `<span id="count">0</span>`
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

// hello is a tiny second component for the navigate test.
type hello struct{ template.BaseComponent }

const helloSrc = `<template><h1>hello</h1></template>`

func TestNavigate_SwapsComponentAndShipsPath(t *testing.T) {
	e := template.New(template.WithNotifier(live.New()))
	_ = e.Register("Counter", []byte(counterSrc), func() template.Component { return &counter{} })
	_ = e.Register("Hello", []byte(helloSrc), func() template.Component { return &hello{} })
	// Both components are registered at URL paths via the route
	// index — adapter would do this in real wiring; tests call
	// it directly so they don't have to spin up an HTTP server.
	e.RegisterRoute("/", "Counter")
	e.RegisterRoute("/hello", "Hello")

	tr, stop, err := e.NewTestSession("Counter", nil)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer stop()

	// Drain the initial "joined" frame.
	if joined, ok := tr.NextOut(2 * time.Second); !ok || joined.Type != "joined" {
		t.Fatalf("initial joined missing or wrong: ok=%v frame=%+v", ok, joined)
	}

	// Navigate to /hello.
	tr.Push(template.Inbound{Type: "navigate", Path: "/hello"})
	after, ok := tr.NextOut(2 * time.Second)
	if !ok {
		t.Fatal("no joined frame after navigate")
	}
	if after.Type != "joined" {
		t.Fatalf("after-nav frame type = %q want joined; frame=%+v", after.Type, after)
	}
	if after.Path != "/hello" {
		t.Errorf("after-nav Path = %q want %q", after.Path, "/hello")
	}
	if after.Rendered == nil || after.Rendered.HTML() != `<h1>hello</h1>` {
		t.Errorf("after-nav Rendered = %+v want <h1>hello</h1>", after.Rendered)
	}
}

func TestNavigate_UnknownPathErrors(t *testing.T) {
	e := template.New(template.WithNotifier(live.New()))
	_ = e.Register("Counter", []byte(counterSrc), func() template.Component { return &counter{} })
	e.RegisterRoute("/", "Counter")

	tr, stop, _ := e.NewTestSession("Counter", nil)
	defer stop()
	_, _ = tr.NextOut(2 * time.Second) // joined

	tr.Push(template.Inbound{Type: "navigate", Path: "/nowhere"})
	frame, ok := tr.NextOut(2 * time.Second)
	if !ok {
		t.Fatal("no response to unknown navigate")
	}
	if frame.Type != "error" {
		t.Errorf("got %+v want error frame", frame)
	}
}

// chat exercises the Stream API: an event handler enqueues
// stream ops that must reach the test transport as individual
// "stream-op" frames (no diff for the streamed children).
type chat struct{ template.BaseComponent }

func (c *chat) Post(ctx *template.Ctx, p template.Payload) {
	ctx.Stream("messages").Append("msg-"+p.String("id"), `<li id="msg-`+p.String("id")+`">hi</li>`)
}

const chatSrc = `<template><ul nl-stream="messages"></ul></template>`

func TestStream_AppendEmitsStreamOp(t *testing.T) {
	e := template.New(template.WithNotifier(live.New()))
	if err := e.Register("Chat", []byte(chatSrc), func() template.Component {
		return &chat{}
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	tr, stop, err := e.NewTestSession("Chat", nil)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer stop()
	if _, ok := tr.NextOut(2 * time.Second); !ok {
		t.Fatal("missing joined frame")
	}

	tr.Push(template.Inbound{Type: "event", Name: "post", Payload: template.Payload{"id": "1"}})

	// Drain frames until we see a stream-op. After the handler
	// runs, the session also calls diffAndSend — that may emit
	// a no-op (nothing changed in the rendered tree) which
	// produces zero frames, or it may not. The stream-op is
	// the one we care about; tolerate ordering.
	saw := false
	for i := 0; i < 3 && !saw; i++ {
		frame, ok := tr.NextOut(500 * time.Millisecond)
		if !ok {
			break
		}
		if frame.Type == "stream-op" {
			saw = true
			if frame.Stream != "messages" || frame.Op != "append" || frame.ID != "msg-1" {
				t.Errorf("stream-op fields = %+v", frame)
			}
		}
	}
	if !saw {
		t.Error("no stream-op frame after Append call")
	}
}

func TestNewTestSession_EventRoundTrip(t *testing.T) {
	e := template.New(template.WithNotifier(live.New()))
	if err := e.Register("Counter", []byte(counterSrc), func() template.Component {
		return &counter{}
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	tr, stop, err := e.NewTestSession("Counter", nil)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer stop()

	// First frame should be "joined" with the initial Rendered.
	joined, ok := tr.NextOut(2 * time.Second)
	if !ok {
		t.Fatal("no joined frame")
	}
	if joined.Type != "joined" {
		t.Fatalf("first frame type = %q want joined; payload=%+v", joined.Type, joined)
	}

	// Fire an event; expect a diff frame with the new count.
	tr.Push(template.Inbound{Type: "event", Name: "inc"})
	diff, ok := tr.NextOut(2 * time.Second)
	if !ok {
		t.Fatal("no diff after inc")
	}
	if diff.Type != "diff" {
		t.Errorf("got %+v want diff", diff)
	}
}
