// Command inertia is a small Inertia app whose pages are registered with the
// //@inertia.Page custom decorator (see ./pages). It shows an extension
// shipping its own decorator that reuses its existing Option-returning
// registrar (inertia.Page), with its pages branded by the inertia icon on the
// dashboard.
//
//	nexus generate handlers ./examples/inertia   # writes pages/nexus_handlers_gen.go
//	go run ./examples/inertia                    # /users, /login + /__nexus dashboard
//
// XHR (Inertia) visit returns the JSON page object:
//	curl -s -H 'X-Inertia: true' http://localhost:8080/users
package main

import (
	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension/inertia"
)

func main() {
	// No pages import, no decorate.Module: `nexus generate handlers` writes a
	// blank-import aggregator that pulls in the pages package, and nexus.Run
	// auto-drains its //@inertia.Page registrations.
	nexus.Run(
		nexus.Config{
			Introspection: true,
			Dashboard:     nexus.DashboardConfig{Enabled: true, Name: "Inertia"},
			Server:        nexus.ServerConfig{Addr: ":8080"},
		},
		inertia.Module(inertia.Config{}), // RootView defaults to "app"
	)
}
