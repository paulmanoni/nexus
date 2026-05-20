package peer

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"time"
)

// Config wires the peer extension. Both server-only (Listen set, no
// Peers) and client-only (Peers set, no Listen) modes are valid.
type Config struct {
	// Identity is the name this app declares to peers. Stamped on
	// outbound X-Nexus-Peer headers and used as the expected mTLS
	// cert CN/SAN. Required whenever Peers or Listen is non-empty.
	Identity string

	// Listen, when set, binds the peer-only HTTP/2 listener. Empty
	// disables the server side entirely; the app can still call
	// peers via the Registry.
	Listen string

	// Peers maps caller-side name → connection spec. Each entry
	// gets one persistent HTTP/2 client at app start.
	Peers map[string]PeerSpec

	// AllowedClients lists the peer identities permitted to call
	// in. Empty + Listen set + AuthMode mTLS = "any cert signed by
	// the configured CA is acceptable", which the validator logs a
	// warning about. Has no effect when AuthMode is HMAC or None.
	AllowedClients []string

	// TLS bundles the server cert + key + trust roots for both
	// inbound and outbound traffic. Required when Listen != "" or
	// any peer uses HTTPS (the typical case).
	TLS TLSConfig

	// AuthMode selects the auth scheme for the peer protocol. Each
	// app picks one; mixing modes across a fleet would mean every
	// pair has to agree at the wire level, which is operator
	// friction. Defaults to AuthMTLS.
	AuthMode AuthMode

	// HMACSecrets is the per-peer shared secret table consumed by
	// AuthMode == AuthHMAC. Keys match the entries in Peers (and
	// AllowedClients on the server side); the secret is treated as
	// raw bytes (use a high-entropy 32+ byte value).
	HMACSecrets map[string]string
}

// PeerSpec configures the outbound connection to one peer.
//
// Two ways to point at the peer; exactly one must be set:
//
//	URL — single hard-coded target. Simplest, no DNS involvement.
//	      Right for fixed-address deployments and dev.
//
//	SRV — DNS SRV record name (e.g. "_nexus._tcp.orders.internal").
//	      Resolves to one or more targets; the framework round-
//	      robins across them and re-resolves periodically so a
//	      replica that joins or leaves the pool surfaces without
//	      a caller redeploy. Right for production meshes where
//	      orders-svc may have N replicas behind one logical name.
type PeerSpec struct {
	URL string // https://orders.internal:7000

	// SRV is a DNS SRV record name resolving to N (host, port,
	// priority, weight) targets. Standard format:
	//   "_<service>._<proto>.<domain>"  e.g.  "_nexus._tcp.orders.internal"
	//
	// The framework calls net.LookupSRV at boot and re-resolves
	// every SRVRefresh interval (default 30s) so target churn
	// reaches the caller without a restart. RFC 2782 priority +
	// weight are honored by the round-robin picker: lower-
	// priority targets are preferred; within a priority,
	// healthy targets are visited in weighted-random order.
	SRV string

	// SRVRefresh is how often net.LookupSRV is re-run. 0 falls
	// back to 30 seconds. Set lower for fast-churning meshes
	// (Kubernetes headless services with rolling restarts);
	// higher for stable on-prem deployments.
	SRVRefresh time.Duration

	// ServerName, when non-empty, overrides the host portion of
	// the URL when verifying the server's TLS certificate. Use
	// with SRV when every replica presents the same logical
	// service cert ("orders-svc") rather than per-host certs —
	// otherwise per-replica certs need the resolved hostname
	// as a SAN, which the nexus pki workflow supports via
	// `--dns` flags.
	ServerName string

	// CACert pins the set of CAs willing to vouch for the server's
	// cert. PEM file path. Empty falls back to the host's root
	// trust store — fine for public CAs, but for an internal mesh
	// you almost always want to pin your own CA here so a stolen
	// public cert can't impersonate the peer.
	CACert string

	// ClientCert / ClientKey override Config.TLS for this specific
	// peer, useful when an app holds different identity certs for
	// different downstream services (rare but real in some PKI
	// setups). Empty falls back to Config.TLS.
	ClientCert string
	ClientKey  string

	// RequestTimeout caps each outbound call's wall time. 0 leaves
	// the client uncapped — rely on the caller's ctx deadline,
	// which is the recommended pattern (deadlines propagate
	// through the Envelope.Deadline field).
	RequestTimeout time.Duration

	// MaxConcurrent caps simultaneous in-flight calls to this peer
	// (across every SRV-resolved target). 0 falls back to a
	// defensive default of 64.
	MaxConcurrent int
}

