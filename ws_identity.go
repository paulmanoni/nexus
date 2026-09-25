package nexus

import (
	"context"
	"sync"
)

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
