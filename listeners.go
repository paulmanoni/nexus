package nexus

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"

	"github.com/paulmanoni/nexus/dashboard"
)

// ListenerScope decides which routes a listener exposes. The framework
// uses the request's bound local address (via http.LocalAddrContextKey)
// to look up the scope and 404s requests to routes outside that scope.
//
// The scope abstraction is opt-in: when Config.Listeners is empty, a
// single listener bound to Config.Addr serves every route (today's
// behavior). The scope filter only fires for explicitly-declared
// listeners.
type ListenerScope int

const (
	// ScopePublic exposes user-facing routes (REST, GraphQL, WebSocket)
	// and hides the /__nexus dashboard surface. The default for any
	// listener whose Scope is left zero — public is the safe default
	// for the listener bound to the world.
	ScopePublic ListenerScope = iota

	// ScopeInternal exposes user-facing routes plus /__nexus/health
	// and /__nexus/ready, so peer services can call your handlers and
	// orchestrators (k8s probes, load balancers) can poll readiness.
	// The rest of /__nexus stays hidden.
	ScopeInternal

	// ScopeAdmin exposes everything — /__nexus surface AND user
	// routes. The admin listener is meant for operators (typically
	// bound to a private subnet or behind an SSH tunnel), so giving
	// it the full route set is a UX win: the dashboard's in-page
	// RestTester / GraphQLTester make relative fetch() calls, and
	// blocking user routes here would silently 404 those.
	//
	// If you need a strictly-dashboard-only listener, that's a
	// future ScopeIntrospection — the current ScopeAdmin trades
	// surface area for ergonomics.
	ScopeAdmin
)

// String returns the lowercase scope name. Dashboards and logs render
// scopes by name; keeping the mapping in one place makes additions
// future-safe.
func (s ListenerScope) String() string {
	switch s {
	case ScopePublic:
		return "public"
	case ScopeInternal:
		return "internal"
	case ScopeAdmin:
		return "admin"
	}
	return "unknown"
}

// Listener declares one bound address with a scope. Multiple listeners
// can share a scope (e.g. one bound to 0.0.0.0:8080 and another to a
// loopback for sidecar health checks).
type Listener struct {
	// Addr is the listen address (e.g. ":8080", "127.0.0.1:9000").
	// Required — an empty Addr is rejected by Run with a precise
	// error message.
	Addr string

	// Scope decides which routes this listener exposes. Zero value
	// is ScopePublic — the conservative default for an exposed port.
	Scope ListenerScope
}

// listenerScopes is the runtime lookup table used by the scope filter
// middleware: port string → scope. Populated by registerLifecycle as
// each net.Listener actually binds (so :0 → random port resolves
// correctly), read on every request.
//
// Keyed by port rather than full address because a dual-stack
// listener bound on `[::]:8080` accepts IPv4 connections that arrive
// with a LocalAddr of `127.0.0.1:8080` — the host parts diverge while
// the port stays stable. Single process / single bind per port is the
// realistic invariant; if you ever need different scopes for the same
// port on different hosts, that's a different feature than this.
type listenerScopes struct {
	mu sync.RWMutex
	m  map[string]ListenerScope
}

func newListenerScopes() *listenerScopes {
	return &listenerScopes{m: map[string]ListenerScope{}}
}

// addrPort extracts the port from "host:port", "[::]:port",
// "127.0.0.1:port", etc. Falls back to the input verbatim when the
// address has no colon — defensive handling for malformed inputs that
// should never reach here in practice.
func addrPort(addr string) string {
	if _, port, err := net.SplitHostPort(addr); err == nil {
		return port
	}
	return addr
}

func (l *listenerScopes) set(addr string, scope ListenerScope) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.m[addrPort(addr)] = scope
}

func (l *listenerScopes) get(addr string) (ListenerScope, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	s, ok := l.m[addrPort(addr)]
	return s, ok
}

func (l *listenerScopes) empty() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.m) == 0
}

