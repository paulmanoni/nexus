package nexus

import (
	"fmt"
	"log"
	"net/http"
	"reflect"

	"github.com/gin-gonic/gin"
	"go.uber.org/fx"

	"github.com/paulmanoni/nexus/registry"
	"github.com/paulmanoni/nexus/trace"
)

// AsRest registers a REST endpoint from a plain Go handler. The handler's
// signature is inspected via reflection:
//
//   - Leading params are fx-injected deps. The first such param should be
//     your service wrapper (see docs on Service) — its type grounds the
//     endpoint in a service node on the dashboard.
//   - The optional last param is an "args" struct whose tags direct gin on
//     how to bind from the request:
//     uri:"id"     → ShouldBindUri
//     query:"x"    → ShouldBindQuery
//     header:"x"   → ShouldBindHeader
//     form:"x"     → ShouldBind (multipart/url-encoded)
//     json:"x"     → ShouldBindJSON (for non-GET; default when other binders are absent)
//   - The return may be (T, error), (T), (error), or nothing. T gets
//     JSON-marshalled with status 200 (201 for POST) on success; errors
//     become status 500 with {"error": "..."}.
//
// Returns an fx.Option; drop it into fx.Provide.
//
//	fx.Provide(
//	    nexus.AsRest("GET", "/pets",     NewListPets),
//	    nexus.AsRest("POST", "/pets",    NewCreatePet),
//	    nexus.AsRest("GET", "/pets/:id", NewGetPet),
//	)
func AsRest(method, path string, fn any, opts ...RestOption) Option {
	// Pointer cfg so nexus.Module(...) can stamp cfg.module after this
	// call returns — the invoke closure below reads it at fx.Start.
	cfg := &restConfig{}
	for _, o := range opts {
		o.applyToRest(cfg)
	}
	sh, err := inspectHandler(fn)
	if err != nil {
		return rawOption{o: fx.Error(err)}
	}
	return asRestInvoke(method, path, cfg, sh)
}

// AsRestHandler registers a REST endpoint whose handler is a plain
// gin.HandlerFunc supplied by a *factory* function. The factory is
// the fx-resolved piece: its parameters are the deps needed to build
// the handler (controllers, resources, other services), its single
// return is the gin.HandlerFunc that serves requests.
//
// Use this when the handler already manages its own request binding
// and response shaping (typical for code migrated from ad-hoc Gin
// routes) but you still want module annotation, metrics, and the
// dashboard packet-animation treatment AsRest provides:
//
//	nexus.Module("oats-rest",
//	    nexus.AsRestHandler("POST", "/api/devices/register",
//	        func(d *DeviceController) gin.HandlerFunc { return d.RegisterDevice },
//	        nexus.Description("Register a device"),
//	        auth.Required(),
//	    ),
//	)
//
// Factory signature requirements:
//   - Zero or more parameters (fx-injected deps).
//   - Exactly one return value of type gin.HandlerFunc.
//
// On the dashboard this endpoint appears under its enclosing
// nexus.Module (same grouping as AsRest / AsQuery), with metrics +
// trace middleware attached so request.op events drive the live
// packet animation.
func AsRestHandler(method, path string, factory any, opts ...RestOption) Option {
	cfg := &restConfig{}
	for _, o := range opts {
		o.applyToRest(cfg)
	}
	rt := reflect.TypeOf(factory)
	ginHandlerType := reflect.TypeOf(gin.HandlerFunc(nil))
	if rt == nil || rt.Kind() != reflect.Func {
		return rawOption{o: fx.Error(fmt.Errorf("nexus: AsRestHandler factory must be a function"))}
	}
	if rt.NumOut() != 1 || rt.Out(0) != ginHandlerType {
		return rawOption{o: fx.Error(fmt.Errorf("nexus: AsRestHandler factory must return exactly gin.HandlerFunc (got %s)", rt))}
	}
	return asRestHandlerInvoke(method, path, cfg, factory)
}

