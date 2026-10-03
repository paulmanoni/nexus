package nexus

import (
	"context"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"unicode"

	"github.com/graphql-go/graphql"
	"github.com/paulmanoni/nexus/v2/di"
	"github.com/paulmanoni/nexus/v2/gql"
	"github.com/paulmanoni/nexus/v2/httpx"
	"github.com/paulmanoni/nexus/v2/internal/graph"
)

// callInput is the per-invocation environment callHandler consults to fill
// Params[T] fields and (legacy) context slots. Embedding all four here
// means as_graph / as_rest / future transports hand over the same struct
// shape no matter what data is actually available.
type callInput struct {
	Ctx    context.Context
	Source any
	Info   graphql.ResolveInfo
	// GinCtx is set by the REST transport for handlers that take a
	// *httpx.Ctx parameter — lets them reach body / path / query /
	// header parsing helpers (multipart file upload, custom status
	// codes, c.Param("...") etc.) while still participating in the
	// reflective registration shape.
	GinCtx *httpx.Ctx
	// WS is set by the WebSocket transport (AsWS) for handlers that
	// take a *WSSession — gives them a typed handle for Send / Emit /
	// room ops tied to the current connection.
	WS *WSSession
}

// Special parameter types recognized by the reflective handler shape. These
// are filled in by the framework at call time rather than fx-injected.
var (
	contextType      = reflect.TypeOf((*context.Context)(nil)).Elem()
	ginContextType   = reflect.TypeOf((*httpx.Ctx)(nil))
	wsSessionType    = reflect.TypeOf((*WSSession)(nil))
	formType         = reflect.TypeOf((*Form)(nil))
	paramsMarkerType = reflect.TypeOf((*nexusParamsMarker)(nil)).Elem()
)

// handlerShape is the reflective view of a user-supplied handler function
// shared by AsRest / AsQuery / AsMutation / AsSubscription / AsWebSocket.
//
// A handler looks like:
//
//	func(deps..., args?) (result, error)
//	func(deps..., args?) error        // REST: no body; 204/empty OK
//	func(deps..., args?) (result)     // subscription channel-returning
//
// The last param, if it is a struct type (not a pointer), is treated as the
// args container. Earlier params are fx-injected deps. The first return is
// the result; the last is error (if the signature ends with one).
// paramKind tells callHandler how to fill a position: a dep from fx, the
// resolve context, the parsed args struct, or a Params[T] bundle.
type paramKind uint8

const (
	paramDep paramKind = iota
	paramCtx
	paramArgs
	paramParams
	paramGinCtx // filled from callInput.GinCtx (REST transport only)
	paramWS     // filled from callInput.WS (AsWS transport only)
	paramForm   // *Form built from callInput.GinCtx (REST only; typed nil elsewhere)
	// paramArgField is a bare scalar parameter fed from field depPos of the
	// bound args struct — the nexus.Arg path. The synthesized struct exists
	// only for binding and schema; the ORIGINAL method is called directly,
	// so an Arg op pays no reflect.MakeFunc trampoline on the hot path.
	paramArgField
	// paramArgBody is a trailing body struct rebuilt from the bound args
	// struct's copied fields — nexus.Arg's body mode.
	paramArgBody
)

type paramSlot struct {
	kind   paramKind
	depPos int // index into depTypes when kind == paramDep
}

type handlerShape struct {
	funcType reflect.Type
	funcVal  reflect.Value
	slots    []paramSlot    // one entry per function param, in order
	depTypes []reflect.Type // types of fx-injected deps only
	argsType reflect.Type   // the args struct type (tagged fields). For
	// Params[T] handlers it's the T inside Params.
	// For legacy flat-args handlers it's the last
	// param type directly.
	hasArgs    bool         // true when argsType is set
	hasForm    bool         // true when the handler declares a *Form param
	hasCtx     bool         // true when the handler takes a context.Context
	paramsType reflect.Type // the concrete Params[T] type; nil if unused
	hasParams  bool         // true when argsType came from a Params[T] param
	returnType reflect.Type // nil for handlers returning only error
	hasError   bool
	errorIdx   int      // index of the error return; -1 if none
	resultIdx  int      // index of the result return; -1 if none
	argBody    *argBody // nexus.Arg body mode; nil otherwise
}

