# Background jobs

`extension/jobs` runs work outside the request that asks for it. Jobs are queued,
retried, cancellable, and report progress that a page can show.

Three drivers store the jobs:

| Driver | Package | Jobs survive | Shared by |
|---|---|---|---|
| `memory` (default) | built in | `nexus dev` rebuilds | one process |
| `db` | `extension/jobs/jobsdb` | restarts | every process on the database |
| `redis` | `extension/jobs/jobsredis` (its own module) | restarts, if Redis persists | every process on the Redis |
| `rabbitmq` | `extension/jobs/jobsamqp` (its own module) | restarts | every process on the broker |

The memory, database and Redis drivers keep a record per job, so a job can be looked up,
cancelled and tracked. The RabbitMQ driver keeps jobs as messages instead: delivery is
pushed and durable, but there's no per-job state (see [RabbitMQ](#rabbitmq)).

## Define a job

A job is a method with a fixed shape. Its receiver comes from DI, like a controller's,
and its arguments are a struct that encodes as JSON:

```go
import "github.com/paulmanoni/nexus/v2/extension/jobs"

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
rec, ok, err := m.Get(ctx, id)   // State, Progress, Result, Error, Attempt, Actor…
found, err := m.Cancel(ctx, id)  // a queued job never runs; a running one's context is cancelled
recs, err := m.List(ctx, jobs.Filter{Name: "ReportService.Export", State: jobs.StateFailed, Limit: 20})
```

- **Cancelling reaches other processes.** A job running in another process is
  cancelled at its next lease renewal.
- **Delivery is at least once.** A job interrupted by a shutdown or a crash runs again,
  so make handlers idempotent.

## Schedules

A job handle runs on a cron schedule:

```go
nexus.Boot(jobs.Module(jobs.Config{}),
    SendDigest.Schedule("0 7 * * *", DigestArgs{}),                     // daily at 07:00
    Cleanup.Schedule("@every 15m", CleanupArgs{}),
    Payroll.Schedule("CRON_TZ=Africa/Dar_es_Salaam 0 8 1 * *", PayrollArgs{}),
)
```

- **The option registers the job too.** `Schedule` also registers the job, so you don't
  pass it to `Boot` separately.
- **Each tick is one job, even with several replicas.** Its ID derives from the job,
  the schedule and the tick. With a shared driver, every process runs the scheduler and
  each tick is still enqueued once.
- **Missed ticks are skipped,** not caught up, if nothing was running at the time.

## Drivers

### Memory

The default. Jobs live in the process and are carried across `nexus dev` rebuilds. A job
the old process was running starts again, resuming from its checkpoint. Nothing survives
a restart, and each process has its own queue.

### Database

```go
import "github.com/paulmanoni/nexus/v2/extension/jobs/jobsdb"

type DB struct{ *db.Manager }

nexus.Boot(
    db.BindFromConfig[DB]("main"),
    jobsdb.Bind[DB](),            // with a store in DI, jobs.Module uses it
    jobs.Module(jobs.Config{}),
)
```

- **Any GORM database works:** Postgres, MySQL and SQLite behave the same.
- **Tables are created on first use** (`nexus_jobs`, `nexus_job_uniques`). Pass
  `jobsdb.NoMigrate()` to leave that to your own migrations, which can call
  `jobsdb.Migrate(gormDB)`.
- **Claims and updates are atomic.** Every write is conditional on the row's version.
- **An outage doesn't stop the app.** While the database is unreachable, workers log and
  keep polling.

### Redis

```go
import "github.com/paulmanoni/nexus/extension/jobs/jobsredis/v2"

nexus.Boot(
    jobsredis.Bind(jobsredis.Config{}), // [jobs.redis] url, else REDIS_URL, else localhost
    jobs.Module(jobs.Config{}),
)
```

```toml
[jobs.redis]
url    = "redis://:password@redis:6379/2"
prefix = "{nexus:jobs}:"   # the default; one hash tag keeps Redis Cluster happy
```

- **It's a separate module** (`go get github.com/paulmanoni/nexus/extension/jobs/jobsredis/v2`),
  so apps that don't use it link no Redis client.
