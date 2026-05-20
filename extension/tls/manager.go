package tls

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"github.com/paulmanoni/nexus"
)

// LetsEncryptStagingURL is Let's Encrypt's staging directory. Picked
// up when Config.Staging is true. Note: staging issues real certs
// from a non-trusted CA, so browsers will show warnings — that's the
// point; it lets you exercise the renewal path without spending
// production quota.
const LetsEncryptStagingURL = "https://acme-staging-v02.api.letsencrypt.org/directory"

// boot runs as the OnBoot lifecycle hook — before any listener binds.
// Reads the effective manifest (already resolved by the framework's
// own OnStart hook upstream of ours), merges its tls: block with the
// in-code Config, validates, and stores the resolved Config on state
// for start() to pick up at OnReady.
//
// Returning a non-nil error aborts boot, which is the right behavior
// for a TLS misconfiguration: the operator should fix the manifest
// rather than discover the cert never issued because validation was
// silently skipped.
func (s *pluginState) boot(ctx context.Context, app *nexus.App) error {
	mf := readManifest(app)
	cfg, disabled, err := resolveConfig(s.inCodeCfg, mf)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.cfg = cfg
	s.disabled = disabled
	s.mu.Unlock()
	if disabled {
		log.Printf("nexus tls: disabled by manifest for this environment — skipping listeners")
	}
	return nil
}

