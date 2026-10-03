package nexus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/dev"
	"github.com/paulmanoni/nexus/v2/frontend/vitehot"
	"github.com/paulmanoni/nexus/v2/httpx"
)

// helper for tests — keeps the empty-config call sites readable.
var noFrontendCfg = &frontendConfig{}

// TestServeFrontend covers the dispatch shape of the SPA mount:
//   - extensionless paths get index.html (SPA routing) with
//     no-cache headers
//   - with no Vite manifest, only content-hashed names under /assets/
//     get the immutable far-future cache header; an unhashed file there
//     revalidates (see TestServeFrontend_ManifestCachePolicy for the
//     manifest-driven rule)
//   - other dotted paths (favicon.ico, robots.txt) revalidate
//   - REST routes registered alongside still win — NoRoute only
//     fires when nothing else claimed the path
func TestServeFrontend(t *testing.T) {
	t.Setenv("GIN_MODE", "test")
	fsys := fstest.MapFS{
		"index.html":              {Data: []byte("<html>app</html>")},
		"favicon.ico":             {Data: []byte("favicon-bytes")},
		"assets/main-DfUgamcr.js": {Data: []byte("console.log(1)")},
		"assets/nested/deep.css":  {Data: []byte(".x{}")},
	}

	app := New(config.Runtime{})
	app.engine.GET("/api/ping", func(c *httpx.Ctx) { c.String(http.StatusOK, "pong") })

	if err := mountFrontend(app, fsys, noFrontendCfg); err != nil {
		t.Fatalf("mountFrontend: %v", err)
	}

	type expect struct {
		status int
		body   string
		// substr in Content-Type, "" to skip
		ct string
		// substr expected in Cache-Control, "" to skip
		cache string
	}
	cases := []struct {
		name string
		path string
		expect
	}{
		{"root → index", "/", expect{200, "<html>app</html>", "text/html", "no-cache"}},
		{"index alias", "/index.html", expect{200, "<html>app</html>", "text/html", "no-cache"}},
		{"top-level favicon", "/favicon.ico", expect{200, "favicon-bytes", "", "no-cache"}},
		{"hashed asset", "/assets/main-DfUgamcr.js", expect{200, "console.log(1)", "javascript", "immutable"}},
		// Unhashed, so a rebuild reuses its name: caching it forever — as the
		// old /assets/ prefix rule did — would serve stale styles.
		{"unhashed asset revalidates", "/assets/nested/deep.css", expect{200, ".x{}", "css", "no-cache"}},
		{"asset that doesn't exist", "/assets/missing.js", expect{404, "", "", ""}},
		{"SPA client route", "/users/123", expect{200, "<html>app</html>", "text/html", "no-cache"}},
		{"REST route wins", "/api/ping", expect{200, "pong", "", ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req, _ := http.NewRequest("GET", tc.path, nil)
			app.engine.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status: got %d want %d body=%q", rec.Code, tc.status, rec.Body.String())
			}
			if tc.body != "" && rec.Body.String() != tc.body {
				t.Fatalf("body: got %q want %q", rec.Body.String(), tc.body)
			}
			if tc.ct != "" && !strings.Contains(rec.Header().Get("Content-Type"), tc.ct) {
				t.Fatalf("content-type: got %q want substr %q", rec.Header().Get("Content-Type"), tc.ct)
			}
			if tc.cache != "" && !strings.Contains(rec.Header().Get("Cache-Control"), tc.cache) {
				t.Fatalf("cache-control: got %q want substr %q", rec.Header().Get("Cache-Control"), tc.cache)
			}
		})
	}
}

// TestServeFrontend_DevModeNoCacheOnAssets pins the fix for the
// "browser reloads but UI stays stale" bug: under NEXUS_DEV=1
// every asset response must carry no-cache so the browser
// refetches main.js / main.css after the dev-reload shim
// triggers location.reload(). Heuristic caching otherwise serves
// the previous bytes and the operator never sees their edits.
func TestServeFrontend_DevModeNoCacheOnAssets(t *testing.T) {
	t.Setenv(dev.Env, "1")
	t.Setenv("GIN_MODE", "test")
	fsys := fstest.MapFS{
		"index.html":         {Data: []byte("<html>app</html>")},
		"main.js":            {Data: []byte("console.log('v1')")},
		"main.css":           {Data: []byte(".x{}")},
		"assets/hashed-a.js": {Data: []byte("// hashed")},
	}

	app := New(config.Runtime{})
	if err := mountFrontend(app, fsys, noFrontendCfg); err != nil {
		t.Fatalf("mountFrontend: %v", err)
	}

	cases := []struct {
		path       string
		wantCache  string
		wantStatus int
	}{
		// Top-level bundler outputs — the user's main.js / main.css.
		// Dev mode must NEVER let the browser cache these or the
		// reload-after-rebuild story is broken.
		{"/main.js", "no-cache", 200},
		{"/main.css", "no-cache", 200},
		// Hashed assets under /assets/ also lose their immutable
		// cache header in dev mode — operators rarely rebuild
		// to a fresh hash during dev, but if they do (Vite-style
		// content hashing turned on locally) we still want
		// fresh-on-reload.
		{"/assets/hashed-a.js", "no-cache", 200},
		// SPA fallback always no-cache regardless of dev mode.
		{"/some/spa/route", "no-cache", 200},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req, _ := http.NewRequest("GET", tc.path, nil)
			app.engine.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status: got %d want %d", rec.Code, tc.wantStatus)
			}
			got := rec.Header().Get("Cache-Control")
			if !strings.Contains(got, tc.wantCache) {
				t.Errorf("cache-control: got %q want substr %q", got, tc.wantCache)
			}
			// no-store guarantees the browser doesn't even hold a
			// disk copy across the reload.
			if !strings.Contains(got, "no-store") {
				t.Errorf("dev assets must include no-store: got %q", got)
			}
		})
	}
}

// TestServeFrontendMissingIndex verifies the boot-time guardrail:
// without an index.html the mount fails fast so a stale or
// unbuilt bundle surfaces at compile/start time, not at first
// request.
func TestServeFrontendMissingIndex(t *testing.T) {
	app := New(config.Runtime{})
	fsys := fstest.MapFS{"assets/main.js": {Data: []byte("x")}}
	err := mountFrontend(app, fsys, noFrontendCfg)
	if err == nil {
		t.Fatal("expected error for missing index.html, got nil")
	}
	if !strings.Contains(err.Error(), "index.html") {
		t.Fatalf("error should mention index.html: %v", err)
	}
}

// TestServeFrontendAtSubPath confirms FrontendAt nests the SPA
// under a sub-path while leaving sibling paths free for other
// handlers — the standard "REST at /api, SPA at /admin" shape.
func TestServeFrontendAtSubPath(t *testing.T) {
	app := New(config.Runtime{})
	app.engine.GET("/api/ping", func(c *httpx.Ctx) { c.String(http.StatusOK, "pong") })

	fsys := fstest.MapFS{
		"index.html":     {Data: []byte("<html>app</html>")},
		"assets/main.js": {Data: []byte("x=2")},
	}
	if err := mountFrontend(app, fsys, &frontendConfig{mountPath: "/admin"}); err != nil {
		t.Fatalf("mountFrontend: %v", err)
	}

	cases := []struct {
		path   string
		status int
		body   string
	}{
		{"/admin/", 200, "<html>app</html>"},
		{"/admin/assets/main.js", 200, "x=2"},
		{"/admin/users/123", 200, "<html>app</html>"},
		{"/api/ping", 200, "pong"},
		// Outside the SPA mount path — should not serve index.html.
		{"/", 404, ""},
		{"/users/123", 404, ""},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req, _ := http.NewRequest("GET", tc.path, nil)
			app.engine.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status: got %d want %d body=%q", rec.Code, tc.status, rec.Body.String())
			}
			if tc.body != "" && rec.Body.String() != tc.body {
				t.Fatalf("body: got %q want %q", rec.Body.String(), tc.body)
			}
		})
	}
}

// TestServeFrontendNormalizesMountPath verifies that FrontendAt
// accepts loose input (no leading slash, trailing slash, "/")
// and normalizes to a canonical "/seg" form internally so the
// dispatcher's prefix-strip stays straightforward.
func TestServeFrontendNormalizesMountPath(t *testing.T) {
	cases := []struct {
		in  string
		out string
	}{
		{"", ""},
		{"/", ""},
		{"admin", "/admin"},
		{"/admin", "/admin"},
		{"/admin/", "/admin"},
		{"admin/", "/admin"},
		{"/a/b/", "/a/b"},
	}
	for _, tc := range cases {
		got := normalizeRoutePrefix(tc.in)
		if got != tc.out {
			t.Errorf("normalize %q: got %q want %q", tc.in, got, tc.out)
		}
	}
}

