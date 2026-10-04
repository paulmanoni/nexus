package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/dev"
	"github.com/paulmanoni/nexus/v2/extension/session"
	"github.com/paulmanoni/nexus/v2/middleware"
)

// The config-driven path: Config.Users names the app's accounts, [auth]
// (or Config.Settings) declares the schemes, and nexus issues and checks
// the credentials — sessions, bearer tokens, API keys.

// sessionUserKey is where a session scheme keeps the signed-in user's id.
const sessionUserKey = "_auth_user_id"

// configPath is the state the config-driven path adds to moduleState.
type configPath struct {
	users    Users
	tokens   TokenStore
	settings *resolvedSettings
	loads    *loadCache
	epochs   *epochCache
	throttle *throttle
}

// credentialError is a credential that arrived but didn't authenticate.
type credentialError struct {
	scheme, reason string
}

func (e *credentialError) Error() string { return "auth: " + e.scheme + ": " + e.reason }

// install reads the settings and builds the schemes. It runs in the
// module's invoke, after nexus.toml is loaded and before the middleware.
func (st *moduleState) resolveConfig() error {
	if st.config.settings != nil {
		return nil
	}
	s := settingsSection.Get()
	if st.cfg.Settings != nil {
		s = *st.cfg.Settings
	}
	rs, err := resolveSettings(s)
	if err != nil {
		return err
	}
	cp := &st.config
	cp.settings = rs
	if rs.cache > 0 {
		cp.loads = &loadCache{ttl: rs.cache, m: map[string]loadEntry{}}
		cp.epochs = &epochCache{ttl: rs.cache, m: map[string]epochEntry{}}
	}
	cp.throttle = newThrottle(rs.throttle, st.cfg.Throttle)
	return nil
}

func (st *moduleState) installConfigPath(app *nexus.App) error {
	if err := st.resolveConfig(); err != nil {
		return err
	}
	rs, cp := st.config.settings, &st.config
	if cp.tokens == nil {
		ms := NewMemoryTokenStore()
		dev.Preserve("auth.tokens", ms)
		cp.tokens = ms
		_, bearer := rs.firstOf(SchemeBearer)
		_, apikey := rs.firstOf(SchemeAPIKey)
		if (bearer || apikey) && app.Environment() == "production" {
			app.Logger().Warn("auth: bearer tokens and API keys are kept in memory — every one is lost on restart and unknown to other replicas; use authdb.Bind[DB]() or auth.Config{Tokens: auth.CacheTokens(cache)} in production")
		}
	}
	st.schemes = nil
	for _, sc := range rs.schemes {
		switch sc.Type {
		case SchemeSession:
			app.RequireCSRF("auth session scheme " + sc.name)
			if rs.isAreaSession(sc.name) {
				st.schemes = append(st.schemes, boundScheme{name: sc.name, typ: sc.Type, extract: areaSessionExtractor{rs, sc.name, areaCookie(sc)}, resolve: st.tokenResolve(sc.name)})
				continue
			}
			session.Install(app, session.Config{CookieName: sc.Cookie, TTL: sc.TTL, Secure: sc.Secure})
			st.schemes = append(st.schemes, boundScheme{name: sc.name, typ: sc.Type, extract: sessionExtractor{rs}, resolve: st.sessionResolve(sc.name)})
		case SchemeBearer:
			st.schemes = append(st.schemes, boundScheme{name: sc.name, typ: sc.Type, extract: bearerShaped(false), resolve: st.tokenResolve(sc.name)})
		case SchemeJWT:
			v, err := newJWTVerifier(sc.SchemeSettings)
			if err != nil {
				return fmt.Errorf("[auth.schemes.%s]: %w", sc.name, err)
			}
			st.schemes = append(st.schemes, boundScheme{name: sc.name, typ: sc.Type, extract: bearerShaped(true), resolve: st.jwtResolve(sc.name, v)})
		case SchemeAPIKey:
			st.schemes = append(st.schemes, boundScheme{name: sc.name, typ: sc.Type, extract: apiKey(sc.Header), resolve: st.tokenResolve(sc.name)})
		}
	}
	return nil
}

// sessionExtractor finds the signed-in user's id in the request's session,
// outside the areas that keep their own.
type sessionExtractor struct{ rs *resolvedSettings }

func (e sessionExtractor) extract(r *http.Request) (string, bool) {
	if a := e.rs.area(r.URL.Path); a != nil && a.Session != "" {
		return "", false
	}
	if !session.Present(r.Context()) {
		return "", false
	}
	id := session.Get(r.Context()).GetString(sessionUserKey)
	return id, id != ""
}

