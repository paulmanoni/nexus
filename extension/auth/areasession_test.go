package auth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/extension/auth"
)

type targetIn struct {
	ID string `json:"id"`
}

func areaSessionApp(t *testing.T) *httptest.Server {
	t.Helper()
	s := auth.Settings{
		Schemes: map[string]auth.SchemeSettings{
			"web":   {Type: auth.SchemeSession},
			"admin": {Type: auth.SchemeSession},
		},
		Passwords:     testSettings.Passwords,
		Areas:         map[string]auth.AreaSettings{"admin": {Prefix: "/admin", Kinds: []string{"staff"}, Session: "admin"}},
		Impersonation: auth.ImpersonationSettings{Permission: "orders.view"},
	}
	app, stop, err := nexus.InProcess(config.Runtime{},
		auth.Module(auth.Config{Users: auth.StaticUsers(newTestUsers()), Settings: &s}),
		nexus.AsRest("POST", "/login", signIn, auth.Public()),
		nexus.AsRest("POST", "/logout", signOut, auth.Public()),
		nexus.AsRest("GET", "/me", whoAmI, auth.Public()),
		nexus.AsRest("GET", "/health", health, auth.Public()),
		nexus.AsRest("POST", "/admin/login", signIn, auth.Public()),
		nexus.AsRest("POST", "/admin/logout", signOut, auth.Public()),
		nexus.AsRest("GET", "/admin/me", whoAmI, auth.Public()),
		nexus.AsRest("POST", "/admin/impersonate", func(ctx context.Context, in targetIn) (*me, error) {
			return &me{}, auth.Impersonate(ctx, in.ID)
		}),
		nexus.AsRest("POST", "/admin/revoke", func(ctx context.Context) (*me, error) {
			return &me{}, auth.RevokeUser(ctx, auth.Current(ctx).ID)
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app)
	t.Cleanup(func() { srv.Close(); _ = stop(context.Background()) })
	return srv
}

func TestAreaSession(t *testing.T) {
	srv := areaSessionApp(t)
	b := newBrowser(t, srv)
	b.do("GET", "/health", "")
	who := func(path string) me {
		t.Helper()
		var m me
		code, body := b.do("GET", path, "")
		if code != 200 {
			t.Fatalf("GET %s = %d %s", path, code, body)
		}
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}

	// One browser, two sign-ins: staff in the admin area, a customer in the app.
	if code, body := b.do("POST", "/admin/login", `{"login":"ana","password":"correct horse"}`); code != 201 {
		t.Fatalf("admin sign-in = %d %s", code, body)
	}
	if m := who("/me"); m.ID != "" {
		t.Fatalf("the admin sign-in leaked into the app: %+v", m)
	}
	if code, body := b.do("POST", "/login", `{"login":"bo","password":"old format pw"}`); code != 201 {
		t.Fatalf("app sign-in = %d %s", code, body)
	}
	if m := who("/admin/me"); m.ID != "1" || m.Scheme != "admin" {
		t.Fatalf("/admin/me = %+v, want ana under the admin scheme", m)
	}
	if m := who("/me"); m.ID != "2" || m.Scheme != "web" {
		t.Fatalf("/me = %+v, want bo under the web scheme", m)
	}

	// The cookie is scoped to the area and HttpOnly.
	u, _ := url.Parse(srv.URL + "/me")
	for _, c := range b.client.Jar.Cookies(u) {
		if c.Name == "nexus_admin_session" {
			t.Fatal("the admin cookie is sent outside /admin")
		}
	}

	// Impersonating in the area acts there only.
	if code, body := b.do("POST", "/admin/impersonate", `{"id":"2"}`); code != 201 {
		t.Fatalf("impersonate = %d %s", code, body)
	}
	if m := who("/admin/me"); m.ID != "2" {
		t.Fatalf("/admin/me while impersonating = %+v", m)
	}
	if m := who("/me"); m.ID != "2" || m.Scheme != "web" {
		t.Fatalf("/me = %+v", m)
	}

	// Signing out of the area leaves the app's sign-in.
	b.do("POST", "/admin/logout", "")
	if m := who("/admin/me"); m.ID != "" {
		t.Fatalf("/admin/me after sign-out = %+v", m)
	}
	if m := who("/me"); m.ID != "2" {
		t.Fatalf("/me after the admin sign-out = %+v", m)
	}

	// RevokeUser ends an area session too.
	b.do("POST", "/admin/login", `{"login":"ana","password":"correct horse"}`)
	if code, body := b.do("POST", "/admin/revoke", ""); code != 201 {
		t.Fatalf("revoke = %d %s", code, body)
	}
	if m := who("/admin/me"); m.ID != "" {
		t.Fatalf("/admin/me after RevokeUser = %+v", m)
	}

	// A customer can't sign in to the area.
	if code, _ := b.do("POST", "/admin/login", `{"login":"bo","password":"old format pw"}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("a customer signing in under /admin = %d", code)
	}
}

func TestAreaSessionSettings(t *testing.T) {
	for name, s := range map[string]auth.Settings{
		"unknown scheme": {
			Schemes: map[string]auth.SchemeSettings{"web": {Type: auth.SchemeSession}},
			Areas:   map[string]auth.AreaSettings{"admin": {Prefix: "/admin", Session: "nope"}},
		},
		"not a session": {
			Schemes: map[string]auth.SchemeSettings{"api": {Type: auth.SchemeBearer}},
			Areas:   map[string]auth.AreaSettings{"admin": {Prefix: "/admin", Session: "api"}},
		},
		"shared": {
			Schemes: map[string]auth.SchemeSettings{"s": {Type: auth.SchemeSession}},
			Areas: map[string]auth.AreaSettings{
				"a": {Prefix: "/a", Session: "s"},
				"b": {Prefix: "/b", Session: "s"},
			},
		},
	} {
		_, stop, err := nexus.InProcess(config.Runtime{}, auth.Module(auth.Config{Users: auth.StaticUsers(newTestUsers()), Settings: &s}))
		if err == nil {
			_ = stop(context.Background())
			t.Errorf("%s: booted", name)
		}
	}
}
