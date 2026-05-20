package nexus

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/paulmanoni/nexus/extension/dashboard"
)

// healthState tracks two signals every production deployment needs:
//
//   - liveness ("alive"): toggles true after fx Start completes and
//     false on Stop. Drives /__nexus/health, used by k8s/lb liveness
//     probes — "is the process up at all?"
//
//   - readiness ("ready"): alive AND every declared peer is reachable.
//     Drives /__nexus/ready, used by k8s readiness probes / load
//     balancers to gate traffic — "is this replica ready to serve?"
//
// The peer-readiness check is what makes split deployments honest. A
// monolith with no peers is ready as soon as it's alive; a split unit
// is only ready when its hard dependencies (declared in Topology) are
// also up. That keeps requests from reaching a pod whose downstream
// peer is still booting.
type healthState struct {
	mu    sync.RWMutex
	alive bool
	peers map[string]peerHealth // peer tag → last probe result
}

// peerHealth is the per-peer record updated by the prober.
type peerHealth struct {
	Ready      bool      `json:"ready"`
	LastError  string    `json:"lastError,omitempty"`
	LastProbed time.Time `json:"lastProbed,omitempty"`
}

func newHealthState() *healthState {
	return &healthState{peers: map[string]peerHealth{}}
}

func (h *healthState) setAlive(v bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.alive = v
}

func (h *healthState) isAlive() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.alive
}

// snapshot returns the current liveness flag and a copy of the
// per-peer table. Callers can render the JSON without holding the
// lock through the response write.
func (h *healthState) snapshot() (alive bool, peers map[string]peerHealth) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make(map[string]peerHealth, len(h.peers))
	for k, v := range h.peers {
		out[k] = v
	}
	return h.alive, out
}

// allPeersReady reports whether every tracked peer has its Ready flag
// set. An empty peer table returns true — a deployment with no
// declared peers is ready as soon as it's alive (the monolith case).
func (h *healthState) allPeersReady() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, p := range h.peers {
		if !p.Ready {
			return false
		}
	}
	return true
}

func (h *healthState) recordPeer(tag string, ready bool, errStr string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.peers[tag] = peerHealth{Ready: ready, LastError: errStr, LastProbed: time.Now()}
}

// mountHealth registers /__nexus/health and /__nexus/ready on the
// engine. Called from New() so the endpoints exist even when
// EnableDashboard is false — they're a framework contract, not a
// dashboard feature. The scope filter (listeners.go) treats this
// pair specially: ScopeInternal exposes them while hiding the rest
// of /__nexus.
//
// /__nexus/health: 200 when alive, 503 otherwise. No body — the
// status code is the contract; orchestrators read it directly.
//
// /__nexus/ready: 200 when alive AND every tracked peer is ready,
// 503 otherwise. JSON body lists per-peer state for human debugging
// — invaluable when "why isn't this pod ready?" is the question.
func mountHealth(e *gin.Engine, h *healthState) {
	e.GET(dashboard.Prefix+"/health", func(c *gin.Context) {
		if !h.isAlive() {
			c.Status(http.StatusServiceUnavailable)
			return
		}
		c.Status(http.StatusOK)
	})
	e.GET(dashboard.Prefix+"/ready", func(c *gin.Context) {
		alive, peers := h.snapshot()
		ready := alive
		for _, p := range peers {
			if !p.Ready {
				ready = false
				break
			}
		}
		status := http.StatusOK
		if !ready {
			status = http.StatusServiceUnavailable
		}
		c.JSON(status, gin.H{
			"alive": alive,
			"ready": ready,
			"peers": peers,
		})
	})
}

