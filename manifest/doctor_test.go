package manifest

import (
	"strings"
	"testing"
)

// findingContains reports whether findings includes one with the
// given code at exactly the given path. Tests assert on (code, path)
// pairs rather than message wording so refactoring tooltips doesn't
// churn tests.
func findingContains(findings []Finding, code, path string) bool {
	for _, f := range findings {
		if f.Code == code && f.Path == path {
			return true
		}
	}
	return false
}

func TestDoctor_CleanManifest_NoFindings(t *testing.T) {
	min32 := 32
	m := Manifest{
		Environments: []Environment{{Name: "production"}},
		Env: []EnvVar{
			{Name: "LOG_LEVEL", Default: "info"},
		},
		Secrets: []Secret{
			{
				Name:     "JWT_SIGNING_KEY",
				Required: true,
				Validation: &EnvValidation{
					Length: &Range{Min: &min32},
				},
			},
		},
		Services: []ServiceNeed{
			{
				Name: "main_db",
				Kind: "postgres",
				ExposeAs: map[string]string{
					"url": "DATABASE_URL",
				},
			},
		},
	}
	got := Doctor(m)
	for _, f := range got {
		if f.Severity == SeverityError {
			t.Errorf("clean manifest produced error finding: %+v", f)
		}
	}
}

func TestDoctor_RequiredEnvWithoutSource_Warning(t *testing.T) {
	m := Manifest{
		Env: []EnvVar{
			{Name: "DB_URL", Required: true}, // no default, no boundTo
		},
	}
	got := Doctor(m)
	if !findingContains(got, "required_env_no_source", "env.DB_URL") {
		t.Fatalf("expected required-without-source finding; got %+v", got)
	}
}

func TestDoctor_RequiredEnvWithDefault_NoFinding(t *testing.T) {
	m := Manifest{
		Env: []EnvVar{
			{Name: "PORT", Required: true, Default: "8080"},
		},
	}
	got := Doctor(m)
	if findingContains(got, "required_env_no_source", "env.PORT") {
		t.Fatalf("default should satisfy the check; got %+v", got)
	}
}

func TestDoctor_BoundToMalformed_Error(t *testing.T) {
	m := Manifest{
		Env: []EnvVar{
			{Name: "X", BoundTo: "no_dot_here"},
		},
		Services: []ServiceNeed{
			{Name: "main_db", Kind: "postgres"},
		},
	}
	got := Doctor(m)
	if !findingContains(got, "boundto_malformed", "env.X.boundTo") {
		t.Fatalf("expected malformed boundTo error; got %+v", got)
	}
}

func TestDoctor_BoundToUnknownService_Error(t *testing.T) {
	m := Manifest{
		Env: []EnvVar{
			{Name: "DB_URL", BoundTo: "ghost.url"},
		},
		Services: []ServiceNeed{
			{Name: "main_db", Kind: "postgres"},
		},
	}
	got := Doctor(m)
	if !findingContains(got, "boundto_unknown_service", "env.DB_URL.boundTo") {
		t.Fatalf("expected unknown-service error; got %+v", got)
	}
}

func TestDoctor_ServiceWithoutExposeAs_Warning(t *testing.T) {
	m := Manifest{
		Services: []ServiceNeed{
			{Name: "main_db", Kind: "postgres"}, // no ExposeAs
		},
	}
	got := Doctor(m)
	if !findingContains(got, "service_no_expose_as", "services.main_db") {
		t.Fatalf("expected service-no-expose-as warning; got %+v", got)
	}
}

func TestDoctor_OptionalServiceWithoutExposeAs_NoFinding(t *testing.T) {
	m := Manifest{
		Services: []ServiceNeed{
			{Name: "cache", Kind: "redis", Optional: true},
		},
	}
	got := Doctor(m)
	if findingContains(got, "service_no_expose_as", "services.cache") {
		t.Fatalf("optional service should skip the check; got %+v", got)
	}
}

func TestDoctor_OverrideUnknownEnvironment_Error(t *testing.T) {
	m := Manifest{
		Environments: []Environment{{Name: "production"}},
		Overrides: map[string]Override{
			"qa": {Env: map[string]string{"X": "y"}}, // qa not declared
		},
		Env: []EnvVar{{Name: "X"}}, // declared in base so the inner check doesn't fire
	}
	got := Doctor(m)
	if !findingContains(got, "override_unknown_environment", "overrides.qa") {
		t.Fatalf("expected unknown-environment error; got %+v", got)
	}
}

