package nexus

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/graphql-go/graphql"
	"github.com/mitchellh/mapstructure"
	"go.uber.org/fx"

	"github.com/paulmanoni/nexus/graph"
	"github.com/paulmanoni/nexus/middleware"
	"github.com/paulmanoni/nexus/ratelimit"
)

// AsQuery registers a GraphQL query from a plain Go handler. The handler's
// signature is inspected reflectively:
//
//   - First param should be the service wrapper (e.g. *AdvertsService).
//     Its type is used as the fx value-group key so MountGraphQL[*AdvertsService]
//     picks up this query.
//   - Subsequent params are fx-injected deps.
//   - Optional last param is an args struct. Field tags drive arg config:
//     graphql:"name"                 — arg name (defaults to lowercased field name)
//     graphql:"name,required"        — NonNull
//     validate:"required"            — graph.Required()
//     validate:"len=3|120"           — graph.StringLength(3, 120)
//     validate:"int=1|100"           — graph.Int(1, 100)
//     validate:"oneof=a|b|c"         — graph.OneOf("a","b","c")
//     Chain multiple rules with commas.
//   - Return type must be (T, error). T is the resolver's return; pointer
//     and slice wrappers are honored.
//
// Op name defaults to the handler's func name, stripping a leading "New"
// and lowercasing the first rune ("NewGetAllAdverts" → "getAllAdverts").
// Override with nexus.Op("explicit").
//
//	fx.Provide(
//	    nexus.AsQuery(NewGetAllAdverts),
//	    nexus.AsMutation(NewCreateAdvert,
//	        nexus.Middleware("auth", "Bearer token", AuthMw)),
//	)
func AsQuery(fn any, opts ...GqlOption) Option {
	return asGqlField(fn, graph.FieldKindQuery, opts)
}

// AsMutation is the mutation analogue of AsQuery.
func AsMutation(fn any, opts ...GqlOption) Option {
	return asGqlField(fn, graph.FieldKindMutation, opts)
}

// AsSubscription is reserved for a follow-up: subscriptions use a separate
// builder (SubscriptionResolver[T] with PubSub + channel plumbing) that we
// haven't taught the reflective path yet. Use graph.NewSubscriptionResolver
// directly for now; once the reflective SubscriptionResolverFromType exists
// this helper will mirror AsQuery/AsMutation.
func AsSubscription(fn any, opts ...GqlOption) Option {
	return rawOption{o: fx.Error(fmt.Errorf("nexus: AsSubscription not yet implemented — use graph.NewSubscriptionResolver directly"))}
}

// GqlOption tunes a GraphQL registration. An interface (not a func type)
// so a single value — notably the nexus.Use cross-transport bundle — can
// satisfy both GqlOption and RestOption by implementing each's applyToX.
type GqlOption interface{ applyToGql(*gqlConfig) }

// gqlOptionFn is the ergonomic adaptor for one-off func-shaped options
// inside this package. Public helpers (Op, Desc, Middleware, etc.) return
// concrete structs so their type names survive in errors + godoc.
type gqlOptionFn func(*gqlConfig)

func (f gqlOptionFn) applyToGql(c *gqlConfig) { f(c) }

type gqlConfig struct {
	opName            string
	description       string
	middlewares       []namedMw
	deprecated        bool
	deprecationReason string
	argValidators     map[string][]graph.Validator // extra, keyed by arg name
	// serviceType, when set, overrides the dep-scan for routing this op
	// onto a specific *Service wrapper. Use nexus.OnService[S]() on
	// handlers whose signature intentionally omits the service wrapper.
	serviceType reflect.Type

	// rateLimit, when set, declares the baseline rate limit for this
	// op. The auto-mount registers it with the app's Store and wires an
	// enforcement middleware. Operators can override the effective limit
	// live via the dashboard without touching code.
	rateLimit *ratelimit.Limit

	// bundles holds the full middleware.Middleware values attached via
	// nexus.Use — the registry uses AsInfo() from each to label the
	// endpoint's middleware list. The Graph realizations are already
	// copied into `middlewares` at option-apply time; bundles is just
	// for dashboard metadata.
	bundles []middleware.Middleware
}

type namedMw struct {
	name, description string
	mw                graph.FieldMiddleware
}

