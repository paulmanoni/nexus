package view

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sync"
	"unsafe"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/di"
	"github.com/paulmanoni/nexus/v2/httpx"
)

// A live view is a struct whose state lives on the server, one copy per
// place it is shown, and changes through events — Phoenix LiveView's model.
// The same type is a page when it is routed and a part of a page when
// another live view embeds it:
//
//	type OrderDetail struct {
//	    view.LiveView
//	    Order view.Assign[Order]
//	}
//
//	type OrderProps struct {
//	    ID  int64  `path:"id"`
//	    Tab string `query:"tab"`
//	}
//
//	func (d *OrderDetail) Mount(ctx context.Context, svc *OrderService, p OrderProps) error {
//	    d.Subscribe("orders")
//	    d.Order.Set(svc.Get(ctx, p.ID))
//	    return nil
//	}
//
//	func (d *OrderDetail) Pay(ctx context.Context, svc *OrderService) error {
//	    …
//	    d.PutFlash("success", "Paid")
//	    return nil
//	}
//
//	templ (d *OrderDetail) Render() { … <button onclick={ view.Send(d.Pay) }>Pay</button> … }
//
// Routed, it is a page: view.Live[*OrderDetail]("/orders/:id", gates…)
// binds the props from the path and the query (path:, query: tags,
// validate: rules). Embedded, it is a component: @view.Component[*OrderDetail](id, props)
// in another live view's template, with the props the parent passes.
//
// Its methods follow conventions — dependencies are pointer or interface
// parameters, injected:
//
//	Mount(ctx, deps…, [props P]) error     the first render
//	Update(ctx, deps…, props P) error      optional: new props — a parent's, or a patch of the page's URL;
//	                                       without it, Mount runs again
//	Info(ctx, deps…, msg view.Message) error  optional: a broadcast to a topic it subscribed to
//	Render() templ.Component               a templ method component
//	Cancel(ctx, deps…, args…) error        an event: any other exported method of this shape
//
// An event sent from its template (view.Send(d.Pay), Submit, Change) reaches
// the instance it was sent from. Event arguments are user input, and an
// event is reachable by any client that can open the page: authorize and
// validate inside it.

// LiveView is what a live view embeds, by value: its link to the page it is
// part of — subscriptions, presence, navigation, flash messages.
//
//	type Chat struct {
//	    view.LiveView
//	    Messages view.Stream[Message]
//	}
type LiveView struct {
	in    *instance
	flash map[string]string
	t     *tracker
	bit   uint64
	ver   uint64
}

// Connected reports whether this render is the live connection's (true)
// or the first, server-rendered page load (false) — skip expensive work,
// or start subscriptions, only when connected.
func (v *LiveView) Connected() bool { return v.sock().connected }

// ID is the view's id where a page embeds it (view.Component), "" for the
// page itself.
func (v *LiveView) ID() string {
	if v.in == nil {
		return ""
	}
	return v.in.id
}

// Subscribe makes the view's Info hear the broadcasts to topics
// (view.Broadcast) while it is shown. Before the page connects it does
// nothing.
func (v *LiveView) Subscribe(topics ...string) {
	if v.in != nil {
		v.sock().subscribe(v.in, topics...)
	}
}

// Track makes the page present on topic as key, with meta (JSON-encoded),
// while the view is shown — or until Untrack. Pages subscribed to topic get
// a PresenceDiff. Before the page connects it does nothing.
func (v *LiveView) Track(topic, key string, meta any) {
	if v.in != nil {
		v.sock().track(v.in, topic, key, meta)
	}
}

// Untrack ends the view's presence as key on topic.
func (v *LiveView) Untrack(topic, key string) {
	if v.in != nil {
		v.sock().untrack(v.in, topic, key)
	}
}

// PushPatch, from an event, moves the browser to href once the event's
// reply is sent: on the page's own path (another query), its Update runs
// with the new props and the page is patched; another live page opens on
// the connection. The browser's history gets the new URL.
func (v *LiveView) PushPatch(href string) { v.push(href) }

// PushNavigate, from an event, opens the page at href — on the connection
// when it is a live page, else with a page load.
func (v *LiveView) PushNavigate(href string) { v.push(href) }

func (v *LiveView) push(href string) {
	if v.in != nil {
		if p := v.in.root(); p.nav != nil {
			*p.nav = href
		}
	}
}

// PutFlash sets a message of kind ("info", "error", …) for the next
// render; the page's next event clears it.
func (v *LiveView) PutFlash(kind, msg string) {
	if v.flash == nil {
		v.flash = map[string]string{}
	}
	v.flash[kind] = msg
	v.touch()
}

// Flash is the message PutFlash left for kind, or "".
func (v *LiveView) Flash(kind string) string {
	if v.t != nil && v.t.rec != nil {
		v.t.rec.read(v.bit)
	}
	return v.flash[kind]
}

func (v *LiveView) clearFlash() {
	if len(v.flash) > 0 {
		clear(v.flash)
		v.touch()
	}
}

func (v *LiveView) touch() {
	if v.t != nil {
		v.ver = v.t.bump()
	} else {
		v.ver++
	}
}

