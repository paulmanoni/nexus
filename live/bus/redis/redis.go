// Package redis is a Redis-backed live.Bus implementation. Mount it
// on a Notifier so notifications fan out across pods: a mutation on
// any node Publishes to a Redis pub/sub channel; every node's
// Notifier forwards the received topic to its local subscribers,
// waking sessions whose component subscribed.
//
// Usage:
//
//	rc := redis.NewClient(&redis.Options{Addr: "redis:6379"})
//	bus, _ := livebusredis.New(rc, "nexus.live")
//	notifier := live.New()
//	stop, _ := notifier.AttachBus(bus)
//	defer stop()
//	defer bus.Close()
//
// One Redis channel is shared by every node — the message payload
// carries the topic (or "" for global). Single-node deployments
// can skip this entirely; AttachBus is opt-in.
package redis

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"

	"github.com/redis/go-redis/v9"
)

// Bus is the Redis pub/sub implementation of live.Bus.
type Bus struct {
	client  *redis.Client
	channel string
	nodeID  string

	pub *redis.PubSub
	out chan string

	mu     sync.Mutex
	closed bool
	done   chan struct{}
}

// defaultChannel is used when New is called with empty channel.
// Match this across every pod sharing notifications.
const defaultChannel = "nexus.live"

// outBuffer is the depth of the internal receive channel. Sized
// to absorb a burst of remote notifications without dropping
// before the Notifier's forwarding goroutine drains; on overflow
// we drop with a log line rather than block the pubsub loop.
const outBuffer = 256

// New connects the bus to a Redis client and starts the subscribe
// loop. channel is the shared pub/sub key (defaults to
// "nexus.live"). Closing client is the caller's responsibility;
// Bus.Close only unsubscribes.
func New(client *redis.Client, channel string) (*Bus, error) {
	if client == nil {
		return nil, fmt.Errorf("livebus/redis: client is nil")
	}
	if channel == "" {
		channel = defaultChannel
	}
	nodeID, err := newNodeID()
	if err != nil {
		return nil, fmt.Errorf("livebus/redis: node id: %w", err)
	}
	b := &Bus{
		client:  client,
		channel: channel,
		nodeID:  nodeID,
		out:     make(chan string, outBuffer),
		done:    make(chan struct{}),
	}
	b.pub = client.Subscribe(context.Background(), channel)
	go b.pumpLoop()
	return b, nil
}

// Publish writes a topic to the shared Redis channel, prefixed
// with this node's ID so receivers can filter their own messages
// out (Redis pub/sub round-trips publishes to the publisher).
func (b *Bus) Publish(topic string) error {
	payload := b.nodeID + "|" + topic
	return b.client.Publish(context.Background(), b.channel, payload).Err()
}

// Subscribe returns the bus's receive channel. Cancel is a no-op
// for the Redis bus — the channel is single-subscriber and lives
// for the bus's lifetime. The Notifier is the expected consumer.
func (b *Bus) Subscribe() (<-chan string, func(), error) {
	return b.out, func() {}, nil
}

// Close stops the Redis subscription and closes the receive
// channel. Idempotent.
func (b *Bus) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	close(b.done)
	b.mu.Unlock()
	return b.pub.Close()
}

// pumpLoop reads messages from the pubsub channel, filters out
// this node's own publishes, and forwards remote topics into the
// out channel. Exits when Close is called.
func (b *Bus) pumpLoop() {
	defer close(b.out)
	ch := b.pub.Channel()
	for {
		select {
		case <-b.done:
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			id, topic, ok := splitPayload(msg.Payload)
			if !ok || id == b.nodeID {
				// Skip malformed payloads (other publishers
				// using the same channel) and our own
				// publishes (would double-fire local notifies).
				continue
			}
			select {
			case b.out <- topic:
			default:
				// Receiver hasn't drained — drop. The Notifier
				// is a buffer-1-per-subscriber broadcaster; one
				// missed nudge gets coalesced with the next.
			}
		}
	}
}

// splitPayload parses "<nodeID>|<topic>" into its parts. Returns
// false when the separator is missing — defensive against junk on
// the channel from a misconfigured peer.
func splitPayload(p string) (id, topic string, ok bool) {
	i := strings.IndexByte(p, '|')
	if i < 0 {
		return "", "", false
	}
	return p[:i], p[i+1:], true
}

// newNodeID returns a random 8-byte hex string used to filter
// own messages out of the pubsub stream. Crypto/rand because the
// IDs share a Redis channel; collisions would be observable as
// dropped notifies.
func newNodeID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
