package nexus

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/paulmanoni/nexus/client"
)

// TestDevAutoMountClientSDK covers the dev-only client SDK fallback:
// under NEXUS_DEV=1 a plain app (no Client config) gets the SDK mounted
// so `nexus dev` can read /__nexus/client/manifest.json, while an
// explicit opt-out, the non-dev case, and an already-mounted handler
// are all respected.
func TestDevAutoMountClientSDK(t *testing.T) {
	t.Run("dev mounts when nothing else did", func(t *testing.T) {
		t.Setenv(NexusDevEnv, "1")
		a := New(Config{})
		if a.ClientHandler() != nil {
			t.Fatal("precondition: handler should be nil before the late invoke")
		}
		devAutoMountClientSDK(a)
		if a.ClientHandler() == nil {
			t.Error("expected client SDK auto-mounted under NEXUS_DEV=1")
		}
	})

	t.Run("no mount when not in dev", func(t *testing.T) {
		// NEXUS_DEV explicitly empty for this subtest.
		t.Setenv(NexusDevEnv, "")
		a := New(Config{})
		devAutoMountClientSDK(a)
		if a.ClientHandler() != nil {
			t.Error("client SDK must NOT auto-mount outside dev")
		}
	})

	t.Run("DevDisabled opts out even in dev", func(t *testing.T) {
		t.Setenv(NexusDevEnv, "1")
		a := New(Config{Client: client.Config{DevDisabled: true}})
		devAutoMountClientSDK(a)
		if a.ClientHandler() != nil {
			t.Error("DevDisabled should keep the SDK closed in dev")
		}
	})

	t.Run("does not replace an explicit mount", func(t *testing.T) {
		t.Setenv(NexusDevEnv, "1")
		a := New(Config{Client: client.Config{Enabled: true}})
		first := a.ClientHandler()
		if first == nil {
			t.Fatal("explicit Client.Enabled should have mounted in New()")
		}
		devAutoMountClientSDK(a)
		if a.ClientHandler() != first {
			t.Error("dev fallback must not replace an explicitly-mounted handler")
		}
	})
}

// TestSDKSwitch covers the one-switch Config.SDK front door: it mounts the
// full client SDK wherever it's set, independently of Introspection — the
// app's own browser bundle imports the SDK, so a production binary that
// locks the dashboard down must still be able to serve it.
func TestSDKSwitch(t *testing.T) {
	t.Run("mounts under dev", func(t *testing.T) {
		t.Setenv(NexusDevEnv, "1")
		a := New(Config{SDK: true})
		if a.ClientHandler() == nil {
			t.Error("SDK=true should mount the client SDK under NEXUS_DEV=1")
		}
	})

	t.Run("mounts when introspection is on, even outside dev", func(t *testing.T) {
		t.Setenv(NexusDevEnv, "")
		a := New(Config{SDK: true, Introspection: true})
		if a.ClientHandler() == nil {
			t.Error("SDK=true should mount when Introspection is true")
		}
	})

	t.Run("mounts with introspection off, outside dev", func(t *testing.T) {
		t.Setenv(NexusDevEnv, "")
		a := New(Config{SDK: true}) // introspection off, not in dev
		if a.ClientHandler() == nil {
			t.Error("SDK=true should mount regardless of Introspection")
		}
	})

	t.Run("stays closed when unset", func(t *testing.T) {
		t.Setenv(NexusDevEnv, "")
		a := New(Config{})
		if a.ClientHandler() != nil {
			t.Error("no SDK switch, no client mount")
		}
	})

	// Mounting isn't enough: with introspection off the routes used to
	// mount behind a gate that 404s every non-allowlisted peer, which
	// for a browser is indistinguishable from not mounting at all.
	t.Run("routes answer an anonymous request with introspection off", func(t *testing.T) {
		t.Setenv(NexusDevEnv, "")
		a := New(Config{SDK: true})
		for _, path := range []string{
			"/__nexus/client/client.js",
			"/__nexus/client/manifest.json",
		} {
			w := httptest.NewRecorder()
			a.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
			if w.Code != http.StatusOK {
				t.Errorf("GET %s = %d, want 200", path, w.Code)
			}
		}
	})
}
