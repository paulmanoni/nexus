package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// logPretty is an io.Writer that reshapes the child app's structured
// (zap-JSON) log lines into the columnar, colorized "Dev Server Logs" view:
//
//	13:58:21  INFO   auth/server.go:112      oauth token store ready  path=data/oauth_tokens.db
//	13:58:21  WARN   settings/automigrate.go auto-migrate skipped     reason=db not connected
//	13:58:23  ERROR  nexus/client.go         auto-dump failed …       file=web/tsconfig.json
//
// Columns mirror the design: time · level badge · source (caller) · message +
// key=value fields. Levels carry the design palette (info=cyan, warn=amber,
// error=red). Lines that aren't zap-JSON objects (gin output, the framework's
// "nexus: listening on …" banner, panics, plain prints) pass through verbatim,
// so nothing is ever swallowed.
//
// It line-buffers: writes arrive as arbitrary byte chunks, so partial lines are
// held until their terminating newline. Concurrency-safe — stdout and stderr
// each get their own instance, and a single instance may be written from more
// than one goroutine.
type logPretty struct {
	w     io.Writer
	color bool
	fmt   logFormatter // chosen renderer (pretty / logfmt / pattern / …)

	mu  sync.Mutex
	buf []byte
}

func newLogPretty(w io.Writer, color bool, f logFormatter) *logPretty {
	if f == nil {
		f = prettyFormatter
	}
	return &logPretty{w: w, color: color, fmt: f}
}

// logFormatter renders one decoded log record into a terminal line. The
// palette is pre-resolved (empty escapes when color is off) so a formatter
// only decides layout, not whether to colorize. New formats are added by
// writing one of these and registering it in resolveLogFormatter — the
// Django-/Spring-style "pluggable formatter" seam.
type logFormatter func(r zapRecord, c palette) string

func (l *logPretty) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		line := l.buf[:i]
		l.buf = l.buf[i+1:]
		out := l.render(line)
		if _, err := io.WriteString(l.w, out+"\n"); err != nil {
			// Report the original length as consumed — the caller only
			// cares that we accepted its bytes, not how we reshaped them.
			return len(p), err
		}
	}
	return len(p), nil
}

// render turns one line into its display form. Non-JSON (or non-log JSON)
// passes through unchanged so gin/banner/panic output is never mangled.
func (l *logPretty) render(line []byte) string {
	t := bytes.TrimSpace(line)
	if len(t) == 0 || t[0] != '{' {
		return string(line)
	}
	rec, ok := parseZapLine(t)
	if !ok {
		return string(line)
	}
	return l.fmt(rec, l.palette())
}

// zapRecord is the decoded shape of one structured log line. Fields holds
// every key that isn't one of the well-known columns, rendered as key=value.
type zapRecord struct {
	level      string
	ts         string
	caller     string
	msg        string
	stacktrace string
	fields     []kv
}

type kv struct{ k, v string }

// parseZapLine decodes a zap-JSON line. Returns ok=false for anything that
// isn't a JSON object carrying a "msg" (the one field zap always emits) — that
// filter keeps arbitrary JSV payloads the app might print from being captured.
func parseZapLine(b []byte) (zapRecord, bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return zapRecord{}, false
	}
	if _, hasMsg := m["msg"]; !hasMsg {
		return zapRecord{}, false
	}
	r := zapRecord{
		level:      asString(m["level"]),
		ts:         formatTS(m["ts"]),
		msg:        asString(m["msg"]),
		caller:     asString(m["caller"]),
		stacktrace: asString(m["stacktrace"]),
	}
	if r.caller == "" {
		r.caller = asString(m["logger"])
	}
	skip := map[string]bool{"level": true, "ts": true, "msg": true, "caller": true, "logger": true, "stacktrace": true}
	keys := make([]string, 0, len(m))
	for k := range m {
		if !skip[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		r.fields = append(r.fields, kv{k, asString(m[k])})
	}
	return r, true
}

