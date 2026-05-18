// Package resolver implements the esbuild plugin that intercepts
// bare imports in user code and resolves them against the project's
// nexus.lock — pointing esbuild at the corresponding cached blob in
// the disk store rather than letting it walk a non-existent
// node_modules tree.
//
// This is the seam that makes "import 'vue'" work without npm.
// esbuild walks the import graph; whenever it sees a bare specifier
// (anything that doesn't start with ./ ../ / or a known scheme), it
// asks the registered plugins via OnResolve. Our plugin:
//
//  1. Looks up "vue" in the lockfile — finds vue@3.4.21.
//  2. Reads the cached blob path from the store using the entry's
//     Resolved URL.
//  3. Returns that path; esbuild reads the file as if it were any
//     other module.
//
// Sub-path imports ("vue/dist/vue.esm.js") are handled by tracking
// the parent package's Resolved URL and joining the sub-path onto
// it, then looking THAT URL up in the lockfile (or store). v0.1
// requires sub-paths to be pre-fetched by the fetcher; runtime
// fetching during a build is intentionally not supported (would
// turn `nexus build` into a network operation).
//
// Relative imports (./foo) and unknown bare specs fall through to
// esbuild's default resolver, which lets it handle the user's own
// source tree the way it always has.
package resolver

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/evanw/esbuild/pkg/api"

	"github.com/paulmanoni/nexus/frontend/deps/lockfile"
	"github.com/paulmanoni/nexus/frontend/deps/store"
)

// Namespace is the esbuild namespace this plugin tags resolved
// entries with. Distinguishes our cached blobs from regular
// on-disk files in subsequent OnLoad events. v0.1 doesn't register
// any OnLoad — esbuild's default Loader inference from file
// extension is good enough for ESM blobs from esm.sh — but the
// namespace is reserved so future per-content-type handling (CSS
// injection, source map fix-up) has a stable hook to anchor on.
const Namespace = "nexus-deps"

// Options configures the plugin. Both fields are required.
type Options struct {
	Lockfile *lockfile.File
	Store    *store.Store
}

// New returns an esbuild plugin that wires OnResolve for every
// import esbuild encounters. Pass into Bundler.AddPlugin before
// any other plugin that also handles OnResolve.
func New(opts Options) (api.Plugin, error) {
	if opts.Lockfile == nil {
		return api.Plugin{}, errors.New("resolver: Lockfile is nil")
	}
	if opts.Store == nil {
		return api.Plugin{}, errors.New("resolver: Store is nil")
	}
	// Build the blob-path → URL reverse map once at plugin
	// construction time. Used by relative / absolute imports
	// nested INSIDE registry-served bodies (esm.sh internal
	// paths like /node/buffer.mjs) — we need to know what URL
	// the importing file came from to resolve such paths.
	blobToURL := buildBlobIndex(opts.Store)

	return api.Plugin{
		Name: "nexus-deps-resolver",
		Setup: func(build api.PluginBuild) {
			// OnResolve fires for every import esbuild
			// encounters. We narrow with a permissive filter
			// and decide internally whether to claim or pass
			// through — gives us one place to reason about
			// precedence.
			build.OnResolve(api.OnResolveOptions{Filter: ".*"}, func(args api.OnResolveArgs) (api.OnResolveResult, error) {
				return resolveOne(opts, blobToURL, args)
			})
			// OnLoad fires for every path we tagged with our
			// Namespace in OnResolve. esbuild can't infer the
			// loader from the path (cached blobs have no
			// extension — they're named after the content
			// hash), so we read the file ourselves and tell
			// esbuild what kind of content it is via the
			// Loader field.
			build.OnLoad(api.OnLoadOptions{
				Filter:    ".*",
				Namespace: Namespace,
			}, func(args api.OnLoadArgs) (api.OnLoadResult, error) {
				return loadOne(opts, args)
			})
		},
	}, nil
}

