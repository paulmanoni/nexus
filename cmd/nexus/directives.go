package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/paulmanoni/deco/transpiler"
)

// nexus annotations have two spellings in v1.80:
//
//	//nexus:rest GET /users/:id   Go's directive form — the nexus 2.0 spelling
//	//@rest GET /users/:id        the v1 form (gofmt rewrites it as // @rest)
//
// Both register the same thing; the grammar after the prefix is identical.
// //nexus: lines are what gofmt and go/doc treat as tool directives (gofmt
// leaves them unspaced, go doc hides them), and the namespace can't collide
// with another tool's @-annotations. nexus 2.0 reads only //nexus:, so every
// //@ annotation is reported (see legacyDirectives); `nexus migrate v2`
// rewrites them.
const (
	directivePrefix       = "nexus:"
	legacyDirectivePrefix = "@"
)

// directiveScanOptions is the deco scan configuration every handler scan
// uses: //nexus: first, then the v1 //@ spelling.
var directiveScanOptions = transpiler.ScanOptions{Prefixes: []string{directivePrefix, legacyDirectivePrefix}}

// splitDirective returns the prefix and fields of a comment line holding a
// //nexus: or //@ directive; fields is empty for any other line. A space
// after the slashes is tolerated here (gofmt's // @x) so the spelling check
// can name the mistake.
func splitDirective(text string) (prefix string, fields []string) {
	content := strings.TrimSpace(strings.TrimLeft(text, "/"))
	for _, p := range directiveScanOptions.Prefixes {
		if rest, ok := strings.CutPrefix(content, p); ok {
			return p, strings.Fields(rest)
		}
	}
	return "", nil
}

// directiveSpelling decides what to do with one scanned directive whose
// keyword is kw. keep is false for a line that is not a nexus annotation.
//
//   - //nexus:kw must be written with no space after the slashes: a spaced
//     "// nexus:kw" is prose to gofmt and go doc, so it is an error when kw
//     is a nexus keyword and ignored otherwise. kw must be a nexus keyword
//     or a qualified pkg.Func decorator: the namespace is nexus's.
//   - //@kw keeps its v1 rules (a nexus keyword or a qualified decorator is
//     read, a near-typo of a nexus keyword is an error, anything else is
//     another tool's) and is recorded as a legacy spelling.
func directiveSpelling(file string, line int, prefix, kw, text string) (keep bool, err error) {
	switch prefix {
	case directivePrefix:
		if !strings.HasPrefix(strings.TrimSpace(text), "//"+directivePrefix) {
			if !legacyNexusKeyword(kw) {
				return false, nil // prose that happens to start with "nexus:"
			}
			return false, fmt.Errorf("%s:%d: %q is not a Go directive (gofmt and go doc treat it as prose) — write //nexus:%s with no space after the slashes",
				displayRel(file), line, strings.TrimSpace(text), kw)
		}
		return true, nil
	case legacyDirectivePrefix:
		if !builtinHandlerKeyword(kw) && !strings.Contains(kw, ".") {
			if want, close := nearHandlerKeyword(kw); close {
				return false, fmt.Errorf("%s:%d: unknown annotation //@%s — did you mean //nexus:%s? (//@%s in the v1 spelling)",
					displayRel(file), line, kw, want, want)
			}
			return false, nil // another tool's annotation
		}
		recordLegacyDirective(file, line, kw)
		return true, nil
	}
	return false, nil
}

// unknownDirective is the error for an unqualified //nexus: keyword nexus
// does not define — with a did-you-mean when it is a probable typo.
func unknownDirective(file string, line int, kw, on string) error {
	if on == "" {
		on = "the package doc"
	}
	if want, close := nearHandlerKeyword(kw); close {
		return fmt.Errorf("%s:%d: unknown annotation //nexus:%s on %s — did you mean //nexus:%s?",
			displayRel(file), line, kw, on, want)
	}
	return fmt.Errorf("%s:%d: unknown annotation //nexus:%s on %s — nexus directives are %s, or a custom decorator //nexus:pkg.Func",
		displayRel(file), line, kw, on, strings.Join(handlerKeywords, ", "))
}

// legacyNexusKeyword reports whether //@kw is a nexus annotation, for tools
// that report the v1 spelling outside the handler scan (nexus lint --v2): a
// built-in keyword, or a qualified decorator naming an exported function
// (//@inertia.Page). Lowercase-qualified words (swag's @contact.name) and
// every other keyword belong to other tools.
func legacyNexusKeyword(kw string) bool {
	if builtinHandlerKeyword(kw) || typeDirectiveKeywords[kw] {
		return true
	}
	pkg, fn, ok := strings.Cut(kw, ".")
	if !ok || pkg == "" || fn == "" || strings.Contains(fn, ".") {
		return false
	}
	return isIdent(pkg) && isIdent(fn) && unicode.IsUpper(rune(fn[0]))
}

func isIdent(s string) bool {
	for i, r := range s {
		if !(r == '_' || unicode.IsLetter(r) || (i > 0 && unicode.IsDigit(r))) {
			return false
		}
	}
	return s != ""
}

// legacyDirectives collects the //@ annotations the handler scans read, so
// the CLI can warn about each once per process (a `nexus dev` session
// rescans on every save).
var legacyDirectives = struct {
	sync.Mutex
	found    map[string]string // "file:line" → keyword
	reported map[string]bool
}{found: map[string]string{}, reported: map[string]bool{}}

func recordLegacyDirective(file string, line int, kw string) {
	legacyDirectives.Lock()
	defer legacyDirectives.Unlock()
	legacyDirectives.found[fmt.Sprintf("%s:%d", displayRel(file), line)] = kw
}

// reportLegacyDirectives prints the //@ annotations not reported yet, as one
// block. A project with none prints nothing.
func reportLegacyDirectives(w io.Writer) {
	legacyDirectives.Lock()
	defer legacyDirectives.Unlock()
	var sites []string
	for at := range legacyDirectives.found {
		if !legacyDirectives.reported[at] {
			sites = append(sites, at)
		}
	}
	if len(sites) == 0 {
		return
	}
	sort.Slice(sites, func(i, j int) bool { return sitesLess(sites[i], sites[j]) })
	fmt.Fprintf(w, "nexus: v2 notice — %d annotation(s) use the v1 //@ spelling; nexus 2.0 reads only //nexus:x (v1.80 accepts both; `nexus migrate v2` rewrites them):\n", len(sites))
	const max = 10
	for i, at := range sites {
		legacyDirectives.reported[at] = true
		if i < max {
			kw := legacyDirectives.found[at]
			fmt.Fprintf(w, "  %s: //@%s → //nexus:%s\n", at, kw, kw)
		}
	}
	if len(sites) > max {
		fmt.Fprintf(w, "  … and %d more (`nexus lint --v2` lists them all)\n", len(sites)-max)
	}
}

// sitesLess orders "file:line" strings by file, then numerically by line.
func sitesLess(a, b string) bool {
	ai, bi := strings.LastIndexByte(a, ':'), strings.LastIndexByte(b, ':')
	if a[:ai] != b[:bi] {
		return a[:ai] < b[:bi]
	}
	var la, lb int
	fmt.Sscan(a[ai+1:], &la)
	fmt.Sscan(b[bi+1:], &lb)
	return la < lb
}
