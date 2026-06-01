package vue

import (
	"regexp"
	"strings"

	"github.com/paulmanoni/nexus/frontend/deps/bundler"
)

// styleBlockRE matches a whole <style ...>...</style> block. Non-greedy
// body so multiple blocks in one SFC are matched independently. CSS/SCSS
// never legitimately contains the literal "</style>", so the simple
// non-greedy match is safe without a full HTML parser.
var styleBlockRE = regexp.MustCompile(`(?is)<style([^>]*)>(.*?)</style>`)

// langAttrRE pulls the lang attribute value out of a tag's attribute
// string, tolerating single/double/unquoted forms.
var langAttrRE = regexp.MustCompile(`(?i)\blang\s*=\s*("([^"]*)"|'([^']*)'|([^\s>]+))`)

// preprocessSFCStyles compiles any inline <style lang="scss"|"sass">
// blocks through the system sass CLI and rewrites them to plain <style>
// blocks (lang attribute stripped) carrying the compiled CSS. This runs
// BEFORE the QuickJS adapter so:
//
//   - the adapter's compileStyle (which has no preprocessor) only ever
//     sees plain CSS, so scoping + url() hoisting work as usual, and
//   - the adapter's "lang not supported" guard never trips for scss/sass.
//
// fileDir is the .vue file's directory, passed to sass as a load-path so
// @use/@import inside the block resolve. Other langs (less, stylus, …)
// are left untouched — the adapter guard rejects those with a clear
// message until they're wired up too.
//
// On the first sass failure the original source is returned along with
// the error so the caller surfaces it as a build diagnostic.
func preprocessSFCStyles(source, fileDir string) (string, error) {
	var firstErr error
	out := styleBlockRE.ReplaceAllStringFunc(source, func(block string) string {
		if firstErr != nil {
			return block
		}
		m := styleBlockRE.FindStringSubmatch(block)
		attrs, body := m[1], m[2]
		lm := langAttrRE.FindStringSubmatch(attrs)
		if lm == nil {
			return block // no lang → plain css, nothing to do
		}
		lang := strings.ToLower(firstNonEmpty(lm[2], lm[3], lm[4]))
		if lang != "scss" && lang != "sass" {
			return block // less/stylus/etc — leave for the adapter guard
		}
		css, err := bundler.CompileSassSource(body, fileDir, lang == "sass")
		if err != nil {
			firstErr = err
			return block
		}
		// Strip the lang attribute; keep everything else (scoped,
		// module would have been guarded earlier, etc.).
		newAttrs := langAttrRE.ReplaceAllString(attrs, "")
		return "<style" + newAttrs + ">\n" + css + "</style>"
	})
	if firstErr != nil {
		return source, firstErr
	}
	return out, nil
}

// PreprocessSource runs the same pre-compile rewrites the esbuild SFC
// Plugin applies before handing source to the compiler: Vuetify
// auto-import (inject `<v-*>` component imports) followed by inline
// <style lang="scss"|"sass"> compilation. fileDir is the .vue file's
// directory (sass load-path). Exported so the unbundled dev server
// (frontend/devserver) compiles .vue identically to `nexus build` instead
// of duplicating the sequence. Returns the rewritten source; a sass error
// is returned with the (partially) rewritten source so the caller can
// surface it.
func PreprocessSource(source, fileDir string) (string, error) {
	rewritten, _ := bundler.VuetifyAutoImport(source)
	return preprocessSFCStyles(rewritten, fileDir)
}

// firstNonEmpty returns the first non-empty string among its args, or "".
func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
