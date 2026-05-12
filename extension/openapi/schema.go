package openapi

import (
	"github.com/paulmanoni/nexus/client"
	"github.com/paulmanoni/nexus/registry"
)

// schemaFromTypeRef converts the framework's TypeRef into an OpenAPI
// Schema. Lossy where OpenAPI's vocabulary doesn't cover a TypeRef
// shape (e.g. "any" maps to an unrestricted Schema), but never
// produces invalid output — every branch returns a well-formed Schema.
//
// Named refs become {"$ref": "#/components/schemas/<name>"} pointers
// — caller is expected to have populated Components.Schemas with the
// corresponding NamedType.
func schemaFromTypeRef(t *registry.TypeRef) *Schema {
	if t == nil {
		return &Schema{}
	}
	switch t.Kind {
	case "primitive":
		return primitiveSchema(t.Primitive)
	case "array":
		return &Schema{Type: "array", Items: schemaFromTypeRef(t.Of)}
	case "map":
		// OpenAPI's "object with arbitrary keys" — additionalProperties
		// carries the value schema. Key type information is dropped:
		// OpenAPI assumes string keys for JSON objects (which is
		// JSON's only legal key type).
		return &Schema{
			Type:                 "object",
			AdditionalProperties: schemaFromTypeRef(t.Of),
		}
	case "object":
		if t.Object == nil {
			return &Schema{Type: "object"}
		}
		return schemaFromNamedType(*t.Object)
	case "ref":
		return &Schema{Ref: "#/components/schemas/" + t.Ref}
	case "any":
		// Empty schema is OpenAPI's "any value allowed". Distinct
		// from {type: "object"} which means "must be an object".
		return &Schema{}
	}
	return &Schema{}
}

// schemaFromNamedType walks a NamedType's fields into an OpenAPI
// object schema. Optional fields are omitted from required[]; the
// wire field name (JSONName, then GraphQLName, then Go name) is what
// lands as the property key.
func schemaFromNamedType(nt registry.NamedType) *Schema {
	props := make(map[string]*Schema, len(nt.Fields))
	var required []string
	for _, f := range nt.Fields {
		name := wireName(f)
		ps := schemaFromTypeRef(&f.Type)
		if f.Description != "" {
			ps.Description = f.Description
		}
		props[name] = ps
		if !f.Optional {
			required = append(required, name)
		}
	}
	out := &Schema{
		Type:        "object",
		Description: nt.Description,
		Properties:  props,
	}
	if len(required) > 0 {
		out.Required = required
	}
	return out
}

// primitiveSchema maps registry's primitive set to OpenAPI's set.
// The registry uses TS-shaped names ("number" for both int and
// float); OpenAPI distinguishes them via type + format. Our
// registry-level "integer" already separates them, so the mapping
// is a direct table lookup.
func primitiveSchema(p string) *Schema {
	switch p {
	case "string":
		return &Schema{Type: "string"}
	case "integer":
		return &Schema{Type: "integer", Format: "int64"}
	case "number":
		return &Schema{Type: "number"}
	case "boolean":
		return &Schema{Type: "boolean"}
	}
	// Unknown primitive — emit a permissive schema so SDK generators
	// don't choke. The registry shouldn't produce these in practice.
	return &Schema{}
}

// securitySchemesFromAuth derives OpenAPI components.securitySchemes
// from the running auth extractor. Mirrors the strategy taxonomy in
// client.ExtractorInfo:
//
//	"bearer" → http + bearer
//	"cookie" → apiKey + in:cookie
//	"apikey" → apiKey + in:header
//	"chain"  → walk inner strategies, emit each
//	"custom" → fallback to bearer with a note
func securitySchemesFromAuth(info *client.AuthInfo) map[string]*SecurityScheme {
	out := map[string]*SecurityScheme{}
	collect(&info.ExtractorInfo, out)
	return out
}

func collect(e *client.ExtractorInfo, into map[string]*SecurityScheme) {
	if e == nil {
		return
	}
	switch e.Strategy {
	case "bearer":
		into["bearerAuth"] = &SecurityScheme{
			Type:         "http",
			Scheme:       "bearer",
			BearerFormat: "token",
			Description:  "Bearer token in the Authorization header.",
		}
	case "cookie":
		into["cookieAuth"] = &SecurityScheme{
			Type:        "apiKey",
			In:          "cookie",
			Name:        nonEmpty(e.CookieName, "session"),
			Description: "Cookie-based session.",
		}
	case "apikey":
		into["apiKeyAuth"] = &SecurityScheme{
			Type:        "apiKey",
			In:          "header",
			Name:        nonEmpty(e.HeaderName, "X-API-Key"),
			Description: "API key header.",
		}
	case "chain":
		for i := range e.Chain {
			collect(&e.Chain[i], into)
		}
	case "custom", "":
		into["bearerAuth"] = &SecurityScheme{
			Type:        "http",
			Scheme:      "bearer",
			Description: "Custom extractor; bearer is the most likely client shape.",
		}
	}
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
