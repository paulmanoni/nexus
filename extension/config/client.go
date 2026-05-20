package config

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/fx"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension"
)

// Client registers the server-backed config-client side. Fetches
// signed snapshots from the named config server, verifies
// against pinned signer keys, caches the result sealed on disk,
// installs into the root-package store that nexus.Get reads.
//
// Boot-time state machine:
//
//   1. Read sealed cache from disk (if present + key + valid sig).
//      → installs the cached snapshot immediately so handlers
//        boot with a usable value tree even if step 2 stalls.
//
//   2. Fetch fresh snapshot from server.
//      → success: verify, install (overlaying step 1's snapshot),
//        write sealed cache, schedule polling refresh loop.
//      → failure: apply OnUnreachable policy:
//          UseCacheOrFail   — if cache exists, use it; else fail boot
//          UseCacheAndWarn  — if cache exists, use it; else install
//                             empty store + SEV1 warning loop
//          UseDefaults      — if cache exists, use it; else install
//                             WithDefaults + SEV1 warning loop
//
// Polling loop runs every WithPollInterval (30s default), hitting
// /__config/version first; the full /__config/snapshot is only
// pulled when the version changed.
func Client(serverURL string, opts ...ClientOption) nexus.Option {
	cfg := defaultClientConfig(serverURL)
	for _, o := range opts {
		o.applyClient(&cfg)
	}
	if err := cfg.validate(); err != nil {
		return nexus.Raw(fx.Error(fmt.Errorf("config.Client: %w", err)))
	}
	holder := &clientHolder{cfg: cfg}

	// EAGER boot — fetch + install the snapshot synchronously,
	// before returning the Option. nexus.Get(...) becomes
	// callable from every Provide constructor, Invoke, handler,
	// and any code between Client(...) and Run(...). Without
	// this, an unlucky fx ordering can run user-side
	// constructors before the snapshot installs and Get returns
	// zero values silently.
	if err := holder.bootInstall(); err != nil {
		return nexus.Raw(fx.Error(fmt.Errorf("config.Client: %w", err)))
	}

	return extension.Use(extension.Plugin{
		Name:    "config",
		Version: "1",
		Options: []nexus.Option{
			nexus.Supply(holder),
		},
		Lifecycle: &extension.Lifecycle{
			OnBoot: func(ctx context.Context, _ *nexus.App) error {
				if err := holder.startPolling(ctx); err != nil {
					return err
				}
				holder.startSubscription(ctx)
				startClientDevWarningLoop(ctx, holder)
				return nil
			},
			OnShutdown: func(_ context.Context) error {
				holder.stopSubscription()
				holder.stopPolling()
				return nil
			},
		},
		Dashboard: &extension.Dashboard{
			Tab: &extension.Tab{
				ID:    "config-client",
				Label: "Config",
				Icon:  "settings",
			},
			Routes: []extension.Route{
				{Method: "GET", Path: "/client", Handler: gin.HandlerFunc(func(c *gin.Context) {
					handleClientStatus(holder)(c)
				})},
			},
		},
	})
}

// clientHolder captures the runtime state across fx.Invoke +
// OnBoot + OnShutdown. fx runs them sequentially so plain field
// access is race-free.
type clientHolder struct {
	cfg clientConfig

	pinnedKeys map[string]ed25519.PublicKey // KID → pubkey
	httpClient *http.Client
	sealKey    []byte // auto-managed; sibling .key file next to CachePath

	// Latest version we've installed. Polled refresh skips the
	// snapshot fetch when the server reports an unchanged
	// version.
	currentVersion atomic.Pointer[string]

	cancelPolling context.CancelFunc
	pollWG        sync.WaitGroup

	// Subscribe loop runs alongside polling. WS push delivers
	// sub-second refresh during steady-state; polling catches
	// anything missed during a WS reconnect window.
	subMu           sync.Mutex
	cancelSubscribe context.CancelFunc
	subWG           sync.WaitGroup
}

// initClient remains as a test-only entry point exercising the
// same boot-state-machine logic that production runs from
// Client(...) directly.
func initClient(h *clientHolder) error { return h.bootInstall() }

