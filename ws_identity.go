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
