package nexus

import (
	"fmt"
	"sync"
)

// Bus is the cross-process transport for notifier events.
// Implementations bridge a local Notifier to peers across pods
// so a mutation on node A reaches sessions connected to node B.
//
// Implementations live in sub-packages so importing live doesn't
// pull every backend's transitive deps:
//   - live/bus/redis — Redis pub/sub (go-redis)
//   - live/bus/nats  — NATS (planned)
//
// Implementations are responsible for any node-identity filtering
// (a publisher receiving its own message is allowed but wasteful);
// AttachBus on the Notifier side coalesces duplicates via the
// existing buffer-1 channel semantics.
type Bus interface {
	// Publish broadcasts a topic to every node subscribed via
	// Subscribe. Empty topic means the global "everything
	// changed" signal (equivalent to Notify with no topic).
	Publish(topic string) error

	// Subscribe returns a channel that receives topic strings
	// published by peers. Cancel detaches the subscription;
	// callers should close the bus separately when done.
	//
	// Single-subscriber model: implementations typically return
	// the same channel for every call (the Notifier is the only
	// expected consumer).
	Subscribe() (<-chan string, func(), error)

	// Close releases the bus's resources (network connections,
	// pubsub subscriptions). Idempotent.
	Close() error
}

// AttachBus wires a Bus to this Notifier. After Attach:
//   - Every Notify() and NotifyTopic() also Publish to the bus
//     so peers see the change.
//   - Incoming bus messages fan out to this node's local
//     subscribers (Subscribe / SubscribeTopic).
//
// Returns a cancel func that detaches the forwarding goroutine.
// The Bus itself is not closed by cancel — caller owns its
// lifecycle (typically via fx Lifecycle.OnStop).
//
// Returns an error if a bus is already attached; only one bus
// per Notifier is supported. To switch buses, detach the old
// one first and call AttachBus again.
func (n *Notifier) AttachBus(b Bus) (func(), error) {
	n.mu.Lock()
	if n.bus != nil {
		n.mu.Unlock()
		return nil, fmt.Errorf("live: bus already attached")
	}
	n.bus = b
	n.mu.Unlock()

	incoming, unsub, err := b.Subscribe()
	if err != nil {
		n.mu.Lock()
		n.bus = nil
		n.mu.Unlock()
		return nil, fmt.Errorf("live: bus subscribe: %w", err)
	}
	done := make(chan struct{})
	var once sync.Once
	go func() {
		for {
			select {
			case <-done:
				unsub()
				return
			case topic, ok := <-incoming:
				if !ok {
					return
				}
				if topic == "" {
					n.notifyLocal()
				} else {
					n.notifyLocalTopic(topic)
				}
			}
		}
	}()
	cancel := func() {
		once.Do(func() {
			close(done)
			n.mu.Lock()
			n.bus = nil
			n.mu.Unlock()
		})
	}
	return cancel, nil
}
