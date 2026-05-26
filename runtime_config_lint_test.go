package nexus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLintRuntimeBlock_CleanConfigNoIssues: a properly-formed
// runtime block produces zero issues. Locks in the "lint stays
// quiet when there's nothing to complain about" contract.
func TestLintRuntimeBlock_CleanConfigNoIssues(t *testing.T) {
	b := RuntimeConfigBlock{
		Server: ServerConfigBlock{
			Addr: ":8080",
			Listeners: map[string]ListenerConfigBlock{
				"admin": {Addr: "127.0.0.1:7000", Scope: "admin"},
			},
		},
		Dashboard: DashboardConfigBlock{Enabled: true, Name: "Test App"},
		IntrospectionNetworks: []string{
			"127.0.0.0/8",
			"10.0.0.0/8",
		},
		Middleware: MiddlewareConfigBlock{
			CORS: &CORSConfigBlock{
				AllowOrigins: []string{"https://app.example.com"},
				MaxAge:       "12h",
			},
			RateLimit: &RateLimitConfigBlock{RPM: 600, Burst: 50},
		},
		GraphQL: GraphQLConfigBlock{DocumentCacheSize: 1024},
	}
	issues := lintRuntimeBlock(b)
	if len(issues) != 0 {
		t.Errorf("expected no issues on clean config, got:\n%+v", issues)
	}
}

// TestLintRuntimeBlock_BadAddr: addr without a port is rejected
// with the field path in the message so operators know where
// to look.
func TestLintRuntimeBlock_BadAddr(t *testing.T) {
	b := RuntimeConfigBlock{
		Server: ServerConfigBlock{Addr: "not-a-host-port"},
	}
	issues := lintRuntimeBlock(b)
	if len(issues) == 0 {
		t.Fatal("expected an error for invalid addr")
	}
	if issues[0].Path != "runtime.server.addr" {
		t.Errorf("path = %q, want runtime.server.addr", issues[0].Path)
	}
}

// TestLintRuntimeBlock_BadListenerScope: unknown listener scope
// names get a clear actionable message.
func TestLintRuntimeBlock_BadListenerScope(t *testing.T) {
	b := RuntimeConfigBlock{
		Server: ServerConfigBlock{
			Listeners: map[string]ListenerConfigBlock{
				"weird": {Addr: ":1234", Scope: "private"},
			},
		},
	}
	issues := lintRuntimeBlock(b)
	if len(issues) == 0 {
		t.Fatal("expected error for invalid scope")
	}
	if !strings.Contains(issues[0].Path, "weird") {
		t.Errorf("path should name listener %q, got %q", "weird", issues[0].Path)
	}
}

// TestLintRuntimeBlock_BadCIDR: invalid CIDR in
// IntrospectionNetworks is caught at lint time so it doesn't
// crash the framework at boot.
func TestLintRuntimeBlock_BadCIDR(t *testing.T) {
	b := RuntimeConfigBlock{
		IntrospectionNetworks: []string{"127.0.0.0/8", "garbage"},
	}
	issues := lintRuntimeBlock(b)
	if len(issues) == 0 {
		t.Fatal("expected error for bad CIDR")
	}
	if !strings.Contains(issues[0].Path, "[1]") {
		t.Errorf("path should reference the second entry (index 1), got %q", issues[0].Path)
	}
}

// TestLintRuntimeBlock_BadMaxAge: bad duration string is caught.
func TestLintRuntimeBlock_BadMaxAge(t *testing.T) {
	b := RuntimeConfigBlock{
		Middleware: MiddlewareConfigBlock{
			CORS: &CORSConfigBlock{MaxAge: "not-a-duration"},
		},
	}
	issues := lintRuntimeBlock(b)
	if len(issues) == 0 {
		t.Fatal("expected error for bad duration")
	}
	if issues[0].Path != "runtime.middleware.cors.max_age" {
		t.Errorf("path = %q", issues[0].Path)
	}
}

// TestLintRuntimeBlock_CORSWildcardWithCredentialsWarns: a
// browser-rejected combo (credentials=true + origins=["*"])
// surfaces as a WARNING — not an error, since the framework
// auto-downgrades, but the operator should know about it.
func TestLintRuntimeBlock_CORSWildcardWithCredentialsWarns(t *testing.T) {
	b := RuntimeConfigBlock{
		Middleware: MiddlewareConfigBlock{
			CORS: &CORSConfigBlock{
				AllowOrigins:     []string{"*"},
				AllowCredentials: true,
			},
		},
	}
	issues := lintRuntimeBlock(b)
	if len(issues) == 0 {
		t.Fatal("expected warning for wildcard + credentials")
	}
	if issues[0].Severity != "warning" {
		t.Errorf("severity = %q, want warning", issues[0].Severity)
	}
}

// TestLintRuntimeBlock_NegativeRateLimit: rpm or burst < 0 are
// operator errors (the framework treats them as garbage).
func TestLintRuntimeBlock_NegativeRateLimit(t *testing.T) {
	b := RuntimeConfigBlock{
		Middleware: MiddlewareConfigBlock{
			RateLimit: &RateLimitConfigBlock{RPM: -10, Burst: 5},
		},
	}
	issues := lintRuntimeBlock(b)
	if len(issues) == 0 {
		t.Fatal("expected error for negative rpm")
	}
	if !strings.Contains(issues[0].Path, "rpm") {
		t.Errorf("path should reference rpm field, got %q", issues[0].Path)
	}
}

// TestLintRuntimeFile_RoundTrip: drives the full file-reading
// path the CLI uses. A nexus.toml with a bad scope surfaces a
// proper Issue all the way through.
func TestLintRuntimeFile_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "nexus.toml")
	if err := os.WriteFile(path, []byte(`
[runtime.server.listeners.weird]
addr = ":1234"
scope = "private"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	issues, err := LintRuntimeFile(path)
	if err != nil {
		t.Fatalf("LintRuntimeFile: %v", err)
	}
	if len(issues) == 0 {
		t.Fatal("expected lint issue for bad scope")
	}
}

// TestLintRuntimeFile_FileWithoutRuntimeBlock: a nexus.toml
// that has no [runtime] block at all should produce ZERO
// issues, not error. This lets `nexus lint` call us
// unconditionally on every nexus.toml.
func TestLintRuntimeFile_FileWithoutRuntimeBlock(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "nexus.toml")
	if err := os.WriteFile(path, []byte(`
# Only deploy manifest content, no [runtime] block.
[environments.production]
description = "Prod"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	issues, err := LintRuntimeFile(path)
	if err != nil {
		t.Fatalf("LintRuntimeFile: %v", err)
	}
	if len(issues) != 0 {
		t.Errorf("expected no issues for file without [runtime] block, got: %+v", issues)
	}
}
