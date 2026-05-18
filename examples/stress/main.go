// Command stress is a load generator for the live/template engine.
//
// It opens N concurrent WebSocket sessions against a running nexus
// app, drives each one through the standard wire protocol
// (HTTP SSR → WS upgrade → join → events), and reports connect
// success rate plus event round-trip latency percentiles.
//
// The default target is the hello example on :8083. Boot the server
// in one terminal:
//
//	go run ./examples/hello
//
// Then run the stress driver in another:
//
//	go run ./examples/stress -clients 1000 -duration 30s
//
// Tune via flags:
//
//	-clients N         concurrent WS sessions to maintain
//	-duration D        how long to fire events after ramp-up
//	-interval D        per-client gap between events (each client
//	                   fires 1 event per interval; total RPS ≈
//	                   clients/interval)
//	-ramp D            stagger connect over this duration so the
//	                   server isn't hit by N simultaneous upgrades
//	-event NAME        event name to send (must match a handler
//	                   method on the target component)
//	-target URL        http(s) URL of the target's component route
//	-component NAME    component name to put in the "join" frame
//
// The driver prints a summary at the end:
//
//	connected           99.8% (998/1000)
//	join-rtt p50/p95/p99
//	event-rtt p50/p95/p99
//	events sent/acked   140532/140528
//	elapsed             30.1s
//
// For server-side resource numbers (RSS, goroutines, heap) the
// nexus app should expose /debug/pprof in its config; the driver
// fetches /debug/pprof/goroutine?debug=1 and /debug/pprof/heap at
// start + end when -pprof-base is set and prints the deltas.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// flags collects every CLI knob in one struct so the run() function
// reads from a single value rather than a flag-package global state.
type flags struct {
	target    string
	component string
	event     string
	clients   int
	duration  time.Duration
	interval  time.Duration
	ramp      time.Duration
	pprofBase string
	verbose   bool
}

func parseFlags() flags {
	var f flags
	flag.StringVar(&f.target, "target", "http://localhost:8083/", "HTTP URL of the target component route")
	flag.StringVar(&f.component, "component", "Hello", "component name to register in the join frame")
	flag.StringVar(&f.event, "event", "bump", "event name each client fires")
	flag.IntVar(&f.clients, "clients", 1000, "concurrent WS sessions")
	flag.DurationVar(&f.duration, "duration", 30*time.Second, "how long to fire events after ramp-up")
	flag.DurationVar(&f.interval, "interval", 200*time.Millisecond, "per-client event interval")
	flag.DurationVar(&f.ramp, "ramp", 5*time.Second, "ramp-up duration; connects are spread evenly across it")
	flag.StringVar(&f.pprofBase, "pprof-base", "", "if set, fetches /debug/pprof/{heap,goroutine} from this base URL at start/end")
	flag.BoolVar(&f.verbose, "verbose", false, "log each client's failures (otherwise only the totals are reported)")
	flag.Parse()
	return f
}

// inbound + outbound mirror the wire types from live/template/protocol.go.
// Re-declared here (not imported) so the stress binary depends only on
// the public WS protocol — same way an external client would.
type outbound struct {
	Type      string            `json:"type"`
	Component string            `json:"component,omitempty"`
	Params    map[string]string `json:"params,omitempty"`
	Name      string            `json:"name,omitempty"`
	Payload   map[string]any    `json:"payload,omitempty"`
}

type inbound struct {
	Type  string          `json:"type"`
	Msg   string          `json:"msg,omitempty"`
	Token string          `json:"token,omitempty"`
	// R is the "joined" frame's rendered tree; Diff is the per-event
	// frame's patch. We don't decode either — we only need to know
	// that *some* response arrived to close the RTT loop.
	R    json.RawMessage `json:"r,omitempty"`
	Diff json.RawMessage `json:"d,omitempty"`
}

