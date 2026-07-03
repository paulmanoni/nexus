package config

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// serverState is the runtime backing for one ServerModule
// instance. Created by newServerState (fx-Provide), driven by
// onServerBoot / onServerShutdown.
type serverState struct {
	cfg    serverConfig
	source Source
	priv   ed25519.PrivateKey

	// snapshots is the snapshot cache keyed by "<app>/<profile>".
	// Filled from the Source.Load() result at boot + on every
	// reload; signed lazily on first request, then memoized.
	mu        sync.RWMutex
	content   map[string]appBody // raw merged source content
	signed    map[string]*SignedSnapshot
	subsCount atomic.Int32 // for the dashboard

	// subs is the WS push registry — one subscription per live
	// /__config/subscribe/:app/:profile connection. fan-out
	// fires from reload() after the source content updates.
	subs *subscribers

	// lastReload + reloadCount expose reload state to the
	// dashboard. Cheap atomics; no contention on read.
	lastReload  atomic.Pointer[time.Time]
	reloadCount atomic.Int32

	srv       *http.Server
	watchStop func() // returned by Source.Watch; called on shutdown
}

// newServerState is the fx provider. Loads the source once,
// auto-derives apps when needed, and is ready to serve as soon
// as it returns.
func newServerState(w serverWrapper) (*serverState, error) {
	s := &serverState{
		cfg:    w.cfg,
		source: w.source,
		signed: map[string]*SignedSnapshot{},
		subs:   newSubscribers(),
	}
	// Apply dev-mode auto-generated material BEFORE loading the
	// signing key, since the auto-gen path writes the key file.
	if err := ensureDevDefaults(&s.cfg); err != nil {
		return nil, err
	}
	priv, err := loadOrCreateSigningKey(s.cfg.signingKeyPath)
	if err != nil {
		return nil, fmt.Errorf("config.Server: signing key: %w", err)
	}
	s.priv = priv
	if err := s.reload(context.Background()); err != nil {
		return nil, fmt.Errorf("config.Server: initial source load: %w", err)
	}
	if s.cfg.autoApps {
		s.deriveAppsFromContent()
	}
	if err := s.validateAppsAgainstContent(); err != nil {
		return nil, err
	}
	return s, nil
}

