package auth_test

import (
	"context"
	"testing"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/extension/auth"
	"github.com/paulmanoni/nexus/v2/extension/auth/authtest"
	"github.com/paulmanoni/nexus/v2/nexustest"
)

type greeting struct {
	Who string `json:"who"`
}

type nameIn struct {
	Name string `json:"name" graphql:"name"`
}

// A nil *Identity is anonymous; the handler decides.
func hello(ctx context.Context, me *auth.Identity, in nameIn) (*greeting, error) {
	if me == nil {
		return &greeting{Who: "stranger " + in.Name}, nil
	}
	return &greeting{Who: me.ID + " " + in.Name}, nil
}

type refunds struct{}

// A non-pointer Identity requires a sign-in.
func (refunds) Refund(ctx context.Context, me auth.Identity, in nameIn) (*greeting, error) {
	return &greeting{Who: me.ID + " refunds " + in.Name}, nil
}

func TestIdentityParameter(t *testing.T) {
	app := nexustest.New(t, config.Runtime{},
		auth.Module(auth.Config{Users: auth.StaticUsers(authtest.NewUsers()), Settings: &auth.Settings{Default: "public"}}),
		nexus.Supply(refunds{}),
		nexus.AsRest("POST", "/hello", hello),
		nexus.AsQuery(hello),
		nexus.AsRest("POST", "/refund", (refunds).Refund),
	)
	ana := app.With(authtest.As(&auth.Identity{ID: "ana"}))

	var g greeting
	app.POST("/hello", map[string]string{"name": "x"}).AssertStatus(201).JSON(&g)
	if g.Who != "stranger x" {
		t.Fatalf("anonymous: %+v", g)
	}
	ana.POST("/hello", map[string]string{"name": "x"}).AssertStatus(201).JSON(&g)
	if g.Who != "ana x" {
		t.Fatalf("signed in: %+v", g)
	}
	data := ana.GraphQL(`{ hello(name: "y") { who } }`, nil)
	if data["hello"].(map[string]any)["who"] != "ana y" {
		t.Fatalf("GraphQL: %v", data)
	}

	app.POST("/refund", map[string]string{"name": "z"}).AssertStatus(401)
	ana.POST("/refund", map[string]string{"name": "z"}).AssertStatus(201).JSON(&g)
	if g.Who != "ana refunds z" {
		t.Fatalf("required identity: %+v", g)
	}
}
