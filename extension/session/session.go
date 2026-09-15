// Package session gives nexus Django-style server-side sessions: a
// cookie carries an opaque session ID, the data lives in a pluggable
// Store, and handlers read and write it through a lazy per-request
// handle — for anonymous visitors and logged-in users alike.
//
//	nexus.Boot(
//	    session.Module(session.Config{}),   // memory store, 14-day TTL
//	)
//
//	func NewAddToCart(svc *ShopService, p nexus.Params[AddArgs]) (*Cart, error) {
//	    s := session.Get(p.Context)
//	    cart, _ := s.Get("cart").([]string)
//	    cart = append(cart, p.Args.SKU)
//	    s.Set("cart", cart)
//	    return buildCart(cart), nil
//	}
//
// Semantics mirror Django's sessions:
//
//   - LAZY: nothing touches the store until a handler reads or
//     writes the session, and nothing is saved unless it was
//     modified (call Touch to force a save).
//   - The cookie is set when the session is first written — not on
//     every anonymous request.
//   - Restart-safe by construction with any external Store; the
//     default MemoryStore additionally survives `nexus dev` rebuilds
//     via the dev-state machinery (production restarts clear it —
//     use CacheStore or your own DB-backed Store there).
//   - Cycle() rotates the session ID in place — call it on login to
//     defeat session fixation. Destroy() deletes the session and
//     expires the cookie.
//
// The session rides the HTTP request, so it is available to REST
// handlers, Inertia pages, and GraphQL resolvers (via p.Context). A
// WebSocket upgrade sees the session of the upgrade request; later
// frames on the socket do not carry one.
package session

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension"
	"github.com/paulmanoni/nexus/httpx"
)

// Config tunes the session extension. The zero value is a working
// dev setup: in-memory store, "nexus_session" cookie, 14-day TTL,
// HttpOnly, SameSite=Lax.
type Config struct {
	// Store holds the session data. Nil selects NewMemoryStore() —
	// fine for dev and single-replica apps (and preserved across
	// `nexus dev` rebuilds), but cleared by a production restart and
	// invisible to other replicas. Point it at CacheStore (Redis via
	// extension/cache) or a DB-backed implementation for production.
	Store Store

	// TTL is the session lifetime, measured from the last SAVE (a
	// request that only reads doesn't extend it — call Touch to
	// refresh explicitly). Zero means 14 days, Django's default.
	TTL time.Duration

	// Cookie attributes. Name defaults to "nexus_session"; Path to
	// "/"; SameSite to Lax; HttpOnly is always set (session IDs are
	// bearer references — script access is never legitimate).
	// Secure should be true wherever the app terminates TLS.
	CookieName string
	Path       string
	Domain     string
	Secure     bool
	SameSite   http.SameSite
}

const (
	defaultTTL    = 14 * 24 * time.Hour
	defaultCookie = "nexus_session"
	// idBytes of entropy per session ID → 43 base64url chars,
	// matching the order of magnitude Django uses.
	idBytes = 32
)

func (c Config) withDefaults() Config {
	if c.Store == nil {
		c.Store = NewMemoryStore()
	}
	if c.TTL <= 0 {
		c.TTL = defaultTTL
	}
	if c.CookieName == "" {
		c.CookieName = defaultCookie
	}
	if c.Path == "" {
		c.Path = "/"
	}
	if c.SameSite == 0 {
		c.SameSite = http.SameSiteLaxMode
	}
	return c
}

// Module enables sessions for the app. Install once, anywhere in the
// option list.
func Module(cfg Config) nexus.Option {
	cfg = cfg.withDefaults()

	// The default memory store survives `nexus dev` rebuilds the same
	// way auth.MemoryUserStore does. No-op outside nexus dev, and for
	// custom stores (which own their durability story).
	if ms, ok := cfg.Store.(*MemoryStore); ok {
		nexus.PreserveDev("session.store", ms)
	}

	return extension.Use(extension.Plugin{
		Name:    "session",
		Version: "1",
		Icon:    "cookie",
		Options: []nexus.Option{
			nexus.Invoke(func(app *nexus.App) {
				app.Router().Use(middleware(cfg))
			}),
		},
	})
}

// ctxKey carries the *Session on the request context so both
// httpx-based handlers and ctx-only code (GraphQL resolvers) reach
// the same instance.
type ctxKey struct{}

// Get returns the request's session handle. Outside a request served
// by an app with session.Module installed it returns an inert,
// never-nil session whose writes are dropped — callers never
// nil-check.
func Get(ctx context.Context) *Session {
	if ctx != nil {
		if s, ok := ctx.Value(ctxKey{}).(*Session); ok {
			return s
		}
	}
	return &Session{closed: true}
}

// middleware attaches a lazy session to every request and saves it
// after the handler chain when (and only when) it was modified.
func middleware(cfg Config) httpx.HandlerFunc {
	return func(c *httpx.Ctx) {
		s := &Session{cfg: cfg, ctx: c}
		if id, err := c.Cookie(cfg.CookieName); err == nil && validID(id) {
			s.id = id
		}
		c.SetRequestContext(context.WithValue(c.Request.Context(), ctxKey{}, s))

		c.Next()

		s.finish(c.Request.Context())
	}
}

