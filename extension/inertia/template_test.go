package inertia_test

import (
	"bytes"
	"encoding/json"
	"html"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension/inertia"
	"github.com/paulmanoni/nexus/httpx"
)

// pageJSON is the data-page attribute value the engine writes for /p.
func pageJSON(t *testing.T, version string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"component": "P",
		"props":     map[string]any{"errors": map[string]any{}, "menu": []string{"home", "about"}, "title": "Widgets"},
		"url":       "/p",
		"version":   version,
	})
	if err != nil {
		t.Fatal(err)
	}
	return html.EscapeString(string(b))
}

// TestTemplateBuiltIndex: with a build, a full load is the built index.html
// byte for byte, except for data-page on the mount and Config.Head before
// </head>. The template's title, meta, stylesheets and Vite tags stay; the
// engine adds no asset tags of its own; a hard-coded data-page is replaced
// and the mount's loader markup kept.
func TestTemplateBuiltIndex(t *testing.T) {
	index := strings.Replace(builtIndex, `<div id="app">`,
		`<div id="app" data-page='{"component":"NotFound","props":{}}'>`, 1)
	app, _ := assetApp(t, "production", withBuild(index), inertia.Config{
		Head: inertia.Head{Meta: []inertia.Meta{{Name: "x-extra", Content: "1"}}},
	})
	res := fullLoad(app).AssertOK()

	want := strings.Replace(index, `<div id="app" data-page='{"component":"NotFound","props":{}}'>`,
		`<div id="app" data-page="`+pageJSON(t, wantVersion())+`">`, 1)
	want = strings.Replace(want, "</head>", `<meta name="x-extra" content="1"></head>`, 1)
	if got := res.String(); got != want {
		t.Fatalf("document differs from index.html beyond the page and Config.Head:\ngot:\n%s\nwant:\n%s", got, want)
	}
	if n := strings.Count(res.String(), "/assets/main-abc123.js"); n != 1 {
		t.Errorf("entry script appears %d times, want 1 (the template's own)", n)
	}
	if res.Header().Get("Vary") != "X-Inertia" {
		t.Errorf("Vary=%q", res.Header().Get("Vary"))
	}
}

// TestTemplateNonce: with a CSP nonce, every tag the engine injects carries
// it, and so do the template's own scripts and stylesheets — which is what
// the engine's manifest tags carried before — while a nonce the template set
// itself is left alone.
func TestTemplateNonce(t *testing.T) {
	index := strings.Replace(builtIndex, "</head>", `<script nonce="own">1</script></head>`, 1)
	app, _ := assetApp(t, "production", withBuild(index), inertia.Config{
		Head:  inertia.Head{Links: []inertia.Link{{Rel: "preconnect", Href: "https://fonts.example"}}, Raw: `<style>b{}</style>`},
		Nonce: func(*httpx.Ctx) string { return "n0nce" },
	})
	t.Setenv(nexus.NexusDevEnv, "1") // the reload shim is injected too
	body := fullLoad(app).AssertOK().String()
	mustContain(t, body,
		`<link nonce="n0nce" rel="preconnect" href="https://fonts.example">`,
		`<style nonce="n0nce">b{}</style>`,
		`<script nonce="n0nce" src="/__nexus/dev/script.js"></script>`,
		`<script nonce="n0nce" type="module" crossorigin src="/assets/main-abc123.js">`,
		`<link nonce="n0nce" rel="stylesheet" crossorigin href="/assets/main-xyz.css">`,
		`<link nonce="n0nce" rel="stylesheet" href="/icons/font.css">`,
		`<script nonce="own">1</script>`,
	)
	if n := strings.Count(body, "nonce="); n != 7 {
		t.Errorf("want 7 nonce attributes, got %d:\n%s", n, body)
	}
}

