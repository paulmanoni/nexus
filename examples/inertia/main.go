// Command inertia is a small, runnable Inertia app: a Vue client under web/
// driven by Go page handlers registered with the //@inertia.Page custom
// decorator (see ./pages). It shows an extension shipping its own decorator
// that reuses its existing Option-returning registrar (inertia.Page), with its
// pages branded by the inertia icon on the dashboard.
//
//	nexus dev ./examples/inertia    # zero-install: viteless serves web/ with HMR,
//	                                # proxying to the Go app — open the printed URL
//	nexus build ./examples/inertia  # bundle web/dist, then a single Go binary
//
// `go run ./examples/inertia` serves the API + dashboard, but the page shell
// needs a built bundle (or `nexus dev`) for the Vue client to mount.
//
// XHR (Inertia) visit returns the JSON page object:
//
//	curl -s -H 'X-Inertia: true' http://localhost:8080/users
package main

import (
	"embed"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension/inertia"
)

//go:embed all:web/dist
var webFS embed.FS

func main() {
	// No pages import, no decorate.Module: `nexus generate handlers` writes a
	// blank-import aggregator that pulls in the pages package, and nexus.Run
	// auto-drains its //@inertia.Page registrations.
	//
	// ServeFrontend names + serves the bundle once; inertia.Module auto-discovers
	// it (via App.FrontendFS) to read the manifest for the shell + asset version.
	nexus.Run(
		nexus.Config{
			Introspection: true,
			Dashboard:     nexus.DashboardConfig{Enabled: true, Name: "Inertia"},
			Server:        nexus.ServerConfig{Addr: ":8080"},
		},
		nexus.ServeFrontend(webFS, "web/dist"),
		inertia.Module(inertia.Config{}),
	)
}
