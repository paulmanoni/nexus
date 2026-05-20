package main

import (
	"crypto/rand"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/spf13/cobra"
)

// newPkiRequestCmd builds `nexus pki request` — run on the peer.
// Generates a fresh ECDSA keypair and emits a CSR for the CA to
// sign; the private key NEVER leaves this host.
//
// SANs are read from --dns and --ip flags so a peer cert can be
// presented to multiple hostnames / direct IP addresses without
// re-issuing. The CN drives extension/peer's AllowedClients check;
// SANs drive TLS hostname verification on whichever address the
// peer is reached at.
func newPkiRequestCmd(stdout, stderr io.Writer) *cobra.Command {
	var (
		out  string
		cn   string
		dns  []string
		ips  []string
	)
	cmd := &cobra.Command{
		Use:   "request",
		Short: "Generate a private key + CSR on the peer host (key never travels)",
		Long: `Generate a CSR for the named peer identity.

Writes:
  <out>/<cn>.key  — private key (0600, stays on this host)
  <out>/<cn>.csr  — CSR (0644, ship to the CA host for signing)

Once the CA signs the CSR, copy the returned <cn>.crt + ca.crt
back here. The peer extension reads <cn>.crt + <cn>.key + ca.crt
to terminate mTLS.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cn == "" {
				return errors.New("--cn cannot be empty")
			}
			parsedIPs, err := parseIPList(ips)
			if err != nil {
				return err
			}
			if err := ensureOutDir(out); err != nil {
				return err
			}
			keyPath := joinOut(out, cn+".key")
			csrPath := joinOut(out, cn+".csr")

			priv, err := genECDSAKey()
			if err != nil {
				return fmt.Errorf("generate keypair: %w", err)
			}
			tmpl := &x509.CertificateRequest{
				Subject:     commonSubject(cn),
				DNSNames:    dns,
				IPAddresses: parsedIPs,
			}
			der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, priv)
			if err != nil {
				return fmt.Errorf("create CSR: %w", err)
			}
			if err := writeKeyPEM(keyPath, priv); err != nil {
				return fmt.Errorf("write %s: %w", keyPath, err)
			}
			if err := writeCSRPEM(csrPath, der); err != nil {
				return fmt.Errorf("write %s: %w", csrPath, err)
			}
			fmt.Fprintf(stdout, "wrote %s (key, 0600 — keep on this host)\n", keyPath)
			fmt.Fprintf(stdout, "wrote %s (CSR — ship to CA host: 'nexus pki sign --csr %s')\n",
				csrPath, csrPath)
			return nil
		},
	}
	cmd.Flags().StringVar(&out, "out", ".", "directory to write the key + CSR into")
	cmd.Flags().StringVar(&cn, "cn", "", "CommonName — the peer identity matched against AllowedClients")
	cmd.Flags().StringSliceVar(&dns, "dns", nil, "DNS SAN(s) the cert should cover (repeatable)")
	cmd.Flags().StringSliceVar(&ips, "ip", nil, "IP SAN(s) the cert should cover (repeatable)")
	return cmd
}

// parseIPList walks the --ip flag values and parses each into a
// net.IP. Rejects malformed entries up-front so the operator sees
// the bad input now instead of a confusing TLS-handshake error in
// production.
func parseIPList(in []string) ([]net.IP, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]net.IP, 0, len(in))
	for _, s := range in {
		ip := net.ParseIP(s)
		if ip == nil {
			return nil, fmt.Errorf("--ip %q is not a valid IP address", s)
		}
		out = append(out, ip)
	}
	return out, nil
}
