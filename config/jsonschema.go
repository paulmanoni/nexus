package config

import (
	"encoding/json"
	"reflect"
	"strings"
)

// SchemaURL is where the nexus docs site publishes the JSON schema of
// nexus.toml. Put `#:schema <SchemaURL>` on the file's first line and
// TOML-aware editors (Taplo / Even Better TOML) complete and check its keys.
const SchemaURL = "https://paulmanoni.github.io/nexus/nexus.toml.schema.json"

// JSONSchema returns a JSON schema (draft 2020-12) for nexus.toml, generated
// from the tables declared in this binary: [runtime] and the framework's
// own tables, the extension blocks with a registered decoder, and every
// config.Section. Like the loader, it rejects keys nothing declares
// (additionalProperties: false).
func JSONSchema() ([]byte, error) {
	typ, _ := documentType()
	root := typeSchema(typ)
	root["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	root["$id"] = SchemaURL
	root["title"] = "nexus.toml"
	root["description"] = "nexus runtime configuration. Every table is declared by nexus, an extension or the app (config.Section); unknown keys fail boot."
	return json.MarshalIndent(root, "", "  ")
}

func typeSchema(t reflect.Type) map[string]any {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Implements(textUnmarshalType) || reflect.PointerTo(t).Implements(textUnmarshalType) {
		return map[string]any{"type": "string"}
	}
	switch t.Kind() {
	case reflect.Struct:
		props := map[string]any{}
		addStructProps(t, props)
		return map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": typeSchema(t.Elem())}
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return map[string]any{"type": "string"}
		}
		return map[string]any{"type": "array", "items": typeSchema(t.Elem())}
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	}
	return map[string]any{}
}

func addStructProps(t reflect.Type, props map[string]any) {
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		// An untagged embedded struct's keys sit in the parent table.
		if f.Anonymous && f.Tag.Get("toml") == "" && ft.Kind() == reflect.Struct {
			addStructProps(ft, props)
			continue
		}
		name := tomlFieldName(f)
		if name == "" {
			continue
		}
		s := typeSchema(f.Type)
		if kinds := f.Tag.Get("schema"); kinds != "" {
			switch kinds {
			case "duration":
				s = map[string]any{"type": "string", "description": `Go duration, e.g. "30s" or "1h30m"`}
			default:
				types := strings.Split(kinds, ",")
				if len(types) == 1 {
					s = map[string]any{"type": types[0]}
				} else {
					s = map[string]any{"type": types}
				}
			}
		}
		if doc := f.Tag.Get("doc"); doc != "" {
			s["description"] = doc
		}
		props[name] = s
	}
}
