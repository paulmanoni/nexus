package nexus

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"reflect"
	"runtime/debug"
	"sync"
	"time"

	"github.com/paulmanoni/nexus/v2/di"
	"github.com/paulmanoni/nexus/v2/httpx"
	"github.com/paulmanoni/nexus/v2/internal/maskhook"
	"github.com/paulmanoni/nexus/v2/middleware"
	"github.com/paulmanoni/nexus/v2/registry"
	"github.com/paulmanoni/nexus/v2/trace"
	"github.com/paulmanoni/nexus/v2/transport/ws"
)

// AsWS registers one message-type-scoped handler on a WebSocket endpoint.
// Multiple AsWS calls with the same path share a single connection pool —
// the framework dispatches inbound messages by their envelope `type` to the
// matching handler.
//
//	type ChatPayload struct{ Text string }
//
//	func NewChatSend(svc *ChatSvc, sess *nexus.WSSession,
//	                 p nexus.Params[ChatPayload]) error {
//	    sess.EmitToRoom("chat.message",
//	        map[string]string{"text": p.Args.Text, "user": sess.UserID()},
//	        "lobby")
//	    return nil
//	}
//
//	nexus.AsWS("/events", "chat.send", NewChatSend, auth.Required())
//
// Wire protocol — every message is wrapped in the framework's envelope:
//
//	{ "type": "chat.send", "data": {...}, "timestamp": <unix> }
//
// The built-in types `ping`, `authenticate`, `subscribe`, `unsubscribe` are
// handled by the hub directly and never reach user handlers. A connection's
// user comes from the authenticated upgrade request (see
// RegisterRequestIdentity); `authenticate` only reports it. A client
// `subscribe` is refused unless the path allows it with ClientRooms — join
// rooms server-side with WSSession.JoinRoom after checking the caller.
//
// Handler signature: same reflective convention as AsRest / AsQuery —
//   - fx-injected deps anywhere (service wrappers, resources, other services);
//   - an optional *nexus.WSSession parameter gets the live connection handle;
//   - an optional nexus.Params[T] carries the decoded message payload in Args;
//   - return (error) — a non-nil error is sent back on the same connection as
//     an `error` envelope event. The connection stays open.
//
// Middleware (auth.Required, rate limits, etc.) on the FIRST AsWS call for a
// path is installed on the HTTP upgrade route and gates every subsequent
// connection. Middleware declared on later AsWS calls for the same path is
// ignored with a warning log — all dispatches share one upgrade route.
func AsWS(path, msgType string, fn any, opts ...WSOption) Option {
	cfg := &wsConfig{}
	for _, o := range opts {
		o.applyToWS(cfg)
	}
	if msgType == "" {
		return rawOption{o: di.Error(fmt.Errorf("nexus: AsWS(%q): message type is required", path))}
	}
	switch msgType {
	case "ping", "authenticate", "subscribe", "unsubscribe":
		return rawOption{o: di.Error(fmt.Errorf("nexus: AsWS(%q, %q): %q is a built-in message the hub answers itself, so this handler would never run — choose another type", path, msgType, msgType))}
	}
	if err := checkBundleTransports(cfg.bundles, middleware.TransportWebSocket, "WS "+path+" "+msgType); err != nil {
		return rawOption{o: di.Error(err)}
	}
	sh, err := inspectHandler(fn)
	if err != nil {
		return rawOption{o: di.Error(err)}
	}
	return asWSInvoke(path, msgType, cfg, sh, fn)
}

// WSOption tunes an AsWS registration. Interface (not a func) so nexus.Use
// and auth.Required() — which return middleware bundles — can satisfy both
// RestOption and WSOption from a single value.
type WSOption interface{ applyToWS(*wsConfig) }

type wsConfig struct {
	baseEndpointConfig
	service     string
	clientRooms func(userID, room string) bool
}

// wsOption is the Option returned by AsWS. Implements moduleAnnotator so
// nexus.Module(...) stamps the module name onto the registration.
type wsOption struct {
	o   di.Option
	cfg *wsConfig
}

func (w *wsOption) nexusOption() di.Option { return w.o }
func (w *wsOption) setModule(name string)  { w.cfg.module = name }

