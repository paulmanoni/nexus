package nexus

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"reflect"
	"sync"

	"braces.dev/errtrace"
	"github.com/paulmanoni/nexus/v2/di"
	"github.com/paulmanoni/nexus/v2/httpx"
	"github.com/paulmanoni/nexus/v2/internal/appctx"
	"github.com/paulmanoni/nexus/v2/internal/maskhook"
	"github.com/paulmanoni/nexus/v2/middleware"
	"github.com/paulmanoni/nexus/v2/registry"
	"github.com/paulmanoni/nexus/v2/trace"
)

// AsRest registers a REST endpoint from a plain Go handler. The handler's
// signature is inspected via reflection:
//
//   - Leading params are fx-injected deps. The first such param should be
//     your service wrapper (see docs on Service) — its type grounds the
//     endpoint in a service node on the dashboard.
//   - The optional last param is an "args" struct whose tags direct gin on
//     how to bind from the request:
//     path:"id"    → ShouldBindUri
//     query:"x"    → ShouldBindQuery
//     header:"x"   → ShouldBindHeader
//     form:"x"     → ShouldBind (multipart/url-encoded)
//     json:"x"     → ShouldBindJSON (for non-GET; default when other binders are absent)
//   - The return may be (T, error), (T), (error), or nothing. T gets
//     JSON-marshalled with status 200 (201 for POST) on success; errors
//     render through the error model (see Error): the status of their code
//     with {"code", "message", "errors"}. Args are validated (validate:
//     tags) after binding; a failure is a 422 InvalidInput.
//
// Returns an di.Option; drop it into di.Provide.
//
//	di.Provide(
//	    nexus.AsRest("GET", "/pets",     NewListPets),
//	    nexus.AsRest("POST", "/pets",    NewCreatePet),
//	    nexus.AsRest("GET", "/pets/:id", NewGetPet),
//	)
func AsRest(method, path string, fn any, opts ...RestOption) Option {
	// Pointer cfg so nexus.Module(...) can stamp cfg.module after this
	// call returns — the invoke closure below reads it at di.Start.
	cfg := &restConfig{}
	for _, o := range opts {
		o.applyToRest(cfg)
	}
	if cfg.optErr != nil {
		return rawOption{o: di.Error(fmt.Errorf("nexus: %s %s: %w", method, path, cfg.optErr))}
	}
	if err := checkBundleTransports(cfg.bundles, middleware.TransportREST, method+" "+path); err != nil {
		return rawOption{o: di.Error(err)}
	}
	if isRestFactory(fn) {
		return asRestFactory(method, path, cfg, fn)
	}
	sh, err := inspectHandlerArgs(fn, cfg.argNames, routePathParams(path)...)
	if err != nil {
		return rawOption{o: di.Error(err)}
	}
	if cfg.envelope != nil {
		if err := cfg.envelope.check(sh.returnType); err != nil {
			return rawOption{o: di.Error(err)}
		}
		cfg.setTag(registry.EnvelopeTag, "true")
	}
	return asRestInvoke(method, path, cfg, sh)
}

type restConfig struct {
	baseEndpointConfig
	service string // optional explicit service name; auto-derived if empty
	// pathPrefix is prepended to the route's path before the endpoint
	// is mounted on the router. Set either per-endpoint via nexus.RoutePrefix
	// as a RestOption, or module-wide by passing nexus.RoutePrefix as
	// an opt to nexus.Module — the framework stamps it on every REST
	// child of that module.
	pathPrefix string
	// renderer, when set via WithRenderer, replaces the default
	// c.JSON(...) success write with a custom encoding (e.g. the
	// Inertia page protocol). Nil for the overwhelming majority of
	// endpoints, which JSON-marshal their return value. See
	// endpoint.go.
	renderer ResponseRenderer
	// action is the controller action being registered, set by
	// ControllerRouter so an ActionOption can resolve against it; nil for a
	// plain AsRest.
	action *actionContext
	// optErr is an option that could not apply (an ActionOption outside a
	// controller); AsRest reports it at boot.
	optErr error
}

// restOption is the Option returned by AsRest. Parallels gqlFieldOption —
// keeps a pointer to the restConfig so Module(...) can stamp the module
// name on it after construction, and the di.Invoke closure picks it up
// at Start time.
type restOption struct {
	o   di.Option
	cfg *restConfig
}

