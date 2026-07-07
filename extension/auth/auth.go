// Package auth is nexus's built-in authentication surface. It owns the
// plumbing — token extraction, identity caching, per-op enforcement,
// context propagation — while leaving the *resolution* step (token →
// Identity) user-supplied. That keeps auth.Module unopinionated: works
// with JWTs, opaque bearer tokens, API keys, session cookies, or any
// custom scheme, as long as the caller can turn a raw token into an
// *auth.Identity.
//
// Minimal wiring — one bearer scheme via the auth.Single shortcut:
//
//	nexus.Run(nexus.Config{...},
//	    auth.Single(func(ctx context.Context, tok string) (*auth.Identity, error) {
//	        u, err := myAPI.ValidateToken(ctx, tok)
//	        if err != nil { return nil, err }
//	        return &auth.Identity{ID: u.ID, Roles: u.Roles, Extra: u}, nil
//	    }, auth.CacheFor(15*time.Minute)),
//	    advertsModule,
//	)
//
// Several schemes (e.g. a bearer JWT for users and an API key for
// service-to-service traffic), tried in declaration order:
//
//	auth.Module(auth.Config{
//	    Authentication: auth.Authentication{
//	        Schemes: []auth.Scheme{
//	            {Resolve: resolveJWT},                                   // defaults to Bearer()
//	            {Name: "apikey", Extract: auth.APIKey("X-API-Key"), Resolve: resolveKey},
//	        },
//	        Cache: auth.CacheFor(15 * time.Minute),
//	    },
//	})
//
// Per-op enforcement (cross-transport — same bundle works on REST +
// GraphQL via the existing nexus.Use attachment):
//
//	nexus.AsMutation(NewCreateAdvert,
//	    auth.Required(),                       // 401 if no valid identity
//	    auth.Requires("ROLE_CREATE_ADVERT"),   // 403 if missing permission
//	)
//
// Resolver access from a handler:
//
//	func NewListAdverts(db *DB) func(ctx context.Context) ([]Advert, error) {
//	    return func(ctx context.Context) ([]Advert, error) {
//	        user, ok := auth.User[MyUser](ctx)
//	        if !ok { /* Required() would have caught this earlier */ }
//	        return db.ListFor(user.ID)
//	    }
//	}
//
// Coexistence with the existing (*Service).Auth API: auth.Module operates
// at the app layer via a global middleware, so services that still call
// (*Service).Auth(UserDetailsFn) keep working as before. Over time,
// migrate resolvers from graph.GetRootInfo to auth.IdentityFrom/User.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/paulmanoni/nexus/di"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/client"
	"github.com/paulmanoni/nexus/extension"
	"github.com/paulmanoni/nexus/extension/dashboard"
	"github.com/paulmanoni/nexus/trace"
)

// Identity is the resolved authenticated user. Roles and Scopes are the
// two first-class permission buckets; Extra carries any backend-specific
// payload the caller wants to thread through to resolvers.
type Identity struct {
	ID     string
	Roles  []string
	Scopes []string
	Extra  any
}

// Has reports whether the identity carries the given permission in
// either Roles or Scopes. Used by the default PermissionFn.
func (i *Identity) Has(perm string) bool {
	for _, r := range i.Roles {
		if r == perm {
			return true
		}
	}
	for _, s := range i.Scopes {
		if s == perm {
			return true
		}
	}
	return false
}

// Resolver turns a raw token into an Identity. Callers implement this
// to plug their auth backend in — a DB lookup, a JWT verification, an
// external API call, anything. Returning an error fails authentication
// for this request (401 when Required() is attached).
type Resolver func(ctx context.Context, token string) (*Identity, error)

// PermissionFn decides whether an identity satisfies a set of required
// permissions. The built-in default (DefaultPermissions) requires the
// identity to have every listed permission in Roles or Scopes.
type PermissionFn func(id *Identity, required []string) bool