// asWSInvoke synthesizes the di.Invoke for one AsWS registration. On start:
//
//  1. Look up (or create) the wsEndpoint for the path under App.wsMu.
//  2. Add this handler to the endpoint's type-dispatch table.
//  3. If the endpoint is fresh, build a hub, mount the HTTP upgrade route
//     with middleware + trace + metrics bundles, and wire hub.Start /
//     hub.Stop to di.Lifecycle.
func asWSInvoke(path, msgType string, cfg *wsConfig, sh handlerShape, rawFn any) Option {
	appType := reflect.TypeOf((*App)(nil))
	lcType := reflect.TypeOf((*di.Lifecycle)(nil)).Elem()

	in := make([]reflect.Type, 0, len(sh.depTypes)+2)
	in = append(in, appType, lcType)
	in = append(in, sh.depTypes...)
	invokeSig := reflect.FuncOf(in, nil, false)

	invokeFn := reflect.MakeFunc(invokeSig, func(args []reflect.Value) []reflect.Value {
		app := args[0].Interface().(*App)
		lc := args[1].Interface().(di.Lifecycle)
		deps := args[2:]

		service := resolveEndpointService(cfg.service, cfg.module, deps, sh.depTypes, app)
		opName := opNameFromFunc(rawFn, service+"."+msgType)
		mountedPath := app.PrefixPath(path)
		endpointName := "WS " + mountedPath + " " + msgType

		handler := wsTypedHandler{
			shape:        sh,
			deps:         deps,
			depTypes:     sh.depTypes,
			service:      service,
			opName:       opName,
			endpointName: endpointName,
			bundles:      cfg.bundles,
		}

		ep, fresh := app.wsEndpointFor(path, service)
		ep.mu.Lock()
		if _, exists := ep.handlers[msgType]; exists {
			ep.mu.Unlock()
			panic(fmt.Sprintf("nexus: AsWS(%q, %q): duplicate registration for this message type", path, msgType))
		}
		ep.handlers[msgType] = handler
		ep.mu.Unlock()

		if cfg.clientRooms != nil {
			allow := cfg.clientRooms
			ep.hub.AllowClientRooms(func(c *ws.Connection, room string) bool { return allow(c.UserID, room) })
		}
		if fresh {
			mountWSEndpoint(app, lc, ep, cfg, msgType)
		} else if len(cfg.bundles) > 0 {
			log.Printf("nexus: AsWS(%q, %q): middleware on non-first registration is ignored — declare on the first AsWS for this path", path, msgType)
		}

		// Per-op registry entry — one row per (path, type) so the
		// dashboard's Endpoints tab lists each handler separately.
		// The recorded Path is the actual mounted URL (prefixed) so
		// dashboard testers and operator copy/paste hit the live
		// route, not the source-declared form.
		registerEndpoint(app, &cfg.baseEndpointConfig, service, registry.Endpoint{
			Name:      endpointName,
			Transport: registry.WebSocket,
			Method:    msgType,
			Path:      mountedPath,
		})
		recordEndpointDeps(app, service, endpointName, deps, sh.depTypes)
		recordEndpointSchema(app, service, endpointName, sh)
		return nil
	})
	return &wsOption{o: di.Invoke(invokeFn.Interface()), cfg: cfg}
}

// wsEndpointFor returns the endpoint state for path, creating it if needed.
// The boolean return is true when this call created the entry — callers use
// it to decide whether to mount the HTTP route (only the first AsWS per path
// does that).
func (a *App) wsEndpointFor(path, service string) (*wsEndpoint, bool) {
	a.wsMu.Lock()
	defer a.wsMu.Unlock()
	if a.wsEndpoints == nil {
		a.wsEndpoints = map[string]*wsEndpoint{}
	}
	if ep, ok := a.wsEndpoints[path]; ok {
		return ep, false
	}
	ep := &wsEndpoint{
		path:     path,
		service:  service,
		hub:      ws.NewHub(a.wsHubOpts...),
		handlers: map[string]wsTypedHandler{},
	}
	a.wsEndpoints[path] = ep
	return ep, true
}

