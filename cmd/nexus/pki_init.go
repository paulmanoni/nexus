package main

import (
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
)

// newPkiInitCmd builds `nexus pki init` — one-time CA setup on the
// host that will sign peer certs. Refuses to overwrite an existing
// ca.key unless --force, because doing so destroys every previously-
// issued leaf cert's trust chain.
//
// The CA is ECDSA P-256, valid for ~10 years (operators almost
// always rotate before that on a real schedule; the long default
// keeps the bootstrap simple and lets short-lived peer certs do
// the actual rotation work). Self-signed with KeyUsageCertSign +
// KeyUsageCRLSign and BasicConstraintsValid + IsCA — the minimum
// the Go verifier needs to accept it as a chain root.
func newPkiInitCmd(stdout, stderr io.Writer) *cobra.Command {
	var (
		out   string
		force bool
		cn    string
		years int
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Generate a new root CA (ca.crt + ca.key)",
		Long: `Generate a self-signed root CA for the peer mesh.

Writes <out>/ca.crt and <out>/ca.key. The private key (ca.key) is
0600 — only the CA-host operator should be able to read it. Refuses
to overwrite an existing ca.key unless --force is passed.

Run this once. Every peer cert subsequently issued chains to this
root; rotating the root means re-bundling every peer.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cn == "" {
				return errors.New("--cn cannot be empty")
			}
			if err := ensureOutDir(out); err != nil {
				return err
			}
			caKeyPath := joinOut(out, caKeyFilename)
			caCertPath := joinOut(out, caCertFilename)
			if !force {
				if _, err := os.Stat(caKeyPath); err == nil {
					return fmt.Errorf("%s already exists — pass --force to overwrite "+
						"(NOTE: overwrites invalidate every peer cert this CA has issued)",
						caKeyPath)
				}
			}

			priv, err := genECDSAKey()
			if err != nil {
				return fmt.Errorf("generate CA key: %w", err)
			}
			serial, err := randomSerial()
			if err != nil {
				return fmt.Errorf("mint CA serial: %w", err)
			}
			now := time.Now()
			tmpl := &x509.Certificate{
				SerialNumber:          serial,
				Subject:               pkix.Name{CommonName: cn},
				NotBefore:             now.Add(-5 * time.Minute), // tolerate small client clock skew
				NotAfter:              now.AddDate(years, 0, 0),
				KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
				IsCA:                  true,
				BasicConstraintsValid: true,
				// Limit chain depth: peer certs are leaves, so the
				// CA never needs to sign intermediate CAs. MaxPathLen=0
				// rejects any chain that tries to add one.
				MaxPathLenZero: true,
			}
			der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
			if err != nil {
				return fmt.Errorf("create self-signed CA cert: %w", err)
			}
			if err := writeCertPEM(caCertPath, der); err != nil {
				return fmt.Errorf("write %s: %w", caCertPath, err)
			}
			if err := writeKeyPEM(caKeyPath, priv); err != nil {
				return fmt.Errorf("write %s: %w", caKeyPath, err)
			}
			fmt.Fprintf(stdout, "wrote %s (cert, %d-year validity, serial %s)\n",
				caCertPath, years, serial.Text(16))
			fmt.Fprintf(stdout, "wrote %s (key, 0600 — keep on CA host only)\n", caKeyPath)
			return nil
		},
	}
	cmd.Flags().StringVar(&out, "out", ".", "directory to write ca.crt + ca.key into")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing ca.key")
	cmd.Flags().StringVar(&cn, "cn", "nexus-peer-ca", "CommonName for the root CA's Subject")
	cmd.Flags().IntVar(&years, "years", 10, "CA validity in years")
	return cmd
}
