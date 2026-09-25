package nexus

import (
	"reflect"
	"testing"

	"github.com/paulmanoni/nexus/registry"
)

// TestTag_CrossTransport proves nexus.Tag stamps the same key/value on every
// transport's registry entry — the channel extensions (inertia.Page) use to
// mark endpoints without an option of their own in this package.
func TestTag_CrossTransport(t *testing.T) {
	type args struct {
		ID string `uri:"id" graphql:"id"`
	}
	newGet := func(p Params[args]) (string, error) { return "ok", nil }
	newSearch := func(p Params[args]) (string, error) { return "ok", nil }
	newSend := func(sess *WSSession, p Params[args]) error { return nil }

	app, err := newApp(Config{Server: ServerConfig{Addr: "127.0.0.1:0"}},
		AsRest("GET", "/items/:id", newGet, Tag("x.kind", "rest"), Tag("", "ignored")),
		AsQuery(newSearch, Tag("x.kind", "gql")),
		AsWS("/events", "item.send", newSend, Tag("x.kind", "ws")),
	)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	defer app.Stop()

	want := map[registry.Transport]string{
		registry.REST:      "rest",
		registry.GraphQL:   "gql",
		registry.WebSocket: "ws",
	}
	for _, e := range app.Registry().Endpoints() {
		exp, tracked := want[e.Transport]
		if !tracked {
			continue
		}
		if got := e.Tags["x.kind"]; got != exp {
			t.Errorf("%s endpoint: Tags[x.kind] = %q, want %q", e.Transport, got, exp)
		}
		if _, ok := e.Tags[""]; ok {
			t.Errorf("%s endpoint: empty-key Tag must be ignored, got tags %v", e.Transport, e.Tags)
		}
		delete(want, e.Transport)
	}
	if len(want) != 0 {
		t.Errorf("no endpoint registered for transports %v", want)
	}
}

// TestRegisterSharedProp walks a shared prop's type into the registry and the
// shared named-type pool, so a named struct reaches the manifest's refs the
// same way an endpoint schema's would.
func TestRegisterSharedProp(t *testing.T) {
	type Viewer struct {
		Name string `json:"name"`
	}
	app := New(Config{})
	app.RegisterSharedProp("can", reflect.TypeOf(map[string]bool{}))
	app.RegisterSharedProp("viewer", reflect.TypeOf(&Viewer{}))
	app.RegisterSharedProp("", reflect.TypeOf(""))
	app.RegisterSharedProp("nil", nil)

	got := app.Registry().SharedProps()
	if len(got) != 2 {
		t.Fatalf("SharedProps = %v, want exactly can + viewer", got)
	}
	if c := got["can"]; c.Kind != "map" || c.Of == nil || c.Of.Primitive != "boolean" {
		t.Errorf("can = %+v, want map of boolean", c)
	}
	if v := got["viewer"]; v.Kind != "ref" || v.Ref != "Viewer" || !v.Optional {
		t.Errorf("viewer = %+v, want optional ref Viewer", v)
	}
	if _, ok := app.SchemaRefs()["Viewer"]; !ok {
		t.Errorf("Viewer missing from the schema refs pool: %v", app.SchemaRefs())
	}
}

// Keys an option in this package owns are refused: auth.public / auth.flow
// would exempt a route from the default auth gate behind the back of the
// options that document it.
func TestTag_RefusesFrameworkKeys(t *testing.T) {
	for _, key := range []string{PublicTag, AuthFlowTag, registry.AuthRequiresTag, registry.HiddenTag, registry.IconTag, registry.EnvelopeTag, registry.ProxyTag} {
		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("Tag(%q) did not panic", key)
				}
			}()
			Tag(key, "x")
		}()
	}
	Tag(registry.PageTag, "Users/Index") // an extension's own key is fine
}
