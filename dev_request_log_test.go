package nexus

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestWriteDevRequestLine(t *testing.T) {
	var buf bytes.Buffer
	at := time.Unix(1781787503, 0)
	writeDevRequestLine(&buf, "info", "GET", "/users", 200, 12*time.Millisecond, at)

	line := buf.String()
	if !strings.HasSuffix(line, "\n") {
		t.Fatalf("line not newline-terminated: %q", line)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("emitted line is not valid JSON: %v\n%s", err, line)
	}
	if m["level"] != "info" {
		t.Errorf("level = %v, want info", m["level"])
	}
	if m["msg"] != "GET /users" {
		t.Errorf("msg = %v, want \"GET /users\"", m["msg"])
	}
	if m["caller"] != "http" {
		t.Errorf("caller = %v, want http", m["caller"])
	}
	if m["status"].(float64) != 200 {
		t.Errorf("status = %v, want 200", m["status"])
	}
	if m["dur"] != "12ms" {
		t.Errorf("dur = %v, want 12ms", m["dur"])
	}
}

func TestDurString(t *testing.T) {
	cases := map[time.Duration]string{
		500 * time.Microsecond:  "500µs",
		3500 * time.Microsecond: "3.5ms",
		12 * time.Millisecond:   "12ms",
		1500 * time.Millisecond: "1500ms",
	}
	for d, want := range cases {
		if got := durString(d); got != want {
			t.Errorf("durString(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestDevRequestLogEnabledGating(t *testing.T) {
	t.Setenv("NEXUS_DEV", "")
	if devRequestLogEnabled() {
		t.Error("should be disabled outside nexus dev (NEXUS_DEV unset)")
	}
	t.Setenv("NEXUS_DEV", "1")
	if !devRequestLogEnabled() {
		t.Error("should be enabled by default under nexus dev")
	}
}
