package registry

import (
	"reflect"
	"sort"
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

// FieldSchema describes one struct field.
//
// Name        — Go field name; used as a last-resort wire identifier.
// JSONName    — wire name on the JSON / REST side; from the `json:` tag.
// GraphQLName — wire name on the GraphQL side; from the `graphql:` tag's
//
//	first comma-separated token. The schema walker also uses
//	this as a fallback for JSONName when the `json:` tag is
//	absent — that's what makes graphql-only-tagged args
//	structs (e.g. `Username string \`graphql:"username"\``)
//	surface as `username` in TS + on the wire instead of
//	leaking the Go field name.
//
// Codegen and manifest projection pick whichever side fits the
// transport.
type FieldSchema struct {
	Name        string  `json:"name"`
	JSONName    string  `json:"jsonName,omitempty"`
	GraphQLName string  `json:"graphqlName,omitempty"`
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
//  1. strip a leading "*" (pointer prefix)
//  2. "[]X" prefix → recurse on X, append "List"
//  3. balanced "[" before "]" → outer + "Of" + recurse(inner)
//     (multi-arg generics: split inner on top-level "," and join with "And")
//  4. strip package path: keep the segment after the last "/" then
//     after the last "."
//  5. drop any remaining non-identifier chars (defensive)
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

// collectedField is a FieldSchema plus the metadata encoding/json uses
// to resolve same-name collisions across embedding: embedding depth
// (shallower dominates) and whether the wire name came from an explicit
// tag (a tagged field dominates an untagged one at equal depth). order
// records source-traversal position so survivors emit in declaration
// order regardless of how grouping shuffles them.
type collectedField struct {
	fs     FieldSchema
	wire   string // effective wire name the collision is keyed on
	depth  int
	tagged bool
	order  int
}

// walkStructFields builds the field list for a struct type. Skips
// unexported fields, honors `json:"-"` (omits the field), reads
// `json:"name,omitempty"` to derive JSONName + Optional, and uses the
// field's `desc:"..."` tag for Description (already a framework
// convention seen elsewhere). Anonymous embedded structs are flattened
// with their fields promoted, and same-wire-name collisions across
// embedding levels are resolved exactly as encoding/json would.
func walkStructFields(t reflect.Type, refs map[string]NamedType) NamedType {
	order := 0
	collected := collectStructFields(t, refs, 0, &order, nil)
	return NamedType{Fields: resolveFieldCollisions(collected)}
}

// collectStructFields walks t recursively, flattening anonymous embedded
// structs and tagging every field with its embedding depth so the caller
// can resolve collisions. ancestors guards against cyclic embedding
// (e.g. `type A struct{ *A }`) so the recursion terminates.
func collectStructFields(t reflect.Type, refs map[string]NamedType, depth int, order *int, ancestors map[reflect.Type]bool) []collectedField {
	out := make([]collectedField, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		// Mirror encoding/json's promotion rules. An anonymous embedded
		// struct is walked for its exported fields even when the embedded
		// type itself is unexported (`type User struct{ base }`) — Go
		// promotes those fields onto the wire. Only drop non-embedded
		// unexported fields and embedded unexported NON-struct fields.
		if f.Anonymous {
			et := f.Type
			if et.Kind() == reflect.Ptr {
				et = et.Elem()
			}
			if !f.IsExported() && et.Kind() != reflect.Struct {
				continue
			}
		} else if !f.IsExported() {
			continue
		}
		jsonTag := f.Tag.Get("json")
		if jsonTag == "-" {
			continue
		}
		gqlTag := f.Tag.Get("graphql")
		if gqlTag == "-" {
			continue
		}
		gqlName := ""
		if gqlTag != "" {
			if name := strings.SplitN(gqlTag, ",", 2)[0]; name != "" {
				gqlName = name
			}
		}
		jsonName := f.Name
		jsonExplicit := false
		omitempty := false
		if jsonTag != "" {
			parts := strings.Split(jsonTag, ",")
			if parts[0] != "" {
				jsonName = parts[0]
				jsonExplicit = true
			}
			for _, p := range parts[1:] {
				if p == "omitempty" {
					omitempty = true
				}
			}
		}
		// graphql-only-tagged fields (no json tag) borrow the graphql
		// name as the wire name so TS codegen + the GraphQL document
		// builder agree with the registered schema arg names. Without
		// this, `Username string \`graphql:"username"\`` would surface
		// as `Username` on the wire and the document would hit the
		// server with `Username:` (which the schema doesn't know).
		if !jsonExplicit && gqlName != "" {
			jsonName = gqlName
		}
		// Anonymous embedded fields with no explicit json tag flatten
		// into the parent struct on the wire: recurse one level deeper so
		// the promoted fields carry the right depth for collision
		// resolution, while the embedded type is still registered as its
		// own SDK ref (exported case) so its standalone interface emits.
		if f.Anonymous && jsonTag == "" {
			et := f.Type
			if et.Kind() == reflect.Ptr {
				et = et.Elem()
			}
			if et.Kind() == reflect.Struct {
				// An exported embed remains a named type in the SDK
				// surface; an unexported one only contributes fields, so
				// its internal name never leaks into refs.
				if f.IsExported() {
					WalkType(f.Type, refs)
				}
				if ancestors[et] {
					continue // cyclic embed — stop before recursing forever
				}
				next := map[reflect.Type]bool{et: true}
				for k := range ancestors {
					next[k] = true
				}
				out = append(out, collectStructFields(et, refs, depth+1, order, next)...)
				continue
			}
		}
		ft := WalkType(f.Type, refs)
		opt := omitempty || ft.Optional
		ft.Optional = false // optionality lives on FieldSchema, not TypeRef, when on a field
		fs := FieldSchema{
			Name:        f.Name,
			JSONName:    jsonName,
			GraphQLName: gqlName,
			Type:        ft,
			Optional:    opt,
			Description: f.Tag.Get("desc"),
		}
		if jsonName == f.Name {
			fs.JSONName = ""
		}
		out = append(out, collectedField{
			fs:     fs,
			wire:   jsonName,
			depth:  depth,
			tagged: jsonExplicit || gqlName != "",
			order:  *order,
		})
		*order++
	}
	return out
}

// resolveFieldCollisions reduces fields sharing a wire name to the single
// dominant one using Go's embedding rules: the shallowest field wins; at
// equal depth a tagged field beats an untagged one; and a genuine tie
// (equal depth AND equal tag-presence) drops them all — exactly what
// encoding/json does with an ambiguous selector. Survivors are returned
// in source-declaration order.
func resolveFieldCollisions(fields []collectedField) []FieldSchema {
	groups := make(map[string][]collectedField)
	seen := make([]string, 0, len(fields))
	for _, cf := range fields {
		if _, ok := groups[cf.wire]; !ok {
			seen = append(seen, cf.wire)
		}
		groups[cf.wire] = append(groups[cf.wire], cf)
	}
	winners := make([]collectedField, 0, len(seen))
	for _, name := range seen {
		if w, ok := dominantField(groups[name]); ok {
			winners = append(winners, w)
		}
	}
	sort.Slice(winners, func(i, j int) bool { return winners[i].order < winners[j].order })
	out := make([]FieldSchema, len(winners))
	for i, w := range winners {
		out[i] = w.fs
	}
	return out
}

// dominantField picks the winner among same-wire-name fields, or reports
// false when the collision is ambiguous (and the field is dropped). Sort
// by depth ascending then tagged-first; the head wins unless the top two
// share both depth and tag-presence, which is a true tie.
func dominantField(g []collectedField) (collectedField, bool) {
	if len(g) == 1 {
		return g[0], true
	}
	sort.Slice(g, func(i, j int) bool {
		if g[i].depth != g[j].depth {
			return g[i].depth < g[j].depth
		}
		if g[i].tagged != g[j].tagged {
			return g[i].tagged
		}
		return g[i].order < g[j].order
	})
	if g[0].depth == g[1].depth && g[0].tagged == g[1].tagged {
		return collectedField{}, false
	}
	return g[0], true
}
