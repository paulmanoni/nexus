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
// The third argument is a function in one of three shapes; the
// framework picks the right path via structural reflection:
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
//	// (b) fx-factory — separate constructor whose params come from
//	// the fx graph at boot. Same shape as AsRest/AsQuery's
//	// constructor pattern.
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
//	// (c) Inline with fx-injected deps — same idea as (b) but
//	// without the extra constructor. Trailing params after
//	// (ctx, []Key) are resolved from the fx graph at boot, then
//	// captured in the closure that the framework registers as the
//	// fetch. Any type fx can resolve is fair game — *DB,
//	// *CacheManager, *Service, etc.
//	nexus.LoadField[User, int64, *BankDetail](
//	    "bankDetail",
//	    func(u User) int64 { return u.ID },
//	    func(ctx context.Context, ids []int64, db *DB, cache *CacheManager) (map[int64]*BankDetail, error) {
//	        // fx resolves *DB and *CacheManager at boot; this
//	        // function is invoked once per request batch with the
//	        // captured deps already in scope.
//	        ...
//	    },
//	)
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

	// Form detection. All three shapes are functions; distinguish by
	// signature so users don't have to import the dataloader package
	// for type-annotation purposes. Order matters: form (c) is a
	// superset of (a) at NumIn>=3, so (c) must be checked first;
	// otherwise (a) would reject any function with deps as a
	// signature mismatch.
	if matchesInlineDepsSignature[Key, Child](factoryType) {
		// Form (c): inline-with-deps. Wrapper params are the
		// factory's params from index 2 onwards (the deps); fx
		// resolves them at boot and the closure captures them.
		depTypes := make([]reflect.Type, 0, factoryType.NumIn()-2)
		for i := 2; i < factoryType.NumIn(); i++ {
			depTypes = append(depTypes, factoryType.In(i))
		}
		wrapperType := reflect.FuncOf(depTypes, nil, factoryType.IsVariadic())
		factoryV := reflect.ValueOf(factory)
		wrapper := reflect.MakeFunc(wrapperType, func(deps []reflect.Value) []reflect.Value {
			// Captured deps live for the lifetime of the app;
			// rebuilding the args slice per request keeps the
			// closure pointer-clean and lets Go GC the
			// per-batch ctx/keys values.
			fetch := func(ctx context.Context, keys []Key) (map[Key]Child, error) {
				args := make([]reflect.Value, 0, 2+len(deps))
				args = append(args, reflect.ValueOf(ctx), reflect.ValueOf(keys))
				args = append(args, deps...)
				results := factoryV.Call(args)
				var m map[Key]Child
				if !results[0].IsNil() {
					m = results[0].Interface().(map[Key]Child)
				}
				var err error
				if !results[1].IsNil() {
					err = results[1].Interface().(error)
				}
				return m, err
			}
			graph.RegisterVirtualField(parentName, fieldName, buildField(fetch))
			return nil
		})
		return rawOption{o: fx.Invoke(wrapper.Interface())}
	}

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

// matchesInlineDepsSignature returns true when t is the inline-deps
// shape: `func(context.Context, []K, ...deps) (map[K]V, error)`.
// At least one dep is required to distinguish from the plain fetch
// signature (form a) which has the same returns but no trailing
// params.
func matchesInlineDepsSignature[K comparable, V any](t reflect.Type) bool {
	if t == nil || t.Kind() != reflect.Func {
		return false
	}
	if t.NumIn() < 3 || t.NumOut() != 2 {
		return false
	}
	ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()
	keyType := reflect.TypeOf((*K)(nil)).Elem()
	valType := reflect.TypeOf((*V)(nil)).Elem()
	errorType := reflect.TypeOf((*error)(nil)).Elem()
	return t.In(0) == ctxType &&
		t.In(1) == reflect.SliceOf(keyType) &&
		t.Out(0) == reflect.MapOf(keyType, valType) &&
		t.Out(1) == errorType
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
