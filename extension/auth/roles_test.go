package auth_test

import (
	"context"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/extension/auth"
	"github.com/paulmanoni/nexus/v2/extension/auth/authtest"
	"github.com/paulmanoni/nexus/v2/nexustest"
)

func viewOrders(ctx context.Context) (*me, error) { return whoAmI(ctx) }

func TestRoles(t *testing.T) {
	users := authtest.NewUsers().Add(auth.Identity{ID: "7", Roles: []string{"clerk"}}, "cy@example.com", "a long password")
	app := nexustest.New(t, config.Runtime{},
		auth.Module(auth.Config{Users: auth.StaticUsers(users), Settings: &auth.Settings{
			Roles: map[string][]string{"clerk": {"orders.view"}, "admin": {"*"}},
		}}),
		nexus.AsRest("GET", "/orders", viewOrders, auth.Requires("orders.view")),
	)
	app.With(authtest.As(&auth.Identity{ID: "1", Roles: []string{"clerk"}})).GET("/orders").AssertOK()
	app.With(authtest.As(&auth.Identity{ID: "2", Roles: []string{"admin"}})).GET("/orders").AssertOK()
	app.With(authtest.As(&auth.Identity{ID: "3", Roles: []string{"guest"}})).GET("/orders").AssertStatus(403)
	app.With(authtest.AsUser("7")).GET("/orders").AssertOK() // Users.Load's roles expand too
}

func TestPermissionCatalogue(t *testing.T) {
	boot := func(s auth.Settings, opts ...nexus.Option) error {
		_, stop, err := nexus.InProcess(config.Runtime{},
			append([]nexus.Option{auth.Module(auth.Config{Users: auth.StaticUsers(authtest.NewUsers()), Settings: &s})}, opts...)...)
		if err == nil {
			_ = stop(context.Background())
		}
		return err
	}
	cat := auth.Settings{Perms: []string{"orders.view", "orders.refund"}, Roles: map[string][]string{"clerk": {"orders.*"}, "root": {"*"}}}
	if err := boot(cat, nexus.AsRest("GET", "/orders", viewOrders, auth.Requires("orders.view"))); err != nil {
		t.Fatalf("declared permissions boot: %v", err)
	}
	err := boot(cat, nexus.AsRest("GET", "/orders", viewOrders, auth.Requires("ordres.view")))
	if err == nil || !strings.Contains(err.Error(), `"ordres.view"`) {
		t.Fatalf("a misspelled gate = %v", err)
	}
	bad := cat
	bad.Roles = map[string][]string{"clerk": {"billing.view"}}
	if err := boot(bad); err == nil || !strings.Contains(err.Error(), "role clerk") {
		t.Fatalf("a role granting an undeclared permission = %v", err)
	}
	if err := boot(auth.Settings{}, nexus.AsRest("GET", "/orders", viewOrders, auth.Requires("anything"))); err != nil {
		t.Fatalf("no catalogue, no check: %v", err)
	}
}
