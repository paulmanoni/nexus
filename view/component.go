package view

import (
	"context"
	"fmt"
	"html"
	"io"
	"reflect"
	"sync"

	"github.com/a-h/templ"

	"github.com/paulmanoni/nexus/v2"
)

// A live component is a part of a live page with state and events of its
// own, as Phoenix LiveView's LiveComponents are:
//
//	type Cart struct {
//	    Items view.Assign[[]Item]
//	    Open  view.Assign[bool]
//	}
//
//	type CartProps struct{ User string }
//
//	func (c *Cart) Mount(ctx context.Context, svc *CartService, p CartProps) error {
//	    c.Items.Set(svc.Items(ctx, p.User))
//	    return nil
//	}
//
//	func (c *Cart) Toggle(ctx context.Context) error {
//	    c.Open.Update(func(o *bool) { *o = !*o })
//	    return nil
//	}
//
//	templ (c *Cart) Render() {
//	    <button onclick={ view.Send(c.Toggle) }>Cart ({ strconv.Itoa(len(c.Items.Get())) })</button>
//	    if c.Open.Get() { … }
//	}
//
// Register it once — view.LiveComponent[*Cart](NewCart) is an option, like
// view.Live — and place it in any live page's template by an id unique
// among its type's on the page:
//
//	@view.Component[*Cart]("cart", CartProps{User: p.User.Get()})
//
// The page keeps one instance per id while its template renders it, and
// drops one it no longer renders. Methods follow a live page's conventions:
//
//	Mount(ctx, deps…, props P) error    the first render (and, without Update, each change of props)
//	Update(ctx, deps…, props P) error   optional: a render with different props
//	Toggle(ctx, deps…, args…) error     an event: any other exported method of this shape
//
// An event sent from its template (view.Send(c.Toggle), Submit, Change)
// reaches its own instance — no target to name — and re-renders the page:
// with Assign fields, only the component's changed parts run; the rest of
// the page is skipped. A page passes it new data through props, or with
// view.UpdateComponent from an event or Info.

// LiveComponent registers T (a pointer to a struct) as a live component.
// With constructors, the DI instance T is the template every placement
// copies; without, it starts from T's zero value.
func LiveComponent[T any](constructors ...any) nexus.Option {
	t := reflect.TypeFor[T]()
	def, err := newComponentDef(t)
	if err != nil {
		return nexus.FailBoot(err)
	}
	componentTypes.Store(LiveKey(t), true)
	in := []reflect.Type{reflect.TypeFor[*nexus.App]()}
	if len(constructors) > 0 {
		in = append(in, t)
	}
	in = append(in, def.depTypes...)
	fn := reflect.MakeFunc(reflect.FuncOf(in, nil, false), func(args []reflect.Value) []reflect.Value {
		app := args[0].Interface().(*nexus.App)
		args = args[1:]
		tmpl := reflect.Zero(t)
		if len(constructors) > 0 {
			tmpl, args = args[0], args[1:]
		}
		components.Lock()
		if components.m[app] == nil {
			components.m[app] = map[reflect.Type]*componentSetup{}
		}
		components.m[app][t] = &componentSetup{def: def, template: tmpl, deps: args}
		components.Unlock()
		return nil
	})
	opts := []nexus.Option{nexus.Invoke(fn.Interface())}
	if len(constructors) > 0 {
		opts = append([]nexus.Option{nexus.Provide(constructors...)}, opts...)
	}
	return nexus.Options(opts...)
}

type componentSetup struct {
	def      *liveDef
	template reflect.Value // *T, or a nil *T
	deps     []reflect.Value
}

var (
	components = struct {
		sync.Mutex
		m map[*nexus.App]map[reflect.Type]*componentSetup
	}{m: map[*nexus.App]map[reflect.Type]*componentSetup{}}
	// componentTypes are the registered component types by LiveKey: Send
	// names the type of a component's method, so the browser finds the
	// instance it belongs to.
	componentTypes sync.Map
)

func componentFor(app *nexus.App, t reflect.Type) *componentSetup {
	components.Lock()
	defer components.Unlock()
	return components.m[app][t]
}

// newComponentDef reads a component type's methods as a live page's are.
func newComponentDef(t reflect.Type) (*liveDef, error) {
	if t.Kind() != reflect.Pointer || t.Elem().Kind() != reflect.Struct {
		return nil, fmt.Errorf("view.LiveComponent[%s]: want a pointer to a struct", t)
	}
	d := &liveDef{t: t, events: map[string]*liveMethod{}, depIndex: map[reflect.Type]int{}}
	render, ok := t.MethodByName("Render")
	if !ok || render.Type.NumIn() != 1 || render.Type.NumOut() != 1 || render.Type.Out(0) != compType {
		return nil, fmt.Errorf("view.LiveComponent[%s]: needs a Render() templ.Component method — a templ method component: templ (x %s) Render() { … }", t, t)
	}
	for i := 0; i < t.NumMethod(); i++ {
		m := t.Method(i)
		if m.Name == "Render" {
			continue
		}
		lm, ok := d.method(m)
		if !ok {
			if m.Name == "Mount" || m.Name == "Update" {
				return nil, fmt.Errorf("view.LiveComponent[%s]: %s must be func(ctx context.Context, deps…, props P) error, got %s", t, m.Name, m.Type)
			}
			continue
		}
		switch m.Name {
		case "Mount", "Update":
			if lm.live >= 0 || len(lm.args) > 1 {
				return nil, fmt.Errorf("view.LiveComponent[%s]: %s takes ctx, dependencies and one props value, got %s", t, m.Name, m.Type)
			}
			if len(lm.args) == 1 {
				p := m.Type.In(lm.args[0])
				if d.props != nil && d.props != p {
					return nil, fmt.Errorf("view.LiveComponent[%s]: Mount and Update take different props (%s, %s)", t, d.props, p)
				}
				d.props = p
			}
			if m.Name == "Mount" {
				d.mount = lm
			} else {
				d.update = lm
			}
		case "Params", "Info":
			return nil, fmt.Errorf("view.LiveComponent[%s]: a component has no %s — the page passes it data through props or view.UpdateComponent", t, m.Name)
		default:
			if lm.live >= 0 {
				return nil, fmt.Errorf("view.LiveComponent[%s]: event %s can't take a *view.Socket", t, m.Name)
			}
			d.events[m.Name] = lm
		}
	}
	return d, nil
}

