package auth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
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
	id, err := Login(ctx, Password{Username: in.Login, Password: in.Password})
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
	var cred *Credential
	var err error
	switch p["grant_type"] {
	case "password":
		sc, ok := st.config.settings.firstOf(SchemeBearer)
		if !ok {
			oauthError(c, http.StatusBadRequest, "unsupported_grant_type", "no bearer scheme issues tokens")
			return
		}
		var id *Identity
		if id, err = Login(ctx, Password{Username: p["username"], Password: p["password"]}); err == nil {
			cred, err = SignIn(ctx, id, Using(sc.name))
		}
	case "refresh_token":
		cred, err = RefreshToken(ctx, p["refresh_token"])
	case "":
		oauthError(c, http.StatusBadRequest, "invalid_request", "grant_type is required")
		return
	default:
		oauthError(c, http.StatusBadRequest, "unsupported_grant_type", "supported: password, refresh_token")
		return
	}
	if err != nil {
		ne := nexus.ErrorOf(err)
		switch ne.Code {
		case nexus.TooMany:
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
