package nexus

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"reflect"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/fx"

	"github.com/paulmanoni/nexus/registry"
	"github.com/paulmanoni/nexus/trace"
	"github.com/paulmanoni/nexus/transport/ws"
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
// handled by the hub directly and never reach user handlers.
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
		return rawOption{o: fx.Error(fmt.Errorf("nexus: AsWS(%q): message type is required", path))}
	}
	sh, err := inspectHandler(fn)
	if err != nil {
		return rawOption{o: fx.Error(err)}
	}
	return asWSInvoke(path, msgType, cfg, sh, fn)
}

// WSOption tunes an AsWS registration. Interface (not a func) so nexus.Use
// and auth.Required() — which return middleware bundles — can satisfy both
// RestOption and WSOption from a single value.
type WSOption interface{ applyToWS(*wsConfig) }

type wsConfig struct {
	baseEndpointConfig
	service string
}

// wsOption is the Option returned by AsWS. Implements moduleAnnotator so
// nexus.Module(...) stamps the module name onto the registration.
type wsOption struct {
	o   fx.Option
	cfg *wsConfig
}

func (w *wsOption) nexusOption() fx.Option   { return w.o }
func (w *wsOption) setModule(name string)    { w.cfg.module = name }
func (w *wsOption) setDeployment(tag string) { w.cfg.deployment = tag }

// asWSInvoke synthesizes the fx.Invoke for one AsWS registration. On start:
//
//  1. Look up (or create) the wsEndpoint for the path under App.wsMu.
//  2. Add this handler to the endpoint's type-dispatch table.
//  3. If the endpoint is fresh, build a hub, mount the HTTP upgrade route
//     with middleware + trace + metrics bundles, and wire hub.Start /
//     hub.Stop to fx.Lifecycle.
func asWSInvoke(path, msgType string, cfg *wsConfig, sh handlerShape, rawFn any) Option {
	appType := reflect.TypeOf((*App)(nil))
	lcType := reflect.TypeOf((*fx.Lifecycle)(nil)).Elem()

	in := make([]reflect.Type, 0, len(sh.depTypes)+2)
	in = append(in, appType, lcType)
	in = append(in, sh.depTypes...)
	invokeSig := reflect.FuncOf(in, nil, false)

	invokeFn := reflect.MakeFunc(invokeSig, func(args []reflect.Value) []reflect.Value {
		app := args[0].Interface().(*App)
		lc := args[1].Interface().(fx.Lifecycle)
		deps := args[2:]

		service := resolveEndpointService(cfg.service, cfg.module, deps, sh.depTypes, app)
		opName := opNameFromFunc(rawFn, service+"."+msgType)

		handler := wsTypedHandler{
			shape:    sh,
			deps:     deps,
			depTypes: sh.depTypes,
			service:  service,
			opName:   opName,
			bundles:  cfg.bundles,
		}

		ep, fresh := app.wsEndpointFor(path, service)
		ep.mu.Lock()
		if _, exists := ep.handlers[msgType]; exists {
			ep.mu.Unlock()
			panic(fmt.Sprintf("nexus: AsWS(%q, %q): duplicate registration for this message type", path, msgType))
		}
		ep.handlers[msgType] = handler
		ep.mu.Unlock()

		if fresh {
			mountWSEndpoint(app, lc, ep, cfg, msgType)
		} else if len(cfg.bundles) > 0 {
			log.Printf("nexus: AsWS(%q, %q): middleware on non-first registration is ignored — declare on the first AsWS for this path", path, msgType)
		}

		// Per-op registry entry — one row per (path, type) so the
		// dashboard's Endpoints tab lists each handler separately.
		endpointName := "WS " + path + " " + msgType
		app.registry.RegisterEndpoint(registry.Endpoint{
			Service:     service,
			Module:      cfg.module,
			Deployment:  cfg.deployment,
			Name:        endpointName,
			Transport:   registry.WebSocket,
			Method:      msgType,
			Path:        path,
			Description: cfg.description,
		})
		attachRestResources(app, service, deps, sh.depTypes)
		if resources := collectResourceNames(deps); len(resources) > 0 {
			app.registry.SetEndpointResources(service, endpointName, resources)
		}
		if svcDeps := collectServiceDeps(deps, sh.depTypes, service); len(svcDeps) > 0 {
			app.registry.SetEndpointServiceDeps(service, endpointName, svcDeps)
		}
		return nil
	})
	return &wsOption{o: fx.Invoke(invokeFn.Interface()), cfg: cfg}
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
		hub:      ws.NewHub(),
		handlers: map[string]wsTypedHandler{},
	}
	a.wsEndpoints[path] = ep
	return ep, true
}

