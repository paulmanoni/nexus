// Command microsplit is the runnable demo for the deployable-modules
// preview. The same binary boots as a monolith (no env vars set) or as
// a single split unit when `nexus build --deployment X` produces it:
//
//	# monolith — both modules in one process
//	nexus build --deployment monolith
//	./bin/monolith
//
//	# split — users on :8081, checkout on :8080 talking to it
//	nexus build --deployment users-svc
//	nexus build --deployment checkout-svc
//	./bin/users-svc &
//	./bin/checkout-svc
//
// Or run both at once with auto-wiring:
//
//	nexus dev --split
//
// All deployment-specific config (port, peers, timeouts) lives in
// nexus.deploy.yaml — this file stays deployment-agnostic.
package main

import (
	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/examples/microsplit/checkout"
	"github.com/paulmanoni/nexus/examples/microsplit/users"
)

func main() {
	nexus.Run(
		nexus.Config{
			EnableDashboard: true,
			DashboardName:   "microsplit",
		},
		users.Module,
		checkout.Module,
	)
}