// areaSessionExtractor reads an area's own session cookie, inside that
// area only: the cookie holds a stored token (Use "session").
type areaSessionExtractor struct {
	rs           *resolvedSettings
	name, cookie string
}

func (e areaSessionExtractor) extract(r *http.Request) (string, bool) {
	if a := e.rs.area(r.URL.Path); a == nil || a.Session != e.name {
		return "", false
	}
	ck, err := r.Cookie(e.cookie)
	if err != nil || ck.Value == "" {
		return "", false
	}
	return ck.Value, true
}

// areaCookie is the cookie an area's session scheme uses.
func areaCookie(sc namedScheme) string {
	if sc.Cookie != "" {
		return sc.Cookie
	}
	return "nexus_" + sc.name + "_session"
}

func (st *moduleState) sessionResolve(name string) resolver {
	return func(ctx context.Context, userID string) (*Identity, error) {
		s := session.Get(ctx)
		cur, err := st.epoch(ctx, userID)
		if err != nil {
			return nil, err
		}
		if s.GetString(sessionEpochKey) != epochString(cur) {
			return nil, &credentialError{name, revokedReason}
		}
		if s.GetString(sessionRecKey) != "" {
			rec, err := st.config.tokens.Load(ctx, hashToken(s.ID()))
			if err != nil {
				return nil, err
			}
			if rec == nil || rec.Use != useSession {
				return nil, &credentialError{name, "this session was signed out (auth.RevokeSession)"}
			}
		}
		if idle := st.config.settings.sessions.Idle; idle > 0 {
			now := time.Now()
			seen, _ := strconv.ParseInt(s.GetString(sessionSeenKey), 10, 64)
			if seen > 0 && now.Sub(time.UnixMilli(seen)) > idle {
				return nil, &credentialError{name, idleReasonPrefix + idle.String()}
			}
			if now.Sub(time.UnixMilli(seen)) > idle/4 {
				s.Set(sessionSeenKey, strconv.FormatInt(now.UnixMilli(), 10))
			}
		}
		real, err := st.loadAs(ctx, name, userID)
		if err != nil {
			return nil, err
		}
		return st.acting(ctx, real, s.GetString(sessionActingKey))
	}
}

// epochString is how a session stores an epoch: "" for none, so a session
// from before any revocation still matches.
func epochString(n int64) string {
	if n == 0 {
		return ""
	}
	return strconv.FormatInt(n, 10)
}

func (st *moduleState) tokenResolve(name string) resolver {
	return func(ctx context.Context, tok string) (*Identity, error) {
		t, err := st.config.tokens.Load(ctx, hashToken(tok))
		if err != nil {
			return nil, err
		}
		if t == nil || t.Scheme != name {
			return nil, &credentialError{name, "unknown, expired or revoked token"}
		}
		if t.Use == "refresh" {
			return nil, &credentialError{name, "a refresh token is not an access token"}
		}
		cur, err := st.epoch(ctx, t.UserID)
		if err != nil {
			return nil, err
		}
		if t.Epoch != cur {
			return nil, &credentialError{name, revokedReason}
		}
		if strings.HasPrefix(t.UserID, clientPrefix) {
			return st.clientIdentity(ctx, name, t.UserID)
		}
		real, err := st.loadAs(ctx, name, t.UserID)
		if err != nil {
			return nil, err
		}
		return st.acting(ctx, real, t.Impersonating)
	}
}

func (st *moduleState) jwtResolve(name string, v *jwtVerifier) resolver {
	return func(ctx context.Context, tok string) (*Identity, error) {
		sub, iat, reason := v.verify(ctx, tok)
		if reason != "" {
			return nil, &credentialError{name, reason}
		}
		if v.sc.Revocable {
			// A token issued before the user's last RevokeUser is refused.
			if iat.IsZero() {
				return nil, &credentialError{name, "no iat claim (a revocable scheme needs one)"}
			}
			epoch, err := st.epoch(ctx, sub)
			if err != nil {
				return nil, err
			}
			if epoch > 0 && iat.Before(time.Unix(0, epoch).Truncate(time.Second)) {
				return nil, &credentialError{name, revokedReason}
			}
		}
		return st.loadAs(ctx, name, sub)
	}
}