// DefaultPermissions is the built-in permission check: every required
// permission must appear in the identity's Roles or Scopes.
func DefaultPermissions(id *Identity, required []string) bool {
	if id == nil {
		return false
	}
	for _, p := range required {
		if !id.Has(p) {
			return false
		}
	}
	return true
}

// Scheme is one way to authenticate a request: an Extractor that pulls a
// credential off the request paired with a Resolver that turns that
// credential into an Identity. A request is authenticated by the FIRST
// scheme whose Extractor finds a credential — that scheme's Resolver then
// runs. List several to accept, say, a bearer JWT for users and an API
// key for service-to-service traffic on the same app.
type Scheme struct {
	// Name labels the scheme in traces and the dashboard. Defaults to the
	// extractor's strategy ("bearer", "apikey", "cookie", "chain") when
	// empty.
	Name string

	// Extract pulls the raw credential from the request. Defaults to
	// Bearer() (Authorization: Bearer <token>) when nil.
	Extract Extractor

	// Resolve turns the extracted credential into an Identity. REQUIRED —
	// it's the single plug each scheme supplies; the package owns
	// extraction ordering, caching, and enforcement.
	Resolve Resolver
}

// Authentication is the "who are you?" half of auth: the ordered list of
// schemes tried per request plus the shared identity cache.
type Authentication struct {
	// Schemes are tried in declaration order; the first whose Extractor
	// yields a credential owns the request. At least one is required.
	Schemes []Scheme

	// Cache memoizes resolved identities (keyed by credential) so a
	// backend call fires at most once per TTL. Zero TTL disables it.
	Cache CacheOption
}

// Config drives auth.Module. Authentication is required; everything else
// has a sensible default.
type Config struct {
	// Authentication declares how requests are authenticated — one or
	// more schemes plus the identity cache. Required.
	Authentication Authentication

	// Authorization declares how a required permission is matched against
	// an identity's roles/scopes — exact by default, or pluggable via
	// Authority (e.g. Wildcard()) / a full Permissions override. The zero
	// value is the exact-match roles+scopes check.
	//
	// When Backend implements Authorize(id, required) bool, the backend's
	// check takes precedence over this field — authorization then lives
	// with the backend. Authorization.Default (deny-by-default) is always
	// honored regardless of the backend.
	Authorization Authorization

	// Backend is the app's cohesive auth backend — one DI-constructed type
	// that supplies request resolution and, optionally, login and
	// authorization (see BackendOption). Optional. When set:
	//
	//   - any Scheme with a nil Resolve inherits the backend's Resolve, so
	//     the resolver can close over app services (a *DB, a token server)
	//     without package globals or a backfill Invoke;
	//   - Manager.Login delegates to the backend's Login when present;
	//   - the backend's Authorize, when present, replaces the
	//     Authorization check above.
	//
	// The zero value leaves every other Config field in sole charge, so
	// existing configs behave identically.
	Backend BackendOption

	// OnResolve fires after every successful resolution — good for
	// audit logging or per-user metrics.
	OnResolve func(ctx context.Context, id *Identity)

	// OnFail fires on extraction / resolution failure. The token is
	// passed so handlers can log prefixes for diagnostics; do NOT log
	// the full token in production.
	OnFail func(ctx context.Context, token string, err error)

	// OnError customizes how 401/403 denials render across every
	// transport — one ErrorHandler replaces the old per-transport
	// OnUnauthenticated / OnForbidden (REST) and GraphQLErrorWrap
	// (GraphQL) fields. Nil uses the default ({"error": msg} on REST/WS,
	// the sentinel error on GraphQL). See ErrorHandler.
	OnError ErrorHandler

	// LoginTokenField names where the access token sits in a login
	// response body, as a dotted path ("token", "accessToken",
	// "data.token"). Bridged into the client SDK manifest so the
	// generated client reads the token from the declared location.
	// Empty → defaults to "data.token" (client.DefaultTokenField), the
	// {status, data:{token}} envelope Go REST handlers typically ship;
	// the SDK still falls back to its heuristic walk (bare/nested token
	// + accessToken) when that path misses, so top-level {token}
	// responses keep working.
	LoginTokenField string

	// CSRFCookie / CSRFHeader name the double-submit CSRF pair the client
	// SDK uses under cookie-based auth strategies: it reads CSRFCookie (a
	// non-HttpOnly cookie the server set) and echoes it in CSRFHeader on
	// state-changing requests so a cross-site post — which rides cookies
	// but can't read this cookie or set the header — is rejected. Empty →
	// defaults to "csrftoken" / "X-CSRFToken" (client.DefaultCSRFCookie /
	// DefaultCSRFHeader, the Django/Laravel convention). Set when your
	// server emits a differently-named pair (e.g. Angular's "XSRF-TOKEN"
	// / "X-XSRF-TOKEN").
	CSRFCookie string
	CSRFHeader string
}

