// Package jobsredis stores background jobs in Redis — the "redis" driver of
// extension/jobs. Several processes share the queue: claims and enqueues are
// atomic Lua scripts, updates are optimistic transactions, and leases hand a
// dead process's jobs to a live one.
//
//	nexus.Boot(
//	    jobsredis.Bind(jobsredis.Config{}),   // [jobs.redis] url, else REDIS_URL
//	    jobs.Module(jobs.Config{}),
//	    ExportReport,
//	)
//
// It is a separate module, so an app that doesn't use it links no Redis
// client. Keys share one hash tag ({nexus:jobs} by default), so the scripts
// work on Redis Cluster too.
package jobsredis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension/jobs"
)

// Config says where the queue lives. Zero values read [jobs.redis] in
// nexus.toml (url, prefix), then REDIS_URL, then redis://127.0.0.1:6379.
type Config struct {
	URL    string // redis://[:password@]host:port[/db]
	Prefix string // key prefix; default "{nexus:jobs}:"
}

// Bind connects to Redis at boot and provides the jobs.Store; with it in DI,
// jobs.Module uses the redis driver. The client closes on shutdown.
func Bind(cfg Config) nexus.Option {
	return nexus.Provide(func(lc nexus.Lifecycle) (jobs.Store, error) {
		url := cfg.URL
		if url == "" {
			url = nexus.Get("jobs.redis.url", os.Getenv("REDIS_URL"))
		}
		if url == "" {
			url = "redis://127.0.0.1:6379"
		}
		opt, err := redis.ParseURL(url)
		if err != nil {
			return nil, fmt.Errorf("jobsredis: url %q: %w", url, err)
		}
		client := redis.NewClient(opt)
		lc.Append(nexus.Hook{OnStop: func(context.Context) error { return client.Close() }})
		prefix := cfg.Prefix
		if prefix == "" {
			prefix = nexus.Get("jobs.redis.prefix", "")
		}
		return New(client, prefix), nil
	})
}

// New returns a store on client; prefix "" is "{nexus:jobs}:".
func New(client redis.UniversalClient, prefix string) jobs.Store {
	if prefix == "" {
		prefix = "{nexus:jobs}:"
	}
	return &store{c: client, p: prefix}
}

type store struct {
	c redis.UniversalClient
	p string
}

func (s *store) Driver() string { return "redis" }
func (s *store) Shared() bool   { return true }

func (s *store) jobKey(id jobs.ID) string   { return s.p + "job:" + string(id) }
func (s *store) queuedKey(q string) string  { return s.p + "q:" + q }
func (s *store) runningKey(q string) string { return s.p + "r:" + q }
func (s *store) allKey() string             { return s.p + "all" }
func (s *store) doneKey() string            { return s.p + "done" }
func (s *store) queuesKey() string          { return s.p + "queues" }
func (s *store) uniqueKey(k string) string  { return s.p + "u:" + k }

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMS(v string) time.Time {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n == 0 {
		return time.Time{}
	}
	return time.UnixMilli(n).UTC()
}

// fields encodes a record as the job hash: the whole record as JSON, and the
// fields the claim script changes on their own, which win when decoding.
func fields(rec jobs.Record, version int64) (map[string]any, error) {
	raw, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	cancel := "0"
	if rec.CancelRequested {
		cancel = "1"
	}
	return map[string]any{
		"rec": raw, "state": string(rec.State), "attempt": rec.Attempt, "max": rec.MaxAttempts,
		"queue": rec.Queue, "worker": rec.Worker, "lease": ms(rec.LeaseUntil), "started": ms(rec.StartedAt),
		"finished": ms(rec.FinishedAt), "error": rec.Error, "cancel": cancel, "v": version,
	}, nil
}

