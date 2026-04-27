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
// nexus.deploy.yaml — this file stays deployment-agnostic. The
// Listeners block below splits the manifest's bound port into a
// public surface (REST/GraphQL/WS) and a separate admin surface (the
// /__nexus dashboard) so the dashboard can be hidden from public
// traffic without touching the manifest.
//
// # Verifying the production-grade features
//
// After `nexus dev --split` is up, each subprocess listens on two
// ports: the public one declared in nexus.deploy.yaml and an admin
// one at +1000. Curl probes:
//
//	# health: 200 once fx Start completes — used by k8s liveness
//	curl -i http://localhost:8081/__nexus/health
//
//	# ready: 200 when alive AND every declared peer is reachable;
//	#        body shows per-peer probe state. Stop users-svc and
//	#        watch checkout-svc's /ready flip to 503.
//	curl http://localhost:8080/__nexus/ready | jq
//
//	# scope filter: dashboard 404s on the public port, served on
//	#               the admin port (+1000).
//	curl -o /dev/null -w 'public:%{http_code}\n' http://localhost:8081/__nexus/config
//	curl -o /dev/null -w 'admin: %{http_code}\n' http://localhost:9081/__nexus/config
//
//	# routing: same userId pins to one users-svc replica when scaled.
//	#          (See scenario_test.go for the in-process verification.)
//	curl -X POST http://localhost:8080/checkout \
//	     -d '{"userId":"u1","orderId":"o1"}' -H 'Content-Type: application/json'
package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/examples/microsplit/checkout"
	"github.com/paulmanoni/nexus/examples/microsplit/users"
)

func main() {
	publicAddr, adminAddr := resolveListenerAddrs()
	nexus.Run(
		nexus.Config{
			EnableDashboard: true,
			DashboardName:   "microsplit",
			// Public listener serves the user-facing surface
			// (REST + GraphQL); admin listener serves /__nexus/*
			// only. /__nexus/health and /__nexus/ready are also
			// available on the admin port (and would be on an
			// internal-scoped listener if we declared one).
			//
			// Both Addrs derive from nexus.deploy.yaml's port for
			// the active deployment, so monolith/users-svc/
			// checkout-svc each get a coherent pair without
			// touching this file.
			Listeners: map[string]nexus.Listener{
				"public": {Addr: publicAddr, Scope: nexus.ScopePublic},
				"admin":  {Addr: adminAddr, Scope: nexus.ScopeAdmin},
			},
		},
		users.Module,
		checkout.Module,
	)
}

// resolveListenerAddrs returns (public, admin) bind addresses derived
// from the active deployment's manifest port. The admin port is
// +1000 from the public port — far enough to avoid colliding with
// adjacent deployments in a side-by-side dev split, close enough that
// the relationship is obvious in logs. Falls back to :8080 + :9080
// when no DeploymentDefaults have been registered (plain `go run`
// without going through `nexus build` codegen).
func resolveListenerAddrs() (string, string) {
	defaults, ok := nexus.Defaults()
	if !ok || defaults.Addr == "" {
		return ":8080", ":9080"
	}
	host, port := splitHostPort(defaults.Addr)
	adminPort, err := strconv.Atoi(port)
	if err != nil {
		// Manifest port wasn't numeric — return the manifest addr
		// as public, leave admin empty (binds to a random port and
		// logs the bound addr at startup).
		return defaults.Addr, ""
	}
	return defaults.Addr, fmt.Sprintf("%s:%d", host, adminPort+1000)
}

// splitHostPort handles the manifest's typical ":8081" form and the
// fully qualified "0.0.0.0:8081" form alike. Returns ("", "8081")
// for the bare-port case so the admin addr keeps the same shape.
func splitHostPort(addr string) (host, port string) {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[:i], addr[i+1:]
	}
	return "", addr
}
