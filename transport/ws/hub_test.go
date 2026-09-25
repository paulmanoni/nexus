package ws

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/paulmanoni/nexus/httpx"
	"github.com/paulmanoni/nexus/httpx/stdrouter"
)

func newHubServer(hub *Hub) *httptest.Server {
	e := stdrouter.New()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	e.GET("/ws", func(c *httpx.Ctx) { hub.serve(c, upgrader) })
	return httptest.NewServer(e)
}

func wsURL(srv *httptest.Server) string {
	return strings.Replace(srv.URL, "http", "ws", 1) + "/ws"
}

func readEvent(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v data=%q", err, string(data))
	}
	return m
}

func TestHub_ConnectAndBroadcast(t *testing.T) {
	hub := NewHub(WithWorkers(2))
	hub.Start(context.Background())
	defer hub.Stop()

	srv := newHubServer(hub)
	defer srv.Close()
	u := wsURL(srv)

	c1, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("dial1: %v", err)
	}
	defer c1.Close()
	c2, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("dial2: %v", err)
	}
	defer c2.Close()

	if e := readEvent(t, c1); e["type"] != EventTypeConnected {
		t.Fatalf("c1 first = %v, want connected", e["type"])
	}
	if e := readEvent(t, c2); e["type"] != EventTypeConnected {
		t.Fatalf("c2 first = %v, want connected", e["type"])
	}

	deadline := time.Now().Add(time.Second)
	for hub.ConnectionCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := hub.ConnectionCount(); got != 2 {
		t.Fatalf("ConnectionCount = %d, want 2", got)
	}

	hub.EmitBroadcast("ping", map[string]any{"n": 1})
	if e := readEvent(t, c1); e["type"] != "ping" {
		t.Fatalf("c1 = %v", e["type"])
	}
	if e := readEvent(t, c2); e["type"] != "ping" {
		t.Fatalf("c2 = %v", e["type"])
	}
}

func TestHub_RoomTargeting(t *testing.T) {
	hub := NewHub(WithWorkers(2))
	hub.AllowClientRooms(func(_ *Connection, room string) bool { return room == "alpha" })
	hub.Start(context.Background())
	defer hub.Stop()

	srv := newHubServer(hub)
	defer srv.Close()
	u := wsURL(srv)

	c1, _, _ := websocket.DefaultDialer.Dial(u, nil)
	defer c1.Close()
	c2, _, _ := websocket.DefaultDialer.Dial(u, nil)
	defer c2.Close()
	readEvent(t, c1)
	readEvent(t, c2)

	if err := c1.WriteJSON(map[string]any{"type": "subscribe", "room": "alpha"}); err != nil {
		t.Fatal(err)
	}
	if ev := readEvent(t, c1); ev["type"] != EventTypeSubscribed {
		t.Fatalf("ack = %v", ev["type"])
	}
	// The guard refuses any other room.
	if err := c2.WriteJSON(map[string]any{"type": "subscribe", "room": "beta"}); err != nil {
		t.Fatal(err)
	}
	if ev := readEvent(t, c2); ev["type"] != EventTypeError {
		t.Fatalf("refused subscribe = %v, want error", ev["type"])
	}

	deadline := time.Now().Add(time.Second)
	for hub.RoomConnectionCount("alpha") < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	hub.EmitToRoom("hello", map[string]any{"k": "v"}, "alpha")
	hub.EmitToRoom("hello", map[string]any{"k": "v"}, "beta")
	if ev := readEvent(t, c1); ev["type"] != "hello" {
		t.Fatalf("c1 = %v want hello", ev["type"])
	}
	_ = c2.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	if _, _, err := c2.ReadMessage(); err == nil {
		t.Fatalf("c2 unexpectedly received a message")
	}
}

