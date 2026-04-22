// Package ws wires WebSocket endpoints onto a Gin engine using gorilla/websocket
// and records metadata about them in the nexus registry.
package ws

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"nexus/registry"
	"nexus/trace"
)

type ConnectFunc func(c *gin.Context, conn *websocket.Conn) error
type MessageFunc func(conn *websocket.Conn, msgType int, data []byte) error
type CloseFunc func(conn *websocket.Conn)

type Builder struct {
	engine          *gin.Engine
	reg             *registry.Registry
	bus             *trace.Bus
	service         string
	path            string
	description     string
	upgrader        websocket.Upgrader
	onConnect       ConnectFunc
	onMessage       MessageFunc
	onClose         CloseFunc
	middleware      []gin.HandlerFunc
	middlewareNames []string
	tags            map[string]string
}

func New(e *gin.Engine, r *registry.Registry, bus *trace.Bus, service, path string) *Builder {
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

func (b *Builder) Use(name string, mw gin.HandlerFunc) *Builder {
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

// Mount attaches the WebSocket endpoint to Gin and records it in the registry. Terminal.
func (b *Builder) Mount() {
	endpoint := "WS " + b.path
	var handlers []gin.HandlerFunc
	if b.bus != nil {
		handlers = append(handlers, trace.Middleware(b.bus, b.service, endpoint, string(registry.WebSocket)))
	}
	handlers = append(handlers, b.middleware...)
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

func (b *Builder) serve(c *gin.Context) {
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
	for {
		t, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if err := b.onMessage(conn, t, data); err != nil {
			return
		}
	}
}