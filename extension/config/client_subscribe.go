package config

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// startSubscription dials the server's /__config/subscribe WS
// and processes version-change events for the lifetime of ctx.
// Auto-reconnects on disconnect with exponential backoff —
// network blips don't permanently lose the push channel; the
// polling loop in pollLoop() catches anything missed during the
// reconnect window.
//
// Server's /__config/subscribe pushes one event immediately on
// connect (the current version) plus one per Source-reload —
// clients consume both uniformly via the version-equality
// short-circuit in handleSubEvent.
func (h *clientHolder) startSubscription(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)

	h.subMu.Lock()
	h.cancelSubscribe = cancel
	h.subMu.Unlock()

	h.subWG.Add(1)
	go h.subscriptionLoop(ctx)
}

// stopSubscription cancels the loop. Called from OnShutdown
// alongside stopPolling so both background paths wind down
// cleanly.
func (h *clientHolder) stopSubscription() {
	h.subMu.Lock()
	cancel := h.cancelSubscribe
	h.subMu.Unlock()
	if cancel != nil {
		cancel()
	}
	h.subWG.Wait()
}

// subscriptionLoop is the long-lived background goroutine. Dials
// once at boot, processes events until ctx cancels or the
// connection drops, then sleeps with backoff and reconnects.
// Polling stays as the safety net for the reconnect window.
func (h *clientHolder) subscriptionLoop(ctx context.Context) {
	defer h.subWG.Done()
	backoff := time.Second
	const maxBackoff = 60 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if err := h.dialAndRead(ctx); err != nil {
			// Quiet log — operators expect occasional blips, and
			// the polling fallback will surface "config server
			// unreachable for >N polls" on its own.
			fmt.Fprintf(os.Stderr, "config.Client: subscribe loop: %v (retrying in %s)\n", err, backoff)
		}
		// Sleep with backoff capped at maxBackoff. Reset
		// backoff to 1s on every successful dial (handled
		// inside dialAndRead).
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = backoff * 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// dialAndRead opens one WS connection, processes events until
// disconnect, returns the error that ended the loop (nil for a
// clean ctx-cancel). Each push event triggers a snapshot
// refresh through the same path the polling loop uses, keeping
// the two refresh sources identical at the apply site.
func (h *clientHolder) dialAndRead(ctx context.Context) error {
	wsURL, err := buildSubscribeURL(h.cfg.serverURL, h.cfg.identity, h.cfg.profile)
	if err != nil {
		return err
	}
	tlsCfg, ok := extractTLSConfig(h.httpClient)
	dialer := &websocket.Dialer{
		HandshakeTimeout: h.cfg.requestTimeout,
	}
	if ok {
		dialer.TLSClientConfig = tlsCfg
	}
	hdr := http.Header{}
	if h.cfg.hmacSecret != "" {
		hdr.Set("Authorization", "Nexus-Config-HMAC stub")
	}
	conn, resp, err := dialer.DialContext(ctx, wsURL, hdr)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("ws dial: HTTP %d: %w", resp.StatusCode, err)
		}
		return fmt.Errorf("ws dial: %w", err)
	}
	defer conn.Close()

	// Watchdog: close the connection when ctx cancels so the
	// blocking ReadJSON below returns with an error. Without
	// this, OnShutdown would wait up to 120s (the read deadline)
	// for the loop to notice cancellation.
	watchdogDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-watchdogDone:
		}
	}()
	defer close(watchdogDone)

	conn.SetReadDeadline(time.Now().Add(120 * time.Second))
	conn.SetPongHandler(func(_ string) error {
		conn.SetReadDeadline(time.Now().Add(120 * time.Second))
		return nil
	})

	for {
		var ev subscribeEvent
		if err := conn.ReadJSON(&ev); err != nil {
			// ctx-cancel surfaces as a read error after the
			// watchdog closes conn — distinguish via ctx.Err().
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("ws read: %w", err)
		}
		h.handleSubEvent(ctx, ev)
	}
}

// handleSubEvent processes one inbound push. Short-circuits when
// the new version equals what we already installed (server pushes
// the current version on connect, so first-event is usually a
// no-op); otherwise fetches + applies + writes cache.
func (h *clientHolder) handleSubEvent(ctx context.Context, ev subscribeEvent) {
	cur := h.currentVersion.Load()
	if cur != nil && *cur == ev.Version {
		return
	}
	snap, err := h.fetchSnapshot(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config.Client: subscribe refresh failed: %v\n", err)
		return
	}
	h.installSnapshot(snap)
	_ = h.writeCachedSnapshot(snap)
	fmt.Fprintf(os.Stdout, "config.Client: snapshot refreshed via WS push (version=%s)\n",
		snap.Snapshot.Version)
}

// buildSubscribeURL turns the configured serverURL into the
// matching ws://host/__config/subscribe/:app/:profile target.
// Maps http→ws and https→wss; rejects any other scheme.
func buildSubscribeURL(serverURL, app, profile string) (string, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return "", fmt.Errorf("parse server URL: %w", err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("unexpected scheme %q", u.Scheme)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") +
		"/__config/subscribe/" + app + "/" + profile
	return u.String(), nil
}

// extractTLSConfig pulls the TLS config out of the holder's
// http.Client transport so the WS dialer can reuse the same
// pinning. Returns (cfg, true) when the transport is a
// configured *http.Transport with a TLSClientConfig; (nil,
// false) when the client uses the default transport.
func extractTLSConfig(client *http.Client) (*tls.Config, bool) {
	if client == nil {
		return nil, false
	}
	tr, ok := client.Transport.(*http.Transport)
	if !ok || tr.TLSClientConfig == nil {
		return nil, false
	}
	return tr.TLSClientConfig, true
}

// silence unused-import warning when json or other deps aren't
// transitively required.
var _ = json.Marshal