- **Claims and enqueues are Lua scripts;** updates are optimistic transactions.
- **Make Redis durable** (AOF or RDB persistence) if jobs must survive a Redis restart.
  Also keep this Redis's eviction policy `noeviction`, so it never drops job keys.

### RabbitMQ

```go
import "github.com/paulmanoni/nexus/extension/jobs/jobsamqp/v2"

nexus.Boot(
    jobsamqp.Bind(jobsamqp.Config{}), // [jobs.rabbitmq] url, else RABBIT_URL, else localhost
    jobs.Module(jobs.Config{}),
)
```

```toml
[jobs.rabbitmq]
url              = "amqp://user:pass@rabbitmq:5672/"
prefix           = "nexus.jobs."   # queue names
consumer_timeout = "8h"            # the longest a job may run
delivery_limit   = 20              # deliveries before a crashing job is dead-lettered
```

Each job queue becomes three kinds of RabbitMQ queue:

| Queue | What it holds |
|---|---|
| `nexus.jobs.<queue>` | The work: a quorum queue |
| `nexus.jobs.<queue>.failed` | Jobs that failed for good |
| `nexus.jobs.<queue>.delay.<ms>` | Delayed jobs and retries, waiting until due; deleted when idle |

- **Delivery is confirmed.** An enqueue returns once the broker confirms the message. A
  worker acknowledges only after the job finishes, so a worker that crashes mid-job hands
  the message back and another process runs it.
- **Long jobs need the consumer timeout.** RabbitMQ closes a channel whose delivery stays
  unacknowledged past its consumer timeout, 30 minutes by default, and the job would run
  again. The work queues are declared with `x-consumer-timeout` (default 8h, RabbitMQ
  3.12+). Keep it above your longest `jobs.Timeout`.
- **Changing queue arguments means deleting the queue.** RabbitMQ refuses to redeclare a
  queue with different arguments, so delete the queue before changing `consumer_timeout`
  or `delivery_limit`.
- **Crash loops end in `.failed`.** A job that crashes its worker again and again is
  dead-lettered there after `delivery_limit` deliveries, instead of looping forever.
- **Shutdown hands jobs on.** A job interrupted by shutdown is republished with its
  checkpoint and without counting the attempt, and it continues in the next process.
- **There's no per-job state.** `Manager.Get`, `Cancel` and `List` return
  `jobs.ErrUnsupported`, progress and results aren't stored, and `jobs.Unique` is refused
  at boot. Checkpoints still travel with a job into its next attempt.
- **Run schedules on one replica.** Every process that runs a schedule enqueues each
  tick, since there's no shared record to dedupe against.
- **The dashboard shows** each queue's waiting messages, plus what this process ran.

### How several processes share a queue

This applies to the database and Redis drivers.

- **Leases.** A worker claims a job with a lease (`[jobs] lease`, default 30s) and renews
  it every third of that while the job runs. If the process dies, the lease lapses and
  another process takes the job over.
- **Crashes use up attempts.** A job whose worker died on its last attempt fails, with
  an error saying its worker was lost.
- **Every write checks ownership.** Progress, results, checkpoints and the outcome are
  written only while the attempt still owns the job. An attempt that lost its lease (after
  a long GC pause, say) sees `jobs.ErrLostOwnership`, and its context is cancelled.
- **Web and worker replicas.** Web replicas can set `run = false` to enqueue without
  running jobs; worker replicas run them.

## Configuration

```toml
[jobs]
driver = "db"              # memory | db | redis | rabbitmq — default: the bound driver's, else memory
run    = true              # false: enqueue here, run elsewhere
shutdown_grace = "10s"     # running jobs get this long on shutdown (0 under nexus dev)
lease  = "30s"             # a claim's lease, renewed while the job runs
poll   = "1s"              # how often idle workers check a shared store

[jobs.queues]              # queue → concurrent workers (default: default = 4)
default = 4
low     = 1
```

- **Shutdown.** Workers stop taking jobs, and running jobs get the grace period. Then
  their contexts are cancelled and they are requeued without using up an attempt.
- **Finished jobs are kept for 24h** (`Config.Retention`), then pruned.
- **Dashboard.** A `jobs` queue node shows the driver, each queue's running, queued,
  delayed, failed and finished counts, the registered jobs, and the latest failure.