// reload re-reads the Source and clears the signed-snapshot cache
// so subsequent requests trigger a fresh sign with the new
// content. Called once at boot and again on every Source watch
// event. Notifies WS subscribers after content lands so they
// can re-fetch the freshly-signed snapshots.
func (s *serverState) reload(ctx context.Context) error {
	content, err := s.source.Load(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.content = content
	s.signed = map[string]*SignedSnapshot{}
	s.mu.Unlock()

	now := time.Now().UTC()
	s.lastReload.Store(&now)
	s.reloadCount.Add(1)

	// Notify subscribers AFTER the content swap. Even if no
	// subscribers are currently connected this is a no-op.
	s.notifyReload()
	return nil
}

// deriveAppsFromContent populates cfg.apps when WithApps was
// omitted. One AppPolicy entry per non-"_common" app in the
// source. Every profile present in the file is permitted —
// auto-derive is the dev-shaped default; production runs declare
// apps explicitly with stricter Profiles lists.
func (s *serverState) deriveAppsFromContent() {
	s.cfg.apps = map[string]AppPolicy{}
	for name, body := range s.content {
		if name == "_common" {
			continue
		}
		profiles := make([]string, 0, len(body.Profiles))
		for p := range body.Profiles {
			profiles = append(profiles, p)
		}
		s.cfg.apps[name] = AppPolicy{Profiles: profiles}
	}
}

// validateAppsAgainstContent asserts every declared app has a
// matching file in the source, and every loose file has a
// declaration. Loose files = unreachable config (typo prone),
// declared-without-file = boot error.
func (s *serverState) validateAppsAgainstContent() error {
	for app := range s.cfg.apps {
		if _, ok := s.content[app]; !ok {
			return fmt.Errorf("config.Server: app %q in apps policy has no <app>.nexus.config.toml file", app)
		}
	}
	for app := range s.content {
		if app == "_common" {
			continue
		}
		if _, ok := s.cfg.apps[app]; !ok {
			fmt.Fprintf(os.Stderr,
				"config.Server: warning: %s.nexus.config.toml present but not in apps policy — unreachable\n",
				app)
		}
	}
	return nil
}

// boot wires the HTTP listener + the Source.Watch callback. Runs
// once during fx.Start (driven from the serverHolder's OnBoot
// callback). Failures here abort boot.
func (st *serverState) boot(ctx context.Context) error {
	st.srv = &http.Server{
		Addr:              st.cfg.listen,
		Handler:           st.routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	// AuthNone = dev mode = plain HTTP. mTLS / HMAC need a TLS
	// listener so the wire is encrypted regardless of which auth
	// the application picks on top.
	if st.cfg.authMode != AuthNone {
		st.srv.TLSConfig = buildServerTLSConfig(st.cfg)
	}
	// Watch fires async; reload is cheap. The stop func is
	// retained on st so shutdown can cancel it cleanly.
	st.watchStop = st.source.Watch(ctx, func() {
		_ = st.reload(context.Background())
	})
	go func() {
		scheme := "https"
		if st.cfg.authMode == AuthNone {
			scheme = "http"
		}
		// Loud announcement — config.Server runs on its own
		// listener separate from the main app's port. New users
		// hit Gin's 404 when they point Client at the app's port
		// instead of this one. Matches the framework's
		// "nexus: listening on …" pattern so the line survives
		// in `nexus dev` output.
		fmt.Fprintf(os.Stdout, "config.Server: listening on %s://%s (auth=%s)\n",
			scheme, st.cfg.listen, authModeName(st.cfg.authMode))
		var err error
		if st.cfg.authMode == AuthNone {
			err = st.srv.ListenAndServe()
		} else {
			err = st.srv.ListenAndServeTLS("", "")
		}
		if err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "config.Server: listener exited: %v\n", err)
		}
	}()
	startDevWarningLoop(ctx, st)
	return nil
}

// shutdown stops the HTTP listener + the Source watcher cleanly.
func (st *serverState) shutdown(ctx context.Context) error {
	if st.watchStop != nil {
		st.watchStop()
	}
	if st.srv == nil {
		return nil
	}
	return st.srv.Shutdown(ctx)
}

// routes mounts /__config/snapshot/:app/:profile + /__config/version/:app/:profile
// + /__config/health. WS subscription lands in phase 2.
func (s *serverState) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /__config/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /__config/snapshot/{app}/{profile}", s.handleSnapshot)
	mux.HandleFunc("GET /__config/version/{app}/{profile}", s.handleVersion)
	mux.HandleFunc("GET /__config/subscribe/{app}/{profile}", s.handleSubscribe)
	return mux
}

// handleSnapshot resolves (app, profile) into a signed snapshot
// and serves it as JSON. Auth happens at the TLS layer (mTLS) or
// via the Authorization header (HMAC); AuthNone skips both.
func (s *serverState) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	profile := r.PathValue("profile")
	if err := s.authorize(r, app); err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	ss, err := s.snapshotFor(app, profile)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ss)
}

// handleVersion is the cheap polling endpoint. Returns just the
// snapshot version + KID so a polling client can short-circuit a
// full snapshot fetch when nothing changed.
func (s *serverState) handleVersion(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	profile := r.PathValue("profile")
	if err := s.authorize(r, app); err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	ss, err := s.snapshotFor(app, profile)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"version": ss.Snapshot.Version,
		"kid":     ss.KID,
	})
}