// Session is the per-request handle. Safe for concurrent use within
// its request (GraphQL resolvers share one); like the Ctx it rides,
// it must not be retained past the request — a goroutine that
// outlives the handler should copy the values it needs.
type Session struct {
	cfg Config
	ctx *httpx.Ctx

	mu      sync.Mutex
	id      string // "" until a cookie arrived or the first write minted one
	data    map[string]any
	loaded  bool
	dirty   bool
	destroy bool
	closed  bool // saved (or inert) — further writes drop
}

// load pulls the stored data on first access. A missing or expired
// session — or a store error — starts fresh rather than failing the
// request: sessions are a convenience layer, not a gate.
func (s *Session) load() {
	if s.loaded {
		return
	}
	s.loaded = true
	s.data = map[string]any{}
	if s.id == "" || s.cfg.Store == nil {
		return
	}
	ctx := context.Background()
	if s.ctx != nil {
		ctx = s.ctx.Request.Context()
	}
	if data, err := s.cfg.Store.Load(ctx, s.id); err == nil && data != nil {
		s.data = data
	}
}

// Get returns the value stored under key, or nil.
func (s *Session) Get(key string) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	return s.data[key]
}

// GetString is Get with a string assertion ("" when absent or not a
// string) — the common case, saved from a type assertion at every
// call site.
func (s *Session) GetString(key string) string {
	v, _ := s.Get(key).(string)
	return v
}

// Set stores value under key and marks the session for saving. The
// value must survive the store's serialization (the built-in stores
// use JSON — numbers come back as float64/json.Number, structs as
// maps). On the session's FIRST write the ID is minted and the
// cookie set, so call Set before writing the response body.
func (s *Session) Set(key string, value any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.load()
	s.data[key] = value
	s.dirty = true
	s.ensureID()
}

// Delete removes key. A no-op session stays clean — deleting from a
// session that was never written doesn't create one.
func (s *Session) Delete(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.load()
	if _, ok := s.data[key]; ok {
		delete(s.data, key)
		s.dirty = true
	}
}

// Clear empties the session's data but keeps its ID and cookie.
func (s *Session) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.load()
	if len(s.data) > 0 {
		s.data = map[string]any{}
		s.dirty = true
	}
}

// Touch marks the session dirty without changing it, forcing a save —
// the way to refresh the TTL on a read-only request.
func (s *Session) Touch() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.load()
	s.dirty = true
	s.ensureID()
}

// ID returns the session's ID ("" until the first write mints one).
func (s *Session) ID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id
}

// Cycle rotates the session ID in place, keeping the data — call it
// when privilege changes (login) so a pre-auth session ID an attacker
// planted can't ride into the authenticated session (fixation).
func (s *Session) Cycle() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.load()
	if s.id != "" && s.cfg.Store != nil {
		ctx := context.Background()
		if s.ctx != nil {
			ctx = s.ctx.Request.Context()
		}
		_ = s.cfg.Store.Delete(ctx, s.id)
	}
	s.id = ""
	s.dirty = true
	s.ensureID()
}

// Destroy deletes the session from the store and expires the cookie —
// logout's counterpart to Cycle.
func (s *Session) Destroy() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.load()
	s.data = map[string]any{}
	s.dirty = false
	s.destroy = true
	if s.ctx != nil {
		s.ctx.SetSameSite(s.cfg.SameSite)
		s.ctx.SetCookie(s.cfg.CookieName, "", -1, s.cfg.Path, s.cfg.Domain, s.cfg.Secure, true)
	}
}

// ensureID mints the ID and sets the cookie on first need. Caller
// holds s.mu.
func (s *Session) ensureID() {
	if s.id != "" {
		return
	}
	s.id = newID()
	if s.ctx != nil {
		s.ctx.SetSameSite(s.cfg.SameSite)
		s.ctx.SetCookie(s.cfg.CookieName, s.id,
			int(s.cfg.TTL/time.Second), s.cfg.Path, s.cfg.Domain, s.cfg.Secure, true)
	}
}

// finish persists a dirty session and detaches the handle. Runs after
// the handler chain; the Ctx is pooled, so the reference must not
// outlive the request.
func (s *Session) finish(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	defer func() { s.ctx = nil }()
	if s.cfg.Store == nil {
		return
	}
	if s.destroy {
		if s.id != "" {
			_ = s.cfg.Store.Delete(ctx, s.id)
		}
		return
	}
	if !s.dirty || s.id == "" {
		return
	}
	if err := s.cfg.Store.Save(ctx, s.id, s.data, s.cfg.TTL); err != nil {
		// Best-effort: a failed save loses this request's writes but
		// must not fail a response that already succeeded.
		fmt.Printf("nexus: session: save %s: %v\n", s.id[:8]+"…", err)
	}
}

func newID() string {
	b := make([]byte, idBytes)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is a broken platform; refuse to mint
		// guessable IDs.
		panic(fmt.Sprintf("session: crypto/rand: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// validID filters obviously foreign cookie values before they reach
// the store: exactly the shape newID emits.
func validID(id string) bool {
	if len(id) != base64.RawURLEncoding.EncodedLen(idBytes) {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		ok := c == '-' || c == '_' ||
			(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if !ok {
			return false
		}
	}
	return true
}
