package nexus

import (
	"fmt"
	"reflect"

	"github.com/paulmanoni/nexus/v2/di"
	"github.com/paulmanoni/nexus/v2/extension/metrics"
	"github.com/paulmanoni/nexus/v2/httpx"
	"github.com/paulmanoni/nexus/v2/middleware"
	"github.com/paulmanoni/nexus/v2/registry"
	"github.com/paulmanoni/nexus/v2/trace"
)

// EndpointOption is the cross-transport per-op option contract: one value
// that every endpoint builder accepts — AsRest, AsQuery /
// AsMutation, and AsWS. It exists so "works on any transport" has a name
// instead of being an unwritten convention you infer from the fact that a
// type happens to implement RestOption, GqlOption, and WSOption all at once.
//
// The framework's built-in cross-transport options satisfy it: Public(),
// Describe(), WithIcon(), HideFromDashboard(), and Use() (hence
// auth.Required() / auth.Requires(), which return MiddlewareOption). The
// compile-time assertions below enforce that — add any new cross-transport
// option to the list so a regression that drops one transport fails to
// build rather than silently narrowing the option.
//
// Write your own cross-transport option by implementing the three
// applyTo* methods and returning EndpointOption:
//
//	func WithAudit(tag string) nexus.EndpointOption { return auditOption{tag} }
//
// Transport-specific options deliberately do NOT satisfy this — e.g. the
// GraphQL-only OnService returns GqlOption and is rejected by AsRest/AsWS at
// compile time, which is the intended guard.
type EndpointOption interface {
	RestOption
	GqlOption
	WSOption
}

// Compile-time proof that every built-in cross-transport option is a full
// EndpointOption. If a future edit drops (say) applyToWS from one of these,
// the build breaks here — the cheapest possible regression test.
var (
	_ EndpointOption = PublicOption{}
	_ EndpointOption = DescribeOption{}
	_ EndpointOption = IconOption{}
	_ EndpointOption = DashboardHiddenOption{}
	_ EndpointOption = MiddlewareOption{}
	_ EndpointOption = AuthRouteOption{}
)

// baseEndpointConfig holds the fields every transport-specific config
// (gqlConfig, restConfig, wsConfig) shares: dashboard description, the
// enclosing module name, and the cross-transport middleware bundles
// attached via nexus.Use.
//
// Transport configs embed it so adding a new shared field — a tag map,
// an audit hook, etc. — is a one-line change instead of three. The
// setModule method below also satisfies the moduleAnnotator interface
// for free, so each transport's option struct only needs to delegate.
type baseEndpointConfig struct {
	// description is the human-readable string shown on the dashboard
	// and (where the transport supports it) in generated SDL.
	description string

	// module is stamped by nexus.Module("name", ...) when this option
	// is a direct child of a module. Populates the registry entry's
	// Module field so the dashboard groups endpoints by module.
	module string

	// bundles holds the full middleware.Middleware values attached via
	// nexus.Use — the registry uses AsInfo() from each to label the
	// endpoint's middleware list. Per-transport realizations (Gin,
	// Graph) are extracted at apply time; this slice is the canonical
	// metadata source for the dashboard.
	bundles []middleware.Middleware

	// tags accumulate registry.Endpoint.Tags entries via options like
	// nexus.AuthRoute (sets "auth.flow" → "login" | "logout" | "me").
	// Surfaced on the dashboard + the client SDK manifest. Lazy-init
	// inside the option setter so endpoints with no tags pay nothing.
	tags map[string]string

	// argNames, from nexus.Arg, name the wire arguments of a handler
	// whose trailing parameters are bare scalars; the registration
	// rewrites the handler around a synthesized args struct before
	// inspection (see adaptScalarArgs).
	argNames []string

	// envelope, when set via nexus.Envelope, pipes the handler's
	// (result, error) through an app-supplied wrap function before the
	// wire write — GraphQL declares the wrap's output type in the
	// schema. The wrap's shape is compiler-checked (Envelope is generic);
	// only the T-vs-handler-return match is verified at registration.
	envelope *envelopeSpec
}

func (b *baseEndpointConfig) setModule(name string) { b.module = name }

