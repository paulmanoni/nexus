package nexus

import (
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/paulmanoni/nexus/v2/internal/graph"
)

// Validate checks v's `validate:` tags — the rules GraphQL arguments use
// (required, len=min|max, int=min|max, oneof=a|b|c) — and returns an
// InvalidInput *Error with every failing field's message, or nil. Nested
// structs are checked too, their fields keyed "parent.child".
//
// Every transport runs it on a handler's bound args before the handler is
// called; call it directly for a value bound by hand (Form.Bind, a decoded
// payload).
func Validate(v any) error {
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil
	}
	errs := Invalid()
	validateStruct(rv, "", errs)
	if errs.Any() {
		return errs
	}
	return nil
}

// validateValue is Validate for an already-reflected args value; it skips
// types that declare no rules without allocating.
func validateValue(rv reflect.Value) error {
	if !rv.IsValid() {
		return nil
	}
	t := rv.Type()
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || len(validationPlan(t)) == 0 {
		return nil
	}
	return Validate(rv.Interface())
}

type fieldRules struct {
	index      []int
	name       string
	validators []graph.Validator
	nested     bool // a struct (or *struct) whose own fields carry rules
	list       bool // a slice of such structs: each element is checked, under name[i].
}

var validationPlans sync.Map // reflect.Type → []fieldRules

// validationPlan lists the fields of struct type t that carry rules, cached
// per type (args types are a finite, registered set).
func validationPlan(t reflect.Type) []fieldRules {
	if v, ok := validationPlans.Load(t); ok {
		return v.([]fieldRules)
	}
	// Store an empty plan first so a self-referencing type terminates.
	validationPlans.Store(t, []fieldRules(nil))
	var plan []fieldRules
	for _, f := range reflect.VisibleFields(t) {
		if f.PkgPath != "" || f.Anonymous {
			continue
		}
		fr := fieldRules{index: f.Index, name: validationName(f), validators: parseValidateTag(f)}
		ft := f.Type
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct && len(validationPlan(ft)) > 0 {
			fr.nested = true
		}
		if et := ft; et.Kind() == reflect.Slice {
			if et = et.Elem(); et.Kind() == reflect.Pointer {
				et = et.Elem()
			}
			if et.Kind() == reflect.Struct && len(validationPlan(et)) > 0 {
				fr.list = true
			}
		}
		if len(fr.validators) > 0 || fr.nested || fr.list {
			plan = append(plan, fr)
		}
	}
	validationPlans.Store(t, plan)
	return plan
}

// validationName is the wire name a field's messages are keyed by: the
// json name, else form / query / path / graphql, else the field name with
// its first rune lowered.
func validationName(f reflect.StructField) string {
	for _, key := range []string{"json", "form", "query", "path", "uri", "graphql"} {
		tag := f.Tag.Get(key)
		if i := strings.IndexByte(tag, ','); i >= 0 {
			tag = tag[:i]
		}
		if tag = strings.TrimSpace(tag); tag != "" && tag != "-" {
			return tag
		}
	}
	return lowerFirst(f.Name)
}

func validateStruct(rv reflect.Value, prefix string, errs *Error) {
	for _, fr := range validationPlan(rv.Type()) {
		fv, err := rv.FieldByIndexErr(fr.index)
		if err != nil { // promoted through a nil embedded pointer
			continue
		}
		name := prefix + fr.name
		val, present := validationInput(fv)
		for _, v := range fr.validators {
			if !present {
				if v.Info.Kind == "required" {
					errs.Field(name, v.Info.Message)
					break
				}
				continue
			}
			if err := v.Fn(val); err != nil {
				errs.Field(name, err.Error())
				break
			}
		}
		if fr.nested {
			for fv.Kind() == reflect.Pointer {
				if fv.IsNil() {
					break
				}
				fv = fv.Elem()
			}
			if fv.Kind() == reflect.Struct {
				validateStruct(fv, name+".", errs)
			}
		}
		if fr.list && fv.Kind() == reflect.Slice {
			for i := range fv.Len() {
				ev := fv.Index(i)
				for ev.Kind() == reflect.Pointer && !ev.IsNil() {
					ev = ev.Elem()
				}
				if ev.Kind() == reflect.Struct {
					validateStruct(ev, name+"["+strconv.Itoa(i)+"].", errs)
				}
			}
		}
	}
}

// validationInput is the field's value in the shape the rule functions
// take (string, int, float64, bool), and whether it is present: a nil
// pointer, map, slice or interface and an empty string count as absent, so
// only `required` applies to them.
func validationInput(v reflect.Value) (any, bool) {
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return nil, false
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.String:
		return v.String(), v.Len() > 0
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return int(v.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return int(v.Uint()), true
	case reflect.Float32, reflect.Float64:
		return v.Float(), true
	case reflect.Bool:
		return v.Bool(), true
	case reflect.Slice, reflect.Map:
		if v.IsNil() {
			return nil, false
		}
	}
	return v.Interface(), true
}