// resolveOne is the core of the plugin. Implementation factored
// out as a non-method function so the tests can drive it directly
// without standing up an esbuild build context.
//
// Return semantics, per esbuild's plugin docs:
//
//   - {Path: "..."} → we claim this import; esbuild reads that
//     path
//   - {} (zero) → pass through to esbuild's default resolver
//   - {Errors: [...]} → fail the build with our diagnostic
//
// Decision tree:
//
//  1. Importer is one of our cached blobs (args.Namespace ==
//     Namespace OR args.Importer matches a known cache path) →
//     all imports are CDN-internal; resolve against the importer's
//     URL and look up in the store. This is the "registry-served
//     ESM transitively references siblings" case.
//  2. Importer is user code → relative/absolute paths fall
//     through to esbuild's default; bare specs go through the
//     lockfile.
func resolveOne(opts Options, blobToURL map[string]string, args api.OnResolveArgs) (api.OnResolveResult, error) {
	p := args.Path

	// --- registry-internal resolution path ---------------------------
	// When the importer file is one of our cached blobs, EVERY
	// import refers to a sibling in the registry. We look up the
	// importer's original URL via the reverse map and resolve the
	// import against it. The check is purely path-based — we don't
	// depend on esbuild's args.Namespace because esbuild doesn't
	// always propagate the namespace for re-entrant resolves of
	// CDN-internal paths.
	if importerURL, ok := blobToURL[args.Importer]; ok {
		absURL, err := resolveRegistryURL(importerURL, p)
		if err == nil && absURL != "" {
			if blob, _, gerr := opts.Store.Get(absURL); gerr == nil {
				return api.OnResolveResult{Path: blob, Namespace: Namespace}, nil
			}
		}
		// If the lookup failed and the import is registry-internal-
		// shaped (absolute path, relative path, or absolute URL),
		// surface a clear "missing in cache" error rather than
		// letting esbuild try a useless filesystem lookup.
		if strings.HasPrefix(p, "/") || strings.HasPrefix(p, ".") || strings.Contains(p, "://") {
			return api.OnResolveResult{
				Errors: []api.Message{{
					Text: fmt.Sprintf("nexus-deps: %s (imported from %s → %s) is not in the cache — run `nexus install`",
						p, importerURL, absURL),
				}},
			}, nil
		}
		// Bare spec — fall through so the lockfile branch below
		// has a chance.
	}

	// Relative imports always belong to esbuild's default
	// resolver — user code referencing user code.
	if strings.HasPrefix(p, "./") || strings.HasPrefix(p, "../") || p == "." || p == ".." {
		return api.OnResolveResult{}, nil
	}
	// Absolute paths are esbuild's job too (rare in user code,
	// common when esbuild re-enters with a fully-resolved path).
	if strings.HasPrefix(p, "/") {
		return api.OnResolveResult{}, nil
	}
	// Protocol-prefixed paths (http://, data:, etc.) — pass.
	if strings.Contains(p, "://") || strings.HasPrefix(p, "data:") {
		return api.OnResolveResult{}, nil
	}

	// Split off any sub-path: "vue/dist/vue.esm.js" → ("vue",
	// "dist/vue.esm.js"). Scoped packages start with "@" and
	// their first segment is part of the spec ("@vue/runtime-dom"
	// is the spec, "@vue/runtime-dom/foo.js" splits as
	// ("@vue/runtime-dom", "foo.js")).
	spec, subpath := splitSpec(p)

	pkg, err := opts.Lockfile.Resolve(spec, "")
	if err != nil {
		if errors.Is(err, lockfile.ErrNotResolved) {
			// Not in our lockfile — let esbuild's default
			// resolver have a go. If the user wrote a typo
			// it'll surface as a normal "could not resolve"
			// error from esbuild itself.
			return api.OnResolveResult{}, nil
		}
		var ae *lockfile.AmbiguousError
		if errors.As(err, &ae) {
			return api.OnResolveResult{
				Errors: []api.Message{{Text: ae.Error()}},
			}, nil
		}
		return api.OnResolveResult{}, fmt.Errorf("resolver: lockfile lookup for %q: %w", spec, err)
	}

	// Resolve to a cached blob path. For root-package imports
	// the entry's Resolved URL is what we hand to the store.
	// For sub-path imports we join the sub-path against the
	// resolved URL and look THAT up — the fetcher should have
	// recursed into it at `nexus add` time.
	targetURL := pkg.Resolved
	if subpath != "" {
		targetURL = joinSubpath(pkg.Resolved, subpath)
	}
	blob, meta, err := opts.Store.Get(targetURL)
	if err != nil {
		if errors.Is(err, store.ErrNotCached) {
			return api.OnResolveResult{
				Errors: []api.Message{{
					Text: fmt.Sprintf("nexus-deps: %s resolves to %s but no cached blob exists — run `nexus install` to populate the cache",
						spec, targetURL),
				}},
			}, nil
		}
		return api.OnResolveResult{}, fmt.Errorf("resolver: store lookup for %s: %w", targetURL, err)
	}
	_ = meta // future: dispatch on meta.ContentType (CSS, etc.)

	return api.OnResolveResult{
		Path:      blob,
		Namespace: Namespace,
	}, nil
}

