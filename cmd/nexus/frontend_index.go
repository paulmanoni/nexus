package main

// Vite-replacement helper: when the project carries an
// `index.html` next to the entry sources (or one level up — the
// canonical Vite layout puts it at the project's frontend root
// with entries under src/), rewrite the source's module +
// stylesheet references to match the bundled output names + drop
// the result into the bundler's output directory.
//
// Without this, a `nexus dev` against a Vite-style project
// bundles main.js / main.css fine but never produces
// islands/index.html, so the framework's ServeFrontend falls
// back to the "no frontend yet" placeholder even though the SPA
// is otherwise ready to load.

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

// indexHTMLSearchPaths is the ordered list of relative locations
// emitIndexHTML probes for a source `index.html`. The first
// match wins. Order matches the most-to-least common Vite /
// Nuxt / Webpack layouts:
//
//   - "index.html"                 → entries + index.html at the same level
//   - "../index.html"              → entries under src/, index.html one up
//   - "../public/index.html"       → CRA shape
var indexHTMLSearchPaths = []string{
	"index.html",
	filepath.Join("..", "index.html"),
	filepath.Join("..", "public", "index.html"),
}

// emitIndexHTML scans for a source index.html under srcDir (or
// the conventional sibling locations above), rewrites its module
// + stylesheet refs to point at the bundler's outputs, and
// writes the result to outDir/index.html. Returns nil if no
// source HTML was found — that's a normal "this project doesn't
// use index.html" case, not an error.
//
// Output filename mapping: entries that produced output files
// are matched by basename-stem, so a source <script src=
// "/src/main.ts"> resolves to "main.js" + (if produced)
// "main.css". The CSS sidecar gets a fresh <link> tag right
// before the script tag — same convention vite uses.
//
// When the caller is the dev watcher (devMode=true), the dev-
// reload shim is also injected before </body> so the browser
// auto-refreshes on every rebuild. Production `nexus build`
// calls with devMode=false so the released bundle stays clean.
//
// outputFiles is esbuild's api.OutputFile slice from the most
// recent build; only their Path field matters for this pass.
func emitIndexHTML(srcDir, outDir string, outputFiles []api.OutputFile, stdout io.Writer, devMode bool) error {
	srcPath, sourceBytes, err := findIndexHTML(srcDir)
	if err != nil {
		return err
	}
	if srcPath == "" {
		// No index.html anywhere — operator either supplies their
		// own via //go:embed or relies on the dev placeholder.
		return nil
	}
	out := rewriteIndexHTML(sourceBytes, outputFiles)
	if devMode {
		out = injectDevReloadScript(out)
	}
	dest := filepath.Join(outDir, "index.html")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(dest, out, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", dest, err)
	}
	// Loud first-time mention so the operator sees the rewrite
	// happen + can confirm the file landed where they expected.
	if stdout != nil && stdout != io.Discard {
		rel, _ := filepath.Rel(filepath.Dir(srcDir), srcPath)
		if rel == "" {
			rel = srcPath
		}
		fmt.Fprintf(stdout, "[web]   ↪ index.html  from %s\n", rel)
	}
	return nil
}

// findIndexHTML probes the conventional locations relative to
// srcDir. Returns the matched path + its bytes, or ("", nil, nil)
// when none of the candidates exist.
func findIndexHTML(srcDir string) (string, []byte, error) {
	for _, rel := range indexHTMLSearchPaths {
		p := filepath.Clean(filepath.Join(srcDir, rel))
		body, err := os.ReadFile(p)
		if err == nil {
			return p, body, nil
		}
		if !os.IsNotExist(err) {
			return "", nil, err
		}
	}
	return "", nil, nil
}

// indexScriptRE matches <script type="module" src="..."> tags.
// Group 1 = the src value (with quotes stripped by the
// non-capturing group's structure). Multiline + case-insensitive
// since the wild diversity of hand-edited index.html files
// includes both Type="module" and TYPE='MODULE'.
//
// We intentionally don't try to handle non-module scripts,
// inline scripts, or scripts in <head> — those are rare in the
// Vite layout this rewrite serves.
var indexScriptRE = regexp.MustCompile(`(?i)<script[^>]*\stype=["']module["'][^>]*\ssrc=["']([^"']+)["'][^>]*></script>`)

