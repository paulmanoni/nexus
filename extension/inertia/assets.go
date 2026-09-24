package inertia

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/internal/vitehot"
)

// pageAssets is what a render needs from the frontend toolchain: where a
// full-page load's document comes from, the <head> tags that load the client
// app when the engine builds that document itself, and the Inertia asset
// version.
type pageAssets struct {
	head    string
	version string
	// document: render into App.FrontendDocument (the app's index.html) when
	// it has one, instead of synthesising a document around head. Set for a
	// live dev server and for a build with a manifest; head is then only the
	// fallback for when there is no index.html (see pageDocument).
	document bool
	hot      *vitehot.Hot // the dev server, when one supplies the assets
	// problem, when set, is why a full-page load would carry no usable asset
	// tags. In development the full load answers with an error page naming it;
	// in production it is logged once and the shell still renders.
	problem *assetProblem
}

// assetProblem describes a page that would load no scripts.
type assetProblem struct {
	title  string
	detail []string // one line per fact, rendered as a list
	fix    []string // the ways out, rendered as a list
	dev    bool     // show an error page instead of the shell
}

func (p *assetProblem) Error() string {
	return p.title + ": " + strings.Join(p.detail, "; ")
}

// assets decides, per request, where a page's scripts come from. This is the
// one place the precedence lives:
//
//  1. nexus-vite-plugin's hot file (App.ViteHot) — the dev server says where
//     it is and which entry it serves. Re-read on change, so a Vite restart on
//     a new port applies on the next page load. A full load renders into the
//     dev server's own index.html (App.FrontendDocument); when it serves none,
//     into a synthesised document carrying the hot file's tags.
//  2. A hot file that is present but unusable (malformed, unknown version,
//     invalid origin) is reported, not skipped: falling through to the
//     manifest would serve stale assets while the developer edits sources.
//  3. NEXUS_VITE_DEV — the fallback for dev servers that don't write a hot
//     file (the viteless engine). Always a synthesised document.
//  4. The build manifest. A full load renders into the built index.html;
//     a module-only build (nexus({ input }), no index.html) gets a
//     synthesised document with the manifest's tags under App.FrontendMount.
//  5. Nothing: a problem — an error page in development, a logged error in
//     production — unless Config.Head loads a module script itself.
//
// The version is empty whenever a dev server supplies the assets, and the
// manifest hash (or Config.Version) otherwise. None of this touches an
// X-Inertia visit beyond the version: it carries no document.
func (e *Engine) assets() pageAssets {
	var reader *vitehot.Reader
	if e.app != nil {
		reader = e.app.ViteHot()
	}
	hot, hotErr := reader.Current() // nil reader → (nil, nil)
	if hot != nil {
		return pageAssets{head: e.hotHeadTags(hot), document: true, hot: hot}
	}
	if hotErr != nil {
		// Current only reports errors when the reader is enabled, i.e. in dev.
		return pageAssets{problem: hotProblem(reader, hotErr)}
	}

	e.envOnce.Do(func() { e.envDev = strings.TrimRight(os.Getenv(devURLEnv), "/") })
	if e.envDev != "" {
		return pageAssets{head: devHeadTags(e.envDev, e.devEntry, e.react)}
	}

	man, manErr, sourced := e.manifest()
	a := pageAssets{version: man.version}
	if e.versionPin != AutoVersion {
		a.version = e.versionPin
	}
	if a.head = man.headTags(e.mount()); a.head != "" {
		a.document = true
		return a
	}
	if e.headLoadsClient {
		// Config.Head loads the client itself (the documented escape hatch):
		// no manifest is not "no assets".
		return a
	}
	a.problem = e.missingAssets(reader, man, manErr, sourced)
	return a
}

// hotProblem reports a hot file that is present but unusable.
func hotProblem(reader *vitehot.Reader, err error) *assetProblem {
	return &assetProblem{
		title:  "The Vite dev server's hot file can't be used",
		detail: []string{err.Error()},
		fix: []string{
			"Restart the Vite dev server (npm run dev, or nexus dev) so nexus-vite-plugin rewrites the file.",
			"Or delete " + reader.Path() + " to use the build manifest instead.",
		},
		dev: true,
	}
}

// mount is the URL path the bundle is served under (App.FrontendMount).
func (e *Engine) mount() string {
	if e.app == nil {
		return ""
	}
	return e.app.FrontendMount()
}