// mountWSEndpoint wires the hub's OnMessage to the framework's dispatch, mounts
// the GET upgrade route on the router with trace + metrics + user middleware, and
// binds the hub's Start/Stop to di.Lifecycle so it shuts down cleanly.
func mountWSEndpoint(app *App, lc di.Lifecycle, ep *wsEndpoint, cfg *wsConfig, firstMsgType string) {
	hub := ep.hub
	hub.OnMessage(func(conn *ws.Connection, _ int, data []byte) error {
		dispatchWSMessage(app, ep, conn, data)
		return nil
	})
	hub.OnIdentify(identifyFromGin)
	hub.OnContext(func(c *httpx.Ctx) context.Context { return wsBaseContext(c.Request.Context()) })

	lc.Append(di.Hook{
		OnStart: func(ctx context.Context) error {
			// Hub.Start needs a context tied to the app's lifecycle —
			// cancelling it stops every read/write pump. We use the
			// user's OnStart ctx upgraded to background so the hub
			// outlives the hook itself (OnStart returns immediately).
			hub.Start(context.Background())
			return nil
		},
		OnStop: func(context.Context) error {
			hub.Stop()
			return nil
		},
	})

	mountedPath := app.PrefixPath(ep.path)
	// traceEndpoint = "" intentionally — WS upgrade is a one-time HTTP
	// request that promotes to a long-lived connection. Wrapping it in
	// trace.Middleware would keep one request.start open for the whole
	// connection lifetime (until close), polluting the waterfall with a
	// trace that contains nothing useful and never ends. Per-frame
	// traces are minted inside dispatchWSMessage instead, each as its
	// own root.
	chain, _ := buildEndpointChain(
		app, ep.service,
		ep.service+".ws."+firstMsgType,
		string(registry.WebSocket),
		"",
		app.gateBundles(cfg.tags, cfg.bundles), hub.ServeGin,
	)
	app.engine.GET(mountedPath, chain...)
}

// dispatchWSMessage unmarshals the envelope, looks up the handler for the
// message type, decodes its `data` into the handler's args type, and calls
// the handler with a WSSession tied to conn. Returns errors as `error`
// envelope events back to the originating connection.
func dispatchWSMessage(app *App, ep *wsEndpoint, conn *ws.Connection, raw []byte) {
	var env wsEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return // not JSON or not our envelope — ignore silently
	}
	if env.Type == "" {
		return
	}
	ep.mu.RLock()
	h, ok := ep.handlers[env.Type]
	ep.mu.RUnlock()
	if !ok {
		return // no handler — client may be speaking a type we don't serve
	}

	// Bracket the handler with request.start / request.end on a
	// fresh trace, same shape as the gin trace.Middleware does for
	// REST. The dashboard's waterfall renders each WS frame as its
	// own root trace; child spans started inside the handler attach
	// via the stashed span in ctx, so a handler doing DB work or
	// fanning out a pubsub publish shows the full chain.
	rootCtx := trace.WithBus(conn.Context(), app.bus)
	rootCtx, _, finish := trace.NewRootSpan(
		rootCtx,
		h.opName,
		h.service,
		h.opName,
		string(registry.WebSocket),
		trace.Str("ws.type", env.Type),
		trace.Str("ws.client_id", conn.ClientID),
	)

	sess := &WSSession{conn: conn, hub: ep.hub, ctx: rootCtx}
	ci := callInput{Ctx: rootCtx, WS: sess}

	// Bind the payload into a fresh args struct. Missing `data` is fine —
	// zero-valued args.
	var argsVal reflect.Value
	if h.shape.hasArgs {
		ptr := reflect.New(h.shape.argsType)
		if len(env.Data) > 0 {
			if err := json.Unmarshal(maskhook.UnmaskJSON(env.Data), ptr.Interface()); err != nil {
				// Decode failed before the handler ran — close the
				// root trace with the error's status so the waterfall
				// doesn't show an open-ended request.
				bad := bindError(err)
				finish(bad.HTTPStatus(), bad)
				_ = sess.Send("error", wsErrorEvent(env.Type, bad))
				return
			}
		}
		if err := validateValue(ptr); err != nil {
			finish(InvalidInput.HTTPStatus(), err)
			_ = sess.Send("error", wsErrorEvent(env.Type, err))
			return
		}
		argsVal = ptr.Elem()
	}

	// Record a request.op event so the dashboard's per-endpoint badges
	// light up on live WS traffic, same as REST / GraphQL. Duration is
	// from just-before-handler to just-after so it reflects user work,
	// not JSON bind cost.
	start := time.Now()
	err := callWSHandler(h, ci, argsVal)
	status := 200
	if err != nil {
		status = ErrorOf(err).HTTPStatus()
	}
	finish(status, err)
	if app.bus != nil {
		// Endpoint MUST match the registry's e.Name ("WS <path>
		// <msgType>") so the dashboard's per-op edge lookup hits.
		// Using h.opName here would land flashes on the module
		// aggregate inbound — i.e. flash every endpoint on the
		// module card instead of the specific WS row.
		ev := trace.Event{
			Kind:       trace.KindRequestOp,
			Service:    h.service,
			Endpoint:   h.endpointName,
			Transport:  string(registry.WebSocket),
			Method:     env.Type,
			Status:     status,
			DurationMs: time.Since(start).Milliseconds(),
			Meta:       map[string]any{"clientId": conn.ClientID, "type": env.Type, "op": h.opName},
		}
		if err != nil {
			ev.Error = err.Error()
		}
		app.bus.Publish(ev)
	}
	if err != nil {
		_ = sess.Send("error", wsErrorEvent(env.Type, err))
	}
}

