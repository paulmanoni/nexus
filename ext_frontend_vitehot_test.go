package nexus

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
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
	t.Setenv(NexusDevEnv, "1")
	t.Setenv(NexusDevRootEnv, dir)

	cfg := &frontendConfig{}
	for _, o := range opts {
		o.applyToFrontend(cfg)
	}
	app := New(Config{})
	app.setFrontendSource(os.DirFS(dir), "web/dist")
	sub, err := fs.Sub(os.DirFS(dir), "web/dist")
	if err != nil {
		t.Fatal(err)
	}
	if err := mountFrontend(app, sub, cfg); err != nil {
		t.Fatalf("mountFrontend: %v", err)
	}
	return &viteDevFixture{dir: dir, dist: dist, app: app}
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
		t.Setenv(NexusDevEnv, "") // the reader consults Enabled per call
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
		t.Setenv(NexusDevEnv, "")
		b, _ := json.Marshal(hot)
		fsys := fstest.MapFS{
			"index.html":                    {Data: []byte("<html>x</html>")},
			".vite/nexus-hot.json":          {Data: b},
			".vite/manifest.json":           {Data: []byte(`{}`)},
			".vite/nexus-hot.json.4242.tmp": {Data: b},
		}
		for _, mount := range []string{"", "/admin"} {
			app := New(Config{})
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
