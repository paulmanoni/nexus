# Background jobs

`extension/jobs` runs work outside the request that asks for it. Jobs are queued,
retried, cancellable, and report progress that a page can show.

::: tip Phase one
This release ships the **memory** driver. Jobs live in the process: they survive
`nexus dev` rebuilds, but not restarts, and they don't spread across replicas. A durable
database driver comes next. The API below won't change.
:::

## Define a job

A job is a method with a fixed shape. Its receiver comes from DI, like a controller's,
and its arguments are a struct that encodes as JSON:

```go
import "github.com/paulmanoni/nexus/extension/jobs"

type ExportArgs struct{ ReportID int }

func (s *ReportService) Export(ctx context.Context, run *jobs.Run, a ExportArgs) error {
    parts := s.parts(a.ReportID)
    for i, part := range parts {
        if err := run.Progress(i+1, len(parts), "rendering "+part.Name); err != nil {
            return err // cancelled, timed out, or shutting down
        }
        …
    }
    return run.SetResult(ExportFile{URL: url})
}

var ExportReport = jobs.Define((*ReportService).Export,
    jobs.Queue("low"), jobs.Timeout(2*time.Hour), jobs.Retry(3))

nexus.Boot(jobs.Module(jobs.Config{}), ExportReport /* , … */)
```

Or annotate the method and let the generator register it:

```go
//@job low timeout=2h retry=3
func (s *ReportService) Export(ctx context.Context, run *jobs.Run, a ExportArgs) error
```

`jobs.DefineFunc` takes a plain function with no receiver. The job's name, which records
and the dashboard show, comes from the method (`ReportService.Export`). Set
`jobs.Name("…")` to keep queued jobs attached across a rename.

| Option | Effect |
|---|---|
| `jobs.Queue("low")` | The queue the job runs on (default `default`) |
| `jobs.Timeout(d)` | Cancels an attempt's context after `d`; the attempt fails |
| `jobs.Retry(n)` | Up to `n` more attempts after a failure (default none), waiting 5s, doubling, capped at 1h |
| `jobs.Backoff(fn)` | Your own wait between attempts |
| `jobs.Unique(ttl)` | Enqueuing the same job with the same arguments while one is pending returns that job's ID |
| `jobs.Name("…")` | A stable name |

## Enqueue

```go
id, err := ExportReport.Enqueue(ctx, ExportArgs{ReportID: 7})
id, err := jobs.Enqueue(ctx, (*ReportService).Export, ExportArgs{ReportID: 7}) // annotated jobs
id, err := ExportReport.Enqueue(ctx, args, jobs.Delay(10*time.Minute))           // or jobs.At(t)
```

- **Enqueue records who asked.** The enqueuing request's user, as
  `nexus.RequestIdentity` sees it, is kept on the job as `run.Actor()`.
- **Enqueuing by method expression** needs the method to be defined once. A method
  defined twice (with different options) must be enqueued through its handle.

## Inside a job

`*jobs.Run` is the attempt's handle:

- `Progress(done, total, message)` records progress. It returns the context's error once
  the job should stop, so a loop that reports progress also notices cancellation.
- `SetResult(v)` stores the job's result as JSON.
- `Checkpoint(state)` and `Resume(&state)` let a long job continue where it stopped
  after a failure or a shutdown, instead of starting over.
- `ID()`, `Attempt()` and `Actor()` identify the run.

Return `jobs.Permanent(err)` for a failure a retry cannot fix, such as bad input or a
missing record. A panic fails the job with its stack trace.

## Look jobs up, cancel them

Inject `*jobs.Manager`:

```go
rec, ok := m.Get(id)        // State, Progress, Result, Error, Attempt, Actor…
m.Cancel(id)                // a queued job never runs; a running one's context is cancelled
m.List(jobs.Filter{Name: "ReportService.Export", State: jobs.StateFailed, Limit: 20})
```

Jobs are delivered **at least once**: a job interrupted by shutdown runs again, so make
handlers idempotent.

## Configuration

```toml
[jobs]
driver = "memory"          # the one driver in this release
run    = true              # false: enqueue here, run elsewhere
shutdown_grace = "10s"     # running jobs get this long on shutdown (0 under nexus dev)

[jobs.queues]              # queue → concurrent workers (default: default = 4)
default = 4
low     = 1
```

- **Shutdown.** Workers stop taking jobs, running jobs get the grace period, and then
  their contexts are cancelled and they are requeued.
- **`nexus dev`.** The queue is carried across the rebuild, and a job the old process was
  running starts again in the new one, resuming from its checkpoint.
- **Dashboard.** A `jobs` queue node shows each queue's running, queued, delayed, failed
  and finished counts, the registered jobs, and the latest failure.