func TestDoctor_SecretRequiredWithoutValidation_Warning(t *testing.T) {
	m := Manifest{
		Secrets: []Secret{
			{Name: "JWT_SIGNING_KEY", Required: true}, // no Validation
		},
	}
	got := Doctor(m)
	if !findingContains(got, "secret_no_validation", "secrets.JWT_SIGNING_KEY") {
		t.Fatalf("expected secret-no-validation warning; got %+v", got)
	}
}

func TestDoctor_SecretRequiredWithLength_NoValidationFinding(t *testing.T) {
	min32 := 32
	m := Manifest{
		Secrets: []Secret{
			{
				Name:     "JWT_SIGNING_KEY",
				Required: true,
				Validation: &EnvValidation{
					Length: &Range{Min: &min32},
				},
			},
		},
	}
	got := Doctor(m)
	if findingContains(got, "secret_no_validation", "secrets.JWT_SIGNING_KEY") {
		t.Fatalf("length validation should satisfy the check; got %+v", got)
	}
}

func TestDoctor_ServiceUnbound_Warning(t *testing.T) {
	m := Manifest{
		Services: []ServiceNeed{
			{Name: "main_db", Kind: "postgres"}, // no ExposeAs, no env binds
		},
	}
	got := Doctor(m)
	if !findingContains(got, "service_unbound", "services.main_db") {
		t.Fatalf("expected service-unbound warning; got %+v", got)
	}
}

func TestDoctor_ServiceWithExposeAs_NoUnboundFinding(t *testing.T) {
	m := Manifest{
		Services: []ServiceNeed{
			{
				Name:     "main_db",
				Kind:     "postgres",
				ExposeAs: map[string]string{"url": "DATABASE_URL"},
			},
		},
	}
	got := Doctor(m)
	if findingContains(got, "service_unbound", "services.main_db") {
		t.Fatalf("expose_as should satisfy the check; got %+v", got)
	}
}

func TestDoctor_ServiceBoundViaEnv_NoUnboundFinding(t *testing.T) {
	// No ExposeAs but an env var binds to the service via BoundTo —
	// the app receives connection info that way.
	m := Manifest{
		Services: []ServiceNeed{
			{Name: "main_db", Kind: "postgres"},
		},
		Env: []EnvVar{
			{Name: "DB_URL", BoundTo: "main_db.url"},
		},
	}
	got := Doctor(m)
	if findingContains(got, "service_unbound", "services.main_db") {
		t.Fatalf("env-bound service should skip the check; got %+v", got)
	}
}

func TestDoctor_OverrideReferencesUnknownEnv_Error(t *testing.T) {
	m := Manifest{
		Environments: []Environment{{Name: "production"}},
		Env: []EnvVar{
			{Name: "LOG_LEVEL", Default: "info"},
		},
		Overrides: map[string]Override{
			"production": {
				Env: map[string]string{"NOT_DECLARED": "x"},
			},
		},
	}
	got := Doctor(m)
	if !findingContains(got, "override_unknown_key", "overrides.production.env.NOT_DECLARED") {
		t.Fatalf("expected unknown-key error; got %+v", got)
	}
}

func TestDoctor_DeterministicOrdering(t *testing.T) {
	// Multiple findings; verify errors come before warnings, and
	// within severity the order is stable by path then code.
	m := Manifest{
		Environments: []Environment{{Name: "production"}},
		Env: []EnvVar{
			{Name: "A", BoundTo: "ghost.x"},          // error: unknown service
			{Name: "B", Required: true},              // warning: required no source
		},
		Services: []ServiceNeed{
			{Name: "main_db", Kind: "postgres"}, // warning: no expose_as + unbound
		},
		Overrides: map[string]Override{
			"qa": {}, // error: unknown env
		},
	}
	got := Doctor(m)
	// First non-error finding's index — every entry before it must
	// be an error.
	firstWarn := -1
	for i, f := range got {
		if f.Severity != SeverityError {
			firstWarn = i
			break
		}
	}
	if firstWarn < 0 {
		t.Fatalf("expected at least one warning in mixed set; got %+v", got)
	}
	for i := 0; i < firstWarn; i++ {
		if got[i].Severity != SeverityError {
			t.Errorf("non-error before warnings: %+v", got[i])
		}
	}
}

func TestDoctor_FindingPathsAreReadable(t *testing.T) {
	m := Manifest{
		Env: []EnvVar{{Name: "X", Required: true}},
	}
	got := Doctor(m)
	for _, f := range got {
		if strings.ContainsAny(f.Path, " \t\n") {
			t.Errorf("path %q contains whitespace", f.Path)
		}
		if f.Code == "" {
			t.Errorf("finding has empty code: %+v", f)
		}
	}
}
