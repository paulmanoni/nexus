// Package fetcher pulls ESM modules from a configurable registry
// (default: https://esm.sh) into the local store, following the
// transitive import graph so a single Fetch("vue") resolves vue
// plus every module vue imports.
//
// Version pinning happens via HTTP redirects: a request to
// `https://esm.sh/vue` is answered with a 302 to
// `https://esm.sh/vue@3.4.21`, which is the URL we record in the
// lockfile. Subsequent installs hit the resolved URL directly so a
// week-later "nexus install" doesn't drift onto vue@3.4.22.
//
// All HTTP I/O goes through an injected http.Client so tests can
// drive the fetcher against httptest.NewServer fixtures rather
// than hitting the real registry.
package fetcher

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/paulmanoni/nexus/frontend/deps/lockfile"
	"github.com/paulmanoni/nexus/frontend/deps/store"
)

// DefaultRegistry is the canonical esm.sh URL — the v0.1 default.
// Override via Fetcher.Registry or NEXUS_REGISTRY at the CLI level.
const DefaultRegistry = "https://esm.sh"

// Fetcher is the HTTP client + store glue. Build one per process
// (cheap; carries no per-call state besides the http.Client which
// has its own pooling).
type Fetcher struct {
	// Registry is the base URL prepended to bare specs. Must NOT
	// have a trailing slash — we append "/" + spec ourselves.
	Registry string

	// Store is where fetched bytes land. Required.
	Store *store.Store

	// HTTP is the client used for all GETs. Defaults to
	// http.DefaultClient if zero; tests inject a client whose
	// Transport points to httptest.NewServer.
	//
	// Important: the client's CheckRedirect MUST allow at least
	// one redirect (the version-pin hop). The default policy
	// (10 hops max, no rewrite of method) is fine.
	HTTP *http.Client

	// URLQuery, when set, is appended to every fetched URL as a
	// query string. esm.sh in particular honors `?target=es2015`
	// to serve pre-lowered code suitable for older JS engines
	// (Goja's case). For the bundler's normal user-code path the
	// default empty value is correct — modern browsers eat the
	// default ES2022-ish output esm.sh serves.
	URLQuery string
}

// New returns a Fetcher with sensible defaults. The store argument
// is required; pass nil for registry to get DefaultRegistry.
func New(s *store.Store, registry string) *Fetcher {
	if registry == "" {
		registry = DefaultRegistry
	}
	registry = strings.TrimRight(registry, "/")
	return &Fetcher{
		Registry: registry,
		Store:    s,
		HTTP:     http.DefaultClient,
	}
}

// Result is what Fetch returns: a single resolved package plus the
// set of additional packages found via transitive recursion. The
// caller (typically `nexus add`) writes them all into the lockfile.
//
// Root is the entry the caller asked for. Transitive is keyed by
// the lockfile Key("spec","version") format and may be empty when
// nothing imported beyond the root.
type Result struct {
	Root        lockfile.Package
	Transitive  map[string]lockfile.Package
}

// Fetch resolves `spec` (e.g. "vue", "vue@3.4.21", "@vue/runtime-dom")
// against the registry, downloads the bytes, hashes them, stashes in
// the store, parses imports, and recurses. The returned Result
// carries every package fetched along the way.
//
// Already-cached blobs are NOT re-downloaded — Fetch reads them
// straight from the store. Already-cached URLs with KNOWN integrity
// (lockfile entry) get their hash verified against the cached blob
// to catch tampering before the resolver hands them to esbuild;
// that's wired up in the CLI layer, not here, since the fetcher
// doesn't see the lockfile by design (separation of concerns).
func (f *Fetcher) Fetch(ctx context.Context, spec string) (Result, error) {
	visited := map[string]lockfile.Package{}
	root, err := f.fetchOne(ctx, spec, visited)
	if err != nil {
		return Result{}, err
	}
	// The root is in visited too; strip it from Transitive so the
	// caller has a clean (Root, Transitive) split.
	delete(visited, lockfile.Key(root.Spec, root.Version))
	return Result{Root: root, Transitive: visited}, nil
}

