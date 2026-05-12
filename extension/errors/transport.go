package errors

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Transport is the receiver an error report is forwarded to.
// Implementations can be built-in (Sentry, Webhook, Stdout) or
// user-supplied (any type satisfying this interface).
//
// Report must NOT block — the caller already runs it in a goroutine
// with a short timeout, but implementations should respect ctx.Done
// to honor the deadline on their own network operations. Returning
// an error is informational only; the plugin does not retry. If
// you need retries, build them inside the transport.
type Transport interface {
	// Name is the human-readable label shown in the dashboard's
	// errors status panel. Kept distinct from any internal type
	// name so the wire payload can identify the transport without
	// reflecting on the Go type.
	Name() string

	// Report ships one captured Event somewhere. Best-effort; an
	// error return is logged at the call site but does not retry.
	Report(ctx context.Context, e Event) error
}

// Stdout writes each captured error to stderr as a one-line JSON
// object. Useful as a fallback transport during development or in
// environments where the platform's log collector is the receiver.
//
//	errors.Plugin(errors.Config{
//	    Transports: []errors.Transport{errors.Stdout()},
//	})
func Stdout() Transport { return stdoutTransport{} }

type stdoutTransport struct{}

func (stdoutTransport) Name() string { return "stdout" }

func (stdoutTransport) Report(_ context.Context, e Event) error {
	// Encode to a single line so log collectors (Loki, Datadog,
	// the journal) ingest one record per error rather than
	// pretty-printed multi-line stacks they then have to glue
	// back together.
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	log.Printf("nexus errors: %s", b)
	return nil
}

// Webhook POSTs each captured Event to an HTTP endpoint as JSON.
// Use for self-hosted error trackers or any internal endpoint that
// can accept the plugin's native Event shape.
//
//	errors.Webhook("https://errors.example.com/ingest")
//	errors.Webhook("https://...", errors.WithWebhookHeader("X-Token", "abc"))
func Webhook(rawURL string, opts ...WebhookOption) Transport {
	w := &webhookTransport{
		url:    rawURL,
		client: &http.Client{Timeout: 5 * time.Second},
		headers: map[string]string{
			"Content-Type": "application/json",
			"User-Agent":   "nexus-errors/0.1",
		},
	}
	for _, o := range opts {
		o(w)
	}
	return w
}

// WebhookOption is the functional-option shape for Webhook
// customization. Headers and HTTP client are the typical knobs.
type WebhookOption func(*webhookTransport)

// WithWebhookHeader adds a header to every request the transport
// makes. Use for auth (X-Api-Key, Bearer) or routing tags.
func WithWebhookHeader(name, value string) WebhookOption {
	return func(w *webhookTransport) { w.headers[name] = value }
}

// WithWebhookClient overrides the default http.Client. Useful when
// you need a custom TLS config, a proxy, or shared connection
// pooling with the rest of the app.
func WithWebhookClient(c *http.Client) WebhookOption {
	return func(w *webhookTransport) { w.client = c }
}

type webhookTransport struct {
	url     string
	client  *http.Client
	headers map[string]string
}

func (w *webhookTransport) Name() string { return "webhook" }

func (w *webhookTransport) Report(ctx context.Context, e Event) error {
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	for k, v := range w.headers {
		req.Header.Set(k, v)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("webhook: HTTP %d", resp.StatusCode)
	}
	return nil
}

// Sentry POSTs each captured Event to Sentry's store endpoint. The
// minimal direct-HTTP implementation avoids pulling in the
// sentry-go SDK as a hard dependency — operators wanting the full
// SDK (breadcrumbs, scope handling, performance traces) can wire
// their own Transport that wraps sentry.CaptureEvent.
//
//	errors.Sentry(os.Getenv("SENTRY_DSN"))
//
// DSN format: https://<publicKey>@<host>/<projectId>
// The transport parses it once at construction. Invalid DSN yields
// a Sentry transport that errors every Report — the plugin logs
// each error but continues running (the dashboard view still works).
func Sentry(dsn string) Transport {
	t := &sentryTransport{
		dsn:    dsn,
		client: &http.Client{Timeout: 5 * time.Second},
	}
	if dsn != "" {
		t.parse()
	}
	return t
}

type sentryTransport struct {
	dsn       string
	client    *http.Client
	parseErr  error
	endpoint  string // resolved POST URL
	publicKey string
}

func (t *sentryTransport) Name() string { return "sentry" }

// parse decomposes a Sentry DSN into the bits the ingestion endpoint
// needs. Sentry's DSN shape:
//
//	https://<publicKey>@<host>/<projectId>
//
// The store endpoint is:
//
//	<scheme>://<host>/api/<projectId>/store/
//
// and the auth header carries the public key + sdk metadata.
func (t *sentryTransport) parse() {
	u, err := url.Parse(t.dsn)
	if err != nil {
		t.parseErr = fmt.Errorf("sentry: invalid DSN: %w", err)
		return
	}
	if u.User == nil || u.User.Username() == "" {
		t.parseErr = fmt.Errorf("sentry: DSN missing public key")
		return
	}
	t.publicKey = u.User.Username()
	path := strings.Trim(u.Path, "/")
	if path == "" {
		t.parseErr = fmt.Errorf("sentry: DSN missing project ID")
		return
	}
	t.endpoint = fmt.Sprintf("%s://%s/api/%s/store/", u.Scheme, u.Host, path)
}

