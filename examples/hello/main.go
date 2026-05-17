// Command hello is the smallest live-template app with all the
// surface area you actually use:
//
//   - a typed component with state ({{ Count }})
//   - a server-side handler (@click="bump")
//   - a server → island push (@click="pingIsland" calls PushIsland)
//   - an embedded vanilla JS island under islands/
//   - WithStatic auto-mounting the islands/ subdir at /islands/
//
// Still ~50 lines of Go + ~30 of template + ~30 of JS. Doubles as
// the boot-time bench baseline (see bench.sh in this directory);
// adding the island doesn't move the needle on boot time because
// islands are dynamic-imported lazily by the browser, not by the
// Go boot path.
//
// Run:
//
//	go run ./examples/hello
//	→ http://localhost:8083
//
// Set NEXUS_HELLO_NODASH=1 to skip the /__nexus dashboard mount
// and isolate the live-template engine's contribution to boot.
package main

import (
	"os"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/live/template"
)

// No //go:embed here — template.Module() defaults to reading
// from disk (os.DirFS at cwd). Run with `go run .` from
// examples/hello/ and the engine picks up templates/Hello.nlt
// + islands/counter.js automatically. For a self-contained
// production binary, add //go:embed + template.WithFS(assets).

type Hello struct {
	template.BaseComponent
	Count int
}

func NewHello() (*Hello, error) { return &Hello{}, nil }

// Bump increments the server-side counter; the next diff
// ships only "{{ Count }}"'s new value (a couple of bytes).
func (h *Hello) Bump(_ *template.Ctx) { h.Count++ }

// PingIsland targets the Counter island via PushIsland. The
// surrounding live template does NOT re-render — the island's
// channel.on("ping") listener fires directly.
func (h *Hello) PingIsland(ctx *template.Ctx) {
	ctx.PushIsland("Counter", "ping", nil)
}

// ClientProps is what :nl-island-props evaluates to on every
// render. Stays small — just the initial value the island
// reads on mount.
func (h *Hello) ClientProps() map[string]any {
	return map[string]any{"initial": 0}
}

var liveModule = nexus.Module("hello",
	template.Module(
		// Serves the islands/ subdir of the cwd at /islands/
		// — counter.js is what /islands/counter.js maps to.
		template.WithStatic("islands"),
	),
	nexus.AsComponent("Hello", NewHello,
		template.WithTemplate("templates/Hello"),
		nexus.Path("/"),
	),
)

func main() {
	dashEnabled := os.Getenv("NEXUS_HELLO_NODASH") != "1"
	nexus.Run(
		nexus.Config{
			Server:    nexus.ServerConfig{Addr: ":8083"},
			Dashboard: nexus.DashboardConfig{Enabled: dashEnabled, Name: "Hello"},
		},
		liveModule,
	)
}
