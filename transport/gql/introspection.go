package gql

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/graphql-go/graphql"
	"github.com/paulmanoni/nexus/httpx"

	graph "github.com/paulmanoni/nexus/graph"
	"github.com/paulmanoni/nexus/internal/maskhook"
)

// productionGate runs the full nexus/graph security suite (the
// same rule set go-graph applies when DEBUG: false) on every
// request whose peer is NOT in the introspection allowlist:
//
//   - max query depth = 10
//   - max aliases     = 4
//   - max complexity  = 200
//   - no __schema / __type introspection
//
// Returns 404 (not 400) on any violation — the gate is meant to be
// indistinguishable from "no such route" to anonymous scanners,
// matching the dashboard gate's pattern.
//
// allow == nil makes the function a pass-through — back-compat for
// callers that don't wire WithAllowIntrospection. When allow is
// set and returns true (peer is on the allowlist or
// Config.Introspection is on), validation is skipped — dev/admin
// retains the loose dev-mode experience including __schema lookup.
func productionGate(allow func(c *httpx.Ctx) bool, schema *graphql.Schema) httpx.HandlerFunc {
	if allow == nil || schema == nil {
		return func(c *httpx.Ctx) { c.Next() }
	}
	verdicts := &gateVerdicts{m: make(map[string]error)}
	return func(c *httpx.Ctx) {
		// Allowed peer (introspection unlocked) — skip validation
		// entirely. Matches go-graph's DEBUG: true semantics.
		if allow(c) {
			c.Next()
			return
		}
		query := extractQuery(c)
		if query == "" {
			c.Next()
			return
		}
		if err := verdicts.check(query, schema); err != nil {
			// One uniform 404 across all rule violations — depth,
			// aliases, complexity, introspection. A more granular
			// surface (400 + detail for legit user errors) would
			// help a benign caller debug, but the gate is meant to
			// stay opaque.
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		c.Next()
	}
}

// gateVerdicts memoizes the gate's validate outcome per query
// string. The rules are pure functions of the query text, so a
// repeat query — the overwhelmingly common case for an app with a
// fixed set of frontend queries — skips the parse and the four AST
// walks that used to run on EVERY request in front of the document
// cache. Bounded: at capacity the map is flushed rather than
// LRU-tracked; an attacker sending unique queries just degrades to
// the uncached (pre-existing) cost.
type gateVerdicts struct {
	mu sync.Mutex
	m  map[string]error
}

const gateVerdictCap = 1024

func (g *gateVerdicts) check(query string, schema *graphql.Schema) error {
	g.mu.Lock()
	if err, ok := g.m[query]; ok {
		g.mu.Unlock()
		return err
	}
	g.mu.Unlock()
	err := graph.ValidateQueryString(query, schema)
	g.mu.Lock()
	if len(g.m) >= gateVerdictCap {
		g.m = make(map[string]error)
	}
	g.m[query] = err
	g.mu.Unlock()
	return err
}

// gateParsedKey stashes the fully-bound request when the gate already
// decoded the body, so cachedHandler skips its own read + decode of
// the same bytes — the gate previously cost every locked-down request
// a second full body read and JSON parse.
const gateParsedKey = "gql.gate.request"

// extractQuery pulls the GraphQL query string out of the request,
// matching simpleHandler / goGraphHandler's parsing rules. Reads
// + restores the body so downstream handlers that DO re-read (the
// non-JSON fallbacks) see the original bytes.
//
// For JSON POSTs — the hot path — it binds the FULL request struct
// exactly as httpx.ShouldBindJSON would (same maskid unmask rewrite,
// same Decode semantics) and stashes it for cachedHandler, so the
// body is read and parsed once per request instead of twice.
//
// Returns "" when the query can't be located — the gate falls
// through to the regular handler in that case, which surfaces the
// parse error in its own response shape.
func extractQuery(c *httpx.Ctx) string {
	if c.Request.Method == http.MethodGet {
		return c.Query("query")
	}
	if c.Request.Body == nil {
		return ""
	}
	body, err := io.ReadAll(c.Request.Body)
	_ = c.Request.Body.Close()
	if err != nil {
		c.Request.Body = io.NopCloser(bytes.NewReader(nil))
		return ""
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	if isJSONContentType(c.GetHeader("Content-Type")) {
		dec := body
		if maskhook.Enabled() {
			dec = maskhook.UnmaskJSON(body)
		}
		var req request
		if json.NewDecoder(bytes.NewReader(dec)).Decode(&req) == nil {
			c.Set(gateParsedKey, &req)
			return req.Query
		}
		return ""
	}
	var probe struct {
		Query string `json:"query"`
	}
	if json.Unmarshal(body, &probe) == nil {
		return probe.Query
	}
	return ""
}

// hasIntrospectionToken kept exported-package-private for the
// introspection-only-fast-path tests. Substring on "__schema" /
// "__type" is reliable because the GraphQL spec reserves the "__"
// prefix for introspection — user types/fields cannot start with
// it. Used as a unit-test sanity check; the runtime gate goes
// through full AST validation via productionGate.
func hasIntrospectionToken(s string) bool {
	if s == "" {
		return false
	}
	return strings.Contains(s, "__schema") || strings.Contains(s, "__type")
}
