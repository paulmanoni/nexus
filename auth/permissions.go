package auth

// AnyOf returns a PermissionFn that passes when the identity has AT
// LEAST ONE of the configured permissions. Use when a handler should
// accept multiple overlapping roles — "admin OR editor" — without
// spelling out the OR at the call site.
//
//	auth.Module(auth.Config{
//	    Permissions: auth.AnyOf("admin", "editor"),
//	})
//
// Note: the `required` argument to the returned PermissionFn is
// IGNORED — AnyOf's behavior is fully configured at construction. Use
// Requires("role") + a custom permission model when you need the
// Requires call site to drive the set.
func AnyOf(perms ...string) PermissionFn {
	set := make(map[string]struct{}, len(perms))
	for _, p := range perms {
		set[p] = struct{}{}
	}
	return func(id *Identity, _ []string) bool {
		if id == nil {
			return false
		}
		for _, r := range id.Roles {
			if _, ok := set[r]; ok {
				return true
			}
		}
		for _, s := range id.Scopes {
			if _, ok := set[s]; ok {
				return true
			}
		}
		return false
	}
}

// AllOf is the same shape as DefaultPermissions but fixes the required
// set at construction time. Useful for app-wide baseline policies:
//
//	auth.Module(auth.Config{
//	    Permissions: auth.AllOf("authenticated"),
//	})
//
// Per-op Requires() can still add on top via the default path by
// changing Permissions to a composite — see the docs example.
func AllOf(perms ...string) PermissionFn {
	return func(id *Identity, _ []string) bool {
		return DefaultPermissions(id, perms)
	}
}