// CacheOption configures how resolved identities are memoized in-memory.
// The cache is process-local on purpose — auth state should be short-
// lived (minutes), and a cross-process cache adds invalidation pain
// that's rarely worth it. Callers that need cross-process cache can
// handle it inside their Resolve function.
type CacheOption struct {
	// TTL is how long a resolved identity stays in cache. 0 disables.
	TTL time.Duration

	// MaxEntries bounds the cache so a misbehaving client can't OOM
	// the app by sending many unique tokens. 0 means unbounded.
	MaxEntries int
}

// CacheFor is a one-liner for the common case — time-only TTL.
// Entries are bounded to 4096 by default so an attacker firing
// endless distinct tokens can't trigger unbounded growth.
func CacheFor(ttl time.Duration) CacheOption {
	return CacheOption{TTL: ttl, MaxEntries: 4096}
}

// ErrUnauthenticated is returned by helpers when no identity is on ctx.
// Middleware converts this to 401 / GraphQL error uniformly.
var ErrUnauthenticated = errors.New("auth: unauthenticated")

// ErrForbidden is returned when an identity is present but lacks the
// required permissions. Middleware converts this to 403.
var ErrForbidden = errors.New("auth: forbidden")

// moduleState is the runtime state the global middleware and per-op
// bundles share. Stashed on request context by the global middleware
// so bundles can read it without a package singleton — keeps multiple
// nexus apps in one process safe.
type moduleState struct {
	cfg          Config
	schemes      []boundScheme // normalized schemes, tried in order
	permissions  PermissionFn
	errorHandler ErrorHandler   // renders 401/403 denials; never nil after Module
	cache        *identityCache // nil when Cache.TTL == 0
	// backend is the resolved Config.Backend (nil when unset). Powers
	// Manager.Login and, when it implements the capability interfaces,
	// scheme resolution + authorization. Populated by finalizeBackend.
	backend any
	// bus is the app-level trace bus captured at Module wire time.
	// We grab it here because the per-route trace.Middleware in AsRest
	// runs AFTER auth bundles in the handler chain — so by the time
	// Required/Requires reject a request, trace.BusFromCtx is still
	// empty. Falling back to this field keeps reject events flowing.
	bus *trace.Bus
}

// Manager is the runtime handle for auth state. di.Provide'd by Module
// so application code can inject it wherever it needs to invalidate
// cached identities (logout flows) or inspect current auth state
// (admin dashboards).
//
//	func NewLogoutHandler(am *auth.Manager) func(ctx, p Params[Args]) (...) {
//	    return func(ctx context.Context, p Params[Args]) (..., error) {
//	        am.Invalidate(p.Args.Token)
//	        return ok, nil
//	    }
//	}
type Manager struct {
	state *moduleState
}

// Invalidate drops the cached identity for the given token. The next
// request bearing that token will re-run Resolve. No-op when the cache
// is disabled.
func (m *Manager) Invalidate(token string) {
	if m.state.cache == nil {
		return
	}
	m.state.cache.delete(token)
}

