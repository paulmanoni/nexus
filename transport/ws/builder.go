// Package ws wires WebSocket endpoints onto a Gin engine using gorilla/websocket
// and records metadata about them in the nexus registry.
package ws

import (
	"net/http"

	"github.com/gorilla/websocket"
	"github.com/paulmanoni/nexus/httpx"

	"github.com/paulmanoni/nexus/registry"
	"github.com/paulmanoni/nexus/trace"
)

type ConnectFunc func(c *httpx.Ctx, conn *websocket.Conn) error
type MessageFunc func(conn *websocket.Conn, msgType int, data []byte) error
type CloseFunc func(conn *websocket.Conn)

type Builder struct {
	engine          httpx.Router
	reg             *registry.Registry
	bus             *trace.Bus
	service         string
	path            string
	description     string
	upgrader        websocket.Upgrader
	onConnect       ConnectFunc
	onMessage       MessageFunc
	onClose         CloseFunc
	hub             *Hub
	middleware      []httpx.HandlerFunc
	middlewareNames []string
	tags            map[string]string
}

func New(e httpx.Router, r *registry.Registry, bus *trace.Bus, service, path string) *Builder {
	return &Builder{
		engine:   e,
		reg:      r,
		bus:      bus,
		service:  service,
		path:     path,
		upgrader: websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
		tags:     map[string]string{},
	}
}

func (b *Builder) Describe(s string) *Builder { b.description = s; return b }

func (b *Builder) Use(name string, mw httpx.HandlerFunc) *Builder {
	b.middleware = append(b.middleware, mw)
	b.middlewareNames = append(b.middlewareNames, name)
	b.reg.EnsureMiddleware(name)
	return b
}

func (b *Builder) Upgrader(u websocket.Upgrader) *Builder { b.upgrader = u; return b }
func (b *Builder) OnConnect(fn ConnectFunc) *Builder      { b.onConnect = fn; return b }
func (b *Builder) OnMessage(fn MessageFunc) *Builder      { b.onMessage = fn; return b }
func (b *Builder) OnClose(fn CloseFunc) *Builder          { b.onClose = fn; return b }
func (b *Builder) Tag(k, v string) *Builder               { b.tags[k] = v; return b }

// WithHub hands connection management to a Hub: rooms, user/client-targeted
// events, broadcast, worker-pool fan-out, slow-client backpressure, and the
// default subscribe/authenticate/ping message protocol all become available.
// When a hub is set, OnConnect/OnMessage/OnClose on the Builder are ignored —
// install hooks on the Hub instead (hub.OnMessage, hub.OnConnect, ...).
func (b *Builder) WithHub(h *Hub) *Builder { b.hub = h; return b }

// Mount attaches the WebSocket endpoint to Gin and records it in the registry. Terminal.
//
// Tracing: NO trace.Middleware on the upgrade route. WS upgrade is a
// one-time HTTP request that promotes to a long-lived connection;
// wrapping it in request.start/request.end would keep one trace open
// for the entire connection lifetime (until close) and produce no
// renderable spans. Per-frame traces are emitted inside b.serve's
// read loop instead — each frame becomes its own root trace on the
// dashboard's waterfall, matching how AsWS's typed dispatcher does
// it.
func (b *Builder) Mount() {
	endpoint := "WS " + b.path
	handlers := append([]httpx.HandlerFunc(nil), b.middleware...)
	handlers = append(handlers, b.serve)
	b.engine.GET(b.path, handlers...)
	b.reg.RegisterEndpoint(registry.Endpoint{
		Service:     b.service,
		Name:        endpoint,
		Transport:   registry.WebSocket,
		Path:        b.path,
		Description: b.description,
		Middleware:  b.middlewareNames,
		Tags:        b.tags,
	})
}

func (b *Builder) serve(c *httpx.Ctx) {
	if b.hub != nil {
		b.hub.serve(c, b.upgrader)
		return
	}
	conn, err := b.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	if b.onConnect != nil {
		if err := b.onConnect(c, conn); err != nil {
			return
		}
	}
	if b.onClose != nil {
		defer b.onClose(conn)
	}
	if b.onMessage == nil {
		return
	}
	endpoint := "WS " + b.path
	for {
		t, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		// Each frame is its own root trace so the dashboard's
		// waterfall renders them as independent requests — same
		// shape AsWS's typed dispatcher emits. NewRootSpan is a
		// no-op when b.bus is nil (never the case in production
		// nexus apps but tests sometimes pass nil).
		ctx := trace.WithBus(c.Request.Context(), b.bus)
		_, _, finish := trace.NewRootSpan(
			ctx, endpoint, b.service, endpoint, string(registry.WebSocket),
			trace.Int("ws.message_type", int64(t)),
			trace.Int("ws.payload_bytes", int64(len(data))),
		)
		err = b.onMessage(conn, t, data)
		status := 200
		if err != nil {
			status = 500
		}
		finish(status, err)
		if err != nil {
			return
		}
	}
}
