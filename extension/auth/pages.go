package auth

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/paulmanoni/nexus/v2"
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
	visit := info.inertia != "" || info.method == http.MethodGet && strings.Contains(info.accept, "text/html")
	if login == "" || !visit {
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
	if info.inertia != "" {
		// Inertia's protocol for "leave this app": a full visit to the URL.
		rc.SetHeader("X-Inertia-Location", to)
		return rc.Reject(http.StatusConflict, err)
	}
	rc.SetHeader("Location", to)
	return rc.Reject(http.StatusFound, err)
}

func (pageErrors) Forbidden(rc *middleware.RequestCtx, err error) error {
	return defaultErrorHandler{}.Forbidden(rc, err)
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
	out := &MeResponse{Can: OpGates(p.Context, app)}
	if id := Current(p.Context); id != nil {
		if pu, ok := st.config.users.(PublicUser); ok {
			out.User = pu.Public(id)
		} else {
			out.User = map[string]string{"id": id.ID, "kind": id.Kind}
		}
	}
	return out, nil
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
	return nexus.Options(opts...)
}
