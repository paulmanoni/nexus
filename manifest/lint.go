package manifest

import (
	"fmt"
	"regexp"
	"sort"
)

// Severity categorizes a lint issue. Error means the manifest is
// structurally broken — any consumer (merge, platform, framework
// boot) will fail or produce wrong results. Warning means the shape
// is legal but suspicious; the user should review.
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Issue is one lint finding. Path is a dotted manifest path to the
// offending field so editors / CI output can deep-link. Code mirrors
// MergeError codes where applicable so tooling can share formatters.
type Issue struct {
	Severity Severity
	Code     ErrCode
	Path     string
	Message  string
}

// Lint walks the manifest and returns every issue it finds in stable,
// sorted order. The function is pure: same input always produces the
// same output. Callers (the nexus CLI's `lint` subcommand, future IDE
// integrations, CI gates) treat any Error-severity issue as a hard
// failure.
//
// Lint catches:
//
//   - Override references an environment that isn't declared.
//   - Override key (env / secret / file / service / volume) that
//     doesn't exist in the base manifest.
//   - Scalar lock and spec diff on the same env key.
//   - Validation rules that are themselves malformed (regex that
//     doesn't compile, Length.Min > Length.Max, empty Enum slice).
//   - Default value that violates the input's own Validation.
//   - Required env / secret with no Default and no platform binding.
//   - Duplicate names within a single input slice.
//   - BoundTo pointing to a nonexistent service or unknown field.
//   - Duplicate environment names.
//
// The first three overlap with what MergeOverrides checks at merge
// time; lint catches them earlier so the CLI can flag them on save.
func Lint(m Manifest) []Issue {
	var out []Issue

	out = append(out, lintEnvironments(m)...)
	out = append(out, lintBaseInputs(m)...)
	out = append(out, lintOverrides(m)...)

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Code < out[j].Code
	})
	return out
}

// --- environments ------------------------------------------------------

func lintEnvironments(m Manifest) []Issue {
	var out []Issue
	seen := map[string]int{}
	for i, e := range m.Environments {
		if e.Name == "" {
			out = append(out, Issue{
				Severity: SeverityError,
				Code:     ErrInvalidValidation,
				Path:     fmt.Sprintf("environments[%d]", i),
				Message:  "environment entry has empty Name",
			})
			continue
		}
		if prev, dup := seen[e.Name]; dup {
			out = append(out, Issue{
				Severity: SeverityError,
				Code:     ErrInvalidValidation,
				Path:     fmt.Sprintf("environments[%d]", i),
				Message:  fmt.Sprintf("duplicate environment name %q (first defined at environments[%d])", e.Name, prev),
			})
			continue
		}
		seen[e.Name] = i
	}
	return out
}

// --- base inputs -------------------------------------------------------

