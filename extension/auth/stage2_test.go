package auth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/extension/auth"
)

func adminOrders(ctx context.Context) (*me, error) { return whoAmI(ctx) }

type publicUsers struct{ *testUsers }

func (publicUsers) Public(id *auth.Identity) any {
	return map[string]string{"name": id.User.(*account).login}
}

func areaApp(t *testing.T, users auth.Users, throttle auth.ThrottleSettings) *httptest.Server {
	t.Helper()
	s := testSettings
	s.Login = "/login"
	s.Areas = map[string]auth.AreaSettings{"admin": {Prefix: "/admin", Kinds: []string{"staff"}, Login: "/admin/login"}}
	s.Throttle = throttle
	s.Endpoints = auth.EndpointSettings{Login: "/auth/login", Logout: "/auth/logout", Me: "/auth/me"}
	app, stop, err := nexus.InProcess(config.Runtime{},
		auth.Module(auth.Config{Users: auth.StaticUsers(users), Settings: &s}),
		nexus.AsRest("GET", "/admin/orders", adminOrders),
		nexus.AsRest("POST", "/admin/login", signIn, auth.Public()),
		nexus.AsRest("GET", "/dash", whoAmI),
		nexus.AsRest("GET", "/health", health, auth.Public()),
	)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app)
	t.Cleanup(func() { srv.Close(); _ = stop(context.Background()) })
	return srv
}

func noRedirects(b *browser) *browser {
	b.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return b
}

func TestAreas(t *testing.T) {
	srv := areaApp(t, newTestUsers(), auth.ThrottleSettings{})

	anon := noRedirects(newBrowser(t, srv))
	anon.header["Accept"] = "text/html"
	req, _ := http.NewRequest("GET", srv.URL+"/admin/orders?page=2", nil)
	req.Header.Set("Accept", "text/html")
	res, err := anon.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 302 || res.Header.Get("Location") != "/admin/login?next=%2Fadmin%2Forders%3Fpage%3D2" {
		t.Fatalf("a page visit = %d %q, want the area's login with next", res.StatusCode, res.Header.Get("Location"))
	}
	req, _ = http.NewRequest("GET", srv.URL+"/dash", nil)
	req.Header.Set("X-Inertia", "true")
	res, _ = anon.client.Do(req)
	res.Body.Close()
	if res.StatusCode != 409 || res.Header.Get("X-Inertia-Location") != "/login?next=%2Fdash" {
		t.Fatalf("an Inertia visit outside areas = %d %q", res.StatusCode, res.Header.Get("X-Inertia-Location"))
	}
	api := newBrowser(t, srv)
	if code, _ := api.do("GET", "/admin/orders", ""); code != 401 {
		t.Fatalf("an API call = %d, want 401", code)
	}

	// A customer can't sign in under /admin, nor reach it with a session
	// from elsewhere.
	cust := newBrowser(t, srv)
	cust.do("GET", "/health", "")
	if code, body := cust.do("POST", "/admin/login", `{"login":"bo","password":"old format pw"}`); code != 422 || !strings.Contains(body, "invalid login or password") {
		t.Fatalf("a customer signing in under /admin = %d %s", code, body)
	}
	if code, _ := cust.do("POST", "/auth/login", `{"login":"bo","password":"old format pw"}`); code != 201 {
		t.Fatalf("a customer signing in outside the area = %d", code)
	}
	if code, _ := cust.do("GET", "/admin/orders", ""); code != 403 {
		t.Fatalf("a customer in /admin = %d, want 403", code)
	}
	if code, _ := cust.do("GET", "/dash", ""); code != 200 {
		t.Fatalf("a customer outside the area = %d", code)
	}

	staff := newBrowser(t, srv)
	staff.do("GET", "/health", "")
	for query, want := range map[string]string{
		"":                       "/admin",        // the area's home
		"?next=/admin/orders":    "/admin/orders", // a safe next
		"?next=//evil.example":   "/admin",        // refused
		"?next=%2F%2Fevil.co%2F": "/admin",        // refused encoded
	} {
		_, out := staff.do("POST", "/admin/login"+query, `{"login":"ana","password":"correct horse"}`)
		var cred auth.Credential
		_ = json.Unmarshal([]byte(out), &cred)
		if cred.Next != want {
			t.Errorf("sign-in %s → next %q, want %q (%s)", query, cred.Next, want, out)
		}
	}
	if code, _ := staff.do("GET", "/admin/orders", ""); code != 200 {
		t.Fatalf("staff in /admin = %d", code)
	}
}

func TestThrottle(t *testing.T) {
	srv := areaApp(t, newTestUsers(), auth.ThrottleSettings{Account: "2/1m", IP: "5/1m"})
	b := newBrowser(t, srv)
	b.do("GET", "/health", "")
	login := func(who, pw string) (int, string) {
		return b.do("POST", "/auth/login", `{"login":"`+who+`","password":"`+pw+`"}`)
	}
	login("ana", "wrong")
	login("ana", "wrong")
	if code, body := login("ana", "correct horse"); code != 429 || !strings.Contains(body, "too many failed sign-ins") {
		t.Fatalf("a locked account = %d %s", code, body)
	}
	req, _ := http.NewRequest("POST", b.srv.URL+"/auth/login", strings.NewReader(`{"login":"ana","password":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRFToken", b.cookie("csrftoken"))
	if res, err := b.client.Do(req); err != nil || res.Header.Get("Retry-After") == "" {
		t.Fatalf("a 429 carries Retry-After: %v %v", err, res.Header)
	}
	if code, _ := login("bo", "old format pw"); code != 201 {
		t.Fatalf("another account = %d", code)
	}
	login("nobody", "x")
	login("nobody2", "x")
	login("nobody3", "x")
	if code, _ := login("bo", "old format pw"); code != 429 {
		t.Fatalf("an IP over its limit = %d, want 429", code)
	}
}

func TestBuiltInEndpoints(t *testing.T) {
	srv := areaApp(t, publicUsers{newTestUsers()}, auth.ThrottleSettings{})
	b := newBrowser(t, srv)
	code, body := b.do("GET", "/auth/me", "")
	if code != 200 || !strings.Contains(body, `"user":null`) || !strings.Contains(body, `"GET /dash":false`) {
		t.Fatalf("anonymous me = %d %s", code, body)
	}
	if code, _ := b.do("POST", "/auth/login", `{"login":"ana","password":"correct horse"}`); code != 201 {
		t.Fatalf("login = %d", code)
	}
	_, body = b.do("GET", "/auth/me", "")
	if !strings.Contains(body, `"user":{"name":"ana"}`) || !strings.Contains(body, `"GET /dash":true`) {
		t.Fatalf("me = %s", body)
	}
	if code, _ := b.do("POST", "/auth/logout", ""); code != 201 {
		t.Fatalf("logout = %d", code)
	}
	if _, body = b.do("GET", "/auth/me", ""); !strings.Contains(body, `"user":null`) {
		t.Fatalf("me after logout = %s", body)
	}
}
