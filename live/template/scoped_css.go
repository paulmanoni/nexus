package template

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// computeScopeID derives a short, stable identifier for one
// component's scoped-style namespace. Stable means a given
// component name produces the same ID across process restarts
// and across pods — important for hot-reload (the client's
// existing tree keeps matching) and split deployments (every
// pod stamps the same attribute).
//
// 8 hex chars from a SHA-256 of the name gives 32 bits of
// entropy. Plenty to avoid collisions between the dozens of
// components a typical app has; not security-bearing — the
// scope attribute is a CSS selector helper, not auth.
func computeScopeID(componentName string) string {
	sum := sha256.Sum256([]byte(componentName))
	return hex.EncodeToString(sum[:4])
}

// rewriteScopedCSS prefixes every top-level selector in body
// with [data-nl-scope="<id>"] so the rules only match elements
// inside the component's SSR container.
//
// State-machine implementation walks the body tracking brace
// depth: selectors live at depth 0 (between rule boundaries),
// declarations at depth 1+. Comma-separated selector lists are
// rewritten per-part so `a, b { … }` → `[…] a, […] b { … }`.
//
// Limitations:
//   - Nested at-rule bodies (@media { .x { … } }) aren't
//     rewritten — the inner .x stays unscoped. Common-enough
//     case to fix in v2; v1 callers can wrap @media rules in
//     a top-level scoped selector or just live with global
//     reach within media queries.
//   - CSS strings/comments aren't tokenized; a `{` or `}`
//     inside a content: "…" value will desync the parser. Same
//     v2 caveat.
//
// Empty body returns empty (no-op for components without styles).
func rewriteScopedCSS(body, scopeID string) string {
	if body == "" {
		return ""
	}
	prefix := `[data-nl-scope="` + scopeID + `"] `
	var out strings.Builder
	out.Grow(len(body) + 64)
	var sel strings.Builder
	depth := 0
	for _, c := range body {
		switch c {
		case '{':
			if depth == 0 {
				writeScopedSelector(&out, sel.String(), prefix)
				sel.Reset()
			}
			out.WriteRune(c)
			depth++
		case '}':
			if depth > 0 {
				depth--
			}
			out.WriteRune(c)
		default:
			if depth == 0 {
				sel.WriteRune(c)
			} else {
				out.WriteRune(c)
			}
		}
	}
	// Trailing selector with no body (malformed but tolerate).
	if s := strings.TrimSpace(sel.String()); s != "" {
		writeScopedSelector(&out, sel.String(), prefix)
	}
	return out.String()
}

// writeScopedSelector emits one selector list (the text between
// the previous } and the next {). Splits on commas, trims, and
// prefixes each part — except at-rules (@media / @keyframes /
// @supports / @font-face / etc.), which pass through unchanged
// because they don't take an element-matching selector.
func writeScopedSelector(out *strings.Builder, raw, prefix string) {
	// Preserve leading whitespace for human-readable output.
	leadingWS := ""
	for _, c := range raw {
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			leadingWS += string(c)
		} else {
			break
		}
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		out.WriteString(raw)
		return
	}
	out.WriteString(leadingWS)
	parts := strings.Split(trimmed, ",")
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if i > 0 {
			out.WriteString(", ")
		}
		if strings.HasPrefix(p, "@") {
			out.WriteString(p)
		} else {
			out.WriteString(prefix)
			out.WriteString(p)
		}
	}
	// Whitespace between the selector and the opening { — matches
	// the original space the user wrote.
	out.WriteByte(' ')
}