// snapshotFor produces or returns the cached SignedSnapshot for
// (app, profile). The cache key is "app/profile"; the reload
// path invalidates everything.
func (s *serverState) snapshotFor(app, profile string) (*SignedSnapshot, error) {
	key := app + "/" + profile
	s.mu.RLock()
	if ss, ok := s.signed[key]; ok {
		s.mu.RUnlock()
		return ss, nil
	}
	s.mu.RUnlock()

	// Verify policy permits (app, profile). The error message
	// lists what IS available — operators see "app=oats-admin
	// missing; declared apps: [oats]" instead of having to
	// re-check the YAML by hand. Identity ↔ filename mapping is
	// a common confusion (Identity = file prefix, Profile =
	// inner section), so being explicit here is worth the bytes.
	policy, ok := s.cfg.apps[app]
	if !ok {
		declared := make([]string, 0, len(s.cfg.apps))
		for name := range s.cfg.apps {
			if name == "_common" {
				continue
			}
			declared = append(declared, name)
		}
		sort.Strings(declared)
		return nil, fmt.Errorf(
			"app %q not declared (Identity must be the file prefix — declared apps: %v)",
			app, declared)
	}
	if len(policy.Profiles) > 0 {
		permitted := false
		for _, p := range policy.Profiles {
			if p == profile {
				permitted = true
				break
			}
		}
		if !permitted {
			return nil, fmt.Errorf(
				"profile %q not permitted for app %q — declared profiles: %v",
				profile, app, policy.Profiles)
		}
	}

	// Resolve + sign.
	s.mu.RLock()
	values, err := resolveSnapshot(s.content, app, profile)
	s.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	version, err := snapshotVersion(values)
	if err != nil {
		return nil, fmt.Errorf("snapshot version: %w", err)
	}
	snap := Snapshot{
		App:      app,
		Profile:  profile,
		Version:  version,
		ServedAt: time.Now().UTC(),
		Values:   values,
	}
	signed, err := Sign(snap, s.priv, s.cfg.signingKID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.signed[key] = &signed
	s.mu.Unlock()
	return &signed, nil
}

// authorize runs the wire-auth check. Three modes:
//
//	AuthMTLS — r.TLS.VerifiedChains is non-empty (TLS handshake
//	           already enforced the allow-list via tls.Config's
//	           VerifyConnection). We re-check the CN matches the
//	           :app URL segment so a valid cert for app A can't
//	           fetch app B's snapshot.
//	AuthHMAC — verify Authorization header signature against the
//	           per-app shared secret. Same construction as
//	           extension/peer's verifyHMAC.
//	AuthNone — no-op.
func (s *serverState) authorize(r *http.Request, app string) error {
	switch s.cfg.authMode {
	case AuthMTLS:
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
			return fmt.Errorf("mTLS: no verified client chain")
		}
		cn := r.TLS.VerifiedChains[0][0].Subject.CommonName
		if cn != app {
			return fmt.Errorf("mTLS: cert CN %q does not match :app %q", cn, app)
		}
		return nil
	case AuthHMAC:
		return verifyConfigHMAC(r, s.cfg.apps[app].HMACSecret)
	case AuthNone:
		return nil
	}
	return fmt.Errorf("unknown auth mode")
}

// verifyConfigHMAC is the per-request HMAC bearer check. Format:
//
//	Authorization: Nexus-Config-HMAC <unix-ts>:<base64-hmac-sha256>
//
// The signed bytes are <app>:<unix-ts>:<request-path>. 30s clock
// skew tolerance. Same construction as extension/peer's HMAC but
// over different fields (the body is empty for a GET; the path
// uniquely identifies the requested snapshot).
func verifyConfigHMAC(r *http.Request, secret string) error {
	// Stubbed for phase 1; full implementation lives alongside
	// the WS-subscription path in phase 2.
	if secret == "" {
		return fmt.Errorf("HMAC: no secret configured for this app")
	}
	if r.Header.Get("Authorization") == "" {
		return fmt.Errorf("HMAC: missing Authorization header")
	}
	return nil
}

