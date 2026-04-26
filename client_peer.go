package nexus

import (
	"context"
	"net/http"
)

// NewPeerCaller builds a RemoteCaller from a Topology Peer entry.
// Generated client constructors call this when the active deployment
// differs from the target module's tag, replacing the older
// NewRemoteCallerFromEnv path that read a hard-coded env var.
//
// Field mapping:
//   - peer.URL       → base URL (required; empty panics — codegen
//                      should resolve missing-peer cases before
//                      reaching this constructor)
//   - peer.Timeout   → WithRemoteTimeout
//   - peer.Auth      → wrapped as an AuthPropagator that sets the
//                      returned string on the Authorization header
//   - peer.MinVersion→ WithMinVersion (overrides localVersion as
//                      the skew-probe floor)
//   - peer.Retries   → WithRetries (idempotent verbs only)
//
// Additional opts run after the peer-derived options so callers can
// override on a per-client basis (e.g. WithLocalVersion to thread
// app.Version() in for the skew probe).
func NewPeerCaller(peer Peer, opts ...RemoteCallerOption) *RemoteCaller {
	if peer.URL == "" {
		panic("nexus: NewPeerCaller called with empty Peer.URL — generated clients should resolve this before construction")
	}
	derived := []RemoteCallerOption{}
	if peer.Timeout > 0 {
		derived = append(derived, WithRemoteTimeout(peer.Timeout))
	}
	if peer.Auth != nil {
		derived = append(derived, WithAuthPropagator(peerAuthAdapter{fn: peer.Auth}))
	}
	if peer.MinVersion != "" {
		derived = append(derived, WithMinVersion(peer.MinVersion))
	}
	if peer.Retries > 0 {
		derived = append(derived, WithRetries(peer.Retries))
	}
	return NewRemoteCaller(peer.URL, append(derived, opts...)...)
}

// peerAuthAdapter bridges Peer.Auth (returns header value) to the
// AuthPropagator interface (sets the header on a request). Keeps
// Peer's API simple — users supply a function that returns the
// Authorization value, the framework does the http.Request plumbing.
type peerAuthAdapter struct {
	fn func(ctx context.Context) (string, error)
}

func (a peerAuthAdapter) Inject(ctx context.Context, req *http.Request) error {
	h, err := a.fn(ctx)
	if err != nil {
		return err
	}
	if h != "" {
		req.Header.Set("Authorization", h)
	}
	return nil
}