// inspectHandler reflects on fn and builds a handlerShape. Returns an error
// describing bad shapes (not a function, wrong return arity, etc.) — these
// surface at fx.Start, so include the handler's Go type for easy debugging.
func inspectHandler(fn any) (handlerShape, error) {
	var sh handlerShape
	if fn == nil {
		return sh, fmt.Errorf("nexus: handler is nil")
	}
	sh.funcVal = reflect.ValueOf(fn)
	sh.funcType = sh.funcVal.Type()
	if sh.funcType.Kind() != reflect.Func {
		return sh, fmt.Errorf("nexus: handler must be a func, got %s", sh.funcType)
	}

	// Walk params. Classify each:
	//
	//   context.Context  → resolve context, framework-filled
	//   Params[T]        → full bundle (ctx + args + source + info)
	//   trailing struct  → legacy flat-args (only if no Params[T] seen)
	//   otherwise        → fx-injected dep
	//
	// Params[T] can appear anywhere in the list, but only once; a second
	// Params[T] or a Params[T] combined with a trailing args struct is a
	// configuration error.
	numIn := sh.funcType.NumIn()
	sh.slots = make([]paramSlot, numIn)

	// First pass: locate Params[T] if present.
	paramsIdx := -1
	for i := 0; i < numIn; i++ {
		in := sh.funcType.In(i)
		if in.Implements(paramsMarkerType) {
			if paramsIdx >= 0 {
				return sh, fmt.Errorf("nexus: handler %s declares more than one Params[T] — use exactly one", sh.funcType)
			}
			paramsIdx = i
		}
	}

	// Legacy flat-args detection only fires when no Params[T] is present.
	argsEnd := numIn
	if paramsIdx < 0 && numIn > 0 && sh.funcType.In(numIn-1).Kind() == reflect.Struct {
		sh.hasArgs = true
		sh.argsType = sh.funcType.In(numIn - 1)
		argsEnd = numIn - 1
	}

	for i := 0; i < numIn; i++ {
		switch {
		case i == paramsIdx:
			sh.paramsType = sh.funcType.In(i)
			sh.hasParams = true
			sh.argsType = paramsArgsField(sh.paramsType)
			sh.hasArgs = sh.argsType != nil && sh.argsType.Kind() == reflect.Struct && sh.argsType.NumField() > 0
			sh.slots[i] = paramSlot{kind: paramParams}
		case i == argsEnd && sh.hasArgs && paramsIdx < 0:
			sh.slots[i] = paramSlot{kind: paramArgs}
		case sh.funcType.In(i) == contextType:
			sh.slots[i] = paramSlot{kind: paramCtx}
			sh.hasCtx = true
		case sh.funcType.In(i) == ginContextType:
			// *httpx.Ctx — REST-only. GraphQL resolvers don't have
			// a Gin context available; callHandler leaves the slot nil
			// in that case which would panic on first use, which is the
			// right signal to the author that the handler is REST-only.
			sh.slots[i] = paramSlot{kind: paramGinCtx}
		case sh.funcType.In(i) == wsSessionType:
			// *WSSession — AsWS only. REST/GraphQL paths pass a typed
			// nil so the handler can guard with `if s == nil`; all
			// WSSession methods already nil-check the receiver.
			sh.slots[i] = paramSlot{kind: paramWS}
		case sh.funcType.In(i) == formType:
			// *Form — REST only (Inertia pages included). GraphQL/WS
			// pass a typed nil; every Form method nil-checks the
			// receiver, so misuse degrades to empty reads, not panics.
			sh.slots[i] = paramSlot{kind: paramForm}
			sh.hasForm = true
		default:
			sh.slots[i] = paramSlot{kind: paramDep, depPos: len(sh.depTypes)}
			sh.depTypes = append(sh.depTypes, sh.funcType.In(i))
		}
	}

	// Returns: accept (T, error), (T), (error), or ().
	numOut := sh.funcType.NumOut()
	sh.resultIdx = -1
	sh.errorIdx = -1
	errType := reflect.TypeOf((*error)(nil)).Elem()
	switch numOut {
	case 0:
		// nothing
	case 1:
		if sh.funcType.Out(0).Implements(errType) {
			sh.hasError = true
			sh.errorIdx = 0
		} else {
			sh.returnType = sh.funcType.Out(0)
			sh.resultIdx = 0
		}
	case 2:
		if !sh.funcType.Out(1).Implements(errType) {
			return sh, fmt.Errorf("nexus: handler %s: second return must be error, got %s",
				sh.funcType, sh.funcType.Out(1))
		}
		sh.returnType = sh.funcType.Out(0)
		sh.resultIdx = 0
		sh.hasError = true
		sh.errorIdx = 1
	default:
		return sh, fmt.Errorf("nexus: handler %s: expected 0..2 returns, got %d",
			sh.funcType, numOut)
	}

	return sh, nil
}