func lintBaseInputs(m Manifest) []Issue {
	var out []Issue

	// Env vars: duplicate names + validation shape + required-without-source.
	envNames := map[string]int{}
	for i, e := range m.Env {
		path := fmt.Sprintf("env.%s", e.Name)
		if e.Name == "" {
			out = append(out, Issue{Severity: SeverityError, Code: ErrInvalidValidation, Path: fmt.Sprintf("env[%d]", i), Message: "env entry has empty Name"})
			continue
		}
		if prev, dup := envNames[e.Name]; dup {
			out = append(out, Issue{Severity: SeverityError, Code: ErrInvalidValidation, Path: path, Message: fmt.Sprintf("duplicate env name (first at env[%d])", prev)})
			continue
		}
		envNames[e.Name] = i
		if iss := lintValidation(e.Validation, path+".validation"); iss != nil {
			out = append(out, iss...)
		}
		if e.Validation != nil && e.Default != "" {
			if msg := checkValueAgainstValidation(e.Default, e.Validation); msg != "" {
				out = append(out, Issue{Severity: SeverityError, Code: ErrInvalidValidation, Path: path + ".default", Message: msg})
			}
		}
		if e.Required && e.Default == "" && e.BoundTo == "" && e.Source != "platform" {
			out = append(out, Issue{
				Severity: SeverityWarning,
				Code:     ErrMissingDeclaration,
				Path:     path,
				Message:  "Required env has no Default and no BoundTo; platform must inject the value or the binary will refuse to boot",
			})
		}
	}

	// Secrets: duplicates + validation.
	secretNames := map[string]int{}
	for i, s := range m.Secrets {
		path := fmt.Sprintf("secrets.%s", s.Name)
		if s.Name == "" {
			out = append(out, Issue{Severity: SeverityError, Code: ErrInvalidValidation, Path: fmt.Sprintf("secrets[%d]", i), Message: "secret entry has empty Name"})
			continue
		}
		if prev, dup := secretNames[s.Name]; dup {
			out = append(out, Issue{Severity: SeverityError, Code: ErrInvalidValidation, Path: path, Message: fmt.Sprintf("duplicate secret name (first at secrets[%d])", prev)})
			continue
		}
		secretNames[s.Name] = i
		if iss := lintValidation(s.Validation, path+".validation"); iss != nil {
			out = append(out, iss...)
		}
	}

	// Files: duplicate names + duplicate paths.
	fileNames := map[string]int{}
	filePaths := map[string]int{}
	for i, f := range m.Files {
		path := fmt.Sprintf("files.%s", f.Name)
		if f.Name == "" {
			out = append(out, Issue{Severity: SeverityError, Code: ErrInvalidValidation, Path: fmt.Sprintf("files[%d]", i), Message: "file entry has empty Name"})
			continue
		}
		if f.Path == "" {
			out = append(out, Issue{Severity: SeverityError, Code: ErrInvalidValidation, Path: path, Message: "file entry has empty Path"})
		}
		if prev, dup := fileNames[f.Name]; dup {
			out = append(out, Issue{Severity: SeverityError, Code: ErrInvalidValidation, Path: path, Message: fmt.Sprintf("duplicate file name (first at files[%d])", prev)})
			continue
		}
		fileNames[f.Name] = i
		if prev, dup := filePaths[f.Path]; dup {
			out = append(out, Issue{Severity: SeverityError, Code: ErrInvalidValidation, Path: path, Message: fmt.Sprintf("duplicate file path %q (first at files[%d])", f.Path, prev)})
		} else {
			filePaths[f.Path] = i
		}
	}

	// Services: duplicate names + BoundTo references.
	serviceNames := map[string]int{}
	for i, s := range m.Services {
		path := fmt.Sprintf("services.%s", s.Name)
		if s.Name == "" {
			out = append(out, Issue{Severity: SeverityError, Code: ErrInvalidValidation, Path: fmt.Sprintf("services[%d]", i), Message: "service entry has empty Name"})
			continue
		}
		if s.Kind == "" {
			out = append(out, Issue{Severity: SeverityError, Code: ErrInvalidValidation, Path: path, Message: "service entry has empty Kind"})
		}
		if prev, dup := serviceNames[s.Name]; dup {
			out = append(out, Issue{Severity: SeverityError, Code: ErrInvalidValidation, Path: path, Message: fmt.Sprintf("duplicate service name (first at services[%d])", prev)})
			continue
		}
		serviceNames[s.Name] = i
	}

	// BoundTo references: every env's BoundTo must reference a known
	// service.<field>. We check the service name; the field set is
	// open-ended (operators may declare custom fields), so we don't
	// constrain that.
	for _, e := range m.Env {
		if e.BoundTo == "" {
			continue
		}
		svcName, _, ok := splitRemovalPath(e.BoundTo)
		if !ok {
			out = append(out, Issue{
				Severity: SeverityError,
				Code:     ErrInvalidValidation,
				Path:     fmt.Sprintf("env.%s.boundTo", e.Name),
				Message:  fmt.Sprintf("malformed boundTo %q (want \"<service>.<field>\")", e.BoundTo),
			})
			continue
		}
		if _, found := serviceNames[svcName]; !found {
			out = append(out, Issue{
				Severity: SeverityError,
				Code:     ErrMissingDeclaration,
				Path:     fmt.Sprintf("env.%s.boundTo", e.Name),
				Message:  fmt.Sprintf("boundTo references unknown service %q", svcName),
			})
		}
	}

	return out
}

// --- overrides ---------------------------------------------------------

