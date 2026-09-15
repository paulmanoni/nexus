// Package bindutil holds the mechanics every typed resource binder
// (db.Bind, cache.Bind, mail.Bind, storage.Bind) shares: discovering
// the embedded manager field on the marker type, constructing the
// holder, and the dashboard-registration option set. Each binder keeps
// only what genuinely differs — how its manager is built and how its
// resource registers.
//
// Before this package the four binders carried verbatim copies, and
// they had already diverged: db evaluated its options lazily (so
// BindFromConfig works under nexus.Boot, which parses nexus.toml after
// option construction) while the other three applied them eagerly.
// Sharing the plumbing makes the lazy semantics uniform by
// construction.
package bindutil

import (
	"fmt"
	"reflect"
)

// Options is the shared dashboard-registration knob set. Each binder
// package re-exports the With* constructors it supports so godoc and
// call sites stay package-local (cache.WithDefault, db.WithDetails, …).
type Options struct {
	Description string
	Details     map[string]any
	AsDefault   bool
}

// Option mutates an Options. The binder packages alias their public
// BindOption to this type, so options remain interchangeable with the
// pre-consolidation API (the config struct was unexported everywhere,
// so no caller could have depended on its shape).
type Option func(*Options)

// Apply folds opts into a value. Call it inside the register invoke —
// never at option-construction time — so options derived from config
// parsed at boot (BindFromConfig under nexus.Boot) resolve lazily.
func Apply(opts []Option) Options {
	var v Options
	for _, o := range opts {
		if o != nil {
			o(&v)
		}
	}
	return v
}

// EmbeddedField returns the index of T's embedded field of exact type
// want (a *Manager pointer type). A T that doesn't match panics here,
// at wiring time, with pkg-qualified guidance — never at request time.
func EmbeddedField[T any](pkg string, want reflect.Type, example string) int {
	t := reflect.TypeFor[T]()
	if t == nil || t.Kind() != reflect.Struct {
		panic(fmt.Sprintf("%s: T must be a struct embedding %s", pkg, want))
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous && f.Type == want {
			return i
		}
	}
	panic(fmt.Sprintf("%s: T (%s) must embed %s, e.g. `%s`", pkg, t, want, example))
}

// NewHolder allocates a *T and stores manager into its embedded field.
func NewHolder[T any](fieldIdx int, manager any) *T {
	h := new(T)
	reflect.ValueOf(h).Elem().Field(fieldIdx).Set(reflect.ValueOf(manager))
	return h
}

// ManagerOf reads the embedded manager back out of a holder.
func ManagerOf[M any](h any, fieldIdx int) M {
	return reflect.ValueOf(h).Elem().Field(fieldIdx).Interface().(M)
}