// indexLinkRE matches <link rel="stylesheet" href="..."> tags
// that point at a source file (we only need to remap user-
// authored stylesheets; preconnect / favicon / external CDN
// links pass through unchanged because their href doesn't
// match any output file). Group 1 = the href value.
var indexLinkRE = regexp.MustCompile(`(?i)<link[^>]*\srel=["']stylesheet["'][^>]*\shref=["']([^"']+)["'][^>]*/?>`)

// rewriteIndexHTML returns the source HTML with module + local
// stylesheet refs remapped to their bundled equivalents.
//
//   - <script type="module" src="/src/main.ts"> → src="/main.js"
//   - <link rel="stylesheet" href="/src/styles.css"> → href="/main.css"
//   - External refs (https://, //cdn.foo, etc.) pass through
//     unchanged so Google Fonts / favicon / preconnect tags stay
//     intact.
//
// Outputs whose stem doesn't match any source ref get appended
// to <head> as new tags — covers the common case where esbuild
// emits a sidecar `main.css` for an entry whose source HTML
// only referenced the .ts file.
func rewriteIndexHTML(source []byte, outputFiles []api.OutputFile) []byte {
	outNames := collectOutputBasenames(outputFiles)
	out := indexScriptRE.ReplaceAllFunc(source, func(match []byte) []byte {
		m := indexScriptRE.FindSubmatch(match)
		if len(m) < 2 {
			return match
		}
		src := string(m[1])
		if isExternalRef(src) {
			return match
		}
		bundled := mapToBundledOutput(src, outNames, ".js")
		if bundled == "" {
			return match
		}
		return []byte(`<script type="module" src="/` + bundled + `"></script>`)
	})
	out = indexLinkRE.ReplaceAllFunc(out, func(match []byte) []byte {
		m := indexLinkRE.FindSubmatch(match)
		if len(m) < 2 {
			return match
		}
		href := string(m[1])
		if isExternalRef(href) {
			return match
		}
		bundled := mapToBundledOutput(href, outNames, ".css")
		if bundled == "" {
			return match
		}
		return []byte(`<link rel="stylesheet" href="/` + bundled + `">`)
	})
	// Inject sidecar CSS — for every bundled .css whose stem
	// matched a script reference we just rewrote, but no <link>
	// for it already exists, add one right before </head>.
	out = injectMissingCSSLinks(out, outputFiles, outNames)
	return out
}

// collectOutputBasenames returns a map of stem → filename for
// every output file. Entries are keyed by the stem (filename
// without extension) so a source ref like "main.ts" resolves to
// "main.js" or "main.css" by looking up "main" + the desired
// extension.
func collectOutputBasenames(files []api.OutputFile) map[string][]string {
	out := map[string][]string{}
	for _, f := range files {
		name := filepath.Base(f.Path)
		stem := strings.TrimSuffix(name, filepath.Ext(name))
		out[stem] = append(out[stem], name)
	}
	return out
}

// mapToBundledOutput resolves a source href / src to a bundled
// output filename. Strips the leading slash + directory parts
// (the bundler outputs are flat in outDir), uses the basename
// stem to look up matching outputs, and picks the one with
// `wantExt`. Returns "" when no output matches — caller leaves
// the ref untouched.
func mapToBundledOutput(srcRef string, outNames map[string][]string, wantExt string) string {
	clean := strings.TrimPrefix(srcRef, "/")
	clean = strings.TrimPrefix(clean, "./")
	stem := filepath.Base(clean)
	stem = strings.TrimSuffix(stem, filepath.Ext(stem))
	candidates, ok := outNames[stem]
	if !ok {
		return ""
	}
	for _, c := range candidates {
		if strings.EqualFold(filepath.Ext(c), wantExt) {
			return c
		}
	}
	return ""
}

// isExternalRef reports whether ref points at something the
// bundler didn't produce: protocol-relative URLs (//cdn.foo),
// absolute HTTP(S), data URIs, and the local favicon-style
// public-folder references. These pass through the rewrite
// unchanged.
func isExternalRef(ref string) bool {
	if ref == "" {
		return false
	}
	if strings.HasPrefix(ref, "//") {
		return true
	}
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return true
	}
	if strings.HasPrefix(ref, "data:") {
		return true
	}
	return false
}