// TLSConfig is the cert material the peer plugin loads. Reused for
// both server-side TLS termination (when Listen != "") and
// client-side identity (when Peers != nil).
type TLSConfig struct {
	Cert   string // PEM path; server cert OR client cert
	Key    string // PEM path; matching private key
	CACert string // PEM path; trust roots — server verifies client certs,
	// client verifies server cert. Required for mTLS.
}

// AuthMode selects the wire-level auth scheme.
type AuthMode int

const (
	// AuthMTLS is the production default — mutual TLS with cert
	// subject pinned against AllowedClients.
	AuthMTLS AuthMode = iota

	// AuthHMAC uses a shared secret per peer pair plus signed
	// timestamps. Cheaper than mTLS, fine for tightly-scoped
	// internal networks (single VPC, no zero-trust requirement).
	AuthHMAC

	// AuthNone disables auth entirely. Refuses to start unless
	// NEXUS_PEER_DEV=1 is set in the environment; logs a loud
	// warning every 60s while running.
	AuthNone
)

const devEnv = "NEXUS_PEER_DEV"

func (c Config) validate() error {
	if c.Listen == "" && len(c.Peers) == 0 {
		return errors.New("peer.Module: neither Listen nor Peers set — nothing to do")
	}
	if c.Identity == "" {
		return errors.New("peer.Module: Identity is required")
	}
	switch c.AuthMode {
	case AuthMTLS:
		if c.Listen != "" && (c.TLS.Cert == "" || c.TLS.Key == "") {
			return errors.New("peer.Module: AuthMTLS + Listen requires TLS.Cert + TLS.Key")
		}
		if c.Listen != "" && c.TLS.CACert == "" {
			return errors.New("peer.Module: AuthMTLS + Listen requires TLS.CACert to verify client certs")
		}
	case AuthHMAC:
		if c.Listen != "" && len(c.HMACSecrets) == 0 {
			return errors.New("peer.Module: AuthHMAC + Listen requires HMACSecrets")
		}
	case AuthNone:
		if os.Getenv(devEnv) != "1" {
			return fmt.Errorf("peer.Module: AuthNone refuses to start without %s=1 in env", devEnv)
		}
	default:
		return fmt.Errorf("peer.Module: unknown AuthMode %d", c.AuthMode)
	}
	for name, p := range c.Peers {
		switch {
		case p.URL == "" && p.SRV == "":
			return fmt.Errorf("peer.Module: peer %q needs either URL or SRV", name)
		case p.URL != "" && p.SRV != "":
			return fmt.Errorf("peer.Module: peer %q has both URL and SRV — pick one", name)
		}
	}
	return nil
}

// buildServerTLSConfig assembles the *tls.Config the inbound HTTP/2
// server uses. mTLS verification + AllowedClients check is wired
// here so an unauthorized cert can't even complete the handshake;
// the dispatcher never sees their requests.
func buildServerTLSConfig(c Config) (*tls.Config, error) {
	if c.AuthMode != AuthMTLS {
		return loadServerKeypairOnly(c.TLS)
	}
	cert, err := tls.LoadX509KeyPair(c.TLS.Cert, c.TLS.Key)
	if err != nil {
		return nil, fmt.Errorf("peer: load server keypair: %w", err)
	}
	caPool, err := loadCAPool(c.TLS.CACert)
	if err != nil {
		return nil, fmt.Errorf("peer: load CA pool: %w", err)
	}
	allowed := make(map[string]struct{}, len(c.AllowedClients))
	for _, id := range c.AllowedClients {
		allowed[id] = struct{}{}
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    caPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
		NextProtos:   []string{"h2"}, // force HTTP/2 negotiation
	}
	if len(allowed) > 0 {
		cfg.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.VerifiedChains) == 0 || len(cs.VerifiedChains[0]) == 0 {
				return errors.New("peer: no verified client chain")
			}
			cn := cs.VerifiedChains[0][0].Subject.CommonName
			if _, ok := allowed[cn]; !ok {
				return fmt.Errorf("peer: client identity %q not in AllowedClients", cn)
			}
			return nil
		}
	}
	return cfg, nil
}

