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

	"github.com/paulmanoni/nexus/httpx"
	"go.uber.org/fx"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/client"
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

	// RuntimeSDK, when true, also mounts the legacy client SDK asset
	// routes (/__nexus/client/client.js, vue.js, *.d.ts). No-bundler
	// apps that import from '/__nexus/client/vue.js' at runtime need
	// this; apps using the typed codegen tree do not.
	//
	// The manifest and contributions routes ALWAYS mount alongside
	// the frontend Plugin — they're how `nexus generate frontend`
	// reaches into the running app. RuntimeSDK only controls the
	// additional static JS surfaces.
	RuntimeSDK bool
	// ManifestPublic, when true, serves the full manifest +
	// contributions to anonymous browsers. Default false on its own
	// (skinny manifest — auth flows only — keeps the schema away
	// from scrapers), but RuntimeSDK:true forces this to true
	// regardless because the runtime SDK's nx.query / nx.mutate /
	// nx.crud surface looks up ops in the fetched manifest body —
	// the skinny shape makes every non-auth call fail at runtime
	// with "no GraphQL query named X".
	//
	// Apps that genuinely want a private schema use the typed
	// codegen path (RuntimeSDK:false) and import per-op functions
	// from web/src/__nexus/; those calls don't depend on a fetched
	// manifest, so the runtime visibility flag stops mattering.
	ManifestPublic bool

	// ClientMiddleware applies to every /__nexus/client/* route.
	// Equivalent to client.Config.Middleware; lifted here so apps
	// declare it once on the frontend Plugin instead of plumbing a
	// separate client.Config alongside.
	ClientMiddleware []httpx.HandlerFunc

	// SDKOutDir, when non-empty, makes the client SDK route group
	// also dump client.js / vue.js / *.d.ts / manifest.json to disk
	// at boot, so an IDE resolving the runtime URL imports
	// ('/__nexus/client/vue.js') can find the paired .d.ts on the
	// local filesystem. Equivalent to client.Config.OutDir.
	//
	// Defaults to "./<Root>/sdk" when RuntimeSDK is true; stays
	// empty otherwise so the typed-codegen path doesn't drop
	// surprise files into the project tree. Override only when the
	// convention doesn't fit (custom layout, multi-tenant builds).
	SDKOutDir string

	// SDKTSConfig, when non-empty, makes the SDK mount merge path
	// mappings (runtime URL → SDKOutDir files) into the named
	// jsconfig.json or tsconfig.json on startup. Equivalent to
	// client.Config.TSConfig. Only takes effect when SDKOutDir is
	// also set — path mappings need a target.
	//
	// Defaults to "./<Root>/tsconfig.json" when RuntimeSDK is true;
	// stays empty otherwise. The merge is idempotent and tolerant
	// of a missing file (logs + skips), so the default is safe
	// even on apps that don't use TypeScript.
	SDKTSConfig string

	// SDKViteConfig, when non-empty, auto-attaches the
	// nexus-vite-plugin to the named Vite config on startup.
	// Equivalent to client.Config.ViteConfig. Only takes effect
	// when SDKOutDir is also set (the plugin file lives there).
	//
	// No auto-default — vite.config.ts presence is detected by
	// client.applyFrontendDefaults via filesystem probing, which
	// is the right call site for "do you have a vite project?"
	// questions. Set explicitly when you want frontend.Plugin to
	// drive the attach instead of relying on the detection.
	SDKViteConfig string
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
			mountClientSDK(cfg),
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
//
// SDKOutDir / SDKTSConfig only default when RuntimeSDK is true —
// opting into the runtime SDK is also opting into the auto-dump
// that makes IntelliSense work on '/__nexus/client/vue.js'-style
// imports. Apps using the typed codegen tree leave RuntimeSDK
// false; the defaults stay empty so we don't write SDK files into
// a project tree the user never asked to populate.
//
// ManifestPublic also follows RuntimeSDK. The runtime SDK's
// nx.query / nx.mutate / nx.crud calls look up ops by name in the
// manifest body the browser fetches; the skinny "auth-flows-only"
// default would make every non-auth call fail at runtime with
// "no GraphQL query named X". Coupling the two flips removes the
// footgun — operators wanting a private schema use the typed
// codegen path (RuntimeSDK:false) and import per-op functions
// instead, which never fetch the manifest at all.
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
	if c.RuntimeSDK {
		if c.SDKOutDir == "" {
			c.SDKOutDir = "./" + c.Root + "/sdk"
		}
		if c.SDKTSConfig == "" {
			c.SDKTSConfig = "./" + c.Root + "/tsconfig.json"
		}
		// The runtime SDK is unusable without the full manifest —
		// see the function-level doc for the reasoning. We can't
		// distinguish "unset" from "explicitly false" on a Go bool,
		// so the coupling is unconditional. Apps that need a private
		// schema must drop RuntimeSDK in favor of the codegen tree.
		c.ManifestPublic = true
		// SDKViteConfig stays opt-in: defaulting it would auto-attach
		// the nexus-vite-plugin to a vite.config.ts that might not
		// exist. client.applyFrontendDefaults already handles the
		// "vite project detected" case via filesystem probing — let
		// that path own the auto-attach decision.
	}
	return c
}

