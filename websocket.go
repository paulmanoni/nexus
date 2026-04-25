package nexus

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"

	"github.com/paulmanoni/nexus/middleware"
	"github.com/paulmanoni/nexus/transport/ws"
)

// WSSession is the per-connection handle injected into AsWS handlers. Every
// handler that declares `*nexus.WSSession` as a parameter receives the session
// tied to the connection that produced the current inbound message. Safe for
// concurrent use — wraps a ws.Hub under the hood.
//
// The framework uses the same envelope protocol as the built-in ws.Hub:
//
//	{ "type": "chat.send", "data": {...}, "timestamp": <unix> }
//
// Emit / EmitToUser / EmitToRoom / EmitToClient publish in that shape; SendRaw
// is the escape hatch for non-envelope payloads.
type WSSession struct {
	conn *ws.Connection
	hub  *ws.Hub
	ctx  context.Context
}

// Context returns a context cancelled when the connection disconnects. Safe to
// pass downstream — long-running work will unblock on hangup.
func (s *WSSession) Context() context.Context {
	if s == nil || s.ctx == nil {
		return context.Background()
	}
	return s.ctx
}

// ClientID is the UUID the hub minted for this connection at upgrade time.
func (s *WSSession) ClientID() string {
	if s == nil || s.conn == nil {
		return ""
	}
	return s.conn.ClientID
}

// UserID is the identity attached at upgrade (via `?userId=` query or a
// gin-context `user` interface) or later via the client-initiated
// `authenticate` protocol message. Empty when the connection is unauthed.
func (s *WSSession) UserID() string {
	if s == nil || s.conn == nil {
		return ""
	}
	return s.conn.UserID
}

// Metadata is the map the hub's identify hook populated at upgrade time. Read
// freely; mutation is not safe across goroutines.
func (s *WSSession) Metadata() map[string]any {
	if s == nil || s.conn == nil {
		return nil
	}
	return s.conn.Metadata
}

// SendRaw writes bytes directly to this connection with no envelope. Use when
// you're speaking a non-nexus protocol (e.g. pre-marshalled binary data).
func (s *WSSession) SendRaw(data []byte) {
	if s == nil || s.conn == nil {
		return
	}
	s.conn.Send(data)
}

// Send wraps data in an envelope and unicasts it to this connection.
func (s *WSSession) Send(eventType string, data any) error {
	if s == nil || s.conn == nil {
		return nil
	}
	return s.conn.SendEvent(ws.NewEvent(eventType, data).ToClient(s.conn.ClientID))
}

// Emit broadcasts an envelope to every connection on this endpoint.
func (s *WSSession) Emit(eventType string, data any) {
	if s == nil || s.hub == nil {
		return
	}
	s.hub.EmitBroadcast(eventType, data)
}

// EmitToUser sends an envelope to every connection authed as one of userIDs.
func (s *WSSession) EmitToUser(eventType string, data any, userIDs ...string) {
	if s == nil || s.hub == nil || len(userIDs) == 0 {
		return
	}
	s.hub.EmitToUsers(eventType, data, userIDs...)
}

// EmitToRoom sends an envelope to every connection subscribed to room.
func (s *WSSession) EmitToRoom(eventType string, data any, room string) {
	if s == nil || s.hub == nil || room == "" {
		return
	}
	s.hub.EmitToRoom(eventType, data, room)
}

// EmitToClient sends an envelope to the connections with the given IDs.
func (s *WSSession) EmitToClient(eventType string, data any, clientIDs ...string) {
	if s == nil || s.hub == nil || len(clientIDs) == 0 {
		return
	}
	s.hub.EmitToClients(eventType, data, clientIDs...)
}

// JoinRoom subscribes this connection to a room. Matching server-side helper
// for the client's `{"type":"subscribe","room":"..."}` message.
func (s *WSSession) JoinRoom(room string) {
	if s == nil || s.hub == nil || s.conn == nil || room == "" {
		return
	}
	s.hub.Join(s.conn, room)
}

// LeaveRoom unsubscribes this connection from a room.
func (s *WSSession) LeaveRoom(room string) {
	if s == nil || s.hub == nil || s.conn == nil || room == "" {
		return
	}
	s.hub.Leave(s.conn, room)
}

// wsEndpoint is the shared state for one AsWS path. Multiple AsWS calls
// targeting the same path populate different entries in `handlers` but share
// the same hub, middleware chain, and registry entry.
type wsEndpoint struct {
	path     string
	service  string
	hub      *ws.Hub
	mu       sync.RWMutex
	handlers map[string]wsTypedHandler
}

// wsTypedHandler is one registered message-type dispatch target.
type wsTypedHandler struct {
	shape    handlerShape
	deps     []reflect.Value
	depTypes []reflect.Type
	service  string
	opName   string
	bundles  []middleware.Middleware
}

// wsEnvelope is the inbound message shape. Matches the ws.Hub's Event for
// round-tripping: clients send `{type, data}` and receive events back.
type wsEnvelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}