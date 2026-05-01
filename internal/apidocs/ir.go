// Package apidocs holds the Sphinx-style API reference generator.
//
// The pipeline is collector → IR → renderer:
//
//   - collector walks a user's app source via go/packages, finds calls
//     to nexus.Module / AsQuery / AsMutation / AsRest / AsWS, resolves
//     each handler constructor, extracts its doc comment, parameter
//     types, and (when present) the nexus.Params[T] argument struct
//     fields with their tags.
//
//   - IR (this file) is the single shape the collector produces and
//     every renderer consumes. JSON-stable so it can be diff'd in CI
//     and consumed by external tools.
//
//   - renderers (HTML for the dashboard's Docs tab, PDF via chromedp)
//     read the IR and never re-parse Go source.
package apidocs

// Doc is the top-level IR document — one per app scan.
type Doc struct {
	Module     string    `json:"module"`               // user's go.mod module path
	GoVersion  string    `json:"goVersion,omitempty"`  // toolchain that produced the IR
	Modules    []Module  `json:"modules"`              // nexus.Module(...) calls found
	Loose      []Handler `json:"loose,omitempty"`      // AsQuery/AsRest/etc. found outside any Module
	Entities   []Entity  `json:"entities,omitempty"`   // named structs referenced by any handler (transitive)
	LoadErrors int       `json:"loadErrors,omitempty"` // type-check errors encountered (best-effort)
}

// Module mirrors a `nexus.Module(name, opts...)` declaration. The
// name is the literal string passed as the first argument; we don't
// constant-fold variables, so apps that compute module names won't
// be captured here yet.
type Module struct {
	Name      string    `json:"name"`
	Doc       string    `json:"doc,omitempty"`       // doc comment on the var/decl that defines the module
	Pos       Pos       `json:"pos"`                 // source location of the nexus.Module call
	Handlers  []Handler `json:"handlers"`            // AsQuery/AsMutation/AsRest/AsWS calls inside this module
	Provides  []string  `json:"provides,omitempty"`  // constructor names passed to nexus.Provide
	RoutePath string    `json:"routePath,omitempty"` // nexus.Path("/x") or nexus.RoutePrefix("/x")
}

// Handler is one endpoint registration (any transport). The shape is
// uniform across kinds so renderers can iterate without switching:
// transport-specific fields are populated only when relevant.
type Handler struct {
	Kind        string `json:"kind"`                  // "query" | "mutation" | "rest" | "ws"
	Name        string `json:"name"`                  // op name (constructor name with "New" prefix stripped)
	Func        string `json:"func"`                  // fully-qualified constructor name
	Doc         string `json:"doc,omitempty"`         // doc comment on the constructor
	Pos         Pos    `json:"pos"`                   // source location of the registration call
	HTTP        *HTTP  `json:"http,omitempty"`        // populated for kind=="rest"
	WS          *WS    `json:"ws,omitempty"`          // populated for kind=="ws"
	Deps        []Dep  `json:"deps,omitempty"`        // injected constructor params (services, DBs, caches)
	Args        *Args  `json:"args,omitempty"`        // resolved Params[T] / trailing struct shape
	Returns     string `json:"returns,omitempty"`     // first non-error return type, pretty-printed
	ReturnsLink string `json:"returnsLink,omitempty"` // entity slug if Returns names a documented type
	Options     []Opt  `json:"options,omitempty"`     // nexus.Use / GraphMiddleware / auth.Required / etc.
}

// HTTP carries the REST-only fields. Method + path are the literal
// args to nexus.AsRest.
type HTTP struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

// WS carries the WebSocket-only fields: the upgrade path plus the
// envelope "type" string this handler dispatches on.
type WS struct {
	Path        string `json:"path"`
	MessageType string `json:"messageType"`
}

