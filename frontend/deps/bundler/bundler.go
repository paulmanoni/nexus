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

// vueRuntimeFlagsBanner is prepended to every emitted JS bundle.
// Defines Vue 3 esm-bundler's three compile-time globals at module
// entry, defensive against any code path Define's identifier
// substitution couldn't reach (e.g. a downstream module reading
// globalThis.__VUE_PROD_DEVTOOLS__ instead of the bare identifier).
//
// The typeof check makes it safe to re-run against an already-set
// window — useful when nexus dev hot-rebuilds incrementally and a
// page reload re-evals the banner.
//
// One line so esbuild keeps it on the first source line and stack
// traces from runtime-core stay readable.
const vueRuntimeFlagsBanner = `if(typeof globalThis.__VUE_PROD_DEVTOOLS__==="undefined"){globalThis.__VUE_PROD_DEVTOOLS__=false;globalThis.__VUE_OPTIONS_API__=true;globalThis.__VUE_PROD_HYDRATION_MISMATCH_DETAILS__=false;}`

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

	// TSConfig is the project's tsconfig.json (or jsconfig.json)
	// path. esbuild reads its compilerOptions.paths and honors
	// them during resolution, so the Vite-classic
	//
	//	"paths": { "@/*": ["islands.src/src/*"] }
	//
	// pattern Just Works in nexus dev / nexus build without
	// requiring the operator to rewrite every `@/views/...`
	// import to a relative path.
	//
	// Empty disables tsconfig integration — esbuild falls back
	// to its standard relative-/-absolute-only resolution. The
	// CLI auto-detects ./tsconfig.json in the project root when
	// this is unset, so library callers usually don't set it.
	//
	// Important: esbuild interprets the path field's "baseUrl"
	// relative to the tsconfig location, not the working dir.
	// Pass an absolute path here to avoid surprises in
	// monorepo-style layouts where the bundler runs from a
	// subdirectory.
	TSConfig string
}

// assetLoaders is the default per-extension loader map handed to
// esbuild. Images + fonts go through the file loader (emitted to
// outDir with a content-hashed name, import returns the public URL
// string) so projects can do:
//
//	import flagPNG from "@/assets/flag.png"  // → "/flag-A1B2C3.png"
//	import "@mdi/font/css/icons.css"         // → bundled CSS
//
// Inline-friendly cases (.txt, .json) use loaders that produce JS
// values directly. SVG defaults to `file` rather than `dataurl`
// because most apps use it as <img src> not as inline CSS — easier
// to override per-project later if needed.
//
// Operators wanting different behavior (e.g. inline tiny PNGs as
// data URLs) can override via a future Options.Loaders field; for
// now the defaults cover the common Vite-port case.
var assetLoaders = map[string]api.Loader{
	// Images.
	".png":  api.LoaderFile,
	".jpg":  api.LoaderFile,
	".jpeg": api.LoaderFile,
	".gif":  api.LoaderFile,
	".webp": api.LoaderFile,
	".avif": api.LoaderFile,
	".svg":  api.LoaderFile,
	".ico":  api.LoaderFile,
	// Fonts.
	".woff":  api.LoaderFile,
	".woff2": api.LoaderFile,
	".ttf":   api.LoaderFile,
	".otf":   api.LoaderFile,
	".eot":   api.LoaderFile,
	// Misc.
	".txt":  api.LoaderText,
	".json": api.LoaderJSON,
	// .css is handled natively by esbuild without a loader entry
	// (CSS bundling is built in). Listing it here would override
	// that to "treat as raw text" — DON'T add it.
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
		// esbuild reads compilerOptions.paths from the given
		// tsconfig and applies them during resolution. Empty
		// string disables the integration cleanly.
		Tsconfig: opts.TSConfig,
		// Asset loaders for files imports typically reference
		// from Vue / React code:
		//
		//   import flag from "@/assets/flag.png"  → URL string
		//   import "./styles.css"                 → bundled CSS
		//
		// `file` loader emits the asset into outDir with a
		// content-hashed filename and the import returns the
		// public URL string. `css` loader bundles via esbuild's
		// native CSS pipeline so @import + url() recurse.
		// `text`/`json` cover the smaller cases.
		//
		// Without these, esbuild errors with "No loader is
		// configured for .png files" which the operator can't
		// fix without dropping into nexus internals — exactly
		// the Vite-port friction this is meant to remove.
		Loader: assetLoaders,
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
			"__VUE_PROD_DEVTOOLS__":                   "false",
			"__VUE_OPTIONS_API__":                     "true",
			"__VUE_PROD_HYDRATION_MISMATCH_DETAILS__": "false",
		},
		// Runtime polyfill as a banner — runs at the top of
		// every emitted JS file BEFORE any imports execute, so
		// even if a Vue chunk somehow slipped past Define's
		// identifier substitution (rare; can happen when a
		// downstream module accesses the flag via globalThis
		// instead of as a bare identifier), the globals are
		// already set. Belt + suspenders.
		//
		// Idempotent (typeof check) so re-running the bundle
		// in dev mode against an already-set-up window object
		// is harmless. Cost: ~250 bytes per bundle, trivially
		// shaken away from non-browser builds since they'd
		// access globalThis anyway.
		Banner: map[string]string{
			"js": vueRuntimeFlagsBanner,
		},
	}

	if opts.Watch {
		// Wire an OnEnd plugin so EVERY build (initial + every file-
		// change rebuild) flows through opts.OnRebuild. esbuild's
		// auto-rebuild loop never calls our code back otherwise —
		// the v0.71.3 path only invoked opts.OnRebuild manually for
		// the initial Rebuild(), so file-change rebuilds wrote (or
		// failed to write) silently with no log output. OnEnd is the
		// supported hook for "run after every build, including the
		// ones esbuild triggers itself."
		if opts.OnRebuild != nil {
			cb := opts.OnRebuild
			buildOpts.Plugins = append(buildOpts.Plugins, api.Plugin{
				Name: "nexus-bundler-onend",
				Setup: func(build api.PluginBuild) {
					build.OnEnd(func(result *api.BuildResult) (api.OnEndResult, error) {
						cb(*result)
						return api.OnEndResult{}, nil
					})
				},
			})
		}

		ctx, ctxErr := api.Context(buildOpts)
		// ctxErr is *ContextError — nil-check the pointer before
		// dereferencing the Errors field, otherwise a nil ctxErr
		// (which esbuild returns when api.Context succeeded) would
		// panic with SIGSEGV at the len(ctxErr.Errors) site.
		if ctxErr != nil && len(ctxErr.Errors) > 0 {
			return Result{}, fmt.Errorf("bundler: esbuild context: %s", ctxErr.Errors[0].Text)
		}
		// Defense-in-depth against esbuild API changes — a future
		// version could return (nil, nil) on an error path we
		// don't recognize, and we'd rather surface a clear message
		// than crash inside ctx.Rebuild().
		if ctx == nil {
			return Result{}, fmt.Errorf("bundler: esbuild returned nil watch context")
		}
		// Watch BEFORE Rebuild so esbuild arms its file-change
		// watcher before the initial bundle finishes. Watch does NOT
		// trigger a build itself; Rebuild runs the first one
		// synchronously and primes the dependency graph esbuild
		// watches afterwards.
		if err := ctx.Watch(api.WatchOptions{}); err != nil {
			return Result{}, fmt.Errorf("bundler: watch: %w", err)
		}
		first := ctx.Rebuild()
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
