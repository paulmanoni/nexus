package inertia

import (
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
