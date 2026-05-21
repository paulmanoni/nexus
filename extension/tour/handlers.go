package tour

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// handlers binds the Store to gin route handlers. Created once
// during plugin registration; every handler is a closure over the
// same instance, so live updates to the Store (Phase 3 may swap
// it via the dashboard) propagate automatically.
type handlers struct{ store Store }

// newID mints a 32-char hex identifier. Cheap, opaque, collision-
// free at the scale the tour plugin sees (tens of tours, hundreds
// of steps). The UUID-style 36-char dashed form is left to
// callers who care about wire compatibility.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is unrecoverable; the caller has
		// no useful fallback so we don't pretend otherwise.
		panic("tour: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// listTours — GET /__nexus/tour/tours?route=...
func (h *handlers) listTours(c *gin.Context) {
	route := strings.TrimSpace(c.Query("route"))
	tours, err := h.store.ListTours(c.Request.Context(), route)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"tours": tours})
}

// getTour — GET /__nexus/tour/tours/:id (returns the hydrated tree)
func (h *handlers) getTour(c *gin.Context) {
	t, err := h.store.GetTour(c.Request.Context(), c.Param("id"))
	if err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, t)
}

// upsertTour — POST /__nexus/tour/tours
//
// Accepts the full hydrated tree in one shot. Mints IDs for any
// row missing one (new tour + new steps recorded in-session both
// arrive without IDs). The badge_number defaults to the row's
// 1-based position so operators don't have to manage it manually.
func (h *handlers) upsertTour(c *gin.Context) {
	var body Tour
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	newTour := body.ID == ""
	if newTour {
		body.ID = newID()
	}
	assignIDsAndBadges(body.Steps, 1)
	// For brand-new tours, slot them at the end of their route's
	// play order. Existing tours keep their Order untouched on
	// upsert — operators reorder via the explicit reorderTours
	// endpoint, not by re-saving with a different order.
	if newTour {
		existing, _ := h.store.ListTours(c.Request.Context(), body.Route)
		body.Order = len(existing)
	}
	if err := h.store.UpsertTour(c.Request.Context(), &body); err != nil {
		writeStoreError(c, err)
		return
	}
	// Re-fetch so the response carries the canonical ordering +
	// timestamps the store wrote.
	saved, err := h.store.GetTour(c.Request.Context(), body.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, saved)
}

// deleteTour — DELETE /__nexus/tour/tours/:id
func (h *handlers) deleteTour(c *gin.Context) {
	if err := h.store.DeleteTour(c.Request.Context(), c.Param("id")); err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// upsertStep — POST /__nexus/tour/steps
//
// Single-step incremental write. The in-page recorder calls this
// every time it captures a click rather than rewriting the whole
// tour, which keeps the recorded sequence stable even if the
// session is interrupted.
func (h *handlers) upsertStep(c *gin.Context) {
	var s Step
	if err := c.ShouldBindJSON(&s); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if s.TourID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "tour_id is required"})
		return
	}
	if s.ID == "" {
		s.ID = newID()
	}
	if err := h.store.UpsertStep(c.Request.Context(), &s); err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, &s)
}

// deleteStep — DELETE /__nexus/tour/steps/:id
func (h *handlers) deleteStep(c *gin.Context) {
	if err := h.store.DeleteStep(c.Request.Context(), c.Param("id")); err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// reorderSteps — POST /__nexus/tour/tours/:id/reorder
type reorderBody struct {
	Items []ReorderItem `json:"items"`
}

func (h *handlers) reorderSteps(c *gin.Context) {
	var body reorderBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.store.ReorderSteps(c.Request.Context(), c.Param("id"), body.Items); err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// reorderTours — POST /__nexus/tour/tours/reorder
//
// Body: { "ids": ["a", "b", "c"] } in the new ordering. The
// store rewrites each tour's Order to its 0-based position in
// the slice. Used by the dashboard's ↑/↓ buttons on the tour
// list when a route filter is active.
type reorderToursBody struct {
	IDs []string `json:"ids"`
}

func (h *handlers) reorderTours(c *gin.Context) {
	var body reorderToursBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.store.ReorderTours(c.Request.Context(), body.IDs); err != nil {
		writeStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// activeForRoute — GET /__nexus/tour/active?route=...
//
// The in-page agent hits this on every navigation to learn whether
// a tour exists for the current URL. Returns the FULL hydrated tree
// so the runner can render without a second round trip.
func (h *handlers) activeForRoute(c *gin.Context) {
	route := strings.TrimSpace(c.Query("route"))
	if route == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "route is required"})
		return
	}
	tours, err := h.store.ListTours(c.Request.Context(), route)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// Hydrate each match — list view dropped the steps for size,
	// but the agent needs them to render.
	out := make([]*Tour, 0, len(tours))
	for _, t := range tours {
		full, err := h.store.GetTour(c.Request.Context(), t.ID)
		if err != nil {
			continue
		}
		out = append(out, full)
	}
	c.JSON(http.StatusOK, gin.H{"tours": out})
}

// writeStoreError maps Store errors to HTTP status codes. ErrNotFound
// is the only typed signal we expose; everything else becomes 500.
func writeStoreError(c *gin.Context, err error) {
	if errors.Is(err, ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
}

// assignIDsAndBadges walks the tree assigning random IDs to any
// step missing one, and 1-based badge numbers in DFS-walk order.
// Existing IDs are preserved (the editor preserves identity
// across edits); existing BadgeNumbers are NOT preserved — the
// numbering is regenerated on every save so renumbers stay in
// sync with structural edits.
func assignIDsAndBadges(steps []*Step, start int) int {
	n := start
	for _, s := range steps {
		if s.ID == "" {
			s.ID = newID()
		}
		s.BadgeNumber = n
		n++
		if len(s.Children) > 0 {
			n = assignIDsAndBadges(s.Children, n)
		}
	}
	return n
}