// Op overrides the inferred op name.
func Op(name string) GqlOption {
	return gqlOptionFn(func(c *gqlConfig) { c.opName = name })
}

// Desc sets the resolver's description (shown on the dashboard and in SDL
// documentation).
func Desc(s string) GqlOption {
	return gqlOptionFn(func(c *gqlConfig) { c.description = s })
}

// GraphMiddleware attaches a named graph-only middleware to the resolver.
// Equivalent to go-graph's WithNamedMiddleware — the name appears in
// FieldInfo.Middlewares for dashboard rendering (and "auth", "cors", etc.
// get labelled "builtin" via nexus/middleware.Builtins).
//
// For cross-transport middleware, prefer nexus.Use(middleware.Middleware{...})
// — it accepts the same bundle on REST and GraphQL alike.
func GraphMiddleware(name, description string, mw graph.FieldMiddleware) GqlOption {
	return gqlOptionFn(func(c *gqlConfig) {
		c.middlewares = append(c.middlewares, namedMw{name, description, mw})
	})
}

// Middleware is a deprecated alias for GraphMiddleware. Exists so existing
// call sites keep compiling while codebases migrate to nexus.Use for
// cross-transport middleware.
//
// Deprecated: use GraphMiddleware for graph-only middleware, or nexus.Use
// with a middleware.Middleware bundle for cross-transport.
func Middleware(name, description string, mw graph.FieldMiddleware) GqlOption {
	return GraphMiddleware(name, description, mw)
}

// Deprecated marks the field deprecated. The reason shows up in SDL and the
// dashboard "deprecated" badge.
func Deprecated(reason string) GqlOption {
	return gqlOptionFn(func(c *gqlConfig) {
		c.deprecated = true
		c.deprecationReason = reason
	})
}

// RateLimit declares a baseline rate limit for this op. The auto-mount
// registers it with the app's rate-limit store and wires an enforcement
// middleware that consults the store on every request. Operators can
// override the effective limit live from the dashboard — the declared
// baseline stays in source-of-truth, the override survives in the store.
//
//	nexus.AsMutation(NewCreateAdvert,
//	    nexus.RateLimit(ratelimit.Limit{RPM: 30, PerIP: true}),
//	)
//
// Burst defaults to RPM/6 when zero (10-second burst window). Set PerIP
// to true to scope the bucket to the caller's IP; leave false for a
// shared global bucket.
func RateLimit(l ratelimit.Limit) GqlOption {
	return gqlOptionFn(func(c *gqlConfig) { c.rateLimit = &l })
}

// OnService routes this registration onto the given service wrapper type
// without requiring the handler to take it as a dep. Use when the handler
// is minimal (`func NewListQuestions(q *QuestionsDB) (...)`) but still
// belongs to a particular service on the dashboard.
//
//	nexus.AsQuery(NewListQuestions, nexus.OnService[*AdvertsService]())
//
// The resolver still needs the owning service to have been provided into
// the fx graph elsewhere so MountGraphQL can pick up the field.
func OnService[S any]() GqlOption {
	var zero S
	t := reflect.TypeOf(zero)
	if t == nil {
		t = reflect.TypeOf((*S)(nil)).Elem()
	}
	return gqlOptionFn(func(c *gqlConfig) { c.serviceType = t })
}

// WithArgValidator adds one or more validators to a named arg, beyond what
// the struct tags declare. Useful for project-specific rules (graph.Custom
// validators that call into other code).
func WithArgValidator(arg string, vs ...graph.Validator) GqlOption {
	return gqlOptionFn(func(c *gqlConfig) {
		if c.argValidators == nil {
			c.argValidators = map[string][]graph.Validator{}
		}
		c.argValidators[arg] = append(c.argValidators[arg], vs...)
	})
}

// argsProvider is an optional interface a handler's args struct may
// implement to supply validators that tag vocabulary can't express.
// See graphapp example — `func (X) NexusValidators() map[string][]graph.Validator`.
type argsProvider interface {
	NexusValidators() map[string][]graph.Validator
}

