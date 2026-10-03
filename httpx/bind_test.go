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

// TestShouldBindUri_PathTag proves the path binder fills `path:"name"` fields
// and ignores the retired `uri:"name"` spelling.
func TestShouldBindUri_PathTag(t *testing.T) {
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
	if a.Slug != "" {
		t.Errorf("uri tag: Slug = %q, want it unbound (uri: is not read)", a.Slug)
	}
}
