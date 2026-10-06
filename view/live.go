package view

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/url"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/a-h/templ"
	"github.com/gorilla/websocket"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/httpx"
)

// LiveRouter is a live view's registration (liveview.go): routed at a
// path, it is a page; any live view can also be embedded in another's
// template with view.Component.
//
//	var Module = nexus.Module("orders",
//	    nexus.Path("/admin"),
//	    view.Live[*OrderDetail]("/orders/:id", auth.Requires("view_orders")).
//	        Provide(NewOrderDetail),
//	)
//
// T is a pointer to a struct. A DI instance of it — from Provide, when there
// is one — is the template each page and each connection copies; without
// one, T starts from its zero value.
type LiveRouter[T any] struct {
	*nexus.Router
}

// Live registers the live view T, routed at prefix and gated by gates — on
// the page and on its socket — or, with prefix "", only for embedding
// (view.Component), which a live view that takes dependencies needs. It is a
// nexus router, so it composes like one: inside nexus.Module it mounts under
// the module's Path.
func Live[T any](prefix string, gates ...nexus.MiddlewareOption) *LiveRouter[T] {
	t := reflect.TypeFor[T]()
	r := nexus.NewRouter(liveName(t), prefix, gates...)
	l := &LiveRouter[T]{Router: r}
	def, err := defFor(t)
	if err != nil {
		r.Register(nexus.FailBoot(err))
		return l
	}
	r.Register(register(def))
	if prefix == "" {
		return l
	}
	def.routed = true
	r.Rest("GET", "", def.pageHandler(), append([]nexus.RestOption{HTML(), nexus.Tag(LiveTag, LiveKey(t))}, def.liveTags()...)...)
	r.Rest("GET", "/_live", def.socketHandler(), nexus.WithRenderer(upgraded{}), nexus.HideFromDashboard())
	r.Rest("POST", "/_upload", uploadHandler, nexus.MaxBody(0), nexus.HideFromDashboard())
	return l
}

// LiveTag is the registry.Endpoint.Tags key on a live page's GET route; its
// value is LiveKey of the page's type. It is how tools (viewtest.Mount) find
// where a live page is served.
const LiveTag = "view.live"

// LiveKey names a live page's type T (a pointer to a struct) in LiveTag:
// its package path and type name.
func LiveKey(t reflect.Type) string {
	if k, ok := liveKeys.Load(t); ok {
		return k.(string)
	}
	e := t
	for e.Kind() == reflect.Pointer {
		e = e.Elem()
	}
	k := e.PkgPath() + "." + e.Name()
	liveKeys.Store(t, k)
	return k
}

var liveKeys sync.Map // reflect.Type → LiveKey

// Provide adds constructors — usually T's — under the live page's module.
func (l *LiveRouter[T]) Provide(fns ...any) *LiveRouter[T] {
	l.Router.Provide(fns...)
	return l
}

// socket is a page's connection, as its views see it.
type socket struct {
	connected bool
	inbox     chan Message
	topics    []string               // what the page is subscribed to on the hub
	subs      map[string][]*instance // by topic, the views whose Info hears it
	wake      chan bool              // an upload changed the page (upload.go)
	uploads   []*Upload              // the uploads the page took files for
	presences []tracked              // where the page is present (presence.go)
}

// Send is an event for an on* attribute: it calls method — a method value of
// the live page, such as l.Cancel — on the server with args.
//
//	<button onclick={ view.Send(l.Cancel, o.ID) }>cancel</button>
//
// args are encoded when the page renders; the server decodes them into the
// method's argument parameters.
func Send(method any, args ...any) templ.ComponentScript {
	mi := methodOf(method)
	return send(mi.name, args, componentType(mi.recv))
}

// SendTo is Send for the method named name on recv, for a live page whose
// type is generic: Go builds a generic type's method values as closures that
// don't carry the method's name, so Send can't read it. It panics, as Send
// does, when recv has no such method.
//
//	<button onclick={ view.SendTo(p, "Edit", row.ID) }>edit</button>
func SendTo(recv any, name string, args ...any) templ.ComponentScript {
	return send(namedMethod(recv, name), args, componentType(LiveKey(reflect.TypeOf(recv))))
}

// send renders an event; comp names the live component type it belongs
// to, "" for the page's.
func send(name string, args []any, comp string) templ.ComponentScript {
	b, err := json.Marshal(args)
	if err != nil {
		panic(fmt.Sprintf("view.Send(%s): arguments are not JSON-encodable: %v", name, err))
	}
	call := "__nx.live.send(this," + jsonString(name) + "," + string(b)
	if comp != "" {
		call += "," + jsonString(comp)
	}
	return templ.ComponentScript{Call: htmlAttr(call + ")")}
}

// methodRecv reads the receiver type of a method value from its name:
// pkg/path.(*T).Method-fm.
var methodRecv = regexp.MustCompile(`^(.*)\.\(\*?([A-Za-z_][A-Za-z0-9_]*)\)\.[A-Za-z_][A-Za-z0-9_]*-fm$`)

