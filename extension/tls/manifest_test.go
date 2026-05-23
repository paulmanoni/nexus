package tls

import (
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/manifest"
)

// TestResolveConfig_NoManifest is the v1 path — operator passed Config
// directly, no nexus.toml tls: block. Pure in-code config
// flows through unchanged after applyDefaults.
func TestResolveConfig_NoManifest(t *testing.T) {
	t.Parallel()
	in := Config{
		Domains: []string{"app.example.com"},
		Email:   "ops@example.com",
	}
	out, disabled, err := resolveConfig(in, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if disabled {
		t.Errorf("disabled=true; want false (no manifest)")
	}
	if out.HTTPSPort != 443 {
		t.Errorf("defaults not applied: HTTPSPort=%d", out.HTTPSPort)
	}
}

// TestResolveConfig_ManifestProvidesEverything is the cloud-native
// path — operator passes empty Config; manifest holds all of it.
// This is the killer demo from a config-management perspective:
// "redeploy with a different env, get different TLS config, never
// touched the binary."
func TestResolveConfig_ManifestProvidesEverything(t *testing.T) {
	t.Parallel()
	red := true
	mf := &manifest.Manifest{
		TLS: &manifest.TLSBlock{
			Domains:  []string{"prod.example.com", "www.example.com"},
			Email:    "ops@example.com",
			CacheDir: "/var/lib/nexus/certs",
			Redirect: &red,
		},
	}

	out, disabled, err := resolveConfig(Config{}, mf)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if disabled {
		t.Errorf("disabled=true; want false")
	}
	if len(out.Domains) != 2 || out.Domains[0] != "prod.example.com" {
		t.Errorf("Domains: got %v, want [prod.example.com www.example.com]", out.Domains)
	}
	if out.Email != "ops@example.com" {
		t.Errorf("Email: got %q, want ops@example.com", out.Email)
	}
	if out.CacheDir != "/var/lib/nexus/certs" {
		t.Errorf("CacheDir: got %q, want /var/lib/nexus/certs", out.CacheDir)
	}
}

// TestResolveConfig_ManifestOverridesInCode validates the precedence
// rule documented on the package: manifest wins where it sets a
// value, in-code Config wins for fields the manifest leaves empty.
// Mixing the two is the migration path — keep a long-lived Email in
// code, let the manifest swap domains per environment.
func TestResolveConfig_ManifestOverridesInCode(t *testing.T) {
	t.Parallel()
	in := Config{
		Domains: []string{"in-code-fallback.example.com"},
		Email:   "in-code@example.com",
	}
	mf := &manifest.Manifest{
		TLS: &manifest.TLSBlock{
			Domains: []string{"manifest-wins.example.com"},
			// Email omitted — should keep in-code value.
		},
	}

	out, _, err := resolveConfig(in, mf)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got := out.Domains[0]; got != "manifest-wins.example.com" {
		t.Errorf("Domains: got %q, want manifest-wins.example.com", got)
	}
	if out.Email != "in-code@example.com" {
		t.Errorf("Email: got %q, want in-code@example.com (preserved)", out.Email)
	}
}

// TestResolveConfig_DisabledSkipsValidation locks in the deliberate
// choice: a disabled env should not fail validation. The operator's
// "this env doesn't want TLS" intent should always win over
// half-filled config that would otherwise reject.
func TestResolveConfig_DisabledSkipsValidation(t *testing.T) {
	t.Parallel()
	mf := &manifest.Manifest{
		TLS: &manifest.TLSBlock{Disabled: true},
	}
	// Note: empty Config — would normally fail validate()
	out, disabled, err := resolveConfig(Config{}, mf)
	if err != nil {
		t.Fatalf("disabled env should bypass validation; got %v", err)
	}
	if !disabled {
		t.Error("disabled=false; want true")
	}
	_ = out // not used past the flag
}

// TestResolveConfig_ManifestWildcardRejected ensures wildcards still
// get caught when they come from the manifest — operators editing
// YAML need the same protection operators writing Go get.
func TestResolveConfig_ManifestWildcardRejected(t *testing.T) {
	t.Parallel()
	mf := &manifest.Manifest{
		TLS: &manifest.TLSBlock{
			Domains: []string{"*.example.com"},
			Email:   "ops@example.com",
		},
	}
	_, _, err := resolveConfig(Config{}, mf)
	if err == nil || !strings.Contains(err.Error(), "wildcard") {
		t.Fatalf("want wildcard error from manifest, got %v", err)
	}
}

// TestMergeOverrides_TLSPatch_DomainsReplace covers the operator's
// most common edit: production has app.example.com, staging
// environment_overrides swaps to staging.example.com. After
// MergeOverrides("staging"), the effective Manifest.TLS.Domains is
// exactly the staging set — no merging of slices.
func TestMergeOverrides_TLSPatch_DomainsReplace(t *testing.T) {
	t.Parallel()
	red := true
	base := manifest.Manifest{
		SchemaVersion: "1",
		App:           manifest.AppIdentity{Name: "demo"},
		Name:          "demo",
		Environments: []manifest.Environment{
			{Name: "production"},
			{Name: "staging"},
		},
		TLS: &manifest.TLSBlock{
			Domains:  []string{"app.example.com"},
			Email:    "ops@example.com",
			Redirect: &red,
		},
		Overrides: map[string]manifest.Override{
			"staging": {
				TLS: &manifest.TLSPatch{
					Domains: []string{"staging.example.com"},
				},
			},
		},
	}

	prod, err := manifest.MergeOverrides(base, "production")
	if err != nil {
		t.Fatalf("merge prod: %v", err)
	}
	if got := prod.TLS.Domains[0]; got != "app.example.com" {
		t.Errorf("prod Domains[0]: got %q, want app.example.com", got)
	}

	staging, err := manifest.MergeOverrides(base, "staging")
	if err != nil {
		t.Fatalf("merge staging: %v", err)
	}
	if len(staging.TLS.Domains) != 1 || staging.TLS.Domains[0] != "staging.example.com" {
		t.Errorf("staging Domains: got %v, want [staging.example.com]", staging.TLS.Domains)
	}
	// Email should inherit from base since staging didn't override.
	if staging.TLS.Email != "ops@example.com" {
		t.Errorf("staging Email: got %q, want ops@example.com (inherited)", staging.TLS.Email)
	}
}

// TestMergeOverrides_TLSPatch_DisabledFlipsEnv exercises the
// "production has TLS, preview is behind cloud LB" pattern — same
// binary, manifest opts the preview env out without touching the
// in-code Plugin() call.
func TestMergeOverrides_TLSPatch_DisabledFlipsEnv(t *testing.T) {
	t.Parallel()
	disabled := true
	base := manifest.Manifest{
		SchemaVersion: "1",
		App:           manifest.AppIdentity{Name: "demo"},
		Name:          "demo",
		Environments: []manifest.Environment{
			{Name: "production"},
			{Name: "preview"},
		},
		TLS: &manifest.TLSBlock{
			Domains: []string{"app.example.com"},
			Email:   "ops@example.com",
		},
		Overrides: map[string]manifest.Override{
			"preview": {
				TLS: &manifest.TLSPatch{Disabled: &disabled},
			},
		},
	}

	preview, err := manifest.MergeOverrides(base, "preview")
	if err != nil {
		t.Fatalf("merge preview: %v", err)
	}
	if !preview.TLS.Disabled {
		t.Errorf("preview Disabled: got false, want true")
	}
	// Domains should still be there (inherit) — Disabled is a runtime
	// flag, not a removal.
	if len(preview.TLS.Domains) != 1 {
		t.Errorf("preview Domains: got %v, want inherited [app.example.com]", preview.TLS.Domains)
	}
}