// asGqlField is the shared body: reflect → synthesize constructor → fx.Provide.
func asGqlField(fn any, kind graph.FieldKind, opts []GqlOption) Option {
	sh, err := inspectHandler(fn)
	if err != nil {
		return rawOption{o: fx.Error(err)}
	}
	if sh.returnType == nil {
		return rawOption{o: fx.Error(fmt.Errorf("nexus: %s handler %s needs a (T, error) return", kind, sh.funcType))}
	}
	cfg := gqlConfig{}
	for _, o := range opts {
		o.applyToGql(&cfg)
	}
	if cfg.opName == "" {
		cfg.opName = opNameFromFunc(fn, string(kind))
	}

	if !(kind == graph.FieldKindQuery || kind == graph.FieldKindMutation) {
		return rawOption{o: fx.Error(fmt.Errorf("nexus: unsupported field kind %q", kind))}
	}

	// Resolve which service this op belongs to.
	//
	//   1. nexus.OnService[*Svc]() option — explicit.
	//   2. Scan deps for any *Service-embedding wrapper.
	//   3. Leave unresolved — the auto-mount will default to the single
	//      service registered in the app, or error if there are multiple.
	//
	// svcDepIdx is -1 when the service isn't in the dep list (OnService
	// or fully-unresolved case); we then unwrap the service instance
	// from a trailing fx-injected slot at call time.
	svcType := cfg.serviceType
	svcDepIdx := -1
	if svcType == nil {
		svcType, svcDepIdx = findServiceDepType(sh.depTypes)
	} else {
		for i, t := range sh.depTypes {
			if t == svcType {
				svcDepIdx = i
				break
			}
		}
	}

	// The synthesized constructor returns a GqlField carrying everything
	// the auto-mount Invoke needs: kind, service instance, the assembled
	// field, and the dep type list so resources can be auto-attached.
	//
	// If the service wrapper isn't among the handler's own deps AND we
	// knew the wrapper type at registration time (OnService case), append
	// it so fx resolves it for us. When the type is fully unknown, the
	// auto-mount defaults to the app's single service at start time.
	ctorInTypes := append([]reflect.Type(nil), sh.depTypes...)
	svcInjectedIdx := svcDepIdx
	if svcInjectedIdx < 0 && svcType != nil {
		svcInjectedIdx = len(ctorInTypes)
		ctorInTypes = append(ctorInTypes, svcType)
	}
	// Every op gets the rate-limit middleware so the global bucket is
	// always consulted — per-op declarations are additive. fx injects
	// *App via a trailing slot so the middleware has the store handle.
	appInjectedIdx := len(ctorInTypes)
	ctorInTypes = append(ctorInTypes, reflect.TypeOf((*App)(nil)))
	outType := reflect.TypeOf(GqlField{})
	fnType := reflect.FuncOf(ctorInTypes, []reflect.Type{outType}, false)

	ctor := reflect.MakeFunc(fnType, func(allDeps []reflect.Value) []reflect.Value {
		// deps slice seen by the handler is the prefix matching its own
		// sh.depTypes; the extra trailing slot (when present) is the
		// service instance fx resolved on our behalf.
		deps := allDeps[:len(sh.depTypes)]
		r := graph.NewResolverFromType(cfg.opName, sh.returnElementType())
		if cfg.description != "" {
			r.WithDescription(cfg.description)
		}
		// Args from struct tags + runtime NexusValidators() method.
		// inputFieldName is non-empty when applyArgsFromStruct detected the
		// single-input-object shape; the resolver closure then reads
		// p.Args[name] as a nested map rather than flat fields.
		var inputFieldName string
		if sh.hasArgs {
			inputFieldName = applyArgsFromStruct(r, sh.argsType)
			if provider, ok := reflect.New(sh.argsType).Elem().Interface().(argsProvider); ok {
				for arg, vs := range provider.NexusValidators() {
					r.WithArgValidator(arg, vs...)
				}
			}
		}
		for arg, vs := range cfg.argValidators {
			r.WithArgValidator(arg, vs...)
		}
		for _, m := range cfg.middlewares {
			r.WithNamedMiddleware(m.name, m.description, m.mw)
		}
		// Unwrap service so we have its name for metrics keying + any
		// downstream logic that needs it. May be nil when the handler
		// omitted the service wrapper (auto-mount fills it later).
		var svc *Service
		if svcInjectedIdx >= 0 {
			svc, _ = unwrapService(allDeps[svcInjectedIdx], svcType)
		}

		// Register bundle metadata so the dashboard's middleware list
		// includes the name even when a bundle has no Graph realization
		// (e.g. a gin-only request-id middleware).
		//
		// The metrics recorder is deliberately NOT attached here —
		// auto-routed ops don't know their service at registration
		// time, so we'd get empty keys. automount.go attaches it after
		// resolveUnresolved fills in the service name.
		if app := findAppInDeps(allDeps, appInjectedIdx); app != nil {
			for _, b := range cfg.bundles {
				app.registry.RegisterMiddleware(b.AsInfo())
			}
		}
		if cfg.deprecated {
			r.WithDeprecated(cfg.deprecationReason)
		}

		capturedSh := sh
		capturedArgsType := sh.argsType
		capturedInputName := inputFieldName
		r.WithRawResolver(func(p graph.ResolveParams) (any, error) {
			var argsVal reflect.Value
			if capturedSh.hasArgs {
				argsPtr := reflect.New(capturedArgsType)
				if capturedInputName != "" {
					if err := bindInputObject(argsPtr.Interface(), capturedInputName, p.Args); err != nil {
						return nil, err
					}
				} else if err := bindGqlArgs(argsPtr.Interface(), p.Args); err != nil {
					return nil, err
				}
				argsVal = argsPtr.Elem()
			}
			return capturedSh.callHandler(callInput{
				Ctx:    p.Context,
				Source: p.Source,
				Info:   p.Info,
			}, deps, argsVal)
		})

		var built any
		switch kind {
		case graph.FieldKindQuery:
			built = r.BuildQuery()
		case graph.FieldKindMutation:
			built = r.BuildMutation()
		}
		// Rate-limit middleware: once we have the app, wrap each resolver
		// with a pre-check that consults the store. Applied BEFORE
		// BuildQuery/Mutation so go-graph picks it up in the chain.
		if cfg.rateLimit != nil && appInjectedIdx >= 0 {
			app := allDeps[appInjectedIdx].Interface().(*App)
			svcName := ""
			if svc != nil {
				svcName = svc.Name()
			}
			attachRateLimitMiddleware(r, app, svcName, cfg.opName, *cfg.rateLimit)
		}
		entry := GqlField{
			Kind:        kind,
			ServiceType: svcType,
			Service:     svc,
			Field:       built,
			DepTypes:    sh.depTypes,
			Deps:        append([]reflect.Value(nil), deps...),
			RateLimit:   cfg.rateLimit,
		}
		return []reflect.Value{reflect.ValueOf(entry)}
	})

	return rawOption{o: fx.Provide(
		fx.Annotate(ctor.Interface(), fx.ResultTags(`group:"`+GqlFieldGroup+`"`)),
	)}
}

