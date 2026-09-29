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
//
// pathParams are the REST route's :name/*name segments (nil for GraphQL): a
// named scalar that is one of them binds from the path only, so a JSON body
// can never override it.
func inspectHandlerArgs(fn any, names []string, pathParams ...string) (handlerShape, error) {
	if len(names) == 0 {
		return inspectHandler(fn)
	}
	syn, err := buildArgStruct(fn, names, pathParams)
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
	// The named scalars were classified as DI deps by the plain inspection
	// (they are the LAST dep entries, in order); reclassify them as
	// args-struct fields and drop them from the dep list. In body mode the
	// trailing struct was classified as the args struct; it is rebuilt from
	// the synthesized struct's copied fields instead.
	end := len(sh.slots)
	if syn.body != nil {
		end--
		sh.slots[end] = paramSlot{kind: paramArgBody}
		sh.argBody = syn.body
	}
	for i := syn.start; i < end; i++ {
		if sh.slots[i].kind != paramDep {
			return sh, fmt.Errorf("nexus: Arg: parameter %d of %s is a framework type and cannot be a named argument",
				i, sh.funcType)
		}
		sh.slots[i] = paramSlot{kind: paramArgField, depPos: i - syn.start}
	}
	sh.depTypes = sh.depTypes[:len(sh.depTypes)-len(names)]
	sh.hasArgs = true
	sh.argsType = syn.typ
	return sh, nil
}

// argStruct is the struct nexus.Arg synthesizes, plus where its parts map back
// onto the handler's parameters.
type argStruct struct {
	typ   reflect.Type
	start int      // index of the first named scalar parameter
	body  *argBody // non-nil in body mode
}

// argBody rebuilds a handler's trailing body struct from the synthesized args
// struct: field fieldStart+i is copied to the body field at paths[i].
type argBody struct {
	typ        reflect.Type
	fieldStart int
	paths      [][]int
}

func (b *argBody) build(args reflect.Value) reflect.Value {
	body := reflect.New(b.typ).Elem()
	for i, p := range b.paths {
		body.FieldByIndex(p).Set(args.Field(b.fieldStart + i))
	}
	return body
}

