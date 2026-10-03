package main

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/paulmanoni/deco/transpiler"
)

// nexus annotations are Go directives: //nexus:rest GET /users/:id. A line
// matching //[a-z0-9]+:[a-z0-9] is what gofmt and go/doc treat as a tool
// directive — gofmt leaves it unspaced (it rewrites v1's //@rest as
// // @rest) and moves it below the doc prose, go/doc hides it from rendered
// documentation, and the namespace cannot collide with another tool's
// @-annotations. The grammar after the prefix is v1's.
const directivePrefix = "nexus:"

// legacyDirectivePrefix is the v1 spelling (//@rest, or gofmt's // @rest).
// The scan still reads it, only to reject nexus keywords written that way
// with a pointer to `nexus migrate v2`; other tools' @-annotations (swag's
// // @Summary, for one) pass through untouched.
const legacyDirectivePrefix = "@"

// directiveScanOptions is the deco scan configuration every handler scan
// uses: //nexus: first, then the legacy prefix so it can be diagnosed.
var directiveScanOptions = transpiler.ScanOptions{Prefixes: []string{directivePrefix, legacyDirectivePrefix}}

// splitDirective returns the prefix and fields of a comment line that holds
// a //nexus: or legacy //@ directive; fields is empty for any other line.
// Matching tolerates a space after the slashes (as the deco scan does) so
// the spelling check can name the mistake.
func splitDirective(text string) (prefix string, fields []string) {
	content := strings.TrimSpace(strings.TrimLeft(text, "/"))
	for _, p := range directiveScanOptions.Prefixes {
		if rest, ok := strings.CutPrefix(content, p); ok {
			return p, strings.Fields(rest)
		}
	}
	return "", nil
}

// directiveSpelling decides what to do with one scanned directive: keep a
// well-formed //nexus:kw (true, nil); drop another tool's @-annotation
// (false, nil); reject a nexus keyword in the v1 //@ spelling, or a
// "// nexus:kw" whose space makes it prose rather than a directive (false,
// error naming file:line).
func directiveSpelling(file string, line int, prefix, kw, text string) (keep bool, err error) {
	switch prefix {
	case legacyDirectivePrefix:
		if !legacyNexusKeyword(kw) {
			return false, nil
		}
		return false, fmt.Errorf("%s:%d: //@%s is the nexus v1 annotation spelling — v2 reads Go directives: write //nexus:%s (run `nexus migrate v2` to rewrite every annotation)",
			displayRel(file), line, kw, kw)
	case directivePrefix:
		if !strings.HasPrefix(text, "//"+directivePrefix) {
			return false, fmt.Errorf("%s:%d: %q is not a Go directive (gofmt and go doc treat it as prose) — write //nexus:%s with no space after the slashes",
				displayRel(file), line, strings.TrimSpace(text), kw)
		}
		return true, nil
	}
	return false, nil
}

// legacyNexusKeyword reports whether //@kw was a nexus v1 annotation: a
// built-in keyword, or a qualified custom decorator naming an exported
// function (//@inertia.Page). Lowercase-qualified words (swag's
// @contact.name) and every other keyword belong to other tools.
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
