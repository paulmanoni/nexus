package connlog

import (
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"
)

func fieldMap(fs []zap.Field) map[string]any {
	out := map[string]any{}
	for _, f := range fs {
		switch f.Type {
		case 15: // zapcore.StringType
			out[f.Key] = f.String
		default:
			out[f.Key] = f.Integer
		}
	}
	return out
}

// TestTransition: first failure logs immediately, repeats collapse into a
// widening heartbeat, a new error kind breaks through, recovery logs once
// with the outage's shape, and a healthy OK is silent.
func TestTransition(t *testing.T) {
	now := time.Unix(1000, 0)
	tr := NewTransition("redis")
	tr.now = func() time.Time { return now }
	errA, errB := errors.New("connection refused"), errors.New("WRONGPASS bad auth")

	if ev, fs := tr.Fail(errA); ev != EventDown || fieldMap(fs)["state"] != "down" {
		t.Fatalf("first failure: ev=%v fields=%v", ev, fs)
	}
	if !tr.Down() {
		t.Fatal("Down() should report true during an outage")
	}
	// Repeats inside the first 10s window are silent.
	for i := 0; i < 5; i++ {
		now = now.Add(time.Second)
		if ev, _ := tr.Fail(errA); ev != EventNone {
			t.Fatalf("repeat %d inside window logged: %v", i, ev)
		}
	}
	// Past 10s → heartbeat, with attempts and duration.
	now = now.Add(6 * time.Second)
	ev, fs := tr.Fail(errA)
	m := fieldMap(fs)
	if ev != EventStillDown || m["state"] != "still-down" || m["attempts"].(int64) != 7 {
		t.Fatalf("heartbeat: ev=%v fields=%v", ev, m)
	}
	// The next window widened to 1m: +30s stays silent, +61s beats.
	now = now.Add(30 * time.Second)
	if ev, _ := tr.Fail(errA); ev != EventNone {
		t.Fatal("widened window did not suppress")
	}
	now = now.Add(31 * time.Second)
	if ev, _ := tr.Fail(errA); ev != EventStillDown {
		t.Fatal("second heartbeat missing")
	}
	// A NEW error kind breaks through immediately.
	now = now.Add(time.Second)
	if ev, _ := tr.Fail(errB); ev != EventStillDown {
		t.Fatal("new error kind suppressed")
	}
	// Recovery logs once, then goes quiet; the next outage is news again.
	now = now.Add(time.Second)
	ev, fs = tr.OK()
	m = fieldMap(fs)
	if ev != EventRecovered || m["state"] != "up" || m["down_for"].(int64) <= 0 {
		t.Fatalf("recovery: ev=%v fields=%v", ev, m)
	}
	if ev, _ := tr.OK(); ev != EventNone {
		t.Fatal("healthy OK logged")
	}
	if ev, _ := tr.Fail(errA); ev != EventDown {
		t.Fatal("next outage not news")
	}
}
