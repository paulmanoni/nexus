package view

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	"github.com/paulmanoni/nexus/v2"
)

// ContextProcessor registers fn as the source of a value every template can
// read with FromContext — what the layout needs on every page (the signed-in
// user's name, unread counts, the navigation), computed on the server from
// the request, as Django's context processors are:
//
//	type Layout struct {
//		User   string
//		Unread int
//	}
//
//	view.ContextProcessor(func(ctx context.Context, inbox *Inbox) (Layout, error) {
//		u := auth.Current(ctx)
//		return Layout{User: u.ID, Unread: inbox.Unread(ctx, u.ID)}, nil
//	})
//
//	{{ l := view.FromContext[Layout](ctx) }}
//	<span>{ l.User }</span>
//
// fn takes ctx — the request's, or a live page's connection's, with the
// visitor's identity — then any dependencies the app's DI container
// provides, and returns the value, with or without an error. Its result type
// names it: one processor per type. It runs at most once per render, and only
// when a template reads it; an error fails the render.
func ContextProcessor(fn any) nexus.Option {
	ft := reflect.TypeOf(fn)
	ctxType := reflect.TypeFor[context.Context]()
	errType := reflect.TypeFor[error]()
	if ft == nil || ft.Kind() != reflect.Func || ft.NumIn() == 0 || ft.In(0) != ctxType ||
		ft.NumOut() == 0 || ft.NumOut() > 2 || (ft.NumOut() == 2 && ft.Out(1) != errType) {
		return nexus.FailBoot(fmt.Errorf("view.ContextProcessor: %T is not func(ctx context.Context, deps…) (T[, error])", fn))
	}
	deps := make([]reflect.Type, ft.NumIn()-1)
	for i := range deps {
		deps[i] = ft.In(i + 1)
	}
	t := ft.Out(0)
	fv := reflect.ValueOf(fn)
	bind := reflect.MakeFunc(reflect.FuncOf(deps, nil, false), func(args []reflect.Value) []reflect.Value {
		processorsMu.Lock()
		processors[t] = func(ctx context.Context) (any, error) {
			out := fv.Call(append([]reflect.Value{reflect.ValueOf(ctx)}, args...))
			if len(out) == 2 && !out[1].IsNil() {
				return nil, out[1].Interface().(error)
			}
			return out[0].Interface(), nil
		}
		processorsMu.Unlock()
		return nil
	})
	return nexus.Invoke(bind.Interface())
}

// FromContext is the value of T's ContextProcessor for this render. It
// panics when no processor returns T, and when the processor fails.
func FromContext[T any](ctx context.Context) T {
	t := reflect.TypeFor[T]()
	taintFrom(ctx)
	r := renderFrom(ctx)
	if r != nil {
		if p, ok := r.processed[t]; ok {
			if p.err != nil {
				panic(p.err)
			}
			v, _ := p.value.(T)
			return v
		}
	}
	processorsMu.RLock()
	fn := processors[t]
	processorsMu.RUnlock()
	if fn == nil {
		panic(fmt.Sprintf("view.FromContext[%s]: no view.ContextProcessor returns %s", t, t))
	}
	value, err := fn(ctx)
	if err != nil {
		err = fmt.Errorf("view.FromContext[%s]: %w", t, err)
	}
	if r != nil {
		if r.processed == nil {
			r.processed = map[reflect.Type]processed{}
		}
		r.processed[t] = processed{value: value, err: err}
	}
	if err != nil {
		panic(err)
	}
	v, _ := value.(T)
	return v
}

type processed struct {
	value any
	err   error
}

var (
	processorsMu sync.RWMutex
	processors   = map[reflect.Type]func(context.Context) (any, error){}
)
