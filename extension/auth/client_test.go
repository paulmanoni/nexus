package auth

import (
	"strings"
	"testing"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension"
	"github.com/paulmanoni/nexus/registry"
)

// makeRegistry seeds a registry with the auth-flow-tagged endpoints
// the contributor scans. Three endpoints exercise the three flows
// the composable supports; the helper keeps each test concise.
func makeRegistry(t *testing.T, flows ...string) *registry.Registry {
	t.Helper()
	reg := registry.New()
	for _, f := range flows {
		reg.RegisterEndpoint(registry.Endpoint{
			Service:   "auth",
			Name:      f, // "login" / "logout" / "me" — endpoint Name matches the flow
			Transport: registry.REST,
			Method:    "POST",
			Path:      "/" + f,
			Tags:      map[string]string{nexus.AuthFlowTag: f},
		})
	}
	return reg
}

func TestAuthContributor_VueEmitsComposable(t *testing.T) {
	reg := makeRegistry(t, "login", "logout", "me")
	files, err := authContributor{}.NexusContribute(extension.GenerateContext{
		Registry: reg,
		Extras:   map[string]any{"frontend.framework": "vue"},
	})
	if err != nil {
		t.Fatalf("NexusContribute: %v", err)
	}
	if len(files) != 1 || files[0].Path != "auth/vue.ts" {
		t.Fatalf("files = %+v, want one entry at auth/vue.ts", files)
	}
	src := string(files[0].Body)
	for _, want := range []string{
		"export function useAuth",
		"async function login(",
		"async function logout(",
		"async function bootstrap(",
		"login as _login",
		"logout as _logout",
		"me as _me",
		"client.tokens.set",
		"client.tokens.clear",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("auth/vue.ts missing %q", want)
		}
	}
}

// TestAuthContributor_OmitsFlowsThatArentRegistered ensures the
// emitted composable doesn't reference symbols the app never declared.
// A cookie-auth setup with only `me` shouldn't get a broken `login`
// import that fails at TS compile time.
func TestAuthContributor_OmitsFlowsThatArentRegistered(t *testing.T) {
	reg := makeRegistry(t, "me") // only "me", no login/logout
	files, err := authContributor{}.NexusContribute(extension.GenerateContext{
		Registry: reg,
		Extras:   map[string]any{"frontend.framework": "vue"},
	})
	if err != nil {
		t.Fatal(err)
	}
	src := string(files[0].Body)
	if strings.Contains(src, "login as _login") {
		t.Error("login symbol leaked into output despite no AuthRoute(login)")
	}
	if strings.Contains(src, "logout as _logout") {
		t.Error("logout symbol leaked")
	}
	if !strings.Contains(src, "me as _me") {
		t.Error("me import missing")
	}
	if !strings.Contains(src, "async function bootstrap") {
		t.Error("bootstrap function missing — should be emitted for the me flow")
	}
}

// TestAuthContributor_NoFlowsNoOutput asserts the "this app has no
// typed auth surface" path emits nothing. An app that auths via
// cookies + middleware never tags an endpoint with AuthRoute, so the
// contributor has nothing to wrap — no auth/* files should land.
func TestAuthContributor_NoFlowsNoOutput(t *testing.T) {
	reg := registry.New()
	reg.RegisterEndpoint(registry.Endpoint{
		Service: "unrelated", Name: "ping", Transport: registry.REST,
	})
	files, err := authContributor{}.NexusContribute(extension.GenerateContext{
		Registry: reg,
		Extras:   map[string]any{"frontend.framework": "vue"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("expected zero files for no-flow app; got %+v", files)
	}
}

// TestAuthContributor_NonVueFrameworkSkipped covers phase-2's scope
// limit: only Vue templates exist. A React or Svelte target should
// get nothing rather than a stale Vue file.
func TestAuthContributor_NonVueFrameworkSkipped(t *testing.T) {
	reg := makeRegistry(t, "login")
	for _, fw := range []string{"react", "svelte", "", "none"} {
		t.Run("framework="+fw, func(t *testing.T) {
			files, err := authContributor{}.NexusContribute(extension.GenerateContext{
				Registry: reg,
				Extras:   map[string]any{"frontend.framework": fw},
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(files) != 0 {
				t.Fatalf("framework=%q: expected zero files, got %+v", fw, files)
			}
		})
	}
}

func TestAuthContributor_NilRegistryRejected(t *testing.T) {
	_, err := authContributor{}.NexusContribute(extension.GenerateContext{
		Extras: map[string]any{"frontend.framework": "vue"},
	})
	if err == nil || !strings.Contains(err.Error(), "Registry is nil") {
		t.Fatalf("err = %v, want 'Registry is nil'", err)
	}
}
