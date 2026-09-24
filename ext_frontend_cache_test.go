package nexus

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

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
	app := New(Config{})
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
	app := New(Config{})
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
	app := New(Config{})
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
	app := New(Config{})
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
	err := mountFrontend(New(Config{}), fstest.MapFS{"assets/x.js": {Data: []byte("x")}}, noFrontendCfg)
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

// `npm run dev` + `go run .` with environment = "development" must boot
// before anything is built: the frontend is the dev server the hot file
// names, and the embedded bundle may be empty.
func TestServeFrontend_UnbuiltBundleBootsInDevelopment(t *testing.T) {
	t.Setenv("GIN_MODE", "test")
	app := New(Config{Environment: "development"})
	if err := mountFrontend(app, fstest.MapFS{}, noFrontendCfg); err != nil {
		t.Fatalf("development must not fail fast on an unbuilt bundle: %v", err)
	}
	if rec := get(app, "/"); rec.Code != 200 {
		t.Fatalf("want the placeholder page, got %d", rec.Code)
	}
}