// fetchOne is the recursive worker. visited is the dedup map across
// the whole traversal — keyed by resolved URL so a diamond import
// (A imports B, A imports C, B imports C) only fetches C once.
//
// We dedupe by RESOLVED URL rather than by spec because two
// different specs ("vue" and "vue@3.4.21") can resolve to the same
// final URL after redirect-pinning; storing them separately in
// `visited` would cause double-fetches.
func (f *Fetcher) fetchOne(ctx context.Context, spec string, visited map[string]lockfile.Package) (lockfile.Package, error) {
	reqURL, err := f.specToURL(spec)
	if err != nil {
		return lockfile.Package{}, err
	}

	// HEAD request first to get the redirect-pinned URL without
	// downloading the body. esm.sh + ETag-aware registries are
	// fast for HEAD; we skip a body transfer when the URL is
	// already in the cache.
	resolved, contentType, etag, err := f.resolve(ctx, reqURL)
	if err != nil {
		return lockfile.Package{}, err
	}

	// Dedup by resolved URL across the whole recursion.
	for _, p := range visited {
		if p.Resolved == resolved {
			return p, nil
		}
	}

	// Already in the store?
	body, meta, gotErr := f.Store.Get(resolved)
	if gotErr != nil && !errors.Is(gotErr, store.ErrNotCached) {
		return lockfile.Package{}, fmt.Errorf("fetcher: read cache for %s: %w", resolved, gotErr)
	}
	var content []byte
	var hash string
	if gotErr == nil {
		// Cache hit — read the blob bytes (we need the body for
		// import-recursion).
		content, err = readFile(body)
		if err != nil {
			return lockfile.Package{}, fmt.Errorf("fetcher: read cached blob %s: %w", resolved, err)
		}
		hash = meta.ContentSHA256
		if contentType == "" {
			contentType = meta.ContentType
		}
	} else {
		// Cache miss — pull the body.
		content, err = f.get(ctx, resolved)
		if err != nil {
			return lockfile.Package{}, err
		}
		path, putErr := f.Store.Put(resolved, bytesReader(content), "", store.Metadata{
			URL:         reqURL,
			ResolvedURL: resolved,
			ContentType: contentType,
			ETag:        etag,
		})
		if putErr != nil {
			return lockfile.Package{}, fmt.Errorf("fetcher: store %s: %w", resolved, putErr)
		}
		_, meta, _ = f.Store.Get(resolved)
		_ = path
		hash = meta.ContentSHA256
	}

	pkgSpec, pkgVersion := parseSpec(spec)
	if v := extractVersionFromURL(resolved); v != "" {
		pkgVersion = v
	}
	pkg := lockfile.Package{
		Spec:        pkgSpec,
		Version:     pkgVersion,
		Resolved:    resolved,
		Integrity:   "sha256-" + hash,
		ContentType: contentType,
	}
	// Record now so concurrent recursion paths find this entry.
	visited[lockfile.Key(pkg.Spec, pkg.Version)] = pkg

	// Recurse into imports. Only JS-shape responses are scanned
	// — CSS / source maps / data files have no imports we care
	// about.
	if isJSContent(contentType, resolved) {
		for _, imp := range ExtractImports(string(content)) {
			// Resolve the import relative to the resolved URL so
			// CDN-internal paths (e.g. /v135/@vue/x/foo.js) chase
			// correctly.
			childSpec, err := resolveAgainst(resolved, imp)
			if err != nil {
				// Unresolvable imports are non-fatal — esbuild's
				// own resolver might still handle them (e.g. a
				// data URI), or the user will see a clearer
				// error at bundle time.
				continue
			}
			// Bare specs ("shared", "@vue/runtime-dom") inside a
			// registry-served body refer to OTHER packages in the
			// same registry — esm.sh-served modules link to peer
			// modules via bare specs, expecting the resolver to
			// look them up. The fetcher does that lookup eagerly
			// by recursing with the bare spec, which specToURL
			// expands against f.Registry. Relative/absolute URLs
			// recurse via the resolved URL as-is.
			child, err := f.fetchOne(ctx, childSpec, visited)
			if err != nil {
				return lockfile.Package{}, fmt.Errorf("fetcher: recurse from %s into %s: %w", spec, imp, err)
			}
			depKey := lockfile.Key(child.Spec, child.Version)
			// Add as a dependency on the parent package (mutate
			// the visited entry, since pkg is a value copy).
			existing := visited[lockfile.Key(pkg.Spec, pkg.Version)]
			existing.Deps = appendUnique(existing.Deps, depKey)
			visited[lockfile.Key(pkg.Spec, pkg.Version)] = existing
		}
	}

	return visited[lockfile.Key(pkg.Spec, pkg.Version)], nil
}