// mountWSEndpoint wires the hub's OnMessage to the framework's dispatch, mounts
// the GET upgrade route on Gin with trace + metrics + user middleware, and
// binds the hub's Start/Stop to fx.Lifecycle so it shuts down cleanly.
func mountWSEndpoint(app *App, lc fx.Lifecycle, ep *wsEndpoint, cfg *wsConfig, firstMsgType string) {
	hub := ep.hub
	hub.OnMessage(func(conn *ws.Connection, _ int, data []byte) error {
		dispatchWSMessage(app, ep, conn, data)
		return nil
	})
	hub.OnIdentify(identifyFromGin)

	lc.Append(fx.Hook{
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

	endpointName := "WS " + ep.path
	chain, _ := buildEndpointChain(
		app, ep.service,
		ep.service+".ws."+firstMsgType,
		string(registry.WebSocket),
		endpointName,
		cfg.bundles, hub.ServeGin,
	)
	app.engine.GET(ep.path, chain...)
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

	sess := &WSSession{conn: conn, hub: ep.hub, ctx: context.Background()}
	ci := callInput{Ctx: sess.ctx, WS: sess}

	// Bind the payload into a fresh args struct. Missing `data` is fine —
	// zero-valued args.
	var argsVal reflect.Value
	if h.shape.hasArgs {
		ptr := reflect.New(h.shape.argsType)
		if len(env.Data) > 0 {
			if err := json.Unmarshal(env.Data, ptr.Interface()); err != nil {
				_ = sess.Send("error", map[string]string{
					"type":    env.Type,
					"message": "invalid payload: " + err.Error(),
				})
				return
			}
		}
		argsVal = ptr.Elem()
	}

	// Record a request.op event so the dashboard's per-endpoint badges
	// light up on live WS traffic, same as REST / GraphQL. Duration is
	// from just-before-handler to just-after so it reflects user work,
	// not JSON bind cost.
	start := time.Now()
	_, err := h.shape.callHandler(ci, h.deps, argsVal)
	if app.bus != nil {
		status := 200
		if err != nil {
			status = 500
		}
		ev := trace.Event{
			Kind:       trace.KindRequestOp,
			Service:    h.service,
			Endpoint:   h.opName,
			Transport:  string(registry.WebSocket),
			Status:     status,
			DurationMs: time.Since(start).Milliseconds(),
			Meta:       map[string]any{"clientId": conn.ClientID, "type": env.Type},
		}
		if err != nil {
			ev.Error = err.Error()
		}
		app.bus.Publish(ev)
	}
	if err != nil {
		_ = sess.Send("error", map[string]string{
			"type":    env.Type,
			"message": err.Error(),
		})
	}
}

// identifyFromGin is the hub identify hook used by AsWS. Matches
// oats_applicant's convention: prefer an auth-middleware-set `user` in
// Gin context, fall back to `?userId=` query, and stash the full claim
// on the connection metadata for handler access via sess.Metadata().
func identifyFromGin(c *gin.Context) (string, map[string]any) {
	meta := map[string]any{}
	if raw, ok := c.Get("user"); ok {
		meta["user"] = raw
		if id, ok := raw.(interface{ GetID() string }); ok {
			return id.GetID(), meta
		}
	}
	if q := c.Query("userId"); q != "" {
		return q, meta
	}
	return "", meta
}

