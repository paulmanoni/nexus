package inertia_test

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension/inertia"
	"github.com/paulmanoni/nexus/internal/vitehot"
	"github.com/paulmanoni/nexus/nexustest"
)

// assetApp boots an in-process app that registers its bundle through
// ServeFrontend (so App.ViteHot watches <tmp>/dist) and one page at /p. It
// returns the app and the on-disk dist dir the hot file lives under. The
// bundle gets a stub index.html unless files carries one.
func assetApp(t *testing.T, env string, files fstest.MapFS, cfg inertia.Config) (*nexustest.App, string) {
	t.Helper()
	if files == nil {
		files = fstest.MapFS{}
	}
	if _, ok := files["dist/index.html"]; !ok {
		files["dist/index.html"] = &fstest.MapFile{Data: []byte("<!doctype html><div id=app></div>")}
	}
	return bootAssets(t, nexus.Config{Environment: env}, files, cfg)
}

// bootAssets is assetApp without the index.html default: files is the whole
// bundle, and ServeFrontend takes fopts.
func bootAssets(t *testing.T, config nexus.Config, files fstest.MapFS, cfg inertia.Config, fopts ...nexus.FrontendOption) (*nexustest.App, string) {
	t.Helper()
	t.Setenv(nexus.NexusDevEnv, "")
	t.Setenv("NEXUS_VITE_DEV", os.Getenv("NEXUS_VITE_DEV")) // restored after the test
	root := t.TempDir()
	t.Setenv(nexus.NexusDevRootEnv, root)
	app := nexustest.New(t, config,
		nexus.ServeFrontend(files, "dist", fopts...),
		inertia.Module(cfg),
		inertia.Page("GET", "/p", "P", NewWidgets),
	)
	return app, filepath.Join(root, "dist")
}

