package peer

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/paulmanoni/nexus/trace"
)

// Registry holds one persistent HTTP/2 client per declared peer.
// Constructors that need to call peers ask fx for a *Registry; the
// peer.Module() option provides it.
//
// Registry is read-only after construction — peers are declared in
// Config and don't change at runtime. DNS resolution may move under
// us, but that's the http.Transport's concern.
type Registry struct {
	peers    map[string]*peerConn
	identity string
	authMode AuthMode
	secrets  map[string]string
}

// peerConn is one tracked peer connection. The http.Client uses
// HTTP/2 by default (ForceAttemptHTTP2 + TLS); transport pooling
// reuses one TCP/TLS session for every Call to this peer.
type peerConn struct {
	name       string
	url        string
	httpClient *http.Client
	sem        chan struct{} // per-peer concurrency limit
	ready      atomic.Bool   // flipped false by the prober on probe failure
}

// NewRegistry is provided into the fx graph by peer.Module. It
// dials lazily — the *http.Transport's HTTP/2 connection is
// established on the first Call, not at construction. Boot stays
// fast even with N peers configured; the cost of unused entries is
// the small fixed struct.
func NewRegistry(cfg Config) (*Registry, error) {
	r := &Registry{
		peers:    make(map[string]*peerConn, len(cfg.Peers)),
		identity: cfg.Identity,
		authMode: cfg.AuthMode,
		secrets:  cfg.HMACSecrets,
	}
	for name, spec := range cfg.Peers {
		client, err := buildPeerHTTPClient(cfg.TLS, spec, cfg.AuthMode)
		if err != nil {
			return nil, fmt.Errorf("peer %q: %w", name, err)
		}
		concurrency := spec.MaxConcurrent
		if concurrency <= 0 {
			concurrency = 64
		}
		pc := &peerConn{
			name:       name,
			url:        spec.URL,
			httpClient: client,
			sem:        make(chan struct{}, concurrency),
		}
		pc.ready.Store(true) // optimistic until the prober proves otherwise
		r.peers[name] = pc
	}
	return r, nil
}

// IsHealthy reports whether the named peer is currently considered
// reachable. Callers can use this to short-circuit calls to known-
// down peers without paying the dial timeout, or to surface "degraded
// mode" UX. Unknown peer names return false.
func (r *Registry) IsHealthy(name string) bool {
	pc, ok := r.peers[name]
	return ok && pc.ready.Load()
}

// Call dispatches a typed peer call. The generic parameter Out is
// the only place a type appears — Go can't infer it from a string
// method name, so callers spell it explicitly:
//
//	order, err := peer.Call[*Order](ctx, registry, "orders-svc",
//	    "createOrder", CreateArgs{UserID: "u1"})
//
// Network failures return a wrapped error tagged "TRANSPORT".
// Application errors returned by the remote handler arrive as
// *peer.Error, fully typed; callers that care about Code can:
//
//	var pe *peer.Error
//	if errors.As(err, &pe) && pe.Code == "VALIDATION" { ... }
//
// The caller's context deadline propagates as Envelope.Deadline so
// the server-side handler honors it without re-implementing
// deadline arithmetic.
func Call[Out any](
	ctx context.Context,
	r *Registry,
	peerName, method string,
	args any,
) (out Out, err error) {
	// Named return so the deferred span.End below can pick up the
	// final error without an extra trailing variable.
	var zero Out
	if r == nil {
		return zero, errors.New("peer.Call: nil Registry")
	}
	pc, ok := r.peers[peerName]
	if !ok {
		return zero, fmt.Errorf("peer.Call: peer %q not declared in Config.Peers", peerName)
	}
	if !pc.ready.Load() {
		// Fast-fail. Without this, every call to a known-down
		// peer would queue behind the dial timeout, draining
		// the per-peer semaphore until upstream callers stack
		// up too.
		return zero, fmt.Errorf("peer.Call %q: peer marked unhealthy", peerName)
	}

	// Acquire a per-peer concurrency slot. One slow peer can't
	// starve every other call site; the caller's ctx decides
	// whether to wait or fail when the budget is full.
	select {
	case pc.sem <- struct{}{}:
		defer func() { <-pc.sem }()
	case <-ctx.Done():
		return zero, fmt.Errorf("peer.Call %q: %w", peerName, ctx.Err())
	}

	// Emit a peer.call span so the outbound shows up on the
	// dashboard's trace waterfall as a child of the caller's
	// inbound request span. trace.HTTPClient (wrapped into
	// pc.httpClient at construction time) handles the actual
	// traceparent header injection on the outbound request,
	// reading the span ID off this ctx.
	ctx, span := trace.StartSpan(ctx, "peer.call "+method,
		trace.Str("peer.name", peerName),
		trace.Str("peer.method", method),
		trace.Str("peer.url", pc.url))
	defer func() { span.End(err) }()

	var argsRaw json.RawMessage
	if args != nil {
		raw, err := json.Marshal(args)
		if err != nil {
			return zero, fmt.Errorf("peer.Call %q.%s: marshal args: %w", peerName, method, err)
		}
		argsRaw = raw
	}
	env := Envelope{Method: method, Args: argsRaw}
	if dl, ok := ctx.Deadline(); ok {
		env.Deadline = dl.UnixNano()
	}
	body, _ := json.Marshal(env)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		pc.url+"/__peer/call", bytes.NewReader(body))
	if err != nil {
		return zero, fmt.Errorf("peer.Call %q.%s: build request: %w", peerName, method, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Nexus-Peer", r.identity)
	if r.authMode == AuthHMAC {
		if err := signHMACRequest(req, body, r.identity, r.secrets[peerName]); err != nil {
			return zero, fmt.Errorf("peer.Call %q.%s: sign: %w", peerName, method, err)
		}
	}

	// TODO trace.go: stamp traceparent here so the inbound side
	// can parent its span to the caller's request span. Reuses
	// nexus/trace's existing propagation helpers — three lines.

	resp, err := pc.httpClient.Do(req)
	if err != nil {
		// Transport failure — let the prober decide whether to
		// eject. A single timeout isn't enough to call the peer
		// dead; we'd false-alarm during transient blips.
		return zero, fmt.Errorf("peer.Call %q.%s: transport: %w", peerName, method, err)
	}
	defer resp.Body.Close()

	var reply Envelope
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		return zero, fmt.Errorf("peer.Call %q.%s: decode reply: %w", peerName, method, err)
	}
	if reply.Err != nil {
		return zero, reply.Err
	}
	if len(reply.Result) == 0 {
		// Void/error-only methods return zero with no error.
		return zero, nil
	}
	if err := json.Unmarshal(reply.Result, &zero); err != nil {
		return zero, fmt.Errorf("peer.Call %q.%s: decode result into %T: %w",
			peerName, method, zero, err)
	}
	return zero, nil
}

