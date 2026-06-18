package oauth2

import (
	"context"
	stderrors "errors"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	oauth2lib "github.com/go-oauth2/oauth2/v4"
	"github.com/go-oauth2/oauth2/v4/errors"
	"github.com/go-oauth2/oauth2/v4/manage"
	"github.com/go-oauth2/oauth2/v4/server"
	"github.com/go-oauth2/oauth2/v4/store"
	"github.com/google/uuid"
	"github.com/paulmanoni/nexus/httpx"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension"
	"github.com/paulmanoni/nexus/extension/auth"
)

// PasswordAuthenticator validates a (username, password) pair against
// the app's user store and returns the user ID that becomes
// TokenInfo.GetUserID() (and downstream auth.Identity.ID). Return a
// typed error and ErrorMapper translates it into the OAuth2
// response; return ErrServiceUnavailable / ErrAccountLocked / etc.
// from the package-level vars to get the standard messages for free.
type PasswordAuthenticator func(ctx context.Context, clientID, username, password string) (userID string, err error)

// IdentityResolver turns a verified OAuth2 TokenInfo into an
// auth.Identity. Defaults to {ID: ti.GetUserID()} — override to
// populate Roles / Scopes / Extra from a user-profile lookup.
type IdentityResolver func(ctx context.Context, ti oauth2lib.TokenInfo) (*auth.Identity, error)

// ErrorMapper translates an internal error (returned by
// Authenticator or any custom handler) into an OAuth2 *errors.Response.
// Return nil to fall through to go-oauth2's stock translation.
type ErrorMapper func(err error) *errors.Response

// Config drives oauth2.Module. The only required field is
// Authenticator (for password grant); everything else has a default.
type Config struct {
	// Authenticator validates credentials for the password grant.
	// Required for password-grant flows. Optional for pure
	// client_credentials apps — leave nil and only client_credentials
	// will issue tokens.
	Authenticator PasswordAuthenticator

	// ClientStore loads OAuth2 clients by ID. Defaults to a single
	// public anonymous client (id="anonymous", no secret) — fine for
	// trusted-network deploys and tests, wrong for the public
	// internet. Provide NewLoaderClientStore or NewStaticClientStore
	// for real workloads.
	ClientStore oauth2lib.ClientStore

	// TokenStore persists access/refresh/code tokens. Defaults to
	// go-oauth2's in-memory store (single-process only). Use
	// NewCacheTokenStore for horizontally-scaled apps.
	TokenStore oauth2lib.TokenStore

	// IdentityResolver enriches the auth.Identity built from each
	// resolved access token. Defaults to {ID: ti.GetUserID()} with
	// the raw token + TokenInfo on .Extra (a *Session) so logout
	// handlers can revoke without re-extracting.
	IdentityResolver IdentityResolver

	// ErrorMapper translates Authenticator errors → OAuth2 responses.
	// Defaults to DefaultErrorMapper (handles ErrInvalidCredentials,
	// ErrAccountDisabled, ErrAccountLocked, ErrServiceUnavailable).
	// Override or wrap to translate domain errors.
	ErrorMapper ErrorMapper

	// ResponseErrorRewriter runs after go-oauth2 builds an OAuth2
	// error response — use it to soften descriptions, translate
	// strings, or log. Optional.
	ResponseErrorRewriter func(*errors.Response)

	// TokenType is the value emitted as token_type in the response.
	// Defaults to "Bearer". Set to "bearer" (lowercase) when
	// migrating clients written against Spring's DefaultTokenServices.
	TokenType string

	// IncludeJTI adds a unique "jti" extension field to every
	// issued token, useful for revocation lists / replay tracking.
	// Off by default.
	IncludeJTI bool

	// AllowGetAccessRequest mirrors server.SetAllowGetAccessRequest.
	// Off by default — the OAuth2 spec recommends POST.
	AllowGetAccessRequest bool

	// TokenPath is the mount path for the token endpoint. Defaults
	// to "/oauth/token".
	TokenPath string

	// RevokePath, when non-empty, mounts a POST handler that
	// removes the access token from the TokenStore. Empty by default.
	RevokePath string

	// IdentityCache bounds how long a resolved auth.Identity stays
	// in the auth.Manager cache. Defaults to 5 minutes; set to
	// negative to disable caching.
	IdentityCache time.Duration

	// Manager is an escape hatch — when non-nil, the package uses
	// it verbatim and ignores ClientStore / TokenStore. Use for
	// exotic config (custom token generator, code-expiry policy)
	// the rest of Config doesn't expose.
	Manager *manage.Manager

	// ServerCustomizer runs after the *server.Server is built and
	// before Mount, giving callers access to any go-oauth2 setter
	// Config doesn't surface (UserAuthorizationHandler, ScopeHandler,
	// custom AccessTokenExpHandler, etc).
	ServerCustomizer func(*server.Server)

	// AuthExtra are auth.Config fields the caller wants to thread
	// through (OnResolve, OnFail, Permissions, error rewriters). The
	// Authentication block (scheme + cache) is owned by this package —
	// oauth2 contributes its own bearer scheme — and is overwritten if
	// set.
	AuthExtra auth.Config
}

