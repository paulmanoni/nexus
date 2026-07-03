package openapi

import (
	"regexp"
	"sort"
	"strings"

	"github.com/paulmanoni/nexus/client"
	"github.com/paulmanoni/nexus/registry"
)

// buildDocument is the main translation function: client.Manifest →
// OpenAPI Document. Pure; deterministic; no I/O. Reads only from the
// passed-in manifest so tests can drive it with hand-built fixtures.
//
// Output is stable across runs (sorted paths, sorted tags) so
// schema diffs in CI are signal, not noise.
func (s *pluginState) buildDocument(m client.Manifest) Document {
	doc := Document{
		OpenAPI: "3.1.0",
		Info: Info{
			Title:       s.cfg.Title,
			Version:     s.cfg.Version,
			Description: s.cfg.Description,
			Contact:     s.cfg.Contact,
			License:     s.cfg.License,
		},
		Servers: s.cfg.Servers,
		Paths:   map[string]PathItem{},
	}

	tagsByName := map[string]struct{}{}
	pathOps := map[string]*PathItem{} // accumulator before assigning into doc.Paths

	for _, ep := range m.Endpoints {
		// Filter by transport + admin policy.
		if !s.shouldInclude(ep) {
			continue
		}

		// Convert the framework's path syntax (":id") to OpenAPI's
		// ("{id}"). The framework's WS endpoints don't use this
		// idiom anyway; only REST routes have parameters.
		path, params := normalizePath(ep.Path)

		// Tag: module if present, else service. Same grouping the
		// dashboard's architecture canvas uses, so docs feel
		// continuous with the live introspection surface.
		tag := ep.Module
		if tag == "" {
			tag = ep.Service
		}
		if tag != "" {
			tagsByName[tag] = struct{}{}
		}

		op := s.buildOperation(ep, tag, params)

		// Locate / create the PathItem and bind the operation by
		// HTTP method. GraphQL gets POST /graphql; WebSocket isn't
		// in OpenAPI's vocabulary so we attach it as a POST with a
		// note in the description.
		method := pickMethod(ep)
		pi, ok := pathOps[path]
		if !ok {
			pi = &PathItem{}
			pathOps[path] = pi
		}
		assignOperation(pi, method, op)
	}

	for p, pi := range pathOps {
		doc.Paths[p] = *pi
	}

	// Components.Schemas — one entry per NamedType in Refs. Sorted
	// for stable output.
	if len(m.Refs) > 0 {
		schemas := make(map[string]*Schema, len(m.Refs))
		for name, nt := range m.Refs {
			schemas[name] = schemaFromNamedType(nt)
		}
		if doc.Components == nil {
			doc.Components = &Components{}
		}
		doc.Components.Schemas = schemas
	}

	// SecuritySchemes — derive from the manifest's Auth block.
	if m.Auth != nil {
		if doc.Components == nil {
			doc.Components = &Components{}
		}
		doc.Components.SecuritySchemes = securitySchemesFromAuth(m.Auth)
	}

	// Tags — sorted alphabetically.
	if len(tagsByName) > 0 {
		tags := make([]Tag, 0, len(tagsByName))
		for name := range tagsByName {
			tags = append(tags, Tag{Name: name})
		}
		sort.Slice(tags, func(i, j int) bool { return tags[i].Name < tags[j].Name })
		doc.Tags = tags
	}

	return doc
}

// shouldInclude applies the plugin's policy on which endpoints make
// it into the spec. Admin routes (/__nexus/*) are excluded by
// default — the SDK consumer is not the same audience as the dashboard
// operator. GraphQL ops require an explicit opt-in.
func (s *pluginState) shouldInclude(ep client.EndpointInfo) bool {
	if s.excludeAdmin() && strings.HasPrefix(ep.Path, "/__nexus/") {
		return false
	}
	switch ep.Transport {
	case "rest":
		return true
	case "graphql":
		return s.includeGraphQL()
	case "websocket":
		// WebSocket doesn't fit OpenAPI's REST-shaped operation
		// model cleanly. Skip until we add AsyncAPI emission as a
		// sibling plugin.
		return false
	}
	return false
}