// bootInstall is the synchronous boot state machine. Runs from
// Client(...) so the snapshot is installed BEFORE fx.New/Start
// even begin — making nexus.Get callable from every constructor
// and invoke in the rest of the app. Fails (returns non-nil
// error) only when the OnUnreachable policy says to fail;
// otherwise installs the best-available snapshot and lets the
// polling loop (started in OnBoot) drive the rest.
func (h *clientHolder) bootInstall() error {
	if len(h.pinnedKeys) == 0 && len(h.cfg.signerKeyPaths) > 0 {
		if err := h.loadPinnedKeys(); err != nil {
			return fmt.Errorf("config.Client: load signer keys: %w", err)
		}
	}
	if h.httpClient == nil {
		if err := h.buildHTTPClient(); err != nil {
			return fmt.Errorf("config.Client: TLS: %w", err)
		}
	}
	// Cache seal key is only needed when a CachePath was given.
	// Dev mode skips both — SEV1 warning loop surfaces the
	// trade-off so an accidental ship-to-prod is loud.
	if len(h.sealKey) == 0 && h.cfg.cachePath != "" {
		if err := h.ensureSealKey(); err != nil {
			return fmt.Errorf("config.Client: seal key: %w", err)
		}
	}

	// Step 1: try the sealed cache. Best-effort — a missing or
	// corrupted cache is recoverable from the server fetch.
	if cached, ok, err := h.loadCachedSnapshot(); err != nil {
		fmt.Fprintf(os.Stderr, "config.Client: cached snapshot unreadable: %v (continuing)\n", err)
	} else if ok {
		h.installSnapshot(cached)
		fmt.Fprintf(os.Stdout, "config.Client: loaded cached snapshot (app=%s profile=%s version=%s)\n",
			cached.Snapshot.App, cached.Snapshot.Profile, cached.Snapshot.Version)
	}

	// Step 2: fetch fresh from server. Loud "connecting" line so
	// operators see exactly which URL the client is trying — the
	// silent-stall path was a frequent confusion when the server
	// was on a different port than expected.
	fmt.Fprintf(os.Stdout, "config.Client: connecting to %s (app=%s profile=%s)\n",
		h.cfg.serverURL, h.cfg.identity, h.cfg.profile)
	fresh, err := h.fetchSnapshot(context.Background())
	if err == nil {
		h.installSnapshot(fresh)
		_ = h.writeCachedSnapshot(fresh) // best-effort
		fmt.Fprintf(os.Stdout, "config.Client: snapshot installed (app=%s profile=%s version=%s)\n",
			fresh.Snapshot.App, fresh.Snapshot.Profile, fresh.Snapshot.Version)
		return nil
	}

	// Server unreachable. Apply OnUnreachable policy.
	if cur := h.currentVersion.Load(); cur != nil {
		// Cache already installed in step 1 — that's our floor.
		// Log + return; polling loop will retry.
		fmt.Fprintf(os.Stderr, "config.Client: server unreachable (%v); running on cached snapshot\n", err)
		return nil
	}
	switch h.cfg.onUnreachable {
	case UseCacheOrFail:
		return fmt.Errorf("config.Client: server unreachable AND no valid cache: %w", err)
	case UseCacheAndWarn:
		nexus.InstallConfigStore(map[string]any{}, "empty")
		fmt.Fprintf(os.Stderr, "config.Client: SEV1 — running on EMPTY config (server unreachable, no cache)\n")
		return nil
	case UseDefaults:
		nexus.InstallConfigStore(h.cfg.defaults, "defaults")
		fmt.Fprintf(os.Stderr, "config.Client: SEV1 — running on WithDefaults (server unreachable, no cache)\n")
		return nil
	}
	return fmt.Errorf("config.Client: unknown OnUnreachable policy %d", h.cfg.onUnreachable)
}

// installSnapshot installs the verified snapshot into the root
// store. On first call, uses InstallConfigStore (creates the
// store); subsequent calls use UpdateConfigStore (swaps + fires
// OnConfigChange callbacks).
func (h *clientHolder) installSnapshot(snap *SignedSnapshot) {
	if prev := h.currentVersion.Load(); prev == nil {
		nexus.InstallConfigStore(snap.Snapshot.Values, snap.Snapshot.Version)
	} else {
		nexus.UpdateConfigStore(snap.Snapshot.Values, snap.Snapshot.Version)
	}
	v := snap.Snapshot.Version
	h.currentVersion.Store(&v)
}

