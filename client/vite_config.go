package client

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Markers fence the proxy entries that `nexus dev` owns. Everything
// between them is regenerated on each sync — adding entries the
// running app advertises and removing entries it no longer does.
// User-added proxy rules belong OUTSIDE the markers; those stay
// untouched across syncs.
const (
	nexusProxyMarkerStart = "// @nexus:proxy-start — managed by `nexus dev`, do not edit between markers"
	nexusProxyMarkerEnd   = "// @nexus:proxy-end"
)

// viteWatchExcludeGlobs are the well-known files that auto-import
// plugins (unplugin-auto-import, unplugin-vue-components — both
// shipped by @nuxt/ui's vite plugin) regenerate at the end of every
// vite build. With `vite build --watch`, that self-write triggers
// the next build, the next build rewrites them again, and the
// frontend watcher pegs a CPU forever. Excluding them from
// rollup's input watcher breaks the cycle without affecting the
// useful watch — source-file edits still trigger a rebuild.
//
// Hard-coded rather than configurable because the file names are
// industry-standard and apply to every project using these
// ecosystem plugins. A project that doesn't generate them won't
// notice the exclude (rollup ignores patterns that match nothing).
var viteWatchExcludeGlobs = []string{
	"**/auto-imports.d.ts",
	"**/components.d.ts",
}

// MergeViteConfig wires three idempotent edits into a Vite config:
//
//  1. Adds `import nexusAutoSelect from '<rel>/nexus-vite-plugin.js'`
//     after the last existing top-level import.
//  2. Adds `nexusAutoSelect()` to the first plugins array it finds.
//  3. Adds `watch: { exclude: [...] }` inside `build:` to suppress
//     the auto-import-plugin self-rebuild loop documented at
//     viteWatchExcludeGlobs.
//
// String-pattern based — does NOT parse the config as TS. Handles
// the 95% of vite.config.{ts,js,mts,mjs} shapes that wrap a plugins
// array inside defineConfig({...}). When the heuristic can't locate
// the relevant block, the function leaves THAT edit untouched and
// prints a hint pointing at the manual one-line change. The other
// edits still apply.
//
// configPath is the absolute or relative path to the user's Vite
// config; sdkDir is where nexus-vite-plugin.js was dumped (matches
// Config.Client.OutDir). Re-running is a no-op once all edits have
// landed — idempotency keys off the literal `nexusAutoSelect`
// identifier and the `auto-imports.d.ts` glob appearing in the file.
func MergeViteConfig(configPath, sdkDir string, stdout io.Writer) error {
	if stdout == nil {
		stdout = io.Discard
	}
	src, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(stdout, "[nexus] vite config %q not found — skip auto-attach\n", configPath)
			return nil
		}
		return fmt.Errorf("read %s: %w", configPath, err)
	}

	body := string(src)
	cfgDir := filepath.Dir(configPath)
	pluginPath := filepath.Join(sdkDir, "nexus-vite-plugin.js")
	rel, err := filepath.Rel(cfgDir, pluginPath)
	if err != nil {
		return fmt.Errorf("relpath %s -> %s: %w", cfgDir, pluginPath, err)
	}
	rel = filepath.ToSlash(rel)
	if !strings.HasPrefix(rel, ".") {
		rel = "./" + rel
	}
	// Modern alias is plain `nexus`; the plugin is no longer just the
	// auto-select rewriter (it bundles manifest-filter + loop-guard
	// too). The legacy `nexusAutoSelect` import name remains
	// recognised below so projects wired before the rename don't get
	// a duplicate import on the next nexus dev.
	importLine := fmt.Sprintf("import nexus from '%s'", rel)

	changed := false
	// Idempotence: any existing reference to `nexus` (new) or
	// `nexusAutoSelect` (legacy) means we've wired it before — skip
	// both edits.
	alreadyWired := strings.Contains(body, "nexusAutoSelect") ||
		strings.Contains(body, "from '"+rel+"'")

	if !alreadyWired {
		body = insertImport(body, importLine)
		changed = true
	}

	// Same dual-name check on the call site so we don't duplicate
	// nexus() into a plugins array that already has nexusAutoSelect().
	if !strings.Contains(body, "nexusAutoSelect()") && !strings.Contains(body, "nexus(") {
		updated, ok := insertIntoPluginsArray(body, "nexus()")
		if !ok {
			fmt.Fprintf(stdout, "[nexus] couldn't locate a `plugins:` array in %s — add `nexus()` manually\n", configPath)
			// Still write the import (helpful even if the array edit failed).
			if changed {
				// #nosec G703 -- CLI helper writes operator-supplied vite config path
				if err := os.WriteFile(configPath, []byte(body), 0600); err != nil {
					return fmt.Errorf("write %s: %w", configPath, err)
				}
			}
			return nil
		}
		body = updated
		changed = true
	}

	if updated, ok := insertWatchExclude(body, viteWatchExcludeGlobs); ok {
		body = updated
		changed = true
		fmt.Fprintf(stdout, "[nexus] added build.watch.exclude for auto-imports.d.ts / components.d.ts in %s\n", configPath)
	}

	if !changed {
		return nil
	}
	// #nosec G703 -- CLI helper writes operator-supplied vite config path
	if err := os.WriteFile(configPath, []byte(body), 0600); err != nil {
		return fmt.Errorf("write %s: %w", configPath, err)
	}
	fmt.Fprintf(stdout, "[nexus] wired auto-select plugin into %s\n", configPath)
	return nil
}

