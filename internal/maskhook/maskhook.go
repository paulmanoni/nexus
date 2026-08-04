// Package maskhook is the framework-side seam for opaque ID masking.
//
// It holds no policy and no cryptography — only the hook slot that
// extension/maskid fills in, plus the JSON tree walkers every transport
// shares. Keeping it a leaf (stdlib only) lets the REST, GraphQL, Inertia
// and WebSocket paths consult it without any of them importing the
// extension, which would cycle.
//
// When no extension is installed every entry point is a nil-pointer load
// and an immediate return, so an app that never enables masking pays
// nothing beyond one atomic read per response.
package maskhook

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"sync/atomic"
)

// Hooks is the policy + codec pair installed by extension/maskid.
//
// IsID decides whether a JSON key names an ID worth masking; Mask and
// Unmask convert between the wire form and the integer the application
// sees. Mask and Unmask are handed the key too — the built-in codec
// ignores it, but a custom one can use it to derive per-field material.
type Hooks struct {
	IsID   func(key string) bool
	Mask   func(key string, n int64) (string, bool)
	Unmask func(key, s string) (int64, bool)

	// TypeAllowed scopes *masking* to a set of response types, named by
	// their Go type. Nil means every type. Unmasking is deliberately
	// never scoped: a value only converts if it decrypts, which only
	// happens for a mask this app minted, so an unscoped type's plain
	// integer passes through untouched either way.
	//
	// The scope exists because masking is not always safe app-wide. An
	// app whose IDs also travel to a system outside it — a legacy
	// backend the same SPA calls — can mask the types that stay inside
	// and leave the rest alone.
	TypeAllowed func(typeName string) bool
}

var active atomic.Pointer[Hooks]

// Install turns masking on process-wide. Called once at boot by
// maskid.Module; calling it again replaces the policy.
func Install(h Hooks) { active.Store(&h) }

// Uninstall turns masking back off. Mainly for tests.
func Uninstall() { active.Store(nil) }

// Enabled reports whether an extension is installed. Every hot path
// checks this first so the disabled case costs one atomic load.
func Enabled() bool { return active.Load() != nil }

// MaskID masks a single integer as the named field would be masked. It
// returns ok=false when masking is off or the field isn't an ID, so the
// caller can leave the value untouched.
func MaskID(key string, n int64) (string, bool) {
	h := active.Load()
	if h == nil || !h.IsID(key) {
		return "", false
	}
	return h.Mask(key, n)
}

// TypeAllowed reports whether masking applies to the named response
// type. True for everything when no scope is configured.
func TypeAllowed(typeName string) bool {
	h := active.Load()
	if h == nil {
		return false
	}
	return h.TypeAllowed == nil || h.TypeAllowed(typeName)
}

// rootTypeName names the Go type a response value carries, looking
// through pointers, slices and the wrapper generics apps like to return
// (Response[T], Page[T]) down to the first named struct. Anonymous and
// map-shaped values yield "", which no scope matches — so a scoped app
// masks only what it named.
func rootTypeName(v any) string {
	t := reflect.TypeOf(v)
	for i := 0; t != nil && i < 8; i++ {
		switch t.Kind() {
		case reflect.Ptr, reflect.Slice, reflect.Array:
			t = t.Elem()
			continue
		}
		break
	}
	if t == nil {
		return ""
	}
	return t.Name()
}

// IsIDKey reports whether the installed policy treats key as an ID
// field. Schema builders use it to decide a field's declared type,
// which is a boot-time decision rather than a per-value one.
func IsIDKey(key string) bool {
	h := active.Load()
	return h != nil && h.IsID(key)
}

// Encode / Decode reach the codec directly, skipping the field policy.
// Used where the mask/no-mask decision was already made at schema-build
// time (the GraphQL MaskedID scalar) rather than per value.
func Encode(n int64) (string, bool) {
	h := active.Load()
	if h == nil {
		return "", false
	}
	return h.Mask("", n)
}

func Decode(s string) (int64, bool) {
	h := active.Load()
	if h == nil {
		return 0, false
	}
	return h.Unmask("", s)
}

// UnmaskParams applies UnmaskParam across a repeated parameter.
func UnmaskParams(key string, vals []string) []string {
	if !Enabled() || len(vals) == 0 {
		return vals
	}
	out := vals
	for i, v := range vals {
		u := UnmaskParam(key, v)
		if u == v {
			continue
		}
		if &out[0] == &vals[0] {
			out = append([]string(nil), vals...)
		}
		out[i] = u
	}
	return out
}

// UnmaskID reverses MaskID.
func UnmaskID(key, s string) (int64, bool) {
	h := active.Load()
	if h == nil || !h.IsID(key) {
		return 0, false
	}
	return h.Unmask(key, s)
}

