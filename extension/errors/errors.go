// Package errors captures unhandled panics and 5xx-shaped failures
// from a nexus app, deduplicates them by fingerprint, surfaces the
// rolling history on the dashboard, and forwards each new occurrence
// to one or more configured transports (Sentry, generic webhook,
// stdout).
//
// The framework's panic-recovery middleware + trace.Bus already
// captures error events with stacks attached. The errors plugin
// subscribes to that bus, classifies events as errors / not-errors,
// groups them by fingerprint into "issues", and publishes each new
// occurrence outward.
//
//	import "github.com/paulmanoni/nexus/extension/errors"
//
//	nexus.Run(
//	    nexus.Config{Server: nexus.ServerConfig{Addr: ":8080"}},
//
//	    errors.Plugin(errors.Config{
//	        Environment: "production",
//	        Release:     gitSHA,
//	        Capacity:    200,                                // ring buffer size
//	        Transports: []errors.Transport{
//	            errors.Sentry(os.Getenv("SENTRY_DSN")),
//	            errors.Webhook("https://errors.internal/ingest"),
//	        },
//	    }),
//
//	    // ... rest of the app
//	)
//
// Manifest integration: the `errors:` block in nexus.toml
// drives Environment, Release, Capacity, SampleRate, and
// IgnorePaths per-environment, with environment_overrides letting
// production / staging / preview each set its own sample rate or
// switch to a different ingestion URL. Transports themselves stay
// in code (they carry Go-only types like http.Client).
package errors

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension"
	"github.com/paulmanoni/nexus/trace"
)

// Config controls the plugin's capture + reporting policy. Sensible
// defaults mean a bare Config{Transports: [...]} works in production;
// every field is independently tunable per environment via manifest.
type Config struct {
	// Environment tags every reported event ("production", "staging",
	// "preview"). Sentry's UI groups + filters by this; webhook
	// receivers see it in the payload. Leave empty in tests.
	Environment string

	// Release tags every event with a version / commit hash so the
	// receiver can group "errors introduced in v1.4.2" without
	// guessing. Typical wiring is a Go ldflags-injected variable or
	// the GIT_SHA env var.
	Release string

	// ServerName tags every event with the host that captured it.
	// Useful when split deployments have multiple instances reporting
	// the same fingerprint — the receiver sees which instance fired.
	// Defaults to os.Hostname().
	ServerName string

	// Capacity is the in-memory ring-buffer size for the dashboard
	// view. Old events evict when full. Defaults to 100.
	Capacity int

	// SampleRate is the fraction of captured events forwarded to
	// transports. 1.0 = every error, 0.1 = 10%, 0.0 = none (still
	// captured for the dashboard). Useful in preview environments
	// where noisy errors aren't worth a Sentry quota burn.
	// Defaults to 1.0.
	SampleRate float64

	// IgnorePaths is a list of request paths whose errors are NOT
	// captured. Useful for health checks and admin surfaces that
	// would otherwise fill the ring buffer with noise.
	// Default: ["/__nexus/health", "/__nexus/ready"].
	IgnorePaths []string

	// Transports is the slice of receivers each captured error is
	// forwarded to. Empty = dashboard-only (no external reporting).
	// Transports run sequentially per event; one transport failing
	// doesn't block the others.
	Transports []Transport

	// Disabled, when true, makes the plugin a no-op. Useful in
	// environments where another error reporter is wired (or you
	// just don't want external reporting from preview).
	Disabled bool
}

// Plugin attaches the error capturer + reporter to the app's
// lifecycle. The plugin subscribes to the app's trace bus, filters
// for error-shaped events, groups them into issues, and forwards new
// occurrences to the configured transports.
func Plugin(cfg Config) nexus.Option {
	state := &pluginState{inCodeCfg: cfg}

	return extension.Use(extension.Plugin{
		Name:    "errors",
		Version: "0.1.0",

		Lifecycle: &extension.Lifecycle{
			// OnBoot resolves the effective config from manifest +
			// in-code, validates, and stores the resolved config
			// on state. No subscription yet — the trace bus is
			// already running, but starting the subscriber here
			// means we'd miss boot-phase errors (which the
			// framework's own startup hooks already report through
			// the lifecycle path).
			OnBoot: state.boot,

			// OnReady starts the subscriber AFTER listeners bind.
			// Order matters: we want to be receiving when the
			// first request lands. Subscribe-late risks missing
			// the very first error if the app crashes at boot.
			OnReady: state.start,

			OnShutdown: state.stop,
		},

		Dashboard: &extension.Dashboard{
			Tab: &extension.Tab{
				ID:    "errors",
				Label: "Errors",
				Icon:  "alert-triangle",
			},
			Routes: []extension.Route{
				{Method: "GET", Path: "/recent", Handler: state.handleRecent},
				{Method: "GET", Path: "/issues", Handler: state.handleIssues},
				{Method: "GET", Path: "/issue/:fingerprint", Handler: state.handleIssue},
				{Method: "GET", Path: "/status", Handler: state.handleStatus},
				{Method: "POST", Path: "/clear", Handler: state.handleClear},
			},
		},
	})
}

// pluginState carries the long-lived state: resolved config, the
// store (ring buffer + issue grouping), the trace.Bus subscription,
// and the cancel function for the subscriber goroutine.
type pluginState struct {
	inCodeCfg Config

	mu        sync.RWMutex
	cfg       Config
	store     *store
	app       *nexus.App
	cancelSub func()
	started   bool
}