// TestServeFrontendWithRoutePrefix confirms the SPA mount honors
// the deployment route prefix and refuses to serve unprefixed
// requests when a prefix is set — otherwise the SPA would swallow
// requests destined for a different mount sharing the listener.
func TestServeFrontendWithRoutePrefix(t *testing.T) {
	app := New(config.Runtime{Server: config.Server{RoutePrefix: "/v1/api"}})
	fsys := fstest.MapFS{
		"index.html":     {Data: []byte("<html>app</html>")},
		"assets/main.js": {Data: []byte("x=1")},
	}
	if err := mountFrontend(app, fsys, noFrontendCfg); err != nil {
		t.Fatalf("mountFrontend: %v", err)
	}

	cases := []struct {
		path   string
		status int
		body   string
	}{
		{"/v1/api/", 200, "<html>app</html>"},
		{"/v1/api/assets/main.js", 200, "x=1"},
		{"/v1/api/spa-route/123", 200, "<html>app</html>"},
		// Unprefixed requests should NOT serve the SPA — they 404
		// so a separate mount on the same listener can stay
		// independent.
		{"/spa-route/123", 404, ""},
		{"/assets/main.js", 404, ""},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req, _ := http.NewRequest("GET", tc.path, nil)
			app.engine.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status: got %d want %d body=%q", rec.Code, tc.status, rec.Body.String())
			}
			if tc.body != "" && rec.Body.String() != tc.body {
				t.Fatalf("body: got %q want %q", rec.Body.String(), tc.body)
			}
		})
	}
}

