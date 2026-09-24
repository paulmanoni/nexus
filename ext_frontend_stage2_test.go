package nexus

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/paulmanoni/nexus/internal/vitehot"
)

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
	t.Setenv(NexusDevEnv, "") // hot files not honoured
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
	t.Setenv(NexusDevEnv, "1")
	t.Setenv(NexusDevRootEnv, dir)
	app := New(Config{})
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
	t.Setenv(NexusDevEnv, "")
	app := New(Config{})
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
		if _, err := New(Config{}).FrontendDocument(ctx); !errors.Is(err, ErrNoFrontendDocument) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("built index.html", func(t *testing.T) {
		t.Setenv("GIN_MODE", "test")
		t.Setenv(NexusDevEnv, "")
		app := New(Config{})
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
		t.Setenv(NexusDevEnv, "")
		app := New(Config{})
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
		t.Setenv(NexusDevEnv, "1")
		t.Setenv(NexusDevRootEnv, t.TempDir())
		app := New(Config{})
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
	t.Setenv(NexusDevEnv, "")
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
		app := New(Config{Server: ServerConfig{RoutePrefix: c.prefix}})
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
