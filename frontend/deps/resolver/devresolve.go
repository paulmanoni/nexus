package resolver

import (
	"errors"
	"fmt"
	"strings"

	"github.com/paulmanoni/nexus/frontend/deps/lockfile"
)

// ResolveURL maps an import specifier to the canonical store URL its bytes
// are cached under — the same decision tree as the esbuild plugin's
// resolveOne, but returning a plain URL instead of an api.OnResolveResult.
//
// The unbundled dev server (frontend/devserver) uses this so it resolves
// bare and registry-internal imports IDENTICALLY to `nexus build`: one
// code path, one set of helpers (splitSpec / joinSubpath / DevSpecRewrite /
// resolveRegistryURL / FetchOnDemand). It then wraps the returned URL as a
// browser-fetchable /@dep/ path.
//
// importerURL is the real registry URL of the importing module when that
// module is itself a cached dep blob (so a relative/absolute import inside
// it resolves against its registry siblings). Pass "" when the importer is
// user source — then only bare specs resolve here and relative/alias
// imports are left to the caller (filesystem).
//
// Returns:
//
//   - (url, true,  nil) — resolved to a cached (or just-fetched) blob URL
//   - ("",  false, nil) — not the resolver's concern; the caller handles it
//     (relative/alias/user code → its own source tree)
//   - ("",  false, err) — a hard error (ambiguous version, lockfile fault)
func (o Options) ResolveURL(spec, importerURL string) (string, bool, error) {
	if o.Lockfile == nil || o.Store == nil {
		return "", false, errors.New("resolver: ResolveURL needs Lockfile + Store")
	}
	p := spec

	// --- registry-internal resolution -------------------------------
	// The importer is one of our cached dep blobs, so every relative /
	// absolute-path / absolute-URL import refers to a registry sibling.
	// Resolve against the importer URL and look the result up in the
	// store (two-attempt: inherited-query, then query-stripped), with an
	// on-demand fetch as the cold-miss fallback. Mirrors resolveOne's
	// namespace branch.
	if importerURL != "" {
		if absURL, err := resolveRegistryURL(importerURL, p); err == nil && absURL != "" {
			if u, ok := o.lookupOrFetch(absURL); ok {
				return u, true, nil
			}
		}
		// A bare spec inside a dep blob falls through to the lockfile
		// branch below (same as resolveOne).
	}

	// Relative / absolute-path / protocol imports from USER code are not
	// the resolver's job — the dev server resolves those against the
	// project's own source tree (or leaves them to the browser).
	if importerURL == "" {
		switch {
		case strings.HasPrefix(p, "./"), strings.HasPrefix(p, "../"), p == ".", p == "..":
			return "", false, nil
		case strings.HasPrefix(p, "/"):
			return "", false, nil
		case strings.Contains(p, "://"), strings.HasPrefix(p, "data:"):
			return "", false, nil
		}
	}

	spec, subpath := splitSpec(p)

	// Dev-mode spec rewrite (vue → vue.development.mjs). Consulted before
	// the lockfile; on any miss we fall through so a bad rewrite never
	// breaks resolution.
	if o.DevSpecRewrite != nil {
		if devURL := o.DevSpecRewrite(spec, subpath); devURL != "" {
			if u, ok := o.lookupOrFetch(devURL); ok {
				return u, true, nil
			}
		}
	}

	pkg, err := o.Lockfile.Resolve(spec, "")
	if err != nil {
		if errors.Is(err, lockfile.ErrNotResolved) {
			// Not in the lockfile, but it may be a valid esm.sh path the
			// install-time walk didn't reach. Skip tsconfig-style aliases
			// (@/, ~/, #…) — esm.sh would 400 those; the caller resolves
			// them against the source tree.
			if o.FetchOnDemand != nil && looksLikePackageImport(p) {
				if u, ok := o.lookupOrFetch(p); ok {
					return u, true, nil
				}
			}
			return "", false, nil
		}
		var ae *lockfile.AmbiguousError
		if errors.As(err, &ae) {
			return "", false, ae
		}
		return "", false, fmt.Errorf("resolver: lockfile lookup for %q: %w", spec, err)
	}

	targetURL := pkg.Resolved
	if subpath != "" {
		targetURL = joinSubpath(pkg.Resolved, subpath)
	}
	if u, ok := o.lookupOrFetch(targetURL); ok {
		return u, true, nil
	}
	return "", false, fmt.Errorf("resolver: %s resolves to %s but no cached blob exists — run `nexus install`", spec, targetURL)
}

// lookupOrFetch returns the cache key a URL's bytes live under. It tries
// the URL as-is, then its query-stripped form (esm.sh path-encodes sibling
// variants without a query), then — when FetchOnDemand is wired — pulls the
// URL and retries at the post-redirect canonical key. Returns ("", false)
// when the bytes can't be located or fetched.
//
// Unlike resolveOne's hot path (which keeps fetching off the parallel
// build to avoid the lockfile write race), ResolveURL runs on the dev
// server's single-flight request goroutines, so an on-demand fetch here is
// safe.
func (o Options) lookupOrFetch(url string) (string, bool) {
	if _, _, err := o.Store.Get(url); err == nil {
		return url, true
	}
	if stripped := stripQuery(url); stripped != url {
		if _, _, err := o.Store.Get(stripped); err == nil {
			return stripped, true
		}
	}
	if o.FetchOnDemand != nil {
		if canonical, err := o.FetchOnDemand(url); err == nil {
			key := url
			if canonical != "" {
				key = canonical
			}
			if _, _, err := o.Store.Get(key); err == nil {
				return key, true
			}
		}
	}
	return "", false
}