// Column widths, tuned to the design's proportions while staying compact in a
// terminal. Time is fixed (HH:MM:SS); level and source are padded so messages
// line up into a clean left edge.
const (
	colTime   = 8
	colLevel  = 5
	colSource = 26
)

// prettyFormatter is the default "Dev Server Logs" renderer: the columnar
// time · level · source · message + key=value layout from the design.
func prettyFormatter(r zapRecord, c palette) string {
	lc := c.level(r.level)

	var b strings.Builder
	// time
	b.WriteString(c.time)
	b.WriteString(pad(r.ts, colTime))
	b.WriteString(c.reset)
	b.WriteString("  ")
	// level badge
	b.WriteString(lc)
	b.WriteString(c.bold)
	b.WriteString(pad(strings.ToUpper(r.level), colLevel))
	b.WriteString(c.reset)
	b.WriteString("  ")
	// source (caller), left-truncated so the file:line tail stays visible
	b.WriteString(c.source)
	b.WriteString(pad(truncLeft(r.caller, colSource), colSource))
	b.WriteString(c.reset)
	b.WriteString("  ")
	// message
	b.WriteString(c.msg)
	b.WriteString(r.msg)
	b.WriteString(c.reset)
	// key=value fields
	for _, f := range r.fields {
		b.WriteString("  ")
		b.WriteString(c.faint)
		b.WriteString(f.k)
		b.WriteString("=")
		b.WriteString(c.reset)
		// Error-level error= fields echo the level color, matching the design.
		val := c.meta
		if r.level == "error" || r.level == "fatal" || r.level == "panic" || r.level == "dpanic" {
			val = lc
		}
		b.WriteString(val)
		b.WriteString(f.v)
		b.WriteString(c.reset)
	}
	// stacktrace, indented and dimmed beneath the line
	if r.stacktrace != "" {
		for _, ln := range strings.Split(strings.TrimRight(r.stacktrace, "\n"), "\n") {
			b.WriteString("\n")
			b.WriteString(strings.Repeat(" ", colTime+colLevel+6))
			b.WriteString(c.faint)
			b.WriteString(ln)
			b.WriteString(c.reset)
		}
	}
	return b.String()
}

// resolveLogFormatter maps a format name (+ optional pattern) to a renderer —
// the Django-/Spring-style "named formatter" lookup. Recognized names:
//
//	pretty   columnar Dev Server Logs view (default)
//	logfmt   single-line key=value stream (level=info caller=… msg="…" k=v)
//	pattern  custom layout from `pattern`, Spring-ish tokens (see patternFormatter)
//
// "raw"/"json"/"off" are handled upstream (the prettifier is bypassed entirely,
// leaving the child's JSON untouched), so they don't appear here. An unknown
// name falls back to pretty with ok=false so the caller can warn.
func resolveLogFormatter(name, pattern string) (logFormatter, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "pretty", "console", "dev":
		return prettyFormatter, true
	case "logfmt":
		return logfmtFormatter, true
	case "pattern":
		p := pattern
		if p == "" {
			p = defaultLogPattern
		}
		return patternFormatter(p), true
	default:
		return prettyFormatter, false
	}
}

// logfmtFormatter renders one record as a single logfmt line — the dense,
// greppable style (level=info ts=13:58:21 caller=… msg="…" key=value). The
// level keeps its color; everything else is plain so it pipes cleanly.
func logfmtFormatter(r zapRecord, c palette) string {
	var b strings.Builder
	b.WriteString(c.level(r.level))
	b.WriteString("level=")
	b.WriteString(r.level)
	b.WriteString(c.reset)
	if r.ts != "" {
		b.WriteString(" ts=")
		b.WriteString(r.ts)
	}
	if r.caller != "" {
		b.WriteString(" caller=")
		b.WriteString(r.caller)
	}
	b.WriteString(" msg=")
	b.WriteString(logfmtQuote(r.msg))
	for _, f := range r.fields {
		b.WriteString(" ")
		b.WriteString(f.k)
		b.WriteString("=")
		b.WriteString(logfmtQuote(f.v))
	}
	return b.String()
}

