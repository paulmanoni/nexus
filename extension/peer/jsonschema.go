package peer

import (
	"reflect"
	"strings"
)

// Schema is the subset of JSON Schema 2020-12 the peer extension
// emits and consumes. Held intentionally small: type / required /
// properties / items / $ref / $defs covers every structural drift
// failure mode the framework cares about without dragging in the
// validation-keyword zoo (pattern, format, oneOf, allOf, conditional
// subschemas, etc.) — those belong in user-level validation
// libraries, not in a wire-shape drift check.
//
// The JSON tags match the JSON-Schema spec field names so the on-
// wire document is recognized by any standard JSON-Schema viewer
// or validator that operators already use.
type Schema struct {
	Type       string             `json:"type,omitempty"`       // object | array | string | integer | number | boolean | null
	Format     string             `json:"format,omitempty"`     // date-time | uuid | …  (informational; not enforced)
	Required   []string           `json:"required,omitempty"`   // names of required object properties
	Properties map[string]*Schema `json:"properties,omitempty"` // object field schemas
	Items      *Schema            `json:"items,omitempty"`      // array element schema
	Ref        string             `json:"$ref,omitempty"`       // "#/$defs/<name>" — cycle break + reuse
	Defs       map[string]*Schema `json:"$defs,omitempty"`      // populated only on the root schema
}

// schemaBuilder is the per-call state for converting a reflect.Type
// into a Schema. Defs is shared across the whole reflective walk so
// recursive struct references collapse to $refs instead of blowing
// the stack on a self-referential type.
type schemaBuilder struct {
	defs map[string]*Schema
	// seen tracks the names currently being built so a cycle
	// (e.g. type Tree struct{ Children []*Tree }) emits a $ref
	// on the recursive descent instead of recursing forever.
	seen map[string]bool
}

// ReflectSchema turns a Go type into the framework's Schema shape.
// The top-level result carries the $defs map; nested struct
// references inside it are $ref strings pointing into that map.
// Pass nil for "no type" (void / error-only handlers) — the
// caller-side check just skips the comparison when either side
// is nil.
//
// The root never $refs itself even when it's a named struct —
// inlining the root keeps the document self-contained for
// readers that don't follow $ref. The same named struct
// referenced from a nested position still uses $ref + an entry
// in $defs.
func ReflectSchema(t reflect.Type) *Schema {
	if t == nil {
		return nil
	}
	b := &schemaBuilder{
		defs: map[string]*Schema{},
		seen: map[string]bool{},
	}
	// Peel pointers at the root the same way build does — *User
	// and User share a schema.
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	var root *Schema
	if t.Kind() == reflect.Struct {
		// Inline the root struct body. Mark the name as seen
		// up front so any self-referential field (Tree.Children
		// → []*Tree) collapses to a $ref into $defs, with the
		// recursive body populated below.
		name := t.Name()
		if name != "" {
			b.seen[name] = true
		}
		root = b.buildStructBody(t)
		if name != "" {
			delete(b.seen, name)
			// If the recursive walk created a self-ref, the
			// referenced body needs to live in $defs.
			if _, used := b.defs[name]; !used && hasRefTo(root, name) {
				b.defs[name] = root
			}
		}
	} else {
		root = b.build(t)
	}
	if len(b.defs) > 0 {
		root.Defs = b.defs
	}
	return root
}

// hasRefTo scans a schema (one level deep — properties + items)
// for a $ref to the given name. Used by ReflectSchema to decide
// whether the root struct's body needs to also live in $defs
// (because a child self-refs back to it).
func hasRefTo(s *Schema, name string) bool {
	if s == nil {
		return false
	}
	if s.Ref == "#/$defs/"+name {
		return true
	}
	if s.Items != nil && hasRefTo(s.Items, name) {
		return true
	}
	for _, p := range s.Properties {
		if hasRefTo(p, name) {
			return true
		}
	}
	return false
}

