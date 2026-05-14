package nexus

import (
	"context"
	"fmt"
	"reflect"

	"github.com/graphql-go/graphql"
	"go.uber.org/fx"

	"github.com/paulmanoni/nexus/dataloader"
	"github.com/paulmanoni/nexus/graph"
)

// LoadField registers a batched virtual field on the GraphQL Object
// type for Parent. The field's resolver pulls a per-request Loader
// from context, enqueues the parent's key, and returns a thunk that
// graphql-go's executor dethunks breadth-first — collapsing N
// individual lookups across sibling resolvers into one fetch call.
//
// This is the framework-shaped fix for the N+1 pattern that nested
// GraphQL resolvers otherwise produce. Parent and Child are Go
// types; Key is the lookup key (typically int64 / string / UUID).
//
// The third argument is either a fetch function or an fx-factory:
//
//	// (a) Direct fetch — closes over any deps you can capture at
//	// module-construction time.
//	nexus.LoadField[User, int64, *BankDetail](
//	    "bankDetail",
//	    func(u User) int64 { return u.ID },
//	    func(ctx context.Context, ids []int64) (map[int64]*BankDetail, error) {
//	        return db.BankDetailsByUserIDs(ctx, ids)
//	    },
//	)
//
//	// (b) fx-factory — params resolved from the fx graph at boot.
//	// Matches the constructor pattern AsRest/AsQuery already use.
//	func NewBankDetailFetcher(db *DB) dataloader.Fetch[int64, *BankDetail] {
//	    return func(ctx context.Context, ids []int64) (map[int64]*BankDetail, error) {
//	        return db.BankDetailsByUserIDs(ctx, ids)
//	    }
//	}
//	nexus.LoadField[User, int64, *BankDetail](
//	    "bankDetail",
//	    func(u User) int64 { return u.ID },
//	    NewBankDetailFetcher,
//	)
//
// The framework picks the form via structural reflection: a function
// matching the fetch signature is form (a); a function whose single
// return matches the fetch signature is form (b).
//
// Parent's SDL name is the Go type's reflect name (User → "User").
// Type aliases unwrap to the original — `type User = users.Row`
// registers against "Row", not "User". Use a defined type
// (`type User users.Row`) or rename if that's not what you want.
func LoadField[Parent any, Key comparable, Child any](
	fieldName string,
	keyFn func(Parent) Key,
	factory any,
) Option {
	if fieldName == "" {
		return rawOption{o: fx.Error(fmt.Errorf("nexus.LoadField: fieldName cannot be empty"))}
	}
	if keyFn == nil {
		return rawOption{o: fx.Error(fmt.Errorf("nexus.LoadField: keyFn cannot be nil"))}
	}
	if factory == nil {
		return rawOption{o: fx.Error(fmt.Errorf("nexus.LoadField: factory cannot be nil"))}
	}

	parentT := reflect.TypeOf((*Parent)(nil)).Elem()
	for parentT.Kind() == reflect.Ptr {
		parentT = parentT.Elem()
	}
	parentName := parentT.Name()
	if parentName == "" {
		return rawOption{o: fx.Error(fmt.Errorf(
			"nexus.LoadField: Parent type must be a named struct (anonymous structs have no SDL name)"))}
	}

	childT := reflect.TypeOf((*Child)(nil)).Elem()
	outputType := graph.OutputType(childT)
	if outputType == nil {
		return rawOption{o: fx.Error(fmt.Errorf(
			"nexus.LoadField: cannot derive GraphQL type for Child=%s on field %q",
			childT, fieldName))}
	}

	// Scoped by (parent, field) so two LoadFields on different
	// parents with the same fieldName get distinct loaders.
	loaderName := parentName + "." + fieldName

	// Build the field that ends up in the schema, parameterized by
	// the eventual fetch. We defer fetch capture until either
	// "now" (direct form) or "fx-resolve time" (factory form).
	buildField := func(fetch dataloader.Fetch[Key, Child]) *graphql.Field {
		return &graphql.Field{
			Type: outputType,
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				parent, ok := coerceParent[Parent](p.Source)
				if !ok {
					return nil, fmt.Errorf(
						"nexus.LoadField %s: cannot coerce source %T to %s",
						loaderName, p.Source, parentT)
				}
				loader := dataloader.Get[Key, Child](p.Context, loaderName, fetch)
				return loader.LoadCtx(p.Context, keyFn(parent)), nil
			},
		}
	}

	factoryType := reflect.TypeOf(factory)
	if factoryType.Kind() != reflect.Func {
		return rawOption{o: fx.Error(fmt.Errorf(
			"nexus.LoadField: factory must be a function, got %s", factoryType))}
	}

	// Form detection. Both forms are functions; distinguish by
	// signature shape so users don't have to import the dataloader
	// package just to type-annotate their factory.
	if matchesFetchSignature[Key, Child](factoryType) {
		// Form (a): direct fetch. Convert to the typed Fetch via
		// a small reflection trampoline so callers don't have to
		// declare the dataloader type either.
		fetch := makeFetchFromUntyped[Key, Child](factory)
		field := buildField(fetch)
		return rawOption{o: fx.Invoke(func() {
			graph.RegisterVirtualField(parentName, fieldName, field)
		})}
	}

	if factoryType.NumOut() == 1 && matchesFetchSignature[Key, Child](factoryType.Out(0)) {
		// Form (b): fx-factory. Build a wrapper whose params match
		// the factory's so fx resolves them, then invoke the
		// factory at boot to obtain the concrete fetch and
		// register the field.
		paramTypes := make([]reflect.Type, factoryType.NumIn())
		for i := 0; i < factoryType.NumIn(); i++ {
			paramTypes[i] = factoryType.In(i)
		}
		wrapperType := reflect.FuncOf(paramTypes, nil, factoryType.IsVariadic())
		wrapper := reflect.MakeFunc(wrapperType, func(args []reflect.Value) []reflect.Value {
			results := reflect.ValueOf(factory).Call(args)
			fetch := makeFetchFromValue[Key, Child](results[0])
			field := buildField(fetch)
			graph.RegisterVirtualField(parentName, fieldName, field)
			return nil
		})
		return rawOption{o: fx.Invoke(wrapper.Interface())}
	}

	return rawOption{o: fx.Error(fmt.Errorf(
		"nexus.LoadField: factory must be Fetch[%s, %s] or a function returning it, got %s",
		reflect.TypeOf((*Key)(nil)).Elem(), childT, factoryType))}
}