// setTag records one registry tag, lazily allocating the map so
// endpoints with no tags pay nothing. The shared body behind Public /
// HideFromDashboard / WithIcon / AuthRoute.
func (b *baseEndpointConfig) setTag(key, value string) {
	if b.tags == nil {
		b.tags = map[string]string{}
	}
	b.tags[key] = value
}

// resolveEndpointService picks the service name a REST or WebSocket
// endpoint registers under. Priority:
//
//  1. explicit — set via a per-endpoint option (reserved for future use).
//  2. The first *Service-wrapper dep in the handler's deps — same
//     convention AsQuery / AsMutation use, just resolved into a name
//     instead of a value-group key.
//  3. module — the enclosing nexus.Module name. Catches the common
//     "REST/WS handler has no service-wrapper dep" case so metrics
//     events still carry a non-empty service.
//  4. defaultServiceName — ultimate fallback for handlers outside any
//     module. Registers the default service on the app so the
//     registry stays consistent.
//
// AsQuery / AsMutation route by service *type* via the fx value-group
// (see asGqlField), not by name, so they don't go through this helper.
func resolveEndpointService(explicit, module string, deps []reflect.Value, depTypes []reflect.Type, app *App) string {
	if explicit != "" {
		return explicit
	}
	if svc := serviceNameFromDeps(deps, depTypes); svc != "" {
		return svc
	}
	if module != "" {
		app.registry.RegisterService(registry.Service{Name: module})
		return module
	}
	app.Service(defaultServiceName)
	return defaultServiceName
}

// registerEndpoint fills the shared fields (Service, Module, Deployment,
// Description) on e from cfg + service, then writes the entry to the
// app's registry. Each transport's mounting code supplies the
// transport-specific fields (Transport, Method, Path, Name, Middleware)
// on the entry it passes in. Centralizing the shared-field stamping
// means a future column (Tags, AuditHook, etc.) is one edit instead of
// three.
func registerEndpoint(app *App, cfg *baseEndpointConfig, service string, e registry.Endpoint) {
	e.Service = service
	e.Module = cfg.module
	e.Description = cfg.description
	if len(cfg.tags) > 0 {
		if e.Tags == nil {
			e.Tags = map[string]string{}
		}
		for k, v := range cfg.tags {
			e.Tags[k] = v
		}
	}
	app.registry.RegisterEndpoint(e)
}

// recordEndpointDeps writes the dep edges for a REST or WebSocket
// endpoint registration: aggregate service→resource attachments, plus
// per-endpoint resource and other-service lists. Identical for REST
// and WS — both walk the handler's deps for NexusResourceProvider
// implementations and *Service-wrapper types.
//
// AsQuery / AsMutation handle the same thing through automount.go
// (attachDeclaredResources + the pendingResources batch) because they
// can't know the service at registration time.
func recordEndpointDeps(app *App, service, endpointName string, deps []reflect.Value, depTypes []reflect.Type) {
	attachEndpointResources(app, service, deps, depTypes)
	if resources := collectResourceNames(deps); len(resources) > 0 {
		app.registry.SetEndpointResources(service, endpointName, resources)
	}
	if svcDeps := collectServiceDeps(deps, depTypes, service); len(svcDeps) > 0 {
		app.registry.SetEndpointServiceDeps(service, endpointName, svcDeps)
	}
}

// recordEndpointSchema reflects the handler's args/return reflect.Type
// into structural TypeRef values and stamps them on the registered
// endpoint via the registry's SetEndpointSchema mutator. Powers the
// client SDK's TS codegen at GET /__nexus/client/manifest.json — every
// endpoint that goes through here surfaces its full request/response
// shape to the SDK without an external schema source.
//
// Named struct types (User, Pet, …) collect into the registry's
// shared refs pool so they appear once in the manifest even when N
// endpoints share them. Anonymous args structs are inlined.
//
// Called from AsRest and AsWS after registerEndpoint runs (so the
// endpoint exists for the mutator to find). The GraphQL path mirrors
// this from automount.go where the handlerShape is in scope.
func recordEndpointSchema(app *App, service, endpointName string, sh handlerShape) {
	refs := app.schemaRefs()
	var args, ret *registry.TypeRef
	if sh.argsType != nil {
		v := registry.WalkType(sh.argsType, refs)
		args = &v
	}
	if sh.returnType != nil {
		v := registry.WalkType(sh.returnType, refs)
		ret = &v
	}
	if args == nil && ret == nil {
		return
	}
	app.registry.SetEndpointSchema(service, endpointName, args, ret)
}