// callWSHandler runs the user's WebSocket handler with panic recovery. Unlike
// REST (whose handlers sit behind the global recoveryMiddleware) a WS message
// handler runs in the connection's read-loop goroutine (ws.Hub.readPump) — and
// an unrecovered panic in a goroutine crashes the WHOLE process in Go. So a
// junior WS handler doing m["k"] on a nil map, or an out-of-range index, would
// take down the server, while the identical bug in a REST handler is caught,
// logged, and shown on the dashboard.
//
// This closes that gap by mirroring recoveryMiddleware: a recovered panic
// becomes a *trace.StackError, which the caller then threads through the SAME
// path a returned error already takes in dispatchWSMessage — finish(500), a
// request.op event on the bus (so the dashboard's error badge lights up with
// the captured stack), and an "error" envelope back to the client.
//
// The recovery invariant is exercised by TestWSHandlerPanicRecovered and, for
// every execution context, TestUserHandlerPanicsAreRecovered — see the note
// there: any new transport that calls a user function MUST recover the same way.
func callWSHandler(h wsTypedHandler, ci callInput, argsVal reflect.Value) (err error) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		se, ok := r.(*trace.StackError)
		if !ok {
			msg := fmt.Sprintf("%v", r)
			if msg == "" {
				msg = "panic"
			}
			se = &trace.StackError{
				Err:   fmt.Errorf("panic: %s", msg),
				Stack: trace.CleanStack(string(debug.Stack())),
			}
		}
		// Mirror to stderr so dev / `nexus dev` terminals see it even with
		// no dashboard open, matching recoveryMiddleware's log line.
		log.Printf("[nexus] panic recovered in WS handler %s — %s\n%s",
			h.opName, se.Error(), trace.StackOf(se))
		err = se
	}()
	_, err = h.shape.callHandler(ci, h.deps, argsVal)
	return err
}

// identifyFromGin is the hub identify hook used by AsWS. A connection's user
// is what the server authenticated for the upgrade request: a value server
// middleware stored as "user" (with a GetID method), else a registered
// request-identity source (extension/auth registers one). Nothing the
// client sends — no query parameter, no message — can set it, because
// EmitToUser delivers to whoever holds the id. The "user" value, when
// present, rides on the connection metadata for handlers.
func identifyFromGin(c *httpx.Ctx) (string, map[string]any) {
	meta := map[string]any{}
	if raw, ok := c.Get("user"); ok {
		meta["user"] = raw
		if id, ok := raw.(interface{ GetID() string }); ok && id.GetID() != "" {
			return id.GetID(), meta
		}
	}
	if id, ok := requestIdentity(c.Request.Context()); ok {
		return id, meta
	}
	return "", meta
}

// ClientRooms lets clients on this WebSocket path join rooms themselves with
// the built-in {"type":"subscribe","room":"…"} message, for the rooms allow
// accepts. userID is the connection's authenticated user ("" when
// anonymous). Without it every client subscribe is refused: a room is an
// audience the server addresses with EmitToRoom, so by default only a
// handler joins one (WSSession.JoinRoom), after checking the caller. The
// hub is shared by every AsWS on the path; the last ClientRooms given for
// the path applies.
//
//	nexus.AsWS("/ws", "jobs.watch", NewWatch, auth.Required(),
//	    nexus.ClientRooms(func(userID, room string) bool { return room == "jobs" && userID != "" }))
func ClientRooms(allow func(userID, room string) bool) WSOption {
	return clientRoomsOption{allow: allow}
}

