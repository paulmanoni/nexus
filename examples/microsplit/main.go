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
// All deployment-specific config (port, peers, timeouts, listeners)
// lives in nexus.deploy.yaml — this file stays deployment-agnostic.
// The manifest's listeners block adds an admin listener at port+1000
// so the dashboard sits off the public surface.
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
	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/examples/microsplit/checkout"
	"github.com/paulmanoni/nexus/examples/microsplit/users"
)

func main() {
	nexus.Run(
		nexus.Config{
			Dashboard: nexus.DashboardConfig{Enabled: true, Name: "microsplit"},
		},
		users.Module,
		checkout.Module,
	)
}
