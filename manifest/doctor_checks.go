package manifest

import (
	"fmt"
	"strings"
)

// Each function in this file implements one diagnostic. Keep them
// pure: in → out, no globals, no I/O. The check signature is fixed
// (manifest.Check); each registered check appears in
// manifest.builtinChecks.
//
// Naming convention: check<What> — present tense, describes what
// the check verifies (not what it warns about). Reads well at the
// registry call site.
//
// Severity guidance:
//   ERROR    — the binary will misbehave or fail to boot.
//              Operator must fix before deploy.
//   WARNING  — surprising / probably wrong, but not blocking.
//              Operator should review.
// Doctor has no Info severity by design; reduce ambiguity.

// ── 1. Required env / secret without a source ──────────────────
//
// An input declared Required: true with no Default and no BoundTo
// means the platform MUST inject the value, or the binary refuses
// to boot. Without a BoundTo pointing at a service field, the
// platform doesn't know what env name to inject — so the operator
// must remember to set it manually.
//
// Warning, not error: a savvy operator may know they'll set it via
// the platform UI / .env. But for cloud-platform consumers, this
// is exactly the "I deployed and it crashed at boot" footgun.
func checkRequiredWithoutSource(m Manifest) []Finding {
	var out []Finding
	for _, e := range m.Env {
		if !e.Required || e.Default != "" || e.BoundTo != "" || e.Source == "platform" {
			continue
		}
		out = append(out, Finding{
			Severity: SeverityWarning,
			Code:     "required_env_no_source",
			Path:     "env." + e.Name,
			Message:  fmt.Sprintf("Required env var %s has no Default and no BoundTo — operator must inject it at deploy time or the binary refuses to boot.", e.Name),
			Hint:     fmt.Sprintf("set a Default for dev fallback, OR add bound_to: <service>.<field> so the platform fills it automatically"),
		})
	}
	for _, s := range m.Secrets {
		if !s.Required {
			continue
		}
		out = append(out, Finding{
			Severity: SeverityWarning,
			Code:     "required_secret_advisory",
			Path:     "secrets." + s.Name,
			Message:  fmt.Sprintf("Secret %s is required — confirm the platform's secret store has it set for every active environment.", s.Name),
			Hint:     "use `nexus cloud secret set` (or your platform's UI) for each env declared in environments[]",
		})
	}
	return out
}

// ── 2. BoundTo points at a real service.field ──────────────────
//
// EnvVar.BoundTo is "<service>.<field>" dotted notation. If the
// service name doesn't exist, the platform can't fill the env at
// deploy — the binary boots, sees the var unset, and may fail
// validation or run with a default that wasn't intended.
//
// Error because the misconfiguration is silent at deploy and only
// surfaces as an unrelated runtime failure (DB unreachable, etc.).
func checkBoundToTargetExists(m Manifest) []Finding {
	if len(m.Env) == 0 || len(m.Services) == 0 {
		return nil
	}
	services := map[string]struct{}{}
	for _, s := range m.Services {
		services[s.Name] = struct{}{}
	}
	var out []Finding
	for _, e := range m.Env {
		if e.BoundTo == "" {
			continue
		}
		svc, field, ok := splitDotted(e.BoundTo)
		if !ok {
			out = append(out, Finding{
				Severity: SeverityError,
				Code:     "boundto_malformed",
				Path:     "env." + e.Name + ".boundTo",
				Message:  fmt.Sprintf("boundTo %q is malformed (want \"<service>.<field>\").", e.BoundTo),
				Hint:     `examples: "main_db.url", "cache.host"`,
			})
			continue
		}
		if _, found := services[svc]; !found {
			out = append(out, Finding{
				Severity: SeverityError,
				Code:     "boundto_unknown_service",
				Path:     "env." + e.Name + ".boundTo",
				Message:  fmt.Sprintf("boundTo references service %q which is not declared in services[].", svc),
				Hint:     fmt.Sprintf("add it via app.DeclareService(manifest.ServiceNeed{Name: %q, ...}), OR fix the bound_to value to reference an existing service.", svc),
			})
		}
		_ = field
	}
	return out
}