// GqlField is the shared-group payload that AsQuery / AsMutation produce and
// fxmod's auto-mount Invoke consumes. Exported so consumers building their
// own mount logic can see what's in the graph, but most users never touch it.
type GqlField struct {
	Kind        graph.FieldKind
	ServiceType reflect.Type
	Service     *Service        // nil if dep[0] didn't unwrap (misuse)
	Field       any             // graph.QueryField or graph.MutationField
	DepTypes    []reflect.Type  // for resource auto-attach
	Deps        []reflect.Value // for resource auto-attach (NexusResourceProvider)
	// RateLimit is the baseline rate limit this op declared. Auto-mount
	// publishes it to the registry so the dashboard can render it and
	// — once operator overrides land — show the effective limit beside
	// the declared one.
	RateLimit *ratelimit.Limit
}

// GqlFieldGroup is the single fx value-group name every reflective GraphQL
// registration feeds. fxmod's auto-mount Invoke reads this group, partitions
// entries by ServiceType, and mounts one schema per service.
const GqlFieldGroup = "nexus.graph.fields"

// findAppInDeps returns the *App reflectively injected at idx, or nil
// when the slot doesn't exist (defensive for paths that skip the App
// injection).
func findAppInDeps(deps []reflect.Value, idx int) *App {
	if idx < 0 || idx >= len(deps) {
		return nil
	}
	if a, ok := deps[idx].Interface().(*App); ok {
		return a
	}
	return nil
}

// WithClientIP is a thin pass-through to ratelimit.WithClientIP so nexus
// callers can thread IP into context without importing the lower-level
// ratelimit package. Kept here for API consistency with other nexus helpers.
func WithClientIP(ctx context.Context, ip string) context.Context {
	return ratelimit.WithClientIP(ctx, ip)
}

