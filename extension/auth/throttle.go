package auth

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/paulmanoni/nexus/v2"
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

// throttle counts failed sign-ins per account and per client IP, in this
// process. Successes don't count; a success clears its account's count.
type throttle struct {
	rules throttleRules
	mu    sync.Mutex
	m     map[string]*failures
}

type failures struct {
	n      int
	since  time.Time
	locked time.Time // account only: refused until then
}

func newThrottle(r throttleRules) *throttle { return &throttle{rules: r, m: map[string]*failures{}} }

// check refuses a sign-in while its account is locked or its IP is over
// its limit.
func (t *throttle) check(ctx context.Context, login string) error {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if f := t.m[accountKey(login)]; f != nil && now.Before(f.locked) {
		return tooMany(f.locked.Sub(now))
	}
	if t.rules.ip.n > 0 {
		if f := t.current(ipKey(ctx), t.rules.ip, now); f != nil && f.n >= t.rules.ip.n {
			return tooMany(f.since.Add(t.rules.ip.window).Sub(now))
		}
	}
	return nil
}

// fail records a failed sign-in, locking the account at its limit.
func (t *throttle) fail(ctx context.Context, login string) {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.rules.account.n > 0 {
		f := t.bump(accountKey(login), t.rules.account, now)
		if f.n >= t.rules.account.n {
			f.locked = now.Add(t.rules.lockout)
			f.n, f.since = 0, now
		}
	}
	if t.rules.ip.n > 0 {
		t.bump(ipKey(ctx), t.rules.ip, now)
	}
}

func (t *throttle) succeed(login string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.m, accountKey(login))
}

// current returns key's failures in its window, dropping an expired one.
func (t *throttle) current(key string, l limit, now time.Time) *failures {
	f := t.m[key]
	if f != nil && now.Sub(f.since) > l.window && now.After(f.locked) {
		delete(t.m, key)
		return nil
	}
	return f
}

func (t *throttle) bump(key string, l limit, now time.Time) *failures {
	f := t.current(key, l, now)
	if f == nil {
		f = &failures{since: now}
		t.m[key] = f
	}
	f.n++
	return f
}

func accountKey(login string) string { return "a:" + strings.ToLower(strings.TrimSpace(login)) }

func ipKey(ctx context.Context) string { return "i:" + nexus.ClientIP(ctx) }

func tooMany(wait time.Duration) error {
	wait = wait.Round(time.Second)
	if wait < time.Second {
		wait = time.Second
	}
	return nexus.Errf(nexus.TooMany, "too many failed sign-ins — try again in %s", wait)
}
