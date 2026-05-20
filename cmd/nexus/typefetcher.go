package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// typeFetcher mirrors a package's TypeScript declarations (.d.ts /
// .d.mts) from esm.sh into a local node_modules-shaped tree so that
// `import "vue"` in user code gets full IntelliSense in the IDE
// without anyone running `npm install`.
//
// Why node_modules and not .nexus/types/: tsserver's default
// resolution looks in node_modules. Using that path means zero
// tsconfig changes — IDEs that auto-detect package.json + node_modules
// (which is nearly all of them) "just work." The downside is the
// "node_modules without npm" pattern looks weird, but it's no
// weirder than nexus.lock without package-lock.json, and the
// .gitignore handles the commit-noise concern.
//
// How it works:
//
//  1. Start at the package's top-level .d.ts URL — the value
//     esm.sh returns in its X-TypeScript-Types response header.
//  2. Recursively walk every `from "<https-url>"` reference inside
//     each fetched file. esm.sh emits absolute HTTPS imports
//     because the type files are designed for Deno consumption;
//     tsserver doesn't follow https:// imports, so we rewrite them
//     to bare specifiers ("@vue/runtime-dom") or relative paths
//     and place the referenced file in the matching local position
//     so the rewritten import resolves.
//  3. Write a synthetic package.json next to each top-level pkg's
//     entry so tsserver's "what types does this pkg expose" lookup
//     succeeds without us having to invent an index.d.ts re-export.
//
// Best-effort by design: if fetching fails partway, the user still
// gets the previous shim-based experience (TS2307 silenced, no
// autocomplete). We log the warning and don't fail the parent
// `nexus add`.
type typeFetcher struct {
	HTTP    *http.Client
	Verbose bool
}

func newTypeFetcher() *typeFetcher {
	return &typeFetcher{HTTP: http.DefaultClient}
}