// ClientIPFromCtx pulls the IP stashed via WithClientIP (or
// ratelimit.WithClientIP). Empty when absent.
func ClientIPFromCtx(ctx context.Context) string {
	return ratelimit.ClientIPFromCtx(ctx)
}

// attachRateLimitMiddleware wires a rate-limit check onto a resolver. The
// middleware runs before the handler and enforces TWO buckets in order:
//
//  1. The global (app-wide) bucket "_global", if declared in Config.
//     Exhausting it denies any request regardless of endpoint.
//  2. The per-op bucket "<service>.<op>", declared via nexus.RateLimit.
//
// Either denial short-circuits with a graphql-native error describing the
// retry-after — clients see a coherent message rather than a generic 500.
func attachRateLimitMiddleware(r *graph.UnifiedResolver[any], app *App, service, op string, declared ratelimit.Limit) {
	if app == nil || app.rlStore == nil {
		return
	}
	key := service + "." + op
	app.rlStore.Declare(key, declared)

	store := app.rlStore
	mw := func(next graph.FieldResolveFn) graph.FieldResolveFn {
		return func(p graph.ResolveParams) (any, error) {
			// Global bucket first — a single shared ceiling across the
			// whole app. Scope matches the op bucket's scope so a
			// per-IP global limit still isolates callers consistently.
			scope := ""
			if declared.PerIP {
				scope = ClientIPFromCtx(p.Context)
			}
			if ok, retry := store.Allow(p.Context, ratelimit.GlobalKey, scope); !ok {
				return nil, fmt.Errorf("global rate limit exceeded — retry after %s", retry.Round(10_000_000))
			}
			if ok, retry := store.Allow(p.Context, key, scope); !ok {
				return nil, fmt.Errorf("rate limit exceeded — retry after %s", retry.Round(10_000_000))
			}
			return next(p)
		}
	}
	r.WithNamedMiddleware("rate-limit", fmt.Sprintf("%d rpm, burst %d%s",
		declared.RPM, declared.EffectiveBurst(),
		func() string {
			if declared.PerIP {
				return ", per-IP"
			}
			return ""
		}()), mw)
}

// findServiceDepType scans dep types for anything that looks like a
// nexus-style service wrapper — either *Service itself, or a pointer to
// a struct embedding *Service. Returns the first match; (nil, -1) when
// the handler has no service wrapper dep.
func findServiceDepType(depTypes []reflect.Type) (reflect.Type, int) {
	for i, t := range depTypes {
		if isServiceWrapperType(t) {
			return t, i
		}
	}
	return nil, -1
}

// isServiceWrapperType reports whether t is *Service or a pointer to a
// struct that embeds *Service. Used to identify which dep carries the
// service identity without requiring a marker interface.
func isServiceWrapperType(t reflect.Type) bool {
	svcPtr := reflect.TypeOf((*Service)(nil))
	if t == svcPtr {
		return true
	}
	if t.Kind() != reflect.Ptr {
		return false
	}
	inner := t.Elem()
	if inner.Kind() != reflect.Struct {
		return false
	}
	for i := 0; i < inner.NumField(); i++ {
		f := inner.Field(i)
		if f.Anonymous && f.Type == svcPtr {
			return true
		}
	}
	return false
}

// applyArgsFromStruct walks an args struct's fields and wires their
// graphql: / validate: tags into the resolver.
//
// Two shapes are recognised:
//
//  1. Flat args (default): each exported field becomes a top-level GraphQL
//     argument. Tag `graphql:"name,required"` controls name + nullability;
//     `validate:"…"` drives validators.
//
//  2. Single input-object: exactly one exported field whose type is itself
//     a struct. The field's name (lowercased) becomes the GraphQL arg name,
//     and the field's type becomes the SDL input type. Use when a mutation
//     has more than a couple of args — clients then pass one `input: { … }`
//     object instead of a long positional list.
//
// inputFieldName returns the GraphQL arg name when the input-object shape
// matched (empty string otherwise), so bindGqlArgs can route decoding.
func applyArgsFromStruct(r *graph.UnifiedResolver[any], argsType reflect.Type) (inputFieldName string) {
	if name, inner, ok := detectInputObject(argsType); ok {
		// Build a zero-value of the inner struct — WithInputObject uses
		// reflection on it to generate the SDL input type + mapstructure
		// decode incoming args.
		r.WithInputObjectFieldName(name)
		r.WithInputObject(reflect.New(inner).Elem().Interface())
		// Validators on the inner struct's fields still fire — go-graph
		// runs per-arg validators after the input object is decoded.
		return name
	}

	for i := 0; i < argsType.NumField(); i++ {
		f := argsType.Field(i)
		if f.PkgPath != "" { // unexported
			continue
		}
		name, required := parseGraphQLTag(f)
		if name == "" {
			continue
		}
		gqlType := goTypeToGraphQL(f.Type)
		if gqlType == nil {
			continue
		}
		if required {
			r.WithArgRequired(name, gqlType)
		} else {
			r.WithArg(name, gqlType)
		}
		if vs := parseValidateTag(f); len(vs) > 0 {
			r.WithArgValidator(name, vs...)
		}
	}
	return ""
}

