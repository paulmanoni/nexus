package nexus

import (
	"context"
	"reflect"
	"sync"

	"github.com/graphql-go/graphql"
)

// Params is the bundle a reflective resolver receives when it wants more
// than just typed args — namely the resolve context, parent source, or
// schema info. Use it as the last parameter of an AsQuery / AsMutation
// handler (or AsRest, where only Context is filled).
//
//	func NewCreateAdvert(
//	    svc *AdvertsService,
//	    dbs *DBManager,
//	    cache *CacheManager,
//	    p nexus.Params[CreateAdvertArgs],
//	) (*AdvertResponse, error) {
//	    advert := Advert{Title: p.Args.Title, EmployerName: p.Args.EmployerName}
//	    return create(p.Context, advert)
//	}
//
// The type parameter T is the args struct — its fields carry the same
// `graphql:"..."` and `validate:"..."` tags as the legacy flat-args form.
// Use Params[struct{}] for resolvers that need Context/Source/Info but
// have no user-supplied args.
//
// For simple handlers that only need a context.Context, you can still take
// that as a plain parameter; Params[T] is additive, not required.
type Params[T any] struct {
	Context context.Context
	Args    T
	Source  any
	Info    graphql.ResolveInfo
	// Method is the HTTP verb for REST handlers ("GET", "POST", …). It lets
	// one handler registered for several methods (e.g. an Inertia page
	// mounted for GET+POST) branch on the verb. Empty for GraphQL / WS.
	Method string
}

// isNexusParams is a marker method that lets the reflective handler walker
// recognise Params[T] without having to pattern-match on the generic type
// name. Every Params instantiation inherits it.
func (Params[T]) isNexusParams() {}

// nexusParamsMarker is the private interface the reflection walker tests
// against. Keeping it unexported prevents unrelated types from accidentally
// opting into the Params-slot treatment by defining the method.
type nexusParamsMarker interface {
	isNexusParams()
}

// paramsArgsField returns the "Args" field's type from a Params[T] type
// (t.Field for the struct). Panics if t doesn't match the shape — only
// called after isNexusParams() passes, so the shape is guaranteed.
func paramsArgsField(t reflect.Type) reflect.Type {
	f, ok := t.FieldByName("Args")
	if !ok {
		return nil
	}
	return f.Type
}

// paramsIndices holds the field positions of the four well-known Params[T]
// fields. -1 means the field is absent. Cached per Params[T] type so
// buildParamsValue avoids a FieldByName string walk on every request.
type paramsIndices struct {
	ctx, args, source, info, method int
}

var paramsIndicesCache sync.Map // reflect.Type → paramsIndices

func getParamsIndices(t reflect.Type) paramsIndices {
	if v, ok := paramsIndicesCache.Load(t); ok {
		return v.(paramsIndices)
	}
	pi := paramsIndices{ctx: -1, args: -1, source: -1, info: -1, method: -1}
	if t.Kind() == reflect.Struct {
		for i := 0; i < t.NumField(); i++ {
			switch t.Field(i).Name {
			case "Context":
				pi.ctx = i
			case "Args":
				pi.args = i
			case "Source":
				pi.source = i
			case "Info":
				pi.info = i
			case "Method":
				pi.method = i
			}
		}
	}
	actual, _ := paramsIndicesCache.LoadOrStore(t, pi)
	return actual.(paramsIndices)
}

// buildParamsValue constructs a Params[T] reflect.Value with the supplied
// Context/Args/Source/Info. Used by as_graph and as_rest before calling a
// handler that takes a Params[T] parameter.
func buildParamsValue(paramsType reflect.Type, ctx context.Context, args reflect.Value, source any, info graphql.ResolveInfo, method string) reflect.Value {
	p := reflect.New(paramsType).Elem()
	idx := getParamsIndices(paramsType)
	if idx.ctx >= 0 {
		if ctx == nil {
			ctx = context.Background()
		}
		p.Field(idx.ctx).Set(reflect.ValueOf(ctx))
	}
	if idx.method >= 0 && method != "" {
		p.Field(idx.method).SetString(method)
	}
	if idx.args >= 0 && args.IsValid() {
		p.Field(idx.args).Set(args)
	}
	if idx.source >= 0 && source != nil {
		p.Field(idx.source).Set(reflect.ValueOf(source))
	}
	if idx.info >= 0 {
		p.Field(idx.info).Set(reflect.ValueOf(info))
	}
	return p
}
