package nexus

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/paulmanoni/nexus/trace"
)

// RemoteCaller is the HTTP variant of a generated cross-module client.
// One per peer service: BaseURL points at the peer's HTTP root, the
// embedded http.Client is wrapped via trace.HTTPClient so traceparent
// is auto-injected on every call (request stitching across services
// is free).
//
// Generated client code never constructs this directly — see
// NewRemoteCaller / NewRemoteCallerFromEnv.
type RemoteCaller struct {
	baseURL string
	client  *http.Client
	auth    AuthPropagator

	// localVersion is the version this binary identifies as — typically
	// app.Version() threaded in by the generated client constructor.
	// Empty disables the skew probe entirely (a binary that doesn't
	// stamp its version can't meaningfully detect drift).
	localVersion string
	// versionProbed flips to true exactly once, the first time Call
	// has fetched the peer's /__nexus/config. After that we've either
	// learned the peer's version (and logged any skew) or we've
	// failed silently — either way we don't probe again.
	versionProbed atomic.Bool
	// versionMu protects the one-shot probe from concurrent first
	// calls racing each other to issue duplicate HTTP requests.
	versionMu sync.Mutex
}

// NewRemoteCaller wraps a base URL with default settings: 30s timeout,
// trace.HTTPClient for traceparent injection, default auth propagator
// (Authorization header forwarding).
//
// Trailing slashes on baseURL are trimmed so callers don't double-slash
// when their handler paths begin with "/".
func NewRemoteCaller(baseURL string, opts ...RemoteCallerOption) *RemoteCaller {
	c := &RemoteCaller{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  trace.HTTPClient(&http.Client{Timeout: 30 * time.Second}),
		auth:    DefaultAuthPropagator(),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// NewRemoteCallerFromEnv reads the named env var for the base URL.
// Generated clients use this so a deployment can wire peer URLs
// through the standard Kubernetes-style envFrom pattern. Panics at
// boot if the env var is unset — fail fast beats a runtime nil-deref
// on the first cross-service call.
func NewRemoteCallerFromEnv(envVar string, opts ...RemoteCallerOption) *RemoteCaller {
	url := os.Getenv(envVar)
	if url == "" {
		panic(fmt.Sprintf("nexus: %s is required for the remote client (set it to the peer's HTTP base URL)", envVar))
	}
	return NewRemoteCaller(url, opts...)
}

// RemoteCallerOption tunes a RemoteCaller. Functional-option pattern
// matches AppOption — keeps the constructor signature stable as new
// knobs land.
type RemoteCallerOption func(*RemoteCaller)

// WithRemoteHTTPClient swaps the default http.Client. Callers that
// already wrap their own client in trace.HTTPClient (for custom
// transports, retries, mTLS) pass it through here. Note: nexus does
// not re-wrap — if you opt out of trace.HTTPClient you opt out of
// automatic traceparent injection.
func WithRemoteHTTPClient(c *http.Client) RemoteCallerOption {
	return func(r *RemoteCaller) { r.client = c }
}

// WithAuthPropagator swaps the default Authorization-forwarding
// propagator for a service-token minter or any other custom
// strategy.
func WithAuthPropagator(p AuthPropagator) RemoteCallerOption {
	return func(r *RemoteCaller) { r.auth = p }
}

// WithLocalVersion stamps the caller's own version onto the
// RemoteCaller so it can detect peer-version skew on the first
// HTTP call. Generated client constructors thread app.Version()
// in here so deployments where service A is on v2 and service B on
// v1 surface a single warning line instead of being a silent source
// of "weird microservice bugs."
//
// Empty version disables the probe (a binary without a stamped
// version can't meaningfully claim drift).
func WithLocalVersion(v string) RemoteCallerOption {
	return func(r *RemoteCaller) { r.localVersion = v }
}

// Invoke is an alias for Call exposed so RemoteCaller satisfies the
// same ClientCallable interface as LocalInvoker. New code should
// prefer Invoke for shape consistency across the in-process and HTTP
// paths; Call stays for backward compatibility with already-generated
// client files.
func (r *RemoteCaller) Invoke(ctx context.Context, method, path string, args, out any) error {
	return r.Call(ctx, method, path, args, out)
}

// Call serializes args into the appropriate place (path, body, query),
// dispatches the request, and decodes the JSON response into out.
// Pointer-to-pointer is fine — ListUsers returns *[]*User, so callers
// pass &out where out is *[]*User.
//
// Non-2xx responses become *RemoteError with the status code and the
// server's response body (best-effort JSON-decoded into the .Body
// field). 5xx errors are still considered "the call returned" — they
// don't trigger retries here; the caller decides.
//
// First-call side effect: if WithLocalVersion was set, the caller
// fetches the peer's /__nexus/config once and logs a single warning
// line on version skew. The probe never fails the user's call —
// errors fetching config are swallowed so an unrelated transient on
// the peer doesn't masquerade as the caller's request failing.
func (r *RemoteCaller) Call(ctx context.Context, method, path string, args, out any) error {
	r.checkPeerVersion(ctx)

	finalPath, body, contentType, err := encodeRequest(method, path, args)
	if err != nil {
		return err
	}
	fullURL := r.baseURL + finalPath

	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if r.auth != nil {
		if err := r.auth.Inject(ctx, req); err != nil {
			return fmt.Errorf("nexus: auth propagator: %w", err)
		}
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	return decodeResponse(resp.StatusCode, respBody, method, path, fullURL, out)
}

// checkPeerVersion runs the one-shot version probe. The atomic flag
// makes the fast path lock-free for every call after the first.
//
// A successful probe stores the peer's version on the caller for
// observability; a failed probe (network blip, 404, etc.) is silent
// — the actual user call will surface real errors anyway, and we'd
// rather not spam logs about config-endpoint quirks.
func (r *RemoteCaller) checkPeerVersion(ctx context.Context) {
	if r.localVersion == "" || r.versionProbed.Load() {
		return
	}
	r.versionMu.Lock()
	defer r.versionMu.Unlock()
	// Re-check inside the lock — a concurrent caller may have just
	// finished the probe.
	if r.versionProbed.Load() {
		return
	}
	defer r.versionProbed.Store(true)

	// Use a tight per-call context so a hung config endpoint doesn't
	// hold up the user's request behind it. 2s is generous for a
	// localhost call, sufficient for an intra-cluster one.
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, r.baseURL+"/__nexus/config", nil)
	if err != nil {
		return
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	var cfg struct {
		Version string `json:"Version"`
	}
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &cfg); err != nil {
		return
	}
	if cfg.Version == "" || cfg.Version == r.localVersion {
		return
	}
	log.Printf("nexus: peer at %s reports version %q; this binary is on %q — possible deployment skew",
		r.baseURL, cfg.Version, r.localVersion)
}

// RemoteError is what a non-2xx peer response unmarshals to. Generated
// clients return it through the function's `error` slot so callers can
// type-assert and react on Status without parsing strings.
type RemoteError struct {
	Status     int    // HTTP status code from the peer
	Method     string // request method
	TargetPath string // logical path before substitution (the AsRest path)
	TargetURL  string // the full URL we hit (for log debugging)
	Message    string // best-effort extracted from the body
	RawBody    []byte // raw response body when JSON decode didn't yield Message
}

func (e *RemoteError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("nexus remote: %s %s: %d %s", e.Method, e.TargetPath, e.Status, e.Message)
	}
	return fmt.Sprintf("nexus remote: %s %s: %d", e.Method, e.TargetPath, e.Status)
}