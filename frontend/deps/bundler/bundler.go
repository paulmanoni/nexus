// Package bundler wraps esbuild's Build API and wires the
// nexus-specific plugins (resolver + Vue SFC) so .jsx / .tsx /
// .vue entry points produce a single self-contained JS bundle
// without any Node-side toolchain.
//
// The bundler is the user-visible surface for "build my frontend":
// `nexus build` and `nexus dev` both drive it. CLI commands like
// `nexus add` don't go through here — they only touch the store +
// fetcher + lockfile.
//
// Plugin layering (registration order = first-crack order):
//
//  1. Resolver plugin (OnResolve) — intercepts bare imports,
//     looks them up in the lockfile, returns the on-disk path
//     of the cached blob. Falls through to esbuild's default
//     resolver for relative paths and unknown specs.
//  2. Vue SFC plugin (OnLoad on *.vue) — runs the .vue source
//     through @vue/compiler-sfc via Goja, returns the resulting
//     JS as if it were a regular module.
//  3. esbuild's native handlers cover .js / .ts / .jsx / .tsx /
//     .css / .json / etc.
package bundler

import (
	"fmt"
	"io"

	"github.com/evanw/esbuild/pkg/api"

	"github.com/paulmanoni/nexus/frontend/deps/lockfile"
	"github.com/paulmanoni/nexus/frontend/deps/store"
)

// Options configures one Build call. All fields are optional
// except Entries; sensible defaults are filled in by applyDefaults.
type Options struct {
	// Entries lists the absolute or project-relative paths of the
	// modules to bundle. Each becomes one output bundle. Most
	// users supply a single entry (e.g. "frontend/app.tsx");
	// multi-entry is supported for sites with several independent
	// island bundles per page.
	Entries []string

	// OutDir is the directory bundles are written into. Defaults
	// to "islands" — matches the convention the rest of the
	// framework expects (template.WithStatic("islands") serves it).
	OutDir string

	// Minify toggles esbuild's minifier. Defaults to true for
	// production-ish output; tests + dev mode flip it off for
	// readable output.
	Minify bool

	// Sourcemap controls source-map emission. Defaults to
	// api.SourceMapLinked (separate .map file alongside the
	// bundle, referenced by a //# sourceMappingURL comment).
	Sourcemap api.SourceMap

	// Watch enables esbuild's native watch mode. When true, Build
	// returns the first build's result immediately and esbuild
	// continues watching in the background. The OnRebuild
	// callback (if any) fires after each subsequent rebuild.
	// Caller must hold a reference to the returned Context to
	// stop the watcher cleanly.
	Watch bool

	// OnRebuild is the watch-mode callback fired after each
	// successful rebuild. Ignored when Watch is false.
	OnRebuild func(api.BuildResult)

	// Lockfile is the deps lockfile the resolver plugin consults.
	// Required when bundling code with bare imports; nil is fine
	// for pure-relative-import bundles (rare).
	Lockfile *lockfile.File

	// Store is the disk cache the resolver plugin reads cached
	// blobs from. Required when Lockfile is non-nil.
	Store *store.Store

	// Target is the JS language level esbuild targets. Defaults
	// to api.ES2022 — covers every browser less than 3 years old.
	Target api.Target

	// LogTo is where esbuild's diagnostics get printed. When nil,
	// esbuild's LogLevel is set to Silent and the caller reads
	// Errors/Warnings off the BuildResult instead. CLI sets this
	// to os.Stderr for interactive feedback.
	LogTo io.Writer
}

// Bundler holds plugins applied across every Build call. The user
// constructs one per project (or per CLI invocation), registers
// plugins via AddPlugin, then calls Build with per-invocation
// Options.
type Bundler struct {
	// Plugins is the list of esbuild plugins applied to every
	// Build call. Populated via AddPlugin. The order matters —
	// earlier plugins get first dibs at OnResolve/OnLoad events
	// for matching paths. Resolver should come BEFORE the Vue SFC
	// plugin so a bare "vue" import resolves before any .vue
	// load handler sees it.
	Plugins []api.Plugin
}

// New returns a Bundler with no plugins registered.
func New() *Bundler { return &Bundler{} }

