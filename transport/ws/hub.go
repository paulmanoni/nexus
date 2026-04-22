package ws

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// --- Event -----------------------------------------------------------------

// Event is the envelope the hub sends to clients. Target decides recipients.
type Event struct {
	Type      string       `json:"type"`
	Data      any          `json:"data"`
	Timestamp int64        `json:"timestamp"`
	Target    *EventTarget `json:"-"`
}

// EventTarget specifies which connections receive an Event.
type EventTarget struct {
	Broadcast bool
	UserIDs   []string
	ClientIDs []string
	Rooms     []string
}

// NewEvent builds an Event with the current unix timestamp and a broadcast target.
func NewEvent(eventType string, data any) *Event {
	return &Event{
		Type:      eventType,
		Data:      data,
		Timestamp: time.Now().Unix(),
		Target:    &EventTarget{Broadcast: true},
	}
}

func (e *Event) ToUser(userIDs ...string) *Event {
	if e.Target == nil {
		e.Target = &EventTarget{}
	}
	e.Target.Broadcast = false
	e.Target.UserIDs = append(e.Target.UserIDs, userIDs...)
	return e
}

func (e *Event) ToClient(clientIDs ...string) *Event {
	if e.Target == nil {
		e.Target = &EventTarget{}
	}
	e.Target.Broadcast = false
	e.Target.ClientIDs = append(e.Target.ClientIDs, clientIDs...)
	return e
}

func (e *Event) ToRoom(rooms ...string) *Event {
	if e.Target == nil {
		e.Target = &EventTarget{}
	}
	e.Target.Broadcast = false
	e.Target.Rooms = append(e.Target.Rooms, rooms...)
	return e
}

func (e *Event) ToBroadcast() *Event {
	if e.Target == nil {
		e.Target = &EventTarget{}
	}
	e.Target.Broadcast = true
	return e
}

// Built-in event types. Keep the string values stable — clients depend on them.
const (
	EventTypeConnected    = "connection.established"
	EventTypeDisconnected = "connection.closed"
	EventTypeError        = "error"
	EventTypePong         = "pong"
	EventTypeSubscribed   = "subscribed"
	EventTypeUnsubscribed = "unsubscribed"
	EventTypeAuthed       = "authenticated"
)

// --- Connection ------------------------------------------------------------

// Connection is one authenticated WebSocket client.
type Connection struct {
	ws       *websocket.Conn
	send     chan []byte
	ClientID string
	UserID   string
	Metadata map[string]any
	hub      *Hub
	closed   atomic.Bool
	drops    atomic.Int32
}

// Send queues a raw message. If the send buffer is repeatedly full the hub
// closes the connection — slow clients do not hold up the fan-out loop.
func (c *Connection) Send(message []byte) {
	if c.closed.Load() {
		return
	}
	select {
	case c.send <- message:
		c.drops.Store(0)
	default:
		if c.drops.Add(1) >= int32(c.hub.cfg.maxDropsBeforeClose) {
			c.hub.cfg.logf("closing slow client clientID=%s drops=%d", c.ClientID, c.drops.Load())
			c.hub.unregister <- c
		}
	}
}

// SendEvent marshals and sends an Event to this single connection.
func (c *Connection) SendEvent(e *Event) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	c.Send(data)
	return nil
}

func (c *Connection) close() {
	if c.closed.CompareAndSwap(false, true) {
		close(c.send)
	}
}

// --- Hub -------------------------------------------------------------------

// Hub is a pub/sub engine for a pool of WebSocket connections with rooms,
// user-/client-targeted events, and a fan-out worker pool.
type Hub struct {
	cfg        hubConfig
	mu         sync.RWMutex
	conns      map[*Connection]bool
	byUser     map[string]map[*Connection]bool
	byRoom     map[string]map[*Connection]bool
	broadcast  chan []byte
	register   chan *Connection
	unregister chan *Connection
	events     chan *Event
	jobs       chan sendJob
	ctx        context.Context
	cancel     context.CancelFunc
	started    atomic.Bool

	// Hooks.
	identify   IdentifyFunc
	onConnect  OnConnectFunc
	onMessage  OnMessageFunc
	onClose    OnDisconnectFunc
}

type sendJob struct {
	conn *Connection
	data []byte
}

// Hooks the caller can install.
type (
	IdentifyFunc     func(c *gin.Context) (userID string, meta map[string]any)
	OnConnectFunc    func(conn *Connection)
	OnMessageFunc    func(conn *Connection, msgType int, data []byte) error
	OnDisconnectFunc func(conn *Connection)
)

