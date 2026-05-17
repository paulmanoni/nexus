// Command crons is a runnable demo of the live/template engine
// re-implementing the /__nexus/crons dashboard view. Same data
// source the existing Vue dashboard reads (cron.Scheduler.
// Snapshots()), same live-update story (the scheduler's
// OnChange hook is already wired into nexus.Notifier at app
// boot, so a job tick wakes every connected tab automatically).
//
// Run:
//
//	go run ./examples/crons
//
// Then open http://localhost:8081. Two demo jobs are registered
// — "heartbeat" runs every 5 seconds, "cleanup" every 30 — and
// the page updates as they fire. Trigger / Pause / Resume
// buttons exercise the mutator API.
//
// This is the cron-tab milestone for the dashboard rewrite: one
// of the five "easy" views the previous audit identified as a
// direct map onto live-template. Pattern transfers to the
// auth / rate-limits / stats / plugins / middlewares tabs
// without modification — list + filter + per-row mutate.
package main

import (
	"context"
	"embed"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension/cron"
	"github.com/paulmanoni/nexus/live/template"
)

//go:embed templates/*.nlt
var assets embed.FS

// CronsPage is the live component for /. Refresh pulls the
// scheduler's current snapshots; Filter scopes the displayed
// list; each event handler delegates to the scheduler's
// public API (Trigger / SetPaused). The scheduler's OnChange
// hook is wired into the app's notifier at boot, so a job
// completion wakes this session within ~50ms and the next
// Refresh sees the updated history.
type CronsPage struct {
	template.BaseComponent
	sched *cron.Scheduler

	Crons  []cron.Snapshot
	Filter string
}

func (c *CronsPage) Mount(_ *template.Ctx) error { return nil }

// Refresh repopulates Crons from the scheduler on every render.
// The session's notifier subscription fires Refresh on every
// upstream change, so the view stays in sync without polling
// — same pattern as examples/live's PostsList.
func (c *CronsPage) Refresh(_ *template.Ctx) error {
	all := c.sched.Snapshots()
	if c.Filter == "" {
		c.Crons = all
		return nil
	}
	needle := strings.ToLower(c.Filter)
	out := all[:0]
	for _, s := range all {
		if strings.Contains(strings.ToLower(s.Name), needle) {
			out = append(out, s)
		}
	}
	c.Crons = out
	return nil
}

// Trigger fires a job immediately. The scheduler runs it
// out-of-band from the regular schedule; the next OnChange
// hook fires when it completes, and the page re-renders with
// the new RunRecord visible in history.
func (c *CronsPage) Trigger(_ *template.Ctx, name string) {
	c.sched.Trigger(name)
}

func (c *CronsPage) Pause(_ *template.Ctx, name string) {
	c.sched.SetPaused(name, true)
}

func (c *CronsPage) Resume(_ *template.Ctx, name string) {
	c.sched.SetPaused(name, false)
}

func (c *CronsPage) ClearFilter(_ *template.Ctx) { c.Filter = "" }

// --- view helpers ---------------------------------------------

// Status collapses Paused / Running / idle into one
// renderable token the template uses for both the label and
// the CSS class.
func (c *CronsPage) Status(s cron.Snapshot) string {
	switch {
	case s.Running:
		return "running"
	case s.Paused:
		return "paused"
	default:
		return "idle"
	}
}

// Until renders a "in 5s" style countdown for the next run.
// Returns "—" for nil (job never scheduled or already past).
// We render only at second resolution — sub-second jitter is
// noise at the dashboard level.
func (c *CronsPage) Until(t *time.Time) string {
	if t == nil {
		return "—"
	}
	d := time.Until(*t).Round(time.Second)
	if d <= 0 {
		return "now"
	}
	return "in " + d.String()
}

// Ago renders "5s ago" for the last-run timestamp; nil is "—".
// We accept *RunRecord and pull Started internally so the
// template can write {{ Ago(c.LastRun) }} without an explicit
// nil-check guard.
func (c *CronsPage) Ago(r *cron.RunRecord) string {
	if r == nil {
		return "—"
	}
	d := time.Since(r.Started).Round(time.Second)
	if d < time.Second {
		return "just now"
	}
	return d.String() + " ago"
}

// LastResult returns "" / "ok" / "fail" so the template can
// stamp a CSS class on the last-run cell without nl-if chains.
func (c *CronsPage) LastResult(r *cron.RunRecord) string {
	if r == nil {
		return ""
	}
	if r.Success {
		return "ok"
	}
	return "fail"
}

// --- wiring ---------------------------------------------------

// liveModule is the full app wiring: one Provide bridging
// the (non-fx) cron Scheduler into the fx graph, one Module
// for the template engine, one AsComponent for the page, and
// one Invoke registering demo jobs at boot. Total: 25 lines.
var liveModule = nexus.Module("crons",
	// The scheduler is built eagerly by app boot and exposed
	// via app.Scheduler() — not natively fx-provided. This
	// adapter puts it in the graph so AsComponent constructors
	// can take it as a typed dependency.
	nexus.Provide(func(app *nexus.App) *cron.Scheduler {
		return app.Scheduler()
	}),

	template.Module(
		template.WithFS(assets),
		template.WithIdleTimeout(30*time.Minute),
		template.WithSessionResumption(30*time.Second),
	),

	nexus.AsComponent("Crons",
		func(sched *cron.Scheduler) (*CronsPage, error) {
			return &CronsPage{sched: sched}, nil
		},
		template.WithTemplate("templates/Crons"),
		nexus.Path("/"),
	),

	// Register demo jobs at boot so the page has something
	// to show. In a real dashboard the scheduler is already
	// populated by other modules' init code.
	nexus.Invoke(func(app *nexus.App) error {
		sched := app.Scheduler()
		jobs := []cron.Job{
			{
				Name:        "heartbeat",
				Schedule:    "@every 5s",
				Description: "logs a heartbeat every 5 seconds",
				Handler: func(ctx context.Context) error {
					log.Println("[heartbeat] tick")
					return nil
				},
			},
			{
				Name:        "cleanup",
				Schedule:    "@every 30s",
				Description: "demo job — long-ish interval, occasional failure",
				Handler: func(ctx context.Context) error {
					// Fail every third call so the dashboard
					// shows the error state too.
					cleanupCalls++
					if cleanupCalls%3 == 0 {
						return fmt.Errorf("simulated cleanup failure (call %d)", cleanupCalls)
					}
					return nil
				},
			},
			{
				Name:        "midnight-report",
				Schedule:    "@every 24h",
				Description: "schedule far out — manual Trigger to test",
				Handler: func(ctx context.Context) error {
					log.Println("[midnight-report] generated")
					return nil
				},
			},
		}
		for _, j := range jobs {
			if err := sched.Register(j); err != nil {
				return fmt.Errorf("register %s: %w", j.Name, err)
			}
		}
		return nil
	}),
)

var cleanupCalls int

func main() {
	nexus.Run(
		nexus.Config{
			Server:    nexus.ServerConfig{Addr: ":8081"},
			Dashboard: nexus.DashboardConfig{Enabled: true, Name: "Crons (live)"},
		},
		liveModule,
	)
}
