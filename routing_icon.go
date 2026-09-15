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

func (o IconOption) applyToRest(c *restConfig) { o.apply(&c.baseEndpointConfig) }
func (o IconOption) applyToGql(c *gqlConfig)   { o.apply(&c.baseEndpointConfig) }
func (o IconOption) applyToWS(c *wsConfig)     { o.apply(&c.baseEndpointConfig) }

func (o IconOption) apply(b *baseEndpointConfig) {
	if o.name != "" {
		b.setTag(registry.IconTag, o.name)
	}
}
