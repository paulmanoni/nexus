package view

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// A live page outlives a dropped connection for a while: its instance — the
// state Mount filled and its events changed, and its subscriptions — waits
// under a resume token the browser holds. A reconnect that presents the
// token, for the same page and the same signed-in user, carries on with it;
// otherwise the page mounts afresh, and the browser sends its forms back
// (runtime.js) so what the user typed survives.

// ResumeGrace is how long a page's state waits for its connection to come
// back.
var ResumeGrace = 30 * time.Second

const resumeMax = 10000 // pages waiting at once; the oldest go first

type parkedPage struct {
	in      *instance
	path    string
	owner   string // the identity the page was opened by ("" when anonymous)
	expires time.Time
}

var parking = struct {
	sync.Mutex
	m map[string]*parkedPage
}{m: map[string]*parkedPage{}}

func newResumeToken() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// park keeps a page's state under token until ResumeGrace runs out.
func park(token string, p *parkedPage) {
	parking.Lock()
	defer parking.Unlock()
	grace := ResumeGrace
	p.expires = time.Now().Add(grace)
	sweepLocked(time.Now())
	for len(parking.m) >= resumeMax {
		var oldest string
		for k, e := range parking.m {
			if oldest == "" || e.expires.Before(parking.m[oldest].expires) {
				oldest = k
			}
		}
		parking.m[oldest].in.sock.close()
		delete(parking.m, oldest)
	}
	parking.m[token] = p
	time.AfterFunc(grace+time.Second, func() {
		parking.Lock()
		defer parking.Unlock()
		sweepLocked(time.Now())
	})
}

// unpark takes the page kept under token, when it is the same page opened by
// the same user and still waiting.
func unpark(token, path, owner string) *parkedPage {
	if token == "" {
		return nil
	}
	parking.Lock()
	defer parking.Unlock()
	sweepLocked(time.Now())
	p, ok := parking.m[token]
	if !ok || p.path != path || p.owner != owner {
		return nil
	}
	delete(parking.m, token)
	return p
}

func sweepLocked(now time.Time) {
	for k, p := range parking.m {
		if now.After(p.expires) {
			p.in.sock.close()
			delete(parking.m, k)
		}
	}
}

// parkedCount is how many pages wait, for tests.
func parkedCount() int {
	parking.Lock()
	defer parking.Unlock()
	return len(parking.m)
}

// dropParked ends every waiting page, for tests.
func dropParked() {
	parking.Lock()
	defer parking.Unlock()
	for k, p := range parking.m {
		p.in.sock.close()
		delete(parking.m, k)
	}
}

// setGrace sets ResumeGrace for a test, under the lock park reads it with.
func setGrace(d time.Duration) (restore func()) {
	parking.Lock()
	defer parking.Unlock()
	old := ResumeGrace
	ResumeGrace = d
	return func() {
		parking.Lock()
		defer parking.Unlock()
		ResumeGrace = old
	}
}

var livePingPeriod = defaultPingPeriod

// liveTimings reads the ping period and LiveIdleTrim for a connection,
// under the lock tests change them with.
func liveTimings() (ping, trim time.Duration) {
	parking.Lock()
	defer parking.Unlock()
	return livePingPeriod, LiveIdleTrim
}

// setLiveTimings changes them for a test.
func setLiveTimings(ping, trim time.Duration) (restore func()) {
	parking.Lock()
	defer parking.Unlock()
	oldPing, oldTrim := livePingPeriod, LiveIdleTrim
	livePingPeriod, LiveIdleTrim = ping, trim
	return func() {
		parking.Lock()
		defer parking.Unlock()
		livePingPeriod, LiveIdleTrim = oldPing, oldTrim
	}
}