// returnElementType strips a single layer of pointer/slice wrapping from the
// handler's first return type so registry / introspection keys on the
// concrete element. []*Pet → Pet, *PetsResponse → PetsResponse.
func (sh handlerShape) returnElementType() reflect.Type {
	t := sh.returnType
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// callHandler invokes the wrapped function. Callers supply the resolve
// context (nil OK for non-GraphQL paths) plus any source/info the current
// transport has; GraphQL fills both from graphql.ResolveParams, REST
// leaves them zero. If the handler takes a Params[T], this method builds
// the concrete Params value reflectively and plugs it into the right slot.
func (sh handlerShape) callHandler(ci callInput, deps []reflect.Value, args reflect.Value) (any, error) {
	in := make([]reflect.Value, len(sh.slots))
	// Build the Form first (when declared) and stash it on the resolve
	// context, so the handler's ctx — and everything the handler passes it
	// to — can reach the same Form via FormFrom. Gated on the precomputed
	// flag so ops without a *Form param pay nothing.
	var formVal *Form
	if sh.hasForm && ci.GinCtx != nil {
		formVal = newForm(ci.GinCtx)
		base := ci.Ctx
		if base == nil {
			base = context.Background()
		}
		ci.Ctx = context.WithValue(base, formCtxKey{}, formVal)
	}
	var paramsVal reflect.Value
	if sh.hasParams {
		method := ""
		if ci.GinCtx != nil {
			method = ci.GinCtx.Request.Method
		}
		paramsVal = buildParamsValue(sh.paramsType, ci.Ctx, args, ci.Source, ci.Info, method)
	}
	for i, slot := range sh.slots {
		switch slot.kind {
		case paramDep:
			in[i] = deps[slot.depPos]
		case paramCtx:
			ctx := ci.Ctx
			if ctx == nil {
				ctx = context.Background()
			}
			in[i] = reflect.ValueOf(ctx)
		case paramArgs:
			in[i] = args
		case paramParams:
			in[i] = paramsVal
		case paramGinCtx:
			if ci.GinCtx == nil {
				// REST wasn't the caller — hand a typed nil so the
				// handler can guard with `if c == nil { ... }` rather
				// than panic on an unexpected invalid reflect.Value.
				in[i] = reflect.Zero(ginContextType)
			} else {
				in[i] = reflect.ValueOf(ci.GinCtx)
			}
		case paramWS:
			if ci.WS == nil {
				in[i] = reflect.Zero(wsSessionType)
			} else {
				in[i] = reflect.ValueOf(ci.WS)
			}
		case paramForm:
			if formVal == nil {
				in[i] = reflect.Zero(formType)
			} else {
				in[i] = reflect.ValueOf(formVal)
			}
		case paramArgField:
			in[i] = args.Field(slot.depPos)
		case paramArgBody:
			in[i] = sh.argBody.build(args)
		}
	}
	out := sh.funcVal.Call(in)

	var err error
	if sh.hasError && !out[sh.errorIdx].IsNil() {
		err = out[sh.errorIdx].Interface().(error)
	}
	if sh.resultIdx < 0 {
		return nil, err
	}
	res := out[sh.resultIdx]
	if (res.Kind() == reflect.Pointer || res.Kind() == reflect.Interface) && res.IsNil() {
		return nil, err
	}
	return res.Interface(), err
}

// opNameFromFunc turns a constructor-style function name into a GraphQL op
// name. "NewListOrders" → "listOrders", "listPets" → "listPets",
// "HandleFoo" → "handleFoo". Anonymous / closure names ("func1") fall back
// to the provided default.
func opNameFromFunc(fn any, fallback string) string {
	rv := reflect.ValueOf(fn)
	name := runtimeFuncName(rv)
	if name == "" {
		return fallback
	}
	// Lowercase the first rune.
	for i, r := range name {
		if i == 0 {
			return string(unicode.ToLower(r)) + name[len(string(r)):]
		}
	}
	return fallback
}

// runtimeFuncName returns the bare function name (without package path or
// method receiver decoration). Returns "" for closures / unnamed funcs.
func runtimeFuncName(v reflect.Value) string {
	if v.Kind() != reflect.Func {
		return ""
	}
	pc := v.Pointer()
	if pc == 0 {
		return ""
	}
	f := runtimeFuncForPC(pc)
	if f == "" {
		return ""
	}
	// e.g. "github.com/paulmanoni/nexus/v2/examples/graphapp.NewListOrders",
	// or for methods "pkg.(*UserService).CreateUser" — the last dot-segment
	// is the bare name either way.
	if idx := strings.LastIndex(f, "."); idx >= 0 {
		f = f[idx+1:]
	}
	// A BOUND method value (svc.CreateUser, as opposed to the method
	// expression (*Svc).CreateUser) is reported by the runtime as
	// "CreateUser-fm" — strip the wrapper suffix so both spellings of the
	// same method yield the same op name.
	f = strings.TrimSuffix(f, "-fm")
	// Closures have names like "NewListOrders.func1" — not useful.
	if strings.HasPrefix(f, "func") {
		return ""
	}
	return f
}

// Params is the bundle a reflective resolver receives when it wants more
// than just typed args — namely the resolve context, parent source, or
// schema info. Use it as the last parameter of an AsQuery / AsMutation
// handler (or AsRest, where only Context is filled).
//
//	func NewCreateOrder(
//	    svc *OrdersService,
//	    dbs *DBManager,
//	    cache *CacheManager,
//	    p nexus.Params[CreateOrderArgs],
//	) (*OrderResponse, error) {
//	    order := Order{Title: p.Args.Title, Total: p.Args.Total}
//	    return create(p.Context, order)
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
	// Info describes the GraphQL field being resolved (field name, parent
	// type, operation). Zero on other transports.
	Info gql.Info
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
// Context/Args/Source/Info (converted to gql.Info). Used by as_graph and as_rest before calling a
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
		p.Field(idx.info).Set(reflect.ValueOf(graph.InfoOf(info)))
	}
	return p
}

