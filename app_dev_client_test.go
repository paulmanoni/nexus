package nexus

import (
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
