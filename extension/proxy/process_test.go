package proxy

import (
	"bytes"
	"io"
	"testing"
	"time"
)

// TestArgsFor is the "which command in which mode" selection the whole feature
// hinges on.
func TestArgsFor(t *testing.T) {
	c := &Command{Dev: []string{"dev-cmd"}, Prod: []string{"prod-cmd"}}
	if got := c.argsFor(true); len(got) != 1 || got[0] != "dev-cmd" {
		t.Errorf("dev args = %v, want [dev-cmd]", got)
	}
	if got := c.argsFor(false); len(got) != 1 || got[0] != "prod-cmd" {
		t.Errorf("prod args = %v, want [prod-cmd]", got)
	}
}

// TestStartProcess_EmptyModeNoLaunch: an empty argv for the active mode means
// "don't launch here" (e.g. prod managed by systemd) — returns (nil, nil).
func TestStartProcess_EmptyModeNoLaunch(t *testing.T) {
	p, err := startProcess(&Command{Dev: []string{"go", "version"}}, false /*prod, Prod is empty*/)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p != nil {
		t.Fatalf("expected nil process when the mode's argv is empty, got %+v", p)
	}
}

// TestStartProcess_RunsAndStops launches a real, always-present command (the Go
// toolchain, on PATH under `go test`) and confirms it starts, exits, and stop()
// is safe.
func TestStartProcess_RunsAndStops(t *testing.T) {
	p, err := startProcess(&Command{Dev: []string{"go", "version"}, Name: "toolchain"}, true)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if p == nil {
		t.Fatal("expected a live process")
	}
	select {
	case <-p.done:
	case <-time.After(15 * time.Second):
		t.Fatal("process did not exit in time")
	}
	p.stop() // must not panic / hang after the child already exited
}

func TestStartProcess_BadCommand(t *testing.T) {
	if _, err := startProcess(&Command{Dev: []string{"definitely-not-a-real-binary-xyz"}}, true); err == nil {
		t.Error("expected error launching a nonexistent binary")
	}
}

// TestPrefixWriter prefixes each complete line and buffers partial trailing input.
func TestPrefixWriter(t *testing.T) {
	var buf bytes.Buffer
	w := &prefixWriter{w: &buf, prefix: []byte("[django] ")}
	_, _ = io.WriteString(w, "starting\nlistening on 8000\n")
	_, _ = io.WriteString(w, "partial-no-newline")

	got := buf.String()
	want := "[django] starting\n[django] listening on 8000\n"
	if got != want {
		t.Errorf("prefixed output = %q, want %q (partial line must stay buffered)", got, want)
	}
}

func TestModeLabel(t *testing.T) {
	if modeLabel(true) != "dev" || modeLabel(false) != "prod" {
		t.Error("modeLabel mismatch")
	}
}
