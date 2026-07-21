package nexus

// EndpointOption is the cross-transport per-op option contract: one value
// that every endpoint builder accepts — AsRest / AsRestHandler, AsQuery /
// AsMutation, and AsWS. It exists so "works on any transport" has a name
// instead of being an unwritten convention you infer from the fact that a
// type happens to implement RestOption, GqlOption, and WSOption all at once.
//
// The framework's built-in cross-transport options satisfy it: Public(),
// Describe(), WithIcon(), HideFromDashboard(), and Use() (hence
// auth.Required() / auth.Requires(), which return MiddlewareOption). The
// compile-time assertions below enforce that — add any new cross-transport
// option to the list so a regression that drops one transport fails to
// build rather than silently narrowing the option.
//
// Write your own cross-transport option by implementing the three
// applyTo* methods and returning EndpointOption:
//
//	func WithAudit(tag string) nexus.EndpointOption { return auditOption{tag} }
//
// Transport-specific options deliberately do NOT satisfy this — e.g. the
// GraphQL-only OnService returns GqlOption and is rejected by AsRest/AsWS at
// compile time, which is the intended guard.
type EndpointOption interface {
	RestOption
	GqlOption
	WSOption
}

// Compile-time proof that every built-in cross-transport option is a full
// EndpointOption. If a future edit drops (say) applyToWS from one of these,
// the build breaks here — the cheapest possible regression test.
var (
	_ EndpointOption = PublicOption{}
	_ EndpointOption = DescribeOption{}
	_ EndpointOption = IconOption{}
	_ EndpointOption = DashboardHiddenOption{}
	_ EndpointOption = MiddlewareOption{}
)
