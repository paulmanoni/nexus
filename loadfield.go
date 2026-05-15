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
// The fetch argument is fully typed — your IDE autocompletes the
// signature end-to-end and shape errors surface at compile time.
// Use this form when the fetch has no fx-injected deps (you close
// over them at module-construction time, or there are none):
//
//	nexus.LoadField[User, int64, *BankDetail](
//	    "bankDetail",
//	    func(u User) int64 { return u.ID },
//	    func(ctx context.Context, ids []int64) (map[int64]*BankDetail, error) {
//	        return db.BankDetailsByUserIDs(ctx, ids)
//	    },
//	)
//
// Need fx-injected deps in the fetch? Use LoadFieldFx instead —
// same parent/key/child generics, but the factory argument is
// `any` so it can accept a constructor (form b) or an inline
// function with trailing deps (form c). The trade-off is less IDE
// help at the call site since Go can't express "function with
// these N fixed params plus M user-chosen params" at the type
// level.
//
// Parent's SDL name is the Go type's reflect name (User → "User").
// Type aliases unwrap to the original (`type User = users.Row`
// registers against "Row"); use a defined type or rename if that
// isn't what you want.
func LoadField[Parent any, Key comparable, Child any](
	fieldName string,
	keyFn func(Parent) Key,
	fetch dataloader.Fetch[Key, Child],
) Option {
	if fieldName == "" {
		return rawOption{o: fx.Error(fmt.Errorf("nexus.LoadField: fieldName cannot be empty"))}
	}
	if keyFn == nil {
		return rawOption{o: fx.Error(fmt.Errorf("nexus.LoadField: keyFn cannot be nil"))}
	}
	if fetch == nil {
		return rawOption{o: fx.Error(fmt.Errorf("nexus.LoadField: fetch cannot be nil"))}
	}

	parentName, outputType, errOpt := resolveLoadFieldTypes[Parent, Child](fieldName)
	if errOpt != nil {
		return errOpt
	}
	field := buildLoadFieldResolver[Parent, Key, Child](
		parentName, fieldName, outputType, keyFn, fetch,
	)
	return rawOption{o: fx.Invoke(func() {
		graph.RegisterVirtualField(parentName, fieldName, field)
	})}
}

// LoadField1 is LoadField with one fx-injected dep. fully typed —
// gopls autocompletes the fetch signature end-to-end, and shape
// errors surface at compile time. Use this over LoadFieldFx when
// your fetch has exactly one trailing dep (the common case: a
// database handle).
//
//	nexus.LoadField1[User, int64, *BankDetail, *DB](
//	    "bankDetail",
//	    func(u User) int64 { return u.ID },
//	    func(ctx context.Context, ids []int64, db *DB) (map[int64]*BankDetail, error) {
//	        return db.BankDetailsByUserIDs(ctx, ids)
//	    },
//	)
//
// fx resolves D1 at boot. Any type registered via Provide works.
func LoadField1[Parent any, Key comparable, Child any, D1 any](
	fieldName string,
	keyFn func(Parent) Key,
	fetch func(ctx context.Context, keys []Key, d1 D1) (map[Key]Child, error),
) Option {
	if errOpt := validateLoadFieldArgs(fieldName, keyFn, fetch); errOpt != nil {
		return errOpt
	}
	parentName, outputType, errOpt := resolveLoadFieldTypes[Parent, Child](fieldName)
	if errOpt != nil {
		return errOpt
	}
	return rawOption{o: fx.Invoke(func(d1 D1) {
		typedFetch := func(ctx context.Context, keys []Key) (map[Key]Child, error) {
			return fetch(ctx, keys, d1)
		}
		field := buildLoadFieldResolver[Parent, Key, Child](
			parentName, fieldName, outputType, keyFn, typedFetch,
		)
		graph.RegisterVirtualField(parentName, fieldName, field)
	})}
}