// TestServeFrontend_DevModeReadsFromDisk verifies the NEXUS_DEV
// swap: when the env var is set, ServeFrontend bypasses the supplied
// embed-style FS and reads from os.DirFS at NEXUS_DEV_ROOT instead,
// so a watching frontend toolchain can refresh the served bundle
// without recompiling Go.
// TestServeFrontend_DevModeRefreshesIndexHTML pins the bug where
// the framework cached index.html at boot — vite's mid-session
// rewrite would land on disk but the served HTML stayed pointing
// at the old asset hashes. In dev mode we must re-read on each
// request so a frontend rebuild is visible on the next refresh.
func TestServeFrontend_DevModeRefreshesIndexHTML(t *testing.T) {
	t.Setenv("GIN_MODE", "test")
	dir := t.TempDir()
	distDir := dir + "/web/dist"
	if err := os.MkdirAll(distDir, 0o755); err != nil {
		t.Fatal(err)
	}
	indexPath := distDir + "/index.html"
	if err := os.WriteFile(indexPath, []byte("<html>v1</html>"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv(dev.Env, "1")
	t.Setenv(dev.RootEnv, dir)

	fsys := os.DirFS(dir)
	sub, err := fs.Sub(fsys, "web/dist")
	if err != nil {
		t.Fatalf("sub: %v", err)
	}
	app := New(config.Runtime{})
	if err := mountFrontend(app, sub, noFrontendCfg); err != nil {
		t.Fatalf("mount: %v", err)
	}

	// First request — boot-time bytes.
	rec := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/", nil)
	app.engine.ServeHTTP(rec, req)
	if got := noShim(rec.Body.String()); got != "<html>v1</html>" {
		t.Fatalf("first GET: got %q, want %q", got, "<html>v1</html>")
	}

	// Simulate vite rewriting index.html with a fresh asset hash.
	if err := os.WriteFile(indexPath, []byte("<html>v2</html>"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec = httptest.NewRecorder()
	app.engine.ServeHTTP(rec, req)
	if got := noShim(rec.Body.String()); got != "<html>v2</html>" {
		t.Errorf("second GET: got %q, want %q (dev-mode re-read regressed — frontend changes won't reach the browser)", got, "<html>v2</html>")
	}
}

// TestServeFrontend_ProductionCachesIndexHTML pins the inverse:
// outside dev mode the boot-time read is authoritative (assets are
// content-hashed; re-reading per request is wasted I/O).
func TestServeFrontend_ProductionCachesIndexHTML(t *testing.T) {
	t.Setenv("GIN_MODE", "test")
	t.Setenv(dev.Env, "")
	dir := t.TempDir()
	distDir := dir + "/web/dist"
	if err := os.MkdirAll(distDir, 0o755); err != nil {
		t.Fatal(err)
	}
	indexPath := distDir + "/index.html"
	if err := os.WriteFile(indexPath, []byte("<html>boot</html>"), 0o644); err != nil {
		t.Fatal(err)
	}

	fsys := os.DirFS(dir)
	sub, err := fs.Sub(fsys, "web/dist")
	if err != nil {
		t.Fatal(err)
	}
	app := New(config.Runtime{})
	if err := mountFrontend(app, sub, noFrontendCfg); err != nil {
		t.Fatal(err)
	}

	// Mutate after boot. Production should keep serving the boot copy.
	if err := os.WriteFile(indexPath, []byte("<html>after-boot</html>"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/", nil)
	app.engine.ServeHTTP(rec, req)
	if got := noShim(rec.Body.String()); got != "<html>boot</html>" {
		t.Errorf("prod GET: got %q, want boot-time bytes %q", got, "<html>boot</html>")
	}
}

func TestServeFrontend_DevModeReadsFromDisk(t *testing.T) {
	t.Setenv("GIN_MODE", "test")
	dir := t.TempDir()
	distDir := dir + "/web/dist"
	if err := os.MkdirAll(distDir+"/assets", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(distDir+"/index.html", []byte("<html>fresh</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(distDir+"/assets/main.js", []byte("fresh-js"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The "embed" FS deliberately carries DIFFERENT bytes — if the
	// dev swap doesn't kick in, the test would observe the embed
	// values. Detecting "stale" is the whole point.
	embedFS := fstest.MapFS{
		"web/dist/index.html":     {Data: []byte("<html>STALE</html>")},
		"web/dist/assets/main.js": {Data: []byte("STALE-JS")},
	}

	t.Setenv(dev.Env, "1")
	t.Setenv(dev.RootEnv, dir)

	// ServeFrontend's dev-mode swap fires inside the function before
	// the fx Invoke captures the FS. Re-running its swap logic here
	// matches what the runtime sees on app boot.
	fsys := fs.FS(embedFS)
	if os.Getenv(dev.Env) == "1" {
		fsys = os.DirFS(os.Getenv(dev.RootEnv))
	}
	sub, err := fs.Sub(fsys, "web/dist")
	if err != nil {
		t.Fatalf("sub: %v", err)
	}
	app := New(config.Runtime{})
	if err := mountFrontend(app, sub, noFrontendCfg); err != nil {
		t.Fatalf("mount: %v", err)
	}

	cases := []struct{ path, want string }{
		{"/", "<html>fresh</html>"},
		{"/assets/main.js", "fresh-js"},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", tc.path, nil)
		app.engine.ServeHTTP(rec, req)
		if got := noShim(rec.Body.String()); got != tc.want {
			t.Errorf("GET %s: got %q, want %q (dev-mode disk swap regressed)", tc.path, got, tc.want)
		}
	}
}

func get(app *App, path string, hdr ...string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	app.engine.ServeHTTP(rec, req)
	return rec
}

// A Vite build: hashed outputs in a renamed assetsDir, one output Vite built
// under a hash-free name, and a public/ copy the manifest does not list.
func viteBundle() fstest.MapFS {
	return fstest.MapFS{
		"index.html": {Data: []byte("<html>shell</html>")},
		".vite/manifest.json": {Data: []byte(`{
			"index.html": {"file":"static/index-DfUgamcr.js","isEntry":true,
			               "css":["static/index-mxpuzdxW.css"],"assets":["static/logo-B2xQ9fLk.svg"]},
			"src/worker.ts": {"file":"static/worker.js","isEntry":true}
		}`)},
		"static/index-DfUgamcr.js":  {Data: []byte("app()")},
		"static/index-mxpuzdxW.css": {Data: []byte(".a{}")},
		"static/logo-B2xQ9fLk.svg":  {Data: []byte("<svg/>")},
		"static/worker.js":          {Data: []byte("work()")},
		"favicon.ico":               {Data: []byte("ico")},
	}
}

func TestServeFrontend_ManifestCachePolicy(t *testing.T) {
	t.Setenv("GIN_MODE", "test")
	app := New(config.Runtime{})
	if err := mountFrontend(app, viteBundle(), noFrontendCfg); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		path, want string
	}{
		// Listed by the manifest and hashed — under a directory the old
		// /assets/ rule never recognised, so these were not cached at all.
		{"/static/index-DfUgamcr.js", "immutable"},
		{"/static/index-mxpuzdxW.css", "immutable"},
		{"/static/logo-B2xQ9fLk.svg", "immutable"},
		// Built by Vite but under a name with no hash.
		{"/static/worker.js", "no-cache"},
		// Copied from public/: not build output.
		{"/favicon.ico", "no-cache"},
	}
	for _, c := range cases {
		rec := get(app, c.path)
		if rec.Code != 200 {
			t.Fatalf("%s: status %d", c.path, rec.Code)
		}
		cc := rec.Header().Get("Cache-Control")
		if !strings.Contains(cc, c.want) {
			t.Errorf("%s: Cache-Control %q, want %q", c.path, cc, c.want)
		}
		if c.want == "no-cache" && strings.Contains(cc, "immutable") {
			t.Errorf("%s: must not be immutable: %q", c.path, cc)
		}
	}
}

func TestServeFrontend_ETagRevalidation(t *testing.T) {
	t.Setenv("GIN_MODE", "test")
	app := New(config.Runtime{})
	if err := mountFrontend(app, viteBundle(), noFrontendCfg); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/favicon.ico", "/static/worker.js", "/"} {
		first := get(app, path)
		tag := first.Header().Get("ETag")
		if first.Code != 200 || tag == "" {
			t.Fatalf("%s: want 200 with an ETag, got %d %q", path, first.Code, tag)
		}
		if again := get(app, path, "If-None-Match", tag); again.Code != http.StatusNotModified {
			t.Errorf("%s: matching If-None-Match should be 304, got %d", path, again.Code)
		}
		if weak := get(app, path, "If-None-Match", "W/"+tag); weak.Code != http.StatusNotModified {
			t.Errorf("%s: a weak match should also be 304, got %d", path, weak.Code)
		}
		if other := get(app, path, "If-None-Match", `"stale"`); other.Code != 200 {
			t.Errorf("%s: a different tag must get the content, got %d", path, other.Code)
		}
	}
}

func TestServeFrontend_ShellIsRevalidatedNotUnstored(t *testing.T) {
	t.Setenv("GIN_MODE", "test")
	app := New(config.Runtime{})
	if err := mountFrontend(app, viteBundle(), noFrontendCfg); err != nil {
		t.Fatal(err)
	}
	cc := get(app, "/").Header().Get("Cache-Control")
	if cc != "no-cache" {
		t.Fatalf("production shell Cache-Control = %q; want no-cache (stored, revalidated — no-store defeats the 304)", cc)
	}
}

// An app whose only entry is a module (an Inertia app declaring
// nexus({ input: 'src/main.ts' })) builds no index.html. The manifest proves
// the build happened, so it must boot; there is just no shell to fall back to.
func TestServeFrontend_ShellLessBuildBoots(t *testing.T) {
	t.Setenv("GIN_MODE", "test")
	app := New(config.Runtime{})
	fsys := fstest.MapFS{
		".vite/manifest.json":     {Data: []byte(`{"src/main.ts":{"file":"assets/main-DfUgamcr.js","isEntry":true}}`)},
		"assets/main-DfUgamcr.js": {Data: []byte("app()")},
	}
	if err := mountFrontend(app, fsys, noFrontendCfg); err != nil {
		t.Fatalf("a built bundle without index.html must boot: %v", err)
	}
	if rec := get(app, "/some/client/route"); rec.Code != http.StatusNotFound {
		t.Errorf("no shell to fall back to: want 404, got %d", rec.Code)
	}
	if rec := get(app, "/assets/main-DfUgamcr.js"); rec.Code != 200 || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("assets still served and cached: %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
}

func TestServeFrontend_UnbuiltBundleStillFailsFast(t *testing.T) {
	t.Setenv("GIN_MODE", "test")
	err := mountFrontend(New(config.Runtime{}), fstest.MapFS{"assets/x.js": {Data: []byte("x")}}, noFrontendCfg)
	if err == nil || !strings.Contains(err.Error(), "neither index.html nor a Vite manifest") {
		t.Fatalf("a bundle with no shell and no manifest was never built; want a boot error, got %v", err)
	}
}

func TestAssetCacheControlWithoutManifest(t *testing.T) {
	cases := map[string]string{
		"assets/index-DfUgamcr.js": "public, max-age=31536000, immutable",
		"assets/logo.png":          "public, no-cache", // the stale-logo case
		"static/app-DfUgamcr.js":   "public, no-cache", // no manifest, no convention: can't know
		"favicon.ico":              "public, no-cache",
	}
	for rel, want := range cases {
		if got := assetCacheControl(rel, nil); got != want {
			t.Errorf("assetCacheControl(%q) = %q, want %q", rel, got, want)
		}
	}
}

// Boot leniency for an unbuilt bundle needs evidence of development
// happening now — nexus dev, or a live Vite dev server — never the
// environment value alone: `nexus new` writes environment = "development"
// into nexus.toml, and deployments ship it.
func TestServeFrontend_UnbuiltBundleBootsInDevelopment(t *testing.T) {
	t.Setenv("GIN_MODE", "test")

	t.Run("environment alone fails fast", func(t *testing.T) {
		t.Setenv(dev.Env, "")
		app := New(config.Runtime{Environment: "development"})
		err := mountFrontend(app, fstest.MapFS{}, noFrontendCfg)
		if err == nil || !strings.Contains(err.Error(), "neither index.html nor a Vite manifest") {
			t.Fatalf("a deployment shipping environment = development must still fail fast, got %v", err)
		}
	})

	t.Run("nexus dev boots to the placeholder", func(t *testing.T) {
		t.Setenv(dev.Env, "1")
		t.Setenv(dev.RootEnv, t.TempDir())
		app := New(config.Runtime{})
		if err := mountFrontend(app, fstest.MapFS{}, noFrontendCfg); err != nil {
			t.Fatalf("nexus dev must not fail fast on an unbuilt bundle: %v", err)
		}
		stopFrontendOnCleanup(t, app)
		if rec := get(app, "/"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "No frontend yet") {
			t.Fatalf("want the placeholder page, got %d", rec.Code)
		}
	})

	// npm run dev + go run . — the frontend is the dev server.
	for _, tc := range []struct {
		name string
		live bool
	}{{"live dev server boots", true}, {"stale hot file fails fast", false}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(dev.Env, "")
			t.Setenv(dev.RootEnv, "")
			dir := t.TempDir()
			dist := filepath.Join(dir, "web", "dist")
			var origin string
			pid := os.Getpid()
			if tc.live {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/index.html" {
						http.NotFound(w, r)
						return
					}
					w.Header().Set("Content-Type", "text/html")
					w.Write([]byte(`<html><head></head><body>from vite<script type="module" src="/src/main.ts"></script></body></html>`))
				}))
				t.Cleanup(srv.Close)
				origin = srv.URL
			} else {
				closed := httptest.NewServer(http.NotFoundHandler())
				origin = closed.URL
				closed.Close()
				dead := exec.Command("true")
				if err := dead.Run(); err != nil {
					t.Skipf("no `true` binary: %v", err)
				}
				pid = dead.ProcessState.Pid()
			}
			writeHotFile(t, dist, vitehot.Hot{Version: 1, Origin: origin, Base: "/", Entries: []string{"index.html"}, PID: pid})
			t.Chdir(dir)
			app := New(config.Runtime{Environment: "development"})
			app.setFrontendSource(fstest.MapFS{}, "web/dist")
			err := mountFrontend(app, fstest.MapFS{}, noFrontendCfg)
			if !tc.live {
				if err == nil {
					t.Fatal("a hot file whose dev server is gone is no evidence of development; want fail-fast")
				}
				return
			}
			if err != nil {
				t.Fatalf("a live dev server must let an unbuilt bundle boot: %v", err)
			}
			if rec := get(app, "/"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "from vite") {
				t.Fatalf("want the dev server's page, got %d %q", rec.Code, rec.Body)
			}
		})
	}
}

func writeHotFile(t *testing.T, dist string, h vitehot.Hot) {
	t.Helper()
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	p := vitehot.Path(dist)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// The reviewer's case, end to end: hash-free kebab-case names are build
// output but not content-addressed, so they must revalidate.
func TestServeFrontend_UnhashedKebabNamesRevalidate(t *testing.T) {
	t.Setenv("GIN_MODE", "test")
	app := New(config.Runtime{})
	fsys := fstest.MapFS{
		"index.html":                  {Data: []byte("<html></html>")},
		".vite/manifest.json":         {Data: []byte(`{"index.html":{"file":"assets/my-component-name.js","isEntry":true,"css":["assets/admin-dashboard.css"],"assets":["assets/font-awesome.woff2","assets/inter-variable.woff2"]},"src/x.ts":{"file":"assets/x-DXv-ZbW9.js"}}`)},
		"assets/my-component-name.js": {Data: []byte("x")},
		"assets/admin-dashboard.css":  {Data: []byte("x")},
		"assets/font-awesome.woff2":   {Data: []byte("y")},
		"assets/inter-variable.woff2": {Data: []byte("y")},
		"assets/x-DXv-ZbW9.js":        {Data: []byte("y")},
	}
	if err := mountFrontend(app, fsys, noFrontendCfg); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/assets/my-component-name.js", "/assets/admin-dashboard.css", "/assets/font-awesome.woff2", "/assets/inter-variable.woff2"} {
		if cc := get(app, p).Header().Get("Cache-Control"); strings.Contains(cc, "immutable") {
			t.Errorf("%s: Cache-Control %q on a hash-free name", p, cc)
		}
	}
	if cc := get(app, "/assets/x-DXv-ZbW9.js").Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("a real Vite hash containing '-' should still be immutable: %q", cc)
	}
}

// staticVite behaves like a Vite dev server for static files: public/ files
// by path, a 404 for one path, and — like Vite's SPA fallback — index.html
// with a 200 for anything else.
type staticVite struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []*http.Request
}

