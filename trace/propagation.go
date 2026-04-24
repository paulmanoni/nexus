package trace

import (
	"context"
	"net/http"
	"strings"
)

// parseTraceparent parses a W3C `traceparent` header value:
//
//	00-<32-hex traceId>-<16-hex spanId>-<2-hex flags>
//
// On success it returns the trace and parent span IDs. All-zero IDs are
// invalid per spec (§3.2.2.3, §3.2.2.4) and treated as absent. An unknown
// version byte (anything non-"00") is rejected so a future upgrade doesn't
// silently misinterpret new formats.
func parseTraceparent(h string) (traceID, parentSpanID string, ok bool) {
	h = strings.TrimSpace(h)
	if h == "" {
		return "", "", false
	}
	parts := strings.Split(h, "-")
	if len(parts) != 4 {
		return "", "", false
	}
	if parts[0] != "00" {
		return "", "", false
	}
	if len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return "", "", false
	}
	if !isHex(parts[1]) || !isHex(parts[2]) || !isHex(parts[3]) {
		return "", "", false
	}
	if allZeros(parts[1]) || allZeros(parts[2]) {
		return "", "", false
	}
	return parts[1], parts[2], true
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func allZeros(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '0' {
			return false
		}
	}
	return true
}

// InjectHeader writes a W3C traceparent into h using the current span from
// ctx. No-op if ctx carries no span, or the span's IDs are the wrong length
// (defensive — our minting always produces the right shape).
//
// The sampled flag is always set ("01") — nexus samples every request.
func InjectHeader(ctx context.Context, h http.Header) {
	span, ok := SpanFromCtx(ctx)
	if !ok || span == nil {
		return
	}
	if len(span.TraceID) != 32 || len(span.SpanID) != 16 {
		return
	}
	h.Set("traceparent", "00-"+span.TraceID+"-"+span.SpanID+"-01")
}

// HTTPClient wraps an *http.Client so every outbound request auto-injects a
// traceparent header derived from the ctx on the request. Use it to stitch a
// trace across service boundaries:
//
//	client := trace.HTTPClient(nil) // or trace.HTTPClient(&myClient)
//	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
//	resp, err := client.Do(req)
//
// If the request already has a traceparent set (e.g. because the caller
// injected one manually), it's left alone.
func HTTPClient(inner *http.Client) *http.Client {
	if inner == nil {
		c := *http.DefaultClient
		inner = &c
	} else {
		c := *inner
		inner = &c
	}
	base := inner.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	inner.Transport = traceTransport{base: base}
	return inner
}

type traceTransport struct{ base http.RoundTripper }

func (t traceTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Header.Get("traceparent") == "" {
		InjectHeader(r.Context(), r.Header)
	}
	return t.base.RoundTrip(r)
}