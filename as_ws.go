package nexus

import (
	"fmt"

	"go.uber.org/fx"
)

// AsWebSocket is reserved. WebSocket endpoints have a multi-callback shape
// (OnConnect / OnMessage / OnClose) or use Hub-managed messaging — neither
// collapses into the single-function reflective signature that AsRest and
// AsQuery/AsMutation share. For now, continue declaring WS endpoints via
// (*nexus.Service).WebSocket(path).OnMessage(...).Mount().
//
// A follow-up will introduce a handler shape like:
//
//	func NewPetsStream(svc *PetsService, hub *ws.Hub, conn *websocket.Conn) error { ... }
//
// with AsWebSocket reflecting on it, registering the ws.Builder, and
// returning an fx.Invoke.
func AsWebSocket(path string, fn any, opts ...WsOption) Option {
	return rawOption{o: fx.Error(fmt.Errorf("nexus: AsWebSocket not yet implemented — use (*Service).WebSocket(%q) for now", path))}
}

// WsOption is a placeholder type so callers can migrate option calls in
// advance of the implementation.
type WsOption func(*wsConfig)

type wsConfig struct{}
