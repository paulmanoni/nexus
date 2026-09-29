package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/paulmanoni/nexus"
	"github.com/robfig/cron/v3"
)

// Schedule enqueues the job with args on a cron schedule — standard five
// fields ("0 2 * * *"), a descriptor ("@hourly", "@every 15m"), and an
// optional time zone ("CRON_TZ=Africa/Dar_es_Salaam 0 8 * * 1-5"). It is a
// nexus.Option that also registers the job:
//
//	nexus.Boot(jobs.Module(jobs.Config{}),
//	    SendDigest.Schedule("0 7 * * *", DigestArgs{}))
//
// Every process with workers runs the schedule, and each tick becomes one
// job even when several processes share a store: the tick's job ID derives
// from the job, the schedule and the tick. Ticks missed while no process
// was running are skipped, not caught up.
func (j *Job[A]) Schedule(spec string, args A) nexus.Option {
	sched, err := cron.ParseStandard(spec)
	if err != nil {
		return nexus.Error(fmt.Errorf("jobs: %s: schedule %q: %w", j.def.name, spec, err))
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return nexus.Error(fmt.Errorf("jobs: %s: schedule arguments don't encode as JSON: %w", j.def.name, err))
	}
	s := &schedule{def: j.def, spec: spec, sched: sched, args: raw}
	return nexus.Options(j.Option, nexus.Invoke(func(m *Manager) {
		m.mu.Lock()
		m.schedules = append(m.schedules, s)
		m.mu.Unlock()
	}))
}

type schedule struct {
	def   *definition
	spec  string
	sched cron.Schedule
	args  json.RawMessage
	next  time.Time
}

// tickID is the job ID of one tick: the same in every process.
func (s *schedule) tickID(tick time.Time) ID {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%s\x00%d", s.def.name, s.spec, tick.Unix()))
	return ID("s" + hex.EncodeToString(sum[:10]))
}

// runSchedules enqueues each schedule's ticks as they come.
func (m *Manager) runSchedules() {
	defer m.background.Done()
	m.mu.Lock()
	scheds := append([]*schedule(nil), m.schedules...)
	m.mu.Unlock()
	now := time.Now()
	for _, s := range scheds {
		s.next = s.sched.Next(now)
	}
	for {
		earliest := scheds[0].next
		for _, s := range scheds[1:] {
			if s.next.Before(earliest) {
				earliest = s.next
			}
		}
		timer := time.NewTimer(max(time.Until(earliest), 0))
		select {
		case <-m.stopping:
			timer.Stop()
			return
		case <-timer.C:
		}
		now := time.Now()
		for _, s := range scheds {
			if s.next.After(now) {
				continue
			}
			tick := s.next
			s.next = s.sched.Next(now)
			if _, err := m.enqueue(context.Background(), s.def, s.args, enqueueConfig{id: s.tickID(tick)}); err != nil {
				m.log.Error("jobs: scheduled enqueue failed", "job", s.def.name, "schedule", s.spec, "error", err)
			}
		}
	}
}