// buildArgStruct validates the Arg names against fn's signature and returns
// the synthesized args struct. When fn's last parameter is a struct it is the
// request body ("body mode"): the names map onto the scalars just before it,
// and the body's exported fields are copied into the synthesized struct so
// one struct carries both for binding, validation and the schema.
func buildArgStruct(fn any, names []string, pathParams []string) (argStruct, error) {
	var out argStruct
	if len(names) == 0 {
		return out, fmt.Errorf("nexus: Arg needs at least one argument name")
	}
	seen := map[string]bool{}
	for _, n := range names {
		if n == "" {
			return out, fmt.Errorf("nexus: Arg: empty argument name")
		}
		if seen[n] {
			return out, fmt.Errorf("nexus: Arg: duplicate argument name %q", n)
		}
		seen[n] = true
		for i, r := range n {
			ok := r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (i > 0 && r >= '0' && r <= '9')
			if !ok {
				return out, fmt.Errorf("nexus: Arg: %q is not a valid argument name (letters, digits, underscore; no leading digit)", n)
			}
		}
	}
	fv := reflect.ValueOf(fn)
	if !fv.IsValid() || fv.Kind() != reflect.Func {
		return out, fmt.Errorf("nexus: Arg: handler must be a func, got %T", fn)
	}
	ft := fv.Type()
	if ft.IsVariadic() {
		return out, fmt.Errorf("nexus: Arg: variadic handlers are not supported (%s)", ft)
	}
	n := ft.NumIn()
	var body reflect.Type
	if n > 0 {
		if last := ft.In(n - 1); last.Kind() == reflect.Struct && !last.Implements(paramsMarkerType) {
			body = last
			n--
		}
	}
	if n < len(names) {
		return out, fmt.Errorf("nexus: Arg names %d argument(s) but %s takes only %d parameter(s) before its args struct",
			len(names), ft, n)
	}

	isPath := map[string]bool{}
	for _, p := range pathParams {
		isPath[p] = true
	}
	out.start = n - len(names)
	fields := make([]reflect.StructField, 0, len(names))
	wire := map[string]bool{}
	for i, name := range names {
		pt := ft.In(out.start + i)
		switch {
		case pt.Kind() == reflect.Struct:
			return out, fmt.Errorf(
				"nexus: Arg(%q): the parameter is already a struct (%s) — register it directly", name, pt)
		case pt.Implements(paramsMarkerType):
			return out, fmt.Errorf("nexus: Arg: the handler already declares Params[T] — drop the Arg option")
		case pt == contextType && body != nil:
			return out, fmt.Errorf(
				"nexus: Arg(%q): %s takes an args struct (%s) with no scalar parameter before it to name — register it directly",
				name, ft, body)
		case pt == contextType:
			return out, fmt.Errorf(
				"nexus: Arg(%q): would name a context.Context parameter — Arg names map onto the LAST %d parameter(s) before any args struct, in order",
				name, len(names))
		}
		gqlTag := name + ", required"
		if pt.Kind() == reflect.Pointer {
			gqlTag = name // pointer parameter → optional argument
		}
		tag := fmt.Sprintf(`json:%q query:%q uri:%q graphql:%q`, name, name, name, gqlTag)
		if isPath[name] {
			// Path only: the route segment is the one source of truth.
			tag = fmt.Sprintf(`json:"-" path:%q graphql:%q`, name, gqlTag)
		}
		wire[name] = true
		fields = append(fields, reflect.StructField{
			Name: "Arg" + strings.ToUpper(name[:1]) + name[1:],
			Type: pt,
			Tag:  reflect.StructTag(tag),
		})
	}
	if body != nil {
		b := &argBody{typ: body, fieldStart: len(fields)}
		var err error
		fields, err = flattenBody(body, nil, fields, b, wire)
		if err != nil {
			return out, err
		}
		out.body = b
	}
	out.typ = reflect.StructOf(fields)
	return out, nil
}

// flattenBody appends t's exported fields (descending into embedded structs)
// to fields, recording each one's index path in b.
func flattenBody(t reflect.Type, prefix []int, fields []reflect.StructField, b *argBody, wire map[string]bool) ([]reflect.StructField, error) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		path := append(append([]int(nil), prefix...), i)
		if f.Anonymous {
			if f.Type.Kind() == reflect.Pointer {
				return fields, fmt.Errorf("nexus: Arg: body %s embeds %s by pointer — embed it by value", t, f.Type)
			}
			if f.Type.Kind() == reflect.Struct {
				var err error
				if fields, err = flattenBody(f.Type, path, fields, b, wire); err != nil {
					return fields, err
				}
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		name := strings.SplitN(f.Tag.Get("json"), ",", 2)[0]
		if name == "" {
			name = f.Name
		}
		if name != "-" && wire[name] {
			return fields, fmt.Errorf("nexus: Arg(%q) collides with the body field %s.%s", name, t, f.Name)
		}
		wire[name] = true
		for _, have := range fields {
			if have.Name == f.Name {
				return fields, fmt.Errorf("nexus: Arg: body field %s.%s clashes with another field of the same name", t, f.Name)
			}
		}
		fields = append(fields, reflect.StructField{Name: f.Name, Type: f.Type, Tag: f.Tag})
		b.paths = append(b.paths, path)
	}
	return fields, nil
}

// routePathParams lists a route template's :name and *name segments.
func routePathParams(path string) []string {
	var out []string
	for _, seg := range strings.Split(path, "/") {
		if len(seg) > 1 && (seg[0] == ':' || seg[0] == '*') {
			out = append(out, seg[1:])
		}
	}
	return out
}