// TestTemplateSSR: SSR head tags go before </head>; the SSR body replaces the
// mount's content (a loader must not sit in front of hydration) and the mount
// is flagged data-server-rendered. A flag the template carries without SSR is
// removed, so the client mounts instead of hydrating nothing.
func TestTemplateSSR(t *testing.T) {
	ssr := &fakeSSR{res: inertia.SSRResult{Head: []string{`<meta name="ssr" content="1">`}, Body: "<main>rendered</main>"}}
	app, _ := assetApp(t, "production", withBuild(builtIndex), inertia.Config{SSR: ssr})
	body := fullLoad(app).AssertOK().String()
	mustContain(t, body,
		`<meta name="ssr" content="1"></head>`,
		`<div id="app" data-server-rendered="true" data-page="`,
		`"><main>rendered</main></div>`,
		"<title>Widgets Inc</title>",
	)
	mustNotContain(t, body, `class="loader"`)

	flagged := strings.Replace(builtIndex, `<div id="app">`, `<div id="app" data-server-rendered="true">`, 1)
	app, _ = assetApp(t, "production", withBuild(flagged), inertia.Config{})
	body = fullLoad(app).AssertOK().String()
	mustNotContain(t, body, "data-server-rendered")
	mustContain(t, body, `<div id="app" data-page="`, `<div class="loader"></div></div>`)
}

// devServerIndex is what a Vite dev server answers for /index.html: its client
// and the app entry injected by transformIndexHtml, URLs root-relative.
const devServerIndex = `<!doctype html>
<html>
<head>
<title>Widgets Dev</title>
<script type="module" src="/@vite/client"></script>
<link rel="stylesheet" href="/src/theme.css">
</head>
<body>
<div id="app"></div>
<script type="module" src="/src/main.ts"></script>
</body>
</html>
`

// TestTemplateFromDevServer: with a live dev server, a full load renders into
// the dev server's own index.html — its tags pointed at the dev server — with
// no second copy of the client or the entry, marked uncacheable, and with the
// reload shim added under nexus dev.
func TestTemplateFromDevServer(t *testing.T) {
	app, dist := assetApp(t, "development", withBuild(builtIndex), inertia.Config{})
	vite, _ := fakeVite(t, devServerIndex)
	writeHot(t, dist, hotFor(vite, "/", "index.html"))
	t.Setenv(nexus.NexusDevEnv, "1")

	res := fullLoad(app).AssertOK()
	body := res.String()
	mustContain(t, body,
		"<title>Widgets Dev</title>",
		`<link rel="stylesheet" href="`+vite+`/src/theme.css">`,
		`<script src="/__nexus/dev/script.js"></script></head>`,
		`<div id="app" data-page="`+pageJSON(t, "")+`"></div>`,
	)
	mustNotContain(t, body, "/assets/main-abc123.js", "Widgets Inc")
	for _, u := range []string{vite + "/@vite/client", vite + "/src/main.ts"} {
		if n := strings.Count(body, u); n != 1 {
			t.Errorf("%s appears %d times, want 1", u, n)
		}
	}
	if cc := res.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("a dev server's page must not be cached; Cache-Control=%q", cc)
	}

}

// TestTemplateShimNotDuplicated: under nexus dev, an index.html that loads
// the reload shim itself doesn't get a second one.
func TestTemplateShimNotDuplicated(t *testing.T) {
	index := strings.Replace(builtIndex, "</body>", `<script src="/__nexus/dev/script.js"></script></body>`, 1)
	app, _ := assetApp(t, "development", withBuild(index), inertia.Config{})
	t.Setenv(nexus.NexusDevEnv, "1")
	body := fullLoad(app).AssertOK().String()
	if n := strings.Count(body, "__nexus/dev/script.js"); n != 1 {
		t.Errorf("reload shim appears %d times, want 1:\n%s", n, body)
	}
}

// TestTemplateXHRUnaffected: an X-Inertia visit is the JSON page object; the
// document — and the dev server's index.html — is never fetched for it.
func TestTemplateXHRUnaffected(t *testing.T) {
	app, dist := assetApp(t, "development", withBuild(builtIndex), inertia.Config{})
	vite, hits := fakeVite(t, devServerIndex)
	writeHot(t, dist, hotFor(vite, "/", "index.html"))

	res := xhrVisit(app).AssertOK()
	if ct := res.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type=%q", ct)
	}
	var page struct{ Component string }
	res.JSON(&page)
	if page.Component != "P" {
		t.Fatalf("component=%q", page.Component)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("an XHR visit fetched the dev server's index.html %d times", n)
	}
}