// ── 3. Service has at least one expose_as entry ────────────────
//
// A ServiceNeed without ExposeAs is a platform-side puzzle: the
// platform provisions the service (Postgres, Redis, ...) but the
// app gives no instructions for which env vars to fill with the
// connection details. The binary will boot with the service
// unreachable.
//
// Warning, not error: some workflows pass connection info via a
// file or out-of-band (e.g. Kubernetes secret mount). Most don't.
func checkServiceHasExposeAs(m Manifest) []Finding {
	var out []Finding
	for _, s := range m.Services {
		if s.Optional {
			continue
		}
		if len(s.ExposeAs) == 0 {
			out = append(out, Finding{
				Severity: SeverityWarning,
				Code:     "service_no_expose_as",
				Path:     "services." + s.Name,
				Message:  fmt.Sprintf("Service %s (%s) declares no expose_as — the platform won't know which env vars to fill with connection details.", s.Name, s.Kind),
				Hint:     `add expose_as: { url: <ENV_NAME>, ... } so the platform-injected env vars match what your code reads`,
			})
		}
	}
	return out
}

// ── 4. Override target environments are declared ───────────────
//
// Already enforced at merge time (MergeOverrides returns
// ErrUnknownEnvironment) but doctor surfaces it pre-merge so the
// operator sees the typo before running anything. Mirrors lint's
// "unknown environment" check but lives in doctor for consistency
// when running doctor without lint.
func checkOverrideEnvironmentsDeclared(m Manifest) []Finding {
	if len(m.Overrides) == 0 {
		return nil
	}
	declared := map[string]struct{}{}
	for _, e := range m.Environments {
		declared[e.Name] = struct{}{}
	}
	var out []Finding
	for env := range m.Overrides {
		if _, ok := declared[env]; !ok {
			out = append(out, Finding{
				Severity: SeverityError,
				Code:     "override_unknown_environment",
				Path:     "overrides." + env,
				Message:  fmt.Sprintf("Override targets environment %q but no such environment is declared in environments[].", env),
				Hint:     fmt.Sprintf("declare it (`environments: { %s: {...} }`), OR remove the override block.", env),
			})
		}
	}
	return out
}

// ── 5. Required secrets carry a length/regex Validation ────────
//
// Secrets (API keys, signing keys) are operator-supplied strings;
// without a Validation rule the binary boots even if someone pastes
// an empty value or a typo. A minimum length is the cheapest
// guardrail.
//
// Warning, not error: the operator may have other controls (the
// platform's secret store may enforce its own minimum). But the
// declaration site is the canonical place to express intent.
func checkSecretRequiredHasValidation(m Manifest) []Finding {
	var out []Finding
	for _, s := range m.Secrets {
		if !s.Required {
			continue
		}
		if s.Validation == nil || (s.Validation.Length == nil && s.Validation.Regex == "" && len(s.Validation.Enum) == 0) {
			out = append(out, Finding{
				Severity: SeverityWarning,
				Code:     "secret_no_validation",
				Path:     "secrets." + s.Name,
				Message:  fmt.Sprintf("Required secret %s has no Validation — a typo or empty value passes the boot check.", s.Name),
				Hint:     "add validation: { length: { min: 32 } } for signing keys, or { regex: '^sk_(test|live)_' } for known-format keys",
			})
		}
	}
	return out
}

