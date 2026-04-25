package nexus

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
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

// Call serializes args into the appropriate place (path, body, query),
// dispatches the request, and decodes the JSON response into out.
// Pointer-to-pointer is fine — ListUsers returns *[]*User, so callers
// pass &out where out is *[]*User.
//
// Non-2xx responses become *RemoteError with the status code and the
// server's response body (best-effort JSON-decoded into the .Body
// field). 5xx errors are still considered "the call returned" — they
// don't trigger retries here; the caller decides.
func (r *RemoteCaller) Call(ctx context.Context, method, path string, args, out any) error {
	expanded, err := expandPath(path, args)
	if err != nil {
		return err
	}

	fullURL := r.baseURL + expanded
	var body io.Reader
	contentType := ""
	if methodHasBody(method) {
		b, err := encodeJSONBody(args)
		if err != nil {
			return fmt.Errorf("nexus: encode body: %w", err)
		}
		if b != nil {
			body = bytes.NewReader(b)
			contentType = "application/json"
		}
	} else {
		qs, err := encodeQuery(args)
		if err != nil {
			return fmt.Errorf("nexus: encode query: %w", err)
		}
		if qs != "" {
			fullURL += "?" + qs
		}
	}

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
	if resp.StatusCode >= 400 {
		re := &RemoteError{
			Status:     resp.StatusCode,
			RawBody:    respBody,
			TargetURL:  fullURL,
			TargetPath: path,
			Method:     method,
		}
		// Best-effort decode of the server's error envelope. Most nexus
		// handlers return {"error": "..."} on non-2xx; we surface that
		// as Message when present.
		var env struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		if json.Unmarshal(respBody, &env) == nil {
			if env.Error != "" {
				re.Message = env.Error
			} else if env.Message != "" {
				re.Message = env.Message
			}
		}
		return re
	}

	if out == nil || len(respBody) == 0 {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("nexus: decode response from %s: %w", fullURL, err)
	}
	return nil
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