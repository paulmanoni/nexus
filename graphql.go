package nexus

import (
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/go-viper/mapstructure/v2"
	"github.com/graphql-go/graphql"
	"github.com/paulmanoni/nexus/v2/di"
	"github.com/paulmanoni/nexus/v2/extension/ratelimit"
	"github.com/paulmanoni/nexus/v2/gql"
	"github.com/paulmanoni/nexus/v2/internal/graph"
	"github.com/paulmanoni/nexus/v2/middleware"
	"github.com/paulmanoni/nexus/v2/registry"
)

// AsQuery registers a GraphQL query from a plain Go handler — usually a
// method expression. The handler's signature is inspected reflectively:
//
//   - A *Service-wrapper parameter (e.g. *OrdersService, or the receiver)
//     grounds the op under that service and its GraphQL endpoint.
//   - Other parameters are DI-injected deps.
//   - The last parameter may be an args struct (or Params[T]). Field tags
//     drive the arguments:
//     graphql:"name"                 — arg name (defaults to the json name, then the lowercased field name)
//     graphql:"name,required"        — NonNull
//     graphql:"name,type=Status"     — a type declared with RegisterGqlType
//     validate:"required"            — required
//     validate:"len=3|120"           — string length 3..120
//     validate:"int=1|100"           — integer range 1..100
//     validate:"oneof=a|b|c"         — one of the listed values
//     Chain multiple rules with commas.
//   - Return type must be (T, error). T is the field's type; pointer and
//     slice wrappers are honored.
//
// The op name is the method's (or function's) name with its first rune
// lowercased ("ListOrders" → "listOrders"); override with nexus.Op.
//
//	nexus.AsQuery((*OrderService).ListOrders)
//	nexus.AsMutation((*OrderService).CreateOrder,
//	    nexus.GraphMiddleware("audit", "logs every resolve", Audit))
func AsQuery(fn any, opts ...GqlOption) Option {
	return asGqlField(fn, graph.FieldKindQuery, opts)
}

// AsMutation is the mutation analogue of AsQuery.
func AsMutation(fn any, opts ...GqlOption) Option {
	return asGqlField(fn, graph.FieldKindMutation, opts)
}

// AsSubscription is reserved: reflective GraphQL subscriptions are not
// implemented yet, and registering one fails boot. Push live updates over
// AsWS until they are.
func AsSubscription(fn any, opts ...GqlOption) Option {
	return rawOption{o: di.Error(fmt.Errorf("nexus: AsSubscription is not implemented yet — push updates with AsWS"))}
}

// GqlOption tunes a GraphQL registration. An interface (not a func type)
// so a single value — notably the nexus.Use cross-transport bundle — can
// satisfy both GqlOption and RestOption by implementing each's applyToX.
type GqlOption interface{ applyToGql(*gqlConfig) }

// gqlOptionFn is the ergonomic adaptor for one-off func-shaped options
// inside this package. Public helpers (Op, GraphMiddleware, etc.) return
// concrete structs so their type names survive in errors + godoc.
type gqlOptionFn func(*gqlConfig)

func (f gqlOptionFn) applyToGql(c *gqlConfig) { f(c) }

type gqlConfig struct {
	baseEndpointConfig
	opName            string
	middlewares       []namedMw
	deprecated        bool
	deprecationReason string
	// serviceType, when set, overrides the dep-scan for routing this op
	// onto a specific *Service wrapper. Use nexus.OnService[S]() on
	// handlers whose signature intentionally omits the service wrapper.
	serviceType reflect.Type

	// rateLimit, when set, declares the baseline rate limit for this
	// op. The auto-mount registers it with the app's Store and wires an
	// enforcement middleware. Operators can override the effective limit
	// live via the dashboard without touching code.
	rateLimit *ratelimit.Limit
}

// gqlFieldOption is the Option returned by AsQuery/AsMutation. It keeps
// a pointer to the shared gqlConfig so Module(...) can stamp the module
// name on it after the option is created — the constructor closure
// reads the same pointer at di.Start time, so stamping takes effect.
type gqlFieldOption struct {
	o   di.Option
	cfg *gqlConfig
}

func (g *gqlFieldOption) nexusOption() di.Option { return g.o }
func (g *gqlFieldOption) setModule(name string)  { g.cfg.module = name }

type namedMw struct {
	name, description string
	mw                gql.Middleware
}

