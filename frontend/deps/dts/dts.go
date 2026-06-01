// Package dts materializes a types-only node_modules tree for editor
// IntelliSense WITHOUT npm. nexus is zero-Node — there is no node_modules
// at build or runtime — so the TypeScript/Vue language server has nothing
// to resolve bare imports (`import { ref } from "vue"`) against. This
// package fills that gap from the SAME source the build uses (the deps in
// nexus.lock, served by esm.sh), so IntelliSense is driven by the lockfile
// with no second package.json and no `npm install`.
//
// For each package it: (1) discovers the package's root .d.ts via esm.sh's
// X-TypeScript-Types header, (2) BFS-crawls the .d.ts graph, and (3) writes
// each file under node_modules/<pkg>/<path> with a per-package package.json
// pointing `types` at the entry. The crux is step 3's REWRITE: esm.sh .d.ts
// reference siblings by absolute `https://esm.sh/<pkg>@<ver>/<path>` URL,
// which the TS language server can't resolve (Deno-only). Each such URL is
// rewritten to a RELATIVE path to the target package's local file, so TS
// resolves the whole graph on disk.
package dts

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

// Getter fetches a URL, returning its body and — for the FIRST request to a
// package (the bare package URL) — the X-TypeScript-Types header value (the
// root .d.ts URL). For .d.ts URLs the header is "". Injected so the emitter
// is testable without network. A non-nil error aborts that package.
type Getter func(url string) (body string, typesHeader string, err error)

// Writer persists one emitted file (relative path under node_modules) with
// its (possibly rewritten) contents. Injected so tests capture output in a
// map instead of touching disk.
type Writer func(relPath string, contents string) error

// Pkg is one dependency to emit types for: its name (incl. scope) and the
// resolved version, plus the esm.sh base used to probe its types header.
type Pkg struct {
	Name    string // "vue", "@vue/runtime-dom", "markdown-it"
	Version string // "3.5.34"
	// ProbeURL is the URL whose X-TypeScript-Types header names the root
	// .d.ts (typically the bare package URL, e.g. https://esm.sh/vue@3.5.34).
	ProbeURL string
}

// Result reports what an emit pass produced.
type Result struct {
	Packages int // packages that got a types entry
	Files    int // total .d.ts files written
	Skipped  []string
}

// esmRefRE matches an esm.sh dependency URL inside a .d.ts — in an import/
// export specifier or a triple-slash reference. Captures pkg@ver and the
// trailing path. Both .d.ts and .d.mts appear.
var esmRefRE = regexp.MustCompile(`https://esm\.sh/((?:@[^/]+/)?[^/@]+)@([^/]+)((?:/[^"'>]*)?)`)

// Emit generates the types-only tree. For each pkg it discovers the root
// .d.ts (via the types header), crawls the graph, rewrites cross-package
// https refs to local relative paths, and writes the files + a package.json.
// Best-effort per package: a package whose types can't be fetched is added
// to Skipped and the rest proceed.
func Emit(pkgs []Pkg, get Getter, write Writer) (Result, error) {
	var res Result
	// node_modules-relative path of every URL we've decided to write, so a
	// ref can compute a relative path to a sibling even across packages.
	urlToRel := map[string]string{}
	// Resolve each pkg's root .d.ts URL first (one header probe per pkg) so
	// rewrites can target packages crawled later.
	type rootPkg struct {
		rootURL string
		p       Pkg
	}
	var ordered []rootPkg
	for _, p := range pkgs {
		_, hdr, err := get(p.ProbeURL)
		if err != nil || hdr == "" {
			res.Skipped = append(res.Skipped, p.Name+"@"+p.Version+" (no types)")
			continue
		}
		ordered = append(ordered, rootPkg{hdr, p})
		// The root file's local path is node_modules/<pkg>/<entry-basename>.
		urlToRel[hdr] = localRel(p.Name, dtsEntryPath(p.Name, hdr))
	}

	// BFS the COMBINED .d.ts graph across all packages once: a file shared
	// between packages (e.g. vue, pulled by vue-router) is written exactly
	// once. Seed the queue with every package root so transitive sharing
	// can't starve a later package — the package.json pass below is
	// independent of who crawled the root.
	written := map[string]bool{}
	var queue []string
	for _, rp := range ordered {
		queue = append(queue, rp.rootURL)
	}
	for len(queue) > 0 {
		u := queue[0]
		queue = queue[1:]
		if written[u] {
			continue
		}
		body, _, err := get(u)
		if err != nil {
			res.Skipped = append(res.Skipped, u+" (fetch failed)")
			continue
		}
		written[u] = true

		selfRel, ok := urlToRel[u]
		if !ok {
			selfRel = localRel(refPkg(u), refPath(u))
			urlToRel[u] = selfRel
		}
		rewritten, refs := rewriteRefs(body, u, urlToRel)
		if err := write(selfRel, rewritten); err != nil {
			return res, fmt.Errorf("dts: write %s: %w", selfRel, err)
		}
		res.Files++
		queue = append(queue, refs...)
	}

	// One package.json per probed top-level package — the editor resolves a
	// bare import "<pkg>" via its "types" entry. Independent of crawl order.
	for _, rp := range ordered {
		entry := dtsEntryPath(rp.p.Name, rp.rootURL)
		pj := packageJSON(rp.p.Name, rp.p.Version, entry)
		if err := write(localRel(rp.p.Name, "package.json"), pj); err != nil {
			return res, fmt.Errorf("dts: write package.json for %s: %w", rp.p.Name, err)
		}
		res.Packages++
	}
	sort.Strings(res.Skipped)
	return res, nil
}