// InvalidateAll flushes the entire identity cache. Use sparingly —
// every active session will pay a Resolve round-trip on its next
// request. Intended for credential-schema migrations or incident
// response.
func (m *Manager) InvalidateAll() {
	if m.state.cache == nil {
		return
	}
	m.state.cache.clear()
}

// InvalidateByIdentity removes every cache entry whose Identity.ID
// matches the argument. Use for "force-logout user X" flows when the
// caller knows the stable identity but not the tokens (users may
// have multiple active sessions). Returns the number of entries
// dropped so the caller can distinguish "forced logout of 3 sessions"
// from "no cached sessions to drop".
func (m *Manager) InvalidateByIdentity(id string) int {
	if m.state.cache == nil || id == "" {
		return 0
	}
	return m.state.cache.deleteWhere(func(e cacheEntry) bool {
		return e.id != nil && e.id.ID == id
	})
}

// CachedIdentity is a redacted snapshot of a cache entry for dashboard
// / admin display. TokenPrefix is the first 8 characters of the raw
// token followed by "…"; the full token never leaves the cache.
type CachedIdentity struct {
	TokenPrefix string
	Identity    *Identity
	ExpiresAt   time.Time
}

// Identities returns a snapshot of every currently-cached identity.
// Safe to call on a disabled cache (returns empty slice). Token
// prefixes are truncated to 8 chars — never log or return the full
// token back to clients.
func (m *Manager) Identities() []CachedIdentity {
	if m.state.cache == nil {
		return nil
	}
	return m.state.cache.snapshot()
}