func newStaticVite(t *testing.T, base string, files map[string]string) *staticVite {
	t.Helper()
	v := &staticVite{}
	v.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v.mu.Lock()
		v.reqs = append(v.reqs, r.Clone(context.Background()))
		v.mu.Unlock()
		rel := strings.TrimPrefix(r.URL.Path, base)
		if body, ok := files[rel]; ok {
			w.Header().Set("Content-Type", "application/octet-stream")
			if strings.HasSuffix(rel, ".svg") {
				w.Header().Set("Content-Type", "image/svg+xml")
			}
			w.Write([]byte(body))
			return
		}
		if rel == "gone.png" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<!doctype html><html>vite spa fallback</html>")
	}))
	t.Cleanup(v.Close)
	return v
}

func (v *staticVite) requests() []*http.Request {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]*http.Request(nil), v.reqs...)
}

func (f *viteDevFixture) do(t *testing.T, method, path, remote string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = remote
	req.Host = "localhost:8080"
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i] == "Host" {
			req.Host = hdr[i+1]
			continue
		}
		req.Header.Set(hdr[i], hdr[i+1])
	}
	f.app.engine.ServeHTTP(rec, req)
	return rec
}

// H1: while a Vite dev server is live, runtime paths in app code
// (<img src="/logo.svg">, fetch("/config.json")) resolve against the Go
// origin; they must reach the dev server, which has the current public/
// files, and fall back to the bundle when it has none.
func TestServeFrontend_DevServerServesStaticFiles(t *testing.T) {
	for _, base := range []string{"/", "/app/"} {
		for _, mount := range []string{"", "/admin"} {
			t.Run("base="+base+",mount="+mount, func(t *testing.T) {
				vite := newStaticVite(t, base, map[string]string{
					"logo.svg":         "vite-logo",
					"config.json":      `{"v":2}`,
					"silent-sso.html":  "<html>sso</html>",
					"fonts/a b.woff2":  "font",
					"nested/deep.json": "deep",
				})
				var opts []FrontendOption
				if mount != "" {
					opts = append(opts, FrontendAt(mount))
				}
				f := newViteDevFixture(t, "<html>built</html>", opts...)
				// An old copy of a public file, as a previous build left it.
				if err := os.WriteFile(filepath.Join(f.dist, "logo.svg"), []byte("stale-logo"), 0o644); err != nil {
					t.Fatal(err)
				}
				f.writeHot(t, vitehot.Hot{Version: 1, Origin: vite.URL, Base: base, Entries: []string{"index.html"}, PID: os.Getpid()})
				const local = "127.0.0.1:50000"

				cases := []struct {
					path, want string
					code       int
				}{
					{"/logo.svg", "vite-logo", 200},
					{"/config.json?v=1", `{"v":2}`, 200},
					{"/silent-sso.html", "<html>sso</html>", 200},
					{"/fonts/a%20b.woff2", "font", 200},
					{"/nested/deep.json", "deep", 200},
					// Vite's SPA fallback (HTML for a non-HTML path) is a
					// miss: the bundle answers.
					{"/assets/app.js", "built-js", 200},
					// A 404 from Vite is a miss too.
					{"/gone.png", "", 404},
				}
				for _, c := range cases {
					rec := f.do(t, http.MethodGet, mount+c.path, local, "Cookie", "session=secret", "Authorization", "Bearer x")
					if rec.Code != c.code || (c.want != "" && rec.Body.String() != c.want) {
						t.Errorf("GET %s: %d %q, want %d %q", mount+c.path, rec.Code, rec.Body, c.code, c.want)
					}
				}
				if ct := f.do(t, http.MethodGet, mount+"/logo.svg", local).Header().Get("Content-Type"); ct != "image/svg+xml" {
					t.Errorf("content type not passed through: %q", ct)
				}
				for _, r := range vite.requests() {
					if !strings.HasPrefix(r.URL.Path, base) {
						t.Errorf("dev server asked for %q, outside its base %q", r.URL.Path, base)
					}
					if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
						t.Errorf("the app's credentials were forwarded to the dev server: %v", r.Header)
					}
					if r.URL.Path == base+"config.json" && r.URL.RawQuery != "v=1" {
						t.Errorf("query not forwarded: %q", r.URL.RawQuery)
					}
				}

				before := len(vite.requests())
				if rec := f.do(t, http.MethodHead, mount+"/logo.svg", local); rec.Code != 200 || rec.Body.Len() != 0 {
					t.Errorf("HEAD: %d %q", rec.Code, rec.Body)
				}
				if len(vite.requests()) != before+1 {
					t.Errorf("HEAD was not answered by the dev server")
				}
				// Not reads, or not from this machine: never forwarded.
				before = len(vite.requests())
				f.do(t, http.MethodPost, mount+"/logo.svg", local)
				if rec := f.do(t, http.MethodGet, mount+"/logo.svg", "192.0.2.7:4000"); rec.Body.String() != "stale-logo" {
					t.Errorf("a remote client got %q, want the bundle's copy", rec.Body)
				}
				if rec := f.do(t, http.MethodGet, mount+"/logo.svg", "[::1]:4000"); rec.Body.String() != "vite-logo" {
					t.Errorf("an IPv6 loopback client got %q", rec.Body)
				}
				if got := len(vite.requests()); got != before+1 {
					t.Errorf("dev server got %d requests, want only the loopback GET", got-before)
				}
			})
		}
	}
}

