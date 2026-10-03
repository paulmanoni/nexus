package auth

import (
	"net/http"

	"github.com/paulmanoni/nexus/v2/middleware"
)

// ErrorHandler customizes how auth denials render, across every transport.
// One implementation replaces the old per-transport hooks —
// OnUnauthenticated + OnForbidden (REST) and GraphQLErrorWrap (GraphQL).
//
// Each method handles one denial kind. Use rc.Transport to branch: on
// REST/WS render a custom envelope with rc.RejectJSON (or rc.Reject for the
// framework's {"code", "message"} body); on GraphQL return the error to surface in
// the errors array (rc has no response body there). The returned error
// always short-circuits the chain.
//
//	type apiErrors struct{}
//	func (apiErrors) Unauthenticated(rc *middleware.RequestCtx, err error) error {
//	    if rc.Transport == middleware.TransportGraphQL {
//	        return fmt.Errorf("UNAUTHENTICATED: %w", err)
//	    }
//	    return rc.RejectJSON(http.StatusUnauthorized,
//	        map[string]any{"success": false, "code": "UNAUTH", "error": err.Error()})
//	}
//	func (apiErrors) Forbidden(rc *middleware.RequestCtx, err error) error { ... }
type ErrorHandler interface {
	// Unauthenticated renders a 401 — no valid identity on the request.
	Unauthenticated(rc *middleware.RequestCtx, err error) error
	// Forbidden renders a 403 — an authenticated identity lacking a
	// required permission.
	Forbidden(rc *middleware.RequestCtx, err error) error
}

// defaultErrorHandler is used when Config.OnError is nil: the denial
// renders through nexus's error model like any failed request — 401/403 with
// {"code", "message"} on REST/WS, extensions.code UNAUTHENTICATED/FORBIDDEN
// on GraphQL.
type defaultErrorHandler struct{}

func (defaultErrorHandler) Unauthenticated(rc *middleware.RequestCtx, err error) error {
	return rc.Reject(http.StatusUnauthorized, err)
}

func (defaultErrorHandler) Forbidden(rc *middleware.RequestCtx, err error) error {
	return rc.Reject(http.StatusForbidden, err)
}
