package authdb_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/extension/auth"
	"github.com/paulmanoni/nexus/v2/extension/auth/authdb"
	"github.com/paulmanoni/nexus/v2/extension/auth/authtest"
	"github.com/paulmanoni/nexus/v2/nexustest"
)

type devices struct {
	N int `json:"n"`
}

func countDevices(ctx context.Context, me auth.Identity) (*devices, error) {
	d, err := auth.Sessions(ctx, me.ID)
	return &devices{len(d)}, err
}

func revoke(ctx context.Context, me auth.Identity) (*devices, error) {
	return &devices{}, auth.RevokeUser(ctx, me.ID)
}

// boot is one process of the app on the shared database file.
func boot(t *testing.T, path string) *nexustest.App {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(path+"?_pragma=busy_timeout(10000)"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	store := authdb.New(func() *gorm.DB { return g })
	users := authtest.NewUsers().Add(auth.Identity{ID: "7"}, "ana", "a long password")
	return nexustest.New(t, config.Runtime{},
		nexus.Provide(func() auth.TokenStore { return store }),
		auth.Module(auth.Config{Users: auth.StaticUsers(users), Settings: &auth.Settings{
			Cache:     -1, // no per-process cache: another process's RevokeUser applies at once
			Schemes:   map[string]auth.SchemeSettings{"api": {Type: auth.SchemeBearer}},
			Endpoints: auth.EndpointSettings{Login: "/login", Me: "/me"},
		}}),
		nexus.AsRest("GET", "/devices", countDevices),
		nexus.AsRest("POST", "/revoke", revoke),
	)
}

func bearer(app *nexustest.App, tok string) *nexustest.App {
	return app.With(map[string][]string{"Authorization": {"Bearer " + tok}})
}

func TestTokensSurviveARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.db")
	first := boot(t, path)
	res := first.POST("/login", map[string]string{"login": "ana", "password": "a long password"}).AssertStatus(201)
	var cred auth.Credential
	res.JSON(&cred)
	if cred.AccessToken == "" {
		t.Fatalf("login: %s", res.String())
	}

	second := boot(t, path) // another process — or the same app after a restart
	if body := bearer(second, cred.AccessToken).GET("/me").AssertOK().String(); !strings.Contains(body, `"id":"7"`) {
		t.Fatalf("the token in another process: %s", body)
	}
	var d devices
	bearer(second, cred.AccessToken).GET("/devices").AssertOK().JSON(&d)
	if d.N != 1 {
		t.Fatalf("Sessions lists the token: %d", d.N)
	}
	bearer(second, cred.AccessToken).POST("/revoke", nil).AssertStatus(201)
	if code := bearer(first, cred.AccessToken).GET("/devices").Code; code != 401 {
		t.Fatalf("RevokeUser reaches the first process through the table: %d", code)
	}
}
