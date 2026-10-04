package auth

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/registry"
)

// The optional permission catalogue ([auth] perms): when an app declares
// its permissions, a gate or role naming one it didn't declare fails boot,
// and Can / Gates / Check with an undeclared name are logged.

// checkCatalogue fails boot for a permission a registered gate or a role
// names that the catalogue doesn't hold. It runs once every route is
// registered (nexus.Setup).
func (st *moduleState) checkCatalogue(app *nexus.App) error {
	rs := st.config.settings
	if rs == nil || len(rs.perms) == 0 {
		return nil
	}
	known := map[string]bool{}
	for _, p := range rs.perms {
		known[p] = true
	}
	var bad []string
	for _, e := range app.Registry().Endpoints() {
		for _, tag := range []string{registry.AuthRequiresTag, registry.AuthRequiresAnyTag} {
			for _, group := range strings.Split(e.Tags[tag], ";") {
				for _, p := range splitPerms(group) {
					if !known[p] {
						bad = append(bad, fmt.Sprintf("%q (gate on %s)", p, endpointName(e)))
					}
				}
			}
		}
	}
	roles := make([]string, 0, len(rs.roles))
	for r := range rs.roles {
		roles = append(roles, r)
	}
	sort.Strings(roles)
	for _, r := range roles {
		for _, p := range rs.roles[r] {
			if !coversCatalogue(p, rs.perms) {
				bad = append(bad, fmt.Sprintf("%q (role %s)", p, r))
			}
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("auth: permissions not in [auth] perms: %s — declare them or fix the spelling", strings.Join(bad, ", "))
	}
	return nil
}

// coversCatalogue reports whether a granted permission (maybe a wildcard)
// matches a catalogue entry: itself, or for "x.*" / "*" at least one entry.
func coversCatalogue(granted string, perms []string) bool {
	probe := &Identity{Perms: []string{granted}}
	for _, p := range perms {
		if p == granted || probe.grants(p) {
			return true
		}
	}
	return false
}

func endpointName(e registry.Endpoint) string {
	if e.Transport == registry.GraphQL || e.Path == "" {
		return e.Name
	}
	return e.Method + " " + e.Path
}

var undeclared sync.Map // perm → logged

// noteUndeclared logs, once per permission, a Can/Gates/Check naming one
// the catalogue doesn't hold.
func noteUndeclared(st *moduleState, perm string) {
	rs := st.config.settings
	if rs == nil || len(rs.perms) == 0 || perm == "" || coversCatalogue(perm, rs.perms) {
		return
	}
	if _, seen := undeclared.LoadOrStore(perm, true); seen || st.app == nil {
		return
	}
	st.app.Logger().Warn("auth: permission " + perm + " is checked but not declared in [auth] perms")
}