// Without a guard, a client cannot join any room: rooms are audiences the
// server addresses, and a client choosing its own would receive messages
// meant for others.
func TestHub_ClientSubscribeRefusedByDefault(t *testing.T) {
	hub := NewHub(WithWorkers(2))
	hub.Start(context.Background())
	defer hub.Stop()
	srv := newHubServer(hub)
	defer srv.Close()

	c, _, err := websocket.DefaultDialer.Dial(wsURL(srv), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	readEvent(t, c)
	if err := c.WriteJSON(map[string]any{"type": "subscribe", "room": "chat:5"}); err != nil {
		t.Fatal(err)
	}
	if ev := readEvent(t, c); ev["type"] != EventTypeError {
		t.Fatalf("subscribe without a guard = %v, want error", ev["type"])
	}
	if n := hub.RoomConnectionCount("chat:5"); n != 0 {
		t.Fatalf("joined a room the server never granted: %d", n)
	}
}

// A socket's user comes from the identify hook (the upgrade request). The
// built-in authenticate message reports it and cannot change it: a client
// naming someone else's id must not receive their EmitToUser events.
func TestHub_AuthenticateCannotClaimAnotherUser(t *testing.T) {
	hub := NewHub(WithWorkers(2))
	hub.OnIdentify(func(c *httpx.Ctx) (string, map[string]any) { return c.Request.Header.Get("X-Test-User"), nil })
	hub.Start(context.Background())
	defer hub.Stop()
	srv := newHubServer(hub)
	defer srv.Close()
	u := wsURL(srv)

	alice, _, err := websocket.DefaultDialer.Dial(u, http.Header{"X-Test-User": {"alice"}})
	if err != nil {
		t.Fatal(err)
	}
	defer alice.Close()
	mallory, _, err := websocket.DefaultDialer.Dial(u, http.Header{"X-Test-User": {"mallory"}})
	if err != nil {
		t.Fatal(err)
	}
	defer mallory.Close()
	anon, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer anon.Close()
	readEvent(t, alice)
	readEvent(t, mallory)
	readEvent(t, anon)

	if err := mallory.WriteJSON(map[string]any{"type": "authenticate", "userId": "alice"}); err != nil {
		t.Fatal(err)
	}
	if ev := readEvent(t, mallory); ev["type"] != EventTypeAuthed || ev["data"].(map[string]any)["userId"] != "mallory" {
		t.Fatalf("authenticate reply = %v, want mallory's own identity", ev)
	}
	if err := anon.WriteJSON(map[string]any{"type": "authenticate", "userId": "alice"}); err != nil {
		t.Fatal(err)
	}
	if ev := readEvent(t, anon); ev["type"] != EventTypeError {
		t.Fatalf("anonymous authenticate = %v, want error", ev["type"])
	}

	deadline := time.Now().Add(time.Second)
	for hub.UserConnectionCount("alice") < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := hub.UserConnectionCount("alice"); got != 1 {
		t.Fatalf("alice connections = %d, want 1 (only her own socket)", got)
	}

	hub.EmitToUsers("private", map[string]any{"secret": true}, "alice")
	if ev := readEvent(t, alice); ev["type"] != "private" {
		t.Fatalf("alice = %v", ev["type"])
	}
	for name, c := range map[string]*websocket.Conn{"mallory": mallory, "anon": anon} {
		_ = c.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
		if _, _, err := c.ReadMessage(); err == nil {
			t.Errorf("%s received alice's event", name)
		}
	}
}

// TestHub_SlowClientIsClosed directly fills a Connection's send buffer past
// maxDropsBeforeClose rather than relying on OS TCP backpressure (which
// swallows hundreds of KB before the buffer shows as full).
func TestHub_SlowClientIsClosed(t *testing.T) {
	hub := NewHub(WithWorkers(1))
	hub.cfg.maxDropsBeforeClose = 3
	hub.Start(context.Background())
	defer hub.Stop()

	// Fabricate a Connection with a tiny pre-full send buffer and register it
	// on the hub without going through the WS upgrade path.
	conn := &Connection{
		send:     make(chan []byte, 1),
		done:     make(chan struct{}),
		ClientID: "fake",
		Metadata: map[string]any{},
		hub:      hub,
	}
	conn.send <- []byte("occupy") // buffer now full

	hub.register <- conn
	deadline := time.Now().Add(time.Second)
	for hub.ConnectionCount() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := hub.ConnectionCount(); got != 1 {
		t.Fatalf("conn not registered, count=%d", got)
	}

	var evicted atomic.Bool
	// Every Send call now hits the `default` branch since the buffer is full
	// and nobody is draining it.
	for i := 0; i < 20 && !evicted.Load(); i++ {
		conn.Send([]byte("spam"))
		time.Sleep(5 * time.Millisecond)
		if hub.ConnectionCount() == 0 {
			evicted.Store(true)
		}
	}
	if !evicted.Load() {
		t.Fatalf("slow client not evicted (drops=%d count=%d)", conn.drops.Load(), hub.ConnectionCount())
	}
}