// component is one placed instance of a live component on a page.
type component struct {
	key     string // its type's LiveKey, "#", its id
	setup   *componentSetup
	in      *instance // its state, called like a page's
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

// withInstance tells the components a page renders, and its events, which
// page they belong to.
func withInstance(ctx context.Context, in *instance) context.Context {
	return context.WithValue(ctx, instanceKey{}, in)
}

func instanceFrom(ctx context.Context) *instance {
	in, _ := ctx.Value(instanceKey{}).(*instance)
	return in
}

// Component places the live component T, registered with LiveComponent,
// on a live page: id tells it apart from the others of its type there, and
// props are its Mount's (and Update's) props.
//
//	@view.Component[*Cart]("cart", CartProps{User: p.User.Get()})
func Component[T any](id string, props any) templ.Component {
	t := reflect.TypeFor[T]()
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		page := instanceFrom(ctx)
		if page == nil {
			return fmt.Errorf("view.Component[%s]: a live component renders on a live page (view.Live)", t)
		}
		setup := componentFor(appFrom(ctx), t)
		if setup == nil {
			return fmt.Errorf("view.Component[%s]: not registered — add view.LiveComponent[%s]() to the app", t, t)
		}
		pv := reflect.ValueOf(props)
		if setup.def.props == nil {
			if props != nil {
				return fmt.Errorf("view.Component[%s]: it has no Mount or Update to take props", t)
			}
		} else if !pv.IsValid() || !pv.Type().AssignableTo(setup.def.props) {
			return fmt.Errorf("view.Component[%s]: props must be %s, got %T", t, setup.def.props, props)
		}
		key := LiveKey(t) + "#" + id
		c := page.comps[key]
		if c == nil {
			c = &component{key: key, setup: setup, in: setup.def.instance(setup.template, setup.deps)}
			c.in.page = page
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
		changedProps := c.setup.def.props != nil && (!c.props.IsValid() || !reflect.DeepEqual(c.props.Interface(), props.Interface()))
		if tracked && r.prev != nil && c.mounted && !changedProps && !c.dirty() && r.changed&errsBit == 0 {
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

// prepare runs Mount on the first render, then Update (or Mount again,
// without one) when the props changed.
func (c *component) prepare(ctx context.Context, props reflect.Value) error {
	def := c.setup.def
	changed := def.props != nil && (!c.props.IsValid() || !reflect.DeepEqual(c.props.Interface(), props.Interface()))
	if c.mounted && !changed {
		return nil
	}
	m := def.mount
	if c.mounted && def.update != nil {
		m = def.update
	}
	if def.props != nil {
		c.props = props
	}
	first := !c.mounted
	c.mounted = true
	if m == nil {
		return nil
	}
	var args []reflect.Value
	if len(m.args) == 1 {
		args = []reflect.Value{props}
	}
	if err := c.in.callValues(ctx, m, nil, args); err != nil {
		if first {
			c.mounted = false
		}
		return fmt.Errorf("view.Component[%s] %s: %w", def.t, map[bool]string{true: "Mount", false: "Update"}[first || def.update == nil], err)
	}
	return nil
}

// UpdateComponent gives the component T placed as id on the page that ctx
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
		return fmt.Errorf("view.UpdateComponent[%s]: no component %q on the page", t, id)
	}
	def := c.setup.def
	m := def.update
	if m == nil {
		m = def.mount
	}
	pv := reflect.ValueOf(props)
	if m == nil || len(m.args) != 1 || !pv.IsValid() || !pv.Type().AssignableTo(def.props) {
		return fmt.Errorf("view.UpdateComponent[%s]: props must be %v", t, def.props)
	}
	return c.in.callValues(ctx, m, nil, []reflect.Value{pv})
}

// sweep drops the components the render numbered gen didn't render.
func (in *instance) sweep(gen uint64) {
	for k, c := range in.comps {
		if c.seen != gen {
			delete(in.comps, k)
		}
	}
}

// componentType is key when it names a registered live component type, for
// Send to name: "" otherwise.
func componentType(key string) string {
	if _, ok := componentTypes.Load(key); ok {
		return key
	}
	return ""
}
