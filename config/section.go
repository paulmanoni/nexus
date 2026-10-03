package config

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// SectionHandle is a declared top-level table of nexus.toml, decoded into T.
// Create one with Section, usually as a package-level variable.
type SectionHandle[T any] struct {
	name    string
	def     T
	val     atomic.Pointer[T]
	present atomic.Bool
}

// Section declares the top-level table [name] of nexus.toml and decodes it
// into T. Declaring a table is what makes it legal in the file: nexus.toml
// is strict, so a table nobody declared — and a key T has no field for —
// fails boot with the file position and a did-you-mean.
//
//	type ShopConfig struct {
//	    Currency string        `toml:"currency"`
//	    TaxRate  float64       `toml:"tax_rate"`
//	    Timeout  time.Duration `toml:"timeout"` // a TOML string: "30s"
//	}
//
//	var Shop = config.Section[ShopConfig]("shop", ShopConfig{Currency: "USD"})
//
//	func price(p int) string { return format(p, Shop.Get().Currency) }
//
// The optional default is the value Get returns before the file loads and
// the base the table decodes over (a key absent from the file keeps its
// default). Keys come from `toml` tags, else the lowercased field name;
// time.Duration fields read Go duration strings. T may be a map
// (map[string]Store for [stores.<name>] tables) or map[string]any for a
// free-form table whose keys are not checked.
//
// config.Get still reads any key of a declared table ("shop.currency"),
// with its ENV override; Get on the handle returns the file's values as
// decoded at boot.
//
// Section panics when name is empty, contains a dot, or is already declared
// (by nexus, an extension, or another Section call) — a declaration is a
// package-level fact, so a clash is a programming error caught at init.
func Section[T any](name string, defaults ...T) *SectionHandle[T] {
	h := &SectionHandle[T]{name: name}
	if len(defaults) > 0 {
		h.def = defaults[0]
	}
	t := reflect.TypeFor[T]()
	shadow, _ := shadowType(t)
	declare(&sectionDecl{
		name: name,
		typ:  shadow,
		def: func() reflect.Value {
			v := reflect.New(shadow).Elem()
			_ = convertShadow(v, reflect.ValueOf(&h.def).Elem(), nil, false)
			return v
		},
		set: func(v reflect.Value, present bool) error {
			out := reflect.New(t)
			if err := convertShadow(out.Elem(), v, []string{name}, true); err != nil {
				return err
			}
			h.val.Store(out.Interface().(*T))
			h.present.Store(present)
			return nil
		},
	})
	return h
}

// Get returns the decoded table: the file's values over the declared
// default, or the default alone before nexus.toml loads.
func (h *SectionHandle[T]) Get() T {
	if p := h.val.Load(); p != nil {
		return *p
	}
	return h.def
}

// Name is the table name the handle declares.
func (h *SectionHandle[T]) Name() string { return h.name }

// Present reports whether the loaded nexus.toml contains the table.
func (h *SectionHandle[T]) Present() bool { return h.present.Load() }

// DeclareExtension records the keys of an [extensions.<name>] block, so they
// are checked as strictly as a section's (with positions) and appear in the
// JSON schema. schema is a value or pointer of the block's struct type. The
// extension still registers its decoder with nexus.RegisterExtensionDecoder;
// without this call its keys are left to the decoder.
func DeclareExtension(name string, schema any) {
	if name == "" || schema == nil {
		return
	}
	t := reflect.TypeOf(schema)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	shadow, _ := shadowType(t)
	sectionsMu.Lock()
	extensionSchemas[name] = shadow
	sectionsMu.Unlock()
}

// Sections returns the name of every declared top-level table, sorted.
func Sections() []string {
	sectionsMu.RLock()
	defer sectionsMu.RUnlock()
	out := make([]string, 0, len(sections))
	for n := range sections {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// IsDeclared reports whether the top-level table [name] is declared.
func IsDeclared(name string) bool {
	sectionsMu.RLock()
	defer sectionsMu.RUnlock()
	_, ok := sections[name]
	return ok
}

// sectionDecl is one declared top-level table.
type sectionDecl struct {
	name string
	// typ is the decode type: the declared type with time.Duration
	// replaced by string (see shadowType), so go-toml can decode it.
	typ reflect.Type
	// def returns the default as a typ value; nil means the zero value.
	def func() reflect.Value
	// set receives the decoded typ value after a successful load; nil for
	// tables whose loader lives in this package ([runtime], [databases]).
	set func(v reflect.Value, present bool) error
	// order keeps the document type deterministic: built-ins first, then
	// declaration order.
	order int
}

var (
	sectionsMu       sync.RWMutex
	sections         = map[string]*sectionDecl{}
	extensionSchemas = map[string]reflect.Type{}
	sectionSeq       int
)

func declare(d *sectionDecl) {
	if d.name == "" || strings.ContainsAny(d.name, ". \t\"'[]") {
		panic(fmt.Sprintf("config.Section: invalid table name %q — use one bare TOML key (no dots)", d.name))
	}
	sectionsMu.Lock()
	defer sectionsMu.Unlock()
	if _, dup := sections[d.name]; dup {
		panic(fmt.Sprintf("config.Section: [%s] is already declared — a table has one owner; read it with config.Get instead", d.name))
	}
	sectionSeq++
	d.order = sectionSeq
	sections[d.name] = d
}

// declaredSections snapshots the declarations in document order.
func declaredSections() []*sectionDecl {
	sectionsMu.RLock()
	defer sectionsMu.RUnlock()
	out := make([]*sectionDecl, 0, len(sections))
	for _, d := range sections {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].order < out[j].order })
	return out
}

// sectionOwners names the package that declares a framework table, so an
// app whose file has [cache.x] but which never imports extension/cache is
// told what to import rather than to declare the table itself.
var sectionOwners = map[string]string{
	"cache":   "github.com/paulmanoni/nexus/v2/extension/cache",
	"storage": "github.com/paulmanoni/nexus/v2/extension/storage",
	"mail":    "github.com/paulmanoni/nexus/v2/extension/mail",
	"jobs":    "github.com/paulmanoni/nexus/v2/extension/jobs",
}

// SectionOwner returns the import path of the framework package that declares
// [name], or "" when nexus has no such table.
func SectionOwner(name string) string { return sectionOwners[name] }
