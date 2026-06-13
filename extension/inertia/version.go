package inertia

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"path"
	"strings"
)

// manifestEntry mirrors the shape of one record in a Vite build manifest
// (web/dist/.vite/manifest.json). Only the fields the shell needs are decoded.
type manifestEntry struct {
	File    string   `json:"file"`    // hashed output, e.g. "assets/main-ab12cd.js"
	CSS     []string `json:"css"`     // hashed stylesheets pulled in by the entry
	IsEntry bool     `json:"isEntry"` // true for the app entry module
}

// manifest is the resolved view of a Vite build manifest: the entry chunk plus
// a content hash used as the Inertia asset version. found is false when no
// manifest exists (a dev build, or a pure-Go app), in which case the engine
// falls back to dev tags / an empty version.
type manifest struct {
	entry   manifestEntry
	found   bool
	version string
}

// loadManifest reads and parses the Vite manifest under root in fsys. It tries
// the modern location (.vite/manifest.json) then the legacy top-level
// manifest.json. The asset version is a short hash of the raw manifest bytes —
// any change to any emitted asset changes the hash, which is exactly the
// cache-busting signal Inertia's version check wants.
func loadManifest(fsys fs.FS, root string) (manifest, error) {
	candidates := []string{
		path.Join(root, ".vite", "manifest.json"),
		path.Join(root, "manifest.json"),
	}
	var raw []byte
	var err error
	for _, p := range candidates {
		raw, err = fs.ReadFile(fsys, p)
		if err == nil {
			break
		}
	}
	if err != nil {
		return manifest{}, err
	}

	var records map[string]manifestEntry
	if err := json.Unmarshal(raw, &records); err != nil {
		return manifest{}, err
	}
	var entry manifestEntry
	for _, e := range records {
		if e.IsEntry {
			entry = e
			break
		}
	}
	sum := sha256.Sum256(raw)
	return manifest{
		entry:   entry,
		found:   true,
		version: hex.EncodeToString(sum[:])[:16],
	}, nil
}

// headTags renders the production <link>/<script> tags for the manifest entry.
// Asset paths are rooted at "/" — matching nexus.ServeFrontend, which serves
// the build's /assets/* under the app root. Returns "" when there is no entry.
func (m manifest) headTags() string {
	if !m.found || m.entry.File == "" {
		return ""
	}
	var b strings.Builder
	for _, css := range m.entry.CSS {
		b.WriteString(`<link rel="stylesheet" href="/`)
		b.WriteString(css)
		b.WriteString(`">`)
	}
	b.WriteString(`<script type="module" src="/`)
	b.WriteString(m.entry.File)
	b.WriteString(`"></script>`)
	return b.String()
}

// devHeadTags renders the dev-server tags used when no build manifest is
// present but a viteless/Vite dev server URL is known (NEXUS_VITE_DEV). The
// client script enables HMR; the entry is the conventional src/main.ts. Full
// dev-topology handling lands in a later release; this keeps `nexus dev`
// usable for Inertia apps in the meantime.
func devHeadTags(devURL string) string {
	// /__nexus/dev/script.js is the framework's live-reload shim (mounted by
	// ServeFrontend under NEXUS_DEV=1): it full-reloads the browser on any file
	// change under the project root, so editing a Go page handler or a .vue
	// restarts the page without a manual refresh. The Vite client handles
	// module loading + HMR.
	return `<script src="/__nexus/dev/script.js"></script>` +
		`<script type="module" src="` + devURL + `/@vite/client"></script>` +
		`<script type="module" src="` + devURL + `/src/main.ts"></script>`
}
