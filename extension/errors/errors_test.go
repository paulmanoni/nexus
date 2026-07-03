package errors

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/paulmanoni/nexus/manifest"
	"github.com/paulmanoni/nexus/trace"
)

// fakeTransport is a recording transport used to assert that
// captured events reach the forwarding layer. Captures every Event
// it receives + tracks call count, behind a mutex for parallel-safe
// reads from test goroutines.
type fakeTransport struct {
	mu     sync.Mutex
	events []Event
	calls  atomic.Int32
}

func (f *fakeTransport) Name() string { return "fake" }
func (f *fakeTransport) Report(_ context.Context, e Event) error {
	f.mu.Lock()
	f.events = append(f.events, e)
	f.mu.Unlock()
	f.calls.Add(1)
	return nil
}
func (f *fakeTransport) snapshot() []Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Event, len(f.events))
	copy(out, f.events)
	return out
}

// TestIsError locks in the classification rules: non-empty Error
// field OR Status >= 500 → captured; 4xx + 2xx + 3xx → ignored.
// This is the gate every captured event passes through.
func TestIsError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		ev   trace.Event
		want bool
	}{
		{trace.Event{Status: 200}, false},
		{trace.Event{Status: 404}, false}, // 4xx is client problem
		{trace.Event{Status: 500}, true},
		{trace.Event{Status: 503}, true},
		{trace.Event{Status: 200, Error: "weird but recorded"}, true}, // explicit error wins
		{trace.Event{}, false},                                        // empty event ignored
	}
	for _, tc := range cases {
		if got := isError(tc.ev); got != tc.want {
			t.Errorf("isError(%+v) = %v, want %v", tc.ev, got, tc.want)
		}
	}
}

// TestFingerprint_GroupsSameSiteSeparatelyFromOthers verifies the
// fingerprint logic groups by Method + Path + topFrame. Two events
// from the same handler get the same fingerprint; a different
// handler at the same path gets a different one.
func TestFingerprint_GroupsSameSiteSeparatelyFromOthers(t *testing.T) {
	t.Parallel()
	a := Event{Method: "GET", Path: "/pets", Stack: "goroutine 1 [running]:\nfoo.Handler(...)\n\t/x.go:10 +0x1"}
	b := Event{Method: "GET", Path: "/pets", Stack: "goroutine 1 [running]:\nfoo.Handler(...)\n\t/x.go:42 +0x9"}
	c := Event{Method: "GET", Path: "/pets", Stack: "goroutine 1 [running]:\nbar.OtherHandler(...)\n\t/x.go:10 +0x1"}

	fa := fingerprint(a)
	fb := fingerprint(b)
	fc := fingerprint(c)

	if fa != fb {
		t.Errorf("same handler different lines should share fingerprint: a=%s b=%s", fa, fb)
	}
	if fa == fc {
		t.Errorf("different handlers should NOT share fingerprint: a=%s c=%s", fa, fc)
	}
}

// TestStore_RingBufferEvicts confirms capacity-bounded behavior:
// after capacity+1 inserts, the oldest event is gone and the recent
// list is exactly len=capacity.
func TestStore_RingBufferEvicts(t *testing.T) {
	t.Parallel()
	s := newStore(3)
	for i := 0; i < 5; i++ {
		s.add(Event{Fingerprint: "f-" + string(rune('a'+i)), Path: "/", CapturedAt: time.Now()})
	}
	got := s.recentSnapshot()
	if len(got) != 3 {
		t.Fatalf("want 3 events after eviction, got %d", len(got))
	}
	// Recent is newest-first; the oldest two (a, b) should be gone.
	for _, e := range got {
		if strings.HasSuffix(e.Fingerprint, "a") || strings.HasSuffix(e.Fingerprint, "b") {
			t.Errorf("evicted event resurfaced: %s", e.Fingerprint)
		}
	}
}

// TestStore_IssueGroupsAndCounts confirms multiple events with the
// same fingerprint become one issue with count == N. Different
// fingerprints stay separate issues.
func TestStore_IssueGroupsAndCounts(t *testing.T) {
	t.Parallel()
	s := newStore(100)
	now := time.Now()
	for i := 0; i < 3; i++ {
		s.add(Event{Fingerprint: "abc", Error: "boom", CapturedAt: now.Add(time.Duration(i) * time.Millisecond)})
	}
	s.add(Event{Fingerprint: "def", Error: "other", CapturedAt: now})

	issues := s.issueSnapshot()
	if len(issues) != 2 {
		t.Fatalf("want 2 issues, got %d", len(issues))
	}
	// Sorted by LastSeen desc — "abc" had the most recent occurrence.
	if issues[0].Fingerprint != "abc" || issues[0].Count != 3 {
		t.Errorf("abc issue: got %+v, want count=3", issues[0])
	}
	if issues[1].Fingerprint != "def" || issues[1].Count != 1 {
		t.Errorf("def issue: got %+v, want count=1", issues[1])
	}
}