// loadPinnedKeys reads every signer key path from cfg and builds
// the KID → pubkey map Verify consults. KID defaults to the
// filename (without extension); operators wanting explicit
// rotation control can name files <kid>.pub.
func (h *clientHolder) loadPinnedKeys() error {
	h.pinnedKeys = map[string]ed25519.PublicKey{}
	for _, p := range h.cfg.signerKeyPaths {
		body, err := os.ReadFile(p) // #nosec G304 -- operator-supplied path
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		// Accept either raw 32-byte Ed25519 pubkey OR the form
		// the server's auto-gen emits (ed25519.PublicKey
		// serialized directly).
		if len(body) != ed25519.PublicKeySize {
			// Try trim whitespace (newline-terminated files).
			body = []byte(strings.TrimSpace(string(body)))
			if len(body) != ed25519.PublicKeySize {
				return fmt.Errorf("%s: expected %d-byte Ed25519 pubkey, got %d bytes",
					p, ed25519.PublicKeySize, len(body))
			}
		}
		kid := strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))
		h.pinnedKeys[kid] = ed25519.PublicKey(body)
	}
	return nil
}

// buildHTTPClient assembles the *http.Client. mTLS material
// optional — when cfg has clientCert+clientKey, we present them;
// otherwise the client just verifies the server cert against
// caCert.
func (h *clientHolder) buildHTTPClient() error {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS13}

	if h.cfg.caCert != "" {
		body, err := os.ReadFile(h.cfg.caCert) // #nosec G304
		if err != nil {
			return fmt.Errorf("read CA %s: %w", h.cfg.caCert, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(body) {
			return fmt.Errorf("CA %s: no PEM blocks parsed", h.cfg.caCert)
		}
		tlsCfg.RootCAs = pool
	}
	if h.cfg.clientCert != "" && h.cfg.clientKey != "" {
		cert, err := tls.LoadX509KeyPair(h.cfg.clientCert, h.cfg.clientKey)
		if err != nil {
			return fmt.Errorf("client keypair: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}

	h.httpClient = &http.Client{
		Transport: &http.Transport{TLSClientConfig: tlsCfg, ForceAttemptHTTP2: true},
		Timeout:   h.cfg.requestTimeout,
	}
	return nil
}

// ensureSealKey reads or creates the per-cache sealing key.
// Lives in <cachePath>.key with 0o600 perms; framework-managed,
// operator never touches it.
func (h *clientHolder) ensureSealKey() error {
	keyPath := h.cfg.cachePath + ".key"
	if _, err := os.Stat(keyPath); err == nil {
		k, err := LoadSealKey(keyPath)
		if err != nil {
			return err
		}
		h.sealKey = k
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	// First-boot: generate + write.
	k, err := GenerateKey()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		return fmt.Errorf("mkdir for seal key: %w", err)
	}
	if err := os.WriteFile(keyPath, k, 0o600); err != nil { // #nosec G306 -- seal key MUST be 0o600
		return fmt.Errorf("write %s: %w", keyPath, err)
	}
	h.sealKey = k
	return nil
}

// fetchSnapshot hits GET /__config/snapshot/:app/:profile and
// returns the verified SignedSnapshot.
func (h *clientHolder) fetchSnapshot(ctx context.Context) (*SignedSnapshot, error) {
	path := fmt.Sprintf("/__config/snapshot/%s/%s", h.cfg.identity, h.cfg.profile)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.cfg.serverURL+path, nil)
	if err != nil {
		return nil, err
	}
	if h.cfg.hmacSecret != "" {
		// Phase 1 stub — same construction as the server's
		// verifyConfigHMAC. Phase 2 elaborates.
		req.Header.Set("Authorization", "Nexus-Config-HMAC stub")
	}
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch snapshot: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Pull the body — the server's error message is what tells
		// the operator e.g. "app \"oats-admin\" not declared",
		// useless if we throw it away on the floor.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			return nil, fmt.Errorf("snapshot fetch: HTTP %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("snapshot fetch: HTTP %d — %s", resp.StatusCode, msg)
	}
	var signed SignedSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&signed); err != nil {
		return nil, fmt.Errorf("decode snapshot: %w", err)
	}
	if len(h.pinnedKeys) > 0 {
		if err := Verify(signed, h.pinnedKeys); err != nil {
			return nil, fmt.Errorf("verify snapshot: %w", err)
		}
	}
	if signed.Snapshot.App != h.cfg.identity {
		return nil, fmt.Errorf("snapshot app=%q, expected %q", signed.Snapshot.App, h.cfg.identity)
	}
	if signed.Snapshot.Profile != h.cfg.profile {
		return nil, fmt.Errorf("snapshot profile=%q, expected %q", signed.Snapshot.Profile, h.cfg.profile)
	}
	return &signed, nil
}

// fetchVersion is the cheap polling endpoint hit on every poll
// tick. Returns just the version string; comparison against
// currentVersion decides whether to pull the full snapshot.
func (h *clientHolder) fetchVersion(ctx context.Context) (string, error) {
	path := fmt.Sprintf("/__config/version/%s/%s", h.cfg.identity, h.cfg.profile)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.cfg.serverURL+path, nil)
	if err != nil {
		return "", err
	}
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", err
	}
	return v.Version, nil
}