// receiverKey is a method value's receiver type as LiveKey names it:
// pkg/path.T — "" when it can't be read.
func receiverKey(method any) string {
	v := reflect.ValueOf(method)
	if v.Kind() != reflect.Func {
		return ""
	}
	m := methodRecv.FindStringSubmatch(runtime.FuncForPC(v.Pointer()).Name())
	if m == nil {
		return ""
	}
	return m[1] + "." + m[2]
}

// htmlAttr escapes a script for an attribute: templ writes Call into the
// attribute as is.
func htmlAttr(js string) string { return html.EscapeString(js) }

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// ScriptAttr is a live event (Send, Submit, Change) as attribute text, for
// an attribute map: templ renders a script only on an element's own on*
// attribute, so the generator rewrites templ.Attributes{"onclick":
// view.Send(…)} to use this.
func ScriptAttr(event templ.ComponentScript) string { return html.UnescapeString(event.Call) }

var methodValueName = regexp.MustCompile(`\.([A-Za-z_][A-Za-z0-9_]*)-fm$`)

// methodName reads the method's name from a method value (x.Cancel).
func methodName(method any) string { return methodOf(method).name }

// methodInfo is what a method value's function name says: the method, and
// its receiver type as LiveKey names it.
type methodInfo struct{ name, recv string }

// methods caches methodInfo by function: a page renders the same method
// values on every row, every render.
var methods sync.Map // uintptr → methodInfo

func methodOf(method any) methodInfo {
	v := reflect.ValueOf(method)
	if v.Kind() != reflect.Func {
		panic(fmt.Sprintf("view.Send: want a method value such as l.Cancel, got %T", method))
	}
	pc := v.Pointer()
	if mi, ok := methods.Load(pc); ok {
		return mi.(methodInfo)
	}
	mi := methodInfo{name: parseMethodName(pc), recv: receiverKey(method)}
	methods.Store(pc, mi)
	return mi
}

func parseMethodName(pc uintptr) string {
	full := runtime.FuncForPC(pc).Name()
	m := methodValueName.FindStringSubmatch(full)
	if m == nil {
		if strings.Contains(full, "[...]") {
			// Go builds a generic type's method values as closures, which
			// don't carry the method's name.
			panic(fmt.Sprintf("view.Send: %s is a method of a generic type, which view can't name — use view.SendTo(recv, \"Method\", …) (or SubmitTo, ChangeTo)", full))
		}
		panic(fmt.Sprintf("view.Send: %s is not a method value — pass l.Cancel, not a function", full))
	}
	return m[1]
}

// namedMethod checks that recv has an exported method called name.
func namedMethod(recv any, name string) string {
	if _, ok := reflect.TypeOf(recv).MethodByName(name); !ok || !isExported(name) {
		panic(fmt.Sprintf("view.SendTo: %T has no exported method %q", recv, name))
	}
	return name
}

func liveName(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	name := strings.TrimSuffix(t.Name(), "Live")
	if name == "" {
		name = t.Name()
	}
	return strings.ToLower(name[:1]) + name[1:]
}

var (
	ctxType     = reflect.TypeFor[context.Context]()
	errType     = reflect.TypeFor[error]()
	compType    = reflect.TypeFor[templ.Component]()
	httpCtxType = reflect.TypeFor[*httpx.Ctx]()
)

// liveMethod is Mount or an event: which parameters are dependencies, which
// are arguments.
type liveMethod struct {
	fn   reflect.Value // the method expression: receiver first
	deps []int         // parameter indexes filled from DI
	args []int         // parameter indexes filled from props or the event
}

type liveDef struct {
	t        reflect.Type // *T
	mount    *liveMethod
	update   *liveMethod
	props    reflect.Type // what Mount and Update take, or nil
	info     *liveMethod
	events   map[string]*liveMethod
	depTypes []reflect.Type // every dependency type any method takes
	depIndex map[reflect.Type]int
	routed   bool // view.Live gave it a path
}