// signHMACRequest computes the HMAC bearer token described in
// server.go's verifyHMAC and stamps it onto the Authorization
// header. Symmetric to the verifier so the two stay in lockstep.
func signHMACRequest(req *http.Request, body []byte, identity, secret string) error {
	if secret == "" {
		return errors.New("no HMAC secret configured for this peer")
	}
	ts := time.Now().Unix()
	bodyHash := sha256.Sum256(body)
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%s:%d:%s", identity, ts, hex.EncodeToString(bodyHash[:]))
	sig := hex.EncodeToString(mac.Sum(nil))
	req.Header.Set("Authorization",
		"Nexus-HMAC "+identity+":"+strconv.FormatInt(ts, 10)+":"+sig)
	return nil
}

// buildPeerHTTPClient assembles the HTTP/2 client for one peer.
// mTLS clients carry the configured cert; HMAC clients skip the
// cert but still terminate TLS (peer protocol is always over TLS
// outside of dev mode).
func buildPeerHTTPClient(global TLSConfig, spec PeerSpec, mode AuthMode) (*http.Client, error) {
	transport := &http.Transport{
		ForceAttemptHTTP2:   true,
		MaxIdleConnsPerHost: 1, // one multiplexed connection per peer
		IdleConnTimeout:     5 * time.Minute,
	}
	switch mode {
	case AuthMTLS:
		tlsCfg, err := buildClientTLSConfig(global, spec)
		if err != nil {
			return nil, err
		}
		transport.TLSClientConfig = tlsCfg
	case AuthHMAC, AuthNone:
		// Still terminate TLS; the server's cert is verified
		// against the system root pool (or the peer-pinned CA
		// if configured). HMAC adds auth on top of TLS, not
		// instead of it.
		tlsCfg, err := buildClientTLSConfigNoMTLS(global, spec)
		if err != nil {
			return nil, err
		}
		transport.TLSClientConfig = tlsCfg
	}
	// Wrap with trace.HTTPClient so every outbound request reads
	// the current span off ctx and stamps a W3C traceparent
	// header. The server side reads it via trace.ParseTraceparent
	// + trace.WithRemoteParent, so the inbound peer.handle span
	// parents to the caller's peer.call span on the dashboard's
	// waterfall — no manual header plumbing in Call itself.
	inner := &http.Client{
		Transport: transport,
		Timeout:   spec.RequestTimeout, // 0 = rely on ctx deadline
	}
	return trace.HTTPClient(inner), nil
}
