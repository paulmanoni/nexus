package cors

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/paulmanoni/nexus/manifest"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// newTestEngine wires a gin engine with the CORS middleware applied
// and a single route the tests can hit. Returns the engine so tests
// can fire arbitrary requests at it.
func newTestEngine(t *testing.T, cfg Config) *gin.Engine {
	t.Helper()
	applyDefaults(&cfg)
	if err := validate(&cfg); err != nil {
		t.Fatalf("validate: %v", err)
	}
	r := gin.New()
	r.Use(ginHandler(&cfg, buildMatcher(&cfg)))
	r.GET("/ping", func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})
	r.POST("/ping", func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})
	return r
}

// TestValidate locks in the configuration guards. Each case mirrors
// a misconfiguration we've actually seen operators ship and need a
// boot-time error for.
func TestValidate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  Config
		want string // substring of expected error
	}{
		{"no origins", Config{}, "AllowOrigins"},
		{"wildcard + creds", Config{AllowOrigins: []string{"*"}, AllowCredentials: true}, "forbids"},
		{"missing scheme", Config{AllowOrigins: []string{"app.example.com"}}, "scheme"},
		{"empty entry", Config{AllowOrigins: []string{""}}, "empty"},
		{"with func skips checks", Config{AllowOriginFunc: func(string) (bool, string) { return true, "" }}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validate(&tc.cfg)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("want nil, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want substring %q, got %v", tc.want, err)
			}
		})
	}
}

// TestSameOriginRequestUntouched — when no Origin header is present
// (the request is same-origin), the middleware writes no CORS
// headers. This is the correct no-op; browsers don't enforce CORS on
// same-origin requests, and we shouldn't taint the response.
func TestSameOriginRequestUntouched(t *testing.T) {
	t.Parallel()
	r := newTestEngine(t, Config{AllowOrigins: []string{"https://app.example.com"}})
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("want no ACAO header for same-origin; got %q", got)
	}
}

// TestAllowedOriginReflected — when the request origin matches the
// list, the response echoes the origin back AND sets Vary: Origin.
// The Vary header is what prevents proxy caches from serving an
// allowed origin's response to a blocked origin's request.
func TestAllowedOriginReflected(t *testing.T) {
	t.Parallel()
	r := newTestEngine(t, Config{AllowOrigins: []string{"https://app.example.com"}})
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	req.Header.Set("Origin", "https://app.example.com")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("ACAO: got %q, want https://app.example.com", got)
	}
	if v := w.Header().Get("Vary"); !strings.Contains(v, "Origin") {
		t.Errorf("Vary header missing Origin: %q", v)
	}
}

// TestDisallowedOriginNoHeaders — a request from an unlisted origin
// receives no CORS headers. The route still runs (server-side curl
// shouldn't be blocked) but the browser will reject the response.
func TestDisallowedOriginNoHeaders(t *testing.T) {
	t.Parallel()
	r := newTestEngine(t, Config{AllowOrigins: []string{"https://app.example.com"}})
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("ACAO should be empty for disallowed origin; got %q", got)
	}
	// Route should still have run.
	if w.Code != http.StatusOK {
		t.Errorf("want route to run anyway (200); got %d", w.Code)
	}
}

// TestSubdomainWildcardMatches verifies the "*.example.com" pattern
// matches one-level subdomains and rejects multi-level. The spec is
// ambiguous on multi-level; we take the stricter reading by default
// because operators usually want exactly that scope.
func TestSubdomainWildcardMatches(t *testing.T) {
	t.Parallel()
	r := newTestEngine(t, Config{AllowOrigins: []string{"https://*.example.com"}})

	cases := []struct {
		origin string
		want   bool
	}{
		{"https://app.example.com", true},
		{"https://api.example.com", true},
		{"https://example.com", false},      // bare apex doesn't match
		{"https://a.b.example.com", false},  // multi-level rejected
		{"http://app.example.com", false},   // wrong scheme
		{"https://app.evil.com", false},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, "/ping", nil)
		req.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		got := w.Header().Get("Access-Control-Allow-Origin")
		if tc.want && got != tc.origin {
			t.Errorf("origin %q: want allowed (ACAO=%q), got %q", tc.origin, tc.origin, got)
		}
		if !tc.want && got != "" {
			t.Errorf("origin %q: want rejected (ACAO=\"\"), got %q", tc.origin, got)
		}
	}
}

