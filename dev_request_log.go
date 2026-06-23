package nexus

import (
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/paulmanoni/nexus/httpx"
)

// devRequestLogEnabled reports whether the per-request dev console logger
// should be installed. It's a DEV-ONLY convenience: nexus dev sets NEXUS_DEV=1
// on the child, and the app then logs one line per HTTP request to stdout so
// navigations show up in the terminal (Django/Spring style) — by default they
// only reach the dashboard's trace stream, leaving the console quiet. Off
// entirely outside `nexus dev` (no NEXUS_DEV), so production binaries never log
// requests to the console. Opt out within dev via nexus.toml:
//
//	[runtime.logging]
//	requests = false
func devRequestLogEnabled() bool {
	if os.Getenv("NEXUS_DEV") != "1" {
		return false
	}
	return Get("runtime.logging.requests", true)
}

// devRequestLogger is a whole-mux middleware that emits one structured
// (zap-JSON) line per completed request to out. The JSON shape matches what
// `nexus dev`'s Dev Server Logs view parses, so requests render in the same
// columnar layout as the app's own logs:
//
//	{"level":"info","ts":1.78e9,"caller":"http","msg":"GET /users","status":200,"dur":"12ms"}
//
// Status drives the level — 5xx→error, 4xx→warn, else info — so failures stand
// out in red/amber. Dashboard traffic (/__nexus*) is skipped: the dashboard is
// WebSocket-driven and self-referential, so logging it would be noise.
func devRequestLogger(out io.Writer) httpx.HandlerFunc {
	return func(c *httpx.Ctx) {
		start := time.Now()
		c.Next()

		path := c.Path()
		if path == "/__nexus" || strings.HasPrefix(path, "/__nexus/") {
			return
		}

		status := c.Writer.Status()
		level := "info"
		switch {
		case status >= 500:
			level = "error"
		case status >= 400:
			level = "warn"
		}
		writeDevRequestLine(out, level, c.Method(), path, status, time.Since(start), start)
	}
}

// writeDevRequestLine hand-builds the JSON line (no encoding/json: the fields
// are known and small, and a single Write keeps lines from interleaving with
// the app's logger under concurrency). ts is epoch seconds so the dev viewer's
// formatTS renders it as a clock time; dur is human-readable.
func writeDevRequestLine(out io.Writer, level, method, path string, status int, dur time.Duration, at time.Time) {
	var b strings.Builder
	b.Grow(128)
	b.WriteString(`{"level":"`)
	b.WriteString(level)
	b.WriteString(`","ts":`)
	b.WriteString(strconv.FormatFloat(float64(at.UnixNano())/1e9, 'f', 3, 64))
	b.WriteString(`,"caller":"http","msg":`)
	b.WriteString(strconv.Quote(method + " " + path))
	b.WriteString(`,"status":`)
	b.WriteString(strconv.Itoa(status))
	b.WriteString(`,"dur":`)
	b.WriteString(strconv.Quote(durString(dur)))
	b.WriteString("}\n")
	_, _ = io.WriteString(out, b.String())
}

// durString renders a latency compactly: sub-ms in µs, else ms with one
// decimal under 10ms, whole ms above.
func durString(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return strconv.FormatInt(int64(d/time.Microsecond), 10) + "µs"
	case d < 10*time.Millisecond:
		return strconv.FormatFloat(float64(d)/float64(time.Millisecond), 'f', 1, 64) + "ms"
	default:
		return strconv.FormatInt(int64(d/time.Millisecond), 10) + "ms"
	}
}
