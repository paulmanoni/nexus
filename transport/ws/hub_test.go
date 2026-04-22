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

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func init() { gin.SetMode(gin.TestMode) }

func newHubServer(hub *Hub) *httptest.Server {
	e := gin.New()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	e.GET("/ws", func(c *gin.Context) { hub.serve(c, upgrader) })
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

	deadline := time.Now().Add(time.Second)
	for hub.RoomConnectionCount("alpha") < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	hub.EmitToRoom("hello", map[string]any{"k": "v"}, "alpha")
	if ev := readEvent(t, c1); ev["type"] != "hello" {
		t.Fatalf("c1 = %v want hello", ev["type"])
	}
	_ = c2.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	if _, _, err := c2.ReadMessage(); err == nil {
		t.Fatalf("c2 unexpectedly received a message")
	}
}

func TestHub_AuthenticateRoutesToUser(t *testing.T) {
	hub := NewHub(WithWorkers(2))
	hub.Start(context.Background())
	defer hub.Stop()

	srv := newHubServer(hub)
	defer srv.Close()
	u := wsURL(srv)

	c1, _, _ := websocket.DefaultDialer.Dial(u, nil)
	defer c1.Close()
	readEvent(t, c1)

	if err := c1.WriteJSON(map[string]any{"type": "authenticate", "userId": "paul"}); err != nil {
		t.Fatal(err)
	}
	if ev := readEvent(t, c1); ev["type"] != EventTypeAuthed {
		t.Fatalf("auth ack = %v", ev["type"])
	}

	deadline := time.Now().Add(time.Second)
	for hub.UserConnectionCount("paul") < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := hub.UserConnectionCount("paul"); got != 1 {
		t.Fatalf("UserConnectionCount = %d", got)
	}

	hub.EmitToUsers("private", map[string]any{"secret": true}, "paul")
	if ev := readEvent(t, c1); ev["type"] != "private" {
		t.Fatalf("user-targeted = %v", ev["type"])
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
