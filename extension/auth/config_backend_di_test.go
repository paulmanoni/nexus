package auth_test

import (
	"context"
	"testing"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension/auth"
)

// dep is an app service the backend closes over, proving UseBackend runs in
// DI (no package globals / backfill Invoke needed).
type dep struct{ prefix string }

type diBackend struct{ d *dep }

func (b *diBackend) Resolve(_ context.Context, token string) (*auth.Identity, error) {
	if token == "" {
		return nil, nil
	}
	return &auth.Identity{ID: b.d.prefix + token}, nil
}
func (b *diBackend) Login(_ context.Context, cred auth.Credentials) (*auth.Identity, error) {
	pw, ok := cred.(auth.Password)
	if !ok {
		return nil, auth.ErrInvalidCredentials
	}
	return &auth.Identity{ID: b.d.prefix + pw.Username}, nil
}

// TestConfigBackend_DIWiring proves a UseBackend constructor is built from
// the container (closing over *dep) and drives both Manager.Resolve and
// Manager.Login — the "seamless with Config" path, no globals.
func TestConfigBackend_DIWiring(t *testing.T) {
	var mgr *auth.Manager
	_, stop, err := nexus.InProcess(nexus.Config{},
		nexus.Provide(func() *dep { return &dep{prefix: "u:"} }),
		auth.Module(auth.Config{
			Authentication: auth.Authentication{
				Schemes: []auth.Scheme{{Extract: auth.Bearer()}}, // Resolve from backend
			},
			Backend: auth.UseBackend(func(d *dep) *diBackend { return &diBackend{d: d} }),
		}),
		nexus.Invoke(func(m *auth.Manager) { mgr = m }),
	)
	if err != nil {
		t.Fatalf("InProcess: %v", err)
	}
	defer stop(context.Background())

	if mgr == nil {
		t.Fatal("Manager not injected")
	}

	// Resolve flows through the backend (scheme had no Resolve).
	id, err := mgr.Resolve(context.Background(), "tok-1")
	if err != nil || id == nil || id.ID != "u:tok-1" {
		t.Fatalf("Manager.Resolve via backend: id=%v err=%v", id, err)
	}

	// Login flows through the backend's Login capability.
	lid, err := mgr.Login(context.Background(), auth.Password{Username: "alice", Password: "x"})
	if err != nil || lid == nil || lid.ID != "u:alice" {
		t.Fatalf("Manager.Login via backend: id=%v err=%v", lid, err)
	}
}

// TestConfigBackend_StaticValue covers StaticBackend + a scheme-less config
// (default bearer scheme provided by the backend).
func TestConfigBackend_StaticValue(t *testing.T) {
	var mgr *auth.Manager
	_, stop, err := nexus.InProcess(nexus.Config{},
		auth.Module(auth.Config{
			Backend: auth.StaticBackend(&diBackend{d: &dep{prefix: "s:"}}),
		}),
		nexus.Invoke(func(m *auth.Manager) { mgr = m }),
	)
	if err != nil {
		t.Fatalf("InProcess: %v", err)
	}
	defer stop(context.Background())

	id, err := mgr.Resolve(context.Background(), "z")
	if err != nil || id == nil || id.ID != "s:z" {
		t.Fatalf("static backend Resolve (default scheme): id=%v err=%v", id, err)
	}
}
