// Command example is a nexus app whose pages are reactive templ components,
// spread over packages — pages/ (the page), ui/ (layout and widgets, built
// on templUI), pets/ (the store and its components, including two shards)
// and state/ (shared page state, provided with DI).
//
//	nexus dev        (from this directory)
package main

import (
	"net/http"

	"github.com/templui/templui/utils"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/view"
	"github.com/paulmanoni/nexus/view/example/pets"
	"github.com/paulmanoni/nexus/view/example/state"
)

func app() []nexus.Option {
	templui := http.NewServeMux()
	utils.SetupScriptRoutes(templui, true)
	return []nexus.Option{
		nexus.Provide(pets.NewStore, state.NewSearch),
		view.Assets("/templui/js/", templui),
		view.Assets("/assets/", http.StripPrefix("/assets/", http.FileServer(http.Dir("assets")))),
	}
}

func main() { nexus.Boot(app()...) }
