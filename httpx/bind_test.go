package httpx

import (
	"net/http/httptest"
	"testing"
)

// paramCtx builds a Ctx whose path params come from the given map.
func paramCtx(params map[string]string) *Ctx {
	return &Ctx{
		Request: httptest.NewRequest("GET", "/", nil),
		param:   func(k string) string { return params[k] },
	}
}

// TestShouldBindUri_PathAndUriTags proves the path binder honors the preferred
// `path:"name"` tag and still honors the legacy `uri:"name"` tag, so old code
// keeps working while new code can use the clearer spelling.
func TestShouldBindUri_PathAndUriTags(t *testing.T) {
	type args struct {
		ID   string `path:"id"`
		Slug string `uri:"slug"`
	}
	c := paramCtx(map[string]string{"id": "42", "slug": "hello"})

	var a args
	if err := c.ShouldBindUri(&a); err != nil {
		t.Fatalf("ShouldBindUri: %v", err)
	}
	if a.ID != "42" {
		t.Errorf("path tag: ID = %q, want %q", a.ID, "42")
	}
	if a.Slug != "hello" {
		t.Errorf("uri tag: Slug = %q, want %q", a.Slug, "hello")
	}
}

// TestShouldBindUri_PathWins confirms that when a field carries both tags, the
// preferred `path` tag is the one consulted.
func TestShouldBindUri_PathWins(t *testing.T) {
	type args struct {
		ID string `path:"new" uri:"old"`
	}
	c := paramCtx(map[string]string{"new": "yes", "old": "no"})

	var a args
	if err := c.ShouldBindUri(&a); err != nil {
		t.Fatalf("ShouldBindUri: %v", err)
	}
	if a.ID != "yes" {
		t.Errorf("ID = %q, want %q (path tag should win over uri)", a.ID, "yes")
	}
}
