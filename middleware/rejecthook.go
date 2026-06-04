package middleware

import (
	"context"

	"github.com/gin-gonic/gin"
)

// RejectHook lets a REST-aware extension customise how RequestCtx.Reject
// renders on gin — running app-supplied callbacks the neutral carrier can't
// know about (e.g. auth's OnUnauthenticated / OnForbidden, which take a
// *gin.Context). The extension registers one via WithRejectHook; the gin
// carrier invokes it before its default JSON abort.
//
// Return true when the hook fully handled the response (the carrier then does
// nothing more); return false to fall through to the default abort — so a hook
// can claim only the statuses it owns and leave the rest alone.
//
// This is the one HTTP-leaning escape that keeps package middleware free of
// any extension dependency: the hook closure lives in the extension; only the
// plumbing lives here.
type RejectHook func(c *gin.Context, status int, err error) bool

type rejectHookKey struct{}

// WithRejectHook returns ctx carrying h, to be invoked by the gin carrier on
// Reject. The global REST middleware that owns the policy (e.g. auth's
// ginAuthMiddleware) installs it per request.
func WithRejectHook(ctx context.Context, h RejectHook) context.Context {
	return context.WithValue(ctx, rejectHookKey{}, h)
}

func rejectHookFrom(ctx context.Context) (RejectHook, bool) {
	h, ok := ctx.Value(rejectHookKey{}).(RejectHook)
	return h, ok
}
