package peer

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// defaultSRVRefresh is the interval used when PeerSpec.SRVRefresh
// is zero. 30s is the sweet spot for most service meshes: fast
// enough that a freshly-added replica enters the pool within one
// minute of its DNS record landing, slow enough that the
// resolver doesn't hammer DNS under quiet conditions.
const defaultSRVRefresh = 30 * time.Second

// resolveInitialTargets does the boot-time target population for
// a PeerSpec. URL specs produce exactly one target (no DNS).
// SRV specs do a synchronous LookupSRV and emit one target per
// record. An SRV lookup that fails at boot leaves the spec with
// zero targets — Call will fast-fail with "no targets resolved"
// until the next refresh round succeeds, which is preferable to
// crashing the whole app on a transient DNS blip.
func resolveInitialTargets(spec PeerSpec) ([]*peerTarget, error) {
	if spec.URL != "" {
		return []*peerTarget{{url: spec.URL}}, nil
	}
	// SRV path. We DON'T propagate the LookupSRV error up — a
	// boot-time DNS miss is recoverable; the resolver loop will
	// retry. Same posture as the in-flight prober's transient-
	// failure handling.
	targets, err := lookupSRVTargets(spec.SRV)
	if err != nil || len(targets) == 0 {
		// Empty target list is a valid (if useless) state.
		// Operators see no-calls-going-anywhere and the prober
		// + dashboard surface it; preferable to a boot crash.
		return nil, nil
	}
	return targets, nil
}

// lookupSRVTargets runs net.LookupSRV on the configured record
// name and builds one peerTarget per response. The SRV record
// format is "_service._proto.domain" — we pass the empty
// service+proto strings to LookupSRV's first two args because
// the caller's `SRV` field already contains the full underscore-
// prefixed name; LookupSRV's "if both are empty, look up name
// verbatim" mode is the simpler API for that case.
//
// RFC 2782 priority + weight ordering is honored at pick time
// (peerConn.pickTarget), not at resolve time — we want the full
// set in the pool so health-based fallback works across
// priorities.
func lookupSRVTargets(name string) ([]*peerTarget, error) {
	if name == "" {
		return nil, errors.New("SRV name is empty")
	}
	// net.LookupSRV with both service and proto empty does a
	// "lookup the literal name" query, which is what we want
	// when the caller hands us the full record name.
	_, records, err := net.LookupSRV("", "", name)
	if err != nil {
		return nil, fmt.Errorf("LookupSRV %q: %w", name, err)
	}
	out := make([]*peerTarget, 0, len(records))
	for _, r := range records {
		// Records carry the target with a trailing dot (FQDN
		// canonical form). Strip it for the URL — most TLS
		// stacks compare hostnames without the trailing dot,
		// and the cert SAN will be in the same shape.
		host := strings.TrimSuffix(r.Target, ".")
		if host == "" {
			continue
		}
		url := fmt.Sprintf("https://%s:%d", host, r.Port)
		out = append(out, &peerTarget{url: url})
	}
	return out, nil
}

// runSRVResolver loops every spec.SRVRefresh and re-runs the
// LookupSRV, swapping pc.targets atomically (under pc.targetsMu)
// when the resolved set differs from the current one. Started
// from peer.Module's OnBoot for every peer whose spec uses SRV;
// stopped cleanly when the parent ctx cancels on shutdown.
//
// "Differs" is determined by URL-set equality — same N URLs in
// any order = no-op. This keeps the targets' ready state and
// cursor stable when DNS returns the same answer twice.
func runSRVResolver(ctx context.Context, pc *peerConn, spec PeerSpec) {
	interval := spec.SRVRefresh
	if interval <= 0 {
		interval = defaultSRVRefresh
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			resolved, err := lookupSRVTargets(spec.SRV)
			if err != nil {
				// Transient DNS hiccup. Keep the existing
				// pool — better to call into possibly-stale
				// targets and let the prober eject the
				// genuinely-dead ones than to drop the whole
				// pool on a flaky resolver.
				continue
			}
			applyResolvedTargets(pc, resolved)
		}
	}
}

// applyResolvedTargets reconciles the freshly-resolved target
// list against pc.targets. Targets already in the pool keep
// their ready state + cursor position; new targets are added
// (optimistically ready=true so the first call exercises them);
// targets dropped from DNS are removed.
//
// Locking: targetsMu is held in write mode for the swap, but
// the reconcile work runs under read-set semantics (we never
// mutate the existing target objects).
func applyResolvedTargets(pc *peerConn, resolved []*peerTarget) {
	pc.targetsMu.Lock()
	defer pc.targetsMu.Unlock()

	// Index existing targets by URL so the reconcile is O(n+m)
	// instead of O(n*m). For typical N targets this is a
	// micro-optimization; we do it for clarity, not speed.
	existing := make(map[string]*peerTarget, len(pc.targets))
	for _, t := range pc.targets {
		existing[t.url] = t
	}
	next := make([]*peerTarget, 0, len(resolved))
	for _, r := range resolved {
		if cur, ok := existing[r.url]; ok {
			next = append(next, cur) // preserve ready state
		} else {
			r.ready.Store(true) // new target — optimistic
			next = append(next, r)
		}
	}
	pc.targets = next
}
