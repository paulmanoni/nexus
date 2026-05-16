package nexus

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"
)

// chatPayload is the test message body, deliberately un-exported and colocated
// with the test so we exercise an anonymous per-test type (what user code
// looks like in practice) through the reflective binder.
type chatPayload struct {
	Text string `json:"text"`
}

// TestAsWS_TypedDispatch drives the full AsWS path end-to-end: boot an app,
// register two message-type handlers on the same path, dial a websocket,
// send a typed envelope, and verify both the handler sees the typed args and
// the emit lands back on the wire.
func TestAsWS_TypedDispatch(t *testing.T) {
	received := make(chan string, 1)
	sendHandler := func(sess *WSSession, p Params[chatPayload]) error {
		received <- p.Args.Text
		sess.Emit("chat.echo", map[string]string{"text": p.Args.Text, "user": sess.UserID()})
		return nil
	}
	typingCount := 0
	typingHandler := func(sess *WSSession, p Params[chatPayload]) error {
		typingCount++
		return nil
	}

	var app *App
	fxApp := fxtest.New(t,
		fxBootOptions(Config{Server: ServerConfig{Addr: "127.0.0.1:0"}, TraceCapacity: 100}),
		AsWS("/events", "chat.send", sendHandler).nexusOption(),
		AsWS("/events", "chat.typing", typingHandler).nexusOption(),
		fx.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	// Our test server shares app as its handler so we get a real
	// listener address without racing the fx-managed :0 server.
	ts := httptest.NewServer(app)
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/events?userId=u42"
	c, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	// The hub emits a connection.established event immediately on upgrade
	// (see ws.Hub.addConn). Drain it before sending so the subsequent
	// ReadMessage loop sees the echo without race.
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := c.ReadMessage(); err != nil {
		t.Fatalf("read greeting: %v", err)
	}

	mustSend := func(msgType, textVal string) {
		msg := map[string]any{"type": msgType, "data": map[string]string{"text": textVal}}
		data, _ := json.Marshal(msg)
		if err := c.WriteMessage(websocket.TextMessage, data); err != nil {
			t.Fatalf("write %s: %v", msgType, err)
		}
	}

	mustSend("chat.send", "hi")

	select {
	case got := <-received:
		if got != "hi" {
			t.Fatalf("handler got %q, want %q", got, "hi")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("chat.send handler never fired")
	}

	// Now the echoed broadcast. With userId=u42 on the upgrade URL, the
	// hub's identify hook attached it to the connection — the handler
	// read sess.UserID() and forwarded it in the echo payload.
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, raw, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	var echo struct {
		Type string `json:"type"`
		Data struct {
			Text string `json:"text"`
			User string `json:"user"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &echo); err != nil {
		t.Fatalf("unmarshal echo: %v\npayload: %s", err, string(raw))
	}
	if echo.Type != "chat.echo" || echo.Data.Text != "hi" {
		t.Fatalf("echo mismatch: %+v", echo)
	}
	if echo.Data.User != "u42" {
		t.Fatalf("UserID not threaded through: %q", echo.Data.User)
	}

	// Verify dispatch is type-scoped: a chat.typing message routes to the
	// typing handler, not the send handler. We don't expect any echo for
	// this one.
	mustSend("chat.typing", "keystroke")
	time.Sleep(100 * time.Millisecond)
	if typingCount != 1 {
		t.Fatalf("typing handler count = %d, want 1", typingCount)
	}

	// Unknown types silently pass through — client can send anything,
	// framework only dispatches registered types.
	mustSend("chat.unregistered", "ignored")
	time.Sleep(50 * time.Millisecond)
	if typingCount != 1 {
		t.Fatalf("unknown type leaked to typing handler (count=%d)", typingCount)
	}
}

// TestAsWS_HandlerErrorSendsErrorEvent asserts that a handler returning a
// non-nil error is translated into an `error` envelope event on the same
// connection, and that the connection stays open.
func TestAsWS_HandlerErrorSendsErrorEvent(t *testing.T) {
	badHandler := func(sess *WSSession, p Params[chatPayload]) error {
		return testErr{"boom"}
	}

	var app *App
	fxApp := fxtest.New(t,
		fxBootOptions(Config{Server: ServerConfig{Addr: "127.0.0.1:0"}}),
		AsWS("/bad", "thing", badHandler).nexusOption(),
		fx.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	ts := httptest.NewServer(app)
	defer ts.Close()

	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/bad", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Drain the connection.established greeting.
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := c.ReadMessage(); err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(map[string]any{"type": "thing", "data": map[string]string{"text": "x"}})
	if err := c.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, raw, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read error envelope: %v", err)
	}
	var env struct {
		Type string `json:"type"`
		Data struct {
			Message string `json:"message"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env.Type != "error" || env.Data.Message != "boom" {
		t.Fatalf("unexpected error envelope: %+v", env)
	}
}

type testErr struct{ s string }

func (e testErr) Error() string { return e.s }

// TestAsWS_FrameEmitsRequestStartEnd asserts every inbound WS frame
// produces a matching pair of request.start + request.end events on
// the bus, with the same TraceID, transport=websocket, and the
// handler's status. This is what makes WS traffic show up on the
// dashboard's trace waterfall the same way REST does — without it,
// a developer looking at /__nexus/events sees only the request.op
// badge and can't drill into the handler's child spans.
func TestAsWS_FrameEmitsRequestStartEnd(t *testing.T) {
	okHandler := func(sess *WSSession, p Params[chatPayload]) error {
		return nil
	}

	var app *App
	fxApp := fxtest.New(t,
		fxBootOptions(Config{Server: ServerConfig{Addr: "127.0.0.1:0"}, TraceCapacity: 100}),
		AsWS("/events", "chat.send", okHandler).nexusOption(),
		fx.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	ts := httptest.NewServer(app)
	defer ts.Close()

	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/events", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := c.ReadMessage(); err != nil { // drain greeting
		t.Fatalf("greeting: %v", err)
	}

	// Subscribe to the bus BEFORE sending so we capture the start
	// event; the bus's backlog buffer is large but we want a
	// deterministic read.
	_, ch, cancel := app.Bus().Subscribe(0, 32)
	defer cancel()

	data, _ := json.Marshal(map[string]any{"type": "chat.send", "data": map[string]string{"text": "hi"}})
	if err := c.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("write: %v", err)
	}

	deadline := time.After(2 * time.Second)
	var startEv, endEv struct {
		traceID, kind, transport, name string
		status                          int
	}
	got := 0
	for got < 2 {
		select {
		case <-deadline:
			t.Fatalf("did not see both request.start + request.end (got %d events)", got)
		case e, ok := <-ch:
			if !ok {
				t.Fatal("bus channel closed")
			}
			if e.Transport != "websocket" {
				continue
			}
			switch e.Kind {
			case "request.start":
				startEv.traceID, startEv.kind, startEv.transport, startEv.name = e.TraceID, string(e.Kind), e.Transport, e.Name
				got++
			case "request.end":
				endEv.traceID, endEv.kind, endEv.transport, endEv.name, endEv.status = e.TraceID, string(e.Kind), e.Transport, e.Name, e.Status
				got++
			}
		}
	}

	if startEv.traceID == "" || startEv.traceID != endEv.traceID {
		t.Errorf("start/end TraceID mismatch: start=%q end=%q", startEv.traceID, endEv.traceID)
	}
	if endEv.status != 200 {
		t.Errorf("expected status 200 on success, got %d", endEv.status)
	}
}

// TestAsWS_HandlerErrorEmitsRequestEnd500 verifies the request.end
// from a failing handler carries status=500 + the error string —
// otherwise the dashboard's "show only failed traces" filter would
// miss WS errors.
func TestAsWS_HandlerErrorEmitsRequestEnd500(t *testing.T) {
	badHandler := func(sess *WSSession, p Params[chatPayload]) error {
		return testErr{"boom"}
	}

	var app *App
	fxApp := fxtest.New(t,
		fxBootOptions(Config{Server: ServerConfig{Addr: "127.0.0.1:0"}, TraceCapacity: 100}),
		AsWS("/bad", "thing", badHandler).nexusOption(),
		fx.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	ts := httptest.NewServer(app)
	defer ts.Close()
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/bad", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, _ = c.ReadMessage() // greeting

	_, ch, cancel := app.Bus().Subscribe(0, 32)
	defer cancel()

	data, _ := json.Marshal(map[string]any{"type": "thing", "data": map[string]string{"text": "x"}})
	_ = c.WriteMessage(websocket.TextMessage, data)

	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("never observed request.end with status 500")
		case e := <-ch:
			if e.Transport == "websocket" && e.Kind == "request.end" {
				if e.Status != 500 {
					t.Errorf("status: want 500, got %d", e.Status)
				}
				if e.Error != "boom" {
					t.Errorf("error: want %q, got %q", "boom", e.Error)
				}
				return
			}
		}
	}
}