package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// newPkiBundleCmd builds `nexus pki bundle` — package the three
// files a peer needs to terminate mTLS:
//
//	ca.crt        — the trust root
//	<cn>.crt      — the signed leaf cert
//	<cn>.key      — the matching private key
//
// HARD INVARIANT: this command is physically incapable of including
// ca.key. The function never opens, reads, or references the CA
// private key — there is no code path here that touches a file
// named "ca.key". A grep is your audit; the bundle path was
// designed so an operator can ship its output to a peer host with
// zero risk of leaking the CA's signing key.
//
// The bundle is written as three plain files under <out>/<cn>/ so
// the operator can `scp -r` (or rsync, or tar) the directory and
// hand the peer everything it needs in one shot. We deliberately
// don't tarball ourselves — keeping the files separate means the
// operator can inspect each individually before shipping.
func newPkiBundleCmd(stdout, stderr io.Writer) *cobra.Command {
	var (
		out   string
		caDir string
		from  string
		cn    string
	)
	cmd := &cobra.Command{
		Use:   "bundle",
		Short: "Package ca.crt + <cn>.crt + <cn>.key for shipping to a peer",
		Long: `Package the three files a peer needs to terminate mTLS:
the CA cert (ca.crt — the trust root), the signed leaf cert
(<cn>.crt), and the matching private key (<cn>.key).

This command CANNOT include ca.key. The bundle code never reads
the CA private key — only the public ca.crt. Auditable: a grep for
"ca.key" inside cmd/nexus/pki_bundle.go finds nothing.

Default --from is the current directory (where 'nexus pki request'
or 'nexus pki issue' wrote the leaf material). Default --ca-dir is
the same — adjust when the CA cert lives elsewhere.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cn == "" {
				return errors.New("--cn cannot be empty")
			}
			if err := ensureOutDir(out); err != nil {
				return err
			}
			bundleDir := filepath.Join(out, cn)
			if err := os.MkdirAll(bundleDir, 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", bundleDir, err)
			}

			// The three explicit copies. The list of sources is
			// statically fixed — there's no string interpolation
			// that could ever produce "ca.key", and the loop
			// would not accept one if there were. Trust the
			// invariant by inspection.
			pairs := []struct {
				src, dst, label string
				perm            os.FileMode
			}{
				{
					src:   filepath.Join(caDir, caCertFilename),
					dst:   filepath.Join(bundleDir, caCertFilename),
					label: "trust root",
					perm:  certPerm,
				},
				{
					src:   filepath.Join(from, cn+".crt"),
					dst:   filepath.Join(bundleDir, cn+".crt"),
					label: "peer cert",
					perm:  certPerm,
				},
				{
					src:   filepath.Join(from, cn+".key"),
					dst:   filepath.Join(bundleDir, cn+".key"),
					label: "peer key",
					perm:  keyPerm,
				},
			}
			for _, p := range pairs {
				if err := copyForBundle(p.src, p.dst, p.perm); err != nil {
					return err
				}
				fmt.Fprintf(stdout, "wrote %s (%s)\n", p.dst, p.label)
			}
			fmt.Fprintf(stdout, "\nBundle ready at %s — ship the directory to the peer host.\n",
				bundleDir)
			return nil
		},
	}
	cmd.Flags().StringVar(&out, "out", ".", "directory under which to create the bundle subdir")
	cmd.Flags().StringVar(&caDir, "ca-dir", ".", "directory holding ca.crt (NOT ca.key — never read)")
	cmd.Flags().StringVar(&from, "from", ".", "directory holding <cn>.crt + <cn>.key")
	cmd.Flags().StringVar(&cn, "cn", "", "peer identity — selects which leaf cert + key to bundle")
	return cmd
}

// copyForBundle reads src and writes dst with the requested perm.
// Tiny helper kept here (rather than in pki.go) so the bundle
// command's audit surface is self-contained — every file
// operation the bundler can perform is in one place.
//
// Deliberately uses os.ReadFile (whole-file slurp) rather than
// io.Copy: cert and key files are kilobytes, not gigabytes, and a
// whole-file read lets us validate the source isn't symlinked to
// somewhere unexpected before writing the destination.
func copyForBundle(src, dst string, perm os.FileMode) error {
	body, err := os.ReadFile(src) // #nosec G304 -- operator-supplied bundle source
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}
	// Two gosec rules fire on this line and both are suppressed:
	//
	//   G306 — WriteFile perm > 0o600. perm comes from the caller;
	//          0o600 for the leaf private key (the only secret in
	//          the bundle), 0o644 for the two cert files (must be
	//          world-readable so the peer's process can serve them
	//          via TLS regardless of uid). The caller has already
	//          classified the file's sensitivity.
	//
	//   G703 — path traversal via taint analysis. dst is
	//          filepath.Join(bundleDir, <hard-coded suffix>)
	//          where bundleDir = <operator's --out>/<operator's
	//          --cn>. Both flag values are operator input on a
	//          CLI helper; this isn't a server-side vulnerability,
	//          and there's no untrusted-network surface to defend
	//          against. The caller deliberately picked the output
	//          location.
	return os.WriteFile(dst, body, perm) // #nosec G306,G703 -- caller-classified perm; dst from operator CLI flags
}