// detectInputObject returns (argName, innerType, true) when argsType is the
// single-struct-field shape — that is, exactly one exported field whose
// type is a non-Params struct. Anonymous wrapper structs like
// `struct{ Input CreateAdvertArgs }` are the canonical form.
func detectInputObject(argsType reflect.Type) (name string, inner reflect.Type, ok bool) {
	if argsType.Kind() != reflect.Struct {
		return "", nil, false
	}
	var (
		exported reflect.StructField
		count    int
	)
	for i := 0; i < argsType.NumField(); i++ {
		f := argsType.Field(i)
		if f.PkgPath != "" {
			continue
		}
		exported = f
		count++
		if count > 1 {
			return "", nil, false
		}
	}
	if count != 1 {
		return "", nil, false
	}
	ft := exported.Type
	if ft.Kind() == reflect.Ptr {
		ft = ft.Elem()
	}
	if ft.Kind() != reflect.Struct {
		return "", nil, false
	}
	// Explicit tag override on the wrapper field takes precedence.
	argName, _ := parseGraphQLTag(exported)
	if argName == "" {
		argName = lowerFirst(exported.Name)
	}
	return argName, ft, true
}

// parseGraphQLTag reads `graphql:"name[,required]"`. When the tag is
// missing or "-", the field is skipped. Empty name falls back to the Go
// field name with the first rune lowercased.
func parseGraphQLTag(f reflect.StructField) (name string, required bool) {
	tag := f.Tag.Get("graphql")
	if tag == "-" {
		return "", false
	}
	parts := strings.Split(tag, ",")
	name = strings.TrimSpace(parts[0])
	if name == "" {
		name = lowerFirst(f.Name)
	}
	for _, p := range parts[1:] {
		if strings.TrimSpace(p) == "required" {
			required = true
		}
	}
	// Non-pointer + non-slice types also imply required by the business rule,
	// but we only auto-promote when the user explicitly asked via validate.
	return name, required
}

// parseValidateTag turns `validate:"required,len=3|120"` into concrete
// graph.Validator instances.
func parseValidateTag(f reflect.StructField) []graph.Validator {
	tag := f.Tag.Get("validate")
	if tag == "" {
		return nil
	}
	var out []graph.Validator
	for _, rule := range strings.Split(tag, ",") {
		rule = strings.TrimSpace(rule)
		if rule == "" {
			continue
		}
		if v := buildValidator(rule); v != nil {
			out = append(out, *v)
		}
	}
	return out
}

func buildValidator(rule string) *graph.Validator {
	switch {
	case rule == "required":
		v := graph.Required()
		return &v
	case strings.HasPrefix(rule, "len="):
		min, max := parseBounds(strings.TrimPrefix(rule, "len="))
		v := graph.StringLength(min, max)
		return &v
	case strings.HasPrefix(rule, "oneof="):
		vals := strings.Split(strings.TrimPrefix(rule, "oneof="), "|")
		anyVals := make([]any, len(vals))
		for i, s := range vals {
			anyVals[i] = s
		}
		v := graph.OneOf(anyVals...)
		return &v
	}
	// Unknown rules fall through silently so adding new rules in graph
	// doesn't break builds here. graph.Custom-based rules go through
	// NexusValidators() or nexus.WithArgValidator instead.
	return nil
}

