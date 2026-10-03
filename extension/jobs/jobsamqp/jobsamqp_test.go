package jobsamqp_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/paulmanoni/nexus/extension/jobs/jobsamqp/v2"
	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/extension/jobs"
)

var bg = context.Background()

// brokerURL is the RabbitMQ the integration tests use; they skip without it.
func brokerURL(t *testing.T) string {
	t.Helper()
	u := os.Getenv("NEXUS_RABBIT_URL")
	if u == "" {
		t.Skip("set NEXUS_RABBIT_URL (e.g. amqp://guest:guest@localhost:5672/) to run the RabbitMQ tests")
	}
	return u
}

// prefix gives each test its own queues, deleted afterwards.
func prefix(t *testing.T, rawURL string) string {
	t.Helper()
	var b [4]byte
	_, _ = rand.Read(b[:])
	p := "nexustest." + hex.EncodeToString(b[:]) + "."
	t.Cleanup(func() {
		conn, err := amqp.Dial(rawURL)
		if err != nil {
			return
		}
		defer conn.Close()
		ch, err := conn.Channel()
		if err != nil {
			return
		}
		for _, q := range []string{"default", "low"} {
			for _, name := range []string{p + q, p + q + ".failed"} {
				_, _ = ch.QueueDelete(name, false, false, false)
			}
		}
	})
	return p
}

type Svc struct {
	calls    atomic.Int32
	attempts sync.Map // payload N → last attempt
	failOnce atomic.Bool
	when     sync.Map // payload N → time it ran
}

type P struct{ N int }

func (s *Svc) Work(ctx context.Context, run *jobs.Run, p P) error {
	s.calls.Add(1)
	s.attempts.Store(p.N, run.Attempt())
	s.when.Store(p.N, time.Now())
	if p.N < 0 && s.failOnce.CompareAndSwap(false, true) {
		return errors.New("first attempt fails")
	}
	return nil
}

func (s *Svc) Broken(ctx context.Context, run *jobs.Run, p P) error {
	return jobs.Permanent(errors.New("cannot process this payload"))
}

var (
	work   = jobs.Define((*Svc).Work, jobs.Retry(2), jobs.Backoff(func(int) time.Duration { return time.Second }))
	broken = jobs.Define((*Svc).Broken)
)

func boot(t *testing.T, rawURL, p string, svc *Svc) *jobs.Manager {
	t.Helper()
	var m *jobs.Manager
	_, stop, err := nexus.InProcess(config.Runtime{},
		jobsamqp.Bind(jobsamqp.Config{URL: rawURL, Prefix: p}),
		jobs.Module(jobs.Config{Queues: map[string]int{"default": 3}}),
		nexus.Supply(svc), work, broken,
		nexus.Invoke(func(mm *jobs.Manager) { m = mm }),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(bg) })
	return m
}

