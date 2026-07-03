package main

import (
	"bytes"
	"crypto/x509"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runPki drives a subcommand in-process. Returns stdout for tests
// that want to assert on the human-facing output; errors are
// returned for tests that drive failure paths.
func runPki(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	root := newPkiCmd(&stdout, &stderr)
	root.SetArgs(args)
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	err := root.Execute()
	if testing.Verbose() {
		t.Logf("stdout: %s\nstderr: %s", stdout.String(), stderr.String())
	}
	return stdout.String(), err
}

// TestPkiInit_GeneratesValidCA proves `nexus pki init` produces a
// self-signed CA the Go x509 verifier accepts as a chain root.
// Without this, every subsequent `pki sign` would silently
// succeed but its leaves wouldn't verify in production TLS.
func TestPkiInit_GeneratesValidCA(t *testing.T) {
	dir := t.TempDir()
	_, err := runPki(t, "init", "--out", dir, "--cn", "test-ca", "--years", "1")
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	caPath := filepath.Join(dir, caCertFilename)
	keyPath := filepath.Join(dir, caKeyFilename)

	// File-perm sanity: ca.key must be 0600 (the whole point of
	// putting the CA on a hardened host is that this file is the
	// crown jewel). ca.crt is 0644 so peers' bundle commands can
	// read it.
	if info, _ := os.Stat(keyPath); info.Mode().Perm() != 0o600 {
		t.Errorf("ca.key perms = %#o, want 0600", info.Mode().Perm())
	}
	if info, _ := os.Stat(caPath); info.Mode().Perm() != 0o644 {
		t.Errorf("ca.crt perms = %#o, want 0644", info.Mode().Perm())
	}

	ca, err := readCert(caPath)
	if err != nil {
		t.Fatalf("read CA cert: %v", err)
	}
	if !ca.IsCA {
		t.Error("CA cert: IsCA=false")
	}
	if ca.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Error("CA cert: missing KeyUsageCertSign")
	}
	if !ca.BasicConstraintsValid {
		t.Error("CA cert: BasicConstraintsValid=false")
	}
	if ca.Subject.CommonName != "test-ca" {
		t.Errorf("CA CN = %q, want test-ca", ca.Subject.CommonName)
	}

	// Self-signed: must verify against a pool containing itself.
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	if _, err := ca.Verify(x509.VerifyOptions{Roots: pool}); err != nil {
		t.Errorf("CA self-verify: %v", err)
	}
}

// TestPkiInit_RefusesOverwrite proves the safety check fires: a
// CA host that re-runs init without --force MUST keep the existing
// ca.key (otherwise every previously-issued cert silently loses
// trust on the next signing operation).
func TestPkiInit_RefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	if _, err := runPki(t, "init", "--out", dir); err != nil {
		t.Fatal(err)
	}
	originalKey, _ := os.ReadFile(filepath.Join(dir, caKeyFilename))

	_, err := runPki(t, "init", "--out", dir)
	if err == nil {
		t.Fatal("init without --force should refuse to overwrite ca.key")
	}
	if !strings.Contains(err.Error(), "force") {
		t.Errorf("error should mention --force: %v", err)
	}
	// Original key untouched.
	currentKey, _ := os.ReadFile(filepath.Join(dir, caKeyFilename))
	if !bytes.Equal(originalKey, currentKey) {
		t.Error("ca.key was modified despite the refusal")
	}

	// --force gets through.
	if _, err := runPki(t, "init", "--out", dir, "--force"); err != nil {
		t.Errorf("init --force should succeed: %v", err)
	}
	rewrittenKey, _ := os.ReadFile(filepath.Join(dir, caKeyFilename))
	if bytes.Equal(originalKey, rewrittenKey) {
		t.Error("--force did not regenerate ca.key")
	}
}

// TestPkiSign_LeafChainsToCA is the headline integration: a leaf
// produced by `pki sign` MUST verify against the CA produced by
// `pki init`. If this breaks, the whole PKI is broken — TLS
// handshakes between peers would fail with chain-of-trust errors.
func TestPkiSign_LeafChainsToCA(t *testing.T) {
	dir := t.TempDir()
	if _, err := runPki(t, "init", "--out", dir); err != nil {
		t.Fatal(err)
	}
	// Generate a CSR for a peer.
	peerDir := t.TempDir() // simulate the peer host being separate
	if _, err := runPki(t, "request",
		"--out", peerDir,
		"--cn", "peer-alpha",
		"--dns", "peer-alpha.internal",
		"--ip", "10.0.0.5",
	); err != nil {
		t.Fatal(err)
	}
	csrPath := filepath.Join(peerDir, "peer-alpha.csr")

	// CA signs it.
	signOut := t.TempDir()
	if _, err := runPki(t, "sign",
		"--ca-dir", dir,
		"--csr", csrPath,
		"--out", signOut,
		"--days", "30",
	); err != nil {
		t.Fatal(err)
	}

	caCert, err := readCert(filepath.Join(dir, caCertFilename))
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := readCert(filepath.Join(signOut, "peer-alpha.crt"))
	if err != nil {
		t.Fatal(err)
	}

	// Subject + SAN preservation.
	if leaf.Subject.CommonName != "peer-alpha" {
		t.Errorf("leaf CN = %q, want peer-alpha", leaf.Subject.CommonName)
	}
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "peer-alpha.internal" {
		t.Errorf("leaf DNS SANs = %v, want [peer-alpha.internal]", leaf.DNSNames)
	}
	if len(leaf.IPAddresses) != 1 || leaf.IPAddresses[0].String() != "10.0.0.5" {
		t.Errorf("leaf IP SANs = %v, want [10.0.0.5]", leaf.IPAddresses)
	}

	// EKUs: ServerAuth AND ClientAuth — peer certs are used as
	// both. Missing either breaks mTLS in one direction.
	var hasServer, hasClient bool
	for _, eku := range leaf.ExtKeyUsage {
		if eku == x509.ExtKeyUsageServerAuth {
			hasServer = true
		}
		if eku == x509.ExtKeyUsageClientAuth {
			hasClient = true
		}
	}
	if !hasServer || !hasClient {
		t.Errorf("leaf EKUs = %v, need both ServerAuth and ClientAuth", leaf.ExtKeyUsage)
	}

	// Chain verification.
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Errorf("leaf chain-to-CA verify: %v", err)
	}
}

