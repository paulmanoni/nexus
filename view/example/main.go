// Command example is a nexus app whose pages are reactive templ components,
// spread over packages — pages/ (the page), ui/ (layout and widgets, built
// on templUI), pets/ (the store and its components, including two shards)
// and state/ (shared page state, provided with DI) — plus two Vue islands
// from the Vite project in web/ (web/src/islands).
//
//	nexus dev        (from this directory)
package main

import (
	"embed"
	"net/http"

	"github.com/templui/templui/utils"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/view"
	"github.com/paulmanoni/nexus/view/example/v2/pets"
	"github.com/paulmanoni/nexus/view/example/v2/state"
)

//go:embed all:web/dist
var webFS embed.FS

func app() []nexus.Option {
	templui := http.NewServeMux()
	utils.SetupScriptRoutes(templui, true)
	return []nexus.Option{
		nexus.Provide(pets.NewStore, state.NewSearch),
		pets.Module,
		view.Assets("/templui/js/", templui),
		view.Assets("/assets/", http.StripPrefix("/assets/", http.FileServer(http.Dir("assets")))),
		nexus.ServeFrontend(webFS, "web/dist"),
	}
}

func main() { nexus.Boot(app()...) }
