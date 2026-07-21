// Package proxy is a strangler-fig bridge: it reverse-proxies routes to a
// legacy upstream (e.g. a Django app being migrated to nexus) AND registers
// each proxied route on the nexus dashboard, tagged as a proxy. The dashboard
// then becomes a live migration board — every enumerated route shows whether
// it's still forwarded to the legacy app or already served by a native nexus
// handler, and the "Proxied" cluster shrinks toward zero as you migrate.
//
// The key move is auto-yield: at boot, for each configured Route, if a native
// nexus endpoint already exists at that method+path, the proxy skips it. So
// migrating a route is purely additive — add the native nexus.AsRest handler,
// rebuild, and on next boot it wins the path, the proxy drops it, and the
// dashboard moves that node out of the "Proxied" cluster into its real
// service. You never edit the proxy config to migrate; the route list stays
// as the full inventory and the board updates itself.
//
//	proxy.Module(proxy.Config{
//	    Upstream: "http://localhost:8000",           // the legacy app
//	    Group:    "Proxied · Django",
//	    Routes: []proxy.Route{
//	        {Method: "GET",  Path: "/api/candidate-placement-details/"},
//	        {Method: "POST", Path: "/api/ussd-request/"},
//	    },
//	    Fallback: &proxy.Fallback{Prefix: "/"},        // one node for the long tail
//	})
package proxy

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension"
	"github.com/paulmanoni/nexus/extension/dashboard"
	"github.com/paulmanoni/nexus/httpx"
	"github.com/paulmanoni/nexus/registry"
	"github.com/paulmanoni/nexus/trace"
)

// Route is one legacy path forwarded to the upstream and shown on the
// dashboard. Method is a REST verb; empty (or "*") forwards every standard
// method (mounted as Any). Path uses nexus route syntax (":id", "*rest") and
// should match the eventual native handler's path exactly, so the auto-yield
// recognizes the migration.
type Route struct {
	Method string
	Path   string
	Name   string // optional dashboard label; defaults to "<METHOD> <path>"
}

// Fallback mounts a single catch-all that forwards everything under Prefix not
// matched by a native or enumerated route — one dashboard node for the long
// tail of legacy routes you never intend to enumerate (admin, template pages).
// Specific routes always win over this wildcard.
type Fallback struct {
	Prefix string // default "/"
	Name   string // dashboard label; default "catch-all → upstream"
}

// Config drives proxy.Module. Upstream is required; the rest have defaults.
type Config struct {
	// Upstream is the absolute base URL of the legacy app (scheme://host).
	Upstream string

	// Command, when set, launches and supervises the upstream process
	// alongside nexus — Dev argv under `nexus dev`, Prod argv for a production
	// binary (either may be empty to mean "externally managed in this mode").
	// So one `nexus dev` boots both nexus and the legacy app. Optional.
	Command *Command

	// Routes is the enumerated, dashboard-visible migration inventory. Each
	// flips from "proxied" to "migrated" once a native handler exists at its
	// method+path.
	Routes []Route

	// Fallback, when set, forwards everything else under its prefix as one
	// catch-all node. Optional.
	Fallback *Fallback

	// Group names the dashboard module the proxied routes cluster under (the
	// architecture graph draws it as one shrinking box). Default "Proxied".
	Group string

	// Icon is the lucide-style icon for the proxied nodes. Default
	// "arrow-left-right".
	Icon string

	// SetHeaders adds/overrides request headers sent upstream. Inbound headers
	// (Authorization, Cookie, CSRF) already forward unchanged — this is for
	// extras (e.g. an internal shared-secret header). Optional.
	SetHeaders map[string]string

	// RewritePath rewrites the request path before forwarding (e.g. strip a
	// prefix the upstream doesn't expect). Optional; identity when nil.
	RewritePath func(string) string

	// Transport overrides the HTTP transport used to reach the upstream (custom
	// timeouts, TLS, connection pool). Optional; http.DefaultTransport when nil.
	Transport http.RoundTripper
}

// Module wires the proxy into a nexus app as an extension.Plugin. Routes are
// mounted + registered at OnBoot (after every native endpoint is in the
// registry), so the auto-yield sees the current migration state.
func Module(cfg Config) nexus.Option {
	if cfg.Upstream == "" {
		return nexus.Error(fmt.Errorf("proxy: Config.Upstream is required"))
	}
	if cfg.Group == "" {
		cfg.Group = "Proxied"
	}
	if cfg.Icon == "" {
		cfg.Icon = "arrow-left-right"
	}
	st := &pluginState{cfg: cfg}
	return extension.Use(extension.Plugin{
		Name:      "proxy",
		Version:   "1",
		Icon:      cfg.Icon,
		Lifecycle: &extension.Lifecycle{OnBoot: st.boot, OnShutdown: st.shutdown},
	})
}

type pluginState struct {
	cfg  Config
	proc *process // supervised upstream, when Config.Command is set
}