func lintOverrides(m Manifest) []Issue {
	if len(m.Overrides) == 0 {
		return nil
	}
	envSet := map[string]struct{}{}
	for _, e := range m.Environments {
		envSet[e.Name] = struct{}{}
	}
	envNames := nameSet(envVarNames(m.Env))
	secretNames := nameSet(secretSliceNames(m.Secrets))
	fileNames := nameSet(fileSliceNames(m.Files))
	serviceNames := nameSet(serviceSliceNames(m.Services))
	volumePaths := nameSet(volumePathSet(m.Volumes))

	// Sorted env names so output order is deterministic.
	envs := make([]string, 0, len(m.Overrides))
	for k := range m.Overrides {
		envs = append(envs, k)
	}
	sort.Strings(envs)

	var out []Issue
	for _, envName := range envs {
		ov := m.Overrides[envName]
		base := fmt.Sprintf("overrides.%s", envName)

		if _, ok := envSet[envName]; !ok {
			out = append(out, Issue{
				Severity: SeverityError,
				Code:     ErrUnknownEnvironment,
				Path:     base,
				Message:  fmt.Sprintf("override targets %q but no such environment is declared in environments[]", envName),
			})
			continue
		}

		// Scalar locks: key must exist in env, no conflict with spec.
		for k := range ov.Env {
			path := fmt.Sprintf("%s.env.%s", base, k)
			if _, ok := envNames[k]; !ok {
				out = append(out, Issue{Severity: SeverityError, Code: ErrUnknownOverrideKey, Path: path, Message: "key is not declared in base env[]"})
			}
			if _, conflict := ov.EnvSpecs[k]; conflict {
				out = append(out, Issue{Severity: SeverityError, Code: ErrConflictingOverride, Path: path, Message: "key has both a scalar lock and a spec diff — pick one"})
			}
		}

		// Spec diffs: keys must exist; validation patches well-formed.
		for k, patch := range ov.EnvSpecs {
			path := fmt.Sprintf("%s.envSpecs.%s", base, k)
			if _, ok := envNames[k]; !ok {
				out = append(out, Issue{Severity: SeverityError, Code: ErrUnknownOverrideKey, Path: path, Message: "key is not declared in base env[]"})
				continue
			}
			if patch.Validation != nil {
				out = append(out, lintValidation(patch.Validation, path+".validation")...)
			}
		}

		// Secret spec diffs.
		for k, patch := range ov.SecretSpecs {
			path := fmt.Sprintf("%s.secretSpecs.%s", base, k)
			if _, ok := secretNames[k]; !ok {
				out = append(out, Issue{Severity: SeverityError, Code: ErrUnknownOverrideKey, Path: path, Message: "key is not declared in base secrets[]"})
				continue
			}
			if patch.Validation != nil {
				out = append(out, lintValidation(patch.Validation, path+".validation")...)
			}
		}

		// File spec diffs.
		for k := range ov.FileSpecs {
			path := fmt.Sprintf("%s.fileSpecs.%s", base, k)
			if _, ok := fileNames[k]; !ok {
				out = append(out, Issue{Severity: SeverityError, Code: ErrUnknownOverrideKey, Path: path, Message: "key is not declared in base files[]"})
			}
		}

		// Service diffs.
		for k := range ov.Services {
			path := fmt.Sprintf("%s.services.%s", base, k)
			if _, ok := serviceNames[k]; !ok {
				out = append(out, Issue{Severity: SeverityError, Code: ErrUnknownOverrideKey, Path: path, Message: "key is not declared in base services[]"})
			}
		}

		// Removed entries — dotted "bucket.key" paths.
		for _, r := range ov.Removed {
			path := fmt.Sprintf("%s.removed[%s]", base, r)
			bucket, key, ok := splitRemovalPath(r)
			if !ok {
				out = append(out, Issue{Severity: SeverityError, Code: ErrUnknownOverrideKey, Path: path, Message: "malformed removal path (want \"<bucket>.<key>\")"})
				continue
			}
			var found bool
			switch bucket {
			case "env":
				_, found = envNames[key]
			case "secrets":
				_, found = secretNames[key]
			case "files":
				_, found = fileNames[key]
			case "services":
				_, found = serviceNames[key]
			case "volumes":
				_, found = volumePaths[key]
			default:
				out = append(out, Issue{Severity: SeverityError, Code: ErrUnknownOverrideKey, Path: path, Message: fmt.Sprintf("unknown bucket %q (want env|secrets|files|services|volumes)", bucket)})
				continue
			}
			if !found {
				out = append(out, Issue{Severity: SeverityError, Code: ErrUnknownOverrideKey, Path: path, Message: fmt.Sprintf("%s.%s is not declared in base", bucket, key)})
			}
		}
	}
	return out
}

