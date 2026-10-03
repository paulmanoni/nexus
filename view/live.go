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
	"reflect"
	"regexp"
	"runtime"
	"strings"
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
		r.Register(nexus.Error(err))
		return l
	}
	r.Rest("GET", "", def.pageHandler(), HTML())
	r.Rest("GET", "/_live", def.socketHandler(), nexus.WithRenderer(upgraded{}), nexus.HideFromDashboard())
	return l
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
	errs *nexus.Errors // the validation errors of the last event
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
		return fmt.Errorf("takes %d argument(s), got %d", len(m.args), len(raw))
	}
	args := make([]reflect.Value, len(m.args))
	for n, i := range m.args {
		p := reflect.New(ft.In(i))
		if err := json.Unmarshal(raw[n], p.Interface()); err != nil {
			return fmt.Errorf("argument %d: %v", n+1, err)
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

func (in *instance) mount(ctx context.Context, c *httpx.Ctx, sock *Socket) error {
	if in.def.mount == nil {
		return nil
	}
	raw := make([]json.RawMessage, len(in.def.params))
	for i, name := range in.def.params {
		v := c.Param(name)
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

// render renders the instance: its Render() component into HTML. A panic
// becomes an error.
func (in *instance) render(ctx context.Context) (_ []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("Render panicked: %v", r)
		}
	}()
	comp, _ := in.v.MethodByName("Render").Call(nil)[0].Interface().(templ.Component)
	if comp == nil {
		return nil, errors.New("Render returned nil")
	}
	var buf bytes.Buffer
	ctx = context.WithValue(ctx, formErrorsKey{}, in.errs)
	if err := comp.Render(withRender(ctx, &render{}), &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
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
		if err := in.mount(ctx, c, &Socket{}); err != nil {
			return fail(err)
		}
		socket := strings.TrimSuffix(c.Request.URL.Path, "/") + "/_live"
		comp := templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
			body, err := in.render(ctx)
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
}

type liveReply struct {
	Ref     int    `json:"ref,omitempty"` // the event this reply answers; 0 for a push
	HTML    string `json:"html,omitempty"`
	Patch   []any  `json:"patch,omitempty"` // or the change from the previous render (diff.go)
	N       int    `json:"n,omitempty"`     // the patched render's token count, to check it
	Error   string `json:"error,omitempty"`
	Invalid bool   `json:"invalid,omitempty"` // the event returned nexus.Errors
}

// upgraded is the renderer of the socket route: the handler already took
// over the connection, so there is nothing to write.
type upgraded struct{}

func (upgraded) Render(*httpx.Ctx, any) error { return nil }

// liveUpgrader compresses messages (permessage-deflate) when the browser
// offers it, as every browser does: a full render shrinks several times over.
var liveUpgrader = websocket.Upgrader{CheckOrigin: httpx.CheckWebSocketOrigin, EnableCompression: true}

const (
	liveWriteWait  = 10 * time.Second
	livePongWait   = 60 * time.Second
	livePingPeriod = 50 * time.Second
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
		conn, err := liveUpgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			return ok // Upgrade has answered the request
		}
		defer conn.Close()
		in := d.instance(args[2], args[3:])
		d.serve(context.WithValue(withApp(ctx, c), socketKey{}, true), c, conn, in)
		return ok
	}).Interface()
}

func (d *liveDef) serve(ctx context.Context, c *httpx.Ctx, conn *websocket.Conn, in *instance) {
	// patches holds the render the browser has and the dictionary it
	// shares, so the next render travels as a patch against them.
	var patches differ
	render := func(ref int, invalid bool) liveReply {
		body, err := in.render(ctx)
		if err != nil {
			return liveReply{Ref: ref, Error: err.Error()}
		}
		html := string(body)
		if patch, n, full := patches.next(html); !full {
			return liveReply{Ref: ref, Patch: patch, N: n, Invalid: invalid}
		}
		return liveReply{Ref: ref, HTML: html, Invalid: invalid}
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
	sock := &Socket{connected: true, inbox: make(chan Message, liveInbox)}
	defer sock.close()
	if err := in.mount(ctx, c, sock); err != nil {
		send(liveReply{Error: err.Error()})
		return
	}
	// The page arrived over HTTP: when this connection renders the same,
	// send nothing — the first change carries the render the browser will
	// patch against. Otherwise (Mount did more once connected, or there is
	// no HTTP render to compare with: a reconnect) send the render now.
	body, err := in.render(ctx)
	if err != nil {
		send(liveReply{Error: err.Error()})
		return
	}
	if joined, ok := takeJoin(c.Request.URL.Query().Get("join")); !ok || joined != string(body) {
		if !sendRender() {
			return
		}
	}

	_ = conn.SetReadDeadline(time.Now().Add(livePongWait))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(livePongWait)) })
	events := make(chan liveEvent)
	go func() {
		defer close(events)
		for {
			var ev liveEvent
			if err := conn.ReadJSON(&ev); err != nil {
				return
			}
			_ = conn.SetReadDeadline(time.Now().Add(livePongWait))
			events <- ev
		}
	}()
	ping := time.NewTicker(livePingPeriod)
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
		case ev, open := <-events:
			if !open {
				return
			}
			if ev.Event == "__resync" { // the browser lost track: send the whole render
				patches.forget()
				if !send(render(ev.Ref, in.errs != nil)) {
					return
				}
				continue
			}
			if !send(d.event(ctx, in, ev, render)) {
				return
			}
		}
	}
}

// event runs one browser event and returns its reply: the new render, or
// an error. A nexus.Errors from the method is not a failure: the page
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
		err = in.callValues(ctx, m, nil, []reflect.Value{arg})
	} else {
		err = in.call(ctx, m, nil, ev.Args)
	}
	if errs, ok := validation(err); ok {
		in.errs = errs
		return render(ev.Ref, true)
	}
	if err != nil {
		if errors.Is(err, nexus.ErrForbidden) {
			log.Printf("view: live %s: event %s refused: %v", d.t, ev.Event, err)
		}
		return liveReply{Ref: ev.Ref, Error: fmt.Sprintf("%s: %v", ev.Event, err)}
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
