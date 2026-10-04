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
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/a-h/templ"
	"github.com/gorilla/websocket"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/httpx"
)

// LiveRouter is a live page: a component whose state lives on the server, per
// connected page, and changes through events sent over a WebSocket.
//
//	var Module = nexus.Module("orders",
//	    nexus.Path("/admin"),
//	    view.Live[*OrdersLive]("/orders", auth.Requires("view_orders")).
//	        Provide(NewOrdersLive),
//	)
//
// T is a pointer to a struct, provided with DI. The DI instance is only the
// template: each page render and each connection gets its own copy, filled by
// Mount. T's methods follow conventions, like a Resource's:
//
//	Mount(ctx, [*view.Socket], deps…, pathParams…) error fills the state
//	Render() templ.Component                              renders it (a templ method component)
//	Params(ctx, deps…, u *url.URL) error                  optional: runs after Mount and on each patch (the page's URL)
//	Info(ctx, deps…, msg view.Message) error              optional: runs on a broadcast to a topic Mount subscribed to
//	Cancel(ctx, deps…, args…) error                       an event: any other exported method of this shape
//
// Dependencies are pointer or interface parameters, injected from DI; the
// other parameters are a path parameter (Mount, in route order) or an event's
// arguments (from view.Send, in order). Markup sends an event with
//
//	<button onclick={ view.Send(l.Cancel, o.ID) }>cancel</button>
//
// after which the server re-renders and the browser patches the page in place.
// Event arguments are user input, and an event is reachable by any client that
// can open the page: authorize and validate inside it.
type LiveRouter[T any] struct {
	*nexus.Router
}

// Live declares a live page at prefix, gated by gates — on the page and on its
// socket. It is a nexus router, so it composes like one: inside nexus.Module
// it mounts under the module's Path.
func Live[T any](prefix string, gates ...nexus.MiddlewareOption) *LiveRouter[T] {
	t := reflect.TypeFor[T]()
	r := nexus.NewRouter(liveName(t), prefix, gates...)
	l := &LiveRouter[T]{Router: r}
	def, err := newLiveDef(t, prefix)
	if err != nil {
		r.Register(nexus.FailBoot(err))
		return l
	}
	r.Rest("GET", "", def.pageHandler(), append([]nexus.RestOption{HTML(), nexus.Tag(LiveTag, LiveKey(t))}, def.liveTags()...)...)
	r.Rest("GET", "/_live", def.socketHandler(), nexus.WithRenderer(upgraded{}), nexus.HideFromDashboard())
	return l
}

// LiveTag is the registry.Endpoint.Tags key on a live page's GET route; its
// value is LiveKey of the page's type. It is how tools (viewtest.Mount) find
// where a live page is served.
const LiveTag = "view.live"

// LiveKey names a live page's type T (a pointer to a struct) in LiveTag:
// its package path and type name.
func LiveKey(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.PkgPath() + "." + t.Name()
}

// Provide adds constructors — usually T's — under the live page's module.
func (l *LiveRouter[T]) Provide(fns ...any) *LiveRouter[T] {
	l.Router.Provide(fns...)
	return l
}

// Socket is what Mount can take to learn about the page it fills.
type Socket struct {
	connected bool
	inbox     chan Message
	topics    []string
}

// Connected reports whether this Mount is for the live connection (true) or
// the first, server-rendered page load (false) — skip expensive work, or
// start subscriptions, only when connected.
func (s *Socket) Connected() bool { return s != nil && s.connected }

// Send is an event for an on* attribute: it calls method — a method value of
// the live page, such as l.Cancel — on the server with args.
//
//	<button onclick={ view.Send(l.Cancel, o.ID) }>cancel</button>
//
// args are encoded when the page renders; the server decodes them into the
// method's argument parameters.
func Send(method any, args ...any) templ.ComponentScript {
	return send(methodName(method), args)
}