// rewriteRefs rewrites every esm.sh https ref in a .d.ts to a relative path
// to the target's local node_modules file, and returns the discovered ref
// URLs to crawl. selfURL is the file being rewritten (to compute relative
// paths from its own location); urlToRel maps known URLs to their local
// node_modules-relative paths and is extended in place for new targets.
func rewriteRefs(body, selfURL string, urlToRel map[string]string) (string, []string) {
	selfRel := urlToRel[selfURL]
	selfDir := path.Dir(selfRel)
	var discovered []string
	out := esmRefRE.ReplaceAllStringFunc(body, func(m string) string {
		sub := esmRefRE.FindStringSubmatch(m)
		pkg, ver, sub3 := sub[1], sub[2], sub[3]
		targetURL := "https://esm.sh/" + pkg + "@" + ver + sub3
		targetRel, ok := urlToRel[targetURL]
		if !ok {
			targetRel = localRel(pkg, strings.TrimPrefix(sub3, "/"))
			urlToRel[targetURL] = targetRel
		}
		discovered = append(discovered, targetURL)
		// Relative path from selfDir to targetRel; ensure it starts with ./
		rel, err := relPath(selfDir, targetRel)
		if err != nil {
			return m // leave untouched on the rare failure
		}
		// Emit the specifier WITHOUT the declaration extension. TS resolves
		// `'../x/shared'` → shared.d.ts on its own; pointing a module
		// specifier at a literal `.d.ts`/`.d.mts` triggers TS2846 ("a
		// declaration file cannot be imported without import type"). The
		// crawl target (discovered/urlToRel) keeps the real .d.ts path; only
		// the in-file specifier is de-extensioned.
		return stripDTSExt(rel)
	})
	return out, discovered
}

// stripDTSExt removes a trailing .d.ts / .d.mts / .d.cts from a module
// specifier so TS treats it as a module path, not a literal declaration
// import. ".d.mts" → "" leaves the bare path TS will re-resolve to the same
// declaration file.
func stripDTSExt(spec string) string {
	for _, ext := range []string{".d.ts", ".d.mts", ".d.cts"} {
		if strings.HasSuffix(spec, ext) {
			return strings.TrimSuffix(spec, ext)
		}
	}
	return spec
}

// localRel is the node_modules-relative path for a file in a package:
// "<pkg>/<sub>". pkg may be scoped ("@vue/runtime-dom").
func localRel(pkg, sub string) string {
	sub = strings.TrimPrefix(sub, "/")
	if sub == "" {
		sub = "index.d.ts"
	}
	return pkg + "/" + sub
}

// dtsEntryPath returns the package-relative path of the root .d.ts (the
// "types" entry) from its full esm.sh URL.
func dtsEntryPath(pkg, rootURL string) string {
	p := refPath(rootURL)
	if p == "" {
		return "index.d.ts"
	}
	return p
}

// refPkg / refPath split an esm.sh URL into its package and in-package path.
func refPkg(u string) string {
	if m := esmRefRE.FindStringSubmatch(u); m != nil {
		return m[1]
	}
	return ""
}

func refPath(u string) string {
	if m := esmRefRE.FindStringSubmatch(u); m != nil {
		return strings.TrimPrefix(m[3], "/")
	}
	return ""
}

// relPath returns a "./"- or "../"-prefixed relative path from fromDir to
// toRel (both node_modules-relative, slash-separated).
func relPath(fromDir, toRel string) (string, error) {
	if fromDir == "." || fromDir == "" {
		return "./" + toRel, nil
	}
	fromParts := strings.Split(fromDir, "/")
	toParts := strings.Split(toRel, "/")
	// common prefix
	i := 0
	for i < len(fromParts) && i < len(toParts)-1 && fromParts[i] == toParts[i] {
		i++
	}
	var b strings.Builder
	ups := len(fromParts) - i
	if ups == 0 {
		b.WriteString("./")
	}
	for j := 0; j < ups; j++ {
		b.WriteString("../")
	}
	b.WriteString(strings.Join(toParts[i:], "/"))
	return b.String(), nil
}

// packageJSON renders a minimal types-only package manifest.
func packageJSON(name, version, typesEntry string) string {
	return fmt.Sprintf(`{
  "name": %q,
  "version": %q,
  "types": "./%s",
  "_comment": "Types-only — generated by `+"`nexus types`"+` from nexus.lock for editor IntelliSense. nexus builds and runs zero-Node; this directory is not a build input."
}
`, name, version, typesEntry)
}