// Server is the wired OAuth2 server. Exported so handlers (revoke,
// custom userinfo, etc.) can reach the underlying *server.Server
// and *manage.Manager when ServerCustomizer isn't enough.
type Server struct {
	*nexus.Service
	Srv     *server.Server
	Manager *manage.Manager
	cfg     Config
}

// Session is the default Identity.Extra payload — carries the raw
// token + TokenInfo so a logout handler can revoke both the token
// store entry and the auth cache without re-extracting headers.
type Session struct {
	Token string
	Info  oauth2lib.TokenInfo
}

// Resolve loads an access token and returns its Identity. Wired into
// auth.Module via the holder pattern below so Resolve has a stable
// reference even though the *Server is built later in fx startup.
func (s *Server) Resolve(ctx context.Context, token string) (*auth.Identity, error) {
	if s == nil || s.Srv == nil {
		return nil, stderrors.New("oauth2: server not ready")
	}
	ti, err := s.Manager.LoadAccessToken(ctx, token)
	if err != nil {
		return nil, err
	}
	resolver := s.cfg.IdentityResolver
	if resolver == nil {
		resolver = defaultIdentityResolver
	}
	id, err := resolver(ctx, ti)
	if err != nil {
		return nil, err
	}
	if id != nil && id.Extra == nil {
		id.Extra = &Session{Token: token, Info: ti}
	}
	return id, nil
}

// HandleToken is the OAuth2 token endpoint. Mounted under TokenPath.
func (s *Server) HandleToken(c *httpx.Ctx) {
	_ = s.Srv.HandleTokenRequest(c.Writer, c.Request)
}

