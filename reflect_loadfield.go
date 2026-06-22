package nexus

import (
	"context"
	"fmt"
	"reflect"

	"github.com/graphql-go/graphql"
	"github.com/paulmanoni/nexus/di"

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
// The fetch argument is one of three shapes; the framework picks
// the right path by reflecting on it at registration time:
//
//	// (a) Direct fetch — no fx deps. Type-check at compile time.
//	nexus.LoadField[User, int64, *Bank](
//	    "bank",
//	    func(u User) int64 { return u.ID },
//	    func(ctx context.Context, ids []int64) (map[int64]*Bank, error) {
//	        return db.BanksByUserIDs(ctx, ids)
//	    },
//	)
//
//	// (b) Constructor returning Fetch[K, V] — fx resolves the
//	//     constructor's params at boot. Inner Fetch is fully typed.
//	func NewBankFetcher(db *DB) dataloader.Fetch[int64, *Bank] {
//	    return func(ctx context.Context, ids []int64) (map[int64]*Bank, error) {
//	        return db.BanksByUserIDs(ctx, ids)
//	    }
//	}
//	nexus.LoadField[User, int64, *Bank]("bank", keyFn, NewBankFetcher)
//
//	// (c) Inline fetch with trailing fx-injected deps — fx resolves
//	//     the deps at boot; ctx + keys remain the head of the fn.
//	nexus.LoadField[User, int64, *Bank](
//	    "bank",
//	    func(u User) int64 { return u.ID },
//	    func(ctx context.Context, ids []int64, db *DB, cache *Cache) (map[int64]*Bank, error) {
//	        ...
//	    },
//	)
//
// Wrong shapes surface as an di.Error at app start with a message
// describing the expected signatures.
//
// Parent's SDL name is the Go type's reflect name (User → "User").
// Type aliases unwrap to the original (`type User = users.Row`
// registers against "Row"); use a defined type or rename if that
// isn't what you want.
func LoadField[Parent any, Key comparable, Child any](
	fieldName string,
	keyFn func(Parent) Key,
	fetch any,
) Option {
	if fieldName == "" {
		return rawOption{o: di.Error(fmt.Errorf("nexus.LoadField: fieldName cannot be empty"))}
	}
	if keyFn == nil {
		return rawOption{o: di.Error(fmt.Errorf("nexus.LoadField: keyFn cannot be nil"))}
	}
	if fetch == nil {
		return rawOption{o: di.Error(fmt.Errorf("nexus.LoadField: fetch cannot be nil"))}
	}

	parentName, outputType, errOpt := resolveLoadFieldTypes[Parent, Child](fieldName)
	if errOpt != nil {
		return errOpt
	}

	fetchType := reflect.TypeOf(fetch)
	if fetchType.Kind() != reflect.Func {
		return rawOption{o: di.Error(fmt.Errorf(
			"nexus.LoadField: fetch must be a function, got %s", fetchType))}
	}

	// Form (a): direct fetch matching dataloader.Fetch[Key, Child].
	// Cheapest path — no fx graph involvement beyond registration.
	if matchesFetchSignature[Key, Child](fetchType) {
		typed := makeFetchFromValue[Key, Child](reflect.ValueOf(fetch))
		field := buildLoadFieldResolver[Parent, Key, Child](
			parentName, fieldName, outputType, keyFn, typed,
		)
		return rawOption{o: di.Invoke(func() {
			graph.RegisterVirtualField(parentName, fieldName, field)
		})}
	}

	// Form (c): inline with trailing deps — `func(ctx, []K, deps...) (map[K]V, error)`.
	// Checked before (b) because both can have NumIn>=3; the shape
	// of the head params (ctx + []Key) is distinctive.
	if matchesInlineDepsSignature[Key, Child](fetchType) {
		return buildInlineDepsOption[Key, Child](
			parentName, fieldName, outputType, keyFn, fetch, fetchType,
		)
	}

	// Form (b): factory whose single return is dataloader.Fetch[K, V].
	if fetchType.NumOut() == 1 && matchesFetchSignature[Key, Child](fetchType.Out(0)) {
		return buildFactoryOption[Parent, Key, Child](
			parentName, fieldName, outputType, keyFn, fetch, fetchType,
		)
	}

	return rawOption{o: di.Error(fmt.Errorf(
		"nexus.LoadField: fetch must be one of: "+
			"func(context.Context, []K) (map[K]V, error), "+
			"func(deps...) dataloader.Fetch[K, V], or "+
			"func(context.Context, []K, deps...) (map[K]V, error); got %s",
		fetchType))}
}

// --- shared internals ---

// resolveLoadFieldTypes resolves the parent SDL name and the child's
// graphql.Output type from the generics.
func resolveLoadFieldTypes[Parent any, Child any](fieldName string) (
	parentName string, outputType graphql.Output, errOpt Option,
) {
	parentT := reflect.TypeOf((*Parent)(nil)).Elem()
	for parentT.Kind() == reflect.Ptr {
		parentT = parentT.Elem()
	}
	parentName = parentT.Name()
	if parentName == "" {
		return "", nil, rawOption{o: di.Error(fmt.Errorf(
			"nexus.LoadField: Parent type must be a named struct (anonymous structs have no SDL name)"))}
	}

	childT := reflect.TypeOf((*Child)(nil)).Elem()
	outputType = graph.OutputType(childT)
	if outputType == nil {
		return "", nil, rawOption{o: di.Error(fmt.Errorf(
			"nexus.LoadField: cannot derive GraphQL type for Child=%s on field %q",
			childT, fieldName))}
	}
	return parentName, outputType, nil
}

// buildLoadFieldResolver wraps a typed fetch in the graphql.Field
// that ends up registered on the parent type. Per-request loader
// lookup happens in the Resolve closure.
func buildLoadFieldResolver[Parent any, Key comparable, Child any](
	parentName, fieldName string,
	outputType graphql.Output,
	keyFn func(Parent) Key,
	fetch dataloader.Fetch[Key, Child],
) *graphql.Field {
	loaderName := parentName + "." + fieldName
	parentT := reflect.TypeOf((*Parent)(nil)).Elem()
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

// buildInlineDepsOption handles form (c): the user wrote a single
// function `func(ctx, []K, deps...) (map[K]V, error)`. We construct
// an di.Invoke whose parameter types are the trailing deps so fx
// resolves them at boot; the wrapper builds a closure that captures
// those resolved deps and calls the user's function on every batch.
func buildInlineDepsOption[Key comparable, Child any](
	parentName, fieldName string,
	outputType graphql.Output,
	keyFn any, // typed as func(Parent) Key — Parent isn't on this fn's generics, so accept any
	factory any,
	factoryType reflect.Type,
) Option {
	depTypes := make([]reflect.Type, 0, factoryType.NumIn()-2)
	for i := 2; i < factoryType.NumIn(); i++ {
		depTypes = append(depTypes, factoryType.In(i))
	}
	wrapperType := reflect.FuncOf(depTypes, nil, factoryType.IsVariadic())
	factoryV := reflect.ValueOf(factory)

	wrapper := reflect.MakeFunc(wrapperType, func(deps []reflect.Value) []reflect.Value {
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
		field := buildVirtualFieldFromUntypedKeyFn[Key, Child](
			parentName, fieldName, outputType, keyFn, fetch,
		)
		graph.RegisterVirtualField(parentName, fieldName, field)
		return nil
	})
	return rawOption{o: di.Invoke(wrapper.Interface())}
}

// buildFactoryOption handles form (b): the user's function returns
// a Fetch[K, V]. fx resolves the factory's params at boot; we call
// the factory once to obtain the concrete fetch and register the
// field.
func buildFactoryOption[Parent any, Key comparable, Child any](
	parentName, fieldName string,
	outputType graphql.Output,
	keyFn func(Parent) Key,
	factory any,
	factoryType reflect.Type,
) Option {
	paramTypes := make([]reflect.Type, factoryType.NumIn())
	for i := 0; i < factoryType.NumIn(); i++ {
		paramTypes[i] = factoryType.In(i)
	}
	wrapperType := reflect.FuncOf(paramTypes, nil, factoryType.IsVariadic())
	wrapper := reflect.MakeFunc(wrapperType, func(args []reflect.Value) []reflect.Value {
		results := reflect.ValueOf(factory).Call(args)
		fetch := makeFetchFromValue[Key, Child](results[0])
		field := buildLoadFieldResolver[Parent, Key, Child](
			parentName, fieldName, outputType, keyFn, fetch,
		)
		graph.RegisterVirtualField(parentName, fieldName, field)
		return nil
	})
	return rawOption{o: di.Invoke(wrapper.Interface())}
}

// buildVirtualFieldFromUntypedKeyFn is buildLoadFieldResolver for the
// form-c codepath where Parent isn't on the local generics. The
// keyFn is `any` here, but at runtime it's been validated as
// `func(Parent) Key` upstream; we reflect-call it.
func buildVirtualFieldFromUntypedKeyFn[Key comparable, Child any](
	parentName, fieldName string,
	outputType graphql.Output,
	keyFn any,
	fetch dataloader.Fetch[Key, Child],
) *graphql.Field {
	loaderName := parentName + "." + fieldName
	keyFnV := reflect.ValueOf(keyFn)
	parentT := keyFnV.Type().In(0)
	return &graphql.Field{
		Type: outputType,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			// Unwrap pointer levels until src's type matches parentT
			// (which might itself be a pointer). graphql-go gives us
			// p.Source as the parent struct value, but downstream
			// reflective walks may hand us a *T.
			src := reflect.ValueOf(p.Source)
			for src.IsValid() && src.Type() != parentT && src.Kind() == reflect.Ptr {
				src = src.Elem()
			}
			if !src.IsValid() || !src.Type().AssignableTo(parentT) {
				return nil, fmt.Errorf(
					"nexus.LoadField %s: cannot coerce source %T to %s",
					loaderName, p.Source, parentT)
			}
			results := keyFnV.Call([]reflect.Value{src})
			key := results[0].Interface().(Key)

			loader := dataloader.Get[Key, Child](p.Context, loaderName, fetch)
			return loader.LoadCtx(p.Context, key), nil
		},
	}
}

// matchesInlineDepsSignature returns true when t is the inline-deps
// shape: `func(context.Context, []K, ...deps) (map[K]V, error)`.
// At least one dep is required to distinguish from a direct fetch
// (form a).
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
// shape of dataloader.Fetch[K, V]. Used both to detect a direct
// fetch (form a) and to detect a factory's return type (form b).
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

// makeFetchFromValue wraps a reflect.Value whose dynamic type is a
// function matching the fetch signature into a typed
// dataloader.Fetch[K, V].
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