func (r *restOption) nexusOption() di.Option { return r.o }
func (r *restOption) setModule(name string)  { r.cfg.module = name }
func (r *restOption) setRestPrefix(p string) { r.cfg.pathPrefix = p + r.cfg.pathPrefix }

// restPrefixAnnotator is implemented by options whose path can be
// prefixed by an enclosing nexus.Module(..., nexus.RoutePrefix("/api"))
// declaration. Only AsRest registrations implement it —
// GraphQL / worker options ignore the prefix.
type restPrefixAnnotator interface {
	setRestPrefix(p string)
}

// routePrefixOption carries a prefix string. Can be used two ways:
//
//  1. Inside nexus.Module's opts list — Module picks it up and stamps
//     the prefix onto every REST child option.
//  2. As a per-endpoint option to AsRest — applied
//     directly via applyToRest.
//
// Always safe to include; GraphQL / worker opts silently ignore it.
type routePrefixOption struct{ prefix string }

func (routePrefixOption) nexusOption() di.Option { return di.Options() }
func (r routePrefixOption) applyToRest(c *restConfig) {
	c.pathPrefix = r.prefix + c.pathPrefix
}

// RoutePrefix prepends a string to the paths of the REST endpoints it
// applies to. Two usage patterns:
//
//	// Module-wide: every AsRest in the module sees "/api/v1".
//	nexus.Module("orders", nexus.RoutePrefix("/api/v1"),
//	    nexus.AsRest("GET", "/orders", NewListOrders),  // → /api/v1/orders
//	    nexus.AsRest("POST", "/orders", NewCreateOrder),
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

// RestOptions bundles several REST options into one, applied in order — for
// extensions whose single option expands to a renderer, tags and an icon
// (inertia.Component).
func RestOptions(opts ...RestOption) RestOption {
	return restOptionFn(func(c *restConfig) {
		for _, o := range opts {
			o.applyToRest(c)
		}
	})
}

