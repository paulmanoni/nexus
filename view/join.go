package view

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// A live page's first render travels over HTTP. The socket that connects
// next must not send it again, so the HTTP render is kept for a moment under
// a join id the page carries: the connection renders its own mount and
// compares — identical, and nothing is sent; different (Mount did more once
// Connected), and the page is corrected. A socket that finds no join (a
// restart, another replica, a reconnect) sends the full render.

const (
	joinTTL = time.Minute
	joinMax = 10000
)

var joins = struct {
	sync.Mutex
	m map[string]joinEntry
}{m: map[string]joinEntry{}}

type joinEntry struct {
	html    string
	expires time.Time
}

// keepJoin stores an HTTP render and returns its join id.
func keepJoin(html string) string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	id := hex.EncodeToString(b[:])
	now := time.Now()
	joins.Lock()
	defer joins.Unlock()
	if len(joins.m) >= joinMax {
		for k, e := range joins.m {
			if now.After(e.expires) || len(joins.m) >= joinMax {
				delete(joins.m, k)
			}
		}
	}
	joins.m[id] = joinEntry{html: html, expires: now.Add(joinTTL)}
	return id
}

// takeJoin returns and forgets an HTTP render kept under id.
func takeJoin(id string) (string, bool) {
	if id == "" {
		return "", false
	}
	joins.Lock()
	defer joins.Unlock()
	e, ok := joins.m[id]
	delete(joins.m, id)
	if !ok || time.Now().After(e.expires) {
		return "", false
	}
	return e.html, true
}
