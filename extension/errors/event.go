package errors

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"time"

	"github.com/paulmanoni/nexus/trace"
)

// Event is the plugin's captured-error shape — distinct from
// trace.Event (the framework's low-level wire format) so the
// dashboard + transports can reason about errors without leaking
// every span-level field a request carries.
//
// Event is what Transports.Report() receives + what the dashboard
// serializes.
type Event struct {
	// Fingerprint is the grouping key — same fingerprint = same
	// "issue". Combines request method + path + the top stack
	// frame so the dashboard can show "this 500 fired 47 times in
	// the last hour" as a single row.
	Fingerprint string `json:"fingerprint"`

	// CapturedAt is the moment the plugin received the event.
	// Distinct from the trace event's Timestamp (which is when
	// the underlying span ended) by usually <1ms; tracked for
	// completeness.
	CapturedAt time.Time `json:"capturedAt"`

	// Service / Endpoint / Method / Path / Status mirror what was
	// in the trace event. Carried into the captured event so
	// transports don't need a back-reference.
	Service  string `json:"service,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	Method   string `json:"method,omitempty"`
	Path     string `json:"path,omitempty"`
	Status   int    `json:"status,omitempty"`

	// Error is the human-readable error message. Empty when the
	// event was captured purely on Status >= 500 (server emitted
	// 500 with no error in c.Errors).
	Error string `json:"error,omitempty"`

	// Stack is the cleaned panic stack (debug.Stack output after
	// trace.CleanStack stripping). Empty when the error wasn't a
	// panic (returned-error or manual 500).
	Stack string `json:"stack,omitempty"`

	// Environment / Release / ServerName are tagging fields the
	// receiver uses to filter + group across deploys. Mirror the
	// fields Sentry expects so the Sentry transport doesn't need a
	// separate model.
	Environment string `json:"environment,omitempty"`
	Release     string `json:"release,omitempty"`
	ServerName  string `json:"serverName,omitempty"`

	// TraceID lets the receiver link back to the full request
	// trace via the dashboard's /__nexus/traces/<id>. Surfaces in
	// the Sentry / webhook payload as a tag.
	TraceID string `json:"traceId,omitempty"`
}

// newEventFromTrace builds an Event from a trace.Event + the
// resolved Config tags. Pure — no I/O.
func newEventFromTrace(ev trace.Event, cfg Config) Event {
	out := Event{
		CapturedAt:  time.Now().UTC(),
		Service:     ev.Service,
		Endpoint:    ev.Endpoint,
		Method:      ev.Method,
		Path:        ev.Path,
		Status:      ev.Status,
		Error:       ev.Error,
		Stack:       ev.Stack,
		Environment: cfg.Environment,
		Release:     cfg.Release,
		ServerName:  cfg.ServerName,
		TraceID:     ev.TraceID,
	}
	out.Fingerprint = fingerprint(out)
	return out
}

// fingerprint hashes the stable bits of an event into a short
// identifier. Two events with the same fingerprint are the same
// "issue" — they group on the dashboard, count up rather than
// flood the recent list.
//
// Inputs (in this order):
//   - Method
//   - Path
//   - First user-code frame from Stack when present, else Error
//
// We avoid the full stack in the hash because line numbers shift
// across releases; the top frame's "package.Function" is stable
// across patch versions and is what operators actually want to
// group on.
func fingerprint(e Event) string {
	h := sha256.New()
	h.Write([]byte(e.Method))
	h.Write([]byte{0})
	h.Write([]byte(e.Path))
	h.Write([]byte{0})
	if e.Stack != "" {
		h.Write([]byte(topFrame(e.Stack)))
	} else {
		h.Write([]byte(e.Error))
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:8]) // 16-char hex — collisions are absurdly unlikely at this scale
}

// topFrame returns the "package.Function" of the topmost frame in
// a cleaned stack trace. Stack format (post trace.CleanStack) is:
//
//	goroutine N [running]:
//	github.com/foo/bar.Func(...)
//	    /path/to/file.go:42 +0xabc
//	...
//
// We scan for the first line that looks like a function call
// signature ("pkg/Path.Func(...)") and return everything before the
// "(". Returns "" on edge-case inputs — the caller's hash absorbs
// the empty value without misgrouping.
func topFrame(stack string) string {
	for _, raw := range strings.Split(stack, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		// Skip the goroutine header.
		if strings.HasPrefix(line, "goroutine ") {
			continue
		}
		// Skip file:line lines (they start with a path char).
		if strings.HasPrefix(line, "/") || strings.Contains(line, ".go:") {
			continue
		}
		// First line with a "(" is the function call line.
		if idx := strings.Index(line, "("); idx > 0 {
			return line[:idx]
		}
	}
	return ""
}

// shouldSample decides whether a captured event should reach
// transports, based on Config.SampleRate. Uses time.Now's nanosecond
// remainder as the entropy source so we don't pull in crypto/rand
// for a sampling decision — non-cryptographic, but uniformly
// distributed enough for sampling.
func shouldSample(rate float64) bool {
	if rate >= 1.0 {
		return true
	}
	if rate <= 0.0 {
		return false
	}
	// 0.1 → keep when (ns % 1000) < 100
	bucket := time.Now().UnixNano() % 1000
	threshold := int64(rate * 1000)
	return bucket < threshold
}

// getOSHostname is a wrapper around os.Hostname so tests can
// substitute it. Kept package-level in errors.go via osHostname
// variable; this is the real implementation.
func getOSHostname() (string, error) {
	return os.Hostname()
}
