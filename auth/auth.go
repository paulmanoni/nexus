// Package auth is nexus's built-in authentication surface. It owns the
// plumbing — token extraction, identity caching, per-op enforcement,
// context propagation — while leaving the *resolution* step (token →
// Identity) user-supplied. That keeps auth.Module unopinionated: works
// with JWTs, opaque bearer tokens, API keys, session cookies, or any
// custom scheme, as long as the caller can turn a raw token into an
// *auth.Identity.
//
// Minimal wiring:
//
//	nexus.Run(nexus.Config{...},
//	    auth.Module(auth.Config{
//	        Resolve: func(ctx context.Context, tok string) (*auth.Identity, error) {
//	            u, err := myAPI.ValidateToken(ctx, tok)
//	            if err != nil { return nil, err }
//	            return &auth.Identity{
//	                ID:    u.ID,
//	                Roles: u.Roles,
//	                Extra: u,
//	            }, nil
//	        },
//	        Cache: auth.CacheFor(15 * time.Minute),
//	    }),
//	    advertsModule,
//	)
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
	"sync"
	"time"

	"go.uber.org/fx"

	"github.com/paulmanoni/nexus"
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

// Config drives auth.Module. Only Resolve is required; everything else
// has a sensible default.
type Config struct {
	// Extract pulls the raw token from the request. Defaults to Bearer()
	// (Authorization: Bearer <token>). Combine strategies with Chain.
	Extract Extractor

	// Resolve is REQUIRED — the function that turns a raw token into an
	// Identity. The package owns extraction, caching, and enforcement;
	// Resolve is the single plug the caller supplies.
	Resolve Resolver

	// Cache memoizes resolved identities so the backend call fires at
	// most once per TTL per token. Zero TTL disables caching entirely.
	Cache CacheOption

	// Permissions overrides the default roles+scopes check. Useful when
	// an app has a hierarchical role model or non-trivial scope matching.
	Permissions PermissionFn

	// OnResolve fires after every successful resolution — good for
	// audit logging or per-user metrics.
	OnResolve func(ctx context.Context, id *Identity)

	// OnFail fires on extraction / resolution failure. The token is
	// passed so handlers can log prefixes for diagnostics; do NOT log
	// the full token in production.
	OnFail func(ctx context.Context, token string, err error)
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
	cfg         Config
	resolve     Resolver
	permissions PermissionFn
	cache       *identityCache // nil when Cache.TTL == 0
}

// Module wires auth into the nexus app:
//
//  1. Installs a global middleware that extracts + (optionally caches)
//     resolves the identity per request, then stashes it on the
//     request context.
//  2. Stashes the shared moduleState on the context so per-op Required /
//     Requires bundles can read custom PermissionFn / cache config.
//  3. Registers a few "auth" middleware names in the registry so the
//     dashboard's middleware chip list labels them consistently.
//
// Module does NOT touch (*Service).Auth. Services using the older
// UserDetailsFn hook continue to work alongside; migration is a
// per-resolver switch from graph.GetRootInfo to auth.User[T].
func Module(cfg Config) nexus.Option {
	if cfg.Resolve == nil {
		return nexus.Raw(fx.Error(fmt.Errorf("auth: Config.Resolve is required")))
	}
	if cfg.Extract == nil {
		cfg.Extract = Bearer()
	}
	if cfg.Permissions == nil {
		cfg.Permissions = DefaultPermissions
	}

	state := &moduleState{
		cfg:         cfg,
		resolve:     cfg.Resolve,
		permissions: cfg.Permissions,
	}
	if cfg.Cache.TTL > 0 {
		state.cache = newIdentityCache(cfg.Cache)
		state.resolve = wrapWithCache(cfg.Resolve, state.cache)
	}

	return nexus.Raw(fx.Invoke(func(app *nexus.App) {
		app.Engine().Use(ginAuthMiddleware(state))
	}))
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

func wrapWithCache(inner Resolver, cache *identityCache) Resolver {
	return func(ctx context.Context, token string) (*Identity, error) {
		if id, ok := cache.get(token); ok {
			return id, nil
		}
		id, err := inner(ctx, token)
		if err != nil {
			return nil, err
		}
		if id != nil {
			cache.set(token, id)
		}
		return id, nil
	}
}