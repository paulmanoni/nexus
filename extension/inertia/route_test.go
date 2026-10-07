package inertia_test

import (
	"context"
	"testing"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/extension/inertia"
)

func TestPageRoute(t *testing.T) {
	auth := inertia.Page("GET,POST", "/auth", "Auth", NewAuthForm, nexus.Name("signIn"))
	widget := inertia.Page("GET", "/widgets/:id", "Widgets/Show", NewWidgets)
	app, stop, err := nexus.InProcess(config.Runtime{}, nexus.Module("site", nexus.Path("/site"), auth, widget))
	if err != nil {
		t.Fatal(err)
	}
	defer stop(context.Background())
	ctx := nexus.WithApp(context.Background(), app)
	if got := auth.URL(ctx); got != "/site/auth" || auth.Method() != "GET" {
		t.Errorf("multi-method page: %q %s", got, auth.Method())
	}
	if got := nexus.URL(ctx, "site:signIn"); got != "/site/auth" {
		t.Errorf("by name, on two methods: %q", got)
	}
	if got := widget.URL(ctx, 3); got != "/site/widgets/3" {
		t.Errorf("page with a parameter: %q", got)
	}
}