// NewHub creates a hub with default settings. Call Start() (or pass it to a
// Builder that calls Start for you) before mounting endpoints on it.
func NewHub(opts ...HubOption) *Hub {
	cfg := defaultHubConfig()
	for _, o := range opts {
		o(&cfg)
	}
	h := &Hub{
		cfg:        cfg,
		conns:      map[*Connection]bool{},
		byUser:     map[string]map[*Connection]bool{},
		byRoom:     map[string]map[*Connection]bool{},
		broadcast:  make(chan []byte, cfg.broadcastBuffer),
		register:   make(chan *Connection, cfg.registerBuffer),
		unregister: make(chan *Connection, cfg.registerBuffer),
		events:     make(chan *Event, cfg.eventBuffer),
		jobs:       make(chan sendJob, cfg.eventBuffer*2),
	}
	return h
}

// Hooks — fluent setters.
func (h *Hub) OnIdentify(fn IdentifyFunc) *Hub         { h.identify = fn; return h }
func (h *Hub) OnConnect(fn OnConnectFunc) *Hub         { h.onConnect = fn; return h }
func (h *Hub) OnMessage(fn OnMessageFunc) *Hub         { h.onMessage = fn; return h }
func (h *Hub) OnDisconnect(fn OnDisconnectFunc) *Hub   { h.onClose = fn; return h }

// Start spins up the hub's main loop and the broadcast worker pool. Safe to
// call repeatedly — only the first call starts goroutines.
func (h *Hub) Start(ctx context.Context) {
	if !h.started.CompareAndSwap(false, true) {
		return
	}
	h.ctx, h.cancel = context.WithCancel(ctx)
	for i := 0; i < h.cfg.workers; i++ {
		go h.sendWorker()
	}
	go h.run()
	h.cfg.logf("websocket hub started (%d workers)", h.cfg.workers)
}

// Stop cancels the hub context, closes every connection, and returns. Idempotent.
func (h *Hub) Stop() {
	if h.cancel != nil {
		h.cancel()
	}
	h.mu.Lock()
	for c := range h.conns {
		c.close()
	}
	h.mu.Unlock()
}

// --- Fan-out ---------------------------------------------------------------

// Emit queues an event for fan-out. Dropped silently if the event channel is
// full — callers trade completeness for backpressure.
func (h *Hub) Emit(e *Event) {
	select {
	case h.events <- e:
	default:
		h.cfg.logf("event channel full, dropping type=%s", e.Type)
	}
}

func (h *Hub) EmitBroadcast(eventType string, data any) {
	h.Emit(NewEvent(eventType, data).ToBroadcast())
}

func (h *Hub) EmitToUsers(eventType string, data any, userIDs ...string) {
	h.Emit(NewEvent(eventType, data).ToUser(userIDs...))
}

func (h *Hub) EmitToClients(eventType string, data any, clientIDs ...string) {
	h.Emit(NewEvent(eventType, data).ToClient(clientIDs...))
}

func (h *Hub) EmitToRoom(eventType string, data any, room string) {
	h.Emit(NewEvent(eventType, data).ToRoom(room))
}

// --- Room membership -------------------------------------------------------

// Join subscribes a connection to a room. Idempotent.
func (h *Hub) Join(conn *Connection, room string) {
	h.mu.Lock()
	if h.byRoom[room] == nil {
		h.byRoom[room] = map[*Connection]bool{}
	}
	h.byRoom[room][conn] = true
	h.mu.Unlock()
	_ = conn.SendEvent(&Event{Type: EventTypeSubscribed, Data: map[string]any{"room": room}})
}

// Leave unsubscribes a connection from a room.
func (h *Hub) Leave(conn *Connection, room string) {
	h.mu.Lock()
	if set, ok := h.byRoom[room]; ok {
		delete(set, conn)
		if len(set) == 0 {
			delete(h.byRoom, room)
		}
	}
	h.mu.Unlock()
	_ = conn.SendEvent(&Event{Type: EventTypeUnsubscribed, Data: map[string]any{"room": room}})
}

// --- Stats -----------------------------------------------------------------

func (h *Hub) ConnectionCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.conns)
}

func (h *Hub) UserConnectionCount(userID string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.byUser[userID])
}

func (h *Hub) RoomConnectionCount(room string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.byRoom[room])
}

// Rooms returns a snapshot of room names and their member counts. Cheap for
// dashboards; do not call on the hot path.
func (h *Hub) Rooms() map[string]int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make(map[string]int, len(h.byRoom))
	for r, set := range h.byRoom {
		out[r] = len(set)
	}
	return out
}

// --- Internals -------------------------------------------------------------

