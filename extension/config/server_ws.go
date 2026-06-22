package config

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// subscribeUpgrader is the gorilla/websocket Upgrader for the
// /__config/subscribe endpoint. Permissive CheckOrigin — the
// config server lives behind mTLS/HMAC auth, and Origin is
// browser-only anyway (the actual subscribers are server-to-
// server Go clients).
var subscribeUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(_ *http.Request) bool { return true },
}

// subscription is one live WS subscriber. Each carries the
// (app, profile) it subscribed to + the send-channel the
// fan-out loop writes version-change events to.
type subscription struct {
	app, profile string
	send         chan subscribeEvent
}

// subscribeEvent is the wire shape pushed to a subscriber when
// the source reloads. Just the new version stamp + KID; clients
// re-fetch the full snapshot via GET on receipt so the WS path
// stays light (one int + one short string per push).
type subscribeEvent struct {
	Version string `json:"version"`
	KID     string `json:"kid"`
}

// subscribers is the server-side broadcaster. One instance lives
// on serverState; handleSubscribe registers; reload() notifies.
type subscribers struct {
	mu    sync.RWMutex
	conns map[*subscription]struct{}
}

func newSubscribers() *subscribers {
	return &subscribers{conns: map[*subscription]struct{}{}}
}

// add registers a fresh subscription. The send channel is
// buffered so a slow consumer doesn't block the broadcast
// goroutine; full channels drop events (the polling fallback
// in clients catches missed pushes).
// add registers a fresh subscription for (app, profile). The app/profile are
// set before the subscription becomes visible in conns, so fanout (which reads
// them under RLock) never races a concurrent field write.
func (s *subscribers) add(app, profile string) *subscription {
	sub := &subscription{app: app, profile: profile, send: make(chan subscribeEvent, 4)}
	s.mu.Lock()
	s.conns[sub] = struct{}{}
	s.mu.Unlock()
	return sub
}

func (s *subscribers) remove(sub *subscription) {
	s.mu.Lock()
	delete(s.conns, sub)
	s.mu.Unlock()
	close(sub.send)
}

// fanout pushes ev to every subscriber whose (app, profile)
// matches. Drops events for any subscriber whose send channel
// is full — the framework prefers "miss a push" over "block the
// broadcaster" because clients re-converge via the polling
// fallback within one poll interval.
func (s *subscribers) fanout(app, profile string, ev subscribeEvent) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for sub := range s.conns {
		if sub.app != app || sub.profile != profile {
			continue
		}
		select {
		case sub.send <- ev:
		default:
			// Channel full — drop. Polling catches it.
		}
	}
}

// count is the dashboard projection — total subscribers across
// all (app, profile) pairs.
func (s *subscribers) count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.conns)
}

// handleSubscribe upgrades the request to a WebSocket and pushes
// version-change events for the requested (app, profile). The
// connection stays open until either side disconnects; the
// fan-out goroutine is the only writer.
func (st *serverState) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	profile := r.PathValue("profile")
	if err := st.authorize(r, app); err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	// Verify (app, profile) is actually serveable before
	// upgrading — saves the WS handshake for a request that
	// would always fail.
	if _, err := st.snapshotFor(app, profile); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	conn, err := subscribeUpgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrader writes its own error response.
		return
	}
	defer conn.Close()

	sub := st.subs.add(app, profile)
	defer st.subs.remove(sub)

	// Push the current version immediately so a freshly-
	// connected client doesn't need to wait for the next
	// source-reload to learn what's live.
	current, err := st.snapshotFor(app, profile)
	if err == nil {
		_ = conn.WriteJSON(subscribeEvent{
			Version: current.Snapshot.Version,
			KID:     current.KID,
		})
	}

	// Reader loop — surfaces client disconnect via the channel
	// close from gorilla/websocket. Anything the client sends
	// is currently ignored (the WS is one-way, server→client).
	disconnect := make(chan struct{})
	go func() {
		defer close(disconnect)
		for {
			if _, _, err := conn.NextReader(); err != nil {
				return
			}
		}
	}()

	// Writer loop — flush every subscribeEvent + close cleanly
	// on disconnect or shutdown.
	pingT := time.NewTicker(30 * time.Second)
	defer pingT.Stop()
	for {
		select {
		case <-disconnect:
			return
		case ev, ok := <-sub.send:
			if !ok {
				return
			}
			conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := conn.WriteJSON(ev); err != nil {
				return
			}
		case <-pingT.C:
			conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := conn.WriteControl(websocket.PingMessage, nil,
				time.Now().Add(5*time.Second)); err != nil {
				return
			}
		}
	}
}

// notifyReload is called by serverState.reload() after the new
// content lands. Pushes an event to every subscriber whose
// (app, profile) is affected by the change — for the local
// source that's "every subscriber" since one file change can
// touch _common (everyone), an app (one consumer per profile),
// etc. The cheap version-equality check on the client side
// short-circuits no-op pushes.
func (st *serverState) notifyReload() {
	st.mu.RLock()
	defer st.mu.RUnlock()
	for app := range st.cfg.apps {
		for _, profile := range st.cfg.apps[app].Profiles {
			snap, err := st.snapshotForLocked(app, profile)
			if err != nil {
				continue
			}
			st.subs.fanout(app, profile, subscribeEvent{
				Version: snap.Snapshot.Version,
				KID:     snap.KID,
			})
		}
	}
}

// snapshotForLocked is the read-lock-held variant of
// snapshotFor — called from notifyReload which already holds
// st.mu.RLock(). Lifted into a separate method so the
// double-locking is explicit, not a bug.
func (st *serverState) snapshotForLocked(app, profile string) (*SignedSnapshot, error) {
	// Without the cache we can't avoid re-resolving here — but
	// the previous reload() flushed the signed map, so the
	// cache-hit path is exactly what we want anyway.
	key := app + "/" + profile
	if ss, ok := st.signed[key]; ok {
		return ss, nil
	}
	// Fall back to unlocked path; safe because the caller
	// holds RLock (snapshotFor takes RLock on values, RWLock
	// to install — neither conflicts here).
	return nil, fmt.Errorf("not yet served")
}
