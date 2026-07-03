package manifest

import (
	"strings"
	"testing"
)

// containsIssue reports whether issues contains one with the given
// code at exactly the given path. We assert on (code, path) tuples
// rather than the human Message so refactoring message wording
// doesn't churn tests.
func containsIssue(issues []Issue, code ErrCode, path string) bool {
	for _, i := range issues {
		if i.Code == code && i.Path == path {
			return true
		}
	}
	return false
}

func TestLint_CleanManifest_NoIssues(t *testing.T) {
	got := Lint(baseManifest())
	// baseManifest's JWT_SIGNING_KEY is Required + EnvScoped with no
	// Default → that's a Secret (no warning rule applied), so this
	// manifest should be clean.
	if len(got) != 0 {
		t.Errorf("clean manifest produced %d issues: %+v", len(got), got)
	}
}

func TestLint_DuplicateEnvironment_Error(t *testing.T) {
	m := baseManifest()
	m.Environments = append(m.Environments, Environment{Name: "production"})
	got := Lint(m)
	if !containsIssue(got, ErrInvalidValidation, "environments[3]") {
		t.Fatalf("expected duplicate-env error at environments[3]; got %+v", got)
	}
}

func TestLint_EmptyEnvironmentName_Error(t *testing.T) {
	m := baseManifest()
	m.Environments = append(m.Environments, Environment{})
	got := Lint(m)
	if !containsIssue(got, ErrInvalidValidation, "environments[3]") {
		t.Fatalf("expected empty-name error; got %+v", got)
	}
}

func TestLint_DuplicateEnvVar_Error(t *testing.T) {
	m := baseManifest()
	m.Env = append(m.Env, EnvVar{Name: "LOG_LEVEL", Default: "x"})
	got := Lint(m)
	if !containsIssue(got, ErrInvalidValidation, "env.LOG_LEVEL") {
		t.Fatalf("expected duplicate env error; got %+v", got)
	}
}

func TestLint_RequiredEnvWithoutSource_Warning(t *testing.T) {
	m := baseManifest()
	m.Env = append(m.Env, EnvVar{Name: "API_BASE_URL", Required: true})
	got := Lint(m)
	if !containsIssue(got, ErrMissingDeclaration, "env.API_BASE_URL") {
		t.Fatalf("expected required-without-source warning; got %+v", got)
	}
}

func TestLint_RequiredEnvWithDefault_NoWarning(t *testing.T) {
	m := baseManifest()
	m.Env = append(m.Env, EnvVar{Name: "API_BASE_URL", Required: true, Default: "http://localhost:8080"})
	got := Lint(m)
	for _, i := range got {
		if i.Path == "env.API_BASE_URL" {
			t.Errorf("unexpected issue on env.API_BASE_URL: %+v", i)
		}
	}
}

func TestLint_BadRegex_Error(t *testing.T) {
	m := baseManifest()
	m.Env[1].Validation = &EnvValidation{Regex: "[invalid"}
	got := Lint(m)
	if !containsIssue(got, ErrInvalidValidation, "env.FEATURE_FLAGS.validation.regex") {
		t.Fatalf("expected bad-regex error; got %+v", got)
	}
}

func TestLint_LengthMinGreaterThanMax_Error(t *testing.T) {
	m := baseManifest()
	min := 50
	max := 10
	m.Secrets[0].Validation = &EnvValidation{Length: &Range{Min: &min, Max: &max}}
	got := Lint(m)
	if !containsIssue(got, ErrInvalidValidation, "secrets.JWT_SIGNING_KEY.validation.length") {
		t.Fatalf("expected length min>max error; got %+v", got)
	}
}

func TestLint_DefaultViolatesEnum_Error(t *testing.T) {
	m := baseManifest()
	m.Env[0].Default = "trace" // not in [debug info warn error]
	got := Lint(m)
	if !containsIssue(got, ErrInvalidValidation, "env.LOG_LEVEL.default") {
		t.Fatalf("expected default-violates-enum error; got %+v", got)
	}
}

func TestLint_DuplicateServiceName_Error(t *testing.T) {
	m := baseManifest()
	m.Services = append(m.Services, ServiceNeed{Name: "main_db", Kind: "postgres"})
	got := Lint(m)
	if !containsIssue(got, ErrInvalidValidation, "services.main_db") {
		t.Fatalf("expected duplicate service error; got %+v", got)
	}
}

func TestLint_DuplicateFilePath_Error(t *testing.T) {
	m := baseManifest()
	m.Files = append(m.Files, File{Name: "other_bundle", Path: "/etc/ssl/app/bundle.pem"})
	got := Lint(m)
	if !containsIssue(got, ErrInvalidValidation, "files.other_bundle") {
		t.Fatalf("expected duplicate path error; got %+v", got)
	}
}

func TestLint_BoundToUnknownService_Error(t *testing.T) {
	m := baseManifest()
	m.Env[0].BoundTo = "nonexistent.host"
	got := Lint(m)
	if !containsIssue(got, ErrMissingDeclaration, "env.LOG_LEVEL.boundTo") {
		t.Fatalf("expected unknown-service-bound error; got %+v", got)
	}
}