// Op overrides the inferred op name.
func Op(name string) GqlOption {
	return gqlOptionFn(func(c *gqlConfig) { c.opName = name })
}

// GraphMiddleware attaches a named GraphQL-only middleware to the field.
// The name appears in the endpoint's middleware list on the dashboard (and
// "auth", "cors", etc. get labelled "builtin" via nexus/middleware.Builtins).
//
//	nexus.AsQuery((*UserService).ListUsers,
//	    nexus.GraphMiddleware("audit", "logs every resolve", Audit))
//
//	func Audit(next gql.Resolver) gql.Resolver {
//	    return func(f gql.Field) (any, error) { return next(f) }
//	}
//
// For cross-transport middleware, prefer nexus.Use(middleware.Middleware{...})
// — it accepts the same bundle on REST and GraphQL alike.
func GraphMiddleware(name, description string, mw gql.Middleware) GqlOption {
	return gqlOptionFn(func(c *gqlConfig) {
		c.middlewares = append(c.middlewares, namedMw{name, description, mw})
	})
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
//	nexus.AsMutation(NewCreateOrder,
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
//	nexus.AsQuery(NewListQuestions, nexus.OnService[*OrdersService]())
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

// asGqlField is the shared body: reflect → synthesize constructor → di.Provide.
func asGqlField(fn any, kind graph.FieldKind, opts []GqlOption) Option {
	// Pointer cfg so nexus.Module(...) can stamp cfg.module on us AFTER
	// this call returns but BEFORE di.Start runs the ctor closure below.
	// The closure captures cfg by reference, so late writes are picked up.
	cfg := &gqlConfig{}
	for _, o := range opts {
		o.applyToGql(cfg)
	}
	// Op name derives from the ORIGINAL handler — the Arg rewrite below
	// replaces it with an anonymous reflect.MakeFunc.
	if cfg.opName == "" {
		cfg.opName = opNameFromFunc(fn, string(kind))
	}
	sh, err := inspectHandlerArgs(fn, cfg.argNames)
	if err != nil {
		return rawOption{o: di.Error(err)}
	}
	if sh.returnType == nil {
		return rawOption{o: di.Error(fmt.Errorf("nexus: %s handler %s needs a (T, error) return", kind, sh.funcType))}
	}
	if cfg.envelope != nil {
		if err := cfg.envelope.check(sh.returnType); err != nil {
			return rawOption{o: di.Error(err)}
		}
		cfg.setTag(registry.EnvelopeTag, "true")
	}
	if err := checkBundleTransports(cfg.bundles, middleware.TransportGraphQL, cfg.opName); err != nil {
		return rawOption{o: di.Error(err)}
	}

	if !(kind == graph.FieldKindQuery || kind == graph.FieldKindMutation) {
		return rawOption{o: di.Error(fmt.Errorf("nexus: unsupported field kind %q", kind))}
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

	// The synthesized constructor returns a gqlField carrying everything
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
	outType := reflect.TypeOf(gqlField{})
	fnType := reflect.FuncOf(ctorInTypes, []reflect.Type{outType}, false)

	ctor := reflect.MakeFunc(fnType, func(allDeps []reflect.Value) []reflect.Value {
		// deps slice seen by the handler is the prefix matching its own
		// sh.depTypes; the extra trailing slot (when present) is the
		// service instance fx resolved on our behalf.
		deps := allDeps[:len(sh.depTypes)]
		// With an Envelope, the schema declares the wrap's output type —
		// the envelope is the wire contract, so introspection and the
		// generated SDK must describe it, not the handler's inner type.
		retElem := sh.returnElementType()
		if cfg.envelope != nil {
			retElem = cfg.envelope.outElem()
		}
		r := graph.NewResolverFromType(cfg.opName, retElem)
		if cfg.description != "" {
			r.WithDescription(cfg.description)
		}
		// Args from struct tags. inputFieldName is non-empty when
		// applyArgsFromStruct detected the single-input-object shape; the
		// resolver closure then reads p.Args[name] as a nested map rather
		// than flat fields.
		var inputFieldName string
		if sh.hasArgs {
			inputFieldName = applyArgsFromStruct(r, sh.argsType)
		}
		r.WithErrorMapper(graphqlError)
		// Deny-by-default gate (if an extension installed one and this
		// field isn't Public()) runs ahead of the field's own middleware
		// so the identity check precedes any permission check.
		if gateApp := allDeps[appInjectedIdx].Interface().(*App); gateApp != nil {
			if gate := gateApp.defaultGraphGate(cfg.tags); gate != nil {
				r.WithNamedMiddleware(gate.Name, gate.AsInfo().Description, graph.Adapt(gate.Graph))
			}
		}
		for _, m := range cfg.middlewares {
			r.WithNamedMiddleware(m.name, m.description, graph.Adapt(m.mw))
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
						return nil, bindError(err)
					}
				} else if err := bindGqlArgs(argsPtr.Interface(), p.Args); err != nil {
					return nil, bindError(err)
				}
				if err := validateValue(argsPtr); err != nil {
					return nil, err
				}
				argsVal = argsPtr.Elem()
			}
			res, err := capturedSh.callHandler(callInput{
				Ctx:    p.Context,
				Source: p.Source,
				Info:   p.Info,
			}, deps, argsVal)
			if env := cfg.envelope; env != nil {
				return env.apply(res, err)
			}
			return res, err
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
		entry := gqlField{
			Kind:        kind,
			ServiceType: svcType,
			Service:     svc,
			Module:      cfg.module,
			Field:       built,
			DepTypes:    sh.depTypes,
			Deps:        append([]reflect.Value(nil), deps...),
			RateLimit:   cfg.rateLimit,
			ArgsType:    sh.argsType,
			ReturnType:  entryReturnType(sh, cfg.envelope),
			Tags:        cfg.tags,
		}
		return []reflect.Value{reflect.ValueOf(entry)}
	})

	return &gqlFieldOption{
		o: di.Provide(
			di.Annotate(ctor.Interface(), di.ResultTags(`group:"`+gqlFieldGroup+`"`)),
		),
		cfg: cfg,
	}
}

