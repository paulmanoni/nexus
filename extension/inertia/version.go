package inertia

import (
	"io/fs"
	"strings"

	"github.com/paulmanoni/nexus/internal/vitemanifest"
)

// manifestEntry is one record of a Vite build manifest. The parser is shared
// with nexus.ServeFrontend (internal/vitemanifest), which reads the same file
// for cache policy.
type manifestEntry = vitemanifest.Chunk

// manifest is the resolved view of a Vite build manifest: the full chunk graph
// (keyed by manifest key), the entry chunk's key, and a content hash used as
// the Inertia asset version. found is false when no manifest exists (a dev
// build, or a pure-Go app), in which case the engine falls back to dev tags /
// an empty version.
type manifest struct {
	records  map[string]manifestEntry
	entryKey string
	found    bool
	version  string
	path     string // the candidate that was read
}

// manifestCandidates are the paths loadManifest tries, in order.
func manifestCandidates(root string) []string { return vitemanifest.Candidates(root) }

// loadManifest reads the Vite manifest under root. A missing manifest is an
// error wrapping fs.ErrNotExist; the asset version is a short hash of the raw
// bytes, so any change to any emitted asset changes it — exactly the signal
// Inertia's version check wants.
func loadManifest(fsys fs.FS, root string) (manifest, error) {
	m, err := vitemanifest.Load(fsys, root)
	if err != nil {
		return manifest{}, err
	}
	return manifest{
		records:  m.Chunks,
		entryKey: m.EntryKey(),
		found:    true,
		version:  m.Version,
		path:     m.Path,
	}, nil
}

// headTags renders the production <link>/<script> tags for the entry chunk and
// its statically-imported chunks, the way Vite's own backend integration does:
//
//   - <link rel="stylesheet"> for the entry's CSS AND every imported chunk's CSS
//     (walked recursively, deduped) — so a code-split build doesn't drop the
//     styles that live on shared/imported chunks;
//   - <link rel="modulepreload"> for each statically-imported chunk (recursive,
//     deduped, excluding the entry itself) so the browser fetches dependencies
//     in parallel instead of discovering them only after parsing the entry;
//   - the entry's <script type="module">.
//
// Dynamic imports are intentionally NOT preloaded (they load on demand). Asset
// paths are rooted at "/" — matching nexus.ServeFrontend, which serves the
// build's /assets/* under the app root. Returns "" when there is no entry.
func (m manifest) headTags() string {
	entry, ok := m.records[m.entryKey]
	if !m.found || !ok || entry.File == "" {
		return ""
	}
	var b strings.Builder

	// 1. Stylesheets: entry first, then imported chunks (recursive, deduped).
	cssDone := map[string]bool{}
	var walkCSS func(key string, seen map[string]bool)
	walkCSS = func(key string, seen map[string]bool) {
		if seen[key] {
			return
		}
		seen[key] = true
		c, ok := m.records[key]
		if !ok {
			return
		}
		for _, css := range c.CSS {
			if cssDone[css] {
				continue
			}
			cssDone[css] = true
			b.WriteString(`<link rel="stylesheet" href="/`)
			b.WriteString(css)
			b.WriteString(`">`)
		}
		for _, imp := range c.Imports {
			walkCSS(imp, seen)
		}
	}
	walkCSS(m.entryKey, map[string]bool{})

	// 2. modulepreload: the entry's static-import graph (not the entry itself).
	preDone := map[string]bool{}
	var walkPreload func(key string)
	walkPreload = func(key string) {
		if preDone[key] {
			return
		}
		preDone[key] = true
		c, ok := m.records[key]
		if !ok {
			return
		}
		if c.File != "" {
			b.WriteString(`<link rel="modulepreload" href="/`)
			b.WriteString(c.File)
			b.WriteString(`">`)
		}
		for _, imp := range c.Imports {
			walkPreload(imp)
		}
	}
	for _, imp := range entry.Imports {
		walkPreload(imp)
	}

	// 3. The entry module script.
	b.WriteString(`<script type="module" src="/`)
	b.WriteString(entry.File)
	b.WriteString(`"></script>`)
	return b.String()
}

// devHeadTags renders the dev-server tags for the NEXUS_VITE_DEV fallback: a
// dev server URL with no hot file describing it. entry is the client app's dev
// module (e.g. "src/main.ts" / "src/main.tsx"); react adds the Vite React Fast
// Refresh preamble that must run before the dev client.
func devHeadTags(devURL, entry string, react bool) string {
	return devTags(devURL+"/@vite/client", devURL+"/"+strings.TrimPrefix(entry, "/"), devURL+"/@react-refresh", react, true)
}

// devTags renders the dev-server <head> tags from resolved URLs: the Vite
// client, the entry module, and (for React) the Fast Refresh runtime.
func devTags(clientURL, entryURL, refreshURL string, react, reloadShim bool) string {
	var b strings.Builder
	// /__nexus/dev/script.js is the framework's live-reload shim (mounted by
	// ServeFrontend under NEXUS_DEV=1): it full-reloads the browser on any file
	// change under the project root, so editing a Go page handler or a .vue/.tsx
	// restarts the page without a manual refresh. The Vite client handles
	// module loading + HMR.
	if reloadShim {
		b.WriteString(`<script src="/__nexus/dev/script.js"></script>`)
	}
	// React Fast Refresh must be installed before @vite/client and the app entry.
	if react {
		b.WriteString(reactRefreshPreamble(refreshURL))
	}
	b.WriteString(`<script type="module" src="` + clientURL + `"></script>`)
	b.WriteString(`<script type="module" src="` + entryURL + `"></script>`)
	return b.String()
}

// reactRefreshPreamble is the Vite React plugin's HMR bootstrap, pointed at the
// dev server's /@react-refresh runtime. Without it a React app's edits do a full
// reload instead of fast-refreshing component state.
func reactRefreshPreamble(refreshURL string) string {
	return `<script type="module">
  import RefreshRuntime from '` + refreshURL + `'
  RefreshRuntime.injectIntoGlobalHook(window)
  window.$RefreshReg$ = () => {}
  window.$RefreshSig$ = () => (type) => type
  window.__vite_plugin_react_preamble_installed__ = true
</script>`
}
