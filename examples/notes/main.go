// Command notes is a small, real app showing decorator-form registration
// (//@provide / //@rest / //@query in the notes package) coexisting with an
// explicit hand-written endpoint — all on one DI container and one dashboard.
//
//	nexus generate handlers ./examples/notes   # writes notes/nexus_handlers_gen.go
//	go run ./examples/notes                    # REST + GraphQL + /__nexus dashboard
//
// In dev you'd just run `nexus dev ./examples/notes` — the generate step is
// injected via overlay, so no committed file is needed to iterate.
package main

import (
	"github.com/paulmanoni/nexus"
)

type pingResp struct {
	Pong bool `json:"pong"`
}

// newPing is a plain explicit handler defined right here in main — it shows the
// decorator-form (the notes/widgets packages) and the explicit API coexisting,
// without main importing any handler package.
func newPing(p nexus.Params[struct{}]) (*pingResp, error) {
	return &pingResp{Pong: true}, nil
}

func main() {
	// No decorate.Module(...) and no handler imports: `nexus generate handlers`
	// emits a blank-import aggregator (nexus_imports_gen.go) that pulls the
	// annotated packages in, and nexus.Run auto-drains their registrations.
	// Introspection opens /__nexus; Dashboard.Enabled mounts the UI.
	nexus.Run(
		nexus.Config{
			Introspection: true,
			Dashboard:     nexus.DashboardConfig{Enabled: true, Name: "Notes"},
			Server:        nexus.ServerConfig{Addr: ":8080"},
		},
		nexus.Module("ops", nexus.AsRest("GET", "/ping", newPing)),
	)
}
