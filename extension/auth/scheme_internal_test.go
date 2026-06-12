package auth

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"
)

func mustBind(t *testing.T, in []Scheme) []boundScheme {
	t.Helper()
	out, err := bindSchemes(in)
	if err != nil {
		t.Fatalf("bindSchemes: %v", err)
	}
	return out
}

func TestBindSchemes_Validation(t *testing.T) {
	if _, err := bindSchemes(nil); err == nil {
		t.Fatal("empty schemes must error")
	}
	if _, err := bindSchemes([]Scheme{{}}); err == nil {
		t.Fatal("a scheme with nil Resolve must error")
	}
	out := mustBind(t, []Scheme{{Resolve: func(context.Context, string) (*Identity, error) { return nil, nil }}})
	if out[0].name != "bearer" {
		t.Fatalf("default scheme name = %q, want \"bearer\"", out[0].name)
	}
	if out[0].extract == nil {
		t.Fatal("a nil Extract should default to Bearer()")
	}
}

func TestAuthenticate_FirstExtractingSchemeOwns(t *testing.T) {
	var bearerHit, apiHit bool
	st := &moduleState{schemes: mustBind(t, []Scheme{
		{Name: "bearer", Resolve: func(_ context.Context, tok string) (*Identity, error) {
			bearerHit = true
			return &Identity{ID: "bearer:" + tok}, nil
		}},
		{Name: "apikey", Extract: APIKey("X-API-Key"), Resolve: func(_ context.Context, tok string) (*Identity, error) {
			apiHit = true
			return &Identity{ID: "apikey:" + tok}, nil
		}},
	})}

	// Only the API key present → the apikey scheme resolves.
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-API-Key", "k1")
	id, _, err := st.authenticate(context.Background(), r)
	if err != nil || id == nil || id.ID != "apikey:k1" {
		t.Fatalf("apikey path: id=%v err=%v", id, err)
	}
	if bearerHit || !apiHit {
		t.Fatalf("wrong scheme ran (bearer=%v api=%v)", bearerHit, apiHit)
	}

	// Both present → the first-declared scheme (bearer) owns the request.
	bearerHit, apiHit = false, false
	r = httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer b1")
	r.Header.Set("X-API-Key", "k1")
	id, _, _ = st.authenticate(context.Background(), r)
	if id == nil || id.ID != "bearer:b1" {
		t.Fatalf("expected bearer to own, got %v", id)
	}
	if !bearerHit || apiHit {
		t.Fatalf("first-extract-owns violated (bearer=%v api=%v)", bearerHit, apiHit)
	}

	// No credential at all → anonymous, no error.
	r = httptest.NewRequest("GET", "/", nil)
	id, tok, err := st.authenticate(context.Background(), r)
	if id != nil || tok != "" || err != nil {
		t.Fatalf("anonymous: id=%v tok=%q err=%v", id, tok, err)
	}
}

func TestResolveVia_SharedCacheHitsOnce(t *testing.T) {
	calls := 0
	st := &moduleState{cache: newIdentityCache(CacheFor(time.Minute))}
	st.schemes = mustBind(t, []Scheme{{Resolve: func(context.Context, string) (*Identity, error) {
		calls++
		return &Identity{ID: "u1"}, nil
	}}})
	for i := 0; i < 3; i++ {
		if _, err := st.resolveVia(context.Background(), st.schemes[0], "tok"); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("resolver called %d times, want 1 (cache should absorb repeats)", calls)
	}
}
