package auth

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/resource"
)

// throttleRules is [auth.throttle] parsed: a zero limit is off.
type throttleRules struct {
	account, ip limit
	lockout     time.Duration
}

type limit struct {
	n      int
	window time.Duration
}

func resolveThrottle(t ThrottleSettings) (throttleRules, error) {
	var r throttleRules
	var err error
	if r.account, err = parseLimit("account", t.Account, "5/15m"); err != nil {
		return r, err
	}
	if r.ip, err = parseLimit("ip", t.IP, "50/15m"); err != nil {
		return r, err
	}
	r.lockout = t.Lockout
	if r.lockout <= 0 {
		r.lockout = 15 * time.Minute
	}
	return r, nil
}

// parseLimit reads "5/15m"; "off" is no limit.
func parseLimit(key, v, def string) (limit, error) {
	if v == "" {
		v = def
	}
	if v == "off" {
		return limit{}, nil
	}
	n, w, ok := strings.Cut(v, "/")
	count, err1 := strconv.Atoi(n)
	window, err2 := time.ParseDuration(w)
	if !ok || err1 != nil || err2 != nil || count <= 0 || window <= 0 {
		return limit{}, fmt.Errorf(`[auth.throttle] %s = %q: want failures/window, like "5/15m", or "off"`, key, v)
	}
	return limit{count, window}, nil
}

// Failures is what a ThrottleStore keeps per account or client IP.
type Failures struct {
	N      int       `json:"n"`
	Since  time.Time `json:"since"`           // the window's start
	Locked time.Time `json:"locked,omitzero"` // account only: refused until then
}

// ThrottleStore keeps sign-in failure counts. Config.Throttle sets it; the
// default counts in the process, so each replica counts on its own —
// CacheThrottle shares the counts through a cache (Redis).
type ThrottleStore interface {
	// Get returns the counts for key, nil when there are none.
	Get(ctx context.Context, key string) (*Failures, error)
	Put(ctx context.Context, key string, f Failures, ttl time.Duration) error
	Delete(ctx context.Context, key string) error
}

// CacheThrottle keeps the counts in a cache, shared by every replica.
// Updates are read-modify-write, so concurrent failures across replicas
// may undercount by a few — the limit still holds within that margin.
func CacheThrottle(c resource.Cache) ThrottleStore { return cacheThrottle{c} }

type cacheThrottle struct{ c resource.Cache }

const throttleKeyPrefix = "auth:throttle:"

func (s cacheThrottle) Get(ctx context.Context, key string) (*Failures, error) {
	var f Failures
	if err := s.c.Get(ctx, throttleKeyPrefix+key, &f); err != nil {
		return nil, nil //nolint:nilerr // a miss is an error to resource.Cache
	}
	return &f, nil
}

func (s cacheThrottle) Put(ctx context.Context, key string, f Failures, ttl time.Duration) error {
	return s.c.Set(ctx, throttleKeyPrefix+key, f, ttl)
}

func (s cacheThrottle) Delete(ctx context.Context, key string) error {
	return s.c.Delete(ctx, throttleKeyPrefix+key)
}

type memoryThrottle struct {
	mu sync.Mutex
	m  map[string]Failures
}

func (s *memoryThrottle) Get(_ context.Context, key string) (*Failures, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.m[key]
	if !ok {
		return nil, nil
	}
	return &f, nil
}

func (s *memoryThrottle) Put(_ context.Context, key string, f Failures, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = f
	return nil
}

func (s *memoryThrottle) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, key)
	return nil
}

// throttle counts failed sign-ins per account and per client IP.
// Successes don't count; a success clears its account's count. A store
// error never blocks a sign-in.
type throttle struct {
	rules throttleRules
	store ThrottleStore
	mu    sync.Mutex // serialises this process's read-modify-writes
}

func newThrottle(r throttleRules, store ThrottleStore) *throttle {
	if store == nil {
		store = &memoryThrottle{m: map[string]Failures{}}
	}
	return &throttle{rules: r, store: store}
}

// check refuses a sign-in while its account is locked or its IP is over
// its limit.
func (t *throttle) check(ctx context.Context, login string) error {
	now := time.Now()
	if t.rules.account.n > 0 {
		if f := t.current(ctx, accountKey(login), t.rules.account, now); f != nil && now.Before(f.Locked) {
			return tooMany(f.Locked.Sub(now))
		}
	}
	if t.rules.ip.n > 0 {
		if f := t.current(ctx, ipKey(ctx), t.rules.ip, now); f != nil && f.N >= t.rules.ip.n {
			return tooMany(f.Since.Add(t.rules.ip.window).Sub(now))
		}
	}
	return nil
}

// fail records a failed sign-in, locking the account at its limit.
func (t *throttle) fail(ctx context.Context, login string) {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if l := t.rules.account; l.n > 0 {
		key := accountKey(login)
		f := t.bumped(ctx, key, l, now)
		if f.N >= l.n {
			f = Failures{Since: now, Locked: now.Add(t.rules.lockout)}
		}
		_ = t.store.Put(ctx, key, f, max(l.window, t.rules.lockout))
	}
	if l := t.rules.ip; l.n > 0 {
		key := ipKey(ctx)
		_ = t.store.Put(ctx, key, t.bumped(ctx, key, l, now), l.window)
	}
}

func (t *throttle) succeed(ctx context.Context, login string) {
	if t.rules.account.n > 0 {
		_ = t.store.Delete(ctx, accountKey(login))
	}
}

// current returns key's failures, nil once both its window and any lock
// have passed.
func (t *throttle) current(ctx context.Context, key string, l limit, now time.Time) *Failures {
	f, err := t.store.Get(ctx, key)
	if err != nil || f == nil || now.Sub(f.Since) > l.window && !now.Before(f.Locked) {
		return nil
	}
	return f
}

func (t *throttle) bumped(ctx context.Context, key string, l limit, now time.Time) Failures {
	f := Failures{Since: now}
	if cur := t.current(ctx, key, l, now); cur != nil && now.Before(cur.Since.Add(l.window)) {
		f = *cur
	}
	f.N++
	return f
}

func accountKey(login string) string { return "a:" + strings.ToLower(strings.TrimSpace(login)) }

func ipKey(ctx context.Context) string { return "i:" + nexus.ClientIP(ctx) }

func tooMany(wait time.Duration) error {
	wait = wait.Round(time.Second)
	if wait < time.Second {
		wait = time.Second
	}
	e := nexus.Errf(nexus.TooMany, "too many failed sign-ins — try again in %s", wait)
	e.RetryAfter = wait
	return e
}
