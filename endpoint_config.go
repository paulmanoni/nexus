package nexus

import "github.com/paulmanoni/nexus/middleware"

// baseEndpointConfig holds the fields every transport-specific config
// (gqlConfig, restConfig, wsConfig) shares: dashboard description, the
// enclosing module name + deployment tag, and the cross-transport
// middleware bundles attached via nexus.Use.
//
// Transport configs embed it so adding a new shared field — a tag map,
// an audit hook, etc. — is a one-line change instead of three. The
// setModule/setDeployment methods below also satisfy the
// moduleAnnotator + deploymentAnnotator interfaces for free, so each
// transport's option struct only needs to delegate.
type baseEndpointConfig struct {
	// description is the human-readable string shown on the dashboard
	// and (where the transport supports it) in generated SDL.
	description string

	// module is stamped by nexus.Module("name", ...) when this option
	// is a direct child of a module. Populates the registry entry's
	// Module field so the dashboard groups endpoints by module.
	module string

	// deployment is stamped by nexus.DeployAs in the enclosing module.
	// Empty for always-local endpoints; populated only when the parent
	// Module declares a deployment tag.
	deployment string

	// bundles holds the full middleware.Middleware values attached via
	// nexus.Use — the registry uses AsInfo() from each to label the
	// endpoint's middleware list. Per-transport realizations (Gin,
	// Graph) are extracted at apply time; this slice is the canonical
	// metadata source for the dashboard.
	bundles []middleware.Middleware
}

func (b *baseEndpointConfig) setModule(name string)    { b.module = name }
func (b *baseEndpointConfig) setDeployment(tag string) { b.deployment = tag }