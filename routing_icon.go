package nexus

import "github.com/paulmanoni/nexus/registry"

// WithIcon sets the dashboard icon for an endpoint — a lucide-style icon name
// rendered on the endpoint's node in the architecture graph and the endpoints
// list. Its main use is branding: an extension that registers endpoints through
// a custom decorator stamps its OWN icon so its endpoints are recognizable in
// the topology (e.g. inertia.Page → the inertia icon, widgets.Panel → a panel
// icon). Cross-transport: works on REST, GraphQL, and WS.
//
//	// inside an extension's decorator-form registrar:
//	decorate.Record(nexus.AsRest("GET", "/widgets"+path, ctor, nexus.WithIcon("layout-panel-top")))
//
// The endpoint still routes and serves normally; this is dashboard presentation
// only. An empty name is ignored (the per-transport default icon shows).
func WithIcon(name string) IconOption { return IconOption{name: name} }

// IconOption is the cross-transport carrier returned by WithIcon — implements
// RestOption, GqlOption, and WSOption so one expression flows through any
// endpoint builder, mirroring HideFromDashboard. It stamps registry.IconTag on
// the endpoint's tags, which the dashboard reads when rendering the node.
type IconOption struct{ name string }

func (o IconOption) applyToRest(c *restConfig) { tagIcon(&c.tags, o.name) }
func (o IconOption) applyToGql(c *gqlConfig)   { tagIcon(&c.tags, o.name) }
func (o IconOption) applyToWS(c *wsConfig)     { tagIcon(&c.tags, o.name) }

func tagIcon(tags *map[string]string, name string) {
	if name == "" {
		return
	}
	if *tags == nil {
		*tags = map[string]string{}
	}
	(*tags)[registry.IconTag] = name
}