func (v *LiveView) sock() *socket {
	if v.in == nil {
		return &socket{}
	}
	return v.in.root().sock
}

func (v *LiveView) bindAssign(t *tracker, bit uint64) { v.t, v.bit = t, bit }
func (v *LiveView) version() uint64                   { return v.ver }

var liveViewType = reflect.TypeFor[LiveView]()

// liveViewOf is the LiveView v (*T) embeds, or nil.
func liveViewOf(v reflect.Value) *LiveView {
	s := v.Elem()
	for i := 0; i < s.NumField(); i++ {
		f := s.Type().Field(i)
		if f.Anonymous && f.Type == liveViewType {
			return (*LiveView)(unsafe.Pointer(s.Field(i).UnsafeAddr()))
		}
	}
	return nil
}

// A live view type is set up per app: its definition, the DI instance its
// copies start from, and its dependencies.
type viewSetup struct {
	def      *liveDef
	template reflect.Value // *T, or a nil *T
	deps     []reflect.Value
}

var (
	views = struct {
		sync.Mutex
		m map[*nexus.App]map[reflect.Type]*viewSetup
	}{m: map[*nexus.App]map[reflect.Type]*viewSetup{}}
	defs sync.Map // reflect.Type → *liveDef, or error
	// embedded are the live view types some page embeds, by LiveKey: Send
	// names the type of such a view's method, so the browser finds the
	// instance it belongs to. A page's own events need no name.
	embedded sync.Map
)

// defFor reads a live view type's methods, once.
func defFor(t reflect.Type) (*liveDef, error) {
	if d, ok := defs.Load(t); ok {
		if err, bad := d.(error); bad {
			return nil, err
		}
		return d.(*liveDef), nil
	}
	d, err := newLiveDef(t)
	if err != nil {
		defs.Store(t, err)
		return nil, err
	}
	defs.Store(t, d)
	return d, nil
}

// register sets t up for app at boot, with its template (when DI has one)
// and dependencies.
func register(def *liveDef) nexus.Option {
	in := append([]reflect.Type{reflect.TypeFor[*nexus.App](), def.t}, def.depTypes...)
	tags := make([]string, len(in))
	tags[1] = `optional:"true"`
	fn := reflect.MakeFunc(reflect.FuncOf(in, nil, false), func(args []reflect.Value) []reflect.Value {
		app := args[0].Interface().(*nexus.App)
		views.Lock()
		if views.m[app] == nil {
			views.m[app] = map[reflect.Type]*viewSetup{}
		}
		views.m[app][def.t] = &viewSetup{def: def, template: args[1], deps: args[2:]}
		views.Unlock()
		return nil
	})
	return nexus.Raw(di.Invoke(di.Annotate(fn.Interface(), di.ParamTags(tags...))))
}

// viewFor is t's setup in app. A type never registered is set up on first
// use when it needs no dependencies.
func viewFor(app *nexus.App, t reflect.Type) (*viewSetup, error) {
	views.Lock()
	defer views.Unlock()
	if s := views.m[app][t]; s != nil {
		return s, nil
	}
	def, err := defFor(t)
	if err != nil {
		return nil, err
	}
	if len(def.depTypes) > 0 {
		return nil, fmt.Errorf("view: %s takes dependencies (%s): register it — view.Live[%s](\"\") to embed it only, or put //nexus:live on it", t, def.depTypes[0], t)
	}
	if views.m[app] == nil {
		views.m[app] = map[reflect.Type]*viewSetup{}
	}
	s := &viewSetup{def: def, template: reflect.Zero(t)}
	views.m[app][t] = s
	return s, nil
}

// bindProps binds a routed view's props from its path parameters and URL
// query (path:, query: tags), then checks their validate: rules.
func bindProps(t reflect.Type, param func(string) string, u *url.URL) (reflect.Value, error) {
	st := t
	if st.Kind() == reflect.Pointer {
		st = st.Elem()
	}
	ptr := reflect.New(st)
	target := "/"
	if u != nil {
		target = u.RequestURI()
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	c := httpx.NewCtx(httptest.NewRecorder(), req, param)
	if err := c.ShouldBindUri(ptr.Interface()); err != nil {
		return reflect.Value{}, nexus.Errf(nexus.InvalidInput, "%v", err)
	}
	if err := c.ShouldBindQuery(ptr.Interface()); err != nil {
		return reflect.Value{}, nexus.Errf(nexus.InvalidInput, "%v", err)
	}
	if err := nexus.Validate(ptr.Interface()); err != nil {
		return reflect.Value{}, err
	}
	if t.Kind() == reflect.Pointer {
		return ptr, nil
	}
	return ptr.Elem(), nil
}

// componentType is key when it names a live view type a page embeds, for
// Send to name: "" otherwise.
func componentType(key string) string {
	if _, ok := embedded.Load(key); ok {
		return key
	}
	return ""
}

// The props an Update or Mount takes, by liveDef.
func (d *liveDef) propsOf(m *liveMethod) reflect.Type {
	if m == nil || len(m.args) == 0 {
		return nil
	}
	return m.fn.Type().In(m.args[0])
}
