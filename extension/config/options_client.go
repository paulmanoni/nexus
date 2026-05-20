package config

import (
	"errors"
	"fmt"
	"time"
)

// UnreachablePolicy controls how config.Client handles the case
// where the config server is down at boot. Three policies match
// real ops splits across "fail loud," "warn and run," and "have
// a floor."
type UnreachablePolicy int

const (
	// UseCacheOrFail — default. If a valid sealed cache exists,
	// boot from it and retry the server in the background.
	// Otherwise, refuse to boot loudly. Right for production
	// where running on stale config is preferable to silent
	// degradation, but running on no config is worse than not
	// running.
	UseCacheOrFail UnreachablePolicy = iota

	// UseCacheAndWarn — boot from cache if present; otherwise
	// install an empty store + log SEV1 every 60s. The
	// background retry loop still runs. Right for systems where
	// the absence of config is degraded-but-survivable (e.g. a
	// service that has reasonable hard-coded fallbacks).
	UseCacheAndWarn

	// UseDefaults — boot from cache if present; otherwise
	// install the WithDefaults map + log SEV1 every 60s. Right
	// for greenfield / edge deployments where the operator can
	// provide explicit defaults via Config.Defaults.
	UseDefaults
)

// ClientOption is the functional-option shape for Client.
type ClientOption interface {
	applyClient(*clientConfig)
}

// clientConfig is the resolved, validated client-side state.
type clientConfig struct {
	serverURL string

	identity string
	profile  string
	label    string

	// SignerKey paths. Multiple paths allowed for rotation —
	// the snapshot's KID picks which one verifies. Loaded into
	// signerKeys keyed by an explicit KID name; default KID
	// derived from the filename when WithSignerKey is used.
	signerKeyPaths []string

	cachePath string

	// TLS material — optional. When AuthMTLS is implied by
	// server's auth config, both clientCert + clientKey + caCert
	// must be set; when server runs AuthHMAC or AuthNone, only
	// caCert (to verify the server's cert) is required.
	caCert     string
	clientCert string
	clientKey  string

	hmacSecret string // when server uses AuthHMAC

	onUnreachable UnreachablePolicy
	defaults      map[string]any

	// Polling interval for version checks. Polling is the
	// phase-1 refresh mechanism; phase 2 will add a WS
	// subscription that obsoletes the poll (with poll as a
	// fallback).
	pollInterval time.Duration

	// Per-request timeout for fetches.
	requestTimeout time.Duration
}

func defaultClientConfig(serverURL string) clientConfig {
	return clientConfig{
		serverURL:      serverURL,
		profile:        "default",
		onUnreachable:  UseCacheOrFail,
		pollInterval:   30 * time.Second,
		requestTimeout: 10 * time.Second,
	}
}

// Identity pins this client's app name. MUST match a key in the
// server's WithApps policy (and, when AuthMTLS is in play, the
// client cert's CN). Required — empty Identity is a boot error.
func Identity(name string) ClientOption {
	return clientOptionFunc(func(c *clientConfig) { c.identity = name })
}

// Profile selects which profile the server resolves on every
// fetch. Defaults to "default" — apps with no per-env split need
// set nothing.
func Profile(p string) ClientOption {
	return clientOptionFunc(func(c *clientConfig) { c.profile = p })
}

// Label optionally pins a git ref (branch/tag/SHA) the server
// should resolve against. Only meaningful when the server uses
// FromGit. Empty = server's default branch.
func Label(l string) ClientOption {
	return clientOptionFunc(func(c *clientConfig) { c.label = l })
}

// SignerKey pins one or more public-key files the client will
// accept snapshots from. Multiple entries cover rotation —
// during a rotation window, the server can sign with KID "v2"
// while clients accept both "v1" and "v2." Required.
func SignerKey(paths ...string) ClientOption {
	return clientOptionFunc(func(c *clientConfig) {
		c.signerKeyPaths = append(c.signerKeyPaths, paths...)
	})
}

// CachePath pins where the sealed cache file lives. Framework
// auto-generates a sibling .key file at first boot; both 0o600.
// Required — opting out of caching means signing the death
// warrant for "doesn't depend on the server."
func CachePath(p string) ClientOption {
	return clientOptionFunc(func(c *clientConfig) { c.cachePath = p })
}

// WithClientTLS pins the TLS material for talking to the server.
// caCert verifies the server's cert; clientCert + clientKey are
// the client's identity for mTLS. Pass clientCert+clientKey
// empty when the server runs AuthHMAC or AuthNone — server
// still authenticates itself, but doesn't expect a client cert.
func WithClientTLS(caCert, clientCert, clientKey string) ClientOption {
	return clientOptionFunc(func(c *clientConfig) {
		c.caCert = caCert
		c.clientCert = clientCert
		c.clientKey = clientKey
	})
}

// WithHMAC sets the per-client HMAC secret used to sign outbound
// requests when the server runs AuthHMAC. Required for HMAC
// mode; ignored otherwise.
func WithHMAC(secret string) ClientOption {
	return clientOptionFunc(func(c *clientConfig) { c.hmacSecret = secret })
}

// OnUnreachable picks the policy for server-down-at-boot.
// Defaults to UseCacheOrFail. See policy doc above.
func OnUnreachable(p UnreachablePolicy) ClientOption {
	return clientOptionFunc(func(c *clientConfig) { c.onUnreachable = p })
}

// WithDefaults provides the fallback value tree consulted when
// OnUnreachable is UseDefaults AND no cache exists. Ignored
// under the other policies.
func WithDefaults(defaults map[string]any) ClientOption {
	return clientOptionFunc(func(c *clientConfig) { c.defaults = defaults })
}

// WithPollInterval overrides the version-check interval (default
// 30s). Phase 2 WS subscription will obsolete polling; this knob
// stays as the fallback path.
func WithPollInterval(d time.Duration) ClientOption {
	return clientOptionFunc(func(c *clientConfig) { c.pollInterval = d })
}

// WithRequestTimeout overrides the per-request HTTP timeout
// (default 10s). Tight enough that a hung server doesn't stall
// startup forever; tunable for high-latency networks.
func WithRequestTimeout(d time.Duration) ClientOption {
	return clientOptionFunc(func(c *clientConfig) { c.requestTimeout = d })
}

type clientOptionFunc func(*clientConfig)

func (f clientOptionFunc) applyClient(c *clientConfig) { f(c) }

// validate runs at boot before the fetch loop starts.
func (c *clientConfig) validate() error {
	if c.serverURL == "" {
		return errors.New("server URL is empty")
	}
	if c.identity == "" {
		return errors.New("Identity required")
	}
	if c.profile == "" {
		return errors.New("Profile required")
	}
	if len(c.signerKeyPaths) == 0 {
		return errors.New("SignerKey required (pin at least one .pub file)")
	}
	if c.cachePath == "" {
		return errors.New("CachePath required (opting out of caching breaks offline-boot)")
	}
	if c.onUnreachable == UseDefaults && len(c.defaults) == 0 {
		return fmt.Errorf("OnUnreachable(UseDefaults) requires WithDefaults(...)")
	}
	return nil
}
