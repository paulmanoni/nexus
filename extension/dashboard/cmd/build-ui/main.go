//go:build cgo && vue

// Command build-ui produces extension/dashboard/ui/dist by
// running the dashboard's Vue 3 SPA through the nexus deps
// pipeline — no Node, no vite, no node_modules. Output drops
// into the same dist/ the framework's go:embed picks up.
//
// Pipeline:
//
//  1. Read extension/dashboard/ui/package.json for the
//     dependency set (vue, @vue-flow/core + siblings, dagre,
//     lucide-vue-next, fontsource fonts).
//  2. For each dep: f.Fetch(spec) — populates ~/.nexus/cache
//     + an in-memory lockfile. Equivalent to `nexus add` but
//     without writing nexus.lock to disk (the framework's own
//     dashboard isn't a user project — its dep graph travels
//     with this script, not a checked-in lockfile).
//  3. Bootstrap QuickJS Vue compiler.
//  4. esbuild bundle: entry=ui/src/main.js, outdir=ui/dist/assets
//     + splitting + content-hashed filenames + linked sourcemap.
//  5. Generate ui/dist/index.html that references the hashed
//     output assets.
//
// Run with:
//
//	CGO_ENABLED=1 go run -tags='cgo vue' ./extension/dashboard/cmd/build-ui
//
// This script is the framework-internal equivalent of
// `nexus build` for SPAs. It deliberately stays a script (not a
// CLI subcommand) because the dashboard is framework
// distribution code, not user code — only the framework's CI
// or local dev rebuilds it.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/paulmanoni/nexus/frontend/deps/bundler"
	"github.com/paulmanoni/nexus/frontend/deps/fetcher"
	"github.com/paulmanoni/nexus/frontend/deps/lockfile"
	"github.com/paulmanoni/nexus/frontend/deps/resolver"
	"github.com/paulmanoni/nexus/frontend/deps/sfc/vue"
	"github.com/paulmanoni/nexus/frontend/deps/store"
)

// frameworkDashboardDir is the path to the dashboard's UI
// source, RELATIVE to this script's cwd-at-invocation. We assume
// the script is run from the repo root: that's the convention
// `go run ./extension/dashboard/cmd/build-ui` produces.
const frameworkDashboardDir = "extension/dashboard/ui"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "build-ui:", err)
		os.Exit(1)
	}
}

