package nexus

import (
	"context"
	"testing"

	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/registry"
)

// TestDecoratedModulesFilter is the isolation contract for annotated
// registrations in multi-boot binaries: a deferred source (decorate's Drain)
// yields the SAME registrations to every boot, and DecoratedModules scopes
// which of them a boot accepts — so one test's InProcess never fails on the
// providers of a module another test file linked in.
func TestDecoratedModulesFilter(t *testing.T) {
	saved := deferredOptionSources
	t.Cleanup(func() { deferredOptionSources = saved })

	type aArgs struct {
		ID int `json:"id"`
	}
	handlerA := func(p Params[aArgs]) (int, error) { return 1, nil }
	handlerB := func(p Params[aArgs]) (int, error) { return 2, nil }
	deferredOptionSources = append(deferredOptionSources, func() []Option {
		return []Option{
			Module("alpha", AsRest("GET", "/alpha", handlerA)),
			Module("beta", AsRest("GET", "/beta", handlerB)),
		}
	})

	endpoints := func(opts ...Option) map[string]bool {
		app, stop, err := InProcess(config.Runtime{}, opts...)
		if err != nil {
			t.Fatalf("InProcess: %v", err)
		}
		defer stop(context.Background())
		out := map[string]bool{}
		for _, e := range app.Registry().Endpoints() {
			if e.Transport == registry.REST {
				out[e.Path] = true
			}
		}
		return out
	}

	got := endpoints(DecoratedModules("alpha"))
	if !got["/alpha"] || got["/beta"] {
		t.Fatalf("DecoratedModules(alpha): got %v, want /alpha only", got)
	}

	// The source is not consumed: a second boot in the same process still
	// sees beta.
	got = endpoints(DecoratedModules("beta"))
	if got["/alpha"] || !got["/beta"] {
		t.Fatalf("DecoratedModules(beta) on second boot: got %v, want /beta only", got)
	}

	// No names: fully isolated from every decorated registration.
	got = endpoints(DecoratedModules())
	if got["/alpha"] || got["/beta"] {
		t.Fatalf("DecoratedModules(): got %v, want neither", got)
	}

	// No marker: everything participates, unchanged.
	got = endpoints()
	if !got["/alpha"] || !got["/beta"] {
		t.Fatalf("unfiltered boot: got %v, want both", got)
	}
}