// Resolve is a direct synchronous resolution path for code that has a
// token in hand outside the HTTP request cycle — background jobs,
// WS message handlers, CLI tools bolted onto the same app. With no
// request to extract from, it tries each scheme's resolver in order and
// returns the first success. Honors the configured cache.
func (m *Manager) Resolve(ctx context.Context, token string) (*Identity, error) {
	st := m.state
	var lastErr error
	for i := range st.schemes {
		id, err := st.resolveVia(ctx, st.schemes[i], token)
		if err == nil {
			return id, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = ErrUnauthenticated
	}
	return nil, lastErr
}

// Login authenticates a credential through the configured Config.Backend —
// the login counterpart to Resolve. It requires a backend that implements
// Login(ctx, Credentials) (*Identity, error); without one it returns
// ErrInvalidCredentials. Apps that log in via auth.Authenticate(ctx, cred,
// backends...) directly don't need this.
func (m *Manager) Login(ctx context.Context, cred Credentials) (*Identity, error) {
	if lb, ok := m.state.backend.(loginCapable); ok {
		return lb.Login(ctx, cred)
	}
	return nil, ErrInvalidCredentials
}

// Module wires auth into the nexus app. It builds an extension.Plugin
// — the same shape custom plugins use — so auth participates in the
// app's plugin registry alongside any other extensions.
//
//  1. Installs a global gin middleware that extracts + (optionally
//     caches) resolves the identity per request, then stashes it on
//     the request context (Options slot, runs first so subsequent
//     route mounts see the middleware).
//  2. Mounts /__nexus/auth and /__nexus/auth/invalidate via the
//     Dashboard slot.
//  3. Bridges the configured ExtractorInfo into the client SDK
//     manifest via the Client.Apply slot — no-op when the SDK
//     isn't mounted.
//
// Module does NOT touch (*Service).Auth. Services using the older
// UserDetailsFn hook continue to work alongside; migration is a
// per-resolver switch from graph.GetRootInfo to auth.User[T].
func Module(cfg Config) nexus.Option {
	// A backend can supply the resolver, so schemes may omit Resolve — and
	// a backend with no schemes at all gets a default bearer scheme.
	schemesIn := cfg.Authentication.Schemes
	if cfg.Backend.set && len(schemesIn) == 0 {
		schemesIn = []Scheme{{}} // Extract defaults to Bearer(); Resolve from the backend
	}
	schemes, err := bindSchemes(schemesIn, cfg.Backend.set)
	if err != nil {
		return nexus.Raw(di.Error(fmt.Errorf("auth: %w", err)))
	}
	eh := cfg.OnError
	if eh == nil {
		eh = defaultErrorHandler{}
	}
	state := &moduleState{
		cfg:          cfg,
		schemes:      schemes,
		permissions:  cfg.Authorization.permissionFn(),
		errorHandler: eh,
	}
	if cfg.Authentication.Cache.TTL > 0 {
		state.cache = newIdentityCache(cfg.Authentication.Cache)
	}
	manager := &Manager{state: state}

	// Stream the auth summary (cached identities + caching flag) over the
	// dashboard's live WS instead of letting the frontend poll GET
	// /__nexus/auth. Live rejection events already flow via the trace bus
	// (LiveEvents: "auth.reject"), so this completes a poll-free auth panel.
	dashboard.RegisterSnapshotExtra("auth", func() any {
		return map[string]any{
			"identities":     manager.Identities(),
			"cachingEnabled": state.cache != nil,
		}
	})

	pluginOpts := []nexus.Option{
		nexus.Raw(di.Supply(manager)),
		// Install the global auth middleware on the gin engine and
		// capture the trace bus. Runs before the Dashboard slot
		// mounts /__nexus/auth, so those routes inherit the
		// middleware.
		nexus.Invoke(func(app *nexus.App) {
			state.bus = app.Bus()
			app.Engine().Use(ginAuthMiddleware(state))
		}),
	}
	// Deny-by-default: supply the "require identity" gate as the
	// framework's default EndpointGate. The framework prepends it to every
	// endpoint (REST/GraphQL/WS) unless the endpoint is nexus.Public().
	if cfg.Authorization.Default.requireAuth {
		pluginOpts = append(pluginOpts,
			nexus.Raw(di.Supply(&nexus.EndpointGate{Middleware: requiredMiddleware()})))
	}

	// Config.Backend: attach the cohesive backend. A static value finalizes
	// now; a UseBackend constructor is built in DI and finalized in an
	// invoke (both before the first request). finalizeBackend fills any
	// scheme missing a Resolve and, when the backend authorizes, overrides
	// the permission check.
	if cfg.Backend.set {
		switch {
		case cfg.Backend.value != nil:
			if err := finalizeBackend(state, cfg.Backend.value); err != nil {
				return raiseBackendError(err)
			}
		case cfg.Backend.ctor != nil:
			opt, err := backendFinalizeOption(state, cfg.Backend.ctor)
			if err != nil {
				return raiseBackendError(err)
			}
			pluginOpts = append(pluginOpts, opt)
		default:
			return raiseBackendError(fmt.Errorf("Backend is set but has neither a value nor a constructor"))
		}
	}

	return extension.Use(extension.Plugin{
		Name:    "auth",
		Version: "1",
		Options: pluginOpts,
		Dashboard: &extension.Dashboard{
			Tab: &extension.Tab{ID: "auth", Label: "Auth"},
			Routes: []extension.Route{
				{Method: "GET", Path: "", Handler: dashboardListHandler(manager)},
				{Method: "POST", Path: "/invalidate", Handler: dashboardInvalidateHandler(manager)},
			},
			LiveEvents: []string{"auth.reject"},
		},
		Client: &extension.Client{
			Namespace: "auth",
			Apply: func(app *nexus.App) error {
				// Bridge the auth strategy into the client SDK manifest.
				// SetClientAuthInfo short-circuits when the SDK isn't
				// mounted (Config.Client.Enabled off, no nexus.ClientUse),
				// so apps without the SDK pay nothing for this hook.
				app.SetClientAuthInfo(func() client.ExtractorInfo {
					return toClientExtractor(manager.Info())
				})
				// Overlay the auth-config hints (login-token location +
				// CSRF names) onto the manifest's Auth section. Static —
				// they come from cfg, not the registry — so a plain set
				// (no closure) is enough. No-op fields keep the SDK's own
				// defaults.
				app.SetClientAuthMeta(client.AuthMeta{
					TokenField: cfg.LoginTokenField,
					CSRFCookie: cfg.CSRFCookie,
					CSRFHeader: cfg.CSRFHeader,
				}.WithDefaults())
				return nil
			},
		},
		// Contributor emits framework-flavored TS that wraps the
		// codegen'd login / logout / me typed functions in a stateful
		// composable. Picked up by the frontend extension's Generate
		// driver at render time; apps without a frontend driver pay
		// nothing for this slot (no driver → no Render call → no
		// NexusContribute invocation).
		Contributor: authContributor{},
	})
}

// Single wires auth with one bearer-token scheme — the overwhelmingly
// common case. Equivalent to Module(Config{Authentication: Authentication{
// Schemes: []Scheme{{Resolve: resolve}}}}) with an optional cache:
//
//	auth.Single(myResolve)
//	auth.Single(myResolve, auth.CacheFor(15*time.Minute))
func Single(resolve Resolver, cache ...CacheOption) nexus.Option {
	a := Authentication{Schemes: []Scheme{{Resolve: resolve}}}
	if len(cache) > 0 {
		a.Cache = cache[0]
	}
	return Module(Config{Authentication: a})
}

// boundScheme is a Scheme with its defaults resolved — used internally by
// the middleware and Manager so the per-request path never re-checks
// nil Extract / derived Name.
type boundScheme struct {
	name    string
	extract Extractor
	resolve Resolver
}

// bindSchemes validates and normalizes the configured schemes: every
// scheme needs a Resolver, a nil Extract defaults to Bearer(), and an
// empty Name derives from the extractor's strategy.
// allowNilResolve permits schemes without a Resolver — used when a
// Config.Backend will supply it in finalizeBackend before the first request.
func bindSchemes(in []Scheme, allowNilResolve bool) ([]boundScheme, error) {
	if len(in) == 0 {
		return nil, fmt.Errorf("Config.Authentication.Schemes must declare at least one scheme")
	}
	out := make([]boundScheme, 0, len(in))
	for i, s := range in {
		if s.Resolve == nil && !allowNilResolve {
			return nil, fmt.Errorf("Config.Authentication.Schemes[%d]: Resolve is required (or set Config.Backend)", i)
		}
		ex := s.Extract
		if ex == nil {
			ex = Bearer()
		}
		name := s.Name
		if name == "" {
			name = Describe(ex).Strategy
		}
		out = append(out, boundScheme{name: name, extract: ex, resolve: s.Resolve})
	}
	return out, nil
}

// resolveVia runs a single scheme's resolver through the shared identity
// cache (keyed by credential). The cache is shared across schemes because
// the public Manager.Resolve has only a token in hand — keying by token
// keeps that path and the request path consistent.
func (st *moduleState) resolveVia(ctx context.Context, sc boundScheme, token string) (*Identity, error) {
	if st.cache != nil {
		if id, ok := st.cache.get(token); ok {
			return id, nil
		}
	}
	id, err := sc.resolve(ctx, token)
	if err != nil {
		return nil, err
	}
	if id != nil && st.cache != nil {
		st.cache.set(token, id)
	}
	return id, nil
}

// authenticate walks the schemes in order and returns the Identity from
// the first scheme whose Extractor finds a credential. That scheme owns
// the request — a resolve error from it does NOT fall through to later
// schemes (extractors rarely overlap, and falling through would muddy
// which failure to report). Returns (nil, "", nil) for an anonymous
// request (no scheme found a credential).
func (st *moduleState) authenticate(ctx context.Context, r *http.Request) (*Identity, string, error) {
	for i := range st.schemes {
		tok, ok := st.schemes[i].extract.Extract(r)
		if !ok {
			continue
		}
		id, err := st.resolveVia(ctx, st.schemes[i], tok)
		return id, tok, err
	}
	return nil, "", nil
}

// toClientExtractor adapts auth.ExtractorInfo (canonical) to
// client.ExtractorInfo (the duplicate that lives in client/ to
// avoid a nexus → client → auth import cycle). Recurses on
// chained extractors so multi-strategy apps surface the full
// shape to SDK consumers.
func toClientExtractor(a ExtractorInfo) client.ExtractorInfo {
	out := client.ExtractorInfo{
		Strategy:   a.Strategy,
		HeaderName: a.HeaderName,
		CookieName: a.CookieName,
	}
	for _, c := range a.Chain {
		out.Chain = append(out.Chain, toClientExtractor(c))
	}
	return out
}

// --- in-memory identity cache -------------------------------------------

// identityCache is a simple TTL + size-bounded map from token → identity.
// Eviction on Set when over MaxEntries is O(n) scan of the oldest —
// acceptable for the small caps we expect (thousands); if anyone needs
// more, swap in an LRU. Not exposed; users who want a different cache
// tier plug it into their Resolve.
type identityCache struct {
	mu         sync.Mutex
	entries    map[string]cacheEntry
	ttl        time.Duration
	maxEntries int
}

type cacheEntry struct {
	id        *Identity
	expiresAt time.Time
}

func newIdentityCache(opt CacheOption) *identityCache {
	return &identityCache{
		entries:    make(map[string]cacheEntry),
		ttl:        opt.TTL,
		maxEntries: opt.MaxEntries,
	}
}

func (c *identityCache) get(token string) (*Identity, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[token]
	if !ok {
		return nil, false
	}
	if time.Now().After(e.expiresAt) {
		delete(c.entries, token)
		return nil, false
	}
	return e.id, true
}

func (c *identityCache) delete(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, token)
}

