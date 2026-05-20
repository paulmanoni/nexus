package nexus

import (
	"context"
	"fmt"
	"reflect"

	"go.uber.org/fx"
)

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
// returned HandlerShape to build an fx.Invoke that resolves deps
// and stamps a bound closure into the extension's dispatch table.
//
// Shape constraints match every other reflective registration in
// nexus — see the package doc on AsRest for the full grammar.
//
// Errors here are returned to the caller as a Go error rather than
// wrapped in an fx.Error option, because the caller usually wants
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
// this to build an fx.FuncOf with the same signature so fx resolves
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
// fx.Invoke has the signature `(*App, dep1, dep2, ...) → ()`. When
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
	return rawOption{o: fx.Invoke(invokeFn.Interface())}
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
				if v.Kind() == reflect.Ptr && v.Type().Elem() == sh.argsType {
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