type clientRoomsOption struct {
	allow func(userID, room string) bool
}

func (o clientRoomsOption) applyToWS(c *wsConfig) { c.clientRooms = o.allow }

// WSSession is the per-connection handle injected into AsWS handlers. Every
// handler that declares `*nexus.WSSession` as a parameter receives the session
// tied to the connection that produced the current inbound message. Safe for
// concurrent use — wraps a ws.Hub under the hood.
//
// The framework uses the same envelope protocol as the built-in ws.Hub:
//
//	{ "type": "chat.send", "data": {...}, "timestamp": <unix> }
//
// Emit / EmitToUser / EmitToRoom / EmitToClient publish in that shape; SendRaw
// is the escape hatch for non-envelope payloads.
type WSSession struct {
	conn *ws.Connection
	hub  *ws.Hub
	ctx  context.Context
}

// Context returns a context cancelled when the connection disconnects. Safe to
// pass downstream — long-running work will unblock on hangup.
func (s *WSSession) Context() context.Context {
	if s == nil || s.ctx == nil {
		return context.Background()
	}
	return s.ctx
}

// ClientID is the UUID the hub minted for this connection at upgrade time.
func (s *WSSession) ClientID() string {
	if s == nil || s.conn == nil {
		return ""
	}
	return s.conn.ClientID
}

// UserID is the identity attached at upgrade (via `?userId=` query or a
// gin-context `user` interface) or later via the client-initiated
// `authenticate` protocol message. Empty when the connection is unauthed.
func (s *WSSession) UserID() string {
	if s == nil || s.conn == nil {
		return ""
	}
	return s.conn.UserID
}

// Metadata is the map the hub's identify hook populated at upgrade time. Read
// freely; mutation is not safe across goroutines.
func (s *WSSession) Metadata() map[string]any {
	if s == nil || s.conn == nil {
		return nil
	}
	return s.conn.Metadata
}

// SendRaw writes bytes directly to this connection with no envelope. Use when
// you're speaking a non-nexus protocol (e.g. pre-marshalled binary data).
func (s *WSSession) SendRaw(data []byte) {
	if s == nil || s.conn == nil {
		return
	}
	s.conn.Send(data)
}

// Send wraps data in an envelope and unicasts it to this connection.
func (s *WSSession) Send(eventType string, data any) error {
	if s == nil || s.conn == nil {
		return nil
	}
	return s.conn.SendEvent(ws.NewEvent(eventType, data).ToClient(s.conn.ClientID))
}

// Emit broadcasts an envelope to every connection on this endpoint.
func (s *WSSession) Emit(eventType string, data any) {
	if s == nil || s.hub == nil {
		return
	}
	s.hub.EmitBroadcast(eventType, data)
}

// EmitToUser sends an envelope to every connection authed as one of userIDs.
func (s *WSSession) EmitToUser(eventType string, data any, userIDs ...string) {
	if s == nil || s.hub == nil || len(userIDs) == 0 {
		return
	}
	s.hub.EmitToUsers(eventType, data, userIDs...)
}

// EmitToRoom sends an envelope to every connection subscribed to room.
func (s *WSSession) EmitToRoom(eventType string, data any, room string) {
	if s == nil || s.hub == nil || room == "" {
		return
	}
	s.hub.EmitToRoom(eventType, data, room)
}

// EmitToClient sends an envelope to the connections with the given IDs.
func (s *WSSession) EmitToClient(eventType string, data any, clientIDs ...string) {
	if s == nil || s.hub == nil || len(clientIDs) == 0 {
		return
	}
	s.hub.EmitToClients(eventType, data, clientIDs...)
}

// JoinRoom subscribes this connection to a room. The server's way to put a
// connection in an audience, after the handler has checked the caller may
// hear it; a client's own `{"type":"subscribe"}` is refused unless the path
// allows it with ClientRooms.
func (s *WSSession) JoinRoom(room string) {
	if s == nil || s.hub == nil || s.conn == nil || room == "" {
		return
	}
	s.hub.Join(s.conn, room)
}

// LeaveRoom unsubscribes this connection from a room.
func (s *WSSession) LeaveRoom(room string) {
	if s == nil || s.hub == nil || s.conn == nil || room == "" {
		return
	}
	s.hub.Leave(s.conn, room)
}