// noMountIndex has "app" as an id only where a browser would not see one: in
// a comment, in a script string, in another attribute's value, in a title.
const noMountIndex = `<!doctype html>
<html>
<head>
<title><div id="app"></title>
<script type="module" src="/assets/main-abc123.js"></script>
<script>document.write('<div id="app"></div>')</script>
</head>
<body>
<!-- <div id="app"></div> -->
<div data-note='id="app"' class="x"></div>
<div id="application"></div>
</body>
</html>
`

// TestTemplateNoMountDev: a document without the mount element is a developer
// error: in development, an error page naming the id and the document.
func TestTemplateNoMountDev(t *testing.T) {
	app, _ := assetApp(t, "development", withBuild(noMountIndex), inertia.Config{})
	res := fullLoad(app).AssertStatus(http.StatusInternalServerError)
	mustContain(t, res.String(), "no mount element", `id=&#34;app&#34;`, "dist/index.html", "RootView")
	mustNotContain(t, res.String(), "data-page")

	// From a dev server, the message names the dev server's page.
	app, dist := assetApp(t, "development", withBuild(builtIndex), inertia.Config{})
	vite, _ := fakeVite(t, noMountIndex)
	writeHot(t, dist, hotFor(vite, "/", "index.html"))
	res = fullLoad(app).AssertStatus(http.StatusInternalServerError)
	mustContain(t, res.String(), "Vite dev server", vite+"/index.html")
}

// TestTemplateNoMountProd: in production the render falls back to the
// synthesised document (with the manifest's tags) and logs once.
func TestTemplateNoMountProd(t *testing.T) {
	out := captureLog(t)
	app, _ := assetApp(t, "production", withBuild(noMountIndex), inertia.Config{})
	for i := 0; i < 3; i++ {
		body := fullLoad(app).AssertOK().String()
		mustContain(t, body, `<div id="app" data-page="`, `<script type="module" src="/assets/main-abc123.js"></script>`)
		mustNotContain(t, body, "application")
	}
	if n := strings.Count(out(), "no mount element"); n != 1 {
		t.Fatalf("want one log line, got %d:\n%s", n, out())
	}
}

// TestTemplateRootView: the mount is found by Config.RootView, on any element
// and however its id attribute is written.
func TestTemplateRootView(t *testing.T) {
	index := "<!doctype html><html><head></head><body><MAIN class=shell ID=root>x</MAIN></body></html>"
	app, _ := assetApp(t, "production", withBuild(index), inertia.Config{RootView: "root"})
	body := fullLoad(app).AssertOK().String()
	mustContain(t, body, `<MAIN class=shell ID=root data-page="`, `">x</MAIN>`)
}

// TestModuleOnlyBuildMount: a module-only build (a manifest, no index.html)
// gets the synthesised document, its asset URLs under wherever ServeFrontend
// serves the bundle.
func TestModuleOnlyBuildMount(t *testing.T) {
	cases := []struct {
		name   string
		config nexus.Config
		fopts  []nexus.FrontendOption
		page   string
		prefix string
	}{
		{"root", nexus.Config{}, nil, "/p", ""},
		{"route prefix", nexus.Config{Server: nexus.ServerConfig{RoutePrefix: "/api"}}, nil, "/api/p", "/api"},
		{"FrontendAt", nexus.Config{}, []nexus.FrontendOption{nexus.FrontendAt("/admin")}, "/p", "/admin"},
		{"both", nexus.Config{Server: nexus.ServerConfig{RoutePrefix: "/api"}}, []nexus.FrontendOption{nexus.FrontendAt("/admin")}, "/api/p", "/api/admin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.config.Environment = "production"
			files := withManifest()
			files["dist/assets/main-abc123.js"] = &fstest.MapFile{Data: []byte("export {}")}
			app, _ := bootAssets(t, tc.config, files, inertia.Config{}, tc.fopts...)
			body := app.Do(httptest.NewRequest(http.MethodGet, tc.page, nil)).AssertOK().String()
			mustContain(t, body,
				"<!doctype html>\n<html>\n<head>\n",
				`<link rel="stylesheet" href="`+tc.prefix+`/assets/main-xyz.css">`,
				`<script type="module" src="`+tc.prefix+`/assets/main-abc123.js"></script>`,
			)
			// The tags point where the bundle is actually served.
			app.Do(httptest.NewRequest(http.MethodGet, tc.prefix+"/assets/main-abc123.js", nil)).AssertOK()
		})
	}
}