func run() error {
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	uiDir := filepath.Join(root, frameworkDashboardDir)
	if _, err := os.Stat(filepath.Join(uiDir, "src", "main.js")); err != nil {
		return fmt.Errorf("run from repo root — couldn't find %s/src/main.js: %w", frameworkDashboardDir, err)
	}

	pkgJSON, err := readPackageJSON(filepath.Join(uiDir, "package.json"))
	if err != nil {
		return err
	}
	specs := make([]string, 0, len(pkgJSON.Dependencies))
	for name, ver := range pkgJSON.Dependencies {
		// Strip ^/~/>=/etc. prefixes so we pin to the bare
		// version. esm.sh's redirect-resolve handles semver
		// ranges in the URL, but for deterministic builds we
		// want a concrete version in the spec.
		clean := strings.TrimLeft(ver, "^~>=< ")
		specs = append(specs, name+"@"+clean)
	}
	sort.Strings(specs)
	fmt.Printf("[1/5] dependencies to fetch (%d):\n", len(specs))
	for _, s := range specs {
		fmt.Println("     ", s)
	}

	// Shared store at ~/.nexus/cache (or NEXUS_CACHE override).
	cacheRoot := os.Getenv("NEXUS_CACHE")
	if cacheRoot == "" {
		cacheRoot = store.DefaultRoot()
	}
	st, err := store.New(cacheRoot)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	f := fetcher.New(st, "")

	fmt.Println("[2/5] fetching deps (skipping cached blobs)")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	lf := lockfile.New()
	for _, spec := range specs {
		res, err := f.Fetch(ctx, spec)
		if err != nil {
			return fmt.Errorf("fetch %s: %w", spec, err)
		}
		lf.Add(res.Root)
		for _, p := range res.Transitive {
			lf.Add(p)
		}
		fmt.Printf("      ✓ %s (+%d transitive)\n", spec, len(res.Transitive))
	}

	// Sub-path imports (e.g. "@vue-flow/core/dist/style.css")
	// declared by user code aren't reachable from the package
	// roots' own import graph, so the recursive fetcher above
	// didn't pull them. Pre-scan the entry file for explicit
	// "<pkg>/<subpath>" import statements and fetch each one
	// directly. This is the script-side equivalent of a user
	// running multiple `nexus add <pkg>/<subpath>` calls.
	subPaths, err := scanSubPathImports(filepath.Join(uiDir, "src", "main.js"), pkgJSON.Dependencies)
	if err != nil {
		return fmt.Errorf("scan sub-paths: %w", err)
	}
	if len(subPaths) > 0 {
		fmt.Printf("      sub-path imports found in main.js (%d):\n", len(subPaths))
		for _, sp := range subPaths {
			res, err := f.Fetch(ctx, sp)
			if err != nil {
				return fmt.Errorf("fetch sub-path %s: %w", sp, err)
			}
			lf.Add(res.Root)
			for _, p := range res.Transitive {
				lf.Add(p)
			}
			fmt.Printf("      ✓ %s (+%d transitive)\n", sp, len(res.Transitive))
		}
	}

	fmt.Println("[3/5] bootstrapping Vue compiler (QuickJS)")
	bundleBytes, err := vue.Bootstrap(ctx, vue.BootstrapOptions{
		Store: st, Fetcher: f,
	})
	if err != nil {
		return fmt.Errorf("vue bootstrap: %w", err)
	}
	compiler, err := vue.NewCompiler(bundleBytes, "@vue/compiler-sfc@"+vue.DefaultCompilerVersion)
	if err != nil {
		return fmt.Errorf("vue compiler: %w", err)
	}
	defer compiler.Close()

	fmt.Println("[4/5] bundling")
	distDir := filepath.Join(uiDir, "dist")
	if err := os.RemoveAll(distDir); err != nil {
		return fmt.Errorf("clean dist: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(distDir, "assets"), 0o755); err != nil {
		return fmt.Errorf("mkdir dist/assets: %w", err)
	}

	rPlugin, err := resolver.New(resolver.Options{Lockfile: lf, Store: st})
	if err != nil {
		return fmt.Errorf("resolver: %w", err)
	}
	vPlugin, err := vue.Plugin(compiler)
	if err != nil {
		return fmt.Errorf("vue plugin: %w", err)
	}
	b := bundler.New()
	b.AddPlugin(rPlugin)
	b.AddPlugin(vPlugin)

	entry := filepath.Join(uiDir, "src", "main.js")
	res, err := b.Build(bundler.Options{
		Entries:  []string{entry},
		OutDir:   filepath.Join(distDir, "assets"),
		Lockfile: lf,
		Store:    st,
		Minify:   true,
	})
	if err != nil {
		return fmt.Errorf("bundle: %w", err)
	}
	if len(res.Errors) > 0 {
		fmt.Fprintf(os.Stderr, "build-ui: %d esbuild errors:\n", len(res.Errors))
		for i, e := range res.Errors {
			if i >= 5 {
				fmt.Fprintf(os.Stderr, "  ... and %d more\n", len(res.Errors)-5)
				break
			}
			loc := ""
			if e.Location != nil {
				loc = fmt.Sprintf(" %s:%d:%d", e.Location.File, e.Location.Line, e.Location.Column)
			}
			fmt.Fprintf(os.Stderr, "  ✗%s %s\n", loc, e.Text)
		}
		return fmt.Errorf("build failed")
	}

	// Catalog written files for the index.html template + a
	// human-readable summary. The type matches writeIndexHTML's
	// expected slice element.
	var outputs []outputFile
	for _, of := range res.OutputFiles {
		name := filepath.Base(of.Path)
		outputs = append(outputs, outputFile{Name: name, Ext: filepath.Ext(name)})
	}
	sort.Slice(outputs, func(i, j int) bool { return outputs[i].Name < outputs[j].Name })

	fmt.Println("[5/5] writing index.html + summary")
	if err := writeIndexHTML(filepath.Join(distDir, "index.html"), outputs); err != nil {
		return fmt.Errorf("write index.html: %w", err)
	}

	var totalBytes int64
	_ = filepath.Walk(distDir, func(_ string, info os.FileInfo, _ error) error {
		if info != nil && !info.IsDir() {
			totalBytes += info.Size()
		}
		return nil
	})
	fmt.Printf("\nbuilt %s\n", distDir)
	fmt.Printf("  %d output files, %.1f KB total\n", len(outputs)+1, float64(totalBytes)/1024)
	for _, o := range outputs {
		fmt.Printf("  - assets/%s\n", o.Name)
	}
	fmt.Println("  - index.html")
	return nil
}

// readPackageJSON pulls the dependency map out of the
// dashboard's package.json. The Go side of the framework still
// uses package.json as the source of truth for which versions
// to bundle — it's the smallest possible cut at "no node, no
// vite": dependencies stay declared in the conventional file,
// just consumed by Go instead of npm.
//
// Once nexus.lock for the dashboard lands properly, this can
// switch to reading nexus.lock directly. v0.1 keeps the
// package.json convention because that's what the existing
// dashboard repo state uses.
type packageJSON struct {
	Dependencies map[string]string `json:"dependencies"`
}

func readPackageJSON(path string) (*packageJSON, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var pj packageJSON
	if err := json.Unmarshal(raw, &pj); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &pj, nil
}

// outputFile is the cataloged shape of one emitted asset.
// Exported field names so the type is usable from both run()
// and writeIndexHTML without a struct redeclaration tax.
type outputFile struct {
	Name string
	Ext  string
}

// writeIndexHTML produces the SPA shell that references the
// hashed bundle files esbuild just emitted. Mirrors what vite
// generates: <link rel="stylesheet"> for every .css, one
// <script type="module"> for the main entry.
//
// The script tag's src uses an absolute path under
// /__nexus/assets/ because the framework mounts the dashboard
// at /__nexus and embeds the dist/ tree there — same as the
// vite config's `base: '/__nexus/'`.
func writeIndexHTML(path string, outputs []outputFile) error {
	var entry, css string
	for _, o := range outputs {
		switch o.Ext {
		case ".js":
			// First .js is the entry (esbuild puts entries
			// before split chunks alphabetically; "main-X.js"
			// sorts to the top).
			if entry == "" {
				entry = o.Name
			}
		case ".css":
			css = o.Name
		}
	}
	if entry == "" {
		return fmt.Errorf("no .js output file — bundle produced %d files, none JS", len(outputs))
	}
	var sb strings.Builder
	sb.WriteString(`<!DOCTYPE html>
<html lang="en">
  <head>
    <meta charset="UTF-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
    <title>Nexus</title>
`)
	if css != "" {
		sb.WriteString(`    <link rel="stylesheet" href="/__nexus/assets/`)
		sb.WriteString(css)
		sb.WriteString("\" />\n")
	}
	sb.WriteString(`  </head>
  <body>
    <div id="app"></div>
    <script type="module" src="/__nexus/assets/`)
	sb.WriteString(entry)
	sb.WriteString("\"></script>\n  </body>\n</html>\n")
	return os.WriteFile(path, []byte(sb.String()), 0o644)
}

// scanSubPathImports reads entryPath and returns the import
// specifiers that declare a sub-path of a known dependency
// (e.g. "@vue-flow/core/dist/style.css" when @vue-flow/core is
// in pkgDeps). Pure regex scan — same false-positive defenses
// as fetcher.ExtractImports would apply, but for v0.1 the entry
// file is a tiny hand-written main.js and the strict-shape
// regex below is enough.
//
// Returned strings are formatted as "<spec>" (no leading /, no
// quotes) so the caller can pass them directly to f.Fetch.
func scanSubPathImports(entryPath string, pkgDeps map[string]string) ([]string, error) {
	body, err := os.ReadFile(entryPath)
	if err != nil {
		return nil, err
	}
	src := string(body)
	// Match either "import X from '...'" or "import '...'" or
	// "import('...')" — capture group 1 is the spec.
	re := regexp.MustCompile(`(?m)import\b[^'"]*['"]([^'"\n]+)['"]`)
	matches := re.FindAllStringSubmatch(src, -1)
	seen := map[string]bool{}
	var out []string
	for _, m := range matches {
		spec := m[1]
		if spec == "" || spec[0] == '.' || spec[0] == '/' {
			continue
		}
		// Only declare-as-sub-path imports — bare package names
		// were already fetched in the main loop. We're looking
		// for "<pkg>/<subpath>" where <pkg> is in deps.
		var matched bool
		for dep := range pkgDeps {
			if strings.HasPrefix(spec, dep+"/") {
				matched = true
				// Convert to the spec shape the fetcher takes:
				// "@vue-flow/core/dist/style.css" stays as-is
				// (no version), specToURL prepends registry +
				// version pin via redirect.
				if !seen[spec] {
					seen[spec] = true
					out = append(out, spec)
				}
				break
			}
		}
		_ = matched
	}
	return out, nil
}

// hashOf is unused in the current pipeline (esbuild does the
// content hashing itself when assetNames includes [hash]) but
// kept around as a building block for future asset workflows
// that need Go-side hashing.
var _ = sha256.New
