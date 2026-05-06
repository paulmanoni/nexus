package registry

import (
	"reflect"
	"strings"
	"time"
)

// TypeRef is the structural shape of a Go type, walked from
// reflect.Type at endpoint registration. JSON-serializable so the
// client SDK manifest can carry it directly without a separate
// codegen pass — every endpoint's request/response shape becomes
// machine-readable.
//
// Field meaning by Kind:
//
//	"primitive"  Primitive: "string"|"number"|"integer"|"boolean"
//	"array"      Of: element type
//	"map"        KeyOf, Of: key + value types
//	"object"     Object: inline (anonymous) struct shape
//	"ref"        Ref: name of a NamedType in the manifest's Refs pool
//	"any"        no further fields (interface{} fallback)
type TypeRef struct {
	Kind      string     `json:"kind"`
	Primitive string     `json:"primitive,omitempty"`
	Ref       string     `json:"ref,omitempty"`
	Of        *TypeRef   `json:"of,omitempty"`
	KeyOf     *TypeRef   `json:"keyOf,omitempty"`
	Object    *NamedType `json:"object,omitempty"`
	Optional  bool       `json:"optional,omitempty"`
}

// NamedType is the field-list view of a Go struct, used for both
// inline anonymous objects (TypeRef.Object) and shared named types
// keyed into a manifest-level Refs pool.
type NamedType struct {
	Description string        `json:"description,omitempty"`
	Fields      []FieldSchema `json:"fields"`
}

// FieldSchema describes one struct field. JSONName is the wire name
// (from the json tag); Name is the Go field name — kept separate so
// codegen can emit the right identifier on each side.
type FieldSchema struct {
	Name        string  `json:"name"`
	JSONName    string  `json:"jsonName,omitempty"`
	Type        TypeRef `json:"type"`
	Optional    bool    `json:"optional,omitempty"`
	Description string  `json:"description,omitempty"`
}

// WalkType reflects a Go type into a TypeRef. Named struct types are
// recorded into refs (caller-managed map) and referenced by name so
// the manifest carries each named type exactly once even when N
// endpoints share it. Anonymous structs are inlined.
//
// Type-kind mapping:
//
//	string                      → primitive:string
//	int*/uint*                  → primitive:integer
//	float*                      → primitive:number
//	bool                        → primitive:boolean
//	[]byte                      → primitive:string (base64 in JSON)
//	time.Time                   → primitive:string (RFC3339)
//	*T                          → walk(T) with Optional=true
//	[]T / [N]T                  → array of walk(T)
//	map[K]V                     → map of walk(K), walk(V)
//	named struct                → ref:Name, registered into refs
//	anonymous struct            → object inline
//	interface{}                 → any
//	everything else             → any
//
// The walk is finite: cycles in the type graph (a *Node with a
// []*Node Children field, say) terminate at the second visit because
// the named type is already in refs.
func WalkType(t reflect.Type, refs map[string]NamedType) TypeRef {
	if t == nil {
		return TypeRef{Kind: "any"}
	}
	// time.Time is special — render as primitive string. Has to come
	// before the struct branch.
	if t == reflect.TypeOf(time.Time{}) {
		return TypeRef{Kind: "primitive", Primitive: "string"}
	}
	switch t.Kind() {
	case reflect.Ptr:
		ref := WalkType(t.Elem(), refs)
		ref.Optional = true
		return ref
	case reflect.String:
		return TypeRef{Kind: "primitive", Primitive: "string"}
	case reflect.Bool:
		return TypeRef{Kind: "primitive", Primitive: "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return TypeRef{Kind: "primitive", Primitive: "integer"}
	case reflect.Float32, reflect.Float64:
		return TypeRef{Kind: "primitive", Primitive: "number"}
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			// []byte rides as base64-encoded string on the wire.
			return TypeRef{Kind: "primitive", Primitive: "string"}
		}
		of := WalkType(t.Elem(), refs)
		return TypeRef{Kind: "array", Of: &of}
	case reflect.Map:
		k := WalkType(t.Key(), refs)
		v := WalkType(t.Elem(), refs)
		return TypeRef{Kind: "map", KeyOf: &k, Of: &v}
	case reflect.Struct:
		// Named struct → put into refs, return Kind="ref". Anonymous
		// struct → inline. Naming heuristic: t.Name() is non-empty for
		// named types declared at package scope.
		raw := t.Name()
		if raw == "" {
			obj := walkStructFields(t, refs)
			return TypeRef{Kind: "object", Object: &obj}
		}
		// Generic instantiations come back from reflect with Go-syntax
		// names like "Response[*pkg/sub.RunState]" or
		// "Page[[]portal.Pet]" — invalid TS identifiers + invalid map
		// keys for the Refs section. Sanitize once into a stable TS
		// name (ResponseOfRunState, PageOfPetList, …) so the output
		// type-checks and stays human-readable.
		name := sanitizeTypeName(raw)
		if name == "" {
			obj := walkStructFields(t, refs)
			return TypeRef{Kind: "object", Object: &obj}
		}
		// Pre-seed an empty NamedType so a recursive type referring to
		// itself terminates at this entry (without it we'd walk
		// forever into a self-referential field).
		if _, exists := refs[name]; !exists {
			refs[name] = NamedType{}
			obj := walkStructFields(t, refs)
			refs[name] = obj
		}
		return TypeRef{Kind: "ref", Ref: name}
	case reflect.Interface:
		return TypeRef{Kind: "any"}
	default:
		return TypeRef{Kind: "any"}
	}
}

