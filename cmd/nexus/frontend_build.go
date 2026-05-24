package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/evanw/esbuild/pkg/api"

	"github.com/paulmanoni/nexus/frontend/deps/bundler"
	"github.com/paulmanoni/nexus/frontend/deps/lockfile"
	"github.com/paulmanoni/nexus/frontend/deps/resolver"
	"github.com/paulmanoni/nexus/frontend/deps/store"
)

// frontendBuild scans <projectRoot>/islands.src for frontend entry
// files (.jsx, .tsx, .ts, .js, .vue) and bundles each one through
// our esbuild-based pipeline into <projectRoot>/islands. The
// resulting bundles get picked up by the embed-gen step that runs
// next, so `go build` ships them inside the binary.
//
// This is the function `nexus build` calls before `go build` —
// replaces the old "user must have run npm/vite separately" model.
//
// Skips silently when:
//   - islands.src does not exist (project doesn't use frontend
//     dependencies)
//   - islands.src exists but has no entry files (rare; user staged
//     a directory but no sources yet)
//
// Returns an error when islands.src exists with sources AND any
// of: nexus.lock is missing, the cache can't be opened, esbuild
// reports build errors, the entries import unresolvable specs.
//
// Conservative defaults:
//
//   - srcDir: <projectRoot>/islands.src
//   - outDir: <projectRoot>/islands
//   - registry/cache: env-controlled via NEXUS_REGISTRY / NEXUS_CACHE
//   - minify: true (production build path)
//   - sourcemap: linked
//
// .vue files are accepted but the Vue SFC plugin is NOT wired in
// v0.1 because the Goja-based compile pipeline has known
// limitations on @vue/compiler-sfc 3.4 + esm.sh (see
// frontend/deps/sfc/vue/bootstrap_test.go for details). A clear
// error guides the user to either pre-compile or stay on vite for
// .vue today.
func frontendBuild(projectRoot string, stdout, stderr io.Writer) error {
	srcDir := filepath.Join(projectRoot, islandsSrcName())
	if _, err := os.Stat(srcDir); errors.Is(err, fs.ErrNotExist) {
		return nil // no frontend in this project — skip
	} else if err != nil {
		return fmt.Errorf("frontend build: stat %s: %w", srcDir, err)
	}

	// .vue source files (anywhere under islands.src/, not just
	// top-level entries) require the QuickJS-backed SFC compiler
	// which is build-tagged behind cgo+vue. When the binary was
	// built pure-Go (or without the vue tag), the vueCompilerHook
	// stays nil and we reject .vue with the same clear message
	// the v0.1 flow used.
	//
	// This check runs BEFORE the empty-entries short-circuit so a
	// project with only `App.vue` (no bootstrap .ts) still surfaces
	// the build-tag hint instead of silently producing nothing — a
	// user staging vue components first and the entry later would
	// otherwise see no output and have no clue why.
	hasVue, err := hasVueSources(srcDir)
	if err != nil {
		return fmt.Errorf("frontend build: scan vue sources: %w", err)
	}
	if hasVue && vueCompilerHook == nil {
		return errors.New("frontend build: .vue sources detected but this nexus was built without Vue SFC support — " +
			"rebuild with `CGO_ENABLED=1 go install -tags vue ./cmd/nexus` to enable")
	}

	actualSrcDir, entries, err := findFrontendEntries(srcDir)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		// islands.src exists but is empty — also skip; user may
		// just be staging the directory.
		return nil
	}
	if actualSrcDir != srcDir {
		// Auto-descended into a conventional subdir (src/ / app/ /
		// client/). Use the resolved path from here on so the
		// bundler watches the right tree.
		srcDir = actualSrcDir
	}

	lockfilePath := filepath.Join(projectRoot, lockfile.Filename)
	lf, err := lockfile.Load(lockfilePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("frontend build: %s", formatMissingLockfileError(srcDir, entries))
		}
		return fmt.Errorf("frontend build: load lockfile: %w", err)
	}

	cacheRoot := os.Getenv("NEXUS_CACHE")
	if cacheRoot == "" {
		cacheRoot = store.DefaultRoot()
	}
	st, err := store.New(cacheRoot)
	if err != nil {
		return fmt.Errorf("frontend build: open cache %s: %w", cacheRoot, err)
	}

	plugin, err := resolver.New(resolver.Options{Lockfile: lf, Store: st})
	if err != nil {
		return fmt.Errorf("frontend build: build resolver: %w", err)
	}

	outDir := filepath.Join(projectRoot, islandsOutName())
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("frontend build: mkdir %s: %w", outDir, err)
	}

	b := bundler.New()
	b.AddPlugin(plugin)

	// When the vue hook is wired (cgo+vue build), bootstrap the
	// compiler + register the SFC plugin. The hook owns the
	// QuickJS lifecycle; we close it on return.
	if hasVue && vueCompilerHook != nil {
		closeVue, vuePlugin, err := vueCompilerHook(lf, st)
		if err != nil {
			return fmt.Errorf("frontend build: vue compiler: %w", err)
		}
		defer closeVue()
		b.AddPlugin(vuePlugin)
	}

	noun := "entry"
	if len(entries) != 1 {
		noun = "entries"
	}
	fmt.Fprintf(stdout, "nexus build: bundling %d frontend %s\n", len(entries), noun)
	res, err := b.Build(bundler.Options{
		Entries:  entries,
		OutDir:   outDir,
		Minify:   true,
		Lockfile: lf,
		Store:    st,
		LogTo:    stderr,
	})
	if err != nil {
		return fmt.Errorf("frontend build: %w", err)
	}
	if len(res.Errors) > 0 {
		// One concrete bundler error is enough — the rest cascade
		// from the same root cause typically.
		return fmt.Errorf("frontend build: %s", res.Errors[0].Text)
	}
	// Vite-replacement step: if the project has an index.html
	// (either next to the entries or one level up — the typical
	// Vite layout has it at the source-root with entries under
	// src/), rewrite its module + stylesheet refs to point at
	// the bundled output names + drop the result into outDir.
	// Without this the operator gets the framework's "no
	// frontend yet" placeholder even though the JS bundled
	// fine.
	if err := emitIndexHTML(srcDir, outDir, res.OutputFiles, stdout, false); err != nil {
		fmt.Fprintf(stderr, "warning: index.html emit: %v\n", err)
	}
	fmt.Fprintf(stdout, "frontend build: wrote %d output %s to %s\n",
		len(res.OutputFiles), pluralize("file", len(res.OutputFiles)), outDir)
	return nil
}

