package manifest

import "testing"

// TestLoadInputsTOML_EnvBridgeIgnored ensures the runtime [env.*] bridge
// (arbitrary string values) doesn't crash the deploy-manifest parser, while
// genuine [env.*] EnvVar declarations are still materialized.
func TestLoadInputsTOML_EnvBridgeIgnored(t *testing.T) {
	doc := `
[env.client]
id     = "ajira_portal-web"
secret = "change-me-in-prod"

[env.LOG_LEVEL]
secret  = false
default = "info"
`
	m, err := LoadInputsTOML([]byte(doc))
	if err != nil {
		t.Fatalf("LoadInputsTOML must not crash on the [env] bridge form: %v", err)
	}
	// Only the real deploy EnvVar declaration is materialized.
	if len(m.Env) != 1 || m.Env[0].Name != "LOG_LEVEL" || m.Env[0].Default != "info" {
		t.Fatalf("expected only the LOG_LEVEL deploy EnvVar, got %+v", m.Env)
	}
}
