package di

import (
	"fmt"
	"reflect"
)

// Populate resolves values from the container into the given pointers at
// build time, mirroring fx.Populate. Each target must be a non-nil pointer;
// its pointee type is resolved from the graph and assigned. Implemented as a
// synthesized Invoke so it participates in normal eager resolution ordering.
//
//	var app *App
//	di.New(opts, di.Populate(&app))   // app is filled, lifecycle NOT started
func Populate(targets ...any) Option {
	if len(targets) == 0 {
		return Options()
	}
	ptrs := make([]reflect.Value, len(targets))
	in := make([]reflect.Type, len(targets))
	for i, t := range targets {
		pv := reflect.ValueOf(t)
		if pv.Kind() != reflect.Ptr || pv.IsNil() {
			return Error(fmt.Errorf("di: Populate target %d must be a non-nil pointer, got %T", i, t))
		}
		ptrs[i] = pv
		in[i] = pv.Type().Elem()
	}
	fn := reflect.MakeFunc(reflect.FuncOf(in, nil, false), func(args []reflect.Value) []reflect.Value {
		for i, a := range args {
			ptrs[i].Elem().Set(a)
		}
		return nil
	})
	return Invoke(fn.Interface())
}