// logfmtQuote wraps a value in quotes when it contains spaces or quotes, the
// standard logfmt escaping rule.
func logfmtQuote(s string) string {
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, " \"=") {
		return strconv.Quote(s)
	}
	return s
}

// defaultLogPattern is the built-in pattern used when format="pattern" without
// an explicit `pattern`. Mirrors Spring's default console layout shape.
const defaultLogPattern = "%time  %-5level  %caller  %msg  %fields"

// patternFormatter builds a renderer from a layout string with Spring-/logback-
// flavored tokens, so a project can pin its own console layout in nexus.toml:
//
//	%time / %d        timestamp (HH:MM:SS)
//	%level / %p       level, lowercase   ·   %-5level pads/uppercases to 5
//	%caller / %logger source (file:line)
//	%msg / %m         message
//	%fields / %X      key=value pairs
//	%n                newline (stripped — lines are newline-delimited already)
//	%%                literal percent
//
// Only the level token carries color (matching the other formatters); the rest
// stays plain so custom layouts read predictably.
func patternFormatter(pattern string) logFormatter {
	return func(r zapRecord, c palette) string {
		var b strings.Builder
		for i := 0; i < len(pattern); i++ {
			if pattern[i] != '%' || i == len(pattern)-1 {
				b.WriteByte(pattern[i])
				continue
			}
			// Consume token: optional "-5" width, then the verb word.
			rest := pattern[i+1:]
			if rest[0] == '%' {
				b.WriteByte('%')
				i++
				continue
			}
			tok, adv := scanPatternToken(rest)
			i += adv
			b.WriteString(renderPatternToken(tok, r, c))
		}
		return strings.TrimRight(b.String(), " ")
	}
}

// scanPatternToken reads one token body after the '%': an optional "-N" pad
// spec followed by a verb word ([a-zA-Z]+). Returns the raw token text and how
// many bytes were consumed from rest.
func scanPatternToken(rest string) (tok string, adv int) {
	j := 0
	for j < len(rest) && (rest[j] == '-' || (rest[j] >= '0' && rest[j] <= '9')) {
		j++
	}
	k := j
	for k < len(rest) && ((rest[k] >= 'a' && rest[k] <= 'z') || (rest[k] >= 'A' && rest[k] <= 'Z')) {
		k++
	}
	return rest[:k], k
}

func renderPatternToken(tok string, r zapRecord, c palette) string {
	// Split an optional leading width spec ("-5") from the verb.
	width, verb := 0, tok
	if dash := strings.IndexFunc(tok, func(ru rune) bool { return ru >= 'a' && ru <= 'z' || ru >= 'A' && ru <= 'Z' }); dash > 0 {
		if n, err := strconv.Atoi(strings.TrimPrefix(tok[:dash], "-")); err == nil {
			width = n
		}
		verb = tok[dash:]
	}
	switch strings.ToLower(verb) {
	case "time", "d", "date":
		return r.ts
	case "level", "p":
		s := r.level
		if width > 0 {
			s = pad(strings.ToUpper(s), width)
		}
		return c.level(r.level) + c.bold + s + c.reset
	case "caller", "logger", "c":
		s := r.caller
		if width > 0 {
			s = pad(truncLeft(s, width), width)
		}
		return s
	case "msg", "m", "message":
		return r.msg
	case "fields", "x":
		parts := make([]string, len(r.fields))
		for i, f := range r.fields {
			parts[i] = f.k + "=" + f.v
		}
		return strings.Join(parts, " ")
	case "n":
		return "" // lines are newline-delimited by the writer
	default:
		return "%" + tok
	}
}

// ---- color palette ---------------------------------------------------------

// palette carries the escape sequences for one render. When color is off every
// field is the empty string, so format() emits plain aligned columns.
type palette struct {
	reset, bold              string
	time, msg, meta          string
	source, faint            string
	info, warn, err, debug   string
	trueColor                bool
}

