package manifest

import (
	"fmt"
	"sort"
	"strings"
)

// MergeOverrides applies the per-environment Override block to the
// base Manifest's inputs (Env / Secrets / Files / Services / Hooks)
// and returns the effective manifest for env. The returned value is
// a deep copy of base with diffs applied and Overrides cleared —
// callers can treat it as the resolved contract for the active
// environment.
//
// The function is pure: same (base, env) always produces the same
// output. Both the orchestration platform (pre-rendering values
// before deploy) and the framework (self-hosted boot resolution)
// call into this single implementation so values stay byte-equivalent.
//
// Resolution rules (from the inputs.go doc):
//
//   - Scalar lock (Override.Env[KEY] = "warn") replaces the effective
//     default and marks the field locked.
//   - Spec diff (Override.EnvSpecs[KEY] = {Default: warn}) merges into
//     the base EnvVar field by field; absent (nil) pointers inherit.
//   - Removal (Override.Removed = ["secrets.STRIPE_API_KEY"]) drops the
//     entry from the effective slice.
//   - Service deep-merge (Override.Services[name] = {Size: large,
//     ExposeAs: {password: DB_PASSWORD}}) replaces named fields and
//     adds new ExposeAs map entries; unmentioned base fields inherit.
//   - Hooks block (Override.Hooks) wholesale replaces base.Hooks for
//     this environment when non-nil. Nil = inherit.
//
// Errors:
//
//   - env not declared in base.Environments → ErrUnknownEnvironment
//     (so a typo doesn't silently no-op).
//   - Override key absent from base inputs → ErrUnknownOverrideKey
//     (overrides may only adjust declared inputs, never introduce
//     them — keeps the base contract authoritative).
//   - Override.Env scalar lock on a key that also has a spec diff →
//     ErrConflictingOverride.
//   - Removed entry that doesn't exist in base → ErrUnknownOverrideKey
//     (consistency with the other rejections).
func MergeOverrides(base Manifest, env string) (Manifest, error) {
	out := deepCopyManifest(base)
	out.Overrides = nil // effective manifest has no diffs left

	if env == "" {
		return out, nil
	}

	if !environmentDeclared(base.Environments, env) {
		return Manifest{}, &MergeError{
			Code: ErrUnknownEnvironment,
			Msg:  fmt.Sprintf("environment %q is not declared in manifest.environments", env),
		}
	}

	ov, ok := base.Overrides[env]
	if !ok {
		// No overrides for this environment — effective = base copy.
		return out, nil
	}

	// Conflict check: same key in scalar lock and spec diff.
	for key := range ov.Env {
		if _, dup := ov.EnvSpecs[key]; dup {
			return Manifest{}, &MergeError{
				Code: ErrConflictingOverride,
				Msg:  fmt.Sprintf("env %q has both a scalar lock and a spec diff for %q — pick one", env, key),
			}
		}
	}

	// Apply env scalar locks: replace Default and mark Source=override.
	for key, locked := range ov.Env {
		idx, found := findEnvVarIndex(out.Env, key)
		if !found {
			return Manifest{}, &MergeError{
				Code: ErrUnknownOverrideKey,
				Msg:  fmt.Sprintf("override env.%s does not match any declared input", key),
			}
		}
		out.Env[idx].Default = locked
		out.Env[idx].Source = "override"
	}

	// Apply env spec diffs: field-by-field merge into the EnvVar.
	for key, patch := range ov.EnvSpecs {
		idx, found := findEnvVarIndex(out.Env, key)
		if !found {
			return Manifest{}, &MergeError{
				Code: ErrUnknownOverrideKey,
				Msg:  fmt.Sprintf("override envSpecs.%s does not match any declared input", key),
			}
		}
		applyEnvVarPatch(&out.Env[idx], patch)
	}

	// Apply secret spec diffs.
	for key, patch := range ov.SecretSpecs {
		idx, found := findSecretIndex(out.Secrets, key)
		if !found {
			return Manifest{}, &MergeError{
				Code: ErrUnknownOverrideKey,
				Msg:  fmt.Sprintf("override secretSpecs.%s does not match any declared secret", key),
			}
		}
		applySecretPatch(&out.Secrets[idx], patch)
	}

	// Apply file spec diffs.
	for key, patch := range ov.FileSpecs {
		idx, found := findFileIndex(out.Files, key)
		if !found {
			return Manifest{}, &MergeError{
				Code: ErrUnknownOverrideKey,
				Msg:  fmt.Sprintf("override fileSpecs.%s does not match any declared file", key),
			}
		}
		applyFilePatch(&out.Files[idx], patch)
	}

	// Apply service diffs.
	for name, patch := range ov.Services {
		idx, found := findServiceIndex(out.Services, name)
		if !found {
			return Manifest{}, &MergeError{
				Code: ErrUnknownOverrideKey,
				Msg:  fmt.Sprintf("override services.%s does not match any declared service", name),
			}
		}
		applyServicePatch(&out.Services[idx], patch)
	}

	// Apply removals (dotted "<bucket>.<key>" paths).
	for _, path := range ov.Removed {
		bucket, key, ok := splitRemovalPath(path)
		if !ok {
			return Manifest{}, &MergeError{
				Code: ErrUnknownOverrideKey,
				Msg:  fmt.Sprintf("override removed entry %q is malformed (want \"<bucket>.<key>\")", path),
			}
		}
		removed, err := removeInput(&out, bucket, key)
		if err != nil {
			return Manifest{}, err
		}
		if !removed {
			return Manifest{}, &MergeError{
				Code: ErrUnknownOverrideKey,
				Msg:  fmt.Sprintf("override removed entry %q does not match any declared input", path),
			}
		}
	}

	// Hooks: wholesale replace when override carries a non-nil Hooks.
	if ov.Hooks != nil {
		hooksCopy := *ov.Hooks
		out.Hooks = &hooksCopy
	}

	// TLS: field-by-field patch. If the base has no TLS block but
	// the override provides one, the patch promotes to a full block
	// for this environment — useful when only one environment needs
	// TLS (e.g. production-only HTTPS with staging on a private LB).
	if ov.TLS != nil {
		if out.TLS == nil {
			out.TLS = &TLSBlock{}
		}
		applyTLSPatch(out.TLS, ov.TLS)
	}

	// CORS: same merge shape as TLS — promote a missing base block
	// when the override provides one, then patch in place.
	if ov.CORS != nil {
		if out.CORS == nil {
			out.CORS = &CORSBlock{}
		}
		applyCORSPatch(out.CORS, ov.CORS)
	}

	// Errors: same promote-then-patch shape. Common override is
	// SampleRate (preview gets 0.1) or Disabled (silence an env).
	if ov.Errors != nil {
		if out.Errors == nil {
			out.Errors = &ErrorsBlock{}
		}
		applyErrorsPatch(out.Errors, ov.Errors)
	}

	return out, nil
}