// Without a live dev server nothing is forwarded — including under a
// production binary that finds a hot file naming a live one.
func TestServeFrontend_NoLiveDevServerNoProxy(t *testing.T) {
	vite := newStaticVite(t, "/", map[string]string{"logo.svg": "vite-logo"})
	f := newViteDevFixture(t, "<html>built</html>")
	if err := os.WriteFile(filepath.Join(f.dist, "logo.svg"), []byte("disk-logo"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rec := f.get(t, "/logo.svg"); rec.Body.String() != "disk-logo" {
		t.Errorf("no hot file: %q", rec.Body)
	}
	f.writeHot(t, vitehot.Hot{Version: 1, Origin: vite.URL, Base: "/", PID: os.Getpid()})
	t.Setenv(dev.Env, "") // hot files not honoured
	if rec := f.get(t, "/logo.svg"); rec.Body.String() != "disk-logo" {
		t.Errorf("hot files disabled: %q", rec.Body)
	}
	if n := len(vite.requests()); n != 0 {
		t.Errorf("dev server got %d requests", n)
	}
}

// A loopback peer is not enough: a local reverse proxy or tunnel makes every
// visitor loopback, and a DNS-rebinding page reaches the port under its own
// name. Neither is forwarded, nor are the dev server's internal routes.
func TestServeFrontend_DevServerProxyOnlyForThisMachine(t *testing.T) {
	vite := newStaticVite(t, "/", map[string]string{
		"logo.svg":                "vite-logo",
		"@fs/etc/hosts.txt":       "fs",
		"node_modules/x/index.js": "dep",
		"__open-in-editor.x":      "editor",
	})
	f := newViteDevFixture(t, "<html>built</html>")
	if err := os.WriteFile(filepath.Join(f.dist, "logo.svg"), []byte("stale-logo"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.writeHot(t, vitehot.Hot{Version: 1, Origin: vite.URL, Base: "/", Entries: []string{"index.html"}, PID: os.Getpid()})
	const local = "127.0.0.1:50000"

	for _, host := range []string{"localhost:8080", "127.0.0.1:8080", "[::1]:8080", "app.localhost:8080", "myapp.test", "LOCALHOST."} {
		if rec := f.do(t, http.MethodGet, "/logo.svg", local, "Host", host); rec.Body.String() != "vite-logo" {
			t.Errorf("Host %s: got %q, want the dev server's copy", host, rec.Body)
		}
	}
	before := len(vite.requests())
	refused := []struct {
		name string
		path string
		hdr  []string
	}{
		{"forwarded for", "/logo.svg", []string{"X-Forwarded-For", "203.0.113.9"}},
		{"forwarded", "/logo.svg", []string{"Forwarded", "for=203.0.113.9"}},
		{"forwarded host", "/logo.svg", []string{"X-Forwarded-Host", "app.example.com"}},
		{"real ip", "/logo.svg", []string{"X-Real-IP", "203.0.113.9"}},
		{"rebinding host", "/logo.svg", []string{"Host", "attacker.example:8080"}},
		{"lan name", "/logo.svg", []string{"Host", "192.168.1.20:8080"}},
		{"test lookalike", "/logo.svg", []string{"Host", "evil.test.com"}},
		{"@fs", "/@fs/etc/hosts.txt", nil},
		{"node_modules", "/node_modules/x/index.js", nil},
		{"vite route", "/__open-in-editor.x", nil},
	}
	for _, c := range refused {
		if rec := f.do(t, http.MethodGet, c.path, local, c.hdr...); strings.Contains(rec.Body.String(), "vite-logo") ||
			rec.Body.String() == "fs" || rec.Body.String() == "dep" || rec.Body.String() == "editor" {
			t.Errorf("%s: forwarded to the dev server (%d %q)", c.name, rec.Code, rec.Body)
		}
	}
	if got := len(vite.requests()); got != before {
		t.Errorf("dev server got %d refused requests", got-before)
	}
}

// Large files stream through; the proxy doesn't hold the body.
func TestServeFrontend_DevServerStreamsLargeFiles(t *testing.T) {
	first := bytes.Repeat([]byte("a"), 64<<10)
	rest := bytes.Repeat([]byte("b"), 4<<20)
	release := make(chan struct{})
	vite := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/big.bin" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(first)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
		w.Write(rest)
	}))
	t.Cleanup(vite.Close)
	f := newViteDevFixture(t, "<html>built</html>")
	f.writeHot(t, vitehot.Hot{Version: 1, Origin: vite.URL, Base: "/", PID: os.Getpid()})
	app := httptest.NewServer(f.app.engine)
	t.Cleanup(app.Close)

	resp, err := http.Get(app.URL + "/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got := make([]byte, len(first))
	done := make(chan error, 1)
	go func() { _, err := io.ReadFull(resp.Body, got); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("the first chunk did not arrive before the dev server finished: the response is buffered")
	}
	close(release)
	tail, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, first) || !bytes.Equal(tail, rest) {
		t.Fatalf("body corrupted: %d + %d bytes", len(got), len(tail))
	}
}

// A shell-less build answers unknown routes with 404 in production; with a
// live dev server standing for that build, development must too (the
// reviewer saw API typos come back as 200 dev pages).
func TestServeFrontend_ShellLessDevMatchesProduction(t *testing.T) {
	t.Setenv("GIN_MODE", "test")
	dir := t.TempDir()
	dist := filepath.Join(dir, "web", "dist")
	if err := os.MkdirAll(filepath.Join(dist, ".vite"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dist, ".vite", "manifest.json"), []byte(`{"src/main.ts":{"file":"assets/main-DfUgamcr.js","isEntry":true}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// This dev server's root even has an index.html — the build still won't.
	vite := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html>vite root page</html>")
	}))
	t.Cleanup(vite.Close)
	t.Setenv(dev.Env, "1")
	t.Setenv(dev.RootEnv, dir)
	app := New(config.Runtime{})
	app.setFrontendSource(os.DirFS(dir), "web/dist")
	sub, _ := fs.Sub(os.DirFS(dir), "web/dist")
	if err := mountFrontend(app, sub, noFrontendCfg); err != nil {
		t.Fatal(err)
	}
	stopFrontendOnCleanup(t, app)
	writeHotFile(t, dist, vitehot.Hot{Version: 1, Origin: vite.URL, Base: "/", Entries: []string{"src/main.ts"}, PID: os.Getpid()})
	for _, p := range []string{"/api/typo", "/", "/index.html"} {
		if rec := get(app, p); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s in dev: %d (production answers 404)\n%s", p, rec.Code, rec.Body)
		}
	}
	if _, err := app.FrontendDocument(context.Background()); !errors.Is(err, ErrNoFrontendDocument) {
		t.Errorf("FrontendDocument for a module-only build: %v, want ErrNoFrontendDocument", err)
	}
}

// Every value from the hot file that lands in a page is escaped for where it
// lands — attribute or inline-script string — whether or not the reader has
// already validated it.
func TestDevHTML_EscapesHotFileValues(t *testing.T) {
	const evil = `http://127.0.0.1:5173"><script>alert(1)</script><x a="`
	page := []byte(`<html><head></head><body>` +
		`<script type="module">import R from "/@react-refresh"; import('/src/lazy.ts')</script>` +
		`<script type="module" src="/src/main.ts"></script><link href='/x.css'></body></html>`)

	fetched := string(absolutizeDevHTML(page, evil))
	disk := string(devIndexFromDisk(page, &vitehot.Hot{Origin: evil, Base: `/b"><img src=x onerror=alert(2)>/`, Entries: []string{"src/other.ts"}}))
	for name, out := range map[string]string{"absolutizeDevHTML": fetched, "devIndexFromDisk": disk} {
		for _, bad := range []string{"<script>alert(1)</script>", "<img src=x", "<x a="} {
			if strings.Contains(out, bad) {
				t.Errorf("%s: hot-file value injected raw markup %q:\n%s", name, bad, out)
			}
		}
	}
	if n := strings.Count(disk, "<script"); n != 4 {
		t.Errorf("devIndexFromDisk: %d script tags, want the page's 2 + client + entry:\n%s", n, disk)
	}
	// The inline module's specifiers are JS strings: the origin must not
	// be able to close them.
	if !strings.Contains(fetched, `import R from "http://127.0.0.1:5173\"\x3e\x3cscript\x3ealert(1)\x3c/script\x3e\x3cx a=\"/@react-refresh"`) {
		t.Errorf("JS string context not escaped:\n%s", fetched)
	}
}

// The dev server answers for its own page; a redirect must not pull a
// document from somewhere else into the app's origin.
func TestServeFrontend_DevIndexRefusesRedirects(t *testing.T) {
	var elsewhereHits int
	var mu sync.Mutex
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		elsewhereHits++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html>elsewhere</html>")
	}))
	t.Cleanup(elsewhere.Close)
	vite := httptest.NewServer(http.RedirectHandler(elsewhere.URL+"/index.html", http.StatusFound))
	t.Cleanup(vite.Close)

	f := newViteDevFixture(t, `<html><head></head><body>built</body></html>`)
	f.writeHot(t, vitehot.Hot{Version: 1, Origin: vite.URL, Base: "/", Entries: []string{"index.html"}, PID: os.Getpid()})
	rec := f.get(t, "/")
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "elsewhere") || !strings.Contains(rec.Body.String(), "built") {
		t.Errorf("want the on-disk fallback, got %d %q", rec.Code, rec.Body)
	}
	mu.Lock()
	defer mu.Unlock()
	if elsewhereHits != 0 {
		t.Errorf("the redirect was followed")
	}
}

func TestServeFrontend_NoDirectoryListings(t *testing.T) {
	t.Setenv("GIN_MODE", "test")
	t.Setenv(dev.Env, "")
	app := New(config.Runtime{})
	fsys := fstest.MapFS{
		"index.html":       {Data: []byte("<html>x</html>")},
		"v1.2/notes.txt":   {Data: []byte("notes")},
		"assets.d/app.css": {Data: []byte(".a{}")},
	}
	if err := mountFrontend(app, fsys, noFrontendCfg); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/v1.2/", "/v1.2", "/assets.d/"} {
		rec := get(app, p)
		if loc := rec.Header().Get("Location"); rec.Code/100 == 3 && loc != "" {
			rec = get(app, loc)
		}
		if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "notes.txt") || strings.Contains(rec.Body.String(), "app.css") {
			t.Errorf("GET %s: %d %q, want 404 and no listing", p, rec.Code, rec.Body)
		}
	}
	if rec := get(app, "/v1.2/notes.txt"); rec.Code != 200 || rec.Body.String() != "notes" {
		t.Errorf("a file in a dotted directory: %d %q", rec.Code, rec.Body)
	}

	f := newViteDevFixture(t, "<html>x</html>")
	if err := os.MkdirAll(filepath.Join(f.dist, "img.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dist, "img.d", "a.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rec := f.get(t, "/img.d/"); rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "a.png") {
		t.Errorf("dev: %d %q, want 404 and no listing", rec.Code, rec.Body)
	}
}

