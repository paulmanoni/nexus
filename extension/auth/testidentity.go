package auth

import (
	"context"
	"net/http"
	"sync"
	"testing"
)

// The test credential: in a test binary only (testing.Testing), a request
// carrying TestIdentityHeader is the identity authtest.As registered under
// its value, and one carrying TestUserHeader is that user, loaded through
// the app's Users. A production binary ignores both headers.

const (
	TestIdentityHeader = "X-Nexus-Test-Identity"
	TestUserHeader     = "X-Nexus-Test-User"
)

var testIdentities sync.Map // key → *Identity

// RegisterTestIdentity stores id for TestIdentityHeader: key; authtest.As
// calls it. Outside a test binary it does nothing.
func RegisterTestIdentity(key string, id *Identity) {
	if testing.Testing() {
		testIdentities.Store(key, id)
	}
}

// testIdentity is the identity a test request names, if any.
func (st *moduleState) testIdentity(ctx context.Context, r *http.Request) (*Identity, bool) {
	if !testing.Testing() {
		return nil, false
	}
	if key := r.Header.Get(TestIdentityHeader); key != "" {
		if v, ok := testIdentities.Load(key); ok {
			return st.expandRoles(v.(*Identity)), true
		}
	}
	if uid := r.Header.Get(TestUserHeader); uid != "" && st.config.users != nil {
		if id, err := st.load(ctx, uid); err == nil && id != nil {
			return id, true
		}
	}
	return nil, false
}
