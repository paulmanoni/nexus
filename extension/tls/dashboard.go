package tls

import (
	"context"
	cryptotls "crypto/tls"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// handleListCerts answers GET /__nexus/tls/certs. Returns one row
// per configured domain — Present:false for domains that haven't
// been issued yet (e.g. first boot, no traffic yet to trigger the
// challenge). The dashboard UI renders an "issuing…" state for those.
//
// Short deadline (3s) is intentional: this is a dashboard call,
// the user is waiting. If the cache is wedged we'd rather error
// than hang the request — operators can hit /__nexus/tls/status
// for the underlying details.
func (s *pluginState) handleListCerts(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	certs := s.snapshotCerts(ctx)
	c.JSON(http.StatusOK, gin.H{"certs": certs})
}

// handleRenew answers POST /__nexus/tls/renew/:domain. Forces a
// fresh issuance by deleting the cached cert and prodding the
// manager with a synthetic ClientHelloInfo for the domain. autocert
// then runs the ACME flow as if the cert had never been issued.
//
// Authorization: this is mounted under /__nexus/, which the
// framework's listenerScope filters guard. In a self-hosted setup
// the operator is expected to put the dashboard on a private
// listener; in a cloud setup the platform brokers access.
func (s *pluginState) handleRenew(c *gin.Context) {
	domain := c.Param("domain")
	if domain == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "domain is required"})
		return
	}

	s.mu.RLock()
	m := s.manager
	cache := s.cfg.Cache
	allowed := containsDomain(s.cfg.Domains, domain)
	s.mu.RUnlock()

	if !allowed {
		// Refuse to renew a domain we wouldn't issue for anyway —
		// surfaces typo'd domains immediately rather than burning
		// an ACME round-trip.
		c.JSON(http.StatusBadRequest, gin.H{
			"error":  "domain not in whitelist",
			"domain": domain,
		})
		return
	}
	if m == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "tls manager not started"})
		return
	}

	// Delete-then-fetch: removing the cached cert forces the next
	// GetCertificate call to run the full ACME flow.
	if err := cache.Delete(c.Request.Context(), domain); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":  "cache delete failed",
			"detail": err.Error(),
		})
		return
	}

	// Trigger fresh issuance. autocert.Manager.GetCertificate reads
	// ServerName from the ClientHelloInfo and runs the ACME flow
	// when the cache is empty (which we just made it). Use a
	// generous timeout — Let's Encrypt staging is usually <5s,
	// production occasionally pauses on rate-limit checks.
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	hello := &cryptotls.ClientHelloInfo{ServerName: domain}
	// Issuance happens off the hot path; do it in a goroutine bound
	// to ctx so the timeout aborts a stuck call without blocking
	// the handler.
	done := make(chan error, 1)
	go func() {
		_, err := m.GetCertificate(hello)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{
				"error":  "issuance failed",
				"domain": domain,
				"detail": err.Error(),
			})
			return
		}
	case <-ctx.Done():
		c.JSON(http.StatusGatewayTimeout, gin.H{
			"error":  "issuance timed out",
			"domain": domain,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"ok":     true,
		"domain": domain,
		"info":   inspectCert(ctx, m, cache, domain),
	})
}

// handleStatus answers GET /__nexus/tls/status. Returns a small
// summary suitable for an oncall glance: are we running, how many
// certs are present, when's the next renewal due. Cheaper than
// /certs because it doesn't parse every cache entry.
func (s *pluginState) handleStatus(c *gin.Context) {
	s.mu.RLock()
	started := s.started
	startErr := s.startErr
	domains := append([]string(nil), s.cfg.Domains...)
	staging := s.cfg.Staging
	httpsPort := s.cfg.HTTPSPort
	httpPort := s.cfg.HTTPPort
	redirect := s.cfg.Redirect != nil && *s.cfg.Redirect
	s.mu.RUnlock()

	payload := gin.H{
		"started":   started,
		"staging":   staging,
		"domains":   domains,
		"httpsPort": httpsPort,
		"httpPort":  httpPort,
		"redirect":  redirect,
	}
	if startErr != nil {
		payload["error"] = startErr.Error()
	}
	c.JSON(http.StatusOK, payload)
}

// containsDomain reports whether a domain is in the configured
// whitelist. Case-insensitive — DNS is case-insensitive and operators
// frequently type the prod URL in mixed case during testing.
func containsDomain(whitelist []string, want string) bool {
	for _, d := range whitelist {
		if equalFoldASCII(d, want) {
			return true
		}
	}
	return false
}

// equalFoldASCII is a small ASCII-only fold so we don't pull in
// strings.EqualFold (which handles Unicode and we don't need it for
// DNS names — IDN punycode is already ASCII before it hits here).
func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca := a[i]
		cb := b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
