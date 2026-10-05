package view

import (
	"log"
	"os"
	"reflect"
	"strings"
	"unsafe"
)

// Assign is a piece of a live page's state, as LiveView's assigns are: the
// page sets it, Render reads it, and the page knows what changed.
//
//	type Orders struct {
//	    Status view.Assign[string]
//	    Rows   view.Assign[[]Order]
//	}
//
//	func (p *Orders) Filter(ctx context.Context, svc *OrderService, status string) error {
//	    p.Status.Set(status)
//	    p.Rows.Set(svc.List(ctx, status))
//	    return nil
//	}
//
//	templ (p *Orders) Render() {
//	    <h1>Orders · { p.Status.Get() }</h1>
//	    for _, o := range p.Rows.Get() { @OrderRow(o) }
//	}
//
// A page whose fields are all Assigns (signals may sit beside them) renders
// only what changed: a part of the template is skipped when none of the
// Assigns it read last time has changed since, and it doesn't run at all.
// Render must depend on Assigns alone — not on plain fields, services or
// the clock — for that to hold; under nexus dev and in tests every skipping
// render is checked against a full one and a difference is reported.
//
// A page with any other field renders everything, as before — unless the
// field is tagged view:"-": one Render doesn't read, or that doesn't change
// once the page is mounted (a configuration pointer, permissions read in
// Mount). Its zero value
// is ready to use; Set and Update are for the page's own goroutine — Mount,
// Params, Info and events.
type Assign[T any] struct {
	v   T
	ver uint64
	t   *tracker
	bit uint64
}

// Get returns the value. In Render it notes that this part of the page
// depends on it.
func (a *Assign[T]) Get() T {
	if a.t != nil && a.t.rec != nil {
		a.t.rec.read(a.bit)
	}
	return a.v
}

// Set replaces the value. Setting an equal value of a comparable type is not
// a change.
func (a *Assign[T]) Set(v T) {
	if equal(a.v, v) {
		return
	}
	a.v = v
	a.touch()
}

// Update changes the value in place — an element of a slice, a key of a map —
// and marks it changed:
//
//	p.Rows.Update(func(rows *[]Order) { (*rows)[i].Status = "paid" })
func (a *Assign[T]) Update(fn func(*T)) {
	fn(&a.v)
	a.touch()
}

func (a *Assign[T]) touch() {
	if a.t != nil {
		a.ver = a.t.bump()
	} else {
		a.ver++
	}
}

func (a *Assign[T]) bindAssign(t *tracker, bit uint64) { a.t, a.bit = t, bit }
func (a *Assign[T]) version() uint64                   { return a.ver }

// equal compares two values of a comparable type; anything else, or a
// comparison that would panic (an interface holding a slice), is unequal.
func equal[T any](a, b T) (eq bool) {
	if !reflect.TypeFor[T]().Comparable() {
		return false
	}
	defer func() {
		if recover() != nil {
			eq = false
		}
	}()
	return any(a) == any(b)
}

type assignField interface {
	bindAssign(t *tracker, bit uint64)
	version() uint64
}

// Bits of a render's reads: one per Assign field (the last shared by any
// beyond it), and one for the page's form errors.
const (
	fieldBits = 62
	errsBit   = uint64(1) << 62
)

// tracker is a live page instance's change tracking: its Assigns, the
// version counter they draw from, and the recorder of the render under way.
type tracker struct {
	page    any // the instance, *T: the receiver skippable parts may read
	epoch   uint64
	fields  []assignField
	errsVer uint64    // the version at which the form errors last changed
	rec     *recorder // the tracked render under way, if any
	off     string    // why the page isn't tracked; "" when it is
	uploads map[string]*Upload
	// flushers are the streams, which let go of a change once it is sent.
	flushers []interface{ flush() }
}

func (t *tracker) bump() uint64 {
	t.epoch++
	return t.epoch
}

// changed is the reads that changed after epoch.
func (t *tracker) changed(epoch uint64) uint64 {
	var m uint64
	for i, f := range t.fields {
		if f.version() > epoch {
			m |= fieldBit(i)
		}
	}
	if t.errsVer > epoch {
		m |= errsBit
	}
	return m
}

func fieldBit(i int) uint64 {
	if i >= fieldBits {
		i = fieldBits - 1
	}
	return uint64(1) << i
}