// sanitizeTypeName converts a Go reflect.Type.Name() into a valid
// TypeScript identifier. Handles the shapes Go's generics +
// pointers + slices produce that aren't valid TS / JSON keys:
//
//	Response[*pkg/sub.RunState]                 → ResponseOfRunState
//	Page[[]portal_admin/migrations.Pet]         → PageOfPetList
//	*Pet                                        → Pet
//	pkg/sub.Foo                                 → Foo
//	Map[K, V]                                   → MapOfKAndV
//	Pet                                         → Pet  (unchanged)
//
// Rules in order:
//   1. strip a leading "*" (pointer prefix)
//   2. "[]X" prefix → recurse on X, append "List"
//   3. balanced "[" before "]" → outer + "Of" + recurse(inner)
//      (multi-arg generics: split inner on top-level "," and join with "And")
//   4. strip package path: keep the segment after the last "/" then
//      after the last "."
//   5. drop any remaining non-identifier chars (defensive)
//
// Empty result is impossible for a non-empty named type at runtime;
// returning "" lets WalkType fall back to inline-object rendering
// in the unlikely case sanitization eats everything.
func sanitizeTypeName(name string) string {
	s := strings.TrimPrefix(name, "*")
	if s == "" {
		return ""
	}

	// "[]X" — slice prefix. Recurse + append "List".
	if strings.HasPrefix(s, "[]") {
		inner := sanitizeTypeName(s[2:])
		if inner == "" {
			return ""
		}
		return inner + "List"
	}

	// "Foo[X]" — generic instantiation. Find matching "]" by depth so
	// nested generics ("Page[Response[Pet]]") parse correctly.
	if i := strings.IndexByte(s, '['); i > 0 {
		depth := 1
		end := -1
		for j := i + 1; j < len(s); j++ {
			switch s[j] {
			case '[':
				depth++
			case ']':
				depth--
				if depth == 0 {
					end = j
				}
			}
			if end >= 0 {
				break
			}
		}
		if end > i {
			outer := sanitizeTypeName(s[:i])
			tail := sanitizeTypeName(s[end+1:])
			// Split top-level commas so multi-arg generics produce
			// "MapOfKAndV" rather than "MapOfKVMashed".
			args := splitTopLevelCommas(s[i+1 : end])
			parts := make([]string, 0, len(args))
			for _, a := range args {
				if p := sanitizeTypeName(strings.TrimSpace(a)); p != "" {
					parts = append(parts, p)
				}
			}
			return outer + "Of" + strings.Join(parts, "And") + tail
		}
	}

	// Strip package path: "pkg/sub.Name" → "Name". Slash first, then
	// dot, so a name with no slash but a dot ("time.Time") still
	// resolves to "Time".
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.LastIndexByte(s, '.'); i >= 0 {
		s = s[i+1:]
	}

	// Defensive: drop any remaining non-identifier chars. Real names
	// pass through; only weird reflect outputs (uncommon) get here.
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// splitTopLevelCommas walks s and splits on commas that aren't
// nested inside brackets. Lets sanitizeTypeName handle multi-arg
// generics where each arg may itself be a generic
// ("Map[K, Response[V]]" → ["K", " Response[V]"]).
func splitTopLevelCommas(s string) []string {
	var out []string
	depth := 0
	last := 0
	for i, r := range s {
		switch r {
		case '[':
			depth++
		case ']':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[last:i])
				last = i + 1
			}
		}
	}
	out = append(out, s[last:])
	return out
}

// walkStructFields builds the field list for a struct type. Skips
// unexported fields, honors `json:"-"` (omits the field), reads
// `json:"name,omitempty"` to derive JSONName + Optional, and uses the
// field's `desc:"..."` tag for Description (already a framework
// convention seen elsewhere).
func walkStructFields(t reflect.Type, refs map[string]NamedType) NamedType {
	out := NamedType{Fields: make([]FieldSchema, 0, t.NumField())}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		jsonTag := f.Tag.Get("json")
		if jsonTag == "-" {
			continue
		}
		jsonName := f.Name
		omitempty := false
		if jsonTag != "" {
			parts := strings.Split(jsonTag, ",")
			if parts[0] != "" {
				jsonName = parts[0]
			}
			for _, p := range parts[1:] {
				if p == "omitempty" {
					omitempty = true
				}
			}
		}
		// Anonymous embedded fields with no explicit json tag flatten
		// into the parent struct on the wire. Walk the embedded type
		// and splice its fields in to mirror that.
		if f.Anonymous && jsonTag == "" {
			embedded := WalkType(f.Type, refs)
			if embedded.Kind == "ref" {
				if nt, ok := refs[embedded.Ref]; ok {
					out.Fields = append(out.Fields, nt.Fields...)
					continue
				}
			}
			if embedded.Kind == "object" && embedded.Object != nil {
				out.Fields = append(out.Fields, embedded.Object.Fields...)
				continue
			}
		}
		ft := WalkType(f.Type, refs)
		opt := omitempty || ft.Optional
		ft.Optional = false // optionality lives on FieldSchema, not TypeRef, when on a field
		fs := FieldSchema{
			Name:        f.Name,
			JSONName:    jsonName,
			Type:        ft,
			Optional:    opt,
			Description: f.Tag.Get("desc"),
		}
		if jsonName == f.Name {
			fs.JSONName = ""
		}
		out.Fields = append(out.Fields, fs)
	}
	return out
}