// loadCachedSnapshot reads the sealed cache, decrypts, verifies
// the signature. Returns (snap, true, nil) on success, (nil,
// false, nil) when no cache exists, (nil, false, err) on
// tampering or corrupted bytes.
func (h *clientHolder) loadCachedSnapshot() (*SignedSnapshot, bool, error) {
	// Dev mode opts out of disk caching — bail before any IO so
	// tests + scratch runs don't litter the working dir.
	if h.cfg.cachePath == "" {
		return nil, false, nil
	}
	body, err := os.ReadFile(h.cfg.cachePath) // #nosec G304 -- operator-supplied path
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	plaintext, err := Unseal(body, h.sealKey)
	if err != nil {
		return nil, false, fmt.Errorf("unseal cache: %w", err)
	}
	var signed SignedSnapshot
	if err := json.Unmarshal(plaintext, &signed); err != nil {
		return nil, false, fmt.Errorf("decode cached snapshot: %w", err)
	}
	if len(h.pinnedKeys) > 0 {
		if err := Verify(signed, h.pinnedKeys); err != nil {
			return nil, false, fmt.Errorf("verify cached snapshot: %w", err)
		}
	}
	return &signed, true, nil
}

// writeCachedSnapshot serializes + seals + writes atomically.
// Best-effort — failures here log but don't fail the boot path
// (a fetched-and-installed snapshot is already in memory).
func (h *clientHolder) writeCachedSnapshot(snap *SignedSnapshot) error {
	if h.cfg.cachePath == "" {
		return nil
	}
	plaintext, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	sealed, err := Seal(plaintext, h.sealKey)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(h.cfg.cachePath), 0o700); err != nil {
		return err
	}
	return atomicWrite(h.cfg.cachePath, sealed, 0o600)
}

// startPolling kicks off the version-poll loop. Returns
// immediately; the loop runs in a goroutine until the cancel
// func fires.
func (h *clientHolder) startPolling(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	h.cancelPolling = cancel
	h.pollWG.Add(1)
	go h.pollLoop(ctx)
	return nil
}

func (h *clientHolder) stopPolling() {
	if h.cancelPolling != nil {
		h.cancelPolling()
	}
	h.pollWG.Wait()
}

// pollLoop wakes up every pollInterval, asks the server for the
// current version, refetches the full snapshot when it changed.
// Continues across server outages — a failed poll just logs and
// retries on the next tick.
func (h *clientHolder) pollLoop(ctx context.Context) {
	defer h.pollWG.Done()
	t := time.NewTicker(h.cfg.pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			version, err := h.fetchVersion(ctx)
			if err != nil {
				// Quiet — operators expect occasional blips,
				// don't want to drown logs in noise.
				continue
			}
			cur := h.currentVersion.Load()
			if cur != nil && *cur == version {
				continue
			}
			// Version changed — pull the full snapshot.
			snap, err := h.fetchSnapshot(ctx)
			if err != nil {
				fmt.Fprintf(os.Stderr, "config.Client: poll refresh failed: %v\n", err)
				continue
			}
			h.installSnapshot(snap)
			_ = h.writeCachedSnapshot(snap)
			fmt.Fprintf(os.Stdout, "config.Client: snapshot refreshed via poll (version=%s)\n",
				snap.Snapshot.Version)
		}
	}
}

// startClientDevWarningLoop fires a SEV1 log line every 60s while
// the client is running in dev mode (NEXUS_CONFIG_DEV=1).
// Mirrors the server's warning loop — operators see it loud in
// journalctl / container logs so an accidental ship-to-prod with
// missing SignerKey or CachePath is obvious instead of silent.
func startClientDevWarningLoop(ctx context.Context, h *clientHolder) {
	if os.Getenv(devEnv) != "1" {
		return
	}
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				var degraded []string
				if len(h.pinnedKeys) == 0 {
					degraded = append(degraded, "signature verification OFF")
				}
				if h.cfg.cachePath == "" {
					degraded = append(degraded, "cache OFF (offline-boot disabled)")
				}
				if len(degraded) == 0 {
					return
				}
				fmt.Fprintf(os.Stderr,
					"config.Client: SEV1 — running with NEXUS_CONFIG_DEV=1 (%s); harden before shipping to prod\n",
					strings.Join(degraded, ", "))
			}
		}
	}()
}

// silence unused-import warning while phase-1 boot wiring is
// scaffolded.
var _ = errors.New
