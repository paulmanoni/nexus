package nexus

import (
	"reflect"

	"github.com/paulmanoni/nexus/registry"
)

// Tag stamps one key/value pair onto an endpoint's registry tags — the
// metadata channel the dashboard, the client SDK manifest, and codegen read.
// It is the exported form of what WithIcon / HideFromDashboard / AuthRoute do
// internally, so an extension can mark the endpoints it registers without a
// dedicated option in this package:
//
//	// extension/inertia: mark a route as a page for the SDK's NexusPageProps
//	nexus.AsRest("GET", path, fn, nexus.Tag(registry.PageTag, component))
//
// Cross-transport (REST / GraphQL / WS). Keys are namespaced by convention
// ("<owner>.<name>", e.g. "inertia.page"); a later Tag with the same key
// replaces the earlier value. An empty key is ignored. Tags are metadata
// only — they never change how the endpoint routes or serves.
func Tag(key, value string) TagOption { return TagOption{key: key, value: value} }

// TagOption is the cross-transport carrier returned by Tag — implements
// RestOption, GqlOption, and WSOption so one expression flows through any
// endpoint builder, mirroring IconOption.
type TagOption struct{ key, value string }

func (o TagOption) applyToRest(c *restConfig) { o.apply(&c.baseEndpointConfig) }
func (o TagOption) applyToGql(c *gqlConfig)   { o.apply(&c.baseEndpointConfig) }
func (o TagOption) applyToWS(c *wsConfig)     { o.apply(&c.baseEndpointConfig) }

func (o TagOption) apply(b *baseEndpointConfig) {
	if o.key != "" {
		b.setTag(o.key, o.value)
	}
}

// RegisterSharedProp records the value type of a page-wide shared prop (an
// Inertia prop every page receives) so the client SDK can type it: t is
// walked into the same named-type pool endpoint schemas use — a named struct
// lands once in the manifest's refs — and the result is stored in the
// registry under key. The manifest projects the set as sharedProps and the
// generator emits it as NexusSharedProps. inertia.ShareScoped and
// inertia.ShareTyped call this; untyped providers (inertia.Share) are not
// recorded and ride NexusSharedProps' index signature.
//
// A nil t or empty key is ignored. Call during option wiring (an Invoke);
// the manifest is built after boot.
func (a *App) RegisterSharedProp(key string, t reflect.Type) {
	if key == "" || t == nil {
		return
	}
	a.registry.SetSharedProp(key, registry.WalkType(t, a.schemaRefs()))
}
