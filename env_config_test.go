package nexus

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/manifest"
)

// writeTOML is defined in database_toml_test.go (same package).

func TestEnvVars_FlattensDottedNames(t *testing.T) {
	p := writeTOML(t, `
[runtime]
environment = "development"

[env.client]
id     = "ajira_portal-web"
secret = "change-me-in-prod"

[env]
region = "eu"
`)
	got, err := EnvVars(p)
	if err != nil {
		t.Fatalf("EnvVars: %v", err)
	}
	want := map[string]string{
		"client.id":     "ajira_portal-web",
		"client.secret": "change-me-in-prod",
		"region":        "eu",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%q = %q, want %q", k, got[k], v)
		}
	}
}

func TestEnvVars_ExpandsPlaceholders(t *testing.T) {
	t.Setenv("REAL_SECRET", "s3cr3t")
	p := writeTOML(t, `
[env.client]
id     = "web"
secret = "${REAL_SECRET}"
`)
	got, err := EnvVars(p)
	if err != nil {
		t.Fatalf("EnvVars: %v", err)
	}
	if got["client.secret"] != "s3cr3t" {
		t.Errorf("client.secret = %q, want expanded s3cr3t", got["client.secret"])
	}
}

func TestLoadConfig_PublishesEnvVars(t *testing.T) {
	p := writeTOML(t, `
[runtime]
environment = "development"

[env.client]
id     = "ajira_portal-web"
secret = "change-me-in-prod"
`)
	// Sanity: not set before.
	_ = os.Unsetenv("client.id")
	if _, err := LoadConfig(p); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := os.Getenv("client.id"); got != "ajira_portal-web" {
		t.Errorf("os.Getenv(client.id) = %q, want ajira_portal-web", got)
	}
	if got := os.Getenv("client.secret"); got != "change-me-in-prod" {
		t.Errorf("os.Getenv(client.secret) = %q, want change-me-in-prod", got)
	}
	// Cleanup so the dotted vars don't leak into other tests.
	_ = os.Unsetenv("client.id")
	_ = os.Unsetenv("client.secret")
}

func TestEnvVars_MissingFileIsNil(t *testing.T) {
	got, err := EnvVars(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil || got != nil {
		t.Errorf("missing file: got (%v, %v), want (nil, nil)", got, err)
	}
}

// A ${VAR} outside [env] is the app's concern at boot, not the frontend's:
// reading [env] must not need it (a CI build without the database secret).
func TestEnvVars_ExpandsOnlyEnvTable(t *testing.T) {
	t.Setenv("REAL_SECRET", "s3cr3t")
	_ = os.Unsetenv("NEXUS_TEST_UNSET_DB_PASSWORD")
	p := writeTOML(t, `
env.root.dotted = "${REAL_SECRET:x}"

[databases.main]
password = "${NEXUS_TEST_UNSET_DB_PASSWORD}"

[env.client]
id      = "web"
secret  = "${REAL_SECRET}"
literal = '${NOT_EXPANDED}'
escaped = "$${KEEP}"
list    = ["${REAL_SECRET}", 2]

[env.more]
"quoted.key" = "${NEXUS_TEST_UNSET_DB_PASSWORD:fallback}"
port = 8080

[runtime]
environment = "${NEXUS_TEST_UNSET_DB_PASSWORD}"
`)
	got, err := EnvVars(p)
	if err != nil {
		t.Fatalf("EnvVars: %v", err)
	}
	want := map[string]string{
		"root.dotted":     "s3cr3t",
		"client.id":       "web",
		"client.secret":   "s3cr3t",
		"client.literal":  "${NOT_EXPANDED}",
		"client.escaped":  "${KEEP}",
		"client.list":     "[s3cr3t 2]",
		"more.quoted.key": "fallback",
		"more.port":       "8080",
	}
	if !maps.Equal(got, want) {
		t.Errorf("EnvVars = %v\nwant       %v", got, want)
	}
}

// EnvVars must agree with what the runtime publishes for the same file.
func TestEnvVars_MatchesRuntimeExpansion(t *testing.T) {
	t.Setenv("REAL_SECRET", `a"b\c`)
	body := `
[env.client]
secret = "${REAL_SECRET}"
multi = """
x ${REAL_SECRET:y}"""
lit = '''${Z}'''
inline = { a = "${REAL_SECRET}", b = true }

[[env.list]]
name = "${REAL_SECRET}"
`
	got, err := EnvVars(writeTOML(t, body))
	if err != nil {
		t.Fatalf("EnvVars: %v", err)
	}
	expanded, err := manifest.ExpandEnvVars([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	want, err := configEnvVars(expanded)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(got, want) {
		t.Errorf("EnvVars = %v\nruntime = %v", got, want)
	}
}

func TestEnvVars_UnsetInEnvIsAnError(t *testing.T) {
	_ = os.Unsetenv("NEXUS_TEST_UNSET_CLIENT_SECRET")
	p := writeTOML(t, "[env.client]\nid = \"web\"\n\nsecret = \"${NEXUS_TEST_UNSET_CLIENT_SECRET}\"\n")
	_, err := EnvVars(p)
	if err == nil || !strings.Contains(err.Error(), "NEXUS_TEST_UNSET_CLIENT_SECRET") || !strings.Contains(err.Error(), "line 4") {
		t.Fatalf("err = %v, want the unset variable and its line", err)
	}
}

func TestEnvVarsSkippingUnset(t *testing.T) {
	_ = os.Unsetenv("NEXUS_TEST_UNSET_CLIENT_SECRET")
	_ = os.Unsetenv("NEXUS_TEST_UNSET_DB_PASSWORD")
	p := writeTOML(t, `[databases.main]
password = "${NEXUS_TEST_UNSET_DB_PASSWORD}"

[env.client]
id = "web"
secret = "${NEXUS_TEST_UNSET_CLIENT_SECRET}"
`)
	vars, skipped, err := EnvVarsSkippingUnset(p)
	if err != nil {
		t.Fatalf("EnvVarsSkippingUnset: %v", err)
	}
	if want := map[string]string{"client.id": "web"}; !maps.Equal(vars, want) {
		t.Errorf("vars = %v, want %v", vars, want)
	}
	want := []SkippedEnvVar{{Key: "client.secret", Var: "NEXUS_TEST_UNSET_CLIENT_SECRET", Line: 6}}
	if !slices.Equal(skipped, want) {
		t.Errorf("skipped = %+v, want %+v", skipped, want)
	}
	if v, s, err := EnvVarsSkippingUnset(filepath.Join(t.TempDir(), "nope.toml")); v != nil || s != nil || err != nil {
		t.Errorf("missing file: (%v, %v, %v)", v, s, err)
	}
	if _, _, err := EnvVarsSkippingUnset(writeTOML(t, "[env]\nx = \"${UNCLOSED\"\n")); err == nil {
		t.Error("a malformed ${ token must still be an error")
	}
	if _, _, err := EnvVarsSkippingUnset(writeTOML(t, "[env\nx = 1\n")); err == nil {
		t.Error("malformed TOML must still be an error")
	}
}
