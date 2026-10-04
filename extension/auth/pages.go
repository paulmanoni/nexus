package auth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/httpx"
	"github.com/paulmanoni/nexus/v2/middleware"
)

// pageErrors is the config path's ErrorHandler: an unauthenticated page
// visit goes to the sign-in page — the area's, else [auth] login — with
// ?next= back to it; everything else answers like defaultErrorHandler.
type pageErrors struct{ st *moduleState }

func (h pageErrors) Unauthenticated(rc *middleware.RequestCtx, err error) error {
	rs := h.st.config.settings
	info, _ := rc.Context.Value(ctxRequestInfo).(requestInfo)
	if rs == nil || rc.Transport != middleware.TransportREST {
		return defaultErrorHandler{}.Unauthenticated(rc, err)
	}
	login := rs.login
	if a := rs.area(info.path); a != nil && a.Login != "" {
		login = a.Login
	}
	if login == "" || !isPageVisit(info) {
		return defaultErrorHandler{}.Unauthenticated(rc, err)
	}
	to := login
	if info.method == http.MethodGet && info.uri != "" {
		sep := "?"
		if strings.Contains(to, "?") {
			sep = "&"
		}
		to += sep + rs.nextParam + "=" + url.QueryEscape(info.uri)
	}
	return redirectVisit(rc, info, to, err)
}

func (h pageErrors) Forbidden(rc *middleware.RequestCtx, err error) error {
	rs := h.st.config.settings
	info, _ := rc.Context.Value(ctxRequestInfo).(requestInfo)
	if rs == nil || rc.Transport != middleware.TransportREST {
		return defaultErrorHandler{}.Forbidden(rc, err)
	}
	to := rs.forbidden
	if a := rs.area(info.path); a != nil && a.Forbidden != "" {
		to = a.Forbidden
	}
	if to == "" || !isPageVisit(info) {
		return defaultErrorHandler{}.Forbidden(rc, err)
	}
	return redirectVisit(rc, info, to, err)
}

// isPageVisit is an Inertia visit or a browser loading a document.
func isPageVisit(info requestInfo) bool {
	return info.inertia != "" || info.method == http.MethodGet && strings.Contains(info.accept, "text/html")
}

// redirectVisit sends a page visit to another page: Inertia's 409 with
// X-Inertia-Location, else a 302.
func redirectVisit(rc *middleware.RequestCtx, info requestInfo, to string, err error) error {
	if info.inertia != "" {
		rc.SetHeader("X-Inertia-Location", to)
		return rc.Reject(http.StatusConflict, err)
	}
	rc.SetHeader("Location", to)
	return rc.Reject(http.StatusFound, err)
}

// authProp is the page prop: who is signed in and what they may call.
func (st *moduleState) authProp(ctx context.Context) (string, any) {
	rs := st.config.settings
	if rs == nil || rs.pageProp == "" || st.app == nil {
		return "", nil
	}
	return rs.pageProp, st.me(ctx, st.app)
}

func init() {
	nexus.RegisterSharedPageProp(func(ctx context.Context) (string, any) {
		if st, ok := stateFrom(ctx); ok {
			return st.authProp(ctx)
		}
		return "", nil
	})
}

// --- [auth.endpoints] ----------------------------------------------------

// SignInRequest is the built-in login endpoint's body.
type SignInRequest struct {
	Login    string `json:"login" validate:"required"`
	Password string `json:"password" validate:"required"`
	// Next is the page to land on (validated); Scheme forces one
	// ([auth.schemes.<name>]), such as a bearer scheme for an API client.
	Next   string `json:"next,omitempty"`
	Scheme string `json:"scheme,omitempty"`
}

// MeResponse is the built-in me endpoint's answer: the user as Users'
// optional Public method shows it (else {id, kind}), and {op: allowed} for
// every op the app registered (auth.OpGates). User is null when anonymous.
type MeResponse struct {
	User any             `json:"user"`
	Can  map[string]bool `json:"can"`
	// Actor is the real user while User is being impersonated.
	Actor any `json:"actor,omitempty"`
}

// PublicUser is an optional Users method: what the me endpoint (and other
// pages) may show of a user. Without it, me shows only {id, kind} — the
// identity's Extra is never serialized by accident.
type PublicUser interface {
	Public(id *Identity) any
}

func signInEndpoint(ctx context.Context, in SignInRequest) (*Credential, error) {
	id, err := Login(ctx, Password{Login: in.Login, Password: in.Password})
	if err != nil {
		return nil, err
	}
	opts := []SignInOption{ReturnTo(in.Next)}
	if in.Scheme != "" {
		opts = append(opts, Using(in.Scheme))
	}
	return SignIn(ctx, id, opts...)
}

