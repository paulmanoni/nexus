package template

import "sync/atomic"

// Stats is the engine's observability snapshot — every counter
// monotonic except SessionsOpen, which is a live gauge. Read via
// Engine.Stats() (lock-free); the values are a coherent point-in-
// time read but individual counters may be slightly stale relative
// to each other on a busy engine. That's the trade-off for using
// atomics instead of a global lock on every event.
//
// Intended consumers:
//   - the nexus dashboard ("/__nexus/live" tab, when wired)
//   - test assertions ("after 5 events the counter should be 5")
//   - external scrapers via a thin HTTP handler the app exposes
//
// The fields are stable; new counters can be appended (additive,
// not breaking) but existing names won't be renamed.
type Stats struct {
	// SessionsOpen is the current number of live sessions. Goes
	// up on Run start, down on Run exit.
	SessionsOpen int64

	// SessionsTotal counts every session ever started, including
	// resumed ones. SessionsTotal - SessionsResumed = number of
	// fresh Mounts.
	SessionsTotal int64

	// SessionsResumed counts sessions that started from a parked
	// entry (state preserved across reconnect).
	SessionsResumed int64

	// SessionsParked counts disconnected sessions whose component
	// was stashed for resumption. Includes entries that expired
	// without ever being claimed — that's the "wasted parks" cost
	// of the resumption TTL knob.
	SessionsParked int64

	// RendersTotal counts every Render call (initial + every
	// re-render). Divide by SessionsTotal for "renders per
	// session"; divide by uptime for "renders per second".
	RendersTotal int64

	// EventsTotal counts inbound client events handled, including
	// the synthetic __model events from nl-model two-way binding.
	EventsTotal int64

	// DiffsTotal counts non-empty diffs sent to clients. Diffs
	// where nothing changed don't count — the wire stays quiet
	// and so does this number.
	DiffsTotal int64

	// DiffsDropped counts frames the session refused to enqueue
	// because the outgoing queue was full (see WithSendBuffer).
	// A non-zero value means at least one client was too slow
	// and got disconnected; treat it as a "slow consumers per
	// hour" indicator.
	DiffsDropped int64
}

// engineStats holds the live atomic counters. Separate from
// Stats so the public type stays plain values and the snapshot
// method can read once per field cleanly.
type engineStats struct {
	sessionsOpen    atomic.Int64
	sessionsTotal   atomic.Int64
	sessionsResumed atomic.Int64
	sessionsParked  atomic.Int64
	rendersTotal    atomic.Int64
	eventsTotal     atomic.Int64
	diffsTotal      atomic.Int64
	diffsDropped    atomic.Int64
}

// snapshot returns a Stats value with each counter loaded once.
// Counters may be slightly out of sync with each other on a
// concurrent engine — acceptable for observability.
func (s *engineStats) snapshot() Stats {
	return Stats{
		SessionsOpen:    s.sessionsOpen.Load(),
		SessionsTotal:   s.sessionsTotal.Load(),
		SessionsResumed: s.sessionsResumed.Load(),
		SessionsParked:  s.sessionsParked.Load(),
		RendersTotal:    s.rendersTotal.Load(),
		EventsTotal:     s.eventsTotal.Load(),
		DiffsTotal:      s.diffsTotal.Load(),
		DiffsDropped:    s.diffsDropped.Load(),
	}
}
