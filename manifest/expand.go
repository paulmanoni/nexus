package manifest

import (
	"bytes"
	"fmt"
	"os"
	"strings"
)

// expandEnvVars walks the raw TOML bytes and replaces every ${VAR}
// or ${VAR:default} token that appears inside a basic-string value
// with its resolved value. Tokens outside basic strings — table
// headers, key names, raw scalars, literal strings ('...' / '''...'''),
// and comments — are left untouched.
//
// Resolution rules (strict mode — undefined vars without a default
// fail the load):
//
//	${KEY}            - must be set + non-empty; otherwise error
//	${KEY:default}    - falls back to literal default when env empty/unset
//	${A:${B:c}}       - one level of nesting; resolved inside-out
//	$${X}             - escape; produces literal ${X} in the output
//
// Literal-string contexts ('...' / '''...''') deliberately bypass
// expansion — that mirrors TOML's own "literal means raw" promise and
// gives operators an escape hatch when they want a literal `${X}` in
// a path or message.
//
// Returns the expanded byte slice ready for toml.Unmarshal. Errors
// carry the 1-based line number of the failing token so callers can
// surface a useful diagnostic.
func expandEnvVars(raw []byte) ([]byte, error) {
	var out bytes.Buffer
	out.Grow(len(raw))

	const (
		stateNormal = iota
		stateBasic        // "..."
		stateLiteral      // '...'
		stateMultiBasic   // """..."""
		stateMultiLiteral // '''...'''
		stateComment      // # to EOL
	)
	state := stateNormal
	line := 1
	i := 0

	// processString reads `[start, end)` from raw (basic-string content,
	// no surrounding quotes), expanding every ${...} token. Returns the
	// expanded byte slice ready to be wrapped in its original quotes.
	// Lines inside the content are counted for error reporting.
	processString := func(content []byte, startLine int) ([]byte, error) {
		var inner bytes.Buffer
		inner.Grow(len(content))
		curLine := startLine
		j := 0
		for j < len(content) {
			// Escape: $${X} → literal ${X}. Detect a single $ followed
			// by $ followed by { — we emit the first $ and skip the
			// second so the {X} pair is written as-is by the next loop
			// iteration.
			if j+2 < len(content) && content[j] == '$' && content[j+1] == '$' && content[j+2] == '{' {
				inner.WriteByte('$')
				j += 2
				continue
			}
			if j+1 < len(content) && content[j] == '$' && content[j+1] == '{' {
				close := findCloseBrace(content, j+2)
				if close < 0 {
					return nil, fmt.Errorf("line %d: unterminated ${...} token", curLine)
				}
				expr := string(content[j+2 : close])
				val, err := resolveExpr(expr)
				if err != nil {
					return nil, fmt.Errorf("line %d: %w", curLine, err)
				}
				// Re-escape the resolved value for the surrounding
				// basic-string context: \ and " must be escaped or the
				// TOML parser would mis-tokenize the substitution.
				val = strings.ReplaceAll(val, `\`, `\\`)
				val = strings.ReplaceAll(val, `"`, `\"`)
				inner.WriteString(val)
				j = close + 1
				continue
			}
			if content[j] == '\n' {
				curLine++
			}
			// Inside a basic string, an escape sequence \X consumes
			// both bytes. We pass them through as-is — the TOML parser
			// will decode them.
			if content[j] == '\\' && j+1 < len(content) {
				inner.WriteByte(content[j])
				inner.WriteByte(content[j+1])
				j += 2
				continue
			}
			inner.WriteByte(content[j])
			j++
		}
		return inner.Bytes(), nil
	}

	for i < len(raw) {
		c := raw[i]

		switch state {
		case stateNormal:
			switch {
			case c == '\n':
				out.WriteByte(c)
				line++
				i++
			case c == '#':
				out.WriteByte(c)
				state = stateComment
				i++
			case c == '"':
				// Distinguish basic from multi-line basic. The TOML
				// spec says """ opens a multi-line string; everything
				// else is the regular "..." form.
				if i+2 < len(raw) && raw[i+1] == '"' && raw[i+2] == '"' {
					end := findMultilineEnd(raw, i+3, `"""`)
					if end < 0 {
						return nil, fmt.Errorf("line %d: unterminated \"\"\"...\"\"\" string", line)
					}
					content := raw[i+3 : end]
					expanded, err := processString(content, line)
					if err != nil {
						return nil, err
					}
					out.WriteString(`"""`)
					out.Write(expanded)
					out.WriteString(`"""`)
					line += bytes.Count(raw[i:end+3], []byte{'\n'})
					i = end + 3
				} else {
					// Single-line basic string. Find the closing quote
					// honoring \" escapes. Newlines inside a "..." are
					// invalid TOML so we bail if we hit one.
					end := findBasicStringEnd(raw, i+1)
					if end < 0 {
						return nil, fmt.Errorf("line %d: unterminated \"...\" string", line)
					}
					content := raw[i+1 : end]
					expanded, err := processString(content, line)
					if err != nil {
						return nil, err
					}
					out.WriteByte('"')
					out.Write(expanded)
					out.WriteByte('"')
					i = end + 1
				}
			case c == '\'':
				// Literal string — content passes through unmodified.
				// TOML spec: no escapes, raw bytes between the quotes.
				// This is the operator's escape hatch for literal
				// `${X}` text.
				if i+2 < len(raw) && raw[i+1] == '\'' && raw[i+2] == '\'' {
					end := findMultilineEnd(raw, i+3, `'''`)
					if end < 0 {
						return nil, fmt.Errorf("line %d: unterminated '''...''' string", line)
					}
					out.Write(raw[i : end+3])
					line += bytes.Count(raw[i:end+3], []byte{'\n'})
					i = end + 3
				} else {
					end := findLiteralStringEnd(raw, i+1)
					if end < 0 {
						return nil, fmt.Errorf("line %d: unterminated '...' string", line)
					}
					out.Write(raw[i : end+1])
					i = end + 1
				}
			default:
				out.WriteByte(c)
				i++
			}
		case stateComment:
			out.WriteByte(c)
			if c == '\n' {
				line++
				state = stateNormal
			}
			i++
		}
	}

	return out.Bytes(), nil
}

// findBasicStringEnd returns the index of the closing `"` of a basic
// (single-line) TOML string starting at `start`. Honors `\"` escapes.
// Returns -1 if the string is not terminated or runs past a newline
// (which TOML forbids for basic strings).
func findBasicStringEnd(raw []byte, start int) int {
	for i := start; i < len(raw); i++ {
		switch raw[i] {
		case '\\':
			i++ // skip escaped byte
		case '"':
			return i
		case '\n':
			return -1
		}
	}
	return -1
}

// findLiteralStringEnd returns the index of the closing `'` of a
// literal (single-line) TOML string starting at `start`. No escape
// handling — TOML literal strings are raw.
func findLiteralStringEnd(raw []byte, start int) int {
	for i := start; i < len(raw); i++ {
		if raw[i] == '\'' {
			return i
		}
		if raw[i] == '\n' {
			return -1
		}
	}
	return -1
}

// findMultilineEnd returns the byte offset of the opening `"""` or
// `'''` of the terminator. Both flavours are 3 bytes long. start is
// the byte after the opener.
func findMultilineEnd(raw []byte, start int, terminator string) int {
	t := []byte(terminator)
	for i := start; i+len(t) <= len(raw); i++ {
		// For basic """ we need to honor \\ escapes so \""" inside
		// the body doesn't close prematurely. For literal ''' there
		// are no escapes per TOML spec.
		if terminator == `"""` && raw[i] == '\\' && i+1 < len(raw) {
			i++ // skip escaped byte (loop +1 jumps past it)
			continue
		}
		if bytes.Equal(raw[i:i+len(t)], t) {
			return i
		}
	}
	return -1
}

// findCloseBrace returns the index of the matching `}` for a `${`
// opener whose contents start at `start`. Brace nesting is tracked so
// `${A:${B:c}}` resolves to the OUTER `}`. Returns -1 for unterminated
// tokens or tokens that span a newline (those are syntax errors).
func findCloseBrace(content []byte, start int) int {
	depth := 1
	for i := start; i < len(content); i++ {
		switch content[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		case '\n':
			return -1
		}
	}
	return -1
}

// resolveExpr resolves a single `${...}` expression body (the bytes
// between `${` and the matching `}`). Handles the KEY-vs-KEY:default
// split and recurses into nested expressions inside the default
// branch.
//
// Empty env values count as "unset" — same as bash's `${X:-default}`
// semantics — so an exported-but-empty `APP_DOMAIN=` falls through to
// its default. The alternative ("any defined env counts, even empty")
// would let a typo in a deploy script silently swap a real default
// for an empty string.
func resolveExpr(expr string) (string, error) {
	var key, def string
	hasDefault := false
	if idx := strings.Index(expr, ":"); idx >= 0 {
		key = expr[:idx]
		def = expr[idx+1:]
		hasDefault = true
	} else {
		key = expr
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", fmt.Errorf("empty variable name in ${%s}", expr)
	}
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v, nil
	}
	if hasDefault {
		return expandPlaceholdersIn(def)
	}
	return "", fmt.Errorf("env var %s is not set (use ${%s:default} to provide a fallback)", key, key)
}