func (l *logPretty) palette() palette {
	if !l.color {
		return palette{}
	}
	p := palette{
		reset:     ansiReset,
		bold:      ansiBold,
		trueColor: colorTrueSupported(),
	}
	// Design hex → truecolor; basic-ANSI fallbacks keep it legible on
	// 16-color terminals.
	p.time = fg(p.trueColor, 91, 100, 112, ansiDim)    // #5b6470
	p.faint = fg(p.trueColor, 60, 67, 79, ansiDim)     // #3c434f
	p.source = fg(p.trueColor, 124, 134, 148, ansiDim) // #7c8694
	p.meta = fg(p.trueColor, 107, 114, 128, ansiDim)   // #6b7280
	p.msg = fg(p.trueColor, 223, 227, 234, "")         // #dfe3ea (near-white → default)
	p.info = fg(p.trueColor, 125, 211, 252, ansiCyan)  // #7dd3fc
	p.warn = fg(p.trueColor, 251, 191, 36, ansiYellow) // #fbbf24
	p.err = fg(p.trueColor, 248, 113, 113, ansiRed)    // #f87171
	p.debug = p.faint
	return p
}

func (p palette) level(l string) string {
	switch l {
	case "warn", "warning":
		return p.warn
	case "error", "fatal", "panic", "dpanic":
		return p.err
	case "debug":
		return p.debug
	default:
		return p.info
	}
}

// fg returns a foreground SGR: a 24-bit truecolor escape when the terminal
// advertises support (COLORTERM), otherwise the supplied basic-ANSI fallback.
func fg(trueColor bool, r, g, b int, basic string) string {
	if trueColor {
		return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", r, g, b)
	}
	return basic
}

// colorTrueSupported reports whether the terminal advertises 24-bit color via
// the de-facto COLORTERM convention (truecolor / 24bit).
func colorTrueSupported() bool {
	ct := strings.ToLower(os.Getenv("COLORTERM"))
	return strings.Contains(ct, "truecolor") || strings.Contains(ct, "24bit")
}

// ---- small helpers ---------------------------------------------------------

// pad right-pads s with spaces to width w (no truncation — callers truncate
// first where a hard column edge matters).
func pad(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(s))
}

// truncLeft keeps the rightmost w runes of s (prefixed with "…" when cut), so a
// long caller like "auth/settings/automigrate.go:18" shows its file:line tail.
func truncLeft(s string, w int) string {
	if len(s) <= w {
		return s
	}
	if w <= 1 {
		return s[len(s)-w:]
	}
	return "…" + s[len(s)-(w-1):]
}

// asString coerces a decoded JSON value to its display string. Numbers use
// json.Number's own text (no float reformatting), bools/others via fmt.
func asString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		return strconv.FormatBool(x)
	default:
		return fmt.Sprintf("%v", x)
	}
}

// formatTS renders zap's timestamp as HH:MM:SS. zap.NewProduction emits epoch
// seconds (a JSON number); ISO8601 strings and already-short clock strings are
// handled too. Unparseable values fall back to their raw text.
func formatTS(v any) string {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return x.String()
		}
		sec := int64(f)
		nsec := int64((f - float64(sec)) * 1e9)
		return time.Unix(sec, nsec).Format("15:04:05")
	case string:
		if x == "" {
			return ""
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z0700"} {
			if t, err := time.Parse(layout, x); err == nil {
				return t.Format("15:04:05")
			}
		}
		// Already a clock-ish string (the design's "13:58:21") — keep last 8.
		if len(x) >= 8 {
			return x[len(x)-8:]
		}
		return x
	default:
		return ""
	}
}

// stdoutIsTerminal reports whether stdout is a real tty — gates the prettifier
// so `nexus dev > log` (or a pipe) keeps the raw JSON for grep/jq. Mirrors
// stdinIsTerminal; stdlib mode bits avoid pulling in golang.org/x/term.
func stdoutIsTerminal() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}
