package trace

import (
	"sync"
	"sync/atomic"
	"time"
)

// Bus is a bounded ring buffer + pub/sub. Slow subscribers drop events rather
// than block producers — the request path must never be held up by a stalled UI.
type Bus struct {
	mu          sync.Mutex
	capacity    int
	buf         []Event
	next        int
	size        int
	seq         atomic.Int64
	subscribers map[int]chan Event
	subID       int
}

func NewBus(capacity int) *Bus {
	if capacity <= 0 {
		capacity = 1000
	}
	return &Bus{
		capacity:    capacity,
		buf:         make([]Event, capacity),
		subscribers: map[int]chan Event{},
	}
}

func (b *Bus) Publish(e Event) {
	e.ID = b.seq.Add(1)
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf[b.next] = e
	b.next = (b.next + 1) % b.capacity
	if b.size < b.capacity {
		b.size++
	}
	// Non-blocking sends under the lock: a full or slow subscriber drops the
	// event rather than blocking the producer (the request path must never be
	// held up by a stalled UI). Sending while holding the lock — rather than
	// copying the subscriber list and sending after unlocking — is what makes
	// cancellation race-free: a channel is only ever sent to while it is still
	// in the map, and cancel() deletes-then-closes it under this same lock, so
	// a send can never land on a closed channel.
	for _, c := range b.subscribers {
		select {
		case c <- e:
		default:
		}
	}
}

// SnapshotByTrace returns every event currently in the ring whose TraceID
// matches, in publish order. Backs the dashboard's per-trace waterfall view.
// Returns nil when no events match.
func (b *Bus) SnapshotByTrace(traceID string) []Event {
	if traceID == "" {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	start := (b.next - b.size + b.capacity) % b.capacity
	var out []Event
	for i := 0; i < b.size; i++ {
		e := b.buf[(start+i)%b.capacity]
		if e.TraceID == traceID {
			out = append(out, e)
		}
	}
	return out
}

// Subscribe atomically captures the backlog (events with ID > sinceID) and
// registers a channel for future events. Doing both under one lock guarantees
// a reconnecting client sees every event exactly once — no gap, no duplicate.
func (b *Bus) Subscribe(sinceID int64, buffer int) ([]Event, <-chan Event, func()) {
	if buffer <= 0 {
		buffer = 128
	}
	ch := make(chan Event, buffer)
	b.mu.Lock()
	start := (b.next - b.size + b.capacity) % b.capacity
	backlog := make([]Event, 0, b.size)
	for i := 0; i < b.size; i++ {
		e := b.buf[(start+i)%b.capacity]
		if e.ID > sinceID {
			backlog = append(backlog, e)
		}
	}
	id := b.subID
	b.subID++
	b.subscribers[id] = ch
	b.mu.Unlock()
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			// Delete and close under the same lock Publish holds while sending,
			// so the channel is removed from the subscriber set before it is
			// closed and no in-flight Publish can send to it afterwards.
			b.mu.Lock()
			delete(b.subscribers, id)
			close(ch)
			b.mu.Unlock()
		})
	}
	return backlog, ch, cancel
}
