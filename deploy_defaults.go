package nexus

import (
	"os"
	"sync"
)

// DeploymentDefaults is the manifest-derived configuration applied
// when Config.Addr / Config.Topology / Config.Deployment / Config.Version
// are not set explicitly. `nexus build --deployment X` codegens a
// zz_deploy_gen.go file in the main package whose init() calls
// SetDeploymentDefaults — so the binary boots with the right port
// and peer table without main.go declaring anything.
//
// Explicit Config fields always win. Defaults fill the gaps; this is
// the same precedence Go's flag package uses for -flag-from-env-var
// patterns and matches the user's intuition about "main.go is the
// override, manifest is the baseline."
type DeploymentDefaults struct {
	// Addr is the listen address (e.g. ":8081") baked from the
	// active deployment's manifest port. Empty when the manifest
	// omits port — Config.Addr / nexus's :8080 default takes over.
	Addr string

	// Deployment names the active unit. When non-empty, the
	// codegen'd init() set this from --deployment X so the binary
	// runs with the right tag without needing NEXUS_DEPLOYMENT.
	Deployment string

	// Topology is the peer table built from the manifest's `peers:`
	// block plus each peer deployment's port (so URLs default to
	// http://localhost:<peer-port> for local dev).
	Topology Topology

	// Listeners is the manifest-derived listener map. When non-nil
	// AND Config.Listeners is empty, newApp adopts this. Lets the
	// operator declare scope shape in nexus.deploy.yaml ("admin"
	// listener at port+1000, etc.) without main.go touching the
	// listener struct.
	Listeners map[string]Listener
}

var (
	deployDefaultsMu sync.RWMutex
	deployDefaults   DeploymentDefaults
	deployDefaultsOK bool
)

// SetDeploymentDefaults stores manifest-derived configuration that
// newApp consults when Config fields are zero. Called from a
// codegen'd init() block — user code should not call this directly.
//
// Calling it twice replaces the previous defaults; the last write
// wins. That makes hot-restart in tests predictable.
func SetDeploymentDefaults(d DeploymentDefaults) {
	deployDefaultsMu.Lock()
	defer deployDefaultsMu.Unlock()
	deployDefaults = d
	deployDefaultsOK = true
}

// loadDeploymentDefaults returns the stored defaults and whether
// any have been registered. Read by newApp before applying Config.
func loadDeploymentDefaults() (DeploymentDefaults, bool) {
	deployDefaultsMu.RLock()
	defer deployDefaultsMu.RUnlock()
	return deployDefaults, deployDefaultsOK
}

// Defaults returns the manifest-derived configuration (the same data
// New consults to fill in zero-valued Config fields). Exposed so
// main.go can read the active deployment's port — useful when the
// caller wants to derive listener addresses from the manifest's
// port without parsing nexus.deploy.yaml itself.
//
// Returns the zero DeploymentDefaults and false when no codegen'd
// init() has run (typical of `go run` against a deployment-agnostic
// monolith). Callers should fall back to their own defaults in that
// case rather than treating the empty Addr as authoritative.
func Defaults() (DeploymentDefaults, bool) {
	return loadDeploymentDefaults()
}

// resolveConfig applies the framework's precedence chain to fill in
// zero-valued Config fields:
//
//  1. Explicit Config fields always win.
//  2. Manifest-derived defaults (set by codegen'd init() blocks via
//     SetDeploymentDefaults) fill the next layer of gaps.
//  3. The NEXUS_DEPLOYMENT env var is the final fallback for
//     Deployment — useful when the same binary boots as different
//     units across environments via env override alone.
//
// One canonical implementation, called by both New (App
// construction) and Run (boot-time topology validation). Without
// this consolidation, three call sites re-implemented the chain and
// drifted (Run skipped step 3, validateTopology re-did step 3
// inline).
func resolveConfig(cfg Config) Config {
	if defaults, ok := loadDeploymentDefaults(); ok {
		if cfg.Addr == "" {
			cfg.Addr = defaults.Addr
		}
		if cfg.Deployment == "" {
			cfg.Deployment = defaults.Deployment
		}
		if cfg.Topology.Peers == nil && defaults.Topology.Peers != nil {
			cfg.Topology = defaults.Topology
		}
		if len(cfg.Listeners) == 0 && len(defaults.Listeners) > 0 {
			cfg.Listeners = defaults.Listeners
		}
	}
	if cfg.Deployment == "" {
		cfg.Deployment = os.Getenv(nexusDeploymentEnv)
	}
	return cfg
}
