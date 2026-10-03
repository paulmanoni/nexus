package connlog

import (
	"sync"
	"time"

	"go.uber.org/zap"
)

// Event is what a Transition tells its caller to log for one attempt:
// nothing, the moment a dependency went down, a periodic still-down
// heartbeat, or the recovery.
type Event int

const (
	EventNone Event = iota
	EventDown
	EventStillDown
	EventRecovered
)

// heartbeatSchedule widens the still-down cadence: quick confirmation early,
// then quiet — a long outage says one line every five minutes instead of one
// per retry tick.
var heartbeatSchedule = []time.Duration{10 * time.Second, time.Minute, 5 * time.Minute}

// Transition tracks one dependency's availability and shapes retry logging
// into STATE CHANGES: the first failure logs immediately, repeats collapse
// into widening heartbeats (a new error kind always breaks through), and
// recovery logs once with the outage's shape. The caller keeps its own
// message and domain fields; Transition supplies the state fields the log
// view, the dashboard, and the dev status strip key on.
type Transition struct {
	mu       sync.Mutex
	resource string
	now      func() time.Time

	down     bool
	since    time.Time
	attempts int
	sig      string
	lastLog  time.Time
	step     int
}

// NewTransition tracks the dependency named resource ("redis", "db:main").
func NewTransition(resource string) *Transition {
	return &Transition{resource: resource, now: time.Now}
}

// Fail records a failed attempt. ev says whether (and how) to log it;
// fields carries resource/state/attempts and, past the first line, how long
// the outage has run.
func (t *Transition) Fail(err error) (ev Event, fields []zap.Field) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	sig := Signature(err)
	t.attempts++
	if !t.down {
		t.down, t.since, t.sig, t.lastLog, t.step = true, now, sig, now, 0
		t.attempts = 1
		return EventDown, t.fieldsLocked("down", false)
	}
	if sig != t.sig {
		// A different failure is news, not a repeat — log it now and start
		// the heartbeat schedule over.
		t.sig, t.lastLog, t.step = sig, now, 0
		return EventStillDown, t.fieldsLocked("still-down", true)
	}
	if now.Sub(t.lastLog) >= heartbeatSchedule[t.step] {
		t.lastLog = now
		if t.step < len(heartbeatSchedule)-1 {
			t.step++
		}
		return EventStillDown, t.fieldsLocked("still-down", true)
	}
	return EventNone, nil
}

// OK records a successful attempt. EventRecovered (with the outage's length
// and attempt count) when the dependency was down; EventNone otherwise.
func (t *Transition) OK() (ev Event, fields []zap.Field) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.down {
		return EventNone, nil
	}
	fields = t.fieldsLocked("up", true)
	t.down, t.attempts, t.sig, t.step = false, 0, "", 0
	return EventRecovered, fields
}

// Down reports whether the dependency is currently considered down.
func (t *Transition) Down() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.down
}

func (t *Transition) fieldsLocked(state string, withDuration bool) []zap.Field {
	fields := []zap.Field{
		zap.String("resource", t.resource),
		zap.String("state", state),
		zap.Int("attempts", t.attempts),
	}
	if withDuration {
		fields = append(fields, zap.Duration("down_for", t.now().Sub(t.since).Round(time.Second)))
	}
	return fields
}