// MaskValue round-trips an arbitrary handler result through JSON and
// replaces every ID-shaped leaf with its masked form.
//
// Working on the JSON tree rather than the Go value is deliberate: the
// keys a policy matches on are exactly the keys the client sees, and it
// sidesteps unexported fields, interface indirection and cycles. The
// returned value is a plain any tree ready to marshal.
//
// A value that can't be marshalled (or masking being off) yields the
// original untouched — masking must never turn a working response into
// an error.
func MaskValue(v any) any {
	if !Enabled() || v == nil || !TypeAllowed(rootTypeName(v)) {
		return v
	}
	tree, err := toTree(v)
	if err != nil {
		return v
	}
	return walk(tree, "", maskLeaf)
}

// MaskProps masks a prop map in place. Each value is an arbitrary Go
// value (a struct, a slice of rows, a scalar), so it goes through the
// same marshal-and-walk as MaskValue, but rooted at its own prop name —
// a prop literally called "id" masks, one called "users" masks the ids
// inside it. Used by the Inertia renderer.
func MaskProps(props map[string]any) {
	if !Enabled() {
		return
	}
	for k, v := range props {
		if !TypeAllowed(rootTypeName(v)) {
			continue
		}
		tree, err := toTree(v)
		if err != nil {
			continue
		}
		props[k] = walk(tree, k, maskLeaf)
	}
}

// UnmaskJSON rewrites a request body, turning masked ID strings back
// into the numbers the handler's args struct expects. A body that isn't
// valid JSON is returned unchanged — binding will report that better
// than we can.
func UnmaskJSON(body []byte) []byte {
	if !Enabled() || len(bytes.TrimSpace(body)) == 0 {
		return body
	}
	tree, err := decode(body)
	if err != nil {
		return body
	}
	out, err := json.Marshal(walk(tree, "", unmaskLeaf))
	if err != nil {
		return body
	}
	return out
}

// UnmaskArgs rewrites a decoded argument map (GraphQL's p.Args, a form,
// a query string) in place.
func UnmaskArgs(args map[string]any) {
	if !Enabled() || len(args) == 0 {
		return
	}
	for k, v := range args {
		args[k] = walk(v, k, unmaskLeaf)
	}
}

// UnmaskParam converts one raw string parameter — a path segment, a
// query value — back to its numeric form. Comma-joined lists
// ("?ids=aB…,cD…") are unmasked element-wise, which is the shape
// list filters take. Anything that doesn't unmask is returned as-is, so
// a genuinely numeric or non-ID parameter passes straight through.
func UnmaskParam(key, value string) string {
	if !Enabled() || value == "" {
		return value
	}
	if !strings.Contains(value, ",") {
		if n, ok := UnmaskID(key, value); ok {
			return itoa(n)
		}
		return value
	}
	parts := strings.Split(value, ",")
	changed := false
	for i, p := range parts {
		if n, ok := UnmaskID(key, strings.TrimSpace(p)); ok {
			parts[i] = itoa(n)
			changed = true
		}
	}
	if !changed {
		return value
	}
	return strings.Join(parts, ",")
}

// walk recurses a decoded JSON tree applying fn to every scalar leaf.
// key is the nearest enclosing object key — slice elements inherit it,
// so `"ids": [1, 2]` masks like `"id": 1` would.
func walk(v any, key string, fn func(key string, v any) any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			t[k] = walk(child, k, fn)
		}
		return t
	case []any:
		for i, child := range t {
			t[i] = walk(child, key, fn)
		}
		return t
	default:
		return fn(key, v)
	}
}

func maskLeaf(key string, v any) any {
	n, ok := asInt(v)
	if !ok {
		return v
	}
	if s, ok := MaskID(key, n); ok {
		return s
	}
	return v
}

func unmaskLeaf(key string, v any) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	if n, ok := UnmaskID(key, s); ok {
		return json.Number(itoa(n))
	}
	return v
}

// asInt accepts the numeric shapes a JSON decode can produce. Fractional
// values are rejected: an ID is never 1.5, and refusing them keeps the
// policy from mangling a coincidentally-named float.
func asInt(v any) (int64, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		if n != float64(int64(n)) {
			return 0, false
		}
		return int64(n), true
	default:
		return 0, false
	}
}

// toTree marshals a Go value and decodes it back with UseNumber, so a
// 64-bit ID survives the round trip that a plain float64 decode would
// silently truncate past 2^53.
func toTree(v any) (any, error) {
	blob, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return decode(blob)
}

func decode(blob []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(blob))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	var buf [20]byte
	i := len(buf)
	u := uint64(n)
	if neg {
		u = uint64(-n)
	}
	for u > 0 {
		i--
		buf[i] = byte('0' + u%10)
		u /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
