// Package nexus is a thin framework over Gin that registers every endpoint
// (REST, GraphQL, WebSocket) into a central registry, traces every request
// into an in-memory event bus, and exposes both under /__nexus for tooling
// — notably the Vue dashboard.
//
// nexus does NOT replace the caller's GraphQL layer: hand it a *graphql.Schema
// (typically built with github.com/paulmanoni/go-graph) and it mounts + introspects.
package nexus

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/paulmanoni/nexus/dashboard"
	"github.com/paulmanoni/nexus/registry"
	"github.com/paulmanoni/nexus/resource"
	"github.com/paulmanoni/nexus/trace"
)

const defaultDashboardName = "Nexus"

type App struct {
	engine        *gin.Engine
	registry      *registry.Registry
	bus           *trace.Bus
	dashboardOn   bool
	dashboardName string
}

type Option func(*App)

// WithEngine supplies a pre-configured Gin engine. Without it, nexus builds a
// bare engine with just Recovery so the caller can bring their own logger.
func WithEngine(e *gin.Engine) Option {
	return func(a *App) { a.engine = e }
}

// WithTracing enables per-request trace events, buffered in a ring of the given
// capacity. Required for the dashboard's event stream to show anything.
func WithTracing(capacity int) Option {
	return func(a *App) { a.bus = trace.NewBus(capacity) }
}

// WithDashboard mounts /__nexus/endpoints (always) and /__nexus/events (if tracing is on).
func WithDashboard() Option {
	return func(a *App) { a.dashboardOn = true }
}

// WithDashboardName sets the brand shown in the dashboard header and the
// browser tab title. Defaults to "Nexus". The name is served over
// /__nexus/config so the client picks it up without a rebuild.
func WithDashboardName(name string) Option {
	return func(a *App) { a.dashboardName = name }
}

func New(opts ...Option) *App {
	a := &App{dashboardName: defaultDashboardName}
	for _, opt := range opts {
		opt(a)
	}
	if a.engine == nil {
		a.engine = gin.New()
		a.engine.Use(gin.Recovery())
	}
	a.registry = registry.New()
	if a.dashboardOn {
		dashboard.Mount(a.engine, a.registry, a.bus, dashboard.Config{Name: a.dashboardName})
	}
	return a
}

func (a *App) Engine() *gin.Engine          { return a.engine }
func (a *App) Registry() *registry.Registry { return a.registry }
func (a *App) Bus() *trace.Bus              { return a.bus }
func (a *App) Run(addr string) error        { return a.engine.Run(addr) }

// Register adds a resource (database, cache, queue) to the app so its health
// shows up on the dashboard. Use Service.Attach(r) to also draw an edge
// between the owning service(s) and the resource.
func (a *App) Register(r resource.Resource) {
	a.registry.RegisterResource(r)
}

// UseReporter is satisfied by any type that exposes an OnUse hook with this
// exact signature. multi.Registry and anything embedding it fit — including
// the project's own DBManager wrapper. This is a structural interface so
// nexus doesn't need to import nexus/multi directly.
type UseReporter interface {
	OnUse(func(ctx context.Context, name string))
}

// OnResourceUse installs an auto-attach hook onto any UseReporter (typically
// a *multi.Registry or a user wrapper around one). Whenever code calls
// target.UsingCtx(ctx, "resource-name") during a request, the hook:
//
//  1. reads the current trace.Span from ctx so we know which service made the call
//  2. AttachResource(service, resource) on the registry — edge appears live
//  3. emits a "downstream" trace event so the Traces tab shows the lookup
//
// Calls with no span in context (e.g. UsingCtx fired from main or a cron
// job outside the trace middleware) are silently ignored — there's no
// service to attribute the usage to.
func (a *App) OnResourceUse(target UseReporter) {
	target.OnUse(func(ctx context.Context, name string) {
		span, ok := trace.SpanFromCtx(ctx)
		if !ok {
			return
		}
		a.registry.AttachResource(span.Service, name)
		if a.bus != nil {
			a.bus.Publish(trace.Event{
				TraceID: span.TraceID,
				Kind:    trace.KindDownstream,
				Service: span.Service,
				Message: "resource.using:" + name,
				Meta:    map[string]any{"resource": name},
			})
		}
	})
}
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.engine.ServeHTTP(w, r)
}
