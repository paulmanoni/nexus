package auth

import (
	"context"
	"strings"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/registry"
)

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

// OpGates answers "which registered ops may this user call?" — keyed by op
// name (the GraphQL field / REST endpoint name the frontend already uses),
// derived from each endpoint's own auth.Requires declaration. The
// permission codenames therefore live in exactly one place, the
// registration; the frontend never sees them.
//
// The canonical wiring is a Scoped fact projected with ShareScoped, so the
// shared "can" prop and every handler that Gets it share ONE evaluation per
// request (the ctor gets *nexus.App via DI — it does not exist at package
// declaration time):
//
//	var CanGates = nexus.NewScoped[map[string]bool](
//	    func(app *nexus.App) nexus.Compute[map[string]bool] {
//	        return func(ctx context.Context) (map[string]bool, error) {
//	            return auth.OpGates(ctx, app), nil
//	        }
//	    })
//
//	nexus.Boot(CanGates, inertia.ShareScoped("can", CanGates), ...)
//	// SPA: v-if="can.saveUser"
//
// The map's keys are OP NAMES — it can only ever contain ops, and only
// permissions some registration stamped. A permission that gates no op
// (a UI-section codename) is invisible here by construction; check
// holdings for those (auth.Can / auth.Gates, or a Scoped over the
// identity's permission list).
//
// Semantics: an op whose registration carries auth.Requires is true only
// when the identity passes the same PermissionFn / Backend.Authorize the
// gate itself runs (anonymous → false); an op with no Requires declaration
// is always true — OpGates reports permission gates, not authentication.
//
// Performance: the registry is compiled once per registry version into a
// gate table with ops GROUPED BY their permission set, cached on the App.
// A call therefore does one map lookup plus one permission check per
// UNIQUE permission set (not per op), then fills the result map — cheap
// enough for an Inertia shared prop evaluated on every page render.
func OpGates(ctx context.Context, app *nexus.App) map[string]bool {
	t := gateTableFor(app)
	out := make(map[string]bool, t.total)
	for _, op := range t.open {
		out[op] = true
	}
	id, authed := IdentityFrom(ctx)
	for i := range t.groups {
		g := &t.groups[i]
		allowed := authed && checkPermissions(ctx, id, g.perms)
		for _, op := range g.ops {
			out[op] = allowed
		}
	}
	return out
}

type gateGroup struct {
	perms []string
	ops   []string
}

type gateTable struct {
	version uint64
	open    []string // ops with no Requires declaration — always true
	groups  []gateGroup
	total   int
}

// opGatesKey is the App.Value slot holding the compiled *gateTable.
type opGatesKey struct{}

// gateTableFor returns the compiled gate table for the app's current
// registry version, rebuilding only when the registry has changed since
// the cached compile. The benign race (two goroutines compiling the same
// version) is harmless: the build is pure and last-write-wins.
func gateTableFor(app *nexus.App) *gateTable {
	v := app.Registry().Version()
	if cached, ok := app.Value(opGatesKey{}); ok {
		if t, ok := cached.(*gateTable); ok && t.version == v {
			return t
		}
	}
	t := buildGateTable(app.Registry(), v)
	app.SetValue(opGatesKey{}, t)
	return t
}

func buildGateTable(reg *registry.Registry, version uint64) *gateTable {
	t := &gateTable{version: version}
	groups := map[string]int{} // joined perms string → index into t.groups
	seen := map[string]bool{}
	for _, e := range reg.Endpoints() {
		if e.Name == "" || seen[e.Name] {
			continue
		}
		seen[e.Name] = true
		t.total++
		tag := e.Tags[registry.AuthRequiresTag]
		if tag == "" {
			t.open = append(t.open, e.Name)
			continue
		}
		idx, ok := groups[tag]
		if !ok {
			idx = len(t.groups)
			groups[tag] = idx
			t.groups = append(t.groups, gateGroup{perms: splitPerms(tag)})
		}
		t.groups[idx].ops = append(t.groups[idx].ops, e.Name)
	}
	return t
}

// splitPerms parses the comma-joined AuthRequiresTag value, dropping
// duplicates so stacked Requires bundles naming the same permission cost
// one check, not two.
func splitPerms(tag string) []string {
	parts := strings.Split(tag, ",")
	out := parts[:0]
	seen := map[string]bool{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}