func newLiveDef(t reflect.Type) (*liveDef, error) {
	if t.Kind() != reflect.Pointer || t.Elem().Kind() != reflect.Struct {
		return nil, fmt.Errorf("view.Live[%s]: want a pointer to a struct", t)
	}
	d := &liveDef{t: t, events: map[string]*liveMethod{}, depIndex: map[reflect.Type]int{}}
	render, ok := t.MethodByName("Render")
	if !ok || render.Type.NumIn() != 1 || render.Type.NumOut() != 1 || render.Type.Out(0) != compType {
		return nil, fmt.Errorf("view.Live[%s]: needs a Render() templ.Component method — a templ method component: templ (x %s) Render() { … }", t, t)
	}
	for i := 0; i < t.NumMethod(); i++ {
		m := t.Method(i)
		if m.Name == "Render" || promoted(t, m.Name) {
			continue
		}
		lm, ok := d.method(m)
		if !ok {
			switch m.Name {
			case "Mount", "Update", "Info":
				return nil, fmt.Errorf("view.Live[%s]: %s must take a context.Context first and return an error, got %s", t, m.Name, m.Type)
			}
			continue // not an event: a helper method
		}
		switch m.Name {
		case "Info":
			if len(lm.args) != 1 || m.Type.In(lm.args[0]) != reflect.TypeFor[Message]() {
				return nil, fmt.Errorf("view.Live[%s]: Info must be func(ctx context.Context, deps…, msg view.Message) error, got %s", t, m.Type)
			}
			d.info = lm
		case "Mount", "Update":
			if len(lm.args) > 1 {
				return nil, fmt.Errorf("view.Live[%s]: %s takes ctx, dependencies and at most one props struct, got %s", t, m.Name, m.Type)
			}
			if len(lm.args) == 1 {
				p := m.Type.In(lm.args[0])
				if p.Kind() != reflect.Struct {
					return nil, fmt.Errorf("view.Live[%s]: %s's props must be a struct value (pointers are dependencies), got %s", t, m.Name, p)
				}
				if d.props != nil && d.props != p {
					return nil, fmt.Errorf("view.Live[%s]: Mount and Update take different props (%s, %s)", t, d.props, p)
				}
				d.props = p
			}
			if m.Name == "Mount" {
				d.mount = lm
			} else {
				d.update = lm
				if len(lm.args) == 0 {
					return nil, fmt.Errorf("view.Live[%s]: Update takes the props, got %s", t, m.Type)
				}
			}
		default:
			d.events[m.Name] = lm
		}
	}
	if tr := trackAssigns(reflect.New(t.Elem())); len(tr.fields) > 0 && tr.off != "" && verifyTracking {
		log.Printf("nexus view: view.Live[%s] renders in full on every event: %s", t, tr.off)
	}
	return d, nil
}

// promoted reports whether t's method name comes from the embedded
// LiveView — a helper, not an event.
func promoted(t reflect.Type, name string) bool {
	_, ok := reflect.PointerTo(liveViewType).MethodByName(name)
	return ok
}

// method classifies m's parameters, or reports that m is not Mount or an
// event: those take a context first and return exactly an error.
func (d *liveDef) method(m reflect.Method) (*liveMethod, bool) {
	ft := m.Type // receiver first
	if ft.NumIn() < 2 || ft.In(1) != ctxType || ft.NumOut() != 1 || ft.Out(0) != errType {
		return nil, false
	}
	lm := &liveMethod{fn: m.Func}
	for i := 2; i < ft.NumIn(); i++ {
		p := ft.In(i)
		switch {
		case (p.Kind() == reflect.Pointer || p.Kind() == reflect.Interface) && len(lm.args) == 0:
			lm.deps = append(lm.deps, i)
			if _, seen := d.depIndex[p]; !seen {
				d.depIndex[p] = len(d.depTypes)
				d.depTypes = append(d.depTypes, p)
			}
		default:
			lm.args = append(lm.args, i)
		}
	}
	return lm, true
}

// instance is one page's or connection's copy of the live state.
type instance struct {
	def  *liveDef
	v    reflect.Value // *T
	deps []reflect.Value
	errs *nexus.Error // the validation errors of the last event
	tr   *tracker     // its Assigns (assign.go)

	lv    *LiveView             // what it embeds, if anything
	id    string                // its id where a page embeds it
	page  *instance             // the page that embeds it; nil for a page
	comps map[string]*component // the views the page embeds, by type and id
	gen   uint64                // its renders so far
	sock  *socket               // a page's connection
	nav   *string               // a page's: where an event under way moves the browser
}

// root is the page an instance belongs to: itself, or a component's page.
func (in *instance) root() *instance {
	if in.page != nil {
		return in.page
	}
	return in
}

func (d *liveDef) instance(template reflect.Value, deps []reflect.Value) *instance {
	v := reflect.New(d.t.Elem())
	if template.IsValid() && !template.IsNil() {
		v.Elem().Set(template.Elem())
	}
	in := &instance{def: d, v: v, deps: deps, tr: trackAssigns(v), sock: &socket{}}
	if in.lv = liveViewOf(v); in.lv != nil {
		*in.lv = LiveView{in: in, t: in.lv.t, bit: in.lv.bit}
	}
	return in
}

// setErrs records the validation errors of an event: Render's reads of
// view.Errors see the change.
func (in *instance) setErrs(e *nexus.Error) {
	if in.errs != e {
		in.errs = e
		in.tr.errsVer = in.tr.bump()
	}
}