// matchesFetchSignature returns true when t structurally matches
// `func(context.Context, []K) (map[K]V, error)` — the underlying
// shape of dataloader.Fetch[K, V]. Used to detect both direct fetch
// functions and factory return types without forcing the user to
// import the dataloader package for type-annotation purposes.
func matchesFetchSignature[K comparable, V any](t reflect.Type) bool {
	if t == nil || t.Kind() != reflect.Func {
		return false
	}
	ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()
	keyType := reflect.TypeOf((*K)(nil)).Elem()
	valType := reflect.TypeOf((*V)(nil)).Elem()
	errorType := reflect.TypeOf((*error)(nil)).Elem()
	return t.NumIn() == 2 &&
		t.In(0) == ctxType &&
		t.In(1) == reflect.SliceOf(keyType) &&
		t.NumOut() == 2 &&
		t.Out(0) == reflect.MapOf(keyType, valType) &&
		t.Out(1) == errorType
}

// makeFetchFromUntyped wraps an `any` whose dynamic type is a
// function matching the fetch signature into a typed
// dataloader.Fetch[K, V]. We can't just type-assert because the
// user may have written a function literal rather than a value
// explicitly typed as dataloader.Fetch.
func makeFetchFromUntyped[K comparable, V any](fn any) dataloader.Fetch[K, V] {
	rv := reflect.ValueOf(fn)
	return func(ctx context.Context, keys []K) (map[K]V, error) {
		results := rv.Call([]reflect.Value{
			reflect.ValueOf(ctx),
			reflect.ValueOf(keys),
		})
		var m map[K]V
		if !results[0].IsNil() {
			m = results[0].Interface().(map[K]V)
		}
		var err error
		if !results[1].IsNil() {
			err = results[1].Interface().(error)
		}
		return m, err
	}
}

// makeFetchFromValue is makeFetchFromUntyped for a reflect.Value
// (skipping a redundant reflect.ValueOf). Used by the factory form
// where we already have the result Value.
func makeFetchFromValue[K comparable, V any](rv reflect.Value) dataloader.Fetch[K, V] {
	return func(ctx context.Context, keys []K) (map[K]V, error) {
		results := rv.Call([]reflect.Value{
			reflect.ValueOf(ctx),
			reflect.ValueOf(keys),
		})
		var m map[K]V
		if !results[0].IsNil() {
			m = results[0].Interface().(map[K]V)
		}
		var err error
		if !results[1].IsNil() {
			err = results[1].Interface().(error)
		}
		return m, err
	}
}

// coerceParent narrows an arbitrary p.Source to Parent. Handles
// Source = Parent, *Parent, or a reflect.Value wrapping either.
func coerceParent[Parent any](source any) (Parent, bool) {
	if v, ok := source.(Parent); ok {
		return v, true
	}
	rv := reflect.ValueOf(source)
	for rv.IsValid() && rv.Kind() == reflect.Ptr {
		rv = rv.Elem()
	}
	if !rv.IsValid() {
		var zero Parent
		return zero, false
	}
	if v, ok := rv.Interface().(Parent); ok {
		return v, true
	}
	var zero Parent
	return zero, false
}