func decode(h map[string]string) (jobs.Record, int64, error) {
	var rec jobs.Record
	if err := json.Unmarshal([]byte(h["rec"]), &rec); err != nil {
		return rec, 0, fmt.Errorf("jobsredis: corrupt job: %w", err)
	}
	rec.State = jobs.State(h["state"])
	rec.Attempt, _ = strconv.Atoi(h["attempt"])
	rec.Worker = h["worker"]
	rec.LeaseUntil = fromMS(h["lease"])
	rec.StartedAt = fromMS(h["started"])
	rec.FinishedAt = fromMS(h["finished"])
	rec.Error = h["error"]
	rec.CancelRequested = h["cancel"] == "1"
	v, _ := strconv.ParseInt(h["v"], 10, 64)
	return rec, v, nil
}

// insertScript adds a job unless its ID exists or, for a unique key, a
// pending job holds the key. Returns {id, inserted}.
var insertScript = redis.NewScript(`
local p, id, queue, runat, created, ukey, uttl = ARGV[1], ARGV[2], ARGV[3], ARGV[4], ARGV[5], ARGV[6], tonumber(ARGV[7])
local jk = p .. 'job:' .. id
if redis.call('EXISTS', jk) == 1 then return {id, 0} end
if ukey ~= '' then
  local holder = redis.call('GET', p .. 'u:' .. ukey)
  if holder then
    local st = redis.call('HGET', p .. 'job:' .. holder, 'state')
    if st == 'queued' or st == 'running' then return {holder, 0} end
  end
  redis.call('SET', p .. 'u:' .. ukey, id, 'PX', math.max(uttl, 1))
end
for i = 8, #ARGV, 2 do redis.call('HSET', jk, ARGV[i], ARGV[i + 1]) end
redis.call('ZADD', p .. 'q:' .. queue, runat, id)
redis.call('ZADD', p .. 'all', created, id)
redis.call('SADD', p .. 'queues', queue)
return {id, 1}
`)

func (s *store) Insert(ctx context.Context, rec jobs.Record) (jobs.ID, bool, error) {
	f, err := fields(rec, 0)
	if err != nil {
		return "", false, err
	}
	args := []any{s.p, string(rec.ID), rec.Queue, ms(rec.RunAt), ms(rec.CreatedAt), rec.UniqueKey,
		max(time.Until(rec.UniqueUntil).Milliseconds(), 0)}
	for k, v := range f {
		args = append(args, k, v)
	}
	res, err := insertScript.Run(ctx, s.c, []string{s.jobKey(rec.ID)}, args...).Slice()
	if err != nil {
		return "", false, err
	}
	return jobs.ID(res[0].(string)), res[1].(int64) == 1, nil
}

// claimScript takes one job of the queue: first a running one whose lease
// expired (failing it instead when its attempts are spent), then the oldest
// due queued one. Returns {"claimed", id}, {"failed", id} or nil.
var claimScript = redis.NewScript(`
local p, queue, now, lease, worker, lostmsg = ARGV[1], ARGV[2], tonumber(ARGV[3]), tonumber(ARGV[4]), ARGV[5], ARGV[6]
local rk, qk = p .. 'r:' .. queue, p .. 'q:' .. queue
local id
local expired = redis.call('ZRANGEBYSCORE', rk, '-inf', '(' .. now, 'LIMIT', 0, 1)
if #expired > 0 then
  id = expired[1]
  local jk = p .. 'job:' .. id
  local attempt = tonumber(redis.call('HGET', jk, 'attempt')) or 0
  local maxa = tonumber(redis.call('HGET', jk, 'max')) or 1
  if attempt >= maxa then
    redis.call('HSET', jk, 'state', 'failed', 'finished', now, 'error', lostmsg)
    redis.call('HINCRBY', jk, 'v', 1)
    redis.call('ZREM', rk, id)
    redis.call('ZADD', p .. 'done', now, id)
    return {'failed', id}
  end
else
  local due = redis.call('ZRANGEBYSCORE', qk, '-inf', now, 'LIMIT', 0, 1)
  if #due == 0 then return nil end
  id = due[1]
end
local jk = p .. 'job:' .. id
redis.call('HINCRBY', jk, 'attempt', 1)
redis.call('HSET', jk, 'state', 'running', 'worker', worker, 'lease', now + lease, 'started', now)
redis.call('HINCRBY', jk, 'v', 1)
redis.call('ZREM', qk, id)
redis.call('ZADD', rk, now + lease, id)
return {'claimed', id}
`)

