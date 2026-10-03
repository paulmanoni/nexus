// Package state holds the app's shared page state.
package state

import "github.com/paulmanoni/nexus/v2/view"

// Search is the pet search: every component that Uses it on a page shares
// its signals. The DI instance holds the defaults; each page gets its own.
type Search struct {
	Query *view.Signal[string]
}

func NewSearch() *Search { return &Search{Query: view.Initial("")} }