func (c *identityCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]cacheEntry)
}

// deleteWhere removes every entry whose predicate returns true.
// Returns the count of dropped entries. Locked for the whole sweep
// so a concurrent set() can't reintroduce an entry we just decided
// to drop.
func (c *identityCache) deleteWhere(pred func(cacheEntry) bool) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for k, e := range c.entries {
		if pred(e) {
			delete(c.entries, k)
			n++
		}
	}
	return n
}

// snapshot returns redacted cache entries. Token keys are truncated
// to an 8-char prefix + "…" so the result is safe to serialize onto
// the dashboard without leaking credentials. Expired entries are
// filtered out at read time to avoid reporting stale rows.
func (c *identityCache) snapshot() []CachedIdentity {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]CachedIdentity, 0, len(c.entries))
	now := time.Now()
	for tok, e := range c.entries {
		if now.After(e.expiresAt) {
			continue
		}
		prefix := tok
		if len(prefix) > 8 {
			prefix = prefix[:8] + "…"
		}
		out = append(out, CachedIdentity{
			TokenPrefix: prefix,
			Identity:    e.id,
			ExpiresAt:   e.expiresAt,
		})
	}
	return out
}

func (c *identityCache) set(token string, id *Identity) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.maxEntries > 0 && len(c.entries) >= c.maxEntries {
		// Evict one expired entry if we can; otherwise drop the
		// oldest. Kept simple because auth caches are typically in
		// the hundreds / low thousands.
		var oldestKey string
		var oldestAt time.Time
		first := true
		for k, e := range c.entries {
			if time.Now().After(e.expiresAt) {
				delete(c.entries, k)
				goto insert
			}
			if first || e.expiresAt.Before(oldestAt) {
				oldestKey = k
				oldestAt = e.expiresAt
				first = false
			}
		}
		if oldestKey != "" {
			delete(c.entries, oldestKey)
		}
	}
insert:
	c.entries[token] = cacheEntry{id: id, expiresAt: time.Now().Add(c.ttl)}
}