// call runs m on the instance with raw argument values (strings for path
// parameters, JSON for event arguments).
func (in *instance) call(ctx context.Context, m *liveMethod, raw []json.RawMessage) error {
	ft := m.fn.Type()
	if len(raw) != len(m.args) {
		return nexus.Errf(nexus.InvalidInput, "takes %d argument(s), got %d", len(m.args), len(raw))
	}
	args := make([]reflect.Value, len(m.args))
	for n, i := range m.args {
		p := reflect.New(ft.In(i))
		if err := json.Unmarshal(raw[n], p.Interface()); err != nil {
			return nexus.Errf(nexus.InvalidInput, "argument %d: %v", n+1, err)
		}
		args[n] = p.Elem()
	}
	return in.callValues(ctx, m, args)
}

// callValues runs m with its arguments as Go values. A panic becomes an
// error, so a bug in one event does not drop the page's connection.
func (in *instance) callValues(ctx context.Context, m *liveMethod, args []reflect.Value) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	ft := m.fn.Type()
	ctx = withInstance(ctx, in.root())
	params := make([]reflect.Value, ft.NumIn())
	params[0] = in.v
	params[1] = reflect.ValueOf(&ctx).Elem()
	for _, i := range m.deps {
		params[i] = in.deps[in.def.depIndex[ft.In(i)]]
	}
	for n, i := range m.args {
		params[i] = args[n]
	}
	out := m.fn.Call(params)
	err, _ = out[0].Interface().(error)
	return err
}

// mount runs Mount with props (a zero Value when it takes none).
func (in *instance) mount(ctx context.Context, props reflect.Value) error {
	if in.def.mount == nil {
		return nil
	}
	var args []reflect.Value
	if len(in.def.mount.args) == 1 {
		args = []reflect.Value{props}
	}
	if err := in.callValues(ctx, in.def.mount, args); err != nil {
		return fmt.Errorf("Mount: %w", err)
	}
	return nil
}

// update runs Update with new props — or Mount again, without one.
func (in *instance) update(ctx context.Context, props reflect.Value) error {
	if in.def.update == nil {
		return in.mount(ctx, props)
	}
	if err := in.callValues(ctx, in.def.update, []reflect.Value{props}); err != nil {
		return fmt.Errorf("Update: %w", err)
	}
	return nil
}

// routedProps binds a routed view's props from its path parameters and URL.
func (in *instance) routedProps(param func(string) string, u *url.URL) (reflect.Value, error) {
	if in.def.props == nil {
		return reflect.Value{}, nil
	}
	return bindProps(in.def.props, param, u)
}

// render renders the instance: its Render() component into HTML and its
// render tree. The page's first render, over HTTP, is recorded too, so its
// markup is what the connection renders (stable ids, rendered.go). A panic
// becomes an error.
func (in *instance) render(ctx context.Context) (_ []byte, _ *rframe, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("Render panicked: %v", r)
		}
	}()
	comp, _ := in.v.MethodByName("Render").Call(nil)[0].Interface().(templ.Component)
	if comp == nil {
		return nil, nil, errors.New("Render returned nil")
	}
	var buf bytes.Buffer
	ctx = context.WithValue(ctx, formErrorsKey{}, in.errs)
	in.gen++
	ctx = withInstance(ctx, in)
	ctx, rec := withRecorder(ctx, &buf)
	if err := comp.Render(withRender(ctx, &render{}), rec.w); err != nil {
		rec.tree()
		return nil, nil, err
	}
	root := rec.tree()
	in.sweep(in.gen)
	return buf.Bytes(), root, nil
}

// renderTree renders the instance's tree for a connection. A tracked page
// records its spots, and skips those of prev — the spots of the tree the
// browser holds — whose Assigns didn't change, except the ones in again.
func (in *instance) renderTree(ctx context.Context, prev *spotTable, again map[uint64]bool) (body []byte, root *rframe, table *spotTable, err error) {
	defer func() {
		in.tr.rec = nil
		if r := recover(); r != nil {
			err = fmt.Errorf("Render panicked: %v", r)
		}
	}()
	comp, _ := in.v.MethodByName("Render").Call(nil)[0].Interface().(templ.Component)
	if comp == nil {
		return nil, nil, nil, errors.New("Render returned nil")
	}
	var buf bytes.Buffer
	ctx = context.WithValue(ctx, formErrorsKey{}, in.errs)
	in.gen++
	ctx = withInstance(ctx, in)
	ctx, rec := withRecorder(ctx, &buf)
	rec.gen = in.gen
	if in.tr.off == "" {
		table = &spotTable{owner: in.tr, epoch: in.tr.epoch, spots: map[uint64]*spot{}}
		rec.track, rec.table, rec.again = in.tr, table, again
		if prev != nil && prev.owner == in.tr {
			rec.prev, rec.changed = prev, in.tr.changed(prev.epoch)
		}
	}
	in.tr.rec = rec
	if err := comp.Render(withRender(ctx, &render{}), rec.w); err != nil {
		rec.tree()
		return nil, nil, nil, err
	}
	root = rec.tree()
	in.sweep(in.gen)
	return buf.Bytes(), root, table, nil
}

