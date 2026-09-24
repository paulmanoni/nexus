package nexus

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/paulmanoni/nexus/internal/vitehot"
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

// Boot leniency for an unbuilt bundle needs evidence of development
// happening now — nexus dev, or a live Vite dev server — never the
// environment value alone: `nexus new` writes environment = "development"
// into nexus.toml, and deployments ship it.
func TestServeFrontend_UnbuiltBundleBootsInDevelopment(t *testing.T) {
	t.Setenv("GIN_MODE", "test")

	t.Run("environment alone fails fast", func(t *testing.T) {
		t.Setenv(NexusDevEnv, "")
		app := New(Config{Environment: "development"})
		err := mountFrontend(app, fstest.MapFS{}, noFrontendCfg)
		if err == nil || !strings.Contains(err.Error(), "neither index.html nor a Vite manifest") {
			t.Fatalf("a deployment shipping environment = development must still fail fast, got %v", err)
		}
	})

	t.Run("nexus dev boots to the placeholder", func(t *testing.T) {
		t.Setenv(NexusDevEnv, "1")
		t.Setenv(NexusDevRootEnv, t.TempDir())
		app := New(Config{})
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
			t.Setenv(NexusDevEnv, "")
			t.Setenv(NexusDevRootEnv, "")
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
			app := New(Config{Environment: "development"})
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
	app := New(Config{})
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
