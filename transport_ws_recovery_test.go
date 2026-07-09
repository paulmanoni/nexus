package nexus

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/paulmanoni/nexus/di"
)

// TestWSHandlerPanicRecovered is the regression lock for the WebSocket recovery
// gap: a WS message handler runs in the connection's read-loop goroutine
// (ws.Hub.readPump), and an unrecovered panic in a goroutine crashes the WHOLE
// Go process. callWSHandler must catch it and route it through the same path a
// returned error takes — so the client gets an "error" envelope, the dashboard
// sees status 500, and the connection stays alive.
func TestWSHandlerPanicRecovered(t *testing.T) {
	// A realistic junior bug: assigning to a nil map panics. Before the
	// recovery wrapper this took down the server.
	panicHandler := func(sess *WSSession, p Params[chatPayload]) error {
		var m map[string]int
		m[p.Args.Text] = 1 // panic: assignment to entry in nil map
		return nil
	}
	okHandler := func(sess *WSSession, p Params[chatPayload]) error {
		sess.Emit("chat.echo", map[string]string{"text": p.Args.Text})
		return nil
	}

	var app *App
	fxApp := newTestApp(t,
		fxBootOptions(Config{Server: ServerConfig{Addr: "127.0.0.1:0"}, TraceCapacity: 100}),
		AsWS("/events", "boom", panicHandler).nexusOption(),
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

	_, ch, cancel := app.Bus().Subscribe(0, 64)
	defer cancel()

	// Fire the panic.
	data, _ := json.Marshal(map[string]any{"type": "boom", "data": map[string]string{"text": "x"}})
	if err := c.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("write boom: %v", err)
	}

	// 1) The client gets an error envelope, not a dropped connection.
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, raw, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("expected error envelope after panic, got read error: %v", err)
	}
	var env struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &env)
	if env.Type != "error" {
		t.Fatalf("want error envelope after panic, got %q (%s)", env.Type, raw)
	}

	// 2) request.end on the bus carries status 500, so the panic shows up in
	//    the dashboard's "failed traces" filter exactly like a returned error.
	deadline := time.After(2 * time.Second)
	sawEnd500 := false
	for !sawEnd500 {
		select {
		case <-deadline:
			t.Fatal("never saw request.end status=500 for the panicking WS frame")
		case e := <-ch:
			if e.Transport == "websocket" && e.Kind == "request.end" && e.Status == 500 {
				sawEnd500 = true
			}
		}
	}

	// 3) The read loop SURVIVED — the whole point. A follow-up message to a
	//    healthy handler still round-trips. If the panic had killed the
	//    readPump goroutine this would hang; if it had crashed the process the
	//    test binary would already be dead.
	data2, _ := json.Marshal(map[string]any{"type": "chat.send", "data": map[string]string{"text": "alive"}})
	if err := c.WriteMessage(websocket.TextMessage, data2); err != nil {
		t.Fatalf("write after panic: %v", err)
	}
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		_, raw, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("connection dead after panic — read: %v", err)
		}
		var echo struct {
			Type string `json:"type"`
			Data struct {
				Text string `json:"text"`
			} `json:"data"`
		}
		_ = json.Unmarshal(raw, &echo)
		if echo.Type == "chat.echo" {
			if echo.Data.Text != "alive" {
				t.Fatalf("echo after panic: got %q, want %q", echo.Data.Text, "alive")
			}
			return
		}
		// otherwise keep reading (ordering slack)
	}
}

// TestUserHandlerPanicsAreRecovered is the cross-context invariant guard.
//
// INVARIANT: every execution context that runs a user-supplied function
// recovers from panics, so one handler bug can never crash the process. The
// recover sites, one per context:
//
//	REST / GraphQL : recoveryMiddleware  (app_recovery.go — global; /graphql is an HTTP route)
//	WebSocket      : callWSHandler       (transport_ws.go)
//	Workers        : runWorker           (app_workers.go)
//	Crons          : cron dispatch       (extension/cron/cron.go)
//	Pubsub subs    : subscriber dispatch (extension/pubsub)
//
// Adding a new transport that calls a user function? Add its recover site AND a
// case here — this test is how the framework teaches the rule instead of a
// person having to.
func TestUserHandlerPanicsAreRecovered(t *testing.T) {
	t.Run("rest", func(t *testing.T) {
		boom := func(p Params[chatPayload]) (string, error) { panic("rest boom") }
		ok := func(p Params[chatPayload]) (string, error) { return "ok", nil }
		app, stop, err := InProcess(Config{},
			AsRest("GET", "/boom", boom),
			AsRest("GET", "/ok", ok),
		)
		if err != nil {
			t.Fatalf("InProcess: %v", err)
		}
		defer stop(context.Background())

		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, httptest.NewRequest("GET", "/boom", nil))
		if rec.Code != 500 {
			t.Fatalf("panicking REST handler: status %d, want 500", rec.Code)
		}
		// Process alive: a subsequent request to a healthy route still serves.
		rec2 := httptest.NewRecorder()
		app.ServeHTTP(rec2, httptest.NewRequest("GET", "/ok", nil))
		if rec2.Code != 200 {
			t.Fatalf("after panic, healthy route: status %d, want 200", rec2.Code)
		}
	})

	t.Run("ws", func(t *testing.T) {
		boom := func(sess *WSSession, p Params[chatPayload]) error { panic("ws boom") }
		var app *App
		fxApp := newTestApp(t,
			fxBootOptions(Config{Server: ServerConfig{Addr: "127.0.0.1:0"}}),
			AsWS("/ws", "boom", boom).nexusOption(),
			di.Populate(&app),
		)
		fxApp.RequireStart()
		defer fxApp.RequireStop()
		ts := httptest.NewServer(app)
		defer ts.Close()

		c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/ws", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _, _ = c.ReadMessage() // greeting

		data, _ := json.Marshal(map[string]any{"type": "boom", "data": map[string]string{"text": "x"}})
		_ = c.WriteMessage(websocket.TextMessage, data)
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, raw, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("WS panic escaped recovery (goroutine/conn dead): %v", err)
		}
		var env struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(raw, &env)
		if env.Type != "error" {
			t.Fatalf("want error envelope after WS panic, got %q", raw)
		}
	})
}
