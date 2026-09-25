// Command inertia is a small, runnable Inertia app: a Vue client under web/
// driven by Go page handlers registered with the //@inertia.Page custom
// decorator (see ./pages). It shows an extension shipping its own decorator
// that reuses its existing Option-returning registrar (inertia.Page), with its
// pages branded by the inertia icon on the dashboard.
//
//	nexus dev ./examples/inertia    # installs web/ deps (npm) on first run, runs Vite
//	                                # beside the app — open the app URL it prints
//	nexus build ./examples/inertia  # vite build → web/dist, then a single Go binary
//
// web/ is an ordinary Vite project; nexus-vite-plugin (web/sdk) tells the app
// where the dev server is, so pages served on the app's origin load their
// modules from Vite with HMR. `go run ./examples/inertia` serves the API and
// dashboard, but the pages need a built bundle (or a running Vite) to mount.
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
	// it and renders pages into its index.html (Vite's in dev, the built one
	// in production), reading the manifest for the asset version.
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