// asRestHandlerInvoke synthesizes the fx.Invoke for AsRestHandler.
// Parallel to asRestInvoke but simpler: instead of building a
// reflective per-request handler, we resolve the factory's deps once
// at boot, call the factory to get the gin.HandlerFunc, and mount
// it directly. The middleware chain (trace → metrics → user .Use()
// bundles → handler) mirrors asRestInvoke's so the dashboard sees
// the same shape either way.
func asRestHandlerInvoke(method, path string, cfg *restConfig, factory any) Option {
	rt := reflect.TypeOf(factory)
	appType := reflect.TypeOf((*App)(nil))

	in := make([]reflect.Type, 0, rt.NumIn()+1)
	in = append(in, appType)
	depTypes := make([]reflect.Type, rt.NumIn())
	for i := 0; i < rt.NumIn(); i++ {
		depTypes[i] = rt.In(i)
		in = append(in, rt.In(i))
	}
	invokeSig := reflect.FuncOf(in, nil, false)

	invokeFn := reflect.MakeFunc(invokeSig, func(args []reflect.Value) []reflect.Value {
		app := args[0].Interface().(*App)
		deps := args[1:]

		service := resolveEndpointService(cfg.service, cfg.module, deps, depTypes, app)
		finalPath := cfg.pathPrefix + path
		opName := opNameFromFactory(factory, method+" "+finalPath)

		// Invoke the factory once to extract the gin.HandlerFunc.
		out := reflect.ValueOf(factory).Call(deps)
		userHandler := out[0].Interface().(gin.HandlerFunc)

		chain, mwNames := buildEndpointChain(
			app, service,
			service+"."+opName,
			string(registry.REST),
			method+" "+finalPath,
			cfg.bundles, userHandler,
		)
		app.engine.Handle(method, finalPath, chain...)

		endpointName := method + " " + finalPath
		registerEndpoint(app, &cfg.baseEndpointConfig, service, registry.Endpoint{
			Name:       endpointName,
			Transport:  registry.REST,
			Method:     method,
			Path:       finalPath,
			Middleware: mwNames,
		})
		recordEndpointDeps(app, service, endpointName, deps, depTypes)
		return nil
	})
	return &restOption{o: fx.Invoke(invokeFn.Interface()), cfg: cfg}
}

// opNameFromFactory derives a dashboard op name for a factory-style
// handler registration. Tries the factory's own runtime name (e.g. a
// closure literal) first; falls back to "<method> <path>" when the
// factory is anonymous — same fallback opNameFromFunc uses for
// anonymous reflective handlers.
func opNameFromFactory(factory any, fallback string) string {
	name := opNameFromFunc(factory, "")
	if name == "" {
		return fallback
	}
	return name
}

type restConfig struct {
	baseEndpointConfig
	service string // optional explicit service name; auto-derived if empty
	// pathPrefix is prepended to the route's path before the endpoint
	// is mounted on Gin. Set either per-endpoint via nexus.RoutePrefix
	// as a RestOption, or module-wide by passing nexus.RoutePrefix as
	// an opt to nexus.Module — the framework stamps it on every REST
	// child of that module.
	pathPrefix string
}

// restOption is the Option returned by AsRest. Parallels gqlFieldOption —
// keeps a pointer to the restConfig so Module(...) can stamp the module
// name on it after construction, and the fx.Invoke closure picks it up
// at Start time.
type restOption struct {
	o   fx.Option
	cfg *restConfig
}

func (r *restOption) nexusOption() fx.Option   { return r.o }
func (r *restOption) setModule(name string)    { r.cfg.module = name }
func (r *restOption) setRestPrefix(p string)   { r.cfg.pathPrefix = p + r.cfg.pathPrefix }
func (r *restOption) setDeployment(tag string) { r.cfg.deployment = tag }

// restPrefixAnnotator is implemented by options whose path can be
// prefixed by an enclosing nexus.Module(..., nexus.RoutePrefix("/api"))
// declaration. Only AsRest / AsRestHandler registrations implement it —
// GraphQL / worker options ignore the prefix.
type restPrefixAnnotator interface {
	setRestPrefix(p string)
}

// routePrefixOption carries a prefix string. Can be used two ways:
//
//   1. Inside nexus.Module's opts list — Module picks it up and stamps
//      the prefix onto every REST child option.
//   2. As a per-endpoint option to AsRest / AsRestHandler — applied
//      directly via applyToRest.
//
// Always safe to include; GraphQL / worker opts silently ignore it.
type routePrefixOption struct{ prefix string }

func (routePrefixOption) nexusOption() fx.Option { return fx.Options() }
func (r routePrefixOption) applyToRest(c *restConfig) {
	c.pathPrefix = r.prefix + c.pathPrefix
}

