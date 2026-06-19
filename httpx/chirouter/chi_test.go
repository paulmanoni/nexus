package chirouter

import (
	"net/http/httptest"
	"testing"

	"github.com/paulmanoni/nexus/httpx"
)

// chi stores the catch-all under the key "*"; the seam must expose it under the
// route's wildcard name WITH gin's leading slash, so c.Param("filepath") on
// /assets/*filepath returns "/index.js" (not "" and not "index.js").
func TestWildcardParamGinCompatible(t *testing.T) {
	r := New()
	var named, suffix string
	r.Handle("GET", "/assets/*filepath", func(c *httpx.Ctx) {
		named = c.Param("filepath")
		suffix = "assets" + named
		c.String(200, suffix)
	})
	req := httptest.NewRequest("GET", "/assets/index-Cv7AL3WY.js", nil)
	r.ServeHTTP(httptest.NewRecorder(), req)
	if named != "/index-Cv7AL3WY.js" {
		t.Fatalf(`c.Param("filepath") = %q, want /index-Cv7AL3WY.js`, named)
	}
	if suffix != "assets/index-Cv7AL3WY.js" {
		t.Fatalf("joined path = %q, want assets/index-Cv7AL3WY.js", suffix)
	}
}

// Named (non-wildcard) params still resolve normally.
func TestNamedParam(t *testing.T) {
	r := New()
	var id string
	r.Handle("GET", "/users/:id", func(c *httpx.Ctx) {
		id = c.Param("id")
		c.String(200, id)
	})
	req := httptest.NewRequest("GET", "/users/42", nil)
	r.ServeHTTP(httptest.NewRecorder(), req)
	if id != "42" {
		t.Fatalf(`c.Param("id") = %q, want 42`, id)
	}
}
