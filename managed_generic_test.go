package nexus

import (
	"context"
	"sync/atomic"
	"testing"

	"go.uber.org/fx"
	"go.uber.org/zap"

	"github.com/paulmanoni/nexus/resource"
)

// fakeMgr mimics a custom manager (db/cache/queue shape): void
// Start()/Stop() + a health func.
type fakeMgr struct {
	started, stopped atomic.Bool
	connected        atomic.Bool
}

func (m *fakeMgr) Start()              { m.started.Store(true); m.connected.Store(true) }
func (m *fakeMgr) Stop()               { m.stopped.Store(true); m.connected.Store(false) }
func (m *fakeMgr) IsConnected() bool   { return m.connected.Load() }

// fakeHandle is the marker, with an extra field (cfg) like the real
// RabbitMQ — proving build-constructs-the-whole-T (no reflection).
type fakeHandle struct {
	*fakeMgr
	cfg string
}

func TestManaged_LifecycleAndInjectionAndResource(t *testing.T) {
	app := New(Config{})

	var got *fakeHandle
	opt := Managed("fake",
		func(_ *zap.Logger) (*fakeHandle, error) {
			return &fakeHandle{fakeMgr: &fakeMgr{}, cfg: "configured"}, nil
		},
		func(h *fakeHandle) resource.Resource {
			return resource.NewQueue("fake", "fake broker",
				map[string]any{"k": "v"}, h.IsConnected)
		},
	)

	fxapp := fx.New(
		fx.Supply(app),
		fx.Supply(zap.NewNop()),
		opt.nexusOption(),
		fx.Populate(&got),
	)
	ctx := context.Background()
	if err := fxapp.Start(ctx); err != nil {
		t.Fatalf("fx start: %v", err)
	}
	if got == nil || got.fakeMgr == nil {
		t.Fatal("handle not injected")
	}
	if got.cfg != "configured" {
		t.Errorf("extra field not preserved: cfg=%q", got.cfg)
	}
	if !got.started.Load() {
		t.Error("Start() was not called on boot")
	}
	if !got.IsConnected() { // promoted from *fakeMgr
		t.Error("expected connected after Start")
	}

	if err := fxapp.Stop(ctx); err != nil {
		t.Fatalf("fx stop: %v", err)
	}
	if !got.stopped.Load() {
		t.Error("Stop() was not called on shutdown")
	}
}

// closerMgr exercises the Close() error lifecycle branch.
type closerMgr struct{ closed atomic.Bool }

func (m *closerMgr) Close() error { m.closed.Store(true); return nil }

func TestManaged_ClosePathAndNilResource(t *testing.T) {
	app := New(Config{})
	type h struct{ *closerMgr }
	var got *h
	opt := Managed("closer",
		func(_ *zap.Logger) (*h, error) { return &h{&closerMgr{}}, nil },
		nil, // no dashboard resource
	)
	fxapp := fx.New(fx.Supply(app), fx.Supply(zap.NewNop()), opt.nexusOption(), fx.Populate(&got))
	ctx := context.Background()
	if err := fxapp.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := fxapp.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !got.closed.Load() {
		t.Error("Close() was not called on shutdown")
	}
}

func TestManaged_NilBuildPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for nil build")
		}
	}()
	_ = Managed[fakeHandle]("x", nil, nil)
}
