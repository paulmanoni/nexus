package view

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"reflect"
	"sync"
	"time"

	"github.com/paulmanoni/nexus/v2"
)

// Relay carries broadcasts between the replicas of an app, so
// view.Broadcast reaches every connected page, not only those on the
// replica that sent it. It moves opaque payloads: Publish sends one to every
// replica, its own included; Subscribe delivers each until ctx ends,
// reconnecting on its own. Redis pub/sub is a natural fit
// (extension/cache/redis/viewrelay); MemoryRelay is one in a process.
type Relay interface {
	Publish(ctx context.Context, payload []byte) error
	Subscribe(ctx context.Context, deliver func(payload []byte)) error
}

// UseRelay makes this app's broadcasts reach the live pages of every replica
// sharing r:
//
//	nexus.Boot(view.UseRelay(viewrelay.New(viewrelay.Config{URL: os.Getenv("REDIS_URL")})), …)
//
// Each broadcast is delivered to this replica's pages at once, as without a
// relay, and published for the others; a replica ignores its own. Data
// crosses replicas as JSON: read it with Message.Decode, which works for
// both. Broadcast still counts only the pages on this replica.
func UseRelay(r Relay) nexus.Option {
	return nexus.Invoke(func(lc nexus.Lifecycle) {
		var stop context.CancelFunc
		lc.Append(nexus.Hook{
			OnStart: func(context.Context) error {
				ctx, cancel := context.WithCancel(context.Background())
				stop = cancel
				startRelay(ctx, r)
				return nil
			},
			OnStop: func(context.Context) error {
				if stop != nil {
					stop()
				}
				stopRelay(r)
				return nil
			},
		})
	})
}

// replica identifies this process's broadcasts on a relay.
var replica = func() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}()

// envelope is a broadcast on the relay.
type envelope struct {
	Origin string          `json:"o"`
	Topic  string          `json:"t"`
	Data   json.RawMessage `json:"d,omitempty"`
}

// relayQueue bounds the broadcasts waiting to be published: past it, a
// broadcast reaches this replica's pages only, and the drop is logged.
const relayQueue = 1024

type activeRelay struct {
	r     Relay
	queue chan envelope
}

var relays = struct {
	sync.Mutex
	active []*activeRelay
}{}

func startRelay(ctx context.Context, r Relay) {
	a := &activeRelay{r: r, queue: make(chan envelope, relayQueue)}
	relays.Lock()
	relays.active = append(relays.active, a)
	relays.Unlock()
	go runPresence(ctx)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case env := <-a.queue:
				b, _ := json.Marshal(env)
				pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				if err := r.Publish(pctx, b); err != nil {
					log.Printf("nexus view: relay: publishing to %q: %v", env.Topic, err)
				}
				cancel()
			}
		}
	}()
	go func() {
		for ctx.Err() == nil {
			err := r.Subscribe(ctx, func(payload []byte) {
				var env envelope
				if json.Unmarshal(payload, &env) != nil || env.Origin == replica {
					return
				}
				if env.Topic == presenceTopic {
					onRemotePresence(env.Origin, env.Data)
					return
				}
				deliver(env.Topic, env.Data)
			})
			if ctx.Err() != nil {
				return
			}
			log.Printf("nexus view: relay: subscription ended: %v; retrying", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}()
}

func stopRelay(r Relay) {
	relays.Lock()
	defer relays.Unlock()
	for i, a := range relays.active {
		if a.r == r {
			relays.active = append(relays.active[:i], relays.active[i+1:]...)
			return
		}
	}
}

// relay hands a broadcast to every active relay.
func relay(topic string, data any) {
	relays.Lock()
	active := append([]*activeRelay(nil), relays.active...)
	relays.Unlock()
	if len(active) == 0 {
		return
	}
	b, err := json.Marshal(data)
	if err != nil {
		log.Printf("nexus view: relay: broadcast to %q stays on this replica: its data is not JSON: %v", topic, err)
		return
	}
	env := envelope{Origin: replica, Topic: topic, Data: b}
	for _, a := range active {
		select {
		case a.queue <- env:
		default:
			log.Printf("nexus view: relay: queue full, broadcast to %q stays on this replica", topic)
		}
	}
}

// Decode reads the message's data into v: the publisher's value as is when
// it was sent on this replica, its JSON when it came from another.
func (m Message) Decode(v any) error {
	if raw, ok := m.Data.(json.RawMessage); ok {
		return json.Unmarshal(raw, v)
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Pointer && !rv.IsNil() && m.Data != nil {
		if dv := reflect.ValueOf(m.Data); dv.Type().AssignableTo(rv.Elem().Type()) {
			rv.Elem().Set(dv)
			return nil
		}
	}
	b, err := json.Marshal(m.Data)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// MemoryRelay is a Relay within one process — for tests, or several apps in
// one binary.
type MemoryRelay struct {
	mu   sync.Mutex
	subs map[int]func([]byte)
	next int
}

func NewMemoryRelay() *MemoryRelay { return &MemoryRelay{subs: map[int]func([]byte){}} }

func (m *MemoryRelay) Publish(_ context.Context, payload []byte) error {
	m.mu.Lock()
	subs := make([]func([]byte), 0, len(m.subs))
	for _, s := range m.subs {
		subs = append(subs, s)
	}
	m.mu.Unlock()
	for _, s := range subs {
		s(payload)
	}
	return nil
}

func (m *MemoryRelay) Subscribe(ctx context.Context, deliver func([]byte)) error {
	m.mu.Lock()
	id := m.next
	m.next++
	m.subs[id] = deliver
	m.mu.Unlock()
	<-ctx.Done()
	m.mu.Lock()
	delete(m.subs, id)
	m.mu.Unlock()
	return ctx.Err()
}
