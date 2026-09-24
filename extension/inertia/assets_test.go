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
// returns the app and the on-disk dist dir the hot file lives under.
func assetApp(t *testing.T, env string, files fstest.MapFS, cfg inertia.Config) (*nexustest.App, string) {
	t.Helper()
	t.Setenv(nexus.NexusDevEnv, "")
	t.Setenv("NEXUS_VITE_DEV", os.Getenv("NEXUS_VITE_DEV")) // restored after the test
	root := t.TempDir()
	t.Setenv(nexus.NexusDevRootEnv, root)
	if files == nil {
		files = fstest.MapFS{}
	}
	files["dist/index.html"] = &fstest.MapFile{Data: []byte("<!doctype html><div id=app></div>")}
	app := nexustest.New(t, nexus.Config{Environment: env},
		nexus.ServeFrontend(files, "dist"),
		inertia.Module(cfg),
		inertia.Page("GET", "/p", "P", NewWidgets),
	)
	return app, filepath.Join(root, "dist")
}

func withManifest() fstest.MapFS {
	return fstest.MapFS{"dist/.vite/manifest.json": {Data: []byte(manifestJSON)}}
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

// TestHotFileTags: a hot file wins over the manifest; tags use its origin,
// base and first non-.html entry, React is detected from that entry, and the
// version is empty while a dev server serves the assets.
func TestHotFileTags(t *testing.T) {
	app, dist := assetApp(t, "development", withManifest(), inertia.Config{})
	writeHot(t, dist, `{"version":1,"origin":"http://127.0.0.1:5173","base":"/app/","entries":["index.html","src/main.tsx"],"pid":0}`)

	res := fullLoad(app).AssertOK()
	body := res.String()
	mustContain(t, body,
		`<script type="module" src="http://127.0.0.1:5173/app/@vite/client"></script>`,
		`<script type="module" src="http://127.0.0.1:5173/app/src/main.tsx"></script>`,
		`http://127.0.0.1:5173/app/@react-refresh`,
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
	writeHot(t, dist, `{"version":1,"origin":"http://127.0.0.1:5173","base":"/","entries":["index.html"],"pid":0}`)

	body := fullLoad(app).AssertOK().String()
	mustContain(t, body, `src="http://127.0.0.1:5173/src/app.ts"`)
	mustNotContain(t, body, "index.html\"></script>", "@react-refresh")
}

// TestHotFileRestartNewPort: a dev server restart on another port is picked up
// on the next page load, with the same engine.
func TestHotFileRestartNewPort(t *testing.T) {
	app, dist := assetApp(t, "development", withManifest(), inertia.Config{})
	writeHot(t, dist, `{"version":1,"origin":"http://127.0.0.1:5173","base":"/","entries":["src/main.ts"],"pid":0}`)
	mustContain(t, fullLoad(app).AssertOK().String(), "http://127.0.0.1:5173/@vite/client")

	writeHot(t, dist, `{"version":1,"origin":"http://127.0.0.1:15173","base":"/","entries":["src/main.ts"],"pid":0}`)
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(vitehot.Path(dist), later, later); err != nil {
		t.Fatal(err)
	}
	body := fullLoad(app).AssertOK().String()
	mustContain(t, body, "http://127.0.0.1:15173/@vite/client", "http://127.0.0.1:15173/src/main.ts")
	mustNotContain(t, body, "127.0.0.1:5173/")
}

// TestHotFileBrokenShowsDevError: a malformed or stale hot file is reported on
// the page, not silently replaced by the build manifest.
func TestHotFileBrokenShowsDevError(t *testing.T) {
	cases := map[string]struct{ hot, want string }{
		"malformed": {`{"version":1,`, "not valid JSON"},
		"version":   {`{"version":99,"origin":"http://127.0.0.1:5173"}`, "schema version 99"},
		"stale":     {`{"version":1,"origin":"http://127.0.0.1:5173","pid":2147480000}`, "no longer running"},
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

// TestHotFileIgnoredInProduction: a hot file left on disk never redirects a
// production page.
func TestHotFileIgnoredInProduction(t *testing.T) {
	app, dist := assetApp(t, "production", withManifest(), inertia.Config{})
	writeHot(t, dist, `{"version":1,"origin":"http://127.0.0.1:5173","base":"/","entries":["src/main.ts"],"pid":0}`)
	body := fullLoad(app).AssertOK().String()
	mustContain(t, body, "/assets/main-abc123.js")
	mustNotContain(t, body, "5173")
}

// TestHotFileAbsentUsesEnvFallback: with no hot file, NEXUS_VITE_DEV keeps
// working exactly as before.
func TestHotFileAbsentUsesEnvFallback(t *testing.T) {
	app, _ := assetApp(t, "development", withManifest(), inertia.Config{})
	t.Setenv("NEXUS_VITE_DEV", "http://localhost:5199/")
	body := fullLoad(app).AssertOK().String()
	mustContain(t, body,
		`<script src="/__nexus/dev/script.js"></script>`,
		`<script type="module" src="http://localhost:5199/@vite/client"></script>`,
		`<script type="module" src="http://localhost:5199/src/main.ts"></script>`,
	)
	mustNotContain(t, body, "/assets/main-abc123.js")
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
