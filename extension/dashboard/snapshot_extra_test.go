package dashboard

import "testing"

// TestSnapshotExtras verifies the plugin contributor seam: a registered
// contributor's payload appears under its name, nil returns are dropped, and
// re-registration overwrites (so repeated wiring across tests/apps is safe).
func TestSnapshotExtras(t *testing.T) {
	// Reset global state for a deterministic test.
	extrasMu.Lock()
	extras = map[string]func() any{}
	extrasMu.Unlock()

	if got := snapshotExtras(); got != nil {
		t.Fatalf("snapshotExtras() with no contributors = %v, want nil", got)
	}

	RegisterSnapshotExtra("auth", func() any { return map[string]any{"identities": 3} })
	RegisterSnapshotExtra("empty", func() any { return nil })
	RegisterSnapshotExtra("", func() any { return 1 }) // ignored: blank name

	out := snapshotExtras()
	if out == nil {
		t.Fatal("snapshotExtras() = nil, want a populated map")
	}
	if _, ok := out["auth"]; !ok {
		t.Errorf("missing 'auth' contributor payload; got keys %v", keysOf(out))
	}
	if _, ok := out["empty"]; ok {
		t.Errorf("nil-returning contributor should be dropped; got keys %v", keysOf(out))
	}
	if _, ok := out[""]; ok {
		t.Errorf("blank-name contributor should be ignored")
	}

	// Re-registration overwrites rather than duplicating.
	RegisterSnapshotExtra("auth", func() any { return "replaced" })
	if v := snapshotExtras()["auth"]; v != "replaced" {
		t.Errorf("re-register did not overwrite: got %v", v)
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