func (h *Hub) run() {
	for {
		select {
		case <-h.ctx.Done():
			return
		case c := <-h.register:
			h.addConn(c)
		case c := <-h.unregister:
			h.removeConn(c)
		case msg := <-h.broadcast:
			h.fanoutRaw(msg)
		case e := <-h.events:
			h.fanoutEvent(e)
		}
	}
}

func (h *Hub) sendWorker() {
	for {
		select {
		case <-h.ctx.Done():
			return
		case job := <-h.jobs:
			h.writeTo(job.conn, job.data)
		}
	}
}

func (h *Hub) writeTo(c *Connection, data []byte) {
	if c.closed.Load() {
		return
	}
	select {
	case c.send <- data:
		c.drops.Store(0)
	default:
		if c.drops.Add(1) >= int32(h.cfg.maxDropsBeforeClose) {
			h.unregister <- c
		}
	}
}

func (h *Hub) addConn(c *Connection) {
	h.mu.Lock()
	h.conns[c] = true
	if c.UserID != "" {
		if h.byUser[c.UserID] == nil {
			h.byUser[c.UserID] = map[*Connection]bool{}
		}
		h.byUser[c.UserID][c] = true
	}
	h.mu.Unlock()
	_ = c.SendEvent(&Event{
		Type: EventTypeConnected,
		Data: map[string]any{"clientId": c.ClientID},
	})
	if h.onConnect != nil {
		h.onConnect(c)
	}
}

func (h *Hub) removeConn(c *Connection) {
	h.mu.Lock()
	if !h.conns[c] {
		h.mu.Unlock()
		return
	}
	delete(h.conns, c)
	if c.UserID != "" {
		if set, ok := h.byUser[c.UserID]; ok {
			delete(set, c)
			if len(set) == 0 {
				delete(h.byUser, c.UserID)
			}
		}
	}
	for room, set := range h.byRoom {
		if _, ok := set[c]; ok {
			delete(set, c)
			if len(set) == 0 {
				delete(h.byRoom, room)
			}
		}
	}
	h.mu.Unlock()
	c.close()
	if h.onClose != nil {
		h.onClose(c)
	}
}

func (h *Hub) fanoutRaw(msg []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.conns {
		select {
		case h.jobs <- sendJob{conn: c, data: msg}:
		default:
		}
	}
}

func (h *Hub) fanoutEvent(e *Event) {
	data, err := json.Marshal(e)
	if err != nil {
		h.cfg.logf("marshal event: %v", err)
		return
	}
	h.mu.RLock()
	if e.Target == nil || e.Target.Broadcast {
		for c := range h.conns {
			select {
			case h.jobs <- sendJob{conn: c, data: data}:
			default:
			}
		}
		h.mu.RUnlock()
		return
	}
	targets := map[*Connection]struct{}{}
	for _, uid := range e.Target.UserIDs {
		for c := range h.byUser[uid] {
			targets[c] = struct{}{}
		}
	}
	for _, room := range e.Target.Rooms {
		for c := range h.byRoom[room] {
			targets[c] = struct{}{}
		}
	}
	if len(e.Target.ClientIDs) > 0 {
		want := map[string]bool{}
		for _, id := range e.Target.ClientIDs {
			want[id] = true
		}
		for c := range h.conns {
			if want[c.ClientID] {
				targets[c] = struct{}{}
			}
		}
	}
	h.mu.RUnlock()
	for c := range targets {
		select {
		case h.jobs <- sendJob{conn: c, data: data}:
		default:
		}
	}
}

// --- Upgrade path ----------------------------------------------------------

// serve is what the Builder wires into Gin when WithHub is used. It upgrades
// the HTTP connection, runs the identify hook, registers the conn, and starts
// the read/write pumps.
func (h *Hub) serve(gctx *gin.Context, upgrader websocket.Upgrader) {
	if h.cfg.maxConnections > 0 && h.ConnectionCount() >= h.cfg.maxConnections {
		gctx.JSON(http.StatusServiceUnavailable, gin.H{"error": "websocket at capacity"})
		return
	}
	ws, err := upgrader.Upgrade(gctx.Writer, gctx.Request, nil)
	if err != nil {
		return
	}

	var userID string
	var meta map[string]any
	if h.identify != nil {
		userID, meta = h.identify(gctx)
	}
	if meta == nil {
		meta = map[string]any{}
	}

	conn := &Connection{
		ws:       ws,
		send:     make(chan []byte, h.cfg.sendBuffer),
		ClientID: uuid.New().String(),
		UserID:   userID,
		Metadata: meta,
		hub:      h,
	}

	h.register <- conn
	go h.writePump(conn)
	go h.readPump(conn)
}

