package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/registry"
)

// TestReverseProxy_Forwards verifies the core forwarding: path + headers reach
// the upstream unchanged, the response comes back, and X-Nexus-Proxied is
// stamped.
func TestReverseProxy_Forwards(t *testing.T) {
	var gotPath, gotAuth, gotExtra, gotFwdHost string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotExtra = r.Header.Get("X-Internal")
		gotFwdHost = r.Header.Get("X-Forwarded-Host")
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, "hello from upstream")
	}))
	defer upstream.Close()

	rp, err := buildReverseProxy(upstream.URL, map[string]string{"X-Internal": "secret"}, nil, false, nil)
	if err != nil {
		t.Fatalf("buildReverseProxy: %v", err)
	}

	req := httptest.NewRequest("GET", "http://portal.example/api/candidate-placement-details/", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	rp.ServeHTTP(rec, req)

	if gotPath != "/api/candidate-placement-details/" {
		t.Errorf("upstream path = %q", gotPath)
	}
	if gotAuth != "Bearer tok123" {
		t.Errorf("Authorization not forwarded: %q", gotAuth)
	}
	if gotExtra != "secret" {
		t.Errorf("SetHeaders not applied: X-Internal = %q", gotExtra)
	}
	if gotFwdHost != "portal.example" {
		t.Errorf("X-Forwarded-Host = %q, want portal.example", gotFwdHost)
	}
	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want 418", rec.Code)
	}
	if rec.Header().Get("X-Nexus-Proxied") != "1" {
		t.Errorf("X-Nexus-Proxied not set")
	}
	if rec.Body.String() != "hello from upstream" {
		t.Errorf("body = %q", rec.Body.String())
	}
}

// TestReverseProxy_UpstreamHost covers name-based vhost upstreams: the
// upstream sees its own Host, and its redirects/cookies point back at the proxy.
func TestReverseProxy_UpstreamHost(t *testing.T) {
	var gotHost string
	var upstreamURL string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		http.SetCookie(w, &http.Cookie{Name: "PHPSESSID", Value: "abc", Domain: "127.0.0.1", Path: "/"})
		w.Header().Add("Set-Cookie", "other=1; Domain=example.org; Path=/")
		http.Redirect(w, r, upstreamURL+"/login.php?next=%2F", http.StatusFound)
	}))
	defer upstream.Close()
	upstreamURL = upstream.URL

	rp, err := buildReverseProxy(upstream.URL, nil, nil, true, nil)
	if err != nil {
		t.Fatalf("buildReverseProxy: %v", err)
	}
	rec := httptest.NewRecorder()
	rp.ServeHTTP(rec, httptest.NewRequest("GET", "http://api.example/", nil))

	if want := strings.TrimPrefix(upstream.URL, "http://"); gotHost != want {
		t.Errorf("upstream Host = %q, want %q", gotHost, want)
	}
	if loc := rec.Header().Get("Location"); loc != "/login.php?next=%2F" {
		t.Errorf("Location = %q, want path-only", loc)
	}
	cookies := rec.Header().Values("Set-Cookie")
	if len(cookies) != 2 || strings.Contains(strings.ToLower(cookies[0]), "domain=") {
		t.Errorf("upstream cookie Domain not stripped: %q", cookies)
	}
	if !strings.Contains(cookies[1], "Domain=example.org") {
		t.Errorf("foreign cookie Domain should be kept: %q", cookies[1])
	}
}

// TestReverseProxy_RewritePath applies a path rewrite before forwarding.
func TestReverseProxy_RewritePath(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
	}))
	defer upstream.Close()

	rp, err := buildReverseProxy(upstream.URL, nil, func(p string) string { return "/legacy" + p }, false, nil)
	if err != nil {
		t.Fatalf("buildReverseProxy: %v", err)
	}
	req := httptest.NewRequest("GET", "http://portal.example/api/x", nil)
	rp.ServeHTTP(httptest.NewRecorder(), req)
	if gotPath != "/legacy/api/x" {
		t.Errorf("rewritten path = %q, want /legacy/api/x", gotPath)
	}
}

// TestReverseProxy_UpstreamDown returns a 502 JSON body, not a panic/plaintext.
func TestReverseProxy_UpstreamDown(t *testing.T) {
	// Reserve a port then close it → guaranteed dial failure.
	l := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead := l.URL
	l.Close()

	rp, err := buildReverseProxy(dead, nil, nil, false, nil)
	if err != nil {
		t.Fatalf("buildReverseProxy: %v", err)
	}
	rec := httptest.NewRecorder()
	rp.ServeHTTP(rec, httptest.NewRequest("GET", "http://portal.example/api/x", nil))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("content-type = %q", ct)
	}
}

func TestBuildReverseProxy_BadUpstream(t *testing.T) {
	for _, bad := range []string{"", "not-a-url", "/relative/only"} {
		if _, err := buildReverseProxy(bad, nil, nil, false, nil); err == nil {
			t.Errorf("upstream %q: expected error, got nil", bad)
		}
	}
}

// TestAutoYield is the heart of the strangler board: a route with a native
// nexus endpoint at the same method+path is considered migrated (proxy yields);
// one without stays proxied.
func TestAutoYield(t *testing.T) {
	reg := registry.New()
	// A native handler already owns GET /api/migrated.
	reg.RegisterEndpoint(registry.Endpoint{
		Name: "GetMigrated", Transport: registry.REST, Method: "GET", Path: "/api/migrated",
	})
	// A proxied endpoint must NOT count as native (would make everything look migrated).
	reg.RegisterEndpoint(registry.Endpoint{
		Name: "proxied", Transport: registry.REST, Method: "GET", Path: "/api/still-proxied",
		Tags: map[string]string{registry.ProxyTag: "http://up"},
	})

	native := nativeRESTIndex(reg)

	if !isMigrated(native, "GET", "/api/migrated") {
		t.Error("GET /api/migrated should be detected as migrated (native handler exists)")
	}
	if isMigrated(native, "POST", "/api/migrated") {
		t.Error("POST /api/migrated should NOT be migrated (only GET is native)")
	}
	if isMigrated(native, "GET", "/api/still-proxied") {
		t.Error("a ProxyTag endpoint must not count as a native handler")
	}
	if isMigrated(native, "GET", "/api/never-touched") {
		t.Error("an unknown path is not migrated")
	}
	// Any-method route yields if any native method exists on the path.
	if !isMigrated(native, anyMethod, "/api/migrated") {
		t.Error("ANY route should yield when any native method exists")
	}
}

// TestSnapshotBurndown reports migrated vs proxied counts live from the registry.
func TestSnapshotBurndown(t *testing.T) {
	reg := registry.New()
	reg.RegisterEndpoint(registry.Endpoint{
		Name: "GetA", Transport: registry.REST, Method: "GET", Path: "/api/a",
	})

	st := &pluginState{cfg: Config{
		Upstream: "http://up",
		Routes: []Route{
			{Method: "GET", Path: "/api/a"},  // native exists → migrated
			{Method: "POST", Path: "/api/b"}, // still proxied
			{Method: "GET", Path: "/api/c"},  // still proxied
		},
	}}

	snap := st.snapshot(reg).(map[string]any)
	if snap["total"].(int) != 3 {
		t.Errorf("total = %v, want 3", snap["total"])
	}
	if snap["migrated"].(int) != 1 {
		t.Errorf("migrated = %v, want 1", snap["migrated"])
	}
	if snap["proxied"].(int) != 2 {
		t.Errorf("proxied = %v, want 2", snap["proxied"])
	}
}