// pageDocument returns the app's index.html located for this render, or nil
// when the engine should synthesise the document around a.head: the render
// has no document to use (a.document false), or App.FrontendDocument has
// none (ErrNoFrontendDocument — no index.html, or a dev server that did not
// serve one). A problem is returned when there is a document but it cannot
// be used: a hot file that went bad since assets() read it, or a document
// with no mount element — a developer error, shown in development and
// logged once in production, where the synthesised document is the fallback.
func (e *Engine) pageDocument(ctx context.Context, a pageAssets) (*pageTemplate, *assetProblem) {
	if !a.document || e.app == nil {
		return nil, nil
	}
	doc, err := e.app.FrontendDocument(ctx)
	if errors.Is(err, nexus.ErrNoFrontendDocument) {
		return nil, nil
	}
	if err != nil {
		return nil, hotProblem(e.app.ViteHot(), err)
	}
	t, ok := parseTemplate(doc.HTML, e.rootView)
	if ok {
		t.fromDev = doc.FromDevServer
		return &t, nil
	}
	return nil, e.noMount(doc.FromDevServer, a.hot)
}

// noMount describes a document with no element to put the page on.
func (e *Engine) noMount(fromDev bool, hot *vitehot.Hot) *assetProblem {
	source := "the built index.html"
	if fromDev && hot != nil {
		source = "the Vite dev server's index.html (" + hot.URL("index.html") + ")"
	} else if _, root, ok := e.app.FrontendFS(); ok {
		source = "the built index.html (" + path.Join(root, "index.html") + " in the bundle nexus.ServeFrontend serves)"
	}
	return &assetProblem{
		title: "The page document has no mount element",
		detail: []string{
			`Expected an element with id="` + e.rootView + `" (inertia.Config.RootView) to put the page on.`,
			"The document is " + source + ".",
		},
		fix: []string{
			`Add <div id="` + e.rootView + `"></div> to the <body> of index.html; the engine puts the page on it.`,
			"Or set inertia.Config.RootView to the id of the element your index.html mounts the app on.",
		},
		dev: e.devMode(),
	}
}

// logNoMount reports, once per engine, a production document with no mount
// element; pages then render into the synthesised document instead.
func (e *Engine) logNoMount(p *assetProblem) {
	e.noMountOnce.Do(func() {
		if e.logf != nil {
			e.logf("inertia: %s; rendering pages into a synthesised document instead, without index.html's head. %s",
				p.Error(), strings.Join(p.fix, " "))
		}
	})
}

// hotHeadTags renders the dev tags for a hot file. The entry is the first
// declared entry that is a module (an .html input is Vite's SPA entry, not
// something a <script> can load); with none, Config.Entry / the default.
func (e *Engine) hotHeadTags(h *vitehot.Hot) string {
	entry := h.ModuleEntry()
	if entry == "" {
		entry = e.devEntry
	}
	react := e.reactForced || isJSXEntry(entry)
	return devTags(h.ClientURL(), h.URL(entry), h.URL("@react-refresh"), react, nexus.IsDev())
}

func isJSXEntry(entry string) bool {
	return strings.HasSuffix(entry, ".tsx") || strings.HasSuffix(entry, ".jsx")
}

// frontendSource is the bundle the manifest is read from: Config.Frontend, or
// the one ServeFrontend registered.
func (e *Engine) frontendSource() (fs.FS, string, bool) {
	if e.cfgFrontend != nil {
		return e.cfgFrontend, e.cfgRoot, true
	}
	if e.app != nil {
		if f, r, ok := e.app.FrontendFS(); ok {
			return f, r, true
		}
	}
	return nil, "", false
}

// manifest returns the build manifest, loading it on first use. A found
// manifest is cached for the process. A missing one is cached too in
// production, but retried on each render in development, where a build can
// land on disk while the app runs.
func (e *Engine) manifest() (man manifest, err error, sourced bool) {
	e.manMu.Lock()
	defer e.manMu.Unlock()
	fsys, root, sourced := e.frontendSource()
	if e.man.found || (e.manTried && !e.devMode()) {
		return e.man, e.manErr, sourced
	}
	e.manTried = true
	if !sourced {
		e.manErr = nil
		return e.man, nil, false
	}
	m, err := loadManifest(fsys, root)
	e.man, e.manErr = m, err
	return m, err, true
}

// devMode is the contract's rule for development: under nexus dev, or when
// the app declares environment = "development".
func (e *Engine) devMode() bool {
	env := ""
	if e.app != nil {
		env = e.app.Environment()
	}
	return vitehot.Enabled(nexus.IsDev(), env)
}

