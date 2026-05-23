package nexus

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/paulmanoni/nexus/manifest"
)

// resolveEffectiveManifest builds the base manifest from declared
// inputs, merges per-environment overrides for the active environment,
// runs boot-time validation, and caches the result on the App so
// /__nexus/manifest and future provenance endpoints can serve a single
// consistent view. Called once from registerLifecycle's OnStart after
// runStartupTasks completes.
//
// The function is split into stages so each step's failure mode is
// clear:
//
//	build  → merge  → validate
//	  │        │        └─ required env vars present, validation rules satisfied
//	  │        └─ override applied (or no-op if no overrides)
//	  └─ all DeclareEnv / DeclareSecret / etc. invokes have run by now
//
// On any error returns a *UserError so the boot failure surfaces with
// op / hint / cause structure rather than a flat string.
func (a *App) resolveEffectiveManifest() error {
	base := manifest.Build(a.manifestInputs())

	env := a.environment
	// If the app didn't declare the active environment, MergeOverrides
	// would reject. Inject a synthetic Environment entry so an app that
	// uses the cloud contract loosely (declares overrides for some envs
	// but not all) still boots — the merge then is a no-op deep copy.
	if env != "" && !environmentDeclared(base.Environments, env) {
		if len(base.Overrides) > 0 {
			// Overrides exist for some env: insist on the active one
			// being declared. Reject with a clear message so the
			// operator sees the typo / misconfig.
			return &UserError{
				Op:   "manifest.resolve",
				Msg:  fmt.Sprintf("active environment %q is not declared in manifest.environments", env),
				Hint: fmt.Sprintf("add it via app.DeclareEnvironment(manifest.Environment{Name: %q}) — declared envs: %v", env, manifest.AvailableEnvironments(base)),
			}
		}
		// No overrides at all → treat the active env as a no-op
		// passthrough by synthesizing a minimal Environment entry so
		// MergeOverrides accepts it.
		base.Environments = append(base.Environments, manifest.Environment{Name: env})
	}

	effective, err := manifest.MergeOverrides(base, env)
	if err != nil {
		return &UserError{
			Op:    "manifest.resolve",
			Msg:   fmt.Sprintf("override merge failed for environment %q", env),
			Cause: err,
			Hint:  "fix the override in nexus.toml (or DeclareOverride) — check manifest.Lint() for write-time validation",
		}
	}

	if err := validateEffectiveEnv(effective); err != nil {
		return err
	}
	if err := validateEffectiveSecrets(effective); err != nil {
		return err
	}

	a.manifest.mu.Lock()
	a.manifest.effective = &effective
	a.manifest.mu.Unlock()
	return nil
}

// validateEffectiveEnv checks every declared env var against the
// effective manifest's expectations: required vars must be set in the
// process env (or have a default), and any value present must satisfy
// the Validation block. Returns the FIRST error so the operator can
// fix one problem at a time (lint reports all-at-once at write time;
// boot is fail-fast).
func validateEffectiveEnv(m manifest.Manifest) error {
	for _, e := range m.Env {
		value, set := lookupEnvValue(e.Name)
		// Empty env var is treated as missing — a deliberate empty
		// string from the platform doesn't satisfy Required, and
		// `unset` and `set to ""` should fail validation the same way.
		if !set || value == "" {
			if e.Required && e.Default == "" && e.BoundTo == "" {
				return &UserError{
					Op:   "manifest.validate",
					Msg:  fmt.Sprintf("required env var %s is not set", e.Name),
					Hint: fmt.Sprintf("set %s in the platform / .env / shell, OR declare a Default", e.Name),
				}
			}
			value = e.Default
		}
		if value == "" {
			continue
		}
		if msg := checkValue(value, e.Validation); msg != "" {
			return &UserError{
				Op:   "manifest.validate",
				Msg:  fmt.Sprintf("env %s: %s", e.Name, msg),
				Hint: "loosen the validation rule, OR set a value that satisfies it",
			}
		}
	}
	return nil
}

// validateEffectiveSecrets mirrors validateEffectiveEnv for secrets.
// Secrets are conventionally surfaced as env vars at runtime (the
// platform writes them in), so the same lookupEnvValue path applies.
func validateEffectiveSecrets(m manifest.Manifest) error {
	for _, s := range m.Secrets {
		value, set := lookupEnvValue(s.Name)
		if !set || value == "" {
			if s.Required {
				return &UserError{
					Op:   "manifest.validate",
					Msg:  fmt.Sprintf("required secret %s is not set", s.Name),
					Hint: fmt.Sprintf("provide via the platform's secret store, OR set %s in the local shell for dev", s.Name),
				}
			}
			continue
		}
		if msg := checkValue(value, s.Validation); msg != "" {
			return &UserError{
				Op:  "manifest.validate",
				Msg: fmt.Sprintf("secret %s: %s", s.Name, msg),
			}
		}
	}
	return nil
}

// checkValue applies a Validation rule set to a single string value.
// Returns "" on success or a human-readable rule violation. Numeric
// Min/Max are interpreted only when the value parses as an int; non-
// numeric values just skip those rules so a free-form string isn't
// accidentally penalized.
func checkValue(value string, v *manifest.EnvValidation) string {
	if v == nil {
		return ""
	}
	if len(v.Enum) > 0 {
		ok := false
		for _, e := range v.Enum {
			if e == value {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Sprintf("value %q not in enum %v", value, v.Enum)
		}
	}
	if v.Regex != "" {
		re, err := regexp.Compile(v.Regex)
		if err == nil && !re.MatchString(value) {
			return fmt.Sprintf("value %q does not match regex %q", value, v.Regex)
		}
	}
	if v.Length != nil {
		if v.Length.Min != nil && len(value) < *v.Length.Min {
			return fmt.Sprintf("value has length %d, below length.min %d", len(value), *v.Length.Min)
		}
		if v.Length.Max != nil && len(value) > *v.Length.Max {
			return fmt.Sprintf("value has length %d, above length.max %d", len(value), *v.Length.Max)
		}
	}
	return ""
}

// lookupEnvValue is a thin wrapper around os.LookupEnv. Centralized
// so a future test seam or platform shim can intercept without
// touching every call site.
func lookupEnvValue(name string) (string, bool) {
	return os.LookupEnv(name)
}

// environmentDeclared mirrors manifest.environmentDeclared but is
// duplicated here to avoid exporting the manifest helper just for one
// internal call.
func environmentDeclared(envs []manifest.Environment, name string) bool {
	for _, e := range envs {
		if e.Name == name {
			return true
		}
	}
	return false
}

// EffectiveManifest returns the merged + validated manifest produced
// at boot. Returns nil before fx.Start completes — callers serving
// /__nexus/manifest fall back to manifest.Build(manifestInputs()) so
// print mode and pre-boot inspection still work.
func (a *App) EffectiveManifest() *manifest.Manifest {
	a.manifest.mu.Lock()
	defer a.manifest.mu.Unlock()
	if a.manifest.effective == nil {
		return nil
	}
	// Return a shallow copy so callers can't mutate the cached value.
	// Slices inside are shared — they're already-finalized read-only
	// snapshots; cloning every nested struct would be wasteful for the
	// JSON-serialize-then-discard usage at /__nexus/manifest.
	cp := *a.manifest.effective
	return &cp
}

// formatEnvList is a small helper for error messages that need to
// list declared env vars without spamming the whole manifest.
func formatEnvList(envs []manifest.EnvVar) string {
	names := make([]string, 0, len(envs))
	for _, e := range envs {
		names = append(names, e.Name)
	}
	return strings.Join(names, ", ")
}
