package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/extension/auth"
)

// pageProps returns what a page renderer would share: the registered
// shared page props, for this request.
func pageProps(ctx context.Context) (map[string]any, error) {
	out := map[string]any{}
	for _, sp := range nexus.SharedPageProps() {
		if k, v := sp(ctx); k != "" {
			out[k] = v
		}
	}
	return out, nil
}

type fakeCache struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (c *fakeCache) Get(ctx context.Context, key string, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok := c.m[key]
	if !ok {
		return errors.New("miss")
	}
	return json.Unmarshal(b, out)
}

func (c *fakeCache) Set(ctx context.Context, key string, v any, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, err := json.Marshal(v)
	c.m[key] = b
	return err
}

func (c *fakeCache) Delete(ctx context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, key)
	return nil
}

func (c *fakeCache) Clear(ctx context.Context) error { return nil }

func stage2bApp(t *testing.T, cache *fakeCache) *httptest.Server {
	t.Helper()
	s := testSettings
	s.Areas = map[string]auth.AreaSettings{"admin": {Prefix: "/admin", Kinds: []string{"staff"}, Forbidden: "/admin/forbidden"}}
	s.Throttle = auth.ThrottleSettings{Account: "2/1m", IP: "off"}
	app, stop, err := nexus.InProcess(config.Runtime{},
		auth.Module(auth.Config{Users: auth.StaticUsers(newTestUsers()), Settings: &s, Throttle: auth.CacheThrottle(cache)}),
		nexus.AsRest("POST", "/login", signIn, auth.Public()),
		nexus.AsRest("POST", "/logout", signOut, auth.Public()),
		nexus.AsRest("GET", "/admin/orders", adminOrders),
		nexus.AsRest("GET", "/props", pageProps, auth.Public()),
		nexus.AsRest("GET", "/health", health, auth.Public()),
	)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app)
	t.Cleanup(func() { srv.Close(); _ = stop(context.Background()) })
	return srv
}

func (b *browser) cookie(name string) string {
	u, _ := url.Parse(b.srv.URL)
	for _, c := range b.client.Jar.Cookies(u) {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

func TestCSRFRotatesOnSignInAndOut(t *testing.T) {
	srv := stage2bApp(t, &fakeCache{m: map[string][]byte{}})
	b := newBrowser(t, srv)
	b.do("GET", "/health", "")
	before := b.cookie("csrftoken")
	if before == "" {
		t.Fatal("no CSRF cookie")
	}
	if code, _ := b.do("POST", "/login", `{"login":"ana","password":"correct horse"}`); code != 201 {
		t.Fatalf("login = %d", code)
	}
	after := b.cookie("csrftoken")
	if after == before || b.cookie("XSRF-TOKEN") != after {
		t.Fatalf("sign-in must rotate the CSRF token: %q → %q (XSRF %q)", before, after, b.cookie("XSRF-TOKEN"))
	}
	b.do("POST", "/logout", "")
	if b.cookie("csrftoken") == after {
		t.Fatal("sign-out must rotate the CSRF token")
	}
}

func TestAuthPageProp(t *testing.T) {
	srv := stage2bApp(t, &fakeCache{m: map[string][]byte{}})
	b := newBrowser(t, srv)
	_, body := b.do("GET", "/props", "")
	if !strings.Contains(body, `"auth":{"user":null,"can":{`) || !strings.Contains(body, `"GET /admin/orders":false`) {
		t.Fatalf("anonymous page props = %s", body)
	}
	b.do("POST", "/login", `{"login":"ana","password":"correct horse"}`)
	_, body = b.do("GET", "/props", "")
	if !strings.Contains(body, `"user":{"id":"1","kind":"staff"}`) || !strings.Contains(body, `"GET /admin/orders":true`) {
		t.Fatalf("signed-in page props = %s", body)
	}
}

func TestForbiddenPage(t *testing.T) {
	srv := stage2bApp(t, &fakeCache{m: map[string][]byte{}})
	b := noRedirects(newBrowser(t, srv))
	b.do("GET", "/health", "")
	b.do("POST", "/login", `{"login":"bo","password":"old format pw"}`)
	req, _ := http.NewRequest("GET", srv.URL+"/admin/orders", nil)
	req.Header.Set("Accept", "text/html")
	res, err := b.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 302 || res.Header.Get("Location") != "/admin/forbidden" {
		t.Fatalf("a refused page visit = %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	if code, _ := b.do("GET", "/admin/orders", ""); code != 403 {
		t.Fatalf("a refused API call = %d", code)
	}
}

func TestCacheThrottleIsShared(t *testing.T) {
	cache := &fakeCache{m: map[string][]byte{}}
	one, two := newBrowser(t, stage2bApp(t, cache)), newBrowser(t, stage2bApp(t, cache))
	one.do("GET", "/health", "")
	two.do("GET", "/health", "")
	one.do("POST", "/login", `{"login":"ana","password":"wrong"}`)
	two.do("POST", "/login", `{"login":"ana","password":"wrong"}`)
	if code, _ := one.do("POST", "/login", `{"login":"ana","password":"correct horse"}`); code != 429 {
		t.Fatalf("two replicas sharing a cache count together: %d, want 429", code)
	}
}

func TestAuthPropIsTyped(t *testing.T) {
	app, stop, err := nexus.InProcess(config.Runtime{},
		auth.Module(auth.Config{Users: auth.StaticUsers(newTestUsers()), Settings: &auth.Settings{}}))
	if err != nil {
		t.Fatal(err)
	}
	defer stop(context.Background())
	m := app.Registry().SharedProps()
	if _, ok := m["auth"]; !ok {
		t.Fatalf("shared props = %v; want auth typed for client.d.ts", m)
	}
}
