// Command microsplit is the runnable demo for the deployable-modules
// preview. The same binary boots as a monolith (no env vars set) or as
// a single split unit when NEXUS_DEPLOYMENT is set:
//
//	# monolith — both modules in one process
//	go run .
//	curl -X POST localhost:8080/checkout -d '{"userId":"u1","orderId":"o7"}' \
//	     -H 'content-type: application/json'
//
//	# split — users on :8081, checkout on :8080 talking to it
//	NEXUS_DEPLOYMENT=users-svc PORT=8081 go run . &
//	NEXUS_DEPLOYMENT=checkout USERS_SVC_URL=http://localhost:8081 PORT=8080 go run .
//
// (For the v0.9 preview we don't filter out remote modules from the
// graph yet — both modules are wired in either binary, but the
// generated UsersClient picks the right transport based on the
// binary's deployment.)
package main

import (
	"os"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/examples/microsplit/checkout"
	"github.com/paulmanoni/nexus/examples/microsplit/users"
)

func main() {
	addr := ":8080"
	if p := os.Getenv("PORT"); p != "" {
		addr = ":" + p
	}
	nexus.Run(
		nexus.Config{
			Addr:            addr,
			EnableDashboard: true,
			Deployment:      nexus.DeploymentFromEnv(),
			DashboardName:   "microsplit",
		},
		users.Module,
		checkout.Module,
	)
}