// attachEndpointResources attaches every NexusResourceProvider dep to
// the endpoint's service so the dashboard draws a service→resource
// edge for the aggregate relationship. Used by both REST and WS via
// recordEndpointDeps. The GraphQL path goes through automount.go's
// attachDeclaredResources, which does the same thing post-mount.
func attachEndpointResources(app *App, service string, deps []reflect.Value, _ []reflect.Type) {
	if service == "" {
		return
	}
	eachResourceProvider(deps, func(p NexusResourceProvider) {
		for _, r := range p.NexusResources() {
			app.registry.AttachResource(service, r.Name())
		}
	})
}

// buildEndpointChain assembles the standard Gin middleware chain shared by
// REST and WebSocket endpoint mounts: optional trace → metrics → user
// bundles → final handler. Each bundle is also registered with the app's
// registry, and the returned mwNames slice is the value to put on the
// resulting registry.Endpoint's Middleware field.
//
// Pass traceEndpoint == "" to skip the trace prefix. AsRest's reflective
// path uses that — it threads tracing inside its handler closure (see
// buildGinHandler) so the chain itself starts at metrics.
//
// Centralized here so a change to chain ordering, an extra always-on
// middleware, or a change to the registry recording shape is one edit
// instead of three.
func buildEndpointChain(
	app *App,
	service string,
	metricsKey string,
	transport string,
	traceEndpoint string,
	bundles []middleware.Middleware,
	handler httpx.HandlerFunc,
) (chain []httpx.HandlerFunc, mwNames []string) {
	chain = make([]httpx.HandlerFunc, 0, len(bundles)+3)
	mwNames = make([]string, 0, len(bundles)+1)

	if traceEndpoint != "" && app.bus != nil {
		chain = append(chain, trace.Middleware(app.bus, service, traceEndpoint, transport))
	}

	metricsBundle := metrics.NewMiddleware(app.metricsStore, metricsKey)
	chain = append(chain, metricsBundle.HTTP)
	mwNames = append(mwNames, metricsBundle.Name)
	app.registry.RegisterMiddleware(metricsBundle.AsInfo())

	for _, mw := range bundles {
		app.registry.RegisterMiddleware(mw.AsInfo())
		mwNames = append(mwNames, mw.Name)
		if mw.HTTP != nil {
			chain = append(chain, mw.HTTP)
		}
	}

	chain = append(chain, handler)
	return chain, mwNames
}

// Describe sets an endpoint's human-readable description — shown on the
// introspection dashboard (/__nexus) and, for GraphQL, emitted into the
// generated SDL documentation. Cross-transport: one expression works on REST
// (AsRest), GraphQL (AsQuery / AsMutation), and WS (AsWS),
// mirroring HideFromDashboard / WithIcon.
//
//	nexus.AsRest("POST", "/devices", NewRegister, nexus.Describe("Register a device"))
//	nexus.AsQuery(NewSearchUsers, nexus.Describe("Full-text user search"))
//	nexus.AsWS("/events", "chat.send", NewChatSend, nexus.Describe("Send a chat message"))
//
// Describe supersedes the transport-specific Desc (GraphQL) and Description
// (REST) helpers, which remain for compatibility.
func Describe(s string) DescribeOption { return DescribeOption{text: s} }

// DescribeOption is the cross-transport carrier returned by Describe — it
// implements RestOption, GqlOption, and WSOption so one value flows through any
// endpoint builder, mirroring IconOption and DashboardHiddenOption. It sets the
// shared baseEndpointConfig.description that registerEndpoint stamps onto the
// registry entry for every transport.
type DescribeOption struct{ text string }

func (o DescribeOption) applyToRest(c *restConfig) { c.description = o.text }
func (o DescribeOption) applyToGql(c *gqlConfig)   { c.description = o.text }
func (o DescribeOption) applyToWS(c *wsConfig)     { c.description = o.text }

