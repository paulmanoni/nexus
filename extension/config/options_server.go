package config

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
)

// AuthMode is the three-mode auth knob shared with extension/peer.
type AuthMode int

const (
	// AuthMTLS — production default. Verifies client cert subject
	// against the apps allow-list during the TLS handshake.
	AuthMTLS AuthMode = iota
	// AuthHMAC — shared-secret per app, signed timestamp + body
	// hash. 30s replay window.
	AuthHMAC
	// AuthNone — open, no client auth at the wire. Requires
	// NEXUS_CONFIG_DEV=1 in env; framework refuses to start
	// otherwise.
	AuthNone
)

// AppPolicy is the per-app entry in the apps allow-list.
type AppPolicy struct {
	// Profiles enumerates profile names this app is permitted to
	// fetch. Empty = no restriction (any profile present in the
	// source yaml works).
	Profiles []string

	// HMACSecret is consulted only when AuthMode is AuthHMAC.
	// Per-app secret matching the client's outbound signature.
	HMACSecret string
}

// ServerOption is the functional-option shape for Server / Module.
type ServerOption interface {
	applyServer(*serverConfig)
}

// serverConfig is the resolved, validated server-side state.
// Built from defaults + functional options. Internal to the
// package; the public surface is the ServerOption-returning
// functions below.
type serverConfig struct {
	listen     string
	authMode   AuthMode
	apps       map[string]AppPolicy
	autoApps   bool // when true, apps are derived from filesystem

	// TLS material. cert+key required; ca required when
	// authMode==AuthMTLS.
	tlsCert string
	tlsKey  string
	tlsCA   string

	// Signing. Private key path → loaded into priv at validate().
	// kid identifies the key on the wire so clients pinning N
	// pubkeys pick the right one.
	signingKeyPath string
	signingKID     string
	priv           ed25519.PrivateKey
}

// defaultServerConfig is the dev-friendly baseline. Operators add
// hardening via WithListen / WithSigning / WithTLS / WithAuth /
// WithApps. Refusing to start outside NEXUS_CONFIG_DEV=1 is
// applied at validate() time.
func defaultServerConfig() serverConfig {
	return serverConfig{
		listen:     ":7100",
		authMode:   AuthNone, // gate-protected; see validate()
		autoApps:   true,
		signingKID: "configd-dev",
	}
}

// WithListen sets the bind address for the config server.
func WithListen(addr string) ServerOption {
	return serverOptionFunc(func(c *serverConfig) { c.listen = addr })
}

// WithAuth picks the wire-auth mode.
func WithAuth(mode AuthMode) ServerOption {
	return serverOptionFunc(func(c *serverConfig) { c.authMode = mode })
}

// WithSigning pins the Ed25519 signing key path + the KID stamped
// on every served snapshot. Mandatory for production runs; dev
// runs auto-generate one in .configd/ when this option is omitted.
func WithSigning(keyPath, kid string) ServerOption {
	return serverOptionFunc(func(c *serverConfig) {
		c.signingKeyPath = keyPath
		c.signingKID = kid
	})
}

// WithTLS pins the server cert / key / optional CA bundle paths.
// CA is required for AuthMTLS (used to verify client certs);
// optional for AuthHMAC + AuthNone (server still terminates TLS
// but doesn't authenticate clients at the cert layer).
func WithTLS(certPath, keyPath, caPath string) ServerOption {
	return serverOptionFunc(func(c *serverConfig) {
		c.tlsCert = certPath
		c.tlsKey = keyPath
		c.tlsCA = caPath
	})
}

// WithApps pins the per-app policy. When omitted, the server
// auto-derives policy from the filesystem (one entry per
// <app>.nexus.config.yaml file in the source dir, all profiles
// permitted). Auto-derive is dev-friendly; production should
// declare apps explicitly.
func WithApps(m map[string]AppPolicy) ServerOption {
	return serverOptionFunc(func(c *serverConfig) {
		c.apps = m
		c.autoApps = false
	})
}

// serverOptionFunc adapts a closure into a ServerOption — same
// pattern as net/http's HandlerFunc. Keeps the public API a set
// of constructor functions without exposing a struct full of
// pointer fields.
type serverOptionFunc func(*serverConfig)

func (f serverOptionFunc) applyServer(c *serverConfig) { f(c) }

// validate runs at boot before the listener binds. Returns the
// first error it finds; loud, single-error output keeps boot logs
// readable.
func (c *serverConfig) validate() error {
	devMode := os.Getenv(devEnv) == "1"

	if c.listen == "" {
		return errors.New("WithListen address is empty")
	}

	// Auth-mode gating. AuthNone outside dev mode is the most
	// dangerous misconfiguration the plugin can produce, so we
	// catch it loud + early.
	if c.authMode == AuthNone && !devMode {
		return fmt.Errorf("AuthNone refuses to start without %s=1 in env", devEnv)
	}

	// TLS is always required (server-side cert, at minimum). Dev
	// mode auto-generates self-signed material to .configd/ when
	// paths are empty; production runs MUST pin them.
	if c.tlsCert == "" || c.tlsKey == "" {
		if !devMode {
			return errors.New("WithTLS cert+key required outside NEXUS_CONFIG_DEV=1")
		}
		// Dev: defer auto-generation to ensureDevTLS() at boot.
	}
	if c.authMode == AuthMTLS && c.tlsCA == "" {
		return errors.New("WithAuth(AuthMTLS) requires WithTLS(..., caPath) for client-cert verification")
	}

	// Signing. Mandatory; dev auto-generates if omitted.
	if c.signingKeyPath == "" && !devMode {
		return errors.New("WithSigning required outside NEXUS_CONFIG_DEV=1")
	}
	if c.signingKID == "" {
		return errors.New("signing KID must be non-empty")
	}

	// AuthHMAC requires per-app secrets in WithApps.
	if c.authMode == AuthHMAC {
		if c.autoApps {
			return errors.New("WithAuth(AuthHMAC) requires explicit WithApps(...) — auto-derive doesn't know secrets")
		}
		for name, p := range c.apps {
			if p.HMACSecret == "" {
				return fmt.Errorf("AuthHMAC: app %q missing HMACSecret", name)
			}
		}
	}
	return nil
}
