// Package live carries the "something changed" signal between
// nexus's mutating subsystems (registry, cron scheduler, rate-limit
// store) and the dashboard's live snapshot stream. It is a leaf
// package by design — registry / cron / ratelimit can import it
// without touching dashboard, and dashboard wires the listening
// side without each mutator needing to know dashboard exists.
//
// Why not a channel directly: callers that mutate state
// (RegisterEndpoint, UpdateWorkerStatus, AttachResource) can't
// block on a slow dashboard, and would have to pre-allocate the
// right buffer size. A non-blocking Notify with select-default
// drop is the right shape for "tell whoever's listening; never
// stall the request path".
package live

import "sync"

// Notifier is the shared signal hub. Multiple subsystems call
// Notify(); multiple consumers (typically just streamLive, but the
// API supports more for future widgets) Subscribe to receive a
// nudge channel.
//
// Notify is cheap (single mutex acquire + N non-blocking sends) and
// safe from any goroutine. The zero value is unusable — callers
// must go through New so the listeners slice is initialized.
type Notifier struct {
	mu        sync.Mutex
	listeners []chan struct{}
}

// New returns a fresh Notifier. Typically created once at app boot
// and threaded into each mutating subsystem via SetChangeHook (or
// the equivalent setter on each package).
func New() *Notifier {
	return &Notifier{}
}

// Notify wakes every current subscriber whose channel has buffer
// room. Subscribers whose channel already has a pending nudge are
// left alone — coalescing N rapid mutations into one wake-up is the
// whole point. Never blocks the caller.
func (n *Notifier) Notify() {
	if n == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, ch := range n.listeners {
		select {
		case ch <- struct{}{}:
		default:
			// Already pending — coalesce. The subscriber will see
			// the next read as one wake-up that covers all the
			// notifies it missed.
		}
	}
}

// Subscribe registers a new listener and returns its nudge channel
// plus a cancel func. The channel has buffer 1 so a single pending
// notify is held even when the subscriber is busy. Cancel removes
// the listener and closes the channel; subsequent Notify calls
// skip it.
//
// Cancel is safe to call multiple times.
func (n *Notifier) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	n.mu.Lock()
	n.listeners = append(n.listeners, ch)
	n.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			n.mu.Lock()
			for i, c := range n.listeners {
				if c == ch {
					n.listeners = append(n.listeners[:i], n.listeners[i+1:]...)
					break
				}
			}
			n.mu.Unlock()
			close(ch)
		})
	}
	return ch, cancel
}