// parseBounds reads "min|max", with either side being "-1" to skip.
func parseBounds(s string) (int, int) {
	parts := strings.Split(s, "|")
	if len(parts) != 2 {
		return -1, -1
	}
	min, _ := strconv.Atoi(parts[0])
	max, _ := strconv.Atoi(parts[1])
	return min, max
}

// goTypeToGraphQL maps a Go arg-struct field type to a graphql.Input. Pointer
// wrappers are unwrapped (nullability comes from the required flag, not from
// `*T`). Unsupported types return nil, causing the field to be skipped.
func goTypeToGraphQL(t reflect.Type) graphql.Input {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		return graphql.String
	case reflect.Bool:
		return graphql.Boolean
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return graphql.Int
	case reflect.Float32, reflect.Float64:
		return graphql.Float
	case reflect.Slice:
		inner := goTypeToGraphQL(t.Elem())
		if inner == nil {
			return nil
		}
		return graphql.NewList(inner)
	}
	// Structs, maps, interfaces, chans — not handled yet; users can use
	// WithArgValidator + a custom converter if needed.
	return nil
}

// bindInputObject handles the single-input-object shape: p.Args[argName]
// is a map[string]any (the GraphQL input type's fields); we decode it into
// the wrapper's single struct field via mapstructure, then clone it back
// into the wrapper's reflect-addressed field. The outer wrapper is
// typically `struct{ Input T }` so argsPtr is `*struct{Input T}`.
func bindInputObject(argsPtr any, argName string, args map[string]any) error {
	raw, ok := args[argName]
	if !ok || raw == nil {
		return nil
	}
	pv := reflect.ValueOf(argsPtr).Elem()
	pt := pv.Type()

	var target reflect.StructField
	found := false
	for i := 0; i < pt.NumField(); i++ {
		f := pt.Field(i)
		if f.PkgPath != "" {
			continue
		}
		target = f
		found = true
		break
	}
	if !found {
		return fmt.Errorf("nexus: input-object wrapper has no exported field")
	}

	// Allocate a fresh value of the inner struct type, decode map → struct.
	inner := reflect.New(target.Type).Interface()
	if err := decodeMap(raw, inner); err != nil {
		return fmt.Errorf("bind input %q: %w", argName, err)
	}
	pv.FieldByName(target.Name).Set(reflect.ValueOf(inner).Elem())
	return nil
}

// decodeMap is the map-to-struct fallback used by bindInputObject. We use
// mapstructure (already a transitive dep via nexus/graph) to honor the
// struct's field names; json-tagged fallback handled by the default.
func decodeMap(src, dst any) error {
	cfg := &mapstructure.DecoderConfig{
		Result:           dst,
		WeaklyTypedInput: true,
		TagName:          "graphql",
	}
	dec, err := mapstructure.NewDecoder(cfg)
	if err != nil {
		return err
	}
	return dec.Decode(src)
}

// bindGqlArgs assigns p.Args map entries into the args struct. Uses the
// graphql: tag for the source key and best-effort type conversion.
func bindGqlArgs(ptr any, args map[string]any) error {
	pv := reflect.ValueOf(ptr).Elem()
	pt := pv.Type()
	for i := 0; i < pt.NumField(); i++ {
		f := pt.Field(i)
		if f.PkgPath != "" {
			continue
		}
		name, _ := parseGraphQLTag(f)
		if name == "" {
			continue
		}
		raw, ok := args[name]
		if !ok || raw == nil {
			continue
		}
		if err := assignArg(pv.Field(i), raw); err != nil {
			return fmt.Errorf("bind %q: %w", name, err)
		}
	}
	return nil
}

func assignArg(dst reflect.Value, raw any) error {
	v := reflect.ValueOf(raw)
	if v.Type().AssignableTo(dst.Type()) {
		dst.Set(v)
		return nil
	}
	if v.Type().ConvertibleTo(dst.Type()) {
		dst.Set(v.Convert(dst.Type()))
		return nil
	}
	// Pointer destination: wrap the raw value.
	if dst.Kind() == reflect.Ptr {
		elemType := dst.Type().Elem()
		if v.Type().ConvertibleTo(elemType) {
			ptr := reflect.New(elemType)
			ptr.Elem().Set(v.Convert(elemType))
			dst.Set(ptr)
			return nil
		}
	}
	return fmt.Errorf("cannot assign %s to %s", v.Type(), dst.Type())
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}