// ── 6. Services with no env var binding to them ────────────────
//
// Inverse of check #2: every service should have at least one env
// var with BoundTo pointing at it, otherwise the app's code path
// for that service has no way to receive the connection details.
//
// Warning, not error: rare cases use a wrapper that reads service
// info from a side channel. But the typical setup wires env vars
// for everything, and an unbound service is a strong "did you
// forget?" signal.
func checkServicesWithoutEnv(m Manifest) []Finding {
	if len(m.Services) == 0 {
		return nil
	}
	bound := map[string]struct{}{}
	for _, e := range m.Env {
		if svc, _, ok := splitDotted(e.BoundTo); ok {
			bound[svc] = struct{}{}
		}
	}
	// Services that declare ExposeAs implicitly bind through it
	// (the platform fills those env vars by name, without BoundTo).
	// Skip them.
	var out []Finding
	for _, s := range m.Services {
		if len(s.ExposeAs) > 0 {
			continue
		}
		if _, ok := bound[s.Name]; ok {
			continue
		}
		out = append(out, Finding{
			Severity: SeverityWarning,
			Code:     "service_unbound",
			Path:     "services." + s.Name,
			Message:  fmt.Sprintf("Service %s (%s) has no expose_as AND no env var binds to it — your code has no way to receive connection details.", s.Name, s.Kind),
			Hint:     "add expose_as on the service entry, OR add bound_to on the env vars that should be filled with this service's connection info",
		})
	}
	return out
}

// ── 7. Overrides only adjust inputs that exist in base ─────────
//
// Same rule MergeOverrides enforces at merge time. Surfaced here
// so a doctor run against a YAML-only source (no print mode)
// catches the same problem ahead of build.
//
// Iterates every override key (env / envSpecs / secretSpecs /
// fileSpecs / services / removed) and confirms the target exists
// in the base manifest.
func checkOverridesReferenceUnknownInputs(m Manifest) []Finding {
	if len(m.Overrides) == 0 {
		return nil
	}
	envNames := nameSetFrom(m.Env, func(e EnvVar) string { return e.Name })
	secretNames := nameSetFrom(m.Secrets, func(s Secret) string { return s.Name })
	fileNames := nameSetFrom(m.Files, func(f File) string { return f.Name })
	serviceNames := nameSetFrom(m.Services, func(s ServiceNeed) string { return s.Name })

	var out []Finding
	for envName, ov := range m.Overrides {
		base := "overrides." + envName

		for key := range ov.Env {
			if _, ok := envNames[key]; !ok {
				out = append(out, unknownOverride(base+".env."+key, "env", key))
			}
		}
		for key := range ov.EnvSpecs {
			if _, ok := envNames[key]; !ok {
				out = append(out, unknownOverride(base+".envSpecs."+key, "env", key))
			}
		}
		for key := range ov.SecretSpecs {
			if _, ok := secretNames[key]; !ok {
				out = append(out, unknownOverride(base+".secretSpecs."+key, "secrets", key))
			}
		}
		for key := range ov.FileSpecs {
			if _, ok := fileNames[key]; !ok {
				out = append(out, unknownOverride(base+".fileSpecs."+key, "files", key))
			}
		}
		for key := range ov.Services {
			if _, ok := serviceNames[key]; !ok {
				out = append(out, unknownOverride(base+".services."+key, "services", key))
			}
		}
	}
	return out
}

// ── helpers ──────────────────────────────────────────────────

func splitDotted(s string) (head, tail string, ok bool) {
	i := strings.IndexByte(s, '.')
	if i <= 0 || i == len(s)-1 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

func nameSetFrom[T any](items []T, name func(T) string) map[string]struct{} {
	out := make(map[string]struct{}, len(items))
	for _, it := range items {
		out[name(it)] = struct{}{}
	}
	return out
}

func unknownOverride(path, bucket, key string) Finding {
	return Finding{
		Severity: SeverityError,
		Code:     "override_unknown_key",
		Path:     path,
		Message:  fmt.Sprintf("Override key %s.%s is not declared in the base manifest.", bucket, key),
		Hint:     fmt.Sprintf("either add %s to the base %s block, or remove this override entry.", key, bucket),
	}
}
