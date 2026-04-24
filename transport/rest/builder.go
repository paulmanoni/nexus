// Package rest wires REST endpoints onto a Gin engine and records metadata
// about them in the nexus registry.
package rest

import (
	"github.com/gin-gonic/gin"

	"github.com/paulmanoni/nexus/metrics"
	"github.com/paulmanoni/nexus/middleware"
	"github.com/paulmanoni/nexus/registry"
	"github.com/paulmanoni/nexus/trace"
)

type Builder struct {
	engine          *gin.Engine
	reg             *registry.Registry
	bus             *trace.Bus
	metrics         metrics.Store
	service         string
	method          string
	path            string
	description     string
	middleware      []gin.HandlerFunc
	middlewareNames []string
	tags            map[string]string
}

// New constructs a builder for a single REST endpoint. The metrics
// store may be nil — in that case the built-in per-op counter + the
// request.op trace event (which drives dashboard packet animations)
// are skipped, and the endpoint renders without live traffic pulses.
// Pass a non-nil store (typically app.Metrics()) to get parity with
// endpoints registered via nexus.AsRest.
func New(e *gin.Engine, r *registry.Registry, bus *trace.Bus, ms metrics.Store, service, method, path string) *Builder {
	return &Builder{
		engine:  e,
		reg:     r,
		bus:     bus,
		metrics: ms,
		service: service,
		method:  method,
		path:    path,
		tags:    map[string]string{},
	}
}

func (b *Builder) Describe(s string) *Builder {
	b.description = s
	return b
}

// Use attaches a named middleware. The name shows up in the dashboard; prefer
// something meaningful ("auth", "rate-limit") over runtime-generated strings.
// If the name isn't already registered, it's auto-added as a Custom middleware.
func (b *Builder) Use(name string, mw gin.HandlerFunc) *Builder {
	b.middleware = append(b.middleware, mw)
	b.middlewareNames = append(b.middlewareNames, name)
	b.reg.EnsureMiddleware(name)
	return b
}

func (b *Builder) Tag(k, v string) *Builder {
	b.tags[k] = v
	return b
}

// Handler mounts the endpoint on Gin and records it in the registry. Terminal.
//
// Middleware chain (in order): trace (request.start/end) → metrics
// (per-op count + request.op event) → user-supplied .Use() middlewares
// → the terminal handler. The metrics slot is what makes the endpoint
// pulse on the Architecture dashboard when traffic hits it — without
// it the UI animator has no request.op event to key off and rows
// stay dark. Nil metrics.Store skips that slot cleanly.
func (b *Builder) Handler(h gin.HandlerFunc) {
	endpoint := b.method + " " + b.path
	var handlers []gin.HandlerFunc
	var mwNames []string
	if b.bus != nil {
		handlers = append(handlers, trace.Middleware(b.bus, b.service, endpoint, string(registry.REST)))
	}
	if b.metrics != nil {
		bundle := metrics.NewMiddleware(b.metrics, b.service+"."+endpoint)
		handlers = append(handlers, bundle.Gin)
		mwNames = append(mwNames, bundle.Name)
		b.reg.RegisterMiddleware(middleware.Info{
			Name:        bundle.Name,
			Description: bundle.Description,
			Kind:        middleware.KindBuiltin,
		})
	}
	mwNames = append(mwNames, b.middlewareNames...)
	handlers = append(handlers, b.middleware...)
	handlers = append(handlers, h)
	b.engine.Handle(b.method, b.path, handlers...)
	b.reg.RegisterEndpoint(registry.Endpoint{
		Service:     b.service,
		Name:        endpoint,
		Transport:   registry.REST,
		Method:      b.method,
		Path:        b.path,
		Description: b.description,
		Middleware:  mwNames,
		Tags:        b.tags,
	})
}