// Package tsgen renders registry schema shapes as TypeScript source.
// It is the ONE implementation behind both TS emitters — the runtime
// client SDK generator (package client) and extension/frontend's
// vendored _client.ts templates. They previously carried byte-identical
// forks of these helpers; type-mapping drift between them would have
// produced clients that disagree about the same schema.
package tsgen

import (
	"fmt"
	"strings"

	"github.com/paulmanoni/nexus/registry"
)

// Type maps a registry TypeRef to a TS type expression:
//
//	string | boolean | integer | number          → string/boolean/number
//	array of T                                   → T[]
//	map with K,V                                 → Record<K, V>
//	ref "Name"                                   → Name
//	object                                       → { fields }
//	any | unknown                                → unknown
//
// Optionality on a TypeRef is rendered as `T | undefined` so the
// type composes inside larger expressions; field-level optionality
// uses `?:` in WriteFields.
func Type(t *registry.TypeRef) string {
	if t == nil {
		return "unknown"
	}
	core := TypeCore(t)
	if t.Optional {
		return core + " | undefined"
	}
	return core
}

func TypeCore(t *registry.TypeRef) string {
	switch t.Kind {
	case "primitive":
		switch t.Primitive {
		case "string":
			return "string"
		case "boolean":
			return "boolean"
		case "integer", "number":
			return "number"
		}
		return "unknown"
	case "array":
		return Type(t.Of) + "[]"
	case "map":
		key := "string"
		if t.KeyOf != nil && t.KeyOf.Kind == "primitive" && t.KeyOf.Primitive == "integer" {
			key = "number"
		}
		return fmt.Sprintf("Record<%s, %s>", key, Type(t.Of))
	case "ref":
		return Ident(t.Ref)
	case "object":
		if t.Object == nil || len(t.Object.Fields) == 0 {
			return "{}"
		}
		var b strings.Builder
		b.WriteString("{ ")
		for i, f := range t.Object.Fields {
			if i > 0 {
				b.WriteString("; ")
			}
			name := f.JSONName
			if name == "" {
				name = f.Name
			}
			opt := ""
			if f.Optional {
				opt = "?"
			}
			fmt.Fprintf(&b, "%s%s: %s", Key(name), opt, Type(&f.Type))
		}
		b.WriteString(" }")
		return b.String()
	case "any", "":
		return "unknown"
	default:
		return "unknown"
	}
}

// Ident sanitizes a Go type name into a TS identifier. Most names
// pass through unchanged; this exists so future additions
// (generics, unicode-named types) have one place to grow.
func Ident(name string) string {
	if name == "" {
		return "unknown"
	}
	return name
}

// Key returns the field-name form for an object literal — quoted
// when the name contains characters that need escaping (dots,
// hyphens, etc., common in JSON tags), bare otherwise.
func Key(name string) string {
	if IsPlainIdent(name) {
		return name
	}
	return Literal(name)
}

func IsPlainIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if i == 0 {
			if !(r == '_' || r == '$' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
				return false
			}
			continue
		}
		if !(r == '_' || r == '$' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

// Literal renders a single-quoted TS string literal, escaping
// backslashes and single quotes.
func Literal(s string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range s {
		if r == '\\' || r == '\'' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('\'')
	return b.String()
}

// EscapeComment kills any embedded "*/" so a block comment doesn't
// accidentally close.
func EscapeComment(s string) string {
	return strings.ReplaceAll(s, "*/", "* /")
}

// WriteFields renders an interface body — one `name?: Type` line per
// field, with descriptions as block comments.
func WriteFields(b *strings.Builder, fields []registry.FieldSchema, indent string) {
	for _, f := range fields {
		name := f.JSONName
		if name == "" {
			name = f.Name
		}
		opt := ""
		if f.Optional {
			opt = "?"
		}
		if f.Description != "" {
			fmt.Fprintf(b, "%s/** %s */\n", indent, EscapeComment(f.Description))
		}
		fmt.Fprintf(b, "%s%s%s: %s\n", indent, Key(name), opt, Type(&f.Type))
	}
}