// diffRender renders the instance for the browser t serves and returns the
// reply's tree: the change from what the browser holds, or (full) the whole.
func (in *instance) diffRender(ctx context.Context, t *treeDiffer) (msg any, full bool, err error) {
	prev := t.spots
	if t.prev == nil {
		prev = nil
	}
	var again map[uint64]bool
	for pass := 0; ; pass++ {
		if pass == 3 {
			prev = nil // render everything
		}
		_, root, table, err := in.renderTree(ctx, prev, again)
		if err != nil {
			return nil, false, err
		}
		if table != nil && table.kept > 0 && verifyTracking {
			// Check the skipping render against a full one.
			_, whole, wholeTable, err := in.renderTree(ctx, nil, nil)
			if err != nil {
				return nil, false, err
			}
			if stale := staleSpots(root, whole); len(stale) > 0 {
				reportStale(in.def.t, stale)
				root, table = whole, wholeTable
			}
		}
		msg, full, missing := t.nextSpots(root, table)
		if missing == nil {
			in.flushStreams()
			return msg, full, nil
		}
		// The browser needs the markup of spots the render skipped.
		if again == nil {
			again = map[uint64]bool{}
		}
		for _, k := range missing {
			again[k] = true
		}
	}
}

// flushStreams lets the page's streams, and its components', go of the
// change a render sent.
func (in *instance) flushStreams() {
	for _, f := range in.tr.flushers {
		f.flush()
	}
	for _, c := range in.comps {
		for _, f := range c.in.tr.flushers {
			f.flush()
		}
	}
}

// setup is the view's setup in the app serving c.
func (d *liveDef) setup(c *httpx.Ctx) (*viewSetup, error) {
	return viewFor(appFrom(withApp(c.Request.Context(), c)), d.t)
}

var bodyTag = regexp.MustCompile(`(?i)<body\b`)

// pageHandler renders the page on first load: a fresh instance, mounted and
// rendered, marked as a live root so the browser connects its socket.
func (d *liveDef) pageHandler() any {
	return func(ctx context.Context, c *httpx.Ctx) (templ.Component, error) {
		vs, err := d.setup(c)
		if err != nil {
			return nil, err
		}
		in := d.instance(vs.template, vs.deps)
		props, err := in.routedProps(c.Param, c.Request.URL)
		if err != nil {
			return nil, err
		}
		if err := in.mount(ctx, props); err != nil {
			return nil, err
		}
		socket := strings.TrimSuffix(c.Request.URL.Path, "/") + "/_live"
		return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
			body, _, err := in.render(ctx)
			if err != nil {
				return err
			}
			attr := ` data-nx-live="` + html.EscapeString(socket) + `" data-nx-live-join="` + keepJoin(string(body)) + `"`
			if loc := bodyTag.FindIndex(body); loc != nil {
				// A full document: the <body> is the live root.
				_, err = w.Write(append(append(append([]byte{}, body[:loc[1]]...), attr...), body[loc[1]:]...))
				return err
			}
			if _, err := io.WriteString(w, "<nx-live"+attr+">"); err != nil {
				return err
			}
			if _, err := w.Write(body); err != nil {
				return err
			}
			_, err = io.WriteString(w, "</nx-live>")
			return err
		}), nil
	}
}

type liveEvent struct {
	Ref    int                 `json:"ref,omitempty"` // echoed in the event's reply
	Event  string              `json:"event"`
	C      string              `json:"c,omitempty"` // the live component it is for: type#id
	Args   []json.RawMessage   `json:"args,omitempty"`
	Form   map[string][]string `json:"form,omitempty"`   // a form event's fields
	Upload *uploadMsg          `json:"upload,omitempty"` // __upload, __cancel_upload
	URL    string              `json:"url,omitempty"`    // __nav: the URL the browser moves to
}

type liveReply struct {
	Ref     int    `json:"ref,omitempty"`   // the event this reply answers; 0 for a push
	Tree    any    `json:"tree,omitempty"`  // the render tree, or its change (rdiff.go)
	Full    bool   `json:"full,omitempty"`  // Tree is the whole tree
	Reset   bool   `json:"reset,omitempty"` // forget the statics held so far
	Error   string `json:"error,omitempty"`
	Invalid bool   `json:"invalid,omitempty"` // the event returned an InvalidInput error
	// Resume is the token a reconnect presents to carry on with this page's
	// state (resume.go); Resumed says a reconnect did.
	Resume  string `json:"resume,omitempty"`
	Resumed bool   `json:"resumed,omitempty"`
	// Navigation (navigate.go): Patch is the URL a patched page now shows;
	// Nav the URL of the page that took the connection, Live its socket;
	// Redirect a URL the browser loads itself.
	Patch    string `json:"patch,omitempty"`
	Nav      string `json:"nav,omitempty"`
	Live     string `json:"live,omitempty"`
	Redirect string `json:"redirect,omitempty"`
	// Uploads are the URLs the browser sends the files it offered to, by
	// their refs (upload.go).
	Uploads map[string]string `json:"uploads,omitempty"`
}

