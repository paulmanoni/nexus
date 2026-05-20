package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// newPkiCmd builds the `nexus pki` command group. Subcommands
// generate and manage the mTLS material the peer extension uses:
//
//	nexus pki init                 # one-time, on the CA host
//	nexus pki request --cn name    # per-peer, key never travels
//	nexus pki sign    --csr file   # CA signs, returns leaf cert
//	nexus pki issue   --cn name    # convenience: request+sign locally
//	nexus pki bundle  --cn name    # package ca.crt + leaf for shipping
//
// Standard-library only — no openssl shelling, no third-party PKI
// deps. ECDSA P-256 throughout: 256-bit keys, fast signatures,
// modern curve. Serial numbers are 128 bits of crypto/rand entropy
// (never incrementing) so two issued certs can't collide and a
// stolen CA can't forge predictable serials.
//
// The leaf CN is the peer identity the extension/peer plugin's
// VerifyConnection callback matches against AllowedClients. Pick
// stable CNs (per-service, not per-host) so cert rotation doesn't
// require AllowedClients edits.
func newPkiCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pki",
		Short: "Generate and manage mTLS certificates for the peer mesh",
		Long: `Manage the PKI material used by extension/peer mTLS.

A typical bootstrap:

  # On the CA host (once):
  nexus pki init

  # Per peer — key never leaves the peer host:
  peer:  nexus pki request --cn peer-alpha --dns peer-alpha.internal
  CA:    nexus pki sign --csr peer-alpha.csr
  → ship ca.crt + peer-alpha.crt back to the peer

  # Or for quick bootstrapping when the CA and peer are colocated:
  nexus pki issue --cn peer-alpha --dns peer-alpha.internal

  # Package a peer's cert + key + the CA cert for shipping
  # (NEVER includes ca.key — the bundle command can't read it):
  nexus pki bundle --cn peer-alpha`,
	}
	cmd.AddCommand(
		newPkiInitCmd(stdout, stderr),
		newPkiRequestCmd(stdout, stderr),
		newPkiSignCmd(stdout, stderr),
		newPkiIssueCmd(stdout, stderr),
		newPkiBundleCmd(stdout, stderr),
	)
	return cmd
}

// --- shared utilities ---

const (
	caCertFilename = "ca.crt"
	caKeyFilename  = "ca.key"

	// Private-key files are 0o600 (owner only). These are real
	// secrets, not user-project files — the v0.73.4 revert was
	// about scaffolding ergonomics, not key material.
	keyPerm  os.FileMode = 0o600
	certPerm os.FileMode = 0o644
	csrPerm  os.FileMode = 0o644
)

// genECDSAKey produces a fresh P-256 keypair. Wraps the stdlib call
// behind a single function so future curve changes (P-384, P-521)
// happen in one place.
func genECDSAKey() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

// randomSerial produces a 128-bit cryptographically random serial.
// Non-negative by construction (Int returns [0, max)). Two calls
// have a 2^-128 collision probability — for an internal mesh
// issuing tens of thousands of certs it's effectively never.
func randomSerial() (*big.Int, error) {
	max := new(big.Int).Lsh(big.NewInt(1), 128)
	return rand.Int(rand.Reader, max)
}

// writeKeyPEM marshals priv into a PKCS#8 PEM block and writes it
// with owner-only perms. PKCS#8 (rather than the older SEC1 form)
// so the file shape is uniform whether the key is ECDSA or, later,
// Ed25519 / RSA.
func writeKeyPEM(path string, priv *ecdsa.PrivateKey) error {
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return fmt.Errorf("marshal private key: %w", err)
	}
	body := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	return os.WriteFile(path, body, keyPerm) // #nosec G306 -- key file MUST be 0o600
}

// writeCertPEM writes a DER cert payload as a PEM CERTIFICATE block.
func writeCertPEM(path string, der []byte) error {
	body := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return os.WriteFile(path, body, certPerm)
}

// writeCSRPEM writes a DER CSR payload as a PEM CERTIFICATE REQUEST
// block. The .csr extension is conventional; we don't enforce it.
func writeCSRPEM(path string, der []byte) error {
	body := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
	return os.WriteFile(path, body, csrPerm)
}

// readCert parses a single PEM CERTIFICATE block from disk into a
// fully-decoded x509.Certificate. Extra blocks after the first are
// ignored — a typical CA bundle file holds multiple certs but the
// PKI commands only ever read the leaf or the root one at a time.
func readCert(path string) (*x509.Certificate, error) {
	body, err := os.ReadFile(path) // #nosec G304 -- operator-supplied PEM path
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(body)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("%s: no CERTIFICATE PEM block", path)
	}
	return x509.ParseCertificate(block.Bytes)
}

// readKey loads a PKCS#8 private key from disk and asserts it's an
// ECDSA key — every PKI flow in this package produces ECDSA, so a
// non-ECDSA key would be either a corrupted file or someone hand-
// crafting a different keypair that we can't sign with.
func readKey(path string) (*ecdsa.PrivateKey, error) {
	body, err := os.ReadFile(path) // #nosec G304 -- operator-supplied key path
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(body)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("%s: no PRIVATE KEY PEM block", path)
	}
	any, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse PKCS8: %w", err)
	}
	priv, ok := any.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s: not an ECDSA key (%T)", path, any)
	}
	return priv, nil
}

// readCSR parses a PEM CERTIFICATE REQUEST block and verifies its
// embedded signature before returning. Verifying here means a
// malformed or tampered CSR is caught BEFORE the sign step ever
// runs; the CA never signs a request it didn't authenticate the
// subject's possession of the matching private key.
func readCSR(path string) (*x509.CertificateRequest, error) {
	body, err := os.ReadFile(path) // #nosec G304 -- operator-supplied CSR path
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(body)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, fmt.Errorf("%s: no CERTIFICATE REQUEST PEM block", path)
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CSR: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("%s: signature invalid (corrupt or tampered): %w", path, err)
	}
	return csr, nil
}

// ensureOutDir is the leading sanity check shared by every command:
// create the output dir if it doesn't exist, error if the path
// points at a file. Created mode is 0o755 — output dirs are not
// secrets themselves; the per-file perms above (0o600 for keys)
// are what actually protect the material.
func ensureOutDir(dir string) error {
	if dir == "" {
		return errors.New("--out cannot be empty")
	}
	info, err := os.Stat(dir)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("--out %q exists but is not a directory", dir)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	return os.MkdirAll(dir, 0o755)
}

// joinOut is the helper every subcommand uses to compose a file
// path under --out without sprinkling filepath.Join calls around.
func joinOut(dir, name string) string { return filepath.Join(dir, name) }

// commonSubject is the bare Subject every leaf gets. Real PKI
// flows often want more (O, OU, country) but the peer extension
// only matches CN; padding the rest invites configuration drift.
func commonSubject(cn string) pkix.Name { return pkix.Name{CommonName: cn} }