// --- patch helpers ------------------------------------------------------

func applyEnvVarPatch(e *EnvVar, p EnvVarPatch) {
	if p.Description != nil {
		e.Description = *p.Description
	}
	if p.Required != nil {
		e.Required = *p.Required
	}
	if p.Default != nil {
		e.Default = *p.Default
	}
	if p.EnvScoped != nil {
		e.EnvScoped = *p.EnvScoped
	}
	if p.Validation != nil {
		e.Validation = cloneValidation(p.Validation)
	}
}

func applySecretPatch(s *Secret, p SecretPatch) {
	if p.Description != nil {
		s.Description = *p.Description
	}
	if p.Required != nil {
		s.Required = *p.Required
	}
	if p.EnvScoped != nil {
		s.EnvScoped = *p.EnvScoped
	}
	if p.RotateAlert != nil {
		s.RotateAlert = *p.RotateAlert
	}
	if p.Validation != nil {
		s.Validation = cloneValidation(p.Validation)
	}
}

func applyFilePatch(f *File, p FilePatch) {
	if p.Description != nil {
		f.Description = *p.Description
	}
	if p.Mode != nil {
		f.Mode = *p.Mode
	}
	if p.Secret != nil {
		f.Secret = *p.Secret
	}
	if p.EnvScoped != nil {
		f.EnvScoped = *p.EnvScoped
	}
}

