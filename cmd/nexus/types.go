package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/paulmanoni/nexus/frontend/deps/dts"
	"github.com/paulmanoni/nexus/frontend/deps/lockfile"
)

// newTypesCmd builds `nexus types` — generate a types-only node_modules
// tree for editor IntelliSense, driven by nexus.lock. No npm: the .d.ts
// files come from esm.sh (the same registry the build uses) and are
// rewritten so the editor's TypeScript/Vue language server resolves bare
// imports (`import { ref } from "vue"`) without a real node_modules.
// nexus build and runtime stay zero-Node; this tree is editor-only.
func newTypesCmd(stdout, stderr io.Writer) *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "types",
		Short: "Generate a types-only node_modules for editor IntelliSense (no npm)",
		Long: `Reads nexus.lock and fetches each dependency's TypeScript
declarations (.d.ts) from the registry, rewriting cross-package URL
references to local relative paths so a standard TS/Vue language server
resolves bare imports without npm.

The output (default: <islands.src>/node_modules) is an EDITOR-ONLY artifact:
nexus build and the runtime never read it. It is gitignored automatically.
Re-run after changing dependencies (` + "`nexus add` / `nexus update`" + `).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTypes(stdout, stderr, out)
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "output node_modules dir (default: <islands.src>/node_modules)")
	return cmd
}

func runTypes(stdout, stderr io.Writer, outDir string) error {
	dc, err := newDepsContext(stdout, stderr)
	if err != nil {
		return err
	}
	lf, err := lockfile.Load(dc.lockfilePath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(stderr, "nexus types: no nexus.lock — run `nexus add <pkg>` first")
			return nil
		}
		return err
	}

	// Default output under the islands source dir so tsconfig (which lives
	// there) finds it via standard node_modules resolution.
	root := filepath.Dir(dc.lockfilePath)
	if outDir == "" {
		outDir = filepath.Join(root, islandsSrcName(), "node_modules")
	}

	res, err := emitLockfileTypes(lf, dc.fetcher.Registry, dc.fetcher.HTTP, outDir, stderr)
	if err != nil {
		return err
	}
	if res.Packages == 0 && res.Files == 0 && len(res.Skipped) == 0 {
		fmt.Fprintln(stderr, "nexus types: no eligible packages in nexus.lock")
		return nil
	}

	fmt.Fprintf(stdout, "nexus types: wrote types for %d package(s), %d file(s) → %s\n",
		res.Packages, res.Files, outDir)
	if len(res.Skipped) > 0 {
		fmt.Fprintf(stdout, "nexus types: %d package(s) had no published types (skipped): %s\n",
			len(res.Skipped), strings.Join(res.Skipped, ", "))
	}
	return nil
}

// emitLockfileTypes is the shared core behind `nexus types` and the
// background type-sync `nexus dev` runs: it turns a loaded lockfile into a
// types-only node_modules tree under outDir. registry/client name the source
// (defaulting to esm.sh + a fresh client when empty/nil). It also writes the
// editor-only node_modules/.gitignore. Returns a zero Result (no error) when
// the lockfile has no eligible packages.
func emitLockfileTypes(lf *lockfile.File, registry string, client *http.Client, outDir string, stderr io.Writer) (dts.Result, error) {
	if registry == "" {
		registry = "https://esm.sh"
	}
	registry = strings.TrimRight(registry, "/")

	// Build the package list from the lockfile: one entry per distinct
	// top-level spec (skip the registry-internal URL-spec entries, which
	// have no bare name an editor would import). The probe URL is the
	// versioned bare package on the registry; its X-TypeScript-Types header
	// names the root .d.ts.
	seen := map[string]bool{}
	var pkgs []dts.Pkg
	for _, p := range lf.Packages {
		// Only real bare package specs (vue, @vue/runtime-dom). Skip
		// URL-shaped specs (https://…) and nexus-client (local SDK).
		if p.Spec == "" || strings.Contains(p.Spec, "://") || p.Version == "" {
			continue
		}
		if p.Spec == "nexus-client" || strings.HasPrefix(p.Spec, "nexus-client/") {
			continue
		}
		if seen[p.Spec] {
			continue
		}
		seen[p.Spec] = true
		pkgs = append(pkgs, dts.Pkg{
			Name:     p.Spec,
			Version:  p.Version,
			ProbeURL: registry + "/" + p.Spec + "@" + p.Version,
		})
	}
	if len(pkgs) == 0 {
		return dts.Result{}, nil
	}

	res, err := dts.Emit(pkgs, newTypesGetter(client), newTypesWriter(outDir))
	if err != nil {
		return res, fmt.Errorf("nexus types: %w", err)
	}

	// Make the tree editor-only + invisible to git. A node_modules/.gitignore
	// of "*" keeps the whole dir out of version control without touching the
	// project's root .gitignore.
	if werr := os.WriteFile(filepath.Join(outDir, ".gitignore"), []byte("*\n"), 0o644); werr != nil {
		fmt.Fprintf(stderr, "nexus types: warning: could not write node_modules/.gitignore: %v\n", werr)
	}
	return res, nil
}

// newTypesGetter returns a dts.Getter backed by an http.Client. For a probe
// (bare package) URL it returns the X-TypeScript-Types header; for a .d.ts
// URL it returns the body. esm.sh 302-redirects bare specs to a pinned
// version and serves types declarations directly.
func newTypesGetter(client *http.Client) dts.Getter {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return func(url string) (string, string, error) {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return "", "", err
		}
		// esm.sh tailors output to the UA; a browser-ish UA gets the real
		// declarations + the X-TypeScript-Types header.
		req.Header.Set("User-Agent", "Mozilla/5.0 (nexus types)")
		resp, err := client.Do(req)
		if err != nil {
			return "", "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			return "", "", fmt.Errorf("GET %s: %s", url, resp.Status)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return "", "", err
		}
		return string(body), resp.Header.Get("X-TypeScript-Types"), nil
	}
}

// newTypesWriter returns a dts.Writer that writes under outDir, creating
// parent dirs. relPath is slash-separated and node_modules-relative.
func newTypesWriter(outDir string) dts.Writer {
	return func(relPath, contents string) error {
		full := filepath.Join(outDir, filepath.FromSlash(relPath))
		// Containment guard: never escape outDir.
		if rel, err := filepath.Rel(outDir, full); err != nil || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("refusing to write outside node_modules: %s", relPath)
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		return os.WriteFile(full, []byte(contents), 0o644)
	}
}