// HandlerShape is the export-facing wrapper around the framework's
// internal handlerShape. Extensions (extension/peer, future RPC
// transports, custom auth bundles) that want to mount user
// handlers using the canonical reflective signature go through
// this type.
//
// The public surface is intentionally narrow: depTypes for fx wiring,
// argsType for body decoding, hasArgs/hasCtx for slot decisions, and
// Invoke for the actual call. Everything else stays unexported so
// the internal shape can evolve without breaking out-of-tree code.
type HandlerShape struct {
	inner handlerShape
}

// InspectHandlerForExt is the public version of inspectHandler.
// Extensions call this once at registration time, then use the
// returned HandlerShape to build an di.Invoke that resolves deps
// and stamps a bound closure into the extension's dispatch table.
//
// Shape constraints match every other reflective registration in
// nexus — see the package doc on AsRest for the full grammar.
//
// Errors here are returned to the caller as a Go error rather than
// wrapped in an di.Error option, because the caller usually wants
// to attach its own context ("peer.AsCall(%q): %w") before letting
// fx see them.
func InspectHandlerForExt(fn any) (HandlerShape, error) {
	sh, err := inspectHandler(fn)
	if err != nil {
		return HandlerShape{}, err
	}
	return HandlerShape{inner: sh}, nil
}

// DepTypes returns the reflective types of every fx-injected dep
// the handler expects, in registration order. Extension code uses
// this to build an di.FuncOf with the same signature so fx resolves
// the deps at boot.
func (h HandlerShape) DepTypes() []reflect.Type { return h.inner.depTypes }

// ArgsType returns the struct type the handler decodes its body
// into — the T in Params[T] (or the trailing flat-args struct for
// legacy handlers). nil when the handler takes no args.
func (h HandlerShape) ArgsType() reflect.Type { return h.inner.argsType }

// HasArgs reports whether the handler expects an args struct.
// Extensions use this to decide whether to allocate + decode a
// body before invoking.
func (h HandlerShape) HasArgs() bool { return h.inner.hasArgs }

