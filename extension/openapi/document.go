package openapi

// Document types — a deliberately small subset of OpenAPI 3.1 sufficient
// for the surfaces the nexus registry exposes (REST + optionally
// GraphQL). Importing kin-openapi or libopenapi would balloon the
// dependency tree for marginal benefit — we don't need every spec
// feature, only the ones the framework can populate automatically.
//
// JSON-encodes to a valid OpenAPI 3.1 document. Run the output
// through Spectral if you want to catch any drift from the spec.

// Document is the root OpenAPI 3.1 object. The order of fields
// follows the spec's document outline so the serialized output is
// readable end-to-end.
type Document struct {
	OpenAPI    string                `json:"openapi"`
	Info       Info                  `json:"info"`
	Servers    []Server              `json:"servers,omitempty"`
	Paths      map[string]PathItem   `json:"paths"`
	Components *Components           `json:"components,omitempty"`
	Tags       []Tag                 `json:"tags,omitempty"`
	Security   []SecurityRequirement `json:"security,omitempty"`
}

// Info populates the spec's info block. Required: Title, Version.
type Info struct {
	Title          string   `json:"title"`
	Version        string   `json:"version"`
	Description    string   `json:"description,omitempty"`
	TermsOfService string   `json:"termsOfService,omitempty"`
	Contact        *Contact `json:"contact,omitempty"`
	License        *License `json:"license,omitempty"`
}

// PathItem holds the per-method operations for a single URL pattern.
// OpenAPI calls these "Path Item Objects".
type PathItem struct {
	Summary     string      `json:"summary,omitempty"`
	Description string      `json:"description,omitempty"`
	Get         *Operation  `json:"get,omitempty"`
	Put         *Operation  `json:"put,omitempty"`
	Post        *Operation  `json:"post,omitempty"`
	Delete      *Operation  `json:"delete,omitempty"`
	Patch       *Operation  `json:"patch,omitempty"`
	Head        *Operation  `json:"head,omitempty"`
	Options     *Operation  `json:"options,omitempty"`
	Parameters  []Parameter `json:"parameters,omitempty"`
}

// Operation describes one method on a path.
type Operation struct {
	Tags        []string              `json:"tags,omitempty"`
	Summary     string                `json:"summary,omitempty"`
	Description string                `json:"description,omitempty"`
	OperationID string                `json:"operationId,omitempty"`
	Parameters  []Parameter           `json:"parameters,omitempty"`
	RequestBody *RequestBody          `json:"requestBody,omitempty"`
	Responses   map[string]Response   `json:"responses"`
	Security    []SecurityRequirement `json:"security,omitempty"`
	Deprecated  bool                  `json:"deprecated,omitempty"`
}

// Parameter is a path/query/header/cookie parameter. The framework
// populates these from path placeholders (":id" → in=path) and from
// known query-style Args struct fields.
type Parameter struct {
	Name        string  `json:"name"`
	In          string  `json:"in"` // "path" | "query" | "header" | "cookie"
	Description string  `json:"description,omitempty"`
	Required    bool    `json:"required,omitempty"`
	Schema      *Schema `json:"schema,omitempty"`
	Deprecated  bool    `json:"deprecated,omitempty"`
}

// RequestBody describes a request payload. For nexus the framework
// always uses application/json; we may add multipart later for file
// uploads.
type RequestBody struct {
	Description string               `json:"description,omitempty"`
	Required    bool                 `json:"required,omitempty"`
	Content     map[string]MediaType `json:"content"`
}

// Response is one entry in the responses map of an Operation. The
// framework's typed return type populates Content["application/json"].
type Response struct {
	Description string               `json:"description"`
	Content     map[string]MediaType `json:"content,omitempty"`
}

// MediaType carries the schema for one MIME type within a request
// body or response. The framework writes application/json only;
// other media types stay open for plugins that wrap multipart, NDJSON,
// or CSV transports.
type MediaType struct {
	Schema *Schema `json:"schema,omitempty"`
}

// Schema is the JSON-Schema-derived subset OpenAPI 3.1 uses for
// describing types. The framework converts each registry.TypeRef
// to a Schema; named refs land in Components.Schemas and Schema.Ref
// points at them.
type Schema struct {
	Ref         string             `json:"$ref,omitempty"`
	Type        string             `json:"type,omitempty"`   // "string" | "integer" | "number" | "boolean" | "array" | "object"
	Format      string             `json:"format,omitempty"` // "date-time" | "byte" | ...
	Description string             `json:"description,omitempty"`
	Items       *Schema            `json:"items,omitempty"`         // when Type == "array"
	Properties  map[string]*Schema `json:"properties,omitempty"`    // when Type == "object"
	Required    []string           `json:"required,omitempty"`      // when Type == "object"
	Enum        []any              `json:"enum,omitempty"`
	Nullable    bool               `json:"nullable,omitempty"`      // OpenAPI 3.1 prefers `type: ["string", "null"]` — kept for tooling compat
	AdditionalProperties *Schema   `json:"additionalProperties,omitempty"` // when Type == "object" (map)
	Example     any                `json:"example,omitempty"`
	Deprecated  bool               `json:"deprecated,omitempty"`
}

// Components houses reusable schemas + security definitions. The
// nexus generator only populates Schemas (from registry.Refs) and
// SecuritySchemes (from auth.ExtractorInfo).
type Components struct {
	Schemas         map[string]*Schema         `json:"schemas,omitempty"`
	SecuritySchemes map[string]*SecurityScheme `json:"securitySchemes,omitempty"`
}

// SecurityScheme describes one auth mechanism. nexus generates a
// "bearer" entry when auth.Bearer is in use, "apiKey" for API key
// extractors, "cookie" for cookie-based auth.
type SecurityScheme struct {
	Type         string `json:"type"`                    // "http" | "apiKey" | "oauth2" | "openIdConnect"
	Scheme       string `json:"scheme,omitempty"`        // "bearer" for type=http
	BearerFormat string `json:"bearerFormat,omitempty"`  // "JWT" hint when applicable
	In           string `json:"in,omitempty"`            // "header" | "query" | "cookie" — for type=apiKey
	Name         string `json:"name,omitempty"`          // header/cookie/query param name — for type=apiKey
	Description  string `json:"description,omitempty"`
}

// SecurityRequirement is the per-operation "this op requires X" map.
// Each key is a SecurityScheme name; the slice value is a list of
// scopes (always empty for non-OAuth2 schemes, but the spec demands
// the array shape).
type SecurityRequirement map[string][]string

// Tag groups operations on the Swagger UI sidebar. The framework
// generates one tag per nexus.Module so the docs are organized the
// same way the dashboard's architecture canvas is.
type Tag struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}