// sentryEvent is the JSON shape Sentry's store endpoint accepts.
// Pulled from the Sentry SDK developer docs — only the fields we
// actually populate.
type sentryEvent struct {
	EventID     string                `json:"event_id,omitempty"`
	Timestamp   float64               `json:"timestamp"`
	Platform    string                `json:"platform"`
	Level       string                `json:"level,omitempty"`
	Logger      string                `json:"logger,omitempty"`
	ServerName  string                `json:"server_name,omitempty"`
	Release     string                `json:"release,omitempty"`
	Environment string                `json:"environment,omitempty"`
	Tags        map[string]string     `json:"tags,omitempty"`
	Message     map[string]string     `json:"message,omitempty"`
	Exception   *sentryExceptionWrap  `json:"exception,omitempty"`
	Request     *sentryRequest        `json:"request,omitempty"`
	Fingerprint []string              `json:"fingerprint,omitempty"`
}

type sentryExceptionWrap struct {
	Values []sentryException `json:"values"`
}

type sentryException struct {
	Type       string             `json:"type"`
	Value      string             `json:"value"`
	Stacktrace *sentryStackTrace  `json:"stacktrace,omitempty"`
}

type sentryStackTrace struct {
	Frames []sentryFrame `json:"frames"`
}

type sentryFrame struct {
	Function string `json:"function,omitempty"`
	Filename string `json:"filename,omitempty"`
	Lineno   int    `json:"lineno,omitempty"`
}

type sentryRequest struct {
	Method string `json:"method,omitempty"`
	URL    string `json:"url,omitempty"`
}

func (t *sentryTransport) Report(ctx context.Context, e Event) error {
	if t.parseErr != nil {
		return t.parseErr
	}
	if t.endpoint == "" {
		return fmt.Errorf("sentry: not configured")
	}

	payload := sentryEvent{
		Timestamp:   float64(e.CapturedAt.UnixNano()) / 1e9,
		Platform:    "go",
		Level:       "error",
		Logger:      "nexus",
		ServerName:  e.ServerName,
		Release:     e.Release,
		Environment: e.Environment,
		Tags:        map[string]string{"transport": "rest"},
		Fingerprint: []string{e.Fingerprint},
	}
	if e.Service != "" {
		payload.Tags["service"] = e.Service
	}
	if e.TraceID != "" {
		payload.Tags["trace_id"] = e.TraceID
	}
	if e.Error != "" {
		payload.Exception = &sentryExceptionWrap{
			Values: []sentryException{{
				Type:       "error",
				Value:      e.Error,
				Stacktrace: parseStackFrames(e.Stack),
			}},
		}
	} else {
		payload.Message = map[string]string{"message": fmt.Sprintf("HTTP %d", e.Status)}
	}
	if e.Method != "" || e.Path != "" {
		payload.Request = &sentryRequest{Method: e.Method, URL: e.Path}
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Sentry-Auth", t.authHeader())

	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("sentry: HTTP %d", resp.StatusCode)
	}
	return nil
}

// authHeader builds the X-Sentry-Auth value Sentry expects. The
// timestamp uses Unix seconds (Sentry's docs say "the time", any
// precise enough representation works in practice).
func (t *sentryTransport) authHeader() string {
	return fmt.Sprintf(
		"Sentry sentry_version=7, sentry_client=nexus-errors/0.1, sentry_timestamp=%d, sentry_key=%s",
		time.Now().Unix(), t.publicKey,
	)
}

// parseStackFrames best-effort converts a cleaned Go stack trace to
// Sentry's stack-frame array. Go's stack format is:
//
//	github.com/foo/bar.Func(args)
//	    /path/file.go:42 +0xabc
//
// Lines come in pairs — odd line is function, even is location. We
// pair them up and emit one frame per pair. Order is reversed for
// Sentry (deepest frame last, matching what Sentry expects).
func parseStackFrames(stack string) *sentryStackTrace {
	if stack == "" {
		return nil
	}
	lines := strings.Split(stack, "\n")
	frames := make([]sentryFrame, 0, len(lines)/2)
	for i := 0; i+1 < len(lines); i++ {
		fnLine := strings.TrimSpace(lines[i])
		locLine := strings.TrimSpace(lines[i+1])
		if fnLine == "" || strings.HasPrefix(fnLine, "goroutine ") {
			continue
		}
		// Function line shape: "pkg.Func(args)" — keep the part
		// before the first paren so the function name in Sentry
		// is readable.
		fname := fnLine
		if idx := strings.Index(fname, "("); idx > 0 {
			fname = fname[:idx]
		}
		// Location line shape: "/path/file.go:42 +0x100" — split
		// on first space to get the file:line, then split that on
		// the last colon to recover lineno.
		if !strings.Contains(locLine, ".go:") {
			continue
		}
		loc := strings.Fields(locLine)
		if len(loc) == 0 {
			continue
		}
		filePart := loc[0]
		filename, lineno := filePart, 0
		if colon := strings.LastIndex(filePart, ":"); colon > 0 {
			filename = filePart[:colon]
			fmt.Sscanf(filePart[colon+1:], "%d", &lineno)
		}
		frames = append(frames, sentryFrame{
			Function: fname,
			Filename: filename,
			Lineno:   lineno,
		})
		i++ // consumed the location line; advance past it
	}
	// Sentry expects deepest frame last; the Go stack already
	// reads top-down with the panic site at the top, so reverse.
	for i, j := 0, len(frames)-1; i < j; i, j = i+1, j-1 {
		frames[i], frames[j] = frames[j], frames[i]
	}
	return &sentryStackTrace{Frames: frames}
}