// HideFromDashboard marks an endpoint as exempt from the introspection
// dashboard (/__nexus): it is dropped from /__nexus/endpoints, the live
// snapshot, and the architecture graph. The endpoint STILL routes and
// serves requests normally — this is dashboard-only visibility, not a 404
// and not an auth gate. Useful for internal/debug/health ops you don't want
// cluttering the topology. Works on REST (AsRest), GraphQL
// (AsQuery / AsMutation), and WS (AsWS):
//
//	nexus.AsRest("GET", "/internal/debug", NewDebug, nexus.HideFromDashboard())
//	nexus.AsQuery(NewInternalReport, nexus.HideFromDashboard())
//	nexus.AsWS("/events", "debug.tap", NewDebugTap, nexus.HideFromDashboard())
func HideFromDashboard() DashboardHiddenOption { return DashboardHiddenOption{} }

// DashboardHiddenOption is the cross-transport carrier returned by
// HideFromDashboard — implements RestOption, GqlOption, and WSOption so one
// expression flows through any endpoint builder, mirroring PublicOption.
// It stamps registry.HiddenTag on the endpoint's tags, which the registry's
// VisibleEndpoints() accessor (used by every dashboard data path) filters on.
type DashboardHiddenOption struct{}

func (DashboardHiddenOption) applyToRest(c *restConfig) { c.setTag(registry.HiddenTag, "true") }
func (DashboardHiddenOption) applyToGql(c *gqlConfig)   { c.setTag(registry.HiddenTag, "true") }
func (DashboardHiddenOption) applyToWS(c *wsConfig)     { c.setTag(registry.HiddenTag, "true") }

// WithIcon sets the dashboard icon for an endpoint — a lucide-style icon name
// rendered on the endpoint's node in the architecture graph and the endpoints
// list. Its main use is branding: an extension that registers endpoints through
// a custom decorator stamps its OWN icon so its endpoints are recognizable in
// the topology (e.g. inertia.Page → the inertia icon, widgets.Panel → a panel
// icon). Cross-transport: works on REST, GraphQL, and WS.
//
//	// inside an extension's decorator-form registrar:
//	decorate.Record(nexus.AsRest("GET", "/widgets"+path, ctor, nexus.WithIcon("layout-panel-top")))
//
// The endpoint still routes and serves normally; this is dashboard presentation
// only. An empty name is ignored (the per-transport default icon shows).
func WithIcon(name string) IconOption { return IconOption{name: name} }

// IconOption is the cross-transport carrier returned by WithIcon — implements
// RestOption, GqlOption, and WSOption so one expression flows through any
// endpoint builder, mirroring HideFromDashboard. It stamps registry.IconTag on
// the endpoint's tags, which the dashboard reads when rendering the node.
type IconOption struct{ name string }

func (o IconOption) applyToRest(c *restConfig) { o.apply(&c.baseEndpointConfig) }
func (o IconOption) applyToGql(c *gqlConfig)   { o.apply(&c.baseEndpointConfig) }
func (o IconOption) applyToWS(c *wsConfig)     { o.apply(&c.baseEndpointConfig) }

func (o IconOption) apply(b *baseEndpointConfig) {
	if o.name != "" {
		b.setTag(registry.IconTag, o.name)
	}
}

// PublicTag is the registry.Endpoint.Tags key set by Public(). When an
// extension installs a default endpoint gate (deny-by-default auth),
// endpoints carrying this tag are exempted from it.
const PublicTag = "auth.public"

// Public marks an endpoint as exempt from any framework-installed default
// gate — the explicit opt-out for deny-by-default auth
// (auth.Authorization.Default). With no default gate configured it's a
// harmless no-op marker. Works on REST (AsRest), GraphQL
// (AsQuery / AsMutation), and WS (AsWS):
//
//	nexus.AsRest("GET", "/health", NewHealth, nexus.Public())
//
// Login endpoints marked nexus.AuthRoute("login") are auto-exempted (you
// can't require auth to obtain auth), so they need no Public().
func Public() PublicOption { return PublicOption{} }

// PublicOption is the cross-transport carrier returned by Public —
// implements RestOption, GqlOption, and WSOption so one expression flows
// through any endpoint builder, mirroring AuthRouteOption / MiddlewareOption.
type PublicOption struct{}