func applyServicePatch(s *ServiceNeed, p ServicePatch) {
	if p.Version != nil {
		s.Version = *p.Version
	}
	if p.Optional != nil {
		s.Optional = *p.Optional
	}
	// ExposeAs deep-merges by key; override keys add to / overwrite
	// base entries, unmentioned base keys inherit.
	if len(p.ExposeAs) > 0 {
		if s.ExposeAs == nil {
			s.ExposeAs = make(map[string]string, len(p.ExposeAs))
		}
		for k, v := range p.ExposeAs {
			s.ExposeAs[k] = v
		}
	}
	if p.Size != nil {
		s.Size = *p.Size
	}
	if p.Backup != nil {
		s.Backup = *p.Backup
	}
	if p.Ephemeral != nil {
		s.Ephemeral = *p.Ephemeral
	}
}

// applyErrorsPatch merges an ErrorsPatch into the effective
// ErrorsBlock. Scalar fields use pointer-set semantics; IgnorePaths
// is a list-replace. Same shape as the CORS patch (kept side-by-side
// deliberately).
func applyErrorsPatch(b *ErrorsBlock, p *ErrorsPatch) {
	if p.Environment != nil {
		b.Environment = *p.Environment
	}
	if p.Release != nil {
		b.Release = *p.Release
	}
	if p.ServerName != nil {
		b.ServerName = *p.ServerName
	}
	if p.Capacity != nil {
		b.Capacity = *p.Capacity
	}
	if p.SampleRate != nil {
		v := *p.SampleRate
		b.SampleRate = &v
	}
	if p.IgnorePaths != nil {
		b.IgnorePaths = append([]string(nil), p.IgnorePaths...)
	}
	if p.Disabled != nil {
		b.Disabled = *p.Disabled
	}
}

// applyCORSPatch merges a CORSPatch into the effective CORS block.
// Slice fields list-replace (non-nil means "fully replace"), scalar
// fields use pointer-set semantics so the override can flip a value
// while leaving it absent means "inherit". Same shape as
// applyTLSPatch — kept in sync deliberately.
func applyCORSPatch(b *CORSBlock, p *CORSPatch) {
	if p.AllowOrigins != nil {
		b.AllowOrigins = append([]string(nil), p.AllowOrigins...)
	}
	if p.AllowMethods != nil {
		b.AllowMethods = append([]string(nil), p.AllowMethods...)
	}
	if p.AllowHeaders != nil {
		b.AllowHeaders = append([]string(nil), p.AllowHeaders...)
	}
	if p.ExposeHeaders != nil {
		b.ExposeHeaders = append([]string(nil), p.ExposeHeaders...)
	}
	if p.AllowCredentials != nil {
		v := *p.AllowCredentials
		b.AllowCredentials = &v
	}
	if p.MaxAge != nil {
		b.MaxAge = *p.MaxAge
	}
	if p.Disabled != nil {
		b.Disabled = *p.Disabled
	}
}

// applyTLSPatch merges a TLSPatch into the effective TLS block. Field
// semantics:
//
//   - Domains: nil → inherit base; non-nil → fully replace. An empty
//     slice is meaningful — it means "no domains in this env",
//     equivalent to disabling the plugin without touching Disabled.
//   - Email / CacheDir: pointer-set, non-nil replaces.
//   - Redirect / Staging / Disabled: pointer-set, non-nil replaces.
//
// The defensive copy of the Domains slice keeps the override map's
// slice from sharing a backing array with the merged manifest.
func applyTLSPatch(t *TLSBlock, p *TLSPatch) {
	if p.Domains != nil {
		t.Domains = append([]string(nil), p.Domains...)
	}
	if p.Email != nil {
		t.Email = *p.Email
	}
	if p.CacheDir != nil {
		t.CacheDir = *p.CacheDir
	}
	if p.Redirect != nil {
		r := *p.Redirect
		t.Redirect = &r
	}
	if p.Staging != nil {
		t.Staging = *p.Staging
	}
	if p.Disabled != nil {
		t.Disabled = *p.Disabled
	}
}