// DefaultNexusProxyPrefixes is the list of framework-reserved URL
// prefixes that nexus dev auto-injects into vite's server.proxy
// block. Routing them through the vite dev server (same-origin
// with the SPA) avoids CORS preflights that the browser would
// otherwise fire against the cross-origin Go listener.
//
// Each prefix corresponds to a transport the framework owns:
//
//	/__nexus  dashboard, manifest, contributions, health
//	/graphql  GraphQL ops (AsQuery / AsMutation / AsSubscription)
//	/oauth    oauth2.Module callback + login flows
//	/ws       AsWS WebSocket endpoints (ws:true on the proxy rule)
//
// Apps with custom prefixes (a sub-router, an /api/v1 base path)
// pass them via the EnsureViteProxyForPrefixes variant.
var DefaultNexusProxyPrefixes = []string{"/__nexus", "/graphql", "/oauth", "/ws"}

// EnsureViteProxyForNexus is the back-compat wrapper around
// SyncViteProxyForPrefixes(DefaultNexusProxyPrefixes). Existing
// callers (the dev CLI's boot path, third-party scripts) keep
// working; new code should prefer SyncViteProxyForPrefixes when
// the prefix set is dynamic (e.g., derived from the running app's
// manifest at boot).
func EnsureViteProxyForNexus(configPath, apiURL string, stdout io.Writer) error {
	return SyncViteProxyForPrefixes(configPath, apiURL, DefaultNexusProxyPrefixes, stdout)
}

// SyncViteProxyForPrefixes makes server.proxy reflect a declarative
// set of prefixes — adds the ones missing, removes the ones the
// runtime no longer advertises. Idempotent: a re-sync with the same
// set is a no-op.
//
// Managed entries live between `// @nexus:proxy-start` and
// `// @nexus:proxy-end` markers inside `server.proxy`. On every
// call the contents between those markers are regenerated to match
// `prefixes` exactly. Entries OUTSIDE the markers are user-owned
// and preserved.
//
// First-run migration: when no markers exist, any existing entry
// in the proxy block whose key matches a prefix in the input set
// is removed and folded into the new managed block — that way
// upgrading a project that was previously hand-wired produces a
// clean managed block without duplicates.
//
// Bootstrap ladder when the proxy block is missing:
//  1. `proxy: { … }` present → prepend the marker block inside it.
//  2. `server: { … }` present without proxy → add `proxy: { ... }`
//     with the marker block.
//  3. `defineConfig({...})` only → add `server: { proxy: { ... } }`.
//  4. None of the above → leave the file alone.
//
// The "/ws" prefix gets ws:true so vite upgrades to WebSocket
// instead of buffering as HTTP.
func SyncViteProxyForPrefixes(configPath, apiURL string, prefixes []string, stdout io.Writer) error {
	if stdout == nil {
		stdout = io.Discard
	}
	if apiURL == "" {
		apiURL = "http://localhost:8080"
	}
	src, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", configPath, err)
	}
	body := string(src)

	// Stable order keeps re-syncs byte-identical when the set hasn't
	// changed — vite's restart-on-change is sensitive enough that an
	// unstable serialization would cause spurious reloads.
	cleaned := dedupeSortPrefixes(prefixes)
	updated, ok := syncProxyManagedBlock(body, apiURL, cleaned)
	if !ok || updated == body {
		return nil
	}
	// #nosec G703 -- CLI helper writes operator-supplied vite config path
	if err := os.WriteFile(configPath, []byte(updated), 0600); err != nil {
		return fmt.Errorf("write %s: %w", configPath, err)
	}
	fmt.Fprintf(stdout, "[nexus] synced vite proxy in %s — %d prefix(es), target %s\n",
		configPath, len(cleaned), apiURL)
	return nil
}

