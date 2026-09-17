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
// wrap must be `func(T, error) (W, error)` — T assignable from the handler's
// result type, W the wire type. The app owns the shape; instantiate a
// generic helper per registration:
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
func Envelope(wrap any) EnvelopeOption {
	return EnvelopeOption{wrap: wrap}
}

// EnvelopeOption is returned by nexus.Envelope. It applies to both AsRest
// and AsQuery/AsMutation registrations.
type EnvelopeOption struct{ wrap any }

func (o EnvelopeOption) nexusOption() di.Option     { return di.Options() }
func (o EnvelopeOption) applyToRest(c *restConfig)  { c.envelope, c.envelopeErr = newEnvelopeSpec(o.wrap) }
func (o EnvelopeOption) applyToGql(c *gqlConfig)    { c.envelope, c.envelopeErr = newEnvelopeSpec(o.wrap) }

// envelopeSpec is the reflected view of an Envelope wrap function.
type envelopeSpec struct {
	fn      reflect.Value
	inType  reflect.Type // T — what the handler returns
	outType reflect.Type // W — what goes on the wire / in the schema
}

var envErrType = reflect.TypeOf((*error)(nil)).Elem()

func newEnvelopeSpec(wrap any) (*envelopeSpec, error) {
	if wrap == nil {
		return nil, fmt.Errorf("nexus: Envelope(nil)")
	}
	v := reflect.ValueOf(wrap)
	t := v.Type()
	if t.Kind() != reflect.Func || t.NumIn() != 2 || t.NumOut() != 2 ||
		t.In(1) != envErrType || !t.Out(1).Implements(envErrType) {
		return nil, fmt.Errorf("nexus: Envelope wrap must be func(T, error) (W, error), got %s", t)
	}
	return &envelopeSpec{fn: v, inType: t.In(0), outType: t.Out(0)}, nil
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

// apply pipes a handler's (result, err) through the wrap function.
func (e *envelopeSpec) apply(result any, err error) (any, error) {
	in := [2]reflect.Value{}
	if result == nil {
		in[0] = reflect.Zero(e.inType)
	} else {
		rv := reflect.ValueOf(result)
		if !rv.Type().AssignableTo(e.inType) {
			return nil, fmt.Errorf("nexus: Envelope wrap takes %s but the handler returned %s", e.inType, rv.Type())
		}
		in[0] = rv
	}
	if err == nil {
		in[1] = reflect.Zero(envErrType)
	} else {
		in[1] = reflect.ValueOf(err)
	}
	out := e.fn.Call(in[:])
	var werr error
	if !out[1].IsNil() {
		werr = out[1].Interface().(error)
	}
	res := out[0]
	if (res.Kind() == reflect.Pointer || res.Kind() == reflect.Interface) && res.IsNil() {
		return nil, werr
	}
	return res.Interface(), werr
}