// missingAssets builds the report for a render with neither a dev server nor
// a usable manifest.
func (e *Engine) missingAssets(reader *vitehot.Reader, man manifest, manErr error, sourced bool) *assetProblem {
	p := &assetProblem{
		title: "No frontend assets to load",
		dev:   e.devMode(),
	}
	switch {
	case !p.dev:
		p.detail = append(p.detail, "No Vite dev server in use (hot files are followed only under nexus dev or environment = \"development\").")
	case reader != nil:
		if _, err := os.Stat(reader.Path()); err == nil {
			// Present but ignored: the dev server it names isn't running.
			// "not found" would send the developer looking for a file
			// that is right there.
			p.detail = append(p.detail, "No Vite dev server: hot file "+reader.Path()+" names a dev server that is not running (it was left by a dev server that exited). Start it again, or delete the file.")
		} else {
			p.detail = append(p.detail, "No Vite dev server: hot file "+reader.Path()+" not found.")
		}
	default:
		p.detail = append(p.detail, "No Vite dev server: no hot file is watched, because nexus.ServeFrontend is not registered.")
	}
	_, root, _ := e.frontendSource()
	tried := strings.Join(manifestCandidates(root), ", ")
	switch {
	case !sourced:
		p.detail = append(p.detail, "No build manifest: no frontend bundle is registered (nexus.ServeFrontend or inertia.Config.Frontend).")
	case man.found:
		p.detail = append(p.detail, "Build manifest "+man.path+" has no entry chunk (no record with \"isEntry\": true and a file).")
	case manErr != nil && !errors.Is(manErr, fs.ErrNotExist):
		p.detail = append(p.detail, "Build manifest unreadable: "+manErr.Error()+".")
	default:
		p.detail = append(p.detail, "No build manifest: tried "+tried+" in the frontend bundle.")
	}
	p.fix = []string{
		"Start the Vite dev server with nexus-vite-plugin in vite.config (npm run dev, or nexus dev); it writes the hot file.",
		"Or build the frontend (nexus build, or vite build) so the manifest exists. build.manifest must be true; nexus-vite-plugin sets it for you.",
	}
	return p
}

// logMissing reports a production render with no asset tags, once per engine
// (one engine per app): the server keeps answering, and the log says why the
// pages are blank without repeating on every request.
func (e *Engine) logMissing(p *assetProblem) {
	e.missingOnce.Do(func() {
		logf := e.logf
		if logf == nil {
			return
		}
		logf("inertia: pages will render blank: full-page loads carry no asset tags. %s "+
			"Build the frontend with nexus-vite-plugin (it sets build.manifest: true) so the manifest is in the embedded bundle.",
			strings.Join(p.detail, " "))
	})
}

// errorPage renders the development error page for a full-page load that
// would otherwise come up blank. Self-contained: no external assets.
func errorPage(p *assetProblem, nonce string) []byte {
	var b strings.Builder
	b.WriteString("<!doctype html>\n<html>\n<head>\n<meta charset=\"utf-8\">\n")
	b.WriteString("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n")
	b.WriteString("<title>Inertia: ")
	b.WriteString(html.EscapeString(p.title))
	b.WriteString("</title>\n<style")
	if nonce != "" {
		fmt.Fprintf(&b, ` nonce="%s"`, html.EscapeString(nonce))
	}
	b.WriteString(`>
body{font:15px/1.5 system-ui,sans-serif;max-width:46rem;margin:3rem auto;padding:0 1rem;color:#1f2328;background:#fff}
h1{font-size:1.25rem;color:#b42318}
li{overflow-wrap:anywhere}
@media (prefers-color-scheme:dark){body{color:#e6edf3;background:#0d1117}h1{color:#ff7b72}}
</style>
</head>
<body>
<h1>Inertia: `)
	b.WriteString(html.EscapeString(p.title))
	b.WriteString("</h1>\n<ul>\n")
	for _, d := range p.detail {
		b.WriteString("<li>")
		b.WriteString(html.EscapeString(d))
		b.WriteString("</li>\n")
	}
	b.WriteString("</ul>\n<p>Either:</p>\n<ol>\n")
	for _, f := range p.fix {
		b.WriteString("<li>")
		b.WriteString(html.EscapeString(f))
		b.WriteString("</li>\n")
	}
	b.WriteString("</ol>\n<p>This page is shown in development only (nexus dev, or environment = &quot;development&quot;).</p>\n</body>\n</html>\n")
	return []byte(b.String())
}
