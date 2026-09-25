//go:build !windows

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/paulmanoni/nexus/internal/vitehot"
)

// fakeVite installs node_modules/.bin/vite in a new frontend dir: a shell
// script standing in for Vite, so the supervisor is tested without Node.
// body runs after the prologue; $HOT is the hot-file path Vite would write
// (dist/.vite/nexus-hot.json) and $$ is the pid it would record.
func fakeVite(t *testing.T, body string) string {
	t.Helper()
	web := t.TempDir()
	writeFile(t, filepath.Join(web, "package.json"), "{}")
	script := "#!/bin/sh\nHOT=\"$PWD/dist/.vite/nexus-hot.json\"\n" + body + "\n"
	bin := filepath.Join(web, "node_modules", ".bin", "vite")
	writeFile(t, bin, script)
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	return web
}

// writeHot is the script fragment that publishes a hot file the way the
// plugin does: pid of the Vite process, an origin nothing listens on.
const writeHot = `mkdir -p "$(dirname "$HOT")"
printf '{"version":1,"origin":"http://127.0.0.1:1","base":"/","entries":["src/main.ts"],"pid":%d}' $$ > "$HOT"
echo "  VITE v6.4.3  ready in 5 ms"
echo "  ➜  Local:   http://localhost:5190/"
echo "[vite] (client) hmr update /src/App.vue"`

// syncBuffer is a bytes.Buffer safe for the supervisor's goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func waitClosed(t *testing.T, ch <-chan struct{}, d time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(d):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func processGone(pid int) bool {
	return syscall.Kill(pid, 0) == syscall.ESRCH
}

func TestDevVite_HotFileIsReadinessAndStopCleansUp(t *testing.T) {
	// SIGTERM → remove the hot file and exit, as the plugin + Vite do.
	web := fakeVite(t, `trap 'rm -f "$HOT"; exit 0' TERM
`+writeHot+`
sleep 60 & wait`)
	var out, notes syncBuffer
	v := startDevVite(context.Background(), devViteConfig{WebDir: web, Out: &out, Notes: &notes})
	waitClosed(t, v.settledCh(), 5*time.Second, "the hot file")

	if got := v.origin(); got != "http://127.0.0.1:1" {
		t.Fatalf("origin = %q, want the hot file's", got)
	}
	if _, err := os.Stat(filepath.Join(web, "sdk", "nexus-vite-plugin.js")); err != nil {
		t.Errorf("SDK plugin not written before Vite started: %v", err)
	}
	pid := v.cmd.Process.Pid

	start := time.Now()
	v.stop()
	if d := time.Since(start); d > time.Second {
		t.Errorf("stop took %s; a Vite that honours SIGTERM should not wait out the grace", d)
	}
	if !processGone(pid) {
		t.Fatal("vite still running after stop")
	}
	if _, err := os.Stat(vitehot.Path(filepath.Join(web, "dist"))); !os.IsNotExist(err) {
		t.Errorf("hot file left after a clean stop: %v", err)
	}
	logged := out.String()
	if strings.Contains(logged, "Local:") || !strings.Contains(logged, "[web]") || !strings.Contains(logged, "hmr update") {
		t.Errorf("vite output not filtered/prefixed:\n%s", logged)
	}
	if strings.Contains(notes.String(), "●") {
		t.Errorf("unexpected warnings: %q", notes.String())
	}
	v.stop() // idempotent
}

func TestDevVite_SIGKILLAfterGrace(t *testing.T) {
	// A Vite that ignores SIGTERM (wedged): stop escalates, and returns
	// only once it is gone. The file it leaves names a dead pid, which
	// readers treat as absent.
	web := fakeVite(t, `trap '' TERM
`+writeHot+`
sleep 60 & wait`)
	var out, notes syncBuffer
	v := startDevVite(context.Background(), devViteConfig{WebDir: web, Out: &out, Notes: &notes, Grace: 200 * time.Millisecond})
	waitClosed(t, v.settledCh(), 5*time.Second, "the hot file")
	pid := v.cmd.Process.Pid

	start := time.Now()
	v.stop()
	if d := time.Since(start); d < 200*time.Millisecond {
		t.Errorf("stop returned after %s, before the grace", d)
	}
	if !processGone(pid) {
		t.Fatal("vite survived SIGKILL")
	}
	if !strings.Contains(notes.String(), "SIGKILL") {
		t.Errorf("escalation not reported: %q", notes.String())
	}
	if h, _ := findDevHot([]string{filepath.Join(web, "dist")}, pid, time.Time{}); h != nil {
		t.Error("the killed vite's hot file still reads as live")
	}
}

