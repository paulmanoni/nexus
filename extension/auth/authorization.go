package auth

import "strings"

// Authority decides whether a single granted permission (a role or scope
// the identity carries) satisfies a single required permission named at a
// Requires(...) gate. The default is exact string equality; swap in
// Wildcard() for prefix matching. This is the knob that turns the
// roles/scopes model from exact-match into something hierarchical without
// rewriting the whole permission check.
type Authority func(granted, required string) bool

// ExactAuthority is the default Authority: a granted permission satisfies
// a required one only when the two strings are equal.
func ExactAuthority(granted, required string) bool { return granted == required }

// Wildcard returns an Authority where a granted permission ending in "*"
// matches any required permission sharing the prefix before it. A bare
// "*" grants everything. Matching is segment-agnostic — "admin:*" covers
// "admin:read" and "admin:users:delete" alike. A granted permission with
// no trailing "*" still matches only by exact equality.
//
//	granted "admin:*" satisfies "admin:read", "admin:users:delete"
//	granted "*"       satisfies anything
//	granted "admin"   satisfies only "admin"
func Wildcard() Authority {
	return func(granted, required string) bool {
		if granted == required || granted == "*" {
			return true
		}
		if prefix, ok := strings.CutSuffix(granted, "*"); ok {
			return strings.HasPrefix(required, prefix)
		}
		return false
	}
}

// Authorization is the "what may you do?" half of auth: how a required
// permission named at a Requires(...) gate is matched against the
// identity's roles and scopes. The zero value is the exact-match
// roles+scopes check (identical to DefaultPermissions).
type Authorization struct {
	// Authority matches a single granted permission against a single
	// required one. nil means exact equality (ExactAuthority).
	Authority Authority

	// Permissions fully overrides the all-required check — the ultimate
	// escape hatch for policies a single Authority can't express
	// (cross-permission rules, externally-evaluated decisions, AnyOf-style
	// baselines). When set, Authority is ignored. nil means "every
	// required permission must be matched under Authority".
	Permissions PermissionFn
}

// permissionFn resolves the Authorization into the single PermissionFn the
// gates evaluate. Precedence: an explicit Permissions override wins; else
// build an all-required check over Authority; else exact match.
func (a Authorization) permissionFn() PermissionFn {
	if a.Permissions != nil {
		return a.Permissions
	}
	authority := a.Authority
	if authority == nil {
		authority = ExactAuthority
	}
	return func(id *Identity, required []string) bool {
		if id == nil {
			return false
		}
		for _, r := range required {
			if !identitySatisfies(id, r, authority) {
				return false
			}
		}
		return true
	}
}

// identitySatisfies reports whether any of the identity's roles or scopes
// satisfies the required permission under the given Authority.
func identitySatisfies(id *Identity, required string, authority Authority) bool {
	for _, g := range id.Roles {
		if authority(g, required) {
			return true
		}
	}
	for _, g := range id.Scopes {
		if authority(g, required) {
			return true
		}
	}
	return false
}
