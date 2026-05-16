package nexus

import (
	"context"
	"testing"
)

// Mirror portal_admin's plain-func pattern: handler takes a
// context, returns a response + error. AsQuery treats the function
// itself as the handler (no constructor wrapper) when the signature
// matches that shape.
func newModAQuery(_ context.Context) (string, error) { return "a", nil }
func newModBQuery(_ context.Context) (string, error) { return "b", nil }

// TestAutoMount_ModuleAttribution_DistinctPartitions pins the fix
// for portal_admin's dashboard regression: two modules, each with
// plain-func GraphQL handlers, both resolved through the per-module
// fallback to a *Service named after the module. Before the
// partition-key fix, both modules' handlers landed in one partition
// keyed only on (reflect.Type, mountPath); whichever module was
// registered first won the partition and tagged every query inside
// (including the OTHER module's handlers) with its name. The
// dashboard then drew the wrong source.
//
// Verify by reading the registry's per-endpoint Service field after
// boot — each query must carry the name of the module that declared
// it, not whichever module happened to land first.
func TestAutoMount_ModuleAttribution_DistinctPartitions(t *testing.T) {
	app, err := newApp(
		Config{Server: ServerConfig{Addr: "127.0.0.1:0"}},
		Module("modA", AsQuery(newModAQuery)),
		Module("modB", AsQuery(newModBQuery)),
	)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	defer app.Stop()

	endpoints := app.Registry().Endpoints()
	got := map[string]string{}
	for _, e := range endpoints {
		if e.Transport != "graphql" {
			continue
		}
		got[e.Name] = e.Service
	}

	// AsQuery derives the op name from the function name with a
	// leading lowercase ("newModAQuery" → "newModAQuery" verbatim
	// since it's already lowercase first letter).
	if got["newModAQuery"] != "modA" {
		t.Errorf("newModAQuery service = %q; want modA (full map = %#v)", got["newModAQuery"], got)
	}
	if got["newModBQuery"] != "modB" {
		t.Errorf("newModBQuery service = %q; want modB (full map = %#v)", got["newModBQuery"], got)
	}
}