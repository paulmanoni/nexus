package main

import (
	"io/fs"
	"os"
	"path/filepath"
)

// devStubIndexHTML is what the stubbed embed serves as index.html. It only has
// to exist and parse: ServeFrontend reads index.html at boot and fails fast
// without it, and under NEXUS_DEV it re-reads the real file from disk per
// request anyway, so nothing ever renders these bytes.
const devStubIndexHTML = "<!doctype html><title>nexus dev</title>\n"

// distStubReplacements maps every file under distRoot to a stub file in tmp,
// for `go build -overlay`. Go's overlay covers files read by //go:embed (not
// just Go sources), so this shrinks the embedded bundle out of the dev binary
// without touching a line of the user's code — no build tags, no scaffold
// change, existing apps included.
//
// Why it's safe: under NEXUS_DEV=1 ServeFrontend swaps the embed.FS for
// os.DirFS and serves the real files from the working tree, and the SPA is
// served by viteless on :5173 regardless. The embedded copy is dead weight
// during dev — it just gets relinked on every save. On a 9.5MB/198-file
// bundle that was ~0.5s of every rebuild.
//
// Scope is deliberately narrow: ONLY the tree a ServeFrontend call names. Apps
// embed assets they genuinely read at runtime (fonts for PDF rendering, seed
// data, templates); stubbing those would break the app in dev in ways that
// look like bugs in the user's code.
//
// Returns nil when distRoot is empty, missing, or contains no files — every
// caller treats that as "contribute nothing".
func distStubReplacements(distRoot, tmp string) (map[string]string, error) {
	if distRoot == "" {
		return nil, nil
	}
	abs, err := filepath.Abs(distRoot)
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return nil, nil //nolint:nilerr // absent bundle: nothing to stub
	}

	// Two stub files serve every replacement — the overlay maps many source
	// paths onto a handful of shadows, so this stays O(1) in writes.
	emptyStub := filepath.Join(tmp, "dist_empty")
	if err := os.WriteFile(emptyStub, nil, 0o644); err != nil {
		return nil, err
	}
	indexStub := filepath.Join(tmp, "dist_index.html")
	if err := os.WriteFile(indexStub, []byte(devStubIndexHTML), 0o644); err != nil {
		return nil, err
	}

	replace := map[string]string{}
	err = filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable corner of the tree: leave it embedded
		}
		if d.IsDir() {
			return nil
		}
		// Only the bundle's own index.html needs to survive as real HTML —
		// it's the one file boot insists on. Nested index.html files (a
		// prerendered route) are ordinary assets.
		if path == filepath.Join(abs, "index.html") {
			replace[path] = indexStub
			return nil
		}
		// Leave the Vite manifest alone. It's a couple of KB, and it's the
		// one file in the bundle something reads through the EMBED rather
		// than off disk in dev: extension/inertia falls back to
		// App.FrontendFS() to resolve entry chunks when NEXUS_VITE_DEV
		// isn't set. Stubbing it would strip an Inertia app's asset tags.
		if d.Name() == "manifest.json" {
			return nil
		}
		replace[path] = emptyStub
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(replace) == 0 {
		return nil, nil
	}
	return replace, nil
}