// App.FrontendDocument is the Stage 2 contract page renderers consume.
func TestApp_FrontendDocument(t *testing.T) {
	ctx := context.Background()

	t.Run("no frontend registered", func(t *testing.T) {
		if _, err := New(config.Runtime{}).FrontendDocument(ctx); !errors.Is(err, ErrNoFrontendDocument) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("built index.html", func(t *testing.T) {
		t.Setenv("GIN_MODE", "test")
		t.Setenv(dev.Env, "")
		app := New(config.Runtime{})
		if err := mountFrontend(app, viteBundle(), noFrontendCfg); err != nil {
			t.Fatal(err)
		}
		doc, err := app.FrontendDocument(ctx)
		if err != nil || doc.FromDevServer || string(doc.HTML) != "<html>shell</html>" {
			t.Fatalf("got (%q, %v, %v)", doc.HTML, doc.FromDevServer, err)
		}
	})

	t.Run("module-only build", func(t *testing.T) {
		t.Setenv("GIN_MODE", "test")
		t.Setenv(dev.Env, "")
		app := New(config.Runtime{})
		fsys := fstest.MapFS{".vite/manifest.json": {Data: []byte(`{"src/main.ts":{"file":"assets/main-DfUgamcr.js","isEntry":true}}`)}}
		if err := mountFrontend(app, fsys, noFrontendCfg); err != nil {
			t.Fatal(err)
		}
		if _, err := app.FrontendDocument(ctx); !errors.Is(err, ErrNoFrontendDocument) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("placeholder", func(t *testing.T) {
		t.Setenv("GIN_MODE", "test")
		t.Setenv(dev.Env, "1")
		t.Setenv(dev.RootEnv, t.TempDir())
		app := New(config.Runtime{})
		if err := mountFrontend(app, fstest.MapFS{}, noFrontendCfg); err != nil {
			t.Fatal(err)
		}
		stopFrontendOnCleanup(t, app)
		if _, err := app.FrontendDocument(ctx); !errors.Is(err, ErrNoFrontendDocument) {
			t.Fatalf("the placeholder is not a document to render into; got %v", err)
		}
	})

	t.Run("dev server page", func(t *testing.T) {
		var hits []string
		srv := newFakeVite(t, "/", &hits)
		f := newViteDevFixture(t, "<html>built</html>")
		f.writeHot(t, vitehot.Hot{Version: 1, Origin: srv.URL, Base: "/", Entries: []string{"index.html"}, PID: os.Getpid()})
		doc, err := f.app.FrontendDocument(ctx)
		if err != nil || !doc.FromDevServer {
			t.Fatalf("got (%v, %v)", doc.FromDevServer, err)
		}
		html := string(doc.HTML)
		for _, want := range []string{`src="` + srv.URL + `/@vite/client"`, `src="` + srv.URL + `/src/main.ts"`, `<a href="/about">`} {
			if !strings.Contains(html, want) {
				t.Errorf("missing %q in\n%s", want, html)
			}
		}
		if strings.Contains(html, "built") {
			t.Error("the built page was used instead of the dev server's")
		}
	})

	t.Run("dev server won't serve its page", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		t.Cleanup(srv.Close)
		f := newViteDevFixture(t, "<html>built</html>")
		f.writeHot(t, vitehot.Hot{Version: 1, Origin: srv.URL, Base: "/", Entries: []string{"index.html"}, PID: os.Getpid()})
		if _, err := f.app.FrontendDocument(ctx); !errors.Is(err, ErrNoFrontendDocument) {
			t.Fatalf("a stale or placeholder copy is no dev document; got %v", err)
		}
	})

	t.Run("dev server for a module-only build", func(t *testing.T) {
		var hits []string
		srv := newFakeVite(t, "/", &hits)
		f := newViteDevFixture(t, "<html>built</html>")
		f.writeHot(t, vitehot.Hot{Version: 1, Origin: srv.URL, Base: "/", Entries: []string{"src/main.ts"}, PID: os.Getpid()})
		if _, err := f.app.FrontendDocument(ctx); !errors.Is(err, ErrNoFrontendDocument) {
			t.Fatalf("got %v", err)
		}
		if len(hits) != 0 {
			t.Errorf("fetched a page the build will not have: %v", hits)
		}
	})

	t.Run("malformed hot file is an error", func(t *testing.T) {
		f := newViteDevFixture(t, "<html>built</html>")
		f.writeHotRaw(t, []byte("{nope"))
		_, err := f.app.FrontendDocument(ctx)
		if err == nil || errors.Is(err, ErrNoFrontendDocument) || !strings.Contains(err.Error(), "nexus-hot.json") {
			t.Fatalf("want the hot-file error, got %v", err)
		}
	})
}

// App.FrontendMount is where the bundle is served — what a renderer must
// link assets under.
func TestApp_FrontendMount(t *testing.T) {
	t.Setenv("GIN_MODE", "test")
	t.Setenv(dev.Env, "")
	cases := []struct {
		prefix, at, want string
	}{
		{"", "", ""},
		{"/v1", "", "/v1"},
		{"", "/admin", "/admin"},
		{"", "admin/", "/admin"},
		{"", "/", ""},
		{"/v1", "/admin", "/v1/admin"},
	}
	for _, c := range cases {
		app := New(config.Runtime{Server: config.Server{RoutePrefix: c.prefix}})
		if got := app.FrontendMount(); got != "" {
			t.Fatalf("before ServeFrontend: %q", got)
		}
		if err := mountFrontend(app, viteBundle(), &frontendConfig{mountPath: c.at}); err != nil {
			t.Fatal(err)
		}
		if got := app.FrontendMount(); got != c.want {
			t.Errorf("prefix %q + FrontendAt(%q): FrontendMount() = %q, want %q", c.prefix, c.at, got, c.want)
		}
		if rec := get(app, c.want+"/static/index-DfUgamcr.js"); rec.Code != 200 {
			t.Errorf("an asset is not served under FrontendMount() %q: %d", c.want, rec.Code)
		}
	}
}

func TestWithReloadShim(t *testing.T) {
	const tag = `<script src="/__nexus/dev/script.js"></script>`
	cases := []struct {
		name string
		dev  bool
		in   string
		want string
	}{
		{"before head end", true, "<html><head><title>x</title></head><body></body></html>",
			"<html><head><title>x</title>" + tag + "</head><body></body></html>"},
		{"case-insensitive", true, "<HTML><HEAD></HEAD></HTML>", "<HTML><HEAD>" + tag + "</HEAD></HTML>"},
		{"no head: before body end", true, "<body><div id=app></div></body>", "<body><div id=app></div>" + tag + "</body>"},
		{"neither: appended", true, "<div id=app></div>", "<div id=app></div>" + tag},
		{"not twice", true, "<head>" + tag + "</head>", "<head>" + tag + "</head>"},
		{"not outside nexus dev", false, "<head></head>", "<head></head>"},
	}
	for _, c := range cases {
		if got := string(withReloadShim(c.dev, []byte(c.in))); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

// noShim removes the reload shim nexus dev adds to SPA pages, for tests
// whose subject is something else about the page.
func noShim(page string) string { return strings.Replace(page, devReloadScriptTag, "", 1) }

// viteDevFixture is a project dir with web/dist on disk, mounted the way
// ServeFrontend mounts it under nexus dev: disk FS, NEXUS_DEV=1, the hot
// reader rooted at NEXUS_DEV_ROOT/web/dist.
type viteDevFixture struct {
	dir, dist string
	app       *App
}

func newViteDevFixture(t *testing.T, index string, opts ...FrontendOption) *viteDevFixture {
	t.Helper()
	t.Setenv("GIN_MODE", "test")
	dir := t.TempDir()
	dist := filepath.Join(dir, "web", "dist")
	if err := os.MkdirAll(filepath.Join(dist, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dist, "index.html"), []byte(index), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dist, "assets", "app.js"), []byte("built-js"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(dev.Env, "1")
	t.Setenv(dev.RootEnv, dir)

	cfg := &frontendConfig{}
	for _, o := range opts {
		o.applyToFrontend(cfg)
	}
	app := New(config.Runtime{})
	app.setFrontendSource(os.DirFS(dir), "web/dist")
	sub, err := fs.Sub(os.DirFS(dir), "web/dist")
	if err != nil {
		t.Fatal(err)
	}
	if err := mountFrontend(app, sub, cfg); err != nil {
		t.Fatalf("mountFrontend: %v", err)
	}
	stopFrontendOnCleanup(t, app)
	return &viteDevFixture{dir: dir, dist: dist, app: app}
}

// stopFrontendOnCleanup ends the dev-reload poller and watcher a direct
// mountFrontend under nexus dev starts; InProcess apps stop them in OnStop.
func stopFrontendOnCleanup(t *testing.T, app *App) {
	t.Helper()
	if app.frontendStop != nil {
		t.Cleanup(app.frontendStop)
	}
}

func (f *viteDevFixture) writeHot(t *testing.T, h vitehot.Hot) {
	t.Helper()
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	f.writeHotRaw(t, b)
}

func (f *viteDevFixture) writeHotRaw(t *testing.T, b []byte) {
	t.Helper()
	p := vitehot.Path(f.dist)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *viteDevFixture) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	// A browser on the developer's machine, as in any dev session.
	req.RemoteAddr = "127.0.0.1:50000"
	req.Host = "localhost:8080"
	f.app.engine.ServeHTTP(rec, req)
	return rec
}

// viteIndex is what Vite 6 returns for GET /index.html: base already applied,
// root-relative (server.origin is NOT prefixed in HTML), the page's own
// inline module turned into an html-proxy src, and a plugin-injected inline
// module (react-refresh preamble) left inline.
func viteIndex(base string) string {
	return `<!doctype html>
<html><head>
  <script type="module" src="` + base + `@vite/client"></script>
<link rel="icon" href="` + base + `favicon.svg">
<link rel='stylesheet' href='` + base + `src/style.css'>
<script type="module" src="` + base + `index.html?html-proxy&index=0.js"></script>
<script type="module">import RefreshRuntime from "` + base + `@react-refresh"
RefreshRuntime.injectIntoGlobalHook(window); import('` + base + `src/lazy.ts')</script>
<script>var legacy = "/not/a/module";</script>
<script src="/__nexus/dev/script.js"></script>
<link rel="preconnect" href="//cdn.example.com">
</head><body><div id="app"></div>
<a href="/about">about</a>
<script type="module" src="` + base + `src/main.ts"></script>
<img src="` + base + `src/logo.png">
</body></html>
`
}

func newFakeVite(t *testing.T, base string, hits *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits = append(*hits, r.URL.Path)
		if r.URL.Path != base+"index.html" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, viteIndex(base))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestServeFrontend_ViteHotServesTransformedIndex: with a live hot file, every
// index.html response is Vite's transformed page with its asset URLs pointed
// at the dev server — and nothing that belongs on the Go origin is moved.
func TestServeFrontend_ViteHotServesTransformedIndex(t *testing.T) {
	for _, base := range []string{"/", "/app/"} {
		t.Run("base="+base, func(t *testing.T) {
			var hits []string
			srv := newFakeVite(t, base, &hits)
			f := newViteDevFixture(t, "<html>built</html>")
			f.writeHot(t, vitehot.Hot{Version: 1, Origin: srv.URL, Base: base, Entries: []string{"index.html"}, PID: os.Getpid()})

			for _, p := range []string{"/", "/index.html", "/users/42"} {
				rec := f.get(t, p)
				if rec.Code != 200 {
					t.Fatalf("GET %s: status %d body=%s", p, rec.Code, rec.Body)
				}
				if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
					t.Errorf("GET %s: cache-control %q, want no-cache", p, cc)
				}
				body := rec.Body.String()
				o := srv.URL + base
				for _, want := range []string{
					`<script type="module" src="` + o + `@vite/client">`,
					`<link rel="icon" href="` + o + `favicon.svg">`,
					`<link rel='stylesheet' href='` + o + `src/style.css'>`,
					`src="` + o + `index.html?html-proxy&index=0.js"`,
					`import RefreshRuntime from "` + o + `@react-refresh"`,
					`import('` + o + `src/lazy.ts')`,
					`<script type="module" src="` + o + `src/main.ts">`,
					`<img src="` + o + `src/logo.png">`,
					// Left on the Go origin: classic inline JS, the
					// app's own /__nexus surface, protocol-relative
					// URLs, and navigation.
					`var legacy = "/not/a/module";`,
					`<script src="/__nexus/dev/script.js">`,
					`href="//cdn.example.com"`,
					`<a href="/about">`,
				} {
					if !strings.Contains(body, want) {
						t.Errorf("GET %s: missing %q in\n%s", p, want, body)
					}
				}
				if strings.Contains(body, "built") {
					t.Errorf("GET %s: served the on-disk build instead of the dev page", p)
				}
				if base != "/" && strings.Contains(body, base+strings.TrimPrefix(base, "/")) {
					t.Errorf("GET %s: base applied twice:\n%s", p, body)
				}
			}
			for _, h := range hits {
				if h != base+"index.html" {
					t.Errorf("dev server got unexpected request %q", h)
				}
			}
			if len(hits) != 3 {
				t.Errorf("dev server fetches = %d, want one per page request (no caching)", len(hits))
			}

			// A file the dev server doesn't have comes from disk.
			if rec := f.get(t, "/assets/app.js"); rec.Code != 200 || rec.Body.String() != "built-js" {
				t.Errorf("asset: %d %q", rec.Code, rec.Body)
			}
			if last := hits[len(hits)-1]; last != base+"assets/app.js" {
				t.Errorf("the dev server was not asked first for a static file: %q", last)
			}
		})
	}
}

// TestServeFrontend_ViteHotFetchFailsFallsBackToDisk: when the dev server
// won't hand over index.html, the on-disk page is served with the client
// injected and its module scripts loaded from the dev server.
func TestServeFrontend_ViteHotFetchFailsFallsBackToDisk(t *testing.T) {
	notFound := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(notFound.Close)
	notHTML := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{}`)
	}))
	t.Cleanup(notHTML.Close)
	down := httptest.NewServer(http.NotFoundHandler())
	downURL := down.URL
	down.Close()

	sourceIndex := `<!doctype html><html><head><title>t</title></head><body><div id="app"></div><script type="module" src="/src/main.ts"></script></body></html>`
	builtIndex := `<!doctype html><html><head><script type="module" crossorigin src="/assets/index-abc.js"></script></head><body><div id="app"></div></body></html>`

	cases := []struct {
		name, origin, base, index string
		entries                   []string
		want                      []string
		wantCount                 map[string]int
	}{
		{
			name: "html entry · nothing extra injected", origin: notFound.URL, base: "/", index: sourceIndex,
			entries: []string{"index.html"},
			want: []string{
				`src="` + notFound.URL + `/@vite/client"`,
				`<script type="module" src="` + notFound.URL + `/src/main.ts"></script></body>`,
			},
			wantCount: map[string]int{"<script": 2, "index.html": 0},
		},
		{
			name: "404 · source index", origin: notFound.URL, base: "/", index: sourceIndex,
			want: []string{
				`<head>` + "\n" + `<script type="module" src="` + notFound.URL + `/@vite/client"></script>`,
				`<script type="module" src="` + notFound.URL + `/src/main.ts"></script>`,
			},
			wantCount: map[string]int{"/src/main.ts": 1},
		},
		{
			name: "not html · base", origin: notHTML.URL, base: "/app/", index: sourceIndex,
			want: []string{
				`src="` + notHTML.URL + `/app/@vite/client"`,
				`src="` + notHTML.URL + `/app/src/main.ts"`,
			},
			wantCount: map[string]int{"/src/main.ts": 1},
		},
		{
			name: "connection refused · built index gets the entry", origin: downURL, base: "/", index: builtIndex,
			want: []string{
				`src="` + downURL + `/@vite/client"`,
				`<script type="module" src="` + downURL + `/src/main.ts"></script>` + "\n</body>",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newViteDevFixture(t, tc.index)
			entries := tc.entries
			if entries == nil {
				entries = []string{"index.html", "src/main.ts"}
			}
			f.writeHot(t, vitehot.Hot{Version: 1, Origin: tc.origin, Base: tc.base, Entries: entries, PID: os.Getpid()})
			rec := f.get(t, "/dashboard")
			if rec.Code != 200 {
				t.Fatalf("status %d body=%s", rec.Code, rec.Body)
			}
			body := noShim(rec.Body.String())
			for _, w := range tc.want {
				if !strings.Contains(body, w) {
					t.Errorf("missing %q in\n%s", w, body)
				}
			}
			for s, n := range tc.wantCount {
				if got := strings.Count(body, s); got != n {
					t.Errorf("%q appears %d times, want %d:\n%s", s, got, n, body)
				}
			}
			if strings.Count(body, "@vite/client") != 1 {
				t.Errorf("client injected %d times:\n%s", strings.Count(body, "@vite/client"), body)
			}
		})
	}
}

// TestServeFrontend_ViteHotErrorPage: a hot file that exists but can't be
// understood is reported on the page, never papered over with the build.
func TestServeFrontend_ViteHotErrorPage(t *testing.T) {
	cases := []struct {
		name  string
		write func(*viteDevFixture)
		want  string
	}{
		{"malformed", func(f *viteDevFixture) { f.writeHotRaw(t, []byte("{nope")) }, "not valid JSON"},
		{"unknown version", func(f *viteDevFixture) {
			f.writeHot(t, vitehot.Hot{Version: 99, Origin: "http://127.0.0.1:1"})
		}, "schema version 99"},
		{"invalid origin", func(f *viteDevFixture) {
			f.writeHot(t, vitehot.Hot{Version: 1, Origin: `http://127.0.0.1:1"><script>alert(1)</script>`, PID: os.Getpid()})
		}, "invalid origin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newViteDevFixture(t, "<html>built</html>")
			tc.write(f)
			for _, p := range []string{"/", "/index.html", "/x/y"} {
				rec := f.get(t, p)
				if rec.Code != http.StatusServiceUnavailable {
					t.Fatalf("GET %s: status %d, want 503", p, rec.Code)
				}
				body := rec.Body.String()
				if !strings.Contains(body, tc.want) || !strings.Contains(body, "nexus-hot.json") {
					t.Errorf("GET %s: error page lacks %q / the file path:\n%s", p, tc.want, body)
				}
				if strings.Contains(body, "built") || strings.Contains(body, "<script>alert(1)") {
					t.Errorf("GET %s: stale build served, or markup from the file injected:\n%s", p, body)
				}
			}
		})
	}
}