// Dep is one injected dependency on a handler constructor — a service
// wrapper, a typed DB, a cache manager, etc. Not the Params[T] arg.
type Dep struct {
	Name     string `json:"name"`               // parameter name in the constructor signature
	Type     string `json:"type"`               // pretty-printed Go type, e.g. "*MainDB"
	TypeLink string `json:"typeLink,omitempty"` // entity slug if Type names a documented type
}

// Args describes the handler's argument shape. For a Params[T]
// signature, Type is T's name and Fields enumerates T's exported
// fields. Anonymous structs (e.g. `struct{ Input X }`) are flattened
// to Type="" with Fields populated.
type Args struct {
	Type     string  `json:"type,omitempty"`     // named struct type, e.g. "CreateAdvertArgs"
	TypeLink string  `json:"typeLink,omitempty"` // entity slug if Type names a documented type
	Doc      string  `json:"doc,omitempty"`      // doc comment on the named type
	Fields   []Field `json:"fields,omitempty"`   // exported fields, in declaration order
}

// Field is one struct field surfaced in the docs. Tags drive the
// per-transport behavior (graphql:"name,required", validate:"...",
// json:"...", path:"id"); we surface them raw so the renderer can
// format per its own conventions.
type Field struct {
	Name     string            `json:"name"`
	Type     string            `json:"type"`
	TypeLink string            `json:"typeLink,omitempty"` // entity slug if Type names a documented type
	Doc      string            `json:"doc,omitempty"`
	Tags     map[string]string `json:"tags,omitempty"`
	Optional bool              `json:"optional,omitempty"` // pointer type or no `required` graphql tag
}

// Opt is one option passed alongside the handler — middleware,
// auth gate, rate limit, etc. We don't try to interpret them yet;
// the literal call expression is captured so renderers can show
// "auth.Required()" verbatim.
type Opt struct {
	Expr string `json:"expr"`          // source representation, e.g. "auth.Required()"
	Doc  string `json:"doc,omitempty"` // doc comment on the called function, when resolvable
}

// Pos is a source location, kept tiny so it round-trips through JSON
// without bloat. Renderers turn this into "GitHub permalink" style
// URLs when given a base.
type Pos struct {
	File string `json:"file"`
	Line int    `json:"line"`
}

// Entity is a named struct type referenced (transitively) by at least
// one handler — return value, dep, or args field. The collector only
// emits entities defined inside the user's go.mod module path; types
// from third-party packages are linked-by-name only, not documented.
type Entity struct {
	Name    string         `json:"name"`              // type name (no package prefix)
	Pkg     string         `json:"pkg"`               // import path of the defining package
	Slug    string         `json:"slug"`              // stable anchor id, used by *Link fields
	Doc     string         `json:"doc,omitempty"`     // doc comment on the type declaration
	Pos     Pos            `json:"pos"`               // source location of the type spec
	Fields  []EntityField  `json:"fields,omitempty"`  // exported fields, in declaration order
	Methods []EntityMethod `json:"methods,omitempty"` // exported methods declared on the type
}

// EntityMethod is one exported method on an Entity — typically helper
// methods on a service wrapper (e.g. AdvertsService.WithTx). Captured
// so service surfaces are documented alongside the typed fields.
type EntityMethod struct {
	Name      string `json:"name"`
	Signature string `json:"signature"`           // pretty-printed func signature, e.g. "(ctx context.Context) (*User, error)"
	Doc       string `json:"doc,omitempty"`       // doc comment on the method
	Pos       Pos    `json:"pos"`                 // source location of the method declaration
	Receiver  string `json:"receiver,omitempty"`  // pointer? value? — kept for transparency in the IR
}

// EntityField mirrors Field but for an Entity's own struct definition.
// Kept separate from Field so the two can evolve independently —
// Entities may eventually grow methods, embedded types, etc.
type EntityField struct {
	Name     string            `json:"name"`
	Type     string            `json:"type"`
	TypeLink string            `json:"typeLink,omitempty"` // slug into Doc.Entities for nested struct refs
	Doc      string            `json:"doc,omitempty"`
	Tags     map[string]string `json:"tags,omitempty"`
}