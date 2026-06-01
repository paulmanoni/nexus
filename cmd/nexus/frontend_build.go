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

	"github.com/paulmanoni/nexus/frontend/deps/lockfile"
	"github.com/paulmanoni/nexus/frontend/deps/sfc/vue"
	"github.com/paulmanoni/nexus/frontend/deps/store"
)

// frontendBuild builds the frontend with Vite so `go build` can embed the
// output. It resolves <projectRoot>/web (or NEXUS_FRONTEND_DIR), runs
// `npm ci`/`npm install` when node_modules is absent, then `npm run build`
// (vite build → web/dist). The embed-gen step that runs next bakes web/dist
// into the binary.
//
// This is the function `nexus build` calls before `go build`. It requires
// Node/npm on PATH (the accepted build-time dependency of the Vite pipeline);
// the runtime stays a single Go binary with the SPA embedded.
//
// Skips silently when <dir>/package.json is absent (a pure-Go app with no
// frontend). Returns an error when npm/vite fail.
func frontendBuild(projectRoot string, stdout, stderr io.Writer) error {
	dir := filepath.Join(projectRoot, frontendDirName())
	pkgJSON := filepath.Join(dir, "package.json")
	if _, err := os.Stat(pkgJSON); errors.Is(err, fs.ErrNotExist) {
		return nil // no frontend project here — pure-Go app, skip
	} else if err != nil {
		return fmt.Errorf("frontend build: stat %s: %w", pkgJSON, err)
	}

	// Install deps when node_modules is absent. Prefer the reproducible
	// `npm ci` when a lockfile exists, else `npm install`.
	if _, err := os.Stat(filepath.Join(dir, "node_modules")); errors.Is(err, fs.ErrNotExist) {
		sub := "install"
		if _, lerr := os.Stat(filepath.Join(dir, "package-lock.json")); lerr == nil {
			sub = "ci"
		}
		fmt.Fprintf(stdout, "%s●%s frontend: node_modules missing — npm %s in %s\n", ansiCyan, ansiReset, sub, dir)
		if err := runFrontendNpm(dir, stdout, stderr, sub); err != nil {
			return fmt.Errorf("frontend build: npm %s in %s: %w", sub, dir, err)
		}
	}

	fmt.Fprintf(stdout, "%s●%s frontend: npm run build (vite) in %s\n", ansiCyan, ansiReset, dir)
	if err := runFrontendNpm(dir, stdout, stderr, "run", "build"); err != nil {
		return fmt.Errorf("frontend build: npm run build in %s: %w", dir, err)
	}
	return nil
}

// runFrontendNpm runs `npm <args...>` in dir, streaming output. execCommand
// (build_embed.go) is the package-level seam tests stub out.
func runFrontendNpm(dir string, stdout, stderr io.Writer, args ...string) error {
	cmd := execCommand("npm", args...)
	cmd.Dir = dir
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
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
var vueCompilerHook func(*lockfile.File, *store.Store) (func(), api.Plugin, vue.SFCCompiler, error)

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
