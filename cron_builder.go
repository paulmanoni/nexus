package nexus

import (
	"context"
	"log"

	"github.com/paulmanoni/nexus/cron"
)

// Cron starts building a scheduled job. Finalize with .Handler(fn). Jobs run
// bare — middleware chains do not apply — but the scheduler still emits
// request.start / request.end trace events so they appear on the dashboard
// Traces tab.
//
//	app.Cron("refresh-pets", "*/5 * * * *").
//	    Describe("Warm the pet cache").
//	    Handler(func(ctx context.Context) error { return nil })
func (a *App) Cron(name, schedule string) *CronBuilder {
	return &CronBuilder{app: a, job: cron.Job{Name: name, Schedule: schedule}}
}

// CronBuilder accumulates optional metadata before Handler registers the job
// with the scheduler.
type CronBuilder struct {
	app *App
	job cron.Job
}

// Describe sets a human-readable description shown on the dashboard.
func (c *CronBuilder) Describe(desc string) *CronBuilder {
	c.job.Description = desc
	return c
}

// Service groups the cron under a service on the dashboard. Optional.
func (c *CronBuilder) Service(name string) *CronBuilder {
	c.job.Service = name
	return c
}

// Handler finalizes the registration. A bad schedule is logged and the job is
// dropped; the app keeps running.
func (c *CronBuilder) Handler(fn func(ctx context.Context) error) {
	c.job.Handler = fn
	if err := c.app.cronSched.Register(c.job); err != nil {
		log.Printf("nexus: %v", err)
	}
}