// TestIgnoredPath ensures health-probe noise stays out of the store.
// Default IgnorePaths blocks the two framework probes.
func TestIgnoredPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path   string
		ignore []string
		want   bool
	}{
		{"/__nexus/health", []string{"/__nexus/health"}, true},
		{"/api/pets", []string{"/__nexus/health"}, false},
		{"", []string{"/__nexus/health"}, false},
		{"/x", nil, false},
	}
	for _, tc := range cases {
		if got := ignoredPath(tc.path, tc.ignore); got != tc.want {
			t.Errorf("ignoredPath(%q, %v) = %v, want %v", tc.path, tc.ignore, got, tc.want)
		}
	}
}

// TestSample bounds — the simplest path through the sampling
// decision. We can't assert exact distribution with the time-based
// entropy source, but we CAN assert the boundary behavior.
func TestSample_Bounds(t *testing.T) {
	t.Parallel()
	if !shouldSample(1.0) {
		t.Error("rate=1.0 should always keep")
	}
	if shouldSample(0.0) {
		t.Error("rate=0.0 should always drop")
	}
}

// TestValidate covers the small set of hard constraints: SampleRate
// must be in [0, 1], Capacity must be non-negative.
func TestValidate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  Config
		want string // substring of expected error; "" = success
	}{
		{"valid", Config{Capacity: 100, SampleRate: 0.5}, ""},
		{"negative capacity", Config{Capacity: -1}, "Capacity"},
		{"sample > 1", Config{SampleRate: 1.5}, "SampleRate"},
		{"sample < 0", Config{SampleRate: -0.1}, "SampleRate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validate(&tc.cfg)
			if tc.want == "" && err != nil {
				t.Fatalf("want nil, got %v", err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Errorf("want substring %q, got %v", tc.want, err)
			}
		})
	}
}

// TestResolveConfig_ManifestOverridesInCode verifies the precedence
// rule documented on the package: manifest wins where set, in-code
// fills the gaps. Same shape as the TLS/CORS plugins' merge tests.
func TestResolveConfig_ManifestOverridesInCode(t *testing.T) {
	t.Parallel()
	rate := 0.25
	mf := &manifest.Manifest{
		Errors: &manifest.ErrorsBlock{
			Environment: "manifest-env",
			SampleRate:  &rate,
		},
	}
	out, err := resolveConfig(Config{Environment: "in-code", Release: "v1.2.3"}, mf)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if out.Environment != "manifest-env" {
		t.Errorf("Environment: got %q, want manifest-env (manifest wins)", out.Environment)
	}
	if out.Release != "v1.2.3" {
		t.Errorf("Release: got %q, want v1.2.3 (in-code kept)", out.Release)
	}
	if out.SampleRate != 0.25 {
		t.Errorf("SampleRate: got %v, want 0.25", out.SampleRate)
	}
}

// TestResolveConfig_DisabledBypassesValidation lets a manifest opt
// out of capture entirely without making the operator fill in a
// valid Config in-code — same shape as the TLS plugin's disabled
// path.
func TestResolveConfig_DisabledBypassesValidation(t *testing.T) {
	t.Parallel()
	mf := &manifest.Manifest{
		Errors: &manifest.ErrorsBlock{Disabled: true},
	}
	out, err := resolveConfig(Config{SampleRate: -42}, mf) // invalid but ignored
	if err != nil {
		t.Fatalf("disabled should skip validation, got %v", err)
	}
	if !out.Disabled {
		t.Error("Disabled=false; want true")
	}
}

// TestMergeOverrides_ErrorsSampleRate covers the per-env override
// path: production at 100%, preview at 10%. Mirrors the TLS / CORS
// merge tests of the same shape.
func TestMergeOverrides_ErrorsSampleRate(t *testing.T) {
	t.Parallel()
	prodRate := 1.0
	previewRate := 0.1
	base := manifest.Manifest{
		SchemaVersion: "1",
		App:           manifest.AppIdentity{Name: "demo"},
		Name:          "demo",
		Environments: []manifest.Environment{
			{Name: "production"},
			{Name: "preview"},
		},
		Errors: &manifest.ErrorsBlock{
			Environment: "production",
			SampleRate:  &prodRate,
		},
		Overrides: map[string]manifest.Override{
			"preview": {
				Errors: &manifest.ErrorsPatch{
					SampleRate: &previewRate,
				},
			},
		},
	}

	prod, err := manifest.MergeOverrides(base, "production")
	if err != nil {
		t.Fatal(err)
	}
	if prod.Errors.SampleRate == nil || *prod.Errors.SampleRate != 1.0 {
		t.Errorf("prod rate: got %v, want 1.0", prod.Errors.SampleRate)
	}

	preview, err := manifest.MergeOverrides(base, "preview")
	if err != nil {
		t.Fatal(err)
	}
	if preview.Errors.SampleRate == nil || *preview.Errors.SampleRate != 0.1 {
		t.Errorf("preview rate: got %v, want 0.1", preview.Errors.SampleRate)
	}
	// Environment inherits from base (not overridden in preview).
	if preview.Errors.Environment != "production" {
		t.Errorf("preview Environment: got %q, want \"production\" (inherited)", preview.Errors.Environment)
	}
}

