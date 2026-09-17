package inertia

import (
	"context"
	"errors"
	"testing"
)

func TestAlwaysFunc(t *testing.T) {
	p := AlwaysFunc(func() (string, error) { return "v", nil })
	if p.kind != kindAlways {
		t.Fatalf("kind = %v", p.kind)
	}
	v, err := p.resolve()
	if err != nil || v != "v" {
		t.Fatalf("resolve = %v, %v", v, err)
	}

	boom := errors.New("boom")
	p = AlwaysFunc(func() (string, error) { return "", boom })
	if _, err := p.resolve(); !errors.Is(err, boom) {
		t.Fatalf("error must propagate, got %v", err)
	}
}

func TestShareProvideJoinsGroup(t *testing.T) {
	// Compile-shape check: ShareProvide must accept a DI ctor returning a
	// SharedProvider; the full render path is covered by the Share tests,
	// and the group tag is identical.
	opt := ShareProvide(func() SharedProvider {
		return func(ctx context.Context) (string, any) { return "can", map[string]bool{"x": true} }
	})
	if opt == nil {
		t.Fatal("nil option")
	}
}