// loadOne reads the cached blob from disk and tells esbuild what
// kind of content it is. The Loader is inferred from Content-Type
// stored in the lockfile entry (which mirrors what the registry
// served when the fetcher first downloaded it):
//
//	application/javascript   →  LoaderJS
//	text/css                 →  LoaderCSS
//	application/json         →  LoaderJSON
//	(else)                   →  LoaderDefault (esbuild's fallback)
//
// We re-scan the lockfile by Path-→URL because esbuild's OnLoadArgs
// doesn't carry plugin-specific data through from OnResolve; the
// cleanest reverse-lookup is by store blob path → URL via the
// EachURL iterator. For typical projects (~30 deps) that's a
// linear scan over a tiny map. Optimizing later via a cached
// reverse-index is straightforward if it ever shows up in profiles.
func loadOne(opts Options, args api.OnLoadArgs) (api.OnLoadResult, error) {
	body, err := os.ReadFile(args.Path)
	if err != nil {
		return api.OnLoadResult{}, fmt.Errorf("resolver: read cached blob %s: %w", args.Path, err)
	}
	contents := string(body)
	loader := api.LoaderJS // safe default — esbuild handles bad
	// JS as syntax errors, which surfaces a clearer message than
	// LoaderDefault would (which falls through to file copy).

	// Look up the Content-Type that was recorded when the fetcher
	// stored this blob. Reverse-mapping path → URL via EachURL is
	// O(N_deps); acceptable at v0.1 dep counts.
	_ = opts.Store.EachURL(func(meta store.Metadata) error {
		// The path returned by OnResolve is the blob path which
		// the store yields the same way. Cheap string compare.
		if meta.ContentSHA256 != "" {
			// Each entry maps URL → SHA256 → blob. We don't have
			// the SHA256 in args.Path directly, but the blob path
			// embeds it: "<root>/cas/<aa>/<bbbb...>". Extract.
			parent, base := splitBlobPath(args.Path)
			_ = parent
			if base == meta.ContentSHA256 {
				loader = loaderFromContentType(meta.ContentType)
				return errors.New("found") // short-circuit
			}
		}
		return nil
	})

	return api.OnLoadResult{
		Contents: &contents,
		Loader:   loader,
	}, nil
}

// loaderFromContentType maps an HTTP Content-Type header to the
// esbuild Loader. We accept the common subset esm.sh serves.
// Unknown types fall back to LoaderJS — most blobs in our store
// are JS, and esbuild's loader inference for an extensionless path
// has no better choice anyway.
func loaderFromContentType(ct string) api.Loader {
	ct = strings.ToLower(ct)
	switch {
	case strings.Contains(ct, "javascript"),
		strings.Contains(ct, "ecmascript"),
		strings.Contains(ct, "typescript"):
		return api.LoaderJS
	case strings.Contains(ct, "css"):
		return api.LoaderCSS
	case strings.Contains(ct, "json"):
		return api.LoaderJSON
	}
	return api.LoaderJS
}

// buildBlobIndex walks every URL → blob mapping in the store and
// returns a path → URL reverse map. Used by the OnResolve handler
// to figure out which registry URL an importer file came from when
// chasing CDN-internal imports.
//
// Built once per plugin construction. Cost is O(N_urls) where
// N_urls is bounded by the lockfile size — typically dozens to
// low hundreds, so the upfront walk is sub-millisecond.
//
// Two subtleties:
//   - The same blob can map to multiple URLs (a bare URL and its
//     resolved-version URL pointing at the same content). We pick
//     the most-resolved URL (longest one) on the theory that it
//     carries the most version info, which makes resolveRegistryURL
//     produce more-specific output.
//   - Missing meta or store-walk errors aren't fatal here — the
//     resolver degrades gracefully to bare-spec-only lookups.
func buildBlobIndex(s *store.Store) map[string]string {
	out := map[string]string{}
	_ = s.EachURL(func(meta store.Metadata) error {
		if meta.ContentSHA256 == "" {
			return nil
		}
		blobPath := blobPathFor(s.Root(), meta.ContentSHA256)
		preferred := meta.ResolvedURL
		if preferred == "" {
			preferred = meta.URL
		}
		// Prefer the longer (more-resolved) URL on collision.
		if existing, ok := out[blobPath]; !ok || len(preferred) > len(existing) {
			out[blobPath] = preferred
		}
		return nil
	})
	return out
}

// blobPathFor reproduces the cas-layout the store uses internally:
// "<root>/cas/<aa>/<bbbb...>". Kept in sync with store.casPath; we
// can't call into the store's unexported helper, so duplicate the
// trivial path math here.
func blobPathFor(root, hash string) string {
	if len(hash) < 2 {
		return ""
	}
	return root + string(os.PathSeparator) + "cas" + string(os.PathSeparator) + hash[:2] + string(os.PathSeparator) + hash[2:]
}

