package nexus

import (
	"fmt"
	"reflect"

	"github.com/paulmanoni/nexus/di"
)

// Envelope declares a response envelope for one endpoint: a function the
// framework pipes the handler's (result, error) through before the wire
// write, so handlers (and service methods registered directly) return plain
// (T, error) while the API keeps a custom success/failure shape.
//
// wrap is `func(T, error) (W, error)` — T assignable from the handler's
// result type, W the wire type — checked by the COMPILER, and invoked
// directly per request (a typed closure, not reflection; the generic
// signature is what keeps the envelope off the reflect.Call path). The app
// owns the shape; instantiate a generic helper per registration:
//
//	func Wrap[T any](v T, err error) (*Response[T], error) {
//	    if err != nil {
//	        return &Response[T]{Status: false, Message: err.Error()}, nil
//	    }
//	    return &Response[T]{Status: true, Data: v}, nil
//	}
//
//	nexus.AsQuery((*UserService).ListUsers, nexus.Envelope(Wrap[[]UserRow]))
//
// On GraphQL the schema (and the generated SDK) declare W, not T — the
// envelope is part of the contract, not a serialization trick. An error the
// wrap converts into a value (the usual case) reaches the client as a normal
// 200/data response; an error the wrap returns follows the transport's
// standard error path. Binding and validation failures happen before the
// handler runs and are NOT enveloped. REST + GraphQL only.
func Envelope[T, W any](wrap func(T, error) (W, error)) EnvelopeOption {
	spec := &envelopeSpec{
		inType:  reflect.TypeFor[T](),
		outType: reflect.TypeFor[W](),
		call: func(v any, err error) (any, error) {
			var t T
			if v != nil {
				tv, ok := v.(T)
				if !ok {
					return nil, fmt.Errorf("nexus: Envelope wrap takes %T but the handler returned %T", t, v)
				}
				t = tv
			}
			return wrap(t, err)
		},
	}
	return EnvelopeOption{spec: spec}
}

// EnvelopeOption is returned by nexus.Envelope. It applies to both AsRest
// and AsQuery/AsMutation registrations.
type EnvelopeOption struct{ spec *envelopeSpec }

func (o EnvelopeOption) nexusOption() di.Option    { return di.Options() }
func (o EnvelopeOption) applyToRest(c *restConfig) { c.envelope = o.spec }
func (o EnvelopeOption) applyToGql(c *gqlConfig)   { c.envelope = o.spec }

// envelopeSpec carries a wrap function's types (for schema/registration
// checks) and its direct-call closure (for the request path).
type envelopeSpec struct {
	inType  reflect.Type // T — what the handler returns
	outType reflect.Type // W — what goes on the wire / in the schema
	call    func(v any, err error) (any, error)
}

// check validates the spec against the handler's result type at
// registration, so a mismatched envelope fails at boot with both types
// named rather than panicking per-request.
func (e *envelopeSpec) check(handlerReturn reflect.Type) error {
	if handlerReturn == nil {
		return fmt.Errorf("nexus: Envelope needs a handler returning (T, error); this handler returns no result")
	}
	if !handlerReturn.AssignableTo(e.inType) {
		return fmt.Errorf("nexus: Envelope wrap takes %s but the handler returns %s", e.inType, handlerReturn)
	}
	return nil
}

// outElem strips pointer wrapping from W for schema/type registration,
// mirroring handlerShape.returnElementType.
func (e *envelopeSpec) outElem() reflect.Type {
	t := e.outType
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// entryReturnType is what registrations advertise as the op's return type:
// the envelope's wire type when one is attached, else the handler's own.
func entryReturnType(sh handlerShape, env *envelopeSpec) reflect.Type {
	if env != nil {
		return env.outType
	}
	return sh.returnType
}

// apply pipes a handler's (result, err) through the wrap closure — a
// direct typed call; the only reflection is the nil-pointer normalization
// on the way out (a typed-nil W must become an untyped nil result, the
// same contract callHandler applies to plain handlers).
func (e *envelopeSpec) apply(result any, err error) (any, error) {
	w, werr := e.call(result, err)
	if w != nil {
		if rv := reflect.ValueOf(w); (rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface) && rv.IsNil() {
			return nil, werr
		}
	}
	return w, werr
}