// asRestInvoke builds a synthetic di.Invoke: the constructor fx sees takes
// (*App, deps...) and registers the handler on the router + the registry.
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
		// app.PrefixPath wraps the deployment-wide prefix on top.
		finalPath := app.PrefixPath(cfg.pathPrefix + path)
		// REST op identifier — "<METHOD> <path>" — unique per endpoint
		// even when the same handler is reused across routes.
		opName := method + " " + finalPath
		handler := buildGinHandler(method, sh, deps, app.bus, service, finalPath, cfg.renderer, cfg.envelope, app)

		// AsRest's reflective path threads tracing inside buildGinHandler
		// (so the handler can read the span back); pass an empty
		// traceEndpoint here to skip the chain-level trace prefix.
		chain, mwNames := buildEndpointChain(
			app, service,
			service+"."+opName,
			string(registry.REST),
			"",
			app.gateBundles(cfg.tags, cfg.bundles), handler,
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
		recordEndpointSchema(app, service, opName, sh)
		return nil
	})
	return &restOption{o: di.Invoke(invokeFn.Interface()), cfg: cfg}
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
	if t.Kind() == reflect.Pointer && t.Elem().Kind() == reflect.Struct {
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

// buildGinHandler synthesizes the httpx.HandlerFunc that binds the args struct
// (if any), calls the user handler reflectively, and writes the response.
//
// renderer is nil for ordinary endpoints (the default c.JSON success write);
// when an option like nexus.WithRenderer / inertia.Page supplies one, it owns
// the success write instead. The error path is unchanged — renderers only
// shape successful returns in this release.
func buildGinHandler(method string, sh handlerShape, deps []reflect.Value, bus *trace.Bus, service, path string, renderer ResponseRenderer, env *envelopeSpec, app *App) httpx.HandlerFunc {
	endpointName := method + " " + path
	return func(c *httpx.Ctx) {
		// Expose the app to a custom renderer (e.g. Inertia) so it can pull
		// per-app state via the appctx key — order-independent, unlike global
		// middleware. Only when a renderer is attached, so ordinary JSON
		// endpoints pay nothing.
		if renderer != nil {
			c.Set(appctx.Key, app)
		}
		// Tracing mirrors what transport/rest does: a request.start/end pair
		// bracketing the handler. We do it inline because AsRest bypasses the
		// rest.Builder path — and via StartRequest, not trace.Middleware:
		// this func is the END of the chain, so Middleware's c.Next() would
		// return immediately and publish request.end before the handler ran.
		if bus != nil {
			_, finish := trace.StartRequest(c, bus, service, endpointName, string(registry.REST))
			defer finish()
		}

		var args reflect.Value
		if sh.hasArgs {
			ptr := reflect.New(sh.argsType)
			if err := bindArgs(c, ptr.Interface()); err != nil {
				var tooLarge *http.MaxBytesError
				if errors.As(err, &tooLarge) {
					c.AbortWithStatusJSON(middleware.ErrorBody(http.StatusRequestEntityTooLarge, err))
					return
				}
				WriteError(c, bindError(err))
				return
			}
			if err := validateValue(ptr); err != nil {
				WriteError(c, err)
				return
			}
			args = ptr.Elem()
		}

		result, err := sh.callHandler(callInput{
			Ctx:    c.Request.Context(),
			GinCtx: c, // available to handlers that take *httpx.Ctx as a param
		}, deps, args)
		// The envelope sees the raw (result, err) pair BEFORE the error
		// branch: converting an error into a wire value (Status:false
		// payload with HTTP 200) is exactly what an envelope is for. An
		// error the wrap returns keeps flowing down the standard path.
		if env != nil {
			result, err = env.apply(result, err)
		}
		if err != nil {
			// Offer the error to a renderer first (e.g. Inertia turning
			// a Redirect/Location sentinel into a 303/409). If it takes
			// ownership we skip the trace error-wrap + status mapping
			// below — a redirect is a normal outcome, not a 500.
			if er, ok := renderer.(ErrorRenderer); ok {
				if handled, rerr := er.RenderError(c, err); handled {
					if rerr != nil {
						_ = c.Error(errtrace.Wrap(rerr))
						if !c.Writer.Written() {
							WriteError(c, rerr)
						}
					}
					return
				}
			}
			// Validation errors are a normal outcome, not a failure of the
			// endpoint: they skip the error-trace machinery. Every other
			// error is recorded with errtrace.Wrap, which captures THIS
			// frame as a bottom-of-stack marker so the dashboard's
			// "▸ stack" toggle has something to show; c.Error hands it to
			// the metrics recorder. Inertia pages never reach here — their
			// renderer's RenderError answered above.
			if CodeOf(err) != InvalidInput {
				_ = c.Error(errtrace.Wrap(err))
			}
			WriteError(c, err)
			return
		}
		// A handler that takes *httpx.Ctx may have already written the
		// response itself (custom status codes, streamed body, etc.).
		// Skip the automatic success write in that case to avoid clobbering
		// the handler's own reply.
		if c.Writer.Written() {
			return
		}
		if sh.resultIdx < 0 {
			if er, ok := renderer.(EmptyRenderer); ok {
				if rerr := er.RenderEmpty(c); rerr != nil {
					_ = c.Error(errtrace.Wrap(rerr))
					if !c.Writer.Written() {
						WriteError(c, rerr)
					}
				}
				return
			}
			c.Status(defaultSuccessStatus(method))
			return
		}
		if renderer != nil {
			if rerr := renderer.Render(c, result); rerr != nil {
				_ = c.Error(errtrace.Wrap(rerr))
				if !c.Writer.Written() {
					WriteError(c, rerr)
				}
			}
			return
		}
		// Single-pass masking: rewrite the marshaled bytes instead of
		// the old marshal → tree → walk → re-marshal pipeline (two
		// encodes and a full map[string]any decode per response).
		// ok=false covers every fallback: masking off, out of scope,
		// unmarshalable — c.JSON then behaves exactly as before.
		if b, ok := maskhook.MaskResponse(result); ok {
			c.Data(defaultSuccessStatus(method), "application/json; charset=utf-8", b)
			return
		}
		c.JSON(defaultSuccessStatus(method), result)
	}
}

// bindArgs binds a request into the args struct using gin's existing
// ShouldBindUri / ShouldBindQuery / ShouldBindHeader / ShouldBindJSON based
// on the tags present on the struct. Multiple tag families may coexist —
// e.g. path:"id" alongside json:"payload" — and each binder runs against its
// own fields.
func bindArgs(c *httpx.Ctx, ptr any) error {
	t := reflect.TypeOf(ptr).Elem()
	sv := surveyFor(t)
	hasURI, hasQuery, hasHeader, hasForm, hasJSON := sv.uri, sv.query, sv.header, sv.form, sv.json

	if hasURI {
		if err := c.ShouldBindUri(ptr); err != nil {
			return fmt.Errorf("bind path: %w", err)
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

type tagSurveyResult struct {
	uri, query, header, form, json bool
}

// surveyCache memoizes tagSurvey per args type. The survey is a pure
// function of the struct's tags but runs inside bindArgs — i.e. on
// every request — and struct-tag lookups are string scanning. Keyed
// by the app's registered args types, a finite set.
var surveyCache sync.Map // reflect.Type → tagSurveyResult

func surveyFor(t reflect.Type) tagSurveyResult {
	if v, ok := surveyCache.Load(t); ok {
		return v.(tagSurveyResult)
	}
	var sv tagSurveyResult
	sv.uri, sv.query, sv.header, sv.form, sv.json = tagSurvey(t)
	surveyCache.Store(t, sv)
	return sv
}

// tagSurvey walks one level of struct fields checking which binder families
// apply. If none are present we still try JSON on methods with bodies — the
// tag vocabulary is a hint, not a wall. Path params are recognized via the
// `path` tag.
func tagSurvey(t reflect.Type) (uri, query, header, form, json bool) {
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag
		if _, ok := tag.Lookup("path"); ok {
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

var httpHandlerFuncType = reflect.TypeOf(httpx.HandlerFunc(nil))

// isRestFactory reports whether fn builds a raw handler rather than being
// one: it takes only DI dependencies and returns an httpx.HandlerFunc.
//
//	func NewUpload(store *Store) httpx.HandlerFunc { … }
//	nexus.AsRest("POST", "/files", NewUpload)
//
// The factory runs once, at boot, so state it sets up is shared by every
// request — unlike a raw handler taking *httpx.Ctx, whose parameters are
// filled per call.
func isRestFactory(fn any) bool {
	t := reflect.TypeOf(fn)
	if t == nil || t.Kind() != reflect.Func || t.NumOut() != 1 || t.Out(0) != httpHandlerFuncType {
		return false
	}
	for i := 0; i < t.NumIn(); i++ {
		if t.In(i) == ginContextType {
			return false
		}
	}
	return true
}

// asRestFactory mounts the handler a factory builds once its dependencies
// resolve.
func asRestFactory(method, path string, cfg *restConfig, factory any) Option {
	rt := reflect.TypeOf(factory)
	in := make([]reflect.Type, 0, rt.NumIn()+1)
	in = append(in, reflect.TypeOf((*App)(nil)))
	depTypes := make([]reflect.Type, rt.NumIn())
	for i := 0; i < rt.NumIn(); i++ {
		depTypes[i] = rt.In(i)
		in = append(in, rt.In(i))
	}
	invoke := reflect.MakeFunc(reflect.FuncOf(in, nil, false), func(args []reflect.Value) []reflect.Value {
		app := args[0].Interface().(*App)
		deps := args[1:]
		service := resolveEndpointService(cfg.service, cfg.module, deps, depTypes, app)
		finalPath := app.PrefixPath(cfg.pathPrefix + path)
		// REST ops are "<METHOD> <path>": unique per route even when one
		// factory serves several.
		opName := method + " " + finalPath
		handler := reflect.ValueOf(factory).Call(deps)[0].Interface().(httpx.HandlerFunc)
		chain, mwNames := buildEndpointChain(app, service, service+"."+opName, string(registry.REST), opName,
			app.gateBundles(cfg.tags, cfg.bundles), handler)
		app.engine.Handle(method, finalPath, chain...)
		registerEndpoint(app, &cfg.baseEndpointConfig, service, registry.Endpoint{
			Name:       opName,
			Transport:  registry.REST,
			Method:     method,
			Path:       finalPath,
			Middleware: mwNames,
		})
		recordEndpointDeps(app, service, opName, deps, depTypes)
		return nil
	})
	return &restOption{o: di.Invoke(invoke.Interface()), cfg: cfg}
}