// specToURL builds the registry URL for a bare spec. Three shapes:
//
//	"vue"               → <registry>/vue?<URLQuery>
//	"vue@3.4.21"        → <registry>/vue@3.4.21?<URLQuery>
//	"https://esm.sh/x"  → "https://esm.sh/x?<URLQuery>"   (already absolute)
//
// Absolute URLs are passed through so recursion into CDN-internal
// imports works without re-prefixing. URLQuery (when set) is
// appended consistently regardless of input shape so esm.sh's
// `?target=es2015` lowering applies to EVERY fetched URL — the
// Vue compiler bootstrap depends on this.
func (f *Fetcher) specToURL(spec string) (string, error) {
	var raw string
	switch {
	case strings.HasPrefix(spec, "http://"), strings.HasPrefix(spec, "https://"):
		raw = spec
	case spec == "":
		return "", errors.New("fetcher: empty spec")
	default:
		raw = f.Registry + "/" + spec
	}
	return appendURLQuery(raw, f.URLQuery), nil
}

// appendURLQuery suffixes the query (without leading "?") onto u
// using "?" or "&" depending on whether u already has a query.
// Empty query returns u unchanged. Malformed URLs are returned
// unchanged too — the subsequent fetch will surface a clearer
// error than we could synthesize here.
func appendURLQuery(u, query string) string {
	if query == "" {
		return u
	}
	if strings.Contains(u, "?") {
		return u + "&" + query
	}
	return u + "?" + query
}

// resolve follows redirects without downloading the body. Returns
// the final URL the body would come from, plus Content-Type and
// ETag headers if present.
//
// We use GET (not HEAD) because some CDNs (Cloudflare, esm.sh's
// edge) serve headers for HEAD that differ from GET — Content-Type
// is sometimes "application/octet-stream" on HEAD but
// "application/javascript" on GET. Using GET costs an extra body
// download we then discard, but the bodies are small and we'd
// fetch them anyway on cache miss.
//
// Closing the body without reading it (when we know we're going to
// re-fetch) is fine — Go's HTTP client returns connections to the
// pool either way.
func (f *Fetcher) resolve(ctx context.Context, reqURL string) (resolved, contentType, etag string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", "", "", err
	}
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return "", "", "", fmt.Errorf("fetcher: GET %s: %w", reqURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", "", &HTTPError{URL: reqURL, Status: resp.StatusCode}
	}
	// resp.Request.URL is the final URL after redirects.
	final := resp.Request.URL.String()
	return final, resp.Header.Get("Content-Type"), resp.Header.Get("ETag"), nil
}

// get downloads the body at url and returns its bytes. Errors carry
// the URL for diagnosis.
func (f *Fetcher) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetcher: GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &HTTPError{URL: url, Status: resp.StatusCode}
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("fetcher: read body %s: %w", url, err)
	}
	return body, nil
}

// resolveAgainst joins an import specifier to its parent's URL.
//
//	parent="https://esm.sh/vue@3.4.21"  imp="./shared.js"
//	  →    "https://esm.sh/shared.js"
//
//	parent="https://esm.sh/vue@3.4.21"  imp="/v135/x/y.js"
//	  →    "https://esm.sh/v135/x/y.js"
//
//	parent="https://esm.sh/vue@3.4.21"  imp="lodash"
//	  →    "lodash"                          (bare spec — caller skips)
//
// Bare specs are returned as-is and the caller drops them — we
// only recurse into URLs we can reach via HTTP. esbuild's plugin
// handles the bare-spec case at bundle time by going through the
// lockfile.
func resolveAgainst(parentURL, imp string) (string, error) {
	if strings.HasPrefix(imp, "http://") || strings.HasPrefix(imp, "https://") {
		return imp, nil
	}
	if strings.HasPrefix(imp, ".") || strings.HasPrefix(imp, "/") {
		base, err := url.Parse(parentURL)
		if err != nil {
			return "", err
		}
		rel, err := url.Parse(imp)
		if err != nil {
			return "", err
		}
		return base.ResolveReference(rel).String(), nil
	}
	// Bare spec — return unchanged so caller can skip it.
	return imp, nil
}