// scopeAllowsPath decides whether the given path is exposed on the
// given scope. The split between health/ready and the rest of the
// dashboard is the load-bearing rule for ScopeInternal: /__nexus/health
// and /ready are safe to expose to peers and orchestrators (they
// reveal nothing sensitive), but the rest of the dashboard is held
// back from internal traffic.
//
// ScopeAdmin allows everything — see ScopeAdmin's doc comment for
// the rationale (operator ergonomics + dashboard testers).
func scopeAllowsPath(scope ListenerScope, path string) bool {
	isDash := strings.HasPrefix(path, dashboard.Prefix)
	isHealth := path == dashboard.Prefix+"/health" || path == dashboard.Prefix+"/ready"
	switch scope {
	case ScopePublic:
		return !isDash
	case ScopeInternal:
		return !isDash || isHealth
	case ScopeAdmin:
		return true
	}
	return false
}

// offsetAddr returns publicAddr with its port shifted by offset.
// Preserves the host part — `127.0.0.1:8081` + 1000 stays loopback-
// bound on `127.0.0.1:9081`, `:8081` becomes `:9081`.
func offsetAddr(publicAddr string, offset int) (string, error) {
	host, portStr, err := net.SplitHostPort(publicAddr)
	if err != nil {
		return "", fmt.Errorf("offsetAddr: parse %q: %w", publicAddr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", fmt.Errorf("offsetAddr: non-numeric port in %q", publicAddr)
	}
	return fmt.Sprintf("%s:%d", host, port+offset), nil
}

// fillListenerAddrs walks an explicit Listeners map and fills in any
// empty Addrs from the resolved public address. Lets users declare
// the listener *shape* in main.go (or via the manifest's listeners
// block) without hardcoding ports — the per-deployment port flows
// in via cfg.Addr, so split binaries each bind to their own port
// without per-binary main.go.
//
// Rules:
//   - non-empty Addr: kept verbatim (explicit override wins)
//   - empty Addr + ScopePublic: filled from publicAddr
//   - empty Addr + ScopeAdmin: filled from publicAddr + 1000
//   - empty Addr + ScopeInternal: filled from publicAddr + 2000
//
// The 1000/2000 offsets are framework conventions — operators who
// need different numbers set Addr explicitly. Returns the filled
// map; doesn't mutate the input.
func fillListenerAddrs(in map[string]Listener, publicAddr string) map[string]Listener {
	if publicAddr == "" {
		publicAddr = ":8080"
	}
	out := make(map[string]Listener, len(in))
	for name, l := range in {
		if l.Addr != "" {
			out[name] = l
			continue
		}
		switch l.Scope {
		case ScopePublic:
			l.Addr = publicAddr
		case ScopeAdmin:
			if a, err := offsetAddr(publicAddr, 1000); err == nil {
				l.Addr = a
			}
		case ScopeInternal:
			if a, err := offsetAddr(publicAddr, 2000); err == nil {
				l.Addr = a
			}
		}
		out[name] = l
	}
	return out
}

// scopeFilterMiddleware returns a gin middleware that 404s requests
// arriving on a listener whose scope doesn't expose the route. Falls
// through (no filtering) when the scope table is empty — that's the
// back-compat path for users who haven't declared Listeners.
//
// Detection: net/http stores the listener's local Addr on the request
// context under http.LocalAddrContextKey. We stringify it and look up
// the scope. Bound addresses (after net.Listen returns) are what land
// here, so :0 (random port) resolves to the actually-bound port.
func scopeFilterMiddleware(scopes *listenerScopes) gin.HandlerFunc {
	return func(c *gin.Context) {
		if scopes == nil || scopes.empty() {
			c.Next()
			return
		}
		addr, _ := c.Request.Context().Value(http.LocalAddrContextKey).(net.Addr)
		if addr == nil {
			c.Next()
			return
		}
		scope, ok := scopes.get(addr.String())
		if !ok {
			c.Next()
			return
		}
		if !scopeAllowsPath(scope, c.Request.URL.Path) {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		c.Next()
	}
}
