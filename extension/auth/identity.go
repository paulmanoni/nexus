package auth

import (
	"context"
	"slices"
	"strings"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/middleware"
	"github.com/paulmanoni/nexus/v2/registry"
)

// grants reports whether Perms grant perm: equal, "*", or a "prefix*"
// whose prefix perm starts with.
func (i *Identity) grants(perm string) bool {
	for _, g := range i.Perms {
		if g == perm || g == "*" {
			return true
		}
		if prefix, ok := strings.CutSuffix(g, "*"); ok && strings.HasPrefix(perm, prefix) {
			return true
		}
	}
	return false
}

// Current returns the request's identity, or nil when it is anonymous —
// the same on every transport.
//
//	if me := auth.Current(ctx); me != nil { … }
func Current(ctx context.Context) *Identity {
	id, _ := IdentityFrom(ctx)
	return id
}

// ID returns the request identity's ID parsed into T (a string or integer
// type); false when anonymous or the ID doesn't parse.
//
//	uid, ok := auth.ID[int64](ctx)
func ID[T SubjectID](ctx context.Context) (T, bool) { return Subject[T](ctx) }

// RequiresAny gates an endpoint on at least one of perms. Like Requires it
// implies authentication: anonymous requests are Unauthenticated, signed-in
// ones without any of perms Forbidden. Stacked with Requires, both apply.
//
//	nexus.AsMutation((*Orders).Refund, auth.RequiresAny("orders.refund", "orders.admin"))
func RequiresAny(perms ...string) nexus.MiddlewareOption {
	mw := builtin("auth:requires_any:"+joinPerms(perms),
		"Requires at least one of the permissions on the identity",
		func(rc *middleware.RequestCtx, next middleware.Next) error {
			id, ok := IdentityFrom(rc.Context)
			if !ok {
				return rejectAuth(rc, ErrUnauthenticated)
			}
			if !anyPermission(rc.Context, id, perms) {
				return rejectAuth(rc, ErrForbidden)
			}
			return next(rc)
		})
	mw.Tags = map[string]string{registry.AuthRequiresAnyTag: strings.Join(perms, ",")}
	return nexus.Use(mw)
}

// Kind gates an endpoint on the identity's kind: one of kinds. It implies
// authentication, and combines with Requires and RequiresAny — the gate a
// path prefix can't express when one mount (/graphql) serves several kinds.
//
//	nexus.AsMutation((*Orders).Refund, auth.Kind("staff"), auth.Requires("orders.refund"))
func Kind(kinds ...string) nexus.MiddlewareOption {
	mw := builtin("auth:kind:"+joinPerms(kinds),
		"Requires an identity of the given kind",
		func(rc *middleware.RequestCtx, next middleware.Next) error {
			id, ok := IdentityFrom(rc.Context)
			if !ok {
				return rejectAuth(rc, ErrUnauthenticated)
			}
			if !slices.Contains(kinds, id.Kind) {
				return rejectAuth(rc, ErrForbidden)
			}
			return next(rc)
		})
	mw.Tags = map[string]string{registry.AuthKindTag: strings.Join(kinds, ",")}
	return nexus.Use(mw)
}

// anyPermission reports whether id holds at least one of perms, through the
// same check Requires uses.
func anyPermission(ctx context.Context, id *Identity, perms []string) bool {
	for _, p := range perms {
		if checkPermissions(ctx, id, []string{p}) {
			return true
		}
	}
	return false
}