type signedOut struct {
	OK bool `json:"ok"`
}

func signOutEndpoint(ctx context.Context, _ struct{}) (*signedOut, error) {
	return &signedOut{OK: true}, SignOut(ctx)
}

func (st *moduleState) meEndpoint(app *nexus.App, p nexus.Params[struct{}]) (*MeResponse, error) {
	return st.me(p.Context, app), nil
}

func (st *moduleState) me(ctx context.Context, app *nexus.App) *MeResponse {
	out := &MeResponse{Can: OpGates(ctx, app)}
	if id := Current(ctx); id != nil {
		out.User = st.public(id)
		if id.Actor != nil {
			out.Actor = st.public(id.Actor)
		}
	}
	return out
}

// public is what may be shown of id: Users.Public's view, else {id, kind}.
func (st *moduleState) public(id *Identity) any {
	if pu, ok := st.config.users.(PublicUser); ok {
		return pu.Public(id)
	}
	return map[string]string{"id": id.ID, "kind": id.Kind}
}

// endpointOptions mounts the [auth.endpoints] that have a path.
func (st *moduleState) endpointOptions() nexus.Option {
	ep := st.config.settings.endpoints
	var opts []nexus.Option
	if ep.Login != "" {
		opts = append(opts, nexus.AsRest("POST", ep.Login, signInEndpoint,
			Public(), nexus.AuthRoute("login"), nexus.Describe("Sign in")))
	}
	if ep.Logout != "" {
		opts = append(opts, nexus.AsRest("POST", ep.Logout, signOutEndpoint,
			Public(), nexus.AuthRoute("logout"), nexus.Describe("Sign out")))
	}
	if ep.Me != "" {
		opts = append(opts, nexus.AsRest("GET", ep.Me, st.meEndpoint,
			Public(), nexus.AuthRoute("me"), nexus.Describe("The signed-in user and what they may do")))
	}
	if imp := st.config.settings.impersonation.Endpoint; imp != "" {
		opts = append(opts,
			nexus.AsRest("POST", imp, impersonateEndpoint, nexus.Describe("Start impersonating a user")),
			nexus.AsRest("DELETE", imp, stopImpersonatingEndpoint, nexus.Describe("Stop impersonating")))
	}
	for _, sc := range st.config.settings.schemes {
		if sc.Type != SchemeOIDC {
			continue
		}
		p := &oidcProvider{name: sc.name, sc: sc.SchemeSettings}
		callback := sc.Redirect
		if u, err := url.Parse(callback); err == nil && u.IsAbs() {
			callback = u.Path
		}
		opts = append(opts,
			nexus.AsRest("GET", sc.Login, st.oidcStart(p), Public(), nexus.Describe("Sign in with "+sc.name)),
			nexus.AsRest("GET", callback, st.oidcCallback(p), Public(), nexus.Describe("Sign-in callback from "+sc.name)))
	}
	if ep.Token != "" {
		opts = append(opts, nexus.AsRest("POST", ep.Token, st.tokenEndpoint,
			Public(), nexus.Describe("OAuth2 token endpoint: password and refresh_token grants")),
			nexus.Invoke(func(app *nexus.App) { app.ExemptCSRF(ep.Token) }))
	}
	if ep.Revoke != "" {
		opts = append(opts, nexus.AsRest("POST", ep.Revoke, revokeEndpoint,
			Public(), nexus.Describe("OAuth2 token revocation")),
			nexus.Invoke(func(app *nexus.App) { app.ExemptCSRF(ep.Revoke) }))
	}
	return nexus.Options(opts...)
}

// oauthParams reads an OAuth2 request: form-encoded as RFC 6749 says, or
// JSON, which many clients send.
func oauthParams(c *httpx.Ctx) map[string]string {
	out := map[string]string{}
	if strings.HasPrefix(c.GetHeader("Content-Type"), "application/json") {
		var m map[string]any
		if json.NewDecoder(io.LimitReader(c.Request.Body, 1<<16)).Decode(&m) == nil {
			for k, v := range m {
				if s, ok := v.(string); ok {
					out[k] = s
				}
			}
		}
		return out
	}
	_ = c.Request.ParseForm()
	for k := range c.Request.PostForm {
		out[k] = c.Request.PostForm.Get(k)
	}
	return out
}

// oauthError answers in RFC 6749 §5.2's shape.
func oauthError(c *httpx.Ctx, status int, code, desc string) {
	c.Header("Cache-Control", "no-store")
	c.JSON(status, map[string]string{"error": code, "error_description": desc})
}