// buildOperation translates one client.EndpointInfo into an
// OpenAPI Operation, including the request body and the typed
// response shape. Auth state goes into the Security array using the
// "bearerAuth" / "cookieAuth" scheme name (matches what
// securitySchemesFromAuth emits).
func (s *pluginState) buildOperation(ep client.EndpointInfo, tag string, pathParams []Parameter) *Operation {
	op := &Operation{
		Summary:     ep.Description,
		OperationID: operationID(ep),
		Responses:   map[string]Response{},
		Deprecated:  ep.Deprecated,
	}
	if tag != "" {
		op.Tags = []string{tag}
	}
	if len(pathParams) > 0 {
		op.Parameters = append(op.Parameters, pathParams...)
	}

	// REST GET / DELETE: typed Args become query parameters (every
	// scalar field) + path params (already handled).
	// REST POST / PUT / PATCH: Args go in the request body.
	// GraphQL (if enabled): always body (variables wrapper).
	method := pickMethod(ep)
	bodyMethod := method == "POST" || method == "PUT" || method == "PATCH"

	if ep.Args != nil {
		if bodyMethod {
			op.RequestBody = &RequestBody{
				Required: true,
				Content: map[string]MediaType{
					"application/json": {Schema: schemaFromTypeRef(ep.Args)},
				},
			}
		} else if ep.Args.Kind == "ref" || ep.Args.Kind == "object" {
			// Promote each top-level Args field to a query
			// parameter. Path-bound fields (already in
			// pathParams) are dropped to avoid duplicates.
			seen := paramNameSet(op.Parameters)
			for _, p := range queryParamsFromArgs(ep.Args) {
				if _, dup := seen[p.Name]; dup {
					continue
				}
				op.Parameters = append(op.Parameters, p)
			}
		}
	}

	// Response — typed return type at 200.
	successCode := "200"
	if method == "POST" {
		successCode = "201"
	}
	if ep.Return != nil {
		op.Responses[successCode] = Response{
			Description: "OK",
			Content: map[string]MediaType{
				"application/json": {Schema: schemaFromTypeRef(ep.Return)},
			},
		}
	} else {
		op.Responses[successCode] = Response{Description: "OK"}
	}
	// Generic error response — covers 4xx + 5xx so SDK generators
	// at least produce an "error" branch instead of silently
	// dropping non-2xx into the success path. Could be more
	// precise per-op once we surface error types from the registry.
	op.Responses["default"] = Response{
		Description: "Error response",
		Content: map[string]MediaType{
			"application/json": {
				Schema: &Schema{
					Type: "object",
					Properties: map[string]*Schema{
						"error":   {Type: "string"},
						"message": {Type: "string"},
					},
					Required: []string{"error"},
				},
			},
		},
	}

	// Security — bearer/cookie/apikey schemes are declared globally
	// in components.securitySchemes; per-op we just name them.
	if ep.AuthRequired {
		op.Security = []SecurityRequirement{
			{"bearerAuth": []string{}},
		}
		// Permission requirements go into the description (OpenAPI
		// can't express RBAC natively without an extension; this
		// at least makes them visible in Swagger UI).
		if len(ep.RequiresPerm) > 0 {
			perms := "**Requires permissions:** " + strings.Join(ep.RequiresPerm, ", ")
			if op.Description != "" {
				op.Description += "\n\n" + perms
			} else {
				op.Description = perms
			}
		}
	}

	return op
}

// pickMethod maps registry transport+method to an OpenAPI HTTP verb.
func pickMethod(ep client.EndpointInfo) string {
	if ep.Transport == "graphql" {
		return "POST"
	}
	m := strings.ToUpper(ep.Method)
	if m == "" {
		return "GET"
	}
	return m
}

// assignOperation puts the operation into the right field of the
// PathItem. Defensive against unknown methods (returns silently —
// the framework only emits the standard set).
func assignOperation(pi *PathItem, method string, op *Operation) {
	switch method {
	case "GET":
		pi.Get = op
	case "PUT":
		pi.Put = op
	case "POST":
		pi.Post = op
	case "DELETE":
		pi.Delete = op
	case "PATCH":
		pi.Patch = op
	case "HEAD":
		pi.Head = op
	case "OPTIONS":
		pi.Options = op
	}
}

// pathParamPattern matches gin's ":name" style path params. Captures
// the param name so we can rewrite to OpenAPI's "{name}" and emit a
// Parameter object per match.
var pathParamPattern = regexp.MustCompile(`:([A-Za-z_][A-Za-z0-9_]*)`)

