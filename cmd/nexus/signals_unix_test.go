//go:build !windows

package main

import (
	"context"
	"syscall"
	"testing"
	"time"
)

// Closing the terminal sends SIGHUP. nexus dev and nexus build must take
// their normal stop path on it — Vite and the app sit in process groups of
// their own, which the hangup never reaches — rather than die by the
// default action and orphan both.
func TestBuildSignalContext_Hangup(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGHUP, syscall.SIGTERM} {
		ctx, stop := buildSignalContext()
		if err := syscall.Kill(syscall.Getpid(), sig); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
			t.Errorf("%v did not cancel the context", sig)
		}
		stop()
	}
}

type fakeQuitter chan struct{}

func (f fakeQuitter) Quit() { f <- struct{}{} }

func TestQuitOnStopSignal_TUI(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGHUP, syscall.SIGTERM} {
		ctx, cancel := context.WithCancel(context.Background())
		q := make(fakeQuitter, 1)
		quitOnStopSignal(ctx, q)
		if err := syscall.Kill(syscall.Getpid(), sig); err != nil {
			t.Fatal(err)
		}
		select {
		case <-q:
		case <-time.After(2 * time.Second):
			t.Errorf("%v did not quit the TUI", sig)
		}
		// A second signal during the teardown is absorbed, not fatal:
		// the process is still here to run the next iteration.
		_ = syscall.Kill(syscall.Getpid(), sig)
		time.Sleep(50 * time.Millisecond)
		cancel()
	}
}
