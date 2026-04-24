package nexus

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/graphql-go/graphql"
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
	// *gin.Context parameter — lets them reach body / path / query /
	// header parsing helpers (multipart file upload, custom status
	// codes, c.Param("...") etc.) while still participating in the
	// reflective registration shape.
	GinCtx *gin.Context
}

// Special parameter types recognized by the reflective handler shape. These
// are filled in by the framework at call time rather than fx-injected.
var (
	contextType      = reflect.TypeOf((*context.Context)(nil)).Elem()
	ginContextType   = reflect.TypeOf((*gin.Context)(nil))
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
)

type paramSlot struct {
	kind    paramKind
	depPos  int // index into depTypes when kind == paramDep
}

type handlerShape struct {
	funcType   reflect.Type
	funcVal    reflect.Value
	slots      []paramSlot     // one entry per function param, in order
	depTypes   []reflect.Type  // types of fx-injected deps only
	argsType   reflect.Type    // the args struct type (tagged fields). For
	                           // Params[T] handlers it's the T inside Params.
	                           // For legacy flat-args handlers it's the last
	                           // param type directly.
	hasArgs    bool            // true when argsType is set
	hasCtx     bool            // true when the handler takes a context.Context
	paramsType reflect.Type    // the concrete Params[T] type; nil if unused
	hasParams  bool            // true when argsType came from a Params[T] param
	returnType reflect.Type    // nil for handlers returning only error
	hasError   bool
	errorIdx   int // index of the error return; -1 if none
	resultIdx  int // index of the result return; -1 if none
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
			// *gin.Context — REST-only. GraphQL resolvers don't have
			// a Gin context available; callHandler leaves the slot nil
			// in that case which would panic on first use, which is the
			// right signal to the author that the handler is REST-only.
			sh.slots[i] = paramSlot{kind: paramGinCtx}
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
	for t != nil && t.Kind() == reflect.Ptr {
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
	var paramsVal reflect.Value
	if sh.hasParams {
		paramsVal = buildParamsValue(sh.paramsType, ci.Ctx, args, ci.Source, ci.Info)
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
	if (res.Kind() == reflect.Ptr || res.Kind() == reflect.Interface) && res.IsNil() {
		return nil, err
	}
	return res.Interface(), err
}

// opNameFromFunc turns a constructor-style function name into a GraphQL op
// name. "NewGetAllAdverts" → "getAllAdverts", "listPets" → "listPets",
// "HandleFoo" → "handleFoo". Anonymous / closure names ("func1") fall back
// to the provided default.
func opNameFromFunc(fn any, fallback string) string {
	rv := reflect.ValueOf(fn)
	name := runtimeFuncName(rv)
	if name == "" {
		return fallback
	}
	// Trim leading "New" if followed by an uppercase letter.
	if strings.HasPrefix(name, "New") && len(name) > 3 && unicode.IsUpper(rune(name[3])) {
		name = name[3:]
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
	// e.g. "github.com/paulmanoni/nexus/examples/graphapp.NewGetAllAdverts"
	if idx := strings.LastIndex(f, "."); idx >= 0 {
		f = f[idx+1:]
	}
	// Closures have names like "NewGetAllAdverts.func1" — not useful.
	if strings.HasPrefix(f, "func") {
		return ""
	}
	return f
}
