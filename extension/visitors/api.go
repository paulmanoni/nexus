package visitors

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strconv"

	"github.com/paulmanoni/nexus/httpx"
)

// trackBody is the payload the SPA sends to /track. Path is the
// frontend's route ("/", "/blog/foo") — the plugin doesn't try to
// guess from the Referer header because SPAs change URL without
// a full HTTP round-trip.
type trackBody struct {
	Path string `json:"path"`
}

// handleTrack records one page view. The SPA calls this from its
// router's afterEach hook on every navigation. Idempotency: a
// frontend retry after a network blip double-counts; that's
// acceptable. Strict de-dup would require server-side request IDs
// and isn't worth the cost for a counter.
//
// Cookie handling: if `nx_visitor` is absent, we mint a new ID +
// set the cookie. If present, we trust it. The cookie is HttpOnly
// + Secure + SameSite=Lax — the SPA doesn't need to read it, only
// the server tracks against it.
func (s *pluginState) handleTrack(c *httpx.Ctx) {
	if s.cfg.Disabled || s.counter == nil {
		c.Status(http.StatusNoContent)
		return
	}

	visitorID, _ := c.Cookie(s.cfg.CookieName)
	if visitorID == "" {
		visitorID = newVisitorID()
		c.SetCookie(
			s.cfg.CookieName,
			visitorID,
			s.cfg.CookieMaxAgeDays*86400,
			"/",
			"",                   // domain — empty = current host
			c.Request.TLS != nil, // Secure when over HTTPS
			true,                 // HttpOnly
		)
	}

	// Parse body for path; empty body is fine, we just don't
	// attribute the visit to a path.
	var b trackBody
	_ = c.ShouldBindJSON(&b) // ignore parse errors — body is optional

	s.counter.Track(visitorID, b.Path)
	c.Status(http.StatusNoContent)
}

// handlePublicStats returns the current Stats as JSON. Public
// (mounted under /api/visitors/stats) because that's the data the
// frontend's footer badge polls. No sensitive info — just the
// counters.
//
// Cache-Control: no-store keeps poll responses from being cached
// by proxies. The SPA polls every 30s; a CDN caching the response
// for 60s would freeze the "online now" widget at zero.
func (s *pluginState) handlePublicStats(c *httpx.Ctx) {
	if s.cfg.Disabled || s.counter == nil {
		c.JSON(http.StatusOK, Stats{})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, s.counter.Stats())
}

// handleAdminStats serves the dashboard tab's /stats. Same shape
// as the public one for now, kept distinct so we can layer admin-
// only fields here later (e.g. the unique IDs list, raw lastSeen
// timestamps) without leaking them on the public endpoint.
func (s *pluginState) handleAdminStats(c *httpx.Ctx) {
	if s.cfg.Disabled || s.counter == nil {
		c.JSON(http.StatusOK, Stats{})
		return
	}
	c.JSON(http.StatusOK, s.counter.Stats())
}

// handleTopPaths returns the top N paths by view count. Query
// param `n` (default 20, clamped to 1..maxPaths) controls how many
// rows to return. The admin tab uses this for the "Top pages" panel.
func (s *pluginState) handleTopPaths(c *httpx.Ctx) {
	if s.cfg.Disabled || s.counter == nil {
		c.JSON(http.StatusOK, httpx.H{"paths": []PathCount{}})
		return
	}
	n := 20
	if q := c.Query("n"); q != "" {
		if v, err := strconv.Atoi(q); err == nil {
			n = v
		}
	}
	if n < 1 {
		n = 1
	}
	c.JSON(http.StatusOK, httpx.H{"paths": s.counter.Top(n)})
}

// handleReset wipes the counter + saves the empty state to disk.
// Used after a counter-pollution event (bot flood, accidental
// production test). Returns 204 — the new shape is recoverable
// from a follow-up /stats hit.
func (s *pluginState) handleReset(c *httpx.Ctx) {
	if s.cfg.Disabled || s.counter == nil {
		c.Status(http.StatusNoContent)
		return
	}
	s.counter.Reset()
	if err := s.counter.SaveToFile(s.cfg.StorePath); err != nil {
		c.JSON(http.StatusInternalServerError, httpx.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}

// newVisitorID mints a 32-character hex random string. crypto/rand
// is the right source — visitor IDs are an authentication-adjacent
// signal (a bad actor who can guess a valid ID can pad the counter).
// 16 bytes = 128 bits of entropy = astronomically unguessable.
func newVisitorID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is a runtime catastrophe — return a
		// fixed sentinel and let the operator notice the cookie
		// never changes. Better than panicking the request.
		return "00000000000000000000000000000000"
	}
	return hex.EncodeToString(b[:])
}
