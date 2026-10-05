package view

import (
	"context"
	"fmt"
	"html"
	"io"
	"reflect"

	"github.com/a-h/templ"
)

// Embedding a live view: any live view (liveview.go) can be placed in
// another's template by an id unique among its type's there.
//
//	@view.Component[*Cart]("cart", CartProps{User: p.User.Get()})
//
// The page keeps one instance per id while its template renders it, and
// drops one it no longer renders — its subscriptions and presence with it.
// Mount runs on the first render, Update (or Mount again) when the props
// change. Its events reach its own instance, its Info hears what it
// subscribed to, and with Assign fields an event of it renders only it and
// the parts of the page around it; re-rendered with the same props and
// nothing changed, it is skipped.

// component is one embedded instance of a live view on a page.
type component struct {
	key     string // its type's LiveKey, "#", its id
	setup   *viewSetup
	in      *instance
	props   reflect.Value
	mounted bool
	epoch   uint64 // its tracker's version counter when it last rendered
	seen    uint64 // the page render that last rendered (or kept) it
}

// dirty reports whether the component changed since it last rendered.
func (c *component) dirty() bool {
	return c.in.tr.off != "" || c.in.tr.changed(c.epoch) != 0
}

type instanceKey struct{}

// withInstance tells the views a page renders, and its events, which page
// they belong to.
func withInstance(ctx context.Context, in *instance) context.Context {
	return context.WithValue(ctx, instanceKey{}, in)
}

func instanceFrom(ctx context.Context) *instance {
	in, _ := ctx.Value(instanceKey{}).(*instance)
	return in
}

// Component places the live view T in a live view's template: id tells it
// apart from the others of its type there, and props are what its Mount and
// Update take (nil when they take none).
//
//	@view.Component[*Cart]("cart", CartProps{User: p.User.Get()})
//
// A T that takes dependencies is registered with view.Live[T] — with a
// path, or "" to embed it only; one that takes none needs nothing.
func Component[T any](id string, props any) templ.Component {
	t := reflect.TypeFor[T]()
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		page := instanceFrom(ctx)
		if page == nil {
			return fmt.Errorf("view.Component[%s]: a live view is embedded in another live view's template", t)
		}
		page = page.root()
		setup, err := viewFor(appFrom(ctx), t)
		if err != nil {
			return fmt.Errorf("view.Component[%s]: %w", t, err)
		}
		pv := reflect.ValueOf(props)
		if setup.def.props == nil {
			if props != nil {
				return fmt.Errorf("view.Component[%s]: its Mount takes no props", t)
			}
		} else if !pv.IsValid() || !pv.Type().AssignableTo(setup.def.props) {
			return fmt.Errorf("view.Component[%s]: props must be %s, got %T", t, setup.def.props, props)
		}
		embedded.Store(LiveKey(t), true)
		key := LiveKey(t) + "#" + id
		c := page.comps[key]
		if c == nil {
			c = &component{key: key, setup: setup, in: setup.def.instance(setup.template, setup.deps)}
			c.in.page, c.in.id = page, id
			if page.comps == nil {
				page.comps = map[string]*component{}
			}
			page.comps[key] = c
		}
		c.seen = page.gen
		return c.render(ctx, Record(ctx, w), w, id, pv)
	})
}