// TestPreflightShortCircuits — a real preflight request (OPTIONS +
// Access-Control-Request-Method) gets a 204 with the full preflight
// header set, and the underlying route never runs. We assert the
// "route never runs" bit by registering a route that would 500 if
// called: hitting a 204 proves the middleware aborted the chain.
func TestPreflightShortCircuits(t *testing.T) {
	t.Parallel()
	cfg := Config{
		AllowOrigins:     []string{"https://app.example.com"},
		AllowMethods:     []string{"GET", "POST", "PUT"},
		AllowHeaders:     []string{"Content-Type", "Authorization"},
		AllowCredentials: true,
		MaxAge:           3600,
	}
	applyDefaults(&cfg)
	if err := validate(&cfg); err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	r.Use(ginHandler(&cfg, buildMatcher(&cfg)))
	// Register an OPTIONS handler that 500s if the middleware
	// didn't abort. If we see 500, the preflight short-circuit
	// silently failed.
	r.OPTIONS("/ping", func(c *gin.Context) {
		c.AbortWithStatus(http.StatusInternalServerError)
	})

	req := httptest.NewRequest(http.MethodOptions, "/ping", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "Content-Type, Authorization")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("want 204; got %d", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, "POST") {
		t.Errorf("Allow-Methods missing POST: %q", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(got, "Content-Type") {
		t.Errorf("Allow-Headers missing Content-Type: %q", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("Allow-Credentials: want \"true\", got %q", got)
	}
	if got := w.Header().Get("Access-Control-Max-Age"); got != "3600" {
		t.Errorf("Max-Age: want \"3600\", got %q", got)
	}
}

// TestPreflightDisallowedOrigin — a preflight from a blocked origin
// gets 204 with NO CORS headers (the browser will then fail the
// pending request). We don't 403 because non-browser clients ignore
// CORS, and a 403 from an OPTIONS preflight would confuse them.
func TestPreflightDisallowedOrigin(t *testing.T) {
	t.Parallel()
	r := newTestEngine(t, Config{AllowOrigins: []string{"https://app.example.com"}})
	req := httptest.NewRequest(http.MethodOptions, "/ping", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("want 204; got %d", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("ACAO should be absent; got %q", got)
	}
}

// TestWildcardEchoesStar — when AllowOrigins=["*"] (no credentials),
// the response echoes "*" rather than the request's origin. This
// matches what most browsers expect for a "fully public" API.
func TestWildcardEchoesStar(t *testing.T) {
	t.Parallel()
	r := newTestEngine(t, Config{AllowOrigins: []string{"*"}})
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	req.Header.Set("Origin", "https://anyone.example.com")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("wildcard config should echo *; got %q", got)
	}
}

// TestExposeHeadersAdvertised — when ExposeHeaders is set, the
// response carries the matching Access-Control-Expose-Headers value
// on actual (non-preflight) requests so JS can read those headers.
func TestExposeHeadersAdvertised(t *testing.T) {
	t.Parallel()
	r := newTestEngine(t, Config{
		AllowOrigins:  []string{"https://app.example.com"},
		ExposeHeaders: []string{"X-Total-Count", "X-RateLimit-Remaining"},
	})
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	req.Header.Set("Origin", "https://app.example.com")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	got := w.Header().Get("Access-Control-Expose-Headers")
	if !strings.Contains(got, "X-Total-Count") || !strings.Contains(got, "X-RateLimit-Remaining") {
		t.Errorf("Expose-Headers: got %q", got)
	}
}

// TestManifestOverridesInCode — manifest's cors block wins over
// in-code Config field-by-field. Same precedence rule as TLS.
func TestManifestOverridesInCode(t *testing.T) {
	t.Parallel()
	creds := false
	mf := &manifest.Manifest{
		CORS: &manifest.CORSBlock{
			AllowOrigins:     []string{"https://manifest.example.com"},
			AllowCredentials: &creds,
		},
	}
	out, err := resolveConfig(Config{
		AllowOrigins:     []string{"https://in-code.example.com"},
		AllowCredentials: true,
	}, mf)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if out.AllowOrigins[0] != "https://manifest.example.com" {
		t.Errorf("origins: got %v, want manifest", out.AllowOrigins)
	}
	if out.AllowCredentials {
		t.Errorf("AllowCredentials: should be false (manifest overrode)")
	}
}

// TestManifestDisabledShortCircuits — a manifest with disabled:true
// bypasses validation entirely so a half-filled in-code Config still
// boots in environments that don't want CORS at all (cloud LB
// handles it).
func TestManifestDisabledShortCircuits(t *testing.T) {
	t.Parallel()
	mf := &manifest.Manifest{
		CORS: &manifest.CORSBlock{Disabled: true},
	}
	// Empty Config would normally fail validate.
	out, err := resolveConfig(Config{}, mf)
	if err != nil {
		t.Fatalf("disabled env should skip validation; got %v", err)
	}
	if !out.Disabled {
		t.Error("Disabled=false; want true")
	}
}

// TestMergeOverrides_CORSDomainsReplace covers the per-env override
// path: production has app.example.com, staging swaps to staging
// origins via environment_overrides. Mirrors the TLS test of the
// same shape.
func TestMergeOverrides_CORSOriginsReplace(t *testing.T) {
	t.Parallel()
	creds := true
	stagingCreds := false
	base := manifest.Manifest{
		SchemaVersion: "1",
		App:           manifest.AppIdentity{Name: "demo"},
		Name:          "demo",
		Environments: []manifest.Environment{
			{Name: "production"},
			{Name: "staging"},
		},
		CORS: &manifest.CORSBlock{
			AllowOrigins:     []string{"https://app.example.com"},
			AllowCredentials: &creds,
		},
		Overrides: map[string]manifest.Override{
			"staging": {
				CORS: &manifest.CORSPatch{
					AllowOrigins:     []string{"https://staging.example.com"},
					AllowCredentials: &stagingCreds,
				},
			},
		},
	}

	prod, err := manifest.MergeOverrides(base, "production")
	if err != nil {
		t.Fatal(err)
	}
	if prod.CORS.AllowOrigins[0] != "https://app.example.com" {
		t.Errorf("prod origins: got %v", prod.CORS.AllowOrigins)
	}
	if prod.CORS.AllowCredentials == nil || !*prod.CORS.AllowCredentials {
		t.Errorf("prod creds: want true")
	}

	staging, err := manifest.MergeOverrides(base, "staging")
	if err != nil {
		t.Fatal(err)
	}
	if staging.CORS.AllowOrigins[0] != "https://staging.example.com" {
		t.Errorf("staging origins: got %v", staging.CORS.AllowOrigins)
	}
	if staging.CORS.AllowCredentials == nil || *staging.CORS.AllowCredentials {
		t.Errorf("staging creds: want false (overridden)")
	}
}
