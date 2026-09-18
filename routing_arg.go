package nexus

import (
	"fmt"
	"reflect"
	"strings"
)

// Arg names the wire argument(s) for a handler that takes bare scalars
// instead of an args struct, killing the one-line adapter such methods
// otherwise force. A registration option, next to Op/Requires/Envelope:
//
//	// func (s *UserService) GetUser(id uint) (*UserDetail, error)
//	nexus.AsQuery((*UserService).GetUser, nexus.Arg("id"), nexus.Op("userShow"))
//	nexus.AsRest("GET", "/users/:id", (*UserService).GetUser, nexus.Arg("id"))
//
//	// func (s *UserService) Move(ctx context.Context, id uint, employerID int) (bool, error)
//	nexus.AsMutation((*UserService).Move, nexus.Arg("id", "employerId"))
//
// Names map POSITIONALLY onto the handler's last len(names) parameters (Go
// reflection cannot see parameter names, so positional is the only possible
// mapping — double-check the order when two args share a type). Everything
// before them keeps its normal classification: receiver and other deps are
// DI-injected, context.Context fills from the request.
//
// The single- or multi-field args struct is synthesized at registration —
// each field tagged json/query/uri/graphql under its name — so binding, the
// GraphQL schema, validation surfaces and the generated SDK see exactly what
// a hand-written wrapper struct would have declared. A non-pointer parameter
// becomes a REQUIRED argument; a pointer parameter an optional one. The op
// name still derives from the method itself (GetUser → getUser).
//
// Guidance: one or two scalars ride Arg well; three or more deserve a dto —
// the struct's field names then document the call. A parameter that is
// already a struct is rejected at boot ("register it directly"), and Arg on
// a handler that declares Params[T] or a trailing args struct is a boot
// error too.
func Arg(names ...string) ArgOption {
	return ArgOption{names: names}
}

// ArgOption is returned by nexus.Arg; it applies to AsRest and
// AsQuery/AsMutation registrations. Repeated Arg options append, so
// Arg("id"), Arg("name") is equivalent to Arg("id", "name").
type ArgOption struct{ names []string }

func (o ArgOption) applyToRest(c *restConfig) { c.argNames = append(c.argNames, o.names...) }
func (o ArgOption) applyToGql(c *gqlConfig)   { c.argNames = append(c.argNames, o.names...) }

// inspectHandlerArgs is inspectHandler for a registration carrying nexus.Arg:
// it validates the names against the handler, synthesizes the args struct for
// binding/schema, and repairs the shape so the scalar parameters are fed from
// the bound struct's fields at call time — the ORIGINAL function is invoked
// directly (no reflect.MakeFunc trampoline on the hot path).
func inspectHandlerArgs(fn any, names []string) (handlerShape, error) {
	if len(names) == 0 {
		return inspectHandler(fn)
	}
	argStruct, start, err := buildArgStruct(fn, names)
	if err != nil {
		return handlerShape{}, err
	}
	sh, err := inspectHandler(fn)
	if err != nil {
		return sh, err
	}
	if sh.hasParams {
		return sh, fmt.Errorf("nexus: Arg: the handler already declares Params[T] — drop the Arg option")
	}
	// The named trailing scalars were classified as DI deps by the plain
	// inspection (they are the LAST dep entries, in order); reclassify them
	// as args-struct fields and drop them from the dep list.
	for i := start; i < len(sh.slots); i++ {
		if sh.slots[i].kind != paramDep {
			return sh, fmt.Errorf("nexus: Arg: parameter %d of %s is a framework type and cannot be a named argument",
				i, sh.funcType)
		}
		sh.slots[i] = paramSlot{kind: paramArgField, depPos: i - start}
	}
	sh.depTypes = sh.depTypes[:len(sh.depTypes)-len(names)]
	sh.hasArgs = true
	sh.argsType = argStruct
	return sh, nil
}

// buildArgStruct validates the Arg names against fn's signature and returns
// the synthesized args struct plus the index of the first named parameter.
func buildArgStruct(fn any, names []string) (reflect.Type, int, error) {
	if len(names) == 0 {
		return nil, 0, fmt.Errorf("nexus: Arg needs at least one argument name")
	}
	seen := map[string]bool{}
	for _, n := range names {
		if n == "" {
			return nil, 0, fmt.Errorf("nexus: Arg: empty argument name")
		}
		if seen[n] {
			return nil, 0, fmt.Errorf("nexus: Arg: duplicate argument name %q", n)
		}
		seen[n] = true
		for i, r := range n {
			ok := r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (i > 0 && r >= '0' && r <= '9')
			if !ok {
				return nil, 0, fmt.Errorf("nexus: Arg: %q is not a valid argument name (letters, digits, underscore; no leading digit)", n)
			}
		}
	}
	fv := reflect.ValueOf(fn)
	if !fv.IsValid() || fv.Kind() != reflect.Func {
		return nil, 0, fmt.Errorf("nexus: Arg: handler must be a func, got %T", fn)
	}
	ft := fv.Type()
	if ft.IsVariadic() {
		return nil, 0, fmt.Errorf("nexus: Arg: variadic handlers are not supported (%s)", ft)
	}
	if ft.NumIn() < len(names) {
		return nil, 0, fmt.Errorf("nexus: Arg names %d argument(s) but %s takes only %d parameter(s)",
			len(names), ft, ft.NumIn())
	}

	start := ft.NumIn() - len(names)
	fields := make([]reflect.StructField, len(names))
	for i, name := range names {
		pt := ft.In(start + i)
		switch {
		case pt.Kind() == reflect.Struct && pt != contextType:
			return nil, 0, fmt.Errorf(
				"nexus: Arg(%q): the parameter is already a struct (%s) — register it directly", name, pt)
		case pt.Implements(paramsMarkerType):
			return nil, 0, fmt.Errorf("nexus: Arg: the handler already declares Params[T] — drop the Arg option")
		case pt == contextType:
			return nil, 0, fmt.Errorf(
				"nexus: Arg(%q): would name a context.Context parameter — Arg names map onto the LAST %d parameter(s), in order",
				name, len(names))
		}
		gqlTag := name + ", required"
		if pt.Kind() == reflect.Pointer {
			gqlTag = name // pointer parameter → optional argument
		}
		fields[i] = reflect.StructField{
			Name: "Arg" + strings.ToUpper(name[:1]) + name[1:],
			Type: pt,
			Tag: reflect.StructTag(fmt.Sprintf(`json:%q query:%q uri:%q graphql:%q`,
				name, name, name, gqlTag)),
		}
	}
	return reflect.StructOf(fields), start, nil
}
