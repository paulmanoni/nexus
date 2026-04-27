package nexus

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/paulmanoni/nexus/dashboard"
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

// peerProber polls every declared peer's /__nexus/health endpoint at
// the configured interval and records reachability into healthState.
// The active deployment's own entry is skipped — a binary doesn't
// probe itself (the loopback succeeds trivially and the entry is a
// placeholder that points at the listener bound to this process).
//
// The prober runs as a single goroutine — peer count is small (one
// per deployment unit, typically a handful) so we don't fan out per
// peer. ctx cancellation stops the loop on fx Stop.
type peerProber struct {
	topology   Topology
	deployment string // active unit; skipped from probing
	state      *healthState
	httpClient *http.Client
	interval   time.Duration
}

func newPeerProber(topology Topology, deployment string, state *healthState) *peerProber {
	return &peerProber{
		topology:   topology,
		deployment: deployment,
		state:      state,
		httpClient: &http.Client{Timeout: 2 * time.Second},
		interval:   5 * time.Second,
	}
}

// run probes every peer once immediately, then on the prober's
// interval until ctx is cancelled. The first probe runs synchronously
// before returning so /__nexus/ready can answer truthfully on the
// first request after fx Start finishes.
func (p *peerProber) run(ctx context.Context) {
	if p == nil || len(p.topology.Peers) == 0 {
		return
	}
	// Seed every non-self peer as "not ready" so /__nexus/ready
	// reports accurately even before the first probe completes — the
	// JSON body lists them as not-yet-probed rather than missing.
	for tag := range p.topology.Peers {
		if tag == p.deployment {
			continue
		}
		p.state.recordPeer(tag, false, "not yet probed")
	}
	p.probeAll(ctx)
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.probeAll(ctx)
		}
	}
}

// probeAll fires one HTTP GET per peer in parallel and records the
// result. Failures (network error, non-200) mark the peer not ready
// with the reason string for the JSON body.
func (p *peerProber) probeAll(ctx context.Context) {
	var wg sync.WaitGroup
	for tag, peer := range p.topology.Peers {
		if tag == p.deployment || peer.URL == "" {
			continue
		}
		wg.Add(1)
		go func(tag, baseURL string) {
			defer wg.Done()
			p.probeOne(ctx, tag, baseURL)
		}(tag, peer.URL)
	}
	wg.Wait()
}

func (p *peerProber) probeOne(ctx context.Context, tag, baseURL string) {
	probeCtx, cancel := context.WithTimeout(ctx, p.httpClient.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, baseURL+dashboard.Prefix+"/health", nil)
	if err != nil {
		p.state.recordPeer(tag, false, err.Error())
		return
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		p.state.recordPeer(tag, false, err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		p.state.recordPeer(tag, false, http.StatusText(resp.StatusCode))
		return
	}
	p.state.recordPeer(tag, true, "")
}
