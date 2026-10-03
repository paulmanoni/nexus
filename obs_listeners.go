package nexus

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/httpx"

	"github.com/paulmanoni/nexus/v2/extension/dashboard"
)

// ServerTLSConfig builds a *tls.Config for a server-terminating
// Listener. certFile and keyFile are required (PEM-encoded server
// cert + key). caFile is optional: when set, the listener requires
// clients to present a certificate signed by that CA (mTLS), and
// 1.3 handshakes for clients without one will fail at the TLS layer
// before any HTTP request is dispatched. Pass "" to skip client auth.
//
// Defaults: TLS 1.2 minimum (1.0/1.1 are deprecated and unsafe);
// modern cipher suite selection left to Go's defaults, which track
// the IETF recommended list.
//
//	cfg, err := nexus.ServerTLSConfig("admin.crt", "admin.key", "admin-ca.crt")
//	if err != nil { log.Fatal(err) }
//	listener := config.Listener{Addr: "10.0.0.5:9443", Scope: config.ScopeAdmin, TLS: cfg}
func ServerTLSConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
	if certFile == "" || keyFile == "" {
		return nil, fmt.Errorf("nexus: ServerTLSConfig: certFile and keyFile are required")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("nexus: ServerTLSConfig: load keypair: %w", err)
	}
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
	}
	if caFile != "" {
		caPEM, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("nexus: ServerTLSConfig: read CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("nexus: ServerTLSConfig: %q contains no valid PEM certificates", caFile)
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg, nil
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
	m  map[string]config.ListenerScope
}

func newListenerScopes() *listenerScopes {
	return &listenerScopes{m: map[string]config.ListenerScope{}}
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

func (l *listenerScopes) set(addr string, scope config.ListenerScope) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.m[addrPort(addr)] = scope
}

func (l *listenerScopes) get(addr string) (config.ListenerScope, bool) {
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
// given scope. /__nexus/health and /__nexus/ready are exposed on
// every scope — k8s liveness/readiness probes hit the service's
// public port, and the framework's own peerProber (health.go) probes
// peers through their declared public URL. The rest of /__nexus/* is
// held back from public + internal scopes and only served on admin.
//
// ScopeAdmin allows everything — see ScopeAdmin's doc comment for
// the rationale (operator ergonomics + dashboard testers).
func scopeAllowsPath(scope config.ListenerScope, path string) bool {
	isDash := strings.HasPrefix(path, dashboard.Prefix)
	isHealth := path == dashboard.Prefix+"/health" || path == dashboard.Prefix+"/ready"
	switch scope {
	case config.ScopePublic:
		return !isDash || isHealth
	case config.ScopeInternal:
		return !isDash || isHealth
	case config.ScopeAdmin:
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
// Public-listener synthesis: if `in` declares no public-scoped
// listener, one is added automatically at publicAddr. That makes
// the manifest's `port:` and `listeners:` blocks composable —
// `port: 8080` + `listeners: {admin: {scope: admin}}` produces
// public=:8080, admin=:9080 without operators having to repeat
// the public entry in YAML.
//
// The 1000/2000 offsets are framework conventions — operators who
// need different numbers set Addr explicitly. Returns the filled
// map; doesn't mutate the input.
func fillListenerAddrs(in map[string]config.Listener, publicAddr string) map[string]config.Listener {
	if publicAddr == "" {
		publicAddr = ":8080"
	}
	out := make(map[string]config.Listener, len(in)+1)
	hasPublic := false
	for name, l := range in {
		if l.Scope == config.ScopePublic {
			hasPublic = true
		}
		if l.Addr != "" {
			out[name] = l
			continue
		}
		switch l.Scope {
		case config.ScopePublic:
			l.Addr = publicAddr
		case config.ScopeAdmin:
			if a, err := offsetAddr(publicAddr, 1000); err == nil {
				l.Addr = a
			}
		case config.ScopeInternal:
			if a, err := offsetAddr(publicAddr, 2000); err == nil {
				l.Addr = a
			}
		}
		out[name] = l
	}
	if !hasPublic {
		out["public"] = config.Listener{Addr: publicAddr, Scope: config.ScopePublic}
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
func scopeFilterMiddleware(scopes *listenerScopes) httpx.HandlerFunc {
	return func(c *httpx.Ctx) {
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