func (PublicOption) applyToRest(c *restConfig) { c.setTag(PublicTag, "true") }
func (PublicOption) applyToGql(c *gqlConfig)   { c.setTag(PublicTag, "true") }
func (PublicOption) applyToWS(c *wsConfig)     { c.setTag(PublicTag, "true") }

// isPublicEndpoint reports whether an endpoint is exempt from the default
// gate: either explicitly via Public() or implicitly because it's a
// framework login flow (AuthRoute("login")) — gating those would make
// authentication impossible.
func isPublicEndpoint(tags map[string]string) bool {
	return tags[PublicTag] == "true" || tags[AuthFlowTag] == "login"
}

// Tag stamps one key/value pair onto an endpoint's registry tags — the
// metadata channel the dashboard, the client SDK manifest, and codegen read.
// It is the exported form of what WithIcon / HideFromDashboard / AuthRoute do
// internally, so an extension can mark the endpoints it registers without a
// dedicated option in this package:
//
//	// extension/inertia: mark a route as a page for the SDK's NexusPageProps
//	nexus.AsRest("GET", path, fn, nexus.Tag(registry.PageTag, component))
//
// Cross-transport (REST / GraphQL / WS). Keys are namespaced by convention
// ("<owner>.<name>", e.g. "inertia.page"); a later Tag with the same key
// replaces the earlier value. An empty key is ignored.
//
// The keys this package's own options own are refused (Tag panics at
// registration, naming the option): some of them are not just metadata —
// auth.public and auth.flow exempt a route from the deny-by-default gate,
// and an auth.requires that disagrees with the middleware would make
// auth.OpGates report an op the endpoint refuses. Use Public, AuthRoute,
// auth.Requires, HideFromDashboard, WithIcon or Envelope for those.
func Tag(key, value string) TagOption {
	if opt, reserved := reservedTags[key]; reserved {
		panic("nexus.Tag: " + key + " is set by " + opt + ", not by Tag")
	}
	return TagOption{key: key, value: value}
}

// reservedTags maps each tag key an option in this package owns to that
// option, for Tag's refusal message.
var reservedTags = map[string]string{
	PublicTag:                "nexus.Public()",
	AuthFlowTag:              "nexus.AuthRoute",
	registry.AuthRequiresTag: "auth.Requires",
	registry.HiddenTag:       "nexus.HideFromDashboard()",
	registry.IconTag:         "nexus.WithIcon",
	registry.EnvelopeTag:     "nexus.Envelope",
	registry.ProxyTag:        "extension/proxy",
}

// TagOption is the cross-transport carrier returned by Tag — implements
// RestOption, GqlOption, and WSOption so one expression flows through any
// endpoint builder, mirroring IconOption.
type TagOption struct{ key, value string }

func (o TagOption) applyToRest(c *restConfig) { o.apply(&c.baseEndpointConfig) }
func (o TagOption) applyToGql(c *gqlConfig)   { o.apply(&c.baseEndpointConfig) }
func (o TagOption) applyToWS(c *wsConfig)     { o.apply(&c.baseEndpointConfig) }

func (o TagOption) apply(b *baseEndpointConfig) {
	if o.key != "" {
		b.setTag(o.key, o.value)
	}
}

// RegisterSharedProp records the value type of a page-wide shared prop (an
// Inertia prop every page receives) so the client SDK can type it: t is
// walked into the same named-type pool endpoint schemas use — a named struct
// lands once in the manifest's refs — and the result is stored in the
// registry under key. The manifest projects the set as sharedProps and the
// generator emits it as NexusSharedProps. inertia.ShareScoped and
// inertia.ShareTyped call this; untyped providers (inertia.Share) are not
// recorded and ride NexusSharedProps' index signature.
//
// A nil t or empty key is ignored. Call during option wiring (an Invoke);
// the manifest is built after boot.
// RegisterIsland records a view island's props type (view.NewIsland does
// it at boot), so the client SDK types it as NexusIslandProps[name].
func (a *App) RegisterIsland(name string, t reflect.Type) {
	if name == "" || t == nil {
		return
	}
	a.registry.SetIsland(name, registry.WalkType(t, a.schemaRefs()))
}

