package main

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/paulmanoni/nexus/internal/vitehot"
)

func TestDevPrimaryURL(t *testing.T) {
	cases := []struct {
		ready    string
		openDash bool
		want     string
	}{
		{":8080", false, "http://localhost:8080/"},
		{":8080", true, "http://localhost:8080/__nexus/"},
		{"127.0.0.1:8190", false, "http://127.0.0.1:8190/"},
		{"[::]:8190", false, "http://localhost:8190/"},
	}
	for _, tc := range cases {
		if got := devPrimaryURL(tc.ready, tc.openDash); got != tc.want {
			t.Errorf("devPrimaryURL(%q, %v) = %q, want %q", tc.ready, tc.openDash, got, tc.want)
		}
	}
}

// writeTestHot writes a hot file naming an origin nothing listens on: a
// dead pid must not be rescued by the reader's liveness probe reaching some
// real dev server (5173 may well be one on a developer's machine).
func writeTestHot(t *testing.T, dist string, pid int) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	origin := "http://" + l.Addr().String()
	l.Close()
	writeTestHotAt(t, dist, origin, pid)
}

func writeTestHotAt(t *testing.T, dist, origin string, pid int) {
	t.Helper()
	b, _ := json.Marshal(vitehot.Hot{Version: vitehot.Version, Origin: origin, Base: "/", PID: pid})
	p := vitehot.Path(dist)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// deadPID is the pid of a process that has exited.
func deadPID(t *testing.T) int {
	t.Helper()
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Skipf("no `true` binary: %v", err)
	}
	return dead.ProcessState.Pid()
}

func TestFindDevHot(t *testing.T) {
	self := os.Getpid()
	longAgo := time.Now().Add(-time.Hour)

	t.Run("nothing written", func(t *testing.T) {
		if h, _ := findDevHot([]string{t.TempDir()}, self, time.Now()); h != nil {
			t.Fatalf("found %+v in an empty dir", h)
		}
	})
	t.Run("this vite's file, by pid", func(t *testing.T) {
		dist := t.TempDir()
		writeTestHot(t, dist, self)
		// since is in the future: only the pid can match.
		h, dir := findDevHot([]string{dist}, self, time.Now().Add(time.Hour))
		if h == nil || dir != dist {
			t.Fatalf("got (%v, %q), want the file in %s", h, dir, dist)
		}
	})
	t.Run("first dir that has one wins", func(t *testing.T) {
		served, fallback := t.TempDir(), t.TempDir()
		writeTestHot(t, fallback, self)
		if _, dir := findDevHot([]string{served, fallback}, self, longAgo); dir != fallback {
			t.Fatalf("dir = %q, want the fallback %q", dir, fallback)
		}
		writeTestHot(t, served, self)
		if _, dir := findDevHot([]string{served, fallback}, self, longAgo); dir != served {
			t.Fatalf("dir = %q, want the served dir %q", dir, served)
		}
	})
	t.Run("another live dev server's older file is not ours", func(t *testing.T) {
		dist := t.TempDir()
		writeTestHot(t, dist, self) // alive (this test process), but not our pid
		old := time.Now().Add(-time.Minute)
		if err := os.Chtimes(vitehot.Path(dist), old, old); err != nil {
			t.Fatal(err)
		}
		if h, _ := findDevHot([]string{dist}, self+1, time.Now()); h != nil {
			t.Fatal("a file written before this vite started was taken as its own")
		}
	})
	t.Run("a launcher's pid differs; a fresh file still counts", func(t *testing.T) {
		dist := t.TempDir()
		start := time.Now()
		writeTestHot(t, dist, self)
		if h, _ := findDevHot([]string{dist}, self+1, start); h == nil {
			t.Fatal("a file written after this vite started was not taken")
		}
	})
	t.Run("stale file from a dead dev server doesn't count", func(t *testing.T) {
		dist := t.TempDir()
		pid := deadPID(t)
		writeTestHot(t, dist, pid)
		if h, _ := findDevHot([]string{dist}, pid, longAgo); h != nil {
			t.Fatal("stale hot file treated as a live dev server")
		}
	})
}
