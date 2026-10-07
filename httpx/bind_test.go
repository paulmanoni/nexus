package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
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

// A repeated field (checkboxes of one name) binds to a slice of any scalar.
func TestShouldBindQuery_Slices(t *testing.T) {
	type args struct {
		Names []string  `query:"name"`
		IDs   []int64   `query:"id"`
		On    []bool    `query:"on"`
		Rates []float64 `query:"rate"`
	}
	c := &Ctx{Request: httptest.NewRequest("GET", "/?name=a&name=b&id=3&id=5&on=true&rate=1.5", nil)}
	var a args
	if err := c.ShouldBindQuery(&a); err != nil {
		t.Fatal(err)
	}
	if len(a.Names) != 2 || a.Names[1] != "b" || len(a.IDs) != 2 || a.IDs[0] != 3 || a.IDs[1] != 5 ||
		len(a.On) != 1 || !a.On[0] || len(a.Rates) != 1 || a.Rates[0] != 1.5 {
		t.Errorf("bound %+v", a)
	}
	bad := &Ctx{Request: httptest.NewRequest("GET", "/?id=3&id=x", nil)}
	if err := bad.ShouldBindQuery(&a); err == nil {
		t.Error("a non-number in an []int64 binds")
	}
}

type bindAddress struct {
	City string `form:"city" query:"city"`
}

type bindPerson struct {
	bindAddress
	Name string `form:"name" query:"name"`
}

// An embedded struct's fields bind as the outer struct's, as encoding/json
// treats them — from a form and from a query alike.
func TestBindEmbeddedStruct(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/?name=q&city=qc", strings.NewReader("name=Ann&city=Dodoma"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	c := NewCtx(httptest.NewRecorder(), req)
	var p bindPerson
	if err := c.ShouldBind(&p); err != nil {
		t.Fatal(err)
	}
	if p.Name != "Ann" || p.City != "Dodoma" {
		t.Errorf("form: %+v", p)
	}
	var q bindPerson
	if err := c.ShouldBindQuery(&q); err != nil {
		t.Fatal(err)
	}
	if q.Name != "q" || q.City != "qc" {
		t.Errorf("query: %+v", q)
	}
}
