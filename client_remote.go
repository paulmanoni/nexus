package nexus

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
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
// NewRemoteCaller / NewRemoteCallerFromEnv / NewPeerCaller.
type RemoteCaller struct {
	baseURL string
	client  *http.Client
	auth    AuthPropagator

	// localVersion is the version this binary identifies as — typically
	// app.Version() threaded in by the generated client constructor.
	// Empty disables the skew probe entirely (a binary that doesn't
	// stamp its version can't meaningfully detect drift).
	localVersion string
	// minVersion overrides localVersion as the comparison floor when
	// non-empty. Set by NewPeerCaller from Peer.MinVersion so the user
	// can declare an explicit floor independent of the local binary's
	// version stamp.
	minVersion string
	// retries caps automatic retries on transport errors for
	// idempotent verbs only. Zero disables. Set by NewPeerCaller from
	// Peer.Retries.
	retries int
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
		// CheckRedirect short-circuits on the first 3xx so the caller
		// sees the redirect response directly rather than silently
		// landing on a different endpoint. Gin's RedirectTrailingSlash
		// (default ON) will turn a malformed "/users/" into a 301 to
		// "/users" — without this, http.Client follows it and decodes
		// the LIST response as a single-user shape, producing a
		// confusing JSON decode error far from the real bug.
		client: trace.HTTPClient(&http.Client{
			Timeout:       30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}),
		auth: DefaultAuthPropagator(),
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

// WithRemoteTimeout overrides the default 30s per-call timeout on the
// underlying http.Client. NewPeerCaller threads Peer.Timeout through
// here when non-zero. Setting to zero leaves the existing timeout in
// place — use a custom http.Client via WithRemoteHTTPClient if you
// genuinely want to remove timeouts.
func WithRemoteTimeout(d time.Duration) RemoteCallerOption {
	return func(r *RemoteCaller) {
		if d <= 0 || r.client == nil {
			return
		}
		r.client.Timeout = d
	}
}

// WithMinVersion sets a comparison floor for the peer-version skew
// probe that's independent of the local binary's stamped version.
// When set, the probe compares the peer's reported Version against
// this value instead of localVersion. Soft-fail same as today —
// mismatch logs a single warning, the call proceeds.
func WithMinVersion(v string) RemoteCallerOption {
	return func(r *RemoteCaller) { r.minVersion = v }
}

// WithRetries caps automatic retries on transport errors. Only
// idempotent HTTP verbs (GET/HEAD/PUT/DELETE/OPTIONS/TRACE) retry —
// POST and PATCH never do. Backoff between attempts is 50ms * 2^n
// with full jitter. Zero disables retries entirely.
func WithRetries(n int) RemoteCallerOption {
	return func(r *RemoteCaller) {
		if n < 0 {
			n = 0
		}
		r.retries = n
	}
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
func (r *RemoteCaller) Call(ctx context.Context, method, path string, args, out any) (rerr error) {
	// Open a child span so the cross-service HTTP call appears on the
	// dashboard waterfall with method/path/peer-URL/status/error.
	// Errors flow through span.End(rerr); UserError fields surface
	// as attrs (op/hint/notes/cause) for the dashboard to render
	// separately from the flat Error string.
	ctx, span := trace.StartSpan(ctx, "remote "+method+" "+path,
		trace.Str("transport", "remote"),
		trace.Str("method", method),
		trace.Str("path", path),
		trace.Str("peer.url", r.baseURL),
	)
	defer func() {
		attachUserErrorAttrs(span, rerr)
		span.End(rerr)
	}()

	r.checkPeerVersion(ctx)

	finalPath, body, contentType, err := encodeRequest(method, path, args)
	if err != nil {
		return err
	}
	fullURL := r.baseURL + finalPath

	// Body needs to be re-readable across retry attempts; cache the
	// bytes once so http.NewRequestWithContext can take a fresh
	// reader on each attempt. Non-body verbs pass nil.
	var bodyBytes []byte
	if body != nil {
		bodyBytes, err = io.ReadAll(body)
		if err != nil {
			return fmt.Errorf("nexus: read request body: %w", err)
		}
	}

	attempts := 1 + r.retries
	if !methodIsIdempotent(method) {
		attempts = 1
	}

	var resp *http.Response
	for attempt := 0; attempt < attempts; attempt++ {
		var bodyReader io.Reader
		if bodyBytes != nil {
			bodyReader = bytes.NewReader(bodyBytes)
		}
		req, rerr := http.NewRequestWithContext(ctx, method, fullURL, bodyReader)
		if rerr != nil {
			return rerr
		}
		req.Header.Set("Accept", "application/json")
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if r.auth != nil {
			if aerr := r.auth.Inject(ctx, req); aerr != nil {
				return fmt.Errorf("nexus: auth propagator: %w", aerr)
			}
		}
		resp, err = r.client.Do(req)
		if err == nil {
			break
		}
		// Last attempt → bubble the error up. Otherwise sleep with
		// jittered backoff and retry. Honor ctx cancellation while
		// waiting so a deadlined caller doesn't sit out a backoff.
		if attempt == attempts-1 {
			return err
		}
		if werr := backoffWait(ctx, attempt); werr != nil {
			return werr
		}
	}
	defer resp.Body.Close()

	span.Set("status", resp.StatusCode)
	respBody, _ := io.ReadAll(resp.Body)
	return decodeResponse(resp.StatusCode, respBody, method, path, fullURL, out)
}

// methodIsIdempotent reports whether automatic retries are safe for
// the given HTTP method. Mirrors net/http.Transport's convention —
// POST and PATCH are explicitly excluded because they typically
// mutate non-idempotently on the server.
func methodIsIdempotent(method string) bool {
	switch strings.ToUpper(method) {
	case "GET", "HEAD", "PUT", "DELETE", "OPTIONS", "TRACE":
		return true
	}
	return false
}

// backoffWait sleeps for 50ms * 2^attempt with full jitter. Returns
// ctx.Err() if the context is cancelled before the wait completes so
// retries don't outlive their caller's deadline.
func backoffWait(ctx context.Context, attempt int) error {
	base := 50 * time.Millisecond * (1 << attempt)
	d := time.Duration(rand.Int63n(int64(base) + 1))
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// checkPeerVersion runs the one-shot version probe. The atomic flag
// makes the fast path lock-free for every call after the first.
//
// A successful probe stores the peer's version on the caller for
// observability; a failed probe (network blip, 404, etc.) is silent
// — the actual user call will surface real errors anyway, and we'd
// rather not spam logs about config-endpoint quirks.
func (r *RemoteCaller) checkPeerVersion(ctx context.Context) {
	// Floor selection: explicit minVersion wins, otherwise compare
	// against the local binary's stamped version. Both empty disables
	// the probe entirely.
	floor := r.minVersion
	if floor == "" {
		floor = r.localVersion
	}
	if floor == "" || r.versionProbed.Load() {
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
	if cfg.Version == "" || cfg.Version == floor {
		return
	}
	log.Printf("nexus warning [version skew]: peer at %s reports version %q; expected %q\n  hint: rebuild and redeploy peers together, or set Peer.MinVersion in Topology to suppress this warning when skew is intentional",
		r.baseURL, cfg.Version, floor)
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
	ue := &UserError{
		Op:  "remote call",
		Msg: fmt.Sprintf("%s %s → %d %s", e.Method, e.TargetPath, e.Status, http.StatusText(e.Status)),
	}
	if e.TargetURL != "" {
		ue.Notes = append(ue.Notes, "url: "+e.TargetURL)
	}
	if e.Message != "" {
		ue.Notes = append(ue.Notes, "peer message: "+e.Message)
	} else if len(e.RawBody) > 0 {
		ue.Notes = append(ue.Notes, "peer body: "+truncate(string(e.RawBody), 200))
	}
	ue.Hint = remoteStatusHint(e.Status)
	return ue.Error()
}

// remoteStatusHint returns a one-line fix recipe for the common
// 4xx/5xx classes a developer hits during cross-service work. Empty
// for statuses where no generic hint helps (custom 4xx, 422, etc.).
func remoteStatusHint(status int) string {
	switch {
	case status == 401:
		return "peer rejected credentials — check that the inbound Authorization header is forwarding (or set Peer.Auth in Topology)"
	case status == 403:
		return "peer accepted credentials but denied the action — check the caller's permissions on the target resource"
	case status == 404:
		return "peer has no handler at this path — confirm the AsRest/AsQuery declaration on the target module matches the client's verb+path"
	case status == 405:
		return "peer has a handler at this path but not for this method — check the verb on AsRest"
	case status == 408 || status == 504:
		return "peer timed out — increase Peer.Timeout in Topology or investigate slow handlers on the target"
	case status == 429:
		return "peer rate-limited the call — reduce traffic or raise the bucket on the peer's RateLimit declaration"
	case status >= 500 && status < 600:
		return "peer returned a server error — check the peer's logs; transient 5xx are auto-retried for idempotent verbs when Peer.Retries > 0"
	}
	return ""
}