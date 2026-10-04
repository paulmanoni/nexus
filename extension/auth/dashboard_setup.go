package auth

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/paulmanoni/nexus/v2/httpx"
)

// The config-driven path's part of the dashboard's Auth tab: what is set up,
// and the throttle's current locks. Decoded by extension/dashboard without
// importing auth, so the field names are the contract.

type dashSetup struct {
	Default    string         `json:"default"`
	Cache      string         `json:"cache"`
	Schemes    []dashScheme   `json:"schemes"`
	Areas      []dashArea     `json:"areas"`
	Endpoints  []dashEndpoint `json:"endpoints"`
	Rules      []string       `json:"rules"`
	Locks      []dashLock     `json:"locks"`
	LocksKnown bool           `json:"locksKnown"` // false: the throttle store can't list
}

type dashScheme struct {
	Name, Type, Reads string
}

type dashArea struct {
	Name, Prefix, Login, Home string
	Kinds                     []string
}

type dashEndpoint struct {
	Method, Path, What string
}

type dashLock struct {
	Key, What string
	Until     time.Time
}

func (st *moduleState) dashboardSetup() *dashSetup {
	rs := st.config.settings
	if rs == nil {
		return nil
	}
	d := &dashSetup{Default: "signed-in", Cache: "off"}
	if rs.public {
		d.Default = "public"
	}
	if rs.cache > 0 {
		d.Cache = rs.cache.String()
	}
	for _, sc := range rs.schemes {
		reads := map[string]string{
			SchemeSession: "session cookie",
			SchemeBearer:  "Authorization: Bearer (opaque)",
			SchemeAPIKey:  "header " + sc.Header,
			SchemeJWT:     "Authorization: Bearer (JWT)",
		}[sc.Type]
		d.Schemes = append(d.Schemes, dashScheme{sc.name, sc.Type, reads})
	}
	for _, a := range rs.areas {
		d.Areas = append(d.Areas, dashArea{a.name, a.Prefix, a.Login, a.Home, a.Kinds})
	}
	ep := rs.endpoints
	for _, e := range []dashEndpoint{
		{"POST", ep.Login, "sign in"}, {"POST", ep.Logout, "sign out"}, {"GET", ep.Me, "me"},
		{"POST", ep.Token, "OAuth2 token"}, {"POST", ep.Revoke, "OAuth2 revoke"},
		{"POST+DELETE", rs.impersonation.Endpoint, "impersonate"},
	} {
		if e.Path != "" {
			d.Endpoints = append(d.Endpoints, e)
		}
	}
	d.Rules = []string{
		"sessions: single " + yesNo(rs.sessions.Single) + ", end on password change " + yesNo(rs.endOnPw) + ", idle " + orNever(rs.sessions.Idle),
		"throttle: account " + limitString(rs.throttle.account) + ", ip " + limitString(rs.throttle.ip) + ", lockout " + rs.throttle.lockout.String(),
		"impersonation needs " + rs.impersonation.Permission,
	}
	if th := st.config.throttle; th != nil {
		if mem, ok := th.store.(*memoryThrottle); ok {
			d.LocksKnown = true
			now := time.Now()
			mem.mu.Lock()
			for k, f := range mem.m {
				if now.Before(f.Locked) {
					d.Locks = append(d.Locks, dashLock{Key: k, What: lockWhat(k), Until: f.Locked})
				} else if strings.HasPrefix(k, "i:") && rs.throttle.ip.n > 0 && f.N >= rs.throttle.ip.n && now.Before(f.Since.Add(rs.throttle.ip.window)) {
					d.Locks = append(d.Locks, dashLock{Key: k, What: lockWhat(k), Until: f.Since.Add(rs.throttle.ip.window)})
				}
			}
			mem.mu.Unlock()
			sort.Slice(d.Locks, func(i, j int) bool { return d.Locks[i].Until.After(d.Locks[j].Until) })
		}
	}
	return d
}

func lockWhat(key string) string {
	switch {
	case strings.HasPrefix(key, "a:"):
		return "account " + strings.TrimPrefix(key, "a:")
	case strings.HasPrefix(key, "i:"):
		return "client IP " + strings.TrimPrefix(key, "i:")
	}
	return key
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// dashboardUnlockHandler clears a throttle lock: POST {"key": …}.
func dashboardUnlockHandler(st *moduleState) httpx.HandlerFunc {
	return func(c *httpx.Ctx) {
		var body struct {
			Key string `json:"key"`
		}
		if err := c.ShouldBindJSON(&body); err != nil || body.Key == "" {
			c.JSON(http.StatusBadRequest, httpx.H{"error": "key required"})
			return
		}
		th := st.config.throttle
		if th == nil {
			c.JSON(http.StatusBadRequest, httpx.H{"error": "auth is not set up yet"})
			return
		}
		if err := th.store.Delete(c.Request.Context(), body.Key); err != nil {
			c.JSON(http.StatusInternalServerError, httpx.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, httpx.H{"unlocked": body.Key})
	}
}

// dashboardRevokeUserHandler signs a user out everywhere: POST {"id": …}.
func dashboardRevokeUserHandler(st *moduleState) httpx.HandlerFunc {
	return func(c *httpx.Ctx) {
		var body struct {
			ID string `json:"id"`
		}
		if err := c.ShouldBindJSON(&body); err != nil || body.ID == "" {
			c.JSON(http.StatusBadRequest, httpx.H{"error": "id required"})
			return
		}
		if st.config.settings == nil {
			c.JSON(http.StatusBadRequest, httpx.H{"error": "auth is not set up yet"})
			return
		}
		if _, err := st.bumpEpoch(c.Request.Context(), body.ID); err != nil {
			c.JSON(http.StatusInternalServerError, httpx.H{"error": err.Error()})
			return
		}
		if st.config.loads != nil {
			st.config.loads.drop(body.ID)
		}
		c.JSON(http.StatusOK, httpx.H{"revoked": body.ID})
	}
}