// LoadField2 is LoadField with two fx-injected deps. See LoadField1
// for the design rationale; this is the same pattern with one more
// type parameter for the second dep.
//
//	nexus.LoadField2[User, int64, *BankDetail, *DB, *CacheManager](
//	    "bankDetail",
//	    func(u User) int64 { return u.ID },
//	    func(ctx context.Context, ids []int64, db *DB, cache *CacheManager) (map[int64]*BankDetail, error) {
//	        ...
//	    },
//	)
func LoadField2[Parent any, Key comparable, Child any, D1 any, D2 any](
	fieldName string,
	keyFn func(Parent) Key,
	fetch func(ctx context.Context, keys []Key, d1 D1, d2 D2) (map[Key]Child, error),
) Option {
	if errOpt := validateLoadFieldArgs(fieldName, keyFn, fetch); errOpt != nil {
		return errOpt
	}
	parentName, outputType, errOpt := resolveLoadFieldTypes[Parent, Child](fieldName)
	if errOpt != nil {
		return errOpt
	}
	return rawOption{o: fx.Invoke(func(d1 D1, d2 D2) {
		typedFetch := func(ctx context.Context, keys []Key) (map[Key]Child, error) {
			return fetch(ctx, keys, d1, d2)
		}
		field := buildLoadFieldResolver[Parent, Key, Child](
			parentName, fieldName, outputType, keyFn, typedFetch,
		)
		graph.RegisterVirtualField(parentName, fieldName, field)
	})}
}

// LoadField3 is LoadField with three fx-injected deps. Past three,
// declare your deps as a struct + use LoadFieldFx form (b), or
// extract a service that wraps the dependencies — three fx-deps in
// one resolver is usually a sign that the resolver wants to be its
// own struct.
//
//	nexus.LoadField3[User, int64, *BankDetail, *DB, *CacheManager, *AuthManager](
//	    "bankDetail",
//	    func(u User) int64 { return u.ID },
//	    func(ctx context.Context, ids []int64, db *DB, cache *CacheManager, auth *AuthManager) (map[int64]*BankDetail, error) {
//	        ...
//	    },
//	)
func LoadField3[Parent any, Key comparable, Child any, D1 any, D2 any, D3 any](
	fieldName string,
	keyFn func(Parent) Key,
	fetch func(ctx context.Context, keys []Key, d1 D1, d2 D2, d3 D3) (map[Key]Child, error),
) Option {
	if errOpt := validateLoadFieldArgs(fieldName, keyFn, fetch); errOpt != nil {
		return errOpt
	}
	parentName, outputType, errOpt := resolveLoadFieldTypes[Parent, Child](fieldName)
	if errOpt != nil {
		return errOpt
	}
	return rawOption{o: fx.Invoke(func(d1 D1, d2 D2, d3 D3) {
		typedFetch := func(ctx context.Context, keys []Key) (map[Key]Child, error) {
			return fetch(ctx, keys, d1, d2, d3)
		}
		field := buildLoadFieldResolver[Parent, Key, Child](
			parentName, fieldName, outputType, keyFn, typedFetch,
		)
		graph.RegisterVirtualField(parentName, fieldName, field)
	})}
}

// validateLoadFieldArgs centralizes the nil-check trio every typed
// LoadField variant performs. Each variant calls this with its own
// fetch (different concrete func type), so the param is any.
func validateLoadFieldArgs(fieldName string, keyFn any, fetch any) Option {
	if fieldName == "" {
		return rawOption{o: fx.Error(fmt.Errorf("nexus.LoadField: fieldName cannot be empty"))}
	}
	if keyFn == nil {
		return rawOption{o: fx.Error(fmt.Errorf("nexus.LoadField: keyFn cannot be nil"))}
	}
	if fetch == nil {
		return rawOption{o: fx.Error(fmt.Errorf("nexus.LoadField: fetch cannot be nil"))}
	}
	return nil
}

