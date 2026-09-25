package nexus

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/paulmanoni/nexus/di"
	"github.com/paulmanoni/nexus/httpx"
	"github.com/paulmanoni/nexus/middleware"
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
	// typingCount is written from the hub's readPump goroutine and read by the
	// test goroutine, so it must be atomic (the reads are only loosely ordered
	// by a sleep below).
	var typingCount atomic.Int32
	typingHandler := func(sess *WSSession, p Params[chatPayload]) error {
		typingCount.Add(1)
		return nil
	}

	var app *App
	fxApp := newTestApp(t,
		fxBootOptions(Config{Server: ServerConfig{Addr: "127.0.0.1:0"}, TraceCapacity: 100}),
		AsWS("/events", "chat.send", sendHandler, Use(testUserMiddleware())).nexusOption(),
		AsWS("/events", "chat.typing", typingHandler).nexusOption(),
		di.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	// Our test server shares app as its handler so we get a real
	// listener address without racing the fx-managed :0 server.
	ts := httptest.NewServer(app)
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/events"
	c, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"X-Test-User": {"u42"}})
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

	// Now the echoed broadcast. The upgrade route's middleware
	// authenticated u42 (server side), the hub's identify hook attached it
	// to the connection, and the handler forwarded sess.UserID().
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
	if typingCount.Load() != 1 {
		t.Fatalf("typing handler count = %d, want 1", typingCount.Load())
	}

	// Unknown types silently pass through — client can send anything,
	// framework only dispatches registered types.
	mustSend("chat.unregistered", "ignored")
	time.Sleep(50 * time.Millisecond)
	if typingCount.Load() != 1 {
		t.Fatalf("unknown type leaked to typing handler (count=%d)", typingCount.Load())
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
	fxApp := newTestApp(t,
		fxBootOptions(Config{Server: ServerConfig{Addr: "127.0.0.1:0"}}),
		AsWS("/bad", "thing", badHandler).nexusOption(),
		di.Populate(&app),
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
	fxApp := newTestApp(t,
		fxBootOptions(Config{Server: ServerConfig{Addr: "127.0.0.1:0"}, TraceCapacity: 100}),
		AsWS("/events", "chat.send", okHandler).nexusOption(),
		di.Populate(&app),
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
		status                         int
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
	fxApp := newTestApp(t,
		fxBootOptions(Config{Server: ServerConfig{Addr: "127.0.0.1:0"}, TraceCapacity: 100}),
		AsWS("/bad", "thing", badHandler).nexusOption(),
		di.Populate(&app),
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

// testUserKey carries a test-authenticated user on the request context, the
// way an auth middleware would.
type testUserKey struct{}

func init() {
	RegisterRequestIdentity(func(ctx context.Context) (string, bool) {
		id, ok := ctx.Value(testUserKey{}).(string)
		return id, ok
	})
}

// testUserMiddleware authenticates the X-Test-User header: it stands in for
// a real auth middleware, putting the user on the request context.
func testUserMiddleware() middleware.Middleware {
	return middleware.Middleware{
		Name: "test-user",
		Gin: func(c *httpx.Ctx) {
			if u := c.Request.Header.Get("X-Test-User"); u != "" {
				c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), testUserKey{}, u))
			}
			c.Next()
		},
	}
}

// A connection's user is only what the server authenticated: a ?userId=
// query and the built-in authenticate message are ignored, so EmitToUser
// reaches the real owner and nobody who merely claims the id. A client
// subscribe is refused unless ClientRooms allows the room.
func TestAsWS_IdentityAndRoomsAreServerSide(t *testing.T) {
	notify := func(sess *WSSession, p Params[chatPayload]) error {
		sess.EmitToUser("private", map[string]string{"text": p.Args.Text}, p.Args.Text)
		sess.EmitToRoom("room.msg", map[string]string{"text": "hello"}, "lobby")
		sess.EmitToRoom("room.msg", map[string]string{"text": "secret"}, "staff")
		return nil
	}
	var app *App
	fxApp := newTestApp(t,
		fxBootOptions(Config{Server: ServerConfig{Addr: "127.0.0.1:0"}}),
		AsWS("/live", "notify", notify, Use(testUserMiddleware()),
			ClientRooms(func(userID, room string) bool { return room == "lobby" && userID != "" })).nexusOption(),
		di.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()
	ts := httptest.NewServer(app)
	defer ts.Close()
	base := "ws" + strings.TrimPrefix(ts.URL, "http") + "/live"

	dial := func(query string, user string) *websocket.Conn {
		t.Helper()
		h := http.Header{}
		if user != "" {
			h.Set("X-Test-User", user)
		}
		c, _, err := websocket.DefaultDialer.Dial(base+query, h)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, _, err := c.ReadMessage(); err != nil {
			t.Fatalf("greeting: %v", err)
		}
		return c
	}
	// The hub's write pump may batch queued events into one frame,
	// newline-separated; pending holds the rest of a frame per connection.
	pending := map[*websocket.Conn][][]byte{}
	next := func(c *websocket.Conn, wait time.Duration) (map[string]any, bool) {
		if len(pending[c]) == 0 {
			c.SetReadDeadline(time.Now().Add(wait))
			_, raw, err := c.ReadMessage()
			if err != nil {
				return nil, false
			}
			for _, line := range bytes.Split(raw, []byte("\n")) {
				if len(bytes.TrimSpace(line)) > 0 {
					pending[c] = append(pending[c], line)
				}
			}
			if len(pending[c]) == 0 {
				return nil, false
			}
		}
		line := pending[c][0]
		pending[c] = pending[c][1:]
		var m map[string]any
		_ = json.Unmarshal(line, &m)
		return m, true
	}
	send := func(c *websocket.Conn, v any) {
		t.Helper()
		if err := c.WriteJSON(v); err != nil {
			t.Fatal(err)
		}
	}

	alice := dial("", "alice")
	defer alice.Close()
	spoof := dial("?userId=alice", "")
	defer spoof.Close()
	claim := dial("", "mallory")
	defer claim.Close()

	send(claim, map[string]any{"type": "authenticate", "userId": "alice"})
	if m, _ := next(claim, 2*time.Second); m["type"] != "authenticated" || m["data"].(map[string]any)["userId"] != "mallory" {
		t.Fatalf("authenticate reply = %v, want mallory's own identity", m)
	}
	send(alice, map[string]any{"type": "subscribe", "room": "lobby"})
	if m, _ := next(alice, 2*time.Second); m["type"] != "subscribed" {
		t.Fatalf("allowed subscribe = %v", m)
	}
	send(claim, map[string]any{"type": "subscribe", "room": "staff"})
	if m, _ := next(claim, 2*time.Second); m["type"] != "error" {
		t.Fatalf("refused subscribe = %v, want error", m)
	}
	send(spoof, map[string]any{"type": "subscribe", "room": "lobby"})
	if m, _ := next(spoof, 2*time.Second); m["type"] != "error" {
		t.Fatalf("anonymous subscribe = %v, want error (guard needs a user)", m)
	}

	send(alice, map[string]any{"type": "notify", "data": map[string]string{"text": "alice"}})
	got := map[string]bool{}
	for {
		m, ok := next(alice, 500*time.Millisecond)
		if !ok {
			break
		}
		typ, _ := m["type"].(string)
		data, _ := m["data"].(map[string]any)
		text, _ := data["text"].(string)
		got[typ+":"+text] = true
	}
	if !got["private:alice"] || !got["room.msg:hello"] || got["room.msg:secret"] {
		t.Fatalf("alice received %v, want her private event and the lobby only", got)
	}
	for name, c := range map[string]*websocket.Conn{"?userId=alice": spoof, "authenticate as alice": claim} {
		for {
			m, ok := next(c, 300*time.Millisecond)
			if !ok {
				break
			}
			if m["type"] == "private" || m["type"] == "room.msg" {
				t.Errorf("%s received %v", name, m)
			}
		}
	}
}

// A handler on a built-in message type would never run: refused at boot.
func TestAsWS_RejectsBuiltinTypes(t *testing.T) {
	for _, typ := range []string{"ping", "authenticate", "subscribe", "unsubscribe"} {
		_, stop, err := InProcess(Config{}, AsWS("/x", typ, func() error { return nil }))
		if err == nil {
			stop(context.Background())
			t.Errorf("AsWS(%q) booted; want an error", typ)
		} else if !strings.Contains(err.Error(), "built-in") {
			t.Errorf("AsWS(%q): %v", typ, err)
		}
	}
}