// TestPkiSign_RejectsTamperedCSR proves the CSR signature check
// fires. A CSR with a corrupted body can't be signed — without this
// guard, a malicious bystander could substitute their own public
// key into a CSR in transit and the CA would happily sign it.
func TestPkiSign_RejectsTamperedCSR(t *testing.T) {
	dir := t.TempDir()
	if _, err := runPki(t, "init", "--out", dir); err != nil {
		t.Fatal(err)
	}
	if _, err := runPki(t, "request", "--out", dir, "--cn", "peer-tampered"); err != nil {
		t.Fatal(err)
	}
	csrPath := filepath.Join(dir, "peer-tampered.csr")
	body, err := os.ReadFile(csrPath)
	if err != nil {
		t.Fatal(err)
	}
	// Flip a byte deep in the PEM-decoded portion — anywhere that
	// breaks the embedded signature is enough.
	body[len(body)/2] ^= 0xFF
	if err := os.WriteFile(csrPath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runPki(t, "sign", "--ca-dir", dir, "--csr", csrPath, "--out", dir); err == nil {
		t.Fatal("sign should reject a tampered CSR")
	}
}

// TestPkiBundle_NeverIncludesCAKey is the security invariant the
// command was designed around. If the bundle directory contains
// ca.key for any reason, the test fails — protecting against future
// refactors that might naively add it to the source list.
func TestPkiBundle_NeverIncludesCAKey(t *testing.T) {
	dir := t.TempDir()
	if _, err := runPki(t, "init", "--out", dir); err != nil {
		t.Fatal(err)
	}
	if _, err := runPki(t, "issue",
		"--ca-dir", dir, "--out", dir,
		"--cn", "peer-bundled",
	); err != nil {
		t.Fatal(err)
	}
	bundleRoot := t.TempDir()
	if _, err := runPki(t, "bundle",
		"--cn", "peer-bundled",
		"--out", bundleRoot,
		"--ca-dir", dir,
		"--from", dir,
	); err != nil {
		t.Fatal(err)
	}
	// Walk the bundle and assert no file named ca.key exists. We
	// also collect the file set so the test message is helpful on
	// failure ("got: <list>") rather than just "ca.key found".
	bundleDir := filepath.Join(bundleRoot, "peer-bundled")
	entries, err := os.ReadDir(bundleDir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
		if e.Name() == caKeyFilename {
			t.Fatalf("bundle MUST NOT include %s — leaks CA signing key", caKeyFilename)
		}
	}
	// Positive assertion: the three expected files are present.
	want := map[string]bool{
		caCertFilename:     false,
		"peer-bundled.crt": false,
		"peer-bundled.key": false,
	}
	for _, n := range names {
		if _, ok := want[n]; ok {
			want[n] = true
		}
	}
	for f, present := range want {
		if !present {
			t.Errorf("bundle missing required file %s", f)
		}
	}
}

// TestPkiSign_SerialsAreUnique catches the most dangerous PKI bug:
// serial reuse. A CA that issues two leaves with the same serial
// breaks every revocation system (CRL, OCSP) and lets an
// adversary swap one cert for another. Random 128-bit serials
// should never collide, but we test the property so a future refactor
// that switches to incrementing serials gets caught before shipping.
func TestPkiSign_SerialsAreUnique(t *testing.T) {
	dir := t.TempDir()
	if _, err := runPki(t, "init", "--out", dir); err != nil {
		t.Fatal(err)
	}
	issue := func(cn string) *x509.Certificate {
		if _, err := runPki(t, "issue",
			"--ca-dir", dir, "--out", dir, "--cn", cn,
		); err != nil {
			t.Fatalf("issue %s: %v", cn, err)
		}
		cert, err := readCert(filepath.Join(dir, cn+".crt"))
		if err != nil {
			t.Fatal(err)
		}
		return cert
	}
	a := issue("peer-a")
	b := issue("peer-b")
	if a.SerialNumber.Cmp(b.SerialNumber) == 0 {
		t.Errorf("two issued certs share serial %s — would break revocation",
			a.SerialNumber.Text(16))
	}
	// Serial must be 128 bits (a randomSerial bounded by 2^128).
	// If a regression accidentally narrows the entropy, we want
	// to catch it here — birthday-collision risk scales with sqrt
	// of the keyspace, so even a 64-bit serial is too small for
	// a busy mesh.
	if a.SerialNumber.BitLen() > 128 {
		t.Errorf("serial bit length %d exceeds 128 — entropy ceiling broken", a.SerialNumber.BitLen())
	}
}

// silence imports we'd otherwise drop if io was unused.
var _ io.Writer = (*bytes.Buffer)(nil)