const lostMessage = "the worker running it stopped renewing its lease (lost), with no attempts left"

func (s *store) Claim(ctx context.Context, queue, worker string, now time.Time, lease time.Duration) (jobs.Record, bool, error) {
	for range 10 {
		res, err := claimScript.Run(ctx, s.c, []string{s.queuedKey(queue), s.runningKey(queue)},
			s.p, queue, now.UnixMilli(), lease.Milliseconds(), worker, lostMessage).Slice()
		if errors.Is(err, redis.Nil) {
			return jobs.Record{}, false, nil
		}
		if err != nil {
			return jobs.Record{}, false, err
		}
		if res[0].(string) == "failed" {
			continue
		}
		rec, ok, err := s.Get(ctx, jobs.ID(res[1].(string)))
		return rec, ok, err
	}
	return jobs.Record{}, false, nil
}

// reindex moves a job between the queue indexes to match its new state.
func (s *store) reindex(ctx context.Context, p redis.Pipeliner, old, rec jobs.Record) {
	id := string(rec.ID)
	p.ZRem(ctx, s.queuedKey(old.Queue), id)
	p.ZRem(ctx, s.runningKey(old.Queue), id)
	switch {
	case rec.State == jobs.StateQueued:
		p.ZAdd(ctx, s.queuedKey(rec.Queue), redis.Z{Score: float64(ms(rec.RunAt)), Member: id})
	case rec.State == jobs.StateRunning:
		p.ZAdd(ctx, s.runningKey(rec.Queue), redis.Z{Score: float64(ms(rec.LeaseUntil)), Member: id})
	case rec.State.Finished():
		p.ZAdd(ctx, s.doneKey(), redis.Z{Score: float64(ms(rec.FinishedAt)), Member: id})
	}
}

func (s *store) Update(ctx context.Context, id jobs.ID, fn func(*jobs.Record) bool) (jobs.Record, bool, error) {
	key := s.jobKey(id)
	for range 10 {
		var out jobs.Record
		var wrote, missing bool
		err := s.c.Watch(ctx, func(tx *redis.Tx) error {
			h, err := tx.HGetAll(ctx, key).Result()
			if err != nil {
				return err
			}
			if len(h) == 0 {
				missing = true
				return nil
			}
			old, v, err := decode(h)
			if err != nil {
				return err
			}
			rec := old
			if !fn(&rec) {
				out = rec
				return nil
			}
			f, err := fields(rec, v+1)
			if err != nil {
				return err
			}
			_, err = tx.TxPipelined(ctx, func(p redis.Pipeliner) error {
				p.HSet(ctx, key, f)
				s.reindex(ctx, p, old, rec)
				return nil
			})
			if err == nil {
				out, wrote = rec, true
			}
			return err
		}, key)
		if errors.Is(err, redis.TxFailedErr) {
			continue
		}
		if err != nil || missing {
			return jobs.Record{}, false, err
		}
		return out, wrote, nil
	}
	return jobs.Record{}, false, fmt.Errorf("jobsredis: job %s stayed contended", id)
}

func (s *store) Get(ctx context.Context, id jobs.ID) (jobs.Record, bool, error) {
	h, err := s.c.HGetAll(ctx, s.jobKey(id)).Result()
	if err != nil || len(h) == 0 {
		return jobs.Record{}, false, err
	}
	rec, _, err := decode(h)
	return rec, err == nil, err
}