// AddPlugin registers a plugin applied to every Build call.
// Plugins run in registration order.
func (b *Bundler) AddPlugin(p api.Plugin) {
	b.Plugins = append(b.Plugins, p)
}

// Result wraps esbuild's BuildResult plus the watch-mode Context
// (nil when Watch was false). Callers in watch mode hold onto Ctx
// and call Stop() / Dispose() to tear down the watcher cleanly.
type Result struct {
	api.BuildResult
	Ctx api.BuildContext
}

// Build produces bundles for opts.Entries. Returns the esbuild
// BuildResult so callers can inspect diagnostics + output files.
// Errors only surface when the configuration itself is invalid;
// per-file build diagnostics land in Result.Errors and don't
// short-circuit to a Go error.
func (b *Bundler) Build(opts Options) (Result, error) {
	if err := opts.validate(); err != nil {
		return Result{}, err
	}
	b.applyDefaults(&opts)

	logLevel := api.LogLevelSilent
	if opts.LogTo != nil {
		logLevel = api.LogLevelInfo
	}

	buildOpts := api.BuildOptions{
		EntryPoints:       opts.Entries,
		Outdir:            opts.OutDir,
		Bundle:            true,
		Write:             true,
		Format:            api.FormatESModule,
		Target:            opts.Target,
		Sourcemap:         opts.Sourcemap,
		MinifyWhitespace:  opts.Minify,
		MinifyIdentifiers: opts.Minify,
		MinifySyntax:      opts.Minify,
		Plugins:           b.Plugins,
		LogLevel:          logLevel,
		// Vue's esm-bundler distribution (which is what esm.sh
		// serves) reads three compile-time flags as bare global
		// identifiers — without build-time substitution they
		// surface in the browser as ReferenceErrors at module
		// load (the very first line of runtime-core.mjs does
		// `__VUE_PROD_DEVTOOLS__ && something`).
		//
		// Defaults match Vue's recommended production values:
		//   - PROD_DEVTOOLS=false       no Vue devtools hook
		//   - OPTIONS_API=true          keep Options API support
		//                               (set false if your app
		//                               is composition-only and
		//                               you want a smaller bundle)
		//   - HYDRATION_MISMATCH=false  drop SSR hydration debug
		//                               output from prod bundles
		//
		// Harmless for non-Vue projects — the identifiers just
		// don't appear in non-Vue code, so the Define has no
		// effect. Caller-supplied Define entries (future
		// option) would merge here rather than replace.
		Define: map[string]string{
			"__VUE_PROD_DEVTOOLS__":                  "false",
			"__VUE_OPTIONS_API__":                    "true",
			"__VUE_PROD_HYDRATION_MISMATCH_DETAILS__": "false",
		},
	}

	if opts.Watch {
		ctx, ctxErr := api.Context(buildOpts)
		if len(ctxErr.Errors) > 0 {
			return Result{}, fmt.Errorf("bundler: esbuild context: %s", ctxErr.Errors[0].Text)
		}
		first := ctx.Rebuild()
		if opts.OnRebuild != nil {
			opts.OnRebuild(first)
		}
		if err := ctx.Watch(api.WatchOptions{}); err != nil {
			return Result{BuildResult: first}, fmt.Errorf("bundler: watch: %w", err)
		}
		return Result{BuildResult: first, Ctx: ctx}, nil
	}

	res := api.Build(buildOpts)
	return Result{BuildResult: res}, nil
}

func (o *Options) validate() error {
	if len(o.Entries) == 0 {
		return fmt.Errorf("bundler: at least one entry required")
	}
	if o.Lockfile != nil && o.Store == nil {
		return fmt.Errorf("bundler: Lockfile provided without Store — resolver plugin would have nowhere to read cached blobs")
	}
	return nil
}

func (b *Bundler) applyDefaults(o *Options) {
	if o.OutDir == "" {
		o.OutDir = "islands"
	}
	if o.Target == 0 {
		o.Target = api.ES2022
	}
	if o.Sourcemap == 0 {
		o.Sourcemap = api.SourceMapLinked
	}
}
