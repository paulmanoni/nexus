package authtest_test

import (
	"context"
	"testing"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/extension/auth"
	"github.com/paulmanoni/nexus/v2/extension/auth/authtest"
	"github.com/paulmanoni/nexus/v2/nexustest"
)

type out struct {
	ID string `json:"id"`
}

func orders(ctx context.Context) (*out, error) { return &out{auth.Current(ctx).ID}, nil }

type loginIn struct {
	Login    string `json:"login"`
	Password string `json:"password"`
}

func login(ctx context.Context, in loginIn) (*out, error) {
	id, err := auth.Login(ctx, auth.Password{Login: in.Login, Password: in.Password})
	if err != nil {
		return nil, err
	}
	return &out{id.ID}, nil
}

func TestAuthtest(t *testing.T) {
	users := authtest.NewUsers().
		Add(auth.Identity{ID: "7", Kind: "staff", Perms: []string{"orders.*"}}, "ana@example.com", "correct horse battery").
		Add(auth.Identity{ID: "8", Kind: "customer"}, "bo@example.com", "another long one")
	app := nexustest.New(t, config.Runtime{},
		auth.Module(auth.Config{Users: auth.StaticUsers(users), Settings: &auth.Settings{
			Cache: -1, // no Load cache: Remove takes effect at once
			Areas: map[string]auth.AreaSettings{"admin": {Prefix: "/admin", Kinds: []string{"staff"}}},
		}}),
		nexus.AsRest("GET", "/admin/orders", orders, auth.Requires("orders.view")),
		nexus.AsRest("POST", "/login", login, auth.Public()),
	)

	app.GET("/admin/orders").AssertStatus(401)
	app.With(authtest.As(&auth.Identity{ID: "1", Kind: "staff", Perms: []string{"orders.view"}})).
		GET("/admin/orders").AssertOK()
	app.With(authtest.As(&auth.Identity{ID: "2", Kind: "customer", Perms: []string{"orders.view"}})).
		GET("/admin/orders").AssertStatus(403) // the area admits staff only
	app.With(authtest.As(&auth.Identity{ID: "3", Kind: "staff"})).
		GET("/admin/orders").AssertStatus(403) // no orders.view

	var o out
	app.With(authtest.AsUser("7")).GET("/admin/orders").AssertOK().JSON(&o)
	if o.ID != "7" {
		t.Fatalf("AsUser loads through Users: %+v", o)
	}
	app.With(authtest.AsUser("8")).GET("/admin/orders").AssertStatus(403)

	app.POST("/login", map[string]string{"login": "ANA@example.com", "password": "correct horse battery"}).AssertStatus(201)
	app.POST("/login", map[string]string{"login": "ana@example.com", "password": "wrong"}).AssertStatus(422)

	users.Remove("7")
	app.With(authtest.AsUser("7")).GET("/admin/orders").AssertStatus(401)
}
