// Package users is the canonical "owns its data, exposes typed REST"
// module: a minimal user catalog tagged DeployAs("users-svc") so it
// can be peeled out into its own binary later.
//
// Layout demonstrates multi-file shadow support:
//
//	types.go     — public types (User, *Args)
//	service.go   — Service struct, constructor, plain methods
//	handlers.go  — gin/graphql handler shims
//	module.go    — Module declaration
//
// Each file contributes to the same `users` package; the shadow
// generator strips Service/handlers/Module-related declarations from
// each file and synthesizes one zz_shadow_gen.go containing the
// HTTP-stub Service. Public types in types.go are preserved verbatim
// in the shadow build (they're called by checkout's signatures).
package users

// User is the public shape returned over the wire. Generated client
// code lives in this package, so it sees this type without an import
// and consumers in other packages get it via `users.User`.
type User struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// GetArgs binds a path parameter named `id`. The framework's gin
// binding reads the `uri:"id"` tag; the shadow generator's path
// expansion reads the same tag — guaranteed to round-trip.
type GetArgs struct {
	ID string `json:"id" uri:"id"`
}

// ListArgs is empty — kept as a real type (rather than struct{}) so
// the generated method has a stable shape if filters are added.
type ListArgs struct{}

// SearchArgs is the typed input for a GraphQL query.
type SearchArgs struct {
	Prefix string `graphql:"prefix" json:"prefix"`
}

// CreateArgs is the typed input for the createUser GraphQL mutation.
// Validation tags surface as chips in the dashboard's args panel —
// gives the drawer a reason to show its Arguments section.
type CreateArgs struct {
	Name string `graphql:"name,required" json:"name" validate:"required,len=2|80"`
}