// TestCustomHeadLoadsClient: an app that loads its client through
// Config.Head renders without a manifest, as it did before the manifest
// became mandatory.
func TestCustomHeadLoadsClient(t *testing.T) {
	head := inertia.Head{Raw: `<script type="module" src="https://cdn.example.com/app.js"></script>`}
	app, _ := assetApp(t, "development", nil, inertia.Config{Head: head})
	body := fullLoad(app).AssertOK().String()
	mustContain(t, body, `src="https://cdn.example.com/app.js"`, `data-page="`)

	// A classic script isn't the client: still the missing-assets page.
	app, _ = assetApp(t, "development", nil, inertia.Config{Head: inertia.Head{Raw: `<script src="/analytics.js"></script>`}})
	mustContain(t, fullLoad(app).AssertStatus(http.StatusInternalServerError).String(), "No frontend assets to load")
}

// captureLog redirects the standard logger for the test and returns a reader
// of what was written.
func captureLog(t *testing.T) func() string {
	t.Helper()
	var mu sync.Mutex
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}))
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

// SSR replaces the mount's content; a loader <style> inside it that was due a
// nonce goes with it, and the render must not trip over the dropped tag.
func TestTemplateSSRNonceLoaderInMount(t *testing.T) {
	index := strings.Replace(builtIndex, `<div id="app"><div class="loader"></div></div>`,
		`<div id="app"><style>.loader{margin:auto}</style><div class="loader"></div></div>`, 1)
	if index == builtIndex {
		t.Fatal("fixture: builtIndex no longer has the loader markup")
	}
	ssr := &fakeSSR{res: inertia.SSRResult{Body: "<main>rendered</main>"}}
	app, _ := assetApp(t, "production", withBuild(index), inertia.Config{
		SSR:   ssr,
		Nonce: func(*httpx.Ctx) string { return "n0nce" },
	})
	body := fullLoad(app).AssertOK().String()
	mustContain(t, body, `<div id="app" data-server-rendered="true"`)
	mustContain(t, body, `<main>rendered</main></div>`)
	mustNotContain(t, body, `.loader{margin:auto}`)
}

// A separate Inertia bundle (Config.Frontend) is not ServeFrontend's: its
// pages get a synthesised document with its own tags rooted at "/", never the
// other bundle's index.html or mount path.
func TestConfigFrontendIgnoresServeFrontendDocument(t *testing.T) {
	spa := fstest.MapFS{"dist/index.html": {Data: []byte(`<!doctype html><html><head><script type="module" src="/admin/assets/spa-11111111.js"></script></head><body><div id="app"></div></body></html>`)}}
	pages := fstest.MapFS{"dist/.vite/manifest.json": {Data: []byte(manifestJSON)}}
	app, _ := bootAssets(t, nexus.Config{Environment: "production"}, spa,
		inertia.Config{Frontend: pages, Root: "dist"}, nexus.FrontendAt("/admin"))
	body := fullLoad(app).AssertOK().String()
	mustContain(t, body, `src="/assets/main-abc123.js"`)
	mustNotContain(t, body, `spa-11111111.js`)
	mustNotContain(t, body, `/admin/assets/main-abc123.js`)
}
