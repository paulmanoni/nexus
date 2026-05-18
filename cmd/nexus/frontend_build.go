package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

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
	srcDir := filepath.Join(projectRoot, "islands.src")
	if _, err := os.Stat(srcDir); errors.Is(err, fs.ErrNotExist) {
		return nil // no frontend in this project — skip
	} else if err != nil {
		return fmt.Errorf("frontend build: stat %s: %w", srcDir, err)
	}

	entries, err := collectFrontendEntries(srcDir)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		// islands.src exists but is empty — also skip; user may
		// just be staging the directory.
		return nil
	}

	// Reject .vue with a clear message rather than producing a
	// broken bundle. Once the Vue SFC bootstrap clears the Goja
	// edge, this branch drops and the SFC plugin wires in here.
	for _, e := range entries {
		if strings.HasSuffix(e, ".vue") {
			return fmt.Errorf("frontend build: %s is a .vue source — v0.1 doesn't yet compile Vue SFC; "+
				"pre-compile it to .js or keep this project on vite for now", e)
		}
	}

	lockfilePath := filepath.Join(projectRoot, lockfile.Filename)
	lf, err := lockfile.Load(lockfilePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("frontend build: %s has entries but no nexus.lock — run `nexus add` for any "+
				"frontend dependencies your code imports", srcDir)
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

	outDir := filepath.Join(projectRoot, "islands")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("frontend build: mkdir %s: %w", outDir, err)
	}

	b := bundler.New()
	b.AddPlugin(plugin)
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
	fmt.Fprintf(stdout, "frontend build: wrote %d output %s to %s\n",
		len(res.OutputFiles), pluralize("file", len(res.OutputFiles)), outDir)
	return nil
}

// collectFrontendEntries walks srcDir and returns one entry path
// per top-level file matching a supported extension. Subdirectories
// are treated as colocated source (their files aren't entries
// themselves but are reachable via relative imports from the
// top-level entries). One entry per top-level file matches the
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
		case ".jsx", ".tsx", ".ts", ".js", ".vue":
			entries = append(entries, filepath.Join(srcDir, name))
		default:
			// .css / .md / .json — colocated but not entries.
		}
	}
	return entries, nil
}

