package main

import (
	"crypto/rand"
	"crypto/x509"
	"fmt"
	"io"
	"net"

	"github.com/spf13/cobra"
)

// newPkiRequestCmd builds `nexus pki request` — run on the peer.
// Generates a fresh ECDSA keypair and emits a CSR for the CA to
// sign; the private key NEVER leaves this host.
//
// SANs are read from --dns-name and --ip-address so a peer cert can be
// presented to multiple hostnames / direct IP addresses without
// re-issuing. The CN drives extension/peer's AllowedClients check;
// SANs drive TLS hostname verification on whichever address the
// peer is reached at.
func newPkiRequestCmd(stdout, stderr io.Writer) *cobra.Command {
	var (
		out       string
		cn        string
		dns       []string
		ips       []string
		dnsLegacy []string
		ipsLegacy []string
	)
	cmd := &cobra.Command{
		Use:   "request",
		Args:  cobra.NoArgs,
		Short: "Generate a private key + CSR on the peer host (key never travels)",
		Long: `Generate a CSR for the named peer identity.

Writes:
  <out>/<cn>.key  — private key (0600, stays on this host)
  <out>/<cn>.csr  — CSR (0644, ship to the CA host for signing)

Once the CA signs the CSR, copy the returned <cn>.crt + ca.crt
back here. The peer extension reads <cn>.crt + <cn>.key + ca.crt
to terminate mTLS.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := nonEmptyFlag("cn", cn); err != nil {
				return err
			}
			dns = append(dns, dnsLegacy...)
			ips = append(ips, ipsLegacy...)
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
	cmd.Flags().StringVarP(&out, "out", "o", ".", "directory to write the key + CSR into")
	cmd.Flags().StringVar(&cn, "cn", "", "CommonName — the peer identity matched against AllowedClients")
	cmd.Flags().StringSliceVar(&dns, "dns-name", nil, "DNS SAN the cert should cover (repeatable)")
	cmd.Flags().StringSliceVar(&ips, "ip-address", nil, "IP SAN the cert should cover (repeatable)")
	_ = cmd.MarkFlagRequired("cn")
	registerSANAliases(cmd, &dnsLegacy, &ipsLegacy)
	return cmd
}

// registerSANAliases keeps the pre-rename --dns / --ip spellings
// working after --dns-name / --ip-address became canonical.
//
// The old names are registered as their OWN flags bound to their own
// slices, then merged into the canonical slices inside RunE. Binding
// both spellings to a single variable would be a last-parsed-wins
// bug: each pflag value carries its own "changed" bool, so the
// second spelling to parse REPLACES the first one's slice instead of
// appending to it.
//
// A Flags().SetNormalizeFunc alias would be tidier, but it is not
// durable here — cobra's AddCommand pushes the root command's global
// normalization func down into every child (command.go
// SetGlobalNormalizationFunc), overwriting a per-command one. The
// root already installs a global normalizer for --output → --out, so
// anything set locally would be silently clobbered.
func registerSANAliases(cmd *cobra.Command, dnsLegacy, ipsLegacy *[]string) {
	cmd.Flags().StringSliceVar(dnsLegacy, "dns", nil, "deprecated alias for --dns-name")
	cmd.Flags().StringSliceVar(ipsLegacy, "ip", nil, "deprecated alias for --ip-address")
	// MarkDeprecated also hides the flag, so help output only ever
	// advertises the canonical spelling.
	_ = cmd.Flags().MarkDeprecated("dns", "use --dns-name instead")
	_ = cmd.Flags().MarkDeprecated("ip", "use --ip-address instead")
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