// normalizePath rewrites a registered path from gin syntax to
// OpenAPI syntax and returns the path-parameter list it derived.
// Examples:
//
//	"/users/:id"            → "/users/{id}",      [Parameter{id, in:path}]
//	"/orgs/:org/users/:id"  → "/orgs/{org}/users/{id}", [..., ...]
//	"/static"               → "/static",          []
func normalizePath(p string) (string, []Parameter) {
	matches := pathParamPattern.FindAllStringSubmatch(p, -1)
	if len(matches) == 0 {
		return p, nil
	}
	params := make([]Parameter, 0, len(matches))
	for _, m := range matches {
		params = append(params, Parameter{
			Name:     m[1],
			In:       "path",
			Required: true,
			Schema:   &Schema{Type: "string"},
		})
	}
	out := pathParamPattern.ReplaceAllString(p, "{$1}")
	return out, params
}

// queryParamsFromArgs promotes top-level scalar fields of an Args
// type to OpenAPI query parameters. Used for GET / DELETE handlers
// where the framework binds query strings into the Args struct.
//
// Non-scalar fields (nested objects, arrays of objects) are skipped
// — OpenAPI's query parameter model can't express them cleanly, and
// passing JSON in query strings is bad form anyway. Operators who
// need that should switch the operation to POST.
func queryParamsFromArgs(ref *registry.TypeRef) []Parameter {
	if ref == nil {
		return nil
	}
	// Resolve refs into named types we don't have here — caller
	// already handed us either an inline object or a ref we can't
	// inline. For "ref" Kind we can't walk fields without the Refs
	// pool; skip rather than emit broken parameters.
	if ref.Kind != "object" || ref.Object == nil {
		return nil
	}
	out := make([]Parameter, 0, len(ref.Object.Fields))
	for _, f := range ref.Object.Fields {
		if !isScalarType(&f.Type) {
			continue
		}
		out = append(out, Parameter{
			Name:        wireName(f),
			In:          "query",
			Required:    !f.Optional,
			Description: f.Description,
			Schema:      schemaFromTypeRef(&f.Type),
		})
	}
	return out
}

// isScalarType reports whether a TypeRef is a primitive (string,
// number, integer, boolean) — i.e. cleanly representable as a query
// parameter. Pointer/optional wrappers preserve scalar-ness.
func isScalarType(t *registry.TypeRef) bool {
	if t == nil {
		return false
	}
	return t.Kind == "primitive"
}

// wireName picks the field's on-the-wire name with the same rules
// the rest of the framework uses: JSONName when set, else
// GraphQLName, else the Go field name. Keeps OpenAPI parameter
// names consistent with what clients actually send.
func wireName(f registry.FieldSchema) string {
	if f.JSONName != "" {
		return f.JSONName
	}
	if f.GraphQLName != "" {
		return f.GraphQLName
	}
	return f.Name
}

// paramNameSet builds a set of parameter names already present.
// Used to dedupe path params from query params when an Args field
// happens to share a name with a :path token.
func paramNameSet(params []Parameter) map[string]struct{} {
	out := make(map[string]struct{}, len(params))
	for _, p := range params {
		out[p.Name] = struct{}{}
	}
	return out
}

// operationID builds a stable, human-readable operationId. SDK
// generators use this as the method name on the generated client,
// so cleanliness matters.
//
// Rules:
//   - GraphQL: use ep.Name (the resolver name — e.g. "listAdverts")
//   - REST:    {service}_{method}_{path-segments}, with path params
//     and slashes squashed to underscores
//
// Examples:
//
//	rest GET /users/:id        →  users_get_users_byId
//	rest POST /orders           →  orders_post_orders
//	graphql query listAdverts   →  listAdverts
func operationID(ep client.EndpointInfo) string {
	if ep.Transport == "graphql" {
		return ep.Name
	}
	// Strip leading slash, replace each ":id" with "byId" and "/"
	// with "_". Heuristics tuned for the typical CRUD shape.
	path := strings.TrimPrefix(ep.Path, "/")
	path = pathParamPattern.ReplaceAllStringFunc(path, func(m string) string {
		// ":id" → "byId" (capitalize the first letter after :)
		name := m[1:]
		if name == "" {
			return ""
		}
		return "by" + strings.ToUpper(name[:1]) + name[1:]
	})
	path = strings.ReplaceAll(path, "/", "_")
	method := strings.ToLower(pickMethod(ep))
	if ep.Service != "" {
		return ep.Service + "_" + method + "_" + path
	}
	return method + "_" + path
}
