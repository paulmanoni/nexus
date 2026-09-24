// Package logx holds the log-shaping helpers the resource managers share:
// collapsing a failure that recurs on a retry tick, and turning a bare network
// error into a line the reader can act on.
package logx

import (
	"errors"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// RepeatGuard collapses a failure that keeps recurring. Managers retry on a
// timer, so without it a database that stays down reprints the same line every
// tick for as long as the outage lasts.
type RepeatGuard struct {
	mu         sync.Mutex
	signature  string
	suppressed int
	lastAt     time.Time
}

// Allow reports whether this occurrence should be logged, and how many
// identical ones it swallowed since the last time it said yes. A change of
// signature always logs, so a new failure is never hidden behind an old one.
func (g *RepeatGuard) Allow(signature string, every time.Duration) (suppressed int, ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	if signature != g.signature {
		g.signature, g.suppressed, g.lastAt = signature, 0, now
		return 0, true
	}
	if now.Sub(g.lastAt) >= every {
		suppressed, g.suppressed, g.lastAt = g.suppressed, 0, now
		return suppressed, true
	}
	g.suppressed++
	return 0, false
}

// Reset forgets the current signature, so the next failure logs immediately.
// Call it once a connection succeeds: the next outage is news again.
func (g *RepeatGuard) Reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.signature, g.suppressed = "", 0
}

// Hint returns one actionable line for the connection errors that dominate a
// developer's logs, or "" when the error is not one of them. what names the
// dependency ("postgres", "redis"); addr is where it was looked for.
func Hint(err error, what, addr string) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case errors.Is(err, syscall.ECONNREFUSED) || strings.Contains(msg, "connection refused"):
		return "nothing is listening on " + addr + " — start " + what + ", or point the config at the host that runs it"
	case errors.Is(err, syscall.ETIMEDOUT) || strings.Contains(msg, "i/o timeout"):
		return addr + " accepted no connection in time — check the host and any firewall in between"
	case strings.Contains(msg, "no such host"):
		return "the host in " + addr + " does not resolve — check it for a typo"
	case strings.Contains(msg, "authentication failed"), strings.Contains(msg, "password authentication"):
		return "credentials for " + what + " were rejected — check the user and password in the config"
	case strings.Contains(msg, "does not exist"):
		return what + " is reachable but the database itself is missing — create it, or fix the name in the config"
	}
	return ""
}

// volatilePointer matches the heap addresses failsafe-go embeds in its
// "last result: &{0x…}" text. They change on every attempt, which would make
// every repeat of the same outage look like a new failure to RepeatGuard.
var volatilePointer = regexp.MustCompile(`0x[0-9a-f]+`)

// Signature reduces an error to something stable enough to compare across
// retries, so RepeatGuard can tell "still the same outage" from "something
// new broke".
func Signature(err error) string {
	if err == nil {
		return ""
	}
	return volatilePointer.ReplaceAllString(Cause(err), "0x…")
}

// Cause strips the retry machinery's wrapper so the message a reader sees is
// the one that explains the failure. failsafe-go reports
// "retries exceeded. last result: &{…}, last error: <the real problem>", and
// only the tail is worth showing.
func Cause(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if i := strings.LastIndex(msg, "last error: "); i >= 0 {
		return msg[i+len("last error: "):]
	}
	return msg
}

// IsRetryState reports whether the error is the retry machinery describing its
// own state rather than a new fact about the dependency. A tripped circuit
// breaker means "we already stopped trying for now", which the caller reported
// when the underlying failure happened — logging it again says nothing new,
// and it is a different string, so it would defeat RepeatGuard.
func IsRetryState(err error) bool {
	return err != nil && strings.Contains(Cause(err), "circuit breaker open")
}