// loadAs loads userID through the per-id cache and stamps the scheme on a
// copy, so a cached identity is never shared mutably.
func (st *moduleState) loadAs(ctx context.Context, scheme, userID string) (*Identity, error) {
	id, err := st.load(ctx, userID)
	if err != nil {
		return nil, err
	}
	if id == nil {
		return nil, &credentialError{scheme, "the account no longer exists"}
	}
	cp := *id
	cp.Scheme = scheme
	return &cp, nil
}

func (st *moduleState) load(ctx context.Context, userID string) (*Identity, error) {
	if st.config.users == nil {
		return nil, errors.New("auth: Config.Users was not constructed")
	}
	if c := st.config.loads; c != nil {
		if id, ok := c.get(userID); ok {
			return id, nil
		}
	}
	id, err := st.config.users.Load(ctx, userID)
	if err != nil || id == nil {
		return id, err
	}
	id = st.expandRoles(id)
	if c := st.config.loads; c != nil {
		c.put(userID, id)
	}
	return id, nil
}

// loadCache keeps Users.Load results per user id: one user with five
// sessions costs one load, and no token is held in memory.
type loadCache struct {
	ttl time.Duration
	mu  sync.Mutex
	m   map[string]loadEntry
}

type loadEntry struct {
	id      *Identity
	expires time.Time
}

func (c *loadCache) get(userID string) (*Identity, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[userID]
	if !ok || time.Now().After(e.expires) {
		delete(c.m, userID)
		return nil, false
	}
	return e.id, true
}

func (c *loadCache) put(userID string, id *Identity) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[userID] = loadEntry{id, time.Now().Add(c.ttl)}
}

func (c *loadCache) drop(userID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, userID)
}

// --- why a request is anonymous ---------------------------------------

type ctxCredential int

const (
	ctxCredentialErr ctxCredential = iota
	ctxCredentialTok
	ctxCookies = ctxCredential(102)
)

// setCookie writes a cookie on the response of the request ctx belongs to.
type setCookie func(*http.Cookie)

type presented struct {
	scheme, token string
}

// unauthenticated is the error a gate rejects an anonymous request with.
// When a credential arrived and failed, the reason is part of it under
// nexus dev; the trace carries it always.
func unauthenticated(ctx context.Context) error {
	ce, ok := ctx.Value(ctxCredentialErr).(*credentialError)
	if !ok || !dev.Enabled() {
		return ErrUnauthenticated
	}
	return &nexus.Error{Code: nexus.Unauthenticated, Message: fmt.Sprintf("auth: unauthenticated — %s: %s", ce.scheme, ce.reason), Cause: ErrUnauthenticated}
}

// credentialReason is the failed credential's reason, for the trace.
func credentialReason(ctx context.Context) string {
	if ce, ok := ctx.Value(ctxCredentialErr).(*credentialError); ok {
		return ce.scheme + ": " + ce.reason
	}
	return ""
}

// defaultGate is the config path's deny-by-default gate: every endpoint
// needs an identity unless it is Public, or [auth] default = "public"; one
// under an area also needs a kind the area admits.
func (st *moduleState) defaultGate() middleware.Middleware {
	return builtin("auth:required",
		"Requires an authenticated identity on ctx ([auth] default)",
		func(rc *middleware.RequestCtx, next middleware.Next) error {
			rs := st.config.settings
			info, _ := rc.Context.Value(ctxRequestInfo).(requestInfo)
			var area *namedArea
			if rs != nil {
				area = rs.area(info.path)
			}
			if area == nil && rs != nil && rs.public {
				return next(rc)
			}
			id, ok := identityFrom(rc.Context)
			if !ok {
				return rejectAuth(rc, unauthenticated(rc.Context))
			}
			if area != nil && !kindIn(id.Kind, area.Kinds) {
				return rejectAuth(rc, ErrForbidden)
			}
			return next(rc)
		})
}

// expandRoles returns id with its Roles' permissions ([auth.roles]) added
// to Perms — a copy, so the app's value is left as it was.
func (st *moduleState) expandRoles(id *Identity) *Identity {
	rs := st.config.settings
	if id == nil || len(id.Roles) == 0 || rs == nil || len(rs.roles) == 0 {
		return id
	}
	cp := *id
	seen := map[string]bool{}
	cp.Perms = nil
	for _, p := range id.Perms {
		if !seen[p] {
			seen[p] = true
			cp.Perms = append(cp.Perms, p)
		}
	}
	for _, role := range id.Roles {
		for _, p := range rs.roles[role] {
			if !seen[p] {
				seen[p] = true
				cp.Perms = append(cp.Perms, p)
			}
		}
	}
	return &cp
}
