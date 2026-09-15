package maskhook

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// MaskResponse renders v as JSON with the mask policy applied in ONE
// pass over the marshaled bytes. The tree pipeline (MaskValue + the
// caller's re-encode) cost every masked response two full encodes and
// a decode into a map[string]any tree; this costs one encode and a
// linear scan that copies bytes verbatim, rewriting only the integer
// spans the policy claims. A side benefit: the response keeps the
// struct's field order instead of the tree's alphabetical re-sort.
//
// ok=false means the caller should fall back to its normal encode:
// masking is off, v is nil or out of scope, or v doesn't marshal.
func MaskResponse(v any) ([]byte, bool) {
	if !Enabled() || v == nil || !typeInScope(v) {
		return nil, false
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	return maskJSONBytes(raw), true
}

// maskJSONBytes rewrites raw (well-formed JSON) with the mask policy:
// an integer value whose key context IsID becomes its masked string.
// Semantics mirror walk() exactly:
//
//   - an object value's key context is its own key;
//   - array elements inherit the key that introduced the array,
//     through any depth of nested arrays ("ids": [[1,2]] masks);
//   - a key the policy Skips suppresses masking for its whole
//     subtree, which is emitted verbatim;
//   - only integer literals convert — floats, strings, bools pass
//     through untouched (asInt's rule, enforced lexically).
func maskJSONBytes(raw []byte) []byte {
	var out bytes.Buffer
	out.Grow(len(raw) + len(raw)/8 + 32)

	type frame struct {
		obj     bool
		key     string // array/root: the key context elements inherit
		pendKey string // object: the key awaiting its value
		isKey   bool   // object: the next string is a key
		skip    bool   // inside a Skip()ed subtree — copy, never mask
	}
	// Virtual root frame behaves like an array with the empty key.
	stack := make([]frame, 1, 16)

	// leafCtx resolves the key context and skip flag for a value
	// appearing at the current position.
	leafCtx := func() (string, bool) {
		top := &stack[len(stack)-1]
		if top.obj {
			return top.pendKey, top.skip || skipKey(top.pendKey)
		}
		return top.key, top.skip
	}

	i, n := 0, len(raw)
	for i < n {
		switch ch := raw[i]; {
		case ch == '{':
			key, skip := leafCtx()
			stack = append(stack, frame{obj: true, key: key, isKey: true, skip: skip})
			out.WriteByte(ch)
			i++
		case ch == '[':
			key, skip := leafCtx()
			stack = append(stack, frame{key: key, skip: skip})
			out.WriteByte(ch)
			i++
		case ch == '}' || ch == ']':
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
			out.WriteByte(ch)
			i++
		case ch == ',':
			if top := &stack[len(stack)-1]; top.obj {
				top.isKey = true
				top.pendKey = ""
			}
			out.WriteByte(ch)
			i++
		case ch == ':':
			out.WriteByte(ch)
			i++
		case ch == '"':
			end := scanJSONString(raw, i)
			if top := &stack[len(stack)-1]; top.obj && top.isKey {
				top.pendKey = unquoteKey(raw[i:end])
				top.isKey = false
			}
			out.Write(raw[i:end])
			i = end
		case ch == '-' || (ch >= '0' && ch <= '9'):
			end := scanJSONNumber(raw, i)
			span := raw[i:end]
			if key, skip := leafCtx(); !skip && key != "" && isIntegerLiteral(span) {
				if id, err := strconv.ParseInt(string(span), 10, 64); err == nil {
					if s, ok := MaskID(key, id); ok {
						out.WriteByte('"')
						out.WriteString(s)
						out.WriteByte('"')
						i = end
						continue
					}
				}
			}
			out.Write(span)
			i = end
		case ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r':
			out.WriteByte(ch)
			i++
		default: // true / false / null
			start := i
			for i < n && raw[i] >= 'a' && raw[i] <= 'z' {
				i++
			}
			if i == start { // defensive: never loop on malformed input
				out.WriteByte(raw[i])
				i++
				continue
			}
			out.Write(raw[start:i])
		}
	}
	return out.Bytes()
}

// scanJSONString returns the index just past the closing quote of the
// string starting at raw[i] (which must be '"').
func scanJSONString(raw []byte, i int) int {
	j := i + 1
	for j < len(raw) {
		switch raw[j] {
		case '\\':
			j += 2
		case '"':
			return j + 1
		default:
			j++
		}
	}
	return len(raw)
}

// scanJSONNumber returns the index just past the number starting at
// raw[i].
func scanJSONNumber(raw []byte, i int) int {
	j := i
	if j < len(raw) && raw[j] == '-' {
		j++
	}
	for j < len(raw) {
		switch c := raw[j]; {
		case c >= '0' && c <= '9', c == '.', c == 'e', c == 'E', c == '+', c == '-':
			j++
		default:
			return j
		}
	}
	return len(raw)
}

// isIntegerLiteral reports whether span is a plain integer — no
// fraction, no exponent. Mirrors asInt's json.Number rule: an ID is
// never 1.5, and 1.0 stays a float.
func isIntegerLiteral(span []byte) bool {
	for _, c := range span {
		if c == '.' || c == 'e' || c == 'E' {
			return false
		}
	}
	return len(span) > 0
}

// unquoteKey decodes an object key token (quotes included). Keys with
// no escapes — the overwhelming norm for Go struct tags — slice the
// raw bytes without allocation beyond the string copy.
func unquoteKey(tok []byte) string {
	if bytes.IndexByte(tok, '\\') < 0 {
		return string(tok[1 : len(tok)-1])
	}
	var s string
	if err := json.Unmarshal(tok, &s); err != nil {
		return string(tok[1 : len(tok)-1])
	}
	return s
}
