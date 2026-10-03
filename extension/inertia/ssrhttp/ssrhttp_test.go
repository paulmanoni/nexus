package ssrhttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRender_Prod posts to the production SSR server's /render and decodes the
// {head, body} response.
func TestRender_Prod(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/render" {
			t.Errorf("prod SSR path = %q, want /render", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"head":["<title>x</title>"],"body":"<main>y</main>"}`))
	}))
	defer srv.Close()

	res, err := New(srv.URL).Render(context.Background(), []byte(`{"component":"X"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Head) != 1 || res.Head[0] != "<title>x</title>" || res.Body != "<main>y</main>" {
		t.Fatalf("decoded SSR result = %+v", res)
	}
}

// TestRender_TransportError surfaces a connection failure as an error so the
// engine can fall back to client rendering.
func TestRender_TransportError(t *testing.T) {
	// Closed server → dial error.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	if _, err := New(url).Render(context.Background(), []byte(`{}`)); err == nil {
		t.Fatal("expected a transport error from a closed SSR server")
	}
}
