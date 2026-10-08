package view

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/a-h/templ"
	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/registry"
)

// Pages, live pages and shards carry view tags the dashboard reads: their
// kind, a live page's type and its events with argument types.
func TestViewEndpointsAreTagged(t *testing.T) {
	app, stop, err := nexus.InProcess(config.Runtime{},
		nexus.Supply(&greeter{greeting: "hi"}),
		Live[*counterLive]("/counter/:name").Provide(newCounterLive),
		Page("GET", "/about", func() templ.Component { return templ.Raw("about") }),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(context.Background())
	byPath := map[string]registry.Endpoint{}
	for _, e := range app.Registry().Endpoints() {
		byPath[e.Path] = e
	}
	live := byPath["/counter/:name"]
	if live.Tags[registry.ViewTag] != "live" || live.Tags[registry.ViewComponentTag] != "view.counterLive" {
		t.Errorf("live page tags = %v", live.Tags)
	}
	if got := live.Tags[registry.ViewEventsTag]; got != "Add(int); Reset()" {
		t.Errorf("events = %q", got)
	}
	if about := byPath["/about"]; about.Tags[registry.ViewTag] != "page" {
		t.Errorf("page tags = %v", about.Tags)
	}
}

// A live event is its own trace: a root span named Type.Event carrying how
// long it took and how much the reply carried.
func TestLiveEventIsTraced(t *testing.T) {
	app, stop, err := nexus.InProcess(config.Runtime{TraceCapacity: 100},
		nexus.Supply(&greeter{greeting: "hi"}),
		Live[*counterLive]("/count/:name").Provide(newCounterLive),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(context.Background())
	srv := httptest.NewServer(app)
	defer srv.Close()

	conn := dialLive(t, srv, "/count/ana/_live")
	reply(t, conn)
	raw, _ := json.Marshal(5)
	if err := conn.WriteJSON(liveEvent{Event: "Add", Args: []json.RawMessage{raw}}); err != nil {
		t.Fatal(err)
	}
	reply(t, conn)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range app.Bus().Recent() {
			if e.Endpoint == "view.counterLive.Add" && e.Meta["live.bytes"] != nil {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	var seen []string
	for _, e := range app.Bus().Recent() {
		seen = append(seen, string(e.Kind)+" "+e.Endpoint)
	}
	t.Fatalf("no live.Add span among %v", seen)
}

// A patch is its own trace too, not part of the long-gone upgrade request:
// two patches on one socket are two traces.
func TestLivePatchIsItsOwnTrace(t *testing.T) {
	app, stop, err := nexus.InProcess(config.Runtime{TraceCapacity: 100},
		nexus.Supply(&greeter{greeting: "hi"}),
		Live[*counterLive]("/count/:name").Provide(newCounterLive),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(context.Background())
	srv := httptest.NewServer(app)
	defer srv.Close()

	conn := dialLive(t, srv, "/count/ana/_live")
	reply(t, conn)
	for i, u := range []string{"/count/ana?mode=a", "/count/ana?mode=b"} {
		if err := conn.WriteJSON(liveEvent{Ref: i + 1, Event: "__nav", URL: u}); err != nil {
			t.Fatal(err)
		}
		reply(t, conn)
	}

	traces := map[string]bool{}
	for deadline := time.Now().Add(2 * time.Second); len(traces) < 2 && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		for _, e := range app.Bus().Recent() {
			if e.Endpoint == "view.counterLive.Update" {
				traces[e.TraceID] = true
			}
		}
	}
	if len(traces) != 2 {
		t.Fatalf("patches ran in %d traces, want 2", len(traces))
	}
}