// build is the recursive workhorse. Walks the type, peeling
// pointers (which mark the schema as nullable but not optional
// — Go's nil-pointer convention is orthogonal to JSON-Schema's
// optional/required), then dispatches on Kind.
func (b *schemaBuilder) build(t reflect.Type) *Schema {
	// Peel pointers. A *T and a T serialize identically as JSON
	// (the *T just decodes to nil on absence); from a schema
	// perspective they describe the same value shape.
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Bool:
		return &Schema{Type: "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return &Schema{Type: "integer"}
	case reflect.Float32, reflect.Float64:
		return &Schema{Type: "number"}
	case reflect.String:
		return &Schema{Type: "string"}
	case reflect.Slice, reflect.Array:
		// []byte is JSON-Schema "string" (base64-encoded by
		// encoding/json). Distinguish that here so a byte
		// slice doesn't get described as a number array.
		if t.Elem().Kind() == reflect.Uint8 {
			return &Schema{Type: "string", Format: "byte"}
		}
		return &Schema{Type: "array", Items: b.build(t.Elem())}
	case reflect.Map:
		// JSON encodes maps as objects; we don't enumerate
		// keys (they're dynamic) so we emit a bare "object"
		// without Properties. This is the standard pattern
		// for free-form maps — a future iteration could add
		// "additionalProperties" with the value schema for
		// stricter matching.
		return &Schema{Type: "object"}
	case reflect.Interface:
		// any / interface{} — wire shape is unknown. Empty
		// schema means "anything goes," which is the right
		// answer (the JSON decoder accepts whatever; we
		// can't structurally compare further).
		return &Schema{}
	case reflect.Struct:
		return b.buildStruct(t)
	default:
		// Channels, funcs, unsafe pointers — these never
		// JSON-encode meaningfully. Treat as opaque rather
		// than crashing the build; the field probably has
		// json:"-" anyway.
		return &Schema{}
	}
}

// buildStruct handles the named-type case. Named structs become
// entries in $defs and are referenced from their use sites via
// $ref — this dedupes shared types (one schema for User even when
// User appears in 5 request shapes) and breaks recursion cycles.
//
// Anonymous structs (no Name) inline their schema directly; they
// can't appear in $defs anyway because there's no name to key on.
func (b *schemaBuilder) buildStruct(t reflect.Type) *Schema {
	name := t.Name()
	if name == "" {
		return b.buildStructBody(t)
	}
	// Cycle break: if we're already building this name, emit a
	// $ref pointing at it. The recursive call up the stack will
	// finish populating defs[name] before we return.
	if b.seen[name] {
		return &Schema{Ref: "#/$defs/" + name}
	}
	if _, done := b.defs[name]; done {
		return &Schema{Ref: "#/$defs/" + name}
	}
	b.seen[name] = true
	body := b.buildStructBody(t)
	delete(b.seen, name)
	b.defs[name] = body
	return &Schema{Ref: "#/$defs/" + name}
}

// buildStructBody fills in Type=object + Properties + Required.
// Walks fields and inspects json + validate tags to decide naming
// and required-ness. Skips unexported fields (Go's encoding/json
// skips them too) and fields tagged json:"-".
func (b *schemaBuilder) buildStructBody(t reflect.Type) *Schema {
	s := &Schema{
		Type:       "object",
		Properties: map[string]*Schema{},
	}
	var required []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		// Promoted (embedded) fields: walk into their type
		// rather than emitting them as a nested object. This
		// matches encoding/json's promotion semantics so the
		// schema mirrors what actually goes on the wire.
		if f.Anonymous {
			sub := b.build(f.Type)
			if sub != nil && sub.Type == "object" {
				for k, v := range sub.Properties {
					s.Properties[k] = v
				}
				required = append(required, sub.Required...)
				continue
			}
		}
		name, omit, skip := jsonFieldName(f)
		if skip {
			continue
		}
		// Required-ness comes from EITHER an explicit
		// validate:"required" tag OR the absence of
		// json:",omitempty" on a non-pointer field. The
		// latter heuristic matches what most Go code means
		// by "this field is mandatory" without needing the
		// validate tag everywhere.
		isRequired := fieldIsRequired(f) || (!omit && f.Type.Kind() != reflect.Ptr)
		s.Properties[name] = b.build(f.Type)
		if isRequired {
			required = append(required, name)
		}
	}
	if len(required) > 0 {
		s.Required = required
	}
	return s
}

// jsonFieldName extracts the JSON key for f honoring the standard
// json tag grammar: `json:"name,omitempty"`, `json:"-"` to skip,
// empty tag falls back to the Go field name. Returns the chosen
// name, whether omitempty was set, and whether the field is
// skipped entirely.
func jsonFieldName(f reflect.StructField) (name string, omitempty, skip bool) {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", false, true
	}
	if tag == "" {
		return f.Name, false, false
	}
	parts := strings.Split(tag, ",")
	name = parts[0]
	if name == "" {
		name = f.Name
	}
	for _, p := range parts[1:] {
		if p == "omitempty" {
			omitempty = true
		}
	}
	return name, omitempty, false
}

// fieldIsRequired checks the validate tag for an explicit "required"
// directive. Matches the convention nexus's own reflective handlers
// already honor (see validate:"required" in graphql_validation.go),
// so a single tag drives both runtime validation and schema
// emission.
func fieldIsRequired(f reflect.StructField) bool {
	tag := f.Tag.Get("validate")
	if tag == "" {
		return false
	}
	for _, p := range strings.Split(tag, ",") {
		// Walk just the directive name (before any "=value").
		name := p
		if eq := strings.IndexByte(p, '='); eq >= 0 {
			name = p[:eq]
		}
		if strings.TrimSpace(name) == "required" {
			return true
		}
	}
	return false
}