func cloneValidation(v *EnvValidation) *EnvValidation {
	if v == nil {
		return nil
	}
	out := *v
	if v.Enum != nil {
		out.Enum = append([]string(nil), v.Enum...)
	}
	if v.Min != nil {
		min := *v.Min
		out.Min = &min
	}
	if v.Max != nil {
		max := *v.Max
		out.Max = &max
	}
	if v.Length != nil {
		out.Length = &Range{}
		if v.Length.Min != nil {
			min := *v.Length.Min
			out.Length.Min = &min
		}
		if v.Length.Max != nil {
			max := *v.Length.Max
			out.Length.Max = &max
		}
	}
	return &out
}

// --- removal -----------------------------------------------------------

func splitRemovalPath(p string) (bucket, key string, ok bool) {
	i := strings.IndexByte(p, '.')
	if i <= 0 || i == len(p)-1 {
		return "", "", false
	}
	return p[:i], p[i+1:], true
}

func removeInput(m *Manifest, bucket, key string) (bool, error) {
	switch bucket {
	case "env":
		if idx, found := findEnvVarIndex(m.Env, key); found {
			m.Env = append(m.Env[:idx], m.Env[idx+1:]...)
			return true, nil
		}
	case "secrets":
		if idx, found := findSecretIndex(m.Secrets, key); found {
			m.Secrets = append(m.Secrets[:idx], m.Secrets[idx+1:]...)
			return true, nil
		}
	case "files":
		if idx, found := findFileIndex(m.Files, key); found {
			m.Files = append(m.Files[:idx], m.Files[idx+1:]...)
			return true, nil
		}
	case "services":
		if idx, found := findServiceIndex(m.Services, key); found {
			m.Services = append(m.Services[:idx], m.Services[idx+1:]...)
			return true, nil
		}
	case "volumes":
		for i, v := range m.Volumes {
			if v.Path == key {
				m.Volumes = append(m.Volumes[:i], m.Volumes[i+1:]...)
				return true, nil
			}
		}
	default:
		return false, &MergeError{
			Code: ErrUnknownOverrideKey,
			Msg:  fmt.Sprintf("override removed bucket %q is not one of env|secrets|files|services|volumes", bucket),
		}
	}
	return false, nil
}

// --- lookups -----------------------------------------------------------

func findEnvVarIndex(s []EnvVar, name string) (int, bool) {
	for i, e := range s {
		if e.Name == name {
			return i, true
		}
	}
	return -1, false
}

func findSecretIndex(s []Secret, name string) (int, bool) {
	for i, e := range s {
		if e.Name == name {
			return i, true
		}
	}
	return -1, false
}

func findFileIndex(s []File, name string) (int, bool) {
	for i, e := range s {
		if e.Name == name {
			return i, true
		}
	}
	return -1, false
}

func findServiceIndex(s []ServiceNeed, name string) (int, bool) {
	for i, e := range s {
		if e.Name == name {
			return i, true
		}
	}
	return -1, false
}

func environmentDeclared(envs []Environment, name string) bool {
	for _, e := range envs {
		if e.Name == name {
			return true
		}
	}
	return false
}

// --- deep copy ---------------------------------------------------------

