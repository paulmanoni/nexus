package auth_test

import (
	"context"
	"testing"

	"github.com/paulmanoni/nexus/extension/auth"
)

func TestSubject_TypedID(t *testing.T) {
	ctx := auth.WithIdentity(context.Background(), &auth.Identity{ID: "42"})

	if v, ok := auth.Subject[uint](ctx); !ok || v != 42 {
		t.Fatalf("uint: got (%v, %v), want (42, true)", v, ok)
	}
	if v, ok := auth.Subject[int64](ctx); !ok || v != 42 {
		t.Fatalf("int64: got (%v, %v), want (42, true)", v, ok)
	}
	if v, ok := auth.Subject[string](ctx); !ok || v != "42" {
		t.Fatalf("string: got (%q, %v), want (\"42\", true)", v, ok)
	}
}

func TestSubject_AnonymousAndUnparseable(t *testing.T) {
	// Anonymous request → false for any T.
	if _, ok := auth.Subject[uint](context.Background()); ok {
		t.Fatal("anonymous request should yield ok=false")
	}
	// Non-numeric ID into a numeric T → false, but still readable as string.
	ctx := auth.WithIdentity(context.Background(), &auth.Identity{ID: "abc"})
	if _, ok := auth.Subject[uint](ctx); ok {
		t.Fatal("non-numeric ID into uint should yield ok=false")
	}
	if v, ok := auth.Subject[string](ctx); !ok || v != "abc" {
		t.Fatalf("string of \"abc\": got (%q, %v)", v, ok)
	}
}

func TestSubjectPtr_NilWhenAnonymous(t *testing.T) {
	if p := auth.SubjectPtr[uint](context.Background()); p != nil {
		t.Fatalf("anonymous request should give nil, got %v", *p)
	}
	ctx := auth.WithIdentity(context.Background(), &auth.Identity{ID: "7"})
	p := auth.SubjectPtr[uint](ctx)
	if p == nil || *p != 7 {
		t.Fatalf("want *uint(7), got %v", p)
	}
}
