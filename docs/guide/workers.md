# Workers & crons

## Workers

A worker is a long-running background function. Its first parameter must be
`context.Context`, which is cancelled on shutdown. The remaining parameters are
injected.

```go
nexus.AsWorker("order-events",
    func(ctx context.Context, db *DB, cache *Cache) error {
        for {
            select {
            case <-ctx.Done():
                return nil
            case ev := <-events:
                handle(ctx, db, cache, ev)
            }
        }
    })
```

With decorators, `//@worker order-events` registers the same thing.

## Crons

```go
app.Cron("refresh-inventory", "@every 30s").
    Describe("Refresh the inventory cache").
    Service("inventory").          // draws the cron → service edge
    Handler(func(ctx context.Context) error {
        return refresh(ctx)
    })

app.Cron("daily-report", "0 9 * * *").
    Handler(sendDailyReport)
```

Schedules accept standard five-field cron expressions and `@every <duration>`.

The dashboard's Crons tab shows:

- each schedule
- the last run and its result
- controls to pause, resume or trigger a run

The dependencies of workers and crons (resources and services) are detected
automatically and drawn on the architecture graph.