func (h *Hub) readPump(c *Connection) {
	defer func() { h.unregister <- c }()
	c.ws.SetReadLimit(h.cfg.maxMessageSize)
	_ = c.ws.SetReadDeadline(time.Now().Add(h.cfg.pongWait))
	c.ws.SetPongHandler(func(string) error {
		return c.ws.SetReadDeadline(time.Now().Add(h.cfg.pongWait))
	})
	for {
		t, data, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		if h.handleBuiltin(c, data) {
			continue
		}
		if h.onMessage != nil {
			if err := h.onMessage(c, t, data); err != nil {
				return
			}
		}
	}
}

// handleBuiltin processes the default protocol: ping/pong, authenticate,
// subscribe, unsubscribe. Returns true if the message was consumed.
func (h *Hub) handleBuiltin(c *Connection, data []byte) bool {
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		return false
	}
	kind, _ := msg["type"].(string)
	switch kind {
	case "ping":
		_ = c.SendEvent(&Event{Type: EventTypePong, Data: map[string]any{"timestamp": time.Now().Unix()}})
		return true
	case "authenticate":
		if uid, ok := msg["userId"].(string); ok && uid != "" {
			h.mu.Lock()
			if c.UserID != "" {
				if set, ok := h.byUser[c.UserID]; ok {
					delete(set, c)
				}
			}
			c.UserID = uid
			if h.byUser[uid] == nil {
				h.byUser[uid] = map[*Connection]bool{}
			}
			h.byUser[uid][c] = true
			h.mu.Unlock()
			_ = c.SendEvent(&Event{Type: EventTypeAuthed, Data: map[string]any{"userId": uid, "status": "success"}})
		}
		return true
	case "subscribe":
		if room, ok := msg["room"].(string); ok {
			h.Join(c, room)
		}
		return true
	case "unsubscribe":
		if room, ok := msg["room"].(string); ok {
			h.Leave(c, room)
		}
		return true
	}
	return false
}

func (h *Hub) writePump(c *Connection) {
	ticker := time.NewTicker(h.cfg.pingPeriod)
	defer func() {
		ticker.Stop()
		_ = c.ws.Close()
	}()
	for {
		select {
		case msg, ok := <-c.send:
			_ = c.ws.SetWriteDeadline(time.Now().Add(h.cfg.writeWait))
			if !ok {
				_ = c.ws.WriteMessage(websocket.CloseMessage, nil)
				return
			}
			w, err := c.ws.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			_, _ = w.Write(msg)
			n := len(c.send)
			for i := 0; i < n; i++ {
				_, _ = w.Write([]byte{'\n'})
				_, _ = w.Write(<-c.send)
			}
			if err := w.Close(); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.ws.SetWriteDeadline(time.Now().Add(h.cfg.writeWait))
			if err := c.ws.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// --- Config ----------------------------------------------------------------

type hubConfig struct {
	workers             int
	eventBuffer         int
	broadcastBuffer     int
	registerBuffer      int
	sendBuffer          int
	maxMessageSize      int64
	writeWait           time.Duration
	pongWait            time.Duration
	pingPeriod          time.Duration
	maxConnections      int
	maxDropsBeforeClose int
	logf                func(format string, args ...any)
}

func defaultHubConfig() hubConfig {
	pong := 60 * time.Second
	return hubConfig{
		workers:             32,
		eventBuffer:         1024,
		broadcastBuffer:     1024,
		registerBuffer:      256,
		sendBuffer:          1024,
		maxMessageSize:      512 * 1024,
		writeWait:           10 * time.Second,
		pongWait:            pong,
		pingPeriod:          pong * 9 / 10,
		maxConnections:      5000,
		maxDropsBeforeClose: 10,
		logf:                func(f string, a ...any) { log.Printf("ws: "+f, a...) },
	}
}

// HubOption configures a Hub at construction.
type HubOption func(*hubConfig)

func WithWorkers(n int) HubOption { return func(c *hubConfig) { c.workers = n } }
func WithEventBuffer(n int) HubOption { return func(c *hubConfig) { c.eventBuffer = n } }
func WithMaxConnections(n int) HubOption {
	return func(c *hubConfig) { c.maxConnections = n }
}
func WithMaxMessageSize(n int64) HubOption {
	return func(c *hubConfig) { c.maxMessageSize = n }
}
func WithPingPong(pong, write time.Duration) HubOption {
	return func(c *hubConfig) {
		c.pongWait = pong
		c.pingPeriod = pong * 9 / 10
		c.writeWait = write
	}
}
func WithLogger(fn func(format string, args ...any)) HubOption {
	return func(c *hubConfig) { c.logf = fn }
}