// start spins up the two http.Servers the plugin owns: the TLS
// server on Config.HTTPSPort using autocert's TLSConfig + the app as
// handler, and the redirect / challenge server on Config.HTTPPort.
//
// Called as the OnReady lifecycle hook so it runs after the
// framework's main listener bound. If boot() set Disabled (the
// manifest opted this env out), start() returns immediately without
// binding anything. Other failures are LOGGED rather than returned,
// because returning an error from OnReady tears the whole app down —
// the dashboard's /__nexus/tls/status surfaces the failure instead.
func (s *pluginState) start(ctx context.Context, app *nexus.App) error {
	s.mu.RLock()
	disabled := s.disabled
	s.mu.RUnlock()
	if disabled {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started {
		// Idempotent: OnReady can in theory fire twice across
		// graceful restarts; honor it without re-binding.
		return nil
	}

	m := &autocert.Manager{
		Prompt:     s.cfg.AcceptTOS,
		Cache:      s.cfg.Cache,
		HostPolicy: autocert.HostWhitelist(s.cfg.Domains...),
		Email:      s.cfg.Email,
	}
	if s.cfg.Staging {
		m.Client = &acme.Client{DirectoryURL: LetsEncryptStagingURL}
	}
	s.manager = m

	// HTTPS server — uses app as handler. The framework's *App
	// implements http.Handler via its embedded gin.Engine, so this
	// is the same handler graph the internal :8080 server serves —
	// just exposed over TLS on :443.
	tlsCfg := m.TLSConfig()
	// MinVersion is the only place the stdlib's default is too
	// permissive for 2026; bump to 1.2 so we don't accept TLS 1.0.
	tlsCfg.MinVersion = tls.VersionTLS12

	httpsSrv := &http.Server{
		Addr:      fmt.Sprintf(":%d", s.cfg.HTTPSPort),
		Handler:   app,
		TLSConfig: tlsCfg,
		// Conservative timeouts. Without them, a slow-loris peer
		// can hold the connection forever, eating fds.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	s.httpsSrv = httpsSrv

	// HTTP server — must bind :80 for ACME HTTP-01 challenges to
	// reach the manager. We layer the redirect-to-HTTPS behavior
	// underneath: m.HTTPHandler answers challenge paths itself and
	// delegates everything else to our handler. If Redirect is
	// false, we serve a 421 Misdirected Request for non-challenge
	// requests — the rare case where an upstream proxy already
	// handles HTTP and we ONLY want :80 for challenges.
	var underlay http.Handler
	if s.cfg.Redirect != nil && *s.cfg.Redirect {
		underlay = http.HandlerFunc(redirectToHTTPS)
	} else {
		underlay = http.HandlerFunc(misdirectedHandler)
	}
	httpSrv := &http.Server{
		Addr:              fmt.Sprintf(":%d", s.cfg.HTTPPort),
		Handler:           m.HTTPHandler(underlay),
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.httpSrv = httpSrv

	// Launch both servers. Errors are non-fatal (logged); the
	// inner-product app keeps serving on its internal port.
	go func() {
		log.Printf("nexus tls: HTTPS listener on :%d (domains: %v)\n", s.cfg.HTTPSPort, s.cfg.Domains)
		if err := httpsSrv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("nexus tls: HTTPS listener exited: %v", err)
			s.mu.Lock()
			s.startErr = err
			s.mu.Unlock()
		}
	}()
	go func() {
		log.Printf("nexus tls: HTTP listener on :%d (ACME challenges + redirect)\n", s.cfg.HTTPPort)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("nexus tls: HTTP listener exited: %v", err)
		}
	}()

	s.started = true
	return nil
}

// stop performs a graceful shutdown of both servers, honoring the
// OnShutdown context's deadline. Errors from the second server are
// reported only if the first succeeded; the goal is "stop everything
// you can, surface the first failure" rather than logging both.
func (s *pluginState) stop(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.started {
		return nil
	}

	var firstErr error
	if s.httpsSrv != nil {
		if err := s.httpsSrv.Shutdown(ctx); err != nil {
			firstErr = err
		}
	}
	if s.httpSrv != nil {
		if err := s.httpSrv.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	s.started = false
	return firstErr
}

// redirectToHTTPS 301s every request to the HTTPS equivalent of its
// URL. Bound under m.HTTPHandler so ACME challenges (which use a
// well-known path under /.well-known/acme-challenge/) are handled by
// the manager BEFORE this code runs — autocert layers itself on top.
//
// The host portion of the redirect is the request's own Host header,
// which is operator-controlled in principle but attacker-controlled
// in practice (TLS terminates on whatever Host the client sent). We
// validate the parsed host looks like a real DNS name before echoing
// it back into a Location header — a malformed value (CRLF, scheme
// injection, path delimiters) is rejected as 400. Together with
// autocert.HostPolicy (the recommended upstream gate) this leaves no
// open-redirect surface: requests for unknown hosts never reach this
// handler, and well-formed hosts can only redirect to themselves.
func redirectToHTTPS(w http.ResponseWriter, r *http.Request) {
	host := stripPort(r.Host)
	if !isValidRedirectHost(host) {
		http.Error(w, "invalid host", http.StatusBadRequest)
		return
	}
	target := "https://" + host + r.URL.RequestURI()
	// #nosec G710 -- host validated above; r.URL.RequestURI() is parsed by net/http
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}

// isValidRedirectHost reports whether host is shaped like a DNS name
// or a bracketed IPv6 literal — i.e. safe to splice back into a
// Location header without enabling open-redirect / response-splitting.
// Permissive on character classes (allows underscores, which appear
// in service-mesh ingress names) but strict on delimiters that would
// let an attacker break out of the host segment.
func isValidRedirectHost(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	// Bracketed IPv6 — accept the bracketed body verbatim; the parser
	// upstream already rejected unbalanced brackets in stripPort.
	if host[0] == '[' && host[len(host)-1] == ']' {
		for i := 1; i < len(host)-1; i++ {
			c := host[i]
			if !(c == ':' || c == '.' ||
				('0' <= c && c <= '9') ||
				('a' <= c && c <= 'f') ||
				('A' <= c && c <= 'F')) {
				return false
			}
		}
		return true
	}
	for i := 0; i < len(host); i++ {
		c := host[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c == '.' || c == '-' || c == '_':
		default:
			return false
		}
	}
	return true
}

// misdirectedHandler answers non-challenge :80 requests with 421 when
// Redirect is disabled. 421 signals to clients (and to upstream
// proxies that misroute) that they should not have hit this listener.
func misdirectedHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusMisdirectedRequest)
	_, _ = w.Write([]byte("nexus tls: HTTPS only — please use https://\n"))
}