// TestCapture_StoresAndForwards is the integration-shaped test:
// drive the consume path with a fabricated trace.Event and assert
// (a) the store grew, (b) the fake transport received the report.
func TestCapture_StoresAndForwards(t *testing.T) {
	t.Parallel()
	ft := &fakeTransport{}
	s := &pluginState{
		cfg: Config{
			Environment: "test",
			Capacity:    50,
			SampleRate:  1.0,
			IgnorePaths: []string{"/__nexus/health"},
			Transports:  []Transport{ft},
		},
		store: newStore(50),
	}

	s.maybeCapture(trace.Event{
		Kind: trace.Kind("request"), Status: 500,
		Error:  "boom",
		Method: "POST", Path: "/api/pets",
		Stack: "goroutine 1 [running]:\nfoo.H(...)\n\t/x.go:1 +0x0",
	})

	// Give the transport goroutine a moment to land.
	deadline := time.Now().Add(500 * time.Millisecond)
	for ft.calls.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	if got := ft.calls.Load(); got != 1 {
		t.Errorf("transport call count: got %d, want 1", got)
	}
	if events := s.store.recentSnapshot(); len(events) != 1 {
		t.Errorf("store size: got %d, want 1", len(events))
	}
	rec := ft.snapshot()
	if len(rec) != 1 {
		t.Fatalf("transport recorded: got %d events, want 1", len(rec))
	}
	if rec[0].Environment != "test" {
		t.Errorf("env tag missing: %+v", rec[0])
	}
	if rec[0].Fingerprint == "" {
		t.Errorf("fingerprint missing")
	}
}

// TestCapture_IgnoredPathSkipped — health-probe noise must not
// reach the store or transports.
func TestCapture_IgnoredPathSkipped(t *testing.T) {
	t.Parallel()
	ft := &fakeTransport{}
	s := &pluginState{
		cfg: Config{
			Capacity:    50,
			SampleRate:  1.0,
			IgnorePaths: []string{"/__nexus/health"},
			Transports:  []Transport{ft},
		},
		store: newStore(50),
	}
	s.maybeCapture(trace.Event{Status: 500, Error: "boom", Path: "/__nexus/health"})
	time.Sleep(50 * time.Millisecond)
	if ft.calls.Load() != 0 {
		t.Errorf("ignored path should not reach transport; got %d calls", ft.calls.Load())
	}
	if len(s.store.recentSnapshot()) != 0 {
		t.Errorf("ignored path should not reach store")
	}
}

// TestSentry_DSNParsing pins the URL composition. A malformed DSN
// surfaces as a parseErr the Sentry transport returns from every
// Report — plugin continues running, just stops forwarding to that
// transport.
func TestSentry_DSNParsing(t *testing.T) {
	t.Parallel()
	cases := []struct {
		dsn        string
		wantParsed bool
		wantEndsIn string
	}{
		{"https://abc123@o123.ingest.sentry.io/4505", true, "/api/4505/store/"},
		{"https://abc123@sentry.example.com/100", true, "/api/100/store/"},
		{"", false, ""},
		{"not a url", false, ""},
		{"https://nokey.example.com/100", false, ""}, // missing user
		{"https://abc@host", false, ""},              // missing project
	}
	for _, tc := range cases {
		t.Run(tc.dsn, func(t *testing.T) {
			s := Sentry(tc.dsn).(*sentryTransport)
			if tc.wantParsed {
				if s.parseErr != nil {
					t.Fatalf("parse err: %v", s.parseErr)
				}
				if !strings.HasSuffix(s.endpoint, tc.wantEndsIn) {
					t.Errorf("endpoint: got %q, want suffix %q", s.endpoint, tc.wantEndsIn)
				}
			} else {
				if s.parseErr == nil && tc.dsn != "" {
					t.Errorf("want parseErr for %q", tc.dsn)
				}
			}
		})
	}
}