// ReturnType returns the handler's first return type (the result).
// nil when the handler returns only an error (or nothing).
// Extensions use this for response shape inspection — schema
// emission, dashboard endpoint metadata.
func (h HandlerShape) ReturnType() reflect.Type { return h.inner.returnType }

// BoundHandler is the closure form an extension dispatcher invokes
// per request. ctx is the per-call context (with deadlines, trace
// IDs, etc.), args is a value assignable to ArgsType (typically a
// freshly-decoded struct value).
//
// The returned any is the handler's first return value; the error
// is its second. Nil-pointer-or-interface results collapse to a
// nil any, matching the existing graphql / REST conventions so
// downstream marshallers don't have to special-case them.
type BoundHandler func(ctx context.Context, args any) (any, error)

// BuildInvokeOption produces a nexus.Option whose underlying
// di.Invoke has the signature `(*App, dep1, dep2, ...) → ()`. When
// fx fires the invoke at app start, it resolves every dep type
// returned by DepTypes, captures them in a BoundHandler closure,
// and hands the closure to mount.
//
// Extensions use this as the single source of fx wiring — they
// don't need to assemble reflect.FuncOf signatures themselves.
// Each extension's AsX option boils down to:
//
//	sh, err := nexus.InspectHandlerForExt(fn)
//	if err != nil { return ... }
//	return sh.BuildInvokeOption(func(app *App, bound BoundHandler) error {
//	    extDispatchTable.Store(name, bound)
//	    return nil
//	})
//
// The mount closure receives the App so it can read app-level state
// (engine, registry, plugin store) before stashing the bound
// handler.
func (h HandlerShape) BuildInvokeOption(mount func(app *App, bound BoundHandler) error) Option {
	depTypes := h.inner.depTypes
	appType := reflect.TypeOf((*App)(nil))

	in := make([]reflect.Type, 0, len(depTypes)+1)
	in = append(in, appType)
	in = append(in, depTypes...)
	// Returning an error from the invoke lets fx fail boot cleanly
	// when the mount step rejects the registration (duplicate
	// method name, schema collision, etc.).
	errType := reflect.TypeOf((*error)(nil)).Elem()
	fnType := reflect.FuncOf(in, []reflect.Type{errType}, false)

	invokeFn := reflect.MakeFunc(fnType, func(args []reflect.Value) []reflect.Value {
		app := args[0].Interface().(*App)
		deps := args[1:]
		bound := h.bind(deps)
		err := mount(app, bound)
		out := reflect.New(errType).Elem()
		if err != nil {
			out.Set(reflect.ValueOf(err))
		}
		return []reflect.Value{out}
	})
	return rawOption{o: di.Invoke(invokeFn.Interface())}
}

// bind captures the resolved deps + the handler's reflective shape
// into a closure that the extension's dispatcher calls per request.
// Internal — extensions reach this via BuildInvokeOption, not
// directly, so the deps slice never leaves nexus's control.
func (h HandlerShape) bind(deps []reflect.Value) BoundHandler {
	sh := h.inner
	return func(ctx context.Context, args any) (any, error) {
		// Build the args reflect.Value. For no-args handlers we
		// pass a zero Value through; callHandler's paramArgs /
		// paramParams branches don't touch it in that case.
		var argsVal reflect.Value
		if sh.hasArgs && sh.argsType != nil {
			if args == nil {
				argsVal = reflect.Zero(sh.argsType)
			} else {
				v := reflect.ValueOf(args)
				// Allow either a value of argsType or a *argsType
				// (extension dispatchers naturally produce the
				// pointer form via reflect.New).
				if v.Kind() == reflect.Pointer && v.Type().Elem() == sh.argsType {
					v = v.Elem()
				}
				if v.Type() != sh.argsType {
					return nil, fmt.Errorf("handler bind: args type mismatch — want %s, got %s",
						sh.argsType, v.Type())
				}
				argsVal = v
			}
		}
		return sh.callHandler(callInput{Ctx: ctx}, deps, argsVal)
	}
}

// runtimeFuncForPC is split out so the rest of reflect.go is easily testable
// without pulling in runtime internals. Returns the function's fully-qualified
// name, or "" if the PC doesn't resolve (happens for closures built via
// reflect.MakeFunc that we synthesize internally).
func runtimeFuncForPC(pc uintptr) string {
	f := runtime.FuncForPC(pc)
	if f == nil {
		return ""
	}
	return f.Name()
}
