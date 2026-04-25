package nexus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// ClientCallable is the seam every generated cross-module client
// dispatches through. Both LocalInvoker (in-process via httptest)
// and RemoteCaller (HTTP) satisfy it, so the helpers below can build
// a single transport-agnostic call path. The codegen picks the
// concrete impl per-deployment in the constructor.
type ClientCallable interface {
	Invoke(ctx context.Context, method, path string, body, out any) error
}

// GqlOptions tunes a GraphQL invocation. Currently only the mount
// path; reserved space for future knobs (per-call headers, custom
// operationName, persisted-query IDs).
type GqlOptions struct {
	Path string // defaults to "/graphql"
}

// GqlCall executes a GraphQL operation through callable. The query
// string is constructed from opName + args (inlined as JSON
// literals) + a selection set walked from T's exported struct
// fields. Returns the decoded data slot (or a typed error if the
// peer reported one).
//
// Limitations of the v1 helper:
//   - Args are inlined into the query body; no $variables pass.
//     Works fine with graphql-go's permissive scalar parsing for
//     strings/ints/bools/maps; complex inputs may need manual
//     escaping. This keeps the helper schema-free at the cost of
//     parameter type-safety from the server's side.
//   - The selection set descends one struct level. Slices/pointers
//     unwrap to their element. Scalars get no sub-selection.
//   - Output type T must be a struct or pointer-to-struct (or a
//     slice thereof) — scalars at the top level don't carry a
//     selection set in GraphQL.
//
// For richer cases (variables, fragments, custom scalars) call into
// the peer's /graphql endpoint directly with your own request body.
func GqlCall[T any](ctx context.Context, callable ClientCallable, opType, opName string, args any, opts GqlOptions) (T, error) {
	var zero T
	rt := reflect.TypeOf(zero)
	selection := buildGqlSelection(rt)
	query := composeGqlQuery(opType, opName, args, selection)

	body := map[string]string{"query": query}

	var raw struct {
		Data   map[string]json.RawMessage `json:"data"`
		Errors []gqlError                 `json:"errors"`
	}
	path := opts.Path
	if path == "" {
		path = "/graphql"
	}
	if err := callable.Invoke(ctx, "POST", path, body, &raw); err != nil {
		return zero, err
	}
	if len(raw.Errors) > 0 {
		return zero, raw.Errors[0]
	}
	dataField, ok := raw.Data[opName]
	if !ok {
		return zero, fmt.Errorf("nexus: GraphQL response missing data.%s", opName)
	}
	if err := json.Unmarshal(dataField, &zero); err != nil {
		return zero, fmt.Errorf("nexus: decode GraphQL data.%s: %w", opName, err)
	}
	return zero, nil
}

// gqlError is the canonical GraphQL error envelope. We only surface
// the first error's message — handlers that need the full list can
// hit the endpoint directly.
type gqlError struct {
	Message string         `json:"message"`
	Path    []any          `json:"path,omitempty"`
	Ext     map[string]any `json:"extensions,omitempty"`
}

func (e gqlError) Error() string {
	if len(e.Path) > 0 {
		return fmt.Sprintf("nexus gql: %s (path: %v)", e.Message, e.Path)
	}
	return "nexus gql: " + e.Message
}

// composeGqlQuery builds a single-line GraphQL document. The opType
// is "query" or "mutation"; arg values are inlined as GraphQL
// literals.
//
//	query { getAllAdverts { id title } }
//	mutation { createAdvert(title: "x", employerName: "y") { id } }
func composeGqlQuery(opType, opName string, args any, selection string) string {
	argsLit := gqlArgsLiteral(args)
	if argsLit != "" {
		argsLit = "(" + argsLit + ")"
	}
	if selection == "" {
		return opType + " { " + opName + argsLit + " }"
	}
	return opType + " { " + opName + argsLit + " " + selection + " }"
}

// gqlArgsLiteral walks args's struct fields and emits a comma-
// separated `name: value` list. The arg name is the field's `graphql`
// or `json` tag (whichever lands first), or the lowercased Go name
// as a fallback. Zero values are skipped — the server's defaults
// take over.
func gqlArgsLiteral(args any) string {
	if args == nil {
		return ""
	}
	v := reflect.ValueOf(args)
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return ""
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return ""
	}
	var parts []string
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		fv := v.Field(i)
		if fv.IsZero() {
			continue
		}
		name := gqlArgName(f)
		if name == "" || name == "-" {
			continue
		}
		parts = append(parts, name+": "+gqlValueLiteral(fv))
	}
	return strings.Join(parts, ", ")
}

// gqlValueLiteral renders a Go value as a GraphQL-syntax literal.
// Scalars use their JSON form; strings get quoted with embedded
// quotes escaped. Composite types fall back to JSON which graphql-go
// generally accepts for input objects.
func gqlValueLiteral(v reflect.Value) string {
	switch v.Kind() {
	case reflect.String:
		b, _ := json.Marshal(v.Interface())
		return string(b)
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		b, _ := json.Marshal(v.Interface())
		return string(b)
	default:
		// Composite: JSON-encode and hope graphql-go's input parser
		// accepts the shape. For anything richer (enums, custom
		// scalars) callers should drop down to a hand-written body.
		b, _ := json.Marshal(v.Interface())
		return string(b)
	}
}

// gqlArgName picks the graphql > json > lowercased-Go-name in that
// order. Mirrors how the framework's resolver builder reads the
// same args struct, so client and server agree on the wire name.
func gqlArgName(f reflect.StructField) string {
	if tag := f.Tag.Get("graphql"); tag != "" {
		name, _, _ := strings.Cut(tag, ",")
		return name
	}
	if tag := f.Tag.Get("json"); tag != "" {
		name, _, _ := strings.Cut(tag, ",")
		return name
	}
	if f.Name == "" {
		return ""
	}
	r := []rune(f.Name)
	r[0] = lowerRune(r[0])
	return string(r)
}

func lowerRune(r rune) rune {
	if r >= 'A' && r <= 'Z' {
		return r + ('a' - 'A')
	}
	return r
}

// buildGqlSelection walks t (skipping pointer/slice indirection) and
// returns "{ field1 field2 nested { sub } }" suitable for splicing
// into a GraphQL document. Returns "" for non-struct types — the
// caller should still emit a valid query.
func buildGqlSelection(t reflect.Type) string {
	if t == nil {
		return ""
	}
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return ""
	}
	var parts []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name := gqlFieldName(f)
		if name == "" || name == "-" {
			continue
		}
		ft := f.Type
		for ft.Kind() == reflect.Pointer || ft.Kind() == reflect.Slice || ft.Kind() == reflect.Array {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct {
			sub := buildGqlSelection(ft)
			if sub == "" {
				continue
			}
			parts = append(parts, name+" "+sub)
		} else {
			parts = append(parts, name)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "{ " + strings.Join(parts, " ") + " }"
}

// gqlFieldName mirrors gqlArgName for the response side. graphql tag
// wins over json (some types use both); falls back to the
// lowercased-first-letter Go name.
func gqlFieldName(f reflect.StructField) string {
	if tag := f.Tag.Get("graphql"); tag != "" {
		name, _, _ := strings.Cut(tag, ",")
		return name
	}
	if tag := f.Tag.Get("json"); tag != "" {
		name, _, _ := strings.Cut(tag, ",")
		return name
	}
	if f.Name == "" {
		return ""
	}
	r := []rune(f.Name)
	r[0] = lowerRune(r[0])
	return string(r)
}

// silenceErrorsImport keeps the errors package referenced for future
// expansion (multi-error wrapping when gqlError stops being a single
// surfaced message).
var _ = errors.New