package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/paulmanoni/nexus/internal/vitehot"
)

func TestDevPrimaryURL(t *testing.T) {
	const (
		ready = ":8080"
		vite  = "http://localhost:5173"
		app   = "http://localhost:8080/"
		dash  = "http://localhost:8080/__nexus/"
	)
	cases := []struct {
		name           string
		ready, viteURL string
		appServesPages bool
		openDash       bool
		want           string
		wantSplit      bool
	}{
		{"hot file: the app serves the SPA", ready, vite, true, false, app, false},
		{"hot file + --open-dash", ready, vite, true, true, dash, false},
		{"dev server without the plugin keeps its URL", ready, vite, false, false, vite + "/", true},
		{"--open-dash doesn't override a plugin-less dev server", ready, vite, false, true, vite + "/", true},
		{"no dev server", ready, "", false, false, app, false},
		{"no dev server + --open-dash", ready, "", false, true, dash, false},
		{"app serves pages but never reported ready", "", vite, true, false, vite + "/", false},
		{"only the dev server reported", "", vite, false, false, vite + "/", false},
		{"nothing reported", "", "", false, false, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, split := devPrimaryURL(tc.ready, tc.viteURL, tc.appServesPages, tc.openDash)
			if got != tc.want || split != tc.wantSplit {
				t.Errorf("devPrimaryURL = (%q, %v), want (%q, %v)", got, split, tc.want, tc.wantSplit)
			}
		})
	}
}

func writeTestHot(t *testing.T, dist string, pid int) {
	t.Helper()
	b, _ := json.Marshal(vitehot.Hot{Version: vitehot.Version, Origin: "http://127.0.0.1:5173", Base: "/", PID: pid})
	p := vitehot.Path(dist)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWaitForHotFile(t *testing.T) {
	ctx := context.Background()

	t.Run("no dist dir", func(t *testing.T) {
		if waitForHotFile(ctx, "", time.Second) {
			t.Fatal("empty dist dir reported a hot file")
		}
	})
	t.Run("absent: gives up after the grace", func(t *testing.T) {
		start := time.Now()
		if waitForHotFile(ctx, t.TempDir(), 150*time.Millisecond) {
			t.Fatal("reported a hot file that doesn't exist")
		}
		if d := time.Since(start); d < 150*time.Millisecond || d > 2*time.Second {
			t.Errorf("waited %s, want about the grace", d)
		}
	})
	t.Run("present", func(t *testing.T) {
		dist := t.TempDir()
		writeTestHot(t, dist, os.Getpid())
		if !waitForHotFile(ctx, dist, 0) {
			t.Fatal("live hot file not seen")
		}
	})
	t.Run("appears during the grace", func(t *testing.T) {
		dist := t.TempDir()
		go func() {
			time.Sleep(100 * time.Millisecond)
			writeTestHot(t, dist, os.Getpid())
		}()
		if !waitForHotFile(ctx, dist, 3*time.Second) {
			t.Fatal("hot file written mid-wait not seen")
		}
	})
	t.Run("stale file from a dead dev server doesn't count", func(t *testing.T) {
		dead := exec.Command("true")
		if err := dead.Run(); err != nil {
			t.Skipf("no `true` binary: %v", err)
		}
		dist := t.TempDir()
		writeTestHot(t, dist, dead.ProcessState.Pid())
		if waitForHotFile(ctx, dist, 100*time.Millisecond) {
			t.Fatal("stale hot file treated as a live dev server")
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		c, cancel := context.WithCancel(ctx)
		cancel()
		if waitForHotFile(c, t.TempDir(), 5*time.Second) {
			t.Fatal("cancelled wait reported a hot file")
		}
	})
}
