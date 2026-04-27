package nexus

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"

	"github.com/paulmanoni/nexus/registry"
)

func TestAsWorker_RunsUntilStopSignalsCtx(t *testing.T) {
	var started int32
	var stopped int32
	done := make(chan struct{})

	worker := func(ctx context.Context) error {
		atomic.StoreInt32(&started, 1)
		<-ctx.Done()
		atomic.StoreInt32(&stopped, 1)
		close(done)
		return nil
	}

	var app *App
	fxApp := fxtest.New(t,
		fxBootOptions(Config{Server: ServerConfig{Addr: "127.0.0.1:0"}}),
		AsWorker("test-worker", worker).nexusOption(),
		fx.Populate(&app),
	)
	fxApp.RequireStart()

	// Wait until the goroutine has entered its body (Status transitioned
	// to "running"). Budget: 500ms is generous for an in-process
	// lifecycle hook to land.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&started) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if atomic.LoadInt32(&started) != 1 {
		t.Fatal("worker never started")
	}

	// Verify the registry entry flipped to running.
	var found bool
	for _, w := range app.Registry().Workers() {
		if w.Name == "test-worker" && w.Status == "running" {
			found = true
		}
	}
	if !found {
		t.Errorf("worker not running in registry; got %+v", app.Registry().Workers())
	}

	fxApp.RequireStop()

	// OnStop cancels ctx; worker returns; runWorker records "stopped".
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("worker did not observe ctx.Done()")
	}
	if atomic.LoadInt32(&stopped) != 1 {
		t.Error("worker stopped flag not set")
	}

	var stopSeen bool
	for _, w := range app.Registry().Workers() {
		if w.Name == "test-worker" && w.Status == "stopped" {
			stopSeen = true
		}
	}
	if !stopSeen {
		t.Errorf("worker should be stopped post-shutdown; got %+v", app.Registry().Workers())
	}
}

func TestAsWorker_ErrorReturnMarksFailed(t *testing.T) {
	worker := func(ctx context.Context) error {
		return errors.New("boom")
	}

	var app *App
	fxApp := fxtest.New(t,
		fxBootOptions(Config{Server: ServerConfig{Addr: "127.0.0.1:0"}}),
		AsWorker("bad-worker", worker).nexusOption(),
		fx.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	// Worker returns immediately; give the goroutine a moment.
	time.Sleep(100 * time.Millisecond)

	var found bool
	for _, w := range app.Registry().Workers() {
		if w.Name == "bad-worker" {
			found = true
			if w.Status != "failed" {
				t.Errorf("status = %q; want failed", w.Status)
			}
			if w.LastError != "boom" {
				t.Errorf("LastError = %q; want boom", w.LastError)
			}
		}
	}
	if !found {
		t.Fatal("bad-worker not registered")
	}
}

func TestAsWorker_CapturesDeps(t *testing.T) {
	// Worker takes a resource-provider + a service wrapper; both should
	// appear in the registered Worker's dep lists, so the dashboard's
	// architecture view can draw edges to them.
	worker := func(ctx context.Context, db *fakeDB, users *UsersService) error {
		<-ctx.Done()
		return nil
	}
	var app *App
	fxApp := fxtest.New(t,
		fxBootOptions(Config{Server: ServerConfig{Addr: "127.0.0.1:0"}}),
		Provide(func() *fakeDB { return &fakeDB{} }).nexusOption(),
		Provide(NewUsersService).nexusOption(),
		AsWorker("cache-invalidation", worker).nexusOption(),
		fx.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	// Brief wait for the goroutine to register — the Invoke that
	// records deps runs synchronously at Start though, so the entry
	// should be there immediately.
	time.Sleep(50 * time.Millisecond)

	var w *registry.Worker
	for _, reg := range app.Registry().Workers() {
		if reg.Name == "cache-invalidation" {
			tmp := reg
			w = &tmp
		}
	}
	if w == nil {
		t.Fatal("worker not registered")
	}
	if len(w.ResourceDeps) != 1 || w.ResourceDeps[0] != "main" {
		t.Errorf("ResourceDeps = %v; want [main]", w.ResourceDeps)
	}
	if len(w.ServiceDeps) != 1 || w.ServiceDeps[0] != "users" {
		t.Errorf("ServiceDeps = %v; want [users]", w.ServiceDeps)
	}
}