package vue

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/evanw/esbuild/pkg/api"

	"github.com/paulmanoni/nexus/frontend/deps/fetcher"
	"github.com/paulmanoni/nexus/frontend/deps/lockfile"
	"github.com/paulmanoni/nexus/frontend/deps/resolver"
	"github.com/paulmanoni/nexus/frontend/deps/store"
)

// DefaultCompilerVersion is the @vue/compiler-sfc version Bootstrap
// pins by default. Bumped per release; users override via
// BootstrapOptions.Version. We pin a known-good version rather than
// resolving "latest" so a fresh clone produces deterministic builds
// matching whatever the project's `nexus.lock` says.
const DefaultCompilerVersion = "3.4.21"

// adapterJS is the JavaScript bridge between Go's compile.go and the
// real @vue/compiler-sfc package. Embedded so the binary is fully
// self-contained: a fresh `nexus build` doesn't need a network hop
// to fetch the adapter source. The compiler bundle itself is still
// fetched once + cached.
//
//go:embed adapter.js
var adapterJS string

// BootstrapOptions configures the one-time compiler-bundle build.
type BootstrapOptions struct {
	// Store is the disk cache the fetcher + resolver share.
	// Required.
	Store *store.Store

	// Fetcher pulls @vue/compiler-sfc from the registry the user
	// configured (NEXUS_REGISTRY). Required.
	Fetcher *fetcher.Fetcher

	// Version is the @vue/compiler-sfc version to pin. Defaults
	// to DefaultCompilerVersion. Pass the exact "3.4.21" shape;
	// no `^` or `~` range support in v0.1.
	Version string

	// BundleDir is the directory the produced compiler bundle is
	// cached in. Defaults to filepath.Join(store.Root(),
	// "sfc-vue") so the bundle sits alongside the rest of the
	// content-addressed cache. Tests pass a t.TempDir().
	BundleDir string
}