// TestServeFrontend_StaleHotFileServesTheBuild: nexus dev stops Vite with
// SIGKILL, so a hot file naming a dead dev server is routine. It reads as
// absent — the build is served, never an error page.
func TestServeFrontend_StaleHotFileServesTheBuild(t *testing.T) {
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Skipf("no `true` binary: %v", err)
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()

	f := newViteDevFixture(t, "<html>built</html>")
	f.writeHot(t, vitehot.Hot{Version: 1, Origin: closedURL, Entries: []string{"index.html"}, PID: dead.ProcessState.Pid()})
	for _, p := range []string{"/", "/index.html", "/x/y"} {
		if rec := f.get(t, p); rec.Code != 200 || noShim(rec.Body.String()) != "<html>built</html>" {
			t.Errorf("GET %s: %d %q, want the build", p, rec.Code, rec.Body)
		}
	}
	if rec := f.get(t, "/assets/app.js"); rec.Code != 200 || rec.Body.String() != "built-js" {
		t.Errorf("asset: %d %q", rec.Code, rec.Body)
	}
	doc, err := f.app.FrontendDocument(context.Background())
	if err != nil || doc.FromDevServer || string(doc.HTML) != "<html>built</html>" {
		t.Errorf("FrontendDocument = (%q, %v, %v), want the built page", doc.HTML, doc.FromDevServer, err)
	}
}

