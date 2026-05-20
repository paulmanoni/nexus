package main

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math/big"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
)

// newPkiSignCmd builds `nexus pki sign` — run on the CA host.
// Loads ca.crt + ca.key, validates the supplied CSR (signature
// check + non-empty CN), and emits a signed leaf certificate.
//
// Leaf params:
//   - ECDSA P-256 keys (inherited from the CSR's public key)
//   - Short-lived: 180 days by default; pair with cert rotation
//     automation so a stolen key has a small blast radius
//   - KeyUsage: DigitalSignature (TLS auth) — the leaf can't sign
//     other certs; chain depth is 1
//   - ExtKeyUsage: ServerAuth + ClientAuth — peer-extension certs
//     are presented BOTH as a server (inbound calls) and as a
//     client (outbound calls), so both EKUs are required
//   - SANs copied verbatim from the CSR
//   - Cryptographically random 128-bit serial via crypto/rand
func newPkiSignCmd(stdout, stderr io.Writer) *cobra.Command {
	var (
		caDir   string
		csrPath string
		out     string
		days    int
	)
	cmd := &cobra.Command{
		Use:   "sign",
		Short: "Sign a CSR with the CA, producing <cn>.crt",
		Long: `Sign a peer CSR. Reads <ca-dir>/ca.crt + <ca-dir>/ca.key,
verifies the CSR's signature, then issues a leaf cert valid for
--days days.

Writes <out>/<cn>.crt (CN derived from the CSR's Subject). Ship
that plus <ca-dir>/ca.crt back to the peer — the peer's existing
<cn>.key + the two certs are everything its mTLS config needs.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			if csrPath == "" {
				return errors.New("--csr is required")
			}
			caCert, caKey, err := loadCA(caDir)
			if err != nil {
				return err
			}
			csr, err := readCSR(csrPath)
			if err != nil {
				return err
			}
			if csr.Subject.CommonName == "" {
				return fmt.Errorf("%s: CSR has empty CN — cannot match against AllowedClients", csrPath)
			}
			if err := ensureOutDir(out); err != nil {
				return err
			}
			leafCN := csr.Subject.CommonName
			leafPath := joinOut(out, leafCN+".crt")

			der, serial, err := signLeaf(csr, caCert, caKey, days)
			if err != nil {
				return err
			}
			if err := writeCertPEM(leafPath, der); err != nil {
				return fmt.Errorf("write %s: %w", leafPath, err)
			}
			fmt.Fprintf(stdout, "wrote %s (cert, %d-day validity, serial %s)\n",
				leafPath, days, serial.Text(16))
			fmt.Fprintf(stdout, "Ship %s + %s/%s to the peer.\n",
				leafPath, caDir, caCertFilename)
			return nil
		},
	}
	cmd.Flags().StringVar(&caDir, "ca-dir", ".", "directory holding ca.crt + ca.key")
	cmd.Flags().StringVar(&csrPath, "csr", "", "path to the CSR file produced by `nexus pki request`")
	cmd.Flags().StringVar(&out, "out", ".", "directory to write the signed leaf cert into")
	cmd.Flags().IntVar(&days, "days", 180, "leaf cert validity in days (recommended: 180)")
	return cmd
}

// loadCA reads ca.crt + ca.key from dir. Both must be present and
// well-formed; missing-key + present-cert is the most confusing
// failure mode (everything looks right at a glance) so we surface
// it with the most specific message.
func loadCA(dir string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certPath := filepath.Join(dir, caCertFilename)
	keyPath := filepath.Join(dir, caKeyFilename)
	cert, err := readCert(certPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", certPath, err)
	}
	if !cert.IsCA {
		return nil, nil, fmt.Errorf("%s: not a CA cert (IsCA=false) — wrong file?", certPath)
	}
	key, err := readKey(keyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", keyPath, err)
	}
	return cert, key, nil
}

// signLeaf is the lowest-level CA operation: build the leaf
// template, mint a random serial, and call x509.CreateCertificate.
// Separated from the cobra glue so newPkiIssueCmd can reuse it for
// the local CA+request convenience flow.
func signLeaf(
	csr *x509.CertificateRequest,
	caCert *x509.Certificate,
	caKey *ecdsa.PrivateKey,
	days int,
) (der []byte, serial *big.Int, err error) {
	serial, err = randomSerial()
	if err != nil {
		return nil, nil, fmt.Errorf("mint leaf serial: %w", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      csr.Subject,
		NotBefore:    now.Add(-5 * time.Minute), // small skew tolerance
		NotAfter:     now.AddDate(0, 0, days),

		KeyUsage: x509.KeyUsageDigitalSignature,
		// Peer certs are used BOTH as a server (when accepting
		// inbound /__peer/call requests) and as a client (when
		// dialing out). Both EKUs are required — omitting one
		// breaks mTLS in one direction.
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
			x509.ExtKeyUsageClientAuth,
		},

		// SANs from the CSR, verbatim. The CN drives identity
		// matching (AllowedClients); SANs drive TLS hostname
		// verification on the dial side. Both come from operator
		// intent expressed at request time.
		DNSNames:    csr.DNSNames,
		IPAddresses: csr.IPAddresses,
	}
	der, err = x509.CreateCertificate(rand.Reader, tmpl, caCert, csr.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("CA sign: %w", err)
	}
	return der, serial, nil
}

// newPkiIssueCmd builds `nexus pki issue` — convenience subcommand
// for the bootstrap case where the CA + the peer live on the same
// host. Effectively `request` followed by `sign` against a CSR
// that's never written to disk.
//
// For production, prefer the split flow (request on peer, sign on
// CA host) so private keys never traverse the network.
func newPkiIssueCmd(stdout, stderr io.Writer) *cobra.Command {
	var (
		caDir string
		out   string
		cn    string
		dns   []string
		ips   []string
		days  int
	)
	cmd := &cobra.Command{
		Use:   "issue",
		Short: "Convenience: generate keypair + sign locally (CA and peer colocated)",
		Long: `Generate a keypair AND sign it locally — for bootstrapping
when the CA and the peer live on the same host.

Writes:
  <out>/<cn>.key  — private key (0600)
  <out>/<cn>.crt  — signed leaf cert (0644)

For production mTLS rollout, prefer 'nexus pki request' on the peer
followed by 'nexus pki sign' on the CA host — that way the private
key never travels.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cn == "" {
				return errors.New("--cn cannot be empty")
			}
			parsedIPs, err := parseIPList(ips)
			if err != nil {
				return err
			}
			caCert, caKey, err := loadCA(caDir)
			if err != nil {
				return err
			}
			if err := ensureOutDir(out); err != nil {
				return err
			}
			priv, err := genECDSAKey()
			if err != nil {
				return fmt.Errorf("generate keypair: %w", err)
			}
			// Build an in-memory CSR (never serialized to disk)
			// so signLeaf can pull Subject + SANs out of it.
			csrTmpl := &x509.CertificateRequest{
				Subject:     commonSubject(cn),
				DNSNames:    dns,
				IPAddresses: parsedIPs,
			}
			csrDER, err := x509.CreateCertificateRequest(rand.Reader, csrTmpl, priv)
			if err != nil {
				return fmt.Errorf("build in-memory CSR: %w", err)
			}
			csr, err := x509.ParseCertificateRequest(csrDER)
			if err != nil {
				return fmt.Errorf("parse in-memory CSR: %w", err)
			}
			der, serial, err := signLeaf(csr, caCert, caKey, days)
			if err != nil {
				return err
			}
			keyPath := joinOut(out, cn+".key")
			leafPath := joinOut(out, cn+".crt")
			if err := writeKeyPEM(keyPath, priv); err != nil {
				return fmt.Errorf("write %s: %w", keyPath, err)
			}
			if err := writeCertPEM(leafPath, der); err != nil {
				return fmt.Errorf("write %s: %w", leafPath, err)
			}
			fmt.Fprintf(stdout, "wrote %s (key, 0600)\n", keyPath)
			fmt.Fprintf(stdout, "wrote %s (cert, %d-day validity, serial %s)\n",
				leafPath, days, serial.Text(16))
			return nil
		},
	}
	cmd.Flags().StringVar(&caDir, "ca-dir", ".", "directory holding ca.crt + ca.key")
	cmd.Flags().StringVar(&out, "out", ".", "directory to write the leaf key + cert into")
	cmd.Flags().StringVar(&cn, "cn", "", "CommonName — the peer identity matched against AllowedClients")
	cmd.Flags().StringSliceVar(&dns, "dns", nil, "DNS SAN(s) the cert should cover (repeatable)")
	cmd.Flags().StringSliceVar(&ips, "ip", nil, "IP SAN(s) the cert should cover (repeatable)")
	cmd.Flags().IntVar(&days, "days", 180, "leaf cert validity in days (recommended: 180)")
	return cmd
}