// injectMissingCSSLinks adds <link rel="stylesheet" href="/X.css">
// tags right before </head> for any bundled .css file whose stem
// matches a successfully-rewritten script ref but didn't already
// have its own <link> in the source. Mirrors what vite does for
// CSS-sidecar bundles.
//
// We use a simple `</head>` lookup rather than full HTML parsing.
// Operator-authored shells are tiny + structured; the worst case
// (`</head>` in a comment) just means the new <link> lands
// somewhere unconventional — never breaks the page.
func injectMissingCSSLinks(html []byte, outputs []api.OutputFile, outNames map[string][]string) []byte {
	existing := indexLinkRE.FindAllSubmatch(html, -1)
	hasLinkFor := map[string]bool{}
	for _, m := range existing {
		if len(m) < 2 {
			continue
		}
		href := string(m[1])
		stem := strings.TrimSuffix(path.Base(strings.TrimPrefix(href, "/")), path.Ext(href))
		hasLinkFor[stem] = true
	}
	var add []string
	seen := map[string]bool{}
	for _, f := range outputs {
		name := filepath.Base(f.Path)
		if !strings.EqualFold(filepath.Ext(name), ".css") {
			continue
		}
		if strings.HasSuffix(name, ".map") {
			continue
		}
		stem := strings.TrimSuffix(name, filepath.Ext(name))
		if hasLinkFor[stem] || seen[name] {
			continue
		}
		// Only inject when the same stem has a JS sibling — that
		// means esbuild produced the CSS as a side-effect of the
		// entry, not a deliberate top-level .css entry the
		// operator already authored separately.
		jsSibling := false
		for _, sibling := range outNames[stem] {
			if strings.EqualFold(filepath.Ext(sibling), ".js") {
				jsSibling = true
				break
			}
		}
		if !jsSibling {
			continue
		}
		add = append(add, `<link rel="stylesheet" href="/`+name+`">`)
		seen[name] = true
	}
	if len(add) == 0 {
		return html
	}
	injection := strings.Join(add, "\n  ") + "\n  </head>"
	// Case-insensitive replace of the first </head>.
	idx := closingHeadIndex(html)
	if idx < 0 {
		// No <head> — append to end as a fallback so the styles
		// still load (browsers tolerate <link> in <body>).
		return append(html, []byte("\n"+strings.Join(add, "\n"))...)
	}
	out := make([]byte, 0, len(html)+len(injection))
	out = append(out, html[:idx]...)
	out = append(out, []byte(injection)...)
	out = append(out, html[idx+len("</head>"):]...)
	return out
}

// closingHeadIndex returns the byte offset of `</head>` (case-
// insensitive) in html, or -1 when absent.
func closingHeadIndex(html []byte) int {
	lower := strings.ToLower(string(html))
	return strings.Index(lower, "</head>")
}

// devReloadScriptTag is the marker the framework's mountDevReload
// scripts off of. Duplicated here so cmd/nexus doesn't have a
// build-time dep on the root pkg's internals.
//
// MUST stay in sync with ext_devreload.go's same-named const.
const devReloadScriptTag = `<script src="/__nexus/dev/script.js"></script>`

// injectDevReloadScript adds the dev-reload shim before </body>
// so the served HTML opens an SSE connection back to the running
// app + auto-reloads on every rebuild. Idempotent — re-injection
// across rebuilds doesn't stack tags because the rewrite operates
// on the SOURCE bytes (always fresh from disk), not on a
// previously-emitted output.
//
// Falls back to appending when </body> is missing — broken HTML
// shouldn't lose the reload wiring entirely.
func injectDevReloadScript(html []byte) []byte {
	tag := []byte("  " + devReloadScriptTag + "\n  </body>")
	idx := closingBodyIndex(html)
	if idx < 0 {
		return append(html, []byte("\n"+devReloadScriptTag+"\n")...)
	}
	out := make([]byte, 0, len(html)+len(tag))
	out = append(out, html[:idx]...)
	out = append(out, tag...)
	out = append(out, html[idx+len("</body>"):]...)
	return out
}

// closingBodyIndex mirrors closingHeadIndex for the </body>
// landing zone of injectDevReloadScript.
func closingBodyIndex(html []byte) int {
	lower := strings.ToLower(string(html))
	return strings.Index(lower, "</body>")
}
