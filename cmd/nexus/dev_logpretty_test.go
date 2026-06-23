package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// sampleLines mirrors the design's data: zap-JSON log records plus a couple of
// non-JSON lines that must pass through untouched.
var sampleLines = strings.Join([]string{
	`{"level":"info","ts":1781787501,"caller":"auth/server.go:112","msg":"oauth token store ready","path":"data/oauth_tokens.db"}`,
	`{"level":"warn","ts":1781787501,"caller":"settings/automigrate.go:18","msg":"auto-migrate skipped","reason":"db not connected"}`,
	`{"level":"error","ts":1781787503,"caller":"nexus/client.go","msg":"auto-dump failed","file":"web/tsconfig.json"}`,
	`nexus: listening on http://:8080`,
	`[GIN-debug] GET /users --> handler (3 handlers)`,
}, "\n") + "\n"

func TestLogPrettyPassthroughNonJSON(t *testing.T) {
	var out bytes.Buffer
	lp := newLogPretty(&out, false, prettyFormatter)
	lp.Write([]byte(sampleLines))
	s := out.String()
	if !strings.Contains(s, "nexus: listening on http://:8080") {
		t.Error("framework banner line should pass through verbatim")
	}
	if !strings.Contains(s, "[GIN-debug] GET /users") {
		t.Error("gin line should pass through verbatim")
	}
}

func TestLogPrettyColumns(t *testing.T) {
	var out bytes.Buffer
	lp := newLogPretty(&out, false, prettyFormatter) // color off → plain columns
	lp.Write([]byte(sampleLines))
	s := out.String()
	// time formatted to clock, level uppercased, caller + msg + field present.
	for _, want := range []string{"INFO", "auth/server.go:112", "oauth token store ready", "path=data/oauth_tokens.db", "WARN", "ERROR"} {
		if !strings.Contains(s, want) {
			t.Errorf("pretty output missing %q\n%s", want, s)
		}
	}
}

func TestLogPrettyPartialLineBuffering(t *testing.T) {
	var out bytes.Buffer
	lp := newLogPretty(&out, false, prettyFormatter)
	// Split one JSON record across two writes — must not emit until newline.
	lp.Write([]byte(`{"level":"info","ts":1781787501,"msg":"split `))
	if out.Len() != 0 {
		t.Fatalf("emitted before newline: %q", out.String())
	}
	lp.Write([]byte(`record"}` + "\n"))
	if !strings.Contains(out.String(), "split record") {
		t.Errorf("buffered line not flushed on newline: %q", out.String())
	}
}

func TestResolveLogFormatter(t *testing.T) {
	for _, name := range []string{"", "pretty", "console", "logfmt", "pattern"} {
		if _, ok := resolveLogFormatter(name, ""); !ok {
			t.Errorf("resolveLogFormatter(%q) reported unknown", name)
		}
	}
	if _, ok := resolveLogFormatter("bogus", ""); ok {
		t.Error("unknown format should report ok=false")
	}
}

func TestLogfmtFormatter(t *testing.T) {
	r := zapRecord{level: "info", ts: "13:58:21", caller: "db/db.go:321", msg: "db connected", fields: []kv{{"driver", "mysql"}}}
	got := logfmtFormatter(r, palette{})
	for _, want := range []string{"level=info", "ts=13:58:21", "caller=db/db.go:321", "msg=", "db connected", "driver=mysql"} {
		if !strings.Contains(got, want) {
			t.Errorf("logfmt missing %q in %q", want, got)
		}
	}
}

func TestPatternFormatter(t *testing.T) {
	r := zapRecord{level: "warn", ts: "13:58:21", caller: "x/y.go:1", msg: "hi", fields: []kv{{"k", "v"}}}
	f := patternFormatter("%time %-5level %caller %msg %fields")
	got := f(r, palette{})
	for _, want := range []string{"13:58:21", "WARN", "x/y.go:1", "hi", "k=v"} {
		if !strings.Contains(got, want) {
			t.Errorf("pattern missing %q in %q", want, got)
		}
	}
	// %% literal and unknown verbs.
	if g := patternFormatter("100%% done")(r, palette{}); !strings.Contains(g, "100% done") {
		t.Errorf("%%%% should render literal percent: %q", g)
	}
}

func TestFormatTS(t *testing.T) {
	if got := formatTS(json.Number("1781787501")); len(got) != 8 || got[2] != ':' {
		t.Errorf("epoch ts should format to HH:MM:SS, got %q", got)
	}
	if got := formatTS("2026-06-23T13:58:21Z"); got != "13:58:21" {
		t.Errorf("RFC3339 ts → %q, want 13:58:21", got)
	}
	if got := formatTS("13:58:21"); got != "13:58:21" {
		t.Errorf("clock string passthrough → %q", got)
	}
}