// Bootstrap materializes the real @vue/compiler-sfc into a single
// self-contained JS bundle the Goja runtime can load. Performs the
// network fetch + esbuild bundle the first time it's called for a
// given version; subsequent calls for the same version short-circuit
// to the cached bundle.
//
// Lifecycle:
//
//  1. Check <BundleDir>/<version>/compiler.bundle.js — if present,
//     read + return its bytes.
//  2. Fetch @vue/compiler-sfc@<version> via the supplied Fetcher
//     (recursive — populates the store with everything compiler-sfc
//     transitively imports).
//  3. Build an in-memory lockfile from the fetch result.
//  4. Synthesize an esbuild Build with the embedded adapter.js as
//     stdin + the resolver plugin pointing at the in-memory
//     lockfile.
//  5. esbuild bundles everything into one IIFE; write to disk;
//     return bytes.
//
// Returns (bundleBytes, version) so callers can immediately
// construct a Compiler.
//
// First call typically takes 1-5 seconds (network + bundle); cached
// calls return in microseconds.
func Bootstrap(ctx context.Context, opts BootstrapOptions) ([]byte, error) {
	if opts.Store == nil {
		return nil, errors.New("vue: Bootstrap requires Store")
	}
	if opts.Fetcher == nil {
		return nil, errors.New("vue: Bootstrap requires Fetcher")
	}
	version := opts.Version
	if version == "" {
		version = DefaultCompilerVersion
	}
	bundleDir := opts.BundleDir
	if bundleDir == "" {
		bundleDir = filepath.Join(opts.Store.Root(), "sfc-vue")
	}
	cachedPath := filepath.Join(bundleDir, version, "compiler.bundle.js")

	// Cache hit fast path.
	if b, err := os.ReadFile(cachedPath); err == nil && len(b) > 0 {
		return b, nil
	}

	// Cache miss: fetch + bundle.
	bundle, err := buildCompilerBundle(ctx, opts.Fetcher, opts.Store, version)
	if err != nil {
		return nil, fmt.Errorf("vue: bootstrap: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(cachedPath), 0o755); err != nil {
		return nil, fmt.Errorf("vue: mkdir bundle dir: %w", err)
	}
	if err := os.WriteFile(cachedPath, bundle, 0o644); err != nil {
		return nil, fmt.Errorf("vue: write cached bundle: %w", err)
	}
	return bundle, nil
}

// buildCompilerBundle does the actual fetch + bundle work. Split
// from Bootstrap so the cache-hit branch is trivially obvious in
// the public function.
//
// Critically, we don't reuse the caller's fetcher as-is — we
// rebuild one with URLQuery="target=es2015" so esm.sh serves
// pre-lowered code that Goja can run. Without this every
// transitive @vue/compiler-sfc dep ships unminified async
// generators, which Goja can't parse and esbuild can't transform
// away from ES2015 target (its own limitation).
func buildCompilerBundle(ctx context.Context, f *fetcher.Fetcher, s *store.Store, version string) ([]byte, error) {
	// Shallow-clone the caller's fetcher, then turn on the ES2015
	// query knob. Keeps the caller's Registry + HTTP settings.
	lowered := &fetcher.Fetcher{
		Registry: f.Registry,
		Store:    f.Store,
		HTTP:     f.HTTP,
		URLQuery: "target=es2015",
	}
	spec := "@vue/compiler-sfc@" + version

	// 1. Fetch + recurse. Populates the store with every file
	//    compiler-sfc transitively imports — all with
	//    ?target=es2015 so the served code is Goja-compatible.
	res, err := lowered.Fetch(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", spec, err)
	}

	// 2. Build an in-memory lockfile from the fetch result. The
	//    resolver plugin reads it during the bundle to translate
	//    bare imports → cached blob paths.
	lf := lockfile.New()
	lf.Add(res.Root)
	for _, dep := range res.Transitive {
		lf.Add(dep)
	}

	// 3. Wire the resolver plugin.
	plugin, err := resolver.New(resolver.Options{Lockfile: lf, Store: s})
	if err != nil {
		return nil, fmt.Errorf("build resolver: %w", err)
	}

	// 4. esbuild the adapter.
	//
	// Stdin feeds the adapter source directly without a temp file.
	// ResolveDir + Sourcefile let esbuild attribute errors to a
	// readable filename if the user ever sees a diagnostic.
	//
	// Format=IIFE so the resulting bundle is a self-contained
	// expression Goja can evaluate to side-effect globalThis. ESM
	// would also work but requires module-loader plumbing we'd
	// rather skip.
	cwd, _ := os.Getwd()
	result := api.Build(api.BuildOptions{
		Stdin: &api.StdinOptions{
			Contents:   adapterJS,
			ResolveDir: cwd,
			Sourcefile: "nexus-vue-adapter.js",
			Loader:     api.LoaderJS,
		},
		Bundle: true,
		Write:  false,
		Format: api.FormatIIFE,
		// Target ES2015 so esbuild down-levels every newer
		// feature (async generators, top-level await, optional
		// chaining, etc.) into ES5-Promise-compatible code Goja
		// can run. Goja supports ES5.1 + most of ES6's non-async
		// features; targeting ES2015 with esbuild's transforms
		// hits that sweet spot. Without this, the Vue compiler
		// bundle has async generators that Goja parser rejects:
		// "Async generators are not supported yet".
		Target:   api.ES2015,
		Plugins:  []api.Plugin{plugin},
		LogLevel: api.LogLevelSilent,
		// MinifyWhitespace cuts ~30% off bundle size without
		// harming Goja parse time. Skip MinifyIdentifiers /
		// MinifySyntax so any in-bundle stack trace from Vue
		// stays readable for diagnosis.
		MinifyWhitespace: true,
		// Inject standard polyfills the Vue compiler expects to
		// find on the global object. process.env.NODE_ENV is
		// what flips Vue's dev-vs-prod branches; define it to
		// "production" so we get the smaller, faster paths.
		Define: map[string]string{
			"process.env.NODE_ENV": `"production"`,
		},
		// We rely on esm.sh's ?target=es2015 query to pre-lower
		// the served code (see buildCompilerBundle's `lowered`
		// fetcher). esbuild itself just bundles + minifies; no
		// further Supported overrides needed because the input
		// is already ES2015-clean.
	})
	if len(result.Errors) > 0 {
		return nil, fmt.Errorf("esbuild: %s", result.Errors[0].Text)
	}
	if len(result.OutputFiles) == 0 {
		return nil, errors.New("esbuild produced no output")
	}
	return result.OutputFiles[0].Contents, nil
}
