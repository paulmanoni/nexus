package view

import (
	"context"
	"testing"

	"github.com/a-h/templ"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
)

func AboutPage() templ.Component { return templ.Raw("about") }

func TestLiveAndPageURLs(t *testing.T) {
	page := Live[*counterLive]("/count/:name").Provide(func() *counterLive { return counterTemplate })
	about := Page("GET", "/about", AboutPage)
	app, stop, err := nexus.InProcess(config.Runtime{},
		nexus.Supply(&greeter{greeting: "hi"}),
		nexus.Module("site", nexus.Path("/site"), page, about),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
	ctx := nexus.WithApp(context.Background(), app)

	if got := page.URL(ctx, "ann", nexus.Query{"mode": "x"}); got != "/site/count/ann?mode=x" {
		t.Errorf("live page: %q", got)
	}
	if got := nexus.URL(ctx, "counter", nexus.P{"name": "bo"}); got != "/site/count/bo" {
		t.Errorf("live page by name: %q", got)
	}
	if got := about.URL(ctx); got != "/site/about" {
		t.Errorf("view.Page: %q", got)
	}
	if got := nexus.URL(ctx, "site:aboutPage"); got != "/site/about" {
		t.Errorf("view.Page by name: %q", got)
	}
	names := map[string]bool{}
	for _, r := range app.Routes() {
		names[r.Name] = true
	}
	if len(names) != 2 {
		t.Errorf("only the page routes are named (not _live or _upload): %v", app.Routes())
	}
}