var (
	assignFieldType = reflect.TypeFor[assignField]()
	signalPkg       = reflect.TypeFor[Signal[int]]().PkgPath()
)

// trackAssigns binds the Assigns of v (*T) to a new tracker. The page is tracked
// when it has Assigns and nothing else that could change: signals, which
// the server never changes, and fields tagged view:"-" may sit beside them.
func trackAssigns(v reflect.Value) *tracker {
	t := &tracker{page: v.Interface()}
	s := v.Elem()
	st := s.Type()
	for i := 0; i < st.NumField(); i++ {
		f := st.Field(i)
		ptr := reflect.NewAt(f.Type, unsafe.Pointer(s.Field(i).UnsafeAddr()))
		if u, ok := ptr.Interface().(*Upload); ok {
			u.bindUpload(t, fieldBit(len(t.fields)), f.Name)
			t.fields = append(t.fields, u)
			if t.uploads == nil {
				t.uploads = map[string]*Upload{}
			}
			t.uploads[f.Name] = u
			continue
		}
		if f, ok := ptr.Interface().(interface{ flush() }); ok {
			t.flushers = append(t.flushers, f)
		}
		if ptr.Type().Implements(assignFieldType) {
			a := ptr.Interface().(assignField)
			a.bindAssign(t, fieldBit(len(t.fields)))
			t.fields = append(t.fields, a)
			continue
		}
		if isSignal(f.Type) || f.Tag.Get("view") == "-" {
			continue
		}
		if t.off == "" {
			t.off = "field " + f.Name + " is not a view.Assign"
		}
	}
	if len(t.fields) == 0 && t.off == "" {
		t.off = "it has no view.Assign fields"
	}
	return t
}

func isSignal(t reflect.Type) bool {
	return t.Kind() == reflect.Pointer && t.Elem().PkgPath() == signalPkg && len(t.Elem().Name()) > 7 && t.Elem().Name()[:7] == "Signal["
}

// verifyTracking checks every render that skips spots against a full one:
// under nexus dev, in tests, or with NEXUS_VIEW_VERIFY=1 (0 turns it off).
var verifyTracking = func() bool {
	switch os.Getenv("NEXUS_VIEW_VERIFY") {
	case "1":
		return true
	case "0":
		return false
	}
	return os.Getenv("NEXUS_DEV") != "" || strings.HasSuffix(os.Args[0], ".test")
}()

// reportStale tells the developer that a page's skipped spots rendered
// differently: the full render was sent.
var reportStale = func(page reflect.Type, labels []string) {
	log.Printf("nexus view: %s: %s rendered differently although none of the view.Assigns read there changed — "+
		"Render reads something else that changed (a plain field, a service, the clock); keep it in a view.Assign. The full render was sent.",
		page, strings.Join(labels, ", "))
}

// staleSpots compares a tracked render with a full one of the same state
// and returns the labels of the skipped spots that differ.
func staleSpots(tracked, whole *rframe) []string {
	if dynSig(tracked) == dynSig(whole) {
		return nil
	}
	var out []string
	var walk func(a, b any)
	walk = func(a, b any) {
		switch a := a.(type) {
		case *kept:
			if dynSig(a) != dynSig(b) {
				out = append(out, a.s.label)
			}
		case *rframe:
			if bf, ok := b.(*rframe); ok && a.fp == bf.fp && len(a.d) == len(bf.d) {
				for i := range a.d {
					walk(a.d[i], bf.d[i])
				}
				return
			}
			if dynSig(a) != dynSig(b) {
				out = append(out, keptIn(a)...)
			}
		case *rcomp:
			if bc, ok := b.(*rcomp); ok && len(a.items) == len(bc.items) {
				for i := range a.items {
					walk(a.items[i], bc.items[i])
				}
				return
			}
			if dynSig(a) != dynSig(b) {
				out = append(out, keptIn(a)...)
			}
		}
	}
	walk(tracked, whole)
	if len(out) == 0 {
		out = []string{"the page"}
	}
	return out
}

// keptIn lists the skipped spots in a part of a tree.
func keptIn(d any) []string {
	var out []string
	switch d := d.(type) {
	case *kept:
		out = append(out, d.s.label)
	case *rframe:
		for _, x := range d.d {
			out = append(out, keptIn(x)...)
		}
	case *rcomp:
		for _, it := range d.items {
			out = append(out, keptIn(it)...)
		}
	}
	return out
}
