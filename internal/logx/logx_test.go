package logx

import (
	"errors"
	"syscall"
	"testing"
	"time"
)

func TestRepeatGuardCollapsesTheSameFailure(t *testing.T) {
	var g RepeatGuard
	if _, ok := g.Allow("down", time.Minute); !ok {
		t.Fatal("first occurrence must log")
	}
	for i := 0; i < 11; i++ {
		if _, ok := g.Allow("down", time.Minute); ok {
			t.Fatalf("occurrence %d logged inside the window", i+2)
		}
	}
	// A window of zero means "the window has elapsed", which is how the
	// count reaches the next line that does log.
	skipped, ok := g.Allow("down", 0)
	if !ok || skipped != 11 {
		t.Fatalf("want (11, true) once the window elapses, got (%d, %v)", skipped, ok)
	}
}

func TestRepeatGuardLogsADifferentFailure(t *testing.T) {
	var g RepeatGuard
	g.Allow("refused", time.Minute)
	if _, ok := g.Allow("auth failed", time.Minute); !ok {
		t.Fatal("a different failure must not be hidden behind the previous one")
	}
}

func TestRepeatGuardResetMakesTheNextOutageNews(t *testing.T) {
	var g RepeatGuard
	g.Allow("down", time.Minute)
	g.Reset()
	if _, ok := g.Allow("down", time.Minute); !ok {
		t.Fatal("after Reset the same failure must log again")
	}
}

func TestSignatureIgnoresHeapAddresses(t *testing.T) {
	a := errors.New("retries exceeded. last result: &{0x140004a2000 <nil>}, last error: connection refused")
	b := errors.New("retries exceeded. last result: &{0x14000ff1180 <nil>}, last error: connection refused")
	if Signature(a) != Signature(b) {
		t.Fatalf("the same outage produced two signatures:\n %s\n %s", Signature(a), Signature(b))
	}
}

func TestCauseStripsTheRetryWrapper(t *testing.T) {
	err := errors.New("retries exceeded. last result: &{0x1}, last error: dial tcp: connection refused")
	if got := Cause(err); got != "dial tcp: connection refused" {
		t.Fatalf("got %q", got)
	}
	plain := errors.New("plain failure")
	if got := Cause(plain); got != "plain failure" {
		t.Fatalf("an unwrapped error must pass through, got %q", got)
	}
}

func TestHintIsActionable(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{syscall.ECONNREFUSED, "start postgres"},
		{errors.New("dial tcp 1.2.3.4:5432: connect: connection refused"), "start postgres"},
		{errors.New("dial tcp: i/o timeout"), "accepted no connection in time"},
		{errors.New("lookup nope: no such host"), "does not resolve"},
		{errors.New("password authentication failed for user"), "credentials"},
	}
	for _, c := range cases {
		got := Hint(c.err, "postgres", "1.2.3.4:5432")
		if got == "" {
			t.Fatalf("no hint for %v", c.err)
		}
		if !contains(got, c.want) {
			t.Fatalf("hint for %v was %q, wanted it to mention %q", c.err, got, c.want)
		}
	}
	if Hint(errors.New("something unrecognised"), "postgres", "x") != "" {
		t.Fatal("an unrecognised error must not get a made-up hint")
	}
	if Hint(nil, "postgres", "x") != "" {
		t.Fatal("nil must yield no hint")
	}
}

func TestIsRetryState(t *testing.T) {
	if !IsRetryState(errors.New("circuit breaker open")) {
		t.Fatal("a tripped breaker is retry state, not a new fact")
	}
	if IsRetryState(errors.New("connection refused")) {
		t.Fatal("a real failure must not be treated as retry state")
	}
	if IsRetryState(nil) {
		t.Fatal("nil is not retry state")
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
