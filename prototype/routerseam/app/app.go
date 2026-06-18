// Package app is a stand-in for nexus's framework + handler code. Every line
// here is written ONCE against httpx and runs unchanged on gin, chi, or
// stdlib. It exercises the gin features the earlier analysis flagged as the
// hard parts of a swap:
//
//   - middleware flow control: Next / Abort / AbortWithStatusJSON
//   - the recovery pattern (defer recover(); c.Next()) catching a downstream panic
//   - post-Next error reading (c.Errors), as metrics/trace middleware do
//   - path params (":id"), query, JSON bind, custom status, NoRoute (SPA)
//
// None of it names gin, chi, or net/http's mux.
package app

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"routerseam/httpx"
)

// Register wires the same endpoints + middleware onto any backend router.
func Register(r httpx.Router) {
	chain := func(h httpx.HandlerFunc) []httpx.HandlerFunc {
		// trace/metrics-style + recovery + auth, mirroring buildEndpointChain.
		return []httpx.HandlerFunc{recovery, metrics, requireToken, h}
	}

	r.Handle("GET", "/pets/:id", chain(getPet)...)
	r.Handle("POST", "/pets", chain(createPet)...)
	r.Handle("GET", "/boom", chain(boom)...)

	// SPA fallback — gin's NoRoute, here neutral.
	r.NoRoute(func(c *httpx.Ctx) {
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("<!doctype html>INDEX"))
	})
}

// --- middleware (each a plain HandlerFunc; ports 1:1 from gin.HandlerFunc) ---

// recovery is the defer-recover-then-Next pattern from app_recovery.go. Because
// httpx.Ctx owns Next(), this works identically on every backend.
func recovery(c *httpx.Ctx) {
	defer func() {
		if v := recover(); v != nil {
			c.Error(fmt.Errorf("panic: %v", v))
			if !c.Written() {
				c.AbortWithStatusJSON(http.StatusInternalServerError, map[string]any{"error": "internal"})
			}
		}
	}()
	c.Next()
}

// metrics brackets the handler and reads accumulated errors afterward, exactly
// like extension/metrics/middleware.go reads c.Errors post-Next.
func metrics(c *httpx.Ctx) {
	c.Next()
	status := c.W.Status()
	if errs := c.Errors(); len(errs) > 0 {
		log.Printf("[metrics] %s %s -> %d (errs=%d)", c.Method(), c.FullPath(), status, len(errs))
	}
}

// requireToken is an auth-style gate: short-circuit via AbortWithStatusJSON,
// the CORS/introspection-gate pattern.
func requireToken(c *httpx.Ctx) {
	if c.Header("X-Token") != "secret" {
		c.AbortWithStatusJSON(http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	c.SetHeader("X-Authed", "1")
	c.Next()
}

// --- handlers ----------------------------------------------------------------

func getPet(c *httpx.Ctx) {
	c.JSON(http.StatusOK, map[string]any{"id": c.Param("id"), "q": c.Query("q")})
}

type createReq struct {
	Name string `json:"name"`
}

func createPet(c *httpx.Ctx) {
	var in createReq
	if err := json.NewDecoder(c.Request().Body).Decode(&in); err != nil {
		c.JSON(http.StatusBadRequest, map[string]any{"error": "bad json"})
		return
	}
	c.JSON(http.StatusCreated, map[string]any{"created": in.Name})
}

func boom(c *httpx.Ctx) { panic("kaboom") }