func (a *App) RegisterSharedProp(key string, t reflect.Type) {
	if key == "" || t == nil {
		return
	}
	a.registry.SetSharedProp(key, registry.WalkType(t, a.schemaRefs()))
}

// AuthFlowTag is the registry.Endpoint.Tags key used by AuthRoute to
// mark which framework auth flow ("login" | "logout" | "me") an
// endpoint belongs to. The client SDK reads this tag to expose
// top-level auth.login() / auth.logout() / auth.me() calls so users
// don't have to know the underlying route shape — the framework
// surfaces it via convention without owning the handlers themselves.
const AuthFlowTag = "auth.flow"

// AuthRoute marks the endpoint as part of one of the framework-aware
// auth flows. Three values are recognized today:
//
//	"login"   endpoint that exchanges credentials for a token /
//	          session cookie. The SDK calls it via auth.login(creds).
//	"logout"  endpoint that revokes the active session. The SDK calls
//	          it via auth.logout() and clears its locally-stored
//	          token on success.
//	"me"      endpoint returning the current Identity. The SDK calls
//	          it via auth.me() to bootstrap an existing session on
//	          page load (cookie-based apps) or check token freshness
//	          (bearer apps).
//
// Apps stay in control of the actual handler — AuthRoute is purely a
// metadata marker. Works on both REST (AsRest) and GraphQL ops
// (AsQuery, AsMutation). The SDK auto-dispatches to the right
// transport based on the manifest's recorded transport tag:
//
//	nexus.AsRest("POST", "/api/login", NewLogin, nexus.AuthRoute("login"))
//	nexus.AsMutation(NewLogin, nexus.AuthRoute("login"))
//	nexus.AsQuery(NewMe, nexus.AuthRoute("me"))
//
// AuthRoute does not gate access — combine with auth.Optional() (for
// /me on bearer apps) or auth.Required() (for /logout) as needed.
//
// Unknown flow values are accepted but ignored by the SDK. The
// recognized set may grow in future framework versions; the tag is
// the contract.
func AuthRoute(flow string) AuthRouteOption {
	return AuthRouteOption{flow: flow}
}

// AuthRouteOption is the cross-transport carrier returned by
// AuthRoute. Implements both RestOption and GqlOption so the same
// expression can flow through AsRest, AsQuery, or AsMutation
// without callers caring which transport the endpoint lives on.
//
// Mirrors the pattern in nexus.Use / MiddlewareOption: a single
// concrete value satisfying multiple per-transport apply
// interfaces, with each transport reading what it needs.
type AuthRouteOption struct{ flow string }

func (a AuthRouteOption) applyToRest(c *restConfig) { c.setTag(AuthFlowTag, a.flow) }
func (a AuthRouteOption) applyToGql(c *gqlConfig)   { c.setTag(AuthFlowTag, a.flow) }
func (a AuthRouteOption) applyToWS(c *wsConfig)     { c.setTag(AuthFlowTag, a.flow) }

// EndpointGate is a middleware the framework applies to every endpoint by
// default — the primitive behind deny-by-default auth. An extension supplies
// one (e.g. auth's Authorization.Default = Authenticated()) and the framework
// prepends it to each endpoint's chain across REST, GraphQL, and WS unless
// the endpoint opts out with Public(). It lives here, not in the auth
// extension, so package nexus can honor the policy without importing the
// extension that configures it (auth imports nexus, never the reverse).
type EndpointGate struct {
	// Middleware is the gate. It should declare AllTransports so the same
	// value gates every transport; a transport it doesn't implement is
	// simply not gated.
	Middleware middleware.Middleware
}

// applyDefaultGate stashes a supplied EndpointGate on the app before any
// endpoint mounts. Registered in fxEarlyOptions so it runs ahead of the
// per-endpoint fx.Invokes (fx runs invokes in registration order, and the
// supplied gate is resolved from the graph regardless of where the
// extension that supplied it sits in the option list). The gate is
// optional: apps without deny-by-default supply nothing and pay nothing.
// The *EndpointGate parameter is optional — wired via di.ParamTags
// (`optional:"true"`) on the applyDefaultGate invoke in fxEarlyOptions — so
// apps that supply no gate resolve it to nil and pay nothing.
func applyDefaultGate(app *App, gate *EndpointGate) {
	app.defaultGate = gate
}

