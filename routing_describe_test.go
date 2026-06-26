package nexus

import (
	"testing"

	"github.com/paulmanoni/nexus/registry"
)

// TestDescribe_CrossTransport proves nexus.Describe sets the registry
// Description on every transport — REST, GraphQL, and WS — from a single
// option value, the way HideFromDashboard and WithIcon already do. This is the
// contract that lets one helper supersede the transport-specific Desc/Description.
func TestDescribe_CrossTransport(t *testing.T) {
	type args struct {
		ID string `uri:"id" graphql:"id"`
	}
	newGet := func(p Params[args]) (string, error) { return "ok", nil }
	newSearch := func(p Params[args]) (string, error) { return "ok", nil }
	newSend := func(sess *WSSession, p Params[args]) error { return nil }

	app, err := newApp(Config{Server: ServerConfig{Addr: "127.0.0.1:0"}},
		AsRest("GET", "/items/:id", newGet, Describe("Fetch one item")),
		AsQuery(newSearch, Describe("Search items")),
		AsWS("/events", "item.send", newSend, Describe("Send an item event")),
	)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	defer app.Stop()

	// One endpoint per transport was registered above; index the description
	// each carries by transport and assert Describe reached all three.
	want := map[registry.Transport]string{
		registry.REST:      "Fetch one item",
		registry.GraphQL:   "Search items",
		registry.WebSocket: "Send an item event",
	}
	got := map[registry.Transport]string{}
	for _, e := range app.Registry().Endpoints() {
		if _, tracked := want[e.Transport]; tracked {
			got[e.Transport] = e.Description
		}
	}
	for tr, exp := range want {
		if got[tr] != exp {
			t.Errorf("%s endpoint: Description = %q, want %q", tr, got[tr], exp)
		}
	}
}