// upgraded is the renderer of the socket route: the handler already took
// over the connection, so there is nothing to write.
type upgraded struct{}

func (upgraded) Render(*httpx.Ctx, any) error { return nil }

// liveUpgrader compresses messages (permessage-deflate) when the browser
// offers it, as every browser does: a full render shrinks several times over.
// Its write buffers come from a pool, held only while a message is written:
// an idle page keeps none.
// Browsers send small messages, so the read buffer is small too.
var liveUpgrader = websocket.Upgrader{CheckOrigin: httpx.CheckWebSocketOrigin, EnableCompression: true,
	ReadBufferSize: 1024, WriteBufferPool: &sync.Pool{}}

// LiveIdleTrim is how long a live page may sit without events or pushes
// before the server lets go of the render tree it keeps for diffing; the
// next reply then carries the whole tree. An idle tab costs its connection
// and its state, not a copy of its page.
var LiveIdleTrim = 2 * time.Minute

const (
	liveWriteWait     = 10 * time.Second
	livePongWait      = 60 * time.Second
	defaultPingPeriod = 50 * time.Second
)

// socketHandler serves the live connection: mount a fresh instance, send
// its render, then for each event call the method and send the new render.
func (d *liveDef) socketHandler() any {
	return func(ctx context.Context, c *httpx.Ctx) (any, error) {
		// A non-nil result, so nexus hands it to the upgraded renderer (which
		// writes nothing) rather than writing an empty response itself.
		done := struct{}{}
		vs, err := d.setup(c)
		if err != nil {
			return nil, err
		}
		owner, _ := nexus.RequestIdentity(ctx)
		rq := d.request(c)
		live := context.WithValue(withApp(ctx, c), socketKey{}, true)
		if lc, handed := ctx.Value(liveConnKey{}).(*liveConn); handed {
			// Another page hands its connection over (navigate.go): it runs
			// on the connection's goroutine once this request is answered.
			in := d.instance(vs.template, vs.deps)
			lc.pending = func(base context.Context) {
				d.serve(detached(base, live), rq, lc, in, false, owner)
			}
			return done, nil
		}
		conn, err := liveUpgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			return done, nil // Upgrade has answered the request
		}
		lc := newLiveConn(conn, c.Request)
		var in *instance
		resumed := false
		if p := unpark(rq.url.Query().Get("resume"), rq.path, owner); p != nil {
			in, resumed = p.in, true
		} else {
			in = d.instance(vs.template, vs.deps)
		}
		// The page runs on a goroutine of its own and this request returns:
		// the connection doesn't keep the request's deep stack and buffers
		// for its whole life. The app's stop closes it (track).
		app := appFrom(live)
		base, stop := context.WithCancel(context.WithoutCancel(live))
		untrack := track(app, conn, stop)
		go func() {
			defer untrack()
			defer stop()
			defer conn.Close()
			d.serve(base, rq, lc, in, resumed, owner)
			// Each page that navigates away hands the connection to the next.
			for lc.next != "" && base.Err() == nil {
				target := lc.next
				lc.next = ""
				if !lc.handoff(base, app, target) {
					b, _ := marshal(liveReply{Redirect: target})
					_ = conn.SetWriteDeadline(time.Now().Add(liveWriteWait))
					_ = conn.WriteMessage(websocket.TextMessage, b)
					return
				}
				run := lc.pending
				lc.pending = nil
				run(base)
			}
		}()
		return done, nil
	}
}

// liveRequest is what a page's socket needs from its request once the
// request has been answered.
type liveRequest struct {
	path   string            // the socket's path
	url    *url.URL          // the socket's URL (join, resume, url)
	params map[string]string // the route's path parameters
}

func (d *liveDef) request(c *httpx.Ctx) liveRequest {
	u := *c.Request.URL
	rq := liveRequest{path: c.Request.URL.Path, url: &u, params: map[string]string{}}
	for _, name := range pathParams(c.FullPath()) {
		rq.params[name] = c.Param(name)
	}
	return rq
}

var pathParam = regexp.MustCompile(`[:*]([A-Za-z_][A-Za-z0-9_]*)`)

// pathParams are the parameter names of a route pattern.
func pathParams(route string) []string {
	var out []string
	for _, m := range pathParam.FindAllStringSubmatch(route, -1) {
		out = append(out, m[1])
	}
	return out
}

// detached is a handed-over page's context: the values of its own request,
// cancelled with the connection.
func detached(base, page context.Context) context.Context {
	ctx, cancel := context.WithCancel(context.WithoutCancel(page))
	context.AfterFunc(base, cancel)
	return ctx
}

