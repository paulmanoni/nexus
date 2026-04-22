// Package rest wires REST endpoints onto a Gin engine and records metadata
// about them in the nexus registry.
package rest

import (
	"github.com/gin-gonic/gin"

	"nexus/registry"
	"nexus/trace"
)

type Builder struct {
	engine          *gin.Engine
	reg             *registry.Registry
	bus             *trace.Bus
	service         string
	method          string
	path            string
	description     string
	middleware      []gin.HandlerFunc
	middlewareNames []string
	tags            map[string]string
}

func New(e *gin.Engine, r *registry.Registry, bus *trace.Bus, service, method, path string) *Builder {
	return &Builder{
		engine:  e,
		reg:     r,
		bus:     bus,
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
func (b *Builder) Handler(h gin.HandlerFunc) {
	endpoint := b.method + " " + b.path
	var handlers []gin.HandlerFunc
	if b.bus != nil {
		handlers = append(handlers, trace.Middleware(b.bus, b.service, endpoint, string(registry.REST)))
	}
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
		Middleware:  b.middlewareNames,
		Tags:        b.tags,
	})
}