// dedupeSortPrefixes drops empties + duplicates and sorts. The sort
// gives stable output so an unchanged input produces byte-identical
// blocks across syncs — important because vite's HMR restarts the
// dev server on any vite.config.ts change.
func dedupeSortPrefixes(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, p := range in {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// syncProxyManagedBlock returns the new body + ok, where ok is true
// when an edit is needed (or a write would land identical bytes —
// the caller short-circuits via the unchanged check). Splits along
// whether the markers already exist:
//
//   - Markers present: replace their contents wholesale.
//   - Markers absent: strip any existing entries whose key is in
//     prefixes, then bootstrap the markers in the right scaffold.
func syncProxyManagedBlock(body, apiURL string, prefixes []string) (string, bool) {
	managed := renderManagedBlock(prefixes, apiURL)
	if startIdx, endIdx, ok := findMarkerRange(body); ok {
		// Replace from the start marker through the end marker
		// (inclusive). The caller compares the result to the
		// original to decide whether to write.
		return body[:startIdx] + managed + body[endIdx:], true
	}
	// First-run migration: pull out any unmanaged entries the user
	// (or an older nexus dev) wrote, so the new managed block
	// doesn't end up duplicating keys.
	stripped := body
	for _, p := range prefixes {
		if next, removed := removeProxyEntryByKey(stripped, p); removed {
			stripped = next
		}
	}
	return bootstrapManagedBlock(stripped, managed)
}

// renderManagedBlock formats the marker-fenced block. Two-space
// indent inside `proxy: {` mirrors the existing injector's shape
// and the convention used by hand-written vite configs in this
// repo's examples/.
func renderManagedBlock(prefixes []string, apiURL string) string {
	var b strings.Builder
	b.WriteString(nexusProxyMarkerStart)
	for _, p := range prefixes {
		b.WriteString("\n      ")
		b.WriteString(renderProxyEntry(p, apiURL))
		b.WriteByte(',')
	}
	b.WriteString("\n      ")
	b.WriteString(nexusProxyMarkerEnd)
	return b.String()
}

// findMarkerRange locates the marker pair and returns the start
// index of the opening marker + the end index (one past the
// closing marker). The third return is false when either marker
// is missing or they appear in the wrong order — in that case the
// caller treats it as "no managed block yet" and falls through to
// bootstrap.
func findMarkerRange(body string) (int, int, bool) {
	start := strings.Index(body, nexusProxyMarkerStart)
	if start < 0 {
		return 0, 0, false
	}
	end := strings.Index(body[start:], nexusProxyMarkerEnd)
	if end < 0 {
		return 0, 0, false
	}
	return start, start + end + len(nexusProxyMarkerEnd), true
}

// bootstrapManagedBlock inserts the marker-fenced managed block into
// the right scaffold. Mirrors the legacy injector's ladder so
// upgrading projects land identical structure to the prior path.
func bootstrapManagedBlock(body, managed string) (string, bool) {
	proxyRe := regexp.MustCompile(`proxy\s*:\s*\{`)
	if loc := proxyRe.FindStringIndex(body); loc != nil {
		openBrace := loc[1] - 1
		ins := "\n      " + managed + ","
		return body[:openBrace+1] + ins + body[openBrace+1:], true
	}
	serverRe := regexp.MustCompile(`server\s*:\s*\{`)
	if loc := serverRe.FindStringIndex(body); loc != nil {
		openBrace := loc[1] - 1
		ins := "\n    proxy: {\n      " + managed + ",\n    },"
		return body[:openBrace+1] + ins + body[openBrace+1:], true
	}
	cfgRe := regexp.MustCompile(`defineConfig\s*\(\s*\{`)
	if loc := cfgRe.FindStringIndex(body); loc != nil {
		openBrace := loc[1] - 1
		ins := "\n  server: { proxy: {\n    " + managed + ",\n  } },"
		return body[:openBrace+1] + ins + body[openBrace+1:], true
	}
	return body, false
}

// removeProxyEntryByKey deletes the JS object property keyed by
// `"prefix"` (single- or double-quoted) from body, including its
// value and trailing comma + same-line whitespace. Brace-counted so
// multi-line values like `"/ws": { target: "...", ws: true, },`
// get removed in full, not just the first line.
//
// Returns (body, false) when the key isn't found, has no `{ ... }`
// value, or the braces are unbalanced — leave the file alone in
// those edge cases rather than mangle it.
func removeProxyEntryByKey(body, prefix string) (string, bool) {
	for _, q := range []string{`"`, `'`} {
		key := q + prefix + q
		idx := strings.Index(body, key)
		if idx < 0 {
			continue
		}
		// skip whitespace + ':' to reach the value's opening brace
		i := idx + len(key)
		for i < len(body) && (body[i] == ' ' || body[i] == '\t' || body[i] == ':') {
			i++
		}
		if i >= len(body) || body[i] != '{' {
			continue
		}
		depth := 0
		end := -1
		for j := i; j < len(body); j++ {
			switch body[j] {
			case '{':
				depth++
			case '}':
				depth--
			}
			if depth == 0 {
				end = j + 1
				break
			}
		}
		if end < 0 {
			continue
		}
		// swallow trailing `,` + same-line whitespace + the newline
		k := end
		if k < len(body) && body[k] == ',' {
			k++
		}
		for k < len(body) && (body[k] == ' ' || body[k] == '\t') {
			k++
		}
		if k < len(body) && body[k] == '\n' {
			k++
		}
		// trim leading same-line whitespace so we don't leave an
		// empty indented line behind
		start := idx
		for start > 0 && (body[start-1] == ' ' || body[start-1] == '\t') {
			start--
		}
		return body[:start] + body[k:], true
	}
	return body, false
}

// renderProxyEntry returns the single-line proxy rule for a prefix.
// WebSocket prefixes get ws:true so vite upgrades the connection
// instead of trying to buffer it as a regular HTTP response. The
// matcher is the literal "/ws" prefix only — apps with custom WS
// paths can layer their own entries on top via vite.config.ts.
func renderProxyEntry(prefix, apiURL string) string {
	if prefix == "/ws" {
		return fmt.Sprintf(`"%s": { target: "%s", changeOrigin: true, ws: true }`, prefix, apiURL)
	}
	return fmt.Sprintf(`"%s": { target: "%s", changeOrigin: true }`, prefix, apiURL)
}

// EnsureViteWatchExclude is the watch.exclude half of MergeViteConfig
// exposed for the dev CLI. The CLI calls this BEFORE spawning the
// frontend watcher so vite reads the patched config on first boot
// rather than continuing with the unpatched copy in memory while
// the framework's later auto-dump rewrites the file behind it.
//
// Idempotent: a no-op when the exclude entry is already present.
// Returns nil for a missing config (skip-with-notice on stdout)
// since not every project keeps vite.config.ts at the conventional
// location and we'd rather not fail the dev loop over it.
func EnsureViteWatchExclude(configPath string, stdout io.Writer) error {
	if stdout == nil {
		stdout = io.Discard
	}
	src, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", configPath, err)
	}
	body := string(src)
	updated, ok := insertWatchExclude(body, viteWatchExcludeGlobs)
	if !ok {
		return nil
	}
	// #nosec G703 -- CLI helper writes operator-supplied vite config path
	if err := os.WriteFile(configPath, []byte(updated), 0600); err != nil {
		return fmt.Errorf("write %s: %w", configPath, err)
	}
	fmt.Fprintf(stdout, "[nexus] added build.watch.exclude for auto-imports.d.ts / components.d.ts in %s\n", configPath)
	return nil
}

// insertWatchExclude injects a build.watch.exclude entry covering
// globs into body. Returns the new body and ok=true on edit, or
// (body, false) when no edit was needed (already present) or no
// suitable insertion point was found.
//
// Strategy:
//   - Idempotency: if body already mentions any of globs (substring),
//     assume it's already wired and skip.
//   - First choice: inject into the existing `build: { … }` block
//     by prepending `watch: { exclude: [...] },` after the opening
//     brace. Preserves the rest of build untouched.
//   - Second choice: inject a complete `build: { watch: { … } },`
//     block at the top of the defineConfig({...}) argument.
//   - Otherwise: leave body alone (caller falls back to no-op).
func insertWatchExclude(body string, globs []string) (string, bool) {
	for _, g := range globs {
		if strings.Contains(body, g) {
			return body, false
		}
	}
	if len(globs) == 0 {
		return body, false
	}

	quoted := make([]string, 0, len(globs))
	for _, g := range globs {
		quoted = append(quoted, "'"+g+"'")
	}
	excludeArr := "[" + strings.Join(quoted, ", ") + "]"

	// Try injecting into existing build: { … }.
	buildRe := regexp.MustCompile(`build\s*:\s*\{`)
	if loc := buildRe.FindStringIndex(body); loc != nil {
		openBrace := loc[1] - 1
		insertion := "\n    watch: { exclude: " + excludeArr + " },"
		return body[:openBrace+1] + insertion + body[openBrace+1:], true
	}

	// No build block — add a fresh one at the top of defineConfig({...}).
	cfgRe := regexp.MustCompile(`defineConfig\s*\(\s*\{`)
	if loc := cfgRe.FindStringIndex(body); loc != nil {
		openBrace := loc[1] - 1
		insertion := "\n  build: { watch: { exclude: " + excludeArr + " } },"
		return body[:openBrace+1] + insertion + body[openBrace+1:], true
	}

	return body, false
}

// insertImport appends importLine after the last top-level `import …`
// line in body. Falls back to prepending when no imports exist.
func insertImport(body, importLine string) string {
	re := regexp.MustCompile(`(?m)^import .*$`)
	matches := re.FindAllStringIndex(body, -1)
	if len(matches) == 0 {
		return importLine + "\n" + body
	}
	last := matches[len(matches)-1]
	return body[:last[1]] + "\n" + importLine + body[last[1]:]
}

// insertIntoPluginsArray finds the first `plugins:` array in body and
// inserts entry inside it (before the closing `]`). Returns false
// when no `plugins:` array is found or the brackets don't balance.
func insertIntoPluginsArray(body, entry string) (string, bool) {
	keyRe := regexp.MustCompile(`plugins\s*:\s*\[`)
	loc := keyRe.FindStringIndex(body)
	if loc == nil {
		return body, false
	}
	openBracket := loc[1] - 1

	close := matchingCloseBracket(body, openBracket)
	if close < 0 {
		return body, false
	}

	inner := body[openBracket+1 : close]
	trimmed := strings.TrimRight(inner, " \t\n")
	suffix := inner[len(trimmed):]

	var insertion string
	switch {
	case strings.TrimSpace(trimmed) == "":
		insertion = entry
	case strings.HasSuffix(trimmed, ","):
		insertion = trimmed + " " + entry
	default:
		insertion = trimmed + ", " + entry
	}
	return body[:openBracket+1] + insertion + suffix + body[close:], true
}

// matchingCloseBracket returns the offset of the `]` that closes the
// `[` at openIdx, ignoring brackets inside strings/templates. Returns
// -1 when the brackets don't balance.
func matchingCloseBracket(s string, openIdx int) int {
	if openIdx >= len(s) || s[openIdx] != '[' {
		return -1
	}
	depth := 0
	var inStr byte
	var inLine, inBlock bool
	for i := openIdx; i < len(s); i++ {
		c := s[i]
		if inLine {
			if c == '\n' {
				inLine = false
			}
			continue
		}
		if inBlock {
			if c == '*' && i+1 < len(s) && s[i+1] == '/' {
				inBlock = false
				i++
			}
			continue
		}
		if inStr != 0 {
			if c == '\\' {
				i++
				continue
			}
			if c == inStr {
				inStr = 0
			}
			continue
		}
		switch c {
		case '/':
			if i+1 < len(s) && s[i+1] == '/' {
				inLine = true
				i++
				continue
			}
			if i+1 < len(s) && s[i+1] == '*' {
				inBlock = true
				i++
				continue
			}
		case '\'', '"', '`':
			inStr = c
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}
