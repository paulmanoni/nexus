package auth

import (
	"net/http"

	"github.com/paulmanoni/nexus/v2/middleware"
)

// defaultErrorHandler renders a denial through nexus's error model, like any
// failed request: 401/403 with {"code", "message"} on REST/WS,
// extensions.code UNAUTHENTICATED/FORBIDDEN on GraphQL. pageErrors uses it
// for everything that isn't a page visit.
type defaultErrorHandler struct{}

func (defaultErrorHandler) Unauthenticated(rc *middleware.RequestCtx, err error) error {
	return rc.Reject(http.StatusUnauthorized, err)
}

func (defaultErrorHandler) Forbidden(rc *middleware.RequestCtx, err error) error {
	return rc.Reject(http.StatusForbidden, err)
}