// HandleRevoke removes the bearer token from the TokenStore. Mounted
// under RevokePath when set. Returns 204 on success / unknown token
// (idempotent) so clients can safely retry.
func (s *Server) HandleRevoke(c *httpx.Ctx) {
	tok := bearerFromHeader(c.GetHeader("Authorization"))
	if tok == "" {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	_ = s.Manager.RemoveAccessToken(c.Request.Context(), tok)
	c.Status(http.StatusNoContent)
}

// holder threads the live *Server through fx startup so the
// auth.Module Resolve closure binds before the *Server is built.
type holder struct{ srv atomic.Pointer[Server] }

func (h *holder) resolve(ctx context.Context, token string) (*auth.Identity, error) {
	s := h.srv.Load()
	if s == nil {
		return nil, stderrors.New("oauth2: server not ready")
	}
	return s.Resolve(ctx, token)
}

// Module wires the OAuth2 server into a nexus app as an
// extension.Plugin — the same shape custom plugins use. Composes
// auth.Module (so /oauth/token-issued bearer tokens flow through the
// standard auth middleware + Required/Requires bundles), provides the
// *Server type, and mounts the token (and optionally revoke) endpoint.
//
// The Plugin appears in app.Plugins() alongside auth — useful for
// dashboards that list installed extensions.
func Module(cfg Config) nexus.Option {
	cfg.applyDefaults()
	h := &holder{}

	authCfg := cfg.AuthExtra
	authCfg.Authentication = auth.Authentication{
		Schemes: []auth.Scheme{{Name: "oauth2", Extract: auth.Bearer(), Resolve: h.resolve}},
		Cache:   auth.CacheFor(cfg.IdentityCache),
	}

	opts := []nexus.Option{
		nexus.Provide(func(app *nexus.App) *Server { return buildServer(app, cfg, h) }),
		// auth.Module is itself an extension.Plugin — composing it
		// inside oauth2's Options means both auth and oauth2 register
		// in app.Plugins(), no special handling needed.
		auth.Module(authCfg),
		nexus.AsRestHandler("POST", cfg.TokenPath,
			func(srv *Server) httpx.HandlerFunc { return srv.HandleToken },
			nexus.Description("OAuth2 token endpoint (password / refresh / client_credentials grants)."),
			// The token endpoint mints credentials, so it must stay
			// reachable under deny-by-default — you can't require a token
			// to obtain one.
			nexus.Public(),
		),
	}
	if cfg.RevokePath != "" {
		opts = append(opts, nexus.AsRestHandler("POST", cfg.RevokePath,
			func(srv *Server) httpx.HandlerFunc { return srv.HandleRevoke },
			nexus.Description("OAuth2 token revocation endpoint."),
			// Revoke authenticates by the token in the body it revokes, so
			// it manages its own auth and opts out of the default gate.
			nexus.Public(),
		))
	}

	return extension.Use(extension.Plugin{
		Name:    "oauth2",
		Version: "1",
		Options: opts,
	})
}

func buildServer(app *nexus.App, cfg Config, h *holder) *Server {
	mgr := cfg.Manager
	if mgr == nil {
		mgr = manage.NewDefaultManager()
		mgr.SetAuthorizeCodeTokenCfg(manage.DefaultAuthorizeCodeTokenCfg)
		mgr.MapClientStorage(cfg.ClientStore)
		mgr.MustTokenStorage(cfg.TokenStore, nil)
	}

	srv := server.NewDefaultServer(mgr)
	srv.SetAllowGetAccessRequest(cfg.AllowGetAccessRequest)
	srv.SetClientInfoHandler(func(r *http.Request) (string, string, error) {
		// Try Basic auth first, fall back to form fields. Matches
		// the contract most go-oauth2 apps configure by hand.
		if id, secret, err := server.ClientBasicHandler(r); err == nil && id != "" {
			return id, secret, nil
		}
		return server.ClientFormHandler(r)
	})

	if cfg.TokenType != "" {
		srv.SetTokenType(cfg.TokenType)
	}
	if cfg.IncludeJTI {
		srv.SetExtensionFieldsHandler(jtiExtension)
	}

	if cfg.Authenticator != nil {
		srv.SetPasswordAuthorizationHandler(func(ctx context.Context, clientID, username, password string) (string, error) {
			return cfg.Authenticator(ctx, clientID, username, password)
		})
	}

	mapper := cfg.ErrorMapper
	if mapper == nil {
		mapper = DefaultErrorMapper
	}
	srv.SetInternalErrorHandler(func(err error) *errors.Response { return mapper(err) })
	if cfg.ResponseErrorRewriter != nil {
		srv.SetResponseErrorHandler(cfg.ResponseErrorRewriter)
	}

	if cfg.ServerCustomizer != nil {
		cfg.ServerCustomizer(srv)
	}

	s := &Server{
		Service: app.Service("oauth2"),
		Srv:     srv,
		Manager: mgr,
		cfg:     cfg,
	}
	h.srv.Store(s)
	return s
}

func (c *Config) applyDefaults() {
	if c.TokenPath == "" {
		c.TokenPath = "/oauth/token"
	}
	if c.TokenType == "" {
		c.TokenType = "Bearer"
	}
	if c.IdentityCache == 0 {
		c.IdentityCache = 5 * time.Minute
	}
	if c.IdentityCache < 0 {
		c.IdentityCache = 0
	}
	if c.ClientStore == nil {
		c.ClientStore = NewStaticClientStore(StaticClient{ID: "anonymous", Public: true})
	}
	if c.TokenStore == nil {
		ts, _ := store.NewMemoryTokenStore()
		c.TokenStore = ts
	}
}

func defaultIdentityResolver(_ context.Context, ti oauth2lib.TokenInfo) (*auth.Identity, error) {
	return &auth.Identity{ID: ti.GetUserID()}, nil
}

func jtiExtension(ti oauth2lib.TokenInfo) map[string]interface{} {
	ext, ok := ti.(oauth2lib.ExtendableTokenInfo)
	if !ok {
		return nil
	}
	values := ext.GetExtension()
	if values == nil {
		values = url.Values{}
	}
	jti := values.Get("jti")
	if jti == "" {
		jti = uuid.NewString()
		values.Set("jti", jti)
		ext.SetExtension(values)
	}
	return map[string]interface{}{"jti": jti}
}

func bearerFromHeader(h string) string {
	const prefix = "Bearer "
	if len(h) > len(prefix) && (h[:len(prefix)] == prefix || h[:len(prefix)] == "bearer ") {
		return h[len(prefix):]
	}
	return ""
}
