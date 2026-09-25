//go:build !windows

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A grandchild that keeps Vite's piped output open after Vite exits (the
// node process behind vite.cmd on Windows) must not hang Wait: viteCmd sets
// WaitDelay, so the pipe is closed and Wait returns.
func TestViteCmd_WaitDoesNotHangOnAnInheritedPipe(t *testing.T) {
	_, web := fakeViteProject(t, "#!/bin/sh\nsleep 30 &\necho $! > child.pid\nexit 0\n")
	cmd, err := viteCmd(context.Background(), web, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	start := time.Now()
	_ = cmd.Run()
	took := time.Since(start)
	if b, err := os.ReadFile(filepath.Join(web, "child.pid")); err == nil {
		if pid, _ := strconv.Atoi(strings.TrimSpace(string(b))); pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	if took > viteKillGrace+3*time.Second {
		t.Errorf("Wait took %s with a grandchild holding the pipe; want about WaitDelay (%s)", took, viteKillGrace)
	}
}

// Vite runs in its own process group, out of reach of the terminal's
// Ctrl-C; cancelling the build's context must stop it, children included.
func TestFrontendBuild_CancelStopsViteGroup(t *testing.T) {
	t.Setenv("NEXUS_FRONTEND_DIR", "")
	root, web := fakeViteProject(t, "#!/bin/sh\nsleep 30 &\necho $! > child.pid\nwait\n")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- frontendBuild(ctx, root, "", &bytes.Buffer{}, &bytes.Buffer{}) }()
	pidFile := filepath.Join(web, "child.pid")
	deadline := time.Now().Add(5 * time.Second)
	for !fileExists(pidFile) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "interrupted") {
			t.Errorf("err = %v, want interrupted", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("frontendBuild did not return after cancel")
	}
	b, _ := os.ReadFile(pidFile)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid <= 0 {
		t.Fatal("fake vite never started its child")
	}
	for i := 0; i < 50 && syscall.Kill(pid, 0) == nil; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if syscall.Kill(pid, 0) == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Error("vite's child survived the cancel")
	}
}