// buildClientTLSConfig assembles the *tls.Config for one outbound
// peer. Pins the configured CA so a downgrade or DNS-poisoning
// attacker can't substitute a different valid cert.
func buildClientTLSConfig(global TLSConfig, spec PeerSpec) (*tls.Config, error) {
	cert, key := spec.ClientCert, spec.ClientKey
	if cert == "" {
		cert = global.Cert
	}
	if key == "" {
		key = global.Key
	}
	if cert == "" || key == "" {
		return nil, errors.New("peer: client TLS requires either PeerSpec.ClientCert/Key or Config.TLS.Cert/Key")
	}
	pair, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		return nil, fmt.Errorf("peer: load client keypair: %w", err)
	}
	caPath := spec.CACert
	if caPath == "" {
		caPath = global.CACert
	}
	var rootPool *x509.CertPool
	if caPath != "" {
		rootPool, err = loadCAPool(caPath)
		if err != nil {
			return nil, fmt.Errorf("peer: load client CA pool: %w", err)
		}
	}
	return &tls.Config{
		Certificates: []tls.Certificate{pair},
		RootCAs:      rootPool,
		MinVersion:   tls.VersionTLS13,
		NextProtos:   []string{"h2"},
	}, nil
}

// buildClientTLSConfigNoMTLS is the non-mTLS client variant —
// terminates TLS (peer protocol is always TLS outside dev) but
// doesn't present a client cert. Server identity is still pinned
// via the CA pool.
func buildClientTLSConfigNoMTLS(global TLSConfig, spec PeerSpec) (*tls.Config, error) {
	caPath := spec.CACert
	if caPath == "" {
		caPath = global.CACert
	}
	var rootPool *x509.CertPool
	if caPath != "" {
		var err error
		rootPool, err = loadCAPool(caPath)
		if err != nil {
			return nil, fmt.Errorf("peer: load client CA pool: %w", err)
		}
	}
	return &tls.Config{
		RootCAs:    rootPool,
		MinVersion: tls.VersionTLS13,
		NextProtos: []string{"h2"},
	}, nil
}

// loadServerKeypairOnly handles the non-mTLS server path (AuthHMAC,
// AuthNone). The server still terminates TLS (peer protocol is
// always over TLS in any non-dev environment) but doesn't verify
// client certs — auth comes from the HMAC bearer or is disabled.
func loadServerKeypairOnly(t TLSConfig) (*tls.Config, error) {
	if t.Cert == "" || t.Key == "" {
		return nil, errors.New("peer: TLS.Cert + TLS.Key required even without mTLS")
	}
	cert, err := tls.LoadX509KeyPair(t.Cert, t.Key)
	if err != nil {
		return nil, fmt.Errorf("peer: load server keypair: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
		NextProtos:   []string{"h2"},
	}, nil
}

// loadCAPool reads a PEM bundle off disk and returns an x509.CertPool
// containing every cert in it. One file path can hold multiple PEM
// blocks (cert chains, multi-CA bundles); AppendCertsFromPEM walks
// them all.
func loadCAPool(path string) (*x509.CertPool, error) {
	body, err := os.ReadFile(path) // #nosec G304 -- operator-supplied PEM path
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(body) {
		return nil, fmt.Errorf("no PEM blocks parsed from %s", path)
	}
	return pool, nil
}