func (st *moduleState) tokenEndpoint(c *httpx.Ctx) {
	ctx := c.Request.Context()
	p := oauthParams(c)
	grant := p["grant_type"]
	switch grant {
	case "password", "refresh_token", "client_credentials":
	case "":
		oauthError(c, http.StatusBadRequest, "invalid_request", "grant_type is required")
		return
	default:
		oauthError(c, http.StatusBadRequest, "unsupported_grant_type", "supported: password, refresh_token, client_credentials")
		return
	}

	// The client: HTTP Basic, else client_id / client_secret in the body.
	id, secret, basic := c.Request.BasicAuth()
	if !basic {
		id, secret = p["client_id"], p["client_secret"]
	}
	var cl *Client
	if id != "" {
		found, err := st.client(ctx, id)
		if err != nil {
			nexus.WriteError(c, err)
			return
		}
		if found == nil || !st.clientSecretOK(found, secret) {
			if basic {
				c.Header("WWW-Authenticate", `Basic realm="token"`)
			}
			oauthError(c, http.StatusUnauthorized, "invalid_client", "unknown client or wrong secret")
			return
		}
		cl = found
	}
	if cl == nil && (grant == "client_credentials" || st.config.settings.oauth2.RequireClient) {
		oauthError(c, http.StatusUnauthorized, "invalid_client", "this grant needs client authentication")
		return
	}
	if cl != nil && !cl.may(grant) {
		oauthError(c, http.StatusBadRequest, "unauthorized_client", "the client may not use "+grant)
		return
	}
	sc, ok := st.config.settings.firstOf(SchemeBearer)
	if !ok {
		oauthError(c, http.StatusBadRequest, "unsupported_grant_type", "no bearer scheme issues tokens")
		return
	}

	var cred *Credential
	var err error
	switch grant {
	case "password":
		var who *Identity
		if who, err = Login(ctx, Password{Login: p["username"], Password: p["password"]}); err == nil {
			cred, err = SignIn(ctx, who, Using(sc.name))
		}
	case "refresh_token":
		cred, err = RefreshToken(ctx, p["refresh_token"])
	case "client_credentials":
		if !cl.confidential() {
			oauthError(c, http.StatusUnauthorized, "invalid_client", "a public client can't use client_credentials")
			return
		}
		var epoch int64
		if epoch, err = st.epoch(ctx, clientPrefix+cl.ID); err == nil {
			cred, err = st.issueTokens(ctx, sc, clientPrefix+cl.ID, epoch, false)
		}
	}
	if err != nil {
		ne := nexus.ErrorOf(err)
		switch ne.Code {
		case nexus.TooMany:
			if ne.RetryAfter > 0 {
				c.Header("Retry-After", strconv.Itoa(int(ne.RetryAfter.Seconds())))
			}
			oauthError(c, http.StatusTooManyRequests, "invalid_grant", ne.Error())
		case nexus.InvalidInput, nexus.Unauthenticated, nexus.Forbidden:
			oauthError(c, http.StatusBadRequest, "invalid_grant", ne.Error())
		default:
			nexus.WriteError(c, err)
		}
		return
	}
	cred.Next = ""
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, cred)
}

// revokeEndpoint ends a token (RFC 7009): 200 whether or not it was valid.
func revokeEndpoint(c *httpx.Ctx) {
	if tok := oauthParams(c)["token"]; tok != "" {
		_ = Revoke(c.Request.Context(), tok)
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, map[string]any{})
}

// warnMissingPages logs each [auth] / area login or forbidden path no GET
// route serves. An app with a frontend (an SPA may route it) isn't checked.
func (st *moduleState) warnMissingPages(app *nexus.App) {
	rs := st.config.settings
	if rs == nil {
		return
	}
	if _, _, ok := app.FrontendFS(); ok {
		return
	}
	served := map[string]bool{}
	for _, e := range app.Registry().Endpoints() {
		if strings.Contains(e.Method, "GET") {
			served[e.Path] = true
		}
	}
	check := func(key, path string) {
		if path == "" {
			return
		}
		if u, err := url.Parse(path); err == nil {
			path = u.Path
		}
		if !served[path] {
			app.Logger().Warn("auth: no GET route serves " + key + " " + path + " — visitors sent there get a 404")
		}
	}
	check("[auth] login", rs.login)
	check("[auth] forbidden", rs.forbidden)
	for _, a := range rs.areas {
		check("[auth.areas."+a.name+"] login", a.Login)
		check("[auth.areas."+a.name+"] forbidden", a.Forbidden)
	}
}
