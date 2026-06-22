package di

import (
	"fmt"
	"reflect"
)

// container is the resolution engine. It holds the registered providers,
// resolved singleton values, value groups, and the lifecycle. Resolution is
// lazy and memoized: a provider runs at most once, the first time one of its
// outputs is demanded.
type container struct {
	byType  map[reflect.Type]*provider // typed output -> provider that yields it
	byGroup map[string][]*provider     // group name  -> providers contributing to it

	values      map[reflect.Type]reflect.Value // resolved singletons (incl. Supply'd)
	groupValues map[string][]reflect.Value     // collected group members (post-execution)

	lc *lifecycle
}

// provider wraps a single constructor with the metadata needed to run it once
// and route its outputs to the right place (typed value, group, or expanded
// Out fields).
type provider struct {
	fn         reflect.Value
	ft         reflect.Type
	resultTags []string // per result index: raw tag, `group:"name"` routes to a group
	paramTags  []string // per param index: raw tag, `group:"..."` / `optional:"true"`

	executing bool
	executed  bool
}

func newContainer() *container {
	c := &container{
		byType:      map[reflect.Type]*provider{},
		byGroup:     map[string][]*provider{},
		values:      map[reflect.Type]reflect.Value{},
		groupValues: map[string][]reflect.Value{},
		lc:          &lifecycle{},
	}
	// The Lifecycle is always resolvable, so any constructor/invoke can take it
	// as a parameter without an explicit Provide — exactly like fx.Lifecycle.
	c.values[lifecycleType] = reflect.ValueOf(Lifecycle(c.lc))
	return c
}

// supply registers a pre-built value keyed by its dynamic type.
func (c *container) supply(val any) error {
	if val == nil {
		return nil
	}
	rv := reflect.ValueOf(val)
	t := rv.Type()
	if _, dup := c.values[t]; dup {
		return fmt.Errorf("di: %s already supplied/provided", t)
	}
	c.values[t] = rv
	return nil
}

// register indexes a provider's outputs so resolve()/resolveGroup() can find
// it. It does not run the constructor — that happens lazily on first demand.
func (c *container) register(spec ProvideSpec) error {
	fn := reflect.ValueOf(spec.Ctor)
	ft := fn.Type()
	if ft.Kind() != reflect.Func {
		return fmt.Errorf("di: Provide expects a function, got %T", spec.Ctor)
	}
	p := &provider{fn: fn, ft: ft, resultTags: spec.ResultTags, paramTags: spec.ParamTags}

	for i := 0; i < ft.NumOut(); i++ {
		ot := ft.Out(i)
		if ot == errorType {
			continue // error result is the abort signal, never a resolvable value
		}
		// Explicit group annotation on this result wins.
		if g := tagGroup(spec.ResultTags, i); g != "" {
			c.byGroup[g] = append(c.byGroup[g], p)
			continue
		}
		// Result object: expand each exported field into a typed output or a
		// group member, per its struct tag.
		if embedsOut(ot) {
			for f := 0; f < ot.NumField(); f++ {
				field := ot.Field(f)
				if field.Anonymous && field.Type == outMarkerType {
					continue
				}
				if g := field.Tag.Get("group"); g != "" {
					c.byGroup[g] = append(c.byGroup[g], p)
					continue
				}
				if err := c.claimType(field.Type, p); err != nil {
					return err
				}
			}
			continue
		}
		if err := c.claimType(ot, p); err != nil {
			return err
		}
	}
	return nil
}

// claimType records that p produces type t, rejecting duplicate providers for
// the same type (fx errors here too — ambiguity is a wiring bug, not a
// last-wins convenience).
func (c *container) claimType(t reflect.Type, p *provider) error {
	if _, dup := c.byType[t]; dup {
		return fmt.Errorf("di: type %s provided more than once", t)
	}
	if _, dup := c.values[t]; dup {
		return fmt.Errorf("di: type %s already supplied", t)
	}
	c.byType[t] = p
	return nil
}

// resolve returns the value for type t, running providers as needed.
func (c *container) resolve(t reflect.Type) (reflect.Value, error) {
	if v, ok := c.values[t]; ok {
		return v, nil
	}
	if embedsIn(t) {
		return c.resolveIn(t)
	}
	p, ok := c.byType[t]
	if !ok {
		return reflect.Value{}, fmt.Errorf("di: no provider for %s", t)
	}
	if err := c.execute(p); err != nil {
		return reflect.Value{}, err
	}
	v, ok := c.values[t]
	if !ok {
		return reflect.Value{}, fmt.Errorf("di: provider for %s ran but produced no value", t)
	}
	return v, nil
}

// resolveIn builds a parameter object: each exported field is resolved
// individually, honoring `optional:"true"` (zero value if unprovided) and
// `group:"name"` (collect a slice from the value group).
func (c *container) resolveIn(t reflect.Type) (reflect.Value, error) {
	out := reflect.New(t).Elem()
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if field.Anonymous && field.Type == inMarkerType {
			continue
		}
		if !out.Field(i).CanSet() {
			continue // unexported field; ignore
		}
		if g := field.Tag.Get("group"); g != "" {
			if field.Type.Kind() != reflect.Slice {
				return reflect.Value{}, fmt.Errorf("di: group field %s.%s must be a slice", t, field.Name)
			}
			slice, err := c.resolveGroup(g, field.Type)
			if err != nil {
				return reflect.Value{}, err
			}
			out.Field(i).Set(slice)
			continue
		}
		v, err := c.resolve(field.Type)
		if err != nil {
			if field.Tag.Get("optional") == "true" {
				continue // leave zero value
			}
			return reflect.Value{}, err
		}
		out.Field(i).Set(v)
	}
	return out, nil
}

