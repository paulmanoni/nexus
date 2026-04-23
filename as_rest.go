package nexus

import (
	"fmt"
	"log"
	"net/http"
	"reflect"

	"github.com/gin-gonic/gin"
	"go.uber.org/fx"

	"github.com/paulmanoni/nexus/metrics"
	"github.com/paulmanoni/nexus/middleware"
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
	cfg := restConfig{}
	for _, o := range opts {
		o.applyToRest(&cfg)
	}
	sh, err := inspectHandler(fn)
	if err != nil {
		return rawOption{o: fx.Error(err)}
	}
	return asRestInvoke(method, path, cfg, sh)
}

type restConfig struct {
	description string
	service     string                  // optional explicit service name; auto-derived if empty
	bundles     []middleware.Middleware // attached via nexus.Use
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
func asRestInvoke(method, path string, cfg restConfig, sh handlerShape) Option {
	appType := reflect.TypeOf((*App)(nil))

	in := make([]reflect.Type, 0, len(sh.depTypes)+1)
	in = append(in, appType)
	in = append(in, sh.depTypes...)
	fnType := reflect.FuncOf(in, nil, false)

	invokeFn := reflect.MakeFunc(fnType, func(args []reflect.Value) []reflect.Value {
		app := args[0].Interface().(*App)
		deps := args[1:]

		service := cfg.service
		if service == "" {
			service = serviceNameFromDeps(deps, sh.depTypes)
		}

		opName := opNameFromFunc(sh.funcVal.Interface(), method+" "+path)
		handler := buildGinHandler(method, sh, deps, app.bus, service, path)

		// Stack any Use-attached middleware bundles in registration order
		// in front of the handler; the auto-attached metrics recorder
		// runs first so the counter captures every request regardless of
		// whether a later bundle aborts.
		chain := make([]gin.HandlerFunc, 0, len(cfg.bundles)+2)
		mwNames := make([]string, 0, len(cfg.bundles)+1)

		metricsBundle := metrics.NewMiddleware(app.metricsStore, service+"."+opName)
		chain = append(chain, metricsBundle.Gin)
		mwNames = append(mwNames, metricsBundle.Name)
		app.registry.RegisterMiddleware(metricsBundle.AsInfo())

		for _, b := range cfg.bundles {
			app.registry.RegisterMiddleware(b.AsInfo())
			mwNames = append(mwNames, b.Name)
			if b.Gin != nil {
				chain = append(chain, b.Gin)
			}
		}
		chain = append(chain, handler)
		app.engine.Handle(method, path, chain...)
		app.registry.RegisterEndpoint(registry.Endpoint{
			Service:     service,
			Name:        opName,
			Transport:   registry.REST,
			Method:      method,
			Path:        path,
			Description: cfg.description,
			Middleware:  mwNames,
		})
		return nil
	})
	return rawOption{o: fx.Invoke(invokeFn.Interface())}
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

		result, err := sh.callHandler(callInput{Ctx: c.Request.Context()}, deps, args)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
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
