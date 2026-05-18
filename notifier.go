package nexus

// Notifier carries the "something changed" signal between nexus's
// mutating subsystems (registry, cron scheduler, rate-limit store)
// and any code that wants to observe state mutations.
//
// Why not a channel directly: callers that mutate state can't
// block on a slow consumer, and would have to pre-allocate the
// right buffer size. A non-blocking Notify with select-default
// drop is the right shape for "tell whoever's listening; never
// stall the request path".
//
// Relocated from live/notifier.go into the root package in the
// same change that removed the .nlt template engine; the Notifier
// is generic pub-sub and the original live/ home no longer made
// sense without the template-engine consumers.

import "sync"

// Notifier is the shared signal hub. Multiple subsystems call
// Notify(); multiple consumers (typically just streamLive, but the
// API supports more for future widgets) Subscribe to receive a
// nudge channel.
//
// Topic-aware: NotifyTopic("post:42") wakes only subscribers
// who joined that topic via SubscribeTopic; the un-topic'd
// Notify() and Subscribe() pair preserves the v0 broadcast
// behavior for callers that don't care about routing. The two
// surfaces overlap deliberately — Notify() also wakes every
// topic subscriber so a global "everything changed" still
// reaches everyone, while NotifyTopic stays scoped.
//
// Notify is cheap (single mutex acquire + N non-blocking sends) and
// safe from any goroutine. The zero value is unusable — callers
// must go through New so the maps are initialized.
type Notifier struct {
	mu        sync.Mutex
	listeners []chan struct{}            // broadcast subscribers (Subscribe)
	topics    map[string][]chan struct{} // topic → subscribers (SubscribeTopic)
	bus       Bus                        // optional cross-process fan-out (AttachBus)
}

// NewNotifier returns a fresh Notifier. Typically created once at
// app boot and threaded into each mutating subsystem via
// SetChangeHook (or the equivalent setter on each package).
//
// Production apps don't need to call this directly — nexus.Run
// wires a singleton into the fx graph; constructors that need a
// notifier just take a *Notifier param.
func NewNotifier() *Notifier {
	return &Notifier{
		topics: make(map[string][]chan struct{}),
	}
}

// Notify wakes every current subscriber whose channel has buffer
// room. Includes both broadcast subscribers (Subscribe) and every
// topic subscriber (SubscribeTopic), so "global change" callers
// don't need to know what topics exist. Subscribers whose channel
// already has a pending nudge are left alone — coalescing N rapid
// mutations into one wake-up is the whole point. Never blocks.
//
// When a Bus is attached, also Publish("") so peer nodes wake
// their local subscribers too — that's the horizontal-scale
// path. Publish errors are swallowed: the wake is best-effort,
// and a flaky bus shouldn't take down the mutator that called us.
func (n *Notifier) Notify() {
	if n == nil {
		return
	}
	n.notifyLocal()
	n.mu.Lock()
	bus := n.bus
	n.mu.Unlock()
	if bus != nil {
		_ = bus.Publish("")
	}
}

// NotifyTopic wakes only subscribers of the given topic.
// Broadcast subscribers (Subscribe with no topic) are NOT woken —
// they're for "tell me about everything" and would defeat the
// scoping if every topic notify also woke them. Use Notify() for
// the global case.
//
// Topic strings are arbitrary; common patterns are entity-scoped
// keys like "post:42" or "user:alice/inbox". An empty topic is a
// no-op (NotifyTopic("") matches no one — SubscribeTopic rejects
// empty strings).
//
// When a Bus is attached, also publishes the topic so peer nodes
// fan it out to their local subscribers. See Notify for the
// rationale on swallowed Publish errors.
func (n *Notifier) NotifyTopic(topic string) {
	if n == nil || topic == "" {
		return
	}
	n.notifyLocalTopic(topic)
	n.mu.Lock()
	bus := n.bus
	n.mu.Unlock()
	if bus != nil {
		_ = bus.Publish(topic)
	}
}

// notifyLocal wakes only this node's subscribers — no bus
// publish. Called by Notify (which then publishes) and by the
// Bus-attached forwarding goroutine (which received the message
// from a peer and shouldn't re-publish it).
func (n *Notifier) notifyLocal() {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, ch := range n.listeners {
		nudge(ch)
	}
	for _, subs := range n.topics {
		for _, ch := range subs {
			nudge(ch)
		}
	}
}

// notifyLocalTopic wakes only this node's subscribers for one
// topic. Same role as notifyLocal but topic-scoped — see comment
// there for why local-only.
func (n *Notifier) notifyLocalTopic(topic string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, ch := range n.topics[topic] {
		nudge(ch)
	}
}

// Subscribe registers a new broadcast listener and returns its
// nudge channel plus a cancel func. The channel has buffer 1 so a
// single pending notify is held even when the subscriber is busy.
// Cancel removes the listener and closes the channel; subsequent
// Notify calls skip it.
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

// SubscribeTopic registers a listener scoped to one topic. Same
// channel + cancel contract as Subscribe; the difference is that
// only NotifyTopic(topic) and the global Notify() wake the
// channel — NotifyTopic for other topics leaves it alone.
//
// Empty topic returns a closed channel and a no-op cancel — the
// caller usually has a programming error in that case (constructed
// a topic key from missing input), and a never-firing channel
// makes the symptom visible without panicking.
func (n *Notifier) SubscribeTopic(topic string) (<-chan struct{}, func()) {
	if topic == "" {
		ch := make(chan struct{})
		close(ch)
		return ch, func() {}
	}
	ch := make(chan struct{}, 1)
	n.mu.Lock()
	n.topics[topic] = append(n.topics[topic], ch)
	n.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			n.mu.Lock()
			subs := n.topics[topic]
			for i, c := range subs {
				if c == ch {
					n.topics[topic] = append(subs[:i], subs[i+1:]...)
					break
				}
			}
			if len(n.topics[topic]) == 0 {
				delete(n.topics, topic)
			}
			n.mu.Unlock()
			close(ch)
		})
	}
	return ch, cancel
}

// nudge does the non-blocking send dance used by Notify and
// NotifyTopic. Extracted so the two callers don't drift.
func nudge(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
		// Already pending — coalesce. The subscriber sees one
		// wake-up covering all the notifies it missed.
	}
}