// stripPort removes the ":port" suffix from a Host header so we can
// rebuild it as https://<host><uri>. r.Host may carry :80, an IPv6
// bracketed form, or neither; this handles all three.
func stripPort(host string) string {
	// Bracketed IPv6: keep everything up to and including the
	// closing bracket; drop ":port" after it.
	if len(host) > 0 && host[0] == '[' {
		if end := indexByte(host, ']'); end > -1 {
			return host[:end+1]
		}
		return host
	}
	if idx := indexByte(host, ':'); idx > -1 {
		return host[:idx]
	}
	return host
}

// indexByte is strings.IndexByte inlined — avoids an import just for
// one call. Keeps the file's dependency footprint to "what the
// servers actually need."
func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// CertInfo is the dashboard's view of one issued certificate. Built
// on demand from the autocert cache + a parse of the leaf certificate
// to recover the not-before / not-after dates. Stored ad-hoc rather
// than tracked in memory because autocert is the source of truth.
type CertInfo struct {
	Domain    string    `json:"domain"`
	Issuer    string    `json:"issuer,omitempty"`
	NotBefore time.Time `json:"notBefore,omitempty"`
	NotAfter  time.Time `json:"notAfter,omitempty"`
	DaysLeft  int       `json:"daysLeft,omitempty"`
	Present   bool      `json:"present"`
	Error     string    `json:"error,omitempty"`
}

// snapshotCerts returns one CertInfo per configured domain. Missing
// certs return Present=false so the dashboard can render "not yet
// issued" rather than dropping the row.
func (s *pluginState) snapshotCerts(ctx context.Context) []CertInfo {
	s.mu.RLock()
	cfg := s.cfg
	m := s.manager
	s.mu.RUnlock()

	out := make([]CertInfo, 0, len(cfg.Domains))
	var wg sync.WaitGroup
	results := make([]CertInfo, len(cfg.Domains))

	for i, domain := range cfg.Domains {
		wg.Add(1)
		go func(i int, domain string) {
			defer wg.Done()
			results[i] = inspectCert(ctx, m, cfg.Cache, domain)
		}(i, domain)
	}
	wg.Wait()
	out = append(out, results...)
	return out
}

// inspectCert peeks at the leaf cert for a domain without triggering
// issuance. Reads directly from the cache so the dashboard load
// doesn't bump autocert into a network round-trip per call.
func inspectCert(ctx context.Context, m *autocert.Manager, cache autocert.Cache, domain string) CertInfo {
	info := CertInfo{Domain: domain}
	if m == nil {
		info.Error = "manager not started yet"
		return info
	}
	raw, err := cache.Get(ctx, domain)
	if err != nil {
		if errors.Is(err, autocert.ErrCacheMiss) {
			return info // Present=false, no error — "not yet issued"
		}
		info.Error = err.Error()
		return info
	}
	// autocert stores PEM-encoded "fullchain + privkey" in one blob.
	// We only want the first certificate (the leaf) to extract
	// metadata.
	cert, err := parseFirstCert(raw)
	if err != nil {
		info.Error = "cache contents not parseable: " + err.Error()
		return info
	}
	info.Present = true
	info.Issuer = cert.Issuer.CommonName
	info.NotBefore = cert.NotBefore
	info.NotAfter = cert.NotAfter
	info.DaysLeft = int(time.Until(cert.NotAfter).Hours() / 24)
	return info
}

// parseFirstCert extracts the first PEM-encoded CERTIFICATE block
// from an autocert cache entry. The entry contains the leaf, the
// chain, and the private key — we only want the leaf, which is the
// first CERTIFICATE block.
func parseFirstCert(raw []byte) (*x509.Certificate, error) {
	rest := raw
	for {
		block, next := pem.Decode(rest)
		if block == nil {
			return nil, errors.New("no CERTIFICATE block in cache entry")
		}
		if block.Type == "CERTIFICATE" {
			return x509.ParseCertificate(block.Bytes)
		}
		rest = next
	}
}