// records loads jobs by ID, skipping any pruned meanwhile.
func (s *store) records(ctx context.Context, ids []string) ([]jobs.Record, error) {
	cmds := make([]*redis.MapStringStringCmd, len(ids))
	_, err := s.c.Pipelined(ctx, func(p redis.Pipeliner) error {
		for i, id := range ids {
			cmds[i] = p.HGetAll(ctx, s.jobKey(jobs.ID(id)))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]jobs.Record, 0, len(ids))
	for _, c := range cmds {
		if h := c.Val(); len(h) > 0 {
			rec, _, err := decode(h)
			if err != nil {
				return nil, err
			}
			out = append(out, rec)
		}
	}
	return out, nil
}

func (s *store) List(ctx context.Context, f jobs.Filter) ([]jobs.Record, error) {
	const page = 200
	var out []jobs.Record
	for start := int64(0); ; start += page {
		ids, err := s.c.ZRevRange(ctx, s.allKey(), start, start+page-1).Result()
		if err != nil {
			return nil, err
		}
		recs, err := s.records(ctx, ids)
		if err != nil {
			return nil, err
		}
		for i := range recs {
			if f.Matches(&recs[i]) {
				out = append(out, recs[i])
				if f.Limit > 0 && len(out) == f.Limit {
					return out, nil
				}
			}
		}
		if len(ids) < page {
			return out, nil
		}
	}
}

func (s *store) NextDue(ctx context.Context, queue string) (time.Time, bool, error) {
	z, err := s.c.ZRangeWithScores(ctx, s.queuedKey(queue), 0, 0).Result()
	if err != nil || len(z) == 0 {
		return time.Time{}, false, err
	}
	return time.UnixMilli(int64(z[0].Score)), true, nil
}

func (s *store) Stats(ctx context.Context, now time.Time) (jobs.Stats, error) {
	st := jobs.Stats{Queues: map[string]jobs.QueueStats{}}
	queues, err := s.c.SMembers(ctx, s.queuesKey()).Result()
	if err != nil {
		return st, err
	}
	nowMS := strconv.FormatInt(now.UnixMilli(), 10)
	for _, q := range queues {
		due, _ := s.c.ZCount(ctx, s.queuedKey(q), "-inf", nowMS).Result()
		delayed, _ := s.c.ZCount(ctx, s.queuedKey(q), "("+nowMS, "+inf").Result()
		running, _ := s.c.ZCard(ctx, s.runningKey(q)).Result()
		st.Queues[q] = jobs.QueueStats{Queued: int(due), Delayed: int(delayed), Running: int(running)}
	}
	ids, err := s.c.ZRevRange(ctx, s.doneKey(), 0, 4999).Result()
	if err != nil {
		return st, err
	}
	done, err := s.records(ctx, ids)
	if err != nil {
		return st, err
	}
	for i := range done {
		rec := &done[i]
		q := st.Queues[rec.Queue]
		switch rec.State {
		case jobs.StateSucceeded:
			q.Succeeded++
		case jobs.StateFailed:
			q.Failed++
			if st.LastFailure == nil {
				st.LastFailure = rec
			}
		case jobs.StateCancelled:
			q.Cancelled++
		}
		st.Queues[rec.Queue] = q
	}
	return st, nil
}

// Prune deletes jobs finished before cutoff; keep is not applied (retention
// bounds the store). Unique keys expire on their own.
func (s *store) Prune(ctx context.Context, cutoff time.Time, _ int) error {
	ids, err := s.c.ZRangeByScore(ctx, s.doneKey(), &redis.ZRangeBy{Min: "-inf", Max: "(" + strconv.FormatInt(cutoff.UnixMilli(), 10)}).Result()
	if err != nil || len(ids) == 0 {
		return err
	}
	_, err = s.c.Pipelined(ctx, func(p redis.Pipeliner) error {
		for _, id := range ids {
			p.Del(ctx, s.jobKey(jobs.ID(id)))
			p.ZRem(ctx, s.doneKey(), id)
			p.ZRem(ctx, s.allKey(), id)
		}
		return nil
	})
	return err
}