func (d *liveDef) serve(ctx context.Context, rq liveRequest, lc *liveConn, in *instance, resumed bool, owner string) {
	conn := lc.conn
	path := rq.path
	page := strings.TrimSuffix(path, "/_live")
	navigated := lc.target
	lc.target = ""
	active := time.Now()
	render := func(ref int, invalid bool) liveReply {
		msg, full, err := in.diffRender(ctx, &lc.tree)
		if err != nil {
			return liveReply{Ref: ref, Error: err.Error()}
		}
		reply := liveReply{Ref: ref, Tree: msg, Full: full, Invalid: invalid}
		if lc.trimmed {
			reply.Reset, lc.trimmed = true, false
		}
		active = time.Now()
		return reply
	}
	send := func(r liveReply) bool {
		b, err := marshal(r)
		if err != nil {
			return false
		}
		_ = conn.SetWriteDeadline(time.Now().Add(liveWriteWait))
		return conn.WriteMessage(websocket.TextMessage, b) == nil
	}
	sendRender := func() bool { return send(render(0, false)) }
	if !resumed {
		in.sock = &socket{connected: true, inbox: make(chan Message, liveInbox)}
	}
	sock := in.sock
	if sock.wake == nil {
		sock.wake = make(chan bool, 1)
	}
	param := func(name string) string { return rq.params[name] }
	// A connection that ends while the page is open leaves its state for a
	// reconnect (resume.go); one the server ends (a refused credential, a
	// failed Mount, shutdown) or that moves to another page takes it along.
	token := newResumeToken()
	keep := false
	defer func() {
		if keep && ctx.Err() == nil {
			park(token, &parkedPage{in: in, path: path, owner: owner})
			return
		}
		sock.close()
	}()
	if resumed {
		lc.tree.reset()
		reply := render(0, in.errs != nil)
		reply.Resume, reply.Resumed, reply.Reset = token, true, true
		if !send(reply) {
			keep = true
			return
		}
	} else {
		shown := pageURL(page, rq.url.Query().Get("url"))
		if navigated != "" {
			shown = pageURL(page, navigated)
		}
		props, err := in.routedProps(param, shown)
		if err != nil {
			send(liveReply{Error: nexus.ErrorOf(err).Error()})
			return
		}
		if err := in.mount(ctx, props); err != nil {
			send(liveReply{Error: err.Error()})
			return
		}
		body, root, table, err := in.renderTree(ctx, nil, nil)
		if err != nil {
			send(liveReply{Error: err.Error()})
			return
		}
		reply := liveReply{Resume: token}
		if navigated != "" {
			// A page another one handed over to: the change from the page the
			// browser holds — two pages of one layout share its frames — or,
			// when their roots differ, its tree against the statics the
			// connection already holds.
			msg, full, _ := lc.tree.nextSpots(root, table)
			reply.Tree, reply.Full, reply.Nav, reply.Live = msg, full, shown.RequestURI(), path
		} else {
			// The page arrived over HTTP: when this connection renders the
			// same, send no markup — the first change carries the tree the
			// browser will patch. Otherwise (Mount did more once connected, or
			// there is no HTTP render to compare with: a reconnect) send the
			// tree now.
			lc.tree.reset()
			if joined, ok := takeJoin(rq.url.Query().Get("join")); !ok || joined != string(body) {
				msg, _, _ := lc.tree.nextSpots(root, table)
				reply.Tree, reply.Full, reply.Reset = msg, true, true
			}
		}
		if !send(reply) {
			return
		}
		in.flushStreams()
	}
	keep = true

	// nav moves the browser to target: a patch of this page, or a hand-off
	// to another (serve returns, and the connection's owner opens it).
	nav := func(ref int, target string) (moved, ok bool) {
		u, err := url.Parse(target)
		if err != nil {
			return false, send(liveReply{Ref: ref, Error: "navigate: " + err.Error()})
		}
		if u.IsAbs() || strings.TrimSuffix(u.Path, "/") != strings.TrimSuffix(page, "/") {
			lc.next, keep = target, false
			return true, true
		}
		props, err := in.routedProps(param, u)
		if err == nil {
			err = in.update(ctx, props)
		}
		if err != nil {
			return false, send(liveReply{Ref: ref, Error: nexus.ErrorOf(err).Error()})
		}
		reply := render(ref, in.errs != nil)
		reply.Patch = u.RequestURI()
		return false, send(reply)
	}

	_ = conn.SetReadDeadline(time.Now().Add(livePongWait))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(livePongWait)) })
	pingEvery, idleTrim := liveTimings()
	ping := time.NewTicker(pingEvery)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			_ = conn.SetWriteDeadline(time.Now().Add(liveWriteWait))
			if conn.WriteMessage(websocket.PingMessage, nil) != nil {
				return
			}
			if lc.tree.prev != nil && time.Since(active) > idleTrim {
				lc.tree.reset()
				lc.trimmed = true
			}
		case msg := <-sock.inbox:
			// Handle every message already waiting, then render once.
			failed := in.inform(ctx, msg, send)
			for drained := false; !drained && !failed; {
				select {
				case more := <-sock.inbox:
					failed = in.inform(ctx, more, send)
				default:
					drained = true
				}
			}
			if failed || !sendRender() {
				return
			}
		case <-sock.wake:
			// An upload's request changed the page: show its progress.
			if in.syncUploads() && !sendRender() {
				return
			}
		case ev, open := <-lc.events:
			if !open {
				return
			}
			if err := nexus.CheckConnection(ctx); err != nil {
				// The page's credential no longer holds: answer, then close.
				keep = false
				send(liveReply{Ref: ev.Ref, Error: nexus.ErrorOf(err).Error()})
				return
			}
			switch ev.Event {
			case "__resync": // the browser lost track: send the whole tree
				lc.tree.reset()
				reply := render(ev.Ref, in.errs != nil)
				reply.Reset = true
				if !send(reply) {
					return
				}
				continue
			case "__nav":
				if moved, ok := nav(ev.Ref, ev.URL); moved || !ok {
					return
				}
				continue
			case "__upload", "__cancel_upload":
				if !send(in.upload(ev, owner, page, render)) {
					return
				}
				continue
			}
			var target string
			in.nav = &target
			reply := d.observedEvent(ctx, in, ev, render)
			in.nav = nil
			if !send(reply) {
				return
			}
			if target != "" {
				if moved, ok := nav(0, target); moved || !ok {
					return
				}
			}
		}
	}
}