// gqlField is the shared-group payload that AsQuery / AsMutation produce and
// fxmod's auto-mount Invoke consumes. Exported so consumers building their
// own mount logic can see what's in the graph, but most users never touch it.
type gqlField struct {
	Kind        graph.FieldKind
	ServiceType reflect.Type
	Service     *Service        // nil if dep[0] didn't unwrap (misuse)
	Module      string          // nexus.Module name this field was declared under; "" if unscoped
	Field       any             // graph.QueryField or graph.MutationField
	DepTypes    []reflect.Type  // for resource auto-attach
	Deps        []reflect.Value // for resource auto-attach (NexusResourceProvider)
	// RateLimit is the baseline rate limit this op declared. Auto-mount
	// publishes it to the registry so the dashboard can render it and
	// — once operator overrides land — show the effective limit beside
	// the declared one.
	RateLimit *ratelimit.Limit
	// ArgsType / ReturnType are the reflect.Type of the handler's args
	// struct + return value, captured at AsQuery/AsMutation time. The
	// auto-mount walks them into registry.TypeRef structures and
	// stamps the result on the registered Endpoint via
	// registry.SetEndpointSchema. Powers the client SDK's TS codegen
	// alongside REST + WS.
	//
	// Either may be nil — handlers that take no args / return only
	// error pass through as the SDK's "no schema" signal.
	ArgsType   reflect.Type
	ReturnType reflect.Type
	// Tags carries registry.Endpoint.Tags entries declared via
	// option helpers (e.g. nexus.AuthRoute → "auth.flow"). Forwarded
	// through the auto-mount so the registered endpoint surfaces the
	// same tags REST endpoints receive via registerEndpoint, keeping
	// the SDK manifest's auth-flow discovery transport-agnostic.
	Tags map[string]string
}