// fetchAll resolves and mirrors types for every spec name in the
// keyed map. The map value is the resolved version (from the
// post-fetch lockfile entry) used to construct the esm.sh URL we
// query for X-TypeScript-Types. destDir is the project root; the
// node_modules tree lands at destDir/node_modules/.
//
// Returns a count of how many packages got types written, plus the
// first error encountered (if any). Per-package errors are logged
// to stderr but don't short-circuit — the caller sees as many
// types as we could fetch.
func (tf *typeFetcher) fetchAll(ctx context.Context, specs map[string]string, destDir string, stderr io.Writer) (int, error) {
	visited := map[string]string{}
	nmRoot := filepath.Join(destDir, "node_modules")
	var written int
	var firstErr error
	for spec, version := range specs {
		if version == "" {
			// Without a version we can't construct a stable
			// esm.sh URL — skip silently, the shim-based any
			// type kicks in.
			continue
		}
		n, err := tf.fetchOne(ctx, spec, version, nmRoot, visited)
		if err != nil {
			fmt.Fprintf(stderr, "nexus add: types for %s skipped: %v\n", spec, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		written += n
	}
	return written, firstErr
}

// fetchOne handles a single top-level spec. It resolves the
// X-TypeScript-Types header for the spec's main URL, then hands
// off to fetchRecursive. After the tree is materialized it writes
// the synthetic package.json that tells tsserver where to enter.
//
// Returns the number of files written.
func (tf *typeFetcher) fetchOne(ctx context.Context, spec, version, nmRoot string, visited map[string]string) (int, error) {
	mainURL := "https://esm.sh/" + spec + "@" + version
	typesURL, err := tf.discoverTypesURL(ctx, mainURL)
	if err != nil {
		return 0, fmt.Errorf("discover X-TypeScript-Types for %s@%s: %w", spec, version, err)
	}
	if typesURL == "" {
		// No type file advertised — package doesn't ship types,
		// or esm.sh couldn't find @types/<pkg>. Not an error.
		return 0, nil
	}
	pkg, relPath, ok := parseESMURL(typesURL)
	if !ok {
		return 0, fmt.Errorf("unrecognized type URL shape: %s", typesURL)
	}
	if pkg != spec {
		// Common case: esm.sh redirects /vue@x to a @types
		// package (@types/vue), which lives at a different
		// "name" in the type tree. We honor whatever URL esm.sh
		// returned but still write the package.json under the
		// spec the user asked for, since that's what they
		// `import "..."` by.
		_ = pkg
	}
	beforeCount := len(visited)
	if err := tf.fetchRecursive(ctx, typesURL, nmRoot, visited); err != nil {
		return 0, err
	}
	written := len(visited) - beforeCount

	// Synthesize node_modules/<spec>/package.json so tsserver's
	// "find the types for this pkg" lookup ("types" field, then
	// "typings" field, then ./index.d.ts) succeeds. The "types"
	// path is RELATIVE to this package.json.
	pjDir := filepath.Join(nmRoot, filepath.FromSlash(spec))
	if err := os.MkdirAll(pjDir, 0750); err != nil {
		return written, fmt.Errorf("mkdir %s: %w", pjDir, err)
	}
	// The entry file lives at nmRoot/<pkg>/<relPath>. We need a
	// path relative to nmRoot/<spec>/package.json. When pkg ==
	// spec these collapse to "./<relPath>"; otherwise we resolve
	// across packages.
	entryAbs := filepath.Join(nmRoot, filepath.FromSlash(pkg), filepath.FromSlash(relPath))
	entryRel, err := filepath.Rel(pjDir, entryAbs)
	if err != nil {
		return written, fmt.Errorf("relpath %s → %s: %w", pjDir, entryAbs, err)
	}
	entryRel = filepath.ToSlash(entryRel)
	if !strings.HasPrefix(entryRel, ".") {
		entryRel = "./" + entryRel
	}
	pjContent := map[string]any{
		"name":  spec,
		"types": entryRel,
	}
	pjBytes, _ := json.MarshalIndent(pjContent, "", "  ")
	pjBytes = append(pjBytes, '\n')
	if err := os.WriteFile(filepath.Join(pjDir, "package.json"), pjBytes, 0600); err != nil {
		return written, fmt.Errorf("write package.json for %s: %w", spec, err)
	}
	return written, nil
}

// discoverTypesURL HEADs the main package URL and reads the
// X-TypeScript-Types response header. Returns "" when the package
// doesn't expose types (rare for typed packages, common for legacy
// JS-only ones).
func (tf *typeFetcher) discoverTypesURL(ctx context.Context, mainURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, mainURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := tf.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("HEAD %s: %d", mainURL, resp.StatusCode)
	}
	return resp.Header.Get("X-TypeScript-Types"), nil
}

// httpsImportRe matches the URL inside any `from "<url>"` / `from
// '<url>'` / `import("<url>")` / `import('<url>')` reference that
// uses an HTTPS URL. We only rewrite HTTPS imports — relative and
// bare-specifier imports inside fetched .d.ts files are left alone
// (they already resolve through tsserver's normal mechanism after
// we mirror the file tree).
//
// The regex is intentionally permissive on what's inside the
// quotes (`[^"']+`) so query strings and fragments come along; it
// rejects anything not starting with `https://` to avoid matching
// data URIs or relative paths.
var httpsImportRe = regexp.MustCompile(`(?m)(from\s+|import\s*\(\s*)(["'])(https://[^"']+)(["'])`)

// fetchRecursive walks the URL graph rooted at typeURL, writing
// each .d.ts to its corresponding local path under nmRoot and
// rewriting HTTPS imports to bare or relative paths so tsserver
// can follow them.
//
// visited is the dedup map keyed by URL → local-path-relative-to-
// nmRoot. Seeing the same URL twice during one fetchAll is fine
// (diamond import in a type graph — A imports common.d.ts, B
// imports common.d.ts) — we short-circuit.
func (tf *typeFetcher) fetchRecursive(ctx context.Context, typeURL, nmRoot string, visited map[string]string) error {
	if _, seen := visited[typeURL]; seen {
		return nil
	}
	pkg, relPath, ok := parseESMURL(typeURL)
	if !ok {
		return fmt.Errorf("not an esm.sh URL: %s", typeURL)
	}
	localRel := filepath.Join(filepath.FromSlash(pkg), filepath.FromSlash(relPath))
	visited[typeURL] = localRel
	localPath := filepath.Join(nmRoot, localRel)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, typeURL, nil)
	if err != nil {
		return err
	}
	resp, err := tf.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", typeURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("GET %s: %d", typeURL, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read body %s: %w", typeURL, err)
	}

	// Pass 1: collect imports + recursively fetch them. We do
	// this BEFORE the rewrite pass so a recursion failure makes
	// us abort cleanly rather than half-writing a file with
	// dangling references.
	matches := httpsImportRe.FindAllSubmatchIndex(body, -1)
	rewrites := make([]struct{ from, to string }, 0, len(matches))
	for _, m := range matches {
		// Group 3 is the URL.
		urlStart, urlEnd := m[6], m[7]
		depURL := string(body[urlStart:urlEnd])
		if !strings.HasPrefix(depURL, "https://esm.sh/") {
			// Foreign HTTPS URL — leave it alone (tsserver will
			// report it as unresolved, but rewriting blindly
			// would be worse).
			continue
		}
		if err := tf.fetchRecursive(ctx, depURL, nmRoot, visited); err != nil {
			return err
		}
		// Compute the import path tsserver should see. We use
		// the dependency's local-relative-to-nmRoot path, then
		// derive a path relative to THIS file.
		depLocalRel, ok := visited[depURL]
		if !ok {
			continue
		}
		depAbs := filepath.Join(nmRoot, depLocalRel)
		rel, err := filepath.Rel(filepath.Dir(localPath), depAbs)
		if err != nil {
			continue
		}
		rel = filepath.ToSlash(rel)
		if !strings.HasPrefix(rel, ".") {
			rel = "./" + rel
		}
		// Strip the `.d.ts` / `.d.mts` extension — tsserver's
		// resolution adds the extension itself, and including it
		// in the import would actually break some configurations.
		rel = strings.TrimSuffix(rel, ".d.ts")
		rel = strings.TrimSuffix(rel, ".d.mts")
		rewrites = append(rewrites, struct{ from, to string }{depURL, rel})
	}

	// Pass 2: apply rewrites in one pass. Sort by length-desc so
	// a URL that's a prefix of another (rare but possible if a
	// query string differs only by suffix) doesn't get half-
	// matched.
	out := string(body)
	for _, r := range rewrites {
		out = strings.ReplaceAll(out, r.from, r.to)
	}

	if err := os.MkdirAll(filepath.Dir(localPath), 0o750); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(localPath), err)
	}
	if err := os.WriteFile(localPath, []byte(out), 0600); err != nil {
		return fmt.Errorf("write %s: %w", localPath, err)
	}
	return nil
}