// deepCopyManifest produces a fully independent Manifest so callers
// can mutate the result without leaking back into the base. We keep
// this scoped to fields the merger actually touches; sections the
// merger never writes to (Modules, Routes, Entities, etc.) are
// shallow-copied for speed — same reference semantics those callers
// already rely on.
func deepCopyManifest(m Manifest) Manifest {
	out := m

	if m.Env != nil {
		out.Env = make([]EnvVar, len(m.Env))
		for i, e := range m.Env {
			out.Env[i] = e
			out.Env[i].Validation = cloneValidation(e.Validation)
		}
	}
	if m.Secrets != nil {
		out.Secrets = make([]Secret, len(m.Secrets))
		for i, s := range m.Secrets {
			out.Secrets[i] = s
			out.Secrets[i].Validation = cloneValidation(s.Validation)
		}
	}
	if m.Files != nil {
		out.Files = make([]File, len(m.Files))
		copy(out.Files, m.Files)
	}
	if m.Services != nil {
		out.Services = make([]ServiceNeed, len(m.Services))
		for i, s := range m.Services {
			out.Services[i] = s
			if s.ExposeAs != nil {
				out.Services[i].ExposeAs = make(map[string]string, len(s.ExposeAs))
				for k, v := range s.ExposeAs {
					out.Services[i].ExposeAs[k] = v
				}
			}
		}
	}
	if m.Volumes != nil {
		out.Volumes = make([]Volume, len(m.Volumes))
		copy(out.Volumes, m.Volumes)
	}
	if m.Environments != nil {
		out.Environments = make([]Environment, len(m.Environments))
		copy(out.Environments, m.Environments)
	}
	if m.Hooks != nil {
		h := *m.Hooks
		out.Hooks = &h
	}
	if m.TLS != nil {
		t := *m.TLS
		if m.TLS.Domains != nil {
			t.Domains = append([]string(nil), m.TLS.Domains...)
		}
		if m.TLS.Redirect != nil {
			r := *m.TLS.Redirect
			t.Redirect = &r
		}
		out.TLS = &t
	}
	if m.CORS != nil {
		c := *m.CORS
		if m.CORS.AllowOrigins != nil {
			c.AllowOrigins = append([]string(nil), m.CORS.AllowOrigins...)
		}
		if m.CORS.AllowMethods != nil {
			c.AllowMethods = append([]string(nil), m.CORS.AllowMethods...)
		}
		if m.CORS.AllowHeaders != nil {
			c.AllowHeaders = append([]string(nil), m.CORS.AllowHeaders...)
		}
		if m.CORS.ExposeHeaders != nil {
			c.ExposeHeaders = append([]string(nil), m.CORS.ExposeHeaders...)
		}
		if m.CORS.AllowCredentials != nil {
			v := *m.CORS.AllowCredentials
			c.AllowCredentials = &v
		}
		out.CORS = &c
	}
	if m.Errors != nil {
		e := *m.Errors
		if m.Errors.IgnorePaths != nil {
			e.IgnorePaths = append([]string(nil), m.Errors.IgnorePaths...)
		}
		if m.Errors.SampleRate != nil {
			v := *m.Errors.SampleRate
			e.SampleRate = &v
		}
		out.Errors = &e
	}
	return out
}

// AvailableEnvironments returns the names of every declared
// environment in stable sorted order. Useful for CLI / lint output.
func AvailableEnvironments(m Manifest) []string {
	out := make([]string, 0, len(m.Environments))
	for _, e := range m.Environments {
		out = append(out, e.Name)
	}
	sort.Strings(out)
	return out
}

// --- error type --------------------------------------------------------

// ErrCode classifies merge failures. Stable identifiers so the lint
// step and the CLI can format error messages consistently.
type ErrCode string

const (
	ErrUnknownEnvironment   ErrCode = "unknown_environment"
	ErrUnknownOverrideKey   ErrCode = "unknown_override_key"
	ErrConflictingOverride  ErrCode = "conflicting_override"
	ErrInvalidValidation    ErrCode = "invalid_validation"
	ErrTypeMismatch         ErrCode = "type_mismatch"
	ErrMissingDeclaration   ErrCode = "missing_declaration"
)

// MergeError carries a code + human message. Implements error so
// callers can type-assert or just print.
type MergeError struct {
	Code ErrCode
	Msg  string
}

func (e *MergeError) Error() string {
	return fmt.Sprintf("manifest: %s: %s", e.Code, e.Msg)
}