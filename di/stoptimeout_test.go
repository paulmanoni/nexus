package di

import (
	"context"
	"testing"
	"time"
)

func TestWithStopTimeoutRecordsOnSpec(t *testing.T) {
	spec := Collect(WithStopTimeout(3 * time.Second))
	if spec.StopTimeout != 3*time.Second {
		t.Fatalf("StopTimeout = %s, want 3s", spec.StopTimeout)
	}
	// Last wins, so a caller can override a default contributed earlier in
	// the option tree.
	spec = Collect(WithStopTimeout(3*time.Second), WithStopTimeout(time.Second))
	if spec.StopTimeout != time.Second {
		t.Fatalf("StopTimeout = %s, want the later 1s", spec.StopTimeout)
	}
}

func TestBuildAppliesDefaultStopTimeout(t *testing.T) {
	if got := Build(Collect()).stopTimeout; got != DefaultStopTimeout {
		t.Fatalf("stopTimeout = %s, want DefaultStopTimeout", got)
	}
	if got := Build(Collect(WithStopTimeout(-1))).stopTimeout; got != DefaultStopTimeout {
		t.Fatalf("negative timeout = %s, want DefaultStopTimeout", got)
	}
	if got := Build(Collect(WithStopTimeout(2 * time.Second))).stopTimeout; got != 2*time.Second {
		t.Fatalf("stopTimeout = %s, want 2s", got)
	}
}

// A hook that honors its context must see the deadline Run installs, rather
// than the unbounded context.Background() that used to be passed. This is the
// http.Server.Shutdown case: it returns ctx.Err() once the window closes.
func TestStopContextCarriesDeadline(t *testing.T) {
	var deadline time.Time
	var ok bool
	app := Build(Collect(
		WithStopTimeout(2*time.Second),
		Invoke(func(lc Lifecycle) {
			lc.Append(Hook{OnStop: func(ctx context.Context) error {
				deadline, ok = ctx.Deadline()
				return nil
			}})
		}),
	))
	stopCtx, cancel := context.WithTimeout(context.Background(), app.stopTimeout)
	defer cancel()
	if err := app.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !ok {
		t.Fatal("OnStop received a context with no deadline")
	}
	if d := time.Until(deadline); d <= 0 || d > 2*time.Second {
		t.Fatalf("deadline %s away, want within (0, 2s]", d)
	}
}

// A hook that ignores its context entirely must not be able to hold shutdown
// open forever — Run abandons it once the window closes. Exercised against
// Stop-on-a-goroutine, the same shape Run uses.
func TestStopAbandonsWedgedHook(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	app := Build(Collect(Invoke(func(lc Lifecycle) {
		lc.Append(Hook{OnStop: func(context.Context) error {
			<-release // never returns within the window
			return nil
		}})
	})))

	stopCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- app.Stop(stopCtx) }()
	select {
	case <-done:
		t.Fatal("Stop returned even though the hook is still blocked")
	case <-stopCtx.Done():
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("waited %s to give up, want ~100ms", el)
	}
}
