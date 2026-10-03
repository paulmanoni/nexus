package session

import (
	"errors"
	"net/http"

	"github.com/paulmanoni/nexus/v2"
	mw "github.com/paulmanoni/nexus/v2/middleware"
)

// ErrNoSession is the rejection [Required] renders when a request arrives
// without an established session.
var ErrNoSession = errors.New("session: no established session on this request")

// Required returns a cross-transport per-op gate that rejects requests
// arriving without an established session — no session cookie, or an unknown
// or expired id. It guards flow continuity (a step that only makes sense
// after an earlier step stored state), not identity: authentication is
// auth.Required's job. The rejection is 428 Precondition Required, telling
// the client to restart the flow rather than to present credentials.
//
//	nexus.AsRest("POST", "/checkout/confirm", NewConfirm, session.Required())
//
// Decorator form: //nexus:session Required.
func Required() nexus.MiddlewareOption {
	gate := mw.FromHandler(mw.NewFunc("session:required", mw.AllTransports,
		func(rc *mw.RequestCtx, next mw.Next) error {
			if !Get(rc.Context).Established() {
				return rc.Reject(http.StatusPreconditionRequired, ErrNoSession)
			}
			return next(rc)
		}))
	gate.Description = "Requires an established session (valid session cookie with live state)"
	gate.Kind = mw.KindBuiltin
	return nexus.Use(gate)
}