// wsEndpoint is the shared state for one AsWS path. Multiple AsWS calls
// targeting the same path populate different entries in `handlers` but share
// the same hub, middleware chain, and registry entry.
type wsEndpoint struct {
	path     string
	service  string
	hub      *ws.Hub
	mu       sync.RWMutex
	handlers map[string]wsTypedHandler
}

// wsTypedHandler is one registered message-type dispatch target.
type wsTypedHandler struct {
	shape    handlerShape
	deps     []reflect.Value
	depTypes []reflect.Type
	service  string
	// opName is the metrics-store key — typically "<service>.<msgType>"
	// or the function name (via opNameFromFunc) — kept in sync with the
	// metrics middleware key registered in asWSInvoke.
	opName string
	// endpointName is the registry's canonical Endpoint.Name for this
	// dispatch (e.g. "WS /events chat.send"). The dashboard's per-op
	// edge index keys on this exact string, so the per-frame
	// request.op event MUST set Endpoint: endpointName to land flashes
	// + packet animations on the right WS row instead of the module-
	// wide aggregate edge.
	endpointName string
	bundles      []middleware.Middleware
}

// wsEnvelope is the inbound message shape. Matches the ws.Hub's Event for
// round-tripping: clients send `{type, data}` and receive events back.
type wsEnvelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// RequestIdentityFunc reports the authenticated subject of a request from
// its context: the user id an identity provider resolved, and whether there
// was one. extension/auth registers one; an app with its own authentication
// can register another.
type RequestIdentityFunc func(ctx context.Context) (id string, ok bool)

var (
	requestIdentityMu    sync.RWMutex
	requestIdentityFuncs []RequestIdentityFunc
)

// RegisterRequestIdentity adds a source of request identity. WebSocket
// endpoints (AsWS) use it to know which user a connection belongs to, which
// is what EmitToUser addresses — so it must read what the server
// authenticated (the request context an auth middleware populated), never
// anything the client sent. The first registered source that reports an id
// wins. Safe to call from package init.
func RegisterRequestIdentity(fn RequestIdentityFunc) {
	if fn == nil {
		return
	}
	requestIdentityMu.Lock()
	requestIdentityFuncs = append(requestIdentityFuncs, fn)
	requestIdentityMu.Unlock()
}

// RequestIdentity reports the authenticated subject of a request, as the
// registered identity sources (extension/auth, or the app's own) see it. It
// is what extensions record as "who did this" — the enqueuer of a background
// job, for instance.
func RequestIdentity(ctx context.Context) (id string, ok bool) { return requestIdentity(ctx) }

// requestIdentity asks each registered source, in order.
func requestIdentity(ctx context.Context) (string, bool) {
	requestIdentityMu.RLock()
	defer requestIdentityMu.RUnlock()
	for _, fn := range requestIdentityFuncs {
		if id, ok := fn(ctx); ok && id != "" {
			return id, true
		}
	}
	return "", false
}

// WSCarrier copies values from a WebSocket upgrade request's context onto a
// connection's base context and returns it. Each message handler's context
// (Params.Context, WSSession.Context) derives from that base, so what a
// carrier copies — extension/auth copies the identity and its own state —
// is what auth.IdentityFrom, auth.Can and the like see in a WS handler.
type WSCarrier func(upgrade, conn context.Context) context.Context

var (
	wsCarrierMu sync.RWMutex
	wsCarriers  []WSCarrier
)

// RegisterWSCarrier adds a WSCarrier. Copy only values that may outlive the
// request: the upgrade request's context also holds per-request state (a
// Scoped memo, a session handle) that must not be shared by every message
// of a long-lived connection. Values are captured once, at the upgrade — a
// connection keeps the identity it was opened with. Safe to call from
// package init.
func RegisterWSCarrier(fn WSCarrier) {
	if fn == nil {
		return
	}
	wsCarrierMu.Lock()
	wsCarriers = append(wsCarriers, fn)
	wsCarrierMu.Unlock()
}

// wsBaseContext builds a connection's base context from its upgrade request
// by running every registered carrier over a fresh background context.
func wsBaseContext(upgrade context.Context) context.Context {
	base := context.Background()
	wsCarrierMu.RLock()
	defer wsCarrierMu.RUnlock()
	for _, fn := range wsCarriers {
		if next := fn(upgrade, base); next != nil {
			base = next
		}
	}
	return base
}