// TestServeFrontend_NoHotFileUnchanged: no hot file, or hot files disabled
// (a production binary), and the page is the bundle's index.html byte for byte.
func TestServeFrontend_NoHotFileUnchanged(t *testing.T) {
	const index = `<html><head></head><body><script type="module" src="/assets/app.js"></script></body></html>`
	t.Run("dev, no hot file", func(t *testing.T) {
		f := newViteDevFixture(t, index)
		for _, p := range []string{"/", "/index.html", "/a/b"} {
			if rec := f.get(t, p); rec.Code != 200 || noShim(rec.Body.String()) != index {
				t.Errorf("GET %s: %d %q", p, rec.Code, rec.Body)
			}
		}
	})
	t.Run("production ignores a hot file", func(t *testing.T) {
		f := newViteDevFixture(t, index)
		f.writeHot(t, vitehot.Hot{Version: 1, Origin: "http://127.0.0.1:1", PID: os.Getpid()})
		t.Setenv(dev.Env, "") // the reader consults Enabled per call
		for _, p := range []string{"/", "/index.html", "/a/b"} {
			if rec := f.get(t, p); rec.Code != 200 || noShim(rec.Body.String()) != index {
				t.Errorf("GET %s: %d %q", p, rec.Code, rec.Body)
			}
		}
	})
}

// TestServeFrontend_NeverServesHotFile: nothing under .vite/ — the hot file,
// the plugin's temp file, the build manifest, the directory itself — is
// served, in any mode, with or without a dev server, at the root or under
// FrontendAt.
func TestServeFrontend_NeverServesHotFile(t *testing.T) {
	hotPaths := []string{
		"/.vite/nexus-hot.json",
		"/.vite/nexus-hot.json.4242.tmp",
		"/.vite/manifest.json",
		"/.vite/",
		"/.vite",
		"/.VITE/Manifest.JSON",
		"/.vite/./nexus-hot.json",
		"/assets/../.vite/nexus-hot.json",
		"/.vite//nexus-hot.json",
		"/.VITE/Nexus-Hot.JSON",
	}
	hot := vitehot.Hot{Version: 1, Origin: "http://127.0.0.1:1", PID: os.Getpid()}

	check := func(t *testing.T, f *viteDevFixture, prefix string) {
		t.Helper()
		for _, p := range hotPaths {
			rec := f.get(t, prefix+p)
			// The router canonicalises unclean paths with a redirect
			// before the SPA handler runs; follow it once.
			if loc := rec.Header().Get("Location"); rec.Code/100 == 3 && loc != "" {
				rec = f.get(t, loc)
			}
			if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "127.0.0.1:1") {
				t.Errorf("GET %s: %d %q, want 404", prefix+p, rec.Code, rec.Body)
			}
		}
	}
	for _, mount := range []string{"", "/admin"} {
		var opts []FrontendOption
		if mount != "" {
			opts = append(opts, FrontendAt(mount))
		}
		t.Run("dev, no hot file, mount="+mount, func(t *testing.T) {
			f := newViteDevFixture(t, "<html>x</html>", opts...)
			check(t, f, mount)
		})
		t.Run("dev, hot file present, mount="+mount, func(t *testing.T) {
			f := newViteDevFixture(t, "<html>x</html>", opts...)
			f.writeHot(t, hot)
			for _, name := range []string{"manifest.json", "nexus-hot.json.4242.tmp"} {
				if err := os.WriteFile(filepath.Join(f.dist, ".vite", name), []byte(`{}`), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			check(t, f, mount)
		})
	}

	// Production: a stale hot file baked into the embed by all:web/dist.
	t.Run("production embed", func(t *testing.T) {
		t.Setenv("GIN_MODE", "test")
		t.Setenv(dev.Env, "")
		b, _ := json.Marshal(hot)
		fsys := fstest.MapFS{
			"index.html":                    {Data: []byte("<html>x</html>")},
			".vite/nexus-hot.json":          {Data: b},
			".vite/manifest.json":           {Data: []byte(`{}`)},
			".vite/nexus-hot.json.4242.tmp": {Data: b},
		}
		for _, mount := range []string{"", "/admin"} {
			app := New(config.Runtime{})
			if err := mountFrontend(app, fsys, &frontendConfig{mountPath: mount}); err != nil {
				t.Fatal(err)
			}
			f := &viteDevFixture{app: app}
			check(t, f, mount)
		}
	})
}

func TestAbsolutizeDevHTML_LeavesAbsoluteURLs(t *testing.T) {
	in := `<script type="module" src="http://127.0.0.1:5173/@vite/client"></script><link href="https://x/y.css"><script type="module">import a from "./rel.js"; import b from "https://x/b.js"</script>`
	got := string(absolutizeDevHTML([]byte(in), "http://vite"))
	if got != in {
		t.Errorf("absolute/relative URLs changed:\n got %s\nwant %s", got, in)
	}
}

// The Inertia SSR bundle `nexus build` writes into dist/ssr rides the embed
// but is server code: never served. A public/ssr/ folder in a bundle with
// no SSR build is an ordinary directory.
func TestServeFrontend_SSRBundleNeverServed(t *testing.T) {
	t.Setenv("GIN_MODE", "test")
	t.Setenv(dev.Env, "")
	get := func(fsys fstest.MapFS, p string) int {
		app := New(config.Runtime{})
		if err := mountFrontend(app, fsys, &frontendConfig{}); err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		app.engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		return rec.Code
	}
	withSSR := fstest.MapFS{
		"index.html":      {Data: []byte("<html>x</html>")},
		"ssr/ssr.js":      {Data: []byte("export default 1")},
		"ssr/favicon.ico": {Data: []byte("ico")},
		"assets/app-1.js": {Data: []byte("app")},
	}
	for _, p := range []string{"/ssr/ssr.js", "/ssr/favicon.ico", "/SSR/ssr.js", "/assets/../ssr/ssr.js"} {
		if code := get(withSSR, p); code != http.StatusNotFound && code/100 != 3 {
			t.Errorf("GET %s with an SSR build: %d, want 404", p, code)
		}
	}
	if code := get(withSSR, "/assets/app-1.js"); code != http.StatusOK {
		t.Errorf("client asset: %d", code)
	}
	plain := fstest.MapFS{
		"index.html":     {Data: []byte("<html>x</html>")},
		"ssr/notes.json": {Data: []byte("{}")},
	}
	if code := get(plain, "/ssr/notes.json"); code != http.StatusOK {
		t.Errorf("an ssr/ folder without an SSR build: %d, want 200", code)
	}
}

// A stopped app leaves no dev-reload poller or watcher running.
func TestDevReloadStopsWithApp(t *testing.T) {
	count := func() int {
		buf := make([]byte, 1<<22)
		n := runtime.Stack(buf, true)
		return strings.Count(string(buf[:n]), "nexus.mountDevReload.func")
	}
	before := count()
	dir := t.TempDir()
	t.Setenv(dev.Env, "1")
	t.Setenv(dev.RootEnv, dir)
	for i := 0; i < 3; i++ {
		fsys := fstest.MapFS{"web/dist/index.html": {Data: []byte("<html>x</html>")}}
		_, stop, err := InProcess(config.Runtime{}, ServeFrontend(fsys, "web/dist"))
		if err != nil {
			t.Fatal(err)
		}
		if err := stop(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for count() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := count() - before; n > 0 {
		t.Errorf("%d dev-reload goroutines outlived their stopped apps", n)
	}
}
