package auth

import (
	"context"
	"slices"
	"strings"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/registry"
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
		allowed := authed && g.allows(ctx, id)
		for _, op := range g.ops {
			out[op] = allowed
		}
	}
	// On the config-driven path every op that isn't Public needs a sign-in,
	// and one under an area a kind it admits.
	if st, ok := stateFrom(ctx); ok && st.config.settings != nil {
		rs := st.config.settings
		for _, m := range t.meta {
			if m.public || !out[m.name] {
				continue
			}
			a := rs.area(m.path)
			if !authed && (a != nil || !rs.public) || authed && a != nil && !kindIn(id.Kind, a.Kinds) {
				out[m.name] = false
			}
		}
	}
	return out
}

// opMeta is what OpGates needs of an op beyond its gates.
type opMeta struct {
	name, path string
	public     bool
}

// gateGroup is the ops sharing one gate: every perm (Requires), at least
// one of each anyOf group (RequiresAny), a kind in each kinds group (Kind).
type gateGroup struct {
	perms []string
	anyOf [][]string
	kinds [][]string
	ops   []string
}

func (g *gateGroup) allows(ctx context.Context, id *Identity) bool {
	if len(g.perms) > 0 && !checkPermissions(ctx, id, g.perms) {
		return false
	}
	for _, any := range g.anyOf {
		if !anyPermission(ctx, id, any) {
			return false
		}
	}
	for _, kinds := range g.kinds {
		if !slices.Contains(kinds, id.Kind) {
			return false
		}
	}
	return true
}

type gateTable struct {
	version uint64
	meta    []opMeta
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
		// A page is also known by its component (pets.Board), so a view's
		// navigation can ask about the page without knowing its route.
		names := []string{e.Name}
		if v := e.Tags[registry.ViewTag]; (v == "page" || v == "live") && e.Tags[registry.ViewComponentTag] != "" {
			names = append(names, e.Tags[registry.ViewComponentTag])
		}
		tag := e.Tags[registry.AuthRequiresTag]
		anyTag, kindTag := e.Tags[registry.AuthRequiresAnyTag], e.Tags[registry.AuthKindTag]
		key := tag
		if anyTag != "" || kindTag != "" {
			key = tag + "|" + anyTag + "|" + kindTag
		}
		for _, name := range names {
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			t.total++
			t.meta = append(t.meta, opMeta{name: name, path: e.Path, public: e.Tags[nexus.PublicTag] != ""})
			if key == "" {
				t.open = append(t.open, name)
				continue
			}
			idx, ok := groups[key]
			if !ok {
				idx = len(t.groups)
				groups[key] = idx
				g := gateGroup{anyOf: splitGroups(anyTag), kinds: splitGroups(kindTag)}
				if tag != "" {
					g.perms = splitPerms(tag)
				}
				t.groups = append(t.groups, g)
			}
			t.groups[idx].ops = append(t.groups[idx].ops, name)
		}
	}
	return t
}

// splitGroups parses a ";"-joined list of comma-joined groups.
func splitGroups(tag string) [][]string {
	if tag == "" {
		return nil
	}
	var out [][]string
	for _, g := range strings.Split(tag, ";") {
		out = append(out, splitPerms(g))
	}
	return out
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
