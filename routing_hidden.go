package nexus

import "github.com/paulmanoni/nexus/registry"

// HideFromDashboard marks an endpoint as exempt from the introspection
// dashboard (/__nexus): it is dropped from /__nexus/endpoints, the live
// snapshot, and the architecture graph. The endpoint STILL routes and
// serves requests normally — this is dashboard-only visibility, not a 404
// and not an auth gate. Useful for internal/debug/health ops you don't want
// cluttering the topology. Works on REST (AsRest / AsRestHandler), GraphQL
// (AsQuery / AsMutation), and WS (AsWS):
//
//	nexus.AsRest("GET", "/internal/debug", NewDebug, nexus.HideFromDashboard())
//	nexus.AsQuery(NewInternalReport, nexus.HideFromDashboard())
//	nexus.AsWS("/events", "debug.tap", NewDebugTap, nexus.HideFromDashboard())
func HideFromDashboard() DashboardHiddenOption { return DashboardHiddenOption{} }

// DashboardHiddenOption is the cross-transport carrier returned by
// HideFromDashboard — implements RestOption, GqlOption, and WSOption so one
// expression flows through any endpoint builder, mirroring PublicOption.
// It stamps registry.HiddenTag on the endpoint's tags, which the registry's
// VisibleEndpoints() accessor (used by every dashboard data path) filters on.
type DashboardHiddenOption struct{}

func (DashboardHiddenOption) applyToRest(c *restConfig) { tagHidden(&c.tags) }
func (DashboardHiddenOption) applyToGql(c *gqlConfig)   { tagHidden(&c.tags) }
func (DashboardHiddenOption) applyToWS(c *wsConfig)     { tagHidden(&c.tags) }

func tagHidden(tags *map[string]string) {
	if *tags == nil {
		*tags = map[string]string{}
	}
	(*tags)[registry.HiddenTag] = "true"
}