// builtIndex is the index.html `vite build` emits for manifestJSON: Vite's
// hashed tags already in it, plus what an app puts there itself.
const builtIndex = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Widgets Inc</title>
<meta name="description" content="widgets">
<link rel="stylesheet" href="/icons/font.css">
<script type="module" crossorigin src="/assets/main-abc123.js"></script>
<link rel="stylesheet" crossorigin href="/assets/main-xyz.css">
</head>
<body>
<div id="app"><div class="loader"></div></div>
</body>
</html>
`

func withManifest() fstest.MapFS {
	return fstest.MapFS{"dist/.vite/manifest.json": {Data: []byte(manifestJSON)}}
}

// withBuild is a full `vite build` output: the manifest and index.html.
func withBuild(index string) fstest.MapFS {
	files := withManifest()
	files["dist/index.html"] = &fstest.MapFile{Data: []byte(index)}
	return files
}

// fakeVite stands in for a Vite dev server. It answers for its client (the
// liveness probe App.ViteHot makes). With index set it serves that as its
// transformed index.html (under any base); otherwise /index.html is a 404, as
// from a dev server that doesn't serve the page. It counts index requests.
func fakeVite(t *testing.T, index string) (origin string, hits *atomic.Int32) {
	t.Helper()
	hits = new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/@vite/client") {
			w.Header().Set("Content-Type", "text/javascript")
			_, _ = w.Write([]byte("export {}"))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/index.html") {
			hits.Add(1)
			if index != "" {
				w.Header().Set("Content-Type", "text/html")
				_, _ = w.Write([]byte(index))
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, hits
}

// hotFor is a hot file naming origin.
func hotFor(origin, base string, entries ...string) string {
	b, _ := json.Marshal(map[string]any{"version": 1, "origin": origin, "base": base, "entries": entries, "pid": 0})
	return string(b)
}

func writeHot(t *testing.T, dist, body string) {
	t.Helper()
	p := vitehot.Path(dist)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fullLoad(app *nexustest.App) *nexustest.Response {
	return app.Do(httptest.NewRequest(http.MethodGet, "/p", nil))
}

func xhrVisit(app *nexustest.App) *nexustest.Response {
	r := httptest.NewRequest(http.MethodGet, "/p", nil)
	r.Header.Set("X-Inertia", "true")
	return app.Do(r)
}

func mustContain(t *testing.T, body string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("missing %q in:\n%s", w, body)
		}
	}
}

func mustNotContain(t *testing.T, body string, bad ...string) {
	t.Helper()
	for _, b := range bad {
		if strings.Contains(body, b) {
			t.Errorf("unexpected %q in:\n%s", b, body)
		}
	}
}

// TestHotFileTags: a hot file wins over the manifest; with no index.html
// from the dev server the synthesised document's tags use its origin, base
// and first non-.html entry, React is detected from that entry, and the
// version is empty while a dev server serves the assets.
func TestHotFileTags(t *testing.T) {
	app, dist := assetApp(t, "development", withManifest(), inertia.Config{})
	vite, _ := fakeVite(t, "")
	writeHot(t, dist, hotFor(vite, "/app/", "index.html", "src/main.tsx"))

	res := fullLoad(app).AssertOK()
	body := res.String()
	mustContain(t, body,
		`<script type="module" src="`+vite+`/app/@vite/client"></script>`,
		`<script type="module" src="`+vite+`/app/src/main.tsx"></script>`,
		`import RefreshRuntime from "`+vite+`/app/@react-refresh"`,
	)
	mustNotContain(t, body, "/assets/main-abc123.js", "index.html\"></script>")

	var page struct{ Version string }
	xhrVisit(app).AssertOK().JSON(&page)
	if page.Version != "" {
		t.Errorf("version should be empty while the dev server serves assets, got %q", page.Version)
	}
}

// TestHotFileHTMLEntrySkipped: an .html-only entry list falls back to
// Config.Entry, and a .ts entry gets no React preamble.
func TestHotFileHTMLEntrySkipped(t *testing.T) {
	app, dist := assetApp(t, "development", withManifest(), inertia.Config{Entry: "src/app.ts"})
	vite, _ := fakeVite(t, "")
	writeHot(t, dist, hotFor(vite, "/", "index.html"))

	body := fullLoad(app).AssertOK().String()
	mustContain(t, body, `src="`+vite+`/src/app.ts"`)
	mustNotContain(t, body, "index.html\"></script>", "@react-refresh")
}

// TestHotFileRestartNewPort: a dev server restart on another port is picked up
// on the next page load, with the same engine.
func TestHotFileRestartNewPort(t *testing.T) {
	app, dist := assetApp(t, "development", withManifest(), inertia.Config{})
	first, _ := fakeVite(t, "")
	second, _ := fakeVite(t, "")
	writeHot(t, dist, hotFor(first, "/", "src/main.ts"))
	mustContain(t, fullLoad(app).AssertOK().String(), first+"/@vite/client")

	writeHot(t, dist, hotFor(second, "/", "src/main.ts"))
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(vitehot.Path(dist), later, later); err != nil {
		t.Fatal(err)
	}
	body := fullLoad(app).AssertOK().String()
	mustContain(t, body, second+"/@vite/client", second+"/src/main.ts")
	mustNotContain(t, body, first+"/")
}

// TestHotFileBrokenShowsDevError: a malformed hot file, or one of an unknown
// schema version, is reported on the page, not silently replaced by the build
// manifest. (Origins here are never dialled: the file is rejected first.)
func TestHotFileBrokenShowsDevError(t *testing.T) {
	cases := map[string]struct{ hot, want string }{
		"malformed": {`{"version":1,`, "not valid JSON"},
		"version":   {`{"version":99,"origin":"http://127.0.0.1:9"}`, "schema version 99"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			app, dist := assetApp(t, "development", withManifest(), inertia.Config{})
			writeHot(t, dist, tc.hot)
			res := fullLoad(app).AssertStatus(http.StatusInternalServerError)
			body := res.String()
			mustContain(t, body, tc.want, vitehot.Path(dist), "hot file can")
			mustNotContain(t, body, "/assets/main-abc123.js", "data-page")
		})
	}
}

// TestHotFileDeadServerFallsThrough: a hot file whose dev server is gone
// (no live pid, origin not answering) reads as absent: the build renders, no
// error page.
func TestHotFileDeadServerFallsThrough(t *testing.T) {
	app, dist := assetApp(t, "development", withBuild(builtIndex), inertia.Config{})
	srv := httptest.NewServer(http.NotFoundHandler())
	dead := srv.URL
	srv.Close()
	writeHot(t, dist, hotFor(dead, "/", "src/main.ts"))
	body := fullLoad(app).AssertOK().String()
	mustContain(t, body, "/assets/main-abc123.js", "<title>Widgets Inc</title>", `data-page="`)
	mustNotContain(t, body, dead)
}