func TestDevVite_NoHotFileWarns(t *testing.T) {
	// vite.config without nexus(): Vite runs, no hot file ever appears.
	web := fakeVite(t, `sleep 60 & wait`)
	var out, notes syncBuffer
	v := startDevVite(context.Background(), devViteConfig{WebDir: web, Out: &out, Notes: &notes, HotTimeout: 150 * time.Millisecond})
	defer v.stop()
	waitClosed(t, v.settledCh(), 5*time.Second, "the hot-file timeout")
	if !strings.Contains(notes.String(), "wrote no hot file") || !strings.Contains(notes.String(), "nexus-vite-plugin") {
		t.Errorf("missing-plugin warning not printed: %q", notes.String())
	}
}

func TestDevVite_EarlyExitReported(t *testing.T) {
	web := fakeVite(t, `echo "failed to load config from vite.config.ts" >&2
exit 1`)
	var out, notes syncBuffer
	v := startDevVite(context.Background(), devViteConfig{WebDir: web, Out: &out, Notes: &notes})
	defer v.stop()
	waitClosed(t, v.settledCh(), 5*time.Second, "vite's exit")
	if !strings.Contains(out.String(), "[web]") || !strings.Contains(out.String(), "failed to load config") {
		t.Errorf("vite's stderr not relayed: %q", out.String())
	}
	if !strings.Contains(notes.String(), "vite exited") {
		t.Errorf("exit not reported: %q", notes.String())
	}
}

func TestDevVite_HotFileInAnotherDirWarns(t *testing.T) {
	web := fakeVite(t, `trap 'rm -f "$HOT"; exit 0' TERM
`+writeHot+`
sleep 60 & wait`)
	served := t.TempDir() // the app reads here; vite writes web/dist
	var out, notes syncBuffer
	v := startDevVite(context.Background(), devViteConfig{WebDir: web, ServedDist: served, Out: &out, Notes: &notes})
	defer v.stop()
	waitClosed(t, v.settledCh(), 5*time.Second, "the hot file")
	if !strings.Contains(notes.String(), "build.outDir") {
		t.Errorf("outDir mismatch not reported: %q", notes.String())
	}
}

func TestDevVite_StopBeforeSpawn(t *testing.T) {
	// No node_modules and no way to install: stop must not hang, and
	// nothing is spawned after it.
	web := t.TempDir()
	writeFile(t, filepath.Join(web, "package.json"), "{}")
	t.Setenv("PATH", t.TempDir()) // no npm
	var out, notes syncBuffer
	v := startDevVite(context.Background(), devViteConfig{WebDir: web, Out: &out, Notes: &notes})
	waitClosed(t, v.settledCh(), 5*time.Second, "the failed start")
	v.stop()
	if v.cmd != nil {
		t.Fatal("spawned without a vite binary")
	}
	if !strings.Contains(notes.String(), "npm") {
		t.Errorf("missing npm not reported: %q", notes.String())
	}
}

func TestWatchDistBuild_RebuildsOnChange(t *testing.T) {
	// A stand-in for `vite build` that records each invocation's args.
	web := fakeVite(t, `mkdir -p dist; echo "$*" >> dist/builds.log; exit 0`)
	writeFile(t, filepath.Join(web, "src", "main.ts"), "export {}")
	writeFile(t, filepath.Join(web, "src", "ssr.ts"), "export {}")
	logPath := filepath.Join(web, "dist", "builds.log")
	builds := func() []string {
		b, _ := os.ReadFile(logPath)
		if len(b) == 0 {
			return nil
		}
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	waitFor := func(n int) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for len(builds()) < n {
			if time.Now().After(deadline) {
				t.Fatalf("waited for %d vite runs, have %q", n, builds())
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out, errs syncBuffer
	stop, err := watchDistBuild(ctx, web, filepath.Join(web, "nexus.toml"), nil, nil, &out, &errs)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(2) // the up-front build: client, then SSR
	if got := builds(); got[0] != "build" || got[1] != "build --ssr src/ssr.ts --outDir dist/ssr" {
		t.Fatalf("vite runs = %q", got)
	}
	// A burst of saves → one rebuild (client + SSR).
	for i := 0; i < 3; i++ {
		writeFile(t, filepath.Join(web, "src", "main.ts"), "export const n = "+string(rune('0'+i)))
	}
	waitFor(4)
	time.Sleep(2 * distDebounce)
	if n := len(builds()); n != 4 {
		t.Errorf("a burst of saves ran vite %d times in total, want 4", n)
	}
	stop() // returns with ctx still live: it cancels its own
	if !strings.Contains(out.String(), "rebuilt in") || errs.String() != "" {
		t.Errorf("out %q, errs %q", out.String(), errs.String())
	}
}