// vueCompilerHook is the build-tagged registration point for the
// Vue SFC compile plugin. The default value (nil, set in this
// file's package init or just declared zero) means "this build
// has no Vue support, reject .vue with a clear message". A
// sibling file with `//go:build cgo && vue` populates the hook
// at init time; without those tags, vue/ never compiles and the
// hook stays nil.
//
// The signature gives the hook everything it needs: the
// project's lockfile (for resolving @vue/compiler-sfc) and the
// shared store. Returns a teardown func + the esbuild plugin
// the bundler should add.
var vueCompilerHook func(*lockfile.File, *store.Store) (func(), api.Plugin, error)

// collectFrontendEntries walks srcDir and returns one entry path
// per top-level JS/TS file. Vue SFCs (.vue) are NOT entries — they
// get pulled in via `import App from './App.vue'` inside a .ts/.tsx
// bootstrap file, and the SFC plugin loads them as transitive
// dependencies. Treating App.vue as its own entry would emit a
// separate App.js bundle alongside main.js (duplicating Vue
// runtime + breaking the watch mode's esbuild context init when
// the SFC compiler's QuickJS worker isn't ready at entry-discovery
// time).
//
// One entry per top-level .ts/.tsx/.jsx/.js file matches the
// convention `nexus island` scaffolds; users with several pages
// produce several bundles automatically.
func collectFrontendEntries(srcDir string) ([]string, error) {
	dirents, err := os.ReadDir(srcDir)
	if err != nil {
		return nil, fmt.Errorf("frontend build: read %s: %w", srcDir, err)
	}
	var entries []string
	for _, e := range dirents {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		switch ext := filepath.Ext(name); ext {
		case ".jsx", ".tsx", ".ts", ".js":
			entries = append(entries, filepath.Join(srcDir, name))
		default:
			// .vue / .css / .md / .json — colocated but not entries.
		}
	}
	return entries, nil
}

// nestedEntryFallbacks is the conventional subdirectory names
// auto-detection checks when the top level of srcDir has no
// matching entries. Vite + Webpack + most JS scaffolds put the
// bootstrap under `src/`; nuxt and a few others use `app/` or
// `client/`. Operators with a different layout can still
// override via NEXUS_ISLANDS_SRC.
//
// Order matters: src first because it's by far the most common.
var nestedEntryFallbacks = []string{"src", "app", "client"}

// findFrontendEntries returns the dir + entry list to drive the
// bundler. Tries top-level first (the canonical islands
// convention), then falls back to conventional subdirs so
// projects that keep their bootstrap under src/ — the Vite shape
// every JS scaffold ships with — work out of the box.
//
// The returned `actualDir` is the directory that produced the
// entries; callers should use it as the bundler's srcDir AND
// the watcher's target. Same as srcDir when entries lived at
// the top level; deeper when an auto-descent picked them up.
//
// Empty entries return (srcDir, nil, nil) so the caller's
// empty-state logic still gets a chance to print its hint
// against the originally-requested directory.
func findFrontendEntries(srcDir string) (actualDir string, entries []string, err error) {
	if entries, err = collectFrontendEntries(srcDir); err != nil {
		return srcDir, nil, err
	}
	if len(entries) > 0 {
		return srcDir, entries, nil
	}
	for _, name := range nestedEntryFallbacks {
		sub := filepath.Join(srcDir, name)
		info, statErr := os.Stat(sub)
		if statErr != nil || !info.IsDir() {
			continue
		}
		nestedEntries, nestedErr := collectFrontendEntries(sub)
		if nestedErr != nil {
			continue
		}
		if len(nestedEntries) > 0 {
			return sub, nestedEntries, nil
		}
	}
	return srcDir, nil, nil
}

// hasVueSources reports whether srcDir (recursively) contains any
// .vue file. The frontendBuild + dev watcher use this to decide
// whether to bootstrap the SFC compiler — needed even when no .vue
// file is a top-level ENTRY, because main.ts may transitively
// import an App.vue that needs compiling.
func hasVueSources(srcDir string) (bool, error) {
	var found bool
	err := filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".vue") {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found, err
}