// TestHotFileIgnoredInProduction: a hot file left on disk never redirects a
// production page.
func TestHotFileIgnoredInProduction(t *testing.T) {
	app, dist := assetApp(t, "production", withBuild(builtIndex), inertia.Config{})
	vite, hits := fakeVite(t, `<!doctype html><div id="app"></div>`)
	writeHot(t, dist, hotFor(vite, "/", "src/main.ts"))
	body := fullLoad(app).AssertOK().String()
	mustContain(t, body, "/assets/main-abc123.js", "<title>Widgets Inc</title>")
	mustNotContain(t, body, vite)
	if n := hits.Load(); n != 0 {
		t.Errorf("production asked the dev server for its page %d times", n)
	}
}

// TestHotFileAbsentUsesEnvFallback: with no hot file, NEXUS_VITE_DEV keeps
// working exactly as before — a synthesised document, even when the bundle
// has a built index.html (the viteless engine is not templated).
func TestHotFileAbsentUsesEnvFallback(t *testing.T) {
	app, _ := assetApp(t, "development", withBuild(builtIndex), inertia.Config{})
	t.Setenv("NEXUS_VITE_DEV", "http://localhost:5199/")
	body := fullLoad(app).AssertOK().String()
	mustContain(t, body,
		"<!doctype html>\n<html>\n<head>\n",
		`<script src="/__nexus/dev/script.js"></script>`,
		`<script type="module" src="http://localhost:5199/@vite/client"></script>`,
		`<script type="module" src="http://localhost:5199/src/main.ts"></script>`,
	)
	mustNotContain(t, body, "/assets/main-abc123.js", "Widgets Inc")
}

// TestNoAssetsDevErrorPage: no dev server and no manifest in development is
// an error page naming the hot file, the manifest paths, and both fixes.
func TestNoAssetsDevErrorPage(t *testing.T) {
	app, dist := assetApp(t, "development", nil, inertia.Config{})
	res := fullLoad(app).AssertStatus(http.StatusInternalServerError)
	if ct := res.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type=%q", ct)
	}
	mustContain(t, res.String(),
		"No frontend assets to load",
		vitehot.Path(dist),
		"dist/.vite/manifest.json",
		"dist/manifest.json",
		"nexus-vite-plugin",
		"build.manifest must be true",
	)
	mustNotContain(t, res.String(), "data-page", "<script", "<link")
}

// TestManifestWithoutEntry: a manifest with no entry chunk is the same
// problem as no manifest at all.
func TestManifestWithoutEntry(t *testing.T) {
	files := fstest.MapFS{"dist/.vite/manifest.json": {Data: []byte(`{"_x.js":{"file":"assets/x.js"}}`)}}
	app, _ := assetApp(t, "development", files, inertia.Config{})
	res := fullLoad(app).AssertStatus(http.StatusInternalServerError)
	mustContain(t, res.String(), "dist/.vite/manifest.json has no entry chunk")
}

// TestNoAssetsProdLogsOnce: in production the shell still renders and the
// problem is logged once, not per request.
func TestNoAssetsProdLogsOnce(t *testing.T) {
	var mu sync.Mutex
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}))
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })

	app, _ := assetApp(t, "production", nil, inertia.Config{})
	for i := 0; i < 3; i++ {
		body := fullLoad(app).AssertOK().String()
		mustContain(t, body, `data-page="`)
	}
	mu.Lock()
	out := buf.String()
	mu.Unlock()
	if n := strings.Count(out, "inertia: pages will render blank"); n != 1 {
		t.Fatalf("want exactly one log line, got %d:\n%s", n, out)
	}
	mustContain(t, out, "dist/.vite/manifest.json", "build.manifest: true")
}

// TestNoAssetsXHRUnaffected: an Inertia XHR visit carries no asset tags, so a
// missing manifest doesn't fail it, even in development.
func TestNoAssetsXHRUnaffected(t *testing.T) {
	app, _ := assetApp(t, "development", nil, inertia.Config{})
	res := xhrVisit(app).AssertOK()
	var page struct{ Component string }
	if err := json.Unmarshal(res.Body(), &page); err != nil {
		t.Fatalf("want JSON page object: %v\n%s", err, res.String())
	}
	if page.Component != "P" {
		t.Fatalf("component=%q", page.Component)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
