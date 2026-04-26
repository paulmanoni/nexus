package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/paulmanoni/nexus/trace"
)

// streamUnitEvents subscribes to a single unit's /__nexus/events
// WebSocket and writes formatted request/cross-service-call lines to
// w. Re-dials on disconnect so a subprocess restart doesn't kill the
// log feed permanently. The unit's color and tag prefix are reused so
// streamed lines align with the rest of the terminal output.
//
// Filtered: only events that matter to a developer following request
// flow are printed (KindRequestStart/End at this service, KindSpanEnd
// for cross-service "remote ..." calls). Internal spans, downstream
// markers, and noise are dropped.
func streamUnitEvents(ctx context.Context, u unit, idx int, w io.Writer) {
	wsURL := url.URL{
		Scheme: "ws",
		Host:   fmt.Sprintf("localhost:%d", u.Port),
		Path:   "/__nexus/events",
	}
	prefix := tagPrefix(u.Tag, idx)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		dialCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		conn, _, err := websocket.DefaultDialer.DialContext(dialCtx, wsURL.String(), nil)
		cancel()
		if err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}

		readEvents(ctx, conn, prefix, w)
		_ = conn.Close()
	}
}

// readEvents pumps trace.Event JSON messages off the WebSocket and
// writes the formatted lines until the connection closes or ctx is
// cancelled. Errors silently end the loop — the caller's reconnect
// logic in streamUnitEvents takes over.
func readEvents(ctx context.Context, conn *websocket.Conn, prefix string, w io.Writer) {
	done := make(chan struct{})
	go func() {
		<-ctx.Done()
		_ = conn.Close()
		close(done)
	}()
	defer func() {
		select {
		case <-done:
		default:
		}
	}()

	for {
		var e trace.Event
		if err := conn.ReadJSON(&e); err != nil {
			return
		}
		line := formatTraceEvent(prefix, e)
		if line != "" {
			fmt.Fprintln(w, line)
		}
	}
}

// formatTraceEvent renders one event into a single line ready for
// the prefix writer. Returns "" when the event isn't useful to show
// in the dev terminal (internal middleware spans, downstream
// markers, span.start mirrors of span.end, etc.).
//
// Output shapes:
//
//	checkout-svc → POST /checkout                                [a3f1c2]
//	checkout-svc   ↳ remote GET /users/:id → 200 1.2ms           [a3f1c2]
//	users-svc    → GET /users/:id                                [a3f1c2]
//	users-svc    ← GET /users/:id 200 0.4ms                      [a3f1c2]
//	checkout-svc ← POST /checkout 200 3.1ms                      [a3f1c2]
//
// Lines from different traces share the same trace ID short-form so
// the developer can correlate concurrent requests visually.
func formatTraceEvent(prefix string, e trace.Event) string {
	short := traceIDShort(e.TraceID)
	switch e.Kind {
	case trace.KindRequestStart:
		// Skip dashboard/health internal traffic — drowns useful
		// signal when the dashboard is open in a browser.
		if strings.HasPrefix(e.Path, "/__nexus/") {
			return ""
		}
		return fmt.Sprintf("%s %s→%s %s %s %s[%s]%s",
			prefix,
			ansiCyan, ansiReset,
			e.Method, e.Path,
			ansiDim, short, ansiReset)

	case trace.KindRequestEnd:
		if strings.HasPrefix(e.Path, "/__nexus/") {
			return ""
		}
		statusCol := statusColor(e.Status)
		errSuffix := ""
		if e.Error != "" {
			errSuffix = fmt.Sprintf(" %serr=%s%s", ansiRed, truncErrLine(e.Error), ansiReset)
		}
		return fmt.Sprintf("%s %s←%s %s %s %s%d%s %s%dms%s %s[%s]%s%s",
			prefix,
			ansiCyan, ansiReset,
			e.Method, e.Path,
			statusCol, e.Status, ansiReset,
			ansiDim, e.DurationMs, ansiReset,
			ansiDim, short, ansiReset,
			errSuffix)

	case trace.KindSpanEnd:
		// Only show cross-service spans. The local + remote client
		// wrappers we wired in client_local.go and client_remote.go
		// emit Name="local <verb> <path>" and "remote <verb> <path>"
		// — those are the cross-module calls a developer cares
		// about. Other spans (DB calls, internal sub-ops) stay quiet
		// in dev split mode; they're visible on the dashboard.
		if !isClientSpan(e.Name) {
			return ""
		}
		statusCol := ansiGreen
		statusText := "ok"
		if e.Error != "" {
			statusCol = ansiRed
			statusText = "err"
		}
		// Try to surface the peer service from the span attrs we
		// stamped (peer.url) — it's nil for local spans, populated
		// for remote ones.
		peerSuffix := ""
		if peerURL, ok := e.Meta["peer.url"].(string); ok && peerURL != "" {
			peerSuffix = fmt.Sprintf(" %s→ %s%s", ansiDim, peerURL, ansiReset)
		}
		return fmt.Sprintf("%s   %s↳%s %s %s%s%s %s%dms%s%s %s[%s]%s",
			prefix,
			ansiYellow, ansiReset,
			e.Name,
			statusCol, statusText, ansiReset,
			ansiDim, e.DurationMs, ansiReset,
			peerSuffix,
			ansiDim, short, ansiReset)
	}
	return ""
}

// isClientSpan recognizes the span Names emitted by LocalInvoker.Invoke
// and RemoteCaller.Call. Both follow the "<transport> <verb> <path>"
// convention (e.g. "remote GET /users/:id"). Anything else is filtered
// out of the dev terminal stream.
func isClientSpan(name string) bool {
	return strings.HasPrefix(name, "remote ") || strings.HasPrefix(name, "local ")
}

// statusColor picks an ANSI color for an HTTP status code: green for
// 2xx, yellow for 3xx, red for 4xx/5xx. Unknown / zero stays dim so
// pre-commit "in flight" lines don't pretend to be successes.
func statusColor(status int) string {
	switch {
	case status >= 200 && status < 300:
		return ansiGreen
	case status >= 300 && status < 400:
		return ansiYellow
	case status >= 400:
		return ansiRed
	}
	return ansiDim
}

// traceIDShort returns the first 6 hex chars of a 32-char trace ID,
// the convention the dashboard uses for at-a-glance correlation
// across concurrent traces. Empty IDs return six dashes so the line
// shape stays stable.
func traceIDShort(id string) string {
	if len(id) >= 6 {
		return id[:6]
	}
	return "------"
}

// truncErrLine flattens a multi-line error (UserError prints with
// hint+notes on extra lines) into a single one-liner so the dev
// stream stays one-line-per-event. The full error is still on the
// dashboard.
func truncErrLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + "…"
	}
	if len(s) > 80 {
		s = s[:80] + "…"
	}
	return s
}