// gqlFieldGroup is the single fx value-group name every reflective GraphQL
// registration feeds. fxmod's auto-mount Invoke reads this group, partitions
// entries by ServiceType, and mounts one schema per service.
const gqlFieldGroup = "nexus.graph.fields"

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
				scope = ClientIP(p.Context)
			}
			if ok, retry := store.Allow(p.Context, ratelimit.GlobalKey, scope); !ok {
				return nil, Errf(TooMany, "global rate limit exceeded — retry after %s", retry.Round(10_000_000))
			}
			if ok, retry := store.Allow(p.Context, key, scope); !ok {
				return nil, Errf(TooMany, "rate limit exceeded — retry after %s", retry.Round(10_000_000))
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
	if t.Kind() != reflect.Pointer {
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
		r.WithInputObjectFieldName(name)
		nullable := isInputObjectNullable(argsType)
		if nullable {
			r.WithInputObjectNullable()
		}
		// Build (and memoize) the InputObject in our shared registry so that
		// the SAME named SDL type is reused if another resolver references
		// this struct via a flat-args field (where goTypeToGraphQL also
		// routes through buildInputObjectForType). Without sharing, the two
		// paths produce distinct *graphql.InputObject instances with the
		// same name and the schema build fails with "multiple types named X".
		gqlType := buildInputObjectForType(inner)
		if gqlType != nil {
			if nullable {
				r.WithArg(name, gqlType)
			} else {
				r.WithArgRequired(name, gqlType)
			}
		} else {
			// Fallback: let go-graph auto-generate (used only when the type
			// has no exported fields the registry can map).
			r.WithInputObject(reflect.New(inner).Elem().Interface())
		}
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
		var gqlType graphql.Input
		if override := parseGraphQLTagType(f); override != "" {
			if t, ok := lookupNamedType(override); ok {
				gqlType = t
			}
		}
		if gqlType == nil {
			gqlType = goTypeToGraphQL(f.Type)
		}
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

// isInputObjectNullable reports whether the input-object wrapper's single
// exported field is a pointer type — in which case the auto-generated SDL
// arg should be nullable. Mirrors detectInputObject's field-discovery loop;
// callers should only invoke this after detectInputObject returns ok=true.
func isInputObjectNullable(argsType reflect.Type) bool {
	if argsType.Kind() != reflect.Struct {
		return false
	}
	for i := 0; i < argsType.NumField(); i++ {
		f := argsType.Field(i)
		if f.PkgPath != "" {
			continue
		}
		return f.Type.Kind() == reflect.Pointer
	}
	return false
}

// detectInputObject returns (argName, innerType, true) when argsType is the
// single-struct-field shape — that is, exactly one exported field whose
// type is a non-Params struct. Anonymous wrapper structs like
// `struct{ Input CreateOrderArgs }` are the canonical form.
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
	if ft.Kind() == reflect.Pointer {
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
// missing or "-", the field is skipped. Empty name falls back to the
// JSON tag name (so a single `json:"id"` is enough to serve both REST
// and GraphQL), then to the Go field name with the first rune
// lowercased.
//
// The JSON-fallback path matters specifically for all-caps acronym
// fields like ID/URL/API: lowerFirst("ID") yields the awkward "iD",
// while the json tag is invariably "id" — so json wins when both
// would otherwise produce arg names a GraphQL caller wouldn't guess.
func parseGraphQLTag(f reflect.StructField) (name string, required bool) {
	tag := f.Tag.Get("graphql")
	if tag == "-" {
		return "", false
	}
	parts := strings.Split(tag, ",")
	name = strings.TrimSpace(parts[0])
	if name == "" {
		if jsonName := jsonFieldName(f); jsonName != "" {
			name = jsonName
		} else {
			name = lowerFirst(f.Name)
		}
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

// parseGraphQLTagType reads an optional `type=Name` segment from the graphql
// tag. Used to opt fields into a pre-registered named GraphQL type (e.g. a
// custom enum) instead of the reflectively-derived default.
func parseGraphQLTagType(f reflect.StructField) string {
	tag := f.Tag.Get("graphql")
	for _, p := range strings.Split(tag, ",") {
		p = strings.TrimSpace(p)
		if strings.HasPrefix(p, "type=") {
			return strings.TrimPrefix(p, "type=")
		}
	}
	return ""
}

// namedTypeRegistry stores opt-in named GraphQL input types (enums, input
// objects) so args can reference them by name via a
// `graphql:"...,type=Foo"` struct tag.
var namedTypeRegistry sync.Map // map[string]graphql.Input

// RegisterGqlType declares Go type T as the GraphQL input type name, which
// an args field opts into with a `graphql:"field,type=Name"` tag.
//
// With values, T must be a string or integer type and becomes an enum whose
// members are the values (each member is named fmt.Sprint(v), and binds
// back to v):
//
//	type Status string
//	nexus.RegisterGqlType("Status", Status("active"), Status("archived"))
//
//	type ListArgs struct {
//	    Status Status `graphql:"status,type=Status"`
//	}
//
// Without values, T must be a struct and becomes an input object named
// name, its fields mapped like an args struct's. Call it once at startup;
// re-registering a name replaces the earlier type. It panics when T can't
// be represented.
func RegisterGqlType[T any](name string, values ...T) {
	if name == "" {
		panic("nexus.RegisterGqlType: empty name")
	}
	t := reflect.TypeFor[T]()
	if len(values) > 0 {
		switch t.Kind() {
		case reflect.String, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		default:
			panic(fmt.Sprintf("nexus.RegisterGqlType(%q): enum values need a string or integer type, got %s", name, t))
		}
		members := graphql.EnumValueConfigMap{}
		for _, v := range values {
			members[fmt.Sprint(v)] = &graphql.EnumValueConfig{Value: v}
		}
		namedTypeRegistry.Store(name, graphql.NewEnum(graphql.EnumConfig{Name: name, Values: members}))
		return
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		panic(fmt.Sprintf("nexus.RegisterGqlType(%q): %s is not a struct; pass enum values for a scalar type", name, t))
	}
	namedTypeRegistry.Store(name, buildInputObject(t, name, nil))
}

func lookupNamedType(name string) (graphql.Input, bool) {
	if name == "" {
		return nil, false
	}
	v, ok := namedTypeRegistry.Load(name)
	if !ok {
		return nil, false
	}
	return v.(graphql.Input), true
}

// jsonFieldName extracts the wire name from a `json:"…"` tag. Returns
// "" when the tag is absent, "-", or empty — letting callers fall back
// to whatever default they prefer.
func jsonFieldName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "" || tag == "-" {
		return ""
	}
	if comma := strings.Index(tag, ","); comma >= 0 {
		tag = tag[:comma]
	}
	return strings.TrimSpace(tag)
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
	case strings.HasPrefix(rule, "int="):
		min, max := parseIntBounds(strings.TrimPrefix(rule, "int="))
		v := graph.IntRange(min, max)
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
	// doesn't break builds here. Checks a tag can't express belong in the
	// handler, returning nexus.Invalid().
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

// parseIntBounds reads "min|max" for int=; an empty side is unbounded.
func parseIntBounds(s string) (int, int) {
	min, max := math.MinInt, math.MaxInt
	lo, hi, ok := strings.Cut(s, "|")
	if !ok {
		return min, max
	}
	if n, err := strconv.Atoi(strings.TrimSpace(lo)); err == nil {
		min = n
	}
	if n, err := strconv.Atoi(strings.TrimSpace(hi)); err == nil {
		max = n
	}
	return min, max
}

// goTypeToGraphQL maps a Go arg-struct field type to a graphql.Input. Pointer
// wrappers are unwrapped (nullability comes from the required flag, not from
// `*T`). Unsupported types return nil, causing the field to be skipped.
func goTypeToGraphQL(t reflect.Type) graphql.Input {
	for t.Kind() == reflect.Pointer {
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
	case reflect.Struct:
		// Build (and memoize) a GraphQL InputObject for this struct so flat-args
		// resolvers can declare nested object arguments. Without this, fields
		// like `searchDataDto: SearchDataDto` would be silently dropped.
		return buildInputObjectForType(t)
	}
	// Maps, interfaces, chans — not mapped; such fields are skipped.
	return nil
}

// inputObjectRegistry deduplicates InputObject types across resolvers so
// the same Go struct reused as an arg in multiple fields produces one named
// GraphQL type (which is what graphql-go's schema builder requires).
var inputObjectRegistry sync.Map // map[reflect.Type]*graphql.InputObject

func buildInputObjectForType(t reflect.Type) graphql.Input {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	if cached, ok := inputObjectRegistry.Load(t); ok {
		return cached.(*graphql.InputObject)
	}
	// Suffix the name with "Input" so it can coexist with an output type
	// named the same — GraphQL requires distinct names per type-kind.
	name := t.Name()
	if name == "" {
		return nil
	}
	if !strings.HasSuffix(name, "Input") {
		name += "Input"
	}
	return buildInputObject(t, name, func(stub *graphql.InputObject) { inputObjectRegistry.Store(t, stub) })
}

// buildInputObject builds struct type t as an input object named name.
// register receives the empty object before its fields are built, so a
// field referring back to t resolves to it.
func buildInputObject(t reflect.Type, name string, register func(*graphql.InputObject)) *graphql.InputObject {
	stub := graphql.NewInputObject(graphql.InputObjectConfig{
		Name:   name,
		Fields: graphql.InputObjectConfigFieldMap{},
	})
	if register != nil {
		register(stub)
	}

	fields := graphql.InputObjectConfigFieldMap{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue
		}
		if f.Anonymous {
			// Flatten embedded structs.
			embT := f.Type
			for embT.Kind() == reflect.Pointer {
				embT = embT.Elem()
			}
			if embT.Kind() == reflect.Struct {
				for j := 0; j < embT.NumField(); j++ {
					ef := embT.Field(j)
					if ef.PkgPath != "" {
						continue
					}
					name, _ := parseGraphQLTag(ef)
					if name == "" {
						continue
					}
					sub := goTypeToGraphQL(ef.Type)
					if sub == nil {
						continue
					}
					if _, exists := fields[name]; !exists {
						fields[name] = &graphql.InputObjectFieldConfig{Type: sub}
					}
				}
			}
			continue
		}
		name, _ := parseGraphQLTag(f)
		if name == "" {
			continue
		}
		sub := goTypeToGraphQL(f.Type)
		if sub == nil {
			continue
		}
		fields[name] = &graphql.InputObjectFieldConfig{Type: sub}
	}
	for fname, fcfg := range fields {
		stub.AddFieldConfig(fname, fcfg)
	}
	return stub
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

// argBinding is the precomputed result of parseGraphQLTag for one field.
// Computed once per args struct type and reused on every request.
type argBinding struct {
	name     string
	fieldIdx int
	required bool
}

// argBindingsCache memoises compiled bindings per args struct type. Args
// structs are user types instantiated at registration and never change at
// runtime, so the cache fills once and is read-only thereafter.
var argBindingsCache sync.Map // reflect.Type → []argBinding

// getArgBindings returns the cached bindings for t, compiling on first use.
// Skips unexported fields and fields whose graphql tag is "-".
func getArgBindings(t reflect.Type) []argBinding {
	if v, ok := argBindingsCache.Load(t); ok {
		return v.([]argBinding)
	}
	if t.Kind() != reflect.Struct {
		argBindingsCache.Store(t, []argBinding(nil))
		return nil
	}
	n := t.NumField()
	bs := make([]argBinding, 0, n)
	for i := 0; i < n; i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue
		}
		name, required := parseGraphQLTag(f)
		if name == "" {
			continue
		}
		bs = append(bs, argBinding{name: name, fieldIdx: i, required: required})
	}
	actual, _ := argBindingsCache.LoadOrStore(t, bs)
	return actual.([]argBinding)
}

// bindGqlArgs assigns p.Args map entries into the args struct. Uses the
// graphql: tag for the source key and best-effort type conversion.
func bindGqlArgs(ptr any, args map[string]any) error {
	pv := reflect.ValueOf(ptr).Elem()
	for _, b := range getArgBindings(pv.Type()) {
		raw, ok := args[b.name]
		if !ok || raw == nil {
			continue
		}
		if err := assignArg(pv.Field(b.fieldIdx), raw); err != nil {
			return fmt.Errorf("bind %q: %w", b.name, err)
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
	// Map → struct (or *struct): GraphQL nested input objects arrive as
	// map[string]any. Decode via mapstructure using the graphql tag, the same
	// way bindInputObject handles single-input-object mode.
	if v.Kind() == reflect.Map {
		structDst := dst
		if dst.Kind() == reflect.Pointer {
			if dst.IsNil() {
				dst.Set(reflect.New(dst.Type().Elem()))
			}
			structDst = dst.Elem()
		}
		if structDst.Kind() == reflect.Struct {
			if err := decodeMap(raw, structDst.Addr().Interface()); err != nil {
				return err
			}
			return nil
		}
	}
	// Slice destination: GraphQL list args arrive as []interface{} regardless of
	// element type, so reflect-level AssignableTo/ConvertibleTo refuse them.
	// Build a typed slice and recurse per element so []int/[]*string/etc. work too.
	if dst.Kind() == reflect.Slice && v.Kind() == reflect.Slice {
		out := reflect.MakeSlice(dst.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			elem := v.Index(i)
			if elem.Kind() == reflect.Interface {
				elem = elem.Elem()
			}
			if !elem.IsValid() {
				continue
			}
			if err := assignArg(out.Index(i), elem.Interface()); err != nil {
				return fmt.Errorf("element %d: %w", i, err)
			}
		}
		dst.Set(out)
		return nil
	}
	// Pointer destination: wrap the raw value.
	if dst.Kind() == reflect.Pointer {
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
