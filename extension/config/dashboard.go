package config

import (
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
)

// ServerStatus is the wire shape served at GET
// /__nexus/config/server. One row per declared app + summary
// metadata about the source + last reload + subscriber count.
type ServerStatus struct {
	Listen      string         `json:"listen"`
	AuthMode    string         `json:"auth_mode"`
	SigningKID  string         `json:"signing_kid"`
	LastReload  string         `json:"last_reload,omitempty"`  // RFC3339; "" before first reload
	ReloadCount int            `json:"reload_count"`
	SubCount    int            `json:"subscriber_count"`
	Apps        []AppStatus    `json:"apps"`
}

// AppStatus is one row in the Apps table.
type AppStatus struct {
	Name     string   `json:"name"`
	Profiles []string `json:"profiles"`
}

// ClientStatus is the wire shape served at GET
// /__nexus/config/client. Surfaces the current snapshot's
// version + last refresh + server URL.
type ClientStatus struct {
	ServerURL      string `json:"server_url"`
	Identity       string `json:"identity"`
	Profile        string `json:"profile"`
	CurrentVersion string `json:"current_version"`
	LastRefresh    string `json:"last_refresh,omitempty"`
	CacheSealed    bool   `json:"cache_sealed"`
	OnUnreachable  string `json:"on_unreachable"`
}

// snapshotServerStatus is the dashboard projection. Cheap walk
// over serverState fields; safe to call from any goroutine.
func snapshotServerStatus(st *serverState) ServerStatus {
	st.mu.RLock()
	defer st.mu.RUnlock()

	rows := make([]AppStatus, 0, len(st.cfg.apps))
	for name, policy := range st.cfg.apps {
		rows = append(rows, AppStatus{Name: name, Profiles: policy.Profiles})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })

	var lastReloadStr string
	if t := st.lastReload.Load(); t != nil {
		lastReloadStr = t.Format(time.RFC3339)
	}
	return ServerStatus{
		Listen:      st.cfg.listen,
		AuthMode:    authModeName(st.cfg.authMode),
		SigningKID:  st.cfg.signingKID,
		LastReload:  lastReloadStr,
		ReloadCount: int(st.reloadCount.Load()),
		SubCount:    st.subs.count(),
		Apps:        rows,
	}
}

// snapshotClientStatus is the client-side projection. Captures
// the current snapshot version + last refresh moment + cache
// state.
func snapshotClientStatus(h *clientHolder) ClientStatus {
	version := ""
	if v := h.currentVersion.Load(); v != nil {
		version = *v
	}
	return ClientStatus{
		ServerURL:      h.cfg.serverURL,
		Identity:       h.cfg.identity,
		Profile:        h.cfg.profile,
		CurrentVersion: version,
		// LastRefresh is wired in phase 4 — a rolling-window
		// timestamp on the clientHolder. Phase-3 placeholder.
		LastRefresh:   "",
		CacheSealed:   len(h.sealKey) == keySize,
		OnUnreachable: unreachablePolicyName(h.cfg.onUnreachable),
	}
}

// handleServerStatus is the gin handler bound to
// /__nexus/config/server.
func handleServerStatus(st *serverState) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, snapshotServerStatus(st))
	}
}

// handleClientStatus is the gin handler bound to
// /__nexus/config/client.
func handleClientStatus(h *clientHolder) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, snapshotClientStatus(h))
	}
}

// authModeName turns the enum into the wire string. Keeps the
// dashboard payload human-readable + grep-friendly.
func authModeName(m AuthMode) string {
	switch m {
	case AuthMTLS:
		return "mtls"
	case AuthHMAC:
		return "hmac"
	case AuthNone:
		return "none"
	}
	return "unknown"
}

// unreachablePolicyName turns the enum into the wire string.
func unreachablePolicyName(p UnreachablePolicy) string {
	switch p {
	case UseCacheOrFail:
		return "use_cache_or_fail"
	case UseCacheAndWarn:
		return "use_cache_and_warn"
	case UseDefaults:
		return "use_defaults"
	}
	return "unknown"
}