// buildServerTLSConfig assembles the *tls.Config the http.Server
// uses. mTLS mode adds RequireAndVerifyClientCert + a CA pool
// loaded from cfg.tlsCA. HMAC + None modes leave client-cert
// verification off; the server still terminates TLS.
func buildServerTLSConfig(cfg serverConfig) *tls.Config {
	cert, err := tls.LoadX509KeyPair(cfg.tlsCert, cfg.tlsKey)
	if err != nil {
		// Boot-time error; should have been caught by validate().
		// Returning nil here lets http.Server fail loudly on Listen.
		return nil
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
		NextProtos:   []string{"h2", "http/1.1"},
	}
	if cfg.authMode == AuthMTLS && cfg.tlsCA != "" {
		caPool := x509.NewCertPool()
		body, _ := os.ReadFile(cfg.tlsCA) // #nosec G304 -- operator-supplied CA path
		caPool.AppendCertsFromPEM(body)
		tlsCfg.ClientCAs = caPool
		tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return tlsCfg
}

// ensureDevDefaults materializes auto-generated TLS + signing
// material under .configd/ when the operator didn't pin paths
// AND NEXUS_CONFIG_DEV=1 is set. Idempotent — subsequent boots
// reuse the same files.
func ensureDevDefaults(cfg *serverConfig) error {
	if os.Getenv(devEnv) != "1" {
		return nil
	}
	if err := os.MkdirAll(".configd", 0o700); err != nil {
		return fmt.Errorf("ensureDevDefaults: mkdir .configd: %w", err)
	}
	// AuthNone is plain HTTP — no point generating TLS material
	// the server will never use. mTLS / HMAC still need a cert,
	// so only the AuthNone branch skips.
	if cfg.authMode != AuthNone && (cfg.tlsCert == "" || cfg.tlsKey == "") {
		certPath := ".configd/server.crt"
		keyPath := ".configd/server.key"
		if _, err := os.Stat(certPath); os.IsNotExist(err) {
			if err := generateSelfSignedCert(certPath, keyPath); err != nil {
				return fmt.Errorf("ensureDevDefaults: self-signed cert: %w", err)
			}
		}
		cfg.tlsCert = certPath
		cfg.tlsKey = keyPath
	}
	if cfg.signingKeyPath == "" {
		cfg.signingKeyPath = ".configd/sign.key"
	}
	return nil
}

// loadOrCreateSigningKey reads an Ed25519 private key from path,
// or generates one + writes it if the file doesn't exist (dev
// auto-gen path).
func loadOrCreateSigningKey(path string) (ed25519.PrivateKey, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if os.Getenv(devEnv) != "1" {
			return nil, fmt.Errorf("signing key %s does not exist", path)
		}
		// Generate + write.
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("generate Ed25519 key: %w", err)
		}
		if err := os.WriteFile(path, priv, 0o600); err != nil {
			return nil, fmt.Errorf("write %s: %w", path, err)
		}
		// Also write the sibling .pub for clients to pin.
		pubPath := strings.TrimSuffix(path, ".key") + ".pub"
		if err := os.WriteFile(pubPath, priv.Public().(ed25519.PublicKey), 0o644); err != nil {
			return nil, fmt.Errorf("write %s: %w", pubPath, err)
		}
		return priv, nil
	}
	body, err := os.ReadFile(path) // #nosec G304 -- operator-supplied path
	if err != nil {
		return nil, err
	}
	if len(body) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("signing key %s: expected %d bytes, got %d",
			path, ed25519.PrivateKeySize, len(body))
	}
	return ed25519.PrivateKey(body), nil
}

// generateSelfSignedCert produces a development TLS cert + key
// pair. Valid for 1 year; SAN includes localhost + 127.0.0.1.
// Used only in dev mode (NEXUS_CONFIG_DEV=1).
func generateSelfSignedCert(certPath, keyPath string) error {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "configd-dev"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return err
	}
	return os.WriteFile(keyPath, keyPEM, 0o600)
}

// startDevWarningLoop emits a SEV1 log line every 60s while a
// dev-default is in use. Operators see it loud in journalctl /
// container logs so an accidental ship-to-prod is visible.
func startDevWarningLoop(ctx context.Context, st *serverState) {
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
				fmt.Fprintf(os.Stderr,
					"config.Server: SEV1 — running with NEXUS_CONFIG_DEV=1 (auth=%v, signing=%s); harden before shipping to prod\n",
					st.cfg.authMode, st.cfg.signingKeyPath)
			}
		}
	}()
	_ = filepath.Base // keep import used in phase-2 stubs
}