// boot runs at OnBoot — resolves the manifest+in-code merge,
// validates, and prepares the store. Subscription starts at OnReady
// (see start).
func (s *pluginState) boot(ctx context.Context, app *nexus.App) error {
	cfg, err := resolveConfig(s.inCodeCfg, readManifest(app))
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.cfg = cfg
	s.app = app
	s.store = newStore(cfg.Capacity)
	s.mu.Unlock()
	return nil
}

// start runs at OnReady — opens a subscription on the app's trace
// bus and spawns the consumer goroutine. The goroutine outlives
// individual requests and only stops on OnShutdown.
func (s *pluginState) start(ctx context.Context, app *nexus.App) error {
	s.mu.Lock()
	if s.cfg.Disabled || s.started {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	bus := app.Bus()
	if bus == nil {
		// No bus on this app — running with tracing disabled.
		// Plugin becomes dashboard-only; manual captures still work.
		return nil
	}

	// Subscribe with a generous buffer — error events are rare
	// compared to span events, but a burst of bad requests can
	// spike. 256 events of headroom covers typical burst patterns
	// without leaking memory.
	_, ch, cancel := bus.Subscribe(0, 256)

	subCtx, subCancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancelSub = func() {
		cancel()
		subCancel()
	}
	s.started = true
	s.mu.Unlock()

	go s.consume(subCtx, ch)
	return nil
}

// stop drains the subscriber on OnShutdown. The bus's cancel func
// closes the channel, which terminates the consume goroutine. We
// don't wait for the goroutine — its only side effect after the
// channel closes is a return, and the OnShutdown deadline shouldn't
// pad on a goroutine that's already exiting.
func (s *pluginState) stop(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		return nil
	}
	if s.cancelSub != nil {
		s.cancelSub()
	}
	s.started = false
	return nil
}

// consume is the goroutine body — receives trace events, filters
// for error-shaped ones, and routes them through the capture path.
// Exits when the channel closes (bus.Subscribe's cancel) or the
// context cancels.
func (s *pluginState) consume(ctx context.Context, ch <-chan trace.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			s.maybeCapture(ev)
		}
	}
}

// maybeCapture inspects a trace event and, if it represents an
// error, records it into the store + forwards it to transports.
// Filters cheaply (no work for non-error events).
func (s *pluginState) maybeCapture(ev trace.Event) {
	if !isError(ev) {
		return
	}
	s.mu.RLock()
	cfg := s.cfg
	store := s.store
	s.mu.RUnlock()

	if ignoredPath(ev.Path, cfg.IgnorePaths) {
		return
	}

	captured := newEventFromTrace(ev, cfg)
	store.add(captured)

	// Sample for transports. Dashboard captures every error
	// regardless — operators looking for "what broke 5 minutes
	// ago" want the complete picture, not a sampled one.
	if !shouldSample(cfg.SampleRate) {
		return
	}
	for _, t := range cfg.Transports {
		// Best-effort, never block — wrap in a goroutine so a
		// slow transport (Sentry under load, webhook with high
		// latency) doesn't stall the subscriber.
		go func(t Transport) {
			tctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = t.Report(tctx, captured)
		}(t)
	}
}

// isError classifies a trace event as worth capturing. Two signals:
//
//  1. Non-empty Error field — the framework's middleware sets this
//     on request-finish events that returned a non-2xx with an
//     error attached.
//  2. Status >= 500 — explicit server errors even without an
//     attached error value (rare but possible: a handler that
//     manually writes a 500 with no error.)
//
// 4xx are NOT captured — those are client problems, not server
// problems, and would drown the signal in noise.
func isError(ev trace.Event) bool {
	if ev.Error != "" {
		return true
	}
	if ev.Status >= 500 {
		return true
	}
	return false
}

// ignoredPath reports whether a request path matches the configured
// ignore list. Exact match only — operators wanting prefix matches
// should use AllowOriginFunc-equivalents (a future SkipFn config
// field). Most apps just ignore the two health probes.
func ignoredPath(path string, ignore []string) bool {
	if path == "" || len(ignore) == 0 {
		return false
	}
	for _, p := range ignore {
		if p == path {
			return true
		}
	}
	return false
}

// applyDefaults fills convenience defaults.
func applyDefaults(cfg *Config) {
	if cfg.Capacity == 0 {
		cfg.Capacity = 100
	}
	if cfg.SampleRate == 0 {
		cfg.SampleRate = 1.0
	}
	if cfg.IgnorePaths == nil {
		cfg.IgnorePaths = []string{"/__nexus/health", "/__nexus/ready"}
	}
	if cfg.ServerName == "" {
		cfg.ServerName = resolveHostname()
	}
}

// validate enforces the small set of hard constraints.
func validate(cfg *Config) error {
	if cfg.Capacity < 0 {
		return fmt.Errorf("errors: Capacity must be >= 0, got %d", cfg.Capacity)
	}
	if cfg.SampleRate < 0 || cfg.SampleRate > 1 {
		return fmt.Errorf("errors: SampleRate must be in [0.0, 1.0], got %v", cfg.SampleRate)
	}
	return nil
}

// resolveHostname returns the machine's hostname, or "" if Hostname()
// errors. Used as the default ServerName tag.
func resolveHostname() string {
	h, err := osHostname()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(h)
}

// hostnameFunc is overrideable in tests via osHostname so we can
// drive a deterministic ServerName without setting an env var or
// mocking os.Hostname.
var osHostname = func() (string, error) {
	return getOSHostname()
}
