package nexus

import (
	"context"
	"net/http"
)

// AuthPropagator decides what (if any) caller identity to attach to a
// cross-module request. The default forwards the inbound Authorization
// header from ctx so user identity threads through the call chain
// without any handler-side wiring. Service-to-service calls that need
// a service token instead swap in a custom implementation.
type AuthPropagator interface {
	Inject(ctx context.Context, req *http.Request) error
}

// authCtxKey is the value-key the auth/middleware stores the inbound
// Authorization header under, for AuthPropagator to read on the
// outbound side.
type authCtxKey struct{}

// WithCallerAuthorization stamps the inbound Authorization header
// onto ctx so DefaultAuthPropagator can forward it. Called by the
// auth middleware on every authenticated request.
//
// Generated client code never calls this directly — the auth surface
// in nexus.auth wires it for you. Exposed so custom middleware (or a
// non-auth.Module setup) can plug in.
func WithCallerAuthorization(ctx context.Context, header string) context.Context {
	if header == "" {
		return ctx
	}
	return context.WithValue(ctx, authCtxKey{}, header)
}

// CallerAuthorization returns the Authorization header captured for
// this request, or "" when none. Used by DefaultAuthPropagator and
// available for custom propagators that want to inspect/decorate it.
func CallerAuthorization(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(authCtxKey{}).(string)
	return v
}

// DefaultAuthPropagator forwards the inbound Authorization header
// unchanged. Most apps want this — the JWT (or bearer token) the
// edge service authenticated against follows the request to peer
// services for free.
func DefaultAuthPropagator() AuthPropagator { return forwardAuthHeader{} }

type forwardAuthHeader struct{}

func (forwardAuthHeader) Inject(ctx context.Context, req *http.Request) error {
	if h := CallerAuthorization(ctx); h != "" {
		req.Header.Set("Authorization", h)
	}
	return nil
}

// AuthPropagatorFunc adapts a function to AuthPropagator. Useful for
// one-off custom propagators without a struct:
//
//	caller := nexus.NewRemoteCaller(url, nexus.WithAuthPropagator(
//	    nexus.AuthPropagatorFunc(func(ctx context.Context, req *http.Request) error {
//	        req.Header.Set("Authorization", "Bearer "+mintServiceToken())
//	        return nil
//	    }),
//	))
type AuthPropagatorFunc func(ctx context.Context, req *http.Request) error

func (f AuthPropagatorFunc) Inject(ctx context.Context, req *http.Request) error {
	return f(ctx, req)
}