func TestLint_BoundToMalformed_Error(t *testing.T) {
	m := baseManifest()
	m.Env[0].BoundTo = "no_dot"
	got := Lint(m)
	if !containsIssue(got, ErrInvalidValidation, "env.LOG_LEVEL.boundTo") {
		t.Fatalf("expected malformed-boundTo error; got %+v", got)
	}
}

func TestLint_OverrideUnknownEnvironment_Error(t *testing.T) {
	m := baseManifest()
	m.Overrides = map[string]Override{
		"qa": {Env: map[string]string{"LOG_LEVEL": "warn"}},
	}
	got := Lint(m)
	if !containsIssue(got, ErrUnknownEnvironment, "overrides.qa") {
		t.Fatalf("expected unknown-env override error; got %+v", got)
	}
}

func TestLint_OverrideUnknownEnvKey_Error(t *testing.T) {
	m := baseManifest()
	m.Overrides = map[string]Override{
		"production": {Env: map[string]string{"NOT_DECLARED": "x"}},
	}
	got := Lint(m)
	if !containsIssue(got, ErrUnknownOverrideKey, "overrides.production.env.NOT_DECLARED") {
		t.Fatalf("expected unknown-env-key error; got %+v", got)
	}
}

func TestLint_OverrideScalarAndSpecConflict_Error(t *testing.T) {
	m := baseManifest()
	def := "warn"
	m.Overrides = map[string]Override{
		"production": {
			Env:      map[string]string{"LOG_LEVEL": "warn"},
			EnvSpecs: map[string]EnvVarPatch{"LOG_LEVEL": {Default: &def}},
		},
	}
	got := Lint(m)
	if !containsIssue(got, ErrConflictingOverride, "overrides.production.env.LOG_LEVEL") {
		t.Fatalf("expected conflict error; got %+v", got)
	}
}

func TestLint_OverrideRemoveUnknownPath_Error(t *testing.T) {
	m := baseManifest()
	m.Overrides = map[string]Override{
		"preview": {Removed: []string{"secrets.NEVER_DECLARED"}},
	}
	got := Lint(m)
	if !containsIssue(got, ErrUnknownOverrideKey, "overrides.preview.removed[secrets.NEVER_DECLARED]") {
		t.Fatalf("expected unknown-removed error; got %+v", got)
	}
}

func TestLint_OverrideRemoveUnknownBucket_Error(t *testing.T) {
	m := baseManifest()
	m.Overrides = map[string]Override{
		"preview": {Removed: []string{"junk.X"}},
	}
	got := Lint(m)
	if !containsIssue(got, ErrUnknownOverrideKey, "overrides.preview.removed[junk.X]") {
		t.Fatalf("expected unknown-bucket error; got %+v", got)
	}
}

func TestLint_OverrideUnknownService_Error(t *testing.T) {
	m := baseManifest()
	large := "large"
	m.Overrides = map[string]Override{
		"production": {Services: map[string]ServicePatch{"missing": {Size: &large}}},
	}
	got := Lint(m)
	if !containsIssue(got, ErrUnknownOverrideKey, "overrides.production.services.missing") {
		t.Fatalf("expected unknown-service error; got %+v", got)
	}
}

func TestLint_DeterministicOrdering(t *testing.T) {
	// Two runs over the same manifest must produce byte-equal Issue
	// slices — the lint output is the user-facing diagnostic; flaky
	// ordering would churn CI gates.
	m := baseManifest()
	m.Env = append(m.Env, EnvVar{Name: "LOG_LEVEL", Default: "x"}) // duplicate
	m.Env = append(m.Env, EnvVar{Name: "ANOTHER", Required: true}) // required without source
	got1 := Lint(m)
	got2 := Lint(m)
	if len(got1) != len(got2) {
		t.Fatalf("length: %d vs %d", len(got1), len(got2))
	}
	for i := range got1 {
		if got1[i] != got2[i] {
			t.Errorf("issue[%d] differs between runs: %+v vs %+v", i, got1[i], got2[i])
		}
	}
}

func TestLint_MultipleIssuesReportedTogether(t *testing.T) {
	// Lint must return all issues in one pass, not fail-fast on the
	// first. CLI / IDE rely on this to show every error per save.
	m := baseManifest()
	m.Environments = append(m.Environments, Environment{Name: "production"}) // dup env
	m.Env[0].Validation = &EnvValidation{Regex: "[bad"}                      // bad regex
	m.Overrides = map[string]Override{
		"qa":         {},                                     // unknown env
		"production": {Env: map[string]string{"BOGUS": "x"}}, // unknown key
	}
	got := Lint(m)
	if len(got) < 4 {
		t.Fatalf("expected >=4 issues, got %d: %+v", len(got), got)
	}
	wantCodes := []ErrCode{ErrInvalidValidation, ErrUnknownEnvironment, ErrUnknownOverrideKey}
	for _, want := range wantCodes {
		found := false
		for _, i := range got {
			if i.Code == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing code %s in output: %+v", want, got)
		}
	}
}

func TestLint_PathsAreReadable(t *testing.T) {
	// Sanity check that paths are dotted, human-readable, and stable.
	// IDE deep-link suggestions rely on this format.
	m := baseManifest()
	m.Env = append(m.Env, EnvVar{Name: "X", Required: true})
	got := Lint(m)
	for _, i := range got {
		if strings.ContainsRune(i.Path, ' ') {
			t.Errorf("path %q contains whitespace", i.Path)
		}
	}
}