// resolveGroup runs every provider contributing to the named group and returns
// their collected outputs as a slice of sliceType.
func (c *container) resolveGroup(name string, sliceType reflect.Type) (reflect.Value, error) {
	for _, p := range c.byGroup[name] {
		if err := c.execute(p); err != nil {
			return reflect.Value{}, err
		}
	}
	members := c.groupValues[name]
	out := reflect.MakeSlice(sliceType, 0, len(members))
	elem := sliceType.Elem()
	for _, m := range members {
		if !m.Type().AssignableTo(elem) {
			return reflect.Value{}, fmt.Errorf("di: group %q member %s not assignable to %s", name, m.Type(), elem)
		}
		out = reflect.Append(out, m)
	}
	return out, nil
}

// execute runs a provider's constructor once, resolving its parameters and
// routing its outputs. Re-entry while executing signals a dependency cycle.
func (c *container) execute(p *provider) error {
	if p.executed {
		return nil
	}
	if p.executing {
		return fmt.Errorf("di: dependency cycle through %s", p.ft)
	}
	p.executing = true
	defer func() { p.executing = false }()

	args, callSlice, err := c.resolveParams(p.ft, p.paramTags)
	if err != nil {
		return err
	}
	var outs []reflect.Value
	if callSlice {
		outs = p.fn.CallSlice(args)
	} else {
		outs = p.fn.Call(args)
	}

	for i := 0; i < p.ft.NumOut(); i++ {
		ot := p.ft.Out(i)
		val := outs[i]
		if ot == errorType {
			if !val.IsNil() {
				return val.Interface().(error)
			}
			continue
		}
		if g := tagGroup(p.resultTags, i); g != "" {
			c.groupValues[g] = append(c.groupValues[g], val)
			continue
		}
		if embedsOut(ot) {
			c.spreadOut(ot, val)
			continue
		}
		c.values[ot] = val
	}
	p.executed = true
	return nil
}

// spreadOut distributes the fields of a result object into typed values and
// group members.
func (c *container) spreadOut(t reflect.Type, v reflect.Value) {
	for f := 0; f < t.NumField(); f++ {
		field := t.Field(f)
		if field.Anonymous && field.Type == outMarkerType {
			continue
		}
		if g := field.Tag.Get("group"); g != "" {
			c.groupValues[g] = append(c.groupValues[g], v.Field(f))
			continue
		}
		c.values[field.Type] = v.Field(f)
	}
}

// resolveParams resolves every parameter of a func type. A param tagged into a
// group (via ParamTags) collects a slice; a param tagged `optional:"true"`
// resolves to its zero value when no provider exists; otherwise it resolves by
// type, with In structs handled by resolveIn.
//
// A trailing variadic parameter is, like dig/fx, NOT resolved by type: an
// unannotated `...T` is ignored (the constructor runs with zero variadic args),
// so stdlib-style constructors such as zap.NewExample(...zap.Option) work
// without a provider for []T. A variadic param explicitly group-tagged collects
// the value group and is passed through as the variadic slice (callSlice=true).
func (c *container) resolveParams(ft reflect.Type, paramTags []string) (args []reflect.Value, callSlice bool, err error) {
	n := ft.NumIn()
	variadic := ft.IsVariadic()
	args = make([]reflect.Value, 0, n)
	for i := 0; i < n; i++ {
		pt := ft.In(i)
		isVariadicParam := variadic && i == n-1
		if g := tagGroup(paramTags, i); g != "" {
			if pt.Kind() != reflect.Slice {
				return nil, false, fmt.Errorf("di: group param %d of %s must be a slice", i, ft)
			}
			slice, gerr := c.resolveGroup(g, pt)
			if gerr != nil {
				return nil, false, gerr
			}
			args = append(args, slice)
			if isVariadicParam {
				// The collected group IS the variadic slice; hand it to
				// CallSlice rather than letting Call re-wrap it.
				return args, true, nil
			}
			continue
		}
		if isVariadicParam {
			break // ignore the variadic argument (dig/fx parity)
		}
		v, rerr := c.resolve(pt)
		if rerr != nil {
			if tagOptional(paramTags, i) {
				args = append(args, reflect.Zero(pt))
				continue
			}
			return nil, false, rerr
		}
		args = append(args, v)
	}
	return args, false, nil
}

// tagGroup returns the `group` value of the i-th raw tag, or "".
func tagGroup(tags []string, i int) string {
	if i >= len(tags) || tags[i] == "" {
		return ""
	}
	return parseTag(tags[i]).Get("group")
}

// tagOptional reports whether the i-th raw tag carries `optional:"true"`.
func tagOptional(tags []string, i int) bool {
	if i >= len(tags) || tags[i] == "" {
		return false
	}
	return parseTag(tags[i]).Get("optional") == "true"
}

// invoke resolves a function's parameters and calls it, propagating a trailing
// error result.
func (c *container) invoke(spec InvokeSpec) error {
	rv := reflect.ValueOf(spec.Fn)
	ft := rv.Type()
	if ft.Kind() != reflect.Func {
		return fmt.Errorf("di: Invoke expects a function, got %T", spec.Fn)
	}
	args, callSlice, err := c.resolveParams(ft, spec.ParamTags)
	if err != nil {
		return err
	}
	var outs []reflect.Value
	if callSlice {
		outs = rv.CallSlice(args)
	} else {
		outs = rv.Call(args)
	}
	for i := 0; i < ft.NumOut(); i++ {
		if ft.Out(i) == errorType {
			if e := outs[i]; !e.IsNil() {
				return e.Interface().(error)
			}
		}
	}
	return nil
}