// boot builds the shared reverse proxy, then mounts + registers each route
// that isn't already served natively. Runs at OnBoot (di.Start, before the
// listener binds) so app.Registry() already holds every native endpoint.
func (s *pluginState) boot(_ context.Context, app *nexus.App) error {
	// Launch the upstream first so it's starting up while we mount routes.
	if s.cfg.Command != nil {
		dev := nexus.IsDev()
		proc, err := startProcess(s.cfg.Command, dev)
		if err != nil {
			return err
		}
		s.proc = proc
		if proc != nil {
			log.Printf("[nexus] proxy: launched %s (%s mode) → %s", proc.name, modeLabel(dev), s.cfg.Upstream)
		}
	}

	rp, err := buildReverseProxy(s.cfg.Upstream, s.cfg.SetHeaders, s.cfg.RewritePath, s.cfg.Transport)
	if err != nil {
		return err
	}
	forward := proxyHandlerFunc(rp)

	router := app.Router()
	reg := app.Registry()
	bus := app.Bus()

	native := nativeRESTIndex(reg)

	for _, r := range s.cfg.Routes {
		method := normMethod(r.Method)
		if isMigrated(native, method, r.Path) {
			continue // a native handler owns this path — proxy yields
		}
		name := r.Name
		if name == "" {
			name = methodLabel(method) + " " + r.Path
		}
		chain := []httpx.HandlerFunc{
			trace.Middleware(bus, s.cfg.Group, name, string(registry.REST)),
			forward,
		}
		if method == anyMethod {
			router.Any(r.Path, chain...)
		} else {
			router.Handle(method, r.Path, chain...)
		}
		s.registerProxied(reg, name, methodLabel(method), r.Path)
	}

	if s.cfg.Fallback != nil {
		prefix := strings.TrimRight(s.cfg.Fallback.Prefix, "/")
		wildPath := prefix + "/*rest"
		name := s.cfg.Fallback.Name
		if name == "" {
			name = "catch-all → upstream"
		}
		router.Any(wildPath, trace.Middleware(bus, s.cfg.Group, name, string(registry.REST)), forward)
		s.registerProxied(reg, name, methodLabel(anyMethod), wildPath)
	}

	// Best-effort readiness gate: don't finish boot until the upstream answers,
	// so we don't start forwarding to a process that isn't listening yet.
	if s.proc != nil && s.cfg.Command.Ready.Path != "" {
		if waitReady(s.cfg.Upstream, s.cfg.Command.Ready.Path, s.cfg.Command.Ready.Timeout) {
			log.Printf("[nexus] proxy: upstream %s ready", s.proc.name)
		} else {
			log.Printf("[nexus] proxy: upstream %s not ready within timeout — forwarding anyway", s.proc.name)
		}
	}

	// Live proxied/migration burndown for the dashboard "Proxied" panel.
	// Recomputed from the registry on each snapshot, so a route flips from
	// proxied → migrated as soon as its native handler is registered.
	dashboard.RegisterSnapshotExtra("proxied", func() any { return s.snapshot(app.Registry()) })
	return nil
}

// shutdown stops the supervised upstream (if any) during di.Stop.
func (s *pluginState) shutdown(_ context.Context) error {
	if s.proc != nil {
		log.Printf("[nexus] proxy: stopping %s", s.proc.name)
		s.proc.stop()
	}
	return nil
}

// registerProxied adds a dashboard endpoint marked as a proxy: it clusters in
// the Group module, carries the ProxyTag (upstream URL) for a "proxied" badge,
// and the configured icon.
func (s *pluginState) registerProxied(reg *registry.Registry, name, method, path string) {
	reg.RegisterEndpoint(registry.Endpoint{
		Service:     s.cfg.Group,
		Module:      s.cfg.Group,
		Name:        name,
		Transport:   registry.REST,
		Method:      method,
		Path:        path,
		Description: "⇄ proxied → " + s.cfg.Upstream,
		Tags: map[string]string{
			registry.ProxyTag: s.cfg.Upstream,
			registry.IconTag:  s.cfg.Icon,
		},
	})
}

// snapshot reports the migration burndown for the enumerated routes, computed
// live from the current registry.
func (s *pluginState) snapshot(reg *registry.Registry) any {
	native := nativeRESTIndex(reg)
	routes := make([]map[string]any, 0, len(s.cfg.Routes))
	migrated := 0
	for _, r := range s.cfg.Routes {
		m := normMethod(r.Method)
		status := "proxied"
		if isMigrated(native, m, r.Path) {
			status = "migrated"
			migrated++
		}
		routes = append(routes, map[string]any{"method": methodLabel(m), "path": r.Path, "status": status})
	}
	return map[string]any{
		"upstream": s.cfg.Upstream,
		"total":    len(s.cfg.Routes),
		"migrated": migrated,
		"proxied":  len(s.cfg.Routes) - migrated,
		"routes":   routes,
	}
}

const anyMethod = "*"

// nativeRESTIndex maps path → set of methods served by NATIVE REST endpoints
// (proxied endpoints, tagged ProxyTag, are excluded so the proxy never counts
// its own registrations as "migrated").
func nativeRESTIndex(reg *registry.Registry) map[string]map[string]bool {
	idx := map[string]map[string]bool{}
	for _, e := range reg.Endpoints() {
		if e.Transport != registry.REST {
			continue
		}
		if _, isProxy := e.Tags[registry.ProxyTag]; isProxy {
			continue
		}
		m := idx[e.Path]
		if m == nil {
			m = map[string]bool{}
			idx[e.Path] = m
		}
		m[strings.ToUpper(e.Method)] = true
	}
	return idx
}

// isMigrated reports whether a native handler already owns (method, path). For
// an Any route ("*"), any native method on the path counts as migrated.
func isMigrated(native map[string]map[string]bool, method, path string) bool {
	methods := native[path]
	if len(methods) == 0 {
		return false
	}
	if method == anyMethod {
		return true
	}
	return methods[method]
}

// normMethod uppercases/trims a verb; empty or "*" becomes the Any sentinel.
func normMethod(m string) string {
	m = strings.ToUpper(strings.TrimSpace(m))
	if m == "" || m == "ANY" {
		return anyMethod
	}
	return m
}

// methodLabel renders a method for display/registry: the Any sentinel shows as
// "ANY".
func methodLabel(m string) string {
	if m == anyMethod {
		return "ANY"
	}
	return m
}
