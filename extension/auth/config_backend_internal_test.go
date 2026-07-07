package auth

import (
	"context"
	"testing"
)

// fakeBackend implements all three capability interfaces.
type fakeBackend struct {
	resolveID   string
	authorizeOK bool
	loginID     string
}

func (f *fakeBackend) Resolve(_ context.Context, token string) (*Identity, error) {
	if token == "" {
		return nil, nil
	}
	return &Identity{ID: f.resolveID}, nil
}
func (f *fakeBackend) Authorize(_ *Identity, _ []string) bool { return f.authorizeOK }
func (f *fakeBackend) Login(_ context.Context, _ Credentials) (*Identity, error) {
	return &Identity{ID: f.loginID}, nil
}

// resolveOnlyBackend implements just Resolve.
type resolveOnlyBackend struct{}

func (resolveOnlyBackend) Resolve(context.Context, string) (*Identity, error) {
	return &Identity{ID: "x"}, nil
}

func newState(t *testing.T, schemes []Scheme) *moduleState {
	t.Helper()
	bound, err := bindSchemes(schemes, true)
	if err != nil {
		t.Fatalf("bindSchemes: %v", err)
	}
	return &moduleState{schemes: bound, permissions: Authorization{}.permissionFn()}
}

func TestFinalizeBackend_FillsResolverAndAuthorize(t *testing.T) {
	st := newState(t, []Scheme{{Extract: Bearer()}})
	base := st.permissions
	b := &fakeBackend{resolveID: "u1", authorizeOK: true}

	if err := finalizeBackend(st, b); err != nil {
		t.Fatalf("finalizeBackend: %v", err)
	}
	// Scheme resolver filled from the backend.
	id, err := st.schemes[0].resolve(context.Background(), "tok")
	if err != nil || id == nil || id.ID != "u1" {
		t.Fatalf("scheme resolver not filled from backend: id=%v err=%v", id, err)
	}
	// Authorization replaced by the backend's Authorize.
	if !st.permissions(nil, []string{"anything"}) {
		t.Error("backend Authorize should now govern permissions")
	}
	if &base == &st.permissions {
		t.Error("permissions func should have been replaced")
	}
	if st.backend == nil {
		t.Error("state.backend should be set")
	}
}

func TestFinalizeBackend_ResolveOnlyKeepsConfigAuthorization(t *testing.T) {
	st := newState(t, []Scheme{{Extract: Bearer()}})
	// A backend without Authorize must NOT touch the permission func.
	if err := finalizeBackend(st, resolveOnlyBackend{}); err != nil {
		t.Fatalf("finalizeBackend: %v", err)
	}
	if st.schemes[0].resolve == nil {
		t.Error("resolver should be filled")
	}
	// Default exact-match: an identity with no roles fails a required perm.
	if st.permissions(&Identity{}, []string{"NEED"}) {
		t.Error("config authorization should still apply for a resolve-only backend")
	}
}

func TestFinalizeBackend_ExplicitResolveWins(t *testing.T) {
	explicit := func(context.Context, string) (*Identity, error) { return &Identity{ID: "explicit"}, nil }
	st := newState(t, []Scheme{{Extract: Bearer(), Resolve: explicit}})
	if err := finalizeBackend(st, &fakeBackend{resolveID: "backend"}); err != nil {
		t.Fatalf("finalizeBackend: %v", err)
	}
	id, _ := st.schemes[0].resolve(context.Background(), "tok")
	if id.ID != "explicit" {
		t.Errorf("explicit scheme Resolve should win over backend, got %q", id.ID)
	}
}

func TestFinalizeBackend_NoResolverIsError(t *testing.T) {
	st := newState(t, []Scheme{{Extract: Bearer()}})
	// A backend that resolves nothing (implements no capability) leaves the
	// scheme without a resolver → error.
	type dud struct{}
	if err := finalizeBackend(st, dud{}); err == nil {
		t.Error("a scheme left without a resolver must error")
	}
}