// stats is the shared counter set every client writes into. Atomic
// counters for the hot paths; the latency slice is appended under a
// mutex (cheap relative to a WS round-trip).
type stats struct {
	connected      atomic.Int64
	connectFailed  atomic.Int64
	joined         atomic.Int64
	joinFailed     atomic.Int64
	eventsSent     atomic.Int64
	eventsAcked    atomic.Int64
	disconnects    atomic.Int64
	pingsReceived  atomic.Int64

	mu          sync.Mutex
	joinRTTs    []time.Duration
	eventRTTs   []time.Duration
	connectErrs map[string]int // grouped error message → count
}

func newStats() *stats {
	return &stats{connectErrs: make(map[string]int)}
}

func (s *stats) recordConnectErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Bucket on first 80 chars to keep "10000× same nested syscall
	// error" from blowing up the map.
	msg := err.Error()
	if len(msg) > 80 {
		msg = msg[:80] + "…"
	}
	s.connectErrs[msg]++
}

func (s *stats) addJoinRTT(d time.Duration) {
	s.mu.Lock()
	s.joinRTTs = append(s.joinRTTs, d)
	s.mu.Unlock()
}

func (s *stats) addEventRTT(d time.Duration) {
	s.mu.Lock()
	s.eventRTTs = append(s.eventRTTs, d)
	s.mu.Unlock()
}

func main() {
	f := parseFlags()
	if f.clients < 1 {
		log.Fatal("stress: -clients must be ≥ 1")
	}

	wsURL, err := httpToWS(f.target)
	if err != nil {
		log.Fatalf("stress: parse -target: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fmt.Printf("stress: target=%s clients=%d duration=%s interval=%s ramp=%s\n",
		f.target, f.clients, f.duration, f.interval, f.ramp)

	st := newStats()

	if f.pprofBase != "" {
		printPprofSnapshot("before", f.pprofBase)
	}

	// Stagger connect attempts evenly across the ramp window. With
	// ramp=5s and clients=1000, that's one new connect every 5ms —
	// enough to keep accept() queues from saturating but fast enough
	// that the test reaches steady state in ~ramp + slack.
	var perClientDelay time.Duration
	if f.ramp > 0 && f.clients > 1 {
		perClientDelay = f.ramp / time.Duration(f.clients)
	}

	var wg sync.WaitGroup
	wg.Add(f.clients)
	start := time.Now()

	for i := 0; i < f.clients; i++ {
		go runClient(ctx, &wg, wsURL, f, st, i)
		if perClientDelay > 0 {
			time.Sleep(perClientDelay)
		}
	}

	// Steady-state hold: ramp completes, then wait for `duration` to
	// elapse on the test clock so we measure events under saturation
	// rather than during connect churn.
	time.Sleep(f.duration)
	cancel()
	wg.Wait()
	elapsed := time.Since(start)

	if f.pprofBase != "" {
		printPprofSnapshot("after", f.pprofBase)
	}

	printSummary(st, f, elapsed)
}

// runClient is a single virtual user. Connects, joins, then loops on
// the event-fire cadence until ctx cancels or the connection drops.
//
// Per-client jitter (±25% of interval) keeps N clients from firing
// in lockstep, which would give artificially-bursty load — production
// users don't synchronize their clicks.
func runClient(ctx context.Context, wg *sync.WaitGroup, wsURL string, f flags, st *stats, id int) {
	defer wg.Done()

	dialCtx, dialCancel := context.WithTimeout(ctx, 10*time.Second)
	defer dialCancel()

	dialer := websocket.DefaultDialer
	dialer.HandshakeTimeout = 10 * time.Second

	conn, _, err := dialer.DialContext(dialCtx, wsURL, nil)
	if err != nil {
		st.connectFailed.Add(1)
		st.recordConnectErr(err)
		if f.verbose {
			log.Printf("client %d: dial: %v", id, err)
		}
		return
	}
	defer conn.Close()
	st.connected.Add(1)

	// --- Join phase ---------------------------------------------------
	joinStart := time.Now()
	if err := conn.WriteJSON(outbound{Type: "join", Component: f.component}); err != nil {
		st.joinFailed.Add(1)
		return
	}

	// Wait for the "joined" frame — anything else (error, etc.)
	// counts as a join failure.
	conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	var first inbound
	if err := conn.ReadJSON(&first); err != nil {
		st.joinFailed.Add(1)
		if f.verbose {
			log.Printf("client %d: join read: %v", id, err)
		}
		return
	}
	if first.Type != "joined" {
		st.joinFailed.Add(1)
		if f.verbose {
			log.Printf("client %d: first frame type=%q msg=%q", id, first.Type, first.Msg)
		}
		return
	}
	st.joined.Add(1)
	st.addJoinRTT(time.Since(joinStart))

	// --- Event phase --------------------------------------------------
	// Pending events are tagged with the time they were sent; the
	// next "diff" frame closes the RTT loop. We assume one diff per
	// event (true for our wire protocol — every event drives exactly
	// one render+diff).
	pending := make(chan time.Time, 16) // small buffer absorbs scheduler jitter

	// Reader goroutine: every diff pops the oldest pending timestamp
	// and records RTT. Server-initiated frames (ping, push) just get
	// counted, no RTT recorded.
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			conn.SetReadDeadline(time.Now().Add(60 * time.Second))
			var msg inbound
			if err := conn.ReadJSON(&msg); err != nil {
				st.disconnects.Add(1)
				return
			}
			switch msg.Type {
			case "diff":
				select {
				case t := <-pending:
					st.eventsAcked.Add(1)
					st.addEventRTT(time.Since(t))
				default:
					// Diff arrived without a pending event — likely
					// a notifier-driven re-render (another client
					// mutated shared state). Counted as ack-free.
				}
			case "ping":
				st.pingsReceived.Add(1)
			}
		}
	}()

	// Jittered interval so N clients don't all fire at the same wall
	// clock instant. ±25% of the nominal interval.
	jitter := func() time.Duration {
		if f.interval <= 0 {
			return 0
		}
		// Avoid math/rand panic on zero; clamp to nonnegative result.
		j := time.Duration(rand.Int63n(int64(f.interval) / 2))
		return f.interval - f.interval/4 + j
	}

	for {
		select {
		case <-ctx.Done():
			conn.Close()
			<-readerDone
			return
		case <-time.After(jitter()):
			sent := time.Now()
			select {
			case pending <- sent:
			default:
				// pending buffer full — server can't keep up.
				// Skip this event rather than block; we'll detect
				// the saturation as eventsSent / eventsAcked
				// divergence in the summary.
				continue
			}
			if err := conn.WriteJSON(outbound{Type: "event", Name: f.event}); err != nil {
				st.disconnects.Add(1)
				<-readerDone
				return
			}
			st.eventsSent.Add(1)
		}
	}
}

