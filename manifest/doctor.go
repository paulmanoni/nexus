package manifest

import "sort"

// Doctor runs a battery of configuration-coherence checks against
// an effective Manifest and returns every finding. Distinct from
// Lint (manifest.Lint), which validates SHAPE — Doctor validates
// SETUP COHERENCE:
//
//   - Lint catches "this YAML key is misspelled / type-wrong".
//   - Doctor catches "this configuration won't work even though
//     it's well-formed" — required env without source, service
//     declared without expose_as, dashboard enabled but
//     introspection closed, etc.
//
// Findings are sorted by severity (errors before warnings before
// info) then by path so output is stable across runs — important
// for CI integration that diffs the report.
func Doctor(m Manifest) []Finding {
	var out []Finding
	for _, check := range builtinChecks {
		out = append(out, check(m)...)
	}
	sortFindings(out)
	return out
}

// Check is the function signature every diagnostic implements.
// Takes the effective manifest (post MergeOverrides), returns zero
// or more findings. Pure: same input always produces same output,
// so checks can be unit-tested in isolation without booting the
// framework.
type Check func(m Manifest) []Finding

// Finding is one diagnostic from Doctor. Mirrors lint.Issue's
// shape but distinct so the two tools can evolve independently —
// Doctor's findings often span multiple sections (e.g. "env X
// references service Y which doesn't exist"), Lint's are
// section-local.
type Finding struct {
	Severity Severity `json:"severity"`
	Code     string   `json:"code"`
	Path     string   `json:"path"`
	Message  string   `json:"message"`
	Hint     string   `json:"hint,omitempty"`
}

// builtinChecks is the registry. Adding a new check is one entry
// here + one function. The registry is package-private to keep
// the public surface tight; callers consume the aggregated output
// of Doctor() rather than picking individual checks.
var builtinChecks = []Check{
	checkRequiredWithoutSource,
	checkBoundToTargetExists,
	checkServiceHasExposeAs,
	checkOverrideEnvironmentsDeclared,
	checkSecretRequiredHasValidation,
	checkServicesWithoutEnv,
	checkOverridesReferenceUnknownInputs,
}

// sortFindings produces deterministic output order. Errors first
// so blocking issues lead the report; within each severity, sort
// by path then code so the same manifest always produces the same
// byte stream (CI diff-friendly).
func sortFindings(f []Finding) {
	sort.SliceStable(f, func(i, j int) bool {
		if f[i].Severity != f[j].Severity {
			return severityRank(f[i].Severity) < severityRank(f[j].Severity)
		}
		if f[i].Path != f[j].Path {
			return f[i].Path < f[j].Path
		}
		return f[i].Code < f[j].Code
	})
}

func severityRank(s Severity) int {
	switch s {
	case SeverityError:
		return 0
	case SeverityWarning:
		return 1
	default:
		return 2 // info / unknown
	}
}
