package httpx

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func formCtx(body string) *Ctx {
	r := httptest.NewRequest("POST", "/", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return &Ctx{Request: r}
}

func TestPostForm(t *testing.T) {
	c := formCtx("a=1&a=2&b=&empty=")
	if got := c.PostForm("a"); got != "1" {
		t.Fatalf("PostForm(a) = %q, want 1", got)
	}
	if got := c.PostForm("missing"); got != "" {
		t.Fatalf("PostForm(missing) = %q, want empty", got)
	}
}

func TestDefaultPostForm(t *testing.T) {
	c := formCtx("a=1&empty=")
	if got := c.DefaultPostForm("a", "x"); got != "1" {
		t.Fatalf("DefaultPostForm(a) = %q, want 1", got)
	}
	if got := c.DefaultPostForm("missing", "x"); got != "x" {
		t.Fatalf("DefaultPostForm(missing) = %q, want x", got)
	}
	// present-but-empty must NOT fall back to the default.
	if got := c.DefaultPostForm("empty", "x"); got != "" {
		t.Fatalf("DefaultPostForm(empty) = %q, want empty", got)
	}
}

func TestGetPostForm(t *testing.T) {
	c := formCtx("empty=&a=1")
	if v, ok := c.GetPostForm("empty"); !ok || v != "" {
		t.Fatalf("GetPostForm(empty) = %q,%v, want \"\",true", v, ok)
	}
	if _, ok := c.GetPostForm("missing"); ok {
		t.Fatalf("GetPostForm(missing) ok = true, want false")
	}
}

func TestPostFormArray(t *testing.T) {
	c := formCtx("a=1&a=2&a=3")
	got := c.PostFormArray("a")
	if len(got) != 3 || got[0] != "1" || got[2] != "3" {
		t.Fatalf("PostFormArray(a) = %v, want [1 2 3]", got)
	}
	if got := c.PostFormArray("missing"); len(got) != 0 {
		t.Fatalf("PostFormArray(missing) = %v, want empty", got)
	}
}