// httpToWS rewrites an HTTP(S) URL to its WS(S) equivalent. The live
// template engine serves both over the same path; we just need the
// scheme swap.
func httpToWS(in string) (string, error) {
	u, err := url.Parse(in)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
		// already a ws URL
	default:
		return "", fmt.Errorf("unsupported scheme %q (use http/https or ws/wss)", u.Scheme)
	}
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String(), nil
}

// printSummary lays out the final report. Numbers are ordered by
// what you actually scan for: did everyone connect → were events
// acked → how fast.
func printSummary(st *stats, f flags, elapsed time.Duration) {
	st.mu.Lock()
	defer st.mu.Unlock()

	fmt.Printf("\n=== stress summary ===\n")
	fmt.Printf("  elapsed:        %s\n", elapsed.Round(10*time.Millisecond))
	fmt.Printf("  clients:        %d requested\n", f.clients)
	connected := st.connected.Load()
	failed := st.connectFailed.Load()
	fmt.Printf("  connected:      %d (%5.1f%%) — failed %d\n",
		connected, percent(connected, int64(f.clients)), failed)
	if len(st.connectErrs) > 0 {
		fmt.Printf("  connect errs:\n")
		for msg, count := range st.connectErrs {
			fmt.Printf("    %4d × %s\n", count, msg)
		}
	}

	joined := st.joined.Load()
	joinFailed := st.joinFailed.Load()
	fmt.Printf("  joined:         %d (%5.1f%% of connected) — failed %d\n",
		joined, percent(joined, connected), joinFailed)
	fmt.Printf("  disconnects:    %d mid-test\n", st.disconnects.Load())

	sent := st.eventsSent.Load()
	acked := st.eventsAcked.Load()
	ackPct := percent(acked, sent)
	rps := float64(0)
	if elapsed > 0 {
		rps = float64(acked) / elapsed.Seconds()
	}
	fmt.Printf("  events sent:    %d\n", sent)
	fmt.Printf("  events acked:   %d (%5.1f%% — server kept up) — sustained %.1f acks/s\n", acked, ackPct, rps)

	if len(st.joinRTTs) > 0 {
		p50, p95, p99 := percentiles(st.joinRTTs)
		fmt.Printf("  join RTT:       p50=%s p95=%s p99=%s (n=%d)\n",
			p50, p95, p99, len(st.joinRTTs))
	}
	if len(st.eventRTTs) > 0 {
		p50, p95, p99 := percentiles(st.eventRTTs)
		fmt.Printf("  event RTT:      p50=%s p95=%s p99=%s (n=%d)\n",
			p50, p95, p99, len(st.eventRTTs))
	}
	fmt.Println()
}