// RoutePrefix prepends a string to the paths of the REST endpoints it
// applies to. Two usage patterns:
//
//	// Module-wide: every AsRest / AsRestHandler in the module sees "/api/v1".
//	nexus.Module("adverts", nexus.RoutePrefix("/api/v1"),
//	    nexus.AsRest("GET", "/adverts", NewListAdverts),  // → /api/v1/adverts
//	    nexus.AsRest("POST", "/adverts", NewCreateAdvert),
//	)
//
//	// Per-endpoint:
//	nexus.AsRest("GET", "/health", NewHealth, nexus.RoutePrefix("/ops"))
//
// The prefix is stored verbatim — include (or omit) the leading slash
// yourself; nexus does not normalize. Stacking within a single Module
// concatenates left-to-right, so `Module(..., RoutePrefix("/a"),
// RoutePrefix("/b"), AsRest("GET", "/x", ...))` mounts at /a/b/x.
// Prefixes do NOT stack across nested Module calls — the inner module
// already stamped its children before the outer sees them. If you
// need stacking, compose the prefix string explicitly.
func RoutePrefix(p string) routePrefixOption {
	return routePrefixOption{prefix: p}
}

// RestOption tunes an AsRest registration. Interface (not a func) so
// nexus.Use can satisfy both GqlOption and RestOption from a single
// value. The one-off func-shaped helpers below adapt via restOptionFn.
type RestOption interface{ applyToRest(*restConfig) }

type restOptionFn func(*restConfig)

func (f restOptionFn) applyToRest(c *restConfig) { f(c) }

// Description sets the human-readable description shown on the dashboard.
func Description(s string) RestOption {
	return restOptionFn(func(c *restConfig) { c.description = s })
}

// asRestInvoke builds a synthetic fx.Invoke: the constructor fx sees takes
// (*App, deps...) and registers the handler on the Gin engine + the registry.
// We build its signature via reflect.FuncOf so any dep type the handler named
// flows through fx's dependency resolution unchanged.
func asRestInvoke(method, path string, cfg *restConfig, sh handlerShape) Option {
	appType := reflect.TypeOf((*App)(nil))

	in := make([]reflect.Type, 0, len(sh.depTypes)+1)
	in = append(in, appType)
	in = append(in, sh.depTypes...)
	fnType := reflect.FuncOf(in, nil, false)

	invokeFn := reflect.MakeFunc(fnType, func(args []reflect.Value) []reflect.Value {
		app := args[0].Interface().(*App)
		deps := args[1:]

		service := resolveEndpointService(cfg.service, cfg.module, deps, sh.depTypes, app)

		// Resolve the final mounted path by prefixing — module-level
		// or per-endpoint RoutePrefix stamped cfg.pathPrefix for us.
		finalPath := cfg.pathPrefix + path
		opName := opNameFromFunc(sh.funcVal.Interface(), method+" "+finalPath)
		handler := buildGinHandler(method, sh, deps, app.bus, service, finalPath)

		// AsRest's reflective path threads tracing inside buildGinHandler
		// (so the handler can read the span back); pass an empty
		// traceEndpoint here to skip the chain-level trace prefix.
		chain, mwNames := buildEndpointChain(
			app, service,
			service+"."+opName,
			string(registry.REST),
			"",
			cfg.bundles, handler,
		)
		app.engine.Handle(method, finalPath, chain...)
		registerEndpoint(app, &cfg.baseEndpointConfig, service, registry.Endpoint{
			Name:       opName,
			Transport:  registry.REST,
			Method:     method,
			Path:       finalPath,
			Middleware: mwNames,
		})
		recordEndpointDeps(app, service, opName, deps, sh.depTypes)
		return nil
	})
	return &restOption{o: fx.Invoke(invokeFn.Interface()), cfg: cfg}
}

// serviceNameFromDeps returns the Service.Name() of the first dep that
// embeds *Service (the "service wrapper" convention), or "" if none do.
// Dashboard uses "" as an anonymous bucket so endpoints still appear.
func serviceNameFromDeps(deps []reflect.Value, depTypes []reflect.Type) string {
	for i, d := range deps {
		if s, ok := unwrapService(d, depTypes[i]); ok {
			return s.name
		}
	}
	return ""
}

// unwrapService walks into a struct dep looking for an embedded *Service field.
// Returns (nil, false) if the dep doesn't follow the wrapper convention.
func unwrapService(v reflect.Value, t reflect.Type) (*Service, bool) {
	// Direct *Service
	if t == reflect.TypeOf((*Service)(nil)) {
		if v.IsNil() {
			return nil, false
		}
		return v.Interface().(*Service), true
	}
	// Pointer to a struct with an embedded *Service field
	if t.Kind() == reflect.Ptr && t.Elem().Kind() == reflect.Struct {
		if v.IsNil() {
			return nil, false
		}
		elem := v.Elem()
		st := t.Elem()
		for i := 0; i < st.NumField(); i++ {
			f := st.Field(i)
			if f.Anonymous && f.Type == reflect.TypeOf((*Service)(nil)) {
				fv := elem.Field(i)
				if fv.IsNil() {
					return nil, false
				}
				return fv.Interface().(*Service), true
			}
		}
	}
	return nil, false
}