// gateBundles prepends the default endpoint gate ahead of an endpoint's own
// bundles, unless no gate is configured or the endpoint is exempt
// (Public() / a login flow). Used by the REST and WS chain builders, which
// share buildEndpointChain.
func (a *App) gateBundles(tags map[string]string, bundles []middleware.Middleware) []middleware.Middleware {
	if a.defaultGate == nil || isPublicEndpoint(tags) {
		return bundles
	}
	out := make([]middleware.Middleware, 0, len(bundles)+1)
	out = append(out, a.defaultGate.Middleware)
	return append(out, bundles...)
}

// defaultGraphGate returns the gate's GraphQL realization to prepend onto a
// field's middleware chain, or nil when no gate applies (none configured,
// endpoint exempt, or the gate has no GraphQL realization).
func (a *App) defaultGraphGate(tags map[string]string) *middleware.Middleware {
	if a.defaultGate == nil || isPublicEndpoint(tags) || a.defaultGate.Middleware.Graph == nil {
		return nil
	}
	return &a.defaultGate.Middleware
}

// ResponseRenderer overrides how a REST endpoint's *successful* return value
// is written to the response. By default an AsRest handler's return is encoded
// with c.JSON(...); attaching a renderer via WithRenderer hands that final
// write to custom code instead.
//
// This is the single extension point the Inertia integration
// (github.com/paulmanoni/nexus/v2/extension/inertia) builds on: a page handler
// stays an ordinary reflective handler returning a typed props struct, and the
// renderer wraps that struct into the Inertia page object — emitting JSON for
// XHR visits or an HTML document shell for full loads. Keeping the hook here
// (rather than re-implementing handler reflection in the extension) means
// params binding, validation, DI, tracing, and metrics behave identically to
// every other REST endpoint.
//
// Render receives the live *httpx.Ctx (headers, request URL, the
// ResponseWriter) and the handler's return value. Returning an error is
// reported through gin.Context.Error and, if nothing has been written yet,
// produces a 500 — same as a handler error. Only successful returns reach a
// renderer; the error path is untouched.
type ResponseRenderer interface {
	Render(c *httpx.Ctx, result any) error
}

// ErrorRenderer is an optional companion to ResponseRenderer: when a renderer
// also implements it, the framework offers handler *errors* to it before the
// default error write. This is how Inertia turns a handler that returns
// inertia.Redirect(...) / inertia.Location(...) into a 303/409 redirect rather
// than a JSON 500.
//
// RenderError reports handled=true when it took ownership of the response (the
// default error write, trace error-wrapping, and status mapping are then all
// skipped — a redirect is not an error to log). Returning handled=false leaves
// the error to the standard path unchanged. A non-nil error return is treated
// like a renderer failure (500 if nothing was written).
type ErrorRenderer interface {
	RenderError(c *httpx.Ctx, err error) (handled bool, rerr error)
}

// EmptyRenderer is an optional companion to ResponseRenderer for handlers
// that return only an error: on success the framework calls RenderEmpty
// instead of writing a bare status. Inertia's resource actions use it so a
// Destroy that returns just an error still redirects.
type EmptyRenderer interface {
	RenderEmpty(c *httpx.Ctx) error
}

// WithRenderer attaches a ResponseRenderer to a single AsRest registration,
// replacing the default JSON success write. It is a REST-only option (GraphQL
// and WebSocket returns are encoded by their own transports).
//
//	nexus.AsRest("GET", "/users", NewListUsers, nexus.WithRenderer(myRenderer))
//
// Most apps never call this directly — higher-level helpers such as
// inertia.Page wire the renderer for you.
func WithRenderer(r ResponseRenderer) RestOption {
	return restOptionFn(func(c *restConfig) { c.renderer = r })
}