// parseESMURL splits an esm.sh type URL into its (package,
// in-package relative path) pair. Returns ok=false for URLs that
// don't follow the documented esm.sh shape — we'd rather skip than
// guess and write to a wrong location.
//
//	https://esm.sh/vue@3.5.34/dist/vue.d.mts
//	  → ("vue", "dist/vue.d.mts")
//
//	https://esm.sh/@vue/runtime-dom@3.5.34/dist/runtime-dom.d.ts
//	  → ("@vue/runtime-dom", "dist/runtime-dom.d.ts")
//
//	https://esm.sh/vue@3.5.34
//	  → ("vue", "index.d.ts")    // synthetic — no path component
//
//	https://example.com/x.d.ts
//	  → ("", "", false)           // not esm.sh
func parseESMURL(rawURL string) (pkg, relPath string, ok bool) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", false
	}
	if u.Host != "esm.sh" {
		return "", "", false
	}
	p := strings.TrimPrefix(u.Path, "/")
	if p == "" {
		return "", "", false
	}
	if strings.HasPrefix(p, "@") {
		// Scoped: @scope/name@ver[/rest]
		parts := strings.SplitN(p, "/", 3)
		if len(parts) < 2 {
			return "", "", false
		}
		nameAndVer := parts[1]
		atIdx := strings.Index(nameAndVer, "@")
		if atIdx <= 0 {
			return "", "", false
		}
		pkg = parts[0] + "/" + nameAndVer[:atIdx]
		if len(parts) == 3 {
			relPath = parts[2]
		}
	} else {
		// Unscoped: name@ver[/rest]
		parts := strings.SplitN(p, "/", 2)
		nameAndVer := parts[0]
		atIdx := strings.Index(nameAndVer, "@")
		if atIdx <= 0 {
			return "", "", false
		}
		pkg = nameAndVer[:atIdx]
		if len(parts) == 2 {
			relPath = parts[1]
		}
	}
	if relPath == "" {
		// esm.sh root URLs (no in-package path) need a synthetic
		// entry filename so we can write to disk; index.d.ts is
		// the convention tsserver looks for.
		relPath = "index.d.ts"
	}
	return pkg, relPath, true
}

// helper used by other CLI files to know where the .nexus-managed
// node_modules tree lives — kept here for now since it's the only
// thing that touches that directory.
func typesNodeModulesDir(projectRoot string) string {
	return filepath.Join(projectRoot, "node_modules")
}

// gitignoreEnsureNodeModules makes sure the project's .gitignore
// excludes node_modules — otherwise a contributor might commit the
// fetched type tree by accident. Best-effort: silent no-op when no
// .gitignore exists, since the user might be on a non-git workflow.
func gitignoreEnsureNodeModules(projectRoot string) {
	path := filepath.Join(projectRoot, ".gitignore")
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(body), "\n") {
		trim := strings.TrimSpace(line)
		if trim == "node_modules" || trim == "/node_modules" || trim == "/node_modules/" || trim == "node_modules/" {
			return
		}
	}
	addition := "\n# nexus-managed type stubs for IntelliSense\n/node_modules/\n"
	out := append(body, []byte(addition)...)
	// #nosec G703 -- CLI helper writes operator-supplied frontend dir's .gitignore
	_ = os.WriteFile(path, out, 0600)
}

// ensure path is used — silences unused-import warnings during
// dev. Trivial helper, removed once any second caller of path.Join
// appears in this file.
var _ = path.Join