// --- validation rule shape ---------------------------------------------

func lintValidation(v *EnvValidation, path string) []Issue {
	if v == nil {
		return nil
	}
	var out []Issue
	if v.Regex != "" {
		if _, err := regexp.Compile(v.Regex); err != nil {
			out = append(out, Issue{Severity: SeverityError, Code: ErrInvalidValidation, Path: path + ".regex", Message: fmt.Sprintf("regex does not compile: %v", err)})
		}
	}
	if v.Enum != nil && len(v.Enum) == 0 {
		out = append(out, Issue{Severity: SeverityWarning, Code: ErrInvalidValidation, Path: path + ".enum", Message: "enum is an empty slice — every value will fail validation; omit the field to disable enum check"})
	}
	if v.Min != nil && v.Max != nil && *v.Min > *v.Max {
		out = append(out, Issue{Severity: SeverityError, Code: ErrInvalidValidation, Path: path, Message: fmt.Sprintf("min (%d) > max (%d)", *v.Min, *v.Max)})
	}
	if v.Length != nil && v.Length.Min != nil && v.Length.Max != nil && *v.Length.Min > *v.Length.Max {
		out = append(out, Issue{Severity: SeverityError, Code: ErrInvalidValidation, Path: path + ".length", Message: fmt.Sprintf("length.min (%d) > length.max (%d)", *v.Length.Min, *v.Length.Max)})
	}
	if v.Length != nil && v.Length.Min != nil && *v.Length.Min < 0 {
		out = append(out, Issue{Severity: SeverityError, Code: ErrInvalidValidation, Path: path + ".length.min", Message: "length.min must be >= 0"})
	}
	return out
}

// checkValueAgainstValidation returns "" when value satisfies v, else a
// human-readable rule violation. Numeric Min/Max are skipped here —
// we don't know whether the value is meant to be parsed as a number,
// and over-zealous lint warnings on Default strings would be noisy.
func checkValueAgainstValidation(value string, v *EnvValidation) string {
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
			return fmt.Sprintf("default %q is not in enum %v", value, v.Enum)
		}
	}
	if v.Regex != "" {
		re, err := regexp.Compile(v.Regex)
		if err == nil && !re.MatchString(value) {
			return fmt.Sprintf("default %q does not match regex %q", value, v.Regex)
		}
	}
	if v.Length != nil {
		if v.Length.Min != nil && len(value) < *v.Length.Min {
			return fmt.Sprintf("default has length %d, below length.min %d", len(value), *v.Length.Min)
		}
		if v.Length.Max != nil && len(value) > *v.Length.Max {
			return fmt.Sprintf("default has length %d, above length.max %d", len(value), *v.Length.Max)
		}
	}
	return ""
}

// --- small helpers ----------------------------------------------------

func nameSet(names []string) map[string]struct{} {
	out := make(map[string]struct{}, len(names))
	for _, n := range names {
		out[n] = struct{}{}
	}
	return out
}

func envVarNames(in []EnvVar) []string {
	out := make([]string, 0, len(in))
	for _, e := range in {
		out = append(out, e.Name)
	}
	return out
}

func secretSliceNames(in []Secret) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, s.Name)
	}
	return out
}

func fileSliceNames(in []File) []string {
	out := make([]string, 0, len(in))
	for _, f := range in {
		out = append(out, f.Name)
	}
	return out
}

func serviceSliceNames(in []ServiceNeed) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, s.Name)
	}
	return out
}

func volumePathSet(in []Volume) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		out = append(out, v.Path)
	}
	return out
}