func eventually(t *testing.T, what string, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Two processes consume one queue; every job runs exactly once.
func TestTwoProcessesShareTheQueue(t *testing.T) {
	u := brokerURL(t)
	p := prefix(t, u)
	svc := &Svc{}
	boot(t, u, p, svc)
	boot(t, u, p, svc)
	for i := 1; i <= 40; i++ {
		if _, err := work.Enqueue(bg, P{N: i}); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "40 jobs", 15*time.Second, func() bool { return svc.calls.Load() >= 40 })
	time.Sleep(300 * time.Millisecond)
	if n := svc.calls.Load(); n != 40 {
		t.Fatalf("%d executions for 40 jobs", n)
	}
}

// A delayed job waits in a TTL queue; a failed attempt retries after its
// backoff, counted as attempt 2.
func TestDelayAndRetry(t *testing.T) {
	u := brokerURL(t)
	p := prefix(t, u)
	svc := &Svc{}
	boot(t, u, p, svc)
	start := time.Now()
	if _, err := work.Enqueue(bg, P{N: 7}, jobs.Delay(1500*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := work.Enqueue(bg, P{N: -1}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the delayed job", 10*time.Second, func() bool { _, ok := svc.when.Load(7); return ok })
	ran, _ := svc.when.Load(7)
	if early := ran.(time.Time).Sub(start); early < 1400*time.Millisecond {
		t.Fatalf("the delayed job ran after %v, before its 1.5s delay", early)
	}
	eventually(t, "the retry", 10*time.Second, func() bool { a, _ := svc.attempts.Load(-1); return a == 2 })
}

// readQueue takes one message off name.
func readQueue(t *testing.T, rawURL, name string) (jobs.Record, bool) {
	t.Helper()
	conn, err := amqp.Dial(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ch, _ := conn.Channel()
	d, ok, err := ch.Get(name, true)
	if err != nil || !ok {
		return jobs.Record{}, false
	}
	var rec jobs.Record
	_ = json.Unmarshal(d.Body, &rec)
	return rec, true
}

func TestFailedJobsAreBuried(t *testing.T) {
	u := brokerURL(t)
	p := prefix(t, u)
	boot(t, u, p, &Svc{})
	id, err := broken.Enqueue(bg, P{N: 3})
	if err != nil {
		t.Fatal(err)
	}
	var rec jobs.Record
	eventually(t, "the buried job", 10*time.Second, func() bool {
		var ok bool
		rec, ok = readQueue(t, u, p+"default.failed")
		return ok
	})
	if rec.ID != id || rec.Error != "cannot process this payload" || rec.State != jobs.StateFailed {
		t.Fatalf("buried = %+v", rec)
	}
}

// A delivery whose consumer dies unacknowledged goes back to the queue.
func TestCrashedConsumerRedelivers(t *testing.T) {
	u := brokerURL(t)
	p := prefix(t, u)
	b, err := jobsamqp.New(jobsamqp.Config{URL: u, Prefix: p})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	now := time.Now()
	if err := b.Publish(bg, jobs.Record{ID: "crash", Name: "Svc.Work", Queue: "default", State: jobs.StateQueued,
		Args: []byte(`{"N":42}`), MaxAttempts: 1, CreatedAt: now, RunAt: now}); err != nil {
		t.Fatal(err)
	}
	conn, err := amqp.Dial(u)
	if err != nil {
		t.Fatal(err)
	}
	ch, _ := conn.Channel()
	if _, ok, err := ch.Get(p+"default", false); !ok || err != nil { // taken, never acked
		t.Fatalf("raw get: %v %v", ok, err)
	}
	_ = conn.Close() // the "crash"

	svc := &Svc{}
	boot(t, u, p, svc)
	eventually(t, "the redelivered job", 10*time.Second, func() bool { _, ok := svc.when.Load(42); return ok })
}

// The work queue is a quorum queue with the consumer timeout long jobs need.
func TestQueueArguments(t *testing.T) {
	u := brokerURL(t)
	p := prefix(t, u)
	boot(t, u, p, &Svc{})
	if _, err := work.Enqueue(bg, P{N: 1}); err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(u)
	pass, _ := parsed.User.Password()
	req, _ := http.NewRequest("GET", "http://"+parsed.Hostname()+":15672/api/queues/%2F/"+url.PathEscape(p+"default"), nil)
	req.SetBasicAuth(parsed.User.Username(), pass)
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 200 {
		t.Skipf("management API unavailable: %v", err)
	}
	defer res.Body.Close()
	var q struct {
		Type      string         `json:"type"`
		Arguments map[string]any `json:"arguments"`
	}
	_ = json.NewDecoder(res.Body).Decode(&q)
	if q.Type != "quorum" || q.Arguments["x-consumer-timeout"] != float64(8*time.Hour/time.Millisecond) ||
		!strings.HasSuffix(q.Arguments["x-dead-letter-routing-key"].(string), ".failed") {
		t.Fatalf("queue = %+v", q)
	}
}

func TestNewRejectsABadURL(t *testing.T) {
	if _, err := jobsamqp.New(jobsamqp.Config{URL: "http://not-amqp"}); err == nil {
		t.Fatal("want an error for a non-AMQP url")
	}
}

type LongSvc struct {
	started  chan int
	resumed  atomic.Int32
	finished atomic.Int32
}

func (s *LongSvc) Long(ctx context.Context, run *jobs.Run, p P) error {
	var step int
	if ok, _ := run.Resume(&step); ok {
		s.resumed.Store(int32(step))
	}
	_ = run.Checkpoint(5)
	s.started <- run.Attempt()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(300 * time.Millisecond):
	}
	s.finished.Add(1)
	return nil
}

var long = jobs.Define((*LongSvc).Long)

// A job interrupted by a shutdown is handed on — attempt uncounted, its
// checkpoint with it — and finishes in the next process, once.
func TestShutdownHandsTheJobOn(t *testing.T) {
	u := brokerURL(t)
	p := prefix(t, u)
	svc := &LongSvc{started: make(chan int, 4)}
	bootLong := func() func() {
		_, stop, err := nexus.InProcess(config.Runtime{},
			jobsamqp.Bind(jobsamqp.Config{URL: u, Prefix: p}),
			jobs.Module(jobs.Config{ShutdownGrace: time.Millisecond}),
			nexus.Supply(svc), long,
		)
		if err != nil {
			t.Fatal(err)
		}
		return func() { _ = stop(bg) }
	}
	stopFirst := bootLong()
	if _, err := long.Enqueue(bg, P{N: 1}); err != nil {
		t.Fatal(err)
	}
	if a := <-svc.started; a != 1 {
		t.Fatalf("first run is attempt %d", a)
	}
	stopFirst()
	stopSecond := bootLong()
	defer stopSecond()
	select {
	case a := <-svc.started:
		if a != 1 {
			t.Fatalf("handed-on run is attempt %d, want 1 (the interrupted one uncounted)", a)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the interrupted job was not handed on")
	}
	eventually(t, "the job to finish", 10*time.Second, func() bool { return svc.finished.Load() == 1 })
	if svc.resumed.Load() != 5 {
		t.Fatalf("resumed from %d, want the checkpoint 5", svc.resumed.Load())
	}
	time.Sleep(500 * time.Millisecond)
	if svc.finished.Load() != 1 {
		t.Fatalf("finished %d times", svc.finished.Load())
	}
}