// SendTo is Send for the method named name on recv, for a live page whose
// type is generic: Go builds a generic type's method values as closures that
// don't carry the method's name, so Send can't read it. It panics, as Send
// does, when recv has no such method.
//
//	<button onclick={ view.SendTo(p, "Edit", row.ID) }>edit</button>
func SendTo(recv any, name string, args ...any) templ.ComponentScript {
	return send(namedMethod(recv, name), args)
}

func send(name string, args []any) templ.ComponentScript {
	b, err := json.Marshal(args)
	if err != nil {
		panic(fmt.Sprintf("view.Send(%s): arguments are not JSON-encodable: %v", name, err))
	}
	return templ.ComponentScript{Call: htmlAttr("__nx.live.send(this," + jsonString(name) + "," + string(b) + ")")}
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
func methodName(method any) string {
	v := reflect.ValueOf(method)
	if v.Kind() != reflect.Func {
		panic(fmt.Sprintf("view.Send: want a method value such as l.Cancel, got %T", method))
	}
	full := runtime.FuncForPC(v.Pointer()).Name()
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
	liveType    = reflect.TypeFor[*Socket]()
	compType    = reflect.TypeFor[templ.Component]()
	httpCtxType = reflect.TypeFor[*httpx.Ctx]()
	urlType     = reflect.TypeFor[*url.URL]()
)

// liveMethod is Mount or an event: which parameters are dependencies, which
// are arguments.
type liveMethod struct {
	fn   reflect.Value // the method expression: receiver first
	deps []int         // parameter indexes filled from DI
	args []int         // parameter indexes filled from the path or the event
	live int           // index of a *view.Socket parameter, or -1
}

type liveDef struct {
	t        reflect.Type // *T
	params   []string     // the prefix's path parameter names, in order
	mount    *liveMethod
	info     *liveMethod
	urlParam *liveMethod // Params: the page's URL
	events   map[string]*liveMethod
	depTypes []reflect.Type // every dependency type any method takes
	depIndex map[reflect.Type]int
}

var pathParam = regexp.MustCompile(`[:*]([A-Za-z_][A-Za-z0-9_]*)`)

func newLiveDef(t reflect.Type, prefix string) (*liveDef, error) {
	if t.Kind() != reflect.Pointer || t.Elem().Kind() != reflect.Struct {
		return nil, fmt.Errorf("view.Live[%s]: want a pointer to a struct", t)
	}
	d := &liveDef{t: t, events: map[string]*liveMethod{}, depIndex: map[reflect.Type]int{}}
	for _, m := range pathParam.FindAllStringSubmatch(prefix, -1) {
		d.params = append(d.params, m[1])
	}
	render, ok := t.MethodByName("Render")
	if !ok || render.Type.NumIn() != 1 || render.Type.NumOut() != 1 || render.Type.Out(0) != compType {
		return nil, fmt.Errorf("view.Live[%s]: needs a Render() templ.Component method — a templ method component: templ (x %s) Render() { … }", t, t)
	}
	for i := 0; i < t.NumMethod(); i++ {
		m := t.Method(i)
		if m.Name == "Render" {
			continue
		}
		lm, ok := d.method(m)
		if !ok {
			if m.Name == "Mount" {
				return nil, fmt.Errorf("view.Live[%s]: Mount must be func(ctx context.Context, [*view.Socket,] deps…, pathParams…) error, got %s", t, m.Type)
			}
			continue // not an event: a helper method
		}
		if m.Name == "Params" {
			if len(lm.args) != 1 || m.Type.In(lm.args[0]) != urlType {
				return nil, fmt.Errorf("view.Live[%s]: Params must be func(ctx context.Context, deps…, u *url.URL) error, got %s", t, m.Type)
			}
			d.urlParam = lm
			continue
		}
		if m.Name == "Info" {
			if len(lm.args) != 1 || m.Type.In(lm.args[0]) != reflect.TypeFor[Message]() {
				return nil, fmt.Errorf("view.Live[%s]: Info must be func(ctx context.Context, deps…, msg view.Message) error, got %s", t, m.Type)
			}
			d.info = lm
			continue
		}
		if m.Name == "Mount" {
			if len(lm.args) != len(d.params) {
				return nil, fmt.Errorf("view.Live[%s]: Mount takes %d path parameter(s), the prefix %q has %d", t, len(lm.args), prefix, len(d.params))
			}
			d.mount = lm
			continue
		}
		d.events[m.Name] = lm
	}
	return d, nil
}

// method classifies m's parameters, or reports that m is not Mount or an
// event: those take a context first and return exactly an error.
func (d *liveDef) method(m reflect.Method) (*liveMethod, bool) {
	ft := m.Type // receiver first
	if ft.NumIn() < 2 || ft.In(1) != ctxType || ft.NumOut() != 1 || ft.Out(0) != errType {
		return nil, false
	}
	lm := &liveMethod{fn: m.Func, live: -1}
	for i := 2; i < ft.NumIn(); i++ {
		p := ft.In(i)
		switch {
		case p == liveType:
			lm.live = i
		case p == urlType:
			lm.args = append(lm.args, i) // Params' page URL: not a dependency
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
}

func (d *liveDef) instance(template reflect.Value, deps []reflect.Value) *instance {
	v := reflect.New(d.t.Elem())
	if !template.IsNil() {
		v.Elem().Set(template.Elem())
	}
	return &instance{def: d, v: v, deps: deps}
}

// call runs m on the instance with raw argument values (strings for path
// parameters, JSON for event arguments).
func (in *instance) call(ctx context.Context, m *liveMethod, live *Socket, raw []json.RawMessage) error {
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
	return in.callValues(ctx, m, live, args)
}

// callValues runs m with its arguments as Go values. A panic becomes an
// error, so a bug in one event does not drop the page's connection.
func (in *instance) callValues(ctx context.Context, m *liveMethod, live *Socket, args []reflect.Value) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	ft := m.fn.Type()
	params := make([]reflect.Value, ft.NumIn())
	params[0] = in.v
	params[1] = reflect.ValueOf(&ctx).Elem()
	if m.live >= 0 {
		params[m.live] = reflect.ValueOf(live)
	}
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

func (in *instance) mount(ctx context.Context, param func(string) string, sock *Socket) error {
	if in.def.mount == nil {
		return nil
	}
	raw := make([]json.RawMessage, len(in.def.params))
	for i, name := range in.def.params {
		v := param(name)
		// A path parameter is a string on the wire: quote it unless the
		// parameter decodes from a number or a bool.
		switch in.def.mount.fn.Type().In(in.def.mount.args[i]).Kind() {
		case reflect.String:
			b, _ := json.Marshal(v)
			raw[i] = b
		default:
			raw[i] = json.RawMessage(v)
		}
	}
	if err := in.call(ctx, in.def.mount, sock, raw); err != nil {
		return fmt.Errorf("Mount: %w", err)
	}
	return nil
}

// params runs Params with the page's URL, when the page has it.
func (in *instance) params(ctx context.Context, sock *Socket, u *url.URL) error {
	if in.def.urlParam == nil {
		return nil
	}
	if err := in.callValues(ctx, in.def.urlParam, sock, []reflect.Value{reflect.ValueOf(u)}); err != nil {
		return fmt.Errorf("Params: %w", err)
	}
	return nil
}

// render renders the instance: its Render() component into HTML, and — for
// the live connection (tree true) — into its render tree. A panic becomes an
// error.
func (in *instance) render(ctx context.Context, tree bool) (_ []byte, _ *rframe, err error) {
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
	if !tree {
		if err := comp.Render(withRender(ctx, &render{}), &buf); err != nil {
			return nil, nil, err
		}
		return buf.Bytes(), nil, nil
	}
	ctx, rec := withRecorder(ctx, &buf)
	if err := comp.Render(withRender(ctx, &render{}), rec.w); err != nil {
		rec.tree()
		return nil, nil, err
	}
	root := rec.tree()
	return buf.Bytes(), root, nil
}

// handlerType builds func(ctx, *httpx.Ctx, T, deps…) (out, error) — a
// reflective nexus handler, so DI supplies the template and the dependencies
// and the router's gates run first.
func (d *liveDef) handlerType(out reflect.Type) reflect.Type {
	in := append([]reflect.Type{ctxType, httpCtxType, d.t}, d.depTypes...)
	return reflect.FuncOf(in, []reflect.Type{out, errType}, false)
}

var bodyTag = regexp.MustCompile(`(?i)<body\b`)

// pageHandler renders the page on first load: a fresh instance, mounted and
// rendered, marked as a live root so the browser connects its socket.
func (d *liveDef) pageHandler() any {
	return reflect.MakeFunc(d.handlerType(compType), func(args []reflect.Value) []reflect.Value {
		fail := func(err error) []reflect.Value {
			return []reflect.Value{reflect.Zero(compType), reflect.ValueOf(&err).Elem()}
		}
		ctx := args[0].Interface().(context.Context)
		c := args[1].Interface().(*httpx.Ctx)
		in := d.instance(args[2], args[3:])
		if err := in.mount(ctx, c.Param, &Socket{}); err != nil {
			return fail(err)
		}
		if err := in.params(ctx, &Socket{}, c.Request.URL); err != nil {
			return fail(err)
		}
		socket := strings.TrimSuffix(c.Request.URL.Path, "/") + "/_live"
		comp := templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
			body, _, err := in.render(ctx, false)
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
		})
		return []reflect.Value{reflect.ValueOf(&comp).Elem(), reflect.Zero(errType)}
	}).Interface()
}

type liveEvent struct {
	Ref   int                 `json:"ref,omitempty"` // echoed in the event's reply
	Event string              `json:"event"`
	Args  []json.RawMessage   `json:"args,omitempty"`
	Form  map[string][]string `json:"form,omitempty"` // a form event's fields
	URL   string              `json:"url,omitempty"`  // __nav: the URL the browser moves to
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
	return reflect.MakeFunc(d.handlerType(reflect.TypeFor[any]()), func(args []reflect.Value) []reflect.Value {
		// A non-nil result, so nexus hands it to the upgraded renderer (which
		// writes nothing) rather than writing an empty response itself.
		done := reflect.New(reflect.TypeFor[any]()).Elem()
		done.Set(reflect.ValueOf(struct{}{}))
		ok := []reflect.Value{done, reflect.Zero(errType)}
		ctx := args[0].Interface().(context.Context)
		c := args[1].Interface().(*httpx.Ctx)
		owner, _ := nexus.RequestIdentity(ctx)
		rq := d.request(c)
		live := context.WithValue(withApp(ctx, c), socketKey{}, true)
		if lc, handed := ctx.Value(liveConnKey{}).(*liveConn); handed {
			// Another page hands its connection over (navigate.go): it runs
			// on the connection's goroutine once this request is answered.
			in := d.instance(args[2], args[3:])
			lc.pending = func(base context.Context) {
				d.serve(detached(base, live), rq, lc, in, nil, owner)
			}
			return ok
		}
		conn, err := liveUpgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			return ok // Upgrade has answered the request
		}
		lc := newLiveConn(conn, c.Request)
		var in *instance
		var sock *Socket
		if p := unpark(rq.url.Query().Get("resume"), rq.path, owner); p != nil {
			in, sock = p.in, p.sock
		} else {
			in = d.instance(args[2], args[3:])
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
			d.serve(base, rq, lc, in, sock, owner)
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
		return ok
	}).Interface()
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
	for _, name := range d.params {
		rq.params[name] = c.Param(name)
	}
	return rq
}

// detached is a handed-over page's context: the values of its own request,
// cancelled with the connection.
func detached(base, page context.Context) context.Context {
	ctx, cancel := context.WithCancel(context.WithoutCancel(page))
	context.AfterFunc(base, cancel)
	return ctx
}

func (d *liveDef) serve(ctx context.Context, rq liveRequest, lc *liveConn, in *instance, sock *Socket, owner string) {
	conn := lc.conn
	path := rq.path
	page := strings.TrimSuffix(path, "/_live")
	navigated := lc.target
	lc.target = ""
	active := time.Now()
	render := func(ref int, invalid bool) liveReply {
		_, root, err := in.render(ctx, true)
		if err != nil {
			return liveReply{Ref: ref, Error: err.Error()}
		}
		msg, full := lc.tree.next(root)
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
	resumed := sock != nil
	if !resumed {
		sock = &Socket{connected: true, inbox: make(chan Message, liveInbox)}
	}
	// A connection that ends while the page is open leaves its state for a
	// reconnect (resume.go); one the server ends (a refused credential, a
	// failed Mount, shutdown) or that moves to another page takes it along.
	token := newResumeToken()
	keep := false
	defer func() {
		if keep && ctx.Err() == nil {
			park(token, &parkedPage{in: in, sock: sock, path: path, owner: owner})
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
		if err := in.mount(ctx, func(name string) string { return rq.params[name] }, sock); err != nil {
			send(liveReply{Error: err.Error()})
			return
		}
		shown := pageURL(page, rq.url.Query().Get("url"))
		if navigated != "" {
			shown = pageURL(page, navigated)
		}
		if err := in.params(ctx, sock, shown); err != nil {
			send(liveReply{Error: err.Error()})
			return
		}
		body, root, err := in.render(ctx, true)
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
			msg, full := lc.tree.next(root)
			reply.Tree, reply.Full, reply.Nav, reply.Live = msg, full, shown.RequestURI(), path
		} else {
			// The page arrived over HTTP: when this connection renders the
			// same, send no markup — the first change carries the tree the
			// browser will patch. Otherwise (Mount did more once connected, or
			// there is no HTTP render to compare with: a reconnect) send the
			// tree now.
			lc.tree.reset()
			if joined, ok := takeJoin(rq.url.Query().Get("join")); !ok || joined != string(body) {
				msg, _ := lc.tree.next(root)
				reply.Tree, reply.Full, reply.Reset = msg, true, true
			}
		}
		if !send(reply) {
			return
		}
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
		if err := in.params(ctx, sock, u); err != nil {
			return false, send(liveReply{Ref: ref, Error: err.Error()})
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
			failed := d.inform(ctx, in, sock, msg, send)
			for drained := false; !drained && !failed; {
				select {
				case more := <-sock.inbox:
					failed = d.inform(ctx, in, sock, more, send)
				default:
					drained = true
				}
			}
			if failed || !sendRender() {
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
			}
			var target string
			if !send(d.observedEvent(context.WithValue(ctx, navKey{}, &target), in, ev, render)) {
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
			err = in.callValues(ctx, m, nil, []reflect.Value{arg})
		}
	} else {
		err = in.call(ctx, m, nil, ev.Args)
	}
	if errs, ok := validation(err); ok {
		in.errs = errs
		return render(ev.Ref, true)
	}
	if err != nil {
		ne := nexus.ErrorOf(err)
		if ne.Code == nexus.Forbidden {
			log.Printf("view: live %s: event %s refused: %v", d.t, ev.Event, err)
		}
		return liveReply{Ref: ev.Ref, Error: fmt.Sprintf("%s: %v", ev.Event, ne)}
	}
	in.errs = nil
	return render(ev.Ref, false)
}

// inform runs Info for msg; it reports whether the connection failed. An
// Info error is sent to the page, which keeps its state.
func (d *liveDef) inform(ctx context.Context, in *instance, sock *Socket, msg Message, send func(liveReply) bool) bool {
	if d.info == nil {
		return false
	}
	if err := in.callValues(ctx, d.info, sock, []reflect.Value{reflect.ValueOf(msg)}); err != nil {
		return !send(liveReply{Error: "Info: " + err.Error()})
	}
	return false
}

func isExported(name string) bool {
	for _, r := range name {
		return unicode.IsUpper(r)
	}
	return false
}
