package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

// DeployManifest is the on-disk schema for nexus.deploy.yaml. The
// single source of truth for the project's deployment topology:
//
//   - which modules each deployment owns locally
//   - which port each deployment listens on
//   - per-peer transport settings (timeout, retries, min_version)
//
// Example:
//
//	deployments:
//	  monolith:
//	    owns: [users, checkout]
//	    port: 8080
//	  users-svc:
//	    owns: [users]
//	    port: 8081
//	  checkout-svc:
//	    owns: [checkout]
//	    port: 8080
//
//	peers:
//	  users-svc:
//	    timeout: 2s
//	  checkout-svc:
//	    timeout: 2s
//
// `nexus build` codegens a zz_deploy_gen.go that wires the active
// deployment's port + peer table into the framework at boot — so
// main.go doesn't need to declare Topology or read PORT from env.
type DeployManifest struct {
	Deployments map[string]DeploymentSpec `yaml:"deployments"`
	Peers       map[string]PeerSpec       `yaml:"peers,omitempty"`
}

// DeploymentSpec describes one deployment unit.
type DeploymentSpec struct {
	// Owns lists module names (the first arg of nexus.Module(...))
	// that compile in their hand-written form for this deployment.
	// Modules not listed are remote: their public surface is
	// replaced by an HTTP stub via go build -overlay.
	Owns []string `yaml:"owns"`

	// Port is the listen port baked into Config.Addr at build time.
	// Optional — when zero, the binary falls back to PORT env var
	// and ultimately to nexus's default :8080. `nexus dev --split`
	// also uses this for its readiness probe, falling back to the
	// --base-port auto-assignment when omitted.
	Port int `yaml:"port,omitempty"`
}

// PeerSpec is the per-peer transport binding consumed by codegen'd
// remote clients. Mirrors nexus.Peer's runtime shape with YAML-friendly
// types. Empty fields fall back to framework defaults.
type PeerSpec struct {
	// URL overrides the default local URL ("http://localhost:<port>")
	// for this peer. Useful for prod where peers live behind LBs or
	// in another network. Supports env-var expansion via the
	// codegen'd init: `${USERS_SVC_URL}` literal becomes an
	// os.Getenv read at boot, defaulting to the local URL when
	// unset.
	URL string `yaml:"url,omitempty"`

	// Timeout caps each remote call. Zero falls back to the
	// RemoteCaller default (30s).
	Timeout time.Duration `yaml:"timeout,omitempty"`

	// MinVersion is the lowest peer Version (read from the peer's
	// /__nexus/config) accepted on the first call. Empty disables
	// the floor; soft-fail behavior matches the runtime Peer field.
	MinVersion string `yaml:"min_version,omitempty"`

	// Retries caps idempotent retries on transport errors. Zero
	// disables retries entirely.
	Retries int `yaml:"retries,omitempty"`

	// Auth declares how to attach the Authorization header on every
	// outgoing call to this peer. nil = no header (falls back to
	// the framework's default propagator that forwards the inbound
	// Authorization through the request context). Codegen reads
	// this and emits a closure on Peer.Auth.
	//
	//	auth:
	//	  type: bearer
	//	  token: ${USERS_SVC_TOKEN}
	Auth *AuthSpec `yaml:"auth,omitempty"`
}

// AuthSpec describes how a peer's Authorization header is built at
// boot. Today supports `bearer` only; extending to `basic` / `mTLS`
// is one switch arm in the codegen.
type AuthSpec struct {
	// Type is the auth scheme. Currently only "bearer" is recognized;
	// other values cause `nexus build` to fail with a clear message.
	Type string `yaml:"type"`

	// Token is the credential. Supports `${ENV}` interpolation: when
	// the value matches `${VAR}` exactly, the codegen emits an
	// os.Getenv("VAR") read at boot. Otherwise it's used verbatim
	// (useful for tests; never check real secrets in).
	Token string `yaml:"token"`
}

// LoadManifest reads and parses a deploy manifest from path. Returns
// a friendly error rather than yaml's terse one when fields are
// missing or the file isn't there.
func LoadManifest(path string) (*DeployManifest, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("nexus build: manifest %s not found — create it with a `deployments:` block listing each unit", abs)
		}
		return nil, fmt.Errorf("read manifest %s: %w", abs, err)
	}
	var m DeployManifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", abs, err)
	}
	if len(m.Deployments) == 0 {
		return nil, fmt.Errorf("manifest %s declares no deployments under `deployments:`", abs)
	}
	// Empty `owns` is permitted: it means "owns every module" — the
	// natural semantics for a monolith deployment. Split units omit
	// `owns` at their own risk; the silent-monolith failure mode is
	// noted in `Owns` below.
	return &m, nil
}

// Names returns the deployment names sorted lexically. Used for
// error messages that need to enumerate valid choices.
func (m *DeployManifest) Names() []string {
	out := make([]string, 0, len(m.Deployments))
	for k := range m.Deployments {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Owns reports whether the named deployment owns the given module
// locally. Unknown deployments return false. An empty `owns` list
// means "owns everything" — the monolith semantic — so any module
// query against an unspecified-owns deployment returns true. Split
// units must list `owns` explicitly; if they don't, they silently
// degrade to monolith mode (no shadows generated for that unit).
func (m *DeployManifest) Owns(deployment, module string) bool {
	spec, ok := m.Deployments[deployment]
	if !ok {
		return false
	}
	if len(spec.Owns) == 0 {
		return true
	}
	for _, n := range spec.Owns {
		if n == module {
			return true
		}
	}
	return false
}

// DeploymentOf returns the name of the split-unit deployment that
// owns the given module, or "" when the module isn't claimed by any
// non-monolith deployment. Used by the build tool as a fallback for
// modules whose source omits nexus.DeployAs(...) — the manifest's
// owns list is the secondary source of truth (auto-inject path).
//
// Monolith deployments (empty owns) are skipped here: they own
// every module, so they'd match every query and aren't a useful
// "split tag" answer.
func (m *DeployManifest) DeploymentOf(module string) string {
	for name, spec := range m.Deployments {
		if len(spec.Owns) == 0 {
			continue
		}
		for _, n := range spec.Owns {
			if n == module {
				return name
			}
		}
	}
	return ""
}
