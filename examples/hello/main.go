// Command hello is the smallest possible live-template app —
// one component, one route, no state, no dependencies beyond
// the framework itself. Used as the baseline for measuring
// boot time:
//
//	go build -o /tmp/hello ./examples/hello
//	./bench.sh    (in this directory)
//
// The "framework overhead" you see in the bench is whatever
// nexus.Run + fx graph build + dashboard mount + gin engine
// init + listener bind take. Adding a real app on top is
// additive — your component's Mount runs once per session, not
// at boot.
//
// Set NEXUS_HELLO_NODASH=1 to disable the /__nexus dashboard
// and isolate the live-template engine's contribution.
package main

import (
	"embed"
	"os"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/live/template"
)

//go:embed templates/*.nlt
var assets embed.FS

type Hello struct {
	template.BaseComponent
}

func NewHello() (*Hello, error) { return &Hello{}, nil }

var liveModule = nexus.Module("hello",
	template.Module(assets),
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