// expandPlaceholdersIn recursively expands `${...}` tokens inside a
// default-value string. Used only for the default branch of a parent
// `${X:default}` — the top-level pass already handles the file body.
// Errors propagate up so an unresolvable nested default surfaces at
// the same load step as a top-level miss.
func expandPlaceholdersIn(s string) (string, error) {
	var out strings.Builder
	i := 0
	for i < len(s) {
		if i+2 < len(s) && s[i] == '$' && s[i+1] == '$' && s[i+2] == '{' {
			out.WriteByte('$')
			i += 2
			continue
		}
		if i+1 < len(s) && s[i] == '$' && s[i+1] == '{' {
			close := findCloseBrace([]byte(s), i+2)
			if close < 0 {
				return "", fmt.Errorf("unterminated ${...} in default expression %q", s)
			}
			val, err := resolveExpr(s[i+2 : close])
			if err != nil {
				return "", err
			}
			out.WriteString(val)
			i = close + 1
			continue
		}
		out.WriteByte(s[i])
		i++
	}
	return out.String(), nil
}

// ExpandEnvVars is the public alias for expandEnvVars — same rules,
// same behavior, exported so extensions outside this package
// (notably extension/config) can apply the placeholder syntax to
// their own TOML files. Keeps `${VAR}` / `${VAR:default}` semantics
// identical across every TOML the framework reads — operators
// learn one rule, not three.
func ExpandEnvVars(raw []byte) ([]byte, error) { return expandEnvVars(raw) }
