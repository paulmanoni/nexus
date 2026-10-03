package jobs_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/extension/jobs"
)

// fakeBroker is an in-memory message broker: delayed publishes arrive at
// RunAt, deliveries must be acked or requeued, failures are buried.
type fakeBroker struct {
	mu       sync.Mutex
	queues   map[string]chan jobs.Record
	buried   []jobs.Record
	acks     atomic.Int32
	requeues atomic.Int32
	publish  []jobs.Record
}

func newFakeBroker() *fakeBroker { return &fakeBroker{queues: map[string]chan jobs.Record{}} }

func (b *fakeBroker) queue(name string) chan jobs.Record {
	b.mu.Lock()
	defer b.mu.Unlock()
	q, ok := b.queues[name]
	if !ok {
		q = make(chan jobs.Record, 1000)
		b.queues[name] = q
	}
	return q
}

func (b *fakeBroker) Driver() string { return "fake" }

func (b *fakeBroker) Publish(_ context.Context, rec jobs.Record) error {
	b.mu.Lock()
	b.publish = append(b.publish, rec)
	b.mu.Unlock()
	q := b.queue(rec.Queue)
	if d := time.Until(rec.RunAt); d > 0 {
		time.AfterFunc(d, func() { q <- rec })
		return nil
	}
	q <- rec
	return nil
}

func (b *fakeBroker) Bury(_ context.Context, rec jobs.Record) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buried = append(b.buried, rec)
	return nil
}

type fakeDelivery struct {
	b   *fakeBroker
	rec jobs.Record
	raw jobs.Record
}

func (d fakeDelivery) Record() jobs.Record { return d.rec }
func (d fakeDelivery) Ack() error          { d.b.acks.Add(1); return nil }
func (d fakeDelivery) Requeue() error {
	d.b.requeues.Add(1)
	d.b.queue(d.raw.Queue) <- d.raw
	return nil
}

func (b *fakeBroker) Consume(ctx context.Context, queue string, concurrency int, handle func(jobs.Delivery)) error {
	q := b.queue(queue)
	sem := make(chan struct{}, concurrency)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case sem <- struct{}{}:
		}
		select {
		case <-ctx.Done():
			<-sem
			return ctx.Err()
		case raw := <-q:
			rec := raw
			rec.Attempt++
			go func() {
				defer func() { <-sem }()
				handle(fakeDelivery{b: b, rec: rec, raw: raw})
			}()
		}
	}
}

func (b *fakeBroker) Depth(_ context.Context, queue string) (int, error) {
	return len(b.queue(queue)), nil
}

func (b *fakeBroker) buriedJobs() []jobs.Record {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]jobs.Record(nil), b.buried...)
}

type BrokerSvc struct {
	calls   atomic.Int32
	fail    int32
	started chan struct{}
	resumed atomic.Int32
}

type Payload struct{ N int }

func (s *BrokerSvc) Run(ctx context.Context, run *jobs.Run, p Payload) error {
	n := s.calls.Add(1)
	var from int
	if ok, _ := run.Resume(&from); ok {
		s.resumed.Store(int32(from))
	}
	_ = run.Checkpoint(int(n))
	if s.started != nil {
		s.started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}
	if n <= s.fail {
		return errors.New("flaky")
	}
	return run.Progress(1, 1, "done")
}

func (s *BrokerSvc) Broken(ctx context.Context, run *jobs.Run, p Payload) error {
	return jobs.Permanent(errors.New("bad payload"))
}

var (
	brokerRun    = jobs.Define((*BrokerSvc).Run, jobs.Retry(2), jobs.Backoff(func(int) time.Duration { return 20 * time.Millisecond }))
	brokerBroken = jobs.Define((*BrokerSvc).Broken, jobs.Retry(3))
)