// isCachedBlobPath reports whether p sits under the store's cas
// directory. Used as a secondary signal for the registry-internal
// resolution branch — esbuild doesn't always populate
// args.Namespace on transitive resolves of imports inside files
// our plugin claimed, so we check the path shape too.
func isCachedBlobPath(root, p string) bool {
	if p == "" || root == "" {
		return false
	}
	return strings.HasPrefix(p, root+string(os.PathSeparator)+"cas"+string(os.PathSeparator))
}

// resolveRegistryURL joins an import specifier (which may be
// relative, absolute-path, or absolute-URL) against the importer's
// origin URL. The result is an absolute URL we can look up in the
// store.
//
//	importer = https://esm.sh/x@1?target=es2015
//	imp      = ./y.mjs           → https://esm.sh/y.mjs?target=es2015
//	imp      = /node/buffer.mjs  → https://esm.sh/node/buffer.mjs?target=es2015
//	imp      = https://x/y       → https://x/y (unchanged)
//	imp      = bare-name         → ""  (not registry-internal)
//
// The importer's QUERY STRING is preserved on the result when the
// import is a relative or absolute-path reference. esm.sh keys its
// content variants on query (`?target=es2015`, `?bundle`, etc.),
// and our fetcher stored sibling files with the same query — the
// resolver has to use the same key shape or every cross-file
// import inside a lowered bundle fails to find its peer.
func resolveRegistryURL(importerURL, imp string) (string, error) {
	if strings.HasPrefix(imp, "http://") || strings.HasPrefix(imp, "https://") {
		return imp, nil
	}
	if strings.HasPrefix(imp, ".") || strings.HasPrefix(imp, "/") {
		base, err := url.Parse(importerURL)
		if err != nil {
			return "", err
		}
		rel, err := url.Parse(imp)
		if err != nil {
			return "", err
		}
		joined := base.ResolveReference(rel)
		// RFC 3986 §5.2.2 says the merged target inherits NOTHING
		// from the base's query — only scheme/host/path. We
		// reattach the base query when the import didn't carry
		// one of its own, since registry-internal siblings live
		// under the same content variant.
		if joined.RawQuery == "" && base.RawQuery != "" {
			joined.RawQuery = base.RawQuery
		}
		return joined.String(), nil
	}
	// Bare spec — caller already had a chance via the lockfile
	// path. Returning "" signals "not a registry-internal import".
	return "", errors.New("bare spec — not a registry-internal import")
}

// splitBlobPath extracts the sha256 from a store blob path. The
// store lays blobs out as "<root>/cas/<aa>/<bbbb...>"; we glue the
// last two path components back together to recover the hash. If
// the path doesn't match this shape (programming error, or
// resolver called on something else), returns ("", "").
func splitBlobPath(p string) (parent, hash string) {
	parts := strings.Split(p, string(os.PathSeparator))
	if len(parts) < 2 {
		return "", ""
	}
	shard := parts[len(parts)-2]
	rest := parts[len(parts)-1]
	if len(shard) != 2 {
		return "", ""
	}
	return strings.Join(parts[:len(parts)-2], string(os.PathSeparator)), shard + rest
}

// splitSpec separates a bare import path into (package, subpath).
//
//	"vue"                          → ("vue", "")
//	"vue/dist/vue.esm.js"          → ("vue", "dist/vue.esm.js")
//	"@vue/runtime-dom"             → ("@vue/runtime-dom", "")
//	"@vue/runtime-dom/foo/bar.js"  → ("@vue/runtime-dom", "foo/bar.js")
//
// Scoped packages reserve the first TWO path segments for the
// spec; unscoped reserve only the first.
func splitSpec(path string) (spec, subpath string) {
	if strings.HasPrefix(path, "@") {
		// Scoped: take @scope/name as the spec.
		parts := strings.SplitN(path, "/", 3)
		if len(parts) < 2 {
			return path, ""
		}
		spec = parts[0] + "/" + parts[1]
		if len(parts) == 3 {
			subpath = parts[2]
		}
		return
	}
	// Unscoped: first segment is the spec.
	if i := strings.Index(path, "/"); i >= 0 {
		return path[:i], path[i+1:]
	}
	return path, ""
}

// joinSubpath appends a sub-path to a package's resolved URL.
// Used to convert "vue/dist/vue.esm.js" + the lockfile's
// "https://esm.sh/vue@3.4.21" into "https://esm.sh/vue@3.4.21/dist/vue.esm.js".
//
// The esm.sh URL form for sub-paths is exactly this concatenation,
// which is why our fetcher's recursive traversal would have already
// fetched these sub-blobs at `nexus add` time.
func joinSubpath(resolvedURL, sub string) string {
	resolvedURL = strings.TrimRight(resolvedURL, "/")
	sub = strings.TrimLeft(sub, "/")
	return resolvedURL + "/" + sub
}