// LoadFieldFx is LoadField for fetches that need fx-injected deps.
// The factory argument is `any`, accepted in either of two shapes
// the framework discriminates via reflection at registration time:
//
//	// (b) fx-factory — separate constructor, fully typed return.
//	func NewBankDetailFetcher(db *DB) dataloader.Fetch[int64, *BankDetail] {
//	    return func(ctx context.Context, ids []int64) (map[int64]*BankDetail, error) {
//	        return db.BankDetailsByUserIDs(ctx, ids)
//	    }
//	}
//	nexus.LoadFieldFx[User, int64, *BankDetail](
//	    "bankDetail",
//	    func(u User) int64 { return u.ID },
//	    NewBankDetailFetcher,
//	)
//
//	// (c) Inline with fx-injected deps — trailing positional args
//	// after (ctx, []Key) are resolved from the fx graph at boot.
//	// Any type fx can resolve is fair game.
//	nexus.LoadFieldFx[User, int64, *BankDetail](
//	    "bankDetail",
//	    func(u User) int64 { return u.ID },
//	    func(ctx context.Context, ids []int64, db *DB, cache *CacheManager) (map[int64]*BankDetail, error) {
//	        ...
//	    },
//	)
//
// Wrong shapes surface as an fx.Error at app start. For typed-fetch
// callsites (no deps), prefer LoadField — same generics, full IDE
// help.
func LoadFieldFx[Parent any, Key comparable, Child any](
	fieldName string,
	keyFn func(Parent) Key,
	factory any,
) Option {
	if fieldName == "" {
		return rawOption{o: fx.Error(fmt.Errorf("nexus.LoadFieldFx: fieldName cannot be empty"))}
	}
	if keyFn == nil {
		return rawOption{o: fx.Error(fmt.Errorf("nexus.LoadFieldFx: keyFn cannot be nil"))}
	}
	if factory == nil {
		return rawOption{o: fx.Error(fmt.Errorf("nexus.LoadFieldFx: factory cannot be nil"))}
	}

	parentName, outputType, errOpt := resolveLoadFieldTypes[Parent, Child](fieldName)
	if errOpt != nil {
		return errOpt
	}

	factoryType := reflect.TypeOf(factory)
	if factoryType.Kind() != reflect.Func {
		return rawOption{o: fx.Error(fmt.Errorf(
			"nexus.LoadFieldFx: factory must be a function, got %s", factoryType))}
	}

	// Form (c) is a superset of (a) at NumIn>=3, so it's checked
	// first. (a) isn't reachable here at all — that's what the
	// typed LoadField is for; if a caller hands LoadFieldFx a
	// no-deps fetch we point them at LoadField in the error.
	if matchesInlineDepsSignature[Key, Child](factoryType) {
		return buildInlineDepsOption[Key, Child](
			parentName, fieldName, outputType, keyFn, factory, factoryType,
		)
	}

	if factoryType.NumOut() == 1 && matchesFetchSignature[Key, Child](factoryType.Out(0)) {
		return buildFactoryOption[Parent, Key, Child](
			parentName, fieldName, outputType, keyFn, factory, factoryType,
		)
	}

	if matchesFetchSignature[Key, Child](factoryType) {
		return rawOption{o: fx.Error(fmt.Errorf(
			"nexus.LoadFieldFx: factory has no fx deps — use nexus.LoadField for typed direct-fetch instead"))}
	}

	return rawOption{o: fx.Error(fmt.Errorf(
		"nexus.LoadFieldFx: factory must be a constructor returning Fetch[K, V] or "+
			"an inline fn (ctx, []K, deps...) (map[K]V, error); got %s",
		factoryType))}
}

// --- shared internals ---

// resolveLoadFieldTypes resolves the parent SDL name and the child's
// graphql.Output type from the generics. Shared by both entry points
// so the error messages and unwrapping rules stay identical.
func resolveLoadFieldTypes[Parent any, Child any](fieldName string) (
	parentName string, outputType graphql.Output, errOpt Option,
) {
	parentT := reflect.TypeOf((*Parent)(nil)).Elem()
	for parentT.Kind() == reflect.Ptr {
		parentT = parentT.Elem()
	}
	parentName = parentT.Name()
	if parentName == "" {
		return "", nil, rawOption{o: fx.Error(fmt.Errorf(
			"nexus.LoadField: Parent type must be a named struct (anonymous structs have no SDL name)"))}
	}

	childT := reflect.TypeOf((*Child)(nil)).Elem()
	outputType = graph.OutputType(childT)
	if outputType == nil {
		return "", nil, rawOption{o: fx.Error(fmt.Errorf(
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
// an fx.Invoke whose parameter types are the trailing deps so fx
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
	return rawOption{o: fx.Invoke(wrapper.Interface())}
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
	return rawOption{o: fx.Invoke(wrapper.Interface())}
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
					"nexus.LoadFieldFx %s: cannot coerce source %T to %s",
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
// At least one dep is required to distinguish from a no-deps fetch
// (which LoadField is for).
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
// shape of dataloader.Fetch[K, V]. Used by LoadFieldFx to detect
// factory return types (and to redirect a no-deps fetch to LoadField).
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
// dataloader.Fetch[K, V]. Used by the factory form where we already
// have the result Value from invoking the constructor.
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