// event runs one browser event and returns its reply: the new render, or
// an error. An InvalidInput error (nexus.Invalid()) from the method is not a failure: the page
// re-renders with it (view.Errors) and the reply is marked invalid.
func (d *liveDef) event(ctx context.Context, in *instance, ev liveEvent, render func(int, bool) liveReply) liveReply {
	page := in
	page.clearFlash()
	if ev.C != "" {
		// A live component's event: its own instance takes it.
		c := in.comps[ev.C]
		if c == nil {
			return liveReply{Ref: ev.Ref, Error: fmt.Sprintf("no live component %q on the page", ev.C)}
		}
		d, in = c.setup.def, c.in
	}
	m, known := d.events[ev.Event]
	if !known || !isExported(ev.Event) {
		return liveReply{Ref: ev.Ref, Error: fmt.Sprintf("no event %q", ev.Event)}
	}
	var err error
	if ev.Form != nil {
		if len(m.args) != 1 {
			return liveReply{Ref: ev.Ref, Error: fmt.Sprintf("%s: a form event takes one argument, the form struct", ev.Event)}
		}
		arg, berr := bindForm(m.fn.Type().In(m.args[0]), ev.Form)
		if berr != nil {
			return liveReply{Ref: ev.Ref, Error: fmt.Sprintf("%s: %v", ev.Event, berr)}
		}
		// The form's validate: tags run before the event, as on every
		// transport; a failure re-renders like an Invalid() the event returns.
		if err = nexus.Validate(arg.Interface()); err == nil {
			err = in.callValues(ctx, m, []reflect.Value{arg})
		}
	} else {
		err = in.call(ctx, m, ev.Args)
	}
	if errs, ok := validation(err); ok {
		page.setErrs(errs)
		return render(ev.Ref, true)
	}
	if err != nil {
		ne := nexus.ErrorOf(err)
		if ne.Code == nexus.Forbidden {
			log.Printf("view: live %s: event %s refused: %v", d.t, ev.Event, err)
		}
		return liveReply{Ref: ev.Ref, Error: fmt.Sprintf("%s: %v", ev.Event, ne)}
	}
	page.setErrs(nil)
	return render(ev.Ref, false)
}

// inform runs the Info of each view subscribed to msg's topic; it reports
// whether the connection failed. An Info error is sent to the page, which
// keeps its state.
func (in *instance) inform(ctx context.Context, msg Message, send func(liveReply) bool) bool {
	for _, v := range slices.Clone(in.sock.subs[msg.Topic]) {
		if v.def.info == nil {
			continue
		}
		if err := v.callValues(ctx, v.def.info, []reflect.Value{reflect.ValueOf(msg)}); err != nil {
			if !send(liveReply{Error: "Info: " + err.Error()}) {
				return true
			}
		}
	}
	return false
}

// clearFlash drops the flash messages of the page and the views it embeds:
// an event starts.
func (in *instance) clearFlash() {
	if in.lv != nil {
		in.lv.clearFlash()
	}
	for _, c := range in.comps {
		if c.in.lv != nil {
			c.in.lv.clearFlash()
		}
	}
}

func isExported(name string) bool {
	for _, r := range name {
		return unicode.IsUpper(r)
	}
	return false
}
