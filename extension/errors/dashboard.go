package errors

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// handleRecent answers GET /__nexus/errors/recent. Returns the
// ring buffer in newest-first order. Capped by Config.Capacity; the
// dashboard typically pages through this via the "Recent errors"
// feed in the trace rail.
func (s *pluginState) handleRecent(c *gin.Context) {
	s.mu.RLock()
	store := s.store
	s.mu.RUnlock()
	if store == nil {
		c.JSON(http.StatusOK, gin.H{"errors": []Event{}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"errors": store.recentSnapshot()})
}

// handleIssues answers GET /__nexus/errors/issues. Returns one row
// per fingerprint with count + first/last seen + sample event.
// Equivalent to Sentry's issues list — what operators glance at to
// triage the noisiest failure modes.
func (s *pluginState) handleIssues(c *gin.Context) {
	s.mu.RLock()
	store := s.store
	s.mu.RUnlock()
	if store == nil {
		c.JSON(http.StatusOK, gin.H{"issues": []*Issue{}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"issues": store.issueSnapshot()})
}

// handleIssue answers GET /__nexus/errors/issue/:fingerprint. Single
// issue with its latest sample event (stack trace + request meta).
func (s *pluginState) handleIssue(c *gin.Context) {
	fp := c.Param("fingerprint")
	s.mu.RLock()
	store := s.store
	s.mu.RUnlock()
	if store == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "store not initialized"})
		return
	}
	issue, ok := store.issueByFingerprint(fp)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "no issue with that fingerprint"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"issue": issue})
}

// handleStatus answers GET /__nexus/errors/status. Plugin-level
// summary: configuration knobs + transport names + counts. Designed
// to fit on the dashboard's plugin chip card.
func (s *pluginState) handleStatus(c *gin.Context) {
	s.mu.RLock()
	cfg := s.cfg
	store := s.store
	started := s.started
	s.mu.RUnlock()

	transports := make([]string, 0, len(cfg.Transports))
	for _, t := range cfg.Transports {
		transports = append(transports, t.Name())
	}
	var recent, issues int
	if store != nil {
		recent, issues = store.stats()
	}
	c.JSON(http.StatusOK, gin.H{
		"started":     started,
		"disabled":    cfg.Disabled,
		"environment": cfg.Environment,
		"release":     cfg.Release,
		"serverName":  cfg.ServerName,
		"capacity":    cfg.Capacity,
		"sampleRate":  cfg.SampleRate,
		"ignorePaths": cfg.IgnorePaths,
		"transports":  transports,
		"recentCount": recent,
		"issueCount":  issues,
	})
}

// handleClear answers POST /__nexus/errors/clear. Wipes the ring
// buffer + issue index. Operators use it after fixing a flood to
// reset the dashboard's view; tests use it to isolate cases.
//
// Returns 204 with no body — matches REST conventions for "command
// accepted, no response payload".
func (s *pluginState) handleClear(c *gin.Context) {
	s.mu.RLock()
	store := s.store
	s.mu.RUnlock()
	if store != nil {
		store.clear()
	}
	c.Status(http.StatusNoContent)
}