// buildGinHandler synthesizes the gin.HandlerFunc that binds the args struct
// (if any), calls the user handler reflectively, and writes the response.
func buildGinHandler(method string, sh handlerShape, deps []reflect.Value, bus *trace.Bus, service, path string) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Tracing mirrors what transport/rest does: a request.start/end pair
		// bracketing the handler. We do it inline because AsRest bypasses the
		// rest.Builder path.
		var span *trace.Span
		if bus != nil {
			h := trace.Middleware(bus, service, method+" "+path, string(registry.REST))
			// Delegate by calling Middleware's returned func inline —
			// it manages c.Next(). We still run our logic inside.
			h(c)
			if s, ok := trace.SpanFrom(c); ok {
				span = s
			}
			// Abort early if middleware short-circuited the response.
			if c.IsAborted() {
				return
			}
			_ = span
		}

		var args reflect.Value
		if sh.hasArgs {
			ptr := reflect.New(sh.argsType)
			if err := bindArgs(c, ptr.Interface()); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			args = ptr.Elem()
		}

		result, err := sh.callHandler(callInput{
			Ctx:    c.Request.Context(),
			GinCtx: c, // available to handlers that take *gin.Context as a param
		}, deps, args)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		// A handler that takes *gin.Context may have already written the
		// response itself (custom status codes, streamed body, etc.).
		// Skip the automatic success write in that case to avoid clobbering
		// the handler's own reply.
		if c.Writer.Written() {
			return
		}
		if sh.resultIdx < 0 {
			c.Status(defaultSuccessStatus(method))
			return
		}
		c.JSON(defaultSuccessStatus(method), result)
	}
}

// bindArgs binds a request into the args struct using gin's existing
// ShouldBindUri / ShouldBindQuery / ShouldBindHeader / ShouldBindJSON based
// on the tags present on the struct. Multiple tag families may coexist —
// e.g. uri:"id" alongside json:"payload" — and each binder runs against its
// own fields.
func bindArgs(c *gin.Context, ptr any) error {
	t := reflect.TypeOf(ptr).Elem()
	hasURI, hasQuery, hasHeader, hasForm, hasJSON := tagSurvey(t)

	if hasURI {
		if err := c.ShouldBindUri(ptr); err != nil {
			return fmt.Errorf("bind uri: %w", err)
		}
	}
	if hasQuery {
		if err := c.ShouldBindQuery(ptr); err != nil {
			return fmt.Errorf("bind query: %w", err)
		}
	}
	if hasHeader {
		if err := c.ShouldBindHeader(ptr); err != nil {
			return fmt.Errorf("bind header: %w", err)
		}
	}
	if hasForm {
		if err := c.ShouldBind(ptr); err != nil {
			return fmt.Errorf("bind form: %w", err)
		}
	}
	if hasJSON && c.Request.Body != nil && c.Request.Method != http.MethodGet {
		if err := c.ShouldBindJSON(ptr); err != nil {
			// Empty body on methods that tolerate it — don't fail the whole call.
			if err.Error() != "EOF" {
				return fmt.Errorf("bind json: %w", err)
			}
		}
	}
	return nil
}

// tagSurvey walks one level of struct fields checking which binder families
// apply. If none are present we still try JSON on methods with bodies — the
// tag vocabulary is a hint, not a wall.
func tagSurvey(t reflect.Type) (uri, query, header, form, json bool) {
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag
		if _, ok := tag.Lookup("uri"); ok {
			uri = true
		}
		if _, ok := tag.Lookup("query"); ok || tag.Get("form") != "" {
			query = true
		}
		if _, ok := tag.Lookup("header"); ok {
			header = true
		}
		if tag.Get("form") != "" {
			form = true
		}
		if _, ok := tag.Lookup("json"); ok {
			json = true
		}
	}
	// If the struct has no recognised tags at all, default to JSON body
	// binding for non-GET methods.
	if !uri && !query && !header && !form && !json {
		json = true
	}
	return
}

func defaultSuccessStatus(method string) int {
	if method == http.MethodPost {
		return http.StatusCreated
	}
	return http.StatusOK
}

// Silence unused-import warnings if any method isn't reached yet.
var _ = log.Printf