// Envelope declares a response envelope for one endpoint: a function the
// framework pipes the handler's (result, error) through before the wire
// write, so handlers (and service methods registered directly) return plain
// (T, error) while the API keeps a custom success/failure shape.
//
// wrap is `func(T, error) (W, error)` — T assignable from the handler's
// result type, W the wire type — checked by the COMPILER, and invoked
// directly per request (a typed closure, not reflection; the generic
// signature is what keeps the envelope off the reflect.Call path). The app
// owns the shape; instantiate a generic helper per registration:
//
//	func Wrap[T any](v T, err error) (*Response[T], error) {
//	    if err != nil {
//	        return &Response[T]{Status: false, Message: err.Error()}, nil
//	    }
//	    return &Response[T]{Status: true, Data: v}, nil
//	}
//
//	nexus.AsQuery((*UserService).ListUsers, nexus.Envelope(Wrap[[]UserRow]))
//
// On GraphQL the schema (and the generated SDK) declare W, not T — the
// envelope is part of the contract, not a serialization trick. The wrap
// receives a handler's error as the *nexus.Error it maps to (ErrorOf: its
// code, and an uncoded error's message hidden outside nexus dev), so
// err.Error() is safe to put on the wire. An error the
// wrap converts into a value (the usual case) reaches the client as a normal
// 200/data response; an error the wrap returns follows the transport's
// standard error path. Binding and validation failures happen before the
// handler runs and are NOT enveloped. REST + GraphQL only.
func Envelope[T, W any](wrap func(T, error) (W, error)) EnvelopeOption {
	spec := &envelopeSpec{
		inType:  reflect.TypeFor[T](),
		outType: reflect.TypeFor[W](),
		call: func(v any, err error) (any, error) {
			var t T
			if v != nil {
				tv, ok := v.(T)
				if !ok {
					return nil, fmt.Errorf("nexus: Envelope wrap takes %T but the handler returned %T", t, v)
				}
				t = tv
			}
			return wrap(t, err)
		},
	}
	return EnvelopeOption{spec: spec}
}

// EnvelopeOption is returned by nexus.Envelope. It applies to both AsRest
// and AsQuery/AsMutation registrations.
type EnvelopeOption struct{ spec *envelopeSpec }

func (o EnvelopeOption) nexusOption() di.Option    { return di.Options() }
func (o EnvelopeOption) applyToRest(c *restConfig) { c.envelope = o.spec }
func (o EnvelopeOption) applyToGql(c *gqlConfig)   { c.envelope = o.spec }

// envelopeSpec carries a wrap function's types (for schema/registration
// checks) and its direct-call closure (for the request path).
type envelopeSpec struct {
	inType  reflect.Type // T — what the handler returns
	outType reflect.Type // W — what goes on the wire / in the schema
	call    func(v any, err error) (any, error)
}

// check validates the spec against the handler's result type at
// registration, so a mismatched envelope fails at boot with both types
// named rather than panicking per-request.
func (e *envelopeSpec) check(handlerReturn reflect.Type) error {
	if handlerReturn == nil {
		return fmt.Errorf("nexus: Envelope needs a handler returning (T, error); this handler returns no result")
	}
	if !handlerReturn.AssignableTo(e.inType) {
		return fmt.Errorf("nexus: Envelope wrap takes %s but the handler returns %s", e.inType, handlerReturn)
	}
	return nil
}

// outElem strips pointer wrapping from W for schema/type registration,
// mirroring handlerShape.returnElementType.
func (e *envelopeSpec) outElem() reflect.Type {
	t := e.outType
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// entryReturnType is what registrations advertise as the op's return type:
// the envelope's wire type when one is attached, else the handler's own.
func entryReturnType(sh handlerShape, env *envelopeSpec) reflect.Type {
	if env != nil {
		return env.outType
	}
	return sh.returnType
}

// apply pipes a handler's (result, err) through the wrap closure — a
// direct typed call; the only reflection is the nil-pointer normalization
// on the way out (a typed-nil W must become an untyped nil result, the
// same contract callHandler applies to plain handlers).
func (e *envelopeSpec) apply(result any, err error) (any, error) {
	if err != nil {
		err = ErrorOf(err)
	}
	w, werr := e.call(result, err)
	if w != nil {
		if rv := reflect.ValueOf(w); (rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface) && rv.IsNil() {
			return nil, werr
		}
	}
	return w, werr
}
