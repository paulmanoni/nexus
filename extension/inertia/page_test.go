package inertia

import (
	"strings"
	"testing"
)

// TestValidatePage: a malformed registration fails at option-build time with
// a message naming the page, instead of a route that never matches or a
// client-side "component not found" at render time.
func TestValidatePage(t *testing.T) {
	handler := func() {}
	ok := func(method, path, component string) {
		t.Helper()
		if err := validatePage(method, path, component, handler); err != nil {
			t.Errorf("validatePage(%q, %q, %q) = %v, want nil", method, path, component, err)
		}
	}
	ok("GET", "/users", "Users/Index")
	ok("get,post", "/login", "Login")
	ok("GET POST", "/login", "Login")

	bad := func(method, path, component string, fn any, want string) {
		t.Helper()
		err := validatePage(method, path, component, fn)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("validatePage(%q, %q, %q) = %v, want containing %q", method, path, component, err, want)
		}
	}
	bad("FETCH", "/users", "Users/Index", handler, "not an HTTP method")
	bad("GET", "users", "Users/Index", handler, `must start with "/"`)
	bad("GET", "/users", "  ", handler, "component name is empty")
	bad("GET", "/users", "Users/Index", nil, "handler is nil")
}