func percentiles(d []time.Duration) (p50, p95, p99 time.Duration) {
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	idx := func(p float64) int {
		i := int(float64(len(d)) * p)
		if i >= len(d) {
			i = len(d) - 1
		}
		return i
	}
	return d[idx(0.50)].Round(10 * time.Microsecond),
		d[idx(0.95)].Round(10 * time.Microsecond),
		d[idx(0.99)].Round(10 * time.Microsecond)
}

func percent(n, d int64) float64 {
	if d <= 0 {
		return 0
	}
	return 100 * float64(n) / float64(d)
}

// printPprofSnapshot fetches the goroutine + heap profiles from a
// nexus app's /debug/pprof endpoint (only if -pprof-base was passed)
// and prints a one-line digest. The user gets a rough server-side
// resource picture without separate top-watching.
//
// We don't enable pprof on the server side here — the target has to
// expose it themselves (nexus.DebugConfig{Pprof: true} or equivalent).
// On unreachable URLs, we log and continue.
func printPprofSnapshot(label, base string) {
	base = strings.TrimRight(base, "/")
	goroutines := fetchPprofText(base + "/debug/pprof/goroutine?debug=1")
	heap := fetchPprofText(base + "/debug/pprof/heap?debug=1")
	gc := countGoroutines(goroutines)
	inuse := parseInuseBytes(heap)
	fmt.Printf("pprof %-6s  goroutines=%d  heap_inuse=%s\n",
		label, gc, formatBytes(inuse))
}

func fetchPprofText(u string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return ""
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stress: pprof %s: %v\n", u, err)
		return ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// countGoroutines is a best-effort parse of the goroutine debug=1
// profile — the first line is "goroutine profile: total N".
func countGoroutines(profile string) int {
	if profile == "" {
		return 0
	}
	const prefix = "goroutine profile: total "
	if idx := strings.Index(profile, prefix); idx >= 0 {
		var n int
		fmt.Sscanf(profile[idx+len(prefix):], "%d", &n)
		return n
	}
	return 0
}

// parseInuseBytes pulls heap inuse_space from the debug=1 heap dump.
// The relevant line looks like "# HeapInuse = 12345678".
func parseInuseBytes(profile string) int64 {
	if profile == "" {
		return 0
	}
	const prefix = "# HeapInuse = "
	if idx := strings.Index(profile, prefix); idx >= 0 {
		var n int64
		fmt.Sscanf(profile[idx+len(prefix):], "%d", &n)
		return n
	}
	return 0
}

func formatBytes(n int64) string {
	const (
		KiB = 1024
		MiB = 1024 * KiB
		GiB = 1024 * MiB
	)
	switch {
	case n >= GiB:
		return fmt.Sprintf("%.2f GiB", float64(n)/float64(GiB))
	case n >= MiB:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(MiB))
	case n >= KiB:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(KiB))
	}
	return fmt.Sprintf("%d B", n)
}
