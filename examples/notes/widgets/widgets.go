// Package widgets is a stand-in for an extension (think inertia) that ships its
// OWN decorator. It exposes a decorator-form registrar, Panel, that records
// into the shared decorate registry — so `//@widgets.Panel "/path"` on a
// handler generates a call to it, and the result flows through
// decorate.Module(...) exactly like the built-in //@rest.
package widgets

import (
	"github.com/paulmanoni/nexus"
)

// Stats is a panel payload (a widgets type a handler returns — which also means
// the handler's file imports this package, so the codegen can resolve the
// //@widgets.Panel import).
type Stats struct {
	Count int `json:"count"`
}

// Icon is the lucide-style icon this extension brands its endpoints with on the
// dashboard.
const Icon = "layout-panel-top"

// Panel is the custom decorator's registrar. Like every nexus registrar it
// RETURNS a nexus.Option (here a REST endpoint under /widgets, branded with the
// extension's own icon via nexus.WithIcon). The codegen records the returned
// option — decorate.Record(widgets.Panel(...)) — so the endpoint shows up on
// the dashboard as a widgets endpoint (just as inertia.Page brands its pages).
//
//	//@widgets.Panel "/stats"   → decorate.Record(widgets.Panel("/stats", NewStatsPanel))
func Panel(path string, ctor any) nexus.Option {
	return nexus.AsRest("GET", "/widgets"+path, ctor, nexus.WithIcon(Icon))
}
