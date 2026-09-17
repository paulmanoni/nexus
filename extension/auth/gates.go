package auth

import "context"

// Can reports whether the request's identity holds the permission —
// evaluated through the same PermissionFn (or Backend.Authorize) the
// Requires() endpoint gate consults, so UI toggles and endpoint gates
// share one rulebook and cannot drift. Anonymous requests are false.
func Can(ctx context.Context, permission string) bool {
	id, ok := IdentityFrom(ctx)
	if !ok {
		return false
	}
	return checkPermissions(ctx, id, []string{permission})
}

// Gates evaluates several permissions at once and returns {permission:
// allowed} — the shape a frontend "can" prop wants, without hand-building
// a struct of Can calls per page:
//
//	props.Can = auth.Gates(ctx, "add_user", "change_user", "delete_user")
//
// Every requested permission appears as a key; for anonymous requests all
// values are false.
func Gates(ctx context.Context, permissions ...string) map[string]bool {
	out := make(map[string]bool, len(permissions))
	id, ok := IdentityFrom(ctx)
	for _, p := range permissions {
		out[p] = ok && checkPermissions(ctx, id, []string{p})
	}
	return out
}