// mountClientSDK installs the /__nexus/client/* routes — manifest,
// contributions, and (when RuntimeSDK is true) the legacy SDK runtime
// JS files — by delegating to nexus.ClientUseWithContributions. The
// builder factory captures the user-side Config and the *App injected
// by fx, then returns a closure HTTP handlers call on each
// contributions.json request.
//
// Idempotent: if nexus.Config.Client.Enabled already mounted the SDK
// (back-compat path), ClientUseWithContributions's existing handler
// check short-circuits. Apps that want the contributions route MUST
// drop Config.Client.Enabled in favor of frontend.Plugin — the
// auto-mounted handler doesn't know about contributors.
func mountClientSDK(cfg Config) nexus.Option {
	ccfg := clientConfigFromFrontend(cfg)
	return nexus.ClientUseWithContributions(ccfg, func(app *nexus.App) client.ContributionsBuilder {
		return func(framework string) (client.ContributionsResponse, error) {
			return renderContributionsResponse(app, cfg, framework)
		}
	})
}

// clientConfigFromFrontend projects the frontend.Config's SDK-related
// fields into a client.Config. Hoisted out of mountClientSDK so tests
// can verify the field mapping without spinning up the whole fx graph
// just to observe one struct.
//
// SkipAssets is the inverse of RuntimeSDK: when the user opts out of
// runtime SDK imports, the static asset routes (client.js / vue.js /
// *.d.ts) don't register. The codegen routes (manifest +
// contributions) mount unconditionally so `nexus generate frontend`
// works either way.
func clientConfigFromFrontend(cfg Config) client.Config {
	ccfg := client.Config{
		Enabled:    true,
		Public:     cfg.ManifestPublic,
		Middleware: cfg.ClientMiddleware,
		OutDir:     cfg.SDKOutDir,
		TSConfig:   cfg.SDKTSConfig,
		ViteConfig: cfg.SDKViteConfig,
		SkipAssets: !cfg.RuntimeSDK,
	}
	return client.ApplyVisibilityDefaults(ccfg, false)
}

// renderContributionsResponse is the closure body for the
// contributions builder. Walks the App's registered contributors,
// invokes each with the live registry + the requested framework, and
// packages the result into the wire shape client/contributions.go
// declared. Errors propagate so the HTTP layer can surface a 500 to
// the CLI.
func renderContributionsResponse(app *nexus.App, cfg Config, framework string) (client.ContributionsResponse, error) {
	out := client.ContributionsResponse{
		Version:   client.SchemaVersion,
		Framework: framework,
	}
	contributors := app.ClientContributors()
	if len(contributors) == 0 {
		return out, nil
	}
	extras := map[string]any{
		"frontend.framework": framework,
		"frontend.root":      cfg.Root,
		"frontend.output":    cfg.Output,
		"frontend.generate":  cfg.Generate,
	}
	ctx := nexus.GenerateContext{
		Registry: app.Registry(),
		Refs:     app.SchemaRefs(),
		Extras:   extras,
	}
	for _, rec := range contributors {
		files, err := rec.Contribute(ctx)
		if err != nil {
			return out, fmt.Errorf("plugin %s: %w", rec.PluginName, err)
		}
		if len(files) == 0 {
			continue
		}
		group := client.ContributionPluginRec{Name: rec.PluginName}
		for _, f := range files {
			group.Files = append(group.Files, client.ContributionFileRec{
				Path: f.Path,
				Body: string(f.Body),
			})
		}
		out.Plugins = append(out.Plugins, group)
	}
	return out, nil
}
