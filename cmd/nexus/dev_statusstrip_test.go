package main

import (
	"io"
	"strings"
	"testing"
)

// TestStatusStrip: the strip redraws beneath every write, erases before the next
// one, renders nothing while healthy, and is a pure passthrough when
// disabled.
func TestStatusStrip(t *testing.T) {
	var out strings.Builder
	s := newStatusStrip(&out, true)
	w := s.Wrap(&out)

	io.WriteString(w, "log line 1\n")
	if strings.Contains(out.String(), "\x1b[2K") {
		t.Fatalf("nothing to erase yet:\n%q", out.String())
	}
	s.Set("res:redis", "✖ redis: down 10s (3×)")
	if !strings.HasSuffix(out.String(), "✖ redis: down 10s (3×)"+ansiReset) {
		t.Fatalf("strip not drawn after Set:\n%q", out.String())
	}
	// The next log write erases the strip, prints, and redraws it.
	io.WriteString(w, "log line 2\n")
	got := out.String()
	if !strings.Contains(got, "\r\x1b[2K"+"log line 2\n") {
		t.Fatalf("write did not erase before logging:\n%q", got)
	}
	if !strings.HasSuffix(got, "✖ redis: down 10s (3×)"+ansiReset) {
		t.Fatalf("strip not redrawn after write:\n%q", got)
	}
	// Two entries render sorted and joined; clearing the last erases fully.
	s.Set("pages", "⚠ 1 page component unresolved")
	if !strings.Contains(out.String(), "⚠ 1 page component unresolved · ✖ redis: down") {
		t.Fatalf("entries not joined:\n%q", out.String())
	}
	s.Clear("res:redis")
	s.Clear("pages")
	io.WriteString(w, "after clear\n")
	if !strings.HasSuffix(out.String(), "after clear\n") {
		t.Fatalf("cleared strip should leave plain output:\n%q", out.String())
	}

	// Disabled: Wrap returns the writer untouched; Set/Clear are no-ops.
	var quiet strings.Builder
	d := newStatusStrip(&quiet, false)
	if dw := d.Wrap(&quiet); dw != io.Writer(&quiet) {
		t.Fatal("disabled strip must not wrap")
	}
	d.Set("x", "y")
	if quiet.Len() != 0 {
		t.Fatal("disabled strip drew output")
	}
}

// TestLogPretty_ResourceStateOnStrip: logx.Transition's structured fields
// pin and clear resource entries.
func TestLogPretty_ResourceStateOnStrip(t *testing.T) {
	var out strings.Builder
	s := newStatusStrip(&out, true)
	lp := newLogPretty(s.Wrap(&out), false, nil)
	lp.strip = s

	io.WriteString(lp, `{"level":"warn","msg":"db: cannot reach the server","resource":"db:main","state":"down","attempts":1}`+"\n")
	if !strings.Contains(out.String(), "✖ db:main: down") || strings.Contains(out.String(), "(1×)") {
		t.Fatalf("down state not pinned:\n%q", out.String())
	}
	io.WriteString(lp, `{"level":"warn","msg":"db: still unreachable","resource":"db:main","state":"still-down","attempts":24,"down_for":"2m0s"}`+"\n")
	if !strings.Contains(out.String(), "✖ db:main: down 2m0s (24×)") {
		t.Fatalf("heartbeat not reflected:\n%q", out.String())
	}
	io.WriteString(lp, `{"level":"info","msg":"db: reconnected","resource":"db:main","state":"up","attempts":24,"down_for":"2m5s"}`+"\n")
	io.WriteString(lp, "plain line\n")
	if !strings.HasSuffix(out.String(), "plain line\n") {
		t.Fatalf("up state should clear the strip:\n%q", out.String())
	}
}

// TestViteLogWriter_PageWarningsPinAndClear: page-component warnings pin a
// counted entry; the plugin's pages-ok all-clear retires it.
func TestViteLogWriter_PageWarningsPinAndClear(t *testing.T) {
	var out strings.Builder
	s := newStatusStrip(&out, true)
	w := newViteLogWriter(s.Wrap(&out), false)
	w.strip = s

	io.WriteString(w, "[nexus] page component '/User/Index' (GET /testme) → not a path under src/Pages — fix it\n")
	if !strings.Contains(out.String(), "⚠ page '/User/Index' missing — fix the component name passed to inertia.Page") {
		t.Fatalf("single invalid-name warning not pinned with its fix:\n%q", out.String())
	}
	io.WriteString(w, "[nexus] page component 'Ghost/Page' (GET /g) → expected src/Pages/Ghost/Page.{vue} — fix it\n")
	if !strings.Contains(out.String(), "⚠ 2 pages missing: /User/Index, Ghost/Page") {
		t.Fatalf("count/names not updated:\n%q", out.String())
	}
	io.WriteString(w, "[nexus] pages ok — every registered page component resolves\n")
	io.WriteString(w, "plain\n")
	if !strings.HasSuffix(out.String(), "plain\n") {
		t.Fatalf("pages ok should clear the strip:\n%q", out.String())
	}
	if !strings.Contains(out.String(), "[nexus] pages ok") {
		t.Fatal("pages ok line should pass through as info")
	}
}

// TestViteLogWriter_SinglePageShowsCreateHint: a lone missing page with a
// valid name pins the FILE to create, self-contained — no pointer back into
// the scroll.
func TestViteLogWriter_SinglePageShowsCreateHint(t *testing.T) {
	var out strings.Builder
	s := newStatusStrip(&out, true)
	w := newViteLogWriter(s.Wrap(&out), false)
	w.strip = s
	io.WriteString(w, "[nexus] page component 'Users/Index' (GET /testme) → expected src/Pages/Users/Index.{vue,tsx,jsx,svelte,ts,js} — create the file, or fix the component name passed to inertia.Page\n")
	if !strings.Contains(out.String(), "⚠ page 'Users/Index' missing — create src/Pages/Users/Index.*") {
		t.Fatalf("create hint not pinned:\n%q", out.String())
	}
}
