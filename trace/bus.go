package trace

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Bus is a bounded ring buffer + pub/sub. Slow subscribers drop events rather
// than block producers — the request path must never be held up by a stalled UI.
//
// The ring and subscriber fan-out are SHARDED BY TRACE ID: with the dashboard
// open, every request publishes 2-3 events, and a single mutex here was the
// last process-wide serialization point on the hot path. Keying the shard on
// the trace means one trace's events always share a shard, so their relative
// order survives; cross-shard order is recovered where it matters (Subscribe
// backlogs) by the global monotonic ID every event already carries.
//
// Full-size buses shard docBusShards ways; small ones (capacity below
// busShards²·2) stay single-shard, keeping exact global ring order for tests
// and tiny configs.
type Bus struct {
	shards   []busShard
	mask     uint32 // len(shards)-1; len is a power of two
	capTotal int
	seq      atomic.Int64
	subMu    sync.Mutex // guards subID across Subscribe calls
	subID    int
}

// busShards is the shard count for full-size buses — a power of two so the
// shard pick is a mask.
const busShards = 8

type busShard struct {
	mu          sync.Mutex
	capacity    int
	buf         []Event
	next        int
	size        int
	subscribers map[int]chan Event
}

func NewBus(capacity int) *Bus {
	if capacity <= 0 {
		capacity = 1000
	}
	n := 1
	if capacity >= busShards*busShards*2 {
		n = busShards
	}
	b := &Bus{
		shards:   make([]busShard, n),
		mask:     uint32(n - 1),
		capTotal: capacity,
	}
	per, extra := capacity/n, capacity%n
	for i := range b.shards {
		c := per
		if i < extra {
			c++
		}
		b.shards[i] = busShard{
			capacity:    c,
			buf:         make([]Event, c),
			subscribers: map[int]chan Event{},
		}
	}
	return b
}

// shardFor picks the shard for a trace ID (FNV-1a). All of one trace's
// events land in one shard, so per-trace ordering rides that shard's mutex.
func (b *Bus) shardFor(traceID string) *busShard {
	if b.mask == 0 {
		return &b.shards[0]
	}
	h := uint32(2166136261)
	for i := 0; i < len(traceID); i++ {
		h ^= uint32(traceID[i])
		h *= 16777619
	}
	return &b.shards[h&b.mask]
}

func (b *Bus) Publish(e Event) {
	e.ID = b.seq.Add(1)
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	sh := b.shardFor(e.TraceID)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sh.buf[sh.next] = e
	sh.next = (sh.next + 1) % sh.capacity
	if sh.size < sh.capacity {
		sh.size++
	}
	// Non-blocking sends under the shard lock: a full or slow subscriber drops
	// the event rather than blocking the producer (the request path must never
	// be held up by a stalled UI). Sending while holding the lock — rather than
	// copying the subscriber list and sending after unlocking — is what makes
	// cancellation race-free: a channel is only ever sent to while it is still
	// in the shard's map, and cancel() deletes it from EVERY shard under each
	// shard's lock before closing, so a send can never land on a closed channel.
	for _, c := range sh.subscribers {
		select {
		case c <- e:
		default:
		}
	}
}

// SnapshotByTrace returns every event currently in the ring whose TraceID
// matches, in publish order. Backs the dashboard's per-trace waterfall view.
// Returns nil when no events match. One trace lives in one shard, so this
// scans a single ring.
func (b *Bus) SnapshotByTrace(traceID string) []Event {
	if traceID == "" {
		return nil
	}
	sh := b.shardFor(traceID)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	start := (sh.next - sh.size + sh.capacity) % sh.capacity
	var out []Event
	for i := 0; i < sh.size; i++ {
		e := sh.buf[(start+i)%sh.capacity]
		if e.TraceID == traceID {
			out = append(out, e)
		}
	}
	return out
}

// Subscribe atomically captures the backlog (events with ID > sinceID) and
// registers for future events. Per shard, backlog capture and registration
// happen under one lock, so within a shard a reconnecting client sees every
// event exactly once — no gap, no duplicate; each event lives in exactly one
// shard, so the guarantee is global. The merged backlog is sorted by the
// bus-wide monotonic ID.
//
// Delivery is fanned in: each shard gets its own buffered channel and one
// forwarder goroutine moves events onto the subscriber's channel. Producers
// therefore only ever contend with their own shard — never with the other
// shards' publishes racing toward the same subscriber. A slow consumer
// backpressures the forwarders, the shard channels fill, and Publish drops —
// the same drop-don't-block contract as before. Per-trace ordering survives:
// one trace → one shard → one FIFO channel → one forwarder.
func (b *Bus) Subscribe(sinceID int64, buffer int) ([]Event, <-chan Event, func()) {
	if buffer <= 0 {
		buffer = 128
	}
	perShard := buffer / len(b.shards)
	if perShard < 16 {
		perShard = 16
	}
	out := make(chan Event, buffer)
	b.subMu.Lock()
	id := b.subID
	b.subID++
	b.subMu.Unlock()

	feeds := make([]chan Event, len(b.shards))
	var backlog []Event
	for i := range b.shards {
		sh := &b.shards[i]
		feeds[i] = make(chan Event, perShard)
		sh.mu.Lock()
		start := (sh.next - sh.size + sh.capacity) % sh.capacity
		for j := 0; j < sh.size; j++ {
			e := sh.buf[(start+j)%sh.capacity]
			if e.ID > sinceID {
				backlog = append(backlog, e)
			}
		}
		sh.subscribers[id] = feeds[i]
		sh.mu.Unlock()
	}
	sort.Slice(backlog, func(i, j int) bool { return backlog[i].ID < backlog[j].ID })

	var wg sync.WaitGroup
	for _, feed := range feeds {
		wg.Add(1)
		go func(feed chan Event) {
			defer wg.Done()
			for e := range feed {
				out <- e
			}
		}(feed)
	}
	go func() {
		wg.Wait()
		close(out)
	}()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			// Remove each feed from its shard before closing it. The delete
			// happens under the shard's lock — the same lock Publish holds
			// while sending — so no in-flight Publish can send to a closed
			// feed. Closing the feeds ends the forwarders, which drain what
			// remains and then close the subscriber channel.
			for i := range b.shards {
				sh := &b.shards[i]
				sh.mu.Lock()
				delete(sh.subscribers, id)
				sh.mu.Unlock()
				close(feeds[i])
			}
		})
	}
	return backlog, out, cancel
}
