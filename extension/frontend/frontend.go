// Package frontend wires a single-page-app bundle into a nexus app as
// a first-class plugin. It does three jobs in one declaration:
//
//  1. Runtime: mounts the built dist directory via nexus.ServeFrontend
//     under the configured Mount path. Same caching policy + SPA
//     fallback the standalone helper has — this package is a thin
//     wrapper, not a reimplementation.
//
//  2. Build-time codegen: declares an extension.Generate driver that
//     the `nexus` CLI picks up to project the live registry into a
//     framework-flavored TS source tree (Vue today; React/Svelte
//     later). The output lands inside Root/Generate so user code can
//     `import { listUsers } from '@/__nexus'` without a runtime
//     manifest fetch.
//
//  3. Manifest plumbing: passes the build-time settings (Root,
//     Output, Generate, Framework) through extension.GenerateContext
//     so other plugins' ClientContributors render against the same
//     framework target. Phase 1 doesn't wire contributors yet — the
//     seam exists so the next PR can light up auth.ts / oauth2.ts
//     without changing this package's surface.
//
// Typical wiring in user main.go:
//
//	//go:embed all:web/dist
//	var webFS embed.FS
//
//	func main() {
//	    nexus.Run(cfg,
//	        frontend.Plugin(frontend.Config{
//	            Root:      "web",
//	            Output:    "dist",
//	            Generate:  "src/__nexus",
//	            Framework: frontend.Vue,
//	            FS:        webFS,
//	        }),
//	        // ...other modules
//	    )
//	}
//
// The //go:embed directive lives in user code because Go's embed
// pragma must be a literal in the package that uses it — the plugin
// can't synthesize it on the user's behalf.
package frontend

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"strings"

	"go.uber.org/fx"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension"
)

// Framework selects the codegen template set. Vue is the only one
// implemented in phase 1; the others are accepted so user code
// declaring `Framework: frontend.React` compiles today and starts
// emitting output when the templates land — no Config field renames.
type Framework string

const (
	Vue    Framework = "vue"
	React  Framework = "react"
	Svelte Framework = "svelte"
	None   Framework = "" // no per-framework adapter — just _client / types / index
)

// Config tunes the frontend plugin. The four "Root/Output/Generate/
// Framework" fields control codegen + bundler invocation; FS + Mount
// control the runtime mount. Zero-value Output and Generate take
// per-Framework defaults so the common case fits in three lines.
type Config struct {
	// Root is the directory holding the frontend project (package.json,
	// vite.config.ts). Relative to the user's go module root. Required.
	Root string

	// Output is the bundler's output directory, relative to Root. The
	// runtime mount reads from path.Join(Root, Output). Defaults to
	// "dist" (Vite's default) when empty.
	Output string

	// Generate is the codegen target directory, relative to Root. The
	// CLI writes the rendered TS source tree here on every `nexus
	// build` / `nexus generate`. Defaults to "src/__nexus".
	Generate string

	// Framework selects the per-framework adapter codegen. "" (None)
	// emits only the transport-neutral files (_client.ts, types.ts,
	// index.ts) — useful for SSR/SSG setups that build their own
	// reactive layer on top.
	Framework Framework

	// FS is the embedded dist supplied by the user via //go:embed.
	// Required at runtime; the codegen path doesn't read it.
	FS fs.FS

	// FSRoot is the path inside FS that holds index.html — matches
	// what nexus.ServeFrontend expects. Defaults to
	// path.Join(Root, Output). Override only when the //go:embed
	// directive declares a different prefix than the build output.
	FSRoot string

	// Mount is the URL prefix the SPA serves under. "" or "/" mounts
	// at the deployment root; "/admin" mounts under that subpath.
	// Composes with the deployment-wide route prefix.
	Mount string
}

// Plugin returns a nexus.Option that registers the frontend extension.
// Validation runs synchronously: a misconfigured Config produces an
// fx.Error wrapped in extension.Use rather than a confusing boot
// failure later. The Option composes:
//
//   - the runtime ServeFrontend mount, and
//   - an extension.Generate driver consumed by `nexus build`.
//
// Phase 1 emits Vue templates only when Framework == Vue; React /
// Svelte resolve to the same transport-neutral output as None until
// their templates ship.
func Plugin(cfg Config) nexus.Option {
	if err := cfg.Validate(); err != nil {
		return nexus.Raw(fx.Error(err))
	}
	if err := cfg.validateRuntime(); err != nil {
		return nexus.Raw(fx.Error(err))
	}
	cfg = cfg.defaults()

	mountOpts := []nexus.FrontendOption{}
	if cfg.Mount != "" {
		mountOpts = append(mountOpts, nexus.FrontendAt(cfg.Mount))
	}

	return extension.Use(extension.Plugin{
		Name:    "frontend",
		Version: pkgVersion,
		Options: []nexus.Option{
			nexus.ServeFrontend(cfg.FS, cfg.FSRoot, mountOpts...),
		},
		Generate: &extension.Generate{
			OutDir: func(app *nexus.App) (string, error) {
				abs, err := filepath.Abs(filepath.Join(cfg.Root, cfg.Generate))
				if err != nil {
					return "", fmt.Errorf("frontend: resolve OutDir: %w", err)
				}
				return abs, nil
			},
			Render: func(ctx extension.GenerateContext) ([]extension.File, error) {
				// Stitch framework choice into Extras so a renderer that
				// branches on it (Vue vs React) doesn't need a separate
				// constructor parameter — the driver and the renderer
				// share the same extension.GenerateContext shape.
				if ctx.Extras == nil {
					ctx.Extras = map[string]any{}
				}
				ctx.Extras["frontend.framework"] = string(cfg.Framework)
				ctx.Extras["frontend.root"] = cfg.Root
				ctx.Extras["frontend.output"] = cfg.Output
				ctx.Extras["frontend.generate"] = cfg.Generate
				return Render(cfg, ctx)
			},
		},
	})
}

// pkgVersion surfaces on the dashboard's plugin list and in
// `nexus doctor`. Bumped manually when the codegen output changes
// shape — consumers' bundles cache against the byte content of the
// generated tree, so a version bump is informational only (no
// runtime gate).
const pkgVersion = "0.1.0-phase1"

// Validate runs the rules every Config must satisfy regardless of
// which entrypoint is in use. Plugin() layers an additional FS check
// on top — the CLI's offline codegen path doesn't need FS, so the
// runtime check lives separately rather than here.
//
// Exported so the `nexus generate frontend` CLI can vet a flag-built
// Config before constructing a Plugin would have any opinion on FS.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Root) == "" {
		return errors.New("frontend.Config.Root is required")
	}
	switch c.Framework {
	case Vue, React, Svelte, None:
	default:
		return fmt.Errorf("frontend.Config.Framework %q is not recognised", c.Framework)
	}
	return nil
}

// validateRuntime is Plugin()'s extra check: the //go:embed FS must
// be supplied so ServeFrontend can mount the bundle. The CLI codegen
// path bypasses this — it never reads the FS.
func (c Config) validateRuntime() error {
	if c.FS == nil {
		return errors.New("frontend.Config.FS is required (declare //go:embed all:<root>/<output> in main.go)")
	}
	return nil
}

// defaults fills the zero-value fields. Called after validate so a
// misconfiguration surfaces with the user's literal field, not a
// surprise default.
func (c Config) defaults() Config {
	if c.Output == "" {
		c.Output = "dist"
	}
	if c.Generate == "" {
		c.Generate = "src/__nexus"
	}
	if c.FSRoot == "" {
		c.FSRoot = path.Join(c.Root, c.Output)
	}
	return c
}