func bootBroker(t *testing.T, b *fakeBroker, svc *BrokerSvc, cfg jobs.Config, opts ...nexus.Option) (*jobs.Manager, func()) {
	t.Helper()
	var m *jobs.Manager
	all := append([]nexus.Option{
		nexus.Provide(func() jobs.Broker { return b }),
		jobs.Module(cfg),
		nexus.Supply(svc), brokerRun, brokerBroken,
		nexus.Invoke(func(mm *jobs.Manager) { m = mm }),
	}, opts...)
	_, stop, err := nexus.InProcess(config.Runtime{}, all...)
	if err != nil {
		t.Fatal(err)
	}
	return m, func() { _ = stop(context.Background()) }
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestBrokerRetriesCarryCheckpoints(t *testing.T) {
	b, svc := newFakeBroker(), &BrokerSvc{fail: 2}
	m, stop := bootBroker(t, b, svc, jobs.Config{})
	defer stop()
	if _, err := brokerRun.Enqueue(context.Background(), Payload{N: 1}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "three attempts", func() bool { return b.acks.Load() == 3 })
	if svc.calls.Load() != 3 || svc.resumed.Load() != 2 {
		t.Fatalf("calls %d, resumed from checkpoint %d (want 3, 2)", svc.calls.Load(), svc.resumed.Load())
	}
	b.mu.Lock()
	last := b.publish[len(b.publish)-1]
	b.mu.Unlock()
	if last.Attempt != 2 || last.Error != "flaky" || !last.RunAt.After(last.CreatedAt) {
		t.Fatalf("retry message = %+v", last)
	}
	if _, _, err := m.Get(context.Background(), last.ID); !errors.Is(err, jobs.ErrUnsupported) {
		t.Fatalf("Get with a broker = %v", err)
	}
	if _, err := m.Cancel(context.Background(), last.ID); !errors.Is(err, jobs.ErrUnsupported) {
		t.Fatalf("Cancel with a broker = %v", err)
	}
}

func TestBrokerBuriesPermanentFailures(t *testing.T) {
	b := newFakeBroker()
	_, stop := bootBroker(t, b, &BrokerSvc{}, jobs.Config{})
	defer stop()
	_, _ = brokerBroken.Enqueue(context.Background(), Payload{N: 2})
	eventually(t, "the job to be buried", func() bool { return len(b.buriedJobs()) == 1 })
	if rec := b.buriedJobs()[0]; rec.Error != "bad payload" || rec.State != jobs.StateFailed || rec.Attempt != 1 {
		t.Fatalf("buried = %+v", rec)
	}
}

func TestBrokerShutdownRepublishes(t *testing.T) {
	b, svc := newFakeBroker(), &BrokerSvc{started: make(chan struct{}, 1)}
	_, stop := bootBroker(t, b, svc, jobs.Config{ShutdownGrace: time.Millisecond})
	_, _ = brokerRun.Enqueue(context.Background(), Payload{N: 3})
	<-svc.started
	stop()
	b.mu.Lock()
	defer b.mu.Unlock()
	last := b.publish[len(b.publish)-1]
	if len(b.publish) != 2 || last.Attempt != 0 || len(last.Checkpoint) == 0 {
		t.Fatalf("after shutdown: %d publishes, last %+v", len(b.publish), last)
	}
}

func TestBrokerBootRules(t *testing.T) {
	unique := jobs.Define((*BrokerSvc).Run, jobs.Name("unique-run"), jobs.Unique(time.Minute))
	b := newFakeBroker()
	cases := []struct {
		name string
		opts []nexus.Option
		want string
	}{
		{"unique", []nexus.Option{nexus.Provide(func() jobs.Broker { return b }), jobs.Module(jobs.Config{}), nexus.Supply(&BrokerSvc{}), unique}, "can't honor"},
		{"store and broker", []nexus.Option{nexus.Provide(func() jobs.Broker { return b }), nexus.Provide(func() jobs.Store { return jobs.NewMemoryStore() }),
			jobs.Module(jobs.Config{}), nexus.Supply(&BrokerSvc{}), brokerRun}, "bind one driver"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, stop, err := nexus.InProcess(config.Runtime{}, tc.opts...)
			if stop != nil {
				defer func() { _ = stop(context.Background()) }()
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want a boot error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestBrokerDashboard(t *testing.T) {
	b, svc := newFakeBroker(), &BrokerSvc{}
	var app *nexus.App
	_, stop := bootBroker(t, b, svc, jobs.Config{}, nexus.Invoke(func(a *nexus.App) { app = a }))
	defer stop()
	_, _ = brokerRun.Enqueue(context.Background(), Payload{N: 4})
	eventually(t, "the job to run", func() bool { return b.acks.Load() == 1 })
	for _, r := range app.Registry().Resources() {
		if r.Name == "jobs" {
			if r.Details["driver"] != "fake" || !strings.Contains(r.Details["this process"].(string), "1 done") {
				t.Fatalf("details = %v", r.Details)
			}
			return
		}
	}
	t.Fatal("no jobs resource")
}