// parseSpec splits "<name>" or "<name>@<version>" into (name, ver).
// Handles scoped names: "@vue/x@3.4.21" → ("@vue/x", "3.4.21").
func parseSpec(spec string) (name, version string) {
	if i := strings.LastIndex(spec, "@"); i > 0 {
		return spec[:i], spec[i+1:]
	}
	return spec, ""
}

// extractVersionFromURL pulls the version out of a resolved URL
// like "https://esm.sh/vue@3.4.21" → "3.4.21". Returns "" when the
// URL has no @<ver> segment (rare; would mean the registry didn't
// version-pin via redirect).
//
// The "@" search starts after the last "/" so a scoped name in the
// URL path (e.g. "@vue/runtime-dom@3.4.21") doesn't confuse the
// split — we look for "@version" specifically.
func extractVersionFromURL(u string) string {
	parsed, err := url.Parse(u)
	if err != nil {
		return ""
	}
	path := parsed.Path
	// Walk segments right-to-left; the first "name@version" is
	// our pin. For a scoped package URL "/@vue/runtime-dom@3.4.21"
	// the last segment is "runtime-dom@3.4.21".
	segs := strings.Split(path, "/")
	for i := len(segs) - 1; i >= 0; i-- {
		seg := segs[i]
		if seg == "" || strings.HasPrefix(seg, "@") {
			continue
		}
		if at := strings.Index(seg, "@"); at >= 0 {
			return seg[at+1:]
		}
	}
	return ""
}

// isJSContent decides whether to parse a fetched body for imports.
// Content-Type is the primary signal; falls back to the URL's path
// extension when the registry returned a vague type
// ("application/octet-stream").
//
// We treat as JS:  application/javascript, text/javascript,
// application/typescript, .js/.mjs/.ts/.tsx/.jsx
//
// We skip:  CSS, source maps, JSON (esm.sh sometimes serves
// import-maps as JSON), and anything not in the above list.
func isJSContent(contentType, urlString string) bool {
	ct := strings.ToLower(contentType)
	switch {
	case strings.Contains(ct, "javascript"),
		strings.Contains(ct, "typescript"),
		strings.Contains(ct, "ecmascript"):
		return true
	case strings.Contains(ct, "css"),
		strings.Contains(ct, "json"),
		strings.Contains(ct, "octet-stream"):
		// Fall through to extension sniff for octet-stream.
		if !strings.Contains(ct, "octet-stream") {
			return false
		}
	}
	parsed, err := url.Parse(urlString)
	if err != nil {
		return false
	}
	p := strings.ToLower(parsed.Path)
	for _, ext := range []string{".js", ".mjs", ".ts", ".tsx", ".jsx"} {
		if strings.HasSuffix(p, ext) {
			return true
		}
	}
	return false
}

// appendUnique appends s to xs only if it isn't already there.
// Used to build deps lists without dupes. O(n) per call; n is
// per-package import count (single digits in practice).
func appendUnique(xs []string, s string) []string {
	for _, x := range xs {
		if x == s {
			return xs
		}
	}
	return append(xs, s)
}

// readFile is a thin wrapper over os.ReadFile that the test can
// stub if needed; today it just delegates.
func readFile(path string) ([]byte, error) {
	return readFileImpl(path)
}

var readFileImpl = osReadFile

// --- HTTP error -----------------------------------------------------

// HTTPError is the typed error returned for non-2xx responses.
// Carries enough detail (URL + status) for diagnosis without a
// stack trace.
type HTTPError struct {
	URL    string
	Status int
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("fetcher: %s returned HTTP %d", e.URL, e.Status)
}

// IntegrityHex strips the "sha256-" prefix from a lockfile
// integrity string. Helper for callers handing the bare hex to
// store.Put.
func IntegrityHex(integrity string) string {
	if strings.HasPrefix(integrity, "sha256-") {
		s := integrity[len("sha256-"):]
		// Sanity: must be 64 hex chars. If not, return empty so
		// store.Put treats it as "no expected hash" rather than
		// always-failing on a malformed pin.
		if len(s) == 64 {
			if _, err := hex.DecodeString(s); err == nil {
				return s
			}
		}
	}
	return ""
}