// render mounts or updates the component as its props say, then renders it
// inside its element — a spot of the page that a tracked render skips when
// the component didn't change.
func (c *component) render(ctx context.Context, rec Rec, w io.Writer, id string, props reflect.Value) error {
	r := rec.r
	tracked := r != nil && r.table != nil
	if r != nil {
		r.flush()
		label := "component " + c.key
		n := &recNode{guarded: tracked, label: label, boundary: true}
		n.key = spotKey(r.top().key, label)
		r.stack = append(r.stack, n)
		defer func() {
			r.flush()
			r.pop()
			r.top().parts = append(r.top().parts, recPart{node: n})
		}()
		n.comps = []*component{c}
		sc := scopeFrom(ctx)
		if tracked && r.prev != nil && c.mounted && !c.propsChanged(props) && !c.dirty() && r.changed&errsBit == 0 {
			if s := r.prev.spots[n.key]; s != nil && s.value != nil && !s.taint && !r.again[n.key] {
				n.kept = s
				r.carry(s, n.key)
				if sc != nil {
					replay(sc, s.effects)
				}
				return nil
			}
		}
		if sc != nil {
			n.scope, n.logStart = sc, len(sc.log)
		}
	}
	if err := c.prepare(ctx, props); err != nil {
		return err
	}
	open := `<nx-c data-nx-c="` + html.EscapeString(id) + `" data-nx-ct="` + html.EscapeString(LiveKey(c.setup.def.t)) + `">`
	if err := rec.static(w, open); err != nil {
		return err
	}
	comp, _ := c.in.v.MethodByName("Render").Call(nil)[0].Interface().(templ.Component)
	if comp == nil {
		return fmt.Errorf("view.Component[%s]: Render returned nil", c.setup.def.t)
	}
	if r != nil {
		// Inside the component, its own Assigns are what changed.
		saved, savedChanged := r.track, r.changed
		if tracked {
			r.track, r.changed = c.in.tr, c.in.tr.changed(c.epoch)|(savedChanged&errsBit)
		}
		c.in.tr.rec = r
		defer func() { r.track, r.changed, c.in.tr.rec = saved, savedChanged, nil }()
	}
	if err := comp.Render(ctx, w); err != nil {
		return err
	}
	c.epoch = c.in.tr.epoch
	return rec.static(w, `</nx-c>`)
}

func (c *component) propsChanged(props reflect.Value) bool {
	return c.setup.def.props != nil && (!c.props.IsValid() || !reflect.DeepEqual(c.props.Interface(), props.Interface()))
}

// prepare runs Mount on the first render, then Update (or Mount again,
// without one) when the props changed.
func (c *component) prepare(ctx context.Context, props reflect.Value) error {
	if c.mounted && !c.propsChanged(props) {
		return nil
	}
	if c.setup.def.props != nil {
		c.props = props
	}
	first := !c.mounted
	c.mounted = true
	var err error
	if first {
		err = c.in.mount(ctx, props)
	} else {
		err = c.in.update(ctx, props)
	}
	if err != nil {
		if first {
			c.mounted = false
		}
		return fmt.Errorf("view.Component[%s] %w", c.setup.def.t, err)
	}
	return nil
}

// UpdateComponent gives the live view T embedded as id on the page that ctx
// is an event (or Info) of new props: its Update runs (or Mount, without
// one) and the page re-renders. The page's template passes its own props
// again on the next render that changes them.
func UpdateComponent[T any](ctx context.Context, id string, props any) error {
	t := reflect.TypeFor[T]()
	page := instanceFrom(ctx)
	if page == nil {
		return fmt.Errorf("view.UpdateComponent[%s]: not in a live page's event", t)
	}
	c := page.comps[LiveKey(t)+"#"+id]
	if c == nil {
		return fmt.Errorf("view.UpdateComponent[%s]: no %q on the page", t, id)
	}
	pv := reflect.ValueOf(props)
	if c.setup.def.props == nil || !pv.IsValid() || !pv.Type().AssignableTo(c.setup.def.props) {
		return fmt.Errorf("view.UpdateComponent[%s]: props must be %v", t, c.setup.def.props)
	}
	return c.in.update(ctx, pv)
}

// sweep drops the components the render numbered gen didn't render: what
// they subscribed to and tracked goes with them.
func (in *instance) sweep(gen uint64) {
	for k, c := range in.comps {
		if c.seen != gen {
			in.sock.drop(c.in)
			delete(in.comps, k)